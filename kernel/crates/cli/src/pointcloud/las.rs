//! 最小 LAS 1.0–1.4 点云读取器（纯 Rust，无 las-rs 依赖）。
//!
//! - 公共头块（227 字节起，兼容 1.4 的 375 字节扩展）；
//! - 点格式 0/1/2/3/5/6/7/8（需要 RGB 时仅 2/3/5/7/8）；
//! - LAZ（laszip 压缩）通过 VLR user_id 识别并明确报错（暂不支持解压）；
//! - 逐条流式读取（BufReader），内存占用与读取缓冲成正比。

use std::fs::File;
use std::io::{BufReader, Read, Seek, SeekFrom};
use std::path::Path;

/// LAS 公共头块解析结果。
/// （data_offset/min/max 保留在结构中：供后续扩展与诊断输出使用）
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct LasHeader {
    /// 点数（1.4 大字段优先，缺省回退 legacy 字段）。
    pub point_count: u64,
    pub point_format: u8,
    pub record_length: u16,
    /// 点数据起始偏移。
    pub data_offset: u64,
    /// XYZ 坐标缩放因子。
    pub scale: [f64; 3],
    /// XYZ 坐标偏移量。
    pub offset: [f64; 3],
    /// 头块声明的包围盒（min/max）；可能不可信，调用方应实测。
    pub min: [f64; 3],
    pub max: [f64; 3],
}

impl LasHeader {
    /// 点记录是否含 RGB。
    pub fn has_color(&self) -> bool {
        matches!(self.point_format, 2 | 3 | 5 | 7 | 8)
    }
}

/// 单点解析结果。
#[derive(Debug, Clone, Default)]
pub struct LasPoint {
    /// 缩放后的真实坐标。
    pub xyz: [f64; 3],
    /// 8bit RGB（点格式无颜色时为 None）。
    pub rgb: Option<[u8; 3]>,
}

/// 打开并解析 LAS 头；同时扫描 VLR 检测 laszip 压缩。
pub fn open(path: &Path) -> Result<(BufReader<File>, LasHeader), String> {
    let file = File::open(path).map_err(|e| format!("无法读取点云源 {}: {e}", path.display()))?;
    let mut reader = BufReader::new(file);

    let mut head = [0u8; 375];
    let n = reader
        .read(&mut head)
        .map_err(|e| format!("读取 LAS 头失败：{e}"))?;
    if n < 227 || &head[0..4] != b"LASF" {
        return Err(format!(
            "{} 不是合法 LAS 文件（LASF 签名缺失或头块过短）",
            path.display()
        ));
    }
    let version = (head[24], head[25]);
    if !(1..=4).contains(&version.0) || version.1 > 4 {
        return Err(format!("不支持的 LAS 版本 {}.{}", version.0, version.1));
    }
    let header_size = u16::from_le_bytes([head[94], head[95]]) as u64;
    let data_offset = u32::from_le_bytes(head[96..100].try_into().unwrap()) as u64;
    let point_format = head[104];
    let record_length = u16::from_le_bytes([head[105], head[106]]);
    let legacy_count = u32::from_le_bytes(head[107..111].try_into().unwrap()) as u64;
    let point_count = if version >= (1, 4) && head.len() >= 251 {
        let large = u64::from_le_bytes(head[247..255].try_into().unwrap());
        if large > 0 {
            large
        } else {
            legacy_count
        }
    } else {
        legacy_count
    };
    if !matches!(point_format, 0 | 1 | 2 | 3 | 5 | 6 | 7 | 8) {
        return Err(format!(
            "不支持的点格式 {}（支持 0/1/2/3/5/6/7/8）",
            point_format
        ));
    }
    if record_length < 20 {
        return Err(format!("非法点记录长度 {record_length}（< 20 字节）"));
    }
    let rd_f64 = |o: usize| f64::from_le_bytes(head[o..o + 8].try_into().unwrap());
    let scale = [rd_f64(131), rd_f64(139), rd_f64(147)];
    let offset = [rd_f64(155), rd_f64(163), rd_f64(171)];
    let max = [rd_f64(179), rd_f64(187), rd_f64(195)];
    let min = [rd_f64(203), rd_f64(211), rd_f64(219)];
    if scale.iter().any(|&s| s == 0.0 || !s.is_finite()) {
        return Err("LAS 头 scale 因子非法（0 或非有限值）".into());
    }

    // ---- LAZ 检测：扫描 VLR user_id ----
    if data_offset > header_size {
        let mut vpos = header_size; // VLR 区紧跟公共头块
        let mut remaining_vlrs = u32::from_le_bytes(head[100..104].try_into().unwrap());
        while remaining_vlrs > 0 && vpos + 54 <= data_offset {
            reader
                .seek(SeekFrom::Start(vpos))
                .map_err(|e| format!("VLR 定位失败：{e}"))?;
            let mut vh = [0u8; 54];
            reader.read_exact(&mut vh).map_err(|e| format!("VLR 读取失败：{e}"))?;
            let user_id = String::from_utf8_lossy(&vh[2..18])
                .trim_end_matches('\0')
                .trim()
                .to_ascii_lowercase();
            let payload = u16::from_le_bytes([vh[20], vh[21]]) as u64;
            if user_id == "laszip" {
                return Err(
                    "暂不支持 LAZ（laszip 压缩）点云，请先解压为标准 LAS 再重试".into(),
                );
            }
            vpos += 54 + payload;
            remaining_vlrs -= 1;
        }
    }
    reader
        .seek(SeekFrom::Start(data_offset))
        .map_err(|e| format!("点数据定位失败：{e}"))?;

    let header = LasHeader {
        point_count,
        point_format,
        record_length,
        data_offset,
        scale,
        offset,
        min,
        max,
    };
    Ok((reader, header))
}

