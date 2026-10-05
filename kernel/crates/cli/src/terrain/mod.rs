//! GeoTIFF DEM → Cesium Quantized-Mesh 地形瓦片金字塔（F-02 / 路线方案 B2）。
//!
//! 产物约定（与 server Terrain 分发 F-03 对接）：
//! - 瓦片：`{output}/{z}/{x}/{y}.terrain`，TMS 布局（Y 原点左下），
//!   gzip 压缩的 quantized-mesh-1.0；
//! - 元数据：`{output}/layer.json`（Cesium TerrainProvider 契约，
//!   profile=tms，projections=["EPSG:4326"]）。
//!
//! 约束与说明：
//! - 源为单波段 GeoTIFF DEM（uint16 / float32，复用 `tangis_geo::tiff`）；
//! - CRS 支持 EPSG:4326 / EPSG:3857；EPSG:4490（CGCS2000）按 WGS84 近似处理
//!   （椭球差异厘米级，地形瓦片应用可接受；严格转换走 `bursa` 算子）；
//! - 高程按椭球高直接使用（源为正高时误差 = 大地水准面差距，本地记录在 layer.json 无，
//!   由调用方保证）；
//! - 采样在源 CRS 内做双线性插值，越界 clamp（瓦片与源 bbox 部分相交时边缘外推）。

pub mod qmesh;

use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};

use rayon::prelude::*;
use serde::Serialize;

use tangis_geo::crs;
use tangis_geo::tiff::{self, GeoTags, Raster};

use crate::raster::reproject::{self, HALF_WORLD};

/// `terrain2tiles` 子命令参数。
#[derive(Debug, Clone)]
pub struct TerrainArgs {
    pub source: PathBuf,
    pub output: PathBuf,
    /// None = 按数据范围自动推导（最小层级：DEM 可被单瓦片覆盖的最小 z）。
    pub min_zoom: Option<u32>,
    /// None = 按源分辨率自动推导（不向上超采样，封顶 22）。
    pub max_zoom: Option<u32>,
    /// 每瓦片网格边长（顶点数/边），默认 65。
    pub grid_size: u32,
    /// layer.json 输出路径；None = {output}/layer.json。
    pub layer_metadata: Option<PathBuf>,
    /// B7 协作式取消标志文件（可选）：存在即安全终止（exit 130）。
    pub cancel_file: Option<PathBuf>,
}

/// 生产结果统计。
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct TerrainSummary {
    pub tiles_written: usize,
    pub min_zoom: u32,
    pub max_zoom: u32,
    /// 源范围（WGS84 经纬度，度）：[w, s, e, n]。
    pub bounds: [f64; 4],
    pub height_min: f64,
    pub height_max: f64,
}

/// DEM 源解析结果。
struct DemSource {
    raster: Raster,
    /// 源 CRS 下的 bbox：[minx, miny, maxx, maxy]（北朝上假定）。
    bbox: [f64; 4],
    /// 4326 / 3857（4490 归一化为 4326）。
    crs: super::raster::geotiff::Crs,
    /// 4490 近似标记（写入 stderr 说明）。
    cgcs2000_approx: bool,
}

