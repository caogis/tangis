//! 真实几何加载：`.obj`（tobj 解析）与 `.osgb`（tangis-osgb 解析）→ 统一网格。
//!
//! # 坐标管线（统一内部约定：Z-up）
//!
//! 内部 [`Mesh`] 统一为 **Z-up**（3D Tiles tile 局部系：x/y 水平、z = 高度，
//! 右手系），与 manifest `Bounds` 及 tileset boundingVolume box（同为 z-up）
//! 保持一致：
//!
//! 1. OSGB：语料本身即 Z-up（仓库生成器写入时已按 OBJ (x,y,z)→(x,−z,y)
//!    转换，见 `crates/osgb/tests/corpus.rs` 头注释），parser 裸读即为内部
//!    约定，不再变换；
//! 2. OBJ：语料源文件是 **Y-up**（实测 y ∈ [−2.38, 21.78] 为高度、z 为
//!    水平），读入时按 (x,y,z)→(x,−z,y) 旋转到 Z-up——该式为绕 X 轴的
//!    旋转（右手系，行列式 +1），且与语料生成器 OBJ→OSGB 所用变换完全
//!    相同，保证同一 tile 的 OBJ 与 OSGB 加载结果逐点一致；
//! 3. GLB 写出（`tiles/b3dm.rs`）：Z-up → glTF 规范要求的 Y-up，
//!    (x,y,z)→(x,z,−y)（上式的逆旋转）。
//!
//! 来源解析优先级（分块级）：
//! 1. `chunk.source` 显式文件路径（相对路径基于 manifest 所在目录）；
//! 2. `--src` 目录（CLI 参数，优先级高于 manifest.source）下按分块 id 匹配
//!    `<chunk_id>.osgb` / `<chunk_id>.obj`；
//! 3. `manifest.source` 目录同上；
//! 4. 无自身来源的父分块：合并其子分块的网格（LOD 聚合语义）。
//!
//! 全部来源解析失败时返回明确错误 —— 内核**禁止**静默降级产出占位几何。
//!
//! # 纹理
//!
//! OSGB 侧：网格绑定的纹理引用（`Mesh::texture`，OSGB 内 Image 文件名，
//! 相对 OSGB 所在目录）在加载时解析为绝对路径并回写 `Mesh::texture`；
//! 绑定纹理但缺 UV / UV 数不符 / 纹理不可用（无内嵌字节且文件路径读不到）
//! 时**明确报错**并列出缺失清单（禁止静默产出无纹理几何）。
//! 纹理字节来源优先级：**内嵌优先**——`Mesh::texture_inline`（OSGB Image
//! 的 INLINE_DATA 原始像素 / INLINE_FILE 编码文件字节）存在时直接使用
//! （RawPixels 编码为 PNG；EncodedFile 按字节签名判定 mimeType），
//! 无内嵌再走文件路径；[`load_texture_image`] 是唯一出口。
//! OBJ 侧仍不支持材质/UV（语料无 mtl/vt）。
//!
//! # Batch（feature）与溯源
//!
//! OSGB 每个 osg::Geometry = 一个 feature（`Mesh::batch_ids`，解析器分配）。
//! [`resolve_chunk_mesh`] 为单个分块返回 [`ResolvedMesh`]：网格 + 与 feature
//! 一一对应的来源文件名（batch table `_tile` 列）。父分块合并子分块时，
//! `merge_meshes` 对 BATCHID 做偏移重排（子 feature 在父内保持唯一），
//! feature_sources 顺序拼接——两者用同一套 `feature_count` 计数保证对齐。

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use tangis_osgb::{InlineTexture, Mesh};

/// 每分块的解析结果：网格 + per-feature 来源标注。
#[derive(Debug, Clone)]
pub struct ResolvedMesh {
    pub mesh: Mesh,
    /// 与 mesh 的 feature（BATCHID）一一对应的来源文件名
    /// （b3dm batch table `_tile` 列；长度 = feature_count(mesh)）。
    pub feature_sources: Vec<String>,
}

/// 已可嵌入 GLB 的纹理图像（内嵌直通或文件读取的结果）。
#[derive(Debug)]
pub struct LoadedTexture {
    pub bytes: Vec<u8>,
    /// glTF mimeType。
    pub mime_type: &'static str,
}

/// GL 常量（osgb INLINE_DATA 像素布局判定用）。
const GL_RGB: u32 = 0x1907;
const GL_RGBA: u32 = 0x1908;
const GL_UNSIGNED_BYTE: u32 = 0x1401;

/// 按字节签名判定图像 mimeType（PNG / JPEG；其余明确报错，不猜）。
pub fn sniff_image_mime(bytes: &[u8]) -> Result<&'static str, String> {
    if bytes.starts_with(&[0x89, b'P', b'N', b'G', 0x0d, 0x0a, 0x1a, 0x0a]) {
        return Ok("image/png");
    }
    if bytes.starts_with(&[0xff, 0xd8, 0xff]) {
        return Ok("image/jpeg");
    }
    Err(format!(
        "内嵌纹理字节既非 PNG 也非 JPEG 签名（前 {} 字节：{:02x?}），禁止猜测 mimeType",
        bytes.len().min(8),
        &bytes[..bytes.len().min(8)]
    ))
}

/// INLINE_DATA 原始像素 → PNG 编码（image crate）。
/// 仅支持 GL_RGB / GL_RGBA + GL_UNSIGNED_BYTE；其余明确报错。
pub fn encode_raw_pixels_png(raw: &tangis_osgb::RawImage) -> Result<Vec<u8>, String> {
    if raw.s <= 0 || raw.t <= 0 {
        return Err(format!("INLINE 原始像素尺寸非法：{}×{}", raw.s, raw.t));
    }
    let comps: usize = match (raw.pixel_format, raw.data_type) {
        (GL_RGB, GL_UNSIGNED_BYTE) => 3,
        (GL_RGBA, GL_UNSIGNED_BYTE) => 4,
        (pf, dt) => {
            return Err(format!(
                "INLINE 原始像素布局不支持编码：pixelFormat=0x{pf:04x} dataType=0x{dt:04x}（仅 GL_RGB/GL_RGBA + GL_UNSIGNED_BYTE）"
            ));
        }
    };
    let (w, h) = (raw.s as usize, raw.t as usize);
    let expected = w * h * comps;
    if raw.data.len() != expected {
        return Err(format!(
            "INLINE 原始像素字节数不符：实际 {} / 期望 {expected}（{}×{}×{comps}）",
            raw.data.len(),
            w,
            h
        ));
    }
    // origin BOTTOM_LEFT（0）时行序自底向上，编码 PNG（自顶向下）需翻转
    let mut rgba: Vec<u8> = Vec::with_capacity(expected);
    let row_len = w * comps;
    for row in 0..h {
        let src_row = if raw.origin == 0 { h - 1 - row } else { row };
        rgba.extend_from_slice(&raw.data[src_row * row_len..(src_row + 1) * row_len]);
    }
    let mut out = Vec::new();
    let img = if comps == 4 {
        image::RgbaImage::from_raw(w as u32, h as u32, rgba).map(image::DynamicImage::ImageRgba8)
    } else {
        image::RgbImage::from_raw(w as u32, h as u32, rgba).map(image::DynamicImage::ImageRgb8)
    };
    let img = img.ok_or_else(|| "INLINE 原始像素缓冲构造图像失败".to_string())?;
    img.write_to(&mut std::io::Cursor::new(&mut out), image::ImageFormat::Png)
        .map_err(|e| format!("INLINE 原始像素 PNG 编码失败: {e}"))?;
    Ok(out)
}

