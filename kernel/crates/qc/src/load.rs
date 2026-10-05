//! 质检输入：扫描数据源（目录 / 单 OBJ / 单 OSGB）并加载为内部 Z-up 网格。
//!
//! - 目录：递归收集 `*.osgb` / `*.obj`，同 stem 时 `.osgb` 优先（与 kernel
//!   build 的 `find_source_in_dir` 约定一致），按文件名排序；
//! - OSGB：`tangis_osgb::parse_file` → `Scene::merged` → 真实语料逆变换
//!   `(x,y,z)→(x,z,−y)` 恢复内部 Z-up（同 cli geometry.rs 管线，质检不加载
//!   纹理）；
//! - OBJ：tobj（single_index + 三角化）→ `(x,y,z)→(x,−z,y)` 转内部 Z-up。

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use tangis_osgb::Mesh;

/// 待加载 tile（扫描产物）。
#[derive(Debug, Clone)]
pub struct TileInput {
    /// tile 名 = 文件 stem（裂缝配对用它解析网格下标）。
    pub name: String,
    pub path: PathBuf,
}

/// 已加载 tile。
#[derive(Debug, Clone)]
pub struct LoadedTile {
    pub name: String,
    pub path: PathBuf,
    /// 内部 Z-up 网格（与 kernel build 一致）。
    pub mesh: Mesh,
}

fn ext_of(p: &Path) -> String {
    p.extension()
        .and_then(|e| e.to_str())
        .map(|e| e.to_ascii_lowercase())
        .unwrap_or_default()
}

/// 扫描数据源：单文件（.obj/.osgb）或目录（递归，osgb 优先于同 stem obj）。
pub fn scan_source(source: &Path) -> Result<Vec<TileInput>, String> {
    if source.is_file() {
        let ext = ext_of(source);
        if ext != "obj" && ext != "osgb" {
            return Err(format!(
                "不支持的几何源扩展名 `.{ext}`（{}）：仅支持 .obj / .osgb",
                source.display()
            ));
        }
        let name = source
            .file_stem()
            .map(|s| s.to_string_lossy().into_owned())
            .ok_or_else(|| format!("路径无文件名: {}", source.display()))?;
        return Ok(vec![TileInput { name, path: source.to_path_buf() }]);
    }
    if !source.is_dir() {
        return Err(format!("--source 不存在或既非文件也非目录: {}", source.display()));
    }
    // stem → (osgb 路径, obj 路径)，递归收集
    let mut by_stem: BTreeMap<String, (Option<PathBuf>, Option<PathBuf>)> = BTreeMap::new();
    collect_dir(source, &mut by_stem)?;
    let mut inputs = Vec::new();
    for (stem, (osgb, obj)) in by_stem {
        if let Some(p) = osgb {
            inputs.push(TileInput { name: stem, path: p });
        } else if let Some(p) = obj {
            inputs.push(TileInput { name: stem, path: p });
        }
    }
    if inputs.is_empty() {
        return Err(format!("目录 {} 中没有 .osgb / .obj 文件", source.display()));
    }
    Ok(inputs)
}

fn collect_dir(
    dir: &Path,
    by_stem: &mut BTreeMap<String, (Option<PathBuf>, Option<PathBuf>)>,
) -> Result<(), String> {
    let entries = std::fs::read_dir(dir)
        .map_err(|e| format!("无法读取目录 {}: {e}", dir.display()))?;
    for e in entries {
        let e = e.map_err(|e| format!("读取目录 {} 失败: {e}", dir.display()))?;
        let p = e.path();
        if p.is_dir() && !p.is_symlink() {
            collect_dir(&p, by_stem)?;
            continue;
        }
        if !p.is_file() {
            continue;
        }
        let stem = match p.file_stem().map(|s| s.to_string_lossy().into_owned()) {
            Some(s) => s,
            None => continue,
        };
        let slot = by_stem.entry(stem).or_default();
        match ext_of(&p).as_str() {
            "osgb" => slot.0.get_or_insert(p),
            "obj" => slot.1.get_or_insert(p),
            _ => continue,
        };
    }
    Ok(())
}

/// 加载单个 tile 为内部 Z-up 网格。
pub fn load_tile(input: &TileInput) -> Result<LoadedTile, String> {
    let mesh = match ext_of(&input.path).as_str() {
        "osgb" => load_osgb(&input.path)?,
        "obj" => load_obj(&input.path)?,
        other => {
            return Err(format!(
                "不支持的几何源扩展名 `.{other}`（{}）：仅支持 .obj / .osgb",
                input.path.display()
            ))
        }
    };
    Ok(LoadedTile { name: input.name.clone(), path: input.path.clone(), mesh })
}

