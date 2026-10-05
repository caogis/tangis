//! 坐标系识别（合规 P0 算子一）。
//!
//! 两条路径，识别失败一律明确报错（禁止猜测/默认值）：
//! 1. **GeoTIFF GeoKey**：解析 GeoKeyDirectoryTag(34735)，优先
//!    ProjectedCSType GeoKey 3072，其次 GeographicType GeoKey 2048；
//!    支持 TIFFTagLocation=0 的直接值与 34737（GeoAsciiParams）引用。
//! 2. **ESRI WKT / .prj**：关键词匹配（GCS_WGS_1984、CGCS2000、
//!    WGS 1984 Web Mercator、CGCS2000 高斯-克吕格分带命名等）。
//!
//! EPSG 支持面：常见地理坐标系（4326/4490/4479/4610/4214/4269）、
//! Web Mercator 3857、UTM 326xx/327xx、CGCS2000 高斯-克吕格系列
//! 4491–4554；不在列表中的 EPSG 明确报错。

use std::path::Path;

use serde::Serialize;

use crate::tiff::{self, TiffError};

/// 结构化坐标系信息。
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct CrsInfo {
    /// EPSG 代码（WKT 路径如无法细化到 EPSG 则为 None 并在 error 场景报错）。
    pub epsg: Option<u16>,
    /// 坐标系名称。
    pub name: String,
    /// 是否地理坐标系（true=经纬度，false=投影坐标）。
    pub is_geographic: bool,
    /// 坐标单位（degree / metre）。
    pub unit: String,
    /// 识别来源（如 "GeoTIFF GeoKey 3072" / "WKT"）。
    pub source: String,
}

/// GeoTIFF 附加地理参考信息（供 CLI 展示与 DEM 坐标映射）。
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct GeoRefMeta {
    /// ModelPixelScaleTag(33550)。
    pub pixel_scale: [f64; 3],
    /// ModelTiepointTag(33922) 首组 (i, j, x, y)。
    pub tiepoint_origin: [f64; 4],
}

/// EPSG 代码 → (名称, 是否地理坐标系, 单位)。
///
/// 覆盖面（任务 F-21 要求"常见 4326/4490/3857/4479 等"）：
/// - 地理：4326 WGS 84、4490 CGCS2000、4479 CGCS2000(3D)、4610 Xian 1980、
///   4214 Beijing 1954、4269 NAD83；
/// - 投影：3857 Web Mercator、32601–32660 / 32701–32760 UTM（WGS 84）、
///   CGCS2000 高斯-克吕格 4491–4554：
///   * 4491–4501：6° 带带号（zone 13–23）
///   * 4502–4512：6° 带中央经线（CM 75E–135E）
///   * 4513–4533：3° 带带号（zone 25–45）
///   * 4534–4554：3° 带中央经线（CM 75E–135E）
///
///   （依据：EPSG Geodetic Parameter Dataset；CGCS2000 系列编号连续有序）
pub fn lookup_epsg(code: u16) -> Option<(String, bool, &'static str)> {
    let (name, is_geo, unit): (String, bool, &'static str) = match code {
        4326 => ("WGS 84".into(), true, "degree"),
        4479 => ("CGCS2000 (geographic 3D)".into(), true, "degree"),
        4490 => ("CGCS2000".into(), true, "degree"),
        4610 => ("Xian 1980".into(), true, "degree"),
        4214 => ("Beijing 1954".into(), true, "degree"),
        4269 => ("NAD83".into(), true, "degree"),
        3857 => ("WGS 84 / Pseudo-Mercator".into(), false, "metre"),
        32601..=32660 => (format!("WGS 84 / UTM zone {}N", code - 32600), false, "metre"),
        32701..=32760 => (format!("WGS 84 / UTM zone {}S", code - 32700), false, "metre"),
        4491..=4501 => (
            format!("CGCS2000 / Gauss-Kruger zone {}", code - 4491 + 13),
            false,
            "metre",
        ),
        4502..=4512 => (
            format!("CGCS2000 / Gauss-Kruger CM {}E", 75 + (code - 4502) * 6),
            false,
            "metre",
        ),
        4513..=4533 => (
            format!(
                "CGCS2000 / 3-degree Gauss-Kruger zone {}",
                code - 4513 + 25
            ),
            false,
            "metre",
        ),
        4534..=4554 => (
            format!(
                "CGCS2000 / 3-degree Gauss-Kruger CM {}E",
                75 + (code - 4534) * 3
            ),
            false,
            "metre",
        ),
        _ => return None,
    };
    Some((name, is_geo, unit))
}

