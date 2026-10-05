//! las2pnts 端到端测试：合成 LAS → 生产 → tileset.json + pnts 解码校验。

use std::path::{Path, PathBuf};

use super::las::las_fixture_bytes;
use super::pnts::decode_pnts;
use super::{cmd_las2pnts, LasArgs};

fn las_args(dir: &Path, n: usize) -> (LasArgs, PathBuf) {
    let src = dir.join("cloud.las");
    std::fs::write(&src, las_fixture_bytes(n)).unwrap();
    (
        LasArgs {
            source: src.clone(),
            output: dir.join("out"),
            origin: Some([116.0, 40.0, 50.0]),
            max_points_per_tile: 1_000,
        },
        src,
    )
}

#[test]
fn las2pnts_end_to_end_tileset_and_pnts() {
    let dir = std::env::temp_dir().join(format!("tangis-pnts-e2e-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    let (args, _src) = las_args(&dir, 2500);
    let summary = cmd_las2pnts(&args).unwrap();
    assert_eq!(summary.point_count, 2500);
    assert!(summary.has_color);
    // 2500 点 / 1000 上限 → 目标 3 瓦片 → k=2；对角线点云非空桶可能少于 4
    assert!(summary.tiles_written >= 1 && summary.tiles_written <= 4);
    assert!(summary.grid[0] as usize * summary.grid[1] as usize >= summary.tiles_written);

    // ---- tileset.json ----
    let ts: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(dir.join("out/tileset.json")).unwrap())
            .unwrap();
    assert_eq!(ts["asset"]["version"], "1.0");
    let root = &ts["root"];
    let transform = root["transform"].as_array().unwrap();
    assert_eq!(transform.len(), 16);
    // 平移 ≈ ECEF(116, 40, 50)：x = R·cos40°·cos116° ≈ -2.144e6
    let t0 = transform[12].as_f64().unwrap();
    assert!((t0 - (-2_144_838.63)).abs() < 5_000.0, "ecef x={t0}");
    let children = root["children"].as_array().unwrap();
    assert_eq!(children.len(), summary.tiles_written);
    for (i, c) in children.iter().enumerate() {
        assert_eq!(c["geometricError"], 0.0);
        assert_eq!(c["content"]["uri"], format!("tiles/{i}.pnts"));
        let b = c["boundingVolume"]["box"].as_array().unwrap();
        assert_eq!(b.len(), 12);
    }
    assert!(ts["geometricError"].as_f64().unwrap() > root["geometricError"].as_f64().unwrap());

    // ---- pnts 内容：点数守恒 + 位置/颜色回读 ----
    let mut total = 0u64;
    for i in 0..children.len() {
        let data = std::fs::read(dir.join(format!("out/tiles/{i}.pnts"))).unwrap();
        let (n, _rtc, pos, col) = decode_pnts(&data).unwrap();
        assert_eq!(n, pos.len());
        assert!(col.is_some());
        total += n as u64;
        // 位置相对 RTC，量级 <= 数据集跨度
        for p in &pos {
            assert!(p[0].abs() <= 60.0 && p[1].abs() <= 60.0 && p[2].abs() <= 60.0, "{p:?}");
        }
    }
    assert_eq!(total, 2500);

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn las2pnts_no_color_and_no_origin() {
    let dir = std::env::temp_dir().join(format!("tangis-pnts-norgb-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    let src = dir.join("cloud.las");
    // fmt 0（无 RGB）：复用头部（227B），点区重写为 20 字节记录
    let header = las_fixture_bytes(0)[..227].to_vec();
    let mut rebuilt = header;
    rebuilt[104] = 0; // point format = 0
    rebuilt[105..107].copy_from_slice(&20u16.to_le_bytes()); // record length
    rebuilt[107..111].copy_from_slice(&100u32.to_le_bytes()); // point count
    for i in 0..100usize {
        rebuilt.extend_from_slice(&(i as i32).to_le_bytes());
        rebuilt.extend_from_slice(&(2 * i as i32).to_le_bytes());
        rebuilt.extend_from_slice(&(3 * i as i32).to_le_bytes());
        rebuilt.extend_from_slice(&0u16.to_le_bytes());
        rebuilt.extend_from_slice(&[0u8; 6]);
    }
    std::fs::write(&src, &rebuilt).unwrap();

    let args = LasArgs {
        source: src,
        output: dir.join("out"),
        origin: None,
        max_points_per_tile: 10_000,
    };
    let summary = cmd_las2pnts(&args).unwrap();
    assert!(!summary.has_color);
    assert_eq!(summary.point_count, 100);
    assert!(summary.tiles_written == 1, "100 点单桶");

    // 无 origin：无 transform
    let ts: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(dir.join("out/tileset.json")).unwrap())
            .unwrap();
    assert!(ts["root"]["transform"].is_null());

    // pnts 无 RGB 属性
    let data = std::fs::read(dir.join("out/tiles/0.pnts")).unwrap();
    let (n, _, _, col) = decode_pnts(&data).unwrap();
    assert_eq!(n, 100);
    assert!(col.is_none());

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn las2pnts_rejects_bad_args() {
    let dir = std::env::temp_dir().join(format!("tangis-pnts-bad-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    let src = dir.join("cloud.las");
    std::fs::write(&src, las_fixture_bytes(10)).unwrap();

    // max-points 越界
    let args = LasArgs {
        source: src.clone(),
        output: dir.join("out"),
        origin: None,
        max_points_per_tile: 10,
    };
    let err = cmd_las2pnts(&args).unwrap_err();
    assert!(err.contains("max-points-per-tile"), "{err}");

    // origin 经度越界
    let args = LasArgs {
        source: src.clone(),
        output: dir.join("out"),
        origin: Some([200.0, 40.0, 0.0]),
        max_points_per_tile: 10_000,
    };
    let err = cmd_las2pnts(&args).unwrap_err();
    assert!(err.contains("origin"), "{err}");

    // 非 LAS 文件
    let bad = dir.join("bad.las");
    std::fs::write(&bad, b"junkjunk").unwrap();
    let args = LasArgs {
        source: bad,
        output: dir.join("out"),
        origin: None,
        max_points_per_tile: 10_000,
    };
    let err = cmd_las2pnts(&args).unwrap_err();
    assert!(err.contains("LASF"), "{err}");

    let _ = std::fs::remove_dir_all(&dir);
}
