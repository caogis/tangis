//! XYZ / WMTS 影像瓦片金字塔生产（WebMercatorQuad）+ metadata.json。
//!
//! 产物约定（与 server F-03 对接）：
//! - 布局 `xyz`（默认）：`{output}/tiles/{z}/{x}/{y}.png`（XYZ，Y 原点左上）；
//! - 布局 `wmts`：`{output}/tiles/{z}/{row}/{col}.png`（WMTS REST 模板
//!   TileMatrix/TileRow/TileCol 顺序，行/列与 XYZ 的 x/y 互换）；
//! - 元数据：`{output}/tiles/metadata.json`（含 `layout` 字段，server 按布局读盘）。

use std::fs;
use std::io::Cursor;
use std::path::Path;

use rayon::prelude::*;
use serde::Serialize;

use super::geotiff::{self, Crs, GeoTiff};
use super::reproject::{self, merc_to_lonlat, tile_bounds, zoom_range};

/// server 按此 schema 读取金字塔元数据。
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct PyramidMetadata {
    pub tile_matrix_set: &'static str,
    /// 磁盘布局：`xyz` = {z}/{x}/{y}；`wmts` = {z}/{row}/{col}。
    pub layout: &'static str,
    /// Web Mercator bbox：`[minx, miny, maxx, maxy]`。
    pub extent: [f64; 4],
    pub min_zoom: u32,
    pub max_zoom: u32,
    pub tile_size: u32,
    pub format: &'static str,
}

/// 重采样方式。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Resampling {
    Nearest,
    Bilinear,
}

/// `raster2tiles` 子命令参数（server 追加参数均已识别，未知参数由 clap 报错）。
#[derive(Debug, Clone)]
pub struct RasterArgs {
    pub source: std::path::PathBuf,
    pub output: std::path::PathBuf,
    pub source_type: String,
    pub pyramid_layout: String,
    pub pyramid_metadata: std::path::PathBuf,
    pub resampling: String,
    /// B7 协作式取消标志文件（可选）：存在即安全终止（exit 130）。
    pub cancel_file: Option<std::path::PathBuf>,
}

/// 生产结果统计。
#[derive(Debug, Clone, PartialEq)]
pub struct XyzSummary {
    pub tiles_written: usize,
    pub min_zoom: u32,
    pub max_zoom: u32,
    pub extent: [f64; 4],
}

/// `raster2tiles` 子命令实现。
pub fn cmd_raster2tiles(args: &RasterArgs) -> Result<XyzSummary, String> {
    // server 传入参数白名单校验：明确报错列出支持值，禁止猜测
    if args.source_type != "image" {
        return Err(format!(
            "不支持的 --source-type `{}`（raster2tiles 当前仅支持 `image`）",
            args.source_type
        ));
    }
    if !matches!(args.pyramid_layout.as_str(), "xyz" | "wmts") {
        return Err(format!(
            "不支持的 --pyramid-layout `{}`（raster2tiles 支持 `xyz` / `wmts`）",
            args.pyramid_layout
        ));
    }
    let layout: &'static str = if args.pyramid_layout == "wmts" { "wmts" } else { "xyz" };
    let resampling = match args.resampling.as_str() {
        "nearest" => Resampling::Nearest,
        "bilinear" => Resampling::Bilinear,
        other => {
            return Err(format!(
                "不支持的 --resampling `{other}`（支持 `nearest` / `bilinear`）"
            ))
        }
    };

    // ---- 读取 GeoTIFF ----
    let bytes = fs::read(&args.source)
        .map_err(|e| format!("无法读取影像源 {}: {e}", args.source.display()))?;
    let tiff = geotiff::read(&bytes)?;

    // ---- 推导金字塔参数 ----
    let bbox_merc = reproject::bbox_to_merc(tiff.bbox, tiff.crs);
    // 源分辨率（米/像素）：X 方向按 bbox 宽/像素宽；Y 方向取 max（墨卡托 Y 非线性，保守取大）
    let res_x = (bbox_merc[2] - bbox_merc[0]) / tiff.width as f64;
    let res_y = (bbox_merc[3] - bbox_merc[1]) / tiff.height as f64;
    let (min_zoom, max_zoom) = zoom_range(res_x.max(res_y));

    let metadata = PyramidMetadata {
        tile_matrix_set: "WebMercatorQuad",
        layout,
        extent: bbox_merc,
        min_zoom,
        max_zoom,
        tile_size: reproject::TILE_SIZE,
        format: "png",
    };

    // ---- 枚举与源 bbox 相交的瓦片并并行渲染 ----
    let mut jobs: Vec<(u32, u32, u32)> = Vec::new();
    for z in min_zoom..=max_zoom {
        let n = 1u64 << z;
        let span = 2.0 * reproject::HALF_WORLD / n as f64;
        // X 索引范围（严格相交：压线不算）
        let x0 = ((bbox_merc[0] + reproject::HALF_WORLD) / span).floor().max(0.0) as u32;
        let x1 = (((bbox_merc[2] + reproject::HALF_WORLD) / span - 1e-9).floor().max(-1.0) as i64)
            .clamp(0, n as i64 - 1) as u32;
        // Y 索引范围（XYZ：y=0 在最北）
        let y0 = ((reproject::HALF_WORLD - bbox_merc[3]) / span).floor().max(0.0) as u32;
        let y1 = (((reproject::HALF_WORLD - bbox_merc[1]) / span - 1e-9).floor().max(-1.0) as i64)
            .clamp(0, n as i64 - 1) as u32;
        for y in y0..=y1 {
            for x in x0..=x1 {
                jobs.push((z, x, y));
            }
        }
    }
    let cancel = crate::cancel::CancelFlag::from_path(args.cancel_file.clone());
    let results: Vec<Result<(std::path::PathBuf, Vec<u8>), String>> = jobs
        .par_iter()
        .map(|&(z, x, y)| {
            // B7：分块边界协作取消——未开始的任务直接跳过
            if cancel.is_canceled() {
                return Err(crate::cancel::canceled_err(&cancel));
            }
            render_tile_png(&tiff, z, x, y, resampling)
                .map(|bytes| (tile_path(&args.output, layout, z, x, y), bytes))
        })
        .collect();

    let mut tiles_written = 0usize;
    for r in results {
        let (path, png) = r?;
        if cancel.is_canceled() {
            return Err(crate::cancel::canceled_err(&cancel));
        }
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)
                .map_err(|e| format!("无法创建瓦片目录 {}: {e}", parent.display()))?;
        }
        fs::write(&path, &png).map_err(|e| format!("无法写瓦片 {}: {e}", path.display()))?;
        tiles_written += 1;
    }
    if tiles_written == 0 {
        return Err(format!(
            "源 bbox {bbox_merc:?} 与 Web Mercator 世界范围无有效交集，未产出任何瓦片"
        ));
    }

    // ---- metadata.json ----
    if let Some(parent) = args.pyramid_metadata.parent() {
        fs::create_dir_all(parent)
            .map_err(|e| format!("无法创建元数据目录 {}: {e}", parent.display()))?;
    }
    let json = serde_json::to_string_pretty(&metadata).map_err(|e| e.to_string())?;
    fs::write(&args.pyramid_metadata, json + "\n").map_err(|e| {
        format!(
            "无法写金字塔元数据 {}: {e}",
            args.pyramid_metadata.display()
        )
    })?;

    Ok(XyzSummary {
        tiles_written,
        min_zoom,
        max_zoom,
        extent: bbox_merc,
    })
}

