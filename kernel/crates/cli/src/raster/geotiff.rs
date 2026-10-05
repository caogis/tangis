//! 最小 GeoTIFF 读取实现（纯 Rust，不引入 GDAL C 依赖）。
//!
//! 支持范围（明确最小化，超出范围一律报错，禁止静默降级）：
//! - 字节序：小端 `II` / 大端 `MM`；
//! - 压缩：无压缩（1）/ Deflate（8、32946），predictor 仅支持 1；
//! - 位深：8 bit/样本；波段：灰度(1)、RGB(3)、RGBA(4)，统一解码为 RGBA8；
//! - 组织：条带（Strip）与分块（Tile）两种，Chunky（PlanarConfiguration=1）；
//! - 地理参考：ModelPixelScaleTag(33550) + ModelTiepointTag(33922) 推 bbox，
//!   GeoKeyDirectoryTag(34735) 识别 CRS —— 仅支持 EPSG:4326 与 EPSG:3857。
//!
//! 已知限制（见交付报告）：多 IFD 只取含地理 tag 的第一个；不支持 LZW/JPEG/PackBits、
//! 16 bit、Palette、Predictor 2/3、PlanarConfiguration=2、旋转仿射（仅北朝上）。

use std::io::Read;

use flate2::read::ZlibDecoder;

/// 支持的源 CRS。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Crs {
    /// WGS84 经纬度。
    Epsg4326,
    /// Web Mercator。
    Epsg3857,
}

/// 解码后的 GeoTIFF：像素统一为 RGBA8 + 地理参考。
#[derive(Debug, Clone)]
pub struct GeoTiff {
    pub width: usize,
    pub height: usize,
    /// RGBA8 交错像素，长度 = width * height * 4。
    pub rgba: Vec<u8>,
    pub crs: Crs,
    /// 源 CRS 下的地理 bbox：`[minx, miny, maxx, maxy]`（北朝上假定）。
    pub bbox: [f64; 4],
}

// ---- TIFF 常量 ----
const TAG_IMAGE_WIDTH: u16 = 256;
const TAG_IMAGE_LENGTH: u16 = 257;
const TAG_BITS_PER_SAMPLE: u16 = 258;
const TAG_COMPRESSION: u16 = 259;
const TAG_PHOTOMETRIC: u16 = 262;
const TAG_STRIP_OFFSETS: u16 = 273;
const TAG_SAMPLES_PER_PIXEL: u16 = 277;
const TAG_ROWS_PER_STRIP: u16 = 278;
const TAG_STRIP_BYTE_COUNTS: u16 = 279;
const TAG_PLANAR_CONFIG: u16 = 284;
const TAG_PREDICTOR: u16 = 317;
const TAG_TILE_WIDTH: u16 = 322;
const TAG_TILE_LENGTH: u16 = 323;
const TAG_TILE_OFFSETS: u16 = 324;
const TAG_TILE_BYTE_COUNTS: u16 = 325;
const TAG_SAMPLE_FORMAT: u16 = 339;
const TAG_MODEL_PIXEL_SCALE: u16 = 33550;
const TAG_MODEL_TIEPOINT: u16 = 33922;
const TAG_GEO_KEY_DIRECTORY: u16 = 34735;

const TYPE_BYTE: u16 = 1;
const TYPE_SHORT: u16 = 3;
const TYPE_LONG: u16 = 4;
const TYPE_DOUBLE: u16 = 12;

const COMPRESSION_NONE: u16 = 1;
const COMPRESSION_DEFLATE: u16 = 8;
const COMPRESSION_DEFLATE_OLD: u16 = 32946;

const GEOKEY_GEOGRAPHIC_TYPE: u16 = 2048;
const GEOKEY_PROJECTED_TYPE: u16 = 3072;

/// 带字节序的切片读取器。
struct Reader<'a> {
    data: &'a [u8],
    little: bool,
}

impl<'a> Reader<'a> {
    fn u16(&self, off: usize) -> Result<u16, String> {
        let b = self
            .slice(off, 2)
            .map_err(|e| format!("TIFF 数据被截断：{e}"))?;
        Ok(if self.little {
            u16::from_le_bytes([b[0], b[1]])
        } else {
            u16::from_be_bytes([b[0], b[1]])
        })
    }

