//! QEM（Quadric Error Metrics，Garland–Heckbert 1997）网格简化。
//!
//! M2-F12a：PRD F-12「模型轻量化」第一步。面向倾斜摄影切片的简化必须保证
//! **瓦片接缝不开裂**：相邻 tile 共享的边界顶点在各自简化后必须位置不变，
//! 否则两块瓦片在接缝处出现裂缝。本 crate 的边界保护语义：
//!
//! 1. **边界边（boundary edge，仅被 1 个存活三角形引用）一律不折叠**；
//! 2. **边界顶点（位于任一边界边上）永不被移除、永不移动**——折叠只允许
//!    「移除内部端点、保留端点」方向；保留端点为边界顶点时新位置强制取
//!    其原位置（不参与 QEM 最优点求解）；
//! 3. 非流形边（被 >2 个存活三角形引用）不折叠。
//!
//! # 算法要点
//!
//! - 每顶点累加其关联三角形所在平面的 4×4 对称二次型 `Q = Σ p pᵀ`
//!   （平面用未归一化法向量 = 叉积，天然带面积权重）；
//! - 边折叠代价 `cost = v*ᵀ (Qᵤ+Qᵥ) v*`，`v*` 为二次型最优解
//!   （解 3×3 线性方程组；退化/奇异时回退两端点中点）；
//! - 二叉堆（最小优先队列）惰性失效：弹出时重算代价，与入堆时不一致
//!   即丢弃（顶点位置/二次型在折叠后变化，陈旧条目自然过期）；
//! - 折叠后保留顶点二次型取和 `Qᵤ += Qᵥ`，UV 取端点线性插值
//!   （插值参数 t 由最优点在前端点连线上的投影参数 clamp 到 [0,1]）；
//! - 收缩至目标面数（`target = max(4, ceil(ratio × 输入面数))`）或堆空为止；
//! - 结束后清理退化三角形（重复索引 / 零面积）并重映射顶点；
//!   输入带法线时按**面积加权平均**重算输出法线（简化改变了顶点邻域，
//!   原法线不再可信）。
//!
//! # 诚实边界
//!
//! - UV 处理为端点线性插值（无独立 UV 二次型约束）；漂移上界由测试断言
//!   （平面网格 uv=位置投影时漂移 < 1e-4，见 `qem::tests`）；
//! - [`approx_hausdorff`] 为单侧（原网格顶点 → 简化网格表面）顶点采样
//!   Hausdorff 近似，非双向精确 Hausdorff 距离；
//! - 不做顶点焊接/重网格化，UV 接缝处同位置异 UV 的复制顶点各自独立简化。

pub mod qem;

use std::time::Duration;

/// 简化输入/输出的统一网格视图（不依赖 `tangis_osgb::Mesh`，避免类型耦合；
/// 调用方按字段拷贝/借用自己的网格类型）。
#[derive(Debug, Clone, Default, PartialEq)]
pub struct MeshData {
    /// 顶点位置。
    pub positions: Vec<[f32; 3]>,
    /// 三角形索引（每 3 个一 face）。
    pub indices: Vec<u32>,
    /// 可选顶点法线（存在时输出按面积加权重算）。
    pub normals: Option<Vec<[f32; 3]>>,
    /// 可选顶点 UV（存在时折叠端点线性插值）。
    pub uvs: Option<Vec<[f32; 2]>>,
    /// 可选每顶点 feature id（BATCHID 语义，纯透传：幸存顶点保留自身值，
    /// 不插值）。存在时输出按幸存顶点重映射，保证数量一致。
    pub batch_ids: Option<Vec<u32>>,
}

/// 简化错误。
#[derive(Debug, thiserror::Error, PartialEq)]
pub enum SimplifyError {
    /// 目标比率非法（须在 (0, 1]）。
    #[error("目标比率 {ratio} 非法：须在 (0, 1] 区间")]
    InvalidRatio { ratio: f32 },
    /// 空网格（无顶点或无索引）。
    #[error("空网格：顶点 {vertices}，索引 {indices}")]
    EmptyMesh { vertices: usize, indices: usize },
    /// 索引超出顶点数组。
    #[error("索引 {index} 超出顶点数 {vertices}")]
    IndexOutOfRange { index: u32, vertices: usize },
    /// 索引数不是 3 的倍数。
    #[error("索引数 {count} 不是 3 的倍数（mode = TRIANGLES）")]
    IndexCountNotMultipleOf3 { count: usize },
    /// UV / 法线数量与顶点不一致。
    #[error("属性数量不符：顶点 {vertices}，属性 {attrs}")]
    AttributeCountMismatch { vertices: usize, attrs: usize },
}

/// 简化统计（实测，不做任何估算）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SimplifyStats {
    /// 输入顶点数（含未被任何存活面引用的顶点）。
    pub vertices_in: usize,
    /// 输出顶点数。
    pub vertices_out: usize,
    /// 输入面数（剔除非流形/退化输入面之前）。
    pub faces_in: usize,
    /// 输出面数。
    pub faces_out: usize,
    /// 实际执行的边折叠次数。
    pub collapses: usize,
    /// 目标面数（`max(4, ceil(ratio × faces_in))`）。
    pub target_faces: usize,
    /// 是否达到目标面数（false = 优先队列耗尽提前停止，受边界保护约束）。
    pub target_reached: bool,
    /// 算法纯耗时（不含输入校验与输出重映射）。
    pub elapsed: Duration,
}