/// 真实 OSGB（GL y-up）→ 内部 Z-up：(x, y, z) → (x, z, −y)。
/// 与 cli geometry.rs 的 `osgb_to_z_up` 同式（绕 X 轴旋转的逆变换）。
fn osgb_to_z_up(v: [f32; 3]) -> [f32; 3] {
    [v[0], v[2], -v[1]]
}

/// OBJ（Y-up 源）→ 内部 Z-up：(x, y, z) → (x, −z, y)。
/// 与 cli geometry.rs 的 `z_up` 同式。
fn z_up(v: [f32; 3]) -> [f32; 3] {
    [v[0], -v[2], v[1]]
}

fn load_osgb(path: &Path) -> Result<Mesh, String> {
    let scene = tangis_osgb::parse_file(path)
        .map_err(|e| format!("解析 OSGB {} 失败: {e}", path.display()))?;
    let mut mesh = scene
        .merged()
        .map_err(|e| format!("OSGB {} 网格合并失败: {e}", path.display()))?;
    for v in &mut mesh.vertices {
        *v = osgb_to_z_up(*v);
    }
    if mesh.vertices.is_empty() || mesh.indices.is_empty() {
        return Err(format!("OSGB {} 不含可用几何", path.display()));
    }
    Ok(mesh)
}

fn load_obj(path: &Path) -> Result<Mesh, String> {
    let (models, _materials) = tobj::load_obj(
        path,
        &tobj::LoadOptions { single_index: true, triangulate: true, ..Default::default() },
    )
    .map_err(|e| format!("解析 OBJ {} 失败: {e}", path.display()))?;
    let mut mesh = Mesh::default();
    for model in &models {
        let m = &model.mesh;
        let base = mesh.vertices.len() as u32;
        mesh.vertices
            .extend(m.positions.chunks_exact(3).map(|p| z_up([p[0], p[1], p[2]])));
        mesh.indices.extend(m.indices.iter().map(|i| i + base));
    }
    if mesh.vertices.is_empty() || mesh.indices.is_empty() {
        return Err(format!("OBJ {} 不含可用几何", path.display()));
    }
    Ok(mesh)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn real_osgb_dir() -> Option<PathBuf> {
        let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../../testdata/osgb-real/osgb");
        if dir.join("Tile_+000_+000.osgb").is_file() {
            Some(dir)
        } else {
            None
        }
    }

    #[test]
    fn scans_real_corpus_with_osgb_priority() {
        let Some(dir) = real_osgb_dir() else {
            eprintln!("跳过：真实语料不存在");
            return;
        };
        let inputs = scan_source(&dir).unwrap();
        // 80 tile：64 精细（Tile_）+ 16 粗层（TileC_）
        assert_eq!(inputs.len(), 80);
        assert!(inputs.iter().all(|i| i.path.extension().unwrap() == "osgb"));
        // 排序稳定（BTreeMap）
        assert_eq!(inputs[0].name, "TileC_+000_+000");
    }

    #[test]
    fn loads_real_tile_z_up() {
        let Some(dir) = real_osgb_dir() else { return };
        let input = TileInput {
            name: "Tile_+000_+000".into(),
            path: dir.join("Tile_+000_+000.osgb"),
        };
        let tile = load_tile(&input).unwrap();
        assert_eq!(tile.mesh.vertices.len(), 1089);
        assert_eq!(tile.mesh.indices.len(), 2048 * 3);
        let (min, max) = tile.mesh.bounds().unwrap();
        // Z-up：x/y ∈ [0,100]（水平），z 为高度（小量级）
        assert!((min[0] - 0.0).abs() < 1e-3 && (max[0] - 100.0).abs() < 1e-3);
        assert!((min[1] - 0.0).abs() < 1e-3 && (max[1] - 100.0).abs() < 1e-3);
        assert!(min[2] > -10.0 && max[2] < 10.0);
    }

    #[test]
    fn rejects_missing_and_unknown() {
        assert!(scan_source(Path::new("/no/such/path")).is_err());
        let f = std::env::temp_dir().join("tangis-qc-bad.txt");
        std::fs::write(&f, b"x").unwrap();
        let err = scan_source(&f).unwrap_err();
        assert!(err.contains("不支持的几何源扩展名"), "{err}");
        let _ = std::fs::remove_file(&f);
    }
}
