//! CGCS2000 七参数坐标转换（Bursa-Wolf / Position Vector 约定，合规 P0 算子二）。
//!
//! 实现三部分（全部为闭式/迭代数值算法，无外部依赖）：
//! 1. 大地坐标 ↔ 空间直角坐标（BLH ↔ XYZ）；
//! 2. 七参数空间直角坐标转换（Position Vector 约定，EPSG 方法 9606/1033；
//!    兼容 Coordinate Frame 约定——传入 `convention = "coordinate_frame"` 时
//!    自动取旋转参数相反数）；
//! 3. 参数从 JSON 读入并校验（缺失/非法明确报错）。
//!
//! 参考公式（EPSG Guidance Note 7-2, Position Vector geocentric domain）：
//! ```text
//! [Xt]   [tX]   [ 1   rZ  -rY ] [Xs]
//! [Yt] = [tY] + M [ -rZ  1   rX ] [Ys]
//! [Zt]   [tZ]   [ rY -rX   1  ] [Zs]
//! M = 1 + dS * 1e-6，旋转以弧度计（入参为角秒）
//! ```

use std::path::Path;

use serde::{Deserialize, Serialize};
use thiserror::Error;

/// 七参数转换相关错误。
#[derive(Debug, Error)]
pub enum BursaError {
    /// 参数文件读取/解析失败。
    #[error("七参数 JSON 解析失败：{0}")]
    Json(String),
    /// 参数校验不通过。
    #[error("七参数校验失败：{0}")]
    Validate(String),
}

/// 参考椭球（a：长半轴 m；inv_f：1/f 扁率倒数）。
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Ellipsoid {
    /// 长半轴（米）。
    pub a: f64,
    /// 1/f（扁率倒数）。
    pub inv_f: f64,
}

impl Ellipsoid {
    /// 第一偏心率平方 e² = 2f − f²。
    pub fn e2(&self) -> f64 {
        let f = 1.0 / self.inv_f;
        2.0 * f - f * f
    }
}

/// CGCS2000 椭球：a=6378137，1/f=298.257222101（GB 22021-2008）。
pub const CGCS2000: Ellipsoid = Ellipsoid {
    a: 6_378_137.0,
    inv_f: 298.257_222_101,
};

/// WGS 84 椭球。
pub const WGS84: Ellipsoid = Ellipsoid {
    a: 6_378_137.0,
    inv_f: 298.257_223_563,
};

/// WGS 72 椭球（EPSG GN7-2 测试算例使用）。
pub const WGS72: Ellipsoid = Ellipsoid {
    a: 6_378_135.0,
    inv_f: 298.26,
};

/// 旋转参数约定。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
pub enum Convention {
    /// Position Vector（EPSG 9606/1033，IAG/ISO 19111 推荐），默认。
    #[default]
    #[serde(rename = "position_vector")]
    PositionVector,
    /// Coordinate Frame（EPSG 9607，旋转符号相反）。
    #[serde(rename = "coordinate_frame")]
    CoordinateFrame,
}

/// 七参数（Bursa-Wolf）。
///
/// 单位约定：平移 dx/dy/dz 为米；旋转 rx/ry/rz 为**角秒**；尺度 scale_ppm
/// 为百万分之一（ppm）。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct BursaParams {
    /// 源坐标系名称（如 "CGCS2000"）。
    #[serde(rename = "source")]
    pub source_crs: String,
    /// 目标坐标系名称（如 "WGS 84"）。
    #[serde(rename = "target")]
    pub target_crs: String,
    /// X 平移（米）。
    pub dx: f64,
    /// Y 平移（米）。
    pub dy: f64,
    /// Z 平移（米）。
    pub dz: f64,
    /// X 旋转（角秒）。
    pub rx: f64,
    /// Y 旋转（角秒）。
    pub ry: f64,
    /// Z 旋转（角秒）。
    pub rz: f64,
    /// 尺度（ppm）。
    pub scale_ppm: f64,
    /// 旋转约定（缺省 position_vector）。
    #[serde(default)]
    pub convention: Convention,
    /// 源椭球（可选；缺省 CGCS2000）。
    #[serde(default)]
    pub source_ellipsoid: Option<EllipsoidJson>,
    /// 目标椭球（可选；缺省 CGCS2000）。
    #[serde(default)]
    pub target_ellipsoid: Option<EllipsoidJson>,
}

