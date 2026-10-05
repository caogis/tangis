//! 真实语料端到端解析测试（osgconv 3.6.5 产出）。
//!
//! 语料位于仓库 `testdata/osgb-real/osgb/`（只读，80 个 tile，含 PNG 纹理
//! atlas）；缺失时本组测试整体跳过（不 fail CI）。
//! 协议字节级验证由 `testdata/osgb-real/verify_protocol.py` 承担；
//! 本文件验证 Rust 解析器的语义正确性。

use std::path::PathBuf;

use tangis_osgb::{parse_bytes_traced, parse_file};

fn corpus_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/osgb-real")
}

fn osgb_dir() -> PathBuf {
    corpus_dir().join("osgb")
}

fn corpus_tiles() -> Vec<PathBuf> {
    let mut tiles: Vec<PathBuf> = std::fs::read_dir(osgb_dir())
        .map(|rd| {
            rd.filter_map(|e| e.ok())
                .map(|e| e.path())
                .filter(|p| p.extension().and_then(|e| e.to_str()) == Some("osgb"))
                .collect()
        })
        .unwrap_or_default();
    tiles.sort();
    tiles
}

fn require_corpus() -> Option<Vec<PathBuf>> {
    let tiles = corpus_tiles();
    if tiles.is_empty() {
        eprintln!("跳过：真实语料 testdata/osgb-real/osgb/ 不存在");
        return None;
    }
    Some(tiles)
}

#[test]
fn parses_all_real_tiles() {
    let Some(tiles) = require_corpus() else {
        return;
    };
    assert_eq!(tiles.len(), 80, "语料应含 80 个 tile");
    for path in &tiles {
        let name = path.file_name().unwrap().to_string_lossy();
        let (scene, objects) = parse_bytes_traced(&std::fs::read(path).unwrap())
            .unwrap_or_else(|e| panic!("解析 {name} 失败: {e}"));
        assert!(!scene.meshes.is_empty(), "{name}: 应至少 1 个 Geometry");
        for (i, m) in scene.meshes.iter().enumerate() {
            assert!(!m.vertices.is_empty(), "{name}: 顶点不应为空");
            assert!(!m.indices.is_empty(), "{name}: 索引不应为空");
            // BATCHID：每 Geometry 一个 feature id，与顶点一一对应
            let ids = m.batch_ids.as_ref().expect("{name}: batch_ids 应存在");
            assert_eq!(ids.len(), m.vertices.len(), "{name}: BATCHID 应与顶点一一对应");
            assert!(ids.iter().all(|&b| b == i as u32), "{name}: mesh {i} 的 BATCHID 应全为 {i}");
            // 真实语料为 EXTERNAL 图像：无内嵌纹理字节
            if let Some(t) = &m.texture_inline {
                match t {
                    tangis_osgb::InlineTexture::EncodedFile(b) => eprintln!(
                        "DBG {name} mesh{i}: EncodedFile {}B head={:02x?}",
                        b.len(), &b[..b.len().min(8)]
                    ),
                    tangis_osgb::InlineTexture::RawPixels(r) => eprintln!(
                        "DBG {name} mesh{i}: RawPixels {}x{} fmt={:#x} dt={:#x} {}B",
                        r.s, r.t, r.pixel_format, r.data_type, r.data.len()
                    ),
                }
            }
            if m.texture_inline.is_some() { eprintln!("STAT {name} mesh{i} has_inline"); }
            let max_idx = m.indices.iter().copied().max().unwrap();
            assert!(
                (max_idx as usize) < m.vertices.len(),
                "{name}: 索引 {max_idx} 越界（顶点 {}）",
                m.vertices.len()
            );
            if let Some(uvs) = &m.uvs {
                assert_eq!(uvs.len(), m.vertices.len(), "{name}: UV 应与顶点一一对应");
            }
            if let Some(ns) = &m.normals {
                assert_eq!(ns.len(), m.vertices.len(), "{name}: 法线应与顶点一一对应");
            }
        }
        // 图像写出模式：语料混合 INLINE_DATA 与 EXTERNAL——
        // 12 个 tile（Tile_+000..+007、TileC_+000..+003）各 1 个 Geometry
        // 内嵌原始像素（RawPixels），其余 68 个 tile 为 EXTERNAL（无内嵌字节）。
        let inline_meshes: Vec<_> = scene
            .meshes
            .iter()
            .filter(|m| m.texture_inline.is_some())
            .collect();
        let expect_inline = name.starts_with("Tile_+00")
            || name.starts_with("TileC_+00");
        if expect_inline {
            assert_eq!(inline_meshes.len(), 1, "{name}: 应恰有 1 个内嵌纹理 Geometry");
        }
        for t in &inline_meshes {
            match t.texture_inline.as_ref().unwrap() {
                tangis_osgb::InlineTexture::EncodedFile(_) => {
                    panic!("{name}: 语料应为 INLINE_DATA（RawPixels），非 EncodedFile")
                }
                tangis_osgb::InlineTexture::RawPixels(r) => {
                    assert_eq!(r.origin, 0, "{name}: origin 应为 BOTTOM_LEFT");
                    assert_eq!((r.s, r.t), (256, 256), "{name}: RawPixels 尺寸");
                    assert_eq!(r.pixel_format, 0x1907, "{name}: 应为 GL_RGB");
                    assert_eq!(r.data_type, 0x1401, "{name}: 应为 GL_UNSIGNED_BYTE");
                    assert_eq!(
                        r.data.len(),
                        r.s as usize * r.t as usize * 3,
                        "{name}: 像素数据长度 = s*t*3"
                    );
                }
            }
        }
        // 语料不应出现未知类需要跳过
        let skipped: Vec<_> = objects.iter().filter(|o| o.skipped).collect();
        assert!(skipped.is_empty(), "{name}: 意外跳过 {skipped:?}");
    }
}