/// 瓦片物理路径：xyz = {z}/{x}/{y}.png；wmts = {z}/{row}/{col}.png（row=y, col=x）。
pub fn tile_path(output: &Path, layout: &str, z: u32, x: u32, y: u32) -> std::path::PathBuf {
    if layout == "wmts" {
        output.join("tiles").join(z.to_string()).join(y.to_string()).join(format!("{x}.png"))
    } else {
        output.join("tiles").join(z.to_string()).join(x.to_string()).join(format!("{y}.png"))
    }
}

/// 渲染单个 XYZ 瓦片并编码 PNG。无数据区 alpha=0（透明填充）。
fn render_tile_png(
    tiff: &GeoTiff,
    z: u32,
    x: u32,
    y: u32,
    resampling: Resampling,
) -> Result<Vec<u8>, String> {
    let n = 1u32 << z;
    if x >= n || y >= n {
        return Err(format!("瓦片索引越界：z{z}/{x}/{y}（n={n}）"));
    }
    let bounds = tile_bounds(z, x, y);
    let span = bounds[2] - bounds[0];
    let size = reproject::TILE_SIZE as usize;
    let mut buf = vec![0u8; size * size * 4]; // 默认全透明
    for py in 0..size {
        // 像素中心
        let my = bounds[3] - (py as f64 + 0.5) / size as f64 * span;
        for px in 0..size {
            let mx = bounds[0] + (px as f64 + 0.5) / size as f64 * span;
            // 目标墨卡托点 → 源地理坐标 → 源像素坐标
            let (gx, gy) = match tiff.crs {
                Crs::Epsg3857 => (mx, my),
                Crs::Epsg4326 => merc_to_lonlat(mx, my),
            };
            let fx = (gx - tiff.bbox[0]) / (tiff.bbox[2] - tiff.bbox[0]) * tiff.width as f64 - 0.5;
            let fy = (tiff.bbox[3] - gy) / (tiff.bbox[3] - tiff.bbox[1]) * tiff.height as f64 - 0.5;
            let c = match resampling {
                Resampling::Nearest => tiff.sample_nearest(fx, fy),
                Resampling::Bilinear => tiff.sample_bilinear(fx, fy),
            };
            if c[3] != 0 {
                let o = (py * size + px) * 4;
                buf[o..o + 4].copy_from_slice(&c);
            }
        }
    }
    let img = image::RgbaImage::from_raw(size as u32, size as u32, buf)
        .ok_or("内部错误：瓦片缓冲区尺寸不匹配")?;
    let mut png = Vec::new();
    img.write_to(&mut Cursor::new(&mut png), image::ImageFormat::Png)
        .map_err(|e| format!("PNG 编码失败（z{z}/{x}/{y}）：{e}"))?;
    Ok(png)
}
