//! DEM 区域脱密（合规 P0 算子三）。
//!
//! 功能：
//! - 读取单波段 GeoTIFF DEM（uint16 / float32，无压缩）；
//! - 指定多边形/矩形区域内按策略脱密：`flatten`（高程置平到
//!   区域均值+delta）或 `noise`（叠加 delta 有界受控噪声，seed 可复现）；
//! - 区域外像素保持不变；
//! - 输出脱密后 GeoTIFF（保留 33550/33922/34735 地理标签）+ 脱密记录
//!   JSON（区域、参数、前后统计），供审批链路留痕。
//!
//! 坐标映射约定（GDAL 兼容）：ModelTiepoint 首组 (i0, j0) ↔ (x0, y0) 为
//! 像元左上角坐标，北向上网格；像素中心
//! `x = x0 + (col + 0.5 − i0)·SX`，`y = y0 − (row + 0.5 − j0)·SY`。
//!
//! 合规约束：DEM 必须带可识别的 CRS（GeoKey 3072/2048 在支持列表内），
//! 否则拒绝脱密——无法确定区域坐标语义的操作不允许静默执行。

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::crs;
use crate::tiff::{self, GeoTags, Raster, TiffError};

/// DEM 脱密错误。
#[derive(Debug, Error)]
pub enum DemError {
    /// 输入/输出文件错误。
    #[error("{0}")]
    Io(String),
    /// GeoTIFF 结构错误。
    #[error("DEM GeoTIFF 读取失败：{0}")]
    Tiff(#[from] TiffError),
    /// 区域/参数非法。
    #[error("脱密参数非法：{0}")]
    Param(String),
    /// CRS 识别失败。
    #[error("{0}")]
    Crs(String),
}

/// 脱密区域（与 GeoTIFF 地理坐标同单位：地理坐标系为度，投影坐标系为米）。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum Region {
    /// 矩形包围盒。
    Bbox {
        /// 最小 X。
        min_x: f64,
        /// 最大 X。
        max_x: f64,
        /// 最小 Y。
        min_y: f64,
        /// 最大 Y。
        max_y: f64,
    },
    /// 多边形（单环，首尾可闭合可不闭合，自动按闭合处理）。
    Polygon {
        /// 环顶点 [[x, y], ...]，至少 3 个不重复顶点。
        ring: Vec<[f64; 2]>,
    },
}

impl Region {
    fn validate(&self) -> Result<(), DemError> {
        match self {
            Region::Bbox {
                min_x,
                max_x,
                min_y,
                max_y,
            } => {
                if !(min_x.is_finite() && max_x.is_finite() && min_y.is_finite() && max_y.is_finite())
                {
                    return Err(DemError::Param("bbox 含非有限数".into()));
                }
                if min_x >= max_x || min_y >= max_y {
                    return Err(DemError::Param(format!(
                        "bbox 无效：要求 min < max（min_x={min_x} max_x={max_x} min_y={min_y} max_y={max_y}）"
                    )));
                }
                Ok(())
            }
            Region::Polygon { ring } => {
                if ring.len() < 3 {
                    return Err(DemError::Param(format!(
                        "多边形顶点数 {} < 3",
                        ring.len()
                    )));
                }
                for p in ring {
                    if !p.iter().all(|v| v.is_finite()) {
                        return Err(DemError::Param("多边形含非有限坐标".into()));
                    }
                }
                Ok(())
            }
        }
    }

    fn contains(&self, x: f64, y: f64) -> bool {
        match self {
            Region::Bbox {
                min_x,
                max_x,
                min_y,
                max_y,
            } => x >= *min_x && x <= *max_x && y >= *min_y && y <= *max_y,
            Region::Polygon { ring } => point_in_ring(x, y, ring),
        }
    }

    fn describe(&self) -> String {
        match self {
            Region::Bbox { .. } => "bbox".into(),
            Region::Polygon { ring } => format!("polygon({} 点)", ring.len()),
        }
    }
}

