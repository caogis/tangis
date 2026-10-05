# TanGIS 切片内核吞吐基准（M1 缩小规模基准 + 外推）

日期：2026-10-04 ｜ 责任范围：kernel（任务 rGOGSJ）
被测版本：本轮 D1/D2 修复后的 kernel（`cargo build --release`，commit 未打，工作区构建）

> **规模声明**：这是 M1 的**缩小规模基准**（语料约 26MB 解压数据，秒级耗时），
> 5GB 结论为线性外推，外推假设见文末。真实 osgconv 语料本轮无法被 M1 reader
> 解析（结论见 `testdata/osgb-real/README.md` 第 3 节），基准语料为
> 「合成 OSGB 格式 + 真实地形几何」。

## 1. 环境与被测对象

| 项 | 值 |
|---|---|
| 机器 | Intel Core i9-9880H @ 2.30GHz（8C16T，16 逻辑核），macOS（x86_64），Docker Desktop 宿主 |
| 构建 | `cargo build --release`（opt-level 3，默认 target-cpu） |
| 并行度 | Rayon 默认 = 16 线程（文件级并行加载 + 分块级并行转 b3dm） |
| 被测命令 | `tangis-kernel build <manifest> --out <dir>`（完整管线：OSGB 解压+解析 → 网格装配 → 带纹理 GLB/b3dm 组装 → 逐块落盘 → 串行 manifest 回写 → tileset.json） |

## 2. 基准语料（testdata/osgb-real/bench-synth/）

由 `make_bench_corpus.py` 生成：80 个真实地形 OBJ 模板（33×33 / 17×17 网格
高度场）× 8 份千米级平移副本 = 640 tile，合成 OSGB 布局（zlib 压缩流），
每个 tile 绑定 256×256 PNG 贴图（147KB，b3dm 内嵌）。

| 指标 | 值 |
|---|---|
| tile 数 | 640（全叶子扁平清单） |
| 总顶点 | 594,560 |
| 总三角形 | 1,114,112 |
| 语料磁盘体积（压缩 OSGB） | 14.23 MB |
| 语料解压后体积 | 26.25 MB（压缩比 1.84×） |

## 3. 结果

| 指标 | 值 |
|---|---|
| 耗时（2 次冷跑） | 1.069 s / 1.043 s |
| 输出体积（640×b3dm + tileset.json） | 127.65 MB（约 199 KB/tile，PNG 贴图占大头） |
| 解析+切片吞吐（按解压数据量） | ≈ 25.2 MB/s |
| 顶点吞吐 | ≈ 5.7×10⁵ 顶点/s |
| 三角形吞吐 | ≈ 1.07×10⁶ 三角形/s |
| tile 吞吐 | ≈ 613 tile/s |

manifest 串行回写（逐块全量落盘 = 断点检查点）在 640 块规模下未见明显瓶颈
（manifest 约 150KB×640 次写）。**注意该机制复杂度是 O(n²) 总写入量**，
数万块规模时将成为可测量成本（M2 优化候选）。

## 4. 5GB 外推（假设驱动，非实测）

按「解压后数据量」线性外推：

```
5 GB ÷ 26.25 MB ≈ 195×
1.05 s × 195 ≈ 205 s ≈ 3.4 分钟
```

按「磁盘压缩量」口径（若 5GB 指磁盘占用）：

```
5 GB ÷ 14.23 MB ≈ 351×
1.05 s × 351 ≈ 369 s ≈ 6.1 分钟
```

**外推假设（全部为有利/中性假设，实际可能更慢）**：

1. **线性扩展**：解析/组装/写盘耗时与数据量成正比；当前管线为
   O(n) 并行 + O(n²) manifest 回写，后者在大 n 时会突破线性；
2. **并行度不变**：同为 16 线程、无内存带宽饱和；
3. **I/O 不成瓶颈**：输出按比例放大约 1 TB 级（每 tile 内嵌 147KB 贴图
   的语料结构下），实际受磁盘写速限制，本轮未测 I/O 上限；
4. **内存无界**：当前 `resolve_meshes` 把**全部网格一次性载入内存**
   （BTreeMap 缓存 + 全量合并），5GB 语料峰值内存 >10GB——内存受限环境
   必须先做流式/分批改造（M2 项）；
5. 单 tile 尺寸分布与基准语料相似（~2000 三角形/tile）。

**结论（给 M2 的输入）**：切片内核本身的解析+组装吞吐在 16 线程下约
25 MB/s（解压口径），5GB 理论 3~6 分钟量级；真正的 M2 风险点是
**全量内存驻留**与 **manifest O(n²) 回写**，以及真实 OSG 解析器缺失
（本轮最大阻塞，见 testdata/osgb-real/README.md）。

