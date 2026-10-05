//! tileset.json 生成（3D Tiles 1.0）。
//!
//! bounds 转换约定（manifest 的 [`Bounds`] 是自定义 min/max 结构）：
//!
//! 1. **地理坐标**（min/max 的 x ∈ [-180, 180]、y ∈ [-90, 90]，即 WGS84 经纬度，
//!    z 为米制高程）：输出 `region` —— `[west, south, east, north, minimumHeight,
//!    maximumHeight]`，其中 west/south/east/north 由度转换为**弧度**（规范要求
//!    region 角度项必须是弧度），两个高度项保持米。demo manifest 即此情形。
//! 2. **局部直角坐标**（米）：输出 `box` —— 12 个数字
//!    `[cx, cy, cz, xx, xy, xz, yx, yy, yz, zx, zy, zz]`，
//!    前 3 项为包围盒中心，后 9 项为三个半轴向量（这里取轴对齐：
//!    X/Y/Z 半轴各指向本轴方向，长度为边长的一半）。
//!
//! # geometricError 策略（按真实几何尺度推导，D2 修复）
//!
//! 早期实现按 `100 * 2^(maxLod - lod)` 纯层级推导，清单为「全叶子扁平结构」
//! 时（所有分块同层级）所有 tile 的 geometricError 都退化为 0，Cesium 的
//! SSE 永不触发细化，一次 b3dm 都不会请求。现改为：
//!
//! - **叶子 tile = 0**：叶子即最细分辨率，无子级可细化，内容始终渲染
//!   （规范允许叶子为 0）；
//! - **非叶子 tile = max(自身真实包围盒对角线(米) × 4, 2 × max(子 ge), 1.0)**：
//!   以包围盒对角线×4 作为该 tile「不渲染时引入的最大屏幕误差」的尺度代理
//!   （因子 4 的经验含义：包围盒对角线长度的内容在屏幕上占约 1/4 视口时
//!   触发细化）；`2 × 子级最大 ge` 保证父 tile 严格大于子 tile（规范要求
//!   geometricError 沿层级递减，相等会导致同层无限细化判断歧义）；
//!   下限 1.0 防止微小程序件（对角线≈0）退化；
//! - **tileset 级 = max(2 × root ge, 全数据集包围盒对角线×4, 1.0)**：
//!   必须大于 root tile 的 ge（它是「整个 tileset 不渲染」的误差，
//!   比 root 更粗），且当 root 本身是叶子（ge=0）时仍保持 > 0 ——
//!   tileset.geometricError = 0 会让 Cesium 认为不渲染该 tileset 无代价，
//!   从而连 root 都不加载（即 D2 缺陷）。
//!
//! # 地理定位 transform（D1 修复）
//!
//! `build --origin <lon> <lat> <height>`（度/米，WGS84）时，root tile 写入
//! `transform`：ENU（东北天，局部坐标 x=东、y=北、z=天，米）→ ECEF 的
//! 4×4 列主序矩阵，平移分量 = (lon, lat, h) 的 WGS84 椭球面 ECEF 坐标。
//! 所有子 tile 按规范继承 root 的 transform，无需逐 tile 重复。
//! **默认无 `--origin` 时不写 transform**：tile 停留在 ECEF 原点附近的局部
//! 坐标系（Cesium 中位于地心，需数据本身已是地心坐标才可见）——这是历史
//! 行为，保持向后兼容。

use std::collections::{BTreeMap, BTreeSet};

use serde_json::{json, Value};
use tangis_manifest::{Bounds, Chunk, ChunkId, ChunkManifest};

/// 几何误差的包围盒对角线倍率（经验因子：见模块注释 geometricError 策略）。
const GE_DIAGONAL_FACTOR: f64 = 4.0;
/// 几何误差下限（米）：防微小程序件退化为 0。
const GE_MIN: f64 = 1.0;

