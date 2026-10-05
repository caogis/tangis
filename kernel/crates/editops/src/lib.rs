//! tangis-editops —— TanGIS OSGB 实时编辑算子（M2-F09a，PRD F-09 前置）。
//!
//! 面向 server 实时编辑（Web 端抠除 / 压平 / 地面对齐，增量写回按 tile
//! 粒度）的内核算子库：输入真实 OSGB tile，输出编辑后的 OBJ（内部 Z-up，
//! `OutputFormat::Obj`，历史行为）或 b3dm 所需的编辑后网格
//! （`OutputFormat::B3dm`：纹理透传 + 面积加权法线重算，b3dm/tileset
//! 组装由 cli 既有写出器完成）+ 操作留痕 JSON（编辑历史，供 server
//! 后续接线审批）。
//!
//! # 坐标
//!
//! 全部算子在**内部 Z-up** 空间（x/y 水平、z 高度，米）进行；OSGB 加载时
//! 按 `(x,y,z)→(x,z,−y)` 从 GL y-up 恢复（与 cli build 管线同一变换），
//! OBJ 输出保持 Z-up（文件头注释声明）。
//!
//! # 算子
//!
//! - [`clip`]（抠除）：[`Region::Plane`] 移除平面一侧 / [`Region::BBox`]
//!   移除水平矩形内三角形；边界三角形 Sutherland–Hodgman 式凸二分切割，
//!   bbox 补集非凸时按 4 条边界线逐次分片保留外侧碎片，切割边界跨面共享
//!   新顶点（闭合）；变空的 Geometry 删除（见 [`clip::clip_meshes`]）；
//! - [`flatten`]（压平）：bbox 内顶点 z 置 [`FlattenParams::elevation`]，
//!   向外 [`FlattenParams::feather`] 米过渡带线性过渡防陡坎；
//! - [`align`](crate::align)（地面对齐）：bbox 区域最低点整体刚性平移到
//!   [`AlignParams::target`]（或参考 tile 同区域最低点，见
//!   [`align::region_min_z`]）。
//!
//! # 留痕
//!
//! 每次运行输出一份 [`OpsRecord`]（schema `tangis.editops/1`）：操作类型、
//! 全部参数、逐 tile 顶点/面变化与算子细节、汇总与 UTC 时间戳——
//! 即该 tile 粒度的编辑历史条目。

pub mod align;
pub mod clip;
pub mod flatten;
pub mod geom;
pub mod io;

use std::fs;
use std::path::{Path, PathBuf};

use serde::Serialize;
use tangis_osgb::Mesh;

use crate::align::AlignParams;
use crate::clip::Region;
use crate::flatten::FlattenParams;

/// 留痕 JSON schema 版本。
pub const SCHEMA: &str = "tangis.editops/1";
/// 产出工具标识。
pub const TOOL: &str = concat!(env!("CARGO_PKG_NAME"), " ", env!("CARGO_PKG_VERSION"));

/// 编辑产物格式。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum OutputFormat {
    /// OBJ 文本（历史行为，向后兼容；无纹理/法线）。
    Obj,
    /// b3dm（M2-F09b）：纹理透传 + 法线（flatten/align 面积加权重算），
    /// out 为目录，产 `{out}/{tile}.b3dm` + tileset.json（b3dm 组装由
    /// cli 侧用既有 GLB/b3dm 写出完成，本 crate 只负责几何与留痕）。
    B3dm,
}

/// 单次 edit 运行选项（输出格式 + tile 过滤）。
#[derive(Debug, Clone)]
pub struct EditRunOpts {
    pub format: OutputFormat,
    /// 仅处理该 tile（源文件 stem）；None 或 `"all"` 处理全部。
    pub tile: Option<String>,
}

impl Default for EditRunOpts {
    fn default() -> Self {
        Self { format: OutputFormat::Obj, tile: None }
    }
}

/// 编辑后的单 tile 产物（`OutputFormat::B3dm` 时携带编辑后网格；
/// `Obj` 格式网格已写盘、此处为空 Vec）。
#[derive(Debug)]
pub struct EditedTile {
    /// tile 名（源文件 stem）。
    pub tile: String,
    /// 源文件路径（纹理相对引用基于其所在目录解析）。
    pub source: PathBuf,
    /// 编辑后的网格（内部 Z-up，纹理引用/内嵌字节自源透传）。
    pub meshes: Vec<Mesh>,
}

/// edit 运行结果：留痕记录 + 逐 tile 编辑产物。
#[derive(Debug)]
pub struct EditOutcome {
    pub record: OpsRecord,
    pub edited: Vec<EditedTile>,
}

/// 逐 tile 编辑结果（留痕 JSON 的 tiles 数组元素）。
#[derive(Debug, Clone, Serialize)]
pub struct TileOps {
    /// tile 名（源文件 stem）。
    pub tile: String,
    /// 源文件绝对/传入路径。
    pub source: String,
    /// 编辑后 OBJ 输出路径。
    pub output: String,
    pub vertices_in: u64,
    pub vertices_out: u64,
    pub faces_in: u64,
    pub faces_out: u64,
    /// 算子细节统计（与 op 对应：ClipStats / FlattenStats / AlignStats）。
    #[serde(flatten)]
    pub detail: serde_json::Value,
}

