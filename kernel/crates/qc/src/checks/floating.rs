//! 检测 3：悬浮块（体素哈希连通分量近似聚类）。
//!
//! 顶点按 `voxel_size`（可配，默认 5 m）量化到体素，体素 26 邻域连通
//! 聚成连通分量（近似，两个独立块体素相邻时会合并——宁漏报勿误报）；
//! 顶点数最多的分视为主地面。其余分量满足
//! `min_z − 主分量 max_z > floating_height_above`（可配，默认 30 m）
//! 且包围盒体积占比 < `floating_max_volume_ratio`（可配，默认 0.05）
//! 时判为悬浮（高处 + 小体积，两个条件同时满足才报）。

use std::collections::{BTreeSet, HashMap};

use tangis_osgb::Mesh;

use super::degenerate::f64p;
use crate::report::{FloatingComponent, FloatingReport};

type Voxel = (i64, i64, i64);

pub fn check_floating(
    mesh: &Mesh,
    voxel_size: f64,
    height_above: f64,
    max_volume_ratio: f64,
) -> FloatingReport {
    let vs = if voxel_size > 0.0 { voxel_size } else { 5.0 };
    // 顶点 → 体素
    let mut voxels: HashMap<Voxel, BTreeSet<usize>> = HashMap::new();
    for (i, v) in mesh.vertices.iter().enumerate() {
        let p = f64p(*v);
        let key = ((p[0] / vs).floor() as i64, (p[1] / vs).floor() as i64, (p[2] / vs).floor() as i64);
        voxels.entry(key).or_default().insert(i);
    }
    // 体素 26 邻域 BFS → 分量（分量成员 = 顶点索引集合）
    let mut comp_of: HashMap<Voxel, usize> = HashMap::new();
    let mut comps: Vec<Vec<usize>> = Vec::new();
    for &key in voxels.keys() {
        if comp_of.contains_key(&key) {
            continue;
        }
        let ci = comps.len();
        let mut stack = vec![key];
        comp_of.insert(key, ci);
        let mut members: Vec<usize> = Vec::new();
        while let Some(k) = stack.pop() {
            members.extend(voxels[&k].iter().copied());
            for dx in -1..=1 {
                for dy in -1..=1 {
                    for dz in -1..=1 {
                        let nk = (k.0 + dx, k.1 + dy, k.2 + dz);
                        if nk != k && voxels.contains_key(&nk) && !comp_of.contains_key(&nk) {
                            comp_of.insert(nk, ci);
                            stack.push(nk);
                        }
                    }
                }
            }
        }
        members.sort_unstable();
        members.dedup();
        comps.push(members);
    }

    // 主分量 = 顶点数最大
    let main = comps
        .iter()
        .enumerate()
        .max_by_key(|(_, m)| m.len())
        .map(|(i, _)| i)
        .unwrap_or(0);
    let main_pts: Vec<[f64; 3]> = comps[main].iter().map(|&i| f64p(mesh.vertices[i])).collect();
    let (main_min, main_max) = bbox(&main_pts);
    let main_volume = volume(main_min, main_max);

    let mut flagged = Vec::new();
    for (ci, members) in comps.iter().enumerate() {
        if ci == main {
            continue;
        }
        let pts: Vec<[f64; 3]> = members.iter().map(|&i| f64p(mesh.vertices[i])).collect();
        let (bmin, bmax) = bbox(&pts);
        let clearance = bmin[2] - main_max[2];
        let ratio = if main_volume > 0.0 { volume(bmin, bmax) / main_volume } else { 0.0 };
        if clearance > height_above && ratio < max_volume_ratio {
            flagged.push(FloatingComponent {
                vertices: members.len(),
                bbox_min: bmin,
                bbox_max: bmax,
                clearance,
                volume_ratio: ratio,
                sample_vertex: pts[0],
            });
        }
    }
    FloatingReport { components: comps.len(), main_vertices: comps[main].len(), flagged }
}