/// WGS84 椭球长半轴（米）。
const WGS84_A: f64 = 6_378_137.0;
/// WGS84 扁率倒数分母。
const WGS84_INV_F: f64 = 298.257_223_563;

/// bounds 的真实几何尺度（米，包围盒对角线长度）。
///
/// - 地理坐标（经纬度）按 WGS84 换算：1° 纬度 ≈ 111_320 m，
///   经度差乘以 cos(中点纬度)（球面近似，仅用于误差量级估计，非大地测量）；
/// - 局部直角坐标直接视为米。
pub fn bounds_diagonal_meters(b: &Bounds) -> f64 {
    let (dx, dy, dz) = if looks_geographic(b) {
        let mid_lat = ((b.min[1] + b.max[1]) / 2.0).to_radians();
        let meters_per_deg = 111_320.0;
        (
            (b.max[0] - b.min[0]) * meters_per_deg * mid_lat.cos(),
            (b.max[1] - b.min[1]) * meters_per_deg,
            b.max[2] - b.min[2],
        )
    } else {
        (
            b.max[0] - b.min[0],
            b.max[1] - b.min[1],
            b.max[2] - b.min[2],
        )
    };
    (dx * dx + dy * dy + dz * dz).sqrt()
}

/// 单个 tile 的 geometricError（米）。策略见模块注释：
/// 叶子 = 0；非叶子 = max(包围盒对角线×4, 2×子级最大 ge, 1.0)。
pub fn geometric_error(lod_bounds: &Bounds, children_max_ge: Option<f64>) -> f64 {
    let is_leaf = children_max_ge.is_none();
    if is_leaf {
        return 0.0;
    }
    let scale = bounds_diagonal_meters(lod_bounds) * GE_DIAGONAL_FACTOR;
    let child_bound = 2.0 * children_max_ge.unwrap_or(0.0);
    scale.max(child_bound).max(GE_MIN)
}

/// ENU→ECEF 的 4×4 列主序 transform（3D Tiles `transform` 布局）。
///
/// WGS84 椭球：把局部东北天坐标系（x=东、y=北、z=天，米）映射到地心地固
/// 坐标系，平移分量 = (lon, lat, h) 处的椭球面点。标准公式：
///
/// - N = a / √(1 − e² sin²φ)（卯酉圈曲率半径）
/// - p = ((N+h) cosφ cosλ, (N+h) cosφ sinλ, (N(1−e²)+h) sinφ)
/// - east = (−sinλ, cosλ, 0)，north = (−sinφcosλ, −sinφsinλ, cosφ)，
///   up = (cosφcosλ, cosφsinλ, sinφ)
pub fn enu_to_ecef_transform(lon_deg: f64, lat_deg: f64, height_m: f64) -> [f64; 16] {
    let f = 1.0 / WGS84_INV_F;
    let e2 = f * (2.0 - f);
    let lon = lon_deg.to_radians();
    let lat = lat_deg.to_radians();
    let (sin_lat, cos_lat) = lat.sin_cos();
    let (sin_lon, cos_lon) = lon.sin_cos();

    let n = WGS84_A / (1.0 - e2 * sin_lat * sin_lat).sqrt();
    let px = (n + height_m) * cos_lat * cos_lon;
    let py = (n + height_m) * cos_lat * sin_lon;
    let pz = (n * (1.0 - e2) + height_m) * sin_lat;

    let east = [-sin_lon, cos_lon, 0.0];
    let north = [-sin_lat * cos_lon, -sin_lat * sin_lon, cos_lat];
    let up = [cos_lat * cos_lon, cos_lat * sin_lon, sin_lat];

    // 列主序 4×4：前三列为旋转基（行 4 补 0），末列为平移（行 4 = 1）
    [
        east[0], east[1], east[2], 0.0, //
        north[0], north[1], north[2], 0.0, //
        up[0], up[1], up[2], 0.0, //
        px, py, pz, 1.0,
    ]
}

