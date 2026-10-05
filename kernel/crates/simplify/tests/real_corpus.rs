//! 真实语料基准（M2-F12a）：testdata 缺失时跳过，不阻塞 CI。
//!
//! 运行方式：`cargo test -p tangis-simplify --test real_corpus -- --nocapture`
//!
//! 报告内容：顶点/面数变化、单侧顶点采样 Hausdorff（原顶点 → 简化面）、
//! 算法耗时；多 tile 场景额外断言瓦片接缝顶点逐字不动。

use std::collections::HashSet;
use std::path::PathBuf;

use tangis_simplify::{approx_hausdorff, simplify, MeshData};

/// 极简 OBJ 读取（v / f 行；f 支持 `v`、`v/vt`、`v//vn`、`v/vt/vn`）。
fn load_obj(path: &std::path::Path) -> MeshData {
    let text = std::fs::read_to_string(path)
        .unwrap_or_else(|e| panic!("读取 {} 失败: {e}", path.display()));
    let mut positions = Vec::new();
    let mut indices = Vec::new();
    for line in text.lines() {
        if let Some(rest) = line.strip_prefix("v ") {
            let c: Vec<f32> =
                rest.split_whitespace().take(3).map(|s| s.parse().unwrap()).collect();
            positions.push([c[0], c[1], c[2]]);
        } else if let Some(rest) = line.strip_prefix("f ") {
            let mut face = [0u32; 3];
            for (i, tok) in rest.split_whitespace().take(3).enumerate() {
                let v = tok.split('/').next().unwrap().parse::<i64>().unwrap();
                let idx = if v > 0 { v - 1 } else { positions.len() as i64 + v };
                face[i] = idx as u32;
            }
            indices.extend_from_slice(&face);
        }
    }
    assert!(!positions.is_empty() && !indices.is_empty(), "{} 无几何", path.display());
    MeshData { positions, indices, normals: None, uvs: None, batch_ids: None }
}

/// 坐标量化键（接缝对照用；坐标为米级网格值，1e-4 米精度足够）。
fn pos_key(p: &[f32; 3]) -> [i64; 3] {
    [
        (p[0] * 1e4).round() as i64,
        (p[1] * 1e4).round() as i64,
        (p[2] * 1e4).round() as i64,
    ]
}

/// 基准跑一个 tile：打印统计并断言收敛与误差上界。
fn bench_tile(name: &str, src: &std::path::Path, ratios: &[f32], bound: f64) {
    let mesh = load_obj(src);
    let faces_in = mesh.indices.len() / 3;
    println!(
        "== {name}: 顶点 {}，面数 {faces_in}（{}）==",
        mesh.positions.len(),
        src.display()
    );
    for &r in ratios {
        let t0 = std::time::Instant::now();
        let (out, stats) = simplify(&mesh, r).unwrap();
        let ms = t0.elapsed().as_secs_f64() * 1000.0;
        let h = approx_hausdorff(&mesh, &out);
        let faces_out = out.indices.len() / 3;
        println!(
            "  ratio={r:<4} 顶点 {:>5}→{:>5}  面数 {:>5}→{:>5}  折叠 {:>3}  \
             Hausdorff {:.4} m  耗时 {:.2} ms",
            stats.vertices_in,
            stats.vertices_out,
            faces_in,
            faces_out,
            stats.collapses,
            h,
            ms
        );
        assert!(faces_out <= stats.target_faces, "面数 {faces_out} 超出目标 {}", stats.target_faces);
        assert!(h < bound, "{name} ratio={r}: Hausdorff {h} 应 < {bound}");
        // 输出索引合法且无退化面
        for f in out.indices.chunks_exact(3) {
            assert!(f[0] != f[1] && f[1] != f[2] && f[0] != f[2]);
        }
    }
}

/// 单 tile（289 顶点合成语料）。
#[test]
fn bench_single_tile_289() {
    let src = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/osgb/src/Tile_+000_+000.obj");
    if !src.is_file() {
        eprintln!("跳过：{} 不存在", src.display());
        return;
    }
    bench_tile("Tile_+000_+000（289 顶点）", &src, &[0.5, 0.2], 8.0);
}