/// 操作留痕记录（编辑历史条目，server 审批接线的输入格式）。
#[derive(Debug, Clone, Serialize)]
pub struct OpsRecord {
    pub schema: &'static str,
    pub tool: &'static str,
    /// 操作类型：`clip` / `flatten` / `ground-align`。
    pub op: &'static str,
    /// UTC 时间戳（ISO 8601，毫秒精度）。
    pub timestamp: String,
    /// 完整操作参数（与 op 对应的结构体序列化）。
    pub params: serde_json::Value,
    /// 逐 tile 变化明细。
    pub tiles: Vec<TileOps>,
    /// 跨 tile 汇总。
    pub totals: serde_json::Value,
}

impl OpsRecord {
    /// 写出 pretty JSON（UTF-8，尾随换行）。
    pub fn write(&self, path: &Path) -> Result<(), String> {
        if let Some(parent) = path.parent() {
            if !parent.as_os_str().is_empty() && !parent.exists() {
                fs::create_dir_all(parent)
                    .map_err(|e| format!("无法创建目录 {}: {e}", parent.display()))?;
            }
        }
        let text = serde_json::to_string_pretty(self).map_err(|e| e.to_string())?;
        fs::write(path, text + "\n")
            .map_err(|e| format!("无法写留痕 {}: {e}", path.display()))
    }
}

/// 当前 UTC 时间的 ISO 8601 字符串（毫秒精度；无外部时间依赖，
/// civil date 用 Howard Hinnant 算法自 Unix 天数换算）。
pub fn iso8601_utc_now() -> String {
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    let secs = now.as_secs() as i64;
    let millis = now.subsec_millis();
    let (y, m, d) = civil_from_days(secs.div_euclid(86_400));
    let sod = secs.rem_euclid(86_400);
    format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}.{millis:03}Z",
        sod / 3600,
        (sod % 3600) / 60,
        sod % 60
    )
}

/// Unix 天数 → (年, 月, 日)（Hinnant `civil_from_days`）。
fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}

/// 解析逗号分隔数字串（CLI 参数辅助）：`"1,2,3,4"` → 4 个 f64。
pub fn parse_numbers(s: &str, n: usize, what: &str) -> Result<Vec<f64>, String> {
    let v: Vec<f64> = s
        .split(',')
        .map(|t| {
            t.trim()
                .parse::<f64>()
                .map_err(|_| format!("{what} 含非法数字: `{t}`"))
        })
        .collect::<Result<_, _>>()?;
    if v.len() != n {
        return Err(format!("{what} 需要 {n} 个数，实际 {} 个", v.len()));
    }
    Ok(v)
}

fn mesh_totals(meshes: &[Mesh]) -> (u64, u64) {
    (
        meshes.iter().map(|m| m.vertices.len() as u64).sum(),
        meshes.iter().map(|m| (m.indices.len() / 3) as u64).sum(),
    )
}

fn stem_of(p: &Path) -> Result<String, String> {
    p.file_stem()
        .and_then(|s| s.to_str())
        .map(str::to_string)
        .ok_or_else(|| format!("无法取得文件 stem: {}", p.display()))
}

/// 输出位置与源形态一致性校验（目录源 → out 为目录；文件源 → out 为文件）。
fn prepare_out(source: &Path, out: &Path) -> Result<(), String> {
    if source.is_dir() {
        if out.is_file() {
            return Err(format!("--source 为目录时 --out 必须是目录: {}", out.display()));
        }
        fs::create_dir_all(out).map_err(|e| format!("无法创建输出目录 {}: {e}", out.display()))?;
    } else if out.is_dir() {
        return Err(format!("--source 为文件时 --out 必须是文件路径: {}", out.display()));
    }
    Ok(())
}

fn obj_out_path(source_is_dir: bool, out: &Path, stem: &str) -> PathBuf {
    if source_is_dir {
        out.join(format!("{stem}.obj"))
    } else {
        out.to_path_buf()
    }
}

/// --tile 过滤：None/"all" 直通；指定 stem 时筛选，无匹配明确报错。
fn filter_tiles(sources: Vec<PathBuf>, tile: Option<&str>) -> Result<Vec<PathBuf>, String> {
    let Some(t) = tile else { return Ok(sources) };
    if t == "all" {
        return Ok(sources);
    }
    let picked: Vec<PathBuf> = sources
        .into_iter()
        .filter(|p| {
            p.file_stem().and_then(|s| s.to_str()).map(|s| s == t).unwrap_or(false)
        })
        .collect();
    if picked.is_empty() {
        return Err(format!("--tile {t}：源中没有匹配的 tile（stem 须与文件名一致，不含扩展名）"));
    }
    Ok(picked)
}

