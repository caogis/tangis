//! 端到端 fixture 测试：构造已知缺陷的语料，验证各检测项命中且干净语料零误报。

use std::path::PathBuf;

use tangis_osgb::Mesh;
use tangis_qc::{run_qc, LoadedTile, QcConfig};

fn tile(name: &str, mesh: Mesh) -> LoadedTile {
    LoadedTile { name: name.to_string(), path: PathBuf::from(format!("/{name}.osgb")), mesh }
}

fn mesh_from(verts: Vec<[f32; 3]>, faces: &[u32]) -> Mesh {
    Mesh { vertices: verts, indices: faces.to_vec(), ..Mesh::default() }
}

#[test]
fn degenerate_fixture_hit() {
    let verts = vec![
        [0.0, 0.0, 0.0],
        [1.0, 0.0, 0.0],
        [0.0, 1.0, 0.0],
        [2.0, 0.0, 0.0], // 与 0/1 共线
    ];
    let faces = [0u32, 1, 2, 0, 1, 3, 0, 0, 2, 2, 1, 0];
    let rep = run_qc(vec![tile("bad", mesh_from(verts, &faces))], &QcConfig::default(), "mem");
    let d = rep.tiles[0].degenerate.as_ref().unwrap();
    assert_eq!(d.zero_area, 2, "共线面 + 重复顶点面（叉积同为零）");
    assert_eq!(d.repeated_vertex, 1);
    assert_eq!(d.duplicate_face, 1, "[2,1,0] 与 [0,1,2] 排序后重复");
    assert!(!rep.summary.passed);
}

#[test]
fn normal_flip_fixture_hit() {
    let verts = vec![[0.0; 3], [1.0, 0.0, 0.0], [1.0, 1.0, 0.0], [0.0, 1.0, 0.0]];
    let faces = [0u32, 1, 2, 0, 3, 2]; // 第二面绕序翻转
    let rep = run_qc(vec![tile("flip", mesh_from(verts, &faces))], &QcConfig::default(), "mem");
    let n = rep.tiles[0].normal_flip.as_ref().unwrap();
    assert_eq!(n.flipped_edges, 1);
    assert_eq!(n.isolated_faces, 2);
    assert!(n.isolated_flip_rate > 0.99);
}

#[test]
fn floating_fixture_hit() {
    // 地面（6×6，顶点间距小于 2 体素边长保证单分量）+ z=50 独立小三角
    let mut verts = vec![[0.0; 3], [6.0, 0.0, 0.0], [6.0, 6.0, 0.0], [0.0, 6.0, 0.0]];
    let mut faces: Vec<u32> = vec![0, 1, 2, 0, 2, 3];
    let base = verts.len() as u32;
    verts.extend_from_slice(&[[3.0, 3.0, 50.0], [3.5, 3.0, 50.0], [3.0, 3.5, 50.0]]);
    faces.extend_from_slice(&[base, base + 1, base + 2]);
    let rep = run_qc(vec![tile("float", mesh_from(verts, &faces))], &QcConfig::default(), "mem");
    let f = rep.tiles[0].floating.as_ref().unwrap();
    assert_eq!(f.components, 2);
    assert_eq!(f.flagged.len(), 1);
    assert!(f.flagged[0].clearance > 49.0);
}

