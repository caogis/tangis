//! 检测 2：法线翻转（共享边邻接面夹角突变）。
//!
//! 对每条被两个有效面（非退化、索引互异）共享的无向边，比较两面几何
//! 法线（顶点绕序叉积归一化）夹角；夹角 > `max_angle_deg`（可配，默认
//! 120°）记为翻转边。孤立法线翻转率 = 仅命中 1 条翻转边的面数 / 有效面数
//! ——孤立翻转通常是单面绕序错误，成片翻折（每面 ≥2 条翻转边）是结构性
//! 折叠，两者在报告中可区分。

use std::collections::HashMap;

use tangis_osgb::Mesh;

use super::degenerate::{cross3, dot3, f64p, normalized, sub, P3};
use super::MAX_SAMPLES;
use crate::report::{FlipSample, NormalFlipReport};

pub fn check_normal_flip(mesh: &Mesh, max_angle_deg: f64) -> NormalFlipReport {
    let verts = &mesh.vertices;
    let face_count = mesh.indices.len() / 3;

    // 预计算各面法线（退化面 = None）
    let mut face_normals: Vec<Option<P3>> = Vec::with_capacity(face_count);
    for f in mesh.indices.chunks_exact(3) {
        let (a, b, c) = (f[0] as usize, f[1] as usize, f[2] as usize);
        let n = match (verts.get(a), verts.get(b), verts.get(c)) {
            (Some(&va), Some(&vb), Some(&vc)) => {
                normalized(cross3(sub(f64p(vb), f64p(va)), sub(f64p(vc), f64p(va))))
            }
            _ => None,
        };
        face_normals.push(n);
    }

    // 无向边 → 入射面列表
    let mut edges: HashMap<(u32, u32), Vec<usize>> = HashMap::new();
    for (fi, f) in mesh.indices.chunks_exact(3).enumerate() {
        if face_normals[fi].is_none() {
            continue;
        }
        for e in [(f[0], f[1]), (f[1], f[2]), (f[2], f[0])] {
            edges.entry((e.0.min(e.1), e.0.max(e.1))).or_default().push(fi);
        }
    }

    let mut edges_checked = 0usize;
    let mut flipped_edges = 0usize;
    let mut flipped_per_face = vec![0usize; face_count];
    let mut samples: Vec<FlipSample> = Vec::new();

    for ((a, b), faces) in &edges {
        if faces.len() != 2 {
            continue; // 只比较恰被两面共享的边（非流形边跳过）
        }
        let (na, nb) = match (&face_normals[faces[0]], &face_normals[faces[1]]) {
            (Some(x), Some(y)) => (x, y),
            _ => continue,
        };
        edges_checked += 1;
        let cos = (dot3(*na, *nb)).clamp(-1.0, 1.0);
        let angle_deg = cos.acos().to_degrees();
        if angle_deg > max_angle_deg {
            flipped_edges += 1;
            flipped_per_face[faces[0]] += 1;
            flipped_per_face[faces[1]] += 1;
            let (pa, pb) = (f64p(verts[*a as usize]), f64p(verts[*b as usize]));
            let mid = [(pa[0] + pb[0]) / 2.0, (pa[1] + pb[1]) / 2.0, (pa[2] + pb[2]) / 2.0];
            if samples.len() < MAX_SAMPLES {
                samples.push(FlipSample {
                    face_a: faces[0],
                    face_b: faces[1],
                    angle_deg,
                    midpoint: mid,
                });
            }
        }
    }
    samples.sort_by(|x, y| y.angle_deg.total_cmp(&x.angle_deg));

    let flipped_faces = flipped_per_face.iter().filter(|&&c| c > 0).count();
    let isolated_faces = flipped_per_face.iter().filter(|&&c| c == 1).count();
    let valid_faces = face_normals.iter().filter(|n| n.is_some()).count();
    let isolated_flip_rate = if valid_faces > 0 {
        isolated_faces as f64 / valid_faces as f64
    } else {
        0.0
    };
    NormalFlipReport {
        edges_checked,
        flipped_edges,
        flipped_faces,
        isolated_faces,
        isolated_flip_rate,
        samples,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::checks::degenerate::check_degenerate;

    fn quad_mesh(reversed_second: bool) -> Mesh {
        // 平面方片拆两三角：翻转第二个三角形绕序
        let verts = vec![
            [0.0, 0.0, 0.0],
            [1.0, 0.0, 0.0],
            [1.0, 1.0, 0.0],
            [0.0, 1.0, 0.0],
        ];
        let faces = if reversed_second {
            [[0u32, 1, 2], [0, 3, 2]]
        } else {
            [[0u32, 1, 2], [0, 2, 3]]
        };
        Mesh {
            vertices: verts,
            indices: faces.iter().flat_map(|f| f.iter().copied()).collect(),
            ..Mesh::default()
        }
    }

    #[test]
    fn detects_single_reversed_face_as_isolated() {
        let r = check_normal_flip(&quad_mesh(true), 120.0);
        assert_eq!(r.edges_checked, 1);
        assert_eq!(r.flipped_edges, 1);
        assert_eq!(r.flipped_faces, 2);
        // 两面各自只有 1 条翻转边 → 都算孤立
        assert_eq!(r.isolated_faces, 2);
        assert!((r.isolated_flip_rate - 1.0).abs() < 1e-12);
        assert_eq!(r.samples.len(), 1);
        assert!(r.samples[0].angle_deg > 179.0, "共面反向夹角应≈180°，实际 {}", r.samples[0].angle_deg);
    }

    #[test]
    fn clean_quad_zero_warnings() {
        let r = check_normal_flip(&quad_mesh(false), 120.0);
        assert_eq!(r.edges_checked, 1);
        assert_eq!(r.flipped_edges, 0);
        assert_eq!(r.isolated_flip_rate, 0.0);
    }

    #[test]
    fn smooth_grid_below_threshold() {
        // 3×3 轻微起伏网格：法线平滑变化，120° 下零翻转
        let mut verts = Vec::new();
        for y in 0..3 {
            for x in 0..3 {
                verts.push([x as f32, y as f32, 0.1 * ((x + y) as f32)]);
            }
        }
        let mut idx = Vec::new();
        for y in 0..2 {
            for x in 0..2 {
                let v = (y * 3 + x) as u32;
                idx.extend_from_slice(&[v, v + 1, v + 3]);
                idx.extend_from_slice(&[v + 1, v + 4, v + 3]);
            }
        }
        let mesh = Mesh { vertices: verts, indices: idx, ..Mesh::default() };
        let r = check_normal_flip(&mesh, 120.0);
        assert_eq!(r.flipped_edges, 0);
        // 2×2 单元的 3×3 网格内部共享边实测 8 条（4 对角 + 4 内部网格线段）
        assert_eq!(r.edges_checked, 8);
        // 干净语料同时过退化检测，零误报交叉验证
        assert_eq!(check_degenerate(&mesh, 1e-10).total, 0);
    }

    #[test]
    fn degenerate_faces_excluded_from_flip_stats() {
        let verts = vec![[0.0; 3], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0]];
        let faces = [[0u32, 1, 2], [0, 1, 1]]; // 第二面退化
        let mesh = Mesh {
            vertices: verts,
            indices: faces.iter().flat_map(|f| f.iter().copied()).collect(),
            ..Mesh::default()
        };
        let r = check_normal_flip(&mesh, 120.0);
        assert_eq!(r.edges_checked, 0);
    }
}
