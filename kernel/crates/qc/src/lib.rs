//! TanGIS 倾斜模型几何质检（M2-F08a，PRD F-08 第一步）。
//!
//! 对 OSGB（经 [`tangis_osgb`] 真实解析，恢复内部 Z-up 坐标）与 OBJ
//! （tobj 解析，Y-up → Z-up）三角网格做五项独立检测，输出结构化质检
//! 报告 JSON（per-tile 明细 + 跨 tile 裂缝配对 + 汇总统计）：
//!
//! 1. **退化三角形**（[`checks::degenerate`]）：零面积（面积 < 可配阈值）、
//!    重复顶点索引、重复面（排序索引三元组去重判定）；
//! 2. **法线翻转**（[`checks::normal_flip`]）：共享边的邻接面几何法线夹角
//!    超阈值（默认 > 120°）即标记翻转边，并统计孤立法线翻转率
//!    （仅有一条翻转边的面 / 全部有效面）；
//! 3. **悬浮块**（[`checks::floating`]）：顶点按体素哈希（26 邻域）近似
//!    聚类为连通分量，最大分视为主地面；分量最低点高于主分量最高点
//!    `floating_height_above`（默认 30 m）且包围盒体积占主分量比例
//!    < `floating_max_volume_ratio`（默认 0.05）判为悬浮；
//! 4. **裂缝初检**（[`checks::crack`]）：tile 名后缀 `+N_+M` 解析网格下标，
//!    同尺寸类内下标相邻（Δ=1）配对；共享平面上 A 侧边界顶点到 B 侧
//!    最近边界顶点距离（坐标量化 1e-4 m 后计算）超过 `crack_gap`
//!    （默认 0.5 m）记为裂缝，输出裂缝位置（边界线段中点，全局坐标）；
//! 5. **自相交初检**（[`checks::self_intersect`]）：三角形 AABB 空间哈希
//!    预筛全量候选对（覆盖率 1.0），共享顶点的邻接对排除后做精确
//!    三角形相交测试（平面侧判 + 线段-三角形 + 共面 2D 边交叉）。
//!
//! # 坐标约定
//!
//! 与 kernel 内部一致：Z-up（x/y 水平、z = 高度，单位米）。裂缝检测中
//! tile 全局位置 = 网格下标 × 该 tile 包围盒水平跨度（同名后缀下标步长 1
//! = 相邻 tile，统一网格布局假设，见模块文档说明）。
//!
//! # 禁止造假
//!
//! 报告全部字段来自实测几何；所有检测阈值可配且有文档化默认值
//! （见 [`QcConfig`]）；检测关闭时对应报告字段为 `null`（不编造 0）。

pub mod checks;
pub mod config;
pub mod load;
pub mod report;
pub mod run;

pub use config::QcConfig;
pub use load::{load_tile, scan_source, LoadedTile, TileInput};
pub use report::{
    CrackPairReport, CrackPosition, DegenerateReport, FaceSample, FlipSample, FloatingComponent,
    FloatingReport, IntersectSample, NormalFlipReport, QcReport, SelfIntersectReport, Summary,
    TileReport,
};
pub use run::run_qc;

use std::path::Path;

/// 便捷入口：扫描 `source`（目录/OBJ/OSGB 文件）→ 加载全部 tile → 质检。
pub fn run_qc_from_source(source: &Path, config: &QcConfig) -> Result<QcReport, String> {
    let inputs = scan_source(source)?;
    let mut tiles = Vec::with_capacity(inputs.len());
    for input in &inputs {
        tiles.push(load_tile(input)?);
    }
    Ok(run_qc(tiles, config, &source.to_string_lossy()))
}

/// 报告 JSON 落盘（pretty + 尾换行；父目录自动创建）。
pub fn write_report(report: &QcReport, path: &Path) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        if !parent.as_os_str().is_empty() && !parent.exists() {
            std::fs::create_dir_all(parent)
                .map_err(|e| format!("无法创建目录 {}: {e}", parent.display()))?;
        }
    }
    let json = serde_json::to_string_pretty(report).map_err(|e| e.to_string())?;
    std::fs::write(path, json + "\n")
        .map_err(|e| format!("无法写质检报告 {}: {e}", path.display()))
}
