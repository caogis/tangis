//! 最小 TIFF / GeoTIFF 读写器（纯 Rust，无 GDAL）。
//!
//! 能力边界（超出即明确报错，禁止静默降级）：
//! - 仅支持**未压缩**（Compression=1）**单页**（单个 IFD）TIFF；
//! - 仅支持单波段（SamplesPerPixel=1）栅格，采样格式 uint16 / float32；
//! - 写出固定为小端（II）非压缩 GeoTIFF，保留 33550/33922/34735 关键 tag。
//!
//! 选型说明：不引入 `tiff` crate 的原因是其 GeoTIFF 扩展 tag（模型像素比例、
//! 连接点、GeoKey 目录）需要自行透传，且交叉编译场景下自研百余行纯 Rust
//! 读写器更可控（PRD 2.7 许可与交叉编译约束）。

use std::collections::BTreeMap;

use thiserror::Error;

/// TIFF/GeoTIFF 读写错误。
#[derive(Debug, Error)]
pub enum TiffError {
    /// 文件不是合法 TIFF（魔数/结构错误）。
    #[error("非法 TIFF：{0}")]
    Invalid(String),
    /// 结构合法但属于不支持的子集（压缩/多页/多波段等）。
    #[error("不支持的 TIFF 结构：{0}")]
    Unsupported(String),
    /// 缺少必需的 tag。
    #[error("缺少必需 tag {0}（{1}）")]
    MissingTag(u16, &'static str),
}

/// GeoTIFF 关键地理 tag（写出时透传保留）。
#[derive(Debug, Clone, PartialEq)]
pub struct GeoTags {
    /// ModelPixelScaleTag(33550)：3 个 double（SX, SY, SZ）。
    pub pixel_scale: [f64; 3],
    /// ModelTiepointTag(33922)：6*n 个 double（i,j,k,x,y,z)*n，至少 1 组。
    pub tiepoint: Vec<f64>,
    /// GeoKeyDirectoryTag(34735)：u16 数组（含 4 个头字段）。
    pub geo_keys: Vec<u16>,
    /// GeoAsciiParamsTag(34737)：ASCII 参数串（可能为 None）。
    pub geo_ascii_params: Option<String>,
}

/// 单波段栅格采样数据。
#[derive(Debug, Clone, PartialEq)]
pub enum Samples {
    /// uint16（常见整型 DEM）。
    U16(Vec<u16>),
    /// float32（常见浮点 DEM）。
    F32(Vec<f32>),
}

impl Samples {
    /// 像素个数。
    pub fn len(&self) -> usize {
        match self {
            Samples::U16(v) => v.len(),
            Samples::F32(v) => v.len(),
        }
    }

    /// 是否为空。
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// 按下标取值（f64 视图）。
    pub fn get_f64(&self, i: usize) -> f64 {
        match self {
            Samples::U16(v) => f64::from(v[i]),
            Samples::F32(v) => f64::from(v[i]),
        }
    }

    /// 按下标写入值（保持原类型；u16 四舍五入并饱和到 [0, 65535]）。
    pub fn set_f64(&mut self, i: usize, v: f64) {
        match self {
            Samples::U16(vec) => {
                vec[i] = v.round().clamp(0.0, u16::MAX as f64) as u16;
            }
            Samples::F32(vec) => vec[i] = v as f32,
        }
    }
}

/// 单波段栅格影像（未压缩 GeoTIFF 的最小模型）。
#[derive(Debug, Clone, PartialEq)]
pub struct Raster {
    /// 宽（像素数）。
    pub width: u32,
    /// 高（像素数）。
    pub height: u32,
    /// 波段数据（长度 = width*height，行优先，从左上角开始）。
    pub samples: Samples,
}

// ---- 常用 tag 编号 ----
const TAG_IMAGE_WIDTH: u16 = 256;
const TAG_IMAGE_HEIGHT: u16 = 257;
const TAG_BITS_PER_SAMPLE: u16 = 258;
const TAG_COMPRESSION: u16 = 259;
const TAG_PHOTOMETRIC: u16 = 262;
const TAG_STRIP_OFFSETS: u16 = 273;
const TAG_SAMPLES_PER_PIXEL: u16 = 277;
const TAG_ROWS_PER_STRIP: u16 = 278;
const TAG_STRIP_BYTE_COUNTS: u16 = 279;
const TAG_PLANAR_CONFIG: u16 = 284;
const TAG_SAMPLE_FORMAT: u16 = 339;
const TAG_MODEL_PIXEL_SCALE: u16 = 33550;
const TAG_MODEL_TIEPOINT: u16 = 33922;
const TAG_GEO_KEY_DIRECTORY: u16 = 34735;
/// ModelTransformationTag：出现即不支持（非简单北向上网格）。
pub const TAG_MODEL_TRANSFORM: u16 = 33920;

const TYPE_ASCII: u16 = 2;
const TYPE_SHORT: u16 = 3;
const TYPE_LONG: u16 = 4;
const TYPE_DOUBLE: u16 = 12;

/// IFD 解析结果：tag → (type, count, 值字节，已统一为小端)。
struct Ifd {
    entries: BTreeMap<u16, (u16, u32, Vec<u8>)>,
}

impl Ifd {
    fn get(&self, tag: u16) -> Option<&(u16, u32, Vec<u8>)> {
        self.entries.get(&tag)
    }

