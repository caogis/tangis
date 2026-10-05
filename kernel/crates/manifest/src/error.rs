use thiserror::Error;

/// Manifest / 拓扑排序相关错误。
#[derive(Debug, Error)]
pub enum ManifestError {
    /// 拓扑排序检测到环：LOD 依赖无法满足（含自依赖）。
    #[error("cycle detected in LOD dependency graph (involved chunks: {chunks})")]
    CycleDetected { chunks: String },

    /// `children` 引用了清单中不存在的分块。
    #[error("chunk `{chunk}` references unknown child `{child}`")]
    UnknownChild { chunk: String, child: String },

    /// journal 记录引用了清单中不存在的分块（journal 与 manifest 不匹配）。
    #[error("journal records unknown chunk `{0}` (journal does not match this manifest)")]
    JournalUnknownChunk(String),

    /// 文件 I/O 错误（journal 追加 / manifest 重写）。
    #[error(transparent)]
    Io(#[from] std::io::Error),

    /// journal 行 JSON 解析失败。
    #[error("journal line {line} parse failed: {source}")]
    Json {
        line: usize,
        #[source]
        source: serde_json::Error,
    },
}