/// 射线法点在多边形内判定（边界点不保证在内，符合常规栅格脱密语义）。
fn point_in_ring(x: f64, y: f64, ring: &[[f64; 2]]) -> bool {
    let n = ring.len();
    let mut inside = false;
    let mut j = n - 1;
    for i in 0..n {
        let (xi, yi) = (ring[i][0], ring[i][1]);
        let (xj, yj) = (ring[j][0], ring[j][1]);
        if (yi > y) != (yj > y) && x < (xj - xi) * (y - yi) / (yj - yi) + xi {
            inside = !inside;
        }
        j = i;
    }
    inside
}

/// 脱密模式。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
pub enum DesensMode {
    /// 置平：区域内高程 = 区域原始均值 + delta。
    Flatten,
    /// 受控噪声：区域内高程 += delta × u，u ∈ [−1, 1]（seed 可复现）。
    Noise,
}

impl DesensMode {
    fn parse(s: &str) -> Result<DesensMode, DemError> {
        match s {
            "flatten" => Ok(DesensMode::Flatten),
            "noise" => Ok(DesensMode::Noise),
            other => Err(DemError::Param(format!(
                "mode `{other}` 不支持（仅 flatten / noise）"
            ))),
        }
    }

    fn as_str(&self) -> &'static str {
        match self {
            DesensMode::Flatten => "flatten",
            DesensMode::Noise => "noise",
        }
    }
}

/// 脱密选项。
#[derive(Debug, Clone)]
pub struct DesensitizeOptions {
    /// 脱密区域。
    pub region: Region,
    /// 模式。
    pub mode: DesensMode,
    /// delta：flatten 时为置平目标相对区域均值的偏移（米）；noise 时为噪声界（米，> 0）。
    pub delta: f64,
    /// 噪声种子（可复现）。
    pub seed: u64,
}

/// 区域高程统计。
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct RegionStats {
    /// 区域内像素数。
    pub count: u64,
    /// 最小值。
    pub min: f64,
    /// 最大值。
    pub max: f64,
    /// 均值。
    pub mean: f64,
}

/// 脱密记录（审批留痕）。
#[derive(Debug, Clone, Serialize)]
pub struct DesensitizeRecord {
    /// 输入文件路径。
    pub input: String,
    /// 输出文件路径。
    pub output: String,
    /// 脱密模式。
    pub mode: String,
    /// delta 参数。
    pub delta: f64,
    /// 噪声种子。
    pub seed: u64,
    /// 脱密区域。
    pub region: Region,
    /// 区域内像素数。
    pub region_pixels: u64,
    /// 脱密前区域统计。
    pub before: RegionStats,
    /// 脱密后区域统计。
    pub after: RegionStats,
    /// 识别出的 CRS（审批链路需明确坐标语义）。
    pub crs: crs::CrsInfo,
    /// 像元尺寸（来自 33550）。
    pub pixel_scale: [f64; 3],
    /// 连接点原点（来自 33922 首组）。
    pub tiepoint_origin: [f64; 4],
}

/// CLI 入口：读 DEM + 区域 JSON，执行脱密，写出 GeoTIFF 与记录 JSON。
///
/// `mode`：`flatten` / `noise`。`delta`：省略时 flatten 视为 0，noise 报错。
/// `seed`：noise 必填（保证可复现）；flatten 忽略。
pub fn run_from_files(
    input: &Path,
    region_path: &Path,
    mode: &str,
    delta: Option<f64>,
    seed: Option<u64>,
    out: &Path,
    record_path: &Path,
) -> Result<DesensitizeRecord, String> {
    let mode = DesensMode::parse(mode).map_err(|e| e.to_string())?;
    let region_text = std::fs::read_to_string(region_path)
        .map_err(|e| format!("无法读取区域 {}: {e}", region_path.display()))?;
    let region: Region = serde_json::from_str(&region_text).map_err(|e| {
        format!(
            "区域 JSON 解析失败：{e}（支持两种形式：\
             矩形 {{\"min_x\":..,\"max_x\":..,\"min_y\":..,\"max_y\":..}} 或 \
             多边形 {{\"ring\":[[x,y],...]}}）"
        )
    })?;
    let delta = delta.unwrap_or(0.0);
    if !delta.is_finite() {
        return Err("delta 必须为有限数".into());
    }
    if mode == DesensMode::Noise {
        if delta <= 0.0 {
            return Err("noise 模式要求 delta > 0（受控噪声界，单位米）".into());
        }
        if seed.is_none() {
            return Err("noise 模式必须提供 --seed（可复现要求，禁止隐式随机）".into());
        }
    }
    let opts = DesensitizeOptions {
        region,
        mode,
        delta,
        seed: seed.unwrap_or(0),
    };
    desensitize(input, &opts, out, record_path).map_err(|e| e.to_string())
}