    fn u32(&self, off: usize) -> Result<u32, String> {
        let b = self
            .slice(off, 4)
            .map_err(|e| format!("TIFF 数据被截断：{e}"))?;
        Ok(if self.little {
            u32::from_le_bytes([b[0], b[1], b[2], b[3]])
        } else {
            u32::from_be_bytes([b[0], b[1], b[2], b[3]])
        })
    }

    fn f64(&self, off: usize) -> Result<f64, String> {
        let b = self
            .slice(off, 8)
            .map_err(|e| format!("TIFF 数据被截断：{e}"))?;
        let mut a = [0u8; 8];
        a.copy_from_slice(b);
        Ok(if self.little {
            f64::from_le_bytes(a)
        } else {
            f64::from_be_bytes(a)
        })
    }

    fn slice(&self, off: usize, len: usize) -> Result<&'a [u8], String> {
        self.data
            .get(off..off + len)
            .ok_or_else(|| format!("偏移 {off} 处需要 {len} 字节，超出文件长度 {}", self.data.len()))
    }
}

fn type_size(kind: u16) -> Result<usize, String> {
    match kind {
        TYPE_BYTE => Ok(1),
        TYPE_SHORT => Ok(2),
        TYPE_LONG => Ok(4),
        TYPE_DOUBLE => Ok(8),
        other => Err(format!("不支持的 TIFF 字段类型 {other}（仅支持 BYTE/SHORT/LONG/DOUBLE）")),
    }
}

/// IFD 摘要：记录每个字段 entry 的起始偏移。
#[derive(Clone)]
struct Ifd {
    entries: Vec<(u16, usize)>,
}

impl Ifd {
    fn find(&self, tag: u16) -> Option<usize> {
        self.entries.iter().find(|(t, _)| *t == tag).map(|(_, o)| *o)
    }

    /// 读取 SHORT/LONG 数组（用于 offset/count 类字段，类型可能是 SHORT 或 LONG）。
    fn read_uint_array(&self, r: &Reader, tag: u16) -> Result<Option<Vec<u64>>, String> {
        let Some(off) = self.find(tag) else {
            return Ok(None);
        };
        let kind = r.u16(off + 2)?;
        let count = r.u32(off + 4)? as usize;
        let unit = type_size(kind)?;
        let data_off = if unit * count <= 4 { off + 8 } else { r.u32(off + 8)? as usize };
        let mut out = Vec::with_capacity(count);
        for i in 0..count {
            let v = match kind {
                TYPE_BYTE => r.slice(data_off + i, 1)?[0] as u64,
                TYPE_SHORT => r.u16(data_off + i * 2)? as u64,
                TYPE_LONG => r.u32(data_off + i * 4)? as u64,
                other => return Err(format!("字段 {tag} 类型 {other} 不是整型数组")),
            };
            out.push(v);
        }
        Ok(Some(out))
    }

    fn read_f64_array(&self, r: &Reader, tag: u16) -> Result<Option<Vec<f64>>, String> {
        let Some(off) = self.find(tag) else {
            return Ok(None);
        };
        let kind = r.u16(off + 2)?;
        if kind != TYPE_DOUBLE {
            return Err(format!("字段 {tag} 必须是 DOUBLE（实际类型 {kind}）"));
        }
        let count = r.u32(off + 4)? as usize;
        let data_off = r.u32(off + 8)? as usize;
        let mut out = Vec::with_capacity(count);
        for i in 0..count {
            out.push(r.f64(data_off + i * 8)?);
        }
        Ok(Some(out))
    }

    fn read_u16(&self, r: &Reader, tag: u16) -> Result<Option<u16>, String> {
        let Some(off) = self.find(tag) else {
            return Ok(None);
        };
        let kind = r.u16(off + 2)?;
        let v = match kind {
            TYPE_BYTE => r.slice(off + 8, 1)?[0] as u16,
            TYPE_SHORT => r.u16(off + 8)?,
            other => return Err(format!("字段 {tag} 类型 {other} 不是 SHORT")),
        };
        Ok(Some(v))
    }

    fn read_u32(&self, r: &Reader, tag: u16) -> Result<Option<u32>, String> {
        Ok(self.read_uint_array(r, tag)?.and_then(|a| a.first().map(|&v| v as u32)))
    }
}

