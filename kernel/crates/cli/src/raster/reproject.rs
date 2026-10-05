//! EPSG:4326 / EPSG:3857 → WebMercatorQuad（XYZ）的重投影数值与瓦片矩阵计算。
//!
//! WebMercatorQuad：EPSG:3857 全世界范围，XYZ 方案（Y 原点左上），瓦片 256px。

/// Web Mercator 半轴（EPSG:3857 定义，球半径 R=6378137）。
pub const HALF_WORLD: f64 = 20037508.342789244;
/// 地球半径（球面墨卡托）。
pub const EARTH_RADIUS: f64 = 6378137.0;
/// 墨卡托可表达的最大纬度（±85.0511°，超出即投影边界）。
pub const MAX_LAT: f64 = 85.0511287798066;
/// XYZ 瓦片边长（px）。
pub const TILE_SIZE: u32 = 256;

/// 经纬度（度）→ Web Mercator（米）。
pub fn lonlat_to_merc(lon: f64, lat: f64) -> (f64, f64) {
    let x = lon.to_radians() * EARTH_RADIUS;
    let clamped = lat.clamp(-MAX_LAT, MAX_LAT);
    // y = R * ln(tan(π/4 + φ/2))，clamp 到 ±πR（±85.0511° 的投影边界）
    let y = (std::f64::consts::FRAC_PI_4 + clamped.to_radians() / 2.0)
        .tan()
        .ln()
        .clamp(-std::f64::consts::PI, std::f64::consts::PI)
        * EARTH_RADIUS;
    (x, y)
}

/// Web Mercator（米）→ 经纬度（度）。
pub fn merc_to_lonlat(x: f64, y: f64) -> (f64, f64) {
    let lon = (x / EARTH_RADIUS).to_degrees();
    let lat = (2.0 * (y / EARTH_RADIUS).exp().atan() - std::f64::consts::FRAC_PI_2).to_degrees();
    (lon, lat)
}

/// 某级 XYZ 瓦片的 Web Mercator bbox：`[minx, miny, maxx, maxy]`。
pub fn tile_bounds(z: u32, x: u32, y: u32) -> [f64; 4] {
    let n = 1u64 << z;
    let span = 2.0 * HALF_WORLD / n as f64;
    let minx = -HALF_WORLD + x as f64 * span;
    let maxy = HALF_WORLD - y as f64 * span;
    [minx, maxy - span, minx + span, maxy]
}

/// 源影像在 Web Mercator 下的 bbox（4326 源按四角重投影后取包围盒）。
pub fn bbox_to_merc(bbox: [f64; 4], crs: crate::raster::geotiff::Crs) -> [f64; 4] {
    use crate::raster::geotiff::Crs;
    match crs {
        Crs::Epsg3857 => bbox,
        Crs::Epsg4326 => {
            let corners = [
                (bbox[0], bbox[1]),
                (bbox[0], bbox[3]),
                (bbox[2], bbox[1]),
                (bbox[2], bbox[3]),
            ];
            let mut minx = f64::INFINITY;
            let mut miny = f64::INFINITY;
            let mut maxx = f64::NEG_INFINITY;
            let mut maxy = f64::NEG_INFINITY;
            for (lon, lat) in corners {
                let (x, y) = lonlat_to_merc(lon, lat);
                minx = minx.min(x);
                miny = miny.min(y);
                maxx = maxx.max(x);
                maxy = maxy.max(y);
            }
            [minx, miny, maxx, maxy]
        }
    }
}

/// 按源分辨率自动推导金字塔 zoom 范围：
/// - `min_zoom = 0`；
/// - `max_zoom` = 满足「瓦片分辨率 ≥ 源分辨率」的最大层级
///   （即不向上超采样），封顶 22、下限 0。
///
/// `src_res_merc`：源影像在 Web Mercator 下的米/像素分辨率。
pub fn zoom_range(src_res_merc: f64) -> (u32, u32) {
    const MAX_Z: u32 = 22;
    if !src_res_merc.is_finite() || src_res_merc <= 0.0 {
        return (0, MAX_Z);
    }
    let denom = TILE_SIZE as f64 * src_res_merc;
    let z = (2.0 * HALF_WORLD / denom).log2().floor();
    let max_zoom = if z.is_finite() {
        (z.max(0.0) as u32).min(MAX_Z)
    } else {
        MAX_Z
    };
    (0, max_zoom)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lonlat_merc_roundtrip() {
        for (lon, lat) in [(0.0, 0.0), (116.391, 39.907), (-122.4, 37.8)] {
            let (x, y) = lonlat_to_merc(lon, lat);
            let (lon2, lat2) = merc_to_lonlat(x, y);
            assert!((lon - lon2).abs() < 1e-9, "{lon} != {lon2}");
            assert!((lat - lat2).abs() < 1e-9, "{lat} != {lat2}");
        }
        // 北京 merc 坐标数值抽查（与球面墨卡托公式独立计算值比对）
        let (x, y) = lonlat_to_merc(116.391, 39.907);
        assert!((x - 12956586.85).abs() < 1.0, "x={x}");
        assert!((y - 4852436.96).abs() < 1.0, "y={y}");
    }

    #[test]
    fn tile_bounds_at_origin() {
        let b = tile_bounds(0, 0, 0);
        assert!((b[0] - -HALF_WORLD).abs() < 1e-6);
        assert!((b[3] - HALF_WORLD).abs() < 1e-6);
        // XYZ Y 原点在左上：z1 的 y=0 是北半球（y 范围 0..+HALF），y=1 是南半球
        let north = tile_bounds(1, 0, 0);
        assert!((north[3] - HALF_WORLD).abs() < 1e-6);
        assert!((north[1]).abs() < 1e-6);
        let south = tile_bounds(1, 0, 1);
        assert!((south[3]).abs() < 1e-6);
        assert!((south[1] + HALF_WORLD).abs() < 1e-6);
    }

    #[test]
    fn zoom_range_matches_source_resolution() {
        // 4096px 宽的全球影像：z_max = log2(4096/256) = 4
        let (min_z, max_z) = zoom_range(2.0 * HALF_WORLD / 4096.0);
        assert_eq!((min_z, max_z), (0, 4));
        // 分辨率极粗时 z_max = 0；极细时封顶 22
        assert_eq!(zoom_range(1e6).1, 0);
        assert_eq!(zoom_range(1e-6).1, 22);
    }
}
