# osgb-real —— 真实 osgconv 语料 + 基准语料

本目录存放「真实 OSG 工具链」生产的语料与吞吐基准语料。生成日期：2026-10-04。

## 1. 真实 osgconv 语料（osgb/）

**工具链**：docker debian:bookworm-slim + openscenegraph 3.6.5（osgconv）。

**生成命令**（两步，均挂载本目录到容器 /work）：

```bash
# ① 生成 80 个带材质/贴图/UV 的 OBJ（宿主机 python3，无第三方依赖）
python3 make_obj_corpus.py
#    产物：objs/*.obj + objs/*.mtl（map_Kd ../textures/atlas256.png）
#          textures/atlas256.png（256×256 RGB，手工构造 PNG）

# ② docker 内批量 osgconv → OSGB
docker run --rm -v "$(pwd):/work" -w /work debian:bookworm-slim bash -c "
  apt-get update -qq && apt-get install -y -qq openscenegraph &&
  mkdir -p osgb &&
  ls objs/*.obj | xargs -P 8 -I{} sh -c 'osgconv {} osgb/\$(basename {} .obj).osgb'"
```

**结构**：8×8 = 64 个精细叶 tile（`Tile_+COL_+ROW.osgb`，33×33 网格地形，
2048 三角形）+ 4×4 = 16 个粗层 tile（`TileC_+COL_+ROW.osgb`，17×17 网格，
覆盖 2×2 叶 tile 区域），共 **80 个 tile**，全部带 `osg::Texture2D` +
`osg::Image`（引用 `objs/../textures/atlas256.png`）。文件约 245KB/个。

**manifest.json**：80 分块（16 父 lod=0，各挂 4 子；64 叶 lod=1），
供 build E2E / 解析验证。

> 转换期 osgconv 有 `Some faces were reversed by the plugin` 警告
> （OBJ→OSG 环绕方向修正），属正常行为。

## 2. 吞吐基准语料（bench-synth/）——合成格式 + 真实几何

**注意：这不是 osgconv 产物。** kernel 的 OSGB reader 目前只支持合成布局
（真实 osgconv 输出不兼容，见下节结论），基准用
`make_bench_corpus.py` 把 objs/ 的 80 个真实几何 OBJ 放大复制为
**640 个合成格式 OSGB**（zlib 压缩、带纹理引用）：

```bash
python3 make_bench_corpus.py    # 产出 bench-synth/*.osgb + manifest-bench.json
```

规模：640 tile / 594,560 顶点 / 1,114,112 三角形 / 磁盘 14.2MB（解压 26.3MB）。

## 3. 真实 OSGB 解析验证结论（2026-10-04，如实记录）

用 `tangis-kernel dump` / `build` 验证真实 osgconv 3.6.5 输出：

**失败点（阻塞级）**：

```
解析 osg::Geometry 时在第 167 字节处字段不匹配：
序言 z3: 期望 0（0x00000000），实际 3329（0x00000d01）
```

`kernel/crates/osgb` reader 是按 `testdata/osgb/make_corpus.py` 的**合成
布局**实现的（逐字段硬编码序言），与真实 OSG 3.6.5 序列化在
`osg::Geometry` 内部布局处不兼容。**M1 内核无法从真实 osgconv 语料解析出
几何，M2 需要真 OSG 解析器**（或按 OSG 3.6 真实布局重写 reader）。

**逐层兼容性明细**（真实文件 24KB 头部后的字节级比对）：

| 层 | 真实布局表现 | 合成 reader | 结论 |
|---|---|---|---|
| 24B 头部 | magic `0x6c910ea1`、features `0x1afb4545`、version 161、attributes `0x4`（bit2 括号） | 完全一致 | ✅ 兼容 |
| 对象帧骨架 | `[u32 名长][名][u64 bodysize][body]` | 一致 | ✅ 兼容 |
| Group/Geode | 序言 + StateSet 引用 + `[u64 slot][子对象]` 子列表 | 一致（dump 已成功走完 Group→Geode 帧） | ✅ 兼容 |
| 类清单 | Group/Geode/Geometry/DrawElementsUShort/EBO/Vec3Array/Vec2Array/VBO/StateSet/Texture2D/Image | 同族，均有对应处理分支 | ✅ 类集合兼容 |
| osg::Geometry | 序言含 VAO 标志（0x0d01），并在 StateSet 引用后**内联完整 StateSet 对象链**（StateSet→Material→Texture2D→Image） | 期望固定序言 `[…][u32 0][01 01 00]…` | ❌ **不兼容（阻塞）** |
| StateSet→纹理 | 真实为嵌套对象图：Geometry 内联 StateSet → Material（名 "atlas"）→ Texture2D → Image（文件名 `objs/../textures/atlas256.png`，为转换时路径） | 只支持 Geode 级 StateSet + 扁平 Image 约定 | ❌ 不兼容（同类问题） |
| DrawElementsUShort/EBO | 出现位置与次序在 Geometry 之后（primitive set 先行） | 支持该类本身 | ⚠️ 类支持、整体次序待真解析器验证 |
| Image 文件名语义 | 转换时 OBJ 目录相对路径（含 `../`） | 按 osgb 所在目录解析 | ⚠️ 需路径解析策略适配 |

**不做绕过**：以上为如实结论，reader 未做任何「为通过而 hack」的改动。

## 4. 文件清单

```
make_obj_corpus.py        OBJ 语料生成器（80 tile + 贴图）
make_bench_corpus.py      基准语料生成器（640 tile 合成 OSGB，旧格式已弃用）
make_1k_corpus.py         千 tile 基准语料生成器（M2-PERF，真实 osgconv）
objs/                     80× .obj/.mtl（中间产物，保留供复现）
objs1k/                   1040× .obj/.mtl（千 tile 中间产物，162MB）
textures/atlas256.png     256×256 RGB 贴图
osgb/                     80× 真实 osgconv OSGB
osgb-1k/                  1040× 真实 osgconv OSGB（243MB，32×32 叶 + 4×4 粗层）
manifest.json             真实语料 E2E manifest（16 父 + 64 子）
manifest-1k.json          千 tile manifest（16 父各挂 64 子 + 1024 叶）
bench-synth/              640× 合成 OSGB + manifest-bench.json + textures/
```

## 5. 千 tile 基准语料（osgb-1k/，M2-PERF）

`python3 make_1k_corpus.py`（需 docker，缺失时自动构建本地镜像
`tangis-osgconv:bookworm`）：生成 1040 个 OBJ → docker 内 osgconv 批量
转换为**真实 OSG 3.6 OSGB**（带 INLINE_DATA 内嵌纹理），并写
manifest-1k.json（16 父 lod=0 各挂 64 子 + 1024 叶 lod=1）。
供 kernel 切片千 tile 级基准（耗时/峰值 RSS/manifest 写入），数据见
`docs/benchmark-kernel-2026-10-04.md` 的 M2-PERF 章节。

> bench-synth/ 为旧合成格式，现 kernel reader（真 OSG 3.6）已不能解析，
> 仅作历史保留。