/// 读取全部 IFD，优先选择含地理 tag（ModelPixelScale）的那个，其次第一个。
fn locate_ifd(r: &Reader) -> Result<Ifd, String> {
    let mut first_ifd: Option<Ifd> = None;
    let mut ifd_off = r.u32(4)? as usize;
    for _ in 0..8 {
        if ifd_off == 0 {
            break;
        }
        let count = r.u16(ifd_off)? as usize;
        let mut entries = Vec::with_capacity(count);
        for i in 0..count {
            let eoff = ifd_off + 2 + i * 12;
            entries.push((r.u16(eoff)?, eoff));
        }
        let ifd = Ifd { entries };
        if first_ifd.is_none() {
            first_ifd = Some(ifd.clone());
        }
        if ifd.find(TAG_MODEL_PIXEL_SCALE).is_some() {
            return Ok(ifd);
        }
        // 下一个 IFD
        let next_rel = ifd_off + 2 + count * 12;
        ifd_off = r.u32(next_rel)? as usize;
    }
    first_ifd.ok_or_else(|| "TIFF 中没有任何 IFD".to_string())
}

/// 解析一段 TIFF/GeoTIFF 字节。
pub fn read(bytes: &[u8]) -> Result<GeoTiff, String> {
    if bytes.len() < 8 {
        return Err(format!("TIFF 文件过短（{} 字节）", bytes.len()));
    }
    let little = match &bytes[0..2] {
        b"II" => true,
        b"MM" => false,
        _ => return Err("不是 TIFF 文件（缺少 II/MM 字节序标记）".to_string()),
    };
    let r = Reader { data: bytes, little };
    let magic = r.u16(2)?;
    if magic != 42 {
        return Err(format!("TIFF magic 为 {magic}（应为 42）"));
    }
    let ifd = locate_ifd(&r)?;

    // ---- 基本几何与格式 ----
    let width = ifd
        .read_u32(&r, TAG_IMAGE_WIDTH)?
        .ok_or("缺少 ImageWidth(256)")? as usize;
    let height = ifd
        .read_u32(&r, TAG_IMAGE_LENGTH)?
        .ok_or("缺少 ImageLength(257)")? as usize;
    if width == 0 || height == 0 {
        return Err(format!("影像尺寸非法：{width}x{height}"));
    }
    let spp = ifd.read_u16(&r, TAG_SAMPLES_PER_PIXEL)?.unwrap_or(1) as usize;
    if !matches!(spp, 1 | 3 | 4) {
        return Err(format!("不支持的 SamplesPerPixel={spp}（仅支持 1/3/4）"));
    }
    let bits = ifd.read_uint_array(&r, TAG_BITS_PER_SAMPLE)?;
    if let Some(bits) = &bits {
        if bits.len() != spp || bits.iter().any(|&b| b != 8) {
            return Err(format!(
                "不支持的位深 {bits:?}（仅支持每样本 8 bit）"
            ));
        }
    }
    let photometric = ifd
        .read_u16(&r, TAG_PHOTOMETRIC)?
        .ok_or("缺少 PhotometricInterpretation(262)")?;
    if spp == 1 && photometric != 1 {
        return Err(format!(
            "单波段影像 PhotometricInterpretation={photometric} 不支持（仅支持 1=BlackIsZero 灰度）"
        ));
    }
    if spp >= 3 && photometric != 2 {
        return Err(format!(
            "多波段影像 PhotometricInterpretation={photometric} 不支持（仅支持 2=RGB；Palette(3)/CMYK(5) 等明确拒绝）"
        ));
    }
    let compression = ifd.read_u16(&r, TAG_COMPRESSION)?.unwrap_or(COMPRESSION_NONE);
    if !matches!(compression, COMPRESSION_NONE | COMPRESSION_DEFLATE | COMPRESSION_DEFLATE_OLD) {
        return Err(format!(
            "不支持的压缩方式 {compression}（仅支持无压缩=1、Deflate=8/32946；LZW=5/JPEG=7/PackBits=32773 等明确拒绝）"
        ));
    }
    let predictor = ifd.read_u16(&r, TAG_PREDICTOR)?.unwrap_or(1);
    if predictor != 1 {
        return Err(format!(
            "不支持的 Predictor={predictor}（仅支持 1；差分预测 2/浮动点 3 明确拒绝）"
        ));
    }
    let planar = ifd.read_u16(&r, TAG_PLANAR_CONFIG)?.unwrap_or(1);
    if planar != 1 {
        return Err(format!(
            "不支持的 PlanarConfiguration={planar}（仅支持 1=Chunky 交错存储）"
        ));
    }
    let sample_format = ifd.read_u16(&r, TAG_SAMPLE_FORMAT)?.unwrap_or(1);
    if sample_format != 1 {
        return Err(format!(
            "不支持的 SampleFormat={sample_format}（仅支持 1=无符号整型）"
        ));
    }

    // ---- 像素解码（条带 / 分块两种组织） ----
    let raw = decode_pixels(&r, &ifd, width, height, spp, compression)?;

    // ---- 统一为 RGBA8 ----
    let mut rgba = vec![0u8; width * height * 4];
    match spp {
        1 => {
            for (i, &v) in raw.iter().enumerate() {
                let o = i * 4;
                rgba[o] = v;
                rgba[o + 1] = v;
                rgba[o + 2] = v;
                rgba[o + 3] = 255;
            }
        }
        3 => {
            for (i, px) in raw.chunks_exact(3).enumerate() {
                let o = i * 4;
                rgba[o..o + 3].copy_from_slice(px);
                rgba[o + 3] = 255;
            }
        }
        _ => {
            rgba.copy_from_slice(&raw);
        }
    }

    // ---- 地理参考 ----
    let (crs, bbox) = parse_georeferencing(&r, &ifd, width, height)?;

    Ok(GeoTiff { width, height, rgba, crs, bbox })
}

