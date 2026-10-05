//! M2-PERF：manifest 回写策略成本实测（旧 O(n²) 整份重写 vs journal 增量）。
//!
//! 用真实 manifest 数据回放两种策略的**精确字节量与耗时**：
//! - 旧策略：按完成次序逐块把整份 manifest `to_string_pretty` + 落盘
//!   （与改造前 `cmd_build` 的串行回写循环同一代码路径）；
//! - 新策略：逐块 journal 追加一行 + 最后 finalize 整份重写一次。
//!
//! 用法：
//! ```text
//! cargo run -p tangis-manifest --release --example write_cost -- \
//!     <initial.json> <final.json>
//! ```
//! `<initial.json>`：切片开始前的 manifest（全 pending）；
//! `<final.json>`：切片完成后的 manifest（全 Done、含真实 bounds）。
//! 回放次序 = topo_sort(initial)（与真实 build 的串行回写次序一致）。

use std::fs;
use std::io::Write;
use std::path::PathBuf;
use std::time::Instant;

use tangis_manifest::{topo_sort, ChunkManifest, ChunkStatus, Journal, ManifestError};

fn load(path: &str) -> Result<ChunkManifest, String> {
    let raw = fs::read_to_string(path).map_err(|e| e.to_string())?;
    serde_json::from_str(&raw).map_err(|e| e.to_string())
}

fn main() -> Result<(), String> {
    let args: Vec<String> = std::env::args().collect();
    if args.len() != 3 {
        return Err("用法: write_cost <initial.json> <final.json>".into());
    }
    let initial = load(&args[1])?;
    let final_m = load(&args[2])?;
    let order = topo_sort(&initial).map_err(|e: ManifestError| e.to_string())?;

    // 完成次序里每块的终态（bounds/status/attempts 取 final）
    let steps: Vec<usize> = order
        .iter()
        .map(|id| {
            initial
                .chunks
                .iter()
                .position(|c| &c.id == id)
                .expect("topo_sort 只输出清单内分块")
        })
        .collect();

    // ---- 旧策略：逐块整份重写（写真实临时文件，与旧行为同路径） ----
    let tmp = std::env::temp_dir().join(format!("write-cost-old-{}", std::process::id()));
    let mut manifest = initial.clone();
    let mut old_bytes = 0u64;
    let mut old_writes = 0u64;
    let t_old = Instant::now();
    for &i in &steps {
        let f = &final_m.chunks[i];
        let c = &mut manifest.chunks[i];
        c.bounds = f.bounds.clone();
        c.status = ChunkStatus::Done;
        c.attempts = f.attempts;
        let json = serde_json::to_string_pretty(&manifest).map_err(|e| e.to_string())?;
        old_bytes += (json.len() + 1) as u64;
        fs::write(&tmp, json + "\n").map_err(|e| e.to_string())?;
        old_writes += 1;
    }
    let old_elapsed = t_old.elapsed();
    let _ = fs::remove_file(&tmp);

    // ---- 新策略：journal 追加 + finalize 一次整份重写 ----
    let dir = std::env::temp_dir().join(format!("write-cost-new-{}", std::process::id()));
    fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    let manifest_path = dir.join("manifest.json");
    let jpath = tangis_manifest::journal_path_for(&manifest_path);
    let mut new_bytes = 0u64;
    let t_new = Instant::now();
    {
        let mut j = Journal::create(jpath.clone()).map_err(|e: ManifestError| e.to_string())?;
        for &i in &steps {
            let f = &final_m.chunks[i];
            let outcome = tangis_manifest::ChunkOutcome {
                id: f.id.clone(),
                attempts: f.attempts,
                bounds: Some(f.bounds.clone()),
            };
            let line = serde_json::to_string(&outcome).map_err(|e| e.to_string())?;
            new_bytes += line.len() as u64 + 1;
            j.record(&outcome).map_err(|e: ManifestError| e.to_string())?;
        }
        // finalize：整份重写一次
        let json = serde_json::to_string_pretty(&final_m).map_err(|e| e.to_string())?;
        let finalize_bytes = json.len() as u64 + 1;
        fs::write(&manifest_path, json + "\n").map_err(|e| e.to_string())?;
        fs::remove_file(&jpath).map_err(|e| e.to_string())?;
        new_bytes += finalize_bytes;
    }
    let new_elapsed = t_new.elapsed();
    let _ = fs::remove_dir_all(&dir);

    let n = steps.len();
    println!("分块数 n = {n}");
    println!("manifest 终态大小 = {} 字节", {
        let json = serde_json::to_string_pretty(&final_m).map_err(|e| e.to_string())?;
        json.len() + 1
    });
    println!();
    println!(
        "旧策略（逐块整份重写）: 写入次数 {old_writes}，总字节 {old_bytes}（{:.1} MB），耗时 {:.1} ms",
        old_bytes as f64 / 1_048_576.0,
        old_elapsed.as_secs_f64() * 1000.0
    );
    println!(
        "新策略（journal 增量 + 一次 finalize）: 总字节 {new_bytes}（{:.1} MB），耗时 {:.1} ms",
        new_bytes as f64 / 1_048_576.0,
        new_elapsed.as_secs_f64() * 1000.0
    );
    let _ = std::io::stdout().flush();
    Ok(())
}
