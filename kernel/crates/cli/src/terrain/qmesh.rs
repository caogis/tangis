//! Quantized-Mesh 1.0（Cesium 地形瓦片）最小编码器。
//!
//! 布局遵循 Cesium 规范：
//! - Header（80 字节 = 10×f64）：tile center（ECEF, f64×3）+ bounding sphere
//!   （center f64×3 + radius f64）+ horizon occlusion point（f64×3，后二者相对 tile center）
//!   （CesiumJS 解码器按 10 个 double 读，网络上流传的"74 字节"说法有误）；
//! - 顶点数据：vertexCount(u32) + 每顶点 (u, v, height) 各 u16 zigzag 增量编码；
//! - 三角形索引：triangleCount(u32) + u16（vertexCount < 65536）high-water-mark 编码；
//! - 边界索引：west/south/east/north 各 (u32 count + u16 HWM 编码)，
//!   顺序 west 南→北、south 东→西、east 北→南、north 西→东。
//!
//! 落盘统一 gzip（CTB 约定，server 分发时带 Content-Encoding: gzip）。

/// WGS84 长半轴（米）。
pub const EARTH_A: f64 = 6378137.0;
/// WGS84 第一偏心率平方。
pub const EARTH_E2: f64 = 6.6943799901413265e-3;
/// WGS84 短半轴（米）。
pub const EARTH_B: f64 = 6356752.314245179;

/// u16 zigzag 编码（i16 值域）。
pub fn zigzag_encode_u16(v: i32) -> u16 {
    ((v << 1) ^ (v >> 15)) as u16
}

/// u16 zigzag 解码（测试/校验用）。
#[cfg(test)]
pub fn zigzag_decode_u16(v: u16) -> i32 {
    ((v >> 1) as i32) ^ (-((v & 1) as i32))
}

/// high-water-mark 编码一段索引（u16 回绕算术）。
///
/// 解码语义（Cesium）：`idx = highest - code`（u16 模 65536），`code == 0` 时水位 +1。
/// 因此任意索引序列（索引 < 65536）均可无损编码：code = (highest - idx) 回绕。
pub fn hwm_encode(indices: &[u32]) -> Vec<u16> {
    let mut out = Vec::with_capacity(indices.len());
    let mut highest: u16 = 0;
    for &idx in indices {
        let idx = idx as u16;
        out.push(highest.wrapping_sub(idx));
        if idx == highest {
            highest = highest.wrapping_add(1);
        }
    }
    out
}

/// high-water-mark 解码（测试/校验用）。
#[cfg(test)]
pub fn hwm_decode(codes: &[u16]) -> Vec<u32> {
    let mut out = Vec::with_capacity(codes.len());
    let mut highest: u16 = 0;
    for &code in codes {
        out.push(highest.wrapping_sub(code) as u32);
        if code == 0 {
            highest = highest.wrapping_add(1);
        }
    }
    out
}

/// 大地坐标（度/米）→ ECEF（WGS84）。
pub fn geodetic_to_ecef(lon_deg: f64, lat_deg: f64, h: f64) -> [f64; 3] {
    let lon = lon_deg.to_radians();
    let lat = lat_deg.to_radians();
    let sin_lat = lat.sin();
    let n = EARTH_A / (1.0 - EARTH_E2 * sin_lat * sin_lat).sqrt();
    [
        (n + h) * lat.cos() * lon.cos(),
        (n + h) * lat.cos() * lon.sin(),
        (n * (1.0 - EARTH_E2) + h) * sin_lat,
    ]
}

/// ECEF → scaled space（除以椭球半径，测试/地平遮蔽点用）。
fn to_scaled_space(p: [f64; 3]) -> [f64; 3] {
    [p[0] / EARTH_A, p[1] / EARTH_A, p[2] / EARTH_B]
}

/// scaled space → ECEF。
fn from_scaled_space(p: [f64; 3]) -> [f64; 3] {
    [p[0] * EARTH_A, p[1] * EARTH_A, p[2] * EARTH_B]
}

/// 地平遮蔽点（Cesium EllipsoidalOccluder 算法）：
/// scaled space 下逐顶点除以 max(1, 模长) 后逐分量取最小，再变换回 ECEF。
/// 入参为 tile 顶点的绝对 ECEF 坐标。
pub fn horizon_occlusion_point(ecef_vertices: &[[f64; 3]]) -> [f64; 3] {
    let mut min = [f64::INFINITY; 3];
    for &v in ecef_vertices {
        let s = to_scaled_space(v);
        let magnitude = (s[0] * s[0] + s[1] * s[1] + s[2] * s[2]).sqrt().max(1.0);
        for i in 0..3 {
            min[i] = min[i].min(s[i] / magnitude);
        }
    }
    from_scaled_space(min)
}

/// 单瓦片量化网格的几何输入（全部为tile 局部/地理值）。
pub struct TileMesh<'a> {
    /// 瓦片经纬度范围（度）：west, south, east, north。
    pub rect: [f64; 4],
    /// 瓦片高程范围（米）。
    pub height_range: (f64, f64),
    /// 顶点序列（度/米），长度 = 顶点数。
    pub cartographics: &'a [(f64, f64, f64)],
    /// 三角形索引（u32，每 3 个一组，CCW）。
    pub indices: &'a [u32],
    /// 四边边界顶点索引：west, south, east, north。
    pub edges: [&'a [u32]; 4],
}

