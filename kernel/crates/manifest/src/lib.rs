//! TanGIS 切片内核：分块 Manifest（ADR-2：分块 Manifest + 拓扑排序 + 调度亲和性 + 断点续切）。
//!
//! 核心语义：
//! - 一个切片任务 = 一个 [`ChunkManifest`]，由若干分块（[`Chunk`]）组成；
//! - 分块按 LOD 组织，父 tile 依赖其子 tile 完成（LOD 自底向上聚合）；
//! - [`topo_sort`] 输出可安全执行的分块顺序，环依赖时报错；
//! - 每个分块带 [`ChunkStatus`] 与 `attempts`，支撑断点续切（Done 的分块跳过）。

pub mod error;
pub mod journal;
pub mod topo;

pub use error::ManifestError;
pub use journal::{journal_path_for, replay, ChunkOutcome, Journal};
pub use topo::topo_sort;

use serde::{Deserialize, Serialize};

/// 分块唯一标识（newtype，防止与其他 id 混用）。
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(transparent)]
pub struct ChunkId(pub String);

impl ChunkId {
    pub fn new(id: impl Into<String>) -> Self {
        Self(id.into())
    }
}

impl std::fmt::Display for ChunkId {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

/// 分块空间包围盒（axis-aligned，单位与坐标系由上层任务定义）。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Bounds {
    pub min: [f64; 3],
    pub max: [f64; 3],
}

/// 分块执行状态，支撑断点续切：`Done` 的分块在恢复时直接跳过。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChunkStatus {
    Pending,
    Running,
    Done,
    Failed,
}

/// 切片任务中的一个分块。
///
/// `lod` 为层级号（数值越小层级越高/越粗），`children` 为其子分块
/// （更细层级的 tile）。父 tile 的聚合需在全部子 tile 完成后进行。
///
/// `source` / `source_offset` 为可选的真实几何来源（M1 扩展，Go 侧生成
/// manifest 时可省略，向后兼容）：
/// - `source`：几何源文件路径（`.osgb` / `.obj`，相对 manifest 文件目录，
///   或绝对路径），build 时解析其中的真实三角形写入 b3dm；无法解析出
///   真实几何时 build **报错**（内核禁止产出占位几何）；
/// - `source_offset`：[f64; 3]，把源局部坐标平移到分块位置。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Chunk {
    pub id: ChunkId,
    pub lod: u32,
    pub bounds: Bounds,
    pub status: ChunkStatus,
    pub attempts: u32,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub children: Vec<ChunkId>,
    /// 真实几何来源：`.osgb` / `.obj` 文件路径（可选）。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source: Option<String>,
    /// 源局部坐标 → 分块位置的平移量（可选）。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source_offset: Option<[f64; 3]>,
}

impl Chunk {
    /// bounds 是否为占位盒（min=[0,0,0]、max=[1,1,1]）。
    ///
    /// build 遇到占位盒且有真实几何来源时，用真实包围盒覆盖并回写 manifest。
    pub fn has_placeholder_bounds(&self) -> bool {
        self.bounds.min == [0.0, 0.0, 0.0] && self.bounds.max == [1.0, 1.0, 1.0]
    }
}

/// 一个切片任务的分块清单（对应 F-02 / F-04：任务幂等，失败可从分块断点恢复）。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ChunkManifest {
    pub task_id: String,
    pub chunks: Vec<Chunk>,
    /// 几何源目录（可选，向后兼容字段）：build 时从该目录按分块 id 匹配
    /// `<chunk_id>.obj` / `<chunk_id>.osgb` 加载真实几何。分块级 `source`
    /// 字段与 CLI `--src` 参数优先级更高。缺省且无法解析出真实几何时
    /// build 报错（内核禁止产出占位几何）。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source: Option<String>,
}

impl ChunkManifest {
    /// 按 id 查找分块。
    pub fn chunk(&self, id: &ChunkId) -> Option<&Chunk> {
        self.chunks.iter().find(|c| &c.id == id)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn bounds(v: f64) -> Bounds {
        Bounds {
            min: [v, v, 0.0],
            max: [v + 1.0, v + 1.0, 1.0],
        }
    }

    fn chunk(id: &str, lod: u32, children: &[&str]) -> Chunk {
        Chunk {
            id: ChunkId::new(id),
            lod,
            bounds: bounds(0.0),
            status: ChunkStatus::Pending,
            attempts: 0,
            children: children.iter().map(|c| ChunkId::new(*c)).collect(),
            source: None,
            source_offset: None,
        }
    }

    #[test]
    fn serde_roundtrip() {
        let manifest = ChunkManifest {
            task_id: "task-001".into(),
            chunks: vec![
                chunk("root", 0, &["c1", "c2"]),
                chunk("c1", 1, &[]),
                chunk("c2", 1, &[]),
            ],
            source: None,
        };
        let json = serde_json::to_string_pretty(&manifest).unwrap();
        let back: ChunkManifest = serde_json::from_str(&json).unwrap();
        assert_eq!(back, manifest);
    }

    #[test]
    fn chunk_id_is_transparent_newtype() {
        let json = serde_json::to_string(&ChunkId::new("abc")).unwrap();
        assert_eq!(json, "\"abc\"");
        let id: ChunkId = serde_json::from_str("\"xyz\"").unwrap();
        assert_eq!(id, ChunkId::new("xyz"));
    }

    #[test]
    fn status_snake_case() {
        assert_eq!(
            serde_json::to_string(&ChunkStatus::Done).unwrap(),
            "\"done\""
        );
        let s: ChunkStatus = serde_json::from_str("\"failed\"").unwrap();
        assert_eq!(s, ChunkStatus::Failed);
    }

    #[test]
    fn source_fields_are_optional_and_backward_compatible() {
        // 旧 manifest（无 source/source_offset）必须能反序列化
        let old = r#"{
            "task_id": "t",
            "chunks": [{
                "id": "c1", "lod": 0,
                "bounds": {"min": [0,0,0], "max": [1,1,1]},
                "status": "pending", "attempts": 0
            }]
        }"#;
        let m: ChunkManifest = serde_json::from_str(old).unwrap();
        assert_eq!(m.chunks[0].source, None);
        assert_eq!(m.chunks[0].source_offset, None);
        assert!(m.chunks[0].has_placeholder_bounds());

        // 带来源的 chunk 序列化/反序列化往返，且省略 None 字段
        let with_src: ChunkManifest = serde_json::from_str(
            r#"{
            "task_id": "t",
            "chunks": [{
                "id": "c1", "lod": 0,
                "bounds": {"min": [0,0,0], "max": [1,1,1]},
                "status": "pending", "attempts": 0,
                "source": "tiles/a.osgb", "source_offset": [10.0, 0.0, 5.0]
            }]
        }"#,
        )
        .unwrap();
        assert_eq!(with_src.chunks[0].source.as_deref(), Some("tiles/a.osgb"));
        assert_eq!(with_src.chunks[0].source_offset, Some([10.0, 0.0, 5.0]));
        let json = serde_json::to_string(&with_src.chunks[0]).unwrap();
        assert!(json.contains("\"source\":\"tiles/a.osgb\""));
        // 无来源时字段不出现（向后兼容输出）
        let json = serde_json::to_string(&m.chunks[0]).unwrap();
        assert!(!json.contains("source"));
    }
}