/// `terrain2tiles` 子命令实现。
pub fn cmd_terrain2tiles(args: &TerrainArgs) -> Result<TerrainSummary, String> {
    if args.grid_size < 3 || args.grid_size > 513 {
        return Err(format!(
            "非法 --grid-size {}（支持 3..=513，默认 65）",
            args.grid_size
        ));
    }
    let g = args.grid_size as usize;

    // ---- 读取 DEM ----
    let dem = read_dem(&args.source)?;

    // ---- 地理范围与 zoom 推导 ----
    let bbox_merc = reproject::bbox_to_merc(dem.bbox, dem.crs);
    let src_res = {
        let rx = (bbox_merc[2] - bbox_merc[0]) / dem.raster.width as f64;
        let ry = (bbox_merc[3] - bbox_merc[1]) / dem.raster.height as f64;
        rx.max(ry)
    };
    let (auto_min, auto_max) = terrain_zoom_range(&bbox_merc, src_res, g as f64);
    let min_zoom = args.min_zoom.unwrap_or(auto_min);
    let max_zoom = args.max_zoom.unwrap_or(auto_max);
    if min_zoom > max_zoom || max_zoom > 22 {
        return Err(format!(
            "非法 zoom 范围 [{min_zoom},{max_zoom}]（要求 0<=min<=max<=22）"
        ));
    }

    // ---- 枚举瓦片（TMS：Y 原点左下）----
    let mut jobs: Vec<(u32, u32, u32)> = Vec::new(); // (z, x, y_tms)
    for z in min_zoom..=max_zoom {
        let n = 1u64 << z;
        let span = 2.0 * HALF_WORLD / n as f64;
        let x0 = ((bbox_merc[0] + HALF_WORLD) / span).floor().max(0.0) as u32;
        let x1 = (((bbox_merc[2] + HALF_WORLD) / span - 1e-9).floor().max(-1.0) as i64)
            .clamp(0, n as i64 - 1) as u32;
        // Y：XYZ 行号（左上原点）与源 bbox 相交行 → 转 TMS（左下原点）
        let y0 = ((HALF_WORLD - bbox_merc[3]) / span).floor().max(0.0) as u32;
        let y1 = (((HALF_WORLD - bbox_merc[1]) / span - 1e-9).floor().max(-1.0) as i64)
            .clamp(0, n as i64 - 1) as u32;
        for y_xyz in y0..=y1 {
            let y_tms = (n as u32 - 1) - y_xyz;
            for x in x0..=x1 {
                jobs.push((z, x, y_tms));
            }
        }
    }
    if jobs.is_empty() {
        return Err(format!(
            "源 bbox {bbox_merc:?} 与 Web Mercator 世界范围无有效交集，未产出任何地形瓦片"
        ));
    }

    // ---- 并行生产瓦片 ----
    let cancel = crate::cancel::CancelFlag::from_path(args.cancel_file.clone());
    let gsize = g;
    let payloads: Vec<Result<TileBuilt, String>> = jobs
        .par_iter()
        .map(|&(z, x, y_tms)| {
            // B7：瓦片边界协作取消——未开始的任务直接跳过
            if cancel.is_canceled() {
                return Err(crate::cancel::canceled_err(&cancel));
            }
            build_tile(&dem, z, x, y_tms, gsize)
        })
        .collect();

    let mut tiles_written = 0usize;
    let mut hmin = f64::INFINITY;
    let mut hmax = f64::NEG_INFINITY;
    for ((z, x, y_tms), r) in jobs.iter().zip(payloads.into_iter()) {
        let tile = r?;
        if cancel.is_canceled() {
            return Err(crate::cancel::canceled_err(&cancel));
        }
        hmin = hmin.min(tile.height_range.0);
        hmax = hmax.max(tile.height_range.1);
        let path = args.output.join(z.to_string()).join(x.to_string()).join(format!("{y_tms}.terrain"));
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)
                .map_err(|e| format!("无法创建瓦片目录 {}: {e}", parent.display()))?;
        }
        fs::write(&path, &tile.gzip).map_err(|e| format!("无法写瓦片 {}: {e}", path.display()))?;
        tiles_written += 1;
    }
    if tiles_written == 0 {
        return Err("未产出任何地形瓦片（内部错误：任务列表非空但全部被跳过）".into());
    }

    // ---- layer.json ----
    let bounds = bounds_wgs84(dem.bbox, dem.crs);
    let layer_path = args
        .layer_metadata
        .clone()
        .unwrap_or_else(|| args.output.join("layer.json"));
    write_layer_json(
        &layer_path,
        &bounds,
        min_zoom,
        max_zoom,
    )?;

    if dem.cgcs2000_approx {
        eprintln!(
            "提示：源 CRS 为 EPSG:4490（CGCS2000），已按 WGS84 近似处理（椭球差异厘米级）；\
             严格转换请先经 `bursa` 算子"
        );
    }

    Ok(TerrainSummary {
        tiles_written,
        min_zoom,
        max_zoom,
        bounds,
        height_min: hmin,
        height_max: hmax,
    })
}

struct TileBuilt {
    gzip: Vec<u8>,
    height_range: (f64, f64),
}

