//! LOD 拓扑排序（ADR-2）：父 tile 依赖全部子 tile 完成，输出自底向上的执行顺序。
//!
//! 采用 Kahn 算法：把「父 → 子」依赖建模为「子 → 父」的有向边，
//! 入度为 0 的分块（叶子，最细层级）先执行；处理不完所有节点即存在环。

use std::collections::{BTreeMap, BTreeSet, VecDeque};

use crate::error::ManifestError;
use crate::{ChunkId, ChunkManifest};

/// 对清单中的分块做拓扑排序，返回自底向上（子先于父）的 [`ChunkId`] 序列。
///
/// 约束：
/// - 子分块必须先于其父分块执行（父 tile 聚合依赖子 tile 产物）；
/// - 同层无依赖的分块按 `ChunkId` 字典序输出，保证顺序确定性（利于调度亲和性续跑）。
///
/// # Errors
/// - [`ManifestError::UnknownChild`]：`children` 引用了不存在的分块；
/// - [`ManifestError::CycleDetected`]：依赖图存在环（含自依赖）。
pub fn topo_sort(manifest: &ChunkManifest) -> Result<Vec<ChunkId>, ManifestError> {
    let ids: BTreeSet<&ChunkId> = manifest.chunks.iter().map(|c| &c.id).collect();

    // 校验 children 引用完整性
    for chunk in &manifest.chunks {
        for child in &chunk.children {
            if !ids.contains(child) {
                return Err(ManifestError::UnknownChild {
                    chunk: chunk.id.to_string(),
                    child: child.to_string(),
                });
            }
        }
    }

    // 入度 = 依赖的孩子数量；边：child -> parent
    let mut in_degree: BTreeMap<&ChunkId, usize> = BTreeMap::new();
    let mut dependents: BTreeMap<&ChunkId, Vec<&ChunkId>> = BTreeMap::new();
    for chunk in &manifest.chunks {
        in_degree.entry(&chunk.id).or_insert(0);
        for child in &chunk.children {
            *in_degree.entry(&chunk.id).or_insert(0) += 1;
            dependents.entry(child).or_default().push(&chunk.id);
        }
    }

    // 就绪队列：入度 0 的分块，按 id 字典序保证确定性（升序队列 + pop_front）
    let mut ready: VecDeque<&ChunkId> = in_degree
        .iter()
        .filter(|(_, &d)| d == 0)
        .map(|(&id, _)| id)
        .collect();
    {
        let mut v: Vec<&ChunkId> = ready.drain(..).collect();
        v.sort();
        ready.extend(v);
    }

    let mut order = Vec::with_capacity(manifest.chunks.len());
    while let Some(id) = ready.pop_front() {
        order.push(id.clone());
        if let Some(parents) = dependents.get(id) {
            for parent in parents {
                let d = in_degree.get_mut(parent).expect("parent must exist");
                *d -= 1;
                if *d == 0 {
                    // 二分插入保持字典序
                    let pos = ready.binary_search(parent).unwrap_or_else(|p| p);
                    ready.insert(pos, parent);
                }
            }
        }
    }

    if order.len() != manifest.chunks.len() {
        let mut stuck: Vec<String> = in_degree
            .iter()
            .filter(|(_, &d)| d > 0)
            .map(|(&id, _)| id.0.clone())
            .collect();
        stuck.sort();
        return Err(ManifestError::CycleDetected { chunks: stuck.join(", ") });
    }

    Ok(order)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{Bounds, Chunk, ChunkStatus};

    fn bounds() -> Bounds {
        Bounds { min: [0.0; 3], max: [1.0; 3] }
    }

    fn chunk(id: &str, lod: u32, children: &[&str]) -> Chunk {
        Chunk {
            id: ChunkId::new(id),
            lod,
            bounds: bounds(),
            status: ChunkStatus::Pending,
            attempts: 0,
            children: children.iter().map(|c| ChunkId::new(*c)).collect(),
            source: None,
            source_offset: None,
        }
    }

    fn manifest(chunks: Vec<Chunk>) -> ChunkManifest {
        ChunkManifest { task_id: "t".into(), chunks, source: None }
    }

    #[test]
    fn sorts_children_before_parents() {
        let m = manifest(vec![
            chunk("root", 0, &["b", "a"]),
            chunk("a", 1, &[]),
            chunk("b", 1, &["d"]),
            chunk("c", 1, &[]),
            chunk("d", 2, &[]),
        ]);
        let order = topo_sort(&m).unwrap();
        let pos = |s: &str| order.iter().position(|id| id.0 == s).unwrap();
        // 子先于父
        assert!(pos("a") < pos("root"));
        assert!(pos("b") < pos("root"));
        assert!(pos("d") < pos("b"));
        assert_eq!(order.len(), 5);
    }

    #[test]
    fn same_level_is_deterministic() {
        let m = manifest(vec![
            chunk("z", 1, &[]),
            chunk("a", 1, &[]),
            chunk("root", 0, &["z", "a"]),
        ]);
        let order = topo_sort(&m).unwrap();
        assert_eq!(
            order.iter().map(|i| i.0.as_str()).collect::<Vec<_>>(),
            vec!["a", "z", "root"]
        );
    }

    #[test]
    fn detects_cycle() {
        let m = manifest(vec![
            chunk("p", 0, &["q"]),
            chunk("q", 1, &["r"]),
            chunk("r", 2, &["p"]), // 环：p -> q -> r -> p
        ]);
        let err = topo_sort(&m).unwrap_err();
        assert!(matches!(err, ManifestError::CycleDetected { .. }));
        assert!(err.to_string().contains("cycle detected"));
    }

    #[test]
    fn detects_self_dependency() {
        let m = manifest(vec![chunk("solo", 0, &["solo"])]);
        let err = topo_sort(&m).unwrap_err();
        assert!(matches!(err, ManifestError::CycleDetected { .. }));
    }

    #[test]
    fn rejects_unknown_child() {
        let m = manifest(vec![chunk("p", 0, &["ghost"])]);
        let err = topo_sort(&m).unwrap_err();
        assert!(matches!(
            err,
            ManifestError::UnknownChild { ref chunk, ref child }
                if chunk == "p" && child == "ghost"
        ));
    }

    #[test]
    fn empty_manifest_ok() {
        let m = manifest(vec![]);
        assert!(topo_sort(&m).unwrap().is_empty());
    }
}
