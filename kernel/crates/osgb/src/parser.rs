//! OSGB 对象图解析器（OSG 3.x 真实二进制序列化协议）。
//!
//! # 格式来源
//!
//! 逐字段逆向自真实 osgconv 3.6.5 语料 `testdata/osgb-real/osgb/*.osgb`，
//! 并与 OSG 3.6 源码逐一对照验证（`OutputStream/InputStream`、
//! `ObjectWrapper/Serializer` 体系、`osgWrappers/serializers` 各类 wrapper），
//! 由 `testdata/osgb-real/verify_protocol.py` 对全部 80 个 tile 做
//! blocksize 对齐全量验证（80/80 通过，对象帧字节级对齐）。
//!
//! # 文件布局
//!
//! 头部见 [`crate::header`]（20 字节固定头 + compressorName，"0" = 无压缩）。
//! 头部之后是一个根对象帧（WRITE_SCENE 只写一个根）。
//!
//! 通用对象帧（attributes bit2 置位，robust 格式）：
//!
//! ```text
//! [u32 类名长度][类名字节][u64 blocksize（含此 u64 自身 + UniqueID + 字段区）]
//!   [u32 UniqueID][字段区…]
//! ```
//!
//! `NULL` 对象 = 字符串 `"NULL"`（无 blocksize/UniqueID）。
//!
//! **UniqueID 复用**（`OutputStream::writeObject` 仅在对象首次出现时写
//! 字段区）：同一实例第二次出现时帧内只有 UniqueID、字段区为空；读取端查
//! `_identifierMap` 命中则直接跳到块尾。本解析器用同样机制去重。
//!
//! # 类支持（associates 链顺序读取基类字段）
//!
//! * `osg::Object`：Name / DataVariance / UserDataContainer(可选对象)
//! * `osg::Node`：InitialBound(USER) / 4 个回调(可选对象) / CullingActive /
//!   NodeMask(v142+) / StateSet(可选对象)
//! * `osg::Group`/`Geode`：Children(USER, `[u32 n][u64 括号][帧×n]`)
//! * `osg::LOD`/`PagedLOD`：CenterMode / UserCenter(USER) / RangeMode /
//!   RangeList(USER) / DatabasePath / RangeDataList / Children
//! * `osg::Drawable`：InitialBound(USER, 包围盒) / 2 个可选对象 /
//!   3 个 displayList 标志 / NodeMask(v142+) / CullingActive(v145+)
//! * `osg::Geometry`：PrimitiveSetList(向量) / Vertex、Normal、Color、
//!   SecondaryColor、FogCoord 数组(可选对象) / TexCoordArrayList /
//!   VertexAttribArrayList
//! * `osg::StateSet`：ModeList / AttributeList / TextureModeList /
//!   TextureAttributeList / RenderingHint 等 / UniformList / DefineList(v151+)
//!   （UniformList/DefineList 编码按 OSG 3.6.5 源码
//!   `osgWrappers/serializers/osg/StateSet.cpp` 的 readUniformList /
//!   readDefineList 推导实现；Uniform 对象帧按
//!   `osgWrappers/serializers/osg/Uniform.cpp` 实现；**真实语料未出现，
//!   仅源码推导，待真实语料验证**）
//! * `osg::Uniform`：Type（枚举）+ NumElements + Elements（值数组，
//!   旧版 `OutputStream::writeArray` 编码，读取后丢弃并记录）+ 2 个回调
//! * `osg::Material` / `osg::Texture` / `osg::Texture2D` → `osg::Image`
//!   （INLINE_DATA / INLINE_FILE / EXTERNAL 三种写入模式；INLINE 两种
//!   模式的字节忠实保留进 [`Mesh::texture_inline`]，供 GLB 写出端直通）
//! * `osg::Array` 系（Vec2/3/4Array、Float/Double/整型数组等）：
//!   BufferObject(可选对象) + Binding/Normalize/PreserveDataType + `[u32 n][元素×]`
//! * `osg::PrimitiveSet` 系（DrawElementsUByte/UShort/UInt、DrawArrays、
//!   DrawArrayLengths）：BufferObject + NumInstances + Mode + 数据
//! * `osg::VertexBufferObject`/`osg::ElementBufferObject`：
//!   Target / Usage / CopyDataAndReleaseGLBufferObject
//! * 其他类：读到类名即按 blocksize 整体跳过（skipped 如实记录，不造假）
//!
//! # 语义映射
//!
//! 每个 `osg::Geometry` 产出一个 [`Mesh`]，其 BATCHID（feature id）=
//! 该 Geometry 在源文件中的出现序号（写入 [`Mesh::batch_ids`]，与顶点
//! 一一对应，供 b3dm batch table per-feature 查询）；顶点/法线/UV 按字段
//! 槽位绑定；Color/SecondaryColor/FogCoord/VertexAttrib 及多单元 TexCoord
//! （unit ≥ 1）读取后丢弃并写入 [`ObjectInfo::notes`]（输出模型无对应
//! 通道，如实记录）。StateSet → Texture2D → Image 的 FileName 绑定为该
//! Geometry 网格的 [`Mesh::texture`]（相对 OSGB 所在目录解析）；INLINE
//! 图像字节（INLINE_DATA 原始像素 / INLINE_FILE 整文件）保留进
//! [`Mesh::texture_inline`]。
//!
//! DrawElements* 索引直接进入网格；DrawArrays 仅 mode=TRIANGLES 直接展开，
//! 其他 mode 记入 notes（不做三角化猜测）。

use std::collections::HashMap;

use crate::error::{OsgbError, Result};
use crate::header::Header;
use crate::reader::Reader;
use crate::scene::{InlineTexture, Mesh, RawImage, Scene};

/// 节点最大嵌套深度（防御畸形文件的循环引用）。
const MAX_DEPTH: u32 = 64;

/// GL 图元模式：TRIANGLES。
const MODE_TRIANGLES: u32 = 4;

/// 解析过程中记录的对象信息（供 `dump` 调试输出）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ObjectInfo {
    /// 对象帧起始字节偏移（namelen u32 处；压缩流为解压后偏移）。
    pub offset: usize,
    pub class: String,
    /// 对象块大小（blocksize u64 原值：含自身 8 字节 + UniqueID + 字段区）。
    pub body_size: u64,
    /// 是否为未知类（按 blocksize 整体跳过）。
    pub skipped: bool,
    /// 读取后无对应输出通道而丢弃的数据明细（如实记录，不造假）。
    pub notes: Vec<String>,
}

/// 顶点数组槽位（Geometry 字段顺序决定用途）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Slot {
    Vertex,
    Normal,
    Color,
    SecondaryColor,
    FogCoord,
    TexCoord(usize),
    VertexAttrib(usize),
}

/// 当前生效纹理绑定（StateSet→Texture2D→Image 链提取）：
/// 文件名 + 可选内嵌字节。子节点继承、兄弟隔离。
#[derive(Debug, Clone)]
struct TexBinding {
    name: String,
    inline: Option<InlineTexture>,
}

/// OSG 3.6 对象图解析器。
struct Parser<'a> {
    r: Reader<'a>,
    version: u32,
    /// UniqueID → 类名（OutputStream 写出端的对象去重机制）。
    ids: HashMap<u32, String>,
    meshes: Vec<Mesh>,
    objects: Vec<ObjectInfo>,
    skipped_count: usize,
    /// 当前 Geometry 构建中的网格。
    cur: Option<Mesh>,
    /// 当前生效纹理绑定（StateSet→Texture2D→Image 链提取，子节点继承、兄弟隔离）。
    cur_tex: Option<TexBinding>,
    /// 旧版 `OutputStream::writeArray` 的数组去重表（ArrayID → 已读）。
    legacy_arrays: std::collections::HashSet<u32>,
    /// 当前数组槽位（Geometry 字段顺序 → 数组用途）。
    slot: Slot,
    depth: u32,
}

/// 解析 OSGB 字节流为场景，同时收集对象树信息。
pub fn parse_bytes_traced(data: &[u8]) -> Result<(Scene, Vec<ObjectInfo>)> {
    let mut r = Reader::new(data);
    let header = Header::parse(&mut r)?;

    // 自定义 domain 版本表（attributes bit0）
    if header.attributes & 0x1 != 0 {
        let n = r.u32()?;
        for _ in 0..n {
            r.string_auto()?;
            r.u32()?;
        }
    }

    // 压缩流：compressorName 之后全部字节为 zlib deflate 流
    let payload: Vec<u8>;
    let body: &[u8] = if header.is_compressed() {
        payload = decompress_zlib(&data[r.pos()..], &header.compressor)?;
        &payload
    } else {
        &data[r.pos()..]
    };

    let mut p = Parser {
        r: Reader::new(body),
        version: header.version,
        ids: HashMap::new(),
        meshes: Vec::new(),
        objects: Vec::new(),
        skipped_count: 0,
        cur: None,
        cur_tex: None,
        legacy_arrays: std::collections::HashSet::new(),
        slot: Slot::Vertex,
        depth: 0,
    };
    p.obj_ref()?; // WRITE_SCENE：恰好一个根对象
    if p.r.pos() != body.len() {
        return Err(OsgbError::BodyOverrun {
            class: "<stream>".into(),
            offset: 0,
            at: p.r.pos(),
            expected: body.len(),
        });
    }
    Ok((Scene { meshes: p.meshes }, p.objects))
}

