//! 解析输出模型：场景 = 网格列表；网格 = 顶点/索引/可选法线与 UV +
//! 可选纹理内嵌字节 + 每顶点 BATCHID。

/// 源 OSGB Image 的内嵌纹理（`OutputStream::writeImage` 的 INLINE 写出模式），
/// 按原始字节忠实保留，供上层（GLB 写出端）优先于文件路径使用。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum InlineTexture {
    /// IMAGE_INLINE_FILE（decision=1）：纹理编码文件整字节（如 PNG/JPEG），
    /// 可直接嵌入 GLB（mimeType 由上层按字节签名判定）。
    EncodedFile(Vec<u8>),
    /// IMAGE_INLINE_DATA（decision=0）：未编码原始像素 + 布局元信息
    /// （GLB 不能直接嵌入原始像素，上层需编码为 PNG 等）。
    RawPixels(RawImage),
}

/// INLINE_DATA 原始像素布局（字段来自 `InputStream::readImage`）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RawImage {
    /// 行序：0 = BOTTOM_LEFT（底行为第 0 行，编码 PNG 时需翻转）、1 = TOP_LEFT。
    pub origin: i32,
    /// 宽（像素）。
    pub s: i32,
    /// 高（像素）。
    pub t: i32,
    /// GL 像素格式（如 0x1907 GL_RGB、0x1908 GL_RGBA）。
    pub pixel_format: u32,
    /// GL 数据类型（如 0x1401 GL_UNSIGNED_BYTE）。
    pub data_type: u32,
    /// 原始像素字节（s×t×分量×分量字节，packing 对齐由调用方校验）。
    pub data: Vec<u8>,
}

/// 单个网格（对应一个 `osg::Geometry`，或多个网格合并后的结果）。
#[derive(Debug, Clone, PartialEq, Default)]
pub struct Mesh {
    /// 顶点位置（局部坐标）。
    pub vertices: Vec<[f32; 3]>,
    /// 三角形索引（顶点数组下标，mode = TRIANGLES）。
    pub indices: Vec<u32>,
    /// 顶点法线（与顶点一一对应，语料中来自第二个 `osg::Vec3Array`）。
    pub normals: Option<Vec<[f32; 3]>>,
    /// 顶点 UV（与顶点一一对应，来自 `osg::Vec2Array`）。
    pub uvs: Option<Vec<[f32; 2]>>,
    /// 绑定的纹理文件引用（来自 Geode 级 StateSet → Texture2D → Image 的
    /// 文件名字段）。相对 OSGB 文件所在目录解析。
    pub texture: Option<String>,
    /// 绑定纹理的内嵌字节（INLINE 写出模式），与 `texture` 同源，
    /// 存在时优先于文件路径（见 [`InlineTexture`]）。
    pub texture_inline: Option<InlineTexture>,
    /// 每顶点 BATCHID：feature id = 源文件内第几个 `osg::Geometry`
    /// （与顶点一一对应；来自解析器，手工构造的网格可为 None，
    /// 上层按「整网格单 feature」处理）。
    pub batch_ids: Option<Vec<u32>>,
}

/// 网格的 feature（BATCHID）数量：batch_ids 最大值 + 1；无 batch_ids 视为
/// 单 feature。与 b3dm 写出端的 BATCH_LENGTH 计算严格一致。
pub fn feature_count(mesh: &Mesh) -> u32 {
    match &mesh.batch_ids {
        Some(ids) => ids.iter().copied().max().map_or(1, |m| m + 1),
        None => 1,
    }
}

impl Mesh {
    /// 平移所有顶点（用于把 OSGB 局部坐标搬到分块位置）。
    pub fn translate(&mut self, offset: [f64; 3]) {
        for v in &mut self.vertices {
            v[0] += offset[0] as f32;
            v[1] += offset[1] as f32;
            v[2] += offset[2] as f32;
        }
    }

    /// 按真实数据计算轴对齐包围盒（空网格返回 None）。
    pub fn bounds(&self) -> Option<([f32; 3], [f32; 3])> {
        let mut it = self.vertices.iter();
        let first = *it.next()?;
        let mut min = first;
        let mut max = first;
        for v in it {
            for a in 0..3 {
                min[a] = min[a].min(v[a]);
                max[a] = max[a].max(v[a]);
            }
        }
        Some((min, max))
    }
}

/// 一个 OSGB 场景：多个 Geometry 的网格列表。
#[derive(Debug, Clone, PartialEq, Default)]
pub struct Scene {
    pub meshes: Vec<Mesh>,
}