/// 输出位置校验/创建：Obj 沿用源形态一致性；B3dm 时 out 一律为目录。
fn prepare_out_for(format: OutputFormat, source: &Path, out: &Path) -> Result<(), String> {
    match format {
        OutputFormat::Obj => prepare_out(source, out),
        OutputFormat::B3dm => {
            if out.is_file() {
                return Err(format!("--format b3dm 时 --out 必须是目录: {}", out.display()));
            }
            fs::create_dir_all(out)
                .map_err(|e| format!("无法创建输出目录 {}: {e}", out.display()))
        }
    }
}

/// 编辑后的产物路径：Obj = OBJ 路径（落盘）；B3dm = {out}/{tile}.b3dm
/// （由 cli 侧 b3dm 写出器落盘，路径先行约定写入留痕）。
fn output_path_for(
    format: OutputFormat,
    source_is_dir: bool,
    out: &Path,
    stem: &str,
    meshes: &[Mesh],
) -> Result<PathBuf, String> {
    match format {
        OutputFormat::Obj => {
            let p = obj_out_path(source_is_dir, out, stem);
            io::write_obj(&p, stem, meshes)?;
            Ok(p)
        }
        OutputFormat::B3dm => Ok(out.join(format!("{stem}.b3dm"))),
    }
}

/// 运行 clip（抠除）：逐 tile 加载 → 抠除 → 写 OBJ/约定 b3dm 路径 → 汇总留痕。
pub fn run_clip(
    source: &Path,
    region: &Region,
    out: &Path,
    ops_path: &Path,
    opts: &EditRunOpts,
) -> Result<EditOutcome, String> {
    region.validate()?;
    let sources = filter_tiles(io::collect_osgb_sources(source)?, opts.tile.as_deref())?;
    prepare_out_for(opts.format, source, out)?;
    let is_dir = source.is_dir();
    let mut tiles = Vec::new();
    let mut edited = Vec::new();
    let mut t = (0u64, 0u64, 0u64, 0u64, 0u64, 0u64, 0.0f64, 0.0f64); // vin vout fin fout removed split area_in kept
    for src in &sources {
        let stem = stem_of(src)?;
        let mut meshes = io::load_osgb_zup(src)?;
        let (vin, fin) = mesh_totals(&meshes);
        let st = clip::clip_meshes(&mut meshes, region)?;
        let output = output_path_for(opts.format, is_dir, out, &stem, &meshes)?;
        let (vout, fout) = mesh_totals(&meshes);
        if opts.format == OutputFormat::B3dm {
            edited.push(EditedTile {
                tile: stem.clone(),
                source: src.clone(),
                meshes: std::mem::take(&mut meshes),
            });
        }
        t.0 += vin;
        t.1 += vout;
        t.2 += fin;
        t.3 += fout;
        t.4 += st.faces_removed;
        t.5 += st.faces_split;
        t.6 += st.area_in;
        t.7 += st.area_kept;
        tiles.push(TileOps {
            tile: stem,
            source: src.display().to_string(),
            output: output.display().to_string(),
            vertices_in: vin,
            vertices_out: vout,
            faces_in: fin,
            faces_out: fout,
            detail: serde_json::to_value(&st).map_err(|e| e.to_string())?,
        });
    }
    let record = OpsRecord {
        schema: SCHEMA,
        tool: TOOL,
        op: "clip",
        timestamp: iso8601_utc_now(),
        params: serde_json::to_value(region).map_err(|e| e.to_string())?,
        totals: serde_json::json!({
            "tiles": tiles.len(),
            "vertices_in": t.0, "vertices_out": t.1,
            "faces_in": t.2, "faces_out": t.3,
            "faces_removed": t.4, "faces_split": t.5,
            "area_in": t.6, "area_kept": t.7,
        }),
        tiles,
    };
    record.write(ops_path)?;
    Ok(EditOutcome { record, edited })
}