/// RGB 字段在点记录内的偏移（无颜色格式返回 None）。
fn color_offset(point_format: u8) -> Option<usize> {
    match point_format {
        2 => Some(20),
        3 | 5 => Some(28),
        7 | 8 => Some(30),
        _ => None,
    }
}

/// 流式逐点读取。返回 Ok(false) 表示读完。
pub fn next_point(
    reader: &mut BufReader<File>,
    header: &LasHeader,
    record_buf: &mut Vec<u8>,
    out: &mut LasPoint,
) -> Result<bool, String> {
    use std::io::Read;
    match reader.read_exact(record_buf) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => return Ok(false),
        Err(e) => return Err(format!("点记录读取失败：{e}")),
    }
    let xyz = [
        i32::from_le_bytes(record_buf[0..4].try_into().unwrap()) as f64 * header.scale[0]
            + header.offset[0],
        i32::from_le_bytes(record_buf[4..8].try_into().unwrap()) as f64 * header.scale[1]
            + header.offset[1],
        i32::from_le_bytes(record_buf[8..12].try_into().unwrap()) as f64 * header.scale[2]
            + header.offset[2],
    ];
    out.xyz = xyz;
    out.rgb = color_offset(header.point_format).map(|o| {
        let rd_u16 = |p: usize| u16::from_le_bytes(record_buf[p..p + 2].try_into().unwrap());
        // 16bit 颜色按高位对齐缩到 8bit（LAS 规范 0..=65535 全幅）
        [(rd_u16(o) >> 8) as u8, (rd_u16(o + 2) >> 8) as u8, (rd_u16(o + 4) >> 8) as u8]
    });
    Ok(true)
}