#[test]
fn first_tile_has_expected_object_tree_and_texture() {
    let Some(_) = require_corpus() else {
        return;
    };
    // 明确锚定 Tile_ 组首个文件（语料另含 TileC_* 低密度组，结构同构）
    let data = std::fs::read(osgb_dir().join("Tile_+000_+000.osgb")).unwrap();
    let (scene, objects) = parse_bytes_traced(&data).unwrap();

    let classes: Vec<&str> = objects.iter().map(|o| o.class.as_str()).collect();
    // 每 tile：Group→Geode→Geometry→StateSet→Material→Texture2D（含 Image 特化）
    // →DrawElementsUShort→EBO→Vec3Array(顶点)+VBO→Vec3Array(法线,VBO引用帧)
    // →Vec2Array(UV,VBO引用帧)
    assert_eq!(
        classes,
        vec![
            "osg::Group",
            "osg::Geode",
            "osg::Geometry",
            "osg::StateSet",
            "osg::Material",
            "osg::Texture2D",
            "osg::DrawElementsUShort",
            "osg::ElementBufferObject",
            "osg::Vec3Array",
            "osg::VertexBufferObject",
            "osg::Vec3Array",
            "osg::VertexBufferObject",
            "osg::Vec2Array",
            "osg::VertexBufferObject",
        ],
        "对象树与逆向结果不符"
    );

    let mesh = &scene.meshes[0];
    // atlas256 语料：33×33 网格 = 1089 顶点，32×32×2 = 2048 三角形
    assert_eq!(mesh.vertices.len(), 1089);
    assert_eq!(mesh.indices.len(), 2048 * 3);
    assert_eq!(mesh.normals.as_ref().unwrap().len(), 1089);
    assert_eq!(mesh.uvs.as_ref().unwrap().len(), 1089);
    // UV 归一化覆盖 [0,1]
    let uvs = mesh.uvs.as_ref().unwrap();
    assert!(uvs.iter().all(|uv| (0.0..=1.0).contains(&uv[0]) && (0.0..=1.0).contains(&uv[1])));
    // 纹理引用：Image FileName（相对 OSGB 所在目录）
    assert_eq!(
        mesh.texture.as_deref(),
        Some("objs/../textures/atlas256.png"),
        "纹理引用应为 Image FileName"
    );
}

#[test]
fn tile_vertices_are_local_meters_zup() {
    let Some(_) = require_corpus() else {
        return;
    };
    // Tile_ 组：100m tile；TileC_ 组：200m tile（结构同构，密度不同）
    // 真实 osgconv 语料为 y-up：水平在 x/z、高程起伏在 y
    for (name, size) in [("Tile_+000_+000.osgb", 100.0f32), ("TileC_+000_+000.osgb", 200.0)] {
        let scene = parse_file(&osgb_dir().join(name)).unwrap();
        let mesh = scene.merged().unwrap();
        let (min, max) = mesh.bounds().unwrap();
        assert!(min[0] >= -1.0 && max[0] <= size + 1.0, "{name}: x 范围异常: {min:?}..{max:?}");
        assert!(min[2] >= -1.0 && max[2] <= size + 1.0, "{name}: z 范围异常: {min:?}..{max:?}");
        assert!(min[1] >= -10.0 && max[1] <= 10.0, "{name}: y（高程）范围异常: {min:?}..{max:?}");
    }
}