/// 读取并解析单波段 GeoTIFF DEM。
fn read_dem(source: &Path) -> Result<DemSource, String> {
    let bytes = fs::read(source)
        .map_err(|e| format!("无法读取 DEM 源 {}: {e}", source.display()))?;
    let raster = tiff::read_raster(&bytes).map_err(|e| format!("TIFF 解析失败：{e}"))?;
    if raster.samples.is_empty() || raster.width == 0 || raster.height == 0 {
        return Err("空栅格（宽/高/波段为 0）".into());
    }
    let geo: GeoTags = tiff::read_geo_tags(&bytes).map_err(|e| format!("地理参考解析失败：{e}"))?;
    let info = crs::identify_geotiff(&bytes).map_err(|e| format!("CRS 识别失败：{e}"))?;
    let (crs_, cgcs) = match info.epsg {
        Some(4326) => (super::raster::geotiff::Crs::Epsg4326, false),
        Some(3857) => (super::raster::geotiff::Crs::Epsg3857, false),
        Some(4490) => (super::raster::geotiff::Crs::Epsg4326, true),
        other => {
            return Err(format!(
                "不支持的 DEM 坐标系 {other:?}（terrain2tiles 支持 EPSG:4326 / 3857 / 4490）"
            ))
        }
    };
    if geo.tiepoint.len() < 6 {
        return Err("ModelTiepoint 缺失或长度非法".into());
    }
    if geo.pixel_scale[0] <= 0.0 || geo.pixel_scale[1] <= 0.0 {
        return Err("ModelPixelScale 非法（X/Y 像元尺寸须为正）".into());
    }
    let (sx, sy) = (geo.pixel_scale[0], geo.pixel_scale[1]);
    // tiepoint (i, j, k, x, y, z)：像素 (i, j) 对应 (x, y)；北朝上假定
    let x0 = geo.tiepoint[3] - geo.tiepoint[0] * sx;
    let y0 = geo.tiepoint[4] - geo.tiepoint[1] * sy;
    let bbox = [
        x0,
        y0 - raster.height as f64 * sy,
        x0 + raster.width as f64 * sx,
        y0,
    ];
    Ok(DemSource {
        raster,
        bbox,
        crs: crs_,
        cgcs2000_approx: cgcs,
    })
}

/// 地形瓦片 zoom 自动推导：
/// - min = 单瓦片即可覆盖 DEM 的最小层级；
/// - max = 采样步长（span/(g-1)）不细于源分辨率的最大层级（不向上超采样）。
fn terrain_zoom_range(bbox_merc: &[f64; 4], src_res: f64, grid: f64) -> (u32, u32) {
    const MAX_Z: u32 = 22;
    let size = (bbox_merc[2] - bbox_merc[0]).max(bbox_merc[3] - bbox_merc[1]);
    let min_z = if size > 0.0 && size.is_finite() {
        ((2.0 * HALF_WORLD / size).log2().floor().max(0.0) as u32).min(MAX_Z)
    } else {
        0
    };
    let max_z = if src_res > 0.0 && src_res.is_finite() {
        let denom = (grid - 1.0) * src_res;
        ((2.0 * HALF_WORLD / denom).log2().floor().max(0.0) as u32).min(MAX_Z)
    } else {
        MAX_Z
    };
    (min_z.min(max_z), max_z)
}

