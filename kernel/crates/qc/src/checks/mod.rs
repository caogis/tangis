//! 五项几何检测实现（每项独立、无状态、可单测）。

pub mod crack;
pub mod degenerate;
pub mod floating;
pub mod normal_flip;
pub mod self_intersect;

/// 报告样例条数上限（防大语料报告膨胀；命中总数仍如实全量统计）。
pub(crate) const MAX_SAMPLES: usize = 5;
