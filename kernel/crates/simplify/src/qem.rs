//! QEM 简化核心实现（仅供 crate 内部与测试使用，算法约定见 crate 文档）。

use std::cmp::{Ordering, Reverse};
use std::collections::{BinaryHeap, HashSet};
use std::time::{Duration, Instant};

use crate::MeshData;

// ---------------------------------------------------------------------------
// 堆可排序 f64
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq)]
struct OrdF64(f64);

impl Eq for OrdF64 {}

impl Ord for OrdF64 {
    fn cmp(&self, other: &Self) -> Ordering {
        // NaN 已在入堆前钳为 0，不会出现
        self.0.partial_cmp(&other.0).unwrap_or(Ordering::Equal)
    }
}

impl PartialOrd for OrdF64 {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

// ---------------------------------------------------------------------------
// 3×3 矩阵（仅本模块使用的最小实现）
// ---------------------------------------------------------------------------

#[derive(Clone, Copy)]
struct Mat3([[f64; 3]; 3]);

impl Mat3 {
    fn det(&self) -> Option<f64> {
        let m = &self.0;
        let v = m[0][0] * (m[1][1] * m[2][2] - m[1][2] * m[2][1])
            - m[0][1] * (m[1][0] * m[2][2] - m[1][2] * m[2][0])
            + m[0][2] * (m[1][0] * m[2][1] - m[1][1] * m[2][0]);
        if v.is_finite() { Some(v) } else { None }
    }

    /// 第 `col` 列替换为 `v` 后的行列式（Cramer 法则用）。
    fn det_with_col(&self, col: usize, v: [f64; 3]) -> Option<f64> {
        let mut m = self.0;
        for (ri, row) in m.iter_mut().enumerate() {
            row[col] = v[ri];
        }
        Mat3(m).det()
    }
}

// ---------------------------------------------------------------------------
// 4×4 对称二次型（上三角 10 元素：q00 q01 q02 q03 q11 q12 q13 q22 q23 q33）
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, Default)]
struct Quadric([f64; 10]);

impl Quadric {
    /// 平面方程 `a x + b y + c z + d = 0` 的外积累加项 `p pᵀ`。
    /// `(a,b,c)` 用未归一化法向量（= 边叉积）时天然带面积权重。
    fn add_plane(&mut self, a: f64, b: f64, c: f64, d: f64) {
        let q = &mut self.0;
        q[0] += a * a;
        q[1] += a * b;
        q[2] += a * c;
        q[3] += a * d;
        q[4] += b * b;
        q[5] += b * c;
        q[6] += b * d;
        q[7] += c * c;
        q[8] += c * d;
        q[9] += d * d;
    }

    fn add(&mut self, other: &Quadric) {
        for (x, y) in self.0.iter_mut().zip(other.0) {
            *x += y;
        }
    }

    /// `vᵀ Q v`，v = [x, y, z, 1]。
    fn eval(&self, p: [f64; 3]) -> f64 {
        let [x, y, z] = p;
        let q = &self.0;
        q[0] * x * x
            + 2.0 * q[1] * x * y
            + 2.0 * q[2] * x * z
            + 2.0 * q[3] * x
            + q[4] * y * y
            + 2.0 * q[5] * y * z
            + 2.0 * q[6] * y
            + q[7] * z * z
            + 2.0 * q[8] * z
            + q[9]
    }

    /// 二次型最优位置：解 `A p = -b`，A = [[q00,q01,q02],[q01,q11,q12],
    /// [q02,q12,q22]]，b = [q03,q13,q23]；行列式近奇异（如共面片秩亏）
    /// 或非有限时返回 None（调用方回退端点中点）。
    fn optimal(&self) -> Option<[f64; 3]> {
        let q = &self.0;
        let m = Mat3([
            [q[0], q[1], q[2]],
            [q[1], q[4], q[5]],
            [q[2], q[5], q[7]],
        ]);
        let det = m.det()?;
        if det.abs() <= 1e-12 {
            return None;
        }
        let b = [-q[3], -q[6], -q[8]];
        Some([
            m.det_with_col(0, b)? / det,
            m.det_with_col(1, b)? / det,
            m.det_with_col(2, b)? / det,
        ])
    }
}

// ---------------------------------------------------------------------------
// 几何工具
// ---------------------------------------------------------------------------

fn sub3(a: [f64; 3], b: [f64; 3]) -> [f64; 3] {
    [a[0] - b[0], a[1] - b[1], a[2] - b[2]]
}

fn cross3(a: [f64; 3], b: [f64; 3]) -> [f64; 3] {
    [
        a[1] * b[2] - a[2] * b[1],
        a[2] * b[0] - a[0] * b[2],
        a[0] * b[1] - a[1] * b[0],
    ]
}

fn dot3(a: [f64; 3], b: [f64; 3]) -> f64 {
    a[0] * b[0] + a[1] * b[1] + a[2] * b[2]
}

fn norm3(a: [f64; 3]) -> f64 {
    dot3(a, a).sqrt()
}