/// 解码全部像素为原始交错字节（len = w*h*spp）。
fn decode_pixels(
    r: &Reader,
    ifd: &Ifd,
    width: usize,
    height: usize,
    spp: usize,
    compression: u16,
) -> Result<Vec<u8>, String> {
    let tile_w = ifd.read_u32(r, TAG_TILE_WIDTH)?;
    let tile_l = ifd.read_u32(r, TAG_TILE_LENGTH)?;
    let tiled = tile_w.is_some() || tile_l.is_some();
    let (offsets, counts): (Vec<u64>, Vec<u64>) = if tiled {
        // 分块（Tiled）组织
        let tw = tile_w.ok_or("缺少 TileWidth(322)")? as usize;
        let tl = tile_l.ok_or("缺少 TileLength(323)")? as usize;
        if !tw.is_power_of_two() || !tl.is_power_of_two() {
            return Err(format!("分块尺寸 {tw}x{tl} 不是 2 的幂（TIFF 规范要求）"));
        }
        let offsets = ifd
            .read_uint_array(r, TAG_TILE_OFFSETS)?
            .ok_or("缺少 TileOffsets(324)")?;
        let counts = ifd
            .read_uint_array(r, TAG_TILE_BYTE_COUNTS)?
            .ok_or("缺少 TileByteCounts(325)")?;
        (offsets, counts)
    } else {
        // 条带（Strip）组织
        let offsets = ifd
            .read_uint_array(r, TAG_STRIP_OFFSETS)?
            .ok_or("缺少 StripOffsets(273)：既非条带也非分块组织，无法读取像素")?;
        let counts = ifd
            .read_uint_array(r, TAG_STRIP_BYTE_COUNTS)?
            .ok_or("缺少 StripByteCounts(279)")?;
        (offsets, counts)
    };
    if offsets.len() != counts.len() {
        return Err(format!(
            "偏移数量({}) 与字节数量({}) 不一致",
            offsets.len(),
            counts.len()
        ));
    }

    let row_bytes = width * spp;
    let mut raw = vec![0u8; row_bytes * height];

    if tiled {
        // ---- 分块组装 ----
        let tw = tile_w.unwrap() as usize;
        let tl = tile_l.unwrap() as usize;
        let across = width.div_ceil(tw);
        let seg_full = tw * tl * spp;
        for (i, (&off, &cnt)) in offsets.iter().zip(&counts).enumerate() {
            let tx = i % across;
            let ty = i / across;
            if ty * tl >= height {
                break;
            }
            let data = decompress_segment(r, off, cnt, compression, seg_full)?;
            let rows = tl.min(height - ty * tl);
            let cols = tw.min(width - tx * tw);
            for row in 0..rows {
                let src = row * tw * spp;
                let dst = (ty * tl + row) * row_bytes + tx * tw * spp;
                let n = cols * spp;
                raw[dst..dst + n].copy_from_slice(&data[src..src + n]);
            }
        }
    } else {
        // ---- 条带组装 ----
        let rows_per_strip = ifd.read_u32(r, TAG_ROWS_PER_STRIP)?.unwrap_or(height as u32) as usize;
        if rows_per_strip == 0 || rows_per_strip > height {
            return Err(format!("RowsPerStrip={rows_per_strip} 非法（应在 1..={height}）"));
        }
        for (i, (&off, &cnt)) in offsets.iter().zip(&counts).enumerate() {
            let row0 = i * rows_per_strip;
            if row0 >= height {
                break;
            }
            let rows = rows_per_strip.min(height - row0);
            let expected = rows * row_bytes;
            let data = decompress_segment(r, off, cnt, compression, expected)?;
            raw[row0 * row_bytes..row0 * row_bytes + expected]
                .copy_from_slice(&data[..expected]);
        }
    }
    Ok(raw)
}

