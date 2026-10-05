//! 质检配置：每项检测独立开关 + 可配阈值，全部有文档化默认值。

use serde::Serialize;

/// 质检配置（报告 `thresholds` 字段原样记录实测所用阈值）。
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct QcConfig {
    /// 检测 1：退化三角形（零面积 / 重复顶点 / 重复面）。
    pub degenerate: bool,
    /// 零面积判定阈值：三角形面积（|叉积|/2，m²）小于该值视为零面积。默认 `1e-10`。
    pub degenerate_area_eps: f64,

    /// 检测 2：法线翻转（共享边邻接面夹角突变）。
    pub normal_flip: bool,
    /// 邻接面法线夹角超过该角度（度）记为翻转边。默认 `120.0`。
    pub normal_flip_max_angle_deg: f64,

    /// 检测 3：悬浮块（连通分量分析）。
    pub floating: bool,
    /// 顶点空间聚类体素边长（m），分量 = 体素 26 邻域连通。默认 `5.0`。
    pub floating_voxel_size: f64,
    /// 分量最低点高于主分量最高点该高差（m）才可能判悬浮。默认 `30.0`。
    pub floating_height_above: f64,
    /// 分量包围盒体积 / 主分量包围盒体积 低于该比例才判悬浮。默认 `0.05`。
    pub floating_max_volume_ratio: f64,

    /// 检测 4：裂缝初检（相邻 tile 共享边界缝隙）。
    pub crack: bool,
    /// 边界顶点配对距离超过该值（m）记为裂缝。默认 `0.5`。
    pub crack_gap: f64,

    /// 检测 5：自相交初检（AABB 预筛 + 精确三角形相交）。
    pub self_intersect: bool,
}

impl Default for QcConfig {
    fn default() -> Self {
        QcConfig {
            degenerate: true,
            degenerate_area_eps: 1e-10,
            normal_flip: true,
            normal_flip_max_angle_deg: 120.0,
            floating: true,
            floating_voxel_size: 5.0,
            floating_height_above: 30.0,
            floating_max_volume_ratio: 0.05,
            crack: true,
            crack_gap: 0.5,
            self_intersect: true,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_are_documented_values() {
        let c = QcConfig::default();
        assert!(c.degenerate && c.normal_flip && c.floating && c.crack && c.self_intersect);
        assert_eq!(c.degenerate_area_eps, 1e-10);
        assert_eq!(c.normal_flip_max_angle_deg, 120.0);
        assert_eq!(c.floating_voxel_size, 5.0);
        assert_eq!(c.floating_height_above, 30.0);
        assert_eq!(c.floating_max_volume_ratio, 0.05);
        assert_eq!(c.crack_gap, 0.5);
    }
}