/// 解压 zlib 流（flate2 ZlibDecoder），失败时返回带压缩器名的明确错误。
fn decompress_zlib(input: &[u8], compressor: &str) -> Result<Vec<u8>> {
    use std::io::Read;
    let mut decoder = flate2::read::ZlibDecoder::new(input);
    let mut out = Vec::new();
    match decoder.read_to_end(&mut out) {
        Ok(_) => Ok(out),
        Err(e) => Err(OsgbError::Compressed {
            attributes: 0,
            message: format!("压缩器 {compressor}: {e}"),
        }),
    }
}

/// 解析 OSGB 字节流为场景。
pub fn parse_bytes(data: &[u8]) -> Result<Scene> {
    Ok(parse_bytes_traced(data)?.0)
}

/// 解析 OSGB 文件为场景。
pub fn parse_file(path: &std::path::Path) -> Result<Scene> {
    let data = std::fs::read(path).map_err(|source| OsgbError::Io {
        path: path.to_path_buf(),
        source,
    })?;
    parse_bytes(&data)
}

impl<'a> Parser<'a> {
    // ---- 基础帧 ----

    /// 读取对象帧头：`[u32 len][name][u64 blocksize]`。
    /// 返回 (类名, 块结束偏移)；NULL 对象返回 None。
    fn frame(&mut self) -> Result<Option<(String, usize)>> {
        let start = self.r.pos();
        let name = self.r.string_auto()?;
        if name == "NULL" {
            return Ok(None);
        }
        let size_pos = self.r.pos();
        let size = self.r.u64()?;
        let end = size_pos + size as usize;
        if end > self.r.remaining() + self.r.pos() || size < 12 {
            return Err(OsgbError::UnknownClassTruncated {
                offset: start,
                class: name,
                body_size: size,
            });
        }
        Ok(Some((name, end)))
    }

    /// 读取对象引用（帧 + UniqueID + 字段）。
    ///
    /// UniqueID 已出现过时（同一实例重复引用）字段区为空，直接跳到块尾。
    fn obj_ref(&mut self) -> Result<Option<String>> {
        let start = self.r.pos();
        let Some((name, end)) = self.frame()? else {
            return Ok(None);
        };
        let uid = self.r.u32()?;
        // 先记录对象帧（字段读取期间的 note 落在本对象上）
        self.push_info(start, name.clone(), end, false);
        if self.ids.contains_key(&uid) {
            // 引用帧：字段区为空，直接对齐到块尾
            self.r.skip(end - self.r.pos())?;
            return Ok(Some(self.ids[&uid].clone()));
        }
        self.fields(&name, end, start)?;
        if self.r.pos() != end {
            return Err(OsgbError::BodyOverrun {
                class: name.clone(),
                offset: start,
                at: self.r.pos(),
                expected: end,
            });
        }
        self.ids.insert(uid, name.clone());
        Ok(Some(name))
    }

    /// ObjectSerializer（可选对象）：`u8 标志 + 条件帧`。
    fn opt_obj(&mut self) -> Result<Option<String>> {
        if self.r.u8()? != 0 {
            self.obj_ref()
        } else {
            Ok(None)
        }
    }

    /// UserSerializer（二进制）：1 字节 ok 标志。
    fn user(&mut self) -> Result<bool> {
        Ok(self.r.u8()? == 1)
    }

    fn push_info(&mut self, start: usize, class: String, end: usize, skipped: bool) {
        let block_pos = start + 4 + class.len();
        self.objects.push(ObjectInfo {
            offset: start,
            class,
            body_size: (end - block_pos) as u64,
            skipped,
            notes: Vec::new(),
        });
        if skipped {
            self.skipped_count += 1;
        }
    }

    fn note(&mut self, msg: String) {
        if let Some(info) = self.objects.last_mut() {
            info.notes.push(msg);
        }
    }

    /// 把 note 落到指定对象（f_stateset 内部解析子对象后 last 已偏移，
    /// StateSet 自身的信息须按索引回写）。
    fn note_on(&mut self, index: usize, msg: String) {
        if let Some(info) = self.objects.get_mut(index) {
            info.notes.push(msg);
        }
    }

    /// 未知类：按 blocksize 整体跳过并如实记录。
    fn skip_fields(&mut self, name: &str, end: usize) -> Result<()> {
        let at = self.r.pos();
        let size = (end - at) as u64;
        self.r.skip(end - at)?;
        self.objects.push(ObjectInfo {
            offset: at,
            class: name.to_string(),
            body_size: size,
            skipped: true,
            notes: Vec::new(),
        });
        self.skipped_count += 1;
        Ok(())
    }

    // ---- associates 链字段 ----

    fn f_object(&mut self) -> Result<()> {
        self.r.string_auto()?; // Name
        self.r.i32()?; // DataVariance
        self.opt_obj()?; // UserDataContainer
        Ok(())
    }

    fn f_node(&mut self) -> Result<()> {
        if self.user()? {
            self.r.u64()?; // BEGIN_BRACKET
            self.r.f64()?;
            self.r.f64()?;
            self.r.f64()?; // Center (Vec3d)
            self.r.f64()?; // Radius
        }
        self.opt_obj()?; // ComputeBoundingSphereCallback
        self.opt_obj()?; // UpdateCallback
        self.opt_obj()?; // EventCallback
        self.opt_obj()?; // CullCallback
        if self.version >= 145 {
            self.r.u8()?; // CullingActive
        }
        if self.version >= 142 {
            self.r.u32()?; // NodeMask
        }
        self.opt_obj()?; // StateSet
        Ok(())
    }

    /// USER Children：`[u32 n][u64 括号][帧×n]`，兄弟节点间纹理隔离。
    fn f_children_like(&mut self) -> Result<()> {
        let n = self.r.u32()?;
        self.r.u64()?; // BEGIN_BRACKET
        for _ in 0..n {
            let saved_tex = self.cur_tex.take();
            self.enter_child()?;
            self.obj_ref()?;
            self.leave_child(saved_tex)?;
        }
        Ok(())
    }

    /// 子节点解析深度计数 +1（进入）。
    fn enter_child(&mut self) -> Result<()> {
        self.depth += 1;
        if self.depth > MAX_DEPTH {
            return Err(OsgbError::BadField {
                class: "<tree>".into(),
                offset: self.r.pos(),
                message: format!("节点嵌套超过 {MAX_DEPTH} 层"),
            });
        }
        Ok(())
    }

    /// 子节点解析深度计数 -1（退出）并恢复纹理上下文。
    fn leave_child(&mut self, saved_tex: Option<TexBinding>) -> Result<()> {
        self.depth -= 1;
        self.cur_tex = saved_tex;
        Ok(())
    }

    fn f_group(&mut self) -> Result<()> {
        if self.user()? {
            self.f_children_like()?;
        }
        Ok(())
    }

    fn f_lod(&mut self) -> Result<()> {
        self.r.u32()?; // CenterMode
        if self.user()? {
            self.r.f64()?;
            self.r.f64()?;
            self.r.f64()?;
            self.r.f64()?; // UserCenter (Vec4d)
        }
        self.r.u32()?; // RangeMode
        if self.user()? {
            let n = self.r.u32()?;
            self.r.u64()?; // BEGIN_BRACKET
            for _ in 0..n {
                self.r.f32()?;
                self.r.f32()?; // Min/Max
            }
        }
        Ok(())
    }

    fn f_pagedlod(&mut self) -> Result<()> {
        if self.user()? {
            // DatabasePath：可选字符串
            if self.r.u8()? != 0 {
                self.r.string_auto()?;
            }
        }
        self.r.u32()?; // NumChildrenThatCannotBeExpired
        self.r.u8()?; // DisableExternalChildrenPaging
        if self.user()? {
            // RangeDataList：文件名列表 + 范围对
            let n = self.r.u32()?;
            self.r.u64()?;
            for _ in 0..n {
                self.r.string_auto()?;
            }
            let n2 = self.r.u32()?;
            self.r.u64()?;
            for _ in 0..n2 {
                self.r.f32()?;
                self.r.f32()?;
            }
        }
        if self.user()? {
            self.f_children_like()?;
        }
        Ok(())
    }

    fn f_drawable(&mut self) -> Result<()> {
        if self.user()? {
            self.r.u64()?; // BEGIN_BRACKET
            for _ in 0..3 {
                self.r.f64()?; // Min (Vec3d)
            }
            for _ in 0..3 {
                self.r.f64()?; // Max (Vec3d)
            }
        }
        self.opt_obj()?; // ComputeBoundingBoxCallback
        self.opt_obj()?; // Shape
        self.r.u8()?; // SupportsDisplayList
        self.r.u8()?; // UseDisplayList
        self.r.u8()?; // UseVertexBufferObjects
        if self.version >= 142 {
            self.r.u32()?; // NodeMask
        }
        if self.version >= 145 {
            self.r.u8()?; // CullingActive
        }
        Ok(())
    }

