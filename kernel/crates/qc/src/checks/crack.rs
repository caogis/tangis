//! 检测 4：裂缝初检（相邻 tile 共享边界缝隙）。
//!
//! 按包围盒水平跨度分尺寸类（精细叶 tile 与粗层 LOD tile 不互配），类内
//! 根据实测包围盒位置自动选择配对模式：
//!
//! - **包围盒邻接模式**（生产常见布局：tile 顶点为全局坐标）：两 tile 在
//!   某水平轴上面-面相对（`|A.max − B.min| ≤ 邻接容差`，容差 =
//!   max(1.0 m, 跨度×5%)）且另一轴区间重叠 ≥ 50% 即配对；共享平面 = 相对
//!   两面的中点。带街道沟槽（如本仓库真实语料 20 m 沟槽）的语料不含共享
//!   边界，如实产出零配对，不跨沟槽比较（那测的是地形高差不是缝隙）。
//! - **名下标模式**（纯局部坐标布局：类内全部包围盒重合）：tile 名后缀
//!   `+N_+M` 解析网格下标，Δ=1 相邻配对，按下标 × 跨度平移到全局位置。
//!
//! 边界顶点：到共享平面距离 ≤ 0.01 m（邻接模式按各自朝向面选取）。坐标
//! 先按 1e-4 m 量化再算 3D 距离。对每个 A 侧边界顶点在 B 侧边界顶点中找
//! 最近点（空间哈希，搜索半径 = max(crack_gap, 1.0) m）：
//! - 距离 > `crack_gap`（可配，默认 0.5 m）→ 裂缝，记录边界线段中点
//!   （全局坐标）；
//! - 搜索半径内无 B 侧顶点 → 记 `unmatched_a`（断边证据）。
//!
//! 无法配对的 tile（无后缀下标 / 无同尺寸邻居 / 沟槽布局无共享边界）计入
//! `crack_tiles_unpaired`，不参与配对。

use std::collections::HashMap;

use crate::report::{CrackPairReport, CrackPosition};

/// 边界顶点判定容差（m）：顶点到共享平面距离小于该值视为边界顶点。
const BORDER_EPS: f64 = 0.01;
/// 坐标量化步长（m），任务规约值。
const QUANT: f64 = 1e-4;
/// 裂缝位置清单条数上限（总数仍全量统计）。
const MAX_POSITIONS: usize = 20;
/// 包围盒邻接判定容差系数（相对跨度）与下限（m）。
const ADJ_RATIO: f64 = 0.05;
const ADJ_MIN: f64 = 1.0;

/// 参与裂缝配对的 tile 描述（顶点为文件原生坐标，平移由配对模式决定）。
#[derive(Debug, Clone)]
pub struct CrackTile {
    pub name: String,
    /// 实测包围盒（原生坐标，m）。
    pub bbox_min: [f64; 3],
    pub bbox_max: [f64; 3],
    /// tile 名后缀网格下标（`+N_+M` → (N, M)）。
    pub grid: Option<(i64, i64)>,
    /// 顶点（原生坐标）。
    pub vertices: Vec<[f64; 3]>,
}

impl CrackTile {
    pub fn new(name: &str, bbox_min: [f64; 3], bbox_max: [f64; 3], vertices: Vec<[f64; 3]>) -> Self {
        CrackTile { name: name.to_string(), bbox_min, bbox_max, grid: parse_grid_index(name), vertices }
    }

    fn span(&self, axis: usize) -> f64 {
        (self.bbox_max[axis] - self.bbox_min[axis]).abs()
    }
}