/// 网格纹理 → GLB 嵌入字节：**内嵌优先**，无内嵌走文件路径；
/// 有纹理绑定但两者都不可用 → Err（禁止静默产出无纹理几何）。
/// 无纹理绑定 → Ok(None)。
pub fn load_texture_image(mesh: &Mesh) -> Result<Option<LoadedTexture>, String> {
    if mesh.texture.is_none() {
        return Ok(None);
    }
    match &mesh.texture_inline {
        Some(InlineTexture::EncodedFile(bytes)) => {
            let mime_type = sniff_image_mime(bytes)?;
            Ok(Some(LoadedTexture { bytes: bytes.clone(), mime_type }))
        }
        Some(InlineTexture::RawPixels(raw)) => Ok(Some(LoadedTexture {
            bytes: encode_raw_pixels_png(raw)?,
            mime_type: "image/png",
        })),
        None => {
            let path = mesh.texture.as_ref().expect("上面已判 None");
            if path.is_empty() {
                return Err(
                    "网格绑定纹理但无内嵌字节且文件名为空，禁止静默产出无纹理几何"
                        .to_string(),
                );
            }
            let bytes = std::fs::read(path)
                .map_err(|e| format!("无法读纹理 {path}: {e}"))?;
            let mime_type = texture_mime(Path::new(path))?;
            Ok(Some(LoadedTexture { bytes, mime_type }))
        }
    }
}

/// 纹理文件扩展名 → glTF mimeType；不支持的扩展名明确报错（不猜类型）。
pub fn texture_mime(path: &Path) -> Result<&'static str, String> {
    match path
        .extension()
        .and_then(|e| e.to_str())
        .map(|e| e.to_ascii_lowercase())
        .as_deref()
    {
        Some("png") => Ok("image/png"),
        Some("jpg") | Some("jpeg") => Ok("image/jpeg"),
        other => Err(format!(
            "纹理 {} 扩展名 `{}` 不支持（仅 .png / .jpg / .jpeg），禁止猜测 mimeType",
            path.display(),
            other.unwrap_or("<无扩展名>")
        )),
    }
}

/// Y-up OBJ 源 → 内部 Z-up 的轴变换：(x, y, z) → (x, −z, y)。
///
/// 绕 X 轴旋转（右手系，保持手性/三角形绕向）。供合成 y-up OBJ 语料使用；
/// 真实语料 OBJ 为 z-up，走 [`load_obj_mesh`] 时同样适用（见各测试的语料说明）。
fn z_up(v: [f32; 3]) -> [f32; 3] {
    [v[0], -v[2], v[1]]
}

/// 真实 OSGB（GL y-up）→ 内部 Z-up：(x, y, z) → (x, z, −y)。
///
/// 真实语料字节级验证：osgconv 写出的 OSGB 顶点 = OBJ 源 (x,y,z)→(x,−z,y)
/// （z-up 源转 GL y-up），本式为其逆变换，恢复出 OBJ 的 z-up 原始坐标。
fn osgb_to_z_up(v: [f32; 3]) -> [f32; 3] {
    [v[0], v[2], -v[1]]
}

/// 按扩展名加载单个几何源文件为合并后的网格。
pub fn load_mesh(path: &Path) -> Result<Mesh, String> {
    let ext = path
        .extension()
        .and_then(|e| e.to_str())
        .map(|e| e.to_ascii_lowercase())
        .unwrap_or_default();
    match ext.as_str() {
        "obj" => load_obj_mesh(path),
        "osgb" => {
            let scene = tangis_osgb::parse_file(path)
                .map_err(|e| format!("解析 OSGB {} 失败: {e}", path.display()))?;
            let mut mesh = scene
                .merged()
                .map_err(|e| format!("OSGB {} 网格合并失败: {e}", path.display()))?;
            // 真实 OSGB 为 GL y-up（数据 = z-up 源经 (x,y,z)→(x,−z,y) 写出），
            // 用其逆变换恢复内部 Z-up
            for v in &mut mesh.vertices {
                *v = osgb_to_z_up(*v);
            }
            if let Some(ns) = &mut mesh.normals {
                for n in ns.iter_mut() {
                    *n = osgb_to_z_up(*n);
                }
            }
            if mesh.vertices.is_empty() || mesh.indices.is_empty() {
                return Err(format!("OSGB {} 不含可用几何", path.display()));
            }
            resolve_osgb_texture(&mut mesh, path)?;
            Ok(mesh)
        }
        other => Err(format!(
            "不支持的几何源扩展名 `.{other}`（{}）：仅支持 .obj / .osgb",
            path.display()
        )),
    }
}

