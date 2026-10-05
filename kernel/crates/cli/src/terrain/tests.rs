//! terrain2tiles 测试：tangis_geo 写 GeoTIFF fixture → 生产 → 解码 .terrain 校验。

use tangis_geo::tiff::{self, GeoTags, Raster, Samples};

use super::qmesh;
use super::terrain_zoom_range;
use super::{cmd_terrain2tiles, gunzip, TerrainArgs};

/// F32 DEM fixture：高程 = 100 + col + 2*row，64x64，1e-4°/px，
/// tiepoint (0,0,0,116.0,41.0,0.0) → bbox lon[116.0,116.0064] lat[40.9936,41.0]。
/// `epsg` 为 GeographicType GeoKey（4326 或 4490）。
fn dem_f32_fixture_epsg(epsg: u16) -> Vec<u8> {
    let (w, h) = (64u32, 64u32);
    let values: Vec<f32> = (0..h)
        .flat_map(|row| (0..w).map(move |col| 100.0 + col as f32 + 2.0 * row as f32))
        .collect();
    let raster = Raster {
        width: w,
        height: h,
        samples: Samples::F32(values),
    };
    let geo = GeoTags {
        pixel_scale: [1e-4, 1e-4, 0.0],
        tiepoint: vec![0.0, 0.0, 0.0, 116.0, 41.0, 0.0],
        geo_keys: vec![1, 1, 0, 2, 1024, 0, 1, 2, 2048, 0, 1, epsg],
        geo_ascii_params: None,
    };
    tiff::write_geo_tiff(&raster, &geo).expect("fixture 生成失败")
}

fn dem_f32_fixture() -> Vec<u8> {
    dem_f32_fixture_epsg(4326)
}

fn terrain_args(dir: &std::path::Path, tif_bytes: &[u8], min_z: u32, max_z: u32) -> TerrainArgs {
    let src = dir.join("dem.tif");
    std::fs::write(&src, tif_bytes).unwrap();
    TerrainArgs {
        source: src,
        output: dir.join("out"),
        min_zoom: Some(min_z),
        max_zoom: Some(max_z),
        grid_size: 33,
        layer_metadata: None,
        cancel_file: None,
    }
}

/// 解码 .terrain 原始字节为结构化数据（校验用独立实现，不依赖编码器内部）。
struct DecodedTile {
    center: [f64; 3],
    radius: f64,
    horizon: [f64; 3],
    u: Vec<u16>,
    v: Vec<u16>,
    h: Vec<u16>,
    indices: Vec<u32>,
    edges: [Vec<u32>; 4],
}

fn decode_terrain(raw: &[u8]) -> DecodedTile {
    assert_eq!(raw.len() % 2, 0);
    let f64_at = |o: usize| f64::from_le_bytes(raw[o..o + 8].try_into().unwrap());
    let center = [f64_at(0), f64_at(8), f64_at(16)];
    let radius = f64_at(48);
    let horizon = [f64_at(56), f64_at(64), f64_at(72)];
    let mut o = 80;
    let rd_u32 = |o: &mut usize| {
        let v = u32::from_le_bytes(raw[*o..*o + 4].try_into().unwrap());
        *o += 4;
        v
    };
    let rd_u16 = |o: &mut usize| {
        let v = u16::from_le_bytes(raw[*o..*o + 2].try_into().unwrap());
        *o += 2;
        v
    };

    let n = rd_u32(&mut o) as usize;
    let mut u = Vec::with_capacity(n);
    let mut v = Vec::with_capacity(n);
    let mut h = Vec::with_capacity(n);
    let mut prev = [0i32; 3];
    for _ in 0..n {
        for (i, dst) in [&mut u, &mut v, &mut h].into_iter().enumerate() {
            let code = rd_u16(&mut o);
            let q = qmesh::zigzag_decode_u16(code) + prev[i];
            prev[i] = q;
            dst.push(q as u16);
        }
    }
    let tri = rd_u32(&mut o) as usize;
    let mut codes = Vec::with_capacity(tri * 3);
    for _ in 0..tri * 3 {
        codes.push(rd_u16(&mut o));
    }
    let indices = qmesh::hwm_decode(&codes);
    let mut edges = [Vec::new(), Vec::new(), Vec::new(), Vec::new()];
    for edge in &mut edges {
        let count = rd_u32(&mut o) as usize;
        let mut codes = Vec::with_capacity(count);
        for _ in 0..count {
            codes.push(rd_u16(&mut o));
        }
        *edge = qmesh::hwm_decode(&codes);
    }
    assert_eq!(o, raw.len(), "字节应恰好消费完");
    DecodedTile {
        center,
        radius,
        horizon,
        u,
        v,
        h,
        indices,
        edges,
    }
}

