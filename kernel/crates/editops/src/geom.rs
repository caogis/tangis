//! 编辑算子的几何内核：半空间凸多边形二分（Sutherland–Hodgman 双向）+
//! 切割顶点去重累加器。坐标一律为内部 Z-up 局部米制（x/y 水平、z 高度）。

use std::collections::{BTreeMap, BTreeSet};

use tangis_osgb::{InlineTexture, Mesh};

/// 点落在切割边界带内的判定容差（米）。
pub(crate) const EPS: f64 = 1e-6;
/// 扇形三角化时丢弃的退化三角形面积阈（m²）。
pub(crate) const DEGEN_AREA: f64 = 1e-12;
/// 新顶点坐标去重量化步长：坐标 ×1e5 取整（0.01 mm 分辨率）。
const QUANT: f64 = 1e5;

/// 半空间：内侧为 `n·p ≥ d`（n 为单位法线时 d 为原点到平面的距离）。
#[derive(Debug, Clone, Copy)]
pub(crate) struct HalfSpace {
    pub n: [f64; 3],
    pub d: f64,
}

impl HalfSpace {
    /// 点到平面的带符号距离（单位法线时）。
    pub fn f(&self, p: &[f64; 3]) -> f64 {
        self.n[0] * p[0] + self.n[1] * p[1] + self.n[2] * p[2] - self.d
    }
}

/// 切分时携带属性的顶点记录（切割插值点上属性线性过渡；
/// 属性保持网格原生的 f32 精度，位置用 f64 做切割数学）。
#[derive(Debug, Clone)]
pub(crate) struct Vtx {
    pub pos: [f64; 3],
    pub normal: Option<[f32; 3]>,
    pub uv: Option<[f32; 2]>,
    pub batch: u32,
}

fn lerp3(a: [f64; 3], b: [f64; 3], t: f64) -> [f64; 3] {
    [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t, a[2] + (b[2] - a[2]) * t]
}

fn lerpf3(a: [f32; 3], b: [f32; 3], t: f32) -> [f32; 3] {
    [
        a[0] + (b[0] - a[0]) * t,
        a[1] + (b[1] - a[1]) * t,
        a[2] + (b[2] - a[2]) * t,
    ]
}

fn lerpf2(a: [f32; 2], b: [f32; 2], t: f32) -> [f32; 2] {
    [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t]
}

/// 切割插值点：位置/UV 线性插值，法线插值后归一化，BATCHID 取边起点。
fn lerp_vtx(a: &Vtx, b: &Vtx, t: f64) -> Vtx {
    let tf = t as f32;
    let normal = match (a.normal, b.normal) {
        (Some(na), Some(nb)) => {
            let n = lerpf3(na, nb, tf);
            let len = (n[0] * n[0] + n[1] * n[1] + n[2] * n[2]).sqrt();
            if len > 1e-12 {
                Some([n[0] / len, n[1] / len, n[2] / len])
            } else {
                Some(na)
            }
        }
        _ => None,
    };
    Vtx {
        pos: lerp3(a.pos, b.pos, t),
        normal,
        uv: match (a.uv, b.uv) {
            (Some(ua), Some(ub)) => Some(lerpf2(ua, ub, tf)),
            _ => None,
        },
        batch: a.batch,
    }
}

/// 三角形面积（3D 叉积模的一半）。
pub(crate) fn tri_area(a: &[f64; 3], b: &[f64; 3], c: &[f64; 3]) -> f64 {
    let u = [b[0] - a[0], b[1] - a[1], b[2] - a[2]];
    let v = [c[0] - a[0], c[1] - a[1], c[2] - a[2]];
    let cx = u[1] * v[2] - u[2] * v[1];
    let cy = u[2] * v[0] - u[0] * v[2];
    let cz = u[0] * v[1] - u[1] * v[0];
    0.5 * (cx * cx + cy * cy + cz * cz).sqrt()
}