/// 解析 OSGB 网格的纹理引用为绝对路径并回写 `mesh.texture`。
///
/// - 引用相对 OSGB 文件所在目录解析；
/// - 绑定纹理但缺 UV / UV 数不符 → Err；
/// - **有内嵌字节（INLINE）时跳过文件存在性检查**（字节直通优先）；
/// - 无内嵌字节时文件必须存在，否则 → Err（缺失清单一次性列出）。
pub fn resolve_osgb_texture(mesh: &mut Mesh, osgb_path: &Path) -> Result<(), String> {
    let Some(reference) = mesh.texture.clone() else {
        return Ok(());
    };
    let mut problems: Vec<String> = Vec::new();
    match &mesh.uvs {
        Some(uvs) if uvs.len() == mesh.vertices.len() => {}
        Some(uvs) => problems.push(format!(
            "UV 数不符：顶点 {} / UV {}",
            mesh.vertices.len(),
            uvs.len()
        )),
        None => problems.push("缺少 UV 数组（TEXCOORD_0）".to_string()),
    }
    let mut resolved = reference.clone();
    if mesh.texture_inline.is_none() {
        // 无内嵌字节：文件路径必须可满足
        let abs = osgb_path
            .parent()
            .unwrap_or_else(|| Path::new("."))
            .join(&reference);
        if !abs.is_file() {
            problems.push(format!("纹理文件不存在: {}", abs.display()));
        }
        resolved = abs.to_string_lossy().into_owned();
    } else if !reference.is_empty() {
        // 有内嵌字节：路径仅作溯源记录，不要求文件存在
        let abs = osgb_path
            .parent()
            .unwrap_or_else(|| Path::new("."))
            .join(&reference);
        resolved = abs.to_string_lossy().into_owned();
    }
    if !problems.is_empty() {
        return Err(format!(
            "OSGB {} 的纹理引用 `{}` 无法满足：\n  - {}",
            osgb_path.display(),
            reference,
            problems.join("\n  - ")
        ));
    }
    mesh.texture = Some(resolved);
    Ok(())
}

/// 解析 OBJ 为网格（Y-up 源 → Z-up 内部约定）。
///
/// - 顶点/索引用 tobj 解析，faces 统一三角化（mode = TRIANGLES）；
/// - 顶点与法线按 (x,y,z)→(x,−z,y) 从 OBJ 的 Y-up 旋转到内部 Z-up
///   （绕 X 轴旋转，右手系；与语料生成器 OBJ→OSGB 变换同式）；
/// - 法线仅在「逐顶点且数量与顶点一致」时保留，否则丢弃
///   （glTF 要求 NORMAL 数与 POSITION 数一致，缺法线由渲染端默认处理）；
/// - UV 与 mtl 材质（含 diffuse 贴图）忽略，见模块注释「纹理说明」。
fn load_obj_mesh(path: &Path) -> Result<Mesh, String> {
    let (models, _materials) = tobj::load_obj(path, &tobj::LoadOptions {
        single_index: true,
        triangulate: true,
        ..Default::default()
    })
    .map_err(|e| format!("解析 OBJ {} 失败: {e}", path.display()))?;

    let mut mesh = Mesh::default();
    for model in &models {
        let m = &model.mesh;
        let base = mesh.vertices.len() as u32;
        // single_index = true 时 positions/normals/texcoords 共用同一索引；
        // tobj 的 positions/normals 为扁平 Vec<f32>，按 xyz 三元组收拢
        mesh.vertices.extend(
            m.positions
                .chunks_exact(3)
                .map(|p| z_up([p[0], p[1], p[2]])),
        );
        mesh.indices.extend(m.indices.iter().map(|i| i + base));
        if m.normals.len() == m.positions.len() {
            let normals = m
                .normals
                .chunks_exact(3)
                .map(|n| z_up([n[0], n[1], n[2]]))
                .collect::<Vec<_>>();
            match &mut mesh.normals {
                Some(acc) => acc.extend(normals),
                None => mesh.normals = Some(normals),
            }
        } else if !m.normals.is_empty() {
            // 法线数量与顶点不一致：丢弃该 shape 的法线（宁缺勿假）
            mesh.normals = None;
        }
    }
    if mesh.vertices.is_empty() || mesh.indices.is_empty() {
        return Err(format!("OBJ {} 不含可用几何", path.display()));
    }
    Ok(mesh)
}

/// 在目录中按分块 id 匹配源文件：`<id>.osgb` 优先，其次 `<id>.obj`。
/// 在源目录中查找分块几何文件：`<chunk_id>.osgb` / `<chunk_id>.obj`
/// （osgb 优先）。支持目录递归（生产布局常见 `Data/Tile_xxx/Tile_xxx.osgb`
/// 多层嵌套）；不跟随符号链接目录（防循环）。
pub fn find_source_in_dir(dir: &Path, chunk_id: &str) -> Option<PathBuf> {
    for ext in ["osgb", "obj"] {
        let p = dir.join(format!("{chunk_id}.{ext}"));
        if p.is_file() {
            return Some(p);
        }
    }
    let mut subdirs: Vec<PathBuf> = match std::fs::read_dir(dir) {
        Ok(rd) => rd
            .filter_map(|e| e.ok())
            .map(|e| e.path())
            .filter(|p| p.is_dir() && !p.is_symlink())
            .collect(),
        Err(_) => return None,
    };
    subdirs.sort();
    for d in subdirs {
        if let Some(p) = find_source_in_dir(&d, chunk_id) {
            return Some(p);
        }
    }
    None
}

/// 合并多个网格（索引做顶点偏移拼接）。
///
/// 法线只有在「每个网格都有且数量与各自顶点一致」时才保留，否则整体丢弃
/// （glTF 要求 NORMAL 与 POSITION 数量一致，混合合并会产出非法属性）。
/// 纹理/UV 规则（不静默降级）：全部网格纹理引用一致才保留；纹理冲突、
/// 部分绑定部分未绑定、UV 部分存在部分缺失均报错。内嵌纹理字节：引用
/// 一致前提下任一带即保留（同一 Image 经 UniqueID 复用时后续网格只有
/// 引用），互相冲突报错。
/// BATCHID：无 batch_ids 的网格视为单 feature（id 0）；子网格 feature id
/// 按累计 feature 数偏移重排，保证父网格内全局唯一（与 feature_sources
/// 拼接共用同一套 [`tangis_osgb::feature_count`] 计数）。
pub fn merge_meshes(meshes: &[&Mesh]) -> Result<Mesh, String> {
    let texture = merged_texture(meshes)?;
    let texture_inline = merged_texture_inline(meshes)?;
    let all_uvs = meshes.iter().all(|m| m.uvs.is_some());
    let any_uvs = meshes.iter().any(|m| m.uvs.is_some());
    if any_uvs && !all_uvs {
        return Err(
            "网格 UV 不一致：部分带 UV 部分缺失，禁止静默丢弃 UV/纹理".to_string(),
        );
    }
    let mut out = Mesh::default();
    let all_normaled = meshes
        .iter()
        .all(|m| m.normals.as_ref().is_some_and(|n| n.len() == m.vertices.len()));
    let mut batch_ids: Vec<u32> = Vec::new();
    let mut feature_offset: u32 = 0;
    for m in meshes {
        let base = out.vertices.len() as u32;
        out.vertices.extend_from_slice(&m.vertices);
        out.indices.extend(m.indices.iter().map(|i| i + base));
        if all_normaled {
            let acc = out.normals.get_or_insert_with(Vec::new);
            acc.extend_from_slice(m.normals.as_ref().unwrap());
        }
        if all_uvs {
            let acc = out.uvs.get_or_insert_with(Vec::new);
            acc.extend_from_slice(m.uvs.as_ref().unwrap());
        }
        // BATCHID 偏移重排：子网格内 feature id + 累计 feature 数
        let ids = m.batch_ids.clone().unwrap_or_else(|| vec![0; m.vertices.len()]);
        if ids.len() != m.vertices.len() {
            return Err(format!(
                "网格 BATCHID 数与顶点数不符：顶点 {} / BATCHID {}",
                m.vertices.len(),
                ids.len()
            ));
        }
        batch_ids.extend(ids.iter().map(|id| id + feature_offset));
        feature_offset += tangis_osgb::feature_count(m);
    }
    if !all_normaled {
        out.normals = None;
    }
    out.texture = texture;
    out.texture_inline = texture_inline;
    out.batch_ids = Some(batch_ids);
    Ok(out)
}