fn cast64(p: [f32; 3]) -> [f64; 3] {
    [p[0] as f64, p[1] as f64, p[2] as f64]
}

/// 点到线段最近距离。
fn point_segment_distance(p: [f64; 3], a: [f64; 3], b: [f64; 3]) -> f64 {
    let ab = sub3(b, a);
    let len2 = dot3(ab, ab);
    if len2 <= 0.0 {
        return norm3(sub3(p, a));
    }
    let t = (dot3(sub3(p, a), ab) / len2).clamp(0.0, 1.0);
    norm3([p[0] - a[0] - ab[0] * t, p[1] - a[1] - ab[1] * t, p[2] - a[2] - ab[2] * t])
}

/// 点到三角形最近距离（Ericson《Real-Time Collision Detection》5.1.5）：
/// 投影落在三角形内取垂直距离，否则取三条边线段的最近距离。
pub(crate) fn point_triangle_distance(p: [f64; 3], a: [f64; 3], b: [f64; 3], c: [f64; 3]) -> f64 {
    let ab = sub3(b, a);
    let ac = sub3(c, a);
    let n = cross3(ab, ac);
    let nn = dot3(n, n);
    if nn <= 1e-30 {
        return point_segment_distance(p, a, b)
            .min(point_segment_distance(p, b, c))
            .min(point_segment_distance(p, a, c));
    }
    let inv_nn = 1.0 / nn;
    let ap = sub3(p, a);
    let d = dot3(n, ap) / nn.sqrt();
    let foot = [
        p[0] - n[0] * inv_nn * d,
        p[1] - n[1] * inv_nn * d,
        p[2] - n[2] * inv_nn * d,
    ];
    // 重心坐标（相对 a，基为 ab/ac）判断投影是否在三角形内
    let af = sub3(foot, a);
    let denom = dot3(ab, ab) * dot3(ac, ac) - dot3(ab, ac).powi(2);
    if denom <= 0.0 {
        return point_segment_distance(p, a, b)
            .min(point_segment_distance(p, b, c))
            .min(point_segment_distance(p, a, c));
    }
    let v = (dot3(ab, af) * dot3(ac, ac) - dot3(ac, af) * dot3(ab, ac)) / denom;
    let w = (dot3(ac, af) * dot3(ab, ab) - dot3(ab, af) * dot3(ab, ac)) / denom;
    if v >= 0.0 && w >= 0.0 && v + w <= 1.0 {
        d.abs()
    } else {
        point_segment_distance(p, a, b)
            .min(point_segment_distance(p, b, c))
            .min(point_segment_distance(p, a, c))
    }
}

/// `points` 中每点到网格（positions/indices）表面的最近距离的最大值
/// （单侧顶点采样 Hausdorff 近似）。网格空时返回 INFINITY。
pub(crate) fn max_vertex_to_mesh_distance(
    points: &[[f32; 3]],
    positions: &[[f32; 3]],
    indices: &[u32],
) -> f64 {
    if indices.is_empty() || positions.is_empty() {
        return f64::INFINITY;
    }
    let tri: Vec<([f64; 3], [f64; 3], [f64; 3])> = indices
        .chunks_exact(3)
        .map(|f| {
            (
                cast64(positions[f[0] as usize]),
                cast64(positions[f[1] as usize]),
                cast64(positions[f[2] as usize]),
            )
        })
        .collect();
    let mut max = 0.0f64;
    for p in points {
        let p = cast64(*p);
        let mut best = f64::INFINITY;
        for (a, b, c) in &tri {
            let d = point_triangle_distance(p, *a, *b, *c);
            if d < best {
                best = d;
            }
        }
        if best > max {
            max = best;
        }
    }
    max
}

// ---------------------------------------------------------------------------
// 简化器
// ---------------------------------------------------------------------------

/// 简化输出元组（位置/索引/UV/法线/batch_ids）。
pub(crate) type MeshParts = (
    Vec<[f32; 3]>,
    Vec<u32>,
    Option<Vec<[f32; 2]>>,
    Option<Vec<[f32; 3]>>,
    Option<Vec<u32>>,
);

struct Face {
    v: [u32; 3],
    alive: bool,
}

/// QEM 边折叠简化器（惰性优先队列 + 拓扑原位更新）。
pub(crate) struct Simplifier {
    pos: Vec<[f64; 3]>,
    uv: Option<Vec<[f64; 2]>>,
    quadric: Vec<Quadric>,
    alive: Vec<bool>,
    alive_verts: usize,
    faces: Vec<Face>,
    /// 每顶点关联的存活面 id（无重复）。
    vfaces: Vec<Vec<u32>>,
    alive_faces: usize,
    collapses: usize,
    heap: BinaryHeap<Reverse<(OrdF64, u32, u32)>>,
    /// 输入带法线 → 输出按面积加权重算。
    need_normals: bool,
    /// 每顶点 feature id（纯透传，幸存顶点保留自身值）。
    batch_ids: Option<Vec<u32>>,
}