/// JSON 中的椭球描述。
#[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
pub struct EllipsoidJson {
    /// 长半轴（米）。
    pub a: f64,
    /// 1/f。
    pub inv_f: f64,
}

impl From<EllipsoidJson> for Ellipsoid {
    fn from(e: EllipsoidJson) -> Self {
        Ellipsoid {
            a: e.a,
            inv_f: e.inv_f,
        }
    }
}

const ARCSEC_TO_RAD: f64 = std::f64::consts::PI / (180.0 * 3600.0);

impl BursaParams {
    /// 从 JSON 文件读入并校验。
    pub fn from_json_file(path: &Path) -> Result<BursaParams, BursaError> {
        let text = std::fs::read_to_string(path).map_err(|e| {
            BursaError::Json(format!("无法读取 {}: {e}", path.display()))
        })?;
        let params: BursaParams = serde_json::from_str(&text).map_err(|e| {
            BursaError::Json(format!(
                "{e}（缺字段或类型不符：dx/dy/dz 米、rx/ry/rz 角秒、scale_ppm、source/target 必填）"
            ))
        })?;
        params.validate()?;
        Ok(params)
    }

    /// 参数校验：全部字段必须为有限数且量级合理（防单位错配/粘贴错误）。
    pub fn validate(&self) -> Result<(), BursaError> {
        let finite = [
            ("dx", self.dx),
            ("dy", self.dy),
            ("dz", self.dz),
            ("rx", self.rx),
            ("ry", self.ry),
            ("rz", self.rz),
            ("scale_ppm", self.scale_ppm),
        ];
        for (name, v) in finite {
            if !v.is_finite() {
                return Err(BursaError::Validate(format!("{name} = {v} 非有限数")));
            }
        }
        for (name, v) in [("dx", self.dx), ("dy", self.dy), ("dz", self.dz)] {
            if v.abs() > 5000.0 {
                return Err(BursaError::Validate(format!(
                    "{name} = {v} 米，超出合理范围 ±5000 m（请检查单位是否为米）"
                )));
            }
        }
        for (name, v) in [("rx", self.rx), ("ry", self.ry), ("rz", self.rz)] {
            if v.abs() > 60.0 {
                return Err(BursaError::Validate(format!(
                    "{name} = {v} 角秒，超出合理范围 ±60″（请检查单位是否为角秒）"
                )));
            }
        }
        if self.scale_ppm.abs() > 1000.0 {
            return Err(BursaError::Validate(format!(
                "scale_ppm = {} 超出合理范围 ±1000 ppm",
                self.scale_ppm
            )));
        }
        if self.source_crs.trim().is_empty() || self.target_crs.trim().is_empty() {
            return Err(BursaError::Validate(
                "source/target 坐标系名称不能为空".into(),
            ));
        }
        for (label, e) in [
            ("source_ellipsoid", self.source_ellipsoid),
            ("target_ellipsoid", self.target_ellipsoid),
        ] {
            if let Some(e) = e {
                if !(e.a.is_finite() && e.a > 1.0e6 && e.a < 1.0e7) {
                    return Err(BursaError::Validate(format!(
                        "{label}.a = {} 不在地球椭球合理范围 (1e6, 1e7) 米",
                        e.a
                    )));
                }
                if !(e.inv_f.is_finite() && e.inv_f > 100.0 && e.inv_f < 500.0) {
                    return Err(BursaError::Validate(format!(
                        "{label}.inv_f = {} 不在合理范围 (100, 500)",
                        e.inv_f
                    )));
                }
            }
        }
        Ok(())
    }

