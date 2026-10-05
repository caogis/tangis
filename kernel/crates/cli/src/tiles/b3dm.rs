//! b3dm（Batched 3D Model，3D Tiles 1.0）二进制组装（真实几何）。
//!
//! 手写字节级组装，不引入 gltf/gltfgen 等依赖。占位三角形路径已删除：
//! 入口只有 [`build_b3dm`]，网格为空在调用方即报错，内核禁止
//! 产出假几何。
//!
//! # Batch（feature）语义
//!
//! 每个 `osg::Geometry`（源 Geode 内每个 Drawable）= 一个 feature：
//! - FeatureTable 写 `BATCH_LENGTH` = feature 数（`Mesh::batch_ids`
//!   最大值 + 1；无 batch_ids 的网格视为单 feature）；
//! - GLB 写每顶点 `_BATCHID` 属性（u32，b3dm 规范约定经 glTF 属性传递），
//!   Cesium pick 据此命中 `Cesium3DTileFeature`；
//! - BatchTable 只写 per-feature **生成元信息**（`_source`/`_tile`/
//!   `_feature_index`/`_vertex_count`/`_triangle_count`）——真实倾斜
//!   OSGB 不携带业务属性，`_source` 明确标注来源为生成元信息，
//!   绝不编造业务属性。
//!
//! # 坐标管线（本模块承担最后一棒：内部 Z-up → glTF Y-up）
//!
//! ```text
//! 源 OBJ（Y-up，y=高度）
//!   └─ geometry.rs 读入 (x,y,z)→(x,−z,y) ─┐
//! 源 OSGB（Z-up，z=高度，语料生成器已转换）┴→ 内部 Mesh（统一 Z-up：
//!                                             x/y 水平、z=高度，与
//!                                             tileset box 同系）
//!   └─ 本模块写 GLB：(x,y,z)→(x,z,−y) → GLB POSITION（Y-up：
//!       水平 x/z、高度 +y，glTF 2.0 规范约定）
//! ```
//!
//! 两式均为绕 X 轴的旋转（右手系，行列式 +1），互为逆变换。Cesium 对
//! b3dm 内嵌 glTF 强制应用 Y_UP_TO_Z_UP（恰为 (x,−z,y)），把 GLB 顶点
//! 旋转回 tile 局部 Z-up 系——与内部坐标、tileset boundingVolume box
//! 完全对齐。**漏做本步**会让模型落到瓦片原点下方（水平范围被当成
//! 高度），这是「Cesium 加载后不可见」缺陷的根因。
//!
//! min/max 约束：轴交换 + 负号会交换分量端点来源，故先逐顶点变换、
//! 再对变换后数据逐分量求 min/max（不搬运旧端点）。
//!
//! # 三角形绕序（winding）
//!
//! 坐标管线全程为绕 X 轴的纯旋转（det = +1，见 `geometry.rs` 坐标管线注释），
//! 本身不改变绕序；绕序反转来自**源数据**：摄影测量语料（OBJ/OSGB）的
//! 三角形按「从外部看为顺时针 CW」的约定书写。实证有二：
//! 1. 语料 `Tile_+000_+000.obj` 首面 `f 1 2 19`（x=0 墙面，外侧 −x）的
//!    几何法向 u×v 的 x 分量为正（指向网格内部）；
//! 2. 浏览器侧闭环定位：Cesium 中禁用 CULL_FACE 后模型立即完整渲染，
//!    即开启剔除时全部正面三角形被当背面剔除。
//!
//! glTF 2.0 规范约定 front face = CCW，Cesium 对 b3dm 默认 cull BACK，
//! CW 源数据直接写入会被整体剔除——模型完全不可见。
//!
//! 修复：写 GLB 时对每个三角形**交换 index 的第 2/3 顶点**
//! （`i[1]↔i[2]`），一次性把所有三角形翻成 CCW（从外部看）。内核索引
//! 统一为 `Vec<u32>`（osgb parser 已把 DrawElementsUShort/UInt 归一为
//! u32），GLB 只发 componentType 5125，无其他索引类型路径。NORMAL 经
//! 旋转映射后物理方向不变；语料若带内向法线，依赖法线的光照可能偏暗
//! （面朝向本身已正确，无 NORMAL 时渲染端按 CCW 绕序计算的平面法向
//! 即朝外）。
//!
//! b3dm 布局（3D Tiles 1.0 规范）：
//! ```text
//! 28 字节 header:
//!   magic                          4B  "b3dm"
//!   version                        4B  1
//!   byteLength                     4B  整个 tile 文件长度（含 header）
//!   featureTableJSONByteLength     4B  FeatureTable JSON 长度（4 字节对齐，空格 0x20 补齐）
//!   featureTableBinaryByteLength   4B  0
//!   batchTableJSONByteLength       4B  BatchTable JSON 长度（4 字节对齐）
//!   batchTableBinaryByteLength     4B  0
//! FeatureTable JSON: {"BATCH_LENGTH":N}
//! BatchTable JSON: per-feature 生成元信息（列状数组，长度 = N）
//! GLB: glTF 2.0 BINARY —— POSITION / 可选 NORMAL / u32 索引 / _BATCHID
//! ```
//!
//! # 纹理
//!
//! 网格带纹理引用时（`Mesh::texture` + `Mesh::uvs`），GLB 嵌入完整纹理链路：
//! TEXCOORD_0 accessor + image（bufferView 内嵌 PNG/JPEG 字节）+ sampler +
//! texture + material。材质选 `pbrMetallicRoughness.baseColorTexture`
//! （metallic=0 / roughness=1，摄影纹理语义；不引 KHR_materials_unlit 扩展，
//! 零扩展依赖兼容性最好）。
//!
//! 有纹理引用但缺 UV / UV 数不符时**明确报错**，禁止静默产出无纹理几何。
//! 无纹理引用的网格不产生任何纹理节点（此时带 UV 属合法情况，UV 不会
//! 写入 GLB）。纹理字节来源（内嵌/文件路径）由调用方决定，本模块只接收
//! [`TextureImage`]。

/// batch table `_source` 字段内容：声明本表全部取值为切片时生成的元信息，
/// 并非源数据业务属性（真实倾斜 OSGB 无扩展业务属性，不编造）。
const BATCH_SOURCE_NOTE: &str = "generated-metadata:tangis-kernel (生成元信息，非源数据业务属性)";

