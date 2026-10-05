//! 质检报告数据模型（serde 序列化为 JSON）。
//!
//! 每项检测关闭时对应字段为 `null`（不编造统计值）；样例与字段解释见
//! `docs/qc-report-README.md`。

use serde::Serialize;

use crate::config::QcConfig;

/// 质检报告根对象。
#[derive(Debug, Clone, Serialize)]
pub struct QcReport {
    /// 报告 schema 版本（当前 `"1"`）。
    pub schema_version: String,
    /// 生成时刻（Unix 秒，浮点；来自实测系统时钟）。
    pub generated_at_unix: f64,
    /// 数据源路径（CLI `--source` 原样）。
    pub source: String,
    /// 实际解析成功的 tile 数。
    pub tile_count: usize,
    /// 本次实测所用全部开关与阈值。
    pub thresholds: QcConfig,
    /// per-tile 明细（文件名排序）。
    pub tiles: Vec<TileReport>,
    /// 裂缝配对结果（每对相邻 tile 一条，含零缝隙对）。
    pub cracks: Vec<CrackPairReport>,
    /// 未参与裂缝配对的 tile 数（无名后缀下标 / 无同尺寸邻居）。
    pub crack_tiles_unpaired: usize,
    /// 汇总统计。
    pub summary: Summary,
}

/// 单 tile 明细。
#[derive(Debug, Clone, Serialize)]
pub struct TileReport {
    /// tile 名（文件 stem）。
    pub name: String,
    /// 来源文件路径。
    pub source_file: String,
    /// 顶点数。
    pub vertices: usize,
    /// 三角形数。
    pub faces: usize,
    /// 检测 1：退化三角形（关闭时 `null`）。
    pub degenerate: Option<DegenerateReport>,
    /// 检测 2：法线翻转（关闭时 `null`）。
    pub normal_flip: Option<NormalFlipReport>,
    /// 检测 3：悬浮块（关闭时 `null`）。
    pub floating: Option<FloatingReport>,
    /// 检测 5：自相交（关闭时 `null`）。
    pub self_intersect: Option<SelfIntersectReport>,
}

/// 退化三角形检测结果。
#[derive(Debug, Clone, Serialize)]
pub struct DegenerateReport {
    /// 零面积面数（面积 < `degenerate_area_eps`）。
    pub zero_area: usize,
    /// 含重复顶点索引的面数（a==b / b==c / a==c）。
    pub repeated_vertex: usize,
    /// 重复面数（排序索引三元组重复，重复实例各计 1）。
    pub duplicate_face: usize,
    /// 三类合计（一面可同时命中多类，合计可能大于命中面数）。
    pub total: usize,
    /// 样例（最多 5 条：面索引 + 重心）。
    pub samples: Vec<FaceSample>,
}

/// 退化面样例。
#[derive(Debug, Clone, Serialize)]
pub struct FaceSample {
    /// 面索引（indices/3）。
    pub face: usize,
    /// 命中类别（zero_area / repeated_vertex / duplicate_face，可多个）。
    pub kinds: Vec<String>,
    /// 面重心（tile 局部坐标，m）。
    pub centroid: [f64; 3],
}

/// 法线翻转检测结果。
#[derive(Debug, Clone, Serialize)]
pub struct NormalFlipReport {
    /// 参与检查的共享边数（两侧均为有效面的边）。
    pub edges_checked: usize,
    /// 翻转边数（邻接面夹角 > 阈值）。
    pub flipped_edges: usize,
    /// 涉及翻转边的面数。
    pub flipped_faces: usize,
    /// 孤立翻转面数（仅有一条翻转边的面 = 单面异常，非成片翻折）。
    pub isolated_faces: usize,
    /// 孤立法线翻转率 = isolated_faces / 有效面数。
    pub isolated_flip_rate: f64,
    /// 样例（最多 5 条，按夹角降序）。
    pub samples: Vec<FlipSample>,
}

/// 翻转边样例。
#[derive(Debug, Clone, Serialize)]
pub struct FlipSample {
    /// 两侧面索引。
    pub face_a: usize,
    pub face_b: usize,
    /// 实测夹角（度）。
    pub angle_deg: f64,
    /// 共享边中点（tile 局部坐标，m）。
    pub midpoint: [f64; 3],
}