/// 判断 bounds 是否为 WGS84 经纬度（度）+ 米制高程。
fn looks_geographic(b: &Bounds) -> bool {
    b.min[0].abs() <= 180.0
        && b.max[0].abs() <= 180.0
        && b.min[1].abs() <= 90.0
        && b.max[1].abs() <= 90.0
}

/// 将 manifest 的 bounds 转为合法的 3D Tiles boundingVolume。
/// 转换规则见模块注释。
pub fn bounds_to_bounding_volume(b: &Bounds) -> Value {
    if looks_geographic(b) {
        // region：[west, south, east, north, minH, maxH]，角度项为弧度，高度为米
        json!({
            "region": [
                b.min[0].to_radians(),
                b.min[1].to_radians(),
                b.max[0].to_radians(),
                b.max[1].to_radians(),
                b.min[2].min(b.max[2]),
                b.max[2].max(b.min[2]),
            ]
        })
    } else {
        // box：中心 + 三个轴对齐半轴向量
        let center = [
            (b.min[0] + b.max[0]) / 2.0,
            (b.min[1] + b.max[1]) / 2.0,
            (b.min[2] + b.max[2]) / 2.0,
        ];
        let half = [
            (b.max[0] - b.min[0]) / 2.0,
            (b.max[1] - b.min[1]) / 2.0,
            (b.max[2] - b.min[2]) / 2.0,
        ];
        json!({
            "box": [
                center[0], center[1], center[2],
                half[0], 0.0, 0.0,
                0.0, half[1], 0.0,
                0.0, 0.0, half[2],
            ]
        })
    }
}

/// 对多个 bounds 做组件级并集（用于多根分块时合成 tileset root）。
fn union_bounds(bs: impl IntoIterator<Item = Bounds>) -> Bounds {
    bs.into_iter().reduce(|acc, b| Bounds {
        min: [
            acc.min[0].min(b.min[0]),
            acc.min[1].min(b.min[1]),
            acc.min[2].min(b.min[2]),
        ],
        max: [
            acc.max[0].max(b.max[0]),
            acc.max[1].max(b.max[1]),
            acc.max[2].max(b.max[2]),
        ],
    }).expect("迭代器非空")
}

/// 把一个分块转为 3D Tiles tile 节点，children 按 manifest 父子关系递归。
/// 返回 (tile 节点, 该 tile 的 geometricError)，ge 供父 tile 计算用。
fn build_tile(
    chunk: &Chunk,
    by_id: &BTreeMap<&ChunkId, &Chunk>,
) -> (Value, f64) {
    let children: Vec<(Value, f64)> = chunk
        .children
        .iter()
        .map(|cid| by_id.get(cid).expect("topo_sort 已校验 children 引用完整"))
        .map(|c| build_tile(c, by_id))
        .collect();

    let children_max_ge = children.iter().map(|(_, ge)| *ge).reduce(f64::max);
    let ge = geometric_error(&chunk.bounds, children_max_ge);

    let mut tile = json!({
        "boundingVolume": bounds_to_bounding_volume(&chunk.bounds),
        "geometricError": ge,
        "content": { "uri": format!("{}.b3dm", chunk.id.0) },
    });
    if !children.is_empty() {
        tile["children"] =
            Value::Array(children.into_iter().map(|(v, _)| v).collect());
    }
    (tile, ge)
}