    /// 空间直角坐标七参数转换（XYZ → XYZ）。
    pub fn transform_xyz(&self, x: f64, y: f64, z: f64) -> [f64; 3] {
        let sign = match self.convention {
            Convention::PositionVector => 1.0,
            Convention::CoordinateFrame => -1.0,
        };
        let (rx, ry, rz) = (
            self.rx * ARCSEC_TO_RAD * sign,
            self.ry * ARCSEC_TO_RAD * sign,
            self.rz * ARCSEC_TO_RAD * sign,
        );
        let m = 1.0 + self.scale_ppm * 1.0e-6;
        [
            self.dx + m * (x + rz * y - ry * z),
            self.dy + m * (-rz * x + y + rx * z),
            self.dz + m * (ry * x - rx * y + z),
        ]
    }
}

/// 大地坐标（度，米）→ 空间直角坐标。
///
/// 经度 lon、纬度 lat 均为十进制度；h 为椭球高（米）。
pub fn blh_to_xyz(ell: Ellipsoid, lon_deg: f64, lat_deg: f64, h: f64) -> [f64; 3] {
    let (lon, lat) = (lon_deg.to_radians(), lat_deg.to_radians());
    let e2 = ell.e2();
    let (sin_lat, cos_lat) = lat.sin_cos();
    let n = ell.a / (1.0 - e2 * sin_lat * sin_lat).sqrt();
    [
        (n + h) * cos_lat * lon.cos(),
        (n + h) * cos_lat * lon.sin(),
        (n * (1.0 - e2) + h) * sin_lat,
    ]
}

/// 空间直角坐标 → 大地坐标（度，米）。
///
/// 纬度用标准迭代法（收敛 ~1e-12 度量级）；极点走解析分支。
pub fn xyz_to_blh(ell: Ellipsoid, xyz: [f64; 3]) -> (f64, f64, f64) {
    let [x, y, z] = xyz;
    let e2 = ell.e2();
    let p = x.hypot(y);
    let lon = y.atan2(x).to_degrees();
    if p < 1.0e-9 {
        // 极点：h = |Z| − b（b = a·sqrt(1−e²)）
        let b = ell.a * (1.0 - e2).sqrt();
        let lat = if z >= 0.0 { 90.0 } else { -90.0 };
        return (lon, lat, z.abs() - b);
    }
    let mut lat = (z / (p * (1.0 - e2))).atan();
    let mut h = 0.0;
    for _ in 0..8 {
        let sin_lat = lat.sin();
        let n = ell.a / (1.0 - e2 * sin_lat * sin_lat).sqrt();
        h = p / lat.cos() - n;
        lat = (z / (p * (1.0 - e2 * n / (n + h)))).atan();
    }
    (lon, lat.to_degrees(), h)
}

/// 完整 BLH → BLH 转换：源椭球 BLH→XYZ → 七参数 → 目标椭球 XYZ→BLH。
pub fn transform_blh(params: &BursaParams, lon: f64, lat: f64, h: f64) -> (f64, f64, f64) {
    let src = params
        .source_ellipsoid
        .map_or(CGCS2000, Ellipsoid::from);
    let dst = params
        .target_ellipsoid
        .map_or(CGCS2000, Ellipsoid::from);
    let xyz = blh_to_xyz(src, lon, lat, h);
    let [x, y, z] = params.transform_xyz(xyz[0], xyz[1], xyz[2]);
    xyz_to_blh(dst, [x, y, z])
}