impl Scene {
    /// 合并全部网格为一个（索引做顶点偏移拼接），供 b3dm 单 primitive 输出。
    ///
    /// 纹理规则（不静默降级）：所有网格纹理引用一致时保留；引用冲突或
    /// 部分有部分无时返回 Err —— 混合纹理无法安全合并为单 primitive。
    /// 内嵌纹理字节：引用一致的前提下，任一网格带内嵌字节即保留
    /// （同一 Image 实例经 UniqueID 复用时后续网格只有引用没有字节）；
    /// 内嵌字节互相冲突返回 Err。BATCHID 直接拼接（解析器按 Geometry
    /// 顺序分配，文件内全局唯一）；任一网格带 BATCHID 则全部必须带。
    pub fn merged(&self) -> Result<Mesh, String> {
        let texture = merged_texture(&self.meshes)?;
        let texture_inline = merged_texture_inline(&self.meshes)?;
        let batch_ids = merged_batch_ids(&self.meshes)?;
        if self.meshes.len() == 1 {
            let mut m = self.meshes[0].clone();
            m.texture = texture;
            m.texture_inline = texture_inline;
            m.batch_ids = batch_ids;
            return Ok(m);
        }
        let mut out = Mesh::default();
        for m in &self.meshes {
            let base = out.vertices.len() as u32;
            out.vertices.extend_from_slice(&m.vertices);
            out.indices.extend(m.indices.iter().map(|i| i + base));
            match (&m.normals, &mut out.normals) {
                (Some(n), Some(acc)) => acc.extend_from_slice(n),
                (Some(n), None) => out.normals = Some(n.clone()),
                _ => {}
            }
            match (&m.uvs, &mut out.uvs) {
                (Some(u), Some(acc)) => acc.extend_from_slice(u),
                (Some(u), None) => out.uvs = Some(u.clone()),
                _ => {}
            }
            if let Some(ids) = &m.batch_ids {
                out.batch_ids.get_or_insert_with(Vec::new).extend_from_slice(ids);
            }
        }
        out.texture = texture;
        out.texture_inline = texture_inline;
        out.batch_ids = batch_ids;
        Ok(out)
    }
}

/// 合并前内嵌纹理一致性检查：任一带内嵌即统一取第一份；互相冲突报错。
fn merged_texture_inline(meshes: &[Mesh]) -> Result<Option<InlineTexture>, String> {
    let mut first: Option<&InlineTexture> = None;
    for m in meshes {
        if let Some(t) = &m.texture_inline {
            match first {
                None => first = Some(t),
                Some(f) if f == t => {}
                Some(_) => return Err("网格内嵌纹理字节冲突，无法合并为单 primitive".to_string()),
            }
        }
    }
    Ok(first.cloned())
}

/// 合并前 BATCHID 一致性检查：全部带或全部不带，混合报错。
fn merged_batch_ids(meshes: &[Mesh]) -> Result<Option<Vec<u32>>, String> {
    let any = meshes.iter().any(|m| m.batch_ids.is_some());
    let all = meshes.iter().all(|m| m.batch_ids.is_some());
    if any && !all {
        return Err("网格 BATCHID 不一致：部分带部分缺失，禁止静默合并".to_string());
    }
    if !any {
        return Ok(None);
    }
    let mut ids = Vec::new();
    for m in meshes {
        let got = m.batch_ids.as_ref().expect("上面已断言全部 Some");
        if got.len() != m.vertices.len() {
            return Err(format!(
                "网格 BATCHID 数与顶点数不符：顶点 {} / BATCHID {}",
                m.vertices.len(),
                got.len()
            ));
        }
        ids.extend_from_slice(got);
    }
    Ok(Some(ids))
}