    fn f_geometry(&mut self) -> Result<()> {
        // PrimitiveSetList（VectorSerializer 二进制：[u32 n][帧×n]，无括号）
        let n = self.r.u32()?;
        for _ in 0..n {
            self.obj_ref()?;
        }
        // 固定槽位数组：Vertex / Normal / Color / SecondaryColor / FogCoord
        for slot in [Slot::Vertex, Slot::Normal, Slot::Color, Slot::SecondaryColor, Slot::FogCoord] {
            self.slot = slot;
            self.opt_obj()?;
        }
        // TexCoordArrayList / VertexAttribArrayList
        let nt = self.r.u32()?;
        for u in 0..nt {
            self.slot = Slot::TexCoord(u as usize);
            self.obj_ref()?;
        }
        let na = self.r.u32()?;
        for u in 0..na {
            self.slot = Slot::VertexAttrib(u as usize);
            self.obj_ref()?;
        }
        Ok(())
    }

    /// MapSerializer 列表块：`[u32 n][n>0: u64 括号 + n×条目]`。
    fn modes_block(&mut self) -> Result<()> {
        let n = self.r.u32()?;
        if n > 0 {
            self.r.u64()?; // BEGIN_BRACKET
            for _ in 0..n {
                self.r.u32()?; // GLenum mode
                self.r.i32()?; // value
            }
        }
        Ok(())
    }

    fn attrs_block(&mut self) -> Result<()> {
        let n = self.r.u32()?;
        if n > 0 {
            self.r.u64()?; // BEGIN_BRACKET
            for _ in 0..n {
                self.obj_ref()?; // StateAttribute
                self.r.i32()?; // Value
            }
        }
        Ok(())
    }

    fn f_stateset(&mut self) -> Result<()> {
        // 自身 ObjectInfo 在 obj_ref 入口已 push（索引 = 当前最后一个）；
        // 后续子对象（Uniform 等）会改变 last，DefineList 等自身信息按此索引回写
        let ss_index = self.objects.len() - 1;
        if self.user()? {
            self.modes_block()?; // ModeList
        }
        if self.user()? {
            self.attrs_block()?; // AttributeList
        }
        if self.user()? {
            let units = self.r.u32()?;
            self.r.u64()?;
            for _ in 0..units {
                self.modes_block()?;
            }
        }
        if self.user()? {
            let units = self.r.u32()?;
            self.r.u64()?;
            for _ in 0..units {
                self.attrs_block()?;
            }
        }
        if self.user()? {
            self.uniforms_block()?;
        }
        self.r.i32()?; // RenderingHint
        self.r.u32()?; // RenderBinMode
        self.r.i32()?; // BinNumber
        self.r.string_auto()?; // BinName
        self.r.u8()?; // NestRenderBins
        self.opt_obj()?; // UpdateCallback
        self.opt_obj()?; // EventCallback
        if self.version >= 151 && self.user()? {
            self.defines_block(ss_index)?;
        }
        Ok(())
    }

    /// UniformList（`StateSet.cpp::readUniformList` 推导；**仅源码推导，
    /// 真实语料未出现，待真实语料验证**）。
    ///
    /// `[u32 n][u64 "{" 块长占位][条目×n]`（readSize + 无条件 BEGIN_BRACKET；
    /// `"}"` 与 PROPERTY("Value") 在二进制模式 0 字节）。每条目 =
    /// `osg::Uniform` 通用对象帧 + `[i32 OVERRIDE value]`。
    fn uniforms_block(&mut self) -> Result<()> {
        let n = self.r.u32()?;
        self.r.u64()?; // BEGIN_BRACKET（readUniformList 无条件读取）
        for _ in 0..n {
            self.obj_ref()?; // readObjectOfType<osg::Uniform>()
            self.r.i32()?; // readValue：StateAttribute::Values（二进制 int）
        }
        Ok(())
    }

    /// DefineList（`StateSet.cpp::readDefineList` 推导，v151+；**仅源码推导，
    /// 真实语料未出现，待真实语料验证**）。
    ///
    /// `[u32 n][u64 "{" 块长占位][条目×n]`；每条目 =
    /// readWrappedString(name)、readWrappedString(value)、
    /// `[i32 OVERRIDE value]`（二进制下 wrapped string 与普通 string
    /// 同为 `[u32 长度][字节]`）。
    fn defines_block(&mut self, ss_index: usize) -> Result<()> {
        let n = self.r.u32()?;
        self.r.u64()?; // BEGIN_BRACKET（readDefineList 无条件读取）
        for _ in 0..n {
            let name = self.r.string_auto()?;
            let value = self.r.string_auto()?;
            self.r.i32()?; // readValue：OVERRIDE 位
            self.note_on(ss_index, format!("DefineList: {name}={value}"));
        }
        Ok(())
    }

    /// `osg::Uniform` 字段（`Uniform.cpp` wrapper 推导；**仅源码推导，
    /// 真实语料未出现，待真实语料验证**）。
    ///
    /// Type（BEGIN_ENUM_SERIALIZER3，二进制 4 字节 int）、NumElements(u32)、
    /// Elements（USER：`[u8 ok]`；ok 时 `[u8 hasArray]`；hasArray 时为
    /// 旧版 `OutputStream::writeArray` 编码）、UpdateCallback/EventCallback。
    fn f_uniform(&mut self, name: String) -> Result<()> {
        let ty = self.r.u32()?; // Type（枚举 = GL 常量，如 0x8B52 FLOAT_VEC4）
        let num_elements = self.r.u32()?; // NumElements
        let mut values = "无数组".to_string();
        if self.user()? {
            // Elements user serializer
            if self.r.u8()? != 0 {
                self.read_legacy_array(ty)?;
                values = "旧版数组（读取后丢弃）".to_string();
            } else {
                values = "空值".to_string();
            }
        }
        self.note(format!(
            "Uniform name={name} type={} elements={num_elements} values={values}",
            uniform_type_name(ty)
        ));
        self.opt_obj()?; // UpdateCallback
        self.opt_obj()?; // EventCallback
        Ok(())
    }

    /// 旧版数组编码（`OutputStream::writeArray` / `InputStream::readArray`
    /// 推导，Uniform 值数组经此写出；**仅源码推导，待真实语料验证**）。
    ///
    /// `[u32 ArrayID]`（PROPERTY("ArrayID") 二进制 0 字节）——ID 已见过的
    /// 共享数组到此为止；否则 `[u32 ArrayType]`（DataTypes.h ID_* 常量）+
    /// `[i32 size][u64 "{" 块长占位][size × 分量数 × 分量字节]`（括号与
    /// `writeArrayImplementation` 均无条件写出；`"}"` 0 字节）。
    /// 值内容读取后丢弃（记录于 notes），只保证流对齐。
    fn read_legacy_array(&mut self, uniform_type: u32) -> Result<()> {
        let id = self.r.u32()?;
        if self.legacy_arrays.contains(&id) {
            return Ok(()); // 共享数组：仅写 ArrayID
        }
        let ty = self.r.u32()?;
        let (comps, width) = legacy_array_layout(ty).ok_or_else(|| OsgbError::BadField {
            class: "osg::Uniform".into(),
            offset: self.r.pos(),
            message: format!(
                "Uniform 值数组类型 ID={ty} 不在旧版 writeArray 已知清单中（Uniform Type={uniform_type:#06x}）"
            ),
        })?;
        let size = self.r.i32()?;
        self.r.u64()?; // BEGIN_BRACKET（writeArrayImplementation 无条件写出）
        if size > 0 {
            self.r.skip(size as usize * comps * width)?;
        }
        self.legacy_arrays.insert(id);
        self.note(format!(
            "Uniform 值数组（旧版 writeArray 编码，ArrayID={id} TypeID={ty}，{size} 元素）读取后丢弃"
        ));
        Ok(())
    }

    fn f_stateattr(&mut self) -> Result<()> {
        self.opt_obj()?; // UpdateCallback
        self.opt_obj()?; // EventCallback
        Ok(())
    }

    fn f_material(&mut self) -> Result<()> {
        self.r.u32()?; // ColorMode（GL 常量枚举，如 0x1603）
        // Ambient / Diffuse / Specular / Emission：Front+Back 无条件成对写入
        for _ in 0..4 {
            if self.user()? {
                self.r.u8()?; // frontAndBack 标志
                for _ in 0..8 {
                    self.r.f32()?; // Front RGBA + Back RGBA
                }
            }
        }
        if self.user()? {
            // Shininess：frontAndBack 标志 + Front + Back
            self.r.u8()?;
            self.r.f32()?;
            self.r.f32()?;
        }
        Ok(())
    }