/// 构建单张地形瓦片：采样网格 → 量化网格编码 → gzip。
fn build_tile(dem: &DemSource, z: u32, x: u32, y_tms: u32, g: usize) -> Result<TileBuilt, String> {
    let n = 1u32 << z;
    if x >= n || y_tms >= n {
        return Err(format!("瓦片索引越界：z{z}/x{x}/y{y_tms}（n={n}）"));
    }
    let span = 2.0 * HALF_WORLD / n as f64;
    let minx = -HALF_WORLD + x as f64 * span;
    let maxx = minx + span;
    let south = -HALF_WORLD + y_tms as f64 * span;
    let north = south + span;

    // 严格相交判定（压线不算），与 xyz 瓦片一致
    if dem_covers_none(dem, minx, south, maxx, north) {
        return Err(format!("瓦片 z{z}/{x}/{y_tms} 与源范围无交集"));
    }

    // ---- 网格采样（j=0 最北行）----
    let mut cartographics: Vec<(f64, f64, f64)> = Vec::with_capacity(g * g);
    let mut heights = Vec::with_capacity(g * g);
    let mut hmin = f64::INFINITY;
    let mut hmax = f64::NEG_INFINITY;
    for j in 0..g {
        let my = north - (j as f64 / (g - 1) as f64) * span;
        for i in 0..g {
            let mx = minx + (i as f64 / (g - 1) as f64) * span;
            let (gx, gy) = match dem.crs {
                super::raster::geotiff::Crs::Epsg3857 => (mx, my),
                super::raster::geotiff::Crs::Epsg4326 => reproject::merc_to_lonlat(mx, my),
            };
            let h = sample_dem(dem, gx, gy);
            hmin = hmin.min(h);
            hmax = hmax.max(h);
            let (lon, lat) = match dem.crs {
                super::raster::geotiff::Crs::Epsg3857 => reproject::merc_to_lonlat(mx, my),
                super::raster::geotiff::Crs::Epsg4326 => (gx, gy),
            };
            heights.push(h);
            cartographics.push((lon, lat, h));
        }
    }

    // ---- 网格三角形索引（CCW）----
    // 顶点按行主序编号（j=0 最北行）。HWM 编解码为 u16 回绕算术，
    // 任意索引序列均可无损编码（见 qmesh::hwm_encode）。
    let mut indices: Vec<u32> = Vec::with_capacity((g - 1) * (g - 1) * 6);
    for j in 0..g - 1 {
        for i in 0..g - 1 {
            let v00 = (j * g + i) as u32;
            let v10 = (j * g + i + 1) as u32;
            let v01 = ((j + 1) * g + i) as u32;
            let v11 = ((j + 1) * g + i + 1) as u32;
            indices.extend_from_slice(&[v00, v01, v10, v10, v01, v11]);
        }
    }

    // ---- 边界索引（west 南→北 / south 东→西 / east 北→南 / north 西→东）----
    let west: Vec<u32> = (0..g).rev().map(|j| (j * g) as u32).collect();
    let south_: Vec<u32> = (0..g).rev().map(|i| ((g - 1) * g + i) as u32).collect();
    let east: Vec<u32> = (0..g).map(|j| (j * g + g - 1) as u32).collect();
    let north_: Vec<u32> = (0..g).map(|i| i as u32).collect();

    // ---- ECEF 中心 ----
    let clon = (minx + maxx) / 2.0;
    let clat = (south + north) / 2.0;
    let (lon_c, lat_c) = reproject::merc_to_lonlat(clon, clat);
    let center = qmesh::geodetic_to_ecef(lon_c, lat_c, (hmin + hmax) / 2.0);

    let raw = qmesh::encode_tile(
        &qmesh::TileMesh {
            rect: [lon_west(minx), south_lat(south), lon_east(maxx), north_lat(north)],
            height_range: (hmin, hmax),
            cartographics: &cartographics,
            indices: &indices,
            edges: [&west, &south_, &east, &north_],
        },
        center,
    )?;

    Ok(TileBuilt {
        gzip: gzip_encode(&raw)?,
        height_range: (hmin, hmax),
    })
}

fn lon_west(minx: f64) -> f64 {
    reproject::merc_to_lonlat(minx, 0.0).0
}
fn lon_east(maxx: f64) -> f64 {
    reproject::merc_to_lonlat(maxx, 0.0).0
}
fn south_lat(south: f64) -> f64 {
    reproject::merc_to_lonlat(0.0, south).1
}
fn north_lat(north: f64) -> f64 {
    reproject::merc_to_lonlat(0.0, north).1
}

/// 瓦片与源 bbox 是否完全无交集（严格相交，压线不算）。
fn dem_covers_none(dem: &DemSource, minx: f64, south: f64, maxx: f64, north: f64) -> bool {
    let b = reproject::bbox_to_merc(dem.bbox, dem.crs);
    maxx <= b[0] + 1e-9 || minx >= b[2] - 1e-9 || north <= b[1] + 1e-9 || south >= b[3] - 1e-9
}