#[test]
fn crack_fixture_hit_and_clean() {
    // 两个相邻 10×10 tile：干净拼缝 vs 抬高 0.8 m 的裂缝缝
    let build = |dz: f64| -> (Mesh, Mesh) {
        let (mut av, mut bv, mut af, mut bf) = (Vec::new(), Vec::new(), Vec::new(), Vec::new());
        for i in 0..11u32 {
            let t = i as f64;
            let base_a = av.len() as u32;
            av.extend_from_slice(&[[t as f32, 0.0, 0.0], [t as f32, 10.0, 0.0]]);
            let z_b = if i == 0 { dz as f32 } else { 0.0 };
            let base_b = bv.len() as u32;
            bv.extend_from_slice(&[[t as f32, 0.0, z_b], [t as f32, 10.0, z_b]]);
            if i < 10 {
                af.extend_from_slice(&[base_a, base_a + 1, base_a + 2]);
                af.extend_from_slice(&[base_a + 1, base_a + 3, base_a + 2]);
                bf.extend_from_slice(&[base_b, base_b + 1, base_b + 2]);
                bf.extend_from_slice(&[base_b + 1, base_b + 3, base_b + 2]);
            }
        }
        (mesh_from(av, &af), mesh_from(bv, &bf))
    };

    // 干净拼缝零告警
    let (a, b) = build(0.0);
    let rep = run_qc(
        vec![tile("Tile_+000_+000", a), tile("Tile_+001_+000", b)],
        &QcConfig::default(),
        "mem",
    );
    assert_eq!(rep.cracks.len(), 1);
    assert_eq!(rep.cracks[0].gaps_over_threshold, 0, "干净拼缝不应报裂缝");
    assert!(rep.summary.passed);

    // 0.8 m 高差 > 默认阈值 0.5 → 命中 + 位置在共享平面
    let (a, b) = build(0.8);
    let rep = run_qc(
        vec![tile("Tile_+000_+000", a), tile("Tile_+001_+000", b)],
        &QcConfig::default(),
        "mem",
    );
    let c = &rep.cracks[0];
    assert_eq!(c.gaps_over_threshold, 2);
    assert!((c.max_gap - 0.8).abs() < 1e-9);
    assert_eq!(c.positions.len(), 2);
    assert!((c.positions[0].x - 10.0).abs() < 1e-9, "裂缝 x 应在共享平面");
    assert!(!rep.summary.passed);
}

#[test]
fn self_intersect_fixture_hit() {
    let verts = vec![
        [-10.0, -10.0, 0.0],
        [10.0, -10.0, 0.0],
        [0.0, 10.0, 0.0],
        [0.0, 0.0, -10.0],
        [0.0, 0.0, 10.0],
        [5.0, 0.0, 0.0],
    ];
    let faces = [0u32, 1, 2, 3, 4, 5];
    let rep = run_qc(vec![tile("x", mesh_from(verts, &faces))], &QcConfig::default(), "mem");
    let s = rep.tiles[0].self_intersect.as_ref().unwrap();
    assert_eq!(s.intersections, 1);
    assert_eq!(s.coverage, 1.0);
}

#[test]
fn clean_multi_tile_fixture_all_pass() {
    // 干净 2×2 tile 场景：顶点一律 tile 局部坐标（0..10），全局位置由
    // 裂缝检测按名字下标 × 跨度推导（真实 OSGB 语料即此布局）
    let mut tiles = Vec::new();
    for name in ["Tile_+000_+000", "Tile_+001_+000", "Tile_+000_+001", "Tile_+001_+001"] {
        let verts = vec![[0.0, 0.0, 0.0], [10.0, 0.0, 0.0], [10.0, 10.0, 0.0], [0.0, 10.0, 0.0]];
        tiles.push(tile(name, mesh_from(verts, &[0u32, 1, 2, 0, 2, 3])));
    }
    let rep = run_qc(tiles, &QcConfig::default(), "mem");
    assert_eq!(rep.cracks.len(), 4, "2×2 网格应有 4 对相邻配对");
    assert!(rep.summary.passed, "干净语料必须零告警: {:?}", rep.summary);
    assert!(rep.cracks.iter().all(|c| c.gaps_over_threshold == 0));
}

#[test]
fn real_corpus_smoke() {
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/osgb-real/osgb");
    if !dir.join("Tile_+000_+000.osgb").is_file() {
        eprintln!("跳过：真实语料不存在");
        return;
    }
    let rep = tangis_qc::run_qc_from_source(&dir, &QcConfig::default()).unwrap();
    assert_eq!(rep.tile_count, 80);
    // 报告可序列化且自洽
    let json = serde_json::to_string(&rep).unwrap();
    assert!(json.contains("\"summary\""));
    // 真实语料布局为 120 m 间距 + 20 m 街道沟槽（全局坐标）：无共享边界，
    // 包围盒邻接模式如实产出零配对（见 docs/qc-report-README.md 说明）
    assert_eq!(rep.summary.crack_pairs_checked, 0);
    assert_eq!(rep.crack_tiles_unpaired, 80);
}