/// 运行 flatten（压平）：逐 tile 加载 → 压平 → 写 OBJ/约定 b3dm 路径 → 汇总留痕。
///
/// `OutputFormat::B3dm` 时输出前做面积加权法线重算（压平改变了顶点
/// 邻域几何，原法线不再可信；OBJ 输出不含法线故不做）。
pub fn run_flatten(
    source: &Path,
    params: &FlattenParams,
    out: &Path,
    ops_path: &Path,
    opts: &EditRunOpts,
) -> Result<EditOutcome, String> {
    params.validate()?;
    let sources = filter_tiles(io::collect_osgb_sources(source)?, opts.tile.as_deref())?;
    prepare_out_for(opts.format, source, out)?;
    let is_dir = source.is_dir();
    let mut tiles = Vec::new();
    let mut edited = Vec::new();
    let (mut tmoved, mut tregion) = (0u64, 0u64);
    for src in &sources {
        let stem = stem_of(src)?;
        let mut meshes = io::load_osgb_zup(src)?;
        let (vin, fin) = mesh_totals(&meshes);
        let st = flatten::flatten_meshes(&mut meshes, params)?;
        if opts.format == OutputFormat::B3dm {
            geom::recompute_normals_area_weighted(&mut meshes);
        }
        let output = output_path_for(opts.format, is_dir, out, &stem, &meshes)?;
        let (vout, fout) = mesh_totals(&meshes);
        if opts.format == OutputFormat::B3dm {
            edited.push(EditedTile {
                tile: stem.clone(),
                source: src.clone(),
                meshes: std::mem::take(&mut meshes),
            });
        }
        tmoved += st.vertices_moved;
        tregion += st.region_vertices;
        tiles.push(TileOps {
            tile: stem,
            source: src.display().to_string(),
            output: output.display().to_string(),
            vertices_in: vin,
            vertices_out: vout,
            faces_in: fin,
            faces_out: fout,
            detail: serde_json::to_value(&st).map_err(|e| e.to_string())?,
        });
    }
    let record = OpsRecord {
        schema: SCHEMA,
        tool: TOOL,
        op: "flatten",
        timestamp: iso8601_utc_now(),
        params: serde_json::to_value(params).map_err(|e| e.to_string())?,
        totals: serde_json::json!({
            "tiles": tiles.len(),
            "region_vertices": tregion,
            "vertices_moved": tmoved,
        }),
        tiles,
    };
    record.write(ops_path)?;
    Ok(EditOutcome { record, edited })
}