#[test]
fn terrain_end_to_end_layout_and_mesh() {
    let dir = std::env::temp_dir().join(format!("tangis-terrain-e2e-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    let args = terrain_args(&dir, &dem_f32_fixture(), 0, 1);
    let summary = cmd_terrain2tiles(&args).unwrap();

    // ---- layer.json（Cesium 契约） ----
    let layer: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(dir.join("out/layer.json")).unwrap())
            .unwrap();
    assert_eq!(layer["format"], "quantized-mesh-1.0");
    assert_eq!(layer["profile"], "tms");
    assert_eq!(layer["projections"][0], "EPSG:4326");
    assert_eq!(layer["tiles"][0], "{z}/{x}/{y}.terrain?v=1.0.0");
    assert_eq!(layer["available"][0]["start"]["level"], 0);
    assert_eq!(layer["available"][0]["end"]["level"], 1);
    let bounds = layer["bounds"].as_array().unwrap();
    assert!((bounds[0].as_f64().unwrap() - 116.0).abs() < 1e-9);
    assert!((bounds[3].as_f64().unwrap() - 41.0).abs() < 1e-9);
    assert_eq!(summary.bounds[0], bounds[0].as_f64().unwrap());

    // ---- TMS 布局：bbox 在北半球东经 → z1 命中 x=1 行，XYZ y=0（北半）→ y_tms = 1 ----
    assert!(dir.join("out/0/0/0.terrain").exists());
    assert!(dir.join("out/1/1/1.terrain").exists());
    assert_eq!(summary.tiles_written, 2, "z0 一张 + z1 东北一张");
    // 不应存在西半球或南半球的瓦片
    assert!(!dir.join("out/1/0/1.terrain").exists());
    assert!(!dir.join("out/1/1/0.terrain").exists());

    // ---- 解码 z0 全世界瓦片 ----
    let gzip = std::fs::read(dir.join("out/0/0/0.terrain")).unwrap();
    let raw = gunzip(&gzip);
    let tile = decode_terrain(&raw);

    let g = 33usize;
    let n = g * g;
    assert_eq!(tile.u.len(), n);
    assert_eq!(tile.indices.len(), (g - 1) * (g - 1) * 6);
    // 顶点全在量化值域内
    for i in 0..n {
        assert!(tile.u[i] <= 32767 && tile.v[i] <= 32767 && tile.h[i] <= 32767);
    }
    // 索引全部合法且首三角形为网格左上（v00=0）
    assert!(tile.indices.iter().all(|&i| (i as usize) < n));
    assert_eq!(&tile.indices[0..3], &[0, g as u32, 1]);

    // ---- 高度量化回读：NW 角（j=0,i=0）= hmin=100，SE 角（j=32,i=32）= hmax ----
    assert_eq!(tile.h[0], 0, "NW 角应为最低高程量化 0");
    let se = (g - 1) * g + (g - 1);
    let hspan = summary.height_max - summary.height_min;
    assert_eq!(tile.h[se], 32767, "SE 角应为最高高程量化满值");
    // 摘要高度与网格角点一致（fixture: 100..289，全局 = z0 瓦片覆盖全域）
    assert!((summary.height_min - 100.0).abs() < 1e-6, "{}", summary.height_min);
    assert!((summary.height_max - 289.0).abs() < 1e-6, "{}", summary.height_max);
    assert!(hspan > 100.0);

    // ---- 边界索引：四边各 g 个，west 南→北首项 = (g-1)*g ----
    for e in &tile.edges {
        assert_eq!(e.len(), g);
    }
    assert_eq!(tile.edges[0][0], ((g - 1) * g) as u32);
    assert_eq!(tile.edges[0][g - 1], 0);
    assert_eq!(tile.edges[3][0], 0);
    assert_eq!(tile.edges[3][g - 1], (g - 1) as u32);

    // ---- header：center 有限、半径合理（< 半个地球）、horizon 有限 ----
    assert!(tile.center.iter().all(|c| c.is_finite()));
    assert!(tile.radius > 0.0 && tile.radius < 2.0e7, "{}", tile.radius);
    assert!(tile.horizon.iter().all(|c| c.is_finite()));
    // z0 中心 = 赤道本初子午线、高程 (100+289)/2 → ECEF (≈a+194.5, ≈0, ≈0)
    assert!((tile.center[0] - (qmesh::EARTH_A + 194.5)).abs() < 1.0, "{:?}", tile.center);
    assert!(tile.center[1].abs() < 1e-3 && tile.center[2].abs() < 1.0, "{:?}", tile.center);

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn terrain_rejects_bad_crs_and_zoom() {
    let dir = std::env::temp_dir().join(format!("tangis-terrain-bad-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    // UTM 50N（32650）→ 明确报错
    let (w, h) = (8u32, 8u32);
    let raster = Raster {
        width: w,
        height: h,
        samples: Samples::F32(vec![0.0; (w * h) as usize]),
    };
    let geo = GeoTags {
        pixel_scale: [1.0, 1.0, 0.0],
        tiepoint: vec![0.0, 0.0, 0.0, 500000.0, 4000000.0, 0.0],
        geo_keys: vec![1, 1, 0, 1, 3072, 0, 1, 32650],
        geo_ascii_params: None,
    };
    let utm = tiff::write_geo_tiff(&raster, &geo).unwrap();
    let args = terrain_args(&dir, &utm, 0, 1);
    let err = cmd_terrain2tiles(&args).unwrap_err();
    assert!(err.contains("42650") || err.contains("32650") || err.contains("不支持的 DEM 坐标系"), "{err}");

    // zoom 范围倒置
    let args = terrain_args(&dir, &dem_f32_fixture(), 3, 1);
    let err = cmd_terrain2tiles(&args).unwrap_err();
    assert!(err.contains("zoom"), "{err}");

    // grid-size 越界
    let mut args = terrain_args(&dir, &dem_f32_fixture(), 0, 1);
    args.grid_size = 2;
    let err = cmd_terrain2tiles(&args).unwrap_err();
    assert!(err.contains("grid-size"), "{err}");

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn terrain_cgcs2000_4490_accepted() {
    let dir = std::env::temp_dir().join(format!("tangis-terrain-cgcs-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    let bytes = dem_f32_fixture_epsg(4490);
    let args = terrain_args(&dir, &bytes, 0, 0);
    let summary = cmd_terrain2tiles(&args).unwrap();
    assert_eq!(summary.tiles_written, 1);

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn terrain_zoom_range_auto() {
    let half = 20037508.342789244;
    let bbox = [-half, -half, half, half]; // 全世界
    // 全球 DEM：单瓦片即可覆盖 → min=0；
    // max = floor(log2(2H / ((g-1)*res))) = floor(log2(40075016.7/19200)) = floor(11.03) = 11
    let (mn, mx) = terrain_zoom_range(&bbox, 300.0, 65.0);
    assert_eq!(mn, 0);
    assert_eq!(mx, 11);

    // 局部小 DEM（约 600m）：min = floor(log2(4e7/600)) ≈ 16；max 同口径比 min 更细
    let small = [0.0, 0.0, 600.0, 600.0];
    let (mn, mx) = terrain_zoom_range(&small, 1.0, 65.0);
    assert_eq!(mn, 16, "log2(40075016.7/600)≈16.03");
    assert!(mx >= mn);
}
