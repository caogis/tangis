//! LAS 点云 → 3D Tiles 点云瓦片（.pnts + tileset.json，F-02 / 路线方案 B1）。
//!
//! 产物约定（与 build 子命令一致）：
//! - 瓦片：`{output}/tiles/{index}.pnts`（RTC_CENTER 补偿 f32 精度）；
//! - 元数据：`{output}/tileset.json`（扁平网格分块，叶子级 ge=0）。
//!
//! 地理定位：`--origin lon lat height` 时与 build 相同——点坐标转为
//! 以数据集中心为原点的局部 ENU 米制，root.transform = ENU→ECEF；
//! 缺省时无 transform（查看器自行摆放）。
//!
//! 内存说明：单遍流式读取（BufReader），点先落入进程内缓冲后按 XY 网格
//! 分块落盘；MVP 阶段整包驻留内存（f32 相对坐标），超大点云分批处理
//! 留待后续（spec 风险 R2：单 tile 点数上限 --max-points-per-tile 可控）。

pub mod las;
pub mod pnts;

use std::fs;
use std::path::PathBuf;

use serde::Serialize;

use crate::tiles::tileset::enu_to_ecef_transform;

/// `las2pnts` 子命令参数。
#[derive(Debug, Clone)]
pub struct LasArgs {
    pub source: PathBuf,
    pub output: PathBuf,
    /// [lon, lat, height]（度/米），地理定位。
    pub origin: Option<[f64; 3]>,
    /// 每瓦片点数上限（默认 50_000）。
    pub max_points_per_tile: u32,
}

/// 生产结果统计。
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct PntsSummary {
    pub point_count: u64,
    pub tiles_written: usize,
    pub grid: [u32; 2],
    /// 数据集实际包围盒（源坐标系）。
    pub bounds_min: [f64; 3],
    pub bounds_max: [f64; 3],
    pub has_color: bool,
}

