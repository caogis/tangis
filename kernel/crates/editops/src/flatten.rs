//! flatten（压平）：bbox 区域内顶点 z 置为目标高程（内部 z-up 语义），
//! 区域边界向外 `feather` 米过渡带内按距离线性过渡，避免陡坎。
//!
//! 过渡带数学：`dist` = 顶点到 bbox 的水平欧氏距离（bbox 内为 0），
//! `t = clamp(dist / feather, 0, 1)`，`z' = elevation + (z0 − elevation) × t`
//! ——dist=0 处 z'=elevation，dist≥feather 处 z'=z0（原状），中间严格线性。

use serde::Serialize;

use tangis_osgb::Mesh;

use crate::geom::EPS;

/// 压平参数（水平 bbox + 目标高程 + 过渡带宽）。
#[derive(Debug, Clone, Serialize)]
pub struct FlattenParams {
    pub minx: f64,
    pub miny: f64,
    pub maxx: f64,
    pub maxy: f64,
    /// 目标高程（米，内部 z-up）。
    pub elevation: f64,
    /// 过渡带宽度（米，≥0；0 = 硬边界，区域内直接置平无过渡）。
    pub feather: f64,
}

impl FlattenParams {
    pub fn validate(&self) -> Result<(), String> {
        if self.minx >= self.maxx || self.miny >= self.maxy {
            return Err(format!(
                "bbox 非法：minx={minx} maxx={maxx} miny={miny} maxy={maxy}（要求 min < max）",
                minx = self.minx,
                maxx = self.maxx,
                miny = self.miny,
                maxy = self.maxy
            ));
        }
        if self.feather < 0.0 {
            return Err(format!("--feather {} 非法：须 ≥ 0", self.feather));
        }
        Ok(())
    }
}

/// 单 tile 压平统计。
#[derive(Debug, Clone, Default, Serialize)]
pub struct FlattenStats {
    /// 区域内（dist=0）顶点数。
    pub region_vertices: u64,
    /// z 被改动的顶点数（含过渡带）。
    pub vertices_moved: u64,
    /// 压平前区域顶点 z 范围。
    pub z_min_before: Option<f64>,
    pub z_max_before: Option<f64>,
}

/// 顶点到 bbox 的水平欧氏距离（bbox 内为 0）。
fn dist_to_bbox(x: f64, y: f64, p: &FlattenParams) -> f64 {
    let dx = (p.minx - x).max(x - p.maxx).max(0.0);
    let dy = (p.miny - y).max(y - p.maxy).max(0.0);
    (dx * dx + dy * dy).sqrt()
}

