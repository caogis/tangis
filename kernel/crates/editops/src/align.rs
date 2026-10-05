//! ground-align（地面对齐）：把 bbox 区域内最低点整体平移对齐到目标高程
//! （或参考 tile 同区域最低点）。刚性平移（z 方向统一偏移），不缩放——
//! 缩放会引入非刚性变形与 tile 接缝错位，此处明确不做（见 README 限制）。

use serde::Serialize;

use tangis_osgb::Mesh;

/// 对齐参数：水平 bbox + 目标高程。
#[derive(Debug, Clone, Serialize)]
pub struct AlignParams {
    pub minx: f64,
    pub miny: f64,
    pub maxx: f64,
    pub maxy: f64,
    /// 目标高程（米，内部 z-up）。
    pub target: f64,
}

impl AlignParams {
    pub fn validate(&self) -> Result<(), String> {
        if self.minx >= self.maxx || self.miny >= self.maxy {
            return Err(format!(
                "bbox 非法：minx={} maxx={} miny={} maxy={}（要求 min < max）",
                self.minx, self.maxx, self.minx, self.maxy
            ));
        }
        Ok(())
    }
}

/// 单 tile 对齐统计。
#[derive(Debug, Clone, Default, Serialize)]
pub struct AlignStats {
    /// 施加的 z 平移量（target − 区域最低点）。
    pub dz: f64,
    pub z_min_before: Option<f64>,
    pub z_min_after: Option<f64>,
    /// 区域内顶点数。
    pub region_vertices: u64,
}

fn in_region(v: &[f32; 3], p: &AlignParams) -> bool {
    let (x, y) = (v[0] as f64, v[1] as f64);
    x >= p.minx && x <= p.maxx && y >= p.miny && y <= p.maxy
}

/// bbox 区域内（x/y 落在矩形内，含边界）最低 z；区域无顶点返回 None。
pub fn region_min_z(meshes: &[Mesh], p: &AlignParams) -> Option<f64> {
    let mut zmin = f64::INFINITY;
    let mut any = false;
    for m in meshes {
        for v in &m.vertices {
            if in_region(v, p) {
                zmin = zmin.min(v[2] as f64);
                any = true;
            }
        }
    }
    if any {
        Some(zmin)
    } else {
        None
    }
}

/// 区域最低点平移对齐到 target：整 tile 全部顶点统一加 dz（刚性平移，
/// 区域外随之起伏，保证 tile 内部不撕裂）。
pub fn align_meshes(meshes: &mut [Mesh], params: &AlignParams) -> Result<AlignStats, String> {
    params.validate()?;
    let zmin = region_min_z(meshes, params)
        .ok_or_else(|| "bbox 区域内没有任何顶点，无法地面对齐".to_string())?;
    let dz = params.target - zmin;
    let mut n = 0u64;
    for m in meshes.iter_mut() {
        for v in &mut m.vertices {
            if in_region(v, params) {
                n += 1;
            }
            v[2] += dz as f32;
        }
    }
    Ok(AlignStats {
        dz,
        z_min_before: Some(zmin),
        z_min_after: Some(params.target),
        region_vertices: n,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> Vec<Mesh> {
        vec![Mesh {
            vertices: vec![
                [5.0, 5.0, 2.0],   // 区域内最低点
                [5.5, 5.5, 7.0],   // 区域内
                [100.0, 100.0, 4.0], // 区域外
            ],
            indices: vec![0, 1, 2],
            normals: None,
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: Some(vec![0; 3]),
        }]
    }

    fn params(target: f64) -> AlignParams {
        AlignParams { minx: 0.0, miny: 0.0, maxx: 10.0, maxy: 10.0, target }
    }

    /// 区域最低点对齐 target：dz = target − z_min，且平移是全局的
    /// （区域外顶点同步抬升，tile 内部不撕裂）。
    #[test]
    fn align_translates_all_vertices() {
        let mut meshes = fixture();
        let st = align_meshes(&mut meshes, &params(10.0)).unwrap();
        assert!((st.dz - 8.0).abs() < 1e-9);
        assert_eq!(st.z_min_before, Some(2.0));
        assert_eq!(st.z_min_after, Some(10.0));
        assert_eq!(st.region_vertices, 2);
        let v = &meshes[0].vertices;
        assert!((v[0][2] - 10.0).abs() < 1e-6, "最低点应对齐 10");
        assert!((v[1][2] - 15.0).abs() < 1e-6);
        assert!((v[2][2] - 12.0).abs() < 1e-6, "区域外顶点同步平移");
        // 相对高差保持（刚性平移）
        assert!((v[1][2] - v[0][2] - 5.0).abs() < 1e-6);
    }

    /// 下移对齐（dz < 0）同样成立。
    #[test]
    fn align_downward() {
        let mut meshes = fixture();
        let st = align_meshes(&mut meshes, &params(0.0)).unwrap();
        assert!((st.dz + 2.0).abs() < 1e-9);
        assert!((meshes[0].vertices[0][2] - 0.0).abs() < 1e-6);
    }

    /// 区域内无顶点 → 明确报错（宁报错不猜测）。
    #[test]
    fn align_errors_on_empty_region() {
        let mut meshes = fixture();
        let p = AlignParams { minx: -9.0, miny: -9.0, maxx: -1.0, maxy: -1.0, target: 0.0 };
        let err = align_meshes(&mut meshes, &p).unwrap_err();
        assert!(err.contains("没有任何顶点"), "{err}");
    }

    /// region_min_z 独立可用（参考 tile 对齐取目标用）。
    #[test]
    fn region_min_z_scan() {
        let meshes = fixture();
        assert_eq!(region_min_z(&meshes, &params(0.0)), Some(2.0));
        let outside = AlignParams { minx: -9.0, miny: -9.0, maxx: -1.0, maxy: -1.0, target: 0.0 };
        assert_eq!(region_min_z(&meshes, &outside), None);
    }
}
