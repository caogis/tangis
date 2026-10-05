//! OSGB 文件头解析（OSG 3.x 真实二进制序列化头）。
//!
//! 头部布局（小端，逐字节逆向自真实 osgconv 3.6.5 语料
//! `testdata/osgb-real/osgb/*.osgb`，并与 OSG 源码
//! `OutputStream::start` / `InputStream::readHeader` 对照确认）：
//!
//! ```text
//! 偏移   类型   字段
//! 0      u32    magic      = 0x6c910ea1（OSG_HEADER_LOW 的小端存储）
//! 4      u32    magic2     = 0x1afb4545（OSG_HEADER_HIGH，与 magic 组成 64 位魔数）
//! 8      u32    write_type = 1（WRITE_SCENE；2=WRITE_OBJECT 4=WRITE_IMAGE）
//! 12     u32    version    = 161（OSG 3.6.x 序列化版本，SOVERSION）
//! 16     u32    attributes 属性位：bit0=自定义 domain 版本、bit1=schema 数据、
//!                          bit2=robust 二进制括号（blocksize 可靠跳过）
//! 20     string compressorName（[u32 长度][字节]，"0" = 无压缩；
//!                          否则为压缩器名如 "zlib"，头之后为 zlib deflate 流）
//! ```

use crate::error::{OsgbError, Result};
use crate::reader::Reader;

/// OSG 魔数低 32 位（小端读出值）。
pub const OSG_MAGIC_LOW: u32 = 0x6c91_0ea1;
/// OSG 魔数高 32 位（小端读出值）。
pub const OSG_MAGIC_HIGH: u32 = 0x1afb_4545;

/// OSGB 文件头。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Header {
    pub magic: u32,
    /// 魔数高 32 位（历史版本语料中曾误标为 feature_mask）。
    pub magic2: u32,
    /// 写出类型（1 = WRITE_SCENE）。
    pub write_type: u32,
    /// 序列化版本号（OSG 3.6.5 语料为 161）。
    pub version: u32,
    /// 属性位（bit2 = robust 二进制括号）。
    pub attributes: u32,
    /// 压缩器名（"0" = 无压缩）。
    pub compressor: String,
}

impl Header {
    /// 流是否为压缩格式（compressorName 非 "0"）。
    pub fn is_compressed(&self) -> bool {
        self.compressor != "0"
    }

    /// 属性位：使用 robust 二进制括号（对象帧带 u64 blocksize，可整体跳过）。
    pub fn has_binary_brackets(&self) -> bool {
        self.attributes & 0x4 != 0
    }

    /// 从流中解析头部（20 字节固定头 + compressorName 字符串）。
    pub fn parse(r: &mut Reader) -> Result<Header> {
        let at = r.pos();
        let header = Header {
            magic: r.u32()?,
            magic2: r.u32()?,
            write_type: r.u32()?,
            version: r.u32()?,
            attributes: r.u32()?,
            compressor: r.string_auto()?,
        };
        if header.magic != OSG_MAGIC_LOW || header.magic2 != OSG_MAGIC_HIGH {
            return Err(OsgbError::BadMagic {
                actual: header.magic,
            });
        }
        if !header.has_binary_brackets() {
            // 无 blocksize 括号的旧格式无法安全跳过未知类，明确拒绝而非瞎猜
            return Err(OsgbError::BadField {
                class: "<header>".into(),
                offset: at + 16,
                message: format!(
                    "attributes=0x{:x} 未置 robust 二进制括号位（bit2），本读取器不支持",
                    header.attributes
                ),
            });
        }
        Ok(header)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 真实 osgconv 3.6.5 语料头部前 29 字节（hexdump 逐字复制）。
    const REAL_HEAD: &[u8] = &[
        0xa1, 0x0e, 0x91, 0x6c, // magic = 0x6c910ea1
        0x45, 0x45, 0xfb, 0x1a, // magic2 = 0x1afb4545
        0x01, 0x00, 0x00, 0x00, // write_type = 1（WRITE_SCENE）
        0xa1, 0x00, 0x00, 0x00, // version = 161
        0x04, 0x00, 0x00, 0x00, // attributes = 4（bit2 robust brackets）
        0x01, 0x00, 0x00, 0x00, b'0', // compressorName = "0"（无压缩）
    ];

    #[test]
    fn parses_real_osgconv_header() {
        let mut r = Reader::new(REAL_HEAD);
        let h = Header::parse(&mut r).unwrap();
        assert_eq!(h.magic, OSG_MAGIC_LOW);
        assert_eq!(h.magic2, OSG_MAGIC_HIGH);
        assert_eq!(h.write_type, 1);
        assert_eq!(h.version, 161);
        assert_eq!(h.attributes, 4);
        assert_eq!(h.compressor, "0");
        assert_eq!(r.pos(), 25);
        assert!(!h.is_compressed());
        assert!(h.has_binary_brackets());
    }

    #[test]
    fn compressor_name_marks_compression() {
        let mut bytes = REAL_HEAD.to_vec();
        // compressorName = "zlib"（长度 4 + 4 字节）
        bytes.truncate(20);
        bytes.extend_from_slice(&4u32.to_le_bytes());
        bytes.extend_from_slice(b"zlib");
        let mut r = Reader::new(&bytes);
        let h = Header::parse(&mut r).unwrap();
        assert!(h.is_compressed());
    }

    #[test]
    fn rejects_bad_magic() {
        let mut bytes = REAL_HEAD.to_vec();
        bytes[0] = 0x00; // 破坏魔数
        let mut r = Reader::new(&bytes);
        let err = Header::parse(&mut r).unwrap_err();
        assert!(matches!(err, OsgbError::BadMagic { .. }));
    }

    #[test]
    fn rejects_non_robust_attributes() {
        let mut bytes = REAL_HEAD.to_vec();
        bytes[16] = 0x00; // 清掉 bit2（robust brackets）
        let mut r = Reader::new(&bytes);
        let err = Header::parse(&mut r).unwrap_err();
        assert!(err.to_string().contains("robust"), "{err}");
    }

    #[test]
    fn truncated_header_errors() {
        for cut in [0usize, 4, 19, 20, 23] {
            let mut r = Reader::new(&REAL_HEAD[..cut]);
            assert!(Header::parse(&mut r).is_err(), "截断到 {cut} 字节应报错");
        }
    }
}