/// 已读入内存的纹理图像（GLB 嵌入不做解码，仅透传字节 + mimeType）。
pub struct TextureImage<'a> {
    pub bytes: &'a [u8],
    /// glTF mimeType，如 "image/png" / "image/jpeg"。
    pub mime_type: &'a str,
}

/// 带真实几何的 b3dm 组装（无纹理路径，保持既有入口）。
///
/// `feature_sources`：与 feature（BATCHID）一一对应的来源标注
/// （batch table `_tile` 列；长度必须等于 feature 数）。
///
/// GLB 含真实顶点/索引（及可选法线、_BATCHID）：
/// - POSITION accessor 的 min/max **按真实数据计算**（规范对 POSITION 必填，
///   Cesium 用它做剔除，硬编码会导致 tile 消失或裁剪错误）；
/// - 索引用 u32（componentType 5125），mode = TRIANGLES；
/// - BIN chunk 各段均为 4 字节对齐（f32×3 / u32 天然对齐）。
pub fn build_b3dm_from_mesh(mesh: &tangis_osgb::Mesh, feature_sources: &[String]) -> Vec<u8> {
    build_b3dm(mesh, None, feature_sources).expect("无纹理路径不可能失败")
}

/// 带纹理的 b3dm 组装：网格必须有 UV（数量与顶点一致），否则 Err。
pub fn build_b3dm_from_mesh_textured(
    mesh: &tangis_osgb::Mesh,
    texture: &TextureImage,
    feature_sources: &[String],
) -> Result<Vec<u8>, String> {
    build_b3dm(mesh, Some(texture), feature_sources)
}

/// 每顶点 BATCHID：无 batch_ids 的网格（OBJ 源等）视为单 feature。
fn batch_ids_of(mesh: &tangis_osgb::Mesh) -> Vec<u32> {
    match &mesh.batch_ids {
        Some(ids) => {
            assert_eq!(
                ids.len(),
                mesh.vertices.len(),
                "BATCHID 数与顶点数不符：顶点 {} / BATCHID {}",
                mesh.vertices.len(),
                ids.len()
            );
            ids.clone()
        }
        None => vec![0; mesh.vertices.len()],
    }
}

