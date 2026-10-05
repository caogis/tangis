//! 检测 5：自相交初检（AABB 空间哈希预筛 + 精确三角形相交测试）。
//!
//! 三角形 AABB 落入空间哈希格收集全量候选对（非抽样，覆盖率 1.0），
//! 排除共享顶点索引的邻接对（合法共边/共点）后做精确相交：
//! 1. 平面侧判快速排除（两面顶点全部位于对方平面同侧 → 不相交）；
//! 2. 近共面走 2D（主轴投影）边交叉 + 点包含测试；
//! 3. 其余走线段-三角形（Möller–Trumbore 段参数化）六个方向。
//!
//! 判定容差相对网格尺度（包围盒对角线 × 1e-9，下限 1e-12）。

use std::collections::{HashMap, HashSet};

use tangis_osgb::Mesh;

use super::degenerate::{cross3, dot3, f64p, norm, sub, P3};
use super::MAX_SAMPLES;
use crate::report::{IntersectSample, SelfIntersectReport};

pub const METHOD: &str = "AABB 空间哈希预筛（全量候选对，非抽样）+ 精确三角形相交（平面侧判/线段-三角形/共面2D）";

pub fn check_self_intersect(mesh: &Mesh) -> SelfIntersectReport {
    let tris: Vec<[P3; 3]> = mesh
        .indices
        .chunks_exact(3)
        .map(|f| {
            [
                f64p(mesh.vertices[f[0] as usize]),
                f64p(mesh.vertices[f[1] as usize]),
                f64p(mesh.vertices[f[2] as usize]),
            ]
        })
        .collect();
    if tris.is_empty() {
        return empty_report();
    }

    // 全局尺度（包围盒对角线）→ 判定容差
    let mut lo = tris[0][0];
    let mut hi = tris[0][0];
    for t in &tris {
        for p in t {
            for a in 0..3 {
                lo[a] = lo[a].min(p[a]);
                hi[a] = hi[a].max(p[a]);
            }
        }
    }
    let diag = norm(sub(hi, lo));
    let eps = (diag * 1e-9).max(1e-12);

    // AABB → 空间哈希
    let cell = (diag / 32.0).max(1e-12);
    let mut grid: HashMap<(i64, i64, i64), Vec<u32>> = HashMap::new();
    let mut tri_cells: Vec<Vec<(i64, i64, i64)>> = Vec::with_capacity(tris.len());
    for (i, t) in tris.iter().enumerate() {
        let (bmin, bmax) = tri_bbox(t);
        let k0 = cell_key(bmin, cell);
        let k1 = cell_key(bmax, cell);
        let mut cells = Vec::new();
        for x in k0.0..=k1.0 {
            for y in k0.1..=k1.1 {
                for z in k0.2..=k1.2 {
                    let key = (x, y, z);
                    grid.entry(key).or_default().push(i as u32);
                    cells.push(key);
                }
            }
        }
        tri_cells.push(cells);
    }

    // 候选对去重（同一对面经多格重复入桶）
    let mut candidates: HashSet<(u32, u32)> = HashSet::new();
    for (i, cells) in tri_cells.iter().enumerate() {
        for key in cells {
            if let Some(list) = grid.get(key) {
                for &j in list {
                    let ju = j as usize;
                    if ju > i {
                        candidates.insert((i as u32, j));
                    }
                }
            }
        }
    }

    let mut intersections = 0usize;
    let mut samples: Vec<IntersectSample> = Vec::new();
    for &(a, b) in &candidates {
        let (ia, ib) = (a as usize, b as usize);
        let fa = &mesh.indices[ia * 3..ia * 3 + 3];
        let fb = &mesh.indices[ib * 3..ib * 3 + 3];
        // 共享顶点索引 = 合法邻接（共边/共点），排除
        if fa.iter().any(|x| fb.contains(x)) {
            continue;
        }
        if triangles_intersect(&tris[ia], &tris[ib], eps) {
            intersections += 1;
            if samples.len() < MAX_SAMPLES {
                let ca = centroid(tris[ia]);
                let cb = centroid(tris[ib]);
                samples.push(IntersectSample {
                    face_a: ia,
                    face_b: ib,
                    midpoint: [(ca[0] + cb[0]) / 2.0, (ca[1] + cb[1]) / 2.0, (ca[2] + cb[2]) / 2.0],
                });
            }
        }
    }

    SelfIntersectReport {
        candidate_pairs: candidates.len(),
        intersections,
        coverage: 1.0,
        method: METHOD.to_string(),
        samples,
    }
}

