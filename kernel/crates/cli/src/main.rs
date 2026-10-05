//! tangis-kernel —— TanGIS 切片内核 CLI（M1）。
//!
//! 子命令：
//! - `plan   <manifest.json>`：加载分块清单，打印 LOD 拓扑排序结果；
//! - `run    <manifest.json>`：按拓扑序并行模拟执行分块（Rayon + 状态回写），
//!   演示断点续切语义：状态为 `Done` 的分块直接跳过，每个分块完成后串行持久化清单；
//! - `build  <manifest.json> --out <dir> [--task-id <id>] [--src <dir>]`：
//!   把清单转换为 3D Tiles 1.0 产物（`<id>.b3dm` + `tileset.json`），
//!   几何来自真实数据源（`.osgb` / `.obj`），分块并行**流式**转换
//!   （Rayon：逐块 解析 →（简化）→ b3dm 写盘 → 释放，M2-PERF），
//!   manifest 状态回写为 journal 增量追加 + 收尾整份重写一次
//!   （分块级断点检查点粒度不变，M2-PERF）。
//!
//! 几何来源解析（优先级）：
//! 1. 分块级 `chunk.source` 显式文件路径（相对路径基于 manifest 所在目录）；
//! 2. `--src <dir>` 目录下 `<chunk_id>.osgb` / `<chunk_id>.obj`；
//! 3. `manifest.source` 目录同上；
//! 4. 无自身来源的父分块：合并子分块网格（LOD 聚合）。
//!
//! **内核禁止产出占位/假几何**：任一分块无法解析出真实几何时 build 直接报错。
//! 分块的 tileset bounds 一律按解析出的真实几何包围盒回写 manifest。
//!
//! CLI 约定（供 Go 侧对接）：
//! - 退出码：成功 0，失败 1（错误信息走 stderr）；
//! - `--task-id <id>`：可选，覆盖 manifest 的 task_id 后再处理
//!   （同一份 manifest 模板服务多个任务时使用，回写 manifest 时一并生效）；
//! - `--src <dir>`：可选，几何源目录（优先级高于 manifest.source）；
//! - `--origin <lon> <lat> <height>`：可选（仅 build），地理定位原点
//!   （度/米，WGS84）。提供时 tileset.json 的 root tile 写入
//!   `transform` = ENU→ECEF 矩阵（局部坐标约定 x=东、y=北、z=天，单位米），
//!   全部子 tile 按规范继承 root 的 transform；**缺省时不写 transform**
//!   （历史行为：tile 位于地心附近的局部坐标系，Cesium 中需手动定位才可见）。
//! - 幂等性：同一 manifest 重复 `build` 时，已 Done 的分块不重复产出，
//!   `tileset.json` 每次全量重建；
//! - 分块 id 直接用作产物文件名，必须是文件名安全的字符串（不含 `/`、`\`）。

mod cancel;
mod geometry;
mod pointcloud;
mod raster;
mod terrain;
mod tiles;

use std::fs;
use std::path::{Path, PathBuf};
use std::thread::sleep;
use std::time::Duration;

use clap::{Parser, Subcommand};
use rayon::prelude::*;
use tangis_manifest::{topo_sort, Bounds, ChunkManifest, ChunkId, ChunkStatus};

#[derive(Parser)]
#[command(
    name = "tangis-kernel",
    version,
    about = "TanGIS 切片内核（M1）：分块 Manifest + LOD 拓扑排序 + 断点续切 + 真实几何 3D Tiles 产物"
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// 加载清单并打印拓扑排序结果（不执行）
    Plan {
        /// 分块清单 JSON 文件路径
        manifest_path: PathBuf,
    },
    /// 按拓扑序并行模拟执行分块（Done 跳过，逐块回写状态）
    Run {
        /// 分块清单 JSON 文件路径（执行过程中就地更新）
        manifest_path: PathBuf,
        /// 模拟每个分块执行的耗时（毫秒）
        #[arg(long, default_value_t = 80)]
        work_ms: u64,
        /// 取消标志文件路径（B7 协作式取消：文件出现即安全终止，exit 130，
        /// journal 保留可断点续切）
        #[arg(long)]
        cancel_file: Option<PathBuf>,
    },
    /// 把清单转换为 3D Tiles 1.0 产物（真实几何 b3dm + tileset.json），支持断点续切
    Build {
        /// 分块清单 JSON 文件路径（执行过程中就地更新状态）
        manifest_path: PathBuf,
        /// 产物输出目录（不存在则创建；tileset.json 位于该目录）
        #[arg(long)]
        out: PathBuf,
        /// 覆盖 manifest 的 task_id（可选，回写 manifest 时一并生效）
        #[arg(long)]
        task_id: Option<String>,
        /// 几何源目录（可选，优先级高于 manifest.source；
        /// 目录内按 `<chunk_id>.osgb` / `<chunk_id>.obj` 匹配分块）
        #[arg(long)]
        src: Option<PathBuf>,
        /// 地理定位原点 `--origin <lon> <lat> <height>`（度/米，WGS84，可选）：
        /// 提供时 root tile 写入 ENU→ECEF transform（子 tile 继承）；
        /// 缺省时不写 transform（tile 位于地心局部坐标系，历史行为）
        #[arg(long, num_args = 3, value_names = ["lon", "lat", "height"])]
        origin: Option<Vec<f64>>,
        /// QEM 网格简化比率（可选，(0,1]）：几何进入 b3dm 前按目标面数
        /// 比率做边折叠简化（边界保护：瓦片接缝顶点不动）；
        /// 简化率与涉及分块写入 <out>/simplify.json 元数据
        #[arg(long)]
        simplify: Option<f32>,
        /// 取消标志文件路径（B7 协作式取消：文件出现即安全终止，exit 130，
        /// 已完成分块保留在 journal，可断点续切）
        #[arg(long)]
        cancel_file: Option<PathBuf>,
    },
    /// QEM 网格简化独立验证：OBJ → 简化 → OBJ + 统计
    #[command(name = "simplify")]
    Simplify {
        /// 输入 OBJ 文件或目录（目录时处理全部 *.obj，--out 须为目录）
        #[arg(long)]
        source: PathBuf,
        /// 目标面数比率（(0,1]，目标面数 = 输入面数 × ratio，向上取整）
        #[arg(long)]
        ratio: f32,
        /// 输出 OBJ 文件或目录（与 --source 类型一致）
        #[arg(long)]
        out: PathBuf,
    },
    /// 倾斜模型几何质检（M2-F08a）：退化三角形/法线翻转/悬浮块/裂缝/自相交 → JSON 报告
    Qc {
        /// 数据源：目录（递归收集 *.osgb/*.obj，同 stem osgb 优先）或单个 OBJ/OSGB 文件
        #[arg(long)]
        source: PathBuf,
        /// 质检报告 JSON 输出路径
        #[arg(long)]
        report: PathBuf,
        /// 零面积阈值（m²，默认 1e-10）
        #[arg(long)]
        degenerate_area: Option<f64>,
        /// 法线翻转夹角阈值（度，默认 120）
        #[arg(long)]
        normal_flip_deg: Option<f64>,
        /// 悬浮判定高差阈值（m，默认 30）
        #[arg(long)]
        float_height: Option<f64>,
        /// 悬浮判定体积占比上限（默认 0.05）
        #[arg(long)]
        float_volume_ratio: Option<f64>,
        /// 裂缝判定缝隙阈值（m，默认 0.5）
        #[arg(long)]
        crack_gap: Option<f64>,
        /// 悬浮聚类体素边长（m，默认 5）
        #[arg(long)]
        voxel_size: Option<f64>,
        /// 关闭检测 1：退化三角形
        #[arg(long)]
        skip_degenerate: bool,
        /// 关闭检测 2：法线翻转
        #[arg(long)]
        skip_normal_flip: bool,
        /// 关闭检测 3：悬浮块
        #[arg(long)]
        skip_floating: bool,
        /// 关闭检测 4：裂缝初检
        #[arg(long)]
        skip_crack: bool,
        /// 关闭检测 5：自相交初检
        #[arg(long)]
        skip_self_intersect: bool,
    },
    /// 调试：解析 OSGB 文件，打印头部、对象树与网格统计
    Dump {
        /// OSGB 文件路径
        osgb_path: PathBuf,
    },
    /// GeoTIFF → XYZ 影像瓦片金字塔（WebMercatorQuad，PNG，无数据区透明）
    #[command(name = "raster2tiles")]
    Raster2Tiles {
        /// 影像源文件（GeoTIFF）
        #[arg(long)]
        source: PathBuf,
        /// 任务输出目录（瓦片位于 <output>/tiles/）
        #[arg(long)]
        output: PathBuf,
        /// 源类型（server 追加；当前仅支持 `image`）
        #[arg(long)]
        source_type: String,
        /// 金字塔布局：xyz（{z}/{x}/{y}）| wmts（{z}/{row}/{col}，TileMatrix REST 顺序）
        #[arg(long)]
        pyramid_layout: String,
        /// 金字塔元数据输出路径（server 约定 <output>/tiles/metadata.json）
        #[arg(long)]
        pyramid_metadata: PathBuf,
        /// 重采样方式：nearest | bilinear（默认 nearest）
        #[arg(long, default_value = "nearest")]
        resampling: String,
        /// 取消标志文件路径（B7 协作式取消：文件出现即安全终止，exit 130）
        #[arg(long)]
        cancel_file: Option<PathBuf>,
    },
    /// GeoTIFF DEM → Cesium Quantized-Mesh 地形瓦片金字塔（TMS 布局 + layer.json）
    #[command(name = "terrain2tiles")]
    Terrain2Tiles {
        /// 单波段 GeoTIFF DEM 源（uint16/float32；EPSG:4326/3857，4490 按 WGS84 近似）
        #[arg(long)]
        source: PathBuf,
        /// 任务输出目录（{z}/{x}/{y}.terrain + layer.json）
        #[arg(long)]
        output: PathBuf,
        /// 最小层级（缺省按数据范围自动推导：DEM 可被单瓦片覆盖的最小层级）
        #[arg(long)]
        min_zoom: Option<u32>,
        /// 最大层级（缺省按源分辨率自动推导，不向上超采样，封顶 22）
        #[arg(long)]
        max_zoom: Option<u32>,
        /// 每瓦片网格边长（顶点数/边，默认 65，范围 3..=513）
        #[arg(long, default_value_t = 65)]
        grid_size: u32,
        /// layer.json 输出路径（缺省 <output>/layer.json）
        #[arg(long)]
        layer_metadata: Option<PathBuf>,
        /// 取消标志文件路径（B7 协作式取消：文件出现即安全终止，exit 130）
        #[arg(long)]
        cancel_file: Option<PathBuf>,
    },
    /// LAS 点云 → 3D Tiles 点云瓦片（pnts + tileset.json）
    #[command(name = "las2pnts")]
    Las2Pnts {
        /// LAS 源文件（1.0–1.4，点格式 0/1/2/3/5/6/7/8；LAZ 暂不支持）
        #[arg(long)]
        source: PathBuf,
        /// 任务输出目录（tiles/{i}.pnts + tileset.json）
        #[arg(long)]
        output: PathBuf,
        /// 地理定位 [lon, lat, height]（度/米）：root.transform = ENU→ECEF
        #[arg(long, num_args = 3, value_names = ["lon", "lat", "height"])]
        origin: Option<Vec<f64>>,
        /// 每瓦片点数上限（默认 50000，范围 1000..=2000000）
        #[arg(long, default_value_t = 50_000)]
        max_points_per_tile: u32,
    },
    /// 合规算子：识别坐标系（GeoTIFF GeoKey / ESRI WKT .prj）
    #[command(name = "crs-identify")]
    CrsIdentify {
        /// 输入文件（.tif/.tiff 或 .prj）
        path: PathBuf,
    },
    /// 合规算子：CGCS2000 七参数（Bursa-Wolf，Position Vector 约定）BLH→BLH 转换
    #[command(name = "bursa")]
    Bursa {
        /// 七参数 JSON 文件（dx/dy/dz 米、rx/ry/rz 角秒、scale_ppm、source/target）
        #[arg(long)]
        params: PathBuf,
        /// 输入点集 JSON：{"points": [[lon, lat, h], ...]}（度/米）
        #[arg(long)]
        points: PathBuf,
        /// 输出 JSON 路径
        #[arg(long)]
        out: PathBuf,
    },
    /// 合规算子：DEM 区域脱密（flatten/noise），输出 GeoTIFF + 脱密记录 JSON
    #[command(name = "desensitize-dem")]
    DesensitizeDem {
        /// 输入 DEM（GeoTIFF，uint16/float32 单波段，无压缩）
        #[arg(long)]
        input: PathBuf,
        /// 区域 JSON：矩形 {"min_x","max_x","min_y","max_y"} 或多边形 {"ring":[[x,y],...]}
        #[arg(long)]
        region: PathBuf,
        /// 脱密模式：flatten（置平到区域均值+delta）| noise（delta 有界受控噪声）
        #[arg(long)]
        mode: String,
        /// delta：flatten 偏移（米，默认 0）；noise 噪声界（米，必填 > 0）
        #[arg(long)]
        delta: Option<f64>,
        /// 噪声种子（noise 必填，保证可复现）
        #[arg(long)]
        seed: Option<u64>,
        /// 输出脱密后 GeoTIFF 路径
        #[arg(long)]
        out: PathBuf,
        /// 脱密记录 JSON 输出路径（审批留痕）
        #[arg(long)]
        record: PathBuf,
    },
    /// OSGB 实时编辑算子（M2-F09a/F09b）：clip / flatten / ground-align。
    ///
    /// `--format obj`（默认，历史行为）：输出编辑后 OBJ（内部 Z-up）；
    /// `--format b3dm`：纹理透传 + 法线重算，out 为目录，产
    /// `{out}/{tile}.b3dm`（cli 既有 GLB/b3dm 写出，含 y-up 转换、
    /// 绕序修正、PNG atlas 嵌入）+ `{out}/tileset.json`（最小单层，
    /// geometricError 按包围盒）+ 留痕 JSON——server 编辑任务可直接
    /// 接入既有分发链路。
    Edit {
        /// 编辑算子
        #[command(subcommand)]
        op: EditOp,
    },
}