/// 解压（或原样读取）一段像素数据，并校验长度。
fn decompress_segment(
    r: &Reader,
    offset: u64,
    count: u64,
    compression: u16,
    expected: usize,
) -> Result<Vec<u8>, String> {
    let compressed = r
        .slice(offset as usize, count as usize)
        .map_err(|e| format!("像素数据段被截断：{e}"))?;
    let data: Vec<u8> = match compression {
        COMPRESSION_NONE => compressed.to_vec(),
        COMPRESSION_DEFLATE | COMPRESSION_DEFLATE_OLD => {
            let mut out = Vec::new();
            ZlibDecoder::new(compressed)
                .read_to_end(&mut out)
                .map_err(|e| format!("Deflate 流解压失败：{e}"))?;
            out
        }
        other => return Err(format!("不支持的压缩方式 {other}")),
    };
    if data.len() < expected {
        return Err(format!(
            "解压后数据不足：期望 {expected} 字节，实际 {} 字节",
            data.len()
        ));
    }
    Ok(data)
}

/// 从 GeoKey 目录提取 `key -> 短整型值`（仅取内联单值，本模块只需要 1024/2048/3072）。
fn geokey_values(dir: &[u16]) -> std::collections::BTreeMap<u16, u16> {
    let mut map = std::collections::BTreeMap::new();
    if dir.len() < 4 {
        return map;
    }
    let n = dir[3] as usize;
    for i in 0..n {
        let base = 4 + i * 4;
        if base + 4 > dir.len() {
            break;
        }
        let key = dir[base];
        let loc = dir[base + 1];
        let _count = dir[base + 2];
        let value = dir[base + 3];
        if loc == 0 {
            map.insert(key, value);
        }
    }
    map
}

/// 解析地理参考：CRS 识别（GeoKey 3072/2048，仅 EPSG:4326/3857）+ bbox。
fn parse_georeferencing(
    r: &Reader,
    ifd: &Ifd,
    width: usize,
    height: usize,
) -> Result<(Crs, [f64; 4]), String> {
    // ---- CRS ----
    let crs = {
        let dir = ifd.read_uint_array_shorts(r, TAG_GEO_KEY_DIRECTORY)?;
        let keys = match dir {
            Some(d) => geokey_values(&d),
            None => std::collections::BTreeMap::new(),
        };
        let proj = keys.get(&GEOKEY_PROJECTED_TYPE).copied();
        let geo = keys.get(&GEOKEY_GEOGRAPHIC_TYPE).copied();
        match proj {
            Some(3857) => Crs::Epsg3857,
            Some(4326) => Crs::Epsg4326,
            Some(other) => {
                return Err(format!(
                    "不支持的投影 CRS：EPSG:{other}（当前仅支持 EPSG:4326 / EPSG:3857，完整 CRS 识别由后续版本提供）"
                ))
            }
            None => match geo {
                Some(4326) => Crs::Epsg4326,
                Some(other) => {
                    return Err(format!(
                        "不支持的地理 CRS：EPSG:{other}（当前仅支持 EPSG:4326）"
                    ))
                }
                None => {
                    return Err(
                        "缺少 CRS 标识（GeoKeyDirectoryTag(34735) 缺失或无 3072/2048 键）".to_string()
                    )
                }
            },
        }
    };

    // ---- bbox：pixel scale + tiepoint（北朝上假定） ----
    let scale = ifd
        .read_f64_array(r, TAG_MODEL_PIXEL_SCALE)?
        .ok_or("缺少 ModelPixelScaleTag(33550)，无法定位影像")?;
    let tie = ifd
        .read_f64_array(r, TAG_MODEL_TIEPOINT)?
        .ok_or("缺少 ModelTiepointTag(33922)，无法定位影像")?;
    if scale.len() < 2 || tie.len() < 6 {
        return Err(format!(
            "地理 tag 长度不足：PixelScale={}（需≥2）、Tiepoint={}（需≥6）",
            scale.len(),
            tie.len()
        ));
    }
    if scale.iter().take(2).any(|&s| !s.is_finite() || s == 0.0) {
        return Err(format!("ModelPixelScale 非法：{:?}", &scale[..2]));
    }
    if tie.iter().take(6).any(|&v| !v.is_finite()) {
        return Err(format!("ModelTiepoint 非法：{:?}", &tie[..6]));
    }
    let (sx, sy) = (scale[0], scale[1]);
    let (ti, tj, tx, ty) = (tie[0], tie[1], tie[3], tie[4]);
    // geo_x(col) = tx + (col - ti) * sx；geo_y(row) = ty - (row - tj) * sy（北朝上）
    let minx = tx - ti * sx;
    let maxx = minx + width as f64 * sx;
    let maxy = ty + tj * sy;
    let miny = maxy - height as f64 * sy;
    let (miny, maxy) = if miny <= maxy { (miny, maxy) } else { (maxy, miny) };
    let bbox = [minx, miny, maxx, maxy];
    if bbox.iter().any(|v| !v.is_finite()) {
        return Err(format!("解析出的 bbox 非法：{bbox:?}"));
    }
    Ok((crs, bbox))
}

