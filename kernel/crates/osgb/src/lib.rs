//! TanGIS OSGB 读取器：真实 OSG 3.x 二进制序列化协议（无 C++ 依赖）。
//!
//! 解析 osgconv 3.6.5 产出的 `.osgb`（版本 161，robust 二进制格式），
//! 输出三角形网格（顶点/索引/可选法线与 UV + 纹理文件引用），供 build
//! 生成 b3dm/3D Tiles。协议逆向依据真实语料 `testdata/osgb-real/osgb/`
//! 字节流 + OSG 3.6 源码对照，由 `testdata/osgb-real/verify_protocol.py`
//! 对全部 80 个 tile 做字节级对齐验证。
//!
//! # 支持范围（详见 [`parser`] 模块文档）
//!
//! - 头部：64 位魔数 / 写出类型 / 版本 / 属性位 / 压缩器名（见 [`Header`]）；
//! - 压缩流：compressorName 非 `"0"` 时为 zlib deflate，解压后同一协议继续；
//! - 对象帧：`[u32 类名][u64 blocksize][u32 UniqueID][字段]`，robust 括号
//!   支持未知类整体跳过（skipped 如实记录）；
//! - **UniqueID 去重**：对象实例重复出现时帧内无字段区（OSG 写出端的
//!   引用压缩机制），按 UniqueID 复用；
//! - 节点：`Group` / `Geode` / `LOD` / `PagedLOD`；
//! - 几何：`Geometry` + `DrawElementsUByte/UShort/UInt`、`DrawArrays`
//!   （仅 TRIANGLES）、`DrawArrayLengths`（记录不支持）；
//! - 数组：`Vec2/3/4Array` 等全部 osg 数组类型（顶点/法线/UV 进入网格，
//!   颜色/雾坐标/VertexAttrib 等读取后丢弃并记录 notes）；
//! - 状态：`StateSet` / `Material` / `Texture` / `Texture2D` → `Image`
//!   （INLINE_DATA / INLINE_FILE / EXTERNAL；INLINE 两种模式的字节保留进
//!   `Mesh::texture_inline`）、UniformList / DefineList（按 OSG 3.6.5
//!   源码推导，**真实语料未出现，待真实语料验证**）、`osg::Uniform`；
//! - **不支持**（遇到即明确报错，不猜测布局）：Uniform 值数组的未知旧版
//!   ArrayType、DrawArrays 非 TRIANGLES 模式三角化、无 robust 括号的旧格式。
//!
//! # 输出
//!
//! [`Scene`] = 网格列表（每个 `osg::Geometry` 一个 [`Mesh`]，BATCHID =
//! Geometry 序号），可用 [`Scene::merged`] 合并为单网格（纹理引用/内嵌
//! 字节冲突时明确报错）。

mod error;
mod header;
mod parser;
mod reader;
mod scene;

pub use error::{OsgbError, Result};
pub use header::Header;
pub use parser::{parse_bytes, parse_bytes_traced, parse_file, ObjectInfo};
pub use reader::{write_compact_int, Reader};
pub use scene::{feature_count, InlineTexture, Mesh, RawImage, Scene};