/// 解析 tile 名后缀 `+N_+M` / `-N_+M`（可负）为 (N, M)。
pub fn parse_grid_index(stem: &str) -> Option<(i64, i64)> {
    let (head, b) = stem.rsplit_once('_')?;
    let (head2, a) = head.rsplit_once('_')?;
    if head2.is_empty() {
        return None;
    }
    let parse = |s: &str| -> Option<i64> {
        let (sign, rest) = match s.strip_prefix('-') {
            Some(r) => (-1i64, r),
            None => (1i64, s.strip_prefix('+').unwrap_or(s)),
        };
        if rest.is_empty() || !rest.bytes().all(|c| c.is_ascii_digit()) {
            return None;
        }
        rest.parse::<i64>().ok().map(|v| sign * v)
    };
    Some((parse(a)?, parse(b)?))
}

/// 全部裂缝配对检测。返回 (配对报告, 未配对 tile 数)。
pub fn check_cracks(tiles: &[CrackTile], crack_gap: f64) -> (Vec<CrackPairReport>, usize) {
    // 按尺寸类分组（跨度量化到 mm 作 key）：精细/粗层不互配
    let mut classes: HashMap<(i64, i64), Vec<usize>> = HashMap::new();
    for (i, t) in tiles.iter().enumerate() {
        let key = ((t.span(0) * 1000.0).round() as i64, ((t.span(1)) * 1000.0).round() as i64);
        classes.entry(key).or_default().push(i);
    }

    let mut reports = Vec::new();
    let mut paired = vec![false; tiles.len()];
    for idxs in classes.values() {
        // 模式判定：类内包围盒位置（min 量化到 mm）分散 → 全局坐标布局；
        // 全部重合 → 纯局部坐标布局（走名下标）
        let mut distinct: std::collections::BTreeSet<(i64, i64)> = Default::default();
        for &i in idxs {
            let t = &tiles[i];
            distinct.insert(((t.bbox_min[0] * 1000.0).round() as i64, (t.bbox_min[1] * 1000.0).round() as i64));
        }
        let bbox_mode = distinct.len() > 1;
        if bbox_mode {
            for a in 0..idxs.len() {
                for b in (a + 1)..idxs.len() {
                    let (i, j) = (idxs[a], idxs[b]);
                    if let Some((axis, plane)) = bbox_adjacency(&tiles[i], &tiles[j]) {
                        reports.push(check_pair(&tiles[i], &tiles[j], axis, plane, [0.0; 3], crack_gap));
                        paired[i] = true;
                        paired[j] = true;
                    }
                }
            }
        } else {
            // 名下标模式：下标 → tile（同下标取首个）
            let mut by_grid: HashMap<(i64, i64), usize> = HashMap::new();
            for &i in idxs {
                if let Some(g) = tiles[i].grid {
                    by_grid.entry(g).or_insert(i);
                }
            }
            // 无序对去重：每 tile 只向 +x / +y 邻居配对
            let mut done: std::collections::BTreeSet<(usize, usize)> = Default::default();
            for &i in idxs {
                let Some(g) = tiles[i].grid else { continue };
                for (d, axis) in [((1i64, 0i64), 0usize), ((0i64, 1i64), 1usize)] {
                    let ng = (g.0 + d.0, g.1 + d.1);
                    if let Some(&j) = by_grid.get(&ng) {
                        if done.insert((i.min(j), i.max(j))) {
                            // 布局假设：下标步长 1 = 一个 tile 跨度；
                            // j 顶点按 +跨度 平移后其低面与 i 的高面贴合，
                            // 共享平面 = i 的高面（局部坐标布局下同类 bbox 重合）
                            let span_i = tiles[i].span(axis);
                            let mut shift = [0.0; 3];
                            shift[axis] = span_i;
                            reports.push(check_pair(
                                &tiles[i],
                                &tiles[j],
                                axis,
                                tiles[i].bbox_max[axis],
                                shift,
                                crack_gap,
                            ));
                            paired[i] = true;
                            paired[j] = true;
                        }
                    }
                }
            }
        }
    }
    reports.sort_by(|a, b| (&a.tile_a, &a.tile_b).cmp(&(&b.tile_a, &b.tile_b)));
    let unpaired = paired.iter().filter(|&&p| !p).count();
    (reports, unpaired)
}