/// 生成完整 tileset.json（未序列化）。
///
/// `origin` 为 `--origin <lon> <lat> <height>`（度/米，WGS84）：Some 时
/// root tile 写入 ENU→ECEF transform（子 tile 继承）；None 时不写
/// transform（历史行为，tile 位于地心局部坐标系）。
///
/// # Errors
/// - 清单为空（3D Tiles 必须有 root tile）。
pub fn build_tileset_json(
    manifest: &ChunkManifest,
    origin: Option<[f64; 3]>,
) -> Result<Value, String> {
    if manifest.chunks.is_empty() {
        return Err(format!(
            "任务 `{}` 清单中没有任何分块，无法生成 tileset.json",
            manifest.task_id
        ));
    }

    let by_id: BTreeMap<&ChunkId, &Chunk> =
        manifest.chunks.iter().map(|c| (&c.id, c)).collect();

    // 被引用过的都是子分块；从未被引用的即根候选，按 id 排序保证确定性
    let referenced: BTreeSet<&ChunkId> = manifest
        .chunks
        .iter()
        .flat_map(|c| c.children.iter())
        .collect();
    let mut roots: Vec<&Chunk> = manifest
        .chunks
        .iter()
        .filter(|c| !referenced.contains(&c.id))
        .collect();
    roots.sort_by(|a, b| a.id.cmp(&b.id));

    // 3D Tiles 要求恰好一个 root：单根直接用；多根则合成一个虚拟 root
    // （无 content，bounds 取各根的并集）
    let (mut root_tile, root_ge) = if roots.len() == 1 {
        build_tile(roots[0], &by_id)
    } else {
        let bounds = union_bounds(roots.iter().map(|c| c.bounds.clone()));
        let children: Vec<(Value, f64)> =
            roots.iter().map(|c| build_tile(c, &by_id)).collect();
        let children_max_ge = children.iter().map(|(_, ge)| *ge).reduce(f64::max);
        let ge = geometric_error(&bounds, children_max_ge);
        (
            json!({
                "boundingVolume": bounds_to_bounding_volume(&bounds),
                "geometricError": ge,
                "children": children.into_iter().map(|(v, _)| v).collect::<Vec<_>>(),
            }),
            ge,
        )
    };

    // D1：--origin 时 root 写 ENU→ECEF transform，全部子 tile 按规范继承
    if let Some([lon, lat, height]) = origin {
        root_tile["transform"] = json!(enu_to_ecef_transform(lon, lat, height));
    }

    // tileset 级 ge：必须 > root ge（root 为叶子时 root ge=0，用全数据集
    // 尺度兜底），保证 Cesium 会加载并细化 root —— 即 D2 修复的关键
    let all_bounds = union_bounds(manifest.chunks.iter().map(|c| c.bounds.clone()));
    let tileset_ge = (root_ge * 2.0)
        .max(bounds_diagonal_meters(&all_bounds) * GE_DIAGONAL_FACTOR)
        .max(GE_MIN);

    Ok(json!({
        "asset": { "version": "1.0" }, // 3D Tiles 1.0
        "geometricError": tileset_ge,  // tileset 级：整个数据集的渲染误差阈值
        "root": root_tile,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use tangis_manifest::ChunkStatus;

    fn bounds(min: [f64; 3], max: [f64; 3]) -> Bounds {
        Bounds { min, max }
    }

    fn chunk(id: &str, lod: u32, b: Bounds, children: &[&str]) -> Chunk {
        Chunk {
            id: ChunkId::new(id),
            lod,
            bounds: b,
            status: ChunkStatus::Pending,
            attempts: 0,
            children: children.iter().map(|c| ChunkId::new(*c)).collect(),
            source: None,
            source_offset: None,
        }
    }

    fn demo_manifest() -> ChunkManifest {
        ChunkManifest {
            task_id: "t".into(),
            chunks: vec![
                chunk("root", 0, bounds([116.3, 39.95, 0.0], [116.4, 40.05, 120.0]), &["a", "b"]),
                chunk("a", 1, bounds([116.3, 39.95, 0.0], [116.35, 40.0, 60.0]), &[]),
                chunk("b", 1, bounds([116.35, 40.0, 0.0], [116.4, 40.05, 60.0]), &[]),
            ],
            source: None,
        }
    }

    #[test]
    fn tileset_deserializes_and_children_match_manifest() {
        let m = demo_manifest();
        let v = build_tileset_json(&m, None).unwrap();

        // 可反序列化（结构合法）
        let back: Value = serde_json::from_value(v.clone()).unwrap();
        assert_eq!(back["asset"]["version"], "1.0");

        // 默认（无 origin）不写 transform
        assert!(back["root"].get("transform").is_none());

        // root.children 数与 manifest 一致（root 分块声明 2 个孩子）
        let root_children = back["root"]["children"].as_array().unwrap();
        let manifest_root = m.chunk(&ChunkId::new("root")).unwrap();
        assert_eq!(root_children.len(), manifest_root.children.len());

        // children 引用的就是 manifest 里的子分块，content.uri 正确
        let uris: Vec<&str> = root_children
            .iter()
            .map(|c| c["content"]["uri"].as_str().unwrap())
            .collect();
        assert_eq!(uris, vec!["a.b3dm", "b.b3dm"]);
        assert_eq!(back["root"]["content"]["uri"], "root.b3dm");
    }

    #[test]
    fn geographic_bounds_become_region_in_radians() {
        let b = bounds([116.3, 39.95, 0.0], [116.4, 40.05, 120.0]);
        let v = bounds_to_bounding_volume(&b);
        let region = v["region"].as_array().unwrap();
        assert_eq!(region.len(), 6);
        // 角度项必须是弧度且在合法范围
        for r in &region[0..4] {
            let x = r.as_f64().unwrap();
            assert!(
                (-std::f64::consts::PI..=std::f64::consts::PI).contains(&x)
            );
        }
        assert!((region[0].as_f64().unwrap() - 116.3f64.to_radians()).abs() < 1e-12);
        assert_eq!(region[4].as_f64().unwrap(), 0.0); // minimumHeight（米）
        assert_eq!(region[5].as_f64().unwrap(), 120.0); // maximumHeight（米）
        // 高度递增（region 约束 minimumHeight <= maximumHeight）
        assert!(region[4].as_f64().unwrap() <= region[5].as_f64().unwrap());
    }

    #[test]
    fn local_bounds_become_box_with_12_numbers() {
        // 取明显超出经纬度量程的局部米制坐标，避免落入地理启发式
        let b = bounds([0.0, 0.0, 0.0], [1000.0, 2000.0, 3000.0]);
        let v = bounds_to_bounding_volume(&b);
        assert!(v.get("region").is_none());
        let bx = v["box"].as_array().unwrap();
        assert_eq!(bx.len(), 12);
        // 中心 = (500, 1000, 1500)；X 半轴 = 500，Z 半轴 = 1500
        assert_eq!(bx[0].as_f64().unwrap(), 500.0);
        assert_eq!(bx[1].as_f64().unwrap(), 1000.0);
        assert_eq!(bx[3].as_f64().unwrap(), 500.0);
        assert_eq!(bx[11].as_f64().unwrap(), 1500.0); // Z 半轴在索引 11（zx,zy,zz）
    }

    // ---------- geometricError 策略（D2） ----------

    #[test]
    fn geometric_error_leaf_zero_and_scale_derived() {
        // 叶子：无子级 → 0（内容始终渲染）
        let leaf = bounds([0.0, 0.0, 0.0], [100.0, 100.0, 20.0]);
        assert_eq!(geometric_error(&leaf, None), 0.0);

        // 非叶子：对角线 × 4（下限 1.0）
        // 对角线 = √(100²+100²+20²) ≈ 143.0 → ge ≈ 572
        let ge = geometric_error(&leaf, Some(0.0));
        let diag = (100.0f64.powi(2) * 2.0 + 20.0f64.powi(2)).sqrt();
        assert!((ge - diag * 4.0).abs() < 1e-9, "{ge} vs {}", diag * 4.0);

        // 2× 子级最大 ge 兜底（保证父 > 子）：子 ge 很大时取 2×子
        let ge = geometric_error(&leaf, Some(1000.0));
        assert!((ge - 2000.0).abs() < 1e-9, "{ge}");

        // 微小程序件：下限 1.0（取明显超出经纬度量程的局部米制坐标，
        // 避免 0 附近的数值被地理坐标启发式捕获）
        let tiny = bounds([1000.0, 1000.0, 1000.0], [1000.01, 1000.01, 1000.01]);
        assert_eq!(geometric_error(&tiny, Some(0.0)), 1.0);
    }

    #[test]
    fn flat_manifest_all_leaf_still_has_positive_tileset_error() {
        // D2 场景：全叶子扁平清单（旧实现所有 ge 全 0，Cesium 0 次请求）
        let m = ChunkManifest {
            task_id: "t".into(),
            source: None,
            chunks: vec![
                chunk("r1", 0, bounds([0.0, 0.0, 0.0], [100.0, 100.0, 20.0]), &[]),
                chunk("r2", 0, bounds([100.0, 0.0, 0.0], [200.0, 100.0, 20.0]), &[]),
            ],
        };
        let v = build_tileset_json(&m, None).unwrap();
        // tileset 级 ge > 0：Cesium 才会加载并渲染
        assert!(v["geometricError"].as_f64().unwrap() >= 1.0);
        // 叶子 tile ge = 0
        assert_eq!(v["root"]["children"][0]["geometricError"].as_f64().unwrap(), 0.0);
    }

    #[test]
    fn geometric_error_strictly_decreases_down_hierarchy() {
        let m = demo_manifest();
        let v = build_tileset_json(&m, None).unwrap();
        let ts_ge = v["geometricError"].as_f64().unwrap();
        let root_ge = v["root"]["geometricError"].as_f64().unwrap();
        let child_ge = v["root"]["children"][0]["geometricError"].as_f64().unwrap();
        // tileset > root > 子，叶子 = 0
        assert!(ts_ge > root_ge, "tileset {ts_ge} > root {root_ge}");
        assert!(root_ge > child_ge, "root {root_ge} > child {child_ge}");
        assert_eq!(child_ge, 0.0);
    }

    // ---------- ENU→ECEF transform（D1） ----------

    /// 变换矩阵的平移列（列主序 4×4 的第 12..15 元素）。
    fn translation_of(t: &[f64; 16]) -> [f64; 3] {
        [t[12], t[13], t[14]]
    }

    #[test]
    fn ecef_transform_matches_known_reference_points() {
        // 黄金值来自 pyproj/PROJ（EPSG:4979 → 4978，独立实现），误差 < 1 m
        let cases: [([f64; 3], [f64; 3]); 3] = [
            // (lon, lat, h) → ECEF
            ([0.0, 0.0, 0.0], [6_378_137.0, 0.0, 0.0]),
            (
                [116.3906, 39.9072, 50.0],
                [-2_177_709.058641, 4_388_774.212359, 4_070_119.020036],
            ),
            (
                [121.5, 31.0, 10.0],
                [-2_859_112.005629, 4_665_646.750075, 3_265_898.667035],
            ),
        ];
        for ([lon, lat, h], expected) in cases {
            let t = enu_to_ecef_transform(lon, lat, h);
            let p = translation_of(&t);
            let err = (p[0] - expected[0]).abs()
                .max((p[1] - expected[1]).abs())
                .max((p[2] - expected[2]).abs());
            assert!(err < 1.0, "({lon},{lat},{h}) 平移误差 {err} m：{p:?} vs {expected:?}");
        }
    }

    #[test]
    fn ecef_transform_axes_at_special_latitudes() {
        // 赤道本初子午线：up 指向 +X，east 指向 +Y，north 指向 +Z
        let t = enu_to_ecef_transform(0.0, 0.0, 0.0);
        assert_eq!(translation_of(&t), [6_378_137.0, 0.0, 0.0]);
        let col = |i: usize| [t[i * 4], t[i * 4 + 1], t[i * 4 + 2]];
        assert!((col(0)[0] - 0.0).abs() < 1e-12 && (col(0)[1] - 1.0).abs() < 1e-12); // east = +Y
        assert!((col(1)[2] - 1.0).abs() < 1e-12); // north = +Z
        assert!((col(2)[0] - 1.0).abs() < 1e-12); // up = +X

        // 北极：平移在 +Z 轴，|p| = 短半轴 b ≈ 6356752.3142 m
        let t = enu_to_ecef_transform(0.0, 90.0, 0.0);
        let p = translation_of(&t);
        assert!(p[0].abs() < 1e-6 && p[1].abs() < 1e-6, "{p:?}");
        let b = 6_356_752.314_245;
        assert!((p[2] - b).abs() < 1.0, "北极平移应为短半轴，实际 {}", p[2]);
    }

    #[test]
    fn ecef_transform_rotation_is_orthonormal() {
        let t = enu_to_ecef_transform(116.3906, 39.9072, 50.0);
        let col = |i: usize| [t[i * 4], t[i * 4 + 1], t[i * 4 + 2]];
        let dot = |a: [f64; 3], b: [f64; 3]| a[0] * b[0] + a[1] * b[1] + a[2] * b[2];
        for i in 0..3 {
            assert!((dot(col(i), col(i)) - 1.0).abs() < 1e-12, "列 {i} 非单位长");
        }
        assert!(dot(col(0), col(1)).abs() < 1e-12, "east·north ≠ 0");
        assert!(dot(col(0), col(2)).abs() < 1e-12, "east·up ≠ 0");
        assert!(dot(col(1), col(2)).abs() < 1e-12, "north·up ≠ 0");
        // 第 4 行 = [0,0,0,1]（列主序布局的 t[3]/t[7]/t[11]/t[15]）
        assert_eq!(t[3], 0.0);
        assert_eq!(t[7], 0.0);
        assert_eq!(t[11], 0.0);
        assert_eq!(t[15], 1.0);
    }

    #[test]
    fn tileset_root_transform_written_when_origin_given() {
        let m = demo_manifest();
        let v = build_tileset_json(&m, Some([116.3906, 39.9072, 50.0])).unwrap();
        let transform = v["root"]["transform"].as_array().unwrap();
        assert_eq!(transform.len(), 16);
        // 平移列 = pyproj 黄金值（误差 < 1 m）；子 tile 不重复写 transform（继承 root）
        assert!((transform[12].as_f64().unwrap() + 2_177_709.058641).abs() < 1.0);
        assert!((transform[13].as_f64().unwrap() - 4_388_774.212359).abs() < 1.0);
        assert!((transform[14].as_f64().unwrap() - 4_070_119.020036).abs() < 1.0);
        let child = &v["root"]["children"][0];
        assert!(child.get("transform").is_none());
    }

    #[test]
    fn multiple_roots_get_synthetic_root() {
        let m = ChunkManifest {
            task_id: "t".into(),
            source: None,
            chunks: vec![
                chunk("r1", 0, bounds([1000.0, 1000.0, 0.0], [1100.0, 1100.0, 50.0]), &[]),
                chunk("r2", 0, bounds([1100.0, 1100.0, 0.0], [1200.0, 1200.0, 50.0]), &[]),
            ],
        };
        let v = build_tileset_json(&m, None).unwrap();
        assert!(v["root"].get("content").is_none()); // 虚拟 root 无 content
        assert_eq!(v["root"]["children"].as_array().unwrap().len(), 2);
        // 并集 bounds：min(1000,1000,0) max(1200,1200,50)
        let bx = v["root"]["boundingVolume"]["box"].as_array().unwrap();
        assert_eq!(bx[0].as_f64().unwrap(), 1100.0); // 中心 x
        assert_eq!(bx[1].as_f64().unwrap(), 1100.0);
        // 虚拟 root 的 ge > 0（旧实现按层级推导会退化为 0）
        assert!(v["root"]["geometricError"].as_f64().unwrap() > 0.0);
    }

    #[test]
    fn empty_manifest_errors() {
        let m = ChunkManifest { task_id: "t".into(), chunks: vec![], source: None };
        assert!(build_tileset_json(&m, None).is_err());
    }
}