impl Simplifier {
    pub(crate) fn new(mesh: &MeshData) -> Self {
        let faces_in = mesh.indices.len() / 3;
        let mut s = Simplifier {
            pos: mesh.positions.iter().map(|p| cast64(*p)).collect(),
            uv: mesh
                .uvs
                .as_ref()
                .map(|u| u.iter().map(|t| [t[0] as f64, t[1] as f64]).collect()),
            quadric: vec![Quadric::default(); mesh.positions.len()],
            alive: vec![true; mesh.positions.len()],
            alive_verts: mesh.positions.len(),
            faces: Vec::with_capacity(faces_in),
            vfaces: vec![Vec::new(); mesh.positions.len()],
            alive_faces: 0,
            collapses: 0,
            heap: BinaryHeap::new(),
            need_normals: mesh.normals.is_some(),
            batch_ids: mesh.batch_ids.clone(),
        };
        // 建面：剔除重复索引的退化输入面（零面积面在 compaction 阶段清理）
        for f in mesh.indices.chunks_exact(3) {
            let [a, b, c] = [f[0], f[1], f[2]];
            if a == b || b == c || a == c {
                continue;
            }
            let id = s.faces.len() as u32;
            s.faces.push(Face { v: [a, b, c], alive: true });
            for v in [a, b, c] {
                s.vfaces[v as usize].push(id);
            }
            s.alive_faces += 1;
        }
        // 每顶点二次型：关联三角形平面外积累加（未归一化法向量 = 面积权重）
        for face in &s.faces {
            let pa = s.pos[face.v[0] as usize];
            let pb = s.pos[face.v[1] as usize];
            let pc = s.pos[face.v[2] as usize];
            let n = cross3(sub3(pb, pa), sub3(pc, pa));
            let d = -dot3(n, pa);
            for &v in &face.v {
                s.quadric[v as usize].add_plane(n[0], n[1], n[2], d);
            }
        }
        s
    }

    pub(crate) fn collapses(&self) -> usize {
        self.collapses
    }

    /// 执行简化至 `target_faces`，返回重映射后的输出网格与算法耗时。
    pub(crate) fn run(&mut self, target_faces: usize) -> (MeshParts, Duration) {
        let start = Instant::now();
        self.seed_heap();
        while self.alive_faces > target_faces {
            match self.pop_valid() {
                Some((keep, rem)) => self.collapse(keep, rem),
                None => break, // 堆耗尽：受边界保护约束无法继续
            }
        }
        let elapsed = start.elapsed();
        (self.compact(), elapsed)
    }

    /// 全部初始边入堆（边界边/非流形边在弹出验证阶段拒绝）。
    fn seed_heap(&mut self) {
        let mut seen: HashSet<(u32, u32)> = HashSet::new();
        let mut edges: Vec<(u32, u32)> = Vec::new();
        for face in &self.faces {
            for i in 0..3 {
                let a = face.v[i];
                let b = face.v[(i + 1) % 3];
                let key = (a.min(b), a.max(b));
                if seen.insert(key) {
                    edges.push(key);
                }
            }
        }
        for (u, v) in edges {
            self.push_edge(u, v);
        }
    }

    fn push_edge(&mut self, u: u32, v: u32) {
        let cost = self.edge_cost(u, v);
        self.heap.push(Reverse((OrdF64(cost), u, v)));
    }

    /// 边 (u,v) 的折叠代价：合并二次型在最优位置（或中点回退）的取值。
    fn edge_cost(&self, u: u32, v: u32) -> f64 {
        let (ui, vi) = (u as usize, v as usize);
        let mut q = self.quadric[ui];
        q.add(&self.quadric[vi]);
        let point = q.optimal().unwrap_or_else(|| {
            [
                (self.pos[ui][0] + self.pos[vi][0]) * 0.5,
                (self.pos[ui][1] + self.pos[vi][1]) * 0.5,
                (self.pos[ui][2] + self.pos[vi][2]) * 0.5,
            ]
        });
        q.eval(point).max(0.0)
    }

    /// 弹出下一条有效折叠：验证存活、代价新鲜（与入堆一致）、流形
    /// （面计数 == 2，排除边界边与非流形边）、端点可移除性
    /// （边界顶点不可移除）。返回 (保留端点, 移除端点)。
    fn pop_valid(&mut self) -> Option<(u32, u32)> {
        loop {
            let Reverse((cost, u, v)) = self.heap.pop()?;
            if !self.alive[u as usize] || !self.alive[v as usize] || u == v {
                continue;
            }
            // 陈旧条目：任一端点位置/二次型已变 → 代价不一致 → 丢弃
            let now = self.edge_cost(u, v);
            if (now - cost.0).abs() > 1e-6 * (1.0 + cost.0.abs()) {
                continue;
            }
            // 流形约束：恰 2 面（1 = 边界边禁折叠；>2 = 非流形禁折叠）
            if self.edge_face_count(u, v) != 2 {
                continue;
            }
            // 移除端点选择：边界顶点不可移除；两端皆边界 → 跳过
            let bu = self.is_boundary_vertex(u);
            let bv = self.is_boundary_vertex(v);
            let (keep, rem) = match (bu, bv) {
                (false, false) | (false, true) => (v, u),
                (true, false) => (u, v),
                (true, true) => continue,
            };
            return Some((keep, rem));
        }
    }