/// 从 GeoKeyDirectory 提取指定 key 的整数值。
///
/// GeoKey 条目 = (KeyID, TIFFTagLocation, Count, Value_Offset)：
/// - TIFFTagLocation=0 → 值即 Value_Offset；
/// - TIFFTagLocation=34737 → 值为 GeoAsciiParams 中 [offset, offset+count) 的
///   ASCII 片段（去掉分隔符 '|'）。
fn geo_key_value(
    geo_keys: &[u16],
    ascii_params: Option<&str>,
    key_id: u16,
) -> Result<Option<String>, String> {
    if geo_keys.len() < 4 {
        return Err(format!(
            "GeoKeyDirectory 头不完整（{} 个 u16，至少 4 个）",
            geo_keys.len()
        ));
    }
    let num_keys = geo_keys[3] as usize;
    if geo_keys.len() < 4 + num_keys * 4 {
        return Err(format!(
            "GeoKeyDirectory 声明 {num_keys} 个 key，实际仅容纳 {} 个",
            (geo_keys.len() - 4) / 4
        ));
    }
    for k in 0..num_keys {
        let base = 4 + k * 4;
        let (kid, loc, _count, value) = (
            geo_keys[base],
            geo_keys[base + 1],
            geo_keys[base + 2],
            geo_keys[base + 3],
        );
        if kid != key_id {
            continue;
        }
        return match loc {
            0 => Ok(Some(value.to_string())),
            34737 => {
                let params = ascii_params.ok_or_else(|| {
                    format!("GeoKey {key_id} 引用 GeoAsciiParams(34737)，但文件缺少该 tag")
                })?;
                let start = value as usize;
                let end = start + _count as usize;
                let raw = params.get(start..end).ok_or_else(|| {
                    format!(
                        "GeoKey {key_id} 的 ASCII 片段 [{start},{end}) 超出 GeoAsciiParams 长度 {}",
                        params.len()
                    )
                })?;
                Ok(Some(
                    raw.trim_matches('|').trim().to_string(),
                ))
            }
            other => Err(format!(
                "GeoKey {key_id} 使用不支持的 TIFFTagLocation={other}（支持 0 与 34737）"
            )),
        };
    }
    Ok(None)
}

/// 解析 GeoTIFF 字节并识别 CRS。
pub fn identify_geotiff(bytes: &[u8]) -> Result<CrsInfo, String> {
    let tags = tiff::read_geo_tags(bytes).map_err(tiff_err_msg)?;
    // 优先投影坐标系（更具体），其次地理坐标系
    let projected = geo_key_value(&tags.geo_keys, tags.geo_ascii_params.as_deref(), 3072)?;
    let geographic = geo_key_value(&tags.geo_keys, tags.geo_ascii_params.as_deref(), 2048)?;
    let (raw, source) = match (projected, geographic) {
        (Some(p), _) => (p, "GeoTIFF GeoKey 3072 (ProjectedCSType)"),
        (None, Some(g)) => (g, "GeoTIFF GeoKey 2048 (GeographicType)"),
        (None, None) => {
            return Err(
                "GeoKeyDirectory 中既无 ProjectedCSType(3072) 也无 GeographicType(2048)，\
                 无法识别 CRS"
                    .into(),
            )
        }
    };
    let code: u16 = raw.trim().parse().map_err(|_| {
        format!("GeoKey 值 `{raw}` 不是合法的 EPSG 代码（应为 0–65535 整数）")
    })?;
    finish(code, source)
}