/// 网格总表面积（逐三角形累加）。
pub(crate) fn mesh_area(mesh: &Mesh) -> f64 {
    mesh.indices
        .chunks_exact(3)
        .map(|t| {
            let (a, b, c) = (
                &mesh.vertices[t[0] as usize],
                &mesh.vertices[t[1] as usize],
                &mesh.vertices[t[2] as usize],
            );
            tri_area(
                &[a[0] as f64, a[1] as f64, a[2] as f64],
                &[b[0] as f64, b[1] as f64, b[2] as f64],
                &[c[0] as f64, c[1] as f64, c[2] as f64],
            )
        })
        .sum()
}

/// 面积加权顶点法线重算（与 tangis-simplify 同策略）：顶点法线 =
/// 邻接三角形未归一化叉积（模长 = 2×面积，即面积权重）求和后归一化；
/// 零向量（对蹠法线抵消的 pinch 顶点）按既定回退保持零。
///
/// flatten/align 只改顶点位置不改拓扑，编辑后原法线不再可信——b3dm
/// 输出前对受影响网格整体重算（OBJ 输出不含法线，不受影响）。
pub fn recompute_normals_area_weighted(meshes: &mut [Mesh]) {
    for mesh in meshes.iter_mut() {
        let mut acc: Vec<[f64; 3]> = vec![[0.0; 3]; mesh.vertices.len()];
        for t in mesh.indices.chunks_exact(3) {
            let (a, b, c) = (
                mesh.vertices[t[0] as usize],
                mesh.vertices[t[1] as usize],
                mesh.vertices[t[2] as usize],
            );
            let u = [(b[0] - a[0]) as f64, (b[1] - a[1]) as f64, (b[2] - a[2]) as f64];
            let v = [(c[0] - a[0]) as f64, (c[1] - a[1]) as f64, (c[2] - a[2]) as f64];
            let n = [
                u[1] * v[2] - u[2] * v[1],
                u[2] * v[0] - u[0] * v[2],
                u[0] * v[1] - u[1] * v[0],
            ];
            for &i in t {
                let s = &mut acc[i as usize];
                *s = [s[0] + n[0], s[1] + n[1], s[2] + n[2]];
            }
        }
        mesh.normals = Some(
            acc.iter()
                .map(|s| {
                    let len = (s[0] * s[0] + s[1] * s[1] + s[2] * s[2]).sqrt();
                    if len > 1e-12 {
                        [(s[0] / len) as f32, (s[1] / len) as f32, (s[2] / len) as f32]
                    } else {
                        [0.0; 3]
                    }
                })
                .collect(),
        );
    }
}

/// 凸多边形按半空间二分（Sutherland–Hodgman 双向）：
/// 返回 `(inside, outside)`——inside 为 `n·p ≥ d - EPS` 一侧的多边形
/// （可能为空），outside 为其余部分；两侧结果均保持凸
/// （凸多边形与半空间的交集仍是凸集）。
pub(crate) fn partition_convex(poly: &[Vtx], hs: &HalfSpace) -> (Vec<Vtx>, Vec<Vtx>) {
    let mut inside = Vec::new();
    let mut outside = Vec::new();
    let n = poly.len();
    for i in 0..n {
        let a = &poly[i];
        let b = &poly[(i + 1) % n];
        let (fa, fb) = (hs.f(&a.pos), hs.f(&b.pos));
        let (a_in, b_in) = (fa >= -EPS, fb >= -EPS);
        if a_in {
            inside.push(a.clone());
        } else {
            outside.push(a.clone());
        }
        if a_in != b_in {
            // 跨边界边：按带符号距离比插值出切割点
            let v = lerp_vtx(a, b, fa / (fa - fb));
            inside.push(v.clone());
            outside.push(v);
        }
    }
    (inside, outside)
}