/// 合并**同一 tile 内**的多个网格（M2-F09b：edit --format b3dm 的
/// 编辑产物合并，单 primitive b3dm 输入）。
///
/// 与 [`merge_meshes`]（父分块 LOD 聚合）的关键差异：**BATCHID 不重排、
/// 直接拼接**——解析器按源文件内 `osg::Geometry` 序号分配 feature id，
/// 文件内已全局唯一；父分块式偏移重排反而会制造空洞（feature_count =
/// max+1 与来源数失配）。重复 id（跨网格冲突）明确报错。
/// 纹理/内嵌纹理/UV/法线一致性规则与 [`merge_meshes`] 相同：
/// 纹理引用全部一致（内嵌任一带即取第一份）、UV/法线要么全有要么全无。
pub fn merge_tile_meshes(meshes: &[&Mesh]) -> Result<Mesh, String> {
    let texture = merged_texture(meshes)?;
    let texture_inline = merged_texture_inline(meshes)?;
    let all_uvs = meshes.iter().all(|m| m.uvs.is_some());
    let any_uvs = meshes.iter().any(|m| m.uvs.is_some());
    if any_uvs && !all_uvs {
        return Err("网格 UV 不一致：部分带 UV 部分缺失，禁止静默丢弃 UV/纹理".to_string());
    }
    let all_normaled = meshes
        .iter()
        .all(|m| m.normals.as_ref().is_some_and(|n| n.len() == m.vertices.len()));
    let any_normaled = meshes
        .iter()
        .any(|m| m.normals.as_ref().is_some_and(|n| n.len() == m.vertices.len()));
    if any_normaled && !all_normaled {
        return Err("网格法线不一致：部分带法线部分缺失，禁止静默混合".to_string());
    }
    let mut out = Mesh::default();
    // 已被先前网格占用的 feature id 集合（同一网格内多顶点共享同一 id
    // 是合法的；冲突只按「跨网格」判定）
    let mut seen: std::collections::BTreeSet<u32> = std::collections::BTreeSet::new();
    for m in meshes {
        let base = out.vertices.len() as u32;
        out.vertices.extend_from_slice(&m.vertices);
        out.indices.extend(m.indices.iter().map(|i| i + base));
        if all_uvs {
            let acc = out.uvs.get_or_insert_with(Vec::new);
            acc.extend_from_slice(m.uvs.as_ref().unwrap());
        }
        if all_normaled {
            let acc = out.normals.get_or_insert_with(Vec::new);
            acc.extend_from_slice(m.normals.as_ref().unwrap());
        }
        if let Some(ids) = &m.batch_ids {
            if ids.len() != m.vertices.len() {
                return Err(format!(
                    "网格 BATCHID 数与顶点数不符：顶点 {} / BATCHID {}",
                    m.vertices.len(),
                    ids.len()
                ));
            }
            let own: std::collections::BTreeSet<u32> = ids.iter().copied().collect();
            let clash: Vec<u32> = own.intersection(&seen).copied().collect();
            if !clash.is_empty() {
                return Err(format!(
                    "网格 BATCHID 冲突：feature id {:?} 在多个子网格重复",
                    clash
                ));
            }
            seen.extend(own);
            out.batch_ids.get_or_insert_with(Vec::new).extend_from_slice(ids);
        } else if out.batch_ids.is_some() {
            return Err("网格 BATCHID 不一致：部分带部分缺失，禁止静默混合".to_string());
        }
    }
    out.texture = texture;
    out.texture_inline = texture_inline;
    Ok(out)
}

