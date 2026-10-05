//! 面向字节切片的小端二进制游标 + OSG compact int 编解码。
//!
//! OSG 3.x 序列化的基础编码之一是 compact int（变长整数）：
//! 每字节低 7 位是数据、最高位是继续位（置 1 表示还有后续字节），
//! 一个 u32 最多占 5 字节（LEB128 变体）。
//! 注意：本仓库的测试语料（合成 OSGB）全部使用定宽 u32/u64，
//! compact int 按 OSG 格式规范实现并单测，供读取其他 OSG 流时复用。

use crate::error::{OsgbError, Result};

/// 小端二进制游标。
pub struct Reader<'a> {
    data: &'a [u8],
    pos: usize,
}

impl<'a> Reader<'a> {
    pub fn new(data: &'a [u8]) -> Self {
        Self { data, pos: 0 }
    }

    /// 当前字节偏移（用于错误定位与对象树报告）。
    pub fn pos(&self) -> usize {
        self.pos
    }

    /// 剩余未读字节数。
    pub fn remaining(&self) -> usize {
        self.data.len() - self.pos
    }

    fn take(&mut self, n: usize) -> Result<&'a [u8]> {
        if self.remaining() < n {
            return Err(OsgbError::UnexpectedEof {
                offset: self.pos,
                needed: n,
            });
        }
        let out = &self.data[self.pos..self.pos + n];
        self.pos += n;
        Ok(out)
    }

    pub fn u8(&mut self) -> Result<u8> {
        Ok(self.take(1)?[0])
    }


    pub fn u16(&mut self) -> Result<u16> {
        Ok(u16::from_le_bytes(self.take(2)?.try_into().unwrap()))
    }

    pub fn u32(&mut self) -> Result<u32> {
        Ok(u32::from_le_bytes(self.take(4)?.try_into().unwrap()))
    }

    pub fn u64(&mut self) -> Result<u64> {
        Ok(u64::from_le_bytes(self.take(8)?.try_into().unwrap()))
    }

    pub fn f32(&mut self) -> Result<f32> {
        Ok(f32::from_le_bytes(self.take(4)?.try_into().unwrap()))
    }

    pub fn f64(&mut self) -> Result<f64> {
        Ok(f64::from_le_bytes(self.take(8)?.try_into().unwrap()))
    }

    pub fn i32(&mut self) -> Result<i32> {
        Ok(i32::from_le_bytes(self.take(4)?.try_into().unwrap()))
    }

    /// 读取 OSG 字符串：`[u32 字节长度][UTF-8 字节]`。
    pub fn string_auto(&mut self) -> Result<String> {
        let n = self.u32()? as usize;
        let at = self.pos;
        let bytes = self.take(n)?;
        String::from_utf8(bytes.to_vec()).map_err(|_| OsgbError::BadField {
            class: "<string>".into(),
            offset: at,
            message: format!("字符串不是合法 UTF-8（{n} 字节）"),
        })
    }

    /// 读取 `n` 字节并按 UTF-8 解码（OSG 类名字符串）。
    pub fn string(&mut self, n: usize) -> Result<String> {
        let bytes = self.take(n)?;
        String::from_utf8(bytes.to_vec())
            .map_err(|_| OsgbError::BadField {
                class: "<classname>".into(),
                offset: self.pos,
                message: format!("类名不是合法 UTF-8（{n} 字节）"),
            })
    }

    /// 跳过 `n` 字节。
    pub fn skip(&mut self, n: usize) -> Result<()> {
        self.take(n).map(|_| ())
    }

    /// 读取恰好 `out.len()` 字节填入 `out`（INLINE 图像字节直通用）。
    pub fn take_bytes(&mut self, out: &mut [u8]) -> Result<()> {
        let bytes = self.take(out.len())?;
        out.copy_from_slice(bytes);
        Ok(())
    }

    /// 读取 OSG compact int（变长整数，1..=5 字节，LEB128 变体）。
    pub fn compact_int(&mut self) -> Result<u64> {
        let mut value: u64 = 0;
        let mut shift = 0u32;
        loop {
            let byte = self.u8()?;
            value |= u64::from(byte & 0x7f) << shift;
            if byte & 0x80 == 0 {
                return Ok(value);
            }
            shift += 7;
            if shift >= 35 {
                return Err(OsgbError::BadField {
                    class: "<compact int>".into(),
                    offset: self.pos,
                    message: "compact int 超过 5 字节上限".into(),
                });
            }
        }
    }

    /// 把 `expected` 作为一个整体断言目标，失败时生成带类名与偏移的错误。
    pub fn expect_u32(&mut self, expected: u32, class: &str, what: &str) -> Result<()> {
        let at = self.pos();
        let actual = self.u32()?;
        if actual != expected {
            return Err(OsgbError::BadField {
                class: class.to_string(),
                offset: at,
                message: format!("{what}: 期望 {expected}（0x{expected:08x}），实际 {actual}（0x{actual:08x}）"),
            });
        }
        Ok(())
    }

    pub fn expect_u8(&mut self, expected: u8, class: &str, what: &str) -> Result<()> {
        let at = self.pos();
        let actual = self.u8()?;
        if actual != expected {
            return Err(OsgbError::BadField {
                class: class.to_string(),
                offset: at,
                message: format!("{what}: 期望 {expected}（0x{expected:02x}），实际 {actual}（0x{actual:02x}）"),
            });
        }
        Ok(())
    }

    pub fn expect_u16(&mut self, expected: u16, class: &str, what: &str) -> Result<()> {
        let at = self.pos();
        let actual = self.u16()?;
        if actual != expected {
            return Err(OsgbError::BadField {
                class: class.to_string(),
                offset: at,
                message: format!("{what}: 期望 {expected}（0x{expected:04x}），实际 {actual}（0x{actual:04x}）"),
            });
        }
        Ok(())
    }
}