    /// 存活面中同时包含 u、v 的数量。
    fn edge_face_count(&self, u: u32, v: u32) -> usize {
        self.vfaces[u as usize]
            .iter()
            .filter(|&&f| self.faces[f as usize].alive && self.faces[f as usize].v.contains(&v))
            .count()
    }

    /// 顶点在当前存活拓扑下是否位于边界边（在存活面中仅出现 1 次的边）上。
    fn is_boundary_vertex(&self, w: u32) -> bool {
        let mut edges: Vec<(u32, usize)> = Vec::new();
        for &f in &self.vfaces[w as usize] {
            let face = &self.faces[f as usize];
            if !face.alive {
                continue;
            }
            for i in 0..3 {
                let (a, b) = (face.v[i], face.v[(i + 1) % 3]);
                let other = if a == w {
                    b
                } else if b == w {
                    a
                } else {
                    continue;
                };
                if let Some(e) = edges.iter_mut().find(|e| e.0 == other) {
                    e.1 += 1;
                } else {
                    edges.push((other, 1));
                }
            }
        }
        edges.iter().any(|e| e.1 == 1)
    }

    /// 折叠 `rem` → `keep`：keep 位置取合并二次型最优点（keep 为边界顶点
    /// 时强制原位置），UV 端点线性插值，二次型求和，拓扑原位更新。
    fn collapse(&mut self, keep: u32, rem: u32) {
        let (k, r) = (keep as usize, rem as usize);
        let keep_was_boundary = self.is_boundary_vertex(keep);
        let old_keep_pos = self.pos[k];
        let rem_pos = self.pos[r];

        let mut q = self.quadric[k];
        q.add(&self.quadric[r]);
        self.quadric[k] = q;

        let new_pos = if keep_was_boundary {
            old_keep_pos
        } else {
            q.optimal().unwrap_or_else(|| {
                [
                    (old_keep_pos[0] + rem_pos[0]) * 0.5,
                    (old_keep_pos[1] + rem_pos[1]) * 0.5,
                    (old_keep_pos[2] + rem_pos[2]) * 0.5,
                ]
            })
        };

        // UV：端点线性插值，t = 最优点在前端点连线上的投影参数
        let t = if keep_was_boundary || new_pos == old_keep_pos {
            0.0
        } else {
            let seg = sub3(rem_pos, old_keep_pos);
            let len2 = dot3(seg, seg);
            if len2 <= 0.0 {
                0.0
            } else {
                (dot3(sub3(new_pos, old_keep_pos), seg) / len2).clamp(0.0, 1.0)
            }
        };
        if let Some(uvs) = &mut self.uv {
            let a = uvs[k];
            let b = uvs[r];
            uvs[k] = [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t];
        }

        self.pos[k] = new_pos;
        self.alive[r] = false;
        self.alive_verts -= 1;

        // rem 的关联面：替换 rem → keep；替换后索引重复 → 面死亡（含
        // 折叠边两侧的两个面）
        let mut dead: Vec<u32> = Vec::new();
        let rem_faces = std::mem::take(&mut self.vfaces[r]);
        for f in rem_faces {
            if !self.faces[f as usize].alive {
                continue;
            }
            {
                let face = &mut self.faces[f as usize];
                for v in &mut face.v {
                    if *v == rem {
                        *v = keep;
                    }
                }
            }
            let vs = self.faces[f as usize].v;
            if vs[0] == vs[1] || vs[1] == vs[2] || vs[0] == vs[2] {
                self.faces[f as usize].alive = false;
                dead.push(f);
            } else if !self.vfaces[k].contains(&f) {
                self.vfaces[k].push(f);
            }
        }
        // 死面从其余端点的关联表移除
        for f in &dead {
            let vs = self.faces[*f as usize].v;
            for w in vs {
                let wi = w as usize;
                if wi != k && self.alive[wi] {
                    self.vfaces[wi].retain(|&x| x != *f);
                }
            }
        }
        self.alive_faces -= dead.len();
        self.collapses += 1;

        // keep 的全部新邻接边重新入堆（旧条目靠代价复验自然过期）
        let mut neighbors: Vec<u32> = Vec::new();
        for &f in &self.vfaces[k] {
            if !self.faces[f as usize].alive {
                continue;
            }
            for v in self.faces[f as usize].v {
                if v != keep && !neighbors.contains(&v) {
                    neighbors.push(v);
                }
            }
        }
        for w in neighbors {
            self.push_edge(keep.min(w), keep.max(w));
        }
    }