/// 解析 GeoTIFF 的像素比例与连接点（DEM 区域映射用）。
pub fn read_georef_meta(bytes: &[u8]) -> Result<GeoRefMeta, String> {
    let tags = tiff::read_geo_tags(bytes).map_err(tiff_err_msg)?;
    let tp: [f64; 4] = tags.tiepoint[0..4]
        .try_into()
        .expect("tiepoint 长度已校验为 6*n");
    Ok(GeoRefMeta {
        pixel_scale: tags.pixel_scale,
        tiepoint_origin: tp,
    })
}

fn tiff_err_msg(e: TiffError) -> String {
    format!("GeoTIFF 解析失败：{e}")
}

fn finish(code: u16, source: &str) -> Result<CrsInfo, String> {
    match lookup_epsg(code) {
        Some((name, is_geo, unit)) => Ok(CrsInfo {
            epsg: Some(code),
            name,
            is_geographic: is_geo,
            unit: unit.to_string(),
            source: source.to_string(),
        }),
        None => Err(format!(
            "EPSG:{code} 不在支持列表内（支持：4326/4479/4490/4610/4214/4269、3857、\
             UTM 326xx/327xx、CGCS2000 高斯-克吕格 4491–4554）；\
             合规要求拒绝猜测未知坐标系"
        )),
    }
}

/// 识别 ESRI WKT / OGC WKT 文本。
///
/// 支持的关键词（大小写不敏感，具体优先于宽泛）：
/// - Web Mercator：`Web_Mercator`（PROJCS，EPSG:3857）
/// - CGCS2000 高斯-克吕格 ESRI 命名：`CGCS2000_3_Degree_GK_CM_117E`、
///   `CGCS2000_3_Degree_GK_Zone_39`、`CGCS2000_GK_CM_117E`（6° 带）、
///   `CGCS2000_GK_Zone_13`（6° 带带号）
/// - 地理：`CGCS2000` / `China_Geodetic_Coordinate_System_2000`（4490）、
///   `GCS_WGS_1984` / `WGS 84`（4326）、`Xian_1980`（4610）、
///   `Beijing_1954`（4214）、`NAD_1983`（4269）
pub fn identify_wkt(text: &str) -> Result<CrsInfo, String> {
    let lower = text.to_ascii_lowercase();
    if !lower.contains("geogcs") && !lower.contains("projcs") {
        return Err(
            "文本中未找到 WKT 关键字 GEOGCS/PROJCS，不是合法的坐标系 WKT 定义".into(),
        );
    }
    let is_projcs = lower.contains("projcs");
    let source = "WKT";

    // —— 投影坐标系：具体模式优先 ——
    if is_projcs {
        if lower.contains("web_mercator") || lower.contains("web mercator") {
            return finish(3857, source);
        }
        // ESRI 命名风格：CGCS2000_3_Degree_GK_CM_<lon>E / CGCS2000_3_Degree_GK_Zone_<z>
        if let Some(code) = parse_cgcs2000_gk_name(&lower) {
            return finish(code, source);
        }
        return Err(
            "PROJCS（投影坐标系）WKT 仅支持 Web Mercator 与 CGCS2000 高斯-克吕格 \
             ESRI 命名风格的自动识别；其余投影请改用 GeoTIFF GeoKey 或 EPSG 代码"
                .into(),
        );
    }

    // —— 地理坐标系 ——
    let table: &[(&str, u16)] = &[
        ("china_geodetic_coordinate_system_2000", 4490),
        ("cgcs2000", 4490),
        ("gcs_wgs_1984", 4326),
        ("wgs_1984", 4326),
        ("wgs 84", 4326),
        ("xian_1980", 4610),
        ("xian 1980", 4610),
        ("beijing_1954", 4214),
        ("beijing 1954", 4214),
        ("nad_1983", 4269),
        ("north_american_1983", 4269),
    ];
    for (kw, code) in table {
        if lower.contains(kw) {
            return finish(*code, source);
        }
    }
    Err(format!(
        "GEOGCS WKT 无法匹配已知坐标系（支持：CGCS2000/WGS 84/Xian 1980/Beijing 1954/\
         NAD83）；原文前 120 字符：`{}`",
        text.chars().take(120).collect::<String>()
    ))
}

