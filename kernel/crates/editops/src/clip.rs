//! clip（抠除）：移除平面一侧 / 水平 bbox 区域内的三角形，边界三角形
//! 做切割（Sutherland–Hodgman 式凸多边形二分），切割边界跨面共享新顶点
//! （闭合），变空的 Geometry 整体删除。
//!
//! # 半空间分片（bbox 补集非凸的处理）
//!
//! 平面切割：抠除侧 = 1 个半空间的内侧，外侧部分（凸）即保留区域。
//! bbox 抠除：保留区域 = bbox 补集（非凸），无法用一次 Sutherland–Hodgman
//! 得到。做法：把 bbox 内部表示为 4 个半空间内侧的**交集**，对每个三角形
//! 依次用各半空间做凸二分——外侧碎片（在 bbox 该侧之外 ⇒ 必在 bbox 外）
//! 保留为独立凸片，内侧碎片继续参与后续边界切分；走完全部边界后仍留在
//! 内侧的碎片即被抠除区域。全过程面积守恒：area_in = area_kept + area_removed。

use serde::Serialize;

use tangis_osgb::Mesh;

use crate::geom::{mesh_area, partition_convex, tri_area, HalfSpace, MeshAcc, Vtx, DEGEN_AREA};

/// 抠除区域（内部 Z-up 坐标，米）。
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Region {
    /// 平面 `ax+by+cz=d`：保留 `keep_positive` 指定的一侧
    /// （pos = 保留 f>0 侧，neg = 保留 f<0 侧，f = ax+by+cz-d），另一侧抠除。
    Plane {
        a: f64,
        b: f64,
        c: f64,
        d: f64,
        keep_positive: bool,
    },
    /// 水平 bbox `[minx,miny,maxx,maxy]`（z 不限，竖直长方体）：内部抠除。
    BBox {
        minx: f64,
        miny: f64,
        maxx: f64,
        maxy: f64,
    },
}

impl Region {
    /// 参数合法性：平面法线非零；bbox min < max。
    pub fn validate(&self) -> Result<(), String> {
        match self {
            Region::Plane { a, b, c, .. } => {
                let len = (a * a + b * b + c * c).sqrt();
                if len < 1e-12 {
                    return Err(format!("切割平面法线 ({a},{b},{c}) 模长为零"));
                }
                Ok(())
            }
            Region::BBox { minx, miny, maxx, maxy } => {
                if minx >= maxx || miny >= maxy {
                    return Err(format!(
                        "bbox 非法：minx={minx} maxx={maxx} miny={miny} maxy={maxy}（要求 min < max）"
                    ));
                }
                Ok(())
            }
        }
    }

    /// 抠除侧的半空间集合：内侧（`n·p ≥ d`）= 被抠除区域。
    fn removed_spaces(&self) -> Vec<HalfSpace> {
        match self {
            Region::Plane { a, b, c, d, keep_positive } => {
                let len = (a * a + b * b + c * c).sqrt();
                let (n, dd) = if *keep_positive {
                    // 保留正侧 ⇒ 抠除负侧：内侧取 f ≤ 0，即 (-n)·p ≥ -d
                    ([-a / len, -b / len, -c / len], -d / len)
                } else {
                    ([a / len, b / len, c / len], d / len)
                };
                vec![HalfSpace { n, d: dd }]
            }
            Region::BBox { minx, miny, maxx, maxy } => vec![
                HalfSpace { n: [1.0, 0.0, 0.0], d: *minx },
                HalfSpace { n: [-1.0, 0.0, 0.0], d: -*maxx },
                HalfSpace { n: [0.0, 1.0, 0.0], d: *miny },
                HalfSpace { n: [0.0, -1.0, 0.0], d: -*maxy },
            ],
        }
    }
}

/// 单 tile 的抠除细节（与 op 对应的算子统计；顶点/面总数在 TileOps 通用
/// 字段中，不在此重复——留痕 JSON 展开时避免同名键）。
#[derive(Debug, Clone, Default, Serialize)]
pub struct ClipStats {
    /// 完全位于抠除侧的面数。
    pub faces_removed: u64,
    /// 跨切割边界被切分的面数（保留部分为切割多边形的扇形三角化）。
    pub faces_split: u64,
    /// 切割边界新增顶点数（跨面去重后；闭合边界的顶点被相邻面共享）。
    pub new_boundary_vertices: u64,
    /// 变空被删除的 Geometry（网格）数。
    pub empty_geometries_removed: u64,
    /// 输入总面积。
    pub area_in: f64,
    /// 保留部分面积（area_in − area_kept = 被抠除面积，面积守恒）。
    pub area_kept: f64,
}