/// 悬浮块检测结果。
#[derive(Debug, Clone, Serialize)]
pub struct FloatingReport {
    /// 连通分量数（体素哈希 26 邻域近似聚类）。
    pub components: usize,
    /// 主地面分量顶点数（最大分量）。
    pub main_vertices: usize,
    /// 判为悬浮的分量清单。
    pub flagged: Vec<FloatingComponent>,
}

/// 悬浮分量明细。
#[derive(Debug, Clone, Serialize)]
pub struct FloatingComponent {
    /// 分量顶点数。
    pub vertices: usize,
    /// 分量包围盒（tile 局部坐标，m）。
    pub bbox_min: [f64; 3],
    pub bbox_max: [f64; 3],
    /// 分量最低点相对主分量最高点的高差（m）。
    pub clearance: f64,
    /// 分量包围盒体积 / 主分量包围盒体积。
    pub volume_ratio: f64,
    /// 分量任一顶点（定位用，tile 局部坐标）。
    pub sample_vertex: [f64; 3],
}

/// 自相交初检结果。
#[derive(Debug, Clone, Serialize)]
pub struct SelfIntersectReport {
    /// AABB 重叠候选对数（已排除共享顶点的邻接对）。
    pub candidate_pairs: usize,
    /// 精确相交对数。
    pub intersections: usize,
    /// 覆盖率：AABB 预筛为全量候选（非抽样），恒 1.0。
    pub coverage: f64,
    /// 方法说明（报告如实注明检测方式）。
    pub method: String,
    /// 样例（最多 5 条）。
    pub samples: Vec<IntersectSample>,
}

/// 自相交样例。
#[derive(Debug, Clone, Serialize)]
pub struct IntersectSample {
    pub face_a: usize,
    pub face_b: usize,
    /// 两面重心中点（定位用，tile 局部坐标，m）。
    pub midpoint: [f64; 3],
}

/// 一对相邻 tile 的裂缝初检结果（零缝隙对也如实记录）。
#[derive(Debug, Clone, Serialize)]
pub struct CrackPairReport {
    pub tile_a: String,
    pub tile_b: String,
    /// 共享轴（"x" 或 "y"，由网格下标差方向决定）。
    pub shared_axis: String,
    /// 共享平面全局坐标（m）。
    pub plane: f64,
    /// A 侧共享边界顶点数。
    pub border_vertices_a: usize,
    /// 找到 B 侧最近点的 A 顶点数（搜索半径 = max(crack_gap, 1.0) m）。
    pub matched_pairs: usize,
    /// 搜索半径内找不到 B 侧顶点的 A 顶点数（严重断边证据）。
    pub unmatched_a: usize,
    /// 配对距离中位数（m，坐标量化 1e-4 m 后计算）。
    pub median_gap: f64,
    /// 配对距离均值（m）。
    pub mean_gap: f64,
    /// 最大配对距离（m）。
    pub max_gap: f64,
    /// 配对距离 > `crack_gap` 的数量（含 unmatched）。
    pub gaps_over_threshold: usize,
    /// 裂缝位置清单（边界线段中点，全局坐标，m；最多 20 条）。
    pub positions: Vec<CrackPosition>,
}

/// 裂缝位置（一对越界顶点的线段中点）。
#[derive(Debug, Clone, Serialize)]
pub struct CrackPosition {
    pub x: f64,
    pub y: f64,
    pub z: f64,
    /// 该点的实测缝隙（m）。
    pub gap: f64,
}

/// 汇总统计。
#[derive(Debug, Clone, Serialize)]
pub struct Summary {
    /// 存在退化面的 tile 数 / 退化面总数。
    pub tiles_with_degenerate: usize,
    pub total_degenerate: usize,
    /// 存在翻转边的 tile 数 / 翻转边总数。
    pub tiles_with_normal_flip: usize,
    pub total_flipped_edges: usize,
    /// 存在悬浮分量的 tile 数 / 悬浮分量总数。
    pub tiles_with_floating: usize,
    pub total_floating_components: usize,
    /// 裂缝配对数 / 存在裂缝的对数 / 裂缝段总数。
    pub crack_pairs_checked: usize,
    pub crack_pairs_flagged: usize,
    pub total_crack_segments: usize,
    /// 存在自相交的 tile 数 / 相交对总数。
    pub tiles_with_self_intersect: usize,
    pub total_self_intersections: usize,
    /// 全部检测零告警 = true。
    pub passed: bool,
}
