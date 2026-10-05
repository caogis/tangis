//! 增量写入日志（journal）——manifest 回写 O(n²) 修复（M2-PERF）。
//!
//! 旧行为：每完成一个分块就把**整份** manifest 重写落盘（断点检查点），
//! 总写入量 O(n²)（n = 分块数，数万块规模成为可测量瓶颈）。
//!
//! 新行为（本模块）：
//! - 每完成一个分块，向 `<manifest>.journal` **追加一行**
//!   [`ChunkOutcome`]（JSONL，带 flush，崩溃安全与旧逐块落盘同级）；
//! - 全部完成后 [`Journal::finalize`] 把最终 manifest **整份重写一次**，
//!   随后删除 journal——manifest 文件内容与旧实现逐字节一致
//!   （`to_string_pretty` + 尾随换行）；
//! - 断点续切：启动时 [`replay`] 把 journal 记录回放到内存 manifest
//!   （Done 跳过语义不变），崩溃后重跑等价于旧实现的中断恢复；
//! - 旧格式兼容：journal 是可选边车文件，不存在时 [`replay`] 返回 0，
//!   老的完整 manifest 文件照常读取。

use std::fs::{self, File, OpenOptions};
use std::io::{BufRead, BufReader, BufWriter, Write};
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::{error::ManifestError, Bounds, ChunkId, ChunkManifest, ChunkStatus};

/// 单个分块的完成记录（journal 的一行）。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ChunkOutcome {
    pub id: ChunkId,
    /// 完成后的累计尝试次数（回放时覆盖，取与现有值的最大者兜底）。
    pub attempts: u32,
    /// 完成后的真实包围盒（build 写；`run` 等不改 bounds 的场景省略）。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub bounds: Option<Bounds>,
}

/// 给定 manifest 路径，返回配套 journal 路径（`<manifest>.journal`）。
pub fn journal_path_for(manifest_path: &Path) -> PathBuf {
    let mut os = manifest_path.as_os_str().to_os_string();
    os.push(".journal");
    PathBuf::from(os)
}

/// 增量写入日志句柄：`create` → 循环 [`Journal::record`] → [`Journal::finalize`]。
pub struct Journal {
    path: PathBuf,
    writer: BufWriter<File>,
}

impl Journal {
    /// 创建（或截断重建）journal 文件。
    pub fn create(path: PathBuf) -> Result<Self, ManifestError> {
        let file = OpenOptions::new().create(true).truncate(true).write(true).open(&path)?;
        Ok(Self { path, writer: BufWriter::new(file) })
    }

    /// 追加一条完成记录并 flush（崩溃安全：落盘粒度 = 单分块，与旧实现同级）。
    pub fn record(&mut self, outcome: &ChunkOutcome) -> Result<(), ManifestError> {
        let mut line = serde_json::to_string(outcome).map_err(|source| ManifestError::Json {
            line: 0,
            source,
        })?;
        line.push('\n');
        self.writer.write_all(line.as_bytes())?;
        self.writer.flush()?;
        Ok(())
    }

    /// 收尾：把最终 manifest 整份重写**一次**（与旧逐块全量写的最终内容
    /// 逐字节一致），成功后删除 journal。任一步失败时 journal 保留
    /// （断点续切仍可恢复）。
    pub fn finalize(
        mut self,
        manifest: &ChunkManifest,
        manifest_path: &Path,
    ) -> Result<(), ManifestError> {
        // 先冲刷 journal 再写 manifest：保证 manifest 落盘时全部记录已持久化
        self.writer.flush()?;
        drop(self.writer);
        let json = serde_json::to_string_pretty(manifest)
            .map_err(|source| ManifestError::Json { line: 0, source })?;
        fs::write(manifest_path, json + "\n")?;
        fs::remove_file(&self.path)?;
        Ok(())
    }
}