/// 编码 OSG compact int（LEB128 变体：低 7 位数据 + 最高位继续位）。
///
/// 供单测 roundtrip 与未来写出端复用。
pub fn write_compact_int(mut value: u64, out: &mut Vec<u8>) {
    loop {
        let mut byte = (value & 0x7f) as u8;
        value >>= 7;
        if value != 0 {
            byte |= 0x80;
        }
        out.push(byte);
        if value == 0 {
            return;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compact_int_single_byte_range() {
        // 0..=127 单字节编码
        for v in [0u64, 1, 42, 127] {
            let mut buf = Vec::new();
            write_compact_int(v, &mut buf);
            assert_eq!(buf.len(), 1, "值 {v} 应为单字节");
            assert_eq!(buf[0], v as u8, "单字节值不应置继续位");
            let mut r = Reader::new(&buf);
            assert_eq!(r.compact_int().unwrap(), v);
        }
    }

    #[test]
    fn compact_int_multibyte_boundaries() {
        // (值, 期望编码字节数, 期望 LE 字节序列)
        let cases: &[(u64, usize, &[u8])] = &[
            (128, 2, &[0x80, 0x01]),
            (129, 2, &[0x81, 0x01]),
            (0x3fff, 2, &[0xff, 0x7f]),
            (0x4000, 3, &[0x80, 0x80, 0x01]),
            (0x1f_ffff, 3, &[0xff, 0xff, 0x7f]),
            (0x20_0000, 4, &[0x80, 0x80, 0x80, 0x01]),
            (u32::MAX as u64, 5, &[0xff, 0xff, 0xff, 0xff, 0x0f]),
        ];
        for &(v, len, bytes) in cases {
            let mut buf = Vec::new();
            write_compact_int(v, &mut buf);
            assert_eq!(buf.len(), len, "值 {v:#x} 编码长度错误: {buf:02x?}");
            assert_eq!(buf.as_slice(), bytes, "值 {v:#x} 编码字节错误");
            let mut r = Reader::new(&buf);
            assert_eq!(r.compact_int().unwrap(), v);
        }
    }

    #[test]
    fn compact_int_roundtrip_sweep() {
        // 采样扫描：每个量级边界附近往返一致（上限 5 字节 = 35 位）
        let mut v = 1u64;
        while v <= u32::MAX as u64 {
            for delta in [-1i64, 0, 1] {
                let x = (v as i64 + delta).max(0) as u64;
                let mut buf = Vec::new();
                write_compact_int(x, &mut buf);
                let mut r = Reader::new(&buf);
                assert_eq!(r.compact_int().unwrap(), x);
            }
            v = v.saturating_mul(2);
            if v == 1 {
                break; // 防御：saturating 后不再增长
            }
        }
    }

    #[test]
    fn compact_int_rejects_overlong() {
        // 连续 5 个继续位：超出 u32 compact int 上限应报错
        let bytes = [0xff, 0xff, 0xff, 0xff, 0xff, 0x01];
        let mut r = Reader::new(&bytes);
        assert!(r.compact_int().is_err());
    }

    #[test]
    fn primitive_reads_are_little_endian() {
        let bytes = [
            0x01, 0x02, // u16 = 0x0201
            0x78, 0x56, 0x34, 0x12, // u32 = 0x12345678
            0x00, 0x00, 0x80, 0x3f, // f32 = 1.0
            0x00, 0x00, 0x20, 0xc0, // f32 = -2.5
        ];
        let mut r = Reader::new(&bytes);
        assert_eq!(r.u16().unwrap(), 0x0201);
        assert_eq!(r.u32().unwrap(), 0x1234_5678);
        assert_eq!(r.f32().unwrap(), 1.0);
        assert_eq!(r.f32().unwrap(), -2.5);
    }

    #[test]
    fn read_past_end_reports_offset() {
        let mut r = Reader::new(&[0u8; 3]);
        r.skip(2).unwrap();
        let err = r.u32().unwrap_err();
        match err {
            OsgbError::UnexpectedEof { offset, needed } => {
                assert_eq!(offset, 2);
                assert_eq!(needed, 4);
            }
            other => panic!("期望 UnexpectedEof，实际 {other:?}"),
        }
    }
}