/// 在源 CRS 坐标 (gx, gy) 处双线性采样高程（越界 clamp）。
fn sample_dem(dem: &DemSource, gx: f64, gy: f64) -> f64 {
    let w = dem.raster.width as f64;
    let h = dem.raster.height as f64;
    let fx = ((gx - dem.bbox[0]) / (dem.bbox[2] - dem.bbox[0]) * w - 0.5).clamp(0.0, w - 1.0);
    let fy = ((dem.bbox[3] - gy) / (dem.bbox[3] - dem.bbox[1]) * h - 0.5).clamp(0.0, h - 1.0);
    let x0 = fx.floor() as usize;
    let y0 = fy.floor() as usize;
    let x1 = (x0 + 1).min(dem.raster.width as usize - 1);
    let y1 = (y0 + 1).min(dem.raster.height as usize - 1);
    let tx = fx - x0 as f64;
    let ty = fy - y0 as f64;
    let s = &dem.raster.samples;
    let v = |xi: usize, yi: usize| s.get_f64(yi * dem.raster.width as usize + xi);
    let top = v(x0, y0) * (1.0 - tx) + v(x1, y0) * tx;
    let bot = v(x0, y1) * (1.0 - tx) + v(x1, y1) * tx;
    top * (1.0 - ty) + bot * ty
}

/// 源 bbox → WGS84 经纬度 [w, s, e, n]。
fn bounds_wgs84(bbox: [f64; 4], crs: super::raster::geotiff::Crs) -> [f64; 4] {
    use super::raster::geotiff::Crs;
    match crs {
        Crs::Epsg4326 => bbox,
        Crs::Epsg3857 => {
            let (w, n) = reproject::merc_to_lonlat(bbox[0], bbox[3]);
            let (e, s) = reproject::merc_to_lonlat(bbox[2], bbox[1]);
            [w, s, e, n]
        }
    }
}

// ---- layer.json（Cesium TerrainProvider 契约） ----

#[derive(Serialize)]
struct LevelPoint {
    level: u32,
}

#[derive(Serialize)]
struct Availability {
    start: LevelPoint,
    end: LevelPoint,
}

#[derive(Serialize)]
struct LayerJson {
    tilejson: &'static str,
    name: String,
    format: &'static str,
    version: &'static str,
    profile: &'static str,
    bounds: [f64; 4],
    projections: [&'static str; 1],
    tiles: [String; 1],
    available: [Availability; 1],
}

fn write_layer_json(
    path: &Path,
    bounds: &[f64; 4],
    min_zoom: u32,
    max_zoom: u32,
) -> Result<(), String> {
    let layer = LayerJson {
        tilejson: "2.1.0",
        name: "terrain".into(),
        format: "quantized-mesh-1.0",
        version: "1.0.0",
        profile: "tms",
        bounds: *bounds,
        projections: ["EPSG:4326"],
        tiles: ["{z}/{x}/{y}.terrain?v=1.0.0".to_string()],
        available: [Availability {
            start: LevelPoint { level: min_zoom },
            end: LevelPoint { level: max_zoom },
        }],
    };
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .map_err(|e| format!("无法创建 layer.json 目录 {}: {e}", parent.display()))?;
    }
    let json = serde_json::to_string_pretty(&layer).map_err(|e| e.to_string())?;
    fs::write(path, json + "\n")
        .map_err(|e| format!("无法写 layer.json {}: {e}", path.display()))
}

/// gzip 压缩（CTB 约定，.terrain 落盘为 gzip）。
fn gzip_encode(raw: &[u8]) -> Result<Vec<u8>, String> {
    let mut enc = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    enc.write_all(raw).map_err(|e| e.to_string())?;
    enc.finish().map_err(|e| format!("gzip 压缩失败：{e}"))
}

/// 测试用：读取 gzip 的 .terrain 并解码原始 quantized-mesh 字节。
#[cfg(test)]
pub(crate) fn gunzip(data: &[u8]) -> Vec<u8> {
    use std::io::Read;
    let mut dec = flate2::read::GzDecoder::new(data);
    let mut out = Vec::new();
    dec.read_to_end(&mut out).unwrap();
    out
}

#[cfg(test)]
mod tests;