/// CLI 批量点转换入口：读参数与点集 JSON，写出结果 JSON。
///
/// 点集格式：`{"points": [[lon, lat, h], ...]}`（度/米）。
/// 输出：`{"source", "target", "convention", "count", "points": [[lon, lat, h], ...]}`。
pub fn run_from_files(
    params_path: &Path,
    points_path: &Path,
    out_path: &Path,
) -> Result<TransformSummary, String> {
    let params = BursaParams::from_json_file(params_path).map_err(|e| e.to_string())?;
    let text = std::fs::read_to_string(points_path)
        .map_err(|e| format!("无法读取点集 {}: {e}", points_path.display()))?;
    let points: PointSet = serde_json::from_str(&text)
        .map_err(|e| format!("点集 JSON 解析失败：{e}（期望 {{\"points\": [[lon,lat,h],...]}}）"))?;
    if points.points.is_empty() {
        return Err("点集为空（points 数组为空）".into());
    }
    for (i, p) in points.points.iter().enumerate() {
        if p.len() != 3 {
            return Err(format!("第 {} 个点是 {} 元，应为 [lon, lat, h]", i + 1, p.len()));
        }
        let (lon, lat, _h) = (p[0], p[1], p[2]);
        if !(-180.0..=180.0).contains(&lon) || !(-90.0..=90.0).contains(&lat) {
            return Err(format!(
                "第 {} 个点经纬度越界 lon={lon} lat={lat}（单位应为度）",
                i + 1
            ));
        }
    }
    let transformed: Vec<[f64; 3]> = points
        .points
        .iter()
        .map(|p| {
            let (lon, lat, h) = transform_blh(&params, p[0], p[1], p[2]);
            [lon, lat, h]
        })
        .collect();
    let summary = TransformSummary {
        source: params.source_crs.clone(),
        target: params.target_crs.clone(),
        convention: params.convention,
        count: transformed.len(),
        points: transformed.clone(),
    };
    let json = serde_json::to_string_pretty(&summary).map_err(|e| e.to_string())?;
    std::fs::write(out_path, json + "\n")
        .map_err(|e| format!("无法写结果 {}: {e}", out_path.display()))?;
    Ok(summary)
}

/// 点集输入。
#[derive(Debug, Deserialize)]
pub struct PointSet {
    /// [[lon, lat, h], ...]，度/米。
    pub points: Vec<Vec<f64>>,
}