/// `edit` 各算子共享的输出参数（M2-F09b 起统一形态）。
#[derive(clap::Args)]
struct EditOutputArgs {
    /// 数据源：.osgb 文件或目录（目录处理全部 *.osgb，排序）
    #[arg(long)]
    source: PathBuf,
    /// 输出：obj 格式 = OBJ 文件/目录（与 --source 形态一致）；
    /// b3dm 格式 = 产物目录（{out}/{tile}.b3dm + tileset.json + 留痕）
    #[arg(long)]
    out: PathBuf,
    /// 操作留痕 JSON 输出路径（别名 --report；缺省：obj = <out>/edit-ops.json
    /// 或 <out>.ops.json，b3dm = <out>/ops.json）
    #[arg(long)]
    ops: Option<PathBuf>,
    /// 操作留痕 JSON 输出路径（--ops 的别名，server 接线契约用名）
    #[arg(long)]
    report: Option<PathBuf>,
    /// 输出格式：obj（默认，向后兼容）| b3dm（纹理透传 + tileset.json）
    #[arg(long, default_value = "obj", value_parser = ["obj", "b3dm"])]
    format: String,
    /// 只处理指定 tile（源文件 stem）；缺省或 all 处理全部
    #[arg(long)]
    tile: Option<String>,
}

/// `edit` 子命令组的三个算子（M2-F09a，PRD F-09 实时编辑前置）。
#[derive(Subcommand)]
enum EditOp {
    /// 抠除：移除平面一侧 / 水平 bbox 区域内三角形（边界切割闭合，空 Geometry 删除）
    Clip {
        #[command(flatten)]
        output: EditOutputArgs,
        /// 切割平面 "a,b,c,d"（f = ax+by+cz-d；与 --bbox 二选一，都缺省报错）
        #[arg(long, value_name = "A,B,C,D")]
        plane: Option<String>,
        /// 平面切割保留侧：pos = 保留 f>0 一侧，neg = 保留 f<0 一侧（默认 pos）
        #[arg(long, default_value = "pos", value_parser = ["pos", "neg"])]
        keep_side: String,
        /// 抠除的水平 bbox "minx,miny,maxx,maxy"（内部 z-up 语义，z 不限）
        #[arg(long, value_name = "MINX,MINY,MAXX,MAXY")]
        bbox: Option<String>,
    },
    /// 压平：bbox 区域内顶点 z 置为 --elevation，边界外 --feather 米线性过渡
    Flatten {
        #[command(flatten)]
        output: EditOutputArgs,
        /// 压平区域 "minx,miny,maxx,maxy"（水平 bbox）
        #[arg(long, value_name = "MINX,MINY,MAXX,MAXY")]
        bbox: String,
        /// 目标高程（米，内部 z-up）
        #[arg(long)]
        elevation: f64,
        /// 过渡带宽度（米，线性过渡防陡坎；0 = 硬边界，默认 0）
        #[arg(long, default_value_t = 0.0)]
        feather: f64,
    },
    /// 地面对齐：bbox 区域最低点整体刚性平移对齐到 --elevation 或参考 tile
    #[command(name = "ground-align")]
    GroundAlign {
        #[command(flatten)]
        output: EditOutputArgs,
        /// 对齐区域 "minx,miny,maxx,maxy"（水平 bbox）
        #[arg(long, value_name = "MINX,MINY,MAXX,MAXY")]
        bbox: String,
        /// 目标高程（米）；与 --ref 二选一，都缺省/同时给出均报错
        #[arg(long)]
        elevation: Option<f64>,
        /// 参考 tile（.osgb）：取其同 bbox 区域最低点为对齐目标
        #[arg(long, value_name = "OSGB")]
        r#ref: Option<PathBuf>,
    },
}

fn load_manifest(path: &Path) -> Result<ChunkManifest, String> {
    let raw = fs::read_to_string(path)
        .map_err(|e| format!("无法读取清单 {}: {e}", path.display()))?;
    serde_json::from_str(&raw).map_err(|e| format!("清单 JSON 解析失败: {e}"))
}

/// build 的结果统计（供 CLI 打印与测试断言）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BuildSummary {
    /// 本次新切的分块数。
    pub built: usize,
    /// 断点续切跳过的分块数。
    pub skipped: usize,
    /// 新产物字节数（不含 tileset.json）。
    pub bytes: u64,
}

/// OBJ → [`tangis_simplify::MeshData`]（tobj 解析，single_index + 三角化；
/// vt 存在且与 v 一一对应时带 UV）。
fn load_obj_mesh_data(path: &Path) -> Result<tangis_simplify::MeshData, String> {
    let (models, _materials) = tobj::load_obj(
        path,
        &tobj::LoadOptions { single_index: true, triangulate: true, ..Default::default() },
    )
    .map_err(|e| format!("解析 OBJ {} 失败: {e}", path.display()))?;
    let mut data = tangis_simplify::MeshData::default();
    for model in &models {
        let m = &model.mesh;
        let base = data.positions.len() as u32;
        data.positions
            .extend(m.positions.chunks_exact(3).map(|p| [p[0], p[1], p[2]]));
        data.indices.extend(m.indices.iter().map(|i| i + base));
        if m.texcoords.len() == m.positions.len() / 3 * 2 {
            let uvs = m
                .texcoords
                .chunks_exact(2)
                .map(|t| [t[0], t[1]])
                .collect::<Vec<_>>();
            match &mut data.uvs {
                Some(acc) => acc.extend(uvs),
                None => data.uvs = Some(uvs),
            }
        } else if !m.texcoords.is_empty() {
            data.uvs = None; // vt 数不符：宁缺勿假
        }
    }
    if data.positions.is_empty() || data.indices.is_empty() {
        return Err(format!("OBJ {} 不含可用几何", path.display()));
    }
    Ok(data)
}

/// MeshData → OBJ 文本（有 UV 时写 v + vt + f v/vt，否则 v + f）。
fn write_obj_mesh_data(path: &Path, mesh: &tangis_simplify::MeshData) -> Result<(), String> {
    let mut s = String::new();
    s.push_str("# tangis-kernel simplify 输出\n");
    for p in &mesh.positions {
        s.push_str(&format!("v {} {} {}\n", p[0], p[1], p[2]));
    }
    if let Some(uvs) = &mesh.uvs {
        for t in uvs {
            s.push_str(&format!("vt {} {}\n", t[0], t[1]));
        }
    }
    let has_uv = mesh.uvs.is_some();
    for f in mesh.indices.chunks_exact(3) {
        if has_uv {
            s.push_str(&format!(
                "f {}/{} {}/{} {}/{}\n",
                f[0] + 1, f[0] + 1, f[1] + 1, f[1] + 1, f[2] + 1, f[2] + 1
            ));
        } else {
            s.push_str(&format!("f {} {} {}\n", f[0] + 1, f[1] + 1, f[2] + 1));
        }
    }
    fs::write(path, s).map_err(|e| format!("无法写 OBJ {}: {e}", path.display()))
}

/// 确保文件父目录存在（输出路径可为尚不存在的多级目录）。
fn ensure_parent_dir(path: &Path) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        if !parent.as_os_str().is_empty() && !parent.exists() {
            fs::create_dir_all(parent)
                .map_err(|e| format!("无法创建目录 {}: {e}", parent.display()))?;
        }
    }
    Ok(())
}

/// build 流程的网格简化挂接点：只读借用 `tangis_osgb::Mesh`，输出新 Mesh
/// （纹理引用原样保留；法线由 simplify 按面积加权重算；UV 端点线性插值）。
fn apply_simplify(mesh: &tangis_osgb::Mesh, ratio: f32) -> Result<tangis_osgb::Mesh, String> {
    let data = tangis_simplify::MeshData {
        positions: mesh.vertices.clone(),
        indices: mesh.indices.clone(),
        normals: mesh.normals.clone(),
        uvs: mesh.uvs.clone(),
        // BATCHID 纯透传：幸存顶点保留自身 feature id，输出按幸存顶点
        // 重映射（数量与输出顶点一致），feature 语义在简化后保真
        batch_ids: mesh.batch_ids.clone(),
    };
    let (out, _stats) = tangis_simplify::simplify(&data, ratio)
        .map_err(|e| format!("网格简化失败: {e}"))?;
    // 纹理引用与内嵌字节原样保留
    Ok(tangis_osgb::Mesh {
        vertices: out.positions,
        indices: out.indices,
        normals: out.normals,
        uvs: out.uvs,
        texture: mesh.texture.clone(),
        texture_inline: mesh.texture_inline.clone(),
        batch_ids: out.batch_ids,
    })
}

/// `simplify` 子命令实现：OBJ（文件或目录）→ QEM 简化 → OBJ + 实测统计。
pub fn cmd_simplify(
    source: &Path,
    ratio: f32,
    out: &Path,
) -> Result<SimplifySummary, String> {
    if source.is_dir() {
        if !out.is_dir() && out.exists() {
            return Err(format!("--source 为目录时 --out 必须是目录: {}", out.display()));
        }
        fs::create_dir_all(out)
            .map_err(|e| format!("无法创建输出目录 {}: {e}", out.display()))?;
        let mut files: Vec<PathBuf> = fs::read_dir(source)
            .map_err(|e| format!("无法读取目录 {}: {e}", source.display()))?
            .filter_map(|e| e.ok())
            .map(|e| e.path())
            .filter(|p| {
                p.extension()
                    .and_then(|e| e.to_str())
                    .map(|e| e.eq_ignore_ascii_case("obj"))
                    .unwrap_or(false)
            })
            .collect();
        files.sort();
        if files.is_empty() {
            return Err(format!("目录 {} 中没有 .obj 文件", source.display()));
        }
        let mut summary = SimplifySummary {
            files: 0,
            vertices_in: 0,
            vertices_out: 0,
            faces_in: 0,
            faces_out: 0,
            collapses: 0,
            hausdorff_max: 0.0,
            elapsed_ms: 0.0,
        };
        for f in &files {
            let one = simplify_one(f, ratio, &out.join(f.file_name().unwrap()))?;
            summary.files += 1;
            summary.vertices_in += one.0.vertices_in;
            summary.vertices_out += one.0.vertices_out;
            summary.faces_in += one.0.faces_in;
            summary.faces_out += one.0.faces_out;
            summary.collapses += one.0.collapses;
            summary.hausdorff_max = summary.hausdorff_max.max(one.1);
            summary.elapsed_ms += one.2;
        }
        Ok(summary)
    } else {
        if out.is_dir() {
            return Err(format!("--source 为文件时 --out 必须是文件路径: {}", out.display()));
        }
        let (stats, h, ms) = simplify_one(source, ratio, out)?;
        Ok(SimplifySummary {
            files: 1,
            vertices_in: stats.vertices_in,
            vertices_out: stats.vertices_out,
            faces_in: stats.faces_in,
            faces_out: stats.faces_out,
            collapses: stats.collapses,
            hausdorff_max: h,
            elapsed_ms: ms,
        })
    }
}

