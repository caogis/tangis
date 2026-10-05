# 数据质检报告（qc）字段说明

对应 kernel `qc` 子命令（M2-F08a，PRD F-08）与 `tangis-qc` crate 的
JSON 报告 schema。样例：本目录 `qc-report-sample-2026-10-04.json`
（`testdata/osgb-real/osgb` 80 tile 实测）。

## 用法

```bash
tangis-kernel qc --source <目录|OBJ|OSGB> --report <out.json> \
  [--degenerate-area 1e-10] [--normal-flip-deg 120] [--float-height 30] \
  [--float-volume-ratio 0.05] [--crack-gap 0.5] [--voxel-size 5] \
  [--skip-degenerate|--skip-normal-flip|--skip-floating|--skip-crack|--skip-self-intersect]
```

坐标约定：内部 Z-up（x/y 水平、z = 高度，米）。OSGB 经真实解析器读取并
恢复 Z-up；OBJ 按 Y-up 读取（与 kernel build 管线一致）——本仓库
`testdata/osgb-real/objs` 实际是 z-up 生成器语料，OBJ 通道请勿用于该目录，
请直接以 `osgb/` 目录为准。

## 根对象

| 字段 | 说明 |
|---|---|
| `schema_version` | 报告 schema 版本，当前 `"1"` |
| `generated_at_unix` | 生成时刻（Unix 秒，实测系统时钟） |
| `source` | `--source` 原样路径 |
| `tile_count` | 实际解析成功的 tile 数 |
| `thresholds` | 本次实测所用全部开关与阈值（`QcConfig` 原样记录，禁止造假即在此体现） |
| `tiles` | per-tile 明细（文件名排序） |
| `cracks` | 裂缝配对结果（每对相邻 tile 一条，零缝隙对也如实记录） |
| `crack_tiles_unpaired` | 未参与裂缝配对的 tile 数（无后缀下标 / 无同尺寸邻居 / 沟槽布局无共享边界） |
| `summary` | 汇总统计，`passed` = 全部检测零告警 |

## tiles[i]（单 tile 明细）

| 字段 | 说明 |
|---|---|
| `name` / `source_file` | tile 名（文件 stem）/ 来源文件路径 |
| `vertices` / `faces` | 顶点数 / 三角形数 |
| `degenerate` | 检测 1 结果（关闭时 `null`，下同） |
| `normal_flip` | 检测 2 结果 |
| `floating` | 检测 3 结果 |
| `self_intersect` | 检测 5 结果 |

### degenerate（退化三角形）

- `zero_area`：面积（|叉积|/2）< `thresholds.degenerate_area_eps`（默认 1e-10 m²）的面数；
- `repeated_vertex`：面内索引重复（a==b / b==c / a==c）；
- `duplicate_face`：排序索引三元组重复（重复实例各计 1）；
- `total`：三类命中次数合计（一面可多命中）；
- `samples`：最多 5 条 `{face, kinds, centroid}`（tile 局部坐标）。

### normal_flip（法线翻转）

共享边两侧面的几何法线（绕序叉积）夹角 > `thresholds.normal_flip_max_angle_deg`
（默认 120°）记翻转边。

- `edges_checked`：参与检查的共享边数（两侧均为有效面）；
- `flipped_edges` / `flipped_faces`：翻转边数 / 涉及面数；
- `isolated_faces`：仅命中 1 条翻转边的面数（孤立 = 单面绕序错误，区别于成片翻折）；
- `isolated_flip_rate` = `isolated_faces` / 有效面数；
- `samples`：最多 5 条 `{face_a, face_b, angle_deg, midpoint}`，按夹角降序。

### floating（悬浮块）

顶点按 `thresholds.floating_voxel_size`（默认 5 m）体素哈希 + 26 邻域 BFS
聚成连通分量；最大分视为主地面。分量满足
「最低点 − 主分量最高点 > `floating_height_above`（默认 30 m）」且
「包围盒体积占比 < `floating_max_volume_ratio`（默认 0.05）」判悬浮
（体积厚度取 0.01 m 下限，薄块按水平投影面积比较）。