/// 对一组网格（一个 tile 的全部 Geometry）执行抠除，变空网格就地删除。
pub fn clip_meshes(meshes: &mut Vec<Mesh>, region: &Region) -> Result<ClipStats, String> {
    region.validate()?;
    let spaces = region.removed_spaces();
    let mut stats = ClipStats::default();
    let mut kept = Vec::with_capacity(meshes.len());
    for mesh in meshes.drain(..) {
        stats.area_in += mesh_area(&mesh);
        let out = clip_one_mesh(&mesh, &spaces, &mut stats);
        if out.indices.is_empty() {
            stats.empty_geometries_removed += 1;
        } else {
            kept.push(out);
        }
    }
    *meshes = kept;
    Ok(stats)
}

fn vtx_of(mesh: &Mesh, i: u32) -> Vtx {
    let p = mesh.vertices[i as usize];
    Vtx {
        pos: [p[0] as f64, p[1] as f64, p[2] as f64],
        normal: mesh.normals.as_ref().map(|ns| ns[i as usize]),
        uv: mesh.uvs.as_ref().map(|us| us[i as usize]),
        batch: mesh.batch_ids.as_ref().map_or(0, |bs| bs[i as usize]),
    }
}

fn clip_one_mesh(mesh: &Mesh, spaces: &[HalfSpace], stats: &mut ClipStats) -> Mesh {
    let mut acc = MeshAcc::new(mesh);
    // 预注册全部原顶点：切割点与其坐标重合时直接复用原顶点；
    // 之后 new_vertices 只统计真正新增的切割点
    let orig: Vec<Vtx> = (0..mesh.vertices.len() as u32)
        .map(|i| vtx_of(mesh, i))
        .collect();
    let base_new = {
        for v in &orig {
            acc.intern(v);
        }
        acc.new_vertices()
    };
    for tri in mesh.indices.chunks_exact(3) {
        let poly = [orig[tri[0] as usize].clone(), orig[tri[1] as usize].clone(), orig[tri[2] as usize].clone()];
        // pieces：目前仍落在全部已处理边界内侧的凸片（候选抠除区）；
        // keepers：已确定落在至少一条边界外侧的凸片（保留）
        let mut pieces: Vec<Vec<Vtx>> = vec![poly.to_vec()];
        let mut keepers: Vec<Vec<Vtx>> = Vec::new();
        let mut split = false;
        for hs in spaces {
            let mut next = Vec::new();
            for p in pieces.drain(..) {
                let (inn, out) = partition_convex(&p, hs);
                if !out.is_empty() {
                    if !inn.is_empty() {
                        split = true;
                    }
                    keepers.push(out);
                }
                if !inn.is_empty() {
                    next.push(inn);
                }
            }
            pieces = next;
            if pieces.is_empty() {
                break;
            }
        }
        if keepers.is_empty() {
            stats.faces_removed += 1;
            continue;
        }
        if split {
            stats.faces_split += 1;
        }
        for kp in &keepers {
            if kp.len() < 3 {
                continue;
            }
            let mut ids = Vec::with_capacity(kp.len());
            for v in kp {
                let (id, _) = acc.intern(v);
                ids.push(id);
            }
            // 凸多边形扇形三角化（丢弃退化碎片）
            for i in 1..ids.len() - 1 {
                let area = tri_area(&kp[0].pos, &kp[i].pos, &kp[i + 1].pos);
                if area <= DEGEN_AREA {
                    continue;
                }
                acc.push_tri(ids[0], ids[i], ids[i + 1]);
                stats.area_kept += area;
            }
        }
    }
    stats.new_boundary_vertices += (acc.new_vertices() - base_new) as u64;
    acc.finish()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// [0,s]² 正方形（2 三角形）。
    fn square(s: f32) -> Mesh {
        Mesh {
            vertices: vec![[0.0, 0.0, 0.0], [s, 0.0, 0.0], [s, s, 0.0], [0.0, s, 0.0]],
            indices: vec![0, 1, 2, 0, 2, 3],
            normals: None,
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: Some(vec![0, 0, 0, 0]),
        }
    }

    /// 平面 x=5，保留 x<5（抠除正侧）。
    fn cut_x5() -> Region {
        Region::Plane { a: 1.0, b: 0.0, c: 0.0, d: 5.0, keep_positive: false }
    }

    /// 平面切割：面积减半、切割点去重、边界闭合、保留侧顶点幸存。
    #[test]
    fn clip_plane_half_area_boundary_closed() {
        let mut meshes = vec![square(10.0)];
        let st = clip_meshes(&mut meshes, &cut_x5()).unwrap();
        // 输入：正方形 2 面 4 顶点
        assert_eq!(st.faces_removed, 0, "两个三角形都被切割");
        assert_eq!(st.faces_split, 2);
        // tri1 保留四边形 → 2 面；tri2 保留三角形 → 1 面
        let out = &meshes[0];
        assert_eq!(out.indices.len() / 3, 3);
        // 原顶点幸存 2 个：(0,0),(0,10)；新增切割点 3 个：(5,0),(5,5),(5,10)
        assert_eq!(out.vertices.len(), 5);
        assert_eq!(st.new_boundary_vertices, 3);
        assert!((st.area_in - 100.0).abs() < 1e-9);
        assert!((st.area_kept - 50.0).abs() < 1e-9, "area_kept={}", st.area_kept);

        let m = &meshes[0];
        // 边界闭合：每个切割点恰好落在切割平面上（x=5），且全局去重
        // （同一几何点不会因相邻面重复切割而产生多个副本）
        for x in [5.0f32; 3] {
            let cnt = m.vertices.iter()
                .filter(|v| v[0] == x && v[1] == 0.0)
                .count();
            assert!(cnt <= 1, "切割点 (5,0) 应去重，实际 {} 个副本", cnt);
        }
        let on_plane: Vec<[f32; 2]> = m.vertices.iter()
            .filter(|v| v[0] == 5.0)
            .map(|v| [v[0], v[1]])
            .collect();
        assert_eq!(on_plane.len(), 3, "切割点应恰为 (5,0),(5,5),(5,10)，实际 {on_plane:?}");
        // 幸存顶点全部在保留侧
        assert!(m.vertices.iter().all(|v| v[0] <= 5.0 + 1e-6));
    }

    /// keep_positive：保留 x>5 一侧（方向翻转语义）。
    #[test]
    fn clip_plane_keep_positive_side() {
        let mut meshes = vec![square(10.0)];
        let region = Region::Plane { a: 1.0, b: 0.0, c: 0.0, d: 5.0, keep_positive: true };
        let st = clip_meshes(&mut meshes, &region).unwrap();
        assert!((st.area_kept - 50.0).abs() < 1e-9);
        assert!(meshes[0].vertices.iter().all(|v| v[0] >= 5.0 - 1e-6));
    }

    /// bbox 抠除：面积守恒（中心 4×4 挖除 → 保留 96）、两三角形都跨界。
    #[test]
    fn clip_bbox_area_conservation() {
        let mut meshes = vec![square(10.0)];
        let region = Region::BBox { minx: 4.0, miny: 4.0, maxx: 6.0, maxy: 6.0 };
        let st = clip_meshes(&mut meshes, &region).unwrap();
        assert!(meshes[0].indices.len() / 3 >= 4, "两个三角形都应被切成多块碎片");
        assert_eq!(st.faces_removed, 0);
        assert_eq!(st.faces_split, 2);
        assert!((st.area_in - 100.0).abs() < 1e-9);
        assert!(
            (st.area_kept - 96.0).abs() < 1e-6,
            "area_kept={}（期望 96）",
            st.area_kept
        );
        // 幸存顶点全部在 bbox 外
        let m = &meshes[0];
        for v in &m.vertices {
            let out = v[0] <= 4.0 || v[0] >= 6.0 || v[1] <= 4.0 || v[1] >= 6.0;
            assert!(out, "顶点 {v:?} 落在抠除 bbox 内");
        }
    }

    /// 三角形完全落入抠除区域 → 整面删除；全部移除后网格变空被删。
    /// 正方形顶点虽全在 bbox 外，但 bbox 与其内部相交 → 边界切分仍发生。
    #[test]
    fn clip_all_removed_drops_mesh() {
        let small = Mesh {
            vertices: vec![[1.0, 1.0, 0.0], [2.0, 1.0, 0.0], [1.0, 2.0, 0.0]],
            indices: vec![0, 1, 2],
            normals: None,
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: Some(vec![0, 0, 0]),
        };
        let mut meshes = vec![square(10.0), small];
        let region = Region::BBox { minx: 0.5, miny: 0.5, maxx: 3.0, maxy: 3.0 };
        let st = clip_meshes(&mut meshes, &region).unwrap();
        assert_eq!(st.faces_removed, 1, "小三角形整体在 bbox 内");
        assert_eq!(st.empty_geometries_removed, 1, "小三角形网格应被整体删除");
        assert_eq!(meshes.len(), 1);
        assert!((st.area_in - 100.0 - 0.5).abs() < 1e-9);
        // 正方形被切走 bbox 与之交叠部分（2.5×2.5 = 6.25）
        assert!((st.area_kept - (100.0 - 6.25)).abs() < 1e-6, "area_kept={}", st.area_kept);
        assert_eq!(st.faces_split, 2);
    }

    /// 参数校验：零法线平面、min≥max 的 bbox 报错。
    #[test]
    fn clip_region_validation() {
        let mut meshes = vec![square(10.0)];
        let bad_plane = Region::Plane { a: 0.0, b: 0.0, c: 0.0, d: 1.0, keep_positive: true };
        assert!(clip_meshes(&mut meshes, &bad_plane).is_err());
        let bad_bbox = Region::BBox { minx: 5.0, miny: 0.0, maxx: 5.0, maxy: 5.0 };
        assert!(clip_meshes(&mut meshes, &bad_bbox).is_err());
        assert_eq!(meshes.len(), 1, "校验失败不得改动输入");
    }
}
