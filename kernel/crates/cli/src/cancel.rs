//! B7 协作式取消：`--cancel-file` 标志文件轮询。
//!
//! 约定（与 server worker A2 对接）：
//! - worker 在收到 CANCELING 状态后创建标志文件（空文件即可）；
//! - 内核在分块/瓦片边界轮询该文件，检测到即**安全终止**：
//!   journal 保留（已完成分块已逐块落盘）、manifest 本体不动，
//!   剩余分块保持 pending，断点续切语义与崩溃恢复完全一致；
//! - 进程以退出码 130 结束（stderr 带 [`CANCEL_ERR_PREFIX`] 前缀），
//!   worker 据此区分「用户取消」（不重试、置 CANCELED）与「执行失败」（重试）。
//!
//! 设计取舍：用标志文件而非进程信号，接口与平台无关（Windows/Linux 一致）、
//! worker 不需要持有子进程句柄；轮询粒度 = 分块边界，最坏取消延迟 =
//! 单个分块的执行时长（对 5GB 任务单分块 < 数秒，可接受）。

use std::path::{Path, PathBuf};

/// 取消类错误的 stderr 标记前缀（main 据此转 exit 130，worker 据此判定）。
pub const CANCEL_ERR_PREFIX: &str = "CANCELED:";

/// 内核被取消时的进程退出码（SIGINT 惯例值，便于运维侧识别）。
pub const CANCEL_EXIT_CODE: i32 = 130;

/// 取消标志：包装可选的标志文件路径。
#[derive(Debug, Clone, Default)]
pub struct CancelFlag {
    path: Option<PathBuf>,
}

impl CancelFlag {
    pub fn from_path(path: Option<PathBuf>) -> Self {
        Self { path }
    }

    /// 未启用取消的常量构造器（测试与不需要取消的调用方使用）。
    pub const fn none() -> Self {
        Self { path: None }
    }

    /// 是否已请求取消（轮询标志文件是否存在；未启用取消时恒 false）。
    /// 文件存在性即语义，内容不解析（worker 写空文件）。
    pub fn is_canceled(&self) -> bool {
        self.path
            .as_deref()
            .map(Path::exists)
            .unwrap_or(false)
    }

    /// 取消来源路径（诊断输出用）。
    pub fn path(&self) -> Option<&Path> {
        self.path.as_deref()
    }
}

/// 构造统一的取消错误文本（带约定前缀，worker/main 据此分支）。
pub fn canceled_err(flag: &CancelFlag) -> String {
    match flag.path() {
        Some(p) => format!("{CANCEL_ERR_PREFIX} 检测到取消标志文件 {}", p.display()),
        None => format!("{CANCEL_ERR_PREFIX} 检测到取消请求"),
    }
}

/// 判断一个 Err(String) 是否为取消类错误。
pub fn is_canceled_err(err: &str) -> bool {
    err.starts_with(CANCEL_ERR_PREFIX)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn flag_polls_file_existence() {
        let dir = std::env::temp_dir().join(format!("tangis-cancel-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("cancel.flag");

        let flag = CancelFlag::from_path(Some(path.clone()));
        assert!(!flag.is_canceled(), "文件不存在不应取消");

        std::fs::write(&path, b"").unwrap();
        assert!(flag.is_canceled());

        // 未启用（None）恒 false
        assert!(!CancelFlag::default().is_canceled());

        let err = canceled_err(&flag);
        assert!(is_canceled_err(&err) && err.contains(path.display().to_string().as_str()));

        let _ = std::fs::remove_dir_all(&dir);
    }
}