    /// 输出重映射：剔除死顶点/死面/零面积面；输入带法线时按面积加权
    /// 平均重算输出法线。
    fn compact(&self) -> MeshParts {
        let mut remap = vec![u32::MAX; self.pos.len()];
        let mut positions = Vec::with_capacity(self.alive_verts);
        for (i, a) in self.alive.iter().enumerate() {
            if *a {
                remap[i] = positions.len() as u32;
                positions.push([
                    self.pos[i][0] as f32,
                    self.pos[i][1] as f32,
                    self.pos[i][2] as f32,
                ]);
            }
        }
        let mut indices = Vec::new();
        let mut accum: Vec<[f64; 3]> = if self.need_normals {
            vec![[0.0; 3]; positions.len()]
        } else {
            Vec::new()
        };
        for face in &self.faces {
            if !face.alive {
                continue;
            }
            let a = remap[face.v[0] as usize];
            let b = remap[face.v[1] as usize];
            let c = remap[face.v[2] as usize];
            let pa = cast64(positions[a as usize]);
            let pb = cast64(positions[b as usize]);
            let pc = cast64(positions[c as usize]);
            let n = cross3(sub3(pb, pa), sub3(pc, pa));
            // 零面积清理
            if dot3(n, n) <= 1e-24 {
                continue;
            }
            if self.need_normals {
                for &v in [a, b, c].iter() {
                    let acc = &mut accum[v as usize];
                    *acc = [acc[0] + n[0], acc[1] + n[1], acc[2] + n[2]];
                }
            }
            indices.extend_from_slice(&[a, b, c]);
        }
        let uvs = self.uv.as_ref().map(|uv| {
            (0..self.pos.len())
                .filter(|&i| self.alive[i])
                .map(|i| [uv[i][0] as f32, uv[i][1] as f32])
                .collect::<Vec<_>>()
        });
        let batch_ids = self.batch_ids.as_ref().map(|ids| {
            (0..self.pos.len())
                .filter(|&i| self.alive[i])
                .map(|i| ids[i])
                .collect::<Vec<_>>()
        });
        let normals = if self.need_normals {
            Some(
                accum
                    .iter()
                    .map(|n| {
                        let len = norm3(*n);
                        if len > 0.0 {
                            [(n[0] / len) as f32, (n[1] / len) as f32, (n[2] / len) as f32]
                        } else {
                            [0.0; 3]
                        }
                    })
                    .collect::<Vec<_>>(),
            )
        } else {
            None
        };
        (positions, indices, uvs, normals, batch_ids)
    }
}