fn bbox(pts: &[[f64; 3]]) -> ([f64; 3], [f64; 3]) {
    let mut min = pts[0];
    let mut max = pts[0];
    for p in pts {
        for a in 0..3 {
            min[a] = min[a].min(p[a]);
            max[a] = max[a].max(p[a]);
        }
    }
    (min, max)
}

fn volume(min: [f64; 3], max: [f64; 3]) -> f64 {
    // 厚度取 0.01 m 下限：平地/薄面主分量的包围盒体积退化为 0 时
    // 体积比失去意义，下限兜底后薄块占比按水平投影面积比较
    (max[0] - min[0]).max(0.0)
        * (max[1] - min[1]).max(0.0)
        * (max[2] - min[2]).max(0.01)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ground_plus_block(z: f32) -> Mesh {
        // 主地面：6×6 方片（2 三角，顶点间距 < 2 个体素边长保证连通）；
        // 孤立小块：z 高处的独立小三角
        let mut verts = vec![[0.0, 0.0, 0.0], [6.0, 0.0, 0.0], [6.0, 6.0, 0.0], [0.0, 6.0, 0.0]];
        let mut idx = vec![0, 1, 2, 0, 2, 3];
        let base = verts.len() as u32;
        verts.extend_from_slice(&[[3.0, 3.0, z], [3.5, 3.0, z], [3.0, 3.5, z]]);
        idx.extend_from_slice(&[base, base + 1, base + 2]);
        Mesh { vertices: verts, indices: idx, ..Mesh::default() }
    }

    #[test]
    fn detects_high_small_isolated_block() {
        let r = check_floating(&ground_plus_block(50.0), 5.0, 30.0, 0.05);
        assert_eq!(r.components, 2);
        assert_eq!(r.main_vertices, 4);
        assert_eq!(r.flagged.len(), 1);
        assert!((r.flagged[0].clearance - 50.0).abs() < 1e-9);
        assert_eq!(r.flagged[0].vertices, 3);
    }

    #[test]
    fn low_block_not_flagged() {
        // z=11：高差 11 < 30（z=5/6 时与地面体素相邻会被并成分量）
        let r = check_floating(&ground_plus_block(11.0), 5.0, 30.0, 0.05);
        assert_eq!(r.components, 2, "低块仍是独立分量");
        assert!(r.flagged.is_empty(), "高差不足不判悬浮");
    }

    #[test]
    fn big_block_not_flagged_by_volume_ratio() {
        // 大块：高差够（40 m）但水平投影与主地面同尺寸 → 体积比 1.0 > 5%
        let mut verts = vec![[0.0, 0.0, 0.0], [6.0, 0.0, 0.0], [6.0, 6.0, 0.0], [0.0, 6.0, 0.0]];
        let mut idx = vec![0, 1, 2, 0, 2, 3];
        let base = verts.len() as u32;
        verts.extend_from_slice(&[[0.0, 0.0, 40.0], [6.0, 0.0, 40.0], [6.0, 6.0, 40.0], [0.0, 6.0, 40.0]]);
        idx.extend_from_slice(&[base, base + 1, base + 2, base, base + 2, base + 3]);
        let r = check_floating(&Mesh { vertices: verts, indices: idx, ..Mesh::default() }, 5.0, 30.0, 0.05);
        assert_eq!(r.components, 2);
        assert!(r.flagged.is_empty(), "大体积分量不应判悬浮");
    }

    #[test]
    fn clean_single_component_zero_warnings() {
        let r = check_floating(&ground_plus_block(0.01), 5.0, 30.0, 0.05);
        // z=0.01 的块与地面同体素 → 合并为单分量
        assert_eq!(r.components, 1);
        assert!(r.flagged.is_empty());
    }

    #[test]
    fn threshold_configurable() {
        // 同一块（z=50，面积占比 ≈ 0.7%）：收紧高度阈值到 10 仍命中；
        // 收紧体积比到 0.001（< 实际 0.7%占比）不命中
        let m = ground_plus_block(50.0);
        assert_eq!(check_floating(&m, 5.0, 10.0, 0.05).flagged.len(), 1);
        assert!(check_floating(&m, 5.0, 10.0, 0.001).flagged.is_empty());
    }
}