    fn f_texture(&mut self) -> Result<()> {
        for _ in 0..5 {
            // WRAP_S/T/R、MIN_FILTER、MAG_FILTER（USER 属性：u8 ok + u32 GLenum）
            if self.user()? {
                self.r.u32()?;
            }
        }
        self.r.f32()?; // MaxAnisotropy
        self.r.u8()?; // UseHardwareMipMapGeneration
        self.r.u8()?; // UnRefImageDataAfterApply
        self.r.u8()?; // ClientStorageHint
        self.r.u8()?; // ResizeNonPowerOfTwoHint
        for _ in 0..4 {
            self.r.f64()?; // BorderColor (Vec4d)
        }
        self.r.i32()?; // BorderWidth
        self.r.u32()?; // InternalFormatMode
        for _ in 0..3 {
            if self.user()? {
                self.r.u32()?; // InternalFormat / SourceFormat / SourceType
            }
        }
        self.r.u8()?; // ShadowComparison
        self.r.u32()?; // ShadowCompareFunc
        self.r.u32()?; // ShadowTextureMode
        self.r.f32()?; // ShadowAmbient
        if self.user()? {
            self.r.string_auto()?; // Swizzle（4 字符字符串，如 "RGBA"）
        }
        if self.version >= 155 {
            self.r.f32()?; // MinLOD
            self.r.f32()?; // MaxLOD
            self.r.f32()?; // LODBias
        }
        Ok(())
    }

    fn f_texture2d(&mut self) -> Result<()> {
        // Image（ImageSerializer：u8 has + readImage 特化读取）
        if self.r.u8()? != 0 {
            self.read_image()?;
        }
        self.r.i32()?; // TextureWidth
        self.r.i32()?; // TextureHeight
        Ok(())
    }

    /// Image 特化读取（InputStream::readImage，非通用对象帧）。
    fn read_image(&mut self) -> Result<()> {
        self.r.string_auto()?; // ClassName（v94+）
        let uid = self.r.u32()?; // UniqueID
        if self.ids.contains_key(&uid) {
            // 重复 Image 引用：仅 ClassName + UniqueID，FileName 起全不读
            return Ok(());
        }
        let fname = self.r.string_auto()?; // FileName
        self.r.i32()?; // WriteHint
        let decision = self.r.i32()?;
        let mut inline: Option<InlineTexture> = None;
        match decision {
            0 => {
                // IMAGE_INLINE_DATA：完整像素数据（原始像素 + 布局元信息，
                // 忠实保留进 InlineTexture::RawPixels）
                let origin = self.r.i32()?;
                let s = self.r.i32()?;
                let t = self.r.i32()?;
                self.r.i32()?; // r
                self.r.i32()?; // internalTextureFormat
                let pixel_format = self.r.i32()? as u32;
                let data_type = self.r.i32()? as u32;
                self.r.i32()?; // packing
                self.r.i32()?; // allocationMode
                let size = self.r.u32()? as usize;
                let mut data = vec![0u8; size];
                self.r.take_bytes(&mut data)?;
                let levels = self.r.u32()?;
                for _ in 0..levels {
                    self.r.u32()?; // mipmap 层偏移
                }
                inline = Some(InlineTexture::RawPixels(RawImage {
                    origin,
                    s,
                    t,
                    pixel_format,
                    data_type,
                    data,
                }));
            }
            1 => {
                // IMAGE_INLINE_FILE：整文件字节（如 PNG/JPEG 编码文件，
                // 忠实保留进 InlineTexture::EncodedFile）
                let size = self.r.u32()? as usize;
                let mut data = vec![0u8; size];
                self.r.take_bytes(&mut data)?;
                inline = Some(InlineTexture::EncodedFile(data));
            }
            2 => {} // IMAGE_EXTERNAL：无数据
            other => {
                return Err(OsgbError::BadField {
                    class: "osg::Image".into(),
                    offset: self.r.pos(),
                    message: format!("未知 Image 写出模式 decision={other}"),
                });
            }
        }
        // osg::Object associate（readImage 末尾 readObjectFields("osg::Object")）
        self.f_object()?;
        self.ids.insert(uid, "osg::Image".to_string());
        // 纹理绑定：StateSet → Texture2D → 本 Image，作用于当前 Geometry 网格
        if self.cur_tex.is_none() {
            self.cur_tex = Some(TexBinding { name: fname, inline });
        } else {
            self.note("多纹理单元：仅保留第一个 Texture2D 的图像引用".into());
        }
        Ok(())
    }

    fn f_bufferdata(&mut self) -> Result<()> {
        self.opt_obj()?; // BufferObject
        Ok(())
    }

    // ---- 分发表 ----

    fn fields(&mut self, name: &str, end: usize, start: usize) -> Result<()> {
        match name {
            "osg::Group" => {
                self.f_object()?;
                self.f_node()?;
                self.f_group()?;
            }
            "osg::Geode" => {
                self.f_object()?;
                self.f_node()?;
                self.f_group()?; // Geode 的 Children 与 Group 同构（USER Drawables）
            }
            "osg::LOD" => {
                self.f_object()?;
                self.f_node()?;
                self.f_group()?;
                self.f_lod()?;
            }
            "osg::PagedLOD" => {
                self.f_object()?;
                self.f_node()?;
                self.f_group()?;
                self.f_lod()?;
                self.f_pagedlod()?;
            }
            "osg::Drawable" => {
                self.f_object()?;
                self.f_node()?;
                self.f_drawable()?;
            }
            "osg::Geometry" => {
                self.f_object()?;
                self.f_node()?;
                self.f_drawable()?;
                let tex = self.cur_tex.clone();
                self.cur = Some(Mesh {
                    texture: tex.as_ref().map(|t| t.name.clone()),
                    texture_inline: tex.and_then(|t| t.inline),
                    ..Mesh::default()
                });
                self.f_geometry()?;
                if let Some(mut mesh) = self.cur.take() {
                    // BATCHID：feature id = 本文件内第几个 osg::Geometry
                    //（每 Geometry 恰好产出并压入一个 Mesh，序号即 meshes.len()）
                    let feature_id = self.meshes.len() as u32;
                    mesh.batch_ids = Some(vec![feature_id; mesh.vertices.len()]);
                    self.meshes.push(mesh);
                }
            }
            "osg::StateSet" => {
                self.f_object()?;
                self.f_stateset()?;
            }
            "osg::StateAttribute" => {
                self.f_object()?;
                self.f_stateattr()?;
            }
            "osg::Material" => {
                self.f_object()?;
                self.f_stateattr()?;
                self.f_material()?;
            }
            "osg::Texture" => {
                self.f_object()?;
                self.f_stateattr()?;
                self.f_texture()?;
            }
            "osg::Texture2D" => {
                self.f_object()?;
                self.f_stateattr()?;
                self.f_texture()?;
                self.f_texture2d()?;
            }
            "osg::Uniform" => {
                // Name 先读（记入 notes），其余按 associates 链
                let name = self.r.string_auto()?; // Name
                self.r.i32()?; // DataVariance
                self.opt_obj()?; // UserDataContainer
                self.f_uniform(name)?;
            }
            "osg::Image" => self.read_image()?,
            "osg::DrawArrays" => {
                self.f_object()?;
                let mode = self.f_primitive_set()?;
                let first = self.r.i32()?;
                let count = self.r.i32()?;
                if mode == MODE_TRIANGLES && count > 0 {
                    if let Some(mesh) = self.cur.as_mut() {
                        mesh.indices
                            .extend((first..first + count).map(|i| i as u32));
                    } else {
                        self.note("DrawArrays 出现在 Geometry 之外，索引被丢弃".into());
                    }
                } else {
                    self.note(format!(
                        "DrawArrays mode={mode}（非 TRIANGLES）不支持三角化，{count} 个顶点被丢弃"
                    ));
                }
            }
            "osg::DrawArrayLengths" => {
                self.f_object()?;
                let mode = self.f_primitive_set()?;
                let n = self.r.u32()?;
                let mut total = 0usize;
                for _ in 0..n {
                    total += self.r.i32()?.max(0) as usize;
                }
                self.note(format!(
                    "DrawArrayLengths mode={mode}（多段图元）不支持三角化，共 {total} 个顶点被丢弃"
                ));
            }
            "osg::VertexBufferObject" | "osg::ElementBufferObject" => {
                // associates 不含 BufferData：Target / Usage / CopyData
                self.f_object()?;
                self.r.u32()?; // Target（GLenum）
                self.r.u32()?; // Usage（GLenum）
                self.r.u8()?; // CopyDataAndReleaseGLBufferObject
            }
            cls if cls.starts_with("osg::DrawElements") => {
                self.f_object()?;
                let mode = self.f_primitive_set()?;
                let count = self.r.u32()? as usize;
                let width = match cls {
                    "osg::DrawElementsUByte" => 1,
                    "osg::DrawElementsUShort" => 2,
                    "osg::DrawElementsUInt" => 4,
                    _ => {
                        return Err(OsgbError::BadField {
                            class: cls.to_string(),
                            offset: start,
                            message: format!("不支持的 DrawElements 类型 {cls}"),
                        });
                    }
                };
                let mut indices = Vec::with_capacity(count);
                for _ in 0..count {
                    indices.push(match width {
                        1 => u32::from(self.r.u8()?),
                        2 => u32::from(self.r.u16()?),
                        _ => self.r.u32()?,
                    });
                }
                if let Some(mesh) = self.cur.as_mut() {
                    mesh.indices.extend(indices);
                } else {
                    self.note(format!(
                        "{cls}（mode={mode}）出现在 Geometry 之外，{count} 个索引被丢弃"
                    ));
                }
            }
            cls if cls.starts_with("osg::") && cls.ends_with("Array") => {
                self.read_array(cls)?;
            }
            other => {
                self.skip_fields(other, end)?;
            }
        }
        Ok(())
    }