/// 输出网格累加器：顶点按量化坐标去重——切割边界的同一交点被相邻
/// 三角形共享（边界闭合），finish 时压缩未被引用的原顶点。
pub(crate) struct MeshAcc {
    with_normals: bool,
    with_uvs: bool,
    with_batches: bool,
    /// 源网格的纹理引用与内嵌字节（原样透传：atlas 不重排，引用同一张图）。
    texture: Option<String>,
    texture_inline: Option<InlineTexture>,
    vertices: Vec<[f32; 3]>,
    normals: Vec<[f32; 3]>,
    uvs: Vec<[f32; 2]>,
    batches: Vec<u32>,
    keys: BTreeMap<[i64; 3], u32>,
    indices: Vec<u32>,
    new_vertices: usize,
}

impl MeshAcc {
    /// 属性数组形态（法线/UV/BATCHID）跟随源网格：源没有则输出也没有。
    /// 纹理引用/内嵌字节自源网格带出（编辑不改变纹理绑定语义）。
    pub fn new(mesh: &Mesh) -> Self {
        Self {
            with_normals: mesh.normals.is_some(),
            with_uvs: mesh.uvs.is_some(),
            with_batches: mesh.batch_ids.is_some(),
            texture: mesh.texture.clone(),
            texture_inline: mesh.texture_inline.clone(),
            vertices: Vec::new(),
            normals: Vec::new(),
            uvs: Vec::new(),
            batches: Vec::new(),
            keys: BTreeMap::new(),
            indices: Vec::new(),
            new_vertices: 0,
        }
    }

    fn key_of(pos: &[f64; 3]) -> [i64; 3] {
        [
            (pos[0] * QUANT).round() as i64,
            (pos[1] * QUANT).round() as i64,
            (pos[2] * QUANT).round() as i64,
        ]
    }

    /// 注册顶点（坐标去重），返回 (输出顶点索引, 是否新建)。
    pub fn intern(&mut self, v: &Vtx) -> (u32, bool) {
        let key = Self::key_of(&v.pos);
        if let Some(&id) = self.keys.get(&key) {
            return (id, false);
        }
        let id = self.vertices.len() as u32;
        self.vertices.push([v.pos[0] as f32, v.pos[1] as f32, v.pos[2] as f32]);
        if self.with_normals {
            self.normals.push(v.normal.unwrap_or([0.0; 3]));
        }
        if self.with_uvs {
            self.uvs.push(v.uv.unwrap_or([0.0; 2]));
        }
        if self.with_batches {
            self.batches.push(v.batch);
        }
        self.keys.insert(key, id);
        self.new_vertices += 1;
        (id, true)
    }

    pub fn push_tri(&mut self, a: u32, b: u32, c: u32) {
        self.indices.extend_from_slice(&[a, b, c]);
    }

    pub fn new_vertices(&self) -> usize {
        self.new_vertices
    }