/// QEM 边折叠简化。
///
/// `target_ratio` ∈ (0, 1]：目标面数 = 输入面数 × ratio（向上取整，下限 4）。
/// 边界保护永远开启（瓦片接缝语义，见 crate 文档）。
pub fn simplify(mesh: &MeshData, target_ratio: f32) -> Result<(MeshData, SimplifyStats), SimplifyError> {
    if !(target_ratio > 0.0 && target_ratio <= 1.0) || target_ratio.is_nan() {
        return Err(SimplifyError::InvalidRatio { ratio: target_ratio });
    }
    let indices = &mesh.indices;
    let vertices = mesh.positions.len();
    if vertices == 0 || indices.is_empty() {
        return Err(SimplifyError::EmptyMesh { vertices, indices: indices.len() });
    }
    if !indices.len().is_multiple_of(3) {
        return Err(SimplifyError::IndexCountNotMultipleOf3 { count: indices.len() });
    }
    if let Some(&i) = indices.iter().find(|&&i| i as usize >= vertices) {
        return Err(SimplifyError::IndexOutOfRange { index: i, vertices });
    }
    if let Some(uvs) = &mesh.uvs {
        if uvs.len() != vertices {
            return Err(SimplifyError::AttributeCountMismatch { vertices, attrs: uvs.len() });
        }
    }
    if let Some(ns) = &mesh.normals {
        if ns.len() != vertices {
            return Err(SimplifyError::AttributeCountMismatch { vertices, attrs: ns.len() });
        }
    }
    if let Some(ids) = &mesh.batch_ids {
        if ids.len() != vertices {
            return Err(SimplifyError::AttributeCountMismatch { vertices, attrs: ids.len() });
        }
    }

    let target_faces = (((indices.len() / 3) as f64 * target_ratio as f64).ceil() as usize).max(4);

    let mut s = qem::Simplifier::new(mesh);
    let ((positions, out_indices, uvs, normals, batch_ids), elapsed) = s.run(target_faces);

    let stats = SimplifyStats {
        vertices_in: vertices,
        vertices_out: positions.len(),
        faces_in: indices.len() / 3,
        faces_out: out_indices.len() / 3,
        collapses: s.collapses(),
        target_faces,
        target_reached: out_indices.len() / 3 <= target_faces,
        elapsed,
    };
    Ok((
        MeshData { positions, indices: out_indices, normals, uvs, batch_ids },
        stats,
    ))
}

/// 单侧顶点采样 Hausdorff 近似：`from` 的每个顶点到 `to` 网格表面的最近距离
/// 取最大值。`to` 为空网格时返回 `f64::INFINITY`（调用方自行处理）。
///
/// 这是「原网格 → 简化网格」方向的保守度量（能反映简化删掉了多少几何），
/// 不含反向（简化网格是否偏离原表面过远的检测）。
pub fn approx_hausdorff(from: &MeshData, to: &MeshData) -> f64 {
    qem::max_vertex_to_mesh_distance(&from.positions, &to.positions, &to.indices)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_invalid_ratio_and_meshes() {
        let tri = MeshData {
            positions: vec![[0.0; 3]; 3],
            indices: vec![0, 1, 2],
            normals: None,
            uvs: None,
            batch_ids: None,
        };
        for ratio in [0.0, -0.5, 1.5] {
            assert_eq!(
                simplify(&tri, ratio),
                Err(SimplifyError::InvalidRatio { ratio })
            );
        }
        // 空网格
        let empty = MeshData::default();
        assert_eq!(
            simplify(&empty, 0.5),
            Err(SimplifyError::EmptyMesh { vertices: 0, indices: 0 })
        );
        // 索引越界
        let bad = MeshData {
            positions: vec![[0.0; 3]; 3],
            indices: vec![0, 1, 3],
            normals: None,
            uvs: None,
            batch_ids: None,
        };
        assert_eq!(
            simplify(&bad, 0.5),
            Err(SimplifyError::IndexOutOfRange { index: 3, vertices: 3 })
        );
        // 索引数非 3 倍数
        let bad2 = MeshData {
            positions: vec![[0.0; 3]; 3],
            indices: vec![0, 1],
            normals: None,
            uvs: None,
            batch_ids: None,
        };
        assert_eq!(
            simplify(&bad2, 0.5),
            Err(SimplifyError::IndexCountNotMultipleOf3 { count: 2 })
        );
        // UV 数不符
        let bad3 = MeshData {
            positions: vec![[0.0; 3]; 3],
            indices: vec![0, 1, 2],
            normals: None,
            uvs: Some(vec![[0.0; 2]; 2]),
            batch_ids: None,
        };
        assert_eq!(
            simplify(&bad3, 0.5),
            Err(SimplifyError::AttributeCountMismatch { vertices: 3, attrs: 2 })
        );
    }
}