/// 单文件简化：返回 (统计, Hausdorff, 耗时 ms)。
fn simplify_one(
    src: &Path,
    ratio: f32,
    dst: &Path,
) -> Result<(tangis_simplify::SimplifyStats, f64, f64), String> {
    let data = load_obj_mesh_data(src)?;
    let t0 = std::time::Instant::now();
    let (out, stats) = tangis_simplify::simplify(&data, ratio)
        .map_err(|e| format!("简化 {} 失败: {e}", src.display()))?;
    let elapsed_ms = t0.elapsed().as_secs_f64() * 1000.0;
    let h = tangis_simplify::approx_hausdorff(&data, &out);
    ensure_parent_dir(dst)?;
    write_obj_mesh_data(dst, &out)?;
    Ok((stats, h, elapsed_ms))
}

/// run 的结果统计。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RunSummary {
    pub executed: usize,
    pub skipped: usize,
}

/// simplify 子命令的结果统计（多文件时顶点/面数为累计值）。
#[derive(Debug, Clone, PartialEq)]
pub struct SimplifySummary {
    pub files: usize,
    pub vertices_in: usize,
    pub vertices_out: usize,
    pub faces_in: usize,
    pub faces_out: usize,
    pub collapses: usize,
    /// 全部文件中最大的单侧顶点采样 Hausdorff（原顶点 → 简化网格表面）。
    pub hausdorff_max: f64,
    pub elapsed_ms: f64,
}

fn main() {
    let cli = Cli::parse();
    if let Err(err) = run_command(cli.command) {
        if cancel::is_canceled_err(&err) {
            // B7：协作式取消——安全终止（journal 已保留，断点续切可恢复），
            // 退出码 130 供 worker/运维区分「用户取消」与「执行失败」。
            eprintln!("{err}");
            eprintln!("已安全终止：已完成部分已落盘，剩余分块保持 pending，重新运行即可断点续切。");
            std::process::exit(cancel::CANCEL_EXIT_CODE);
        }
        eprintln!("错误: {err}");
        std::process::exit(1);
    }
}

fn run_command(command: Command) -> Result<(), String> {
    match command {
        Command::Dump { osgb_path } => {
            let data = fs::read(&osgb_path)
                .map_err(|e| format!("无法读取 {}: {e}", osgb_path.display()))?;
            let mut hr = tangis_osgb::Reader::new(&data);
            let header =
                tangis_osgb::Header::parse(&mut hr).map_err(|e| format!("头部解析失败: {e}"))?;
            let (scene, objects) = tangis_osgb::parse_bytes_traced(&data)
                .map_err(|e| format!("解析 {} 失败: {e}", osgb_path.display()))?;
            println!("文件: {}（{} 字节）", osgb_path.display(), data.len());
            println!(
                "头部: magic=0x{:08x} version={} attributes=0x{:x} 压缩={}",
                header.magic,
                header.version,
                header.attributes,
                if header.is_compressed() { "是" } else { "否" }
            );
            println!("对象树（{} 个对象）:", objects.len());
            for (i, o) in objects.iter().enumerate() {
                let note = if o.skipped { "  [未知类，整体跳过]" } else { "" };
                println!(
                    "  {:>2}. @{:<6} {:<26} body={} 字节{}",
                    i + 1,
                    o.offset,
                    o.class,
                    o.body_size,
                    note
                );
            }
            println!("网格（{} 个）:", scene.meshes.len());
            for (i, m) in scene.meshes.iter().enumerate() {
                match m.bounds() {
                    Some((min, max)) => println!(
                        "  {:>2}. 顶点={} 索引={} 法线={} uv={} bounds=({:?} .. {:?})",
                        i + 1,
                        m.vertices.len(),
                        m.indices.len(),
                        m.normals.as_ref().map_or(0, |n| n.len()),
                        m.uvs.as_ref().map_or(0, |u| u.len()),
                        min,
                        max
                    ),
                    None => println!(
                        "  {:>2}. 顶点={} 索引={}（空网格）",
                        i + 1,
                        m.vertices.len(),
                        m.indices.len()
                    ),
                }
            }
            Ok(())
        }
        Command::Plan { manifest_path } => {
            let manifest = load_manifest(&manifest_path)?;
            let order = topo_sort(&manifest).map_err(|e| e.to_string())?;
            println!("任务 {}：共 {} 个分块，拓扑执行顺序（子 tile 先于父 tile）：",
                manifest.task_id, order.len());
            for (i, id) in order.iter().enumerate() {
                let chunk = manifest.chunk(id).expect("topo_sort 只输出清单内分块");
                println!(
                    "  {:>2}. {}  lod={}  children={}  status={:?}",
                    i + 1,
                    chunk.id,
                    chunk.lod,
                    chunk.children.len(),
                    chunk.status
                );
            }
            Ok(())
        }
        Command::Run { manifest_path, work_ms, cancel_file } => {
            let cancel = cancel::CancelFlag::from_path(cancel_file);
            let summary = cmd_run(&manifest_path, work_ms, &cancel)?;
            println!(
                "完成：本次新执行 {} 个，续切跳过 {} 个。清单已回写至 {}",
                summary.executed,
                summary.skipped,
                manifest_path.display()
            );
            Ok(())
        }
        Command::Build { manifest_path, out, task_id, src, origin, simplify, cancel_file } => {
            // --origin 期望恰好 3 个数（lon/lat/height），clap num_args=3 已约束数量
            let origin = match origin.as_deref() {
                Some(&[lon, lat, height]) => Some([lon, lat, height]),
                Some(other) => {
                    return Err(format!(
                        "--origin 需要 3 个值（经度 纬度 高程），实际 {} 个",
                        other.len()
                    ));
                }
                None => None,
            };
            if let Some(r) = simplify {
                if !(r > 0.0 && r <= 1.0) {
                    return Err(format!("--simplify 比率 {r} 非法：须在 (0, 1] 区间"));
                }
            }
            let cancel = cancel::CancelFlag::from_path(cancel_file);
            let summary = cmd_build(
                &manifest_path, &out, task_id.as_deref(), src.as_deref(), origin, simplify, &cancel,
            )?;
            println!("tileset: {}", out.join("tileset.json").display());
            println!(
                "build 完成：本次新切 {} 个 b3dm，续切跳过 {} 个，共 {} 字节",
                summary.built, summary.skipped, summary.bytes
            );
            Ok(())
        }
        Command::Simplify { source, ratio, out } => {
            let summary = cmd_simplify(&source, ratio, &out)?;
            println!(
                "simplify 完成：{} 个文件，顶点 {}→{}，面数 {}→{}，\
                 折叠 {} 次，Hausdorff（原顶点→简化面）最大 {:.6}，总耗时 {:.1} ms",
                summary.files,
                summary.vertices_in, summary.vertices_out,
                summary.faces_in, summary.faces_out,
                summary.collapses,
                summary.hausdorff_max,
                summary.elapsed_ms
            );
            println!("输出: {}", out.display());
            Ok(())
        }
        Command::Qc {
            source, report, degenerate_area, normal_flip_deg, float_height,
            float_volume_ratio, crack_gap, voxel_size,
            skip_degenerate, skip_normal_flip, skip_floating, skip_crack, skip_self_intersect,
        } => {
            let mut cfg = tangis_qc::QcConfig {
                degenerate: !skip_degenerate,
                normal_flip: !skip_normal_flip,
                floating: !skip_floating,
                crack: !skip_crack,
                self_intersect: !skip_self_intersect,
                ..tangis_qc::QcConfig::default()
            };
            if let Some(v) = degenerate_area {
                if v < 0.0 {
                    return Err(format!("--degenerate-area {v} 非法：须 ≥ 0"));
                }
                cfg.degenerate_area_eps = v;
            }
            if let Some(v) = normal_flip_deg {
                if !(0.0..=180.0).contains(&v) {
                    return Err(format!("--normal-flip-deg {v} 非法：须在 [0, 180] 区间"));
                }
                cfg.normal_flip_max_angle_deg = v;
            }
            if let Some(v) = float_height {
                if v < 0.0 {
                    return Err(format!("--float-height {v} 非法：须 ≥ 0"));
                }
                cfg.floating_height_above = v;
            }
            if let Some(v) = float_volume_ratio {
                if !(v > 0.0 && v < 1.0) {
                    return Err(format!("--float-volume-ratio {v} 非法：须在 (0, 1) 区间"));
                }
                cfg.floating_max_volume_ratio = v;
            }
            if let Some(v) = crack_gap {
                if v <= 0.0 {
                    return Err(format!("--crack-gap {v} 非法：须 > 0"));
                }
                cfg.crack_gap = v;
            }
            if let Some(v) = voxel_size {
                if v <= 0.0 {
                    return Err(format!("--voxel-size {v} 非法：须 > 0"));
                }
                cfg.floating_voxel_size = v;
            }
            let rep = tangis_qc::run_qc_from_source(&source, &cfg)?;
            tangis_qc::write_report(&rep, &report)?;
            let s = &rep.summary;
            println!(
                "qc 完成：{} tile，退化 {}（{} tile），翻转边 {}（{} tile），\
                 悬浮分量 {}（{} tile），裂缝 {}/{} 对、{} 段，自相交 {}（{} tile）",
                rep.tile_count,
                s.total_degenerate, s.tiles_with_degenerate,
                s.total_flipped_edges, s.tiles_with_normal_flip,
                s.total_floating_components, s.tiles_with_floating,
                s.crack_pairs_flagged, s.crack_pairs_checked, s.total_crack_segments,
                s.total_self_intersections, s.tiles_with_self_intersect,
            );
            println!("报告: {}", report.display());
            if s.passed {
                println!("结论: PASS（全部检测零告警）");
            } else {
                println!("结论: FAIL（存在告警，详见报告）");
            }
            Ok(())
        }
        Command::Raster2Tiles { source, output, source_type, pyramid_layout, pyramid_metadata, resampling, cancel_file } => {
            let args = raster::xyz::RasterArgs {
                source,
                output,
                source_type,
                pyramid_layout,
                pyramid_metadata,
                resampling,
                cancel_file,
            };
            let summary = raster::xyz::cmd_raster2tiles(&args)?;
            println!(
                "raster2tiles 完成：{} 张瓦片（z{}..z{}），extent={:?}",
                summary.tiles_written, summary.min_zoom, summary.max_zoom, summary.extent
            );
            println!("metadata: {}", args.pyramid_metadata.display());
            Ok(())
        }
        Command::Terrain2Tiles { source, output, min_zoom, max_zoom, grid_size, layer_metadata, cancel_file } => {
            let args = terrain::TerrainArgs {
                source,
                output,
                min_zoom,
                max_zoom,
                grid_size,
                layer_metadata,
                cancel_file,
            };
            let summary = terrain::cmd_terrain2tiles(&args)?;
            println!(
                "terrain2tiles 完成：{} 张瓦片（z{}..z{}，quantized-mesh-1.0/tms，gzip），\
                 bounds(wgs84)={:?}，高程 {:.2}..{:.2}",
                summary.tiles_written, summary.min_zoom, summary.max_zoom,
                summary.bounds, summary.height_min, summary.height_max
            );
            let layer = args.layer_metadata.unwrap_or_else(|| args.output.join("layer.json"));
            println!("layer.json: {}", layer.display());
            Ok(())
        }
        Command::Las2Pnts { source, output, origin, max_points_per_tile } => {
            let origin = origin.map(|v| [v[0], v[1], v[2]]);
            let args = pointcloud::LasArgs {
                source,
                output,
                origin,
                max_points_per_tile,
            };
            let summary = pointcloud::cmd_las2pnts(&args)?;
            println!(
                "las2pnts 完成：{} 点 → {} 张 pnts 瓦片（网格 {:?}），颜色={}",
                summary.point_count, summary.tiles_written, summary.grid,
                if summary.has_color { "RGB" } else { "无" }
            );
            println!("tileset.json: {}", args.output.join("tileset.json").display());
            Ok(())
        }
        Command::CrsIdentify { path } => {
            let info = tangis_geo::crs::identify_file(&path)?;
            println!("{}", serde_json::to_string_pretty(&info).map_err(|e| e.to_string())?);
            if path
                .extension()
                .and_then(|e| e.to_str())
                .map(|e| e.eq_ignore_ascii_case("tif") || e.eq_ignore_ascii_case("tiff"))
                .unwrap_or(false)
            {
                let bytes = fs::read(&path).map_err(|e| format!("无法读取 {}: {e}", path.display()))?;
                if let Ok(meta) = tangis_geo::crs::read_georef_meta(&bytes) {
                    println!(
                        "georef: pixel_scale={:?} tiepoint_origin(i,j,x,y)={:?}",
                        meta.pixel_scale, meta.tiepoint_origin
                    );
                }
            }
            Ok(())
        }
        Command::Bursa { params, points, out } => {
            let summary = tangis_geo::bursa::run_from_files(&params, &points, &out)?;
            println!(
                "bursa 完成：{} 点，{} → {}（{:?} 约定）",
                summary.count, summary.source, summary.target, summary.convention
            );
            println!("输出: {}", out.display());
            Ok(())
        }
        Command::DesensitizeDem { input, region, mode, delta, seed, out, record } => {
            let rec = tangis_geo::dem::run_from_files(
                &input, &region, &mode, delta, seed, &out, &record,
            )?;
            println!(
                "desensitize-dem 完成：mode={} delta={} seed={} 区域像素 {}，\
                 高程 min/max/mean {:.3}/{:.3}/{:.3} → {:.3}/{:.3}/{:.3}",
                rec.mode,
                rec.delta,
                rec.seed,
                rec.region_pixels,
                rec.before.min,
                rec.before.max,
                rec.before.mean,
                rec.after.min,
                rec.after.max,
                rec.after.mean
            );
            println!("输出: {}", out.display());
            println!("记录: {}", record.display());
            Ok(())
        }
        Command::Edit { op } => run_edit(op),
    }
}