## 5. 复现

```bash
cd testdata/osgb-real
python3 make_obj_corpus.py && python3 make_bench_corpus.py
cd ../../kernel && cargo build --release
./target/release/tangis-kernel build \
  ../testdata/osgb-real/bench-synth/manifest-bench.json --out /tmp/bench-out
```

---

# M2-PERF：流式切片（O(n²) manifest 回写修复 + 峰值内存优化）

日期：2026-10-04 ｜ 任务 ID：rXBAy4 ｜ 全部数字实测（非外推标注除外）

> M1 第 4 节指出的两个 M2 风险点本轮全部落地：
> ① manifest 逐块整份重写（O(n²)）→ **journal 增量追加 + finalize 一次整份重写**；
> ② `resolve_meshes` 全量网格驻留 → **逐分块流式「解析 →（简化）→ b3dm → 释放」**。
> 产物语义不变：新旧二进制对同一语料的全部 b3dm + tileset.json **逐字节一致**。

## M1. 环境与被测对象

| 项 | 值 |
|---|---|
| 机器 | 同 M1（i9-9880H 8C16T，macOS x86_64） |
| 构建 | `cargo build --release`；改造前后二进制分别留存实测 |
| 计时/内存 | `/usr/bin/time -l`（wall time + maximum resident set size） |
| 语料 | 80 tile 真实 osgconv（`testdata/osgb-real/osgb/`）+ **新增 1040 tile 真实 osgconv**（`testdata/osgb-real/osgb-1k/`，`make_1k_corpus.py` 生成：32×32 叶 + 4×4 粗层，docker osgconv 真实 OSG 3.6 格式） |

注意：M1 的 bench-synth（640 tile）为旧合成格式，现 reader（真 OSG 3.6）已
不能解析，本轮弃用；千 tile 基准全部用 osgconv 真实语料。

## M2. manifest O(n²) 实测与修复

**现状证据**（改造前代码路径）：`cmd_build` 每完成一个分块调用
`save_manifest` 把**整份** manifest `to_string_pretty` + `fs::write` 落盘，
共 n 次。用 `crates/manifest/examples/write_cost.rs`（回放真实完成次序、
与旧代码同一序列化路径）实测：

| 规模 | manifest 终态大小 | 旧策略写入次数 | 旧策略总写入字节 | 旧策略耗时 | 新策略总字节 | 新策略耗时 |
|---|---|---|---|---|---|---|
| 80 tile | 26,987 B | 80 | 2,051,422 B（2.0 MB） | 25.3 ms | 36,991 B（**÷55**） | 1.5 ms（**÷17**） |
| 1040 tile | 352,511 B | 1040 | 346,482,751 B（**330.4 MB**） | 2,750.9 ms | 485,476 B（**÷714**） | 15.7 ms（**÷175**） |

旧策略总写入量 ≈ n × manifest 大小（O(n²) 实锤）：1040 块时仅 manifest
回写就产生 330MB 写入、2.75s 串行耗时（与端到端提速幅度吻合，见 M3）。

**修复方案**（`crates/manifest/src/journal.rs`，公开 API 向后兼容新增）：
- 每完成一分块 → `<manifest>.journal` **追加一行** JSONL
  （`ChunkOutcome { id, attempts, bounds }`，逐条 flush，崩溃安全与旧逐块落盘同级）；
- 全部完成 → `finalize` 整份重写 manifest **一次**（`to_string_pretty` +
  尾随换行，与旧实现最终文件**逐字节一致**，已 `cmp` 验证），随后删除 journal；
- 断点续切：启动时 `replay` 把 journal 回放到内存（Done 跳过语义不变）；
  崩溃残留 journal 时重跑等价旧实现的中断恢复（有自动化测试）；
- 旧格式兼容：journal 是可选边车文件，不存在时 `replay` 返回 0，
  老 manifest 文件照常读取（有自动化测试）。

## M3. 峰值内存与端到端前后对比（真实 osgconv 语料，warm cache 2 次）

| 指标 | 改造前 | 改造后 | 变化 |
|---|---|---|---|
| **80 tile** 耗时 | 0.08 / 0.09 s | 0.03 / 0.03 s | ≈÷2.8 |
| **80 tile** 峰值 RSS | 44.6 / 44.5 MB | 14.1 / 13.6 MB | **−69%** |
| **1040 tile** 耗时 | 5.00 / 2.92 s | 0.31 / 0.47 s | ≈÷8（中位） |
| **1040 tile** 峰值 RSS | 540.3 / 562.0 MB | 19.2 / 18.4 MB | **−96.6%** |
| 1040 tile 输出 | 69,555,716 B | 69,555,716 B | **逐字节一致** |
| 1040 tile manifest 写入 | 1040 次整份（330.4 MB） | 1040 行 append + 1 次 finalize（0.47 MB） | ÷714 |