/// 合并前内嵌纹理一致性检查：任一带内嵌即取第一份（UniqueID 复用场景）；
/// 互相冲突报错。
fn merged_texture_inline(meshes: &[&Mesh]) -> Result<Option<InlineTexture>, String> {
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

/// 合并前纹理一致性检查：全部一致取之，冲突/部分缺失报错。
fn merged_texture(meshes: &[&Mesh]) -> Result<Option<String>, String> {
    let mut first: Option<&str> = None;
    for m in meshes {
        match (&m.texture, first) {
            (None, None) => {}
            (Some(t), None) => first = Some(t),
            (Some(t), Some(f)) if t == f => {}
            (Some(t), Some(f)) => {
                return Err(format!(
                    "子分块纹理引用冲突：`{f}` 与 `{t}`，无法合并为单 primitive（禁止静默丢弃纹理）"
                ));
            }
            (None, Some(f)) => {
                return Err(format!(
                    "子分块纹理引用不一致：部分绑定 `{f}`、部分无纹理，禁止静默丢弃纹理"
                ));
            }
        }
    }
    Ok(first.map(|s| s.to_string()))
}

/// 生效源目录解析（`--src` 优先，其次 `manifest.source` 相对 manifest 目录）。
pub fn effective_source_dir(
    manifest: &tangis_manifest::ChunkManifest,
    base_dir: &Path,
    src_dir: Option<&Path>,
) -> Option<PathBuf> {
    src_dir.map(|p| p.to_path_buf()).or_else(|| {
        manifest.source.as_ref().map(|s| {
            let p = PathBuf::from(s);
            if p.is_absolute() {
                p
            } else {
                base_dir.join(p)
            }
        })
    })
}

enum SourceKind {
    File(PathBuf),
    MergeChildren,
}

/// 单分块来源类别判定（与流式解析共用；错误信息与旧全量版一致）。
fn source_kind(
    chunk: &tangis_manifest::Chunk,
    base_dir: &Path,
    src_dir: Option<&Path>,
) -> Result<SourceKind, String> {
    if let Some(s) = &chunk.source {
        let p = PathBuf::from(s);
        let p = if p.is_absolute() { p } else { base_dir.join(p) };
        return Ok(SourceKind::File(p));
    }
    if let Some(dir) = src_dir {
        if let Some(p) = find_source_in_dir(dir, &chunk.id.0) {
            return Ok(SourceKind::File(p));
        }
    }
    if chunk.children.is_empty() {
        return Err(format!(
            "分块 `{}` 无法解析真实几何来源：无 chunk.source，\
             源目录中不存在 {}.osgb / {}.obj（或未声明源目录）\
             （内核禁止产出占位几何）",
            chunk.id, chunk.id, chunk.id
        ));
    }
    Ok(SourceKind::MergeChildren)
}

/// 流式解析**单个**分块的真实几何网格 + per-feature 来源标注
/// （M2-PERF：替代旧 `resolve_meshes` 全量驻留——峰值内存从
/// O(全部分块) 降为 O(单分块)，父分块按需递归加载子分块源文件合并）。
///
/// `by_id` 为 manifest 全部分块的 id 索引；`src_dir` 为生效源目录
/// （[`effective_source_dir`] 的结果）。同一子文件被父分块合并时
/// 会重复加载（与旧版「唯一文件缓存」相比多读几次小文件，
/// 换取无常驻网格缓存）。
pub fn resolve_chunk_mesh(
    id: &tangis_manifest::ChunkId,
    by_id: &BTreeMap<&tangis_manifest::ChunkId, &tangis_manifest::Chunk>,
    base_dir: &Path,
    src_dir: Option<&Path>,
) -> Result<ResolvedMesh, String> {
    let chunk = by_id
        .get(id)
        .copied()
        .ok_or_else(|| format!("内部错误：分块 {id} 不在清单中"))?;
    match source_kind(chunk, base_dir, src_dir)? {
        SourceKind::File(p) => {
            let mesh = load_mesh(&p)?;
            let label = p
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_else(|| p.to_string_lossy().into_owned());
            let feature_sources = vec![label; tangis_osgb::feature_count(&mesh) as usize];
            Ok(ResolvedMesh { mesh, feature_sources })
        }
        SourceKind::MergeChildren => {
            let mut child_meshes: Vec<Mesh> = Vec::new();
            let mut feature_sources: Vec<String> = Vec::new();
            for cid in &chunk.children {
                let rm = resolve_chunk_mesh(cid, by_id, base_dir, src_dir)?;
                child_meshes.push(rm.mesh);
                feature_sources.extend(rm.feature_sources);
            }
            let refs: Vec<&Mesh> = child_meshes.iter().collect();
            let merged = merge_meshes(&refs)
                .map_err(|e| format!("分块 `{}` 合并子分块失败: {e}", chunk.id))?;
            if merged.vertices.is_empty() || merged.indices.is_empty() {
                return Err(format!(
                    "分块 `{}` 合并子分块几何后为空，无法产出 b3dm",
                    chunk.id
                ));
            }
            Ok(ResolvedMesh { mesh: merged, feature_sources })
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn testdata_src() -> PathBuf {
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/osgb/src")
    }

    /// 真实 osgconv 3.6.5 语料根目录（osgb/ + objs/ + textures/）。
    fn real_corpus() -> PathBuf {
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/osgb-real")
    }

    /// 真实语料缺失时跳过当前测试（不 fail CI）。
    fn require_real_corpus() -> Option<PathBuf> {
        let dir = real_corpus().join("osgb");
        if dir.join("Tile_+000_+000.osgb").is_file() {
            Some(dir)
        } else {
            eprintln!("跳过：真实语料 testdata/osgb-real/osgb/ 不存在");
            None
        }
    }

    #[test]
    fn loads_obj_from_testdata() {
        let mesh = load_mesh(&testdata_src().join("Tile_+000_+000.obj")).unwrap();
        assert_eq!(mesh.vertices.len(), 289);
        assert_eq!(mesh.indices.len(), 512 * 3);
        let (min, max) = mesh.bounds().unwrap();
        // 语料 OBJ 是 Y-up（y = 高度）；读入按 (x,y,z)→(x,−z,y) 转内部 Z-up：
        // x ∈ [0,100] 不变，y = −z_obj ∈ [−100,0]，z = y_obj（高度，非负为主）
        assert!(min[0] >= 0.0 - 1e-4 && max[0] <= 100.0 + 1e-4);
        assert!(min[1] >= -100.0 - 1e-4 && max[1] <= 1e-4, "y 应为 −z_obj ∈ [−100,0]");
        assert!(min[2] > -10.0 && max[2] < 30.0, "z 应为高度（源 y ∈ [−2.38, 21.78] 量级）");
    }

    #[test]
    fn loads_osgb_from_real_corpus() {
        let Some(osgb_dir) = require_real_corpus() else {
            return;
        };
        let mesh = load_mesh(&osgb_dir.join("Tile_+000_+000.osgb")).unwrap();
        assert_eq!(mesh.vertices.len(), 1089);
        assert_eq!(mesh.indices.len(), 2048 * 3);
        assert_eq!(mesh.normals.as_ref().unwrap().len(), 1089);
        // 纹理相对引用已解析为绝对路径（objs/../textures/atlas256.png）
        let resolved = mesh.texture.as_ref().expect("真实语料带纹理引用");
        assert!(Path::new(resolved).is_file(), "纹理应存在: {resolved}");
        assert!(resolved.ends_with("textures/atlas256.png"), "{resolved}");
    }

    #[test]
    fn obj_and_osgb_load_to_identical_meshes() {
        // 真实语料同一 tile 的两种源对照：OSGB 经 osgb_to_z_up 恢复后必须
        // 等于 OBJ 源（z-up）的原始坐标。字节级依据：OSGB 顶点 = OBJ (x,y,z)
        // →(x,−z,y)，本测试是逆变换正确性的端到端证据。
        // 语料顶点排列顺序在两种格式间不同，故按包围盒对照；f32 落盘有
        // 舍入，用 1e-3 容差。
        let Some(osgb_dir) = require_real_corpus() else {
            return;
        };
        let obj_path = real_corpus().join("objs").join("Tile_+000_+000.obj");
        if !obj_path.is_file() {
            eprintln!("跳过：真实语料 objs/ 源不存在");
            return;
        }
        // 裸读 OBJ v 行（真实语料 OBJ 为 z-up，无旋转；测试内简析即可）
        let text = std::fs::read_to_string(&obj_path).unwrap();
        let mut raw: Vec<[f32; 3]> = Vec::new();
        for line in text.lines() {
            if let Some(rest) = line.strip_prefix("v ") {
                let c: Vec<f32> = rest.split_whitespace().take(3).map(|s| s.parse().unwrap()).collect();
                raw.push([c[0], c[1], c[2]]);
            }
        }
        assert_eq!(raw.len(), 1089);

        let osgb = load_mesh(&osgb_dir.join("Tile_+000_+000.osgb")).unwrap();
        assert_eq!(osgb.vertices.len(), raw.len());

        let obj_min_max = |pts: &[[f32; 3]]| {
            let mut min = pts[0];
            let mut max = pts[0];
            for p in pts {
                for a in 0..3 {
                    min[a] = min[a].min(p[a]);
                    max[a] = max[a].max(p[a]);
                }
            }
            (min, max)
        };
        let (omin, omax) = obj_min_max(&raw);
        let (smin, smax) = osgb.bounds().unwrap();
        for a in 0..3 {
            assert!(
                (omin[a] - smin[a]).abs() < 1e-3 && (omax[a] - smax[a]).abs() < 1e-3,
                "包围盒不一致 axis {a}: OBJ [{},{},{}]-[{},{},{}] vs OSGB [{},{},{}]-[{},{},{}]",
                omin[0], omin[1], omin[2], omax[0], omax[1], omax[2],
                smin[0], smin[1], smin[2], smax[0], smax[1], smax[2],
            );
        }
        // OSGB 路径带法线（宁全勿缺）；Z-up 验证：x/y ∈ [0,100]（水平），
        // z 为高度（OBJ 源 z ∈ [−2.14, 3.76]）
        assert_eq!(osgb.normals.as_ref().unwrap().len(), 1089);
        assert!((smin[0] - 0.0).abs() < 1e-3 && (smax[0] - 100.0).abs() < 1e-3);
        assert!((smin[1] - 0.0).abs() < 1e-3 && (smax[1] - 100.0).abs() < 1e-3);
        assert!(smin[2] > -10.0 && smax[2] < 10.0, "z 应为高度量级");
    }

    #[test]
    fn rejects_unknown_extension_and_missing_file() {
        let err = load_mesh(Path::new("nope.txt")).unwrap_err();
        assert!(err.contains("不支持的几何源扩展名"), "{err}");
        assert!(load_mesh(Path::new("no/such/file.obj")).is_err());
    }

    #[test]
    fn finds_source_by_stem_with_osgb_priority() {
        // 真实语料目录含 .osgb（OBJ 源在 objs/ 子目录不参与本测试）
        let Some(dir) = require_real_corpus() else {
            return;
        };
        let p = find_source_in_dir(&dir, "Tile_+000_+000").unwrap();
        assert_eq!(p.extension().unwrap(), "osgb");
        assert!(find_source_in_dir(&dir, "NoSuchTile").is_none());
    }

    #[test]
    fn finds_source_recursively_in_nested_dirs() {
        // 生产布局 Data/Tile_xxx/Tile_xxx.osgb：递归查找命中子目录文件
        let base = std::env::temp_dir().join(format!("tangis-find-src-{}", std::process::id()));
        let nested = base.join("Data").join("Tile_+003_+007");
        std::fs::create_dir_all(&nested).unwrap();
        std::fs::write(nested.join("Tile_+003_+007.osgb"), b"x").unwrap();
        let p = find_source_in_dir(&base, "Tile_+003_+007").unwrap();
        assert_eq!(p, nested.join("Tile_+003_+007.osgb"));
        assert!(find_source_in_dir(&base, "Missing").is_none());
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn merge_meshes_offsets_indices_and_drops_partial_normals() {
        let a = Mesh {
            vertices: vec![[0.0, 0.0, 0.0], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0]],
            indices: vec![0, 1, 2],
            normals: Some(vec![[0.0, 0.0, 1.0]; 3]),
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: None,
        };
        let mut b = a.clone();
        b.vertices = vec![[5.0, 0.0, 0.0], [6.0, 0.0, 0.0], [5.0, 1.0, 0.0]];
        b.normals = None; // 部分无法线 → 合并后整体丢弃

        let merged = merge_meshes(&[&a, &b]).unwrap();
        assert_eq!(merged.vertices.len(), 6);
        assert_eq!(merged.indices, vec![0, 1, 2, 3, 4, 5]);
        assert!(merged.normals.is_none(), "部分网格无法线时应整体丢弃法线");

        // 全部带法线时保留
        let merged = merge_meshes(&[&a, &a]).unwrap();
        assert_eq!(merged.normals.as_ref().unwrap().len(), 6);
    }

    fn textured_mesh(texture: Option<&str>, uvs: Option<Vec<[f32; 2]>>) -> Mesh {
        Mesh {
            vertices: vec![[0.0, 0.0, 0.0], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0]],
            indices: vec![0, 1, 2],
            normals: None,
            uvs,
            texture: texture.map(|t| t.to_string()),
            texture_inline: None,
            batch_ids: None,
        }
    }

    #[test]
    fn merge_meshes_texture_consistency() {
        let a = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        let b = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        let merged = merge_meshes(&[&a, &b]).unwrap();
        assert_eq!(merged.texture.as_deref(), Some("/a.png"));
        assert_eq!(merged.uvs.as_ref().unwrap().len(), 6);

        // 纹理冲突
        let c = textured_mesh(Some("/b.png"), Some(vec![[0.0; 2]; 3]));
        assert!(merge_meshes(&[&a, &c]).is_err());
        // 部分绑定
        let d = textured_mesh(None, Some(vec![[0.0; 2]; 3]));
        assert!(merge_meshes(&[&a, &d]).is_err());
        // UV 部分缺失
        let e = textured_mesh(Some("/a.png"), None);
        assert!(merge_meshes(&[&a, &e]).is_err());
        // 无纹理合并保持 None
        let f = textured_mesh(None, None);
        assert_eq!(merge_meshes(&[&f, &f]).unwrap().texture, None);
    }

    #[test]
    fn resolve_osgb_texture_resolves_relative_reference() {
        let Some(osgb_dir) = require_real_corpus() else {
            return;
        };
        let osgb = osgb_dir.join("Tile_+000_+000.osgb");
        let mesh = load_mesh(&osgb).unwrap();
        // Image FileName `objs/../textures/atlas256.png` 相对 OSGB 目录解析
        let expected = osgb_dir.join("objs/../textures/atlas256.png");
        assert_eq!(mesh.texture.as_deref(), Some(expected.to_str().unwrap()));
        // UV 已解析且与顶点一一对应
        assert_eq!(mesh.uvs.as_ref().unwrap().len(), mesh.vertices.len());
    }

    #[test]
    fn resolve_osgb_texture_reports_missing_file_and_uv() {
        let Some(osgb_dir) = require_real_corpus() else {
            return;
        };
        let osgb = osgb_dir.join("Tile_+000_+000.osgb");
        // 真实语料带纹理且引用可满足：直接通过
        let mut mesh = load_mesh(&osgb).unwrap();
        resolve_osgb_texture(&mut mesh, &osgb).unwrap();
        assert!(mesh.texture.is_some());

        // 真实语料 Tile_+000 带 INLINE_DATA 内嵌字节：即使 texture 指向
        // 不存在的文件也 Ok（字节直通优先，文件路径仅作溯源）
        let mut mesh = load_mesh(&osgb).unwrap();
        assert!(
            mesh.texture_inline.is_some(),
            "语料 Tile_+000 应带内嵌纹理（RawPixels）"
        );
        mesh.texture = Some("textures/no-such-atlas.png".into());
        resolve_osgb_texture(&mut mesh, &osgb).unwrap();

        // 真实语料 RawPixels（256×256 GL_RGB）→ PNG 编码直通
        let tex = load_texture_image(&mesh).unwrap().expect("内嵌像素应产出纹理");
        assert_eq!(tex.mime_type, "image/png");
        assert_eq!(
            sniff_image_mime(&tex.bytes).unwrap(),
            "image/png",
            "编码产物必须是合法 PNG 字节"
        );

        // 无内嵌字节 + 文件不存在 → 报错带缺失清单
        let mut mesh = load_mesh(&osgb).unwrap();
        mesh.texture = Some("textures/no-such-atlas.png".into());
        mesh.texture_inline = None;
        let err = resolve_osgb_texture(&mut mesh, &osgb).unwrap_err();
        assert!(err.contains("纹理文件不存在"), "{err}");
        assert!(err.contains("no-such-atlas.png"), "{err}");

        // 绑定纹理但缺 UV → 报错
        let mut mesh = load_mesh(&osgb).unwrap();
        mesh.texture = Some(
            osgb_dir
                .join("objs/../textures/atlas256.png")
                .to_string_lossy()
                .into_owned(),
        );
        mesh.texture_inline = None;
        mesh.uvs = None;
        let err = resolve_osgb_texture(&mut mesh, &osgb).unwrap_err();
        assert!(err.contains("缺少 UV 数组"), "{err}");
    }

    #[test]
    fn resolve_chunk_mesh_maps_by_id_and_merges_parent() {
        use tangis_manifest::{Bounds, Chunk, ChunkId, ChunkManifest, ChunkStatus};

        let mk = |id: &str, children: &[&str]| Chunk {
            id: ChunkId::new(id),
            lod: 0,
            bounds: Bounds { min: [0.0; 3], max: [1.0; 3] },
            status: ChunkStatus::Pending,
            attempts: 0,
            children: children.iter().map(|c| ChunkId::new(*c)).collect(),
            source: None,
            source_offset: None,
        };
        let Some(src) = require_real_corpus() else {
            return;
        };
        let manifest = ChunkManifest {
            task_id: "t".into(),
            // 父分块 id 与文件名不匹配 → 走子分块合并分支
            chunks: vec![
                mk("parent", &["Tile_+000_+000", "Tile_+000_+001"]),
                mk("Tile_+000_+000", &[]),
                mk("Tile_+000_+001", &[]),
            ],
            source: Some(src.to_string_lossy().into_owned()),
        };
        let by_id: BTreeMap<&tangis_manifest::ChunkId, &tangis_manifest::Chunk> =
            manifest.chunks.iter().map(|c| (&c.id, c)).collect();
        // 生效源目录 = effective_source_dir（--src 优先，其次 manifest.source）
        let dir = effective_source_dir(&manifest, &src, None);

        let leaf = resolve_chunk_mesh(
            &ChunkId::new("Tile_+000_+000"),
            &by_id,
            &src,
            dir.as_deref(),
        )
        .unwrap();
        assert_eq!(leaf.mesh.vertices.len(), 1089);
        // per-feature 来源标注：单 Geometry 语料 = 1 个 feature
        assert_eq!(leaf.feature_sources, vec!["Tile_+000_+000.osgb".to_string()]);
        assert_eq!(leaf.mesh.batch_ids.as_ref().unwrap(), &[0; 1089]);

        let parent =
            resolve_chunk_mesh(&ChunkId::new("parent"), &by_id, &src, dir.as_deref())
                .unwrap();
        assert_eq!(parent.mesh.vertices.len(), 1089 * 2);
        assert_eq!(parent.mesh.indices.len(), 2048 * 3 * 2);
        // 父合并两个子：feature_sources 拼接、BATCHID 偏移重排
        assert_eq!(parent.feature_sources.len(), 2);
        assert_eq!(parent.feature_sources[1], "Tile_+000_+001.osgb");
        let ids = parent.mesh.batch_ids.as_ref().unwrap();
        assert!(ids[..1089].iter().all(|&b| b == 0), "子 0 的 feature id 应为 0");
        assert!(ids[1089..].iter().all(|&b| b == 1), "子 1 的 feature id 应偏移为 1");
    }

    #[test]
    fn resolve_chunk_mesh_errors_on_unmatched_leaf() {
        use tangis_manifest::{Bounds, Chunk, ChunkId, ChunkManifest, ChunkStatus};

        let Some(src) = require_real_corpus() else {
            return;
        };
        let manifest = ChunkManifest {
            task_id: "t".into(),
            chunks: vec![Chunk {
                id: ChunkId::new("no-such-stem"),
                lod: 0,
                bounds: Bounds { min: [0.0; 3], max: [1.0; 3] },
                status: ChunkStatus::Pending,
                attempts: 0,
                children: vec![],
                source: None,
                source_offset: None,
            }],
            source: Some(src.to_string_lossy().into_owned()),
        };
        let by_id: BTreeMap<&tangis_manifest::ChunkId, &tangis_manifest::Chunk> =
            manifest.chunks.iter().map(|c| (&c.id, c)).collect();
        let dir = effective_source_dir(&manifest, &src, None);
        let err = resolve_chunk_mesh(
            &ChunkId::new("no-such-stem"),
            &by_id,
            &src,
            dir.as_deref(),
        )
        .unwrap_err();
        assert!(err.contains("禁止产出占位几何"), "{err}");
    }

    // ---- 内嵌纹理直通（INLINE 优先于文件路径）----

    #[test]
    fn sniff_image_mime_by_signature() {
        assert_eq!(sniff_image_mime(&[0x89, b'P', b'N', b'G', 0x0d, 0x0a, 0x1a, 0x0a]).unwrap(), "image/png");
        assert_eq!(sniff_image_mime(&[0xff, 0xd8, 0xff, 0xe0]).unwrap(), "image/jpeg");
        let err = sniff_image_mime(b"RIFF....").unwrap_err();
        assert!(err.contains("禁止猜测"), "{err}");
    }

    #[test]
    fn inline_encoded_file_takes_priority_over_missing_file() {
        // 生成器产物语料 atlas.png（与 tiles::b3dm 测试同源）
        let png = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../../testdata/osgb/textured/textures/atlas.png");
        let png = std::fs::read(&png).expect("atlas.png 应存在");
        let mut mesh = textured_mesh(Some("no/such/file.png"), Some(vec![[0.0; 2]; 3]));
        mesh.texture_inline = Some(InlineTexture::EncodedFile(png.clone()));

        // 有内嵌字节：文件路径不存在也不影响（resolve 跳过存在性检查）
        let osgb = testdata_src().join("Tile_+000_+000.obj");
        resolve_osgb_texture(&mut mesh, &osgb).unwrap();

        let tex = load_texture_image(&mesh).unwrap().expect("内嵌字节应产出纹理");
        assert_eq!(tex.mime_type, "image/png");
        assert_eq!(tex.bytes, png, "INLINE_FILE 字节必须直通（不重编码）");
    }

    #[test]
    fn inline_raw_pixels_encode_to_png() {
        // 2×1 RGBA，origin BOTTOM_LEFT（行序翻转）
        let raw = tangis_osgb::RawImage {
            origin: 0,
            s: 2,
            t: 2,
            pixel_format: 0x1908,
            data_type: 0x1401,
            data: vec![
                1, 2, 3, 4, 5, 6, 7, 8, // 底行（origin=0 → 编码后成为末行）
                9, 10, 11, 12, 13, 14, 15, 16, // 顶行（编码后成为首行）
            ],
        };
        let mut mesh = textured_mesh(Some("inline"), Some(vec![[0.0; 2]; 3]));
        mesh.texture_inline = Some(InlineTexture::RawPixels(raw));
        let tex = load_texture_image(&mesh).unwrap().expect("RawPixels 应编码为 PNG");
        assert_eq!(tex.mime_type, "image/png");
        // 解码回读：尺寸与行序
        let img = image::load_from_memory(&tex.bytes).unwrap();
        assert_eq!((img.width(), img.height()), (2, 2));
        assert_eq!(img.to_rgba8().get_pixel(0, 0).0, [9, 10, 11, 12], "TOP 行应到 PNG 首行");
        assert_eq!(img.to_rgba8().get_pixel(1, 1).0, [5, 6, 7, 8]);
    }

    #[test]
    fn texture_without_inline_or_file_errors_loudly() {
        let mut mesh = textured_mesh(Some("/no/such/atlas.png"), Some(vec![[0.0; 2]; 3]));
        let err = load_texture_image(&mesh).unwrap_err();
        assert!(err.contains("无法读纹理"), "{err}");

        // 空文件名 + 无内嵌 → 明确报错
        mesh.texture = Some(String::new());
        let err = load_texture_image(&mesh).unwrap_err();
        assert!(err.contains("无内嵌字节且文件名为空"), "{err}");

        // 不支持的内嵌签名 → 明确报错（不猜 mimeType）
        mesh.texture_inline = Some(InlineTexture::EncodedFile(b"RIFF0000".to_vec()));
        let err = load_texture_image(&mesh).unwrap_err();
        assert!(err.contains("禁止猜测"), "{err}");
    }

    #[test]
    fn merge_meshes_offsets_batch_ids_and_merges_inline() {
        let mut a = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        a.batch_ids = Some(vec![0, 0, 0]);
        a.texture_inline = Some(InlineTexture::EncodedFile(vec![1, 2, 3]));
        let mut b = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        b.batch_ids = Some(vec![0, 0, 0]); // 子网格内各自从 0 起

        let merged = merge_meshes(&[&a, &b]).unwrap();
        assert_eq!(merged.batch_ids.as_ref().unwrap(), &[0, 0, 0, 1, 1, 1]);
        assert_eq!(merged.texture_inline, a.texture_inline, "UniqueID 复用取第一份字节");

        // 内嵌字节冲突 → Err
        let mut c = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        c.texture_inline = Some(InlineTexture::EncodedFile(vec![9]));
        let err = merge_meshes(&[&a, &c]).unwrap_err();
        assert!(err.contains("冲突"), "{err}");
    }

    /// merge_tile_meshes（edit --format b3dm 用）：BATCHID 不重排直接拼接
    /// （解析器按源文件 Geometry 序号分配，文件内唯一），索引做顶点偏移；
    /// 跨网格重复 id 报错；纹理/UV 一致性规则同 merge_meshes。
    #[test]
    fn merge_tile_meshes_keeps_batch_ids_and_checks_conflicts() {
        let mut a = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        a.batch_ids = Some(vec![2, 2, 2]); // 非从 0 起的文件级 id
        let mut b = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        b.vertices = vec![[5.0, 0.0, 0.0], [6.0, 0.0, 0.0], [5.0, 1.0, 0.0]];
        b.batch_ids = Some(vec![5, 5, 5]);

        let merged = merge_tile_meshes(&[&a, &b]).unwrap();
        assert_eq!(merged.vertices.len(), 6);
        assert_eq!(merged.indices, vec![0, 1, 2, 3, 4, 5], "索引做顶点偏移");
        assert_eq!(merged.batch_ids.as_ref().unwrap(), &[2, 2, 2, 5, 5, 5], "BATCHID 不重排");
        assert_eq!(merged.uvs.as_ref().unwrap().len(), 6);
        assert_eq!(tangis_osgb::feature_count(&merged), 6, "feature 数 = max id + 1");

        // 跨网格重复 id → Err
        let mut c = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        c.batch_ids = Some(vec![2, 2, 2]);
        let err = merge_tile_meshes(&[&a, &c]).unwrap_err();
        assert!(err.contains("冲突"), "{err}");

        // 部分带 BATCHID → Err
        let d = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        let err = merge_tile_meshes(&[&a, &d]).unwrap_err();
        assert!(err.contains("BATCHID 不一致"), "{err}");

        // 无 BATCHID 网格合并：整体单 feature
        let e = textured_mesh(Some("/a.png"), Some(vec![[0.0; 2]; 3]));
        let merged = merge_tile_meshes(&[&e, &e]).unwrap();
        assert!(merged.batch_ids.is_none());
        assert_eq!(tangis_osgb::feature_count(&merged), 1);
    }
}