/// 执行脱密并写出结果（核心入口，测试直接调用）。
pub fn desensitize(
    input: &Path,
    opts: &DesensitizeOptions,
    out: &Path,
    record_path: &Path,
) -> Result<DesensitizeRecord, DemError> {
    opts.region.validate()?;
    let bytes = std::fs::read(input)
        .map_err(|e| DemError::Io(format!("无法读取 {}: {e}", input.display())))?;
    let raster = tiff::read_raster(&bytes)?;
    let geo = tiff::read_geo_tags(&bytes)?;
    let crs_info = crs::identify_geotiff(&bytes).map_err(DemError::Crs)?;

    let (width, height) = (raster.width, raster.height);
    if raster.samples.is_empty() {
        return Err(DemError::Tiff(TiffError::Invalid("空栅格".into())));
    }

    // 像素中心地理坐标（北向上网格，GDAL 兼容约定）
    let (sx, sy) = (geo.pixel_scale[0], geo.pixel_scale[1]);
    if sx <= 0.0 || sy <= 0.0 {
        return Err(DemError::Param(format!(
            "ModelPixelScale 非法（SX={sx}, SY={sy}，要求 > 0，北向上网格）"
        )));
    }
    let (i0, j0, x0, y0) = (
        geo.tiepoint[0],
        geo.tiepoint[1],
        geo.tiepoint[3],
        geo.tiepoint[4],
    );

    // 区域掩膜 + 脱密前统计
    let n = width as usize * height as usize;
    let mut mask = vec![false; n];
    let mut count = 0u64;
    let mut before_sum = 0.0f64;
    let mut before_min = f64::INFINITY;
    let mut before_max = f64::NEG_INFINITY;
    for row in 0..height {
        for col in 0..width {
            let x = x0 + (f64::from(col) + 0.5 - i0) * sx;
            let y = y0 - (f64::from(row) + 0.5 - j0) * sy;
            if opts.region.contains(x, y) {
                let idx = row as usize * width as usize + col as usize;
                let v = raster.samples.get_f64(idx);
                if !v.is_finite() {
                    return Err(DemError::Param(format!(
                        "像素 ({col},{row}) 高程 {v} 非有限数"
                    )));
                }
                mask[idx] = true;
                count += 1;
                before_sum += v;
                before_min = before_min.min(v);
                before_max = before_max.max(v);
            }
        }
    }
    if count == 0 {
        return Err(DemError::Param(format!(
            "脱密区域 {} 未覆盖任何像素，请检查区域坐标（单位应与 CRS `{}` 一致）",
            opts.region.describe(),
            crs_info.name
        )));
    }
    let before_mean = before_sum / count as f64;
    let before = RegionStats {
        count,
        min: before_min,
        max: before_max,
        mean: before_mean,
    };

    // 应用脱密
    let mut out_samples = raster.samples.clone();
    match opts.mode {
        DesensMode::Flatten => {
            let target = before_mean + opts.delta;
            for (idx, m) in mask.iter().enumerate() {
                if *m {
                    out_samples.set_f64(idx, target);
                }
            }
        }
        DesensMode::Noise => {
            for (idx, m) in mask.iter().enumerate() {
                if *m {
                    let u = noise_unit(opts.seed, idx as u64);
                    out_samples.set_f64(idx, raster.samples.get_f64(idx) + opts.delta * u);
                }
            }
        }
    }

    // 脱密后统计（区域外已保证不变）
    let mut after_sum = 0.0f64;
    let mut after_min = f64::INFINITY;
    let mut after_max = f64::NEG_INFINITY;
    for (idx, m) in mask.iter().enumerate() {
        if *m {
            let v = out_samples.get_f64(idx);
            after_sum += v;
            after_min = after_min.min(v);
            after_max = after_max.max(v);
        }
    }
    let after = RegionStats {
        count,
        min: after_min,
        max: after_max,
        mean: after_sum / count as f64,
    };

    // 写出脱密后 GeoTIFF（地理标签原样保留）
    let out_raster = Raster {
        width,
        height,
        samples: out_samples,
    };
    let out_geo = GeoTags {
        pixel_scale: geo.pixel_scale,
        tiepoint: geo.tiepoint.clone(),
        geo_keys: geo.geo_keys.clone(),
        geo_ascii_params: geo.geo_ascii_params.clone(),
    };
    let out_bytes = tiff::write_geo_tiff(&out_raster, &out_geo)
        .map_err(|e| DemError::Io(format!("写出 GeoTIFF 失败：{e}")))?;
    std::fs::write(out, &out_bytes)
        .map_err(|e| DemError::Io(format!("无法写 {}: {e}", out.display())))?;

    let record = DesensitizeRecord {
        input: display_path(input),
        output: display_path(out),
        mode: opts.mode.as_str().into(),
        delta: opts.delta,
        seed: opts.seed,
        region: opts.region.clone(),
        region_pixels: count,
        before,
        after,
        crs: crs_info,
        pixel_scale: geo.pixel_scale,
        tiepoint_origin: [i0, j0, x0, y0],
    };
    let json = serde_json::to_string_pretty(&record)
        .map_err(|e| DemError::Io(format!("记录序列化失败：{e}")))?;
    std::fs::write(record_path, json + "\n")
        .map_err(|e| DemError::Io(format!("无法写 {}: {e}", record_path.display())))?;
    Ok(record)
}