内存驻留点定位（改造前）：`resolve_meshes` 把全部 `ResolvedMesh`
（BTreeMap 缓存 + 每分块 `Mesh::clone`）一次性驻留，`--simplify` 时再复制
一份；1040 块 × ~64KB 网格 + 解析缓存 ≈ 540MB（实测吻合），且随块数线性
增长——5GB 语料（约 2 万块量级）外推 >10GB 内存必爆。改造后逐分块
「解析 →（简化）→ b3dm → 写盘 → 释放」，峰值 = 并行度 × 单分块工作集，
**与总块数无关**（1040 块实测 19MB ≈ 80 块实测 14MB）。

**产物一致性验证**：
- 方法：改造前/后 release 二进制分别对同一 pristine manifest 全量 build，
  `diff -r` 全部产物 + `cmp` 最终 manifest；
- 结果：80 tile 与 1040 tile 两组均为 **0 diff**（全部 b3dm、tileset.json、
  manifest 终态逐字节一致）；
- 自动化测试：`build_outputs_are_byte_identical_across_runs`（同语料两次
  独立 build 产物与 manifest 终态逐字节断言）、
  `build_resumes_from_partial_journal`（断点续切终态 = 一次性切完）、
  `build_failure_then_resume_produces_reference_state`（失败→修复→续切）。

## M4. 千 tile 基准（1040 tile 真实 osgconv）

语料：`make_1k_corpus.py` → 1024 叶（33×33 网格，2048 三角形/块，带
INLINE_DATA 内嵌 256×256 纹理）+ 16 粗层（17×17，各挂 64 子）= 1040 块；
磁盘 243MB（OSGB），单块 ≈ 245KB。

| 指标 | 改造前 | 改造后 |
|---|---|---|
| 端到端耗时（warm，2 次） | 5.00 / 2.92 s | 0.31 / 0.47 s |
| 峰值 RSS | 540.3 / 562.0 MB | 19.2 / 18.4 MB |
| b3dm 总输出 | 69.56 MB（1040 块） | 同左（字节一致） |
| manifest 大小（终态） | 352,511 B | 同左（字节一致） |
| manifest 写入次数 | 1040（每次整份） | 1040 append + 1 finalize |
| manifest 总写入量 | 330.4 MB / 2,751 ms | 0.47 MB / 15.7 ms |

**5GB 外推更新**（替换 M1 第 4 节的内存无界假设）：流式改造后峰值内存
不再随块数增长（O(并行度 × 单块)），5GB 级语料内核侧内存占用维持在
几十 MB 量级；耗时按「解压数据量」口径仍可用 M1 吞吐（≈25 MB/s）线性
外推，O(n²) manifest 项已消除。

## M5. 复现

```bash
# 语料（1040 tile，需 docker；本地构建 tangis-osgconv:bookworm 镜像）
cd testdata/osgb-real && python3 make_1k_corpus.py

# 写入策略微基准（旧 O(n²) vs journal）
cd kernel && cargo run -p tangis-manifest --release --example write_cost -- \
  ../testdata/osgb-real/manifest-1k.json <final-manifest.json>

# 端到端（/usr/bin/time -l 看 wall + peak RSS）
cargo build --release
./target/release/tangis-kernel build ../testdata/osgb-real/manifest-1k.json --out /tmp/out
```

## M6. 已知限制

1. 父分块（LOD 聚合）合并子分块时按需重新加载子源文件——同一子文件
   会被读多次（子自身一次 + 每个祖先一次）；语料文件均为 ~245KB 小文件，
   实测影响可忽略，但超大单文件（数百 MB/块）场景下重复读会成为成本；
2. journal 是 JSONL 文本（每行一条完成记录），千块级约 0.5MB；十万块级
   仍为 O(n) 追加，未做分段 compact；
3. 失败路径下 journal 保留、manifest 本体不更新——下游若直接读 manifest
   本体（而非等 finalize）看到的是「上次完成态」，需按 kernel 退出码判断；
4. 千 tile 语料 objs1k/（162MB）+ osgb-1k/（243MB）留在 testdata/ 未清理，
   供后续基准复用；`make_1k_corpus.py` 可随时重建。