    /// PrimitiveSet 公共字段：BufferObject + NumInstances + Mode。返回 Mode。
    fn f_primitive_set(&mut self) -> Result<u32> {
        self.f_bufferdata()?;
        self.r.i32()?; // NumInstances
        self.r.u32() // Mode（GLenum 图元模式）→ 返回值即 Result<u32>
    }

    /// 数组类：Object + BufferData(BufferObject) + Binding/Normalize/Preserve
    /// + `[u32 n][元素×n]`；按当前槽位路由到网格。
    fn read_array(&mut self, cls: &str) -> Result<()> {
        self.f_object()?;
        self.f_bufferdata()?;
        self.r.u32()?; // Binding
        self.r.u8()?; // Normalize
        self.r.u8()?; // PreserveDataType
        let count = self.r.u32()? as usize;

        // 元素分量数与标量类型（小端；OSG Vec*b*Array = 有符号字节）
        let (comps, kind): (usize, u8) = match cls {
            "osg::Vec2Array" => (2, b'f'),
            "osg::Vec3Array" => (3, b'f'),
            "osg::Vec4Array" => (4, b'f'),
            "osg::Vec2dArray" => (2, b'd'),
            "osg::Vec3dArray" => (3, b'd'),
            "osg::Vec4dArray" => (4, b'd'),
            "osg::FloatArray" => (1, b'f'),
            "osg::DoubleArray" => (1, b'd'),
            "osg::Vec2iArray" => (2, b'i'),
            "osg::Vec3iArray" => (3, b'i'),
            "osg::Vec4iArray" => (4, b'i'),
            "osg::Vec2uiArray" => (2, b'i'),
            "osg::Vec3uiArray" => (3, b'i'),
            "osg::Vec4uiArray" => (4, b'i'),
            "osg::Vec2sArray" => (2, b's'),
            "osg::Vec3sArray" => (3, b's'),
            "osg::Vec4sArray" => (4, b's'),
            "osg::Vec2usArray" => (2, b's'),
            "osg::Vec3usArray" => (3, b's'),
            "osg::Vec4usArray" => (4, b's'),
            "osg::Vec2bArray" => (2, b'b'),
            "osg::Vec3bArray" => (3, b'b'),
            "osg::Vec4bArray" => (4, b'b'),
            "osg::Vec2ubArray" => (2, b'b'),
            "osg::Vec3ubArray" => (3, b'b'),
            "osg::Vec4ubArray" => (4, b'b'),
            "osg::ByteArray" | "osg::UByteArray" => (1, b'b'),
            "osg::ShortArray" | "osg::UShortArray" => (1, b's'),
            "osg::IntArray" | "osg::UIntArray" => (1, b'i'),
            other => {
                return Err(OsgbError::BadField {
                    class: other.to_string(),
                    offset: self.r.pos(),
                    message: "不支持的数组类型".into(),
                });
            }
        };

        // 读取元素（f32 数组可能进入网格，其余读完即弃）
        let mut f32s: Vec<[f32; 3]> = Vec::new();
        let mut f32uv: Vec<[f32; 2]> = Vec::new();
        let want = match self.slot {
            Slot::Vertex | Slot::Normal => 1,
            Slot::TexCoord(0) => 2,
            _ => 0,
        };
        for _ in 0..count {
            match kind {
                b'f' => {
                    if want == 1 && comps >= 3 {
                        f32s.push([self.r.f32()?, self.r.f32()?, self.r.f32()?]);
                    } else if want == 2 && comps == 2 {
                        f32uv.push([self.r.f32()?, self.r.f32()?]);
                    } else {
                        for _ in 0..comps {
                            self.r.f32()?;
                        }
                    }
                }
                b'd' => {
                    for _ in 0..comps {
                        self.r.f64()?;
                    }
                }
                b'i' => {
                    for _ in 0..comps {
                        self.r.i32()?;
                    }
                }
                b's' => {
                    for _ in 0..comps {
                        self.r.u16()?;
                    }
                }
                _ => {
                    for _ in 0..comps {
                        self.r.u8()?;
                    }
                }
            }
        }

        // 槽位路由（读取已完成，路由失败不影响流对齐）
        if let Some(mesh) = self.cur.as_mut() {
            match self.slot {
                Slot::Vertex => {
                    if f32s.is_empty() {
                        if count > 0 {
                            self.note(format!("顶点数组 {cls} 分量数不符（{comps} 分量），{count} 元素被丢弃"));
                        }
                    } else if mesh.vertices.is_empty() {
                        mesh.vertices = f32s;
                    } else {
                        self.note("多个顶点数组，后续数组被丢弃".into());
                    }
                }
                Slot::Normal => {
                    if f32s.is_empty() {
                        if count > 0 {
                            self.note(format!("法线数组 {cls} 分量数不符（{comps} 分量），{count} 元素被丢弃"));
                        }
                    } else if mesh.normals.is_none() {
                        mesh.normals = Some(f32s);
                    } else {
                        self.note("多个法线数组，后续数组被丢弃".into());
                    }
                }
                Slot::TexCoord(0) => {
                    if f32uv.is_empty() {
                        if count > 0 {
                            self.note(format!("UV 数组 {cls} 不是 2 分量 f32（{comps} 分量），{count} 元素被丢弃"));
                        }
                    } else if mesh.uvs.is_none() {
                        mesh.uvs = Some(f32uv);
                    } else {
                        self.note("多个 UV 数组，后续数组被丢弃".into());
                    }
                }
                Slot::Color => self.note(format!("Color 数组（{cls}×{count}）读取后丢弃：输出模型无颜色通道")),
                Slot::SecondaryColor => self.note(format!("SecondaryColor 数组（{cls}×{count}）读取后丢弃")),
                Slot::FogCoord => self.note(format!("FogCoord 数组（{cls}×{count}）读取后丢弃")),
                Slot::TexCoord(u) => self.note(format!("TexCoord unit {u}（{cls}×{count}）读取后丢弃：仅 unit 0 进入 UV")),
                Slot::VertexAttrib(u) => self.note(format!("VertexAttrib unit {u}（{cls}×{count}）读取后丢弃")),
            }
        } else if count > 0 {
            self.note(format!("{cls}（{count} 元素）出现在 Geometry 之外，数据被丢弃"));
        }
        Ok(())
    }
}

/// 旧版 `OutputStream::writeArray` 的 ArrayType → (分量数, 分量字节宽)。
///
/// 常量与布局依据 OSG 3.6.5 `include/osgDB/DataTypes.h` 的 ID_* 清单与
/// `readArrayImplementation(…, numComponentsPerElements, componentSizeInBytes)`
/// 调用表（如 ID_VEC3_ARRAY → 3 × 4 字节）。未知 ID 返回 None（明确报错）。
fn legacy_array_layout(ty: u32) -> Option<(usize, usize)> {
    Some(match ty {
        0 | 1 => (1, 1),   // ID_BYTE_ARRAY / ID_UBYTE_ARRAY
        2 | 3 => (1, 2),   // ID_SHORT_ARRAY / ID_USHORT_ARRAY
        4 | 5 => (1, 4),   // ID_INT_ARRAY / ID_UINT_ARRAY
        6 => (1, 4),       // ID_FLOAT_ARRAY
        7 => (1, 8),       // ID_DOUBLE_ARRAY
        8 | 21 => (2, 1),  // ID_VEC2B_ARRAY / ID_VEC2UB_ARRAY
        9 | 22 => (3, 1),  // ID_VEC3B_ARRAY / ID_VEC3UB_ARRAY
        10 | 11 => (4, 1), // ID_VEC4B_ARRAY / ID_VEC4UB_ARRAY
        12 | 23 => (2, 2), // ID_VEC2S_ARRAY / ID_VEC2US_ARRAY
        13 | 24 => (3, 2), // ID_VEC3S_ARRAY / ID_VEC3US_ARRAY
        14 | 25 => (4, 2), // ID_VEC4S_ARRAY / ID_VEC4US_ARRAY
        15..=17 => (ty as usize - 13, 4), // ID_VEC2/3/4_ARRAY
        18..=20 => (ty as usize - 16, 8), // ID_VEC2/3/4D_ARRAY
        26..=28 => (ty as usize - 24, 4), // ID_VEC2/3/4I_ARRAY
        29..=31 => (ty as usize - 27, 4), // ID_VEC2/3/4UI_ARRAY
        32 | 33 => (1, 8), // ID_UINT64_ARRAY / ID_INT64_ARRAY
        _ => return None,
    })
}