/// 解析 ESRI 风格 CGCS2000 高斯-克吕格投影名 → EPSG 代码。
fn parse_cgcs2000_gk_name(lower: &str) -> Option<u16> {
    let three_degree = lower.contains("3_degree") || lower.contains("3 degree");
    let zone = if let Some(pos) = lower.find("_gk_zone_") {
        let tail = &lower[pos + "_gk_zone_".len()..];
        let num: String = tail
            .chars()
            .take_while(|c| c.is_ascii_digit())
            .collect();
        num.parse::<u16>().ok()?
    } else if let Some(pos) = lower.find("_gk_cm_") {
        let tail = &lower[pos + "_gk_cm_".len()..];
        let num: String = tail
            .chars()
            .take_while(|c| c.is_ascii_digit())
            .collect();
        let cm = num.parse::<u16>().ok()?;
        if three_degree {
            if !(75..=135).contains(&cm) || (cm - 75) % 3 != 0 {
                return None;
            }
            return Some(4534 + (cm - 75) / 3);
        }
        if !(75..=135).contains(&cm) || (cm - 75) % 6 != 0 {
            return None;
        }
        return Some(4502 + (cm - 75) / 6);
    } else {
        return None;
    };
    // GK_Zone：zone 号 → EPSG
    if three_degree {
        // 3° 带 zone 25–45（CM = zone*3）
        if (25..=45).contains(&zone) {
            return Some(4513 + (zone - 25));
        }
        return None;
    }
    // 6° 带 zone 13–23（CM = zone*6）
    if (13..=23).contains(&zone) {
        return Some(4491 + (zone - 13));
    }
    None
}