    fn u16_opt(&self, tag: u16) -> Result<Option<u16>, TiffError> {
        match self.get(tag) {
            None => Ok(None),
            Some(&(ty, count, ref raw)) => {
                if ty != TYPE_SHORT || count != 1 {
                    return Err(TiffError::Unsupported(format!(
                        "tag {tag} 类型/数量不符（期望 SHORT×1）"
                    )));
                }
                Ok(Some(u16::from_le_bytes(raw[..2].try_into().unwrap())))
            }
        }
    }

    fn u32_opt(&self, tag: u16) -> Result<Option<u32>, TiffError> {
        match self.get(tag) {
            None => Ok(None),
            Some(&(ty, count, ref raw)) => {
                if ty != TYPE_LONG || count != 1 {
                    return Err(TiffError::Unsupported(format!(
                        "tag {tag} 类型/数量不符（期望 LONG×1）"
                    )));
                }
                Ok(Some(u32::from_le_bytes(raw[..4].try_into().unwrap())))
            }
        }
    }

    fn f64_vec(&self, tag: u16) -> Result<Option<Vec<f64>>, TiffError> {
        match self.get(tag) {
            None => Ok(None),
            Some(&(ty, _, ref raw)) => {
                if ty != TYPE_DOUBLE {
                    return Err(TiffError::Unsupported(format!(
                        "tag {tag} 类型不符（期望 DOUBLE）"
                    )));
                }
                Ok(Some(
                    raw.chunks_exact(8)
                        .map(|c| f64::from_le_bytes(c.try_into().unwrap()))
                        .collect(),
                ))
            }
        }
    }

    fn u16_vec(&self, tag: u16) -> Result<Option<Vec<u16>>, TiffError> {
        match self.get(tag) {
            None => Ok(None),
            Some(&(ty, _, ref raw)) => {
                if ty != TYPE_SHORT {
                    return Err(TiffError::Unsupported(format!(
                        "tag {tag} 类型不符（期望 SHORT）"
                    )));
                }
                Ok(Some(
                    raw.chunks_exact(2)
                        .map(|c| u16::from_le_bytes(c.try_into().unwrap()))
                        .collect(),
                ))
            }
        }
    }

