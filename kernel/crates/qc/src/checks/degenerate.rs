//! 检测 1：退化三角形（零面积 / 重复顶点索引 / 重复面）。
//!
//! - 零面积：面积 = |cross(b−a, c−a)|/2 < `area_eps`（可配，默认 1e-10 m²）；
//! - 重复顶点：面内索引相等（a==b / b==c / a==c）；
//! - 重复面：排序后的索引三元组重复出现（重复实例各计 1，首个不计）。
//!
//! 一面可同时命中多类，`total` 为三类命中次数合计（去重面数以明细为准）。

use std::collections::HashSet;

use tangis_osgb::Mesh;

use crate::checks::MAX_SAMPLES;
use crate::report::{DegenerateReport, FaceSample};

pub fn check_degenerate(mesh: &Mesh, area_eps: f64) -> DegenerateReport {
    let verts = &mesh.vertices;
    let mut zero_area = 0usize;
    let mut repeated_vertex = 0usize;
    let mut duplicate_face = 0usize;
    let mut samples: Vec<FaceSample> = Vec::new();
    let mut seen: HashSet<[u32; 3]> = HashSet::new();

    for (fi, f) in mesh.indices.chunks_exact(3).enumerate() {
        let (a, b, c) = (f[0], f[1], f[2]);
        let mut kinds: Vec<&'static str> = Vec::new();
        if a == b || b == c || a == c {
            repeated_vertex += 1;
            kinds.push("repeated_vertex");
        }
        // 顶点坐标重复同样构成退化（面内任意两顶点位置相同）
        let pa = f64p(verts.get(a as usize).copied().unwrap_or([0.0; 3]));
        let pb = f64p(verts.get(b as usize).copied().unwrap_or([0.0; 3]));
        let pc = f64p(verts.get(c as usize).copied().unwrap_or([0.0; 3]));
        let cross = cross3(sub(pb, pa), sub(pc, pa));
        let area = norm(cross) / 2.0;
        if area < area_eps {
            zero_area += 1;
            kinds.push("zero_area");
        }
        let mut sorted = [a, b, c];
        sorted.sort_unstable();
        if !seen.insert(sorted) {
            duplicate_face += 1;
            kinds.push("duplicate_face");
        }
        if !kinds.is_empty() && samples.len() < MAX_SAMPLES {
            samples.push(FaceSample {
                face: fi,
                kinds: kinds.iter().map(|k| (*k).to_string()).collect(),
                centroid: [
                    (pa[0] + pb[0] + pc[0]) / 3.0,
                    (pa[1] + pb[1] + pc[1]) / 3.0,
                    (pa[2] + pb[2] + pc[2]) / 3.0,
                ],
            });
        }
    }
    DegenerateReport {
        zero_area,
        repeated_vertex,
        duplicate_face,
        total: zero_area + repeated_vertex + duplicate_face,
        samples,
    }
}

// ---- 小向量工具（f64，检测专用）----

pub(crate) type P3 = [f64; 3];

pub(crate) fn f64p(v: [f32; 3]) -> P3 {
    [v[0] as f64, v[1] as f64, v[2] as f64]
}

pub(crate) fn sub(a: P3, b: P3) -> P3 {
    [a[0] - b[0], a[1] - b[1], a[2] - b[2]]
}

pub(crate) fn cross3(a: P3, b: P3) -> P3 {
    [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]]
}

pub(crate) fn dot3(a: P3, b: P3) -> f64 {
    a[0] * b[0] + a[1] * b[1] + a[2] * b[2]
}

pub(crate) fn norm(a: P3) -> f64 {
    dot3(a, a).sqrt()
}

/// 归一化；零向量返回 None。
pub(crate) fn normalized(a: P3) -> Option<P3> {
    let n = norm(a);
    if n < 1e-20 {
        return None;
    }
    Some([a[0] / n, a[1] / n, a[2] / n])
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mesh_of(faces: &[[u32; 3]], verts: &[[f32; 3]]) -> Mesh {
        Mesh {
            vertices: verts.to_vec(),
            indices: faces.iter().flat_map(|f| f.iter().copied()).collect(),
            ..Mesh::default()
        }
    }

    #[test]
    fn detects_each_degeneracy_kind() {
        let verts = vec![
            [0.0, 0.0, 0.0],
            [1.0, 0.0, 0.0],
            [0.0, 1.0, 0.0],
            [2.0, 0.0, 0.0], // 共线点（零面积）
        ];
        let faces = [
            [0, 1, 2], // 干净
            [0, 1, 3], // 共线 → 零面积
            [0, 0, 2], // 重复顶点索引
            [1, 2, 0], // 与 face 0 同面（排序后 0,1,2 重复）
        ];
        let r = check_degenerate(&mesh_of(&faces, &verts), 1e-10);
        // [0,1,3] 共线 → 零面积；[0,0,2] 重复顶点同时叉积为零 → 双命中
        assert_eq!(r.zero_area, 2);
        assert_eq!(r.repeated_vertex, 1);
        assert_eq!(r.duplicate_face, 1);
        assert_eq!(r.total, 4);
        assert_eq!(r.samples.len(), 3);
    }

    #[test]
    fn clean_mesh_zero_warnings() {
        let verts = vec![[0.0; 3], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0], [1.0, 1.0, 0.1]];
        let faces = [[0, 1, 2], [1, 3, 2]];
        let r = check_degenerate(&mesh_of(&faces, &verts), 1e-10);
        assert_eq!((r.zero_area, r.repeated_vertex, r.duplicate_face, r.total), (0, 0, 0, 0));
        assert!(r.samples.is_empty());
    }

    #[test]
    fn repeated_coords_without_repeated_index_counted() {
        // 索引不同但坐标相同的两个顶点 → 零面积
        let verts = vec![[0.0; 3], [1.0, 0.0, 0.0], [0.0; 3]];
        let faces = [[0, 1, 2]];
        let r = check_degenerate(&mesh_of(&faces, &verts), 1e-10);
        assert_eq!(r.zero_area, 1);
        assert_eq!(r.repeated_vertex, 0);
    }
}