fn empty_report() -> SelfIntersectReport {
    SelfIntersectReport {
        candidate_pairs: 0,
        intersections: 0,
        coverage: 1.0,
        method: METHOD.to_string(),
        samples: Vec::new(),
    }
}

fn tri_bbox(t: &[[f64; 3]; 3]) -> ([f64; 3], [f64; 3]) {
    let mut min = t[0];
    let mut max = t[0];
    for p in t {
        for a in 0..3 {
            min[a] = min[a].min(p[a]);
            max[a] = max[a].max(p[a]);
        }
    }
    (min, max)
}

fn cell_key(p: [f64; 3], cell: f64) -> (i64, i64, i64) {
    ((p[0] / cell).floor() as i64, (p[1] / cell).floor() as i64, (p[2] / cell).floor() as i64)
}

fn centroid(t: [[f64; 3]; 3]) -> [f64; 3] {
    [
        (t[0][0] + t[1][0] + t[2][0]) / 3.0,
        (t[0][1] + t[1][1] + t[2][1]) / 3.0,
        (t[0][2] + t[1][2] + t[2][2]) / 3.0,
    ]
}

fn plane_of(t: &[[f64; 3]; 3]) -> (P3, f64) {
    let n = cross3(sub(t[1], t[0]), sub(t[2], t[0]));
    let d = dot3(n, t[0]);
    (n, d)
}

/// 精确三角形相交（eps 为绝对距离容差）。
pub(crate) fn triangles_intersect(t1: &[[f64; 3]; 3], t2: &[[f64; 3]; 3], eps: f64) -> bool {
    let (n1, d1) = plane_of(t1);
    let (n2, d2) = plane_of(t2);
    let n1n = norm(n1);
    let n2n = norm(n2);
    if n1n < 1e-20 || n2n < 1e-20 {
        return false; // 退化面不参与（退化检测单独负责）
    }
    // 平面侧判：t2 全部顶点在 t1 平面同侧（容差外）→ 不相交
    let dist2: Vec<f64> = t2.iter().map(|p| dot3(n1, *p) - d1).collect();
    if dist2.iter().all(|&d| d > eps * n1n) || dist2.iter().all(|&d| d < -eps * n1n) {
        return false;
    }
    let dist1: Vec<f64> = t1.iter().map(|p| dot3(n2, *p) - d2).collect();
    if dist1.iter().all(|&d| d > eps * n2n) || dist1.iter().all(|&d| d < -eps * n2n) {
        return false;
    }
    // 近共面：2D 判定（主轴投影）
    if dist2.iter().all(|&d| d.abs() <= eps * n1n * 4.0) {
        return coplanar_overlap(t1, t2, n1, eps);
    }
    // 非共面：六次线段-三角形测试
    for e in [(0, 1), (1, 2), (2, 0)] {
        if seg_tri(t1[e.0], t1[e.1], t2, n2, eps) {
            return true;
        }
        if seg_tri(t2[e.0], t2[e.1], t1, n1, eps) {
            return true;
        }
    }
    false
}

/// 线段 p0→p1 与三角形（含其平面法线 n）相交测试（Möller–Trumbore 段参数化）。
fn seg_tri(p0: P3, p1: P3, tri: &[[f64; 3]; 3], n: P3, eps: f64) -> bool {
    let dir = sub(p1, p0);
    let denom = dot3(n, dir);
    let nn = norm(n);
    if denom.abs() <= eps * nn * norm(dir) {
        return false; // 段与平面平行（共面情形另有处理）
    }
    let t = (dot3(n, tri[0]) - dot3(n, p0)) / denom;
    if !(-1e-9..=1.0 + 1e-9).contains(&t) {
        return false;
    }
    let p = [p0[0] + t * dir[0], p0[1] + t * dir[1], p0[2] + t * dir[2]];
    point_in_tri(p, tri)
}