/// 按扩展名识别文件：`.tif/.tiff` → GeoTIFF，`.prj` → WKT。
pub fn identify_file(path: &Path) -> Result<CrsInfo, String> {
    let ext = path
        .extension()
        .and_then(|e| e.to_str())
        .map(|e| e.to_ascii_lowercase())
        .unwrap_or_default();
    match ext.as_str() {
        "tif" | "tiff" => {
            let bytes = std::fs::read(path)
                .map_err(|e| format!("无法读取 {}: {e}", path.display()))?;
            identify_geotiff(&bytes)
        }
        "prj" => {
            let text = std::fs::read_to_string(path)
                .map_err(|e| format!("无法读取 {}: {e}", path.display()))?;
            identify_wkt(&text)
        }
        other => Err(format!(
            "扩展名 `.{other}` 不支持（crs-identify 仅支持 .tif/.tiff/.prj）"
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tiff::{GeoTags, Raster, Samples};

    /// 写一个带 GeoKey 的最小 GeoTIFF。
    fn geotiff_with_keys(keys: Vec<u16>) -> Vec<u8> {
        let raster = Raster {
            width: 4,
            height: 4,
            samples: Samples::U16(vec![0; 16]),
        };
        let geo = GeoTags {
            pixel_scale: [0.001, 0.001, 0.0],
            tiepoint: vec![0.0, 0.0, 0.0, 116.0, 40.0, 0.0],
            geo_keys: keys,
            geo_ascii_params: None,
        };
        tiff::write_geo_tiff(&raster, &geo).unwrap()
    }

    /// 标准 GeoKey 头 + 若干 key 条目。
    fn keys(entries: &[(u16, u16, u16, u16)]) -> Vec<u16> {
        let mut v = vec![1u16, 1, 0, entries.len() as u16];
        for &(a, b, c, d) in entries {
            v.extend_from_slice(&[a, b, c, d]);
        }
        v
    }

    #[test]
    fn identifies_cgcs2000_geographic_from_geotiff() {
        let bytes = geotiff_with_keys(keys(&[(2048, 0, 1, 4490)]));
        let info = identify_geotiff(&bytes).unwrap();
        assert_eq!(info.epsg, Some(4490));
        assert_eq!(info.name, "CGCS2000");
        assert!(info.is_geographic);
        assert_eq!(info.unit, "degree");
    }

    #[test]
    fn prefers_projected_key_over_geographic() {
        let bytes = geotiff_with_keys(keys(&[(2048, 0, 1, 4490), (3072, 0, 1, 4548)]));
        let info = identify_geotiff(&bytes).unwrap();
        assert_eq!(info.epsg, Some(4548));
        assert!(!info.is_geographic);
        assert_eq!(info.name, "CGCS2000 / 3-degree Gauss-Kruger CM 117E");
    }

    #[test]
    fn identifies_web_mercator_and_wgs84() {
        let info = identify_geotiff(&geotiff_with_keys(keys(&[(3072, 0, 1, 3857)]))).unwrap();
        assert_eq!(info.epsg, Some(3857));
        assert!(!info.is_geographic);
        assert_eq!(info.unit, "metre");

        let info = identify_geotiff(&geotiff_with_keys(keys(&[(2048, 0, 1, 4326)]))).unwrap();
        assert_eq!(info.epsg, Some(4326));
        assert_eq!(info.name, "WGS 84");
    }

    #[test]
    fn missing_geokey_directory_fails_loudly() {
        let raster = Raster {
            width: 2,
            height: 2,
            samples: Samples::U16(vec![0; 4]),
        };
        let geo = GeoTags {
            pixel_scale: [1.0, 1.0, 0.0],
            tiepoint: vec![0.0, 0.0, 0.0, 0.0, 0.0, 0.0],
            geo_keys: vec![1, 1, 0, 0], // 声明 0 个 key
            geo_ascii_params: None,
        };
        let bytes = tiff::write_geo_tiff(&raster, &geo).unwrap();
        let err = identify_geotiff(&bytes).unwrap_err();
        assert!(err.contains("3072") && err.contains("2048"), "{err}");
    }

    #[test]
    fn unknown_epsg_fails_with_reason() {
        let bytes = geotiff_with_keys(keys(&[(2048, 0, 1, 9999)]));
        let err = identify_geotiff(&bytes).unwrap_err();
        assert!(err.contains("EPSG:9999"), "{err}");
        assert!(err.contains("不在支持列表"), "{err}");
    }

    #[test]
    fn wkt_esri_wgs1984_and_cgcs2000() {
        let wgs84 = r#"GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984",SPHEROID["WGS_1984",6378137.0,298.257223563]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]]"#;
        let info = identify_wkt(wgs84).unwrap();
        assert_eq!(info.epsg, Some(4326));
        assert!(info.is_geographic);

        let cgcs = r#"GEOGCS["CGCS2000",DATUM["D_China_2000",SPHEROID["CGCS2000",6378137.0,298.257222101]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]]"#;
        let info = identify_wkt(cgcs).unwrap();
        assert_eq!(info.epsg, Some(4490));
        assert_eq!(info.name, "CGCS2000");
    }

    #[test]
    fn wkt_web_mercator_projcs() {
        let wkt = r#"PROJCS["WGS_1984_Web_Mercator_Auxiliary_Sphere",GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984",SPHEROID["WGS_1984",6378137.0,298.257223563]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]],PROJECTION["Mercator_Auxiliary_Sphere"],PARAMETER["False_Easting",0.0],PARAMETER["False_Northing",0.0],UNIT["Meter",1.0]]"#;
        let info = identify_wkt(wkt).unwrap();
        assert_eq!(info.epsg, Some(3857));
        assert!(!info.is_geographic);
        assert_eq!(info.unit, "metre");
    }

    #[test]
    fn wkt_cgcs2000_gk_cm_and_zone_names() {
        let cm117 = r#"PROJCS["CGCS2000_3_Degree_GK_CM_117E",GEOGCS["CGCS2000",DATUM["D_China_2000",SPHEROID["CGCS2000",6378137.0,298.257222101]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]],PROJECTION["Transverse_Mercator"],PARAMETER["Central_Meridian",117.0],UNIT["Meter",1.0]]"#;
        assert_eq!(identify_wkt(cm117).unwrap().epsg, Some(4548));

        let zone39 = r#"PROJCS["CGCS2000_3_Degree_GK_Zone_39",GEOGCS["CGCS2000",DATUM["D_China_2000",SPHEROID["CGCS2000",6378137.0,298.257222101]]],PROJECTION["Transverse_Mercator"],UNIT["Meter",1.0]]"#;
        assert_eq!(identify_wkt(zone39).unwrap().epsg, Some(4527));

        let gk6_cm117 = r#"PROJCS["CGCS2000_GK_CM_117E",GEOGCS["CGCS2000",DATUM["D_China_2000",SPHEROID["CGCS2000",6378137.0,298.257222101]]],PROJECTION["Transverse_Mercator"],UNIT["Meter",1.0]]"#;
        assert_eq!(identify_wkt(gk6_cm117).unwrap().epsg, Some(4509));
    }

    #[test]
    fn wkt_unknown_and_non_wkt_fail_with_reason() {
        let err = identify_wkt("hello world").unwrap_err();
        assert!(err.contains("GEOGCS/PROJCS"), "{err}");

        let err = identify_wkt(r#"GEOGCS["Mars_2000",DATUM["D_Mars",SPHEROID["Mars",3396190.0,169.9]]]"#).unwrap_err();
        assert!(err.contains("无法匹配已知坐标系"), "{err}");

        // PROJCS 但不是支持的面
        let err = identify_wkt(r#"PROJCS["Some_Albertas",GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984"]],PROJECTION["Albers"],UNIT["Meter",1.0]]"#).unwrap_err();
        assert!(err.contains("仅支持"), "{err}");
    }

    #[test]
    fn identify_file_dispatches_by_extension() {
        let dir = std::env::temp_dir().join(format!("tangis-geo-crs-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();

        let prj = dir.join("sample.prj");
        std::fs::write(&prj, r#"GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984"]]"#).unwrap();
        assert_eq!(identify_file(&prj).unwrap().epsg, Some(4326));

        let tif = dir.join("sample.tif");
        std::fs::write(&tif, geotiff_with_keys(keys(&[(3072, 0, 1, 4491)]))).unwrap();
        let info = identify_file(&tif).unwrap();
        assert_eq!(info.epsg, Some(4491));
        assert_eq!(info.name, "CGCS2000 / Gauss-Kruger zone 13");

        let bad = dir.join("sample.xyz");
        std::fs::write(&bad, b"").unwrap();
        let err = identify_file(&bad).unwrap_err();
        assert!(err.contains("不支持"), "{err}");

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn epsg_table_common_codes() {
        assert_eq!(lookup_epsg(4479).unwrap().0, "CGCS2000 (geographic 3D)");
        assert!(lookup_epsg(4490).unwrap().1);
        assert!(!lookup_epsg(3857).unwrap().1);
        assert_eq!(lookup_epsg(4534).unwrap().0, "CGCS2000 / 3-degree Gauss-Kruger CM 75E");
        assert_eq!(lookup_epsg(4554).unwrap().0, "CGCS2000 / 3-degree Gauss-Kruger CM 135E");
        assert_eq!(lookup_epsg(4501).unwrap().0, "CGCS2000 / Gauss-Kruger zone 23");
        assert_eq!(lookup_epsg(32650).unwrap().0, "WGS 84 / UTM zone 50N");
        assert!(lookup_epsg(4325).is_none());
    }
}