fn build_b3dm(
    mesh: &tangis_osgb::Mesh,
    texture: Option<&TextureImage>,
    feature_sources: &[String],
) -> Result<Vec<u8>, String> {
    use serde_json::json;

    assert!(!mesh.vertices.is_empty(), "真实几何网格不能为空");
    assert!(!mesh.indices.is_empty(), "真实几何网格必须有索引");

    let batch_ids = batch_ids_of(mesh);
    let batch_length = tangis_osgb::feature_count(mesh) as usize;
    if feature_sources.len() != batch_length {
        return Err(format!(
            "feature 来源数与 BATCH_LENGTH 不符：来源 {} / feature {batch_length}",
            feature_sources.len()
        ));
    }

    // 纹理前置校验：有纹理就必须有完整 UV（禁止静默出无纹理几何）
    let uvs = match texture {
        None => None,
        Some(_) => match &mesh.uvs {
            Some(u) if u.len() == mesh.vertices.len() => Some(u),
            Some(u) => {
                return Err(format!(
                    "网格绑定纹理但 UV 数不符：顶点 {} / UV {}",
                    mesh.vertices.len(),
                    u.len()
                ));
            }
            None => {
                return Err("网格绑定纹理但无 UV 数组，禁止静默产出无纹理几何".to_string());
            }
        },
    };

    // 内部 Z-up → glTF Y-up：(x, y, z) → (x, z, −y)（见模块注释坐标管线）。
    // POSITION 与 NORMAL 同步旋转；TEXCOORD 与轴无关，不动。
    fn y_up(v: [f32; 3]) -> [f32; 3] {
        [v[0], v[2], -v[1]]
    }
    let positions: Vec<[f32; 3]> = mesh.vertices.iter().map(|v| y_up(*v)).collect();
    let normals_up: Option<Vec<[f32; 3]>> = mesh
        .normals
        .as_ref()
        .map(|ns| ns.iter().map(|n| y_up(*n)).collect());

    // 绕序恢复：源语料（OBJ/OSGB）三角形从外部看为 CW，glTF 规范要求
    // front face = CCW（Cesium b3dm 默认 cull BACK，不翻会被整体剔除）。
    // 交换每个三角形的第 2/3 个索引（i[1]↔i[2]）一次性翻成 CCW
    // （见模块注释「三角形绕序」）。
    debug_assert_eq!(
        mesh.indices.len() % 3,
        0,
        "索引数必须是 3 的倍数（TRIANGLES）"
    );
    let indices_out: Vec<u32> = mesh
        .indices
        .chunks_exact(3)
        .flat_map(|t| [t[0], t[2], t[1]])
        .collect();
    debug_assert_eq!(indices_out.len(), mesh.indices.len());

    // BIN：POSITION | [NORMAL] | indices | [TEXCOORD_0] | [image]
    let mut bin: Vec<u8> = Vec::new();
    let push_f32s = |bin: &mut Vec<u8>, vs: &[[f32; 3]]| {
        for v in vs {
            bin.extend_from_slice(&v[0].to_le_bytes());
            bin.extend_from_slice(&v[1].to_le_bytes());
            bin.extend_from_slice(&v[2].to_le_bytes());
        }
    };
    let pos_offset = 0usize;
    push_f32s(&mut bin, &positions);
    let pos_len = bin.len();

    let normal_offset = if let Some(normals) = &normals_up {
        let off = bin.len();
        push_f32s(&mut bin, normals);
        Some((off, bin.len() - off))
    } else {
        None
    };

    let idx_offset = bin.len();
    for i in &indices_out {
        bin.extend_from_slice(&i.to_le_bytes());
    }
    let idx_len = bin.len() - idx_offset;
    debug_assert_eq!(bin.len() % 4, 0, "f32×3 / u32 段天然 4 字节对齐");

    // TEXCOORD_0（f32×2）
    let uv_offset = uvs.map(|_| {
        let off = bin.len();
        for uv in uvs.unwrap() {
            bin.extend_from_slice(&uv[0].to_le_bytes());
            bin.extend_from_slice(&uv[1].to_le_bytes());
        }
        (off, bin.len() - off)
    });

    // _BATCHID（每顶点 u32；b3dm 规范：feature id 经 glTF 属性传递，
    // Cesium pick 依赖此属性区分 feature）
    let batchid_offset = bin.len();
    for id in &batch_ids {
        bin.extend_from_slice(&id.to_le_bytes());
    }
    let batchid_len = bin.len() - batchid_offset;

    // 图像字节（任意长度，pad 到 4 字节对齐；bufferView 不含 pad）
    let image_view = texture.map(|tex| {
        let off = bin.len();
        bin.extend_from_slice(tex.bytes);
        while !bin.len().is_multiple_of(4) {
            bin.push(0);
        }
        (off, tex.bytes.len(), tex.mime_type)
    });

    // POSITION min/max 按变换后的真实数据逐分量计算
    // （(x,z,−y) 中的负号使 z 分量的端点来自内部 y 的对端，不能搬运旧端点）
    let (min, max) = {
        let mut it = positions.iter();
        let first = *it.next().expect("顶点非空");
        let mut min = first;
        let mut max = first;
        for v in it {
            for a in 0..3 {
                min[a] = min[a].min(v[a]);
                max[a] = max[a].max(v[a]);
            }
        }
        (min, max)
    };

    let mut accessors = vec![
        json!({
            "bufferView": 0,
            "componentType": 5126, // FLOAT
            "count": mesh.vertices.len(),
            "type": "VEC3",
            "min": [min[0], min[1], min[2]],
            "max": [max[0], max[1], max[2]]
        }),
    ];
    let mut attributes = json!({ "POSITION": 0 });
    let mut buffer_views = vec![
        json!({
            "buffer": 0, "byteOffset": pos_offset, "byteLength": pos_len,
            "target": 34962 // ARRAY_BUFFER
        }),
    ];
    if let Some((off, len)) = normal_offset {
        let idx = buffer_views.len();
        attributes["NORMAL"] = json!(idx);
        buffer_views.push(json!({
            "buffer": 0, "byteOffset": off, "byteLength": len,
            "target": 34962
        }));
        accessors.insert(1, json!({
            "bufferView": idx,
            "componentType": 5126,
            "count": mesh.normals.as_ref().unwrap().len(),
            "type": "VEC3"
        }));
        // 有 NORMAL 时 indices 的 accessor id 顺延
    }
    let indices_accessor = accessors.len();
    let indices_view = buffer_views.len();
    buffer_views.push(json!({
        "buffer": 0, "byteOffset": idx_offset, "byteLength": idx_len,
        "target": 34963 // ELEMENT_ARRAY_BUFFER
    }));
    accessors.push(json!({
        "bufferView": indices_view,
        "componentType": 5125, // UNSIGNED_INT
        "count": indices_out.len(),
        "type": "SCALAR"
    }));

    // 纹理链路：UV accessor + image bufferView，随后注入 material/sampler/texture
    let mut material = None;
    if texture.is_some() {
        let uv_acc = accessors.len();
        let (off, len) = uv_offset.expect("有纹理必有 UV 段");
        let uv_view = buffer_views.len();
        attributes["TEXCOORD_0"] = json!(uv_acc);
        buffer_views.push(json!({
            "buffer": 0, "byteOffset": off, "byteLength": len,
            "target": 34962
        }));
        accessors.push(json!({
            "bufferView": uv_view,
            "componentType": 5126,
            "count": uvs.expect("有纹理必有 UV").len(),
            "type": "VEC2"
        }));
        let (img_off, img_len, mime) = image_view.expect("有纹理必有图像段");
        let img_view = buffer_views.len();
        buffer_views.push(json!({
            "buffer": 0, "byteOffset": img_off, "byteLength": img_len
        }));
        // 材质选 pbrMetallicRoughness.baseColorTexture（metallic=0/roughness=1，
        // 摄影纹理语义），不引 KHR_materials_unlit 扩展，兼容性最好
        material = Some(json!({
            "images": [{ "bufferView": img_view, "mimeType": mime }],
            "samplers": [{
                "magFilter": 9729,  // LINEAR
                "minFilter": 9987,  // LINEAR_MIPMAP_LINEAR
                "wrapS": 33071,     // CLAMP_TO_EDGE
                "wrapT": 33071
            }],
            "textures": [{ "sampler": 0, "source": 0 }],
            "materials": [{
                "pbrMetallicRoughness": {
                    "baseColorTexture": { "index": 0 },
                    "metallicFactor": 0.0,
                    "roughnessFactor": 1.0
                },
                "doubleSided": true
            }],
            "primitive_material": 0
        }));
    }

    // _BATCHID accessor + bufferView（置于既有 accessor 之后，不扰动
    // POSITION/NORMAL/indices/TEXCOORD_0 的序号约定）
    let batchid_acc = accessors.len();
    let batchid_view = buffer_views.len();
    attributes["_BATCHID"] = json!(batchid_acc);
    buffer_views.push(json!({
        "buffer": 0, "byteOffset": batchid_offset, "byteLength": batchid_len,
        "target": 34962
    }));
    accessors.push(json!({
        "bufferView": batchid_view,
        "componentType": 5125, // UNSIGNED_INT
        "count": batch_ids.len(),
        "type": "SCALAR"
    }));

    let mut gltf = json!({
        "asset": { "version": "2.0" },
        "scene": 0,
        "scenes": [{ "nodes": [0] }],
        "nodes": [{ "mesh": 0 }],
        "meshes": [{
            "primitives": [{
                "attributes": attributes,
                "indices": indices_accessor,
                "mode": 4 // TRIANGLES
            }]
        }],
        "buffers": [{ "byteLength": bin.len() }],
        "bufferViews": buffer_views,
        "accessors": accessors,
    });

    if let Some(m) = material {
        let obj = gltf.as_object_mut().expect("gltf 必为对象");
        obj.insert("images".into(), m["images"].clone());
        obj.insert("samplers".into(), m["samplers"].clone());
        obj.insert("textures".into(), m["textures"].clone());
        obj.insert("materials".into(), m["materials"].clone());
        obj["meshes"][0]["primitives"][0]["material"] = m["primitive_material"].clone();
    }

    let mut json_chunk = serde_json::to_vec(&gltf).expect("glTF JSON 必可序列化");
    while !json_chunk.len().is_multiple_of(4) {
        json_chunk.push(b' ');
    }

    // FeatureTable / BatchTable：BATCH_LENGTH = feature 数；batch table 只写
    // per-feature 生成元信息（_source 注明非业务属性，绝不编造）
    let mut vertex_counts = vec![0u64; batch_length];
    for &id in &batch_ids {
        vertex_counts[id as usize] += 1;
    }
    let mut triangle_counts = vec![0u64; batch_length];
    for tri in mesh.indices.chunks_exact(3) {
        triangle_counts[batch_ids[tri[0] as usize] as usize] += 1;
    }
    let feature_index: Vec<u32> = (0..batch_length as u32).collect();
    let batch_table = json!({
        "_source": vec![BATCH_SOURCE_NOTE; batch_length],
        "_tile": feature_sources,
        "_feature_index": feature_index,
        "_vertex_count": vertex_counts,
        "_triangle_count": triangle_counts,
    });
    let ft_json = json!({ "BATCH_LENGTH": batch_length })
        .to_string()
        .into_bytes();
    let bt_json = serde_json::to_vec(&batch_table).expect("BatchTable JSON 必可序列化");

    let glb = assemble_glb(&json_chunk, &bin);
    Ok(assemble_b3dm(&glb, &ft_json, &bt_json))
}