/// 对一组网格执行压平（拓扑不变，仅改顶点 z）。
pub fn flatten_meshes(meshes: &mut [Mesh], params: &FlattenParams) -> Result<FlattenStats, String> {
    params.validate()?;
    let mut st = FlattenStats::default();
    let (mut zmin, mut zmax) = (f64::INFINITY, f64::NEG_INFINITY);
    let mut any_region = false;
    for mesh in meshes.iter_mut() {
        for v in &mut mesh.vertices {
            let (x, y, z0) = (v[0] as f64, v[1] as f64, v[2] as f64);
            let dist = dist_to_bbox(x, y, params);
            if dist > 0.0 && dist >= params.feather {
                continue; // 过渡带之外不动
            }
            if dist == 0.0 {
                any_region = true;
                st.region_vertices += 1;
                zmin = zmin.min(z0);
                zmax = zmax.max(z0);
            }
            let t = if params.feather > 0.0 { dist / params.feather } else { 0.0 };
            let z1 = params.elevation + (z0 - params.elevation) * t;
            if (z1 - z0).abs() > EPS {
                st.vertices_moved += 1;
            }
            v[2] = z1 as f32;
        }
    }
    if any_region {
        st.z_min_before = Some(zmin);
        st.z_max_before = Some(zmax);
    }
    Ok(st)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 一块 z=10 的平地上叠一个尖峰（峰顶在 bbox 中心）。
    fn fixture() -> Vec<Mesh> {
        vec![Mesh {
            vertices: vec![
                [0.0, 0.0, 10.0],  // 0: bbox 外（距 bbox 10）
                [12.0, 5.0, 10.0], // 1: 过渡带外（dist=2 ≥ feather）
                [7.0, 5.0, 10.0],  // 2: 过渡带内（dist=1 → t=0.25）
                [5.0, 5.0, 30.0],  // 3: 区域内（峰顶）
                [6.0, 5.0, 10.0],  // 4: 区域内（边缘角上，dist=0）
            ],
            indices: vec![0, 1, 3, 1, 2, 3, 2, 4, 3],
            normals: None,
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: Some(vec![0; 5]),
        }]
    }

    fn params(feather: f64) -> FlattenParams {
        FlattenParams { minx: 5.0, miny: 5.0, maxx: 6.0, maxy: 6.0, elevation: 5.0, feather }
    }

    /// 区域内 z 全等 elevation；过渡带内线性；带外不动。
    #[test]
    fn flatten_region_equal_and_feather_linear() {
        let mut meshes = fixture();
        let st = flatten_meshes(&mut meshes, &params(4.0)).unwrap();
        let v = &meshes[0].vertices;
        // 区域内全等
        assert_eq!(v[3][2], 5.0);
        assert_eq!(v[4][2], 5.0);
        // 过渡带线性：dist=1, feather=4 → t=0.25 → z = 5 + (10-5)*0.25 = 6.25
        assert!((v[2][2] - 6.25).abs() < 1e-6, "z={}", v[2][2]);
        // 过渡带外不动
        assert_eq!(v[0][2], 10.0);
        assert_eq!(v[1][2], 10.0);
        // 拓扑不变
        assert_eq!(meshes[0].indices.len(), 9);
        assert_eq!(st.region_vertices, 2);
        assert_eq!(st.vertices_moved, 3);
        assert_eq!(st.z_min_before, Some(10.0));
        assert_eq!(st.z_max_before, Some(30.0));
    }

    /// feather=0：硬边界，区域内直接置平，区域外一律不动。
    #[test]
    fn flatten_hard_edge() {
        let mut meshes = fixture();
        let st = flatten_meshes(&mut meshes, &params(0.0)).unwrap();
        let v = &meshes[0].vertices;
        assert_eq!(v[3][2], 5.0);
        assert_eq!(v[4][2], 5.0);
        assert_eq!(v[2][2], 10.0, "dist=1 带外（feather=0）不得改动");
        assert_eq!(st.vertices_moved, 2);
        assert_eq!(st.region_vertices, 2);
    }

    /// bbox 参数校验。
    #[test]
    fn flatten_validates_params() {
        let mut meshes = fixture();
        let bad = FlattenParams { minx: 6.0, miny: 5.0, maxx: 5.0, maxy: 6.0, elevation: 0.0, feather: 1.0 };
        assert!(flatten_meshes(&mut meshes, &bad).is_err());
        let bad = FlattenParams { minx: 5.0, miny: 5.0, maxx: 6.0, maxy: 6.0, elevation: 0.0, feather: -1.0 };
        assert!(flatten_meshes(&mut meshes, &bad).is_err());
    }

    /// 区域内无任何顶点：统计为空但不报错（其余区域照常处理）。
    #[test]
    fn flatten_empty_region_reports_none() {
        let mut meshes = fixture();
        let p = FlattenParams { minx: 100.0, miny: 100.0, maxx: 101.0, maxy: 101.0, elevation: 0.0, feather: 0.0 };
        let st = flatten_meshes(&mut meshes, &p).unwrap();
        assert_eq!(st.region_vertices, 0);
        assert_eq!(st.z_min_before, None);
        assert_eq!(st.vertices_moved, 0);
    }
}