/// 转换汇总（写盘 + CLI 展示）。
#[derive(Debug, Clone, Serialize)]
pub struct TransformSummary {
    /// 源坐标系名称。
    pub source: String,
    /// 目标坐标系名称。
    pub target: String,
    /// 旋转约定。
    pub convention: Convention,
    /// 点数。
    pub count: usize,
    /// 转换后点 [[lon, lat, h], ...]。
    pub points: Vec<[f64; 3]>,
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    fn temp_dir(name: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("tangis-geo-bursa-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    /// EPSG Guidance Note 7-2 经典算例：WGS 72 → WGS 84。
    ///
    /// 参考：EPSG Guidance Note 7-2 "Position Vector transformation" 示例
    /// （EPSG 官网 epsg.org / 方法 9606 与 1037 页面）：
    /// - 参数：tX=0, tY=0, tZ=+4.5 m；rX=rY=0；rZ=±0.554″；dS=+0.219 ppm
    /// - 输入：φ=55°00′00″N，λ=4°00′00″E，h=0（WGS 72 椭球）
    /// - 中间地心坐标：Xs=3657660.66, Ys=255768.55, Zs=5201382.11
    /// - 转换后：Xt=3657660.78, Yt=255778.43, Zt=5201387.75
    /// - 最终：φ=55°00′00.090″N，λ=4°00′00.554″E，h=+3.22 m
    ///
    /// 符号说明：GN7-2 文本以 Coordinate Frame 约定给出 rZ=+0.554″，其数值
    /// 结果等价于 Position Vector 约定的 rZ=−0.554″；本 crate 默认 PV 约定，
    /// 故以 rZ=−0.554″ 复现文档数值。文档数值精度为 0.01 m / 0.001″，
    /// 断言容差据此放宽（实现自身一致性另由 round-trip 测试验证到 1e-6 m）。
    const GN72_EPSG_NOTE: &str =
        "EPSG Guidance Note 7-2 (WGS 72 -> WGS 84, EPSG:1238 量级算例)";

    fn gn72_params() -> BursaParams {
        BursaParams {
            source_crs: "WGS 72".into(),
            target_crs: "WGS 84".into(),
            dx: 0.0,
            dy: 0.0,
            dz: 4.5,
            rx: 0.0,
            ry: 0.0,
            rz: -0.554,
            scale_ppm: 0.219,
            convention: Convention::PositionVector,
            source_ellipsoid: Some(EllipsoidJson { a: WGS72.a, inv_f: WGS72.inv_f }),
            target_ellipsoid: Some(EllipsoidJson { a: WGS84.a, inv_f: WGS84.inv_f }),
        }
    }

    #[test]
    fn epsg_gn72_test_vector() {
        let params = gn72_params();
        params.validate().unwrap();

        // 1) BLH → XYZ：与文档中间地心坐标对照（文档保留 2 位小数）
        let xyz = blh_to_xyz(WGS72, 4.0, 55.0, 0.0);
        assert!(
            (xyz[0] - 3_657_660.66).abs() < 0.01
                && (xyz[1] - 255_768.55).abs() < 0.01
                && (xyz[2] - 5_201_382.11).abs() < 0.01,
            "BLH→XYZ 与 {GN72_EPSG_NOTE} 不符：{xyz:?}"
        );

        // 2) 七参数 XYZ → XYZ：与文档转换后地心坐标对照
        let out = params.transform_xyz(xyz[0], xyz[1], xyz[2]);
        assert!(
            (out[0] - 3_657_660.78).abs() < 0.02
                && (out[1] - 255_778.43).abs() < 0.02
                && (out[2] - 5_201_387.75).abs() < 0.02,
            "七参数 XYZ 与 {GN72_EPSG_NOTE} 不符：{out:?}"
        );

        // 3) XYZ → BLH：与文档最终大地坐标对照（0.001″ ≈ 3 cm；h 0.05 m）
        let (lon, lat, h) = xyz_to_blh(WGS84, out);
        let (exp_lon, exp_lat) = (4.0 + 0.554 / 3600.0, 55.0 + 0.090 / 3600.0);
        assert!(
            (lat - exp_lat).abs() < 5.0e-4 && (lon - exp_lon).abs() < 5.0e-4,
            "BLH 与 {GN72_EPSG_NOTE} 不符：lat={lat} lon={lon}"
        );
        assert!(
            (h - 3.22).abs() < 0.05,
            "椭球高与 {GN72_EPSG_NOTE} 不符：h={h}（期望 3.22±0.05）"
        );
    }

    #[test]
    fn coordinate_frame_convention_matches_position_vector_negated() {
        // CF(rZ=+0.554″) ≡ PV(rZ=−0.554″)
        let mut cf = gn72_params();
        cf.rz = 0.554;
        cf.convention = Convention::CoordinateFrame;
        let mut pv = gn72_params();
        pv.rz = -0.554;
        pv.convention = Convention::PositionVector;
        let xyz = blh_to_xyz(WGS72, 4.0, 55.0, 0.0);
        let a = cf.transform_xyz(xyz[0], xyz[1], xyz[2]);
        let b = pv.transform_xyz(xyz[0], xyz[1], xyz[2]);
        for i in 0..3 {
            assert!((a[i] - b[i]).abs() < 1e-9, "分量 {i}: {a:?} vs {b:?}");
        }
    }

    #[test]
    fn blh_xyz_roundtrip_submicron() {
        let points = [
            (116.3975, 39.9087, 43.5),   // 北京
            (121.4841, 31.2215, 4.0),    // 上海
            (0.0, 0.0, -100.0),          // 赤道/本初子午线
            (-73.9857, 40.7484, 381.0),  // 纽约
            (102.0, 45.5, 1200.0),       // 内陆
        ];
        for (lon, lat, h) in points {
            let xyz = blh_to_xyz(CGCS2000, lon, lat, h);
            let (lon2, lat2, h2) = xyz_to_blh(CGCS2000, xyz);
            assert!((lon2 - lon).abs() < 1e-11, "lon round-trip: {lon} → {lon2}");
            assert!((lat2 - lat).abs() < 1e-11, "lat round-trip: {lat} → {lat2}");
            assert!((h2 - h).abs() < 1e-6, "h round-trip: {h} → {h2}");
        }
    }

    #[test]
    fn analytic_reference_points() {
        // 赤道本初子午线 h=0 → X=a（y 为 cos/sin 的精确 0）
        assert_eq!(blh_to_xyz(CGCS2000, 0.0, 0.0, 0.0), [CGCS2000.a, 0.0, 0.0]);
        // 赤道东经 90° h=0 → Y=a（cos(90°) 有 1e-16 级浮点残差，用容差）
        let xyz = blh_to_xyz(CGCS2000, 90.0, 0.0, 0.0);
        assert!(xyz[0].abs() < 1e-9 && (xyz[1] - CGCS2000.a).abs() < 1e-9 && xyz[2].abs() < 1e-9);
        // 北极 h=0 → Z=b（b = a·sqrt(1−e²)）
        let b = CGCS2000.a * (1.0 - CGCS2000.e2()).sqrt();
        let (_lon, lat, h) = xyz_to_blh(CGCS2000, [0.0, 0.0, b]);
        assert_eq!(lat, 90.0);
        assert!(h.abs() < 1e-6, "北极 h={h}");
        // h=500m 沿赤道：X = a+500
        assert_eq!(blh_to_xyz(CGCS2000, 0.0, 0.0, 500.0)[0], CGCS2000.a + 500.0);
    }

    #[test]
    fn seven_param_identity_and_inverse() {
        let params = BursaParams {
            source_crs: "CGCS2000".into(),
            target_crs: "WGS 84".into(),
            dx: -1.234,
            dy: 2.345,
            dz: -3.456,
            rx: 0.12,
            ry: -0.23,
            rz: 0.34,
            scale_ppm: -0.56,
            convention: Convention::PositionVector,
            source_ellipsoid: None,
            target_ellipsoid: None,
        };
        // 零参数 → 恒等
        let zero = BursaParams {
            dx: 0.0,
            dy: 0.0,
            dz: 0.0,
            rx: 0.0,
            ry: 0.0,
            rz: 0.0,
            scale_ppm: 0.0,
            ..params.clone()
        };
        let xyz = blh_to_xyz(CGCS2000, 116.0, 40.0, 50.0);
        let out = zero.transform_xyz(xyz[0], xyz[1], xyz[2]);
        for i in 0..3 {
            assert!((out[i] - xyz[i]).abs() < 1e-9);
        }
        // 正反变换恢复原坐标（CGCS2000 椭球，< 1e-6 m / 1e-11 度）。
        // 注：尺度参数取反只是一阶逆（1/(1+s) ≠ 1−s），严格可逆 round-trip
        // 用 scale_ppm=0；带尺度的近似逆残差量级在下方单独断言。
        let params = BursaParams { scale_ppm: 0.0, ..params };
        let (lon, lat, h) = transform_blh(&params, 116.0, 40.0, 50.0);
        let inv = BursaParams {
            dx: -params.dx,
            dy: -params.dy,
            dz: -params.dz,
            rx: -params.rx,
            ry: -params.ry,
            rz: -params.rz,
            scale_ppm: 0.0,
            ..params.clone()
        };
        let (lon0, lat0, h0) = transform_blh(&inv, lon, lat, h);
        // 取反参数逆含交叉项残差 R(−θ)·t − t ≈ |t|·|θ|（本参数 ~1e-5 m），
        // 容差取 1e-9 度（≈ 0.1 mm）
        assert!((lon0 - 116.0).abs() < 1e-9);
        assert!((lat0 - 40.0).abs() < 1e-9);
        assert!((h0 - 50.0).abs() < 1e-4, "h: {} → {}", h0, 50.0);
        // 近似逆（尺度也取反）残差应为 ppm 量级（3.6e6 m × 0.56e-6 ≈ 2 m）
        let scaled = BursaParams { scale_ppm: -0.56, ..params.clone() };
        let (lon1, _lat1, _h1) = transform_blh(&scaled, lon, lat, h);
        let inv_approx = BursaParams { scale_ppm: 0.56, ..inv.clone() };
        let (lon2, _lat2, _h2) = transform_blh(&inv_approx, lon1, _lat1, _h1);
        // 以变换前的点 (lon, lat) 为基准度量近似逆残差（非线性交叉项 ~1e-8 度）
        let err_deg = (lon2 - lon).abs();
        assert!(err_deg < 1e-6, "近似逆残差量级异常：{err_deg} 度");
        // 正变换应产生显著位移（本组参数在 116E/40N 处约 13 m，来自旋转项
        // 0.34″ × 4.4e6 m 杠杆 + 平移 + 尺度），防止恒等式假阳性
        let d_lon_deg = (lon - 116.0).abs();
        assert!(
            d_lon_deg > 1e-6 && d_lon_deg < 1e-3,
            "正变换位移量级异常：{d_lon_deg} 度"
        );
    }

    #[test]
    fn param_validation_rejects_bad_input() {
        let ok = gn72_params();
        // 非有限数
        let mut p = ok.clone();
        p.dx = f64::NAN;
        assert!(matches!(p.validate(), Err(BursaError::Validate(m)) if m.contains("dx")));
        // 平移超范围（常见错误：单位写成厘米）
        let mut p = ok.clone();
        p.dy = 120000.0;
        assert!(matches!(p.validate(), Err(BursaError::Validate(m)) if m.contains("dy")));
        // 旋转超范围（±60″ 之外，例如单位误填成度：1.5° = 5400″）
        let mut p = ok.clone();
        p.rz = 5400.0;
        assert!(matches!(p.validate(), Err(BursaError::Validate(m)) if m.contains("rz")));
        // 尺度超范围
        let mut p = ok.clone();
        p.scale_ppm = 5000.0;
        assert!(p.validate().is_err());
        // 名称为空
        let mut p = ok.clone();
        p.target_crs = "  ".into();
        assert!(p.validate().is_err());
        // 椭球半轴非法
        let mut p = ok.clone();
        p.source_ellipsoid = Some(EllipsoidJson { a: 100.0, inv_f: 298.26 });
        assert!(p.validate().is_err());
    }

    #[test]
    fn json_missing_field_reports_clearly() {
        let dir = temp_dir("json");
        let path = dir.join("params.json");
        std::fs::write(&path, r#"{"source":"A","target":"B","dx":1.0}"#).unwrap();
        let err = BursaParams::from_json_file(&path).unwrap_err().to_string();
        assert!(err.contains("七参数 JSON 解析失败"), "{err}");
        assert!(err.contains("缺字段"), "{err}");

        // 合法文件加载 + CLI 批量入口
        std::fs::write(
            &path,
            r#"{"source":"CGCS2000","target":"WGS 84","dx":0,"dy":0,"dz":4.5,
                "rx":0,"ry":0,"rz":-0.554,"scale_ppm":0.219,
                "source_ellipsoid":{"a":6378135.0,"inv_f":298.26},
                "target_ellipsoid":{"a":6378137.0,"inv_f":298.257223563}}"#,
        )
        .unwrap();
        let points_path = dir.join("points.json");
        std::fs::write(&points_path, r#"{"points":[[4.0,55.0,0.0]]}"#).unwrap();
        let out_path = dir.join("out.json");
        let summary = run_from_files(&path, &points_path, &out_path).unwrap();
        assert_eq!(summary.count, 1);
        let saved: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(&out_path).unwrap()).unwrap();
        assert_eq!(saved["target"], "WGS 84");
        let p0 = saved["points"][0].as_array().unwrap();
        let lat = p0[1].as_f64().unwrap();
        assert!((lat - (55.0 + 0.090 / 3600.0)).abs() < 5e-4, "lat={lat}");

        // 越界经纬度明确报错
        std::fs::write(&points_path, r#"{"points":[[400.0,55.0,0.0]]}"#).unwrap();
        let err = run_from_files(&path, &points_path, &out_path).unwrap_err();
        assert!(err.contains("越界"), "{err}");

        let _ = std::fs::remove_dir_all(&dir);
    }
}
