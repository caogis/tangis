//! osgb 解析错误类型。

use std::path::PathBuf;

#[derive(Debug, thiserror::Error)]
pub enum OsgbError {
    #[error("无法读取 {path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: std::io::Error,
    },

    #[error("OSGB 数据在第 {offset} 字节处意外结束（还需要 {needed} 字节）")]
    UnexpectedEof { offset: usize, needed: usize },

    #[error("OSGB 魔数不合法：期望 0x6c910ea1，实际 0x{actual:08x}")]
    BadMagic { actual: u32 },

    #[error("OSGB 流声明 zlib 压缩（attributes=0x{attributes:08x}），但解压失败：{message}")]
    Compressed { attributes: u32, message: String },

    #[error("在第 {offset} 字节遇到未知对象类 `{class}` 且无法跳过（体大小 {body_size} 字节越界）")]
    UnknownClassTruncated {
        offset: usize,
        class: String,
        body_size: u64,
    },

    #[error("解析 {class} 时在第 {offset} 字节处字段不匹配：{message}")]
    BadField {
        class: String,
        offset: usize,
        message: String,
    },

    #[error("对象 `{class}`（起始 {offset}）解析结束时未对齐：读到 {at}，对象体应结束于 {expected}")]
    BodyOverrun {
        class: String,
        offset: usize,
        at: usize,
        expected: usize,
    },
}

pub type Result<T> = std::result::Result<T, OsgbError>;