/// 真实 osgconv 语料单 tile（1089 顶点，.osgb 解析；OSGB 为 GL y-up，
/// 简化与坐标朝向无关，直接用解析顶点）。
#[test]
fn bench_real_corpus_tile_1089() {
    let src = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/osgb-real/osgb/Tile_+000_+000.osgb");
    if !src.is_file() {
        eprintln!("跳过：{} 不存在", src.display());
        return;
    }
    let scene = tangis_osgb::parse_file(&src)
        .unwrap_or_else(|e| panic!("解析 {} 失败: {e}", src.display()));
    let merged = scene.merged().unwrap();
    let mesh = MeshData {
        positions: merged.vertices,
        indices: merged.indices,
        normals: None,
        uvs: None,
        batch_ids: None,
    };
    let faces_in = mesh.indices.len() / 3;
    println!(
        "== Tile_+000_+000（.osgb 1089 顶点）: 顶点 {}，面数 {faces_in}（{}）==",
        mesh.positions.len(),
        src.display()
    );
    assert_eq!(mesh.positions.len(), 1089);
    for &r in [0.5f32, 0.2].iter() {
        let t0 = std::time::Instant::now();
        let (out, stats) = simplify(&mesh, r).unwrap();
        let ms = t0.elapsed().as_secs_f64() * 1000.0;
        let h = approx_hausdorff(&mesh, &out);
        let faces_out = out.indices.len() / 3;
        println!(
            "  ratio={r:<4} 顶点 {:>5}→{:>5}  面数 {:>5}→{:>5}  折叠 {:>3}  \
             Hausdorff {:.4} m  耗时 {:.2} ms",
            stats.vertices_in,
            stats.vertices_out,
            faces_in,
            faces_out,
            stats.collapses,
            h,
            ms
        );
        assert!(faces_out <= stats.target_faces);
        assert!(h < 8.0, "ratio={r}: Hausdorff {h} 应 < 8.0");
    }
}

/// 多 tile（4 块相邻合成语料）：各自独立简化 + 接缝顶点逐字不动断言。
#[test]
fn bench_multi_tile_and_seam_protection() {
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/osgb/src");
    let tiles = ["Tile_+000_+000", "Tile_+000_+001", "Tile_+001_+000", "Tile_+001_+001"];
    let paths: Vec<PathBuf> = tiles.iter().map(|t| dir.join(format!("{t}.obj"))).collect();
    if paths.iter().any(|p| !p.is_file()) {
        eprintln!("跳过：{} 缺失", dir.display());
        return;
    }

    let originals: Vec<MeshData> = paths.iter().map(|p| load_obj(p)).collect();
    let simplified: Vec<MeshData> = originals
        .iter()
        .map(|m| simplify(m, 0.5).unwrap().0)
        .collect();

    let mut total_v_in = 0;
    let mut total_v_out = 0;
    let mut total_f_in = 0;
    let mut total_f_out = 0;
    let mut max_h = 0.0f64;
    let t0 = std::time::Instant::now();
    for (i, m) in originals.iter().enumerate() {
        let (out, stats) = simplify(m, 0.5).unwrap();
        let h = approx_hausdorff(m, &out);
        max_h = max_h.max(h);
        println!(
            "  {} 顶点 {:>4}→{:>4} 面数 {:>4}→{:>4} Hausdorff {:.4} m",
            tiles[i],
            stats.vertices_in,
            stats.vertices_out,
            m.indices.len() / 3,
            out.indices.len() / 3,
            h
        );
        total_v_in += stats.vertices_in;
        total_v_out += stats.vertices_out;
        total_f_in += m.indices.len() / 3;
        total_f_out += out.indices.len() / 3;
    }
    let ms = t0.elapsed().as_secs_f64() * 1000.0;
    println!(
        "== 4-tile 合计 ratio=0.5：顶点 {total_v_in}→{total_v_out}，面数 \
         {total_f_in}→{total_f_out}，max Hausdorff {max_h:.4} m，耗时 {ms:.2} ms =="
    );

    // 接缝保护：任意两块 tile 原始共享坐标（接缝/共角顶点）在两块
    // 各自简化后的输出中都必须逐字存在
    let key_sets: Vec<HashSet<[i64; 3]>> = originals
        .iter()
        .map(|m| m.positions.iter().map(pos_key).collect())
        .collect();
    let out_keys: Vec<HashSet<[i64; 3]>> = simplified
        .iter()
        .map(|m| m.positions.iter().map(pos_key).collect())
        .collect();
    for i in 0..tiles.len() {
        for j in (i + 1)..tiles.len() {
            let shared: Vec<&[i64; 3]> = key_sets[i]
                .intersection(&key_sets[j])
                .collect();
            if shared.is_empty() {
                continue;
            }
            let n_shared = shared.len();
            for k in shared {
                assert!(
                    out_keys[i].contains(k),
                    "{} 丢失接缝顶点 {k:?}（与 {} 共享）",
                    tiles[i],
                    tiles[j]
                );
                assert!(
                    out_keys[j].contains(k),
                    "{} 丢失接缝顶点 {k:?}（与 {} 共享）",
                    tiles[j],
                    tiles[i]
                );
            }
            println!(
                "  接缝 {}–{}：{n_shared} 个共享顶点全部原样保留",
                tiles[i],
                tiles[j],
            );
        }
    }
    assert!(max_h < 8.0, "多 tile Hausdorff {max_h} 应 < 8.0");
}