/// 包围盒邻接判定：返回 (共享轴, 共享平面全局坐标)。
/// 条件：某轴上 |A.max − B.min| 或 |B.max − A.min| ≤ 容差（面-面相对），
/// 且另一轴区间重叠 ≥ 较小区间的 50%。
fn bbox_adjacency(a: &CrackTile, b: &CrackTile) -> Option<(usize, f64)> {
    let span = a.span(0).max(a.span(1));
    let tol = (span * ADJ_RATIO).max(ADJ_MIN);
    for axis in 0..2usize {
        let other = 1 - axis;
        // 重叠检查（另一轴）
        let ov = (a.bbox_max[other].min(b.bbox_max[other]) - a.bbox_min[other].max(b.bbox_min[other]))
            .max(0.0);
        let min_span = a.span(other).min(b.span(other));
        if min_span <= 0.0 || ov < 0.5 * min_span {
            continue;
        }
        // A 在低侧 / B 在低侧
        let d_ab = (a.bbox_max[axis] - b.bbox_min[axis]).abs();
        let d_ba = (b.bbox_max[axis] - a.bbox_min[axis]).abs();
        if d_ab <= tol {
            return Some((axis, (a.bbox_max[axis] + b.bbox_min[axis]) / 2.0));
        }
        if d_ba <= tol {
            return Some((axis, (b.bbox_max[axis] + a.bbox_min[axis]) / 2.0));
        }
    }
    None
}

/// 一对相邻 tile 的裂缝检测。
/// `axis` = 共享轴；`plane` = 共享平面全局坐标；`shift` = b 顶点平移量
/// （名下标模式为下标×跨度，包围盒模式为零）。
fn check_pair(
    a: &CrackTile,
    b: &CrackTile,
    axis: usize,
    plane: f64,
    shift: [f64; 3],
    crack_gap: f64,
) -> CrackPairReport {
    let shift_v = |v: [f64; 3]| [v[0] + shift[0], v[1] + shift[1], v[2] + shift[2]];
    let on_plane = |v: [f64; 3]| (v[axis] - plane).abs() <= BORDER_EPS;

    let a_border: Vec<[f64; 3]> = a.vertices.iter().copied().filter(|v| on_plane(*v)).collect();
    let b_border: Vec<[f64; 3]> =
        b.vertices.iter().map(|v| shift_v(*v)).filter(|v| on_plane(*v)).collect();

    let search = crack_gap.max(1.0);
    // B 侧边界顶点空间哈希（量化后 y/z 平面）
    let cell = search;
    let mut b_grid: HashMap<(i64, i64), Vec<[f64; 3]>> = HashMap::new();
    for v in &b_border {
        let key = ((v[1] / cell).floor() as i64, (v[2] / cell).floor() as i64);
        b_grid.entry(key).or_default().push(quant(*v));
    }

    let mut gaps: Vec<f64> = Vec::new();
    let mut unmatched = 0usize;
    let mut positions: Vec<CrackPosition> = Vec::new();
    for v in &a_border {
        let vq = quant(*v);
        let (cy, cz) = ((vq[1] / cell).floor() as i64, (vq[2] / cell).floor() as i64);
        let mut best = (f64::INFINITY, vq);
        for dy in -1..=1 {
            for dz in -1..=1 {
                if let Some(list) = b_grid.get(&(cy + dy, cz + dz)) {
                    for w in list {
                        let d = dist3(vq, *w);
                        if d < best.0 {
                            best = (d, *w);
                        }
                    }
                }
            }
        }
        if !best.0.is_finite() {
            unmatched += 1;
            continue;
        }
        gaps.push(best.0);
        if best.0 > crack_gap && positions.len() < MAX_POSITIONS {
            // 裂缝位置 = 边界线段中点（共享轴取平面坐标，另两轴取两点中点）
            let (x, y) = if axis == 0 {
                (plane, (vq[1] + best.1[1]) / 2.0)
            } else {
                ((vq[0] + best.1[0]) / 2.0, plane)
            };
            positions.push(CrackPosition { x, y, z: (vq[2] + best.1[2]) / 2.0, gap: best.0 });
        }
    }

    let over_threshold = gaps.iter().filter(|&&g| g > crack_gap).count() + unmatched;
    gaps.sort_by(|x, y| x.total_cmp(y));
    let (median, mean, max) = if gaps.is_empty() {
        (0.0, 0.0, 0.0)
    } else {
        let n = gaps.len();
        let median = if n % 2 == 1 { gaps[n / 2] } else { (gaps[n / 2 - 1] + gaps[n / 2]) / 2.0 };
        let sum: f64 = gaps.iter().sum();
        (median, sum / n as f64, gaps[n - 1])
    };

    CrackPairReport {
        tile_a: a.name.clone(),
        tile_b: b.name.clone(),
        shared_axis: if axis == 0 { "x".to_string() } else { "y".to_string() },
        plane,
        border_vertices_a: a_border.len(),
        matched_pairs: gaps.len(),
        unmatched_a: unmatched,
        median_gap: median,
        mean_gap: mean,
        max_gap: max,
        gaps_over_threshold: over_threshold,
        positions,
    }
}