/// 点在三角形内（符号面积法；子面积量纲与 n² 相同，容差取其相对值）。
fn point_in_tri(p: P3, t: &[[f64; 3]; 3]) -> bool {
    let n = cross3(sub(t[1], t[0]), sub(t[2], t[0]));
    let n2 = dot3(n, n);
    if n2 < 1e-40 {
        return false;
    }
    let tol = n2 * 1e-9;
    let b0 = dot3(n, cross3(sub(t[1], p), sub(t[2], p)));
    let b1 = dot3(n, cross3(sub(t[2], p), sub(t[0], p)));
    let b2 = dot3(n, cross3(sub(t[0], p), sub(t[1], p)));
    b0 >= -tol && b1 >= -tol && b2 >= -tol
}

/// 近共面两三角形的 2D 相交（投影到垂直于法线主轴的平面：丢弃最大分量轴，
/// 保证投影不退化）。
fn coplanar_overlap(t1: &[[f64; 3]; 3], t2: &[[f64; 3]; 3], n: P3, eps: f64) -> bool {
    let ax = dominant_axis(n);
    let (i, j) = match ax {
        0 => (1usize, 2usize),
        1 => (0usize, 2usize),
        _ => (0usize, 1usize),
    };
    let p1 = [
        [t1[0][i], t1[0][j]],
        [t1[1][i], t1[1][j]],
        [t1[2][i], t1[2][j]],
    ];
    let p2 = [
        [t2[0][i], t2[0][j]],
        [t2[1][i], t2[1][j]],
        [t2[2][i], t2[2][j]],
    ];
    // 边-边交叉
    for e1 in [(0usize, 1usize), (1, 2), (2, 0)] {
        for e2 in [(0usize, 1usize), (1, 2), (2, 0)] {
            if seg2_intersect(p1[e1.0], p1[e1.1], p2[e2.0], p2[e2.1], eps) {
                return true;
            }
        }
    }
    // 完全包含（顶点在另一面内）
    point_in_tri2(p1[0], &p2, eps)
        || point_in_tri2(p2[0], &p1, eps)
        || point_in_tri2(p1[1], &p2, eps)
        || point_in_tri2(p2[1], &p1, eps)
}

fn dominant_axis(n: P3) -> usize {
    let a = [n[0].abs(), n[1].abs(), n[2].abs()];
    if a[0] >= a[1] && a[0] >= a[2] {
        0
    } else if a[1] >= a[2] {
        1
    } else {
        2
    }
}

/// 2D 线段相交（含端点接触，容差 eps）。
fn seg2_intersect(a0: [f64; 2], a1: [f64; 2], b0: [f64; 2], b1: [f64; 2], eps: f64) -> bool {
    fn cross2(o: [f64; 2], a: [f64; 2], b: [f64; 2]) -> f64 {
        (a[0] - o[0]) * (b[1] - o[1]) - (a[1] - o[1]) * (b[0] - o[0])
    }
    let d1 = cross2(b0, b1, a0);
    let d2 = cross2(b0, b1, a1);
    let d3 = cross2(a0, a1, b0);
    let d4 = cross2(a0, a1, b1);
    if ((d1 > eps && d2 < -eps) || (d1 < -eps && d2 > eps))
        && ((d3 > eps && d4 < -eps) || (d3 < -eps && d4 > eps))
    {
        return true;
    }
    // 共线接触或端点落在段上
    let on = |p: [f64; 2], q0: [f64; 2], q1: [f64; 2]| {
        p[0] >= q0[0].min(q1[0]) - eps
            && p[0] <= q0[0].max(q1[0]) + eps
            && p[1] >= q0[1].min(q1[1]) - eps
            && p[1] <= q0[1].max(q1[1]) + eps
            && cross2(q0, q1, p).abs() < eps * 1e3
    };
    on(a0, b0, b1) || on(a1, b0, b1) || on(b0, a0, a1) || on(b1, a0, a1)
}