/// 编码单瓦片 Quantized-Mesh（未压缩字节）。`center` 为瓦片 ECEF 中心。
pub fn encode_tile(mesh: &TileMesh, center: [f64; 3]) -> Result<Vec<u8>, String> {
    let n = mesh.cartographics.len();
    if n == 0 || n > u16::MAX as usize {
        return Err(format!("顶点数 {n} 越界（1..=65535）"));
    }
    if mesh.indices.is_empty() {
        return Err("三角形索引为空".into());
    }

    let (hmin, hmax) = mesh.height_range;
    let hspan = if hmax > hmin { hmax - hmin } else { 1.0 };
    let (west, south, east, north) = (mesh.rect[0], mesh.rect[1], mesh.rect[2], mesh.rect[3]);
    let (lspan, aspan) = (east - west, north - south);
    if lspan <= 0.0 || aspan <= 0.0 {
        return Err(format!("瓦片经纬度范围非法：{:?}", mesh.rect));
    }

    // ---- 顶点绝对 ECEF（BS/地平点用）与量化 ----
    let mut ecef_abs = Vec::with_capacity(n);
    let mut offsets = Vec::with_capacity(n * 3);
    let mut q = Vec::with_capacity(n * 3); // (u, v, h) 量化值
    for &(lon, lat, h) in mesh.cartographics {
        let e = geodetic_to_ecef(lon, lat, h);
        ecef_abs.push(e);
        for i in 0..3 {
            offsets.push(e[i] - center[i]);
        }
        let u = ((lon - west) / lspan * 32767.0).round().clamp(0.0, 32767.0) as i32;
        let v = ((lat - south) / aspan * 32767.0).round().clamp(0.0, 32767.0) as i32;
        let hq = ((h - hmin) / hspan * 32767.0).round().clamp(0.0, 32767.0) as i32;
        q.push([u, v, hq]);
    }

    // ---- BS 半径与地平遮蔽点（tile 局部坐标）----
    let mut radius = 0.0f64;
    for c in offsets.chunks(3) {
        radius = radius.max((c[0] * c[0] + c[1] * c[1] + c[2] * c[2]).sqrt());
    }
    let horizon_abs = horizon_occlusion_point(&ecef_abs);
    let horizon = [
        horizon_abs[0] - center[0],
        horizon_abs[1] - center[1],
        horizon_abs[2] - center[2],
    ];

    // ---- 序列化 ----
    let mut out = Vec::new();
    // header
    for v in center {
        out.extend_from_slice(&v.to_le_bytes());
    }
    for v in [0.0f64; 3] {
        out.extend_from_slice(&v.to_le_bytes()); // BS center = tile center（局部原点）
    }
    out.extend_from_slice(&radius.to_le_bytes());
    for v in horizon {
        out.extend_from_slice(&v.to_le_bytes());
    }
    debug_assert_eq!(out.len(), 80);

    // vertices：zigzag 增量（首项增量自 0）
    out.extend_from_slice(&(n as u32).to_le_bytes());
    let mut prev = [0i32; 3];
    for qv in &q {
        for i in 0..3 {
            out.extend_from_slice(&zigzag_encode_u16(qv[i] - prev[i]).to_le_bytes());
            prev[i] = qv[i];
        }
    }

    // indices：HWM
    out.extend_from_slice(&((mesh.indices.len() / 3) as u32).to_le_bytes());
    for code in hwm_encode(mesh.indices) {
        out.extend_from_slice(&code.to_le_bytes());
    }

    // edges：west, south, east, north
    for edge in &mesh.edges {
        out.extend_from_slice(&(edge.len() as u32).to_le_bytes());
        for code in hwm_encode(edge) {
            out.extend_from_slice(&code.to_le_bytes());
        }
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn zigzag_roundtrip() {
        for v in [0i32, 1, -1, 32767, -32768, 255, -255] {
            assert_eq!(zigzag_decode_u16(zigzag_encode_u16(v)), v, "{v}");
        }
    }

    #[test]
    fn hwm_roundtrip_and_watermark_semantics() {
        // 编码 [0,1,2,1,3]：水位随首个新最大值推进
        let codes = hwm_encode(&[0, 1, 2, 1, 3]);
        assert_eq!(codes, vec![0, 0, 0, 2, 0]);
        assert_eq!(hwm_decode(&codes), vec![0, 1, 2, 1, 3]);
        // 逆序尾部：不产生新水位
        let codes = hwm_encode(&[0, 1, 2, 1, 0]);
        assert_eq!(codes, vec![0, 0, 0, 2, 3]);
        assert_eq!(hwm_decode(&codes), vec![0, 1, 2, 1, 0]);
        // 回绕：水位 0 时直接编码任意索引（如边界列表从非 0 索引开始）
        let codes = hwm_encode(&[5, 3, 1]);
        assert_eq!(codes, vec![65531, 65533, 65535]);
        assert_eq!(hwm_decode(&codes), vec![5, 3, 1]);
    }

    #[test]
    fn geodetic_ecef_known_values() {
        // 赤道本初子午线海平面：x = a
        let e = geodetic_to_ecef(0.0, 0.0, 0.0);
        assert!((e[0] - EARTH_A).abs() < 1e-6, "{e:?}");
        assert!(e[1].abs() < 1e-9 && e[2].abs() < 1e-9);
        // 北极海平面：z = b
        let e = geodetic_to_ecef(0.0, 90.0, 0.0);
        assert!((e[2] - EARTH_B).abs() < 1e-3, "{e:?}");
    }

    #[test]
    fn horizon_point_degenerate_inside() {
        // 单顶点：scaled space 除以 max(1,|s|) 后取 min，可稳定求值不 panic
        let p = horizon_occlusion_point(&[geodetic_to_ecef(116.0, 40.0, 100.0)]);
        assert!(p.iter().all(|v| v.is_finite()), "{p:?}");
    }
}