/// `osg::Uniform::Type` 枚举值 → 名称（GL uniform 类型常量；
/// 依据 OSG `include/osg/Uniform` 的 Type 定义 = GL 常量）。未知值给十六进制。
fn uniform_type_name(ty: u32) -> String {
    let (name, known): (&str, bool) = match ty {
        0x1406 => ("FLOAT", true),
        0x8b50 => ("FLOAT_VEC2", true),
        0x8b51 => ("FLOAT_VEC3", true),
        0x8b52 => ("FLOAT_VEC4", true),
        0x140a => ("DOUBLE", true),
        0x8ffc => ("DOUBLE_VEC2", true),
        0x8ffd => ("DOUBLE_VEC3", true),
        0x8ffe => ("DOUBLE_VEC4", true),
        0x1404 => ("INT", true),
        0x8b53 => ("INT_VEC2", true),
        0x8b54 => ("INT_VEC3", true),
        0x8b55 => ("INT_VEC4", true),
        0x1405 => ("UNSIGNED_INT", true),
        0x8dc6 => ("UNSIGNED_INT_VEC2", true),
        0x8dc7 => ("UNSIGNED_INT_VEC3", true),
        0x8dc8 => ("UNSIGNED_INT_VEC4", true),
        0x8b56 => ("BOOL", true),
        0x8b57 => ("BOOL_VEC2", true),
        0x8b58 => ("BOOL_VEC3", true),
        0x8b59 => ("BOOL_VEC4", true),
        0x8b5a => ("FLOAT_MAT2", true),
        0x8b5b => ("FLOAT_MAT3", true),
        0x8b5c => ("FLOAT_MAT4", true),
        0x8b5e => ("SAMPLER_2D", true),
        0x8b60 => ("SAMPLER_CUBE", true),
        _ => ("", false),
    };
    if known {
        name.to_string()
    } else {
        format!("0x{ty:04x}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 手工字节帧构造器（按 parser.rs 记录的帧布局写测试语料）。
    mod frames {
        /// `[u32 类名长][类名][u64 blocksize][u32 uid][字段]`
        pub fn frame(name: &str, uid: u32, fields: &[u8]) -> Vec<u8> {
            let nb = name.as_bytes();
            let mut buf = Vec::new();
            buf.extend_from_slice(&(nb.len() as u32).to_le_bytes());
            buf.extend_from_slice(nb);
            buf.extend_from_slice(&((8 + 4 + fields.len()) as u64).to_le_bytes());
            buf.extend_from_slice(&uid.to_le_bytes());
            buf.extend_from_slice(fields);
            buf
        }

        pub fn u8(v: u8, buf: &mut Vec<u8>) {
            buf.extend_from_slice(&[v]);
        }

        pub fn u32(v: u32, buf: &mut Vec<u8>) {
            buf.extend_from_slice(&v.to_le_bytes());
        }

        pub fn i32(v: i32, buf: &mut Vec<u8>) {
            buf.extend_from_slice(&v.to_le_bytes());
        }

        pub fn f32(v: f32, buf: &mut Vec<u8>) {
            buf.extend_from_slice(&v.to_le_bytes());
        }

        pub fn f64(v: f64, buf: &mut Vec<u8>) {
            buf.extend_from_slice(&v.to_le_bytes());
        }

        /// `[u32 长度][UTF-8 字节]`
        pub fn string(s: &str, buf: &mut Vec<u8>) {
            u32(s.len() as u32, buf);
            buf.extend_from_slice(s.as_bytes());
        }

        /// osg::Object associates 字段：Name + DataVariance + UserDataContainer
        pub fn object_fields(name: &str, buf: &mut Vec<u8>) {
            string(name, buf);
            i32(0, buf); // DataVariance
            u8(0, buf); // UserDataContainer = NULL
        }

        /// osg::Node associates 字段（全部缺省：无 bound/回调；StateSet 槽
        /// 可选附帧）
        pub fn node_fields(stateset: Option<&[u8]>, buf: &mut Vec<u8>) {
            u8(0, buf); // InitialBound USER 标志
            u8(0, buf); // ComputeBoundingSphereCallback
            u8(0, buf); // UpdateCallback
            u8(0, buf); // EventCallback
            u8(0, buf); // CullCallback
            u8(1, buf); // CullingActive（v145+）
            u32(0, buf); // NodeMask（v142+）
            match stateset {
                Some(frame) => {
                    u8(1, buf);
                    buf.extend_from_slice(frame);
                }
                None => u8(0, buf), // StateSet = NULL
            }
        }

        /// USER Children 前缀：`[u8 ok=1][u32 n][u64 括号占位]`
        pub fn children_prefix(n: u32, buf: &mut Vec<u8>) {
            u8(1, buf);
            u32(n, buf);
            buf.extend_from_slice(&0u64.to_le_bytes());
        }
    }

    /// 25 字节无压缩文件头（与 header.rs 真实语料逐字节一致）。
    fn header() -> Vec<u8> {
        let mut h = Vec::new();
        h.extend_from_slice(&0x6c91_0ea1u32.to_le_bytes());
        h.extend_from_slice(&0x1afb_4545u32.to_le_bytes());
        h.extend_from_slice(&1u32.to_le_bytes()); // WRITE_SCENE
        h.extend_from_slice(&161u32.to_le_bytes()); // version
        h.extend_from_slice(&4u32.to_le_bytes()); // robust brackets
        h.extend_from_slice(&1u32.to_le_bytes());
        h.push(b'0'); // compressorName = "0"
        h
    }

    #[test]
    fn frame_reads_null_and_sizes() {
        // [u32=4]"NULL" → None；正常帧 end = u64 位置 + blocksize
        let mut buf = Vec::new();
        buf.extend_from_slice(&4u32.to_le_bytes());
        buf.extend_from_slice(b"NULL");
        let frame_bytes = buf.clone();
        buf.extend_from_slice(&[0u8; 4]);

        let mut p = Parser {
            r: Reader::new(&buf),
            version: 161,
            ids: HashMap::new(),
            meshes: Vec::new(),
            objects: Vec::new(),
            skipped_count: 0,
            cur: None,
            cur_tex: None,
            legacy_arrays: std::collections::HashSet::new(),
            slot: Slot::Vertex,
            depth: 0,
        };
        assert!(p.frame().unwrap().is_none());
        p.r = Reader::new(&frame_bytes);
        assert!(p.frame().unwrap().is_none());
    }

    #[test]
    fn slot_constants_match_gl() {
        assert_eq!(MODE_TRIANGLES, 4);
    }

    #[test]
    fn legacy_array_layout_covers_data_types_table() {
        // 与 osgDB/DataTypes.h ID_* 常量对照抽查
        assert_eq!(legacy_array_layout(6), Some((1, 4))); // FLOAT
        assert_eq!(legacy_array_layout(7), Some((1, 8))); // DOUBLE
        assert_eq!(legacy_array_layout(15), Some((2, 4))); // VEC2
        assert_eq!(legacy_array_layout(16), Some((3, 4))); // VEC3
        assert_eq!(legacy_array_layout(17), Some((4, 4))); // VEC4
        assert_eq!(legacy_array_layout(19), Some((3, 8))); // VEC3D
        assert_eq!(legacy_array_layout(31), Some((4, 4))); // VEC4UI
        assert_eq!(legacy_array_layout(50), None); // DrawArrays 不在此通道
    }

    #[test]
    fn uniform_type_names_match_gl_constants() {
        assert_eq!(uniform_type_name(0x1406), "FLOAT");
        assert_eq!(uniform_type_name(0x8b52), "FLOAT_VEC4");
        assert_eq!(uniform_type_name(0x8b5e), "SAMPLER_2D");
        assert_eq!(uniform_type_name(0x1234), "0x1234");
    }

    /// 构造「Geode→(StateSet→Texture2D→Image)→Geometry」最小语料，
    /// Image 按 INLINE_DATA（decision=0）写入 2×2 RGBA 原始像素。
    fn inline_corpus(decision: i32) -> Vec<u8> {
        use frames as f;

        let mut data = header();

        // Image 特化帧体（readImage 顺序）
        let mut image = Vec::new();
        f::string("osg::Image", &mut image); // ClassName
        f::u32(8, &mut image); // UniqueID
        f::string("inline-tex.png", &mut image); // FileName
        f::i32(0, &mut image); // WriteHint
        f::i32(decision, &mut image);
        let pixels: Vec<u8> = (0..16).collect();
        match decision {
            0 => {
                f::i32(0, &mut image); // origin BOTTOM_LEFT
                f::i32(2, &mut image); // s
                f::i32(2, &mut image); // t
                f::i32(1, &mut image); // r
                f::i32(0x1908, &mut image); // internalTextureFormat GL_RGBA
                f::i32(0x1908, &mut image); // pixelFormat
                f::i32(0x1401, &mut image); // dataType GL_UNSIGNED_BYTE
                f::i32(4, &mut image); // packing
                f::i32(1, &mut image); // allocationMode
                f::u32(pixels.len() as u32, &mut image); // data size
                image.extend_from_slice(&pixels); // 2×2 RGBA
                f::u32(0, &mut image); // mipmap levels = 0（仅 INLINE_DATA 有）
            }
            // INLINE_FILE：size + 整文件字节，无 mipmap levels（与
            // OutputStream::writeImage 的 INLINE_FILE 分支一致）
            _ => {
                let file = [0x89u8, b'P', b'N', b'G', 0x0d, 0x0a, 0x1a, 0x0a];
                f::u32(file.len() as u32, &mut image);
                image.extend_from_slice(&file);
            }
        }
        f::object_fields("", &mut image); // osg::Object associates
        // 注意：Image 经 ImageSerializer 特化读取，不带通用对象帧头
        let image_frame = image;

        // Texture2D：ImageSerializer 槽 + 特化字段
        let mut tex2d = Vec::new();
        f::object_fields("", &mut tex2d);
        f::u8(0, &mut tex2d); // UpdateCallback
        f::u8(0, &mut tex2d); // EventCallback
        for _ in 0..5 {
            f::u8(0, &mut tex2d); // WRAP_S/T/R、MIN/MAG FILTER：USER 缺省
        }
        f::f32(1.0, &mut tex2d); // MaxAnisotropy
        f::u8(0, &mut tex2d); // UseHardwareMipMapGeneration
        f::u8(0, &mut tex2d); // UnRefImageDataAfterApply
        f::u8(0, &mut tex2d); // ClientStorageHint
        f::u8(0, &mut tex2d); // ResizeNonPowerOfTwoHint
        for _ in 0..4 {
            f::f64(0.0, &mut tex2d); // BorderColor
        }
        f::i32(0, &mut tex2d); // BorderWidth
        f::u32(0, &mut tex2d); // InternalFormatMode
        for _ in 0..3 {
            f::u8(0, &mut tex2d); // InternalFormat/SourceFormat/SourceType：USER 缺省
        }
        f::u8(0, &mut tex2d); // ShadowComparison
        f::u32(0, &mut tex2d); // ShadowCompareFunc
        f::u32(0, &mut tex2d); // ShadowTextureMode
        f::f32(0.0, &mut tex2d); // ShadowAmbient
        f::u8(0, &mut tex2d); // Swizzle：USER 缺省
        f::f32(0.0, &mut tex2d); // MinLOD（v155+）
        f::f32(0.0, &mut tex2d); // MaxLOD
        f::f32(0.0, &mut tex2d); // LODBias
        f::u8(1, &mut tex2d); // Image 槽：有
        tex2d.extend_from_slice(&image_frame);
        f::i32(0, &mut tex2d); // TextureWidth
        f::i32(0, &mut tex2d); // TextureHeight
        let tex2d_frame = f::frame("osg::Texture2D", 3, &tex2d);

        // StateSet：TextureAttributeList 单元 0 挂 Texture2D；Uniform/Define 空
        let mut stateset = Vec::new();
        f::object_fields("", &mut stateset);
        f::u8(1, &mut stateset); // ModeList USER
        f::u32(0, &mut stateset); // 空
        f::u8(1, &mut stateset); // AttributeList USER
        f::u32(0, &mut stateset); // 空
        f::u8(1, &mut stateset); // TextureModeList USER
        f::u32(0, &mut stateset); // 单元数 0
        stateset.extend_from_slice(&0u64.to_le_bytes());
        f::u8(1, &mut stateset); // TextureAttributeList USER
        f::u32(1, &mut stateset); // 单元数 1
        stateset.extend_from_slice(&0u64.to_le_bytes());
        f::u32(1, &mut stateset); // 单元 0 属性数
        stateset.extend_from_slice(&0u64.to_le_bytes());
        stateset.extend_from_slice(&tex2d_frame);
        f::i32(0, &mut stateset); // OVERRIDE value
        f::u8(1, &mut stateset); // UniformList USER
        f::u32(0, &mut stateset); // 空
        stateset.extend_from_slice(&0u64.to_le_bytes());
        f::i32(0, &mut stateset); // RenderingHint
        f::u32(0, &mut stateset); // RenderBinMode
        f::i32(0, &mut stateset); // BinNumber
        f::string("", &mut stateset); // BinName
        f::u8(0, &mut stateset); // NestRenderBins
        f::u8(0, &mut stateset); // UpdateCallback
        f::u8(0, &mut stateset); // EventCallback
        f::u8(1, &mut stateset); // DefineList USER（v151+）
        f::u32(0, &mut stateset); // 空
        stateset.extend_from_slice(&0u64.to_le_bytes());
        let stateset_frame = f::frame("osg::StateSet", 2, &stateset);

        // Geometry：StateSet 槽（纹理绑定）+ 顶点 + UV + DrawElementsUShort
        let mut geometry = Vec::new();
        f::object_fields("", &mut geometry);
        f::node_fields(Some(&stateset_frame), &mut geometry);
        f::u8(0, &mut geometry); // InitialBound USER
        f::u8(0, &mut geometry); // ComputeBoundingBoxCallback
        f::u8(0, &mut geometry); // Shape
        f::u8(0, &mut geometry); // SupportsDisplayList
        f::u8(0, &mut geometry); // UseDisplayList
        f::u8(1, &mut geometry); // UseVertexBufferObjects
        f::u32(0, &mut geometry); // NodeMask（v142+，Drawable 侧）
        f::u8(1, &mut geometry); // CullingActive（v145+，Drawable 侧）
        f::u32(1, &mut geometry); // PrimitiveSetList 数
        let mut de = Vec::new();
        f::object_fields("", &mut de);
        f::u8(0, &mut de); // BufferObject
        f::i32(0, &mut de); // NumInstances
        f::u32(4, &mut de); // Mode = TRIANGLES
        f::u32(3, &mut de); // 索引数
        de.extend_from_slice(&[0u8, 0, 1, 0, 2, 0]); // u16 [0,1,2]
        geometry.extend_from_slice(&f::frame("osg::DrawElementsUShort", 7, &de));
        // 固定槽位：Vertex 有、Normal 有、其余 NULL
        let mut vec3 = Vec::new();
        f::object_fields("", &mut vec3);
        f::u8(0, &mut vec3); // BufferObject
        f::u32(0, &mut vec3); // Binding
        f::u8(0, &mut vec3); // Normalize
        f::u8(0, &mut vec3); // PreserveDataType
        f::u32(3, &mut vec3);
        for v in 0..3u32 {
            f::f32(v as f32, &mut vec3);
            f::f32(0.0, &mut vec3);
            f::f32(0.0, &mut vec3);
        }
        let vec3_frame = f::frame("osg::Vec3Array", 5, &vec3);
        f::u8(1, &mut geometry);
        geometry.extend_from_slice(&vec3_frame); // Vertex
        f::u8(0, &mut geometry); // Normal
        f::u8(0, &mut geometry); // Color
        f::u8(0, &mut geometry); // SecondaryColor
        f::u8(0, &mut geometry); // FogCoord
        f::u32(1, &mut geometry); // TexCoordArrayList 单元数
        let mut vec2 = Vec::new();
        f::object_fields("", &mut vec2);
        f::u8(0, &mut vec2);
        f::u32(0, &mut vec2);
        f::u8(0, &mut vec2);
        f::u8(0, &mut vec2);
        f::u32(3, &mut vec2);
        for _ in 0..3 {
            f::f32(0.5, &mut vec2);
            f::f32(0.5, &mut vec2);
        }
        geometry.extend_from_slice(&f::frame("osg::Vec2Array", 6, &vec2));
        f::u32(0, &mut geometry); // VertexAttribArrayList 单元数
        let geometry_frame = f::frame("osg::Geometry", 4, &geometry);

        // Geode：子 = [Geometry]（真实语料结构：StateSet 挂在 Geometry 节点上）
        let mut geode = Vec::new();
        f::object_fields("", &mut geode);
        f::node_fields(None, &mut geode);
        f::children_prefix(1, &mut geode);
        geode.extend_from_slice(&geometry_frame);
        data.extend_from_slice(&f::frame("osg::Geode", 1, &geode));
        data
    }

    #[test]
    fn parses_handbuilt_inline_data_image_into_mesh() {
        let data = inline_corpus(0);
        let scene = parse_bytes(&data).expect("INLINE_DATA 手工语料应可解析");
        assert_eq!(scene.meshes.len(), 1);
        let mesh = &scene.meshes[0];
        // 纹理绑定：FileName + INLINE_DATA 原始像素保留
        assert_eq!(mesh.texture.as_deref(), Some("inline-tex.png"));
        match &mesh.texture_inline {
            Some(InlineTexture::RawPixels(raw)) => {
                assert_eq!(raw.origin, 0);
                assert_eq!(raw.s, 2);
                assert_eq!(raw.t, 2);
                assert_eq!(raw.pixel_format, 0x1908);
                assert_eq!(raw.data_type, 0x1401);
                assert_eq!(raw.data, (0..16).collect::<Vec<u8>>());
            }
            other => panic!("应为 RawPixels，实际 {other:?}"),
        }
        // BATCHID：单 Geometry = feature 0
        assert_eq!(mesh.batch_ids.as_ref().unwrap(), &[0, 0, 0]);
        assert_eq!(mesh.vertices.len(), 3);
        assert_eq!(mesh.indices, vec![0, 1, 2]);
        assert_eq!(mesh.uvs.as_ref().unwrap().len(), 3);
    }

    #[test]
    fn parses_handbuilt_inline_file_image_into_mesh() {
        let data = inline_corpus(1);
        let scene = parse_bytes(&data).expect("INLINE_FILE 手工语料应可解析");
        let mesh = &scene.meshes[0];
        assert_eq!(mesh.texture.as_deref(), Some("inline-tex.png"));
        match &mesh.texture_inline {
            Some(InlineTexture::EncodedFile(bytes)) => {
                assert_eq!(bytes, &[0x89, b'P', b'N', b'G', 0x0d, 0x0a, 0x1a, 0x0a]);
            }
            other => panic!("应为 EncodedFile，实际 {other:?}"),
        }
    }

    #[test]
    fn parses_handbuilt_uniform_list_and_define_list() {
        use frames as f;

        // osg::Uniform（FLOAT_VEC4 × 1，无值数组）
        let mut uniform = Vec::new();
        f::object_fields("uColor", &mut uniform);
        f::u32(0x8b52, &mut uniform); // Type = FLOAT_VEC4
        f::u32(1, &mut uniform); // NumElements
        f::u8(1, &mut uniform); // Elements USER：ok
        f::u8(0, &mut uniform); // hasArray = false
        f::u8(0, &mut uniform); // UpdateCallback
        f::u8(0, &mut uniform); // EventCallback
        let uniform_frame = f::frame("osg::Uniform", 10, &uniform);

        let mut stateset = Vec::new();
        f::object_fields("", &mut stateset);
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset); // ModeList 空
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset); // AttributeList 空
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset);
        stateset.extend_from_slice(&0u64.to_le_bytes()); // TextureModeList 空
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset);
        stateset.extend_from_slice(&0u64.to_le_bytes()); // TextureAttributeList 空
        f::u8(1, &mut stateset); // UniformList USER
        f::u32(1, &mut stateset); // 1 个 uniform
        stateset.extend_from_slice(&0u64.to_le_bytes()); // "{" 占位
        stateset.extend_from_slice(&uniform_frame);
        f::i32(2, &mut stateset); // OVERRIDE value
        f::i32(0, &mut stateset); // RenderingHint
        f::u32(0, &mut stateset); // RenderBinMode
        f::i32(0, &mut stateset); // BinNumber
        f::string("", &mut stateset); // BinName
        f::u8(0, &mut stateset); // NestRenderBins
        f::u8(0, &mut stateset); // UpdateCallback
        f::u8(0, &mut stateset); // EventCallback
        f::u8(1, &mut stateset); // DefineList USER
        f::u32(1, &mut stateset); // 1 条 define
        stateset.extend_from_slice(&0u64.to_le_bytes());
        f::string("USE_FOG", &mut stateset);
        f::string("1", &mut stateset);
        f::i32(2, &mut stateset); // OVERRIDE
        let stateset_frame = f::frame("osg::StateSet", 2, &stateset);

        let mut data = header();
        data.extend_from_slice(&stateset_frame);
        let (scene, objects) = parse_bytes_traced(&data).expect("Uniform/Define 手工语料应可解析");
        assert!(scene.meshes.is_empty());

        let ss = objects.iter().find(|o| o.class == "osg::StateSet").unwrap();
        assert!(
            ss.notes.iter().any(|n| n.contains("USE_FOG=1")),
            "DefineList 应记入 notes: {:?}",
            ss.notes
        );
        let u = objects.iter().find(|o| o.class == "osg::Uniform").unwrap();
        assert!(
            u.notes.iter().any(|n| n.contains("name=uColor")
                && n.contains("FLOAT_VEC4")
                && n.contains("空值")),
            "Uniform 应记入 notes: {:?}",
            u.notes
        );
    }

    #[test]
    fn parses_handbuilt_uniform_with_legacy_value_array() {
        use frames as f;

        // Uniform FLOAT × 2，值走旧版 writeArray（FloatArray，ArrayID=7）
        let mut uniform = Vec::new();
        f::object_fields("uScale", &mut uniform);
        f::u32(0x1406, &mut uniform); // FLOAT
        f::u32(2, &mut uniform); // NumElements
        f::u8(1, &mut uniform); // Elements USER ok
        f::u8(1, &mut uniform); // hasArray = true
        f::u32(7, &mut uniform); // ArrayID
        f::u32(6, &mut uniform); // ArrayType = ID_FLOAT_ARRAY
        f::i32(2, &mut uniform); // size
        uniform.extend_from_slice(&0u64.to_le_bytes()); // "{" 占位
        uniform.extend_from_slice(&1.5f32.to_le_bytes());
        uniform.extend_from_slice(&2.5f32.to_le_bytes());
        f::u8(0, &mut uniform); // UpdateCallback
        f::u8(0, &mut uniform); // EventCallback
        let uniform_frame = f::frame("osg::Uniform", 10, &uniform);

        let mut stateset = Vec::new();
        f::object_fields("", &mut stateset);
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset); // ModeList 空
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset); // AttributeList 空
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset);
        stateset.extend_from_slice(&0u64.to_le_bytes()); // TextureModeList 空
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset);
        stateset.extend_from_slice(&0u64.to_le_bytes()); // TextureAttributeList 空
        f::u8(1, &mut stateset); // UniformList USER
        f::u32(1, &mut stateset);
        stateset.extend_from_slice(&0u64.to_le_bytes());
        stateset.extend_from_slice(&uniform_frame);
        f::i32(0, &mut stateset); // OVERRIDE value
        f::i32(0, &mut stateset); // RenderingHint
        f::u32(0, &mut stateset); // RenderBinMode
        f::i32(0, &mut stateset); // BinNumber
        f::string("", &mut stateset); // BinName
        f::u8(0, &mut stateset); // NestRenderBins
        f::u8(0, &mut stateset); // UpdateCallback
        f::u8(0, &mut stateset); // EventCallback
        f::u8(0, &mut stateset); // DefineList USER：无
        let stateset_frame = f::frame("osg::StateSet", 2, &stateset);

        let mut data = header();
        data.extend_from_slice(&stateset_frame);
        let (_, objects) = parse_bytes_traced(&data).expect("旧版数组 Uniform 应可解析");
        let u = objects.iter().find(|o| o.class == "osg::Uniform").unwrap();
        assert!(
            u.notes.iter().any(|n| n.contains("uScale")
                && n.contains("FLOAT")
                && n.contains("读取后丢弃")),
            "Uniform 值数组应记入 notes: {:?}",
            u.notes
        );
    }

    #[test]
    fn rejects_uniform_with_unknown_legacy_array_type() {
        use frames as f;

        let mut uniform = Vec::new();
        f::object_fields("uBad", &mut uniform);
        f::u32(0x1406, &mut uniform);
        f::u32(1, &mut uniform);
        f::u8(1, &mut uniform); // Elements ok
        f::u8(1, &mut uniform); // hasArray
        f::u32(7, &mut uniform); // ArrayID
        f::u32(54, &mut uniform); // ArrayType = ID_DRAWELEMENTS_UINT（不在支持清单）
        let uniform_frame = f::frame("osg::Uniform", 10, &uniform);

        let mut stateset = Vec::new();
        f::object_fields("", &mut stateset);
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset);
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset);
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset);
        stateset.extend_from_slice(&0u64.to_le_bytes());
        f::u8(1, &mut stateset);
        f::u32(0, &mut stateset);
        stateset.extend_from_slice(&0u64.to_le_bytes());
        f::u8(1, &mut stateset); // UniformList
        f::u32(1, &mut stateset);
        stateset.extend_from_slice(&0u64.to_le_bytes());
        stateset.extend_from_slice(&uniform_frame);
        f::i32(0, &mut stateset);
        f::i32(0, &mut stateset);
        f::u32(0, &mut stateset);
        f::i32(0, &mut stateset);
        f::string("", &mut stateset);
        f::u8(0, &mut stateset);
        f::u8(0, &mut stateset);
        f::u8(0, &mut stateset);
        f::u8(0, &mut stateset);
        let stateset_frame = f::frame("osg::StateSet", 2, &stateset);

        let mut data = header();
        data.extend_from_slice(&stateset_frame);
        let err = parse_bytes(&data).unwrap_err();
        assert!(
            err.to_string().contains("不在旧版 writeArray 已知清单"),
            "未知旧版数组类型应明确报错: {err}"
        );
    }

    #[test]
    fn rejects_unknown_image_write_mode() {
        let data = inline_corpus(7); // 未知 decision
        let err = parse_bytes(&data).unwrap_err();
        assert!(
            err.to_string().contains("未知 Image 写出模式"),
            "{err}"
        );
    }

}