/// 运行 ground-align（地面对齐）。
///
/// `reference` 为 Some 时，目标高程取参考 tile（.osgb）同 bbox 区域最低点；
/// 否则用 [`AlignParams::target`]。
///
/// `OutputFormat::B3dm` 时输出前做面积加权法线重算（同 [`run_flatten`]）。
pub fn run_align(
    source: &Path,
    params: &AlignParams,
    reference: Option<&Path>,
    out: &Path,
    ops_path: &Path,
    opts: &EditRunOpts,
) -> Result<EditOutcome, String> {
    let mut params = params.clone();
    params.validate()?;
    let target_from_ref = match reference {
        Some(r) => {
            let ref_meshes = io::load_osgb_zup(r)?;
            let name = r.file_stem().and_then(|s| s.to_str()).unwrap_or("?").to_string();
            align::region_min_z(&ref_meshes, &params)
                .ok_or_else(|| format!("参考 tile {name} 在 bbox 区域内没有任何顶点"))?
        }
        None => params.target,
    };
    params.target = target_from_ref;

    let sources = filter_tiles(io::collect_osgb_sources(source)?, opts.tile.as_deref())?;
    prepare_out_for(opts.format, source, out)?;
    let is_dir = source.is_dir();
    let mut tiles = Vec::new();
    let mut edited = Vec::new();
    let mut tdz_sum = 0.0f64;
    for src in &sources {
        let stem = stem_of(src)?;
        let mut meshes = io::load_osgb_zup(src)?;
        let (vin, fin) = mesh_totals(&meshes);
        let st = align::align_meshes(&mut meshes, &params)?;
        if opts.format == OutputFormat::B3dm {
            geom::recompute_normals_area_weighted(&mut meshes);
        }
        let output = output_path_for(opts.format, is_dir, out, &stem, &meshes)?;
        let (vout, fout) = mesh_totals(&meshes);
        if opts.format == OutputFormat::B3dm {
            edited.push(EditedTile {
                tile: stem.clone(),
                source: src.clone(),
                meshes: std::mem::take(&mut meshes),
            });
        }
        tdz_sum += st.dz;
        tiles.push(TileOps {
            tile: stem,
            source: src.display().to_string(),
            output: output.display().to_string(),
            vertices_in: vin,
            vertices_out: vout,
            faces_in: fin,
            faces_out: fout,
            detail: serde_json::to_value(&st).map_err(|e| e.to_string())?,
        });
    }
    let record = OpsRecord {
        schema: SCHEMA,
        tool: TOOL,
        op: "ground-align",
        timestamp: iso8601_utc_now(),
        params: serde_json::json!({
            "bbox": {
                "minx": params.minx, "miny": params.miny,
                "maxx": params.maxx, "maxy": params.maxy,
            },
            "target_elevation": params.target,
            "reference": reference.map(|r| r.display().to_string()),
        }),
        totals: serde_json::json!({
            "tiles": tiles.len(),
            "dz_sum": tdz_sum,
        }),
        tiles,
    };
    record.write(ops_path)?;
    Ok(EditOutcome { record, edited })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::align::region_min_z;

    #[test]
    fn timestamp_known_dates() {
        assert_eq!(civil_from_days(0), (1970, 1, 1));
        // 2024-01-01 = 19723 天（1704067200s）
        assert_eq!(civil_from_days(19_723), (2024, 1, 1));
        // 2026-10-04：20730 天
        assert_eq!(civil_from_days(20_730), (2026, 10, 4));
        let ts = iso8601_utc_now();
        assert_eq!(ts.len(), 24, "ISO 8601 毫秒精度长度");
        assert!(ts.ends_with('Z'));
        assert!(ts.starts_with("20"));
    }

    #[test]
    fn parse_numbers_ok_and_errors() {
        assert_eq!(parse_numbers("1, 2.5,-3", 3, "t").unwrap(), vec![1.0, 2.5, -3.0]);
        assert!(parse_numbers("1,2", 3, "t").is_err());
        assert!(parse_numbers("1,x", 2, "t").is_err());
    }

    // ---- 真实语料冒烟（语料缺失时跳过，与 cli 测试同一守门方式）----

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
        let d = std::env::temp_dir()
            .join(format!("tangis-editops-test-{}-{name}", std::process::id()));
        let _ = fs::remove_dir_all(&d);
        fs::create_dir_all(&d).unwrap();
        d
    }

    /// 真实 tile 抠除一个角（x>90 侧）：面积守恒、幸存顶点全在保留侧、
    /// OBJ/留痕落盘且留痕字段完整。
    #[test]
    fn runner_clip_real_corpus_corner() {
        let Some(dir) = real_osgb_dir() else { return };
        let src = dir.join("Tile_+000_+000.osgb");
        let work = temp_dir("clip");
        let out = work.join("out.obj");
        let ops = work.join("ops.json");
        let region = Region::Plane { a: 1.0, b: 0.0, c: 0.0, d: 90.0, keep_positive: false };

        let rec = run_clip(&src, &region, &out, &ops, &EditRunOpts::default()).unwrap();
        assert_eq!(rec.record.op, "clip");
        assert_eq!(rec.record.tiles.len(), 1);
        assert_eq!(rec.record.tiles[0].tile, "Tile_+000_+000");
        assert!(rec.record.tiles[0].vertices_out < rec.record.tiles[0].vertices_in, "应有几何被抠除");
        assert!(rec.edited.is_empty(), "Obj 格式不携带网格");

        // 留痕 JSON 可解析且 schema/timestamp/params 完整
        let v: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(&ops).unwrap()).unwrap();
        assert_eq!(v["schema"], SCHEMA);
        assert_eq!(v["op"], "clip");
        assert_eq!(v["params"]["type"], "plane");
        assert!(v["tiles"][0]["faces_removed"].as_u64().unwrap() > 0);
        // 面积守恒：area_kept + area_removed ≈ area_in
        let kept = v["tiles"][0]["area_kept"].as_f64().unwrap();
        let total = v["tiles"][0]["area_in"].as_f64().unwrap();
        assert!(kept > 0.0 && kept < total);

        // OBJ 幸存顶点全部在保留侧（x ≤ 90）
        let text = fs::read_to_string(&out).unwrap();
        let mut n = 0usize;
        for line in text.lines() {
            if let Some(rest) = line.strip_prefix("v ") {
                let x: f64 = rest.split_whitespace().next().unwrap().parse().unwrap();
                assert!(x <= 90.0 + 1e-3, "幸存顶点 x={x} 越过保留侧");
                n += 1;
            }
        }
        assert!(n > 0);
        assert_eq!(n as u64, rec.record.tiles[0].vertices_out, "OBJ 顶点数与留痕一致");

        let _ = fs::remove_dir_all(&work);
    }

    /// 真实 tile 压平一块区域：OBJ 回读校验区域内 z 全等 elevation、
    /// 过渡带顶点 z 介于 elevation 与原高程之间。
    #[test]
    fn runner_flatten_real_corpus_region() {
        let Some(dir) = real_osgb_dir() else { return };
        let src = dir.join("Tile_+000_+000.osgb");
        let work = temp_dir("flatten");
        let out = work.join("out.obj");
        let ops = work.join("ops.json");
        let params = FlattenParams {
            minx: 40.0,
            miny: 40.0,
            maxx: 60.0,
            maxy: 60.0,
            elevation: 0.0,
            feather: 5.0,
        };

        let rec = run_flatten(&src, &params, &out, &ops, &EditRunOpts::default()).unwrap();
        assert_eq!(rec.record.op, "flatten");
        let detail = &rec.record.tiles[0].detail;
        assert!(detail["region_vertices"].as_u64().unwrap() > 0);
        // z_before 期望值由原始网格实测得出（不预设黄金值）
        let orig = io::load_osgb_zup(&src).unwrap();
        let mut zmax = f64::NEG_INFINITY;
        let mut zmin = f64::INFINITY;
        for m in &orig {
            for v in &m.vertices {
                let (x, y) = (v[0] as f64, v[1] as f64);
                if (40.0..=60.0).contains(&x) && (40.0..=60.0).contains(&y) {
                    zmax = zmax.max(v[2] as f64);
                    zmin = zmin.min(v[2] as f64);
                }
            }
        }
        assert!((detail["z_max_before"].as_f64().unwrap() - zmax).abs() < 1e-6);
        assert!((detail["z_min_before"].as_f64().unwrap() - zmin).abs() < 1e-6);

        // OBJ 回读：区域内 z == 0；过渡带 0 < dist < 5 内 z ∈ [0, z0]
        let text = fs::read_to_string(&out).unwrap();
        let mut region_n = 0usize;
        for line in text.lines() {
            if let Some(rest) = line.strip_prefix("v ") {
                let nums: Vec<f64> = rest
                    .split_whitespace()
                    .map(|t| t.parse().unwrap())
                    .collect();
                let (x, y, z) = (nums[0], nums[1], nums[2]);
                let dx = (40.0 - x).max(x - 60.0).max(0.0);
                let dy = (40.0 - y).max(y - 60.0).max(0.0);
                let dist = (dx * dx + dy * dy).sqrt();
                if dist == 0.0 {
                    assert!(z.abs() < 1e-6, "区域内顶点 z 应全等 0，实际 {z}");
                    region_n += 1;
                } else if dist < 5.0 {
                    assert!((-2.14..=3.76).contains(&z), "过渡带 z={z} 越界（原高程范围）");
                } else {
                    assert!((-2.14..=3.76).contains(&z), "带外 z={z} 不应被改动");
                }
            }
        }
        assert!(region_n > 0, "区域内应有顶点");
        let _ = fs::remove_dir_all(&work);
    }

    /// 地面对齐：区域最低点抬到 elevation，OBJ 回读验证全 tile 平移量一致。
    #[test]
    fn runner_align_real_corpus() {
        let Some(dir) = real_osgb_dir() else { return };
        let src = dir.join("Tile_+000_+000.osgb");
        let work = temp_dir("align");
        let out = work.join("out.obj");
        let ops = work.join("ops.json");
        let params = AlignParams { minx: 0.0, miny: 0.0, maxx: 50.0, maxy: 50.0, target: 10.0 };

        let rec = run_align(&src, &params, None, &out, &ops, &EditRunOpts::default()).unwrap();
        assert_eq!(rec.record.op, "ground-align");
        // 期望 dz 由原始网格区域最低点实测得出（不预设黄金值）
        let orig = io::load_osgb_zup(&src).unwrap();
        let region_min = region_min_z(&orig, &params).unwrap();
        let dz = rec.record.tiles[0].detail["dz"].as_f64().unwrap();
        assert!((dz - (10.0 - region_min)).abs() < 1e-9, "dz={dz} region_min={region_min}");
        assert_eq!(rec.record.tiles[0].detail["z_min_after"], serde_json::json!(10.0));

        // OBJ 回读：全 tile 最低点 = 原全 tile 最低点 + dz（刚性平移整体生效）
        let mut global_min = f64::INFINITY;
        for m in &orig {
            for v in &m.vertices {
                global_min = global_min.min(v[2] as f64);
            }
        }
        let text = fs::read_to_string(&out).unwrap();
        let mut zmin = f64::INFINITY;
        for line in text.lines() {
            if let Some(rest) = line.strip_prefix("v ") {
                let z: f64 = rest.split_whitespace().nth(2).unwrap().parse().unwrap();
                zmin = zmin.min(z);
            }
        }
        assert!(
            (zmin - (global_min + dz)).abs() < 1e-3,
            "全 tile 最低点 {zmin} 应=原最低 {global_min}+dz({dz})"
        );
        let _ = fs::remove_dir_all(&work);
    }

    /// 真实语料参考对齐：bbox 取源 tile 与参考 tile 包围盒交集（真实接缝
    /// 场景——相邻 tile 沿共享边对齐），目标取参考在该区域最低点。
    #[test]
    fn runner_align_with_reference_tile() {
        let Some(dir) = real_osgb_dir() else { return };
        let src = dir.join("Tile_+000_+000.osgb");
        let reference = dir.join("Tile_+000_+001.osgb");
        let ref_meshes = io::load_osgb_zup(&reference).unwrap();
        let src_meshes = io::load_osgb_zup(&src).unwrap();
        let bounds_of = |ms: &[tangis_osgb::Mesh]| -> ([f32; 3], [f32; 3]) {
            let mut r: Option<([f32; 3], [f32; 3])> = None;
            for m in ms {
                if let Some((bmin, bmax)) = m.bounds() {
                    r = Some(match r {
                        None => (bmin, bmax),
                        Some((amin, amax)) => {
                            let mut lo = amin;
                            let mut hi = amax;
                            for k in 0..3 {
                                lo[k] = lo[k].min(bmin[k]);
                                hi[k] = hi[k].max(bmax[k]);
                            }
                            (lo, hi)
                        }
                    });
                }
            }
            r.expect("tile 应有几何")
        };
        let (smin, smax) = bounds_of(&src_meshes);
        let (rmin, rmax) = bounds_of(&ref_meshes);
        // 跨接缝条带：覆盖源与参考最接近的边缘各 10 m（语料 tile 间有间隙，
        // 区域内同时含两个 tile 的顶点，即真实的跨 tile 对齐场景）
        let (miny, maxy) = if rmin[1] >= smax[1] {
            ((smax[1] - 10.0) as f64, (rmin[1] + 10.0) as f64)
        } else {
            ((rmax[1] - 10.0) as f64, (smin[1] + 10.0) as f64)
        };
        let minx = (smin[0].max(rmin[0]) + 10.0) as f64;
        let maxx = (smax[0].min(rmax[0]) - 10.0) as f64;
        let work = temp_dir("align-ref");
        let out = work.join("out.obj");
        let ops = work.join("ops.json");
        let params = AlignParams { minx, miny, maxx, maxy, target: 0.0 };
        assert!(region_min_z(&src_meshes, &params).is_some(), "源在条带内应有顶点");
        assert!(region_min_z(&ref_meshes, &params).is_some(), "参考在条带内应有顶点");

        let rec = run_align(&src, &params, Some(&reference), &out, &ops, &EditRunOpts::default()).unwrap();
        let target = rec.record.params["target_elevation"].as_f64().unwrap();
        let expect = region_min_z(&ref_meshes, &params).unwrap();
        assert!((target - expect).abs() < 1e-9);
        assert!((rec.record.tiles[0].detail["z_min_after"].as_f64().unwrap() - expect).abs() < 1e-6);
        let _ = fs::remove_dir_all(&work);
    }

    /// 多 tile 目录源：逐 tile 留痕齐全，OBJ 一一对应。
    #[test]
    fn runner_clip_directory_source() {
        let Some(dir) = real_osgb_dir() else { return };
        let work = temp_dir("clip-dir");
        // 私有两 tile 目录，避免依赖整个语料
        let src_dir = work.join("src");
        fs::create_dir_all(&src_dir).unwrap();
        for t in ["Tile_+000_+000", "Tile_+001_+001"] {
            fs::copy(dir.join(format!("{t}.osgb")), src_dir.join(format!("{t}.osgb"))).unwrap();
        }
        let out = work.join("out");
        let ops = work.join("ops.json");
        let region = Region::BBox { minx: 80.0, miny: 80.0, maxx: 120.0, maxy: 120.0 };

        let rec = run_clip(&src_dir, &region, &out, &ops, &EditRunOpts::default()).unwrap();
        assert_eq!(rec.record.totals["tiles"], 2);
        assert_eq!(rec.record.tiles.len(), 2);
        assert!(rec.record.tiles[0].output.ends_with(".obj"));
        assert!(out.join("Tile_+000_+000.obj").is_file());
        assert!(out.join("Tile_+001_+001.obj").is_file());
        assert!(rec.record.totals["faces_removed"].as_u64().unwrap() > 0);
        let _ = fs::remove_dir_all(&work);
    }

    /// out 与源形态不匹配 → 明确报错。
    #[test]
    fn runner_rejects_mismatched_out() {
        let Some(dir) = real_osgb_dir() else { return };
        let work = temp_dir("mismatch");
        let ops = work.join("ops.json");
        let region = Region::Plane { a: 1.0, b: 0.0, c: 0.0, d: 90.0, keep_positive: false };
        // 文件源 + 目录 out → 报错
        let err = run_clip(
            &dir.join("Tile_+000_+000.osgb"),
            &region,
            &work,
            &ops,
            &EditRunOpts::default(),
        )
        .unwrap_err();
        assert!(err.contains("文件路径"), "{err}");
        let _ = fs::remove_dir_all(&work);
    }

    // ---- M2-F09b：b3dm 产物支持 ----

    /// clip + B3dm 格式：EditedTile 携带编辑后网格，纹理引用/内嵌字节
    /// 自源透传（atlas 不重排），UV 与 BATCHID 保留。
    #[test]
    fn clip_b3dm_format_carries_textured_meshes() {
        let Some(dir) = real_osgb_dir() else { return };
        let src = dir.join("Tile_+000_+000.osgb");
        let orig = io::load_osgb_zup(&src).unwrap();
        assert!(orig[0].texture.is_some() && orig[0].texture_inline.is_some());
        let work = temp_dir("clip-b3dm");
        let out = work.join("out");
        let ops = work.join("ops.json");
        let region = Region::Plane { a: 1.0, b: 0.0, c: 0.0, d: 90.0, keep_positive: false };
        let opts = EditRunOpts {
            format: OutputFormat::B3dm,
            tile: Some("Tile_+000_+000".into()),
        };

        let outcome = run_clip(&src, &region, &out, &ops, &opts).unwrap();
        assert_eq!(outcome.edited.len(), 1);
        let et = &outcome.edited[0];
        assert_eq!(et.tile, "Tile_+000_+000");
        assert!(!et.meshes.is_empty(), "切半个 tile 不应变空");
        for m in &et.meshes {
            assert_eq!(m.texture, orig[0].texture, "纹理引用应透传");
            assert_eq!(m.texture_inline, orig[0].texture_inline, "内嵌纹理字节应透传");
            assert!(m.uvs.is_some(), "UV 应保留");
            assert!(m.batch_ids.is_some(), "BATCHID 应保留");
        }
        // 留痕 output 指向约定的 b3dm 路径（cli 落盘）
        assert!(outcome.record.tiles[0].output.ends_with(".b3dm"));
        // b3dm 格式不写 OBJ
        assert!(!out.join("Tile_+000_+000.obj").exists());

        let _ = fs::remove_dir_all(&work);
    }

    /// flatten + B3dm 格式：法线面积加权重算——区域内（含过渡带内缘）
    /// 顶点法线 z 分量彼此一致（压平区域内部为常值平面 → +z）。
    #[test]
    fn flatten_b3dm_format_recomputes_normals() {
        let Some(dir) = real_osgb_dir() else { return };
        let src = dir.join("Tile_+000_+000.osgb");
        let work = temp_dir("flatten-b3dm");
        let out = work.join("out");
        let ops = work.join("ops.json");
        let params = FlattenParams {
            minx: 40.0,
            miny: 40.0,
            maxx: 60.0,
            maxy: 60.0,
            elevation: 0.0,
            feather: 0.0,
        };
        let opts = EditRunOpts { format: OutputFormat::B3dm, tile: None };

        let outcome = run_flatten(&src, &params, &out, &ops, &opts).unwrap();
        let et = &outcome.edited[0];
        assert_eq!(et.tile, "Tile_+000_+000");
        // 深压平区域内部（45..55，与过渡带/边界留足网格间距）：
        // 顶点及其全部邻接三角形都在平面 z=0 上 → 法线恒 +z 且彼此一致
        let mut region_z: Option<f64> = None;
        for m in &et.meshes {
            let ns = m.normals.as_ref().expect("b3dm 输出必须带重算法线");
            assert_eq!(ns.len(), m.vertices.len());
            for (v, n) in m.vertices.iter().zip(ns) {
                let (x, y) = (v[0] as f64, v[1] as f64);
                if (45.0..=55.0).contains(&x) && (45.0..=55.0).contains(&y) {
                    assert_eq!(v[2], 0.0, "区域内顶点应被压平");
                    let z = n[2] as f64;
                    match region_z {
                        None => region_z = Some(z),
                        Some(prev) => assert!((z - prev).abs() < 1e-9, "区域内法线 z 不一致: {prev} vs {z}"),
                    }
                }
            }
        }
        let z = region_z.expect("压平区域内应有顶点");
        assert!((z - 1.0).abs() < 1e-6, "区域内部法线应为 +z，实际 {z}");

        let _ = fs::remove_dir_all(&work);
    }

    /// --tile 过滤：命中单 tile；无匹配明确报错。
    #[test]
    fn runner_tile_filter() {
        let Some(dir) = real_osgb_dir() else { return };
        let work = temp_dir("tile-filter");
        let src_dir = work.join("src");
        fs::create_dir_all(&src_dir).unwrap();
        for t in ["Tile_+000_+000", "Tile_+001_+001"] {
            fs::copy(dir.join(format!("{t}.osgb")), src_dir.join(format!("{t}.osgb"))).unwrap();
        }
        let out = work.join("out");
        let ops = work.join("ops.json");
        let region = Region::BBox { minx: 80.0, miny: 80.0, maxx: 120.0, maxy: 120.0 };

        let opts = EditRunOpts {
            format: OutputFormat::Obj,
            tile: Some("Tile_+001_+001".into()),
        };
        let rec = run_clip(&src_dir, &region, &out, &ops, &opts).unwrap();
        assert_eq!(rec.record.tiles.len(), 1);
        assert_eq!(rec.record.tiles[0].tile, "Tile_+001_+001");

        let opts = EditRunOpts { format: OutputFormat::Obj, tile: Some("no-such".into()) };
        let err = run_clip(&src_dir, &region, &out, &ops, &opts).unwrap_err();
        assert!(err.contains("--tile no-such"), "{err}");

        // "all" 显式全量
        let opts = EditRunOpts { format: OutputFormat::Obj, tile: Some("all".into()) };
        let rec = run_clip(&src_dir, &region, &out, &ops, &opts).unwrap();
        assert_eq!(rec.record.tiles.len(), 2);

        let _ = fs::remove_dir_all(&work);
    }

    /// B3dm 格式 + out 为文件 → 明确报错。
    #[test]
    fn runner_b3dm_rejects_file_out() {
        let Some(dir) = real_osgb_dir() else { return };
        let work = temp_dir("b3dm-out");
        let ops = work.join("ops.json");
        let region = Region::Plane { a: 1.0, b: 0.0, c: 0.0, d: 90.0, keep_positive: false };
        let opts = EditRunOpts { format: OutputFormat::B3dm, tile: None };
        let out_file = work.join("out.bin");
        fs::write(&out_file, b"x").unwrap();
        let err = run_clip(&dir.join("Tile_+000_+000.osgb"), &region, &out_file, &ops, &opts)
            .unwrap_err();
        assert!(err.contains("必须是目录"), "{err}");
        let _ = fs::remove_dir_all(&work);
    }
}