- `components`：连通分量数。注意：顶点间距大于体素边长时分量会退化
  （如粗层 tile 17×17 网格间距 12.5 m > 5 m → 每顶点一分量），属近似算法
  的如实表现；
- `main_vertices`：主分量顶点数；
- `flagged[]`：`{vertices, bbox_min, bbox_max, clearance, volume_ratio, sample_vertex}`。

### self_intersect（自相交初检）

- `candidate_pairs`：AABB 空间哈希预筛候选对数（排除共享顶点索引的邻接对）；
- `intersections`：精确相交对数（平面侧判 + 线段-三角形 + 共面 2D）；
- `coverage`：恒 1.0（全量候选对，非抽样）；
- `method`：方法说明字符串；
- `samples`：最多 5 条 `{face_a, face_b, midpoint}`。

## cracks[i]（一对相邻 tile）

配对模式（按实测包围盒自动选择，见 `crates/qc/src/checks/crack.rs` 模块文档）：

- **包围盒邻接模式**（全局坐标布局）：某轴上 `|A.max − B.min| ≤ max(1 m, 跨度×5%)`
  且另一轴重叠 ≥ 50% 配对；共享平面 = 相对两面中点；
- **名下标模式**（纯局部坐标布局，类内包围盒全部重合）：tile 名后缀
  `+N_+M` 下标 Δ=1 配对，按下标 × 跨度平移。

边界顶点距离：坐标量化 1e-4 m 后取 3D 最近点（空间哈希，搜索半径 =
max(`crack_gap`, 1.0) m）。

| 字段 | 说明 |
|---|---|
| `tile_a` / `tile_b` / `shared_axis` / `plane` | 配对双方 / 共享轴（x/y）/ 共享平面全局坐标 |
| `border_vertices_a` | A 侧共享边界顶点数 |
| `matched_pairs` | 找到 B 侧最近点的 A 顶点数 |
| `unmatched_a` | 搜索半径内无 B 侧顶点的 A 顶点数（断边证据） |
| `median_gap` / `mean_gap` / `max_gap` | 配对距离统计（m） |
| `gaps_over_threshold` | 距离 > `crack_gap` 的数量（含 unmatched） |
| `positions[]` | 裂缝位置 = 边界线段中点 `{x, y, z, gap}`（最多 20 条，总数见 `gaps_over_threshold`） |

## 汇总统计（summary）

`tiles_with_*` = 命中 tile 数；`total_*` = 命中总量；
`crack_pairs_checked / crack_pairs_flagged / total_crack_segments` =
配对数 / 告警对数 / 裂缝段总数；`passed` = 全部为零。

## 真实语料基线（2026-10-04，80 tile，实测）

`testdata/osgb-real/osgb`（osconv 3.6.5 产物，8×8 精细 + 4×4 粗层）：

- 退化三角形 0；法线翻转边 0；悬浮分量 0；自相交 0 —— 几何干净；
- **裂缝 0 对配对（80 tile 全部未配对）**：该语料为 120 m 间距布局
  （100 m tile + 20 m 街道沟槽，见 `testdata/osgb-real/make_obj_corpus.py`，
  顶点为全局坐标），tile 之间不存在共享边界，包围盒邻接模式如实产出零配对
  ——不跨沟槽比较（那测的是地形高差，不是制造缝隙）。此结论是布局事实，
  不代表「接缝质量合格」；
- 粗层 tile（17×17 网格、12.5 m 间距）在默认 5 m 体素下每顶点独立成
  分量（`components` = 顶点数），悬浮判定仍有效（无高差即不告警）。

## 已知限制

1. 悬浮聚类为体素哈希近似：体素相邻的独立块会合并（宁漏报勿误报）；
   顶点间距 > 体素边长的稀疏网格分量数虚高（不影响悬浮判定结论）；
2. 裂缝检测是边界顶点最近邻距离，非逐边连续缝隙测量；边界采样密度不足时
   可能低估缝隙；
3. 自相交为三角形对精确测试，不含"几乎接触"的容差报告（间隙 < 1e-9 视为
   不相交）；
4. OBJ 通道按 Y-up 解释（与 kernel build 一致），z-up OBJ 语料会读错高度轴；
5. 法线翻转只比较恰被两面共享的边（非流形边跳过）。