/// 合并前的纹理一致性检查：全部一致取之，冲突/部分缺失报错。
fn merged_texture(meshes: &[Mesh]) -> Result<Option<String>, String> {
    let mut first: Option<&str> = None;
    for m in meshes {
        match (&m.texture, first) {
            (None, None) => {}
            (Some(t), None) => first = Some(t),
            (Some(t), Some(f)) if t == f => {}
            (Some(t), Some(f)) => {
                return Err(format!(
                    "网格纹理引用冲突：`{f}` 与 `{t}`，无法合并为单 primitive（禁止静默丢弃纹理）"
                ));
            }
            (None, Some(f)) => {
                return Err(format!(
                    "网格纹理引用不一致：部分网格绑定 `{f}`、部分无纹理，禁止静默丢弃纹理"
                ));
            }
        }
    }
    Ok(first.map(|s| s.to_string()))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample(id: u32) -> Mesh {
        Mesh {
            vertices: vec![[id as f32, 0.0, 0.0], [id as f32, 1.0, 0.0], [id as f32, 0.0, 1.0]],
            indices: vec![0, 1, 2],
            normals: Some(vec![[0.0, 0.0, 1.0]; 3]),
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: Some(vec![id, id, id]),
        }
    }

    #[test]
    fn merged_offsets_indices() {
        let scene = Scene { meshes: vec![sample(0), sample(1)] };
        let m = scene.merged().unwrap();
        assert_eq!(m.vertices.len(), 6);
        assert_eq!(m.indices, vec![0, 1, 2, 3, 4, 5]);
        assert_eq!(m.normals.as_ref().unwrap().len(), 6);
        // BATCHID 拼接保留（解析器分配的 id 文件内全局唯一）
        assert_eq!(m.batch_ids.as_ref().unwrap(), &[0, 0, 0, 1, 1, 1]);
    }

    #[test]
    fn merged_texture_consistency() {
        // 全部一致 → 保留
        let mut a = sample(0);
        a.texture = Some("atlas.png".into());
        let mut b = sample(1);
        b.texture = Some("atlas.png".into());
        let scene = Scene { meshes: vec![a.clone(), b] };
        assert_eq!(scene.merged().unwrap().texture.as_deref(), Some("atlas.png"));

        // 单网格直接透传
        let scene = Scene { meshes: vec![a.clone()] };
        assert_eq!(scene.merged().unwrap().texture.as_deref(), Some("atlas.png"));

        // 冲突 → Err
        let mut c = sample(1);
        c.texture = Some("other.png".into());
        let scene = Scene { meshes: vec![a.clone(), c] };
        let err = scene.merged().unwrap_err();
        assert!(err.contains("冲突"), "{err}");

        // 部分缺失 → Err（禁止静默丢纹理）
        let d = sample(1);
        let scene = Scene { meshes: vec![a, d] };
        let err = scene.merged().unwrap_err();
        assert!(err.contains("不一致"), "{err}");
    }

    #[test]
    fn merged_inline_texture_dedup_and_conflict() {
        // UniqueID 复用场景：第二个网格只有引用没有内嵌字节 → 取第一份字节
        let mut a = sample(0);
        a.texture = Some("atlas.png".into());
        a.texture_inline = Some(InlineTexture::EncodedFile(vec![1, 2, 3]));
        let mut b = sample(1);
        b.texture = Some("atlas.png".into());
        let merged = Scene { meshes: vec![a.clone(), b] }.merged().unwrap();
        assert_eq!(merged.texture_inline, a.texture_inline);

        // 内嵌字节互相冲突 → Err
        let mut c = sample(1);
        c.texture = Some("atlas.png".into());
        c.texture_inline = Some(InlineTexture::EncodedFile(vec![9]));
        let err = Scene { meshes: vec![a, c] }.merged().unwrap_err();
        assert!(err.contains("冲突"), "{err}");
    }

    #[test]
    fn merged_batch_ids_consistency() {
        // 全部带 → 拼接；数量与顶点不符 → Err；混合 → Err
        let mut a = sample(0);
        a.batch_ids = Some(vec![0, 0]); // 与 3 顶点不符
        let b = sample(1);
        let err = Scene { meshes: vec![a, b] }.merged().unwrap_err();
        assert!(err.contains("不符"), "{err}");

        let a = Mesh { batch_ids: Some(vec![0; 3]), ..sample(0) };
        let b = Mesh { batch_ids: None, ..sample(1) };
        let err = Scene { meshes: vec![a, b] }.merged().unwrap_err();
        assert!(err.contains("BATCHID 不一致"), "{err}");

        // 全部不带 → None（合法）
        let a = Mesh { batch_ids: None, ..sample(0) };
        let b = Mesh { batch_ids: None, ..sample(1) };
        assert!(Scene { meshes: vec![a, b] }.merged().unwrap().batch_ids.is_none());
    }

    #[test]
    fn feature_count_matches_batch_ids() {
        assert_eq!(feature_count(&sample(0)), 1);
        let mut m = sample(2);
        m.batch_ids = Some(vec![0, 1, 1]);
        assert_eq!(feature_count(&m), 2);
        m.batch_ids = None;
        assert_eq!(feature_count(&m), 1);
    }

    #[test]
    fn bounds_from_real_data() {
        let m = sample(5);
        let (min, max) = m.bounds().unwrap();
        assert_eq!(min, [5.0, 0.0, 0.0]);
        assert_eq!(max, [5.0, 1.0, 1.0]);
        assert!(Mesh::default().bounds().is_none());
    }

    #[test]
    fn translate_moves_vertices() {
        let mut m = sample(0);
        m.translate([10.0, 20.0, 30.0]);
        assert_eq!(m.vertices[1], [10.0, 21.0, 30.0]);
    }
}