impl Ifd {
    /// SHORT 数组读取（GeoKey 目录专用）。
    fn read_uint_array_shorts(&self, r: &Reader, tag: u16) -> Result<Option<Vec<u16>>, String> {
        let Some(off) = self.find(tag) else {
            return Ok(None);
        };
        let kind = r.u16(off + 2)?;
        if kind != TYPE_SHORT {
            return Err(format!("字段 {tag} 必须是 SHORT 数组（实际类型 {kind}）"));
        }
        let count = r.u32(off + 4)? as usize;
        let data_off = if 2 * count <= 4 { off + 8 } else { r.u32(off + 8)? as usize };
        let mut out = Vec::with_capacity(count);
        for i in 0..count {
            out.push(r.u16(data_off + i * 2)?);
        }
        Ok(Some(out))
    }
}

impl GeoTiff {
    /// 最近邻采样：`fx/fy` 为源像素坐标（可为小数/越界），越界返回全透明。
    pub fn sample_nearest(&self, fx: f64, fy: f64) -> [u8; 4] {
        let col = fx.floor() as isize;
        let row = fy.floor() as isize;
        if col < 0 || row < 0 || col >= self.width as isize || row >= self.height as isize {
            return [0, 0, 0, 0];
        }
        let o = (row as usize * self.width + col as usize) * 4;
        [
            self.rgba[o],
            self.rgba[o + 1],
            self.rgba[o + 2],
            self.rgba[o + 3],
        ]
    }

    /// 双线性采样（按 alpha 加权混合颜色），越界返回全透明。
    pub fn sample_bilinear(&self, fx: f64, fy: f64) -> [u8; 4] {
        if fx < -1.0 || fy < -1.0 || fx > self.width as f64 || fy > self.height as f64 {
            return [0, 0, 0, 0];
        }
        let x0 = fx.floor();
        let y0 = fy.floor();
        let (tx, ty) = (fx - x0, fy - y0);
        let mut acc = [0.0f64; 4];
        let mut wsum = 0.0f64;
        for (dy, wy) in [(0.0, 1.0 - ty), (1.0, ty)] {
            for (dx, wx) in [(0.0, 1.0 - tx), (1.0, tx)] {
                let px = self.sample_nearest(x0 + dx, y0 + dy);
                let a = px[3] as f64 / 255.0;
                let w = wx * wy * a; // 透明像素不污染颜色
                acc[0] += px[0] as f64 * w;
                acc[1] += px[1] as f64 * w;
                acc[2] += px[2] as f64 * w;
                acc[3] += px[3] as f64 * wx * wy;
                wsum += w;
            }
        }
        if wsum > 0.0 {
            acc[0] /= wsum;
            acc[1] /= wsum;
            acc[2] /= wsum;
        }
        [
            acc[0].round().clamp(0.0, 255.0) as u8,
            acc[1].round().clamp(0.0, 255.0) as u8,
            acc[2].round().clamp(0.0, 255.0) as u8,
            acc[3].round().clamp(0.0, 255.0) as u8,
        ]
    }
}