    /// 压缩未被引用的顶点后产出网格（纹理引用/内嵌字节自源网格透传，
    /// 不重排 atlas）。
    pub fn finish(self) -> Mesh {
        let used: BTreeSet<u32> = self.indices.iter().copied().collect();
        let mut remap: BTreeMap<u32, u32> = BTreeMap::new();
        let mut vertices = Vec::with_capacity(used.len());
        let mut normals = Vec::new();
        let mut uvs = Vec::new();
        let mut batches = Vec::new();
        for (ni, old) in used.iter().enumerate() {
            let old = *old as usize;
            remap.insert(old as u32, ni as u32);
            vertices.push(self.vertices[old]);
            if self.with_normals {
                normals.push(self.normals[old]);
            }
            if self.with_uvs {
                uvs.push(self.uvs[old]);
            }
            if self.with_batches {
                batches.push(self.batches[old]);
            }
        }
        Mesh {
            vertices,
            indices: self.indices.iter().map(|i| remap[i]).collect(),
            normals: self.with_normals.then_some(normals),
            uvs: self.with_uvs.then_some(uvs),
            texture: self.texture,
            texture_inline: self.texture_inline,
            batch_ids: self.with_batches.then_some(batches),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tri(a: [f64; 3], b: [f64; 3], c: [f64; 3]) -> Vec<Vtx> {
        vec![
            Vtx { pos: a, normal: None, uv: None, batch: 0 },
            Vtx { pos: b, normal: None, uv: None, batch: 0 },
            Vtx { pos: c, normal: None, uv: None, batch: 0 },
        ]
    }

    fn poly_area(poly: &[Vtx]) -> f64 {
        (1..poly.len() - 1)
            .map(|i| tri_area(&poly[0].pos, &poly[i].pos, &poly[i + 1].pos))
            .sum()
    }

    /// 直角三角形 (0,0),(2,0),(0,2) 被 x+y=1 二分：内侧（x+y≥1）1.5、外侧 0.5。
    #[test]
    fn partition_areas_conserved() {
        let poly = tri([0.0, 0.0, 0.0], [2.0, 0.0, 0.0], [0.0, 2.0, 0.0]);
        let hs = HalfSpace { n: [1.0, 1.0, 0.0], d: 1.0 };
        let (inside, outside) = partition_convex(&poly, &hs);
        assert!(!inside.is_empty());
        assert!(!outside.is_empty());
        assert!((poly_area(&inside) - 1.5).abs() < 1e-9, "inside={}", poly_area(&inside));
        assert!((poly_area(&outside) - 0.5).abs() < 1e-9, "outside={}", poly_area(&outside));
        assert_eq!(inside.len(), 4, "切割多边形应为四边形（原 2 顶点 + 2 切点）");
        assert_eq!(outside.len(), 3, "外侧应为三角形（原 1 顶点 + 2 切点）");
    }

    /// 顶点恰好落在切割平面上：不炸、面积守恒、不产生重复切点。
    #[test]
    fn partition_vertex_on_plane() {
        let poly = tri([0.0, 0.0, 0.0], [2.0, 0.0, 0.0], [1.0, 0.0, 5.0]);
        // x=1 平面恰好过第三顶点
        let hs = HalfSpace { n: [1.0, 0.0, 0.0], d: 1.0 };
        let (inside, outside) = partition_convex(&poly, &hs);
        assert!((poly_area(&inside) - 2.5).abs() < 1e-9);
        assert!((poly_area(&outside) - 2.5).abs() < 1e-9);
    }

    /// 全内/全外判定：多边形完全在一侧时另一侧为空。
    #[test]
    fn partition_fully_one_side() {
        let poly = tri([0.0, 0.0, 0.0], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0]);
        let hs = HalfSpace { n: [1.0, 0.0, 0.0], d: 10.0 };
        let (inside, outside) = partition_convex(&poly, &hs);
        assert!(inside.is_empty());
        assert_eq!(outside.len(), 3);

        let hs = HalfSpace { n: [1.0, 0.0, 0.0], d: -10.0 };
        let (inside, outside) = partition_convex(&poly, &hs);
        assert_eq!(inside.len(), 3);
        assert!(outside.is_empty());
    }

    /// MeshAcc：坐标去重 + finish 压缩未引用顶点。
    #[test]
    fn mesh_acc_dedup_and_compact() {
        let mesh = Mesh {
            vertices: vec![[0.0, 0.0, 0.0], [1.0, 0.0, 0.0], [1.0, 1.0, 0.0], [9.0, 9.0, 9.0]],
            indices: vec![0, 1, 2],
            normals: None,
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: Some(vec![7, 7, 7, 7]),
        };
        let mut acc = MeshAcc::new(&mesh);
        for i in 0..4u32 {
            let p = mesh.vertices[i as usize];
            acc.intern(&Vtx { pos: [p[0] as f64, p[1] as f64, p[2] as f64], normal: None, uv: None, batch: 7 });
        }
        // 与顶点 0 重合的插值点 → 复用，不新建
        let (id, created) = acc.intern(&Vtx {
            pos: [0.0, 0.0, 0.0],
            normal: None,
            uv: None,
            batch: 7,
        });
        assert_eq!(id, 0);
        assert!(!created);
        acc.push_tri(0, 1, 2);
        let out = acc.finish();
        assert_eq!(out.vertices.len(), 3, "未引用顶点 3 应被压缩");
        assert_eq!(out.indices, vec![0, 1, 2]);
        assert_eq!(out.batch_ids.as_deref(), Some(&[7u32, 7, 7][..]));
    }

    /// MeshAcc 纹理透传：texture 引用与内嵌字节自源网格原样带出
    /// （M2-F09b：编辑产物接 b3dm 链路，atlas 不重排）。
    #[test]
    fn mesh_acc_carries_texture_and_inline() {
        let mesh = Mesh {
            vertices: vec![[0.0, 0.0, 0.0], [2.0, 0.0, 0.0], [0.0, 2.0, 0.0]],
            indices: vec![0, 1, 2],
            normals: None,
            uvs: Some(vec![[0.0, 0.0], [1.0, 0.0], [0.0, 1.0]]),
            texture: Some("textures/atlas.png".into()),
            texture_inline: Some(InlineTexture::EncodedFile(vec![1, 2, 3])),
            batch_ids: None,
        };
        let mut acc = MeshAcc::new(&mesh);
        for i in 0..3u32 {
            let p = mesh.vertices[i as usize];
            acc.intern(&Vtx {
                pos: [p[0] as f64, p[1] as f64, p[2] as f64],
                normal: None,
                uv: Some(mesh.uvs.as_ref().unwrap()[i as usize]),
                batch: 0,
            });
        }
        acc.push_tri(0, 1, 2);
        let out = acc.finish();
        assert_eq!(out.texture.as_deref(), Some("textures/atlas.png"));
        assert_eq!(out.texture_inline, mesh.texture_inline);
        assert_eq!(out.uvs.as_ref().unwrap().len(), 3);
    }

    /// 面积加权法线重算：平面网格 → 全部 [0,0,1]；尖峰邻域权重随面积变化；
    /// 输出数量与顶点一致。
    #[test]
    fn recompute_normals_area_weighted_flat_is_up() {
        let mut mesh = Mesh {
            vertices: vec![
                [0.0, 0.0, 0.0],
                [10.0, 0.0, 0.0],
                [10.0, 10.0, 0.0],
                [0.0, 10.0, 0.0],
            ],
            indices: vec![0, 1, 2, 0, 2, 3],
            normals: Some(vec![[1.0, 0.0, 0.0]; 4]), // 陈旧法线（应被覆盖）
            uvs: None,
            texture: None,
            texture_inline: None,
            batch_ids: None,
        };
        recompute_normals_area_weighted(std::slice::from_mut(&mut mesh));
        let ns = mesh.normals.as_ref().unwrap();
        assert_eq!(ns.len(), 4);
        for n in ns {
            assert!((n[0]).abs() < 1e-6 && (n[1]).abs() < 1e-6 && (n[2] - 1.0).abs() < 1e-6, "{n:?}");
        }

        // 拉高一个顶点：与其相邻的顶点法线 z 分量下降但保持归一化
        mesh.vertices[2][2] = 5.0;
        recompute_normals_area_weighted(std::slice::from_mut(&mut mesh));
        let ns = mesh.normals.as_ref().unwrap();
        for n in ns {
            let len = (n[0] * n[0] + n[1] * n[1] + n[2] * n[2]).sqrt();
            assert!((len - 1.0).abs() < 1e-6 || len == 0.0, "应归一化: {n:?}");
        }
        assert!(ns[2][2] < 1.0 && ns[0][2] < 1.0, "邻接斜面顶点的法线应偏离 +z");
        assert!((ns[1][2] - (100.0f64 / 12_500.0f64.sqrt()) as f32).abs() < 1e-6,
            "只邻接单个斜面的顶点法线应等于该面归一化法线");
    }
}