// ---------------------------------------------------------------------------
// 单元测试
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::SimplifyError;
    use crate::simplify;

    /// n×n 平面网格（z=0，(n+1)² 顶点、2n² 面），可选 uv = (x, y)。
    fn plane_grid(n: usize, with_uv: bool) -> MeshData {
        let mut positions = Vec::new();
        for y in 0..=n {
            for x in 0..=n {
                positions.push([x as f32, y as f32, 0.0]);
            }
        }
        let mut indices = Vec::new();
        for y in 0..n {
            for x in 0..n {
                let a = (y * (n + 1) + x) as u32;
                let b = a + 1;
                let c = a + n as u32 + 1;
                let d = c + 1;
                indices.extend_from_slice(&[a, b, c, b, d, c]);
            }
        }
        let uvs = with_uv.then(|| {
            positions.iter().map(|p| [p[0], p[1]]).collect()
        });
        MeshData { positions, indices, normals: None, uvs, batch_ids: None }
    }

    /// 边界顶点索引（z=0 平面网格的周界）。
    fn grid_boundary_vertices(n: usize) -> Vec<usize> {
        let mut v = Vec::new();
        for y in 0..=n {
            for x in 0..=n {
                if x == 0 || x == n || y == 0 || y == n {
                    v.push(y * (n + 1) + x);
                }
            }
        }
        v
    }

    /// 平面网格简化：目标达到、边界顶点逐字不动、几何零漂移。
    #[test]
    fn plane_grid_boundary_fixed_and_target_reached() {
        let n = 10usize; // 200 面
        let mesh = plane_grid(n, false);
        let (out, stats) = simplify(&mesh, 0.5).unwrap();
        assert_eq!(stats.target_faces, 100);
        assert!(stats.target_reached, "平面网格应达到目标面数");
        // 边界顶点逐字不动（瓦片接缝核心保证）
        let bvs = grid_boundary_vertices(n);
        for &i in &bvs {
            let p = mesh.positions[i];
            assert!(
                out.positions.contains(&p),
                "边界顶点 {p:?} 必须原样保留"
            );
        }
        // 平面仍为平面：任意输出顶点 z == 0
        assert!(out.positions.iter().all(|p| p[2] == 0.0), "平面简化不得出平面");
        // 单侧 Hausdorff ≈ 0（原顶点仍在简化表面上）
        let h = max_vertex_to_mesh_distance(&mesh.positions, &out.positions, &out.indices);
        assert!(h < 1e-5, "平面网格简化后顶点漂移应为 0，实测 {h}");
    }

    /// UV 漂移上界：uv = (x, y) 时线性插值后 UV 与位置解析映射一致。
    #[test]
    fn plane_grid_uv_drift_bounded() {
        let n = 10usize;
        let mesh = plane_grid(n, true);
        let (out, _stats) = simplify(&mesh, 0.2).unwrap();
        assert!(out.uvs.is_some());
        let mut drift = 0.0f64;
        for (p, uv) in out.positions.iter().zip(out.uvs.as_ref().unwrap()) {
            drift = drift.max(((p[0] - uv[0]) as f64).abs());
            drift = drift.max(((p[1] - uv[1]) as f64).abs());
        }
        assert!(drift < 1e-4, "UV 漂移应 < 1e-4，实测 {drift}");
    }

    /// 闭合球面：50% / 20% 面数 + 顶点误差上界 + 仍是闭合流形。
    #[test]
    fn sphere_ratios_and_error_bound() {
        let (rings, segs) = (13usize, 24usize);
        let mesh = uv_sphere(1.0, rings, segs);
        let faces_in = mesh.indices.len() / 3;
        assert_eq!(faces_in, segs * (rings - 2) * 2);

        for ratio in [0.5f32, 0.2] {
            let (out, stats) = simplify(&mesh, ratio).unwrap();
            assert!(stats.target_reached, "球面 ratio={ratio} 应达到目标");
            let faces_out = out.indices.len() / 3;
            assert!(faces_out <= stats.target_faces && faces_out >= stats.target_faces - 2,
                "ratio={ratio}: 面数 {faces_out} 应在 [{}, {}]", stats.target_faces - 2, stats.target_faces);

            // 单侧顶点误差上界
            let h = max_vertex_to_mesh_distance(&mesh.positions, &out.positions, &out.indices);
            // 实测基线：0.5→0.036、0.2→0.070（r=1 UV 球），上界留 ~40% 余量
            let bound = if ratio == 0.5 { 0.05 } else { 0.10 };
            assert!(h < bound, "ratio={ratio}: Hausdorff {h} 应 < {bound}");

            // 仍是闭合流形：每条边恰 2 面
            let mut edges = std::collections::HashMap::new();
            for f in out.indices.chunks_exact(3) {
                for i in 0..3 {
                    let key = (f[i].min(f[(i + 1) % 3]), f[i].max(f[(i + 1) % 3]));
                    *edges.entry(key).or_insert(0usize) += 1;
                }
            }
            assert!(edges.values().all(|&c| c == 2), "简化后球面必须闭合");
            // 半径不爆
            let rmax = out
                .positions
                .iter()
                .map(|p| (p[0] * p[0] + p[1] * p[1] + p[2] * p[2]).sqrt())
                .fold(0.0f32, f32::max);
            assert!(rmax < 1.05, "球面半径 {rmax} 不应超过 1.05");
        }
    }

    /// 细分立方体：50%/20% 面数 + 顶点误差上界 + 包围盒保持。
    #[test]
    fn subdivided_cube_ratios_and_error_bound() {
        let n = 4usize; // 每面 n×n → 全局 6·2n² = 192 面
        let mesh = subdivided_cube(1.0, n);
        let faces_in = mesh.indices.len() / 3;
        assert_eq!(faces_in, 6 * 2 * n * n);
        // 生成器自检：焊接后的立方体必须是闭合流形（每条边恰 2 面）
        {
            let mut edges = std::collections::HashMap::new();
            for f in mesh.indices.chunks_exact(3) {
                for i in 0..3 {
                    let key = (f[i].min(f[(i + 1) % 3]), f[i].max(f[(i + 1) % 3]));
                    *edges.entry(key).or_insert(0usize) += 1;
                }
            }
            assert!(edges.values().all(|&c| c == 2), "测试立方体必须闭合");
        }
        for (ratio, bound) in [(0.5f32, 0.05f64), (0.2, 0.10)] {
            let (out, stats) = simplify(&mesh, ratio).unwrap();
            assert!(stats.target_reached, "立方体 ratio={ratio} 应达到目标");
            // 顶点误差上界（实测 h=0：平坦面内中点折叠不出表面）
            let h = max_vertex_to_mesh_distance(&mesh.positions, &out.positions, &out.indices);
            assert!(h < bound, "ratio={ratio}: Hausdorff {h} 应 < {bound}");
            // 包围盒保持（单位立方体 [0,1]³）
            let eps = 1e-4f32;
            for p in &out.positions {
                for a in p {
                    assert!(-eps <= *a && *a <= 1.0 + eps, "顶点 {p:?} 越出包围盒");
                }
            }
        }
    }

    /// 两块相邻 tile（共享一条边的两个网格）各自简化后：接缝顶点
    /// 位置逐字不变（瓦片接缝不开裂的核心验证）。
    #[test]
    fn two_adjacent_tiles_seam_vertices_immovable() {
        let n = 8usize;
        let tile_a = plane_grid(n, false);
        // tile_b：在 x 方向平移 n（与 tile_a 共享 x = n 那条边线）
        let mut tile_b = plane_grid(n, false);
        for p in &mut tile_b.positions {
            p[0] += n as f32;
        }
        let (out_a, _) = simplify(&tile_a, 0.3).unwrap();
        let (out_b, _) = simplify(&tile_b, 0.3).unwrap();

        // 共享接缝 = tile_a 的 x == n 边界顶点（也是 tile_b 的 x == n 边界顶点）
        let seam: Vec<[f32; 3]> = (0..=n)
            .map(|y| tile_a.positions[y * (n + 1) + n])
            .collect();
        for p in &seam {
            assert!(
                out_a.positions.contains(p),
                "tile_a 接缝顶点 {p:?} 被移动"
            );
            assert!(
                out_b.positions.contains(p),
                "tile_b 接缝顶点 {p:?} 被移动"
            );
        }
    }

    /// 退化输入：重复索引面被剔除，输出无 NaN、索引合法。
    #[test]
    fn degenerate_input_cleaned() {
        let mesh = MeshData {
            positions: vec![[0.0, 0.0, 0.0], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0], [1.0, 1.0, 0.0]],
            indices: vec![0, 1, 2, 0, 0, 0, 1, 2, 3],
            normals: None,
            uvs: None,
            batch_ids: None,
        };
        let (out, _stats) = simplify(&mesh, 1.0).unwrap();
        assert!(out.positions.iter().all(|p| p.iter().all(|x| x.is_finite())));
        for i in &out.indices {
            assert!((*i as usize) < out.positions.len());
        }
        for f in out.indices.chunks_exact(3) {
            assert!(f[0] != f[1] && f[1] != f[2] && f[0] != f[2], "输出不得含退化面");
        }
        // ratio=1 输入非退化面保留
        assert_eq!(out.indices.len() / 3, 2);
    }

    /// 普通立方体（12 面）：QEM 收缩 + 4 面下限语义 + 拓扑合法。
    /// 闭合立方体受「面死亡产生新边界」约束，未必达到 4 面目标——
    /// 这里断言的是收缩单调性与输出合法性（target_reached 反映真实行为）。
    #[test]
    fn plain_cube_shrinks_with_valid_topology() {
        let mesh = subdivided_cube(1.0, 1); // 12 面
        let (out, stats) = simplify(&mesh, 0.2).unwrap();
        assert_eq!(stats.target_faces, 4);
        let faces_out = out.indices.len() / 3;
        assert!(faces_out <= 12, "面数不得增加");
        assert!(faces_out >= 4, "不得低于 4 面下限");
        assert_eq!(stats.target_reached, faces_out <= 4);
        assert!(out.positions.len() <= mesh.positions.len());
        for f in out.indices.chunks_exact(3) {
            assert!(f[0] != f[1] && f[1] != f[2] && f[0] != f[2]);
        }
        assert!(out.positions.iter().all(|p| {
            p.iter().all(|a| (-1e-4..=1.0 + 1e-4).contains(a))
        }));
    }

    /// 输入带法线时输出法线重算：单位立方体面法线为 ±坐标轴。
    #[test]
    fn normals_recomputed_area_weighted() {
        let mut mesh = subdivided_cube(1.0, 2);
        mesh.normals = Some(vec![[0.0, 0.0, 1.0]; mesh.positions.len()]);
        let (out, _stats) = simplify(&mesh, 0.5).unwrap();
        let ns = out.normals.expect("输入带法线则输出必须带法线");
        assert_eq!(ns.len(), out.positions.len());
        for n in &ns {
            let len2 = (n[0] * n[0] + n[1] * n[1] + n[2] * n[2]) as f64;
            // 归一化法线；零向量仅允许出现在「对蹠法线抵消」的 pinch 顶点
            //（面积加权平均的既定回退，见 compact()）
            assert!(len2 < 1e-5 || (1.0 - len2).abs() < 1e-5, "法线应归一化或零回退: {n:?}");
        }
    }

    /// batch_ids 透传：幸存顶点保留自身 feature id，数量与顶点一致。
    #[test]
    fn batch_ids_carried_through() {
        let mut mesh = plane_grid(6, false);
        mesh.batch_ids = Some(mesh.positions.iter().map(|p| (p[0] / 2.0) as u32).collect());
        let (out, _stats) = simplify(&mesh, 0.3).unwrap();
        let ids = out.batch_ids.expect("输入带 batch_ids 则输出必须带");
        assert_eq!(ids.len(), out.positions.len());
        // 幸存顶点的 batch id 必须来自输入顶点集合（纯透传，无插值）
        let valid: HashSet<u32> = mesh.batch_ids.as_ref().unwrap().iter().copied().collect();
        for id in &ids {
            assert!(valid.contains(id), "batch id {id} 不在输入集合中");
        }
    }

    /// 错误路径：NaN 比率（PartialEq 对 NaN 失效，用 matches! 断言）。
    #[test]
    fn nan_ratio_rejected() {
        let mesh = plane_grid(2, false);
        assert!(matches!(
            simplify(&mesh, f32::NAN),
            Err(SimplifyError::InvalidRatio { .. })
        ));
    }

    /// 单位立方体（边长 `size`，原点在 [0,0,0]），每面 n×n 细分。
    /// 面间共享顶点按量化坐标焊接（n 为 2 的幂时坐标可精确表示），
    /// 保证闭合流形——不焊接的 6 片独立网格会全是假边界。
    fn subdivided_cube(size: f32, n: usize) -> MeshData {
        let mut positions: Vec<[f32; 3]> = Vec::new();
        let mut index_of: std::collections::HashMap<[i64; 3], u32> = std::collections::HashMap::new();
        let mut indices: Vec<u32> = Vec::new();
        let step = size / n as f32;
        let quant = |x: f32| (x / step).round() as i64;
        // 6 面：每面由原点 o 与两个边向量 u、v 张成（u×v 朝外）
        let faces: [([f32; 3], [f32; 3], [f32; 3]); 6] = [
            ([0.0, 0.0, size], [size, 0.0, 0.0], [0.0, size, 0.0]),   // +z
            ([size, 0.0, 0.0], [-size, 0.0, 0.0], [0.0, size, 0.0]),  // -z
            ([0.0, size, size], [size, 0.0, 0.0], [0.0, 0.0, -size]), // +y
            ([0.0, 0.0, 0.0], [size, 0.0, 0.0], [0.0, 0.0, size]),    // -y
            ([size, 0.0, size], [0.0, 0.0, -size], [0.0, size, 0.0]), // +x
            ([0.0, 0.0, 0.0], [0.0, 0.0, size], [0.0, size, 0.0]),    // -x
        ];
        for (o, u, v) in faces {
            // 面内顶点全局焊接
            let mut local = vec![0u32; (n + 1) * (n + 1)];
            for iy in 0..=n {
                for ix in 0..=n {
                    let fu = ix as f32 / n as f32;
                    let fv = iy as f32 / n as f32;
                    let p = [
                        o[0] + u[0] * fu + v[0] * fv,
                        o[1] + u[1] * fu + v[1] * fv,
                        o[2] + u[2] * fu + v[2] * fv,
                    ];
                    let key = [quant(p[0]), quant(p[1]), quant(p[2])];
                    let id = match index_of.get(&key) {
                        Some(&i) => i,
                        None => {
                            let i = positions.len() as u32;
                            positions.push(p);
                            index_of.insert(key, i);
                            i
                        }
                    };
                    local[iy * (n + 1) + ix] = id;
                }
            }
            let idx = |ix: usize, iy: usize| local[iy * (n + 1) + ix];
            for iy in 0..n {
                for ix in 0..n {
                    let a = idx(ix, iy);
                    let b = idx(ix + 1, iy);
                    let c = idx(ix, iy + 1);
                    let d = idx(ix + 1, iy + 1);
                    indices.extend_from_slice(&[a, b, c, b, d, c]);
                }
            }
        }
        MeshData { positions, indices, normals: None, uvs: None, batch_ids: None }
    }

    /// UV 球（半径 r，rings = 纬度行数含两极，segs = 经度分段）。
    fn uv_sphere(r: f32, rings: usize, segs: usize) -> MeshData {
        let mut positions = vec![[0.0, r, 0.0]]; // 北极
        for ring in 1..rings - 1 {
            let phi = std::f64::consts::PI * ring as f64 / (rings - 1) as f64;
            for s in 0..segs {
                let theta = 2.0 * std::f64::consts::PI * s as f64 / segs as f64;
                positions.push([
                    (phi.sin() * theta.cos()) as f32 * r,
                    (phi.cos()) as f32 * r,
                    (phi.sin() * theta.sin()) as f32 * r,
                ]);
            }
        }
        positions.push([0.0, -r, 0.0]); // 南极
        let north = 0u32;
        let south = positions.len() as u32 - 1;
        let mut indices = Vec::new();
        // 北极帽
        for s in 0..segs as u32 {
            let a = 1 + s;
            let b = 1 + (s + 1) % segs as u32;
            indices.extend_from_slice(&[north, a, b]);
        }
        // 中间带
        for ring in 0..rings - 3 {
            let top = 1 + (ring * segs) as u32;
            let bot = top + segs as u32;
            for s in 0..segs as u32 {
                let sn = (s + 1) % segs as u32;
                indices.extend_from_slice(&[top + s, bot + sn, bot + s]);
                indices.extend_from_slice(&[top + s, top + sn, bot + sn]);
            }
        }
        // 南极帽
        let last_ring_start = 1 + ((rings - 3) * segs) as u32;
        for s in 0..segs as u32 {
            let a = last_ring_start + s;
            let b = last_ring_start + (s + 1) % segs as u32;
            indices.extend_from_slice(&[south, b, a]);
        }
        MeshData { positions, indices, normals: None, uvs: None, batch_ids: None }
    }
}