fn display_path(p: &Path) -> String {
    p.display().to_string()
}

/// 受控噪声：splitmix64 散列（seed, 像素索引）→ [−1, 1]。
///
/// 逐像素散列保证与处理顺序无关，同一 (seed, 索引) 永远得到同一噪声值。
fn noise_unit(seed: u64, idx: u64) -> f64 {
    let mut z = seed ^ idx.wrapping_mul(0x9E37_79B9_7F4A_7C15);
    z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
    z ^= z >> 31;
    (z as f64 / u64::MAX as f64) * 2.0 - 1.0
}

/// 测试与 fixture 共用：提交到 testdata/ 的确定性 DEM 样例路径。
pub fn testdata_fixture_path() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/geo/dem_cgcs2000_u16.tif")
}

/// 生成确定性测试 fixture：32×32 uint16，EPSG:4490（GeoKey 3072），
/// 像元 0.001°，连接点 (116.0, 40.0)，高程 = 50 + (row + col) % 101。
pub fn build_fixture() -> Vec<u8> {
    let (w, h) = (32u32, 32u32);
    let mut values = Vec::with_capacity((w * h) as usize);
    for row in 0..h {
        for col in 0..w {
            values.push(50u16 + ((row + col) % 101) as u16);
        }
    }
    let raster = Raster {
        width: w,
        height: h,
        samples: tiff::Samples::U16(values),
    };
    // GeoKeyDirectory：3072=4490（CGCS2000），2048=4490，GTModelTypeGeoKey 1（投影/地面坐标系族）
    let geo_keys: Vec<u16> = vec![
        1, 1, 0, 3, // 头：版本 1.1.0，3 个 key
        1024, 0, 1, 1, // GTModelTypeGeoKey = ModelTypeGeographic
        2048, 0, 1, 4490, // GeographicTypeGeoKey = CGCS2000
        3072, 0, 1, 4490, // ProjectedCSTypeGeoKey = CGCS2000
    ];
    let geo = GeoTags {
        pixel_scale: [0.001, 0.001, 0.0],
        tiepoint: vec![0.0, 0.0, 0.0, 116.0, 40.0, 0.0],
        geo_keys,
        geo_ascii_params: None,
    };
    tiff::write_geo_tiff(&raster, &geo).expect("fixture 生成失败")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tiff::Samples;

    fn temp_dir(name: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("tangis-geo-dem-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    /// 8×8 float32 样例：高程 = 100 + row*10 + col，像元 1m，原点 (0, 80)。
    fn f32_dem() -> Vec<u8> {
        let (w, h) = (8u32, 8u32);
        let values: Vec<f32> = (0..h)
            .flat_map(|row| {
                (0..w).map(move |col| 100.0 + f32::from(row as u16) * 10.0 + f32::from(col as u16))
            })
            .collect();
        let raster = Raster {
            width: w,
            height: h,
            samples: Samples::F32(values),
        };
        let geo = GeoTags {
            pixel_scale: [1.0, 1.0, 0.0],
            tiepoint: vec![0.0, 0.0, 0.0, 0.0, 80.0, 0.0],
            geo_keys: vec![1, 1, 0, 1, 2048, 0, 1, 4490],
            geo_ascii_params: None,
        };
        tiff::write_geo_tiff(&raster, &geo).unwrap()
    }

    #[test]
    fn flatten_bbox_region_and_roundtrip() {
        let dir = temp_dir("flatten");
        let input = dir.join("dem.tif");
        std::fs::write(&input, f32_dem()).unwrap();

        // 区域 x∈[1.5, 4.5]，y∈[72.5, 76.5]（含边界，中心判定）：
        // 列 1..4（x=1.5..4.5）× 行 3..7（y=76.5..72.5）= 4×5 = 20 像素
        let region_path = dir.join("region.json");
        std::fs::write(
            &region_path,
            r#"{"min_x":1.5,"max_x":4.5,"min_y":72.5,"max_y":76.5}"#,
        )
        .unwrap();
        let out = dir.join("out.tif");
        let record_path = dir.join("record.json");
        let record = desensitize(
            &input,
            &DesensitizeOptions {
                region: Region::Bbox {
                    min_x: 1.5,
                    max_x: 4.5,
                    min_y: 72.5,
                    max_y: 76.5,
                },
                mode: DesensMode::Flatten,
                delta: 5.0,
                seed: 0,
            },
            &out,
            &record_path,
        )
        .unwrap();
        assert_eq!(record.region_pixels, 20);

        // 前统计：该区域高程 100+row*10+col，row 3..7、col 1..4
        let expected_mean: f64 = (0..20usize)
            .map(|k| {
                let row = 3 + k / 4;
                let col = 1 + k % 4;
                100.0 + row as f64 * 10.0 + col as f64
            })
            .sum::<f64>()
            / 20.0;
        assert!((record.before.mean - expected_mean).abs() < 1e-9);
        assert!((record.before.min - (100.0 + 30.0 + 1.0)).abs() < 1e-9);
        assert!((record.before.max - (100.0 + 70.0 + 4.0)).abs() < 1e-9);

        // 置平 = 均值 + delta
        assert!((record.after.mean - (expected_mean + 5.0)).abs() < 1e-9);
        assert!((record.after.min - record.after.max).abs() < 1e-9);

        // 读回：区域内常数、区域外不变、地理标签保留
        let out_bytes = std::fs::read(&out).unwrap();
        let raster = tiff::read_raster(&out_bytes).unwrap();
        let geo = tiff::read_geo_tags(&out_bytes).unwrap();
        assert_eq!(geo.geo_keys, vec![1, 1, 0, 1, 2048, 0, 1, 4490]);
        assert_eq!(geo.pixel_scale, [1.0, 1.0, 0.0]);
        let target = expected_mean + 5.0;
        for row in 0..8u32 {
            for col in 0..8u32 {
                let idx = (row * 8 + col) as usize;
                let v = raster.samples.get_f64(idx);
                let x = (f64::from(col) + 0.5) * 1.0;
                let y = 80.0 - (f64::from(row) + 0.5) * 1.0;
                let inside = (1.5..=4.5).contains(&x) && (72.5..=76.5).contains(&y);
                if inside {
                    assert!((v - target).abs() < 1e-6, "({col},{row}) v={v}");
                } else {
                    let orig = 100.0 + f64::from(row) * 10.0 + f64::from(col);
                    assert!((v - orig).abs() < 1e-6, "区域外被改动 ({col},{row}) v={v}");
                }
            }
        }

        // 记录 JSON 可解析且字段齐全
        let rec: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(&record_path).unwrap()).unwrap();
        assert_eq!(rec["mode"], "flatten");
        assert_eq!(rec["crs"]["epsg"], 4490);
        assert_eq!(rec["region_pixels"], 20);

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn noise_is_bounded_reproducible_and_outside_untouched() {
        let dir = temp_dir("noise");
        let input = dir.join("dem.tif");
        std::fs::write(&input, f32_dem()).unwrap();
        let region = Region::Bbox {
            min_x: -10.0,
            max_x: 10.0,
            min_y: -10.0,
            max_y: 90.0, // 全图
        };
        let opts = DesensitizeOptions {
            region,
            mode: DesensMode::Noise,
            delta: 3.0,
            seed: 42,
        };
        let out1 = dir.join("n1.tif");
        let out2 = dir.join("n2.tif");
        let r1 = desensitize(&input, &opts, &out1, &dir.join("r1.json")).unwrap();
        let r2 = desensitize(&input, &opts, &out2, &dir.join("r2.json")).unwrap();
        // 可复现：同 seed 字节级一致
        assert_eq!(
            std::fs::read(&out1).unwrap(),
            std::fs::read(&out2).unwrap()
        );
        // 有界：|Δ| ≤ delta；且确实发生了变化
        let src = tiff::read_raster(&f32_dem()).unwrap();
        let dst = tiff::read_raster(&std::fs::read(&out1).unwrap()).unwrap();
        let mut changed = 0;
        for i in 0..64usize {
            let d = dst.samples.get_f64(i) - src.samples.get_f64(i);
            assert!(d.abs() <= 3.0 + 1e-9, "噪声越界：{d}");
            if d.abs() > 1e-9 {
                changed += 1;
            }
        }
        assert!(changed > 32, "全图噪声应产生明显变化，仅 {changed} 像素变化");
        assert_eq!(r1.after.min, r2.after.min);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn different_seeds_give_different_results() {
        let dir = temp_dir("seeds");
        let input = dir.join("dem.tif");
        std::fs::write(&input, f32_dem()).unwrap();
        let region = Region::Bbox {
            min_x: -10.0,
            max_x: 10.0,
            min_y: -10.0,
            max_y: 90.0,
        };
        let o1 = DesensitizeOptions { region: region.clone(), mode: DesensMode::Noise, delta: 2.0, seed: 1 };
        let o2 = DesensitizeOptions { region, mode: DesensMode::Noise, delta: 2.0, seed: 2 };
        let a = desensitize(&input, &o1, &dir.join("a.tif"), &dir.join("a.json")).unwrap();
        let b = desensitize(&input, &o2, &dir.join("b.tif"), &dir.join("b.json")).unwrap();
        let ta = tiff::read_raster(&std::fs::read(dir.join("a.tif")).unwrap()).unwrap();
        let tb = tiff::read_raster(&std::fs::read(dir.join("b.tif")).unwrap()).unwrap();
        assert!(ta.samples != tb.samples, "不同 seed 应产生不同噪声");
        assert_ne!(a.after.mean, b.after.mean);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn u16_flatten_with_clamp() {
        let dir = temp_dir("u16");
        let raster = Raster {
            width: 4,
            height: 4,
            samples: Samples::U16(vec![100, 200, 300, 400, 100, 200, 300, 400, 100, 200, 300, 400, 100, 200, 300, 400]),
        };
        let geo = GeoTags {
            pixel_scale: [0.001, 0.001, 0.0],
            tiepoint: vec![0.0, 0.0, 0.0, 116.0, 40.0, 0.0],
            geo_keys: vec![1, 1, 0, 1, 2048, 0, 1, 4490],
            geo_ascii_params: None,
        };
        let input = dir.join("dem.tif");
        std::fs::write(&input, tiff::write_geo_tiff(&raster, &geo).unwrap()).unwrap();
        // 全图置平到均值 + (−1000) → 饱和到 0
        let region = Region::Bbox {
            min_x: 115.0,
            max_x: 117.0,
            min_y: 39.0,
            max_y: 41.0,
        };
        let out = dir.join("out.tif");
        let record = desensitize(
            &input,
            &DesensitizeOptions {
                region,
                mode: DesensMode::Flatten,
                delta: -1000.0,
                seed: 0,
            },
            &out,
            &dir.join("rec.json"),
        )
        .unwrap();
        assert_eq!(record.region_pixels, 16);
        assert_eq!(record.after.max, 0.0);
        let dst = tiff::read_raster(&std::fs::read(&out).unwrap()).unwrap();
        for i in 0..16 {
            assert_eq!(dst.samples.get_f64(i), 0.0);
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn polygon_region() {
        let dir = temp_dir("polygon");
        let input = dir.join("dem.tif");
        std::fs::write(&input, f32_dem()).unwrap();
        // 三角形 (0,80)-(8,80)-(8,72)：右上半个图（含对角线附近）
        let region_path = dir.join("poly.json");
        std::fs::write(
            &region_path,
            r#"{"ring":[[0,80],[8,80],[8,72],[0,80]]}"#,
        )
        .unwrap();
        let out = dir.join("out.tif");
        let record = desensitize(
            &input,
            &DesensitizeOptions {
                region: Region::Polygon {
                    ring: vec![[0.0, 80.0], [8.0, 80.0], [8.0, 72.0], [0.0, 80.0]],
                },
                mode: DesensMode::Flatten,
                delta: 0.0,
                seed: 0,
            },
            &out,
            &dir.join("rec.json"),
        )
        .unwrap();
        assert!(record.region_pixels > 16 && record.region_pixels < 64);
        // 独立射线法核对数量
        let mut expect = 0u64;
        for row in 0..8u32 {
            for col in 0..8u32 {
                let x = f64::from(col) + 0.5;
                let y = 80.0 - (f64::from(row) + 0.5);
                if point_in_ring(x, y, &[[0.0, 80.0], [8.0, 80.0], [8.0, 72.0], [0.0, 80.0]]) {
                    expect += 1;
                }
            }
        }
        assert_eq!(record.region_pixels, expect);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn errors_are_loud() {
        let dir = temp_dir("errors");
        let input = dir.join("dem.tif");
        std::fs::write(&input, f32_dem()).unwrap();
        let opts_full_region = |region| DesensitizeOptions {
            region,
            mode: DesensMode::Flatten,
            delta: 0.0,
            seed: 0,
        };

        // 区域不覆盖任何像素
        let err = desensitize(
            &input,
            &opts_full_region(Region::Bbox {
                min_x: 500.0,
                max_x: 600.0,
                min_y: 0.0,
                max_y: 1.0,
            }),
            &dir.join("x.tif"),
            &dir.join("x.json"),
        )
        .unwrap_err();
        assert!(err.to_string().contains("未覆盖任何像素"), "{err}");

        // bbox min >= max
        let err = desensitize(
            &input,
            &opts_full_region(Region::Bbox {
                min_x: 5.0,
                max_x: 5.0,
                min_y: 0.0,
                max_y: 1.0,
            }),
            &dir.join("x.tif"),
            &dir.join("x.json"),
        )
        .unwrap_err();
        assert!(err.to_string().contains("min < max"), "{err}");

        // 多边形顶点不足
        let err = desensitize(
            &input,
            &opts_full_region(Region::Polygon {
                ring: vec![[0.0, 0.0], [1.0, 1.0]],
            }),
            &dir.join("x.tif"),
            &dir.join("x.json"),
        )
        .unwrap_err();
        assert!(err.to_string().contains("顶点数"), "{err}");

        // 无 GeoKey 的 GeoTIFF → CRS 拒绝
        let raster = Raster {
            width: 2,
            height: 2,
            samples: Samples::F32(vec![0.0; 4]),
        };
        let geo = GeoTags {
            pixel_scale: [1.0, 1.0, 0.0],
            tiepoint: vec![0.0, 0.0, 0.0, 0.0, 0.0, 0.0],
            geo_keys: vec![1, 1, 0, 0],
            geo_ascii_params: None,
        };
        let no_crs = dir.join("no_crs.tif");
        std::fs::write(&no_crs, tiff::write_geo_tiff(&raster, &geo).unwrap()).unwrap();
        let err = desensitize(
            &no_crs,
            &opts_full_region(Region::Bbox {
                min_x: -1.0,
                max_x: 1.0,
                min_y: -1.0,
                max_y: 1.0,
            }),
            &dir.join("x.tif"),
            &dir.join("x.json"),
        )
        .unwrap_err();
        assert!(err.to_string().contains("3072"), "{err}");

        // run_from_files 参数路径
        let region_path = dir.join("region.json");
        std::fs::write(&region_path, r#"{"min_x":0,"max_x":8,"min_y":70,"max_y":80}"#).unwrap();
        let err = run_from_files(&input, &region_path, "smooth", None, None, &dir.join("o.tif"), &dir.join("o.json"))
            .unwrap_err();
        assert!(err.contains("mode"), "{err}");
        let err = run_from_files(&input, &region_path, "noise", None, None, &dir.join("o.tif"), &dir.join("o.json"))
            .unwrap_err();
        assert!(err.contains("delta > 0"), "{err}");
        let err = run_from_files(&input, &region_path, "noise", Some(5.0), None, &dir.join("o.tif"), &dir.join("o.json"))
            .unwrap_err();
        assert!(err.contains("--seed"), "{err}");

        let _ = std::fs::remove_dir_all(&dir);
    }

    /// testdata fixture：确定性生成 + 完整 round-trip（读 → 脱密 → 写 → 读回）。
    #[test]
    fn testdata_fixture_roundtrip() {
        let fixture = testdata_fixture_path();
        if let Some(parent) = fixture.parent() {
            std::fs::create_dir_all(parent).unwrap();
        }
        std::fs::write(&fixture, build_fixture()).unwrap();

        let bytes = std::fs::read(&fixture).unwrap();
        let info = crs::identify_geotiff(&bytes).unwrap();
        assert_eq!(info.epsg, Some(4490));
        assert_eq!(info.name, "CGCS2000");

        let dir = temp_dir("fixture");
        let region = Region::Bbox {
            min_x: 116.002,
            max_x: 116.02,
            min_y: 39.98,
            max_y: 39.996,
        };
        let out = dir.join("desensitized.tif");
        let record = desensitize(
            &fixture,
            &DesensitizeOptions {
                region,
                mode: DesensMode::Noise,
                delta: 25.0,
                seed: 2026,
            },
            &out,
            &dir.join("record.json"),
        )
        .unwrap();
        assert_eq!(record.region_pixels, 288); // 18 列 × 16 行（中心判定，含边界）
        assert_eq!(record.crs.epsg, Some(4490));
        // 噪声有界：脱密后区域值域展宽 ≤ 原始值域 + 2×delta
        assert!(
            record.after.max - record.after.min
                <= (record.before.max - record.before.min) + 2.0 * record.delta + 1e-9
        );
        // 记录写盘
        assert!(dir.join("record.json").exists());
        let _ = std::fs::remove_dir_all(&dir);
    }
}