/// `las2pnts` 子命令实现。
pub fn cmd_las2pnts(args: &LasArgs) -> Result<PntsSummary, String> {
    if args.max_points_per_tile < 1_000 || args.max_points_per_tile > 2_000_000 {
        return Err(format!(
            "非法 --max-points-per-tile {}（支持 1000..=2000000）",
            args.max_points_per_tile
        ));
    }
    if let Some(o) = args.origin {
        if !(-180.0..=180.0).contains(&o[0]) || !(-90.0..=90.0).contains(&o[1]) {
            return Err(format!("非法 --origin {:?}（经纬度越界）", o));
        }
    }

    // ---- 流式读取全量点（f32 相对坐标延迟到分块后） ----
    let (mut reader, header) = las::open(&args.source)?;
    if header.point_count == 0 {
        return Err("LAS 文件无点（point_count = 0）".into());
    }
    let record_len = header.record_length as usize;
    let mut record_buf = vec![0u8; record_len];
    let mut pt = las::LasPoint::default();

    // 第一遍：收集原始坐标 + 实测包围盒（头块 bbox 不信任）
    let mut xs: Vec<[f64; 3]> = Vec::with_capacity(header.point_count.min(1 << 24) as usize);
    let mut cols: Vec<[u8; 3]> = if header.has_color() {
        Vec::with_capacity(xs.capacity())
    } else {
        Vec::new()
    };
    let mut bmin = [f64::INFINITY; 3];
    let mut bmax = [f64::NEG_INFINITY; 3];
    let mut actual = 0u64;
    while las::next_point(&mut reader, &header, &mut record_buf, &mut pt)? {
        for k in 0..3 {
            bmin[k] = bmin[k].min(pt.xyz[k]);
            bmax[k] = bmax[k].max(pt.xyz[k]);
        }
        xs.push(pt.xyz);
        if header.has_color() {
            cols.push(pt.rgb.unwrap_or([255; 3]));
        }
        actual += 1;
    }
    if actual == 0 {
        return Err("LAS 点数据为空".into());
    }
    for k in 0..3 {
        if !(bmax[k] > bmin[k]) {
            bmax[k] = bmin[k] + 1.0; // 退化轴兜底
        }
    }

    // ---- 网格分块（XY 平面） ----
    let capacity = args.max_points_per_tile as u64;
    let target_tiles = (actual + capacity - 1) / capacity;
    let k = ((target_tiles as f64).sqrt().ceil() as u32).max(1).min(256);
    let span_x = bmax[0] - bmin[0];
    let span_y = bmax[1] - bmin[1];
    let cell = (span_x / k as f64).max(span_y / k as f64).max(1e-9);
    let gx = ((span_x / cell).ceil() as u32).max(1);
    let gy = ((span_y / cell).ceil() as u32).max(1);

    // 数据集中心（局部原点）
    let center = [
        (bmin[0] + bmax[0]) / 2.0,
        (bmin[1] + bmax[1]) / 2.0,
        (bmin[2] + bmax[2]) / 2.0,
    ];

    // ---- 分桶 ----
    #[derive(Default)]
    struct Bucket {
        pos: Vec<[f32; 3]>,
        col: Vec<[u8; 3]>,
        min: [f64; 3],
        max: [f64; 3],
    }
    let mut buckets: Vec<Bucket> = (0..(gx as usize) * (gy as usize))
        .map(|_| Bucket {
            pos: Vec::new(),
            col: Vec::new(),
            min: [f64::INFINITY; 3],
            max: [f64::NEG_INFINITY; 3],
        })
        .collect();
    for (i, xyz) in xs.iter().enumerate() {
        let cx = (((xyz[0] - bmin[0]) / cell) as u32).min(gx - 1) as usize;
        let cy = (((xyz[1] - bmin[1]) / cell) as u32).min(gy - 1) as usize;
        let b = &mut buckets[cy * gx as usize + cx];
        b.pos.push([
            (xyz[0] - center[0]) as f32,
            (xyz[1] - center[1]) as f32,
            (xyz[2] - center[2]) as f32,
        ]);
        if header.has_color() {
            b.col.push(cols[i]);
        }
        for kk in 0..3 {
            b.min[kk] = b.min[kk].min(xyz[kk]);
            b.max[kk] = b.max[kk].max(xyz[kk]);
        }
    }
    drop(xs);
    drop(cols);

    // ---- 逐桶写 pnts ----
    let tiles_dir = args.output.join("tiles");
    fs::create_dir_all(&tiles_dir)
        .map_err(|e| format!("无法创建瓦片目录 {}: {e}", tiles_dir.display()))?;

    let mut children: Vec<serde_json::Value> = Vec::new();
    let mut tiles_written = 0usize;
    let mut points_in_tiles = 0u64;
    for (idx, b) in buckets.iter().enumerate() {
        if b.pos.is_empty() {
            continue;
        }
        let rtc = [
            (b.min[0] + b.max[0]) / 2.0 - center[0],
            (b.min[1] + b.max[1]) / 2.0 - center[1],
            (b.min[2] + b.max[2]) / 2.0 - center[2],
        ];
        let colors = if header.has_color() { Some(&b.col[..]) } else { None };
        let pnts_bytes = pnts::encode_pnts(&b.pos, colors, rtc)?;
        let path = tiles_dir.join(format!("{idx}.pnts"));
        fs::write(&path, &pnts_bytes).map_err(|e| format!("无法写 {}：{e}", path.display()))?;
        tiles_written += 1;
        points_in_tiles += b.pos.len() as u64;

        // boundingVolume box（局部 ENU）：中心 + 三轴半长
        let bc = [
            (b.min[0] + b.max[0]) / 2.0 - center[0],
            (b.min[1] + b.max[1]) / 2.0 - center[1],
            (b.min[2] + b.max[2]) / 2.0 - center[2],
        ];
        let hx = ((b.max[0] - b.min[0]) / 2.0).max(0.01);
        let hy = ((b.max[1] - b.min[1]) / 2.0).max(0.01);
        let hz = ((b.max[2] - b.min[2]) / 2.0).max(0.01);
        children.push(serde_json::json!({
            "boundingVolume": {
                "box": [bc[0], bc[1], bc[2], hx, 0.0, 0.0, 0.0, hy, 0.0, 0.0, 0.0, hz]
            },
            "geometricError": 0.0,
            "content": { "uri": format!("tiles/{idx}.pnts") }
        }));
    }
    if tiles_written == 0 {
        return Err("未产出任何 pnts 瓦片（内部错误：分桶为空）".into());
    }
    debug_assert_eq!(points_in_tiles, actual);

    // ---- tileset.json ----
    let root_half = [
        (bmax[0] - bmin[0]) / 2.0,
        (bmax[1] - bmin[1]) / 2.0,
        (bmax[2] - bmin[2]) / 2.0,
    ];
    let root_box = [
        0.0,
        0.0,
        0.0,
        root_half[0].max(0.01),
        0.0,
        0.0,
        0.0,
        root_half[1].max(0.01),
        0.0,
        0.0,
        0.0,
        root_half[2].max(0.01),
    ];
    let diag = (root_half[0].powi(2) + root_half[1].powi(2) + root_half[2].powi(2)).sqrt();
    let root_ge = (diag * 4.0).max(1.0);
    let mut root = serde_json::json!({
        "boundingVolume": { "box": root_box },
        "geometricError": root_ge,
        "refine": "ADD",
        "children": children,
    });
    let mut tileset = serde_json::json!({
        "asset": { "version": "1.0" },
        "geometricError": root_ge * 2.0,
        "root": root,
    });
    if let Some(o) = args.origin {
        root["transform"] = serde_json::json!(enu_to_ecef_transform(o[0], o[1], o[2]));
        tileset["root"] = root;
    }
    let tileset_path = args.output.join("tileset.json");
    fs::write(
        &tileset_path,
        serde_json::to_string_pretty(&tileset).map_err(|e| e.to_string())? + "\n",
    )
    .map_err(|e| format!("无法写 {}：{e}", tileset_path.display()))?;

    Ok(PntsSummary {
        point_count: actual,
        tiles_written,
        grid: [gx, gy],
        bounds_min: bmin,
        bounds_max: bmax,
        has_color: header.has_color(),
    })
}

#[cfg(test)]
mod tests;