    fn ascii(&self, tag: u16) -> Result<Option<String>, TiffError> {
        match self.get(tag) {
            None => Ok(None),
            Some(&(ty, _, ref raw)) => {
                if ty != TYPE_ASCII {
                    return Err(TiffError::Unsupported(format!(
                        "tag {tag} 类型不符（期望 ASCII）"
                    )));
                }
                Ok(Some(
                    raw.iter().filter(|&&b| b != 0).map(|&b| b as char).collect(),
                ))
            }
        }
    }
}

fn type_size(ty: u16) -> Result<u32, TiffError> {
    match ty {
        1 | TYPE_ASCII => Ok(1),
        TYPE_SHORT => Ok(2),
        TYPE_LONG => Ok(4),
        TYPE_DOUBLE => Ok(8),
        other => Err(TiffError::Unsupported(format!(
            "TIFF 字段类型 {other} 不支持"
        ))),
    }
}

/// 解析第一个 IFD（多页 TIFF 明确报错）。
fn parse_ifd(data: &[u8]) -> Result<Ifd, TiffError> {
    if data.len() < 8 {
        return Err(TiffError::Invalid(format!(
            "文件过小（{} 字节），不足以容纳 TIFF 头",
            data.len()
        )));
    }
    let bo: [u8; 2] = data[0..2].try_into().unwrap();
    let le = match &bo {
        b"II" => true,
        b"MM" => false,
        other => {
            return Err(TiffError::Invalid(format!(
                "字节序标记 {:?} 非法（应为 II/MM）",
                String::from_utf8_lossy(other)
            )))
        }
    };
    let read_u16 = |off: usize| -> u16 {
        let b: [u8; 2] = data[off..off + 2].try_into().unwrap();
        if le {
            u16::from_le_bytes(b)
        } else {
            u16::from_be_bytes(b)
        }
    };
    let read_u32 = |off: usize| -> u32 {
        let b: [u8; 4] = data[off..off + 4].try_into().unwrap();
        if le {
            u32::from_le_bytes(b)
        } else {
            u32::from_be_bytes(b)
        }
    };
    let magic = read_u16(2);
    if magic != 42 {
        return Err(TiffError::Invalid(format!(
            "TIFF magic = {magic}，应为 42"
        )));
    }
    let ifd_off = read_u32(4) as usize;
    if ifd_off + 2 > data.len() {
        return Err(TiffError::Invalid(format!(
            "IFD 偏移 {ifd_off} 越界（文件 {} 字节）",
            data.len()
        )));
    }
    let count = read_u16(ifd_off) as usize;
    if ifd_off + 2 + count * 12 + 4 > data.len() {
        return Err(TiffError::Invalid("IFD 条目区越界".into()));
    }
    let mut entries = BTreeMap::new();
    let mut off = ifd_off + 2;
    for _ in 0..count {
        let tag = read_u16(off);
        let ty = read_u16(off + 2);
        let cnt = read_u32(off + 4) as usize;
        let size = type_size(ty)? as usize * cnt;
        let raw: Vec<u8> = if size <= 4 {
            data[off + 8..off + 8 + size].to_vec()
        } else {
            let voff = read_u32(off + 8) as usize;
            if voff + size > data.len() {
                return Err(TiffError::Invalid(format!(
                    "tag {tag} 外部值偏移 {voff}+{size} 越界"
                )));
            }
            data[voff..voff + size].to_vec()
        };
        // 大端源统一转小端，后续全部按 LE 解读
        let raw = if le {
            raw
        } else {
            match ty {
                TYPE_SHORT => raw
                    .chunks_exact(2)
                    .flat_map(|c| u16::from_be_bytes(c.try_into().unwrap()).to_le_bytes())
                    .collect(),
                TYPE_LONG => raw
                    .chunks_exact(4)
                    .flat_map(|c| u32::from_be_bytes(c.try_into().unwrap()).to_le_bytes())
                    .collect(),
                TYPE_DOUBLE => raw
                    .chunks_exact(8)
                    .flat_map(|c| f64::from_be_bytes(c.try_into().unwrap()).to_le_bytes())
                    .collect(),
                _ => raw,
            }
        };
        entries.insert(tag, (ty, cnt as u32, raw));
        off += 12;
    }
    let next = read_u32(off);
    if next != 0 {
        return Err(TiffError::Unsupported(
            "多页 TIFF（IFD 链非空）暂不支持；请提供单页 DEM".into(),
        ));
    }
    Ok(Ifd { entries })
}

/// 解析 GeoTIFF 关键地理 tag（33550/33922/34735 必须齐全）。
pub fn read_geo_tags(data: &[u8]) -> Result<GeoTags, TiffError> {
    let ifd = parse_ifd(data)?;
    if ifd.entries.contains_key(&TAG_MODEL_TRANSFORM) {
        return Err(TiffError::Unsupported(
            "存在 ModelTransformationTag(33920)（仿射变换网格），暂不支持".into(),
        ));
    }
    let pixel_scale =
        ifd.f64_vec(TAG_MODEL_PIXEL_SCALE)?
            .ok_or(TiffError::MissingTag(
                TAG_MODEL_PIXEL_SCALE,
                "ModelPixelScale",
            ))?;
    if pixel_scale.len() != 3 {
        return Err(TiffError::Unsupported(format!(
            "ModelPixelScale 应为 3 个 double，实际 {} 个",
            pixel_scale.len()
        )));
    }
    let tiepoint = ifd
        .f64_vec(TAG_MODEL_TIEPOINT)?
        .ok_or(TiffError::MissingTag(TAG_MODEL_TIEPOINT, "ModelTiepoint"))?;
    if tiepoint.is_empty() || tiepoint.len() % 6 != 0 {
        return Err(TiffError::Unsupported(format!(
            "ModelTiepoint 应为 6*n 个 double，实际 {} 个",
            tiepoint.len()
        )));
    }
    let geo_keys =
        ifd.u16_vec(TAG_GEO_KEY_DIRECTORY)?
            .ok_or(TiffError::MissingTag(
                TAG_GEO_KEY_DIRECTORY,
                "GeoKeyDirectory",
            ))?;
    if geo_keys.len() < 4 {
        return Err(TiffError::Unsupported(format!(
            "GeoKeyDirectory 头不完整（{} 个 u16，至少 4 个）",
            geo_keys.len()
        )));
    }
    Ok(GeoTags {
        pixel_scale: pixel_scale.try_into().unwrap(),
        tiepoint,
        geo_keys,
        geo_ascii_params: ifd.ascii(34737)?,
    })
}

/// 解析未压缩单波段 uint16/float32 栅格。
pub fn read_raster(data: &[u8]) -> Result<Raster, TiffError> {
    let ifd = parse_ifd(data)?;
    let width = ifd
        .u32_opt(TAG_IMAGE_WIDTH)?
        .ok_or(TiffError::MissingTag(TAG_IMAGE_WIDTH, "ImageWidth"))?;
    let height = ifd
        .u32_opt(TAG_IMAGE_HEIGHT)?
        .ok_or(TiffError::MissingTag(TAG_IMAGE_HEIGHT, "ImageHeight"))?;
    let compression = ifd.u16_opt(TAG_COMPRESSION)?.unwrap_or(1);
    if compression != 1 {
        return Err(TiffError::Unsupported(format!(
            "压缩方式 {compression} 不支持（仅无压缩 Compression=1）"
        )));
    }
    let planar = ifd.u16_opt(TAG_PLANAR_CONFIG)?.unwrap_or(1);
    if planar != 1 {
        return Err(TiffError::Unsupported(
            "PlanarConfiguration=2（波段分离）不支持".into(),
        ));
    }
    let spp = ifd.u16_opt(TAG_SAMPLES_PER_PIXEL)?.unwrap_or(1);
    if spp != 1 {
        return Err(TiffError::Unsupported(format!(
            "波段数 {spp} 不支持（DEM 需单波段）"
        )));
    }
    let bits = ifd
        .u16_opt(TAG_BITS_PER_SAMPLE)?
        .ok_or(TiffError::MissingTag(TAG_BITS_PER_SAMPLE, "BitsPerSample"))?;
    let format = ifd.u16_opt(TAG_SAMPLE_FORMAT)?.unwrap_or(1);
    let samples = match (bits, format) {
        (16, 1) => Samples::U16(
            decode_strips(data, &ifd, width, height, 2)?
                .chunks_exact(2)
                .map(|c| u16::from_le_bytes(c.try_into().unwrap()))
                .collect(),
        ),
        (32, 3) => Samples::F32(
            decode_strips(data, &ifd, width, height, 4)?
                .chunks_exact(4)
                .map(|c| f32::from_le_bytes(c.try_into().unwrap()))
                .collect(),
        ),
        (b, f) => {
            return Err(TiffError::Unsupported(format!(
                "bits={b}/SampleFormat={f} 不支持（支持 uint16 / float32 单波段 DEM）"
            )))
        }
    };
    Ok(Raster {
        width,
        height,
        samples,
    })
}

/// 收集全部条带字节并严格校验总长度（短缺/超长都报错）。
fn decode_strips(
    data: &[u8],
    ifd: &Ifd,
    width: u32,
    height: u32,
    bytes_per_pixel: usize,
) -> Result<Vec<u8>, TiffError> {
    let offsets = ifd
        .get(TAG_STRIP_OFFSETS)
        .ok_or(TiffError::MissingTag(TAG_STRIP_OFFSETS, "StripOffsets"))?;
    let counts = ifd
        .get(TAG_STRIP_BYTE_COUNTS)
        .ok_or(TiffError::MissingTag(TAG_STRIP_BYTE_COUNTS, "StripByteCounts"))?;
    let rows_per_strip = ifd.u32_opt(TAG_ROWS_PER_STRIP)?.unwrap_or(u32::MAX);
    if rows_per_strip == 0 {
        return Err(TiffError::Invalid("RowsPerStrip=0 非法".into()));
    }
    let strip_rows = (rows_per_strip as usize).min(height as usize);
    let strip_count =
        (height as usize).div_ceil(strip_rows);
    for e in [offsets, counts] {
        if e.1 as usize != strip_count {
            return Err(TiffError::Unsupported(format!(
                "StripOffsets/StripByteCounts 条目数（{}）与分条数（{strip_count}）不符",
                e.1
            )));
        }
    }
    let to_u32s = |e: &(u16, u32, Vec<u8>)| -> Vec<u32> {
        e.2.chunks_exact(4)
            .map(|c| u32::from_le_bytes(c.try_into().unwrap()))
            .collect()
    };
    let offs = to_u32s(offsets);
    let cnts = to_u32s(counts);
    let expected_total = width as usize * height as usize * bytes_per_pixel;
    let mut out = Vec::with_capacity(expected_total);
    for s in 0..strip_count {
        let (off, cnt) = (offs[s] as usize, cnts[s] as usize);
        let end = off
            .checked_add(cnt)
            .ok_or_else(|| TiffError::Invalid(format!("条带 {s} 偏移/长度溢出")))?;
        if end > data.len() {
            return Err(TiffError::Invalid(format!(
                "条带 {s} 越界（{off}+{cnt} > 文件 {} 字节）",
                data.len()
            )));
        }
        out.extend_from_slice(&data[off..end]);
    }
    if out.len() != expected_total {
        return Err(TiffError::Unsupported(format!(
            "条带字节总数 {} 与 width*height*bpp={expected_total} 不符",
            out.len()
        )));
    }
    Ok(out)
}

/// 写出小端无压缩单波段 GeoTIFF（保留 33550/33922/34735）。
pub fn write_geo_tiff(raster: &Raster, geo: &GeoTags) -> Result<Vec<u8>, TiffError> {
    if raster.width == 0 || raster.height == 0 {
        return Err(TiffError::Invalid("空栅格（宽/高为 0）".into()));
    }
    let pixels = raster.width as usize * raster.height as usize;
    if raster.samples.len() != pixels {
        return Err(TiffError::Invalid(format!(
            "波段数据长度 {} 与 width*height={} 不符",
            raster.samples.len(),
            pixels
        )));
    }
    let (bits, sample_format, pixel_bytes): (u16, u16, Vec<u8>) = match &raster.samples {
        Samples::U16(v) => (
            16,
            1,
            v.iter().flat_map(|x| x.to_le_bytes()).collect::<Vec<u8>>(),
        ),
        Samples::F32(v) => (
            32,
            3,
            v.iter().flat_map(|x| x.to_le_bytes()).collect::<Vec<u8>>(),
        ),
    };
    if !geo.tiepoint.is_empty() && !geo.tiepoint.len().is_multiple_of(6) {
        return Err(TiffError::Invalid(
            "GeoTags.tiepoint 应为 6*n 个 double".into(),
        ));
    }
    if geo.geo_keys.len() < 4 {
        return Err(TiffError::Invalid(
            "GeoTags.geo_keys 应含 4 个头字段".into(),
        ));
    }

    // 条目：tag 升序；值 ≤4 字节内联，否则外部存储。
    // (tag, type, count, inline 4 字节 | external 字节)
    type Entry = (u16, u16, u32, Option<[u8; 4]>, Option<Vec<u8>>);
    let inline_short = |v: u16| {
        let mut b = [0u8; 4];
        b[..2].copy_from_slice(&v.to_le_bytes());
        b
    };
    let inline_long = |v: u32| v.to_le_bytes();
    let ext_doubles = |v: &[f64]| v.iter().flat_map(|x| x.to_le_bytes()).collect::<Vec<u8>>();
    let ext_shorts = |v: &[u16]| v.iter().flat_map(|x| x.to_le_bytes()).collect::<Vec<u8>>();

    let strip_entry_idx = 5usize; // tag 273 在下方升序表中的位置
    let mut entries: Vec<Entry> = vec![
        (TAG_IMAGE_WIDTH, TYPE_LONG, 1, Some(inline_long(raster.width)), None),
        (TAG_IMAGE_HEIGHT, TYPE_LONG, 1, Some(inline_long(raster.height)), None),
        (TAG_BITS_PER_SAMPLE, TYPE_SHORT, 1, Some(inline_short(bits)), None),
        (TAG_COMPRESSION, TYPE_SHORT, 1, Some(inline_short(1)), None),
        (TAG_PHOTOMETRIC, TYPE_SHORT, 1, Some(inline_short(1)), None),
        // StripOffsets：数据区偏移在布局确定后回填（单条带）
        (TAG_STRIP_OFFSETS, TYPE_LONG, 1, None, None),
        (TAG_SAMPLES_PER_PIXEL, TYPE_SHORT, 1, Some(inline_short(1)), None),
        (TAG_ROWS_PER_STRIP, TYPE_LONG, 1, Some(inline_long(raster.height)), None),
        (
            TAG_STRIP_BYTE_COUNTS,
            TYPE_LONG,
            1,
            Some(inline_long(pixel_bytes.len() as u32)),
            None,
        ),
        (TAG_PLANAR_CONFIG, TYPE_SHORT, 1, Some(inline_short(1)), None),
        (TAG_SAMPLE_FORMAT, TYPE_SHORT, 1, Some(inline_short(sample_format)), None),
        (TAG_MODEL_PIXEL_SCALE, TYPE_DOUBLE, 3, None, Some(ext_doubles(&geo.pixel_scale))),
        (
            TAG_MODEL_TIEPOINT,
            TYPE_DOUBLE,
            geo.tiepoint.len() as u32,
            None,
            Some(ext_doubles(&geo.tiepoint)),
        ),
        (
            TAG_GEO_KEY_DIRECTORY,
            TYPE_SHORT,
            geo.geo_keys.len() as u32,
            None,
            Some(ext_shorts(&geo.geo_keys)),
        ),
    ];

    let n = entries.len() as u32;
    let ifd_size = 2 + n * 12 + 4;
    // 外部值区紧跟 IFD，按条目顺序布局，逐项累计偏移；
    // 条带数据紧跟全部外部值。StripOffsets（LONG×1，4 字节）内联，值在此确定。
    let mut externals: Vec<(u32, Vec<u8>)> = Vec::new(); // (绝对偏移, 字节)
    let mut cursor = (8 + ifd_size) as usize;
    for e in entries.iter() {
        let size = type_size(e.1)? as usize * e.2 as usize;
        if size <= 4 {
            continue; // 内联
        }
        let bytes = e
            .4
            .clone()
            .ok_or_else(|| TiffError::Invalid(format!("tag {} 外部值缺失", e.0)))?;
        debug_assert_eq!(bytes.len(), size);
        externals.push((cursor as u32, bytes));
        cursor += size;
    }
    let strip_data_off = cursor as u32;
    entries[strip_entry_idx].3 = Some(inline_long(strip_data_off));

    let mut out = Vec::with_capacity(strip_data_off as usize + pixel_bytes.len());
    out.extend_from_slice(b"II");
    out.extend_from_slice(&42u16.to_le_bytes());
    out.extend_from_slice(&8u32.to_le_bytes()); // IFD 起始偏移
    out.extend_from_slice(&(n as u16).to_le_bytes());
    let mut ext_iter = externals.iter().peekable();
    for e in entries.iter() {
        out.extend_from_slice(&e.0.to_le_bytes());
        out.extend_from_slice(&e.1.to_le_bytes());
        out.extend_from_slice(&e.2.to_le_bytes());
        if let Some(inline) = e.3 {
            out.extend_from_slice(&inline);
        } else {
            let &(off, _) = ext_iter.peek().ok_or_else(|| {
                TiffError::Invalid("内部布局错误：外部值耗尽".into())
            })?;
            out.extend_from_slice(&off.to_le_bytes());
            ext_iter.next();
        }
    }
    out.extend_from_slice(&0u32.to_le_bytes()); // 无后续 IFD
    for (_, bytes) in &externals {
        out.extend_from_slice(bytes);
    }
    debug_assert_eq!(out.len(), strip_data_off as usize);
    out.extend_from_slice(&pixel_bytes);
    Ok(out)
}