/// 把 journal 回放到内存 manifest：逐行应用 [`ChunkOutcome`]。
///
/// - journal 文件不存在（旧格式 / 首次运行）→ `Ok(0)`；
/// - 返回应用的记录数（含对已 Done 分块的重复应用）；
/// - journal 引用未知分块 → [`ManifestError::JournalUnknownChunk`]
///   （journal 与 manifest 不匹配，明确报错而非静默忽略）。
pub fn replay(
    manifest: &mut ChunkManifest,
    journal_path: &Path,
) -> Result<usize, ManifestError> {
    let file = match File::open(journal_path) {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(0),
        Err(e) => return Err(e.into()),
    };
    let mut applied = 0usize;
    for (i, line) in BufReader::new(file).lines().enumerate() {
        let line = line?;
        if line.trim().is_empty() {
            continue;
        }
        let outcome: ChunkOutcome = serde_json::from_str(&line).map_err(|source| {
            ManifestError::Json { line: i + 1, source }
        })?;
        let chunk = manifest
            .chunks
            .iter_mut()
            .find(|c| c.id == outcome.id)
            .ok_or_else(|| ManifestError::JournalUnknownChunk(outcome.id.0.clone()))?;
        chunk.status = ChunkStatus::Done;
        chunk.attempts = chunk.attempts.max(outcome.attempts);
        if let Some(b) = outcome.bounds {
            chunk.bounds = b;
        }
        applied += 1;
    }
    Ok(applied)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Chunk;

    fn bounds(v: f64) -> Bounds {
        Bounds { min: [v, v, 0.0], max: [v + 1.0, v + 1.0, 1.0] }
    }

    fn chunk(id: &str, lod: u32) -> Chunk {
        Chunk {
            id: ChunkId::new(id),
            lod,
            bounds: bounds(0.0),
            status: ChunkStatus::Pending,
            attempts: 0,
            children: Vec::new(),
            source: None,
            source_offset: None,
        }
    }

    fn manifest() -> ChunkManifest {
        ChunkManifest {
            task_id: "t".into(),
            chunks: vec![chunk("a", 0), chunk("b", 1), chunk("c", 1)],
            source: None,
        }
    }

    fn tmp(name: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!(
            "tangis-journal-test-{}-{name}",
            std::process::id()
        ));
        let _ = fs::remove_dir_all(&d);
        fs::create_dir_all(&d).unwrap();
        d
    }

    #[test]
    fn journal_roundtrip_and_finalize_writes_full_manifest_once() {
        let dir = tmp("roundtrip");
        let manifest_path = dir.join("manifest.json");
        // 初始 manifest 落盘（pending 状态）
        let mut m = manifest();
        fs::write(
            &manifest_path,
            serde_json::to_string_pretty(&m).unwrap() + "\n",
        )
        .unwrap();

        let jpath = journal_path_for(&manifest_path);
        assert_eq!(jpath.to_str().unwrap(), format!("{}/manifest.json.journal", dir.display()));

        let mut j = Journal::create(jpath.clone()).unwrap();
        j.record(&ChunkOutcome {
            id: ChunkId::new("b"),
            attempts: 1,
            bounds: Some(bounds(5.0)),
        })
        .unwrap();
        j.record(&ChunkOutcome { id: ChunkId::new("c"), attempts: 1, bounds: None }).unwrap();

        // journal 存在时 manifest 本体尚未更新（增量语义：只在 finalize 重写）
        assert!(jpath.is_file());

        // 断点续切回放：b/c → Done，b 带真实 bounds
        let mut resumed = manifest();
        assert_eq!(replay(&mut resumed, &jpath).unwrap(), 2);
        assert_eq!(resumed.chunks[1].status, ChunkStatus::Done);
        assert_eq!(resumed.chunks[1].bounds, bounds(5.0));
        assert_eq!(resumed.chunks[2].status, ChunkStatus::Done);
        assert_eq!(resumed.chunks[0].status, ChunkStatus::Pending);

        // finalize：manifest 整份重写一次，journal 删除
        m.chunks[1].status = ChunkStatus::Done;
        m.chunks[1].attempts = 1;
        m.chunks[1].bounds = bounds(5.0);
        m.chunks[2].status = ChunkStatus::Done;
        m.chunks[2].attempts = 1;
        j.finalize(&m, &manifest_path).unwrap();
        assert!(!jpath.exists(), "finalize 后 journal 应删除");

        let on_disk = fs::read_to_string(&manifest_path).unwrap();
        // 与旧 save_manifest 逐字节一致：to_string_pretty + 尾随换行
        assert_eq!(on_disk, serde_json::to_string_pretty(&m).unwrap() + "\n");
        let back: ChunkManifest = serde_json::from_str(&on_disk).unwrap();
        assert_eq!(back, m);

        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn replay_missing_journal_is_zero_and_old_manifest_still_readable() {
        let dir = tmp("compat");
        let manifest_path = dir.join("manifest.json");
        // 旧格式 manifest（无 journal 边车）照常读取 + replay 返回 0
        fs::write(
            &manifest_path,
            r#"{"task_id":"old","chunks":[{"id":"a","lod":0,"bounds":{"min":[0,0,0],"max":[1,1,1]},"status":"pending","attempts":0}]}"#,
        )
        .unwrap();
        let mut m: ChunkManifest =
            serde_json::from_str(&fs::read_to_string(&manifest_path).unwrap()).unwrap();
        assert_eq!(replay(&mut m, &journal_path_for(&manifest_path)).unwrap(), 0);
        assert_eq!(m.chunks[0].status, ChunkStatus::Pending);
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn resume_after_partial_journal_is_idempotent() {
        // 场景：崩溃后重跑——journal 里已有 b 的记录，回放后 b 跳过
        let dir = tmp("resume");
        let jpath = dir.join("manifest.json.journal");
        fs::write(
            &jpath,
            r#"{"id":"b","attempts":2,"bounds":{"min":[1,2,3],"max":[4,5,6]}}"#,
        )
        .unwrap();
        let mut m = manifest();
        assert_eq!(replay(&mut m, &jpath).unwrap(), 1);
        assert_eq!(m.chunks[1].status, ChunkStatus::Done);
        assert_eq!(m.chunks[1].attempts, 2);
        assert_eq!(m.chunks[1].bounds.min, [1.0, 2.0, 3.0]);

        // 重放同一条记录幂等（重复应用结果不变）
        assert_eq!(replay(&mut m, &jpath).unwrap(), 1);
        assert_eq!(m.chunks[1].attempts, 2);
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn replay_rejects_unknown_chunk() {
        let dir = tmp("unknown");
        let jpath = dir.join("manifest.json.journal");
        fs::write(&jpath, r#"{"id":"nope","attempts":1}"#).unwrap();
        let mut m = manifest();
        let err = replay(&mut m, &jpath).unwrap_err();
        assert!(err.to_string().contains("unknown chunk `nope`"), "{err}");
        fs::remove_dir_all(&dir).unwrap();
    }
}