/// `edit` 子命令组：参数解析 → tangis-editops 算子 → 摘要打印。
/// 留痕 JSON 总是产出（编辑历史，server 审批接线的输入）。
/// `--format b3dm` 时编辑网格再走 cli 既有 GLB/b3dm 写出（纹理透传）
/// 并产出最小 tileset.json（M2-F09b）。
fn run_edit(op: EditOp) -> Result<(), String> {
    fn default_ops_path(source: &Path, out: &Path) -> PathBuf {
        if source.is_dir() {
            out.join("edit-ops.json")
        } else {
            out.with_extension("ops.json")
        }
    }
    fn parse_bbox(s: &str) -> Result<(f64, f64, f64, f64), String> {
        let v = tangis_editops::parse_numbers(s, 4, "--bbox")?;
        Ok((v[0], v[1], v[2], v[3]))
    }
    fn parse_format(s: &str) -> Result<tangis_editops::OutputFormat, String> {
        match s {
            "obj" => Ok(tangis_editops::OutputFormat::Obj),
            "b3dm" => Ok(tangis_editops::OutputFormat::B3dm),
            other => Err(format!("--format {other} 非法：仅支持 obj | b3dm")),
        }
    }
    fn finish(
        out: &Path,
        ops: &Path,
        format: tangis_editops::OutputFormat,
        outcome: tangis_editops::EditOutcome,
    ) -> Result<(), String> {
        if format == tangis_editops::OutputFormat::B3dm {
            write_edit_b3dm(&outcome, out)?;
            println!(
                "edit 完成：{} tile（--format b3dm，纹理透传 + tileset.json）",
                outcome.record.totals["tiles"]
            );
        } else {
            println!("edit 完成：{} tile", outcome.record.totals["tiles"]);
        }
        println!("输出: {}", out.display());
        println!("留痕: {}", ops.display());
        Ok(())
    }
    match op {
        EditOp::Clip { output, plane, keep_side, bbox } => {
            let EditOutputArgs { source, out, ops, report, format, tile } = output;
            let region = match (plane, bbox) {
                (Some(p), None) => {
                    let v = tangis_editops::parse_numbers(&p, 4, "--plane")?;
                    tangis_editops::clip::Region::Plane {
                        a: v[0],
                        b: v[1],
                        c: v[2],
                        d: v[3],
                        keep_positive: keep_side == "pos",
                    }
                }
                (None, Some(b)) => {
                    let (minx, miny, maxx, maxy) = parse_bbox(&b)?;
                    tangis_editops::clip::Region::BBox { minx, miny, maxx, maxy }
                }
                (Some(_), Some(_)) => {
                    return Err("--plane 与 --bbox 只能二选一".into());
                }
                (None, None) => {
                    return Err("clip 需要 --plane \"a,b,c,d\" 或 --bbox \"minx,miny,maxx,maxy\"".into());
                }
            };
            let fmt = parse_format(&format)?;
            let ops = ops.or(report).unwrap_or_else(|| {
                if fmt == tangis_editops::OutputFormat::B3dm {
                    out.join("ops.json")
                } else {
                    default_ops_path(&source, &out)
                }
            });
            let opts = tangis_editops::EditRunOpts { format: fmt, tile };
            let outcome = tangis_editops::run_clip(&source, &region, &out, &ops, &opts)?;
            let t = &outcome.record.totals;
            println!(
                "edit clip：面 {}→{}（整体移除 {}，切割 {}），\
                 顶点 {}→{}，面积 {:.3}→{:.3} m²",
                t["faces_in"], t["faces_out"],
                t["faces_removed"], t["faces_split"],
                t["vertices_in"], t["vertices_out"],
                t["area_in"], t["area_kept"],
            );
            finish(&out, &ops, fmt, outcome)
        }
        EditOp::Flatten { output, bbox, elevation, feather } => {
            let EditOutputArgs { source, out, ops, report, format, tile } = output;
            let (minx, miny, maxx, maxy) = parse_bbox(&bbox)?;
            let params = tangis_editops::flatten::FlattenParams {
                minx, miny, maxx, maxy, elevation, feather,
            };
            let fmt = parse_format(&format)?;
            let ops = ops.or(report).unwrap_or_else(|| {
                if fmt == tangis_editops::OutputFormat::B3dm {
                    out.join("ops.json")
                } else {
                    default_ops_path(&source, &out)
                }
            });
            let opts = tangis_editops::EditRunOpts { format: fmt, tile };
            let outcome = tangis_editops::run_flatten(&source, &params, &out, &ops, &opts)?;
            println!(
                "edit flatten：区域内顶点 {}，改动 {}（feather={feather}，\
                 elevation={elevation}）",
                outcome.record.totals["region_vertices"],
                outcome.record.totals["vertices_moved"],
            );
            finish(&out, &ops, fmt, outcome)
        }
        EditOp::GroundAlign { output, bbox, elevation, r#ref } => {
            let EditOutputArgs { source, out, ops, report, format, tile } = output;
            let (minx, miny, maxx, maxy) = parse_bbox(&bbox)?;
            match (elevation, &r#ref) {
                (Some(_), Some(_)) => return Err("--elevation 与 --ref 只能二选一".into()),
                (None, None) => {
                    return Err("ground-align 需要 --elevation <米> 或 --ref <参考.osgb>".into())
                }
                _ => {}
            }
            let params = tangis_editops::align::AlignParams {
                minx, miny, maxx, maxy, target: elevation.unwrap_or(0.0),
            };
            let fmt = parse_format(&format)?;
            let ops = ops.or(report).unwrap_or_else(|| {
                if fmt == tangis_editops::OutputFormat::B3dm {
                    out.join("ops.json")
                } else {
                    default_ops_path(&source, &out)
                }
            });
            let opts = tangis_editops::EditRunOpts { format: fmt, tile };
            let outcome =
                tangis_editops::run_align(&source, &params, r#ref.as_deref(), &out, &ops, &opts)?;
            println!(
                "edit ground-align：目标高程 {:.3} m，dz 合计 {:.3}",
                outcome.record.params["target_elevation"].as_f64().unwrap_or(0.0),
                outcome.record.totals["dz_sum"],
            );
            finish(&out, &ops, fmt, outcome)
        }
    }
}

/// `edit --format b3dm` 的产物落盘（M2-F09b）：逐 tile 合并编辑网格 →
/// 纹理解析（内嵌优先，复用 cli build 管线）→ 既有 b3dm 写出（含 GLB
/// y-up 转换、绕序修正）；随后写最小 tileset.json（复用 build 的
/// tileset 生成器，geometricError 按真实包围盒）。
/// 留痕 JSON 已由 tangis-editops 运行器写盘，此处不再重复。
fn write_edit_b3dm(
    outcome: &tangis_editops::EditOutcome,
    out: &Path,
) -> Result<(), String> {
    use tangis_manifest::{Bounds, Chunk, ChunkId, ChunkManifest, ChunkStatus};

    let mut chunks = Vec::new();
    for et in &outcome.edited {
        if et.meshes.is_empty() {
            return Err(format!(
                "tile {} 编辑后无几何，无法产出 b3dm（编辑区域覆盖了全部三角形？）",
                et.tile
            ));
        }
        // 纹理引用解析（相对源 OSGB 目录；有内嵌字节时跳过文件存在性检查）
        let mut meshes = et.meshes.clone();
        for m in &mut meshes {
            geometry::resolve_osgb_texture(m, &et.source)?;
        }
        let refs: Vec<&tangis_osgb::Mesh> = meshes.iter().collect();
        let merged = geometry::merge_tile_meshes(&refs)
            .map_err(|e| format!("tile {} 合并编辑网格失败: {e}", et.tile))?;
        let feature_sources =
            vec![format!("{}.osgb", et.tile); tangis_osgb::feature_count(&merged) as usize];
        let texture_info = geometry::load_texture_image(&merged)
            .map_err(|e| format!("tile {} 纹理不可用: {e}", et.tile))?;
        let b3dm = match &texture_info {
            None => tiles::b3dm::build_b3dm_from_mesh(&merged, &feature_sources),
            Some(tex) => tiles::b3dm::build_b3dm_from_mesh_textured(
                &merged,
                &tiles::b3dm::TextureImage { bytes: &tex.bytes, mime_type: tex.mime_type },
                &feature_sources,
            )
            .map_err(|e| format!("tile {} 生成带纹理 b3dm 失败: {e}", et.tile))?,
        };
        let path = out.join(format!("{}.b3dm", et.tile));
        fs::write(&path, &b3dm)
            .map_err(|e| format!("无法写产物 {}: {e}", path.display()))?;
        let (min, max) = merged
            .bounds()
            .ok_or_else(|| format!("tile {} 无几何包围盒", et.tile))?;
        chunks.push(Chunk {
            id: ChunkId::new(et.tile.clone()),
            lod: 1,
            bounds: Bounds {
                min: [min[0] as f64, min[1] as f64, min[2] as f64],
                max: [max[0] as f64, max[1] as f64, max[2] as f64],
            },
            status: ChunkStatus::Done,
            attempts: 1,
            children: vec![],
            source: None,
            source_offset: None,
        });
        println!("  {}  OK（{} 字节）", et.tile, b3dm.len());
    }
    let manifest = ChunkManifest { task_id: "edit".into(), chunks, source: None };
    let tileset = tiles::tileset::build_tileset_json(&manifest, None)?;
    let path = out.join("tileset.json");
    let text = serde_json::to_string_pretty(&tileset).map_err(|e| e.to_string())?;
    fs::write(&path, text + "\n")
        .map_err(|e| format!("无法写产物 {}: {e}", path.display()))?;
    Ok(())
}

/// `run` 子命令实现：并行模拟执行 + journal 增量回写（finalize 整份收尾）。
///
/// `cancel`（B7）：分块边界轮询取消标志；检测到即安全终止——已执行分块的
/// journal 已落盘、剩余保持 pending，返回带 [`cancel::CANCEL_ERR_PREFIX`]
/// 的 Err（main 转 exit 130），不 finalize manifest。
pub fn cmd_run(
    manifest_path: &Path,
    work_ms: u64,
    cancel: &cancel::CancelFlag,
) -> Result<RunSummary, String> {
    let mut manifest = load_manifest(manifest_path)?;
    // 断点续切：回放增量 journal（旧格式无 journal → 0 条）
    let journal_path = tangis_manifest::journal_path_for(manifest_path);
    tangis_manifest::replay(&mut manifest, &journal_path)
        .map_err(|e| format!("journal 回放失败: {e}"))?;
    let order = topo_sort(&manifest).map_err(|e| e.to_string())?;
    println!("任务 {}：开始切片，共 {} 个分块（断点续切：Done 跳过）",
        manifest.task_id, order.len());

    let index_of: std::collections::BTreeMap<&ChunkId, usize> = manifest
        .chunks
        .iter()
        .enumerate()
        .map(|(i, c)| (&c.id, i))
        .collect();

    // 待执行分块索引（拓扑序）
    let pending: Vec<usize> = order
        .iter()
        .map(|id| index_of[id])
        .filter(|&i| manifest.chunks[i].status != ChunkStatus::Done)
        .collect();
    let skipped = order.len() - pending.len();

    // 并行模拟执行（每个分块 sleep 互不依赖）；B7：分块边界协作取消
    pending.par_iter().for_each(|&i| {
        let chunk = &manifest.chunks[i];
        if cancel.is_canceled() {
            return; // 取消：未开始的分块直接跳过（journal 不记录，保持 pending）
        }
        sleep(Duration::from_millis(work_ms));
        let _ = chunk; // 模拟工作负载
    });
    if cancel.is_canceled() {
        return Err(cancel::canceled_err(cancel));
    }

    // 串行增量回写：逐块追加 journal（断点检查点），finalize 整份收尾一次
    let mut journal = if !pending.is_empty() || journal_path.exists() {
        Some(tangis_manifest::Journal::create(journal_path.clone())
            .map_err(|e| format!("无法创建 journal {}: {e}", journal_path.display()))?)
    } else {
        None
    };
    let mut executed = 0usize;
    for &i in &pending {
        let chunk = &mut manifest.chunks[i];
        chunk.status = ChunkStatus::Done;
        chunk.attempts += 1;
        executed += 1;
        if let Some(j) = journal.as_mut() {
            j.record(&tangis_manifest::ChunkOutcome {
                id: chunk.id.clone(),
                attempts: chunk.attempts,
                bounds: None,
            })
            .map_err(|e| format!("journal 写入失败: {e}"))?;
        }
    }
    if let Some(j) = journal {
        j.finalize(&manifest, manifest_path)
            .map_err(|e| format!("manifest 收尾失败: {e}"))?;
    }

    Ok(RunSummary { executed, skipped })
}

/// `build` 子命令实现：真实几何 → 并行 b3dm → manifest 增量回写 → tileset.json。
///
/// `origin` = `--origin <lon> <lat> <height>`（度/米，WGS84）：Some 时
/// tileset root 写 ENU→ECEF transform（D1）；None 时不写 transform。
///
/// `simplify_ratio` = `--simplify <ratio>`：Some 时网格在进 b3dm 前做
/// QEM 边折叠简化（边界保护），并在产物目录写 `simplify.json` 元数据。
///
/// M2-PERF 两项改造（产物语义不变）：
/// 1. **流式网格**：逐分块「解析 →（简化）→ b3dm 写盘 → 释放」，
///    Rayon 分块级并行保持；不再把全部网格驻留内存（旧实现峰值
///    O(全部分块)，5GB 语料内存爆风险），峰值降为 O(单分块 × 并行度)；
/// 2. **manifest 增量回写**：每完成一分块向 `<manifest>.journal` 追加一行
///    （断点检查点粒度不变），结束时 `finalize` 整份重写一次并删除
///    journal——总写入量从 O(n²) 降为 O(n)；启动时回放 journal 支持
///    断点续切（Done 跳过语义不变，旧格式 manifest 照常读取）。
pub fn cmd_build(
    manifest_path: &Path,
    out: &Path,
    task_id: Option<&str>,
    src: Option<&Path>,
    origin: Option<[f64; 3]>,
    simplify_ratio: Option<f32>,
    cancel: &cancel::CancelFlag,
) -> Result<BuildSummary, String> {
    if let Some([lon, lat, _]) = origin {
        if !(-180.0..=180.0).contains(&lon) || !(-90.0..=90.0).contains(&lat) {
            return Err(format!(
                "--origin 经纬度越界：lon={lon}（[-180,180]） lat={lat}（[-90,90]）"
            ));
        }
    }
    let mut manifest = load_manifest(manifest_path)?;
    if let Some(id) = task_id {
        // 覆盖任务 id：同一份 manifest 模板可服务多个任务
        manifest.task_id = id.to_string();
    }
    // 断点续切：回放增量 journal（旧格式无 journal → 0 条）
    let journal_path = tangis_manifest::journal_path_for(manifest_path);
    tangis_manifest::replay(&mut manifest, &journal_path)
        .map_err(|e| format!("journal 回放失败: {e}"))?;
    let order = topo_sort(&manifest).map_err(|e| e.to_string())?;
    if order.is_empty() {
        return Err("清单中没有任何分块，无法 build".into());
    }
    fs::create_dir_all(out)
        .map_err(|e| format!("无法创建输出目录 {}: {e}", out.display()))?;

    println!(
        "任务 {}：开始 build，共 {} 个分块（断点续切：Done 跳过，Rayon 并行流式切片）",
        manifest.task_id,
        order.len()
    );

    let index_of: std::collections::BTreeMap<&ChunkId, usize> = manifest
        .chunks
        .iter()
        .enumerate()
        .map(|(i, c)| (&c.id, i))
        .collect();
    let pending: Vec<usize> = order
        .iter()
        .map(|id| index_of[id])
        .filter(|&i| manifest.chunks[i].status != ChunkStatus::Done)
        .collect();
    let skipped = order.len() - pending.len();

    // 分块 id 用作产物文件名，提前整体校验（快速失败）
    for &i in &pending {
        let id = &manifest.chunks[i].id.0;
        if id.is_empty() || id.contains(['/', '\\']) || id == "." || id == ".." {
            return Err(format!(
                "分块 id `{id}` 不是安全的文件名（不能为空、不能含路径分隔符）"
            ));
        }
    }

    // 几何源目录：--src 优先，其次 manifest.source
    let base_dir = manifest_path
        .parent()
        .map(|d| d.to_path_buf())
        .unwrap_or_else(|| PathBuf::from("."));
    let src_dir = geometry::effective_source_dir(&manifest, &base_dir, src);
    let by_id: std::collections::BTreeMap<&ChunkId, &tangis_manifest::Chunk> =
        manifest.chunks.iter().map(|c| (&c.id, c)).collect();

    // 增量 journal：有待执行分块、或残留未收尾 journal 时才创建
    let journal = if !pending.is_empty() || journal_path.exists() {
        Some(tangis_manifest::Journal::create(journal_path.clone())
            .map_err(|e| format!("无法创建 journal {}: {e}", journal_path.display()))?)
    } else {
        None
    };

    // 并行流式转换：解析 →（简化）→ b3dm → 写盘 → 释放，逐块 journal 检查点
    type TilePayload = (usize, u64, [f32; 3], [f32; 3]);
    let journal = std::sync::Mutex::new(journal);
    let results: std::sync::Mutex<Vec<Result<TilePayload, String>>> =
        std::sync::Mutex::new(Vec::new());
    pending.par_iter().for_each(|&i| {
        // B7：分块边界协作取消——未开始的分块直接跳过（journal 不记录，
        // 状态保持 pending，断点续切语义与崩溃恢复一致）
        if cancel.is_canceled() {
            results.lock().unwrap().push(Err(cancel::canceled_err(cancel)));
            return;
        }
        let res = build_one_chunk(
            i, &manifest, &by_id, &base_dir, src_dir.as_deref(), out, simplify_ratio,
        );
        // 增量检查点（与旧「逐块整份落盘」同级崩溃安全，O(1) 写入）
        let mut journal = journal.lock().unwrap();
        if let (Ok((_, _, min, max)), Some(j)) = (&res, journal.as_mut()) {
            let chunk = &manifest.chunks[i];
            let outcome = tangis_manifest::ChunkOutcome {
                id: chunk.id.clone(),
                attempts: chunk.attempts + 1,
                bounds: Some(Bounds {
                    min: [min[0] as f64, min[1] as f64, min[2] as f64],
                    max: [max[0] as f64, max[1] as f64, max[2] as f64],
                }),
            };
            if let Err(e) = j.record(&outcome) {
                drop(journal);
                results.lock().unwrap().push(Err(format!("journal 写入失败: {e}")));
                return;
            }
        }
        drop(journal);
        results.lock().unwrap().push(res);
    });

    // 串行收尾：按 pending 序应用状态（与旧 stdout/错误顺序一致）
    let mut built = 0usize;
    let mut bytes = 0u64;
    let mut payloads = results.into_inner().unwrap();
    payloads.sort_by_key(|r| match r {
        Ok((i, ..)) => *i,
        Err(_) => usize::MAX,
    });

    // B7：取消优先于失败——journal 保留（不 finalize）、manifest 本体不动，
    // 已完成分块已逐块落盘，重跑即可断点续切；main 据前缀转 exit 130。
    if let Some(cancel_err) = payloads
        .iter()
        .find_map(|r| r.as_ref().err().filter(|e| cancel::is_canceled_err(e)).cloned())
    {
        let journal = journal.into_inner().unwrap();
        drop(journal);
        return Err(cancel_err);
    }

    for r in &payloads {
        let (i, size, min, max) = r.as_ref().map_err(|e| e.clone())?;
        let chunk = &mut manifest.chunks[*i];
        chunk.bounds = Bounds {
            min: [min[0] as f64, min[1] as f64, min[2] as f64],
            max: [max[0] as f64, max[1] as f64, max[2] as f64],
        };
        chunk.status = ChunkStatus::Done;
        chunk.attempts += 1;
        built += 1;
        bytes += size;
        println!("  {}  OK（{size} 字节）", chunk.id);
    }

    // 失败路径：journal 保留（断点续切），manifest 本体不动（与旧实现
    // 「成功部分已逐块落盘」等价的恢复语义）
    let journal = journal.into_inner().unwrap();
    if built < pending.len() {
        drop(journal);
        return Err(payloads
            .into_iter()
            .find_map(|r| r.err())
            .expect("有分块未成功时必存在错误"));
    }

    // --simplify 元数据旁车（简化率 + 本次涉及分块；供下游清单核对）
    if let Some(ratio) = simplify_ratio {
        let meta = serde_json::json!({
            "simplify_ratio": ratio,
            "chunks": pending.iter()
                .map(|&i| manifest.chunks[i].id.0.clone())
                .collect::<Vec<_>>(),
        });
        let path = out.join("simplify.json");
        let text = serde_json::to_string_pretty(&meta).map_err(|e| e.to_string())?;
        fs::write(&path, text + "\n")
            .map_err(|e| format!("无法写简化元数据 {}: {e}", path.display()))?;
    }

    // 增量收尾：manifest 整份重写一次（内容与旧逐块全量写一致）+ 删 journal
    if let Some(j) = journal {
        j.finalize(&manifest, manifest_path)
            .map_err(|e| format!("manifest 收尾失败: {e}"))?;
    }

    // tileset.json 全量重建（它描述的是整个数据集，与本次新切多少块无关；
    // bounds 已来自真实几何包围盒；--origin 时 root 附带 ENU→ECEF transform）
    // 注意：--simplify 后 bounds 来自简化后几何的包围盒（QEM 最优点/中点
    // 不越出原包围盒的凸包，实际偏差可忽略）
    let tileset = tiles::tileset::build_tileset_json(&manifest, origin)?;
    let tileset_path = out.join("tileset.json");
    let tileset_json = serde_json::to_string_pretty(&tileset).map_err(|e| e.to_string())?;
    fs::write(&tileset_path, tileset_json + "\n")
        .map_err(|e| format!("无法写产物 {}: {e}", tileset_path.display()))?;

    Ok(BuildSummary { built, skipped, bytes })
}

/// 流式切一个分块：解析网格 →（简化）→ b3dm → 写盘。成功返回
/// (index, b3dm 字节数, bounds min, bounds max)。
fn build_one_chunk(
    i: usize,
    manifest: &ChunkManifest,
    by_id: &std::collections::BTreeMap<&ChunkId, &tangis_manifest::Chunk>,
    base_dir: &Path,
    src_dir: Option<&Path>,
    out: &Path,
    simplify_ratio: Option<f32>,
) -> Result<(usize, u64, [f32; 3], [f32; 3]), String> {
    let chunk = &manifest.chunks[i];
    let mut rm = geometry::resolve_chunk_mesh(&chunk.id, by_id, base_dir, src_dir)?;
    if let Some(ratio) = simplify_ratio {
        let mesh = apply_simplify(&rm.mesh, ratio)?;
        // 简化可能收掉最大 batch id 的全部顶点：feature_sources 对齐到
        // 新 feature 数（batch id 值不变，仅长度收缩）
        rm.feature_sources
            .truncate(tangis_osgb::feature_count(&mesh) as usize);
        rm.mesh = mesh;
    }
    let mesh = &rm.mesh;
    if mesh.vertices.is_empty() || mesh.indices.is_empty() {
        return Err(format!("分块 {} 解析出的几何为空", chunk.id));
    }
    // 纹理：内嵌字节（OSGB INLINE_DATA/INLINE_FILE）优先，无内嵌走文件路径；
    // 两者都不可用即失败（禁止静默产出无纹理几何）。mimeType：文件按扩展名，
    // 内嵌编码文件按字节签名，INLINE_DATA 原始像素编码为 PNG。
    let texture_info = geometry::load_texture_image(mesh)
        .map_err(|e| format!("分块 {} 纹理不可用: {e}", chunk.id))?;
    let b3dm = match &texture_info {
        None => tiles::b3dm::build_b3dm_from_mesh(mesh, &rm.feature_sources),
        Some(tex) => tiles::b3dm::build_b3dm_from_mesh_textured(
            mesh,
            &tiles::b3dm::TextureImage { bytes: &tex.bytes, mime_type: tex.mime_type },
            &rm.feature_sources,
        )
        .map_err(|e| format!("分块 {} 生成带纹理 b3dm 失败: {e}", chunk.id))?,
    };
    let path = out.join(format!("{}.b3dm", chunk.id));
    fs::write(&path, &b3dm)
        .map_err(|e| format!("无法写产物 {}: {e}", path.display()))?;
    let (min, max) = mesh.bounds().expect("已校验非空");
    Ok((i, b3dm.len() as u64, min, max))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 真实 osgconv 3.6.5 语料（osgb 目录）；缺失时返回 None 由测试跳过。
    fn real_osgb_dir() -> Option<PathBuf> {
        let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../../testdata/osgb-real/osgb");
        if dir.join("Tile_+000_+000.osgb").is_file() {
            Some(dir)
        } else {
            eprintln!("跳过：真实语料 testdata/osgb-real/osgb/ 不存在");
            None
        }
    }

    fn temp_dir(name: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("tangis-kernel-test-{}-{name}", std::process::id()));
        let _ = fs::remove_dir_all(&d);
        fs::create_dir_all(&d).unwrap();
        d
    }

    /// 写一份 4 叶分块（stem 与 testdata 文件名一致）的 manifest。
    fn write_manifest(path: &Path, source: Option<&str>) {
        let tiles = [
            "Tile_+000_+000",
            "Tile_+000_+001",
            "Tile_+001_+000",
            "Tile_+001_+001",
        ];
        let chunks: Vec<String> = tiles
            .iter()
            .map(|t| {
                format!(
                    r#"{{"id":"{t}","lod":1,"bounds":{{"min":[0,0,0],"max":[1,1,1]}},"status":"pending","attempts":0}}"#
                )
            })
            .collect();
        let src_line = source
            .map(|s| format!(r#","source":"{s}""#))
            .unwrap_or_default();
        let json = format!(
            r#"{{"task_id":"e2e","chunks":[{}]{src_line}}}"#,
            chunks.join(",")
        );
        fs::write(path, json).unwrap();
    }

    /// B7：预置取消标志 → build 安全终止（错误带取消前缀、journal 保留）；
    /// 撤掉标志重跑 → 断点续切完成。
    #[test]
    fn build_cancel_keeps_journal_and_resume_completes() {
        let Some(src_abs) = real_osgb_dir() else { return };
        let dir = temp_dir("build-cancel");
        let manifest_path = dir.join("manifest.json");
        write_manifest(&manifest_path, Some(&src_abs.to_string_lossy()));
        let out = dir.join("out");

        let flag_path = dir.join("cancel.flag");
        fs::write(&flag_path, b"").unwrap();
        let cancel = cancel::CancelFlag::from_path(Some(flag_path.clone()));
        let err = cmd_build(&manifest_path, &out, None, None, None, None, &cancel).unwrap_err();
        assert!(cancel::is_canceled_err(&err), "{err}");

        // journal 保留（未 finalize 删除）；manifest 本体未标记 Done
        let journal_path = tangis_manifest::journal_path_for(&manifest_path);
        assert!(journal_path.exists(), "取消后 journal 应保留供断点续切");
        let m: ChunkManifest =
            serde_json::from_str(&fs::read_to_string(&manifest_path).unwrap()).unwrap();
        assert!(m.chunks.iter().all(|c| c.status == ChunkStatus::Pending));

        // 撤销标志重跑：全部完成并 finalize
        fs::remove_file(&flag_path).unwrap();
        let summary = cmd_build(&manifest_path, &out, None, None, None, None, &cancel).unwrap();
        assert_eq!(summary.built + summary.skipped, 4);
        assert!(!journal_path.exists(), "完成后 journal 应被 finalize 清除");

        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn build_produces_real_geometry_and_updates_bounds() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        let dir = temp_dir("build-e2e");
        let manifest_path = dir.join("manifest.json");
        write_manifest(&manifest_path, Some(&src_abs.to_string_lossy()));
        let out = dir.join("out");

        let summary = cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        assert_eq!(summary.built, 4);
        assert_eq!(summary.skipped, 0);

        // 每个分块都有 b3dm，且字节数远大于占位三角形（真实几何：
        // 1089 顶点 ×12B POSITION + 1089×12B NORMAL + 6144×4B 索引 ≈ 50KB）
        for t in ["Tile_+000_+000", "Tile_+000_+001", "Tile_+001_+000", "Tile_+001_+001"] {
            let b = fs::read(out.join(format!("{t}.b3dm"))).unwrap();
            assert_eq!(&b[0..4], b"b3dm");
            assert!(b.len() > 45_000, "{t}: {} 字节", b.len());
        }

        // manifest bounds 被真实几何包围盒覆盖（不再是 [0,0,0]-[1,1,1] 占位盒）。
        // 真实语料 OSGB（y-up）经 load_mesh 的 (x,y,z)→(x,−z,y) 旋转到内部
        // Z-up 后 x ∈ [0,100]：任一真实包围盒的 X 跨度 = 100，远大于占位盒的 1。
        let m: ChunkManifest =
            serde_json::from_str(&fs::read_to_string(&manifest_path).unwrap()).unwrap();
        for c in &m.chunks {
            assert_eq!(c.status, ChunkStatus::Done);
            let span_x = c.bounds.max[0] - c.bounds.min[0];
            assert!((span_x - 100.0).abs() < 1.0, "X 跨度应为 100，实际 {span_x}");
        }

        // tileset.json 存在且 root bounds 来自真实几何（box 体积远大于占位盒）
        let tileset: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(out.join("tileset.json")).unwrap()).unwrap();
        let bx = tileset["root"]["boundingVolume"]["box"].as_array().unwrap();
        let half_x = bx[3].as_f64().unwrap();
        assert!(half_x > 10.0, "root X 半轴应来自真实几何，实际 {half_x}");

        // 断点续切：重复 build 全部 SKIP
        let again = cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        assert_eq!(again.built, 0);
        assert_eq!(again.skipped, 4);

        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn build_with_src_flag_overrides_manifest_source() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        let dir = temp_dir("build-src-flag");
        let manifest_path = dir.join("manifest.json");
        // manifest 故意不写 source，全靠 --src
        write_manifest(&manifest_path, None);
        let out = dir.join("out");

        let summary = cmd_build(&manifest_path, &out, None, Some(&src_abs), None, None, &crate::cancel::CancelFlag::none()).unwrap();
        assert_eq!(summary.built, 4);

        let _ = fs::remove_dir_all(&dir);
    }

    /// E2E（Y-up 缺陷回归）：build 产出的 b3dm 内嵌 glTF POSITION 必须是
    /// Y-up（水平 x/z、高度 +y），accessor min/max 呈「高度在 y」形态；
    /// tileset box（Z-up tile 局部系）的 z 范围 ≈ 源高度范围，与 Cesium
    /// Y_UP_TO_Z_UP 旋转后的几何自然对齐。
    #[test]
    fn build_emits_y_up_glb_and_box_height_matches() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        let dir = temp_dir("build-yup-e2e");
        let manifest_path = dir.join("manifest.json");
        // 单 tile：便于把 box 与该 tile 高度范围一一对照
        let tiles = ["Tile_+000_+000"];
        let chunks: Vec<String> = tiles
            .iter()
            .map(|t| {
                format!(
                    r#"{{"id":"{t}","lod":1,"bounds":{{"min":[0,0,0],"max":[1,1,1]}},"status":"pending","attempts":0}}"#
                )
            })
            .collect();
        let json = format!(
            r#"{{"task_id":"yup","chunks":[{}],"source":"{}"}}"#,
            chunks.join(","),
            src_abs.to_string_lossy()
        );
        fs::write(&manifest_path, json).unwrap();
        let out = dir.join("out");

        cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();

        // ---- 解析 b3dm → GLB JSON chunk → POSITION accessor min/max ----
        let b = fs::read(out.join("Tile_+000_+000.b3dm")).unwrap();
        let ft_len = u32::from_le_bytes(b[12..16].try_into().unwrap()) as usize;
        let bt_len = u32::from_le_bytes(b[20..24].try_into().unwrap()) as usize;
        let glb = &b[28 + ft_len + bt_len..];
        let json_len = u32::from_le_bytes(glb[12..16].try_into().unwrap()) as usize;
        let v: serde_json::Value = serde_json::from_slice(&glb[20..20 + json_len]).unwrap();
        let acc = &v["accessors"][0];
        let min: Vec<f64> = acc["min"].as_array().unwrap()
            .iter().map(|x| x.as_f64().unwrap()).collect();
        let max: Vec<f64> = acc["max"].as_array().unwrap()
            .iter().map(|x| x.as_f64().unwrap()).collect();
        // 真实语料 Tile_+000_+000：内部 Z-up = OBJ 源 z-up 坐标
        // x∈[0,100]、y∈[0,100]、高度 z∈[−2.1355,3.7574]
        // → GLB Y-up：min ≈ [0, −2.1355, −100]，max ≈ [100, 3.7574, 0]（高度在 y）
        let close = |a: f64, e: f64| (a - e).abs() < 1e-3;
        for (k, (lo, hi)) in [(0.0, 100.0), (-2.1355, 3.7574), (-100.0, 0.0)]
            .iter()
            .enumerate()
        {
            assert!(close(min[k], *lo), "min[{k}]={} 期望 {lo}", min[k]);
            assert!(close(max[k], *hi), "max[{k}]={} 期望 {hi}", max[k]);
        }
        // 高度跨度（y）远小于水平跨度（x/z）：Y-up 形态的另一重证据
        assert!(max[1] - min[1] < max[0] - min[0]);

        // ---- tileset box（Z-up tile 局部系）：z 范围 ≈ 源高度范围 ----
        let ts: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(out.join("tileset.json")).unwrap()).unwrap();
        let bx = ts["root"]["boundingVolume"]["box"].as_array().unwrap();
        let cz = bx[2].as_f64().unwrap();
        let hz = bx[11].as_f64().unwrap();
        assert!(close(cz - hz, -2.1355), "box z 下限 {} 应≈高度下限", cz - hz);
        assert!(close(cz + hz, 3.7574), "box z 上限 {} 应≈高度上限", cz + hz);

        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn build_fails_loudly_without_any_source() {
        let dir = temp_dir("build-no-src");
        let manifest_path = dir.join("manifest.json");
        write_manifest(&manifest_path, None);
        let out = dir.join("out");

        let err = cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap_err();
        assert!(err.contains("禁止产出占位几何"), "{err}");

        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn build_parent_chunk_merges_children_geometry() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        let dir = temp_dir("build-merge");
        let manifest_path = dir.join("manifest.json");
        let tiles = ["Tile_+000_+000", "Tile_+000_+001"];
        let children: Vec<String> = tiles
            .iter()
            .map(|t| {
                format!(
                    r#"{{"id":"{t}","lod":1,"bounds":{{"min":[0,0,0],"max":[1,1,1]}},"status":"pending","attempts":0}}"#
                )
            })
            .collect();
        let json = format!(
            r#"{{"task_id":"merge","chunks":[
                {{"id":"root","lod":0,"bounds":{{"min":[0,0,0],"max":[1,1,1]}},"status":"pending","attempts":0,"children":["Tile_+000_+000","Tile_+000_+001"]}},
                {}
            ],"source":"{}"}}"#,
            children.join(","),
            src_abs.to_string_lossy()
        );
        fs::write(&manifest_path, json).unwrap();
        let out = dir.join("out");

        let summary = cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        assert_eq!(summary.built, 3); // root + 2 个叶子
        // 父 b3dm = 两个子分块合并几何（2×1089 顶点）；真实语料 b3dm 以
        // 纹理 atlas（~190KB PNG，父子相同）体积为主，几何翻倍表现为
        // root 严格大于 leaf
        let root_b3dm = fs::read(out.join("root.b3dm")).unwrap();
        let leaf_b3dm = fs::read(out.join("Tile_+000_+000.b3dm")).unwrap();
        assert!(root_b3dm.len() > leaf_b3dm.len(), "root {} 应 > leaf {}", root_b3dm.len(), leaf_b3dm.len());

        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn run_parallel_executes_and_persists() {
        let dir = temp_dir("run-e2e");
        let manifest_path = dir.join("manifest.json");
        write_manifest(&manifest_path, None);

        let summary = cmd_run(&manifest_path, 1, &crate::cancel::CancelFlag::none()).unwrap();
        assert_eq!(summary.executed, 4);
        assert_eq!(summary.skipped, 0);

        // 全部 Done 且已回写
        let m: ChunkManifest =
            serde_json::from_str(&fs::read_to_string(&manifest_path).unwrap()).unwrap();
        assert!(m.chunks.iter().all(|c| c.status == ChunkStatus::Done));

        // 断点续切：再跑一次全部跳过
        let again = cmd_run(&manifest_path, 1, &crate::cancel::CancelFlag::none()).unwrap();
        assert_eq!(again.executed, 0);
        assert_eq!(again.skipped, 4);

        let _ = fs::remove_dir_all(&dir);
    }

    /// E2E：--origin 时 tileset.json 结构验证（D1 transform + D2 geometricError）。
    #[test]
    fn build_with_origin_writes_transform_and_positive_errors() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        let dir = temp_dir("build-origin-e2e");
        let manifest_path = dir.join("manifest.json");
        write_manifest(&manifest_path, Some(&src_abs.to_string_lossy()));
        let out = dir.join("out");

        // 北京附近原点（黄金值来自 pyproj，与 tileset 单测同一组）
        let summary =
            cmd_build(&manifest_path, &out, None, None, Some([116.3906, 39.9072, 50.0]), None, &crate::cancel::CancelFlag::none())
                .unwrap();
        assert_eq!(summary.built, 4);

        let ts: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(out.join("tileset.json")).unwrap())
                .unwrap();

        // D1：root.transform 为 16 元素列主序矩阵，平移列 = 原点 ECEF（误差 < 1 m）
        let t = ts["root"]["transform"].as_array().expect("root.transform 应存在");
        assert_eq!(t.len(), 16);
        assert!((t[12].as_f64().unwrap() + 2_177_709.058641).abs() < 1.0, "tx={}", t[12]);
        assert!((t[13].as_f64().unwrap() - 4_388_774.212359).abs() < 1.0, "ty={}", t[13]);
        assert!((t[14].as_f64().unwrap() - 4_070_119.020036).abs() < 1.0, "tz={}", t[14]);
        // 子 tile 不重复写 transform（按规范继承 root）
        for c in ts["root"]["children"].as_array().unwrap() {
            assert!(c.get("transform").is_none());
        }

        // D2：扁平 4 叶清单（旧实现 ge 全 0）——tileset/root ge > 0，叶子 ge = 0
        let ts_ge = ts["geometricError"].as_f64().unwrap();
        assert!(ts_ge > 0.0, "tileset geometricError 必须 > 0，实际 {ts_ge}");
        assert!(ts["root"]["geometricError"].as_f64().unwrap() > 0.0);
        for c in ts["root"]["children"].as_array().unwrap() {
            assert_eq!(c["geometricError"].as_f64().unwrap(), 0.0, "叶子 ge 应为 0");
        }

        // 无 --origin 时保持历史行为：无 transform（对照）
        let dir2 = temp_dir("build-no-origin-e2e");
        let manifest2 = dir2.join("manifest.json");
        write_manifest(&manifest2, Some(&src_abs.to_string_lossy()));
        let out2 = dir2.join("out");
        cmd_build(&manifest2, &out2, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        let ts2: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(out2.join("tileset.json")).unwrap())
                .unwrap();
        assert!(ts2["root"].get("transform").is_none(), "无 --origin 不得写 transform");
        // D2 修复与 origin 无关：ge 仍应 > 0
        assert!(ts2["geometricError"].as_f64().unwrap() > 0.0);

        let _ = fs::remove_dir_all(&dir);
        let _ = fs::remove_dir_all(&dir2);
    }

    #[test]
    fn build_rejects_out_of_range_origin() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        let dir = temp_dir("build-origin-range");
        let manifest_path = dir.join("manifest.json");
        write_manifest(&manifest_path, Some(&src_abs.to_string_lossy()));
        let out = dir.join("out");
        let err = cmd_build(&manifest_path, &out, None, None, Some([200.0, 0.0, 0.0]), None, &crate::cancel::CancelFlag::none())
            .unwrap_err();
        assert!(err.contains("越界"), "{err}");
        let _ = fs::remove_dir_all(&dir);
    }

    /// --simplify：b3dm 几何收缩（POSITION 顶点数下降、TEXCOORD 随动）、
    /// 简化率写入 <out>/simplify.json 元数据、tileset bounds 来自简化后几何。
    #[test]
    fn build_with_simplify_reduces_geometry_and_writes_meta() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        // 基线：不简化的单 tile b3dm
        let dir0 = temp_dir("simplify-base");
        let manifest0 = dir0.join("manifest.json");
        let tiles = ["Tile_+000_+000"];
        let chunks: Vec<String> = tiles
            .iter()
            .map(|t| {
                format!(
                    r#"{{"id":"{t}","lod":1,"bounds":{{"min":[0,0,0],"max":[1,1,1]}},"status":"pending","attempts":0}}"#
                )
            })
            .collect();
        let json = format!(
            r#"{{"task_id":"simp","chunks":[{}],"source":"{}"}}"#,
            chunks.join(","),
            src_abs.to_string_lossy()
        );
        fs::write(&manifest0, &json).unwrap();
        let out0 = dir0.join("out");
        cmd_build(&manifest0, &out0, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        let base_b = fs::read(out0.join("Tile_+000_+000.b3dm")).unwrap();

        // --simplify 0.5
        let dir = temp_dir("simplify-on");
        let manifest_path = dir.join("manifest.json");
        fs::write(&manifest_path, &json).unwrap();
        let out = dir.join("out");
        let summary =
            cmd_build(&manifest_path, &out, None, None, None, Some(0.5), &crate::cancel::CancelFlag::none()).unwrap();
        assert_eq!(summary.built, 1);

        let simp_b = fs::read(out.join("Tile_+000_+000.b3dm")).unwrap();
        let count_of = |b: &[u8]| -> usize {
            let ft_len = u32::from_le_bytes(b[12..16].try_into().unwrap()) as usize;
            let bt_len = u32::from_le_bytes(b[20..24].try_into().unwrap()) as usize;
            let glb = &b[28 + ft_len + bt_len..];
            let json_len = u32::from_le_bytes(glb[12..16].try_into().unwrap()) as usize;
            let v: serde_json::Value =
                serde_json::from_slice(&glb[20..20 + json_len]).unwrap();
            v["accessors"][0]["count"].as_u64().unwrap() as usize
        };
        let (base_v, simp_v) = (count_of(&base_b), count_of(&simp_b));
        assert_eq!(base_v, 1089, "基线顶点数");
        assert!(
            simp_v <= 1089 && simp_v > 500,
            "ratio=0.5 简化后顶点数应显著下降但不归零，实际 {simp_v}"
        );
        assert!(simp_b.len() < base_b.len(), "简化后 b3dm 应更小");

        // simplify.json 元数据
        let meta: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(out.join("simplify.json")).unwrap())
                .unwrap();
        assert_eq!(meta["simplify_ratio"].as_f64().unwrap(), 0.5);
        assert_eq!(meta["chunks"][0].as_str().unwrap(), "Tile_+000_+000");

        // 无 --simplify 时不写该文件
        assert!(!out0.join("simplify.json").exists());

        let _ = fs::remove_dir_all(&dir);
        let _ = fs::remove_dir_all(&dir0);
    }

    // ---- M2-PERF：流式切片 + manifest 增量 journal ----

    /// 产物确定性（改造前后字节一致的自动化证据）：同一语料两次独立
    /// build，全部 b3dm 与 tileset.json 必须逐字节一致。
    #[test]
    fn build_outputs_are_byte_identical_across_runs() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        let mk = |name: &str| {
            let dir = temp_dir(name);
            let manifest_path = dir.join("manifest.json");
            write_manifest(&manifest_path, Some(&src_abs.to_string_lossy()));
            let out = dir.join("out");
            cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
            (dir, manifest_path, out)
        };
        let (d1, m1, o1) = mk("determinism-a");
        let (d2, m2, o2) = mk("determinism-b");

        // 全部产物（含 tileset.json）逐字节 diff 为空
        let entries = fs::read_dir(&o1).unwrap().count();
        assert_eq!(entries, fs::read_dir(&o2).unwrap().count());
        for e in fs::read_dir(&o1).unwrap().filter_map(|e| e.ok()) {
            let a = e.path();
            let b = o2.join(a.file_name().unwrap());
            assert_eq!(fs::read(&a).unwrap(), fs::read(&b).unwrap(), "{a:?}");
        }
        // manifest 终态（含回写 bounds/status）也逐字节一致
        assert_eq!(fs::read(&m1).unwrap(), fs::read(&m2).unwrap());

        let _ = fs::remove_dir_all(&d1);
        let _ = fs::remove_dir_all(&d2);
    }

    /// journal 生命周期：build 过程中存在增量检查点文件，正常收尾后
    /// 删除；manifest 终态为整份一次写入（非逐块重写）。
    #[test]
    fn build_journal_created_and_finalized() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        let dir = temp_dir("journal-finalize");
        let manifest_path = dir.join("manifest.json");
        write_manifest(&manifest_path, Some(&src_abs.to_string_lossy()));
        let out = dir.join("out");

        let summary = cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        assert_eq!(summary.built, 4);

        // 收尾后 journal 不存在，manifest 全部 Done
        assert!(!tangis_manifest::journal_path_for(&manifest_path).exists());
        let m: ChunkManifest =
            serde_json::from_str(&fs::read_to_string(&manifest_path).unwrap()).unwrap();
        assert!(m.chunks.iter().all(|c| c.status == ChunkStatus::Done));
        assert!(m.chunks.iter().all(|c| c.attempts == 1));

        let _ = fs::remove_dir_all(&dir);
    }

    /// 断点续切（journal 回放）：模拟崩溃残留——manifest 仍全 pending、
    /// journal 已记录 2 块完成。重跑必须跳过这 2 块，只新切 2 块，
    /// 收尾后 manifest 终态与一次性切完逐字节一致。
    #[test]
    fn build_resumes_from_partial_journal() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        let tiles = [
            "Tile_+000_+000",
            "Tile_+000_+001",
            "Tile_+001_+000",
            "Tile_+001_+001",
        ];

        let mk = |name: &str| {
            let dir = temp_dir(name);
            let manifest_path = dir.join("manifest.json");
            write_manifest(&manifest_path, Some(&src_abs.to_string_lossy()));
            (dir, manifest_path)
        };
        // 参照：一次性切完的终态
        let (ref_dir, ref_manifest) = mk("resume-ref");
        let ref_out = ref_dir.join("out");
        cmd_build(&ref_manifest, &ref_out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        let expected = fs::read_to_string(&ref_manifest).unwrap();

        // 模拟崩溃：全 pending manifest + journal 记录 2 块（bounds 取参照终态）
        let (dir, manifest_path) = mk("resume-crash");
        let refm: ChunkManifest =
            serde_json::from_str(&expected).unwrap();
        let mut j = tangis_manifest::Journal::create(
            tangis_manifest::journal_path_for(&manifest_path),
        )
        .unwrap();
        for t in &tiles[..2] {
            let c = refm.chunk(&tangis_manifest::ChunkId::new(*t)).unwrap();
            j.record(&tangis_manifest::ChunkOutcome {
                id: c.id.clone(),
                attempts: c.attempts,
                bounds: Some(c.bounds.clone()),
            })
            .unwrap();
        }
        drop(j);

        let out = dir.join("out");
        let summary = cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        assert_eq!(summary.built, 2, "只新切未完成的 2 块");
        assert_eq!(summary.skipped, 2, "journal 已完成的 2 块跳过");

        // 收尾后与一次性切完的 manifest 终态逐字节一致（幂等语义）
        assert!(!tangis_manifest::journal_path_for(&manifest_path).exists());
        assert_eq!(fs::read_to_string(&manifest_path).unwrap(), expected);

        let _ = fs::remove_dir_all(&dir);
        let _ = fs::remove_dir_all(&ref_dir);
    }

    /// 失败→修复→续切全流程：在**私有临时源目录**（复制语料）中污染一个
    /// 源文件——失败时 manifest 本体不动（journal 保留）；修复后续切产出
    /// 与一次性切完的 manifest 终态逐字节一致。
    /// （不动共享语料目录：测试并行执行，污染会与其他测试竞争。）
    #[test]
    fn build_failure_then_resume_produces_reference_state() {
        let Some(src_abs) = real_osgb_dir() else {
            return;
        };
        // 私有源目录：复制 4 个语料文件（测试源清单与 write_manifest 一致）
        let src_dir = temp_dir("fail-resume-src");
        for t in [
            "Tile_+000_+000",
            "Tile_+000_+001",
            "Tile_+001_+000",
            "Tile_+001_+001",
        ] {
            fs::copy(src_abs.join(format!("{t}.osgb")), src_dir.join(format!("{t}.osgb")))
                .unwrap();
        }
        let bad = src_dir.join("Tile_+001_+001.osgb");
        let src_str = src_dir.to_string_lossy().into_owned();

        // 参照终态
        let ref_dir = temp_dir("fail-resume-ref");
        let ref_manifest = ref_dir.join("manifest.json");
        write_manifest(&ref_manifest, Some(&src_str));
        cmd_build(&ref_manifest, &ref_dir.join("out"), None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        let expected = fs::read_to_string(&ref_manifest).unwrap();

        // 污染 → 失败（journal 保留）
        let dir = temp_dir("fail-resume");
        let manifest_path = dir.join("manifest.json");
        write_manifest(&manifest_path, Some(&src_str));
        fs::write(&bad, b"not an osgb").unwrap();
        let out = dir.join("out");
        assert!(cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).is_err());
        assert!(tangis_manifest::journal_path_for(&manifest_path).exists());

        // 还源 → 续切：journal 已完成的块跳过，终态与参照逐字节一致
        fs::copy(src_abs.join("Tile_+001_+001.osgb"), &bad).unwrap();
        let summary = cmd_build(&manifest_path, &out, None, None, None, None, &crate::cancel::CancelFlag::none()).unwrap();
        assert!(summary.skipped >= 1, "失败前完成的块应被跳过");
        assert!(!tangis_manifest::journal_path_for(&manifest_path).exists());
        assert_eq!(fs::read_to_string(&manifest_path).unwrap(), expected);

        let _ = fs::remove_dir_all(&dir);
        let _ = fs::remove_dir_all(&ref_dir);
        let _ = fs::remove_dir_all(&src_dir);
    }

        // ---- M2-F09b：edit --format b3dm ----

        /// 解析 b3dm：返回 (FeatureTable JSON, BatchTable JSON, GLB glTF JSON, BIN 字节)。
        fn parse_b3dm(b: &[u8]) -> (serde_json::Value, serde_json::Value, serde_json::Value, Vec<u8>) {
            assert_eq!(&b[0..4], b"b3dm");
            assert_eq!(u32::from_le_bytes(b[8..12].try_into().unwrap()), b.len() as u32);
            let ft_len = u32::from_le_bytes(b[12..16].try_into().unwrap()) as usize;
            let bt_len = u32::from_le_bytes(b[20..24].try_into().unwrap()) as usize;
            let ft: serde_json::Value = serde_json::from_slice(&b[28..28 + ft_len]).unwrap();
            let bt: serde_json::Value =
                serde_json::from_slice(&b[28 + ft_len..28 + ft_len + bt_len]).unwrap();
            let glb = &b[28 + ft_len + bt_len..];
            assert_eq!(&glb[0..4], b"glTF");
            let json_len = u32::from_le_bytes(glb[12..16].try_into().unwrap()) as usize;
            let v: serde_json::Value = serde_json::from_slice(&glb[20..20 + json_len]).unwrap();
            let bin_len = u32::from_le_bytes(glb[20 + json_len..24 + json_len].try_into().unwrap())
                as usize;
            let bin = glb[28 + json_len..28 + json_len + bin_len].to_vec();
            (ft, bt, v, bin)
        }

        /// 按 primitive attributes 精确定位并解码 f32 属性（VEC3/VEC2）。
        fn decode_f32_attr(
            v: &serde_json::Value,
            bin: &[u8],
            attr: &str,
        ) -> Vec<Vec<f64>> {
            let acc_idx = v["meshes"][0]["primitives"][0]["attributes"][attr]
                .as_u64()
                .unwrap_or_else(|| panic!("{attr} 属性应存在")) as usize;
            let acc = &v["accessors"][acc_idx];
            let ncomp = match acc["type"].as_str().unwrap() {
                "VEC3" => 3,
                "VEC2" => 2,
                other => panic!("不支持的属性维度 {other}"),
            };
            let bv = &v["bufferViews"][acc["bufferView"].as_u64().unwrap() as usize];
            let off = bv["byteOffset"].as_u64().unwrap() as usize;
            let count = acc["count"].as_u64().unwrap() as usize;
            (0..count)
                .map(|i| {
                    (0..ncomp)
                        .map(|k| {
                            f32::from_le_bytes(bin[off + (i * ncomp + k) * 4..off + (i * ncomp + k) * 4 + 4]
                                .try_into()
                                .unwrap()) as f64
                        })
                        .collect()
                })
                .collect()
        }

        /// 真实语料 clip --format b3dm 端到端：`{out}/{tile}.b3dm` +
        /// `{out}/tileset.json` + `{out}/ops.json`；GLB 含纹理（images=1、PNG
        /// 字节签名）、TEXCOORD_0、_BATCHID；tileset root content 指向产物。
        #[test]
        fn edit_clip_b3dm_real_corpus_e2e() {
            let Some(src_abs) = real_osgb_dir() else { return };
            let out = temp_dir("edit-b3dm-clip");
            let run = || run_edit(EditOp::Clip {
                output: EditOutputArgs {
                    source: src_abs.join("Tile_+000_+000.osgb"),
                    out: out.clone(),
                    ops: None,
                    report: None,
                    format: "b3dm".into(),
                    tile: Some("Tile_+000_+000".into()),
                },
                plane: Some("1,0,0,90".into()),
                keep_side: "neg".into(),
                bbox: None,
            });
            run().unwrap();

            // 产物三件套
            let b = fs::read(out.join("Tile_+000_+000.b3dm")).unwrap();
            assert!(out.join("ops.json").is_file(), "b3dm 格式默认留痕 = {}/ops.json", out.display());
            let ts: serde_json::Value =
                serde_json::from_str(&fs::read_to_string(out.join("tileset.json")).unwrap()).unwrap();

            // b3dm → GLB：纹理链路 + 属性齐全
            let (ft, bt, v, bin) = parse_b3dm(&b);
            assert_eq!(ft["BATCH_LENGTH"], 1);
            assert_eq!(bt["_tile"][0], "Tile_+000_+000.osgb");
            let prim = &v["meshes"][0]["primitives"][0];
            assert!(prim["attributes"]["TEXCOORD_0"].is_u64(), "TEXCOORD_0 应存在");
            assert!(prim["attributes"]["_BATCHID"].is_u64(), "_BATCHID 应保留");
            assert_eq!(v["images"].as_array().unwrap().len(), 1, "images=1");
            assert_eq!(v["images"][0]["mimeType"], "image/png");
            let bv = &v["bufferViews"][v["images"][0]["bufferView"].as_u64().unwrap() as usize];
            let (off, _len) = (
                bv["byteOffset"].as_u64().unwrap() as usize,
                bv["byteLength"].as_u64().unwrap() as usize,
            );
            assert_eq!(
                &bin[off..off + 8],
                &[0x89, b'P', b'N', b'G', 0x0d, 0x0a, 0x1a, 0x0a],
                "嵌入图像应为 PNG 字节（INLINE_DATA 原始像素已编码）"
            );
            assert_eq!(prim["material"].as_u64(), Some(0), "带纹理应有材质");

            // 顶点数与留痕一致（切割后 1022，见 runner 侧测试）
            let pos = decode_f32_attr(&v, &bin, "POSITION");
            assert_eq!(pos.len(), 1022);
            // 幸存顶点全部在保留侧（GLB y-up：水平在 x/z，内部 x → GLB x）
            for p in &pos {
                assert!(p[0] <= 90.0 + 1e-3, "GLB x={} 越过保留侧", p[0]);
            }

            // tileset：结构同 build 单 tile，geometricError 按包围盒且 > 0
            assert_eq!(ts["asset"]["version"], "1.0");
            assert!(ts["geometricError"].as_f64().unwrap() > 0.0);
            assert_eq!(ts["root"]["content"]["uri"], "Tile_+000_+000.b3dm");
            assert!(ts["root"]["boundingVolume"]["box"].as_array().unwrap().len() == 12);

            // 留痕 JSON：schema 不变，output 指向 b3dm
            let ops: serde_json::Value =
                serde_json::from_str(&fs::read_to_string(out.join("ops.json")).unwrap()).unwrap();
            assert_eq!(ops["schema"], "tangis.editops/1");
            assert_eq!(ops["op"], "clip");
            assert!(ops["tiles"][0]["output"].as_str().unwrap().ends_with(".b3dm"));

            // 幂等：重复运行（覆盖写）不报错
            run().unwrap();
            let _ = fs::remove_dir_all(&out);
        }

        /// 真实语料 flatten --format b3dm：GLB NORMAL 为重算后的面积加权法线——
        /// 压平区域内部（45..55，网格间距 ≈3.1 m，与边界/过渡带留足间距）顶点
        /// 法线 z-up z 分量一致且 ≈ 1（常值平面 → +z）。
        #[test]
        fn edit_flatten_b3dm_normals_recomputed_region_consistent() {
            let Some(src_abs) = real_osgb_dir() else { return };
            let out = temp_dir("edit-b3dm-flat");
            run_edit(EditOp::Flatten {
                output: EditOutputArgs {
                    source: src_abs.join("Tile_+000_+000.osgb"),
                    out: out.clone(),
                    ops: None,
                    report: None,
                    format: "b3dm".into(),
                    tile: None,
                },
                bbox: "40,40,60,60".into(),
                elevation: 0.0,
                feather: 5.0,
            })
            .unwrap();

            let b = fs::read(out.join("Tile_+000_+000.b3dm")).unwrap();
            let (_ft, _bt, v, bin) = parse_b3dm(&b);
            let pos = decode_f32_attr(&v, &bin, "POSITION");
            let nor = decode_f32_attr(&v, &bin, "NORMAL");
            assert_eq!(pos.len(), nor.len(), "NORMAL 与 POSITION 一一对应");
            // GLB y-up → 内部 Z-up：pos = (x, −z, y)，normal = (nx, −nz, ny)
            //（z-up 法线 z 分量 = GLB 的 y 分量，[0,0,1] → [0,1,0]）
            let mut region_z: Option<f64> = None;
            let mut region_n = 0usize;
            for (p, n) in pos.iter().zip(&nor) {
                let (zx, zy) = (p[0], -p[2]); // 内部 x/y（水平）
                let nz = n[1]; // 内部法线 z 分量
                if (45.0..=55.0).contains(&zx) && (45.0..=55.0).contains(&zy) {
                    match region_z {
                        None => region_z = Some(nz),
                        Some(prev) => {
                            assert!((nz - prev).abs() < 1e-9, "区域内法线 z 不一致: {prev} vs {nz}")
                        }
                    }
                    region_n += 1;
                }
            }
            assert!(region_n > 0, "压平区域内应有顶点");
            let z = region_z.unwrap();
            assert!((z - 1.0).abs() < 1e-6, "区域内部法线应为 +z（重算生效），实际 {z}");
            let _ = fs::remove_dir_all(&out);
        }

        /// edit 参数契约：--format 非法（clap 拒绝）、源缺失、--tile 无匹配
        /// 均明确报错。
        #[test]
        fn edit_contract_errors_are_explicit() {
            let Some(src_abs) = real_osgb_dir() else { return };
            let out = temp_dir("edit-err");
            // 源缺失
            let err = run_edit(EditOp::Clip {
                output: EditOutputArgs {
                    source: out.join("no-such.osgb"),
                    out: out.clone(),
                    ops: None,
                    report: None,
                    format: "b3dm".into(),
                    tile: None,
                },
                plane: Some("1,0,0,1".into()),
                keep_side: "neg".into(),
                bbox: None,
            })
            .unwrap_err();
            assert!(err.contains("编辑源"), "{err}");
            // --tile 无匹配
            let err = run_edit(EditOp::Clip {
                output: EditOutputArgs {
                    source: src_abs.clone(),
                    out: out.clone(),
                    ops: None,
                    report: None,
                    format: "obj".into(),
                    tile: Some("NoSuch".into()),
                },
                plane: Some("1,0,0,1".into()),
                keep_side: "neg".into(),
                bbox: None,
            })
            .unwrap_err();
            assert!(err.contains("--tile NoSuch"), "{err}");
            let _ = fs::remove_dir_all(&out);
        }
    }