// ---- 内部工具 ----

fn quant(v: [f64; 3]) -> [f64; 3] {
    [(v[0] / QUANT).round() * QUANT, (v[1] / QUANT).round() * QUANT, (v[2] / QUANT).round() * QUANT]
}

fn dist3(a: [f64; 3], b: [f64; 3]) -> f64 {
    ((a[0] - b[0]).powi(2) + (a[1] - b[1]).powi(2) + (a[2] - b[2]).powi(2)).sqrt()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_grid_index() {
        assert_eq!(parse_grid_index("Tile_+000_+000"), Some((0, 0)));
        assert_eq!(parse_grid_index("Tile_+003_+007"), Some((3, 7)));
        assert_eq!(parse_grid_index("TileC_-001_+002"), Some((-1, 2)));
        assert_eq!(parse_grid_index("plain"), None);
        assert_eq!(parse_grid_index("Tile_x_y"), None);
        assert_eq!(parse_grid_index("_+001_+002"), None, "空 stem 前缀不算");
    }

    /// 名下标模式：a 覆盖局部 x∈[0,10]，b 同为局部坐标；b 边界 z 偏移 `dz`。
    fn pair_local(dz: f64) -> (CrackTile, CrackTile) {
        let mut av = Vec::new();
        let mut bv = Vec::new();
        for i in 0..11 {
            let t = i as f64;
            av.push([t, 0.0, 0.0]);
            av.push([t, 10.0, 0.0]);
            // b 的共享边界（局部 x=0）z 偏移 dz
            bv.push([t, 0.0, if (t as i32) == 0 { dz } else { 0.0 }]);
            bv.push([t, 10.0, if (t as i32) == 0 { dz } else { 0.0 }]);
        }
        let bb = [0.0, 0.0, 0.0];
        let bt = [10.0, 10.0, 0.0];
        (
            CrackTile::new("Tile_+000_+000", bb, bt, av),
            CrackTile::new("Tile_+001_+000", bb, bt, bv),
        )
    }

    #[test]
    fn detects_border_gap_local_mode() {
        let (a, b) = pair_local(0.8);
        let (reports, unpaired) = check_cracks(&[a, b], 0.5);
        assert_eq!(unpaired, 0);
        assert_eq!(reports.len(), 1);
        let r = &reports[0];
        assert_eq!(r.shared_axis, "x");
        assert!((r.plane - 10.0).abs() < 1e-9);
        assert_eq!(r.border_vertices_a, 2, "x=10 列的 2 个顶点");
        assert_eq!(r.gaps_over_threshold, 2);
        assert!((r.max_gap - 0.8).abs() < 1e-9);
        assert_eq!(r.positions.len(), 2);
        assert!((r.positions[0].x - 10.0).abs() < 1e-9);
    }

    #[test]
    fn clean_seam_zero_warnings_local_mode() {
        let (a, b) = pair_local(0.0);
        let (reports, _) = check_cracks(&[a, b], 0.5);
        assert_eq!(reports.len(), 1);
        let r = &reports[0];
        assert_eq!(r.gaps_over_threshold, 0);
        assert_eq!(r.unmatched_a, 0);
        assert!(r.positions.is_empty());
        assert!(r.max_gap < 1e-9, "精确贴合缝隙应为 0，实际 {}", r.max_gap);
    }

    /// 包围盒邻接模式：全局坐标布局，a x∈[0,10]，b x∈[10,20]，b 边界 z 偏移 `dz`。
    fn pair_global(dz: f64) -> (CrackTile, CrackTile) {
        let (mut a, mut b) = pair_local(dz);
        a.bbox_max[0] = 10.0;
        // a 全局原点 (0,0)；b 全局原点 (10,0)
        for v in &mut b.vertices {
            v[0] += 10.0;
        }
        b.bbox_min[0] = 10.0;
        b.bbox_max[0] = 20.0;
        (a, b)
    }

    #[test]
    fn detects_border_gap_bbox_mode() {
        let (a, b) = pair_global(0.8);
        let (reports, unpaired) = check_cracks(&[a, b], 0.5);
        assert_eq!(unpaired, 0);
        assert_eq!(reports.len(), 1);
        let r = &reports[0];
        assert_eq!(r.shared_axis, "x");
        assert!((r.plane - 10.0).abs() < 1e-9);
        assert_eq!(r.gaps_over_threshold, 2);
        assert!((r.max_gap - 0.8).abs() < 1e-9);
    }

    #[test]
    fn clean_seam_zero_warnings_bbox_mode() {
        let (a, b) = pair_global(0.0);
        let (reports, _) = check_cracks(&[a, b], 0.5);
        assert_eq!(reports.len(), 1);
        assert_eq!(reports[0].gaps_over_threshold, 0);
        assert!(reports[0].max_gap < 1e-9);
    }

    #[test]
    fn gutter_layout_not_paired() {
        // 真实语料布局：20 m 街道沟槽 → 无共享边界，如实零配对
        // （a x∈[0,10]，b x∈[30,40]）
        let (a, mut b) = pair_global(0.0);
        for v in &mut b.vertices {
            v[0] += 20.0;
        }
        b.bbox_min[0] = 30.0;
        b.bbox_max[0] = 40.0;
        let (reports, unpaired) = check_cracks(&[a, b], 0.5);
        assert!(reports.is_empty(), "沟槽布局不应配对");
        assert_eq!(unpaired, 2);
    }

    #[test]
    fn different_span_classes_not_paired() {
        let (a, mut b) = pair_local(0.0);
        b.bbox_max[0] = 20.0; // 尺寸类不同（粗层 vs 精细）
        let (reports, _) = check_cracks(&[a, b], 0.5);
        assert!(reports.is_empty(), "不同尺寸类不配对");
    }

    #[test]
    fn unnamed_tiles_counted_unpaired() {
        let (a, b) = pair_local(0.0);
        let mut c = a.clone();
        c.name = "plain".into();
        c.grid = None;
        let (reports, unpaired) = check_cracks(&[c, a, b], 0.5);
        assert_eq!(unpaired, 1);
        assert_eq!(reports.len(), 1, "有效对仍配对");
    }

    #[test]
    fn bbox_mode_and_index_mode_agree_on_clean_and_gap() {
        // 两种模式对同一几何（全局=平移后的局部）结论一致
        let (la, lb) = pair_local(0.6);
        let (ga, gb) = pair_global(0.6);
        let (lr, _) = check_cracks(&[la, lb], 0.5);
        let (gr, _) = check_cracks(&[ga, gb], 0.5);
        assert_eq!(lr[0].gaps_over_threshold, gr[0].gaps_over_threshold);
        assert!((lr[0].max_gap - gr[0].max_gap).abs() < 1e-9);
    }
}