/// GLB 组装：12B header + JSON chunk + BIN chunk。
fn assemble_glb(json_chunk: &[u8], bin: &[u8]) -> Vec<u8> {
    debug_assert_eq!(json_chunk.len() % 4, 0);
    debug_assert_eq!(bin.len() % 4, 0);
    let json_len = json_chunk.len() as u32;
    let bin_len = bin.len() as u32;
    let total: u32 = 12 + 8 + json_len + 8 + bin_len;

    let mut out = Vec::with_capacity(total as usize);
    out.extend_from_slice(b"glTF");
    out.extend_from_slice(&2u32.to_le_bytes());
    out.extend_from_slice(&total.to_le_bytes());
    out.extend_from_slice(&json_len.to_le_bytes());
    out.extend_from_slice(&0x4E4F_534A_u32.to_le_bytes()); // "JSON"
    out.extend_from_slice(json_chunk);
    out.extend_from_slice(&bin_len.to_le_bytes());
    out.extend_from_slice(&0x004E_4942_u32.to_le_bytes()); // "BIN"
    out.extend_from_slice(bin);
    debug_assert_eq!(out.len() as u32, total);
    out
}

/// b3dm 组装：28B header + FeatureTable JSON + BatchTable JSON + GLB。
/// JSON 段均以空格 0x20 补齐到 4 字节对齐（规范要求）。
fn assemble_b3dm(glb: &[u8], ft_json: &[u8], bt_json: &[u8]) -> Vec<u8> {
    let mut ft = ft_json.to_vec();
    while !ft.len().is_multiple_of(4) {
        ft.push(b' ');
    }
    let mut bt = bt_json.to_vec();
    while !bt.len().is_multiple_of(4) {
        bt.push(b' ');
    }

    let header_len = 28u32;
    let total = header_len + ft.len() as u32 + bt.len() as u32 + glb.len() as u32;

    let mut out = Vec::with_capacity(total as usize);
    out.extend_from_slice(b"b3dm");
    out.extend_from_slice(&1u32.to_le_bytes());
    out.extend_from_slice(&total.to_le_bytes());
    out.extend_from_slice(&(ft.len() as u32).to_le_bytes());
    out.extend_from_slice(&0u32.to_le_bytes());
    out.extend_from_slice(&(bt.len() as u32).to_le_bytes());
    out.extend_from_slice(&0u32.to_le_bytes());
    out.extend_from_slice(&ft);
    out.extend_from_slice(&bt);
    out.extend_from_slice(glb);
    debug_assert_eq!(out.len() as u32, total);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn le_u32(buf: &[u8], off: usize) -> u32 {
        u32::from_le_bytes(buf[off..off + 4].try_into().unwrap())
    }

    /// 与 feature（BATCHID）一一对应的测试来源标注。
    fn single_source() -> Vec<String> {
        vec!["Tile_+000_+000.osgb".to_string()]
    }

    #[test]
    fn b3dm_header_fields_consistent() {
        let mesh = sample_mesh();
        let b = build_b3dm_from_mesh(&mesh, &single_source());
        assert_eq!(&b[0..4], b"b3dm");
        assert_eq!(le_u32(&b, 4), 1); // version
        assert_eq!(le_u32(&b, 8), b.len() as u32); // byteLength == 实际长度
        let ft_len = le_u32(&b, 12) as usize;
        assert_eq!(le_u32(&b, 16), 0); // featureTableBinary
        let bt_len = le_u32(&b, 20) as usize;
        assert_eq!(le_u32(&b, 24), 0); // batchTableBinary
        // byteLength 自洽：28 header + FT JSON + BT JSON + GLB
        let glb_len = b.len() - 28 - ft_len - bt_len;
        assert!(glb_len > 0);
        // JSON 段均 4 字节对齐且可解析
        assert_eq!(ft_len % 4, 0);
        assert_eq!(bt_len % 4, 0);
        let ft: serde_json::Value = serde_json::from_slice(&b[28..28 + ft_len]).unwrap();
        assert_eq!(ft["BATCH_LENGTH"], 1);
        let bt: serde_json::Value =
            serde_json::from_slice(&b[28 + ft_len..28 + ft_len + bt_len]).unwrap();
        assert!(bt.get("_source").is_some());
    }

    fn sample_mesh() -> tangis_osgb::Mesh {
        tangis_osgb::Mesh {
            vertices: vec![[0.0; 3], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0]],
            indices: vec![0, 1, 2],
            normals: None,
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: None,
        }
    }

    #[test]
    fn glb_structure_legal() {
        let g = assemble_glb(
            // 4 字节对齐的假 JSON chunk 仅用于头部结构断言（调用方负责补齐）
            b"abcd",
            &[0u8; 8],
        );
        assert_eq!(&g[0..4], b"glTF"); // glTF 2.0 规范 magic
        assert_eq!(le_u32(&g, 4), 2); // version
        assert_eq!(le_u32(&g, 8), g.len() as u32); // 总长一致

        let json_len = le_u32(&g, 12);
        assert_eq!(&g[16..20], b"JSON"); // JSON chunkType 位于 16..20，数据自 20 起
        assert_eq!(json_len % 4, 0, "JSON chunk 必须 4 字节对齐");

        let bin_off = 20 + json_len as usize;
        let bin_len = le_u32(&g, bin_off);
        // chunkType 为 4 字节："BIN" + 尾部 0x00（规范值 0x004E4942）
        assert_eq!(&g[bin_off + 4..bin_off + 7], b"BIN");
        assert_eq!(g[bin_off + 7], 0x00);
        assert_eq!(bin_len % 4, 0, "BIN chunk 必须 4 字节对齐");
        assert_eq!(bin_len, 8);
        assert_eq!(bin_off + 8 + bin_len as usize, g.len());
    }

    fn gltf_of(b: &[u8]) -> serde_json::Value {
        // b3dm: 28B header + FT JSON + BT JSON + GLB；GLB: 12B header + JSON chunk
        let glb = glb_of(b);
        let json_len = le_u32(glb, 12) as usize;
        serde_json::from_slice(&glb[20..20 + json_len]).unwrap()
    }

    /// b3dm FeatureTable JSON（28B header 之后、4 字节对齐的 FT 段）。
    fn ft_of(b: &[u8]) -> serde_json::Value {
        let ft_len = le_u32(b, 12) as usize;
        serde_json::from_slice(&b[28..28 + ft_len]).unwrap()
    }

    /// b3dm BatchTable JSON（FT 段之后的 BT 段）。
    fn bt_of(b: &[u8]) -> serde_json::Value {
        let ft_len = le_u32(b, 12) as usize;
        let bt_len = le_u32(b, 20) as usize;
        serde_json::from_slice(&b[28 + ft_len..28 + ft_len + bt_len]).unwrap()
    }

    /// 从 b3dm 中切出内嵌 GLB 字节（28B header + FT JSON + BT JSON 之后全部）。
    fn glb_of(b: &[u8]) -> &[u8] {
        let ft_len = le_u32(b, 12) as usize;
        let bt_len = le_u32(b, 20) as usize;
        &b[28 + ft_len + bt_len..]
    }

    /// 按 accessor 声明解码 BIN 中的 u32 标量序列（经 primitive 精确定位）。
    fn decode_scalar_u32(v: &serde_json::Value, bin: &[u8], acc_idx: usize) -> Vec<u32> {
        let acc = &v["accessors"][acc_idx];
        assert_eq!(acc["type"], "SCALAR");
        assert_eq!(acc["componentType"], 5125);
        let bv = &v["bufferViews"][acc["bufferView"].as_u64().unwrap() as usize];
        let off = bv["byteOffset"].as_u64().unwrap() as usize;
        let count = acc["count"].as_u64().unwrap() as usize;
        (0..count)
            .map(|i| le_u32(bin, off + i * 4))
            .collect()
    }

    /// 从 GLB 解码 u32 索引（按 primitive indices 精确定位）。
    fn u32_indices_of(b: &[u8]) -> Vec<u32> {
        let v = gltf_of(b);
        let bin = bin_of(b);
        let acc_idx = v["meshes"][0]["primitives"][0]["indices"]
            .as_u64()
            .expect("primitive 必有 indices") as usize;
        decode_scalar_u32(&v, &bin, acc_idx)
    }

    fn bin_of(b: &[u8]) -> Vec<u8> {
        let glb = glb_of(b);
        let json_len = le_u32(glb, 12) as usize;
        let bin_off = 20 + json_len;
        let bin_len = le_u32(glb, bin_off) as usize;
        glb[bin_off + 8..bin_off + 8 + bin_len].to_vec()
    }

    /// 从 GLB 解码每顶点 _BATCHID（按 primitive attributes["_BATCHID"]
    /// 精确定位——indices 同为 SCALAR/5125，模糊匹配会误读绕序交换后的索引）。
    fn batch_ids_of_glb(b: &[u8]) -> Vec<u32> {
        let v = gltf_of(b);
        let bin = bin_of(b);
        let acc_idx = v["meshes"][0]["primitives"][0]["attributes"]["_BATCHID"]
            .as_u64()
            .expect("primitive 必有 _BATCHID 属性") as usize;
        decode_scalar_u32(&v, &bin, acc_idx)
    }

    /// FT/BT 布局正确性：解析自产 b3dm 的 FeatureTable / BatchTable JSON，
    /// 断言 BATCH_LENGTH、_BATCHID 属性与 per-feature 统计字段一致。
    #[test]
    fn b3dm_feature_and_batch_table_layout() {
        // 两个 feature：feature 边界与三角形边界对齐——顶点 0..3 属
        // feature 0（三角形 0），顶点 3..6 属 feature 1（三角形 1）。
        // 三角形归属按首顶点 BATCHID（build 内约定），语料不得跨 feature。
        let mesh = tangis_osgb::Mesh {
            vertices: vec![
                [0.0; 3],
                [1.0, 0.0, 0.0],
                [0.0, 1.0, 0.0],
                [1.0, 1.0, 1.0],
                [2.0, 1.0, 1.0],
                [1.0, 2.0, 1.0],
            ],
            indices: vec![0, 1, 2, 3, 4, 5],
            normals: None,
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: Some(vec![0, 0, 0, 1, 1, 1]),
        };
        let sources = vec!["Tile_A.osgb".to_string(), "Tile_B.osgb".to_string()];
        let b = build_b3dm_from_mesh(&mesh, &sources);

        // ---- FeatureTable ----
        let ft = ft_of(&b);
        assert_eq!(ft["BATCH_LENGTH"], 2, "BATCH_LENGTH = feature 数");

        // ---- BatchTable（列状数组，长度 = BATCH_LENGTH）----
        let bt = bt_of(&b);
        for key in ["_source", "_tile", "_feature_index", "_vertex_count", "_triangle_count"] {
            let col = bt[key].as_array().unwrap_or_else(|| panic!("{key} 应为列数组"));
            assert_eq!(col.len(), 2, "{key} 列长度 = BATCH_LENGTH");
        }
        assert!(bt["_source"][0]
            .as_str()
            .unwrap()
            .contains("生成元信息"), "_source 必须注明来源为生成元信息");
        assert_eq!(bt["_source"][1], bt["_source"][0]);
        assert_eq!(bt["_tile"][0], "Tile_A.osgb");
        assert_eq!(bt["_tile"][1], "Tile_B.osgb");
        assert_eq!(bt["_feature_index"][0], 0);
        assert_eq!(bt["_feature_index"][1], 1);
        assert_eq!(bt["_vertex_count"][0], 3);
        assert_eq!(bt["_vertex_count"][1], 3);
        assert_eq!(bt["_triangle_count"][0], 1);
        assert_eq!(bt["_triangle_count"][1], 1);

        // ---- GLB _BATCHID 属性 ----
        let v = gltf_of(&b);
        let prim = &v["meshes"][0]["primitives"][0];
        let batchid_acc = prim["attributes"]["_BATCHID"]
            .as_u64()
            .expect("_BATCHID 属性必须存在") as usize;
        let acc = &v["accessors"][batchid_acc];
        assert_eq!(acc["componentType"], 5125);
        assert_eq!(acc["count"], 6);
        assert_eq!(
            batch_ids_of_glb(&b),
            vec![0, 0, 0, 1, 1, 1],
            "BIN 中每顶点 BATCHID 与输入一致"
        );

        // ---- 来源数与 feature 数不符 → 明确报错 ----
        let err = build_b3dm(&mesh, None, &sources[..1]).unwrap_err();
        assert!(err.contains("不符"), "{err}");
    }

    /// 无 batch_ids 的网格（OBJ 源等）视为单 feature：BATCH_LENGTH=1，
    /// 每顶点 BATCHID 全 0。
    #[test]
    fn b3dm_without_batch_ids_is_single_feature() {
        let b = build_b3dm_from_mesh(&sample_mesh(), &single_source());
        assert_eq!(ft_of(&b)["BATCH_LENGTH"], 1);
        assert_eq!(batch_ids_of_glb(&b), vec![0, 0, 0]);
    }

    #[test]
    fn b3dm_from_mesh_embeds_real_geometry() {
        // 输入网格为内部 Z-up 约定（x/y 水平、z 高度）
        let mesh = tangis_osgb::Mesh {
            vertices: vec![
                [10.0, 20.0, 3.0],
                [-4.0, 2.5, 0.0],
                [0.0, 1.0, -7.5],
                [5.0, 5.0, 5.0],
            ],
            indices: vec![0, 1, 2, 2, 1, 3],
            normals: Some(vec![[0.0, 0.0, 1.0]; 4]),
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: None,
        };
        let b = build_b3dm_from_mesh(&mesh, &single_source());

        // b3dm 头自洽
        assert_eq!(&b[0..4], b"b3dm");
        assert_eq!(le_u32(&b, 8), b.len() as u32);

        let v = gltf_of(&b);
        let prim = &v["meshes"][0]["primitives"][0];
        assert_eq!(prim["mode"], 4);
        let pos_acc = &v["accessors"][0];
        assert_eq!(pos_acc["count"], 4);
        // min/max 按变换后（Z-up→Y-up：(x,y,z)→(x,z,−y)）数据计算：
        // 变换后顶点 = [10,3,−20] [−4,0,−2.5] [0,−7.5,−1] [5,5,−5]
        let min: Vec<f64> = pos_acc["min"].as_array().unwrap()
            .iter().map(|x| x.as_f64().unwrap()).collect();
        let max: Vec<f64> = pos_acc["max"].as_array().unwrap()
            .iter().map(|x| x.as_f64().unwrap()).collect();
        assert_eq!(min, vec![-4.0, -7.5, -20.0]);
        assert_eq!(max, vec![10.0, 5.0, -1.0]);

        // 索引 accessor：u32、count=6
        let idx_acc = &v["accessors"][2];
        assert_eq!(idx_acc["componentType"], 5125);
        assert_eq!(idx_acc["count"], 6);

        // BIN 内 POSITION 数据逐字节核对（bufferView 0，第一个顶点已转 Y-up = [10,3,−20]）
        let bin = bin_of(&b);
        let pos_view = &v["bufferViews"][0];
        let off = pos_view["byteOffset"].as_u64().unwrap() as usize;
        assert_eq!(10.0f32.to_le_bytes(), bin[off..off + 4]);
        assert_eq!(3.0f32.to_le_bytes(), bin[off + 4..off + 8]);
        assert_eq!((-20.0f32).to_le_bytes(), bin[off + 8..off + 12]);

        // NORMAL 存在时 attributes.NORMAL 指向 accessor 1
        assert_eq!(prim["attributes"]["NORMAL"], 1);
    }

    /// GLB POSITION 必须是 Y-up（水平 x/z、高度 +y）：
    /// - 对典型 Z-up 内部网格（水平 x∈[0,100]、y∈[−100,0]、高度 z∈[−2.38,21.78]），
    ///   accessor min/max 应呈「高度分量在 y」的形态（与 Cesium
    ///   Y_UP_TO_Z_UP 旋转及 tileset box 对齐）；
    /// - 变换可逆：对 BIN 中每个顶点施加逆旋转 (x,−z,y) 必须还原内部 Z-up 顶点。
    #[test]
    fn b3dm_glb_positions_are_y_up_and_transform_is_invertible() {
        let mesh = tangis_osgb::Mesh {
            vertices: vec![
                [0.0, -100.0, -2.38],
                [100.0, -100.0, 21.78],
                [100.0, 0.0, 21.78],
            ],
            indices: vec![0, 1, 2],
            normals: Some(vec![[0.0, 0.0, 1.0], [0.0, 1.0, 0.0], [1.0, 0.0, 0.0]]),
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: None,
        };
        let b = build_b3dm_from_mesh(&mesh, &single_source());

        let v = gltf_of(&b);
        let pos_acc = &v["accessors"][0];
        let min: Vec<f64> = pos_acc["min"].as_array().unwrap()
            .iter().map(|x| x.as_f64().unwrap()).collect();
        let max: Vec<f64> = pos_acc["max"].as_array().unwrap()
            .iter().map(|x| x.as_f64().unwrap()).collect();
        // 高度（内部 z ∈ [−2.38, 21.78]）落在 GLB 的 y 分量；水平在 x/z
        // （f32 舍入：−2.38 → −2.3800001；−0.0 == 0.0 但 vec! 相等比较含符号位差异，
        // 故用容差逐分量比较）
        let minmax: [(&Vec<f64>, [f64; 3]); 2] = [
            (&min, [0.0, -2.38, 0.0]),
            (&max, [100.0, 21.78, 100.0]),
        ];
        for (got, expect) in minmax {
            for a in 0..3 {
                assert!(
                    (got[a] - expect[a]).abs() < 1e-5,
                    "分量 {a}: {got:?} 期望 {expect:?}"
                );
            }
        }

        // BIN 逐顶点解码，逆旋转 (x,−z,y) 还原内部 Z-up 顶点；法线同步旋转
        let bin = bin_of(&b);
        let off = v["bufferViews"][0]["byteOffset"].as_u64().unwrap() as usize;
        for (i, expect) in mesh.vertices.iter().enumerate() {
            let o = off + i * 12;
            let f32_at = |k: usize| {
                f32::from_le_bytes(bin[o + k * 4..o + k * 4 + 4].try_into().unwrap())
            };
            let glb = [f32_at(0), f32_at(1), f32_at(2)];
            let back = [glb[0], -glb[2], glb[1]];
            for a in 0..3 {
                assert!(
                    (back[a] - expect[a]).abs() < 1e-5,
                    "逆变换不还原: {back:?} vs {expect:?}"
                );
            }
        }
    }

    #[test]
    fn b3dm_from_mesh_without_normals() {
        let mesh = tangis_osgb::Mesh {
            vertices: vec![[0.0; 3], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0]],
            indices: vec![0, 1, 2],
            normals: None,
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: None,
        };
        let b = build_b3dm_from_mesh(&mesh, &single_source());
        let v = gltf_of(&b);
        assert_eq!(v["meshes"][0]["primitives"][0]["indices"], 1);
        // POSITION(0) + indices(1) + _BATCHID(2)
        assert_eq!(v["accessors"].as_array().unwrap().len(), 3);
        // BIN = 36 顶点字节 + 12 _BATCHID 字节 + 12 索引字节
        assert_eq!(v["buffers"][0]["byteLength"], 60);
    }

    /// 纹理测试用真实 PNG（testdata/osgb/textured/textures/atlas.png，生成器产物）。
    fn atlas_png() -> Vec<u8> {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../../testdata/osgb/textured/textures/atlas.png");
        std::fs::read(path).expect("atlas.png 应存在")
    }

    fn textured_mesh() -> tangis_osgb::Mesh {
        tangis_osgb::Mesh {
            vertices: vec![[0.0; 3], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0]],
            indices: vec![0, 1, 2],
            normals: Some(vec![[0.0, 0.0, 1.0]; 3]),
            uvs: Some(vec![[0.0, 0.0], [1.0, 0.0], [0.0, 1.0]]),
            texture: Some("textures/atlas.png".into()),
            texture_inline: None,
            batch_ids: None,
        }
    }

    #[test]
    fn b3dm_textured_embeds_full_texture_pipeline() {
        let png = atlas_png();
        let mesh = textured_mesh();
        let b = build_b3dm_from_mesh_textured(
            &mesh,
            &TextureImage { bytes: &png, mime_type: "image/png" },
            &single_source(),
        )
        .unwrap();

        // b3dm/GLB 头自洽
        assert_eq!(&b[0..4], b"b3dm");
        assert_eq!(le_u32(&b, 8), b.len() as u32);

        let v = gltf_of(&b);
        let prim = &v["meshes"][0]["primitives"][0];
        // 纹理链路节点齐全且互相引用
        assert_eq!(prim["attributes"]["TEXCOORD_0"], 3); // POSITION/NORMAL/indices 后
        assert_eq!(prim["material"], 0);
        assert_eq!(v["textures"][0]["sampler"], 0);
        assert_eq!(v["textures"][0]["source"], 0);
        assert_eq!(v["images"][0]["mimeType"], "image/png");
        let img_view = v["images"][0]["bufferView"].as_u64().unwrap() as usize;
        // 图像字节完整嵌入 BIN（bufferView 指向真实 PNG 内容）
        let bin = bin_of(&b);
        let bv = &v["bufferViews"][img_view];
        let off = bv["byteOffset"].as_u64().unwrap() as usize;
        let len = bv["byteLength"].as_u64().unwrap() as usize;
        assert_eq!(len, png.len());
        assert_eq!(&bin[off..off + len], &png[..]);
        // 图像段之后 BIN 仍 4 字节对齐且 buffer 总长自洽
        assert_eq!(bin.len() % 4, 0);
        assert_eq!(v["buffers"][0]["byteLength"], bin.len());

        // material：pbr baseColorTexture + 系数明示
        let mat = &v["materials"][0];
        assert_eq!(mat["pbrMetallicRoughness"]["baseColorTexture"]["index"], 0);
        assert_eq!(mat["pbrMetallicRoughness"]["metallicFactor"], 0.0);
        assert_eq!(mat["pbrMetallicRoughness"]["roughnessFactor"], 1.0);
        assert_eq!(mat["doubleSided"], true);

        // TEXCOORD_0 数据逐字节核对（第一个 UV = [0,0]）
        let uv_view = &v["bufferViews"]
            [v["accessors"][prim["attributes"]["TEXCOORD_0"].as_u64().unwrap() as usize]
                ["bufferView"]
                .as_u64()
                .unwrap() as usize];
        let uv_off = uv_view["byteOffset"].as_u64().unwrap() as usize;
        assert_eq!(0.0f32.to_le_bytes(), bin[uv_off..uv_off + 4]);
    }

    #[test]
    fn b3dm_textured_rejects_missing_or_mismatched_uv() {
        let png = atlas_png();
        let mut mesh = textured_mesh();
        mesh.uvs = None;
        let err = build_b3dm_from_mesh_textured(
            &mesh,
            &TextureImage { bytes: &png, mime_type: "image/png" },
            &single_source(),
        )
        .unwrap_err();
        assert!(err.contains("无 UV 数组"), "{err}");

        let mut mesh = textured_mesh();
        mesh.uvs = Some(vec![[0.0, 0.0]; 2]); // 数量与顶点不符
        let err = build_b3dm_from_mesh_textured(
            &mesh,
            &TextureImage { bytes: &png, mime_type: "image/png" },
            &single_source(),
        )
        .unwrap_err();
        assert!(err.contains("UV 数不符"), "{err}");
    }

    #[test]
    fn b3dm_untextured_mesh_with_uvs_has_no_texture_nodes() {
        // 无纹理引用时即使带 UV 也不写任何纹理节点（UV 仅在绑定时输出）
        let mut mesh = textured_mesh();
        mesh.texture = None;
        let b = build_b3dm_from_mesh(&mesh, &single_source());
        let v = gltf_of(&b);
        assert!(v.get("images").is_none());
        assert!(v.get("materials").is_none());
        assert!(v["meshes"][0]["primitives"][0].get("material").is_none());
    }

    /// 绕序修复断言（缺陷 1 回归防护）。
    ///
    /// 源语料三角形从外部看为 CW，glTF 要求 front face = CCW（Cesium
    /// b3dm 默认 cull BACK，不翻则全部正面被剔除、模型不可见）。坐标
    /// 变换 `(x,z,−y)` 是 det = +1 的纯旋转，不改绕序，故修复方式是
    /// 写 GLB 时对每个三角形交换 `i[1]↔i[2]`。断言两层：
    ///
    /// 1. **交换确实发生且只交换一次**：GLB 索引 = 源索引逐三角形
    ///    `[a,b,c] → [a,c,b]`，长度不变；
    /// 2. **语义验证（首三角形叉积符号对比）**：源坐标系几何法向
    ///    `n_s = (b−a)×(c−a)` 经旋转映射 M（即 `y_up(n_s)`）后，与 GLB
    ///    实际三角形的几何法向 `n_g` 必须反向（点积 < 0）——GLB 绕序
    ///    恰与源绕序相反，证明每个三角形恰好被翻转一次。手工算例：
    ///    源 `[0,0,0]/[1,0,0]/[0,1,0]`，`n_s = (0,0,1)`，`M·n_s = (0,1,0)`；
    ///    GLB 顶点（交换后顺序 v0,v2,v1）= `[0,0,0]/[0,0,−1]/[1,0,0]`，
    ///    `n_g = (0,−1,0) = −M·n_s`，点积 = −1。若漏交换则点积 = +1，
    ///    测试失败。
    #[test]
    fn b3dm_winding_swapped_once_and_reverses_geometric_normal() {
        let mesh = tangis_osgb::Mesh {
            vertices: vec![
                [0.0, 0.0, 0.0],
                [1.0, 0.0, 0.0],
                [0.0, 1.0, 0.0],
                [1.0, 1.0, 2.0],
            ],
            indices: vec![0, 1, 2, 2, 1, 3, 0, 3, 1],
            normals: None,
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: None,
        };
        let b = build_b3dm_from_mesh(&mesh, &single_source());
        let out_idx = u32_indices_of(&b);

        // 断言 1：逐三角形 [a,b,c] → [a,c,b]，长度不变（只交换一次）
        assert_eq!(out_idx.len(), mesh.indices.len());
        for (src, out) in mesh.indices.chunks_exact(3).zip(out_idx.chunks_exact(3)) {
            assert_eq!(out, &[src[0], src[2], src[1]], "源 {src:?} → 输出 {out:?}");
        }

        // 断言 2：映射后的源法向与 GLB 实际法向同向（点积 > 0）
        let v = gltf_of(&b);
        let bin = bin_of(&b);
        let pos_view = &v["bufferViews"][0];
        let pos_off = pos_view["byteOffset"].as_u64().unwrap() as usize;
        let f32_at = |vi: usize, k: usize| {
            f32::from_le_bytes(bin[pos_off + vi * 12 + k * 4..pos_off + vi * 12 + k * 4 + 4]
                .try_into()
                .unwrap())
        };
        let sub = |p: usize, q: usize| {
            [
                f32_at(p, 0) - f32_at(q, 0),
                f32_at(p, 1) - f32_at(q, 1),
                f32_at(p, 2) - f32_at(q, 2),
            ]
        };
        let cross = |u: [f32; 3], w: [f32; 3]| {
            [
                u[1] * w[2] - u[2] * w[1],
                u[2] * w[0] - u[0] * w[2],
                u[0] * w[1] - u[1] * w[0],
            ]
        };
        let y_up = |n: [f32; 3]| [n[0], n[2], -n[1]];
        for t in mesh.indices.chunks_exact(3) {
            let (a, b, c) = (
                mesh.vertices[t[0] as usize],
                mesh.vertices[t[1] as usize],
                mesh.vertices[t[2] as usize],
            );
            let sub_src = |p: [f32; 3], q: [f32; 3]| [p[0] - q[0], p[1] - q[1], p[2] - q[2]];
            let n_s = cross(sub_src(b, a), sub_src(c, a));
            // GLB 中三角形顶点为交换后的 (t0, t2, t1)
            let v0 = t[0] as usize;
            let v1 = t[2] as usize;
            let v2 = t[1] as usize;
            let n_gl = cross(sub(v1, v0), sub(v2, v0));
            let expect = y_up(n_s);
            let dot: f32 = n_gl[0] * expect[0] + n_gl[1] * expect[1] + n_gl[2] * expect[2];
            assert!(
                dot < 0.0,
                "三角形 {t:?} GLB 绕序未相对源翻转: n_g={n_gl:?} M·n_s={expect:?} dot={dot}"
            );
        }
    }

    /// GLB chunk 头严格校验（缺陷 2 回归防护）。
    ///
    /// 浏览器严格解析器要求：声明的 JSON/BIN chunk length 必须与实际字节
    /// 数一致、GLB total length = 12 + (8+JSON) + (8+BIN)。历史上出现过
    /// 声明 BIN length 远超文件实际长度的畸形（浏览器读到 5,130,562 =
    /// 0x004E4942 恰为 "BIN\0" 的 LE 值，即 type 被当 length 读取的头错位
    /// 特征）。此测试逐字节解析自产 GLB，保证头与实际永不脱节。
    #[test]
    fn glb_chunk_headers_match_actual_bytes() {
        for b in [
            // 无纹理路径
            build_b3dm_from_mesh(&sample_mesh(), &single_source()),
            // 带纹理路径（图像段含任意长度 + 4 字节 pad）
            build_b3dm_from_mesh_textured(
                &textured_mesh(),
                &TextureImage { bytes: &atlas_png(), mime_type: "image/png" },
                &single_source(),
            )
            .unwrap(),
        ] {
            let g = glb_of(&b);
            let total = le_u32(g, 8) as usize;
            assert_eq!(total, g.len(), "GLB total length 必须等于实际字节数");

            let json_len = le_u32(g, 12) as usize;
            assert_eq!(&g[16..20], b"JSON");
            assert_eq!(json_len % 4, 0, "JSON chunk 必须 4 字节对齐");

            let bin_hdr = 20 + json_len;
            let bin_len = le_u32(g, bin_hdr) as usize;
            assert_eq!(&g[bin_hdr + 4..bin_hdr + 8], b"BIN\0");
            assert_eq!(bin_len % 4, 0, "BIN chunk 必须 4 字节对齐");

            // 严格解析器的核心断言：声明 length 与实际字节一致，
            // 总长 = 12B header + (8+len) + JSON + (8+len) + BIN
            assert_eq!(
                bin_hdr + 8 + bin_len,
                g.len(),
                "BIN chunk 声明长度必须精确覆盖到文件末尾"
            );
            assert_eq!(total, 12 + 8 + json_len + 8 + bin_len);

            // buffer.byteLength 不得超出 BIN chunk 实际容量
            let v = gltf_of(&b);
            assert!(
                v["buffers"][0]["byteLength"].as_u64().unwrap() as usize <= bin_len,
                "buffer.byteLength 超出 BIN chunk 实际容量"
            );
        }
    }
}