#[test]
fn rejects_truncated_data() {
    let Some(tiles) = require_corpus() else {
        return;
    };
    let data = std::fs::read(&tiles[0]).unwrap();
    for cut in [0usize, 5, 19, 20, 24, 30, 100, data.len() - 1] {
        let err = tangis_osgb::parse_bytes(&data[..cut]).unwrap_err();
        assert!(!err.to_string().is_empty(), "截断到 {cut} 应有明确错误");
    }
    assert!(tangis_osgb::parse_bytes(&data).is_ok());
}

#[test]
fn header_of_real_corpus_is_version_161() {
    let Some(tiles) = require_corpus() else {
        return;
    };
    let data = std::fs::read(&tiles[0]).unwrap();
    let mut r = tangis_osgb::Reader::new(&data);
    let h = tangis_osgb::Header::parse(&mut r).unwrap();
    assert_eq!(h.version, 161);
    assert_eq!(h.write_type, 1);
    assert!(!h.is_compressed());
    assert!(h.has_binary_brackets());
}

// ---- zlib 压缩流：把真实 tile 原地重压缩（compressorName="zlib"）后必须
//      解析出完全一致的场景（数据真实、非合成）----

#[test]
fn parses_zlib_recompressed_tiles_identically() {
    let Some(tiles) = require_corpus() else {
        return;
    };
    use std::io::Write as _;
    for path in tiles.iter().take(4) {
        let name = path.file_name().unwrap().to_string_lossy();
        let plain_bytes = std::fs::read(path).unwrap();
        let plain = parse_bytes_traced(&plain_bytes).unwrap();

        // 切头：20 字节固定头 + compressorName 字符串，其后是对象流
        let mut hr = tangis_osgb::Reader::new(&plain_bytes);
        let header = tangis_osgb::Header::parse(&mut hr).unwrap();
        assert!(!header.is_compressed());
        let body = &plain_bytes[hr.pos()..];

        // 重写头：attributes 不变（robust brackets），compressorName="zlib"
        let mut zipped: Vec<u8> = Vec::new();
        zipped.extend_from_slice(&plain_bytes[..20]);
        zipped.extend_from_slice(&4u32.to_le_bytes());
        zipped.extend_from_slice(b"zlib");
        let mut enc = flate2::write::ZlibEncoder::new(Vec::new(), flate2::Compression::default());
        enc.write_all(body).unwrap();
        zipped.extend_from_slice(&enc.finish().unwrap());

        let zres = parse_bytes_traced(&zipped)
            .unwrap_or_else(|e| panic!("{name}: zlib 重压缩版应可解析: {e}"));
        assert_eq!(plain, zres, "{name}: 压缩与未压缩解析结果必须完全一致");
    }
}

#[test]
fn corrupt_zlib_stream_reports_clear_error() {
    let Some(tiles) = require_corpus() else {
        return;
    };
    use std::io::Write as _;
    let plain_bytes = std::fs::read(&tiles[0]).unwrap();
    let mut hr = tangis_osgb::Reader::new(&plain_bytes);
    let header = tangis_osgb::Header::parse(&mut hr).unwrap();
    let body = &plain_bytes[hr.pos()..];
    assert!(!header.is_compressed());

    let mut zipped: Vec<u8> = Vec::new();
    zipped.extend_from_slice(&plain_bytes[..20]);
    zipped.extend_from_slice(&4u32.to_le_bytes());
    zipped.extend_from_slice(b"zlib");
    let mut enc = flate2::write::ZlibEncoder::new(Vec::new(), flate2::Compression::default());
    enc.write_all(body).unwrap();
    let payload = enc.finish().unwrap();
    let zstart = zipped.len();
    zipped.extend_from_slice(&payload);
    // 破坏 zlib 流数据区（跳过 2 字节 zlib 头）
    for b in &mut zipped[zstart + 2..zstart + 10] {
        *b ^= 0xff;
    }
    let err = tangis_osgb::parse_bytes(&zipped).unwrap_err();
    assert!(
        err.to_string().contains("解压失败") || err.to_string().contains("压缩器"),
        "损坏 zlib 流应报解压错误，实际 {err}"
    );
}