/// 测试 fixture：构造最小 LAS 1.2（fmt 3，含 RGB）字节：n 个点。
#[cfg(test)]
pub fn las_fixture_bytes(n: usize) -> Vec<u8> {
    let mut b = Vec::new();
    b.extend_from_slice(b"LASF");
    b.extend_from_slice(&0u16.to_le_bytes()); // source id
    b.extend_from_slice(&0u16.to_le_bytes()); // global encoding
    b.extend_from_slice(&[0u8; 16]); // GUID
    b.push(1);
    b.push(2); // 1.2
    b.extend_from_slice(&[0u8; 32]); // system id
    b.extend_from_slice(&[0u8; 32]); // software
    b.extend_from_slice(&0u16.to_le_bytes()); // day
    b.extend_from_slice(&2026u16.to_le_bytes()); // year
    b.extend_from_slice(&227u16.to_le_bytes()); // header size
    let data_offset = 227u32;
    b.extend_from_slice(&data_offset.to_le_bytes());
    b.extend_from_slice(&0u32.to_le_bytes()); // VLR count
    b.push(3); // point format
    b.extend_from_slice(&34u16.to_le_bytes()); // record length
    b.extend_from_slice(&(n as u32).to_le_bytes()); // legacy count
    b.extend_from_slice(&[0u8; 20]); // by return
    // scale / offset
    for s in [0.001f64; 3] {
        b.extend_from_slice(&s.to_le_bytes());
    }
    for o in [400000.0f64, 3000000.0, 100.0] {
        b.extend_from_slice(&o.to_le_bytes());
    }
    // max/min（写对称范围）
    let m = 10.0f64;
    for v in [m, m, m] {
        b.extend_from_slice(&v.to_le_bytes());
    }
    for v in [-m, -m, -m] {
        b.extend_from_slice(&v.to_le_bytes());
    }
    assert_eq!(b.len(), 227);
    // points: x=i mm→*0.001, y=2i, z=3i；RGB 16bit（高位 8bit 即期望色）
    for i in 0..n {
        let p = |k: i32| (i as i32 * k).to_le_bytes();
        b.extend_from_slice(&p(1));
        b.extend_from_slice(&p(2));
        b.extend_from_slice(&p(3));
        b.extend_from_slice(&100u16.to_le_bytes()); // intensity
        b.extend_from_slice(&[0u8; 6]); // attributes
        b.extend_from_slice(&0f64.to_le_bytes()); // gps time
        for c in [(i % 256) as u16 * 257, 0x4040, 0x8080] {
            b.extend_from_slice(&c.to_le_bytes());
        }
    }
    b
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn header_parse_and_stream_read() {
        let dir = std::env::temp_dir().join(format!("tangis-las-hdr-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("f.las");
        std::fs::write(&path, las_fixture_bytes(7)).unwrap();

        let (mut reader, header) = open(&path).unwrap();
        assert_eq!(header.point_count, 7);
        assert_eq!(header.point_format, 3);
        assert_eq!(header.record_length, 34);
        assert!(header.has_color());

        let mut rec = vec![0u8; header.record_length as usize];
        let mut pt = LasPoint::default();
        let mut count = 0;
        let mut first: Option<LasPoint> = None;
        while next_point(&mut reader, &header, &mut rec, &mut pt).unwrap() {
            if first.is_none() {
                first = Some(pt.clone());
            }
            count += 1;
        }
        assert_eq!(count, 7);
        let f = first.unwrap();
        // 点 0：x=0, y=0, z=0 → scale+offset = (400000, 3000000, 100)
        assert!((f.xyz[0] - 400000.0).abs() < 1e-6);
        assert!((f.xyz[1] - 3000000.0).abs() < 1e-6);
        assert!((f.xyz[2] - 100.0).abs() < 1e-6);
        assert_eq!(f.rgb, Some([0, 64, 128]));

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn rejects_garbage_and_laz() {
        let dir = std::env::temp_dir().join(format!("tangis-las-bad-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();

        let p = dir.join("garbage.las");
        std::fs::write(&p, b"NOPE").unwrap();
        assert!(open(&p).unwrap_err().contains("LASF"));

        // laszip VLR：user_id "laszip encoded"
        let mut b = Vec::new();
        b.extend_from_slice(&las_fixture_bytes(0)[..227]);
        let mut vlr = vec![0u8; 54];
        vlr[2..8].copy_from_slice(b"laszip");
        let payload = 100u16;
        vlr[20..22].copy_from_slice(&payload.to_le_bytes());
        b.extend_from_slice(&vlr);
        b.extend_from_slice(&vec![0u8; payload as usize]);
        // 修补 header_size 之后的字段：data_offset（227+54+100 = 381）与 VLR 数
        b[96..100].copy_from_slice(&381u32.to_le_bytes());
        b[100..104].copy_from_slice(&1u32.to_le_bytes());
        let p = dir.join("laz.las");
        std::fs::write(&p, &b).unwrap();
        let err = open(&p).unwrap_err();
        assert!(err.contains("LAZ"), "{err}");

        let _ = std::fs::remove_dir_all(&dir);
    }
}