fn point_in_tri2(p: [f64; 2], t: &[[f64; 2]; 3], eps: f64) -> bool {
    fn sign(a: [f64; 2], b: [f64; 2], c: [f64; 2]) -> f64 {
        (b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0])
    }
    let d1 = sign(p, t[0], t[1]);
    let d2 = sign(p, t[1], t[2]);
    let d3 = sign(p, t[2], t[0]);
    let has_neg = d1 < -eps || d2 < -eps || d3 < -eps;
    let has_pos = d1 > eps || d2 > eps || d3 > eps;
    !(has_neg && has_pos)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mesh_of(faces: &[[[f32; 3]; 3]]) -> Mesh {
        let mut verts: Vec<[f32; 3]> = Vec::new();
        let mut idx = Vec::new();
        for f in faces {
            let base = verts.len() as u32;
            verts.extend_from_slice(f);
            idx.extend_from_slice(&[base, base + 1, base + 2]);
        }
        Mesh { vertices: verts, indices: idx, ..Mesh::default() }
    }

    #[test]
    fn detects_crossing_triangles() {
        // 水平面 z=0 大三角 × 竖直面 y=0 大三角 → 相交
        let m = mesh_of(&[
            [[-10.0, -10.0, 0.0], [10.0, -10.0, 0.0], [0.0, 10.0, 0.0]],
            [[0.0, 0.0, -10.0], [0.0, 0.0, 10.0], [5.0, 0.0, 0.0]],
        ]);
        let r = check_self_intersect(&m);
        assert_eq!(r.intersections, 1);
        assert!(r.candidate_pairs >= 1);
        assert_eq!(r.coverage, 1.0);
        assert!(r.method.contains("AABB"));
    }

    #[test]
    fn adjacent_and_separate_triangles_not_flagged() {
        // 共边两三角（共享顶点坐标但不共享索引）→ 需要如实报告吗？
        // 共享顶点索引的邻接对排除；这里测空间分离的三角形
        let m = mesh_of(&[
            [[0.0, 0.0, 0.0], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0]],
            [[5.0, 5.0, 0.0], [6.0, 5.0, 0.0], [5.0, 6.0, 0.0]],
        ]);
        let r = check_self_intersect(&m);
        assert_eq!(r.intersections, 0);
    }

    #[test]
    fn shared_index_pair_excluded() {
        // 精确共边但不共享索引（重复顶点）的合法拼接对：物理相交于边
        // ——共享顶点索引的对直接排除（合法邻接），此处验证排除逻辑
        let verts = vec![[0.0; 3], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0], [1.0, 1.0, 0.5]];
        let idx = vec![0, 1, 2, 1, 3, 2]; // face0 与 face1 共享顶点 1、2
        let m = Mesh { vertices: verts, indices: idx, ..Mesh::default() };
        let r = check_self_intersect(&m);
        // AABB 重叠候选可能存在，但共享顶点被排除
        assert_eq!(r.intersections, 0);
    }

    #[test]
    fn touching_coplanar_triangles_detected() {
        // 共面重叠（如重复铺装）应检出
        let m = mesh_of(&[
            [[0.0, 0.0, 0.0], [2.0, 0.0, 0.0], [0.0, 2.0, 0.0]],
            [[0.5, 0.5, 0.0], [2.5, 0.5, 0.0], [0.5, 2.5, 0.0]],
        ]);
        let r = check_self_intersect(&m);
        assert_eq!(r.intersections, 1, "共面重叠应检出");
    }

    #[test]
    fn clean_terrain_grid_zero_warnings() {
        // 4×4 平整网格（含共享顶点索引的常规邻接）零误报
        let mut verts = Vec::new();
        for y in 0..4 {
            for x in 0..4 {
                verts.push([x as f32, y as f32, 0.0]);
            }
        }
        let mut idx = Vec::new();
        for y in 0..3 {
            for x in 0..3 {
                let v = (y * 4 + x) as u32;
                idx.extend_from_slice(&[v, v + 1, v + 4]);
                idx.extend_from_slice(&[v + 1, v + 5, v + 4]);
            }
        }
        let r = check_self_intersect(&Mesh { vertices: verts, indices: idx, ..Mesh::default() });
        assert_eq!(r.intersections, 0);
    }

    #[test]
    fn coplanar_containment_detected() {
        // 小三角完全在大三角内部（共面包含，无边交叉）
        let m = mesh_of(&[
            [[-5.0, -5.0, 1.0], [5.0, -5.0, 1.0], [0.0, 5.0, 1.0]],
            [[0.0, 0.0, 1.0], [0.5, 0.0, 1.0], [0.0, 0.5, 1.0]],
        ]);
        let r = check_self_intersect(&m);
        assert_eq!(r.intersections, 1, "共面包含应检出");
    }
}
