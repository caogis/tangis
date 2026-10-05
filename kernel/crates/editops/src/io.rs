//! 磁盘 IO：OSGB 加载（统一到内部 Z-up）、OBJ 写出、编辑源收集。
//!
//! # 坐标管线（与 cli build 管线严格一致）
//!
//! 真实 OSGB 顶点为 GL y-up（z-up 源经 `(x,y,z)→(x,−z,y)` 写出），加载时用
//! 其逆变换 `(x,y,z)→(x,z,−y)` 恢复内部 Z-up（绕 X 轴旋转，右手系，三角形
//! 绕向不变）。所有编辑算子在 Z-up 空间进行；OBJ 输出亦为 Z-up（文件头
//! 注释声明），与 manifest `Bounds` / tileset boundingVolume 同一约定。

use std::fs;
use std::path::{Path, PathBuf};

use tangis_osgb::Mesh;

/// 真实 OSGB（GL y-up）→ 内部 Z-up：`(x, y, z) → (x, z, −y)`。
fn osgb_to_z_up(v: [f32; 3]) -> [f32; 3] {
    [v[0], v[2], -v[1]]
}

/// 解析 OSGB 为 z-up 网格列表（每个 osg::Geometry 一个 [`Mesh`]，
/// 保持 per-Geometry 结构——空 Geometry 删除语义依赖它）。
pub fn load_osgb_zup(path: &Path) -> Result<Vec<Mesh>, String> {
    let scene = tangis_osgb::parse_file(path)
        .map_err(|e| format!("解析 OSGB {} 失败: {e}", path.display()))?;
    let mut out = Vec::with_capacity(scene.meshes.len());
    for mut m in scene.meshes {
        for v in &mut m.vertices {
            *v = osgb_to_z_up(*v);
        }
        if let Some(ns) = &mut m.normals {
            for n in ns.iter_mut() {
                *n = osgb_to_z_up(*n);
            }
        }
        if !m.vertices.is_empty() && !m.indices.is_empty() {
            out.push(m);
        }
    }
    if out.is_empty() {
        return Err(format!("OSGB {} 不含可用几何", path.display()));
    }
    Ok(out)
}

/// 收集编辑源：单个 .osgb 文件，或目录下全部 *.osgb（排序，非递归）。
pub fn collect_osgb_sources(source: &Path) -> Result<Vec<PathBuf>, String> {
    if source.is_dir() {
        let mut files: Vec<PathBuf> = fs::read_dir(source)
            .map_err(|e| format!("无法读取目录 {}: {e}", source.display()))?
            .filter_map(|e| e.ok())
            .map(|e| e.path())
            .filter(|p| {
                p.extension()
                    .and_then(|e| e.to_str())
                    .map(|e| e.eq_ignore_ascii_case("osgb"))
                    .unwrap_or(false)
            })
            .collect();
        files.sort();
        if files.is_empty() {
            return Err(format!("目录 {} 中没有 .osgb 文件", source.display()));
        }
        Ok(files)
    } else if source.is_file() {
        Ok(vec![source.to_path_buf()])
    } else {
        Err(format!("编辑源 {} 不存在", source.display()))
    }
}

/// 网格列表 → OBJ 文本（内部 Z-up；多网格按 `<tile>_m<i>` 分组保留
/// Geometry 边界）。返回实写 (顶点数, 面数)。
pub fn write_obj(path: &Path, tile: &str, meshes: &[Mesh]) -> Result<(usize, usize), String> {
    let mut s = String::from("# tangis-editops 编辑输出（操作留痕见配套 ops JSON）\n");
    s.push_str(&format!("# tile: {tile}\n"));
    s.push_str("# 坐标约定: 内部 Z-up（x/y 水平, z 高度, 单位米）\n");
    let single = meshes.len() == 1;
    let (mut nv, mut nf) = (0usize, 0usize);
    for (i, m) in meshes.iter().enumerate() {
        if m.vertices.is_empty() || m.indices.is_empty() {
            continue;
        }
        if single {
            s.push_str(&format!("o {tile}\n"));
        } else {
            s.push_str(&format!("o {tile}_m{i}\n"));
        }
        for v in &m.vertices {
            s.push_str(&format!("v {} {} {}\n", v[0], v[1], v[2]));
        }
        let base = nv as u32;
        for f in m.indices.chunks_exact(3) {
            s.push_str(&format!(
                "f {} {} {}\n",
                f[0] + base + 1,
                f[1] + base + 1,
                f[2] + base + 1
            ));
        }
        nv += m.vertices.len();
        nf += m.indices.len() / 3;
    }
    s.push_str(&format!("# vertices: {nv} faces: {nf}\n"));
    if let Some(parent) = path.parent() {
        if !parent.as_os_str().is_empty() && !parent.exists() {
            fs::create_dir_all(parent)
                .map_err(|e| format!("无法创建目录 {}: {e}", parent.display()))?;
        }
    }
    fs::write(path, s).map_err(|e| format!("无法写 OBJ {}: {e}", path.display()))?;
    Ok((nv, nf))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// OBJ 写出：v/f 计数、分组、1-based 索引、坐标约定注释。
    #[test]
    fn write_obj_counts_and_groups() {
        let meshes = vec![
            Mesh {
                vertices: vec![[0.0, 0.0, 0.0], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0]],
                indices: vec![0, 1, 2],
                normals: None,
                uvs: None,
                texture: None,
                texture_inline: None,
                batch_ids: Some(vec![0; 3]),
            },
            Mesh {
                vertices: vec![[2.0, 0.0, 0.0], [3.0, 0.0, 0.0], [2.0, 1.0, 0.0]],
                indices: vec![0, 1, 2],
                normals: None,
                uvs: None,
                texture: None,
                texture_inline: None,
                batch_ids: Some(vec![1; 3]),
            },
        ];
        let path = std::env::temp_dir().join(format!("tangis-editops-test-{}-a.obj", std::process::id()));
        let (nv, nf) = write_obj(&path, "TileX", &meshes).unwrap();
        assert_eq!((nv, nf), (6, 2));
        let text = fs::read_to_string(&path).unwrap();
        assert!(text.contains("# 坐标约定: 内部 Z-up"));
        assert!(text.contains("o TileX_m0"));
        assert!(text.contains("o TileX_m1"));
        // 第二组索引偏移到全局 1-based（4 5 6）
        assert!(text.contains("f 4 5 6"));
        assert!(text.contains("# vertices: 6 faces: 2"));
        let _ = fs::remove_file(&path);
    }

    /// 空网格列表（全部几何被抠除）产出仅注释的 OBJ 且不报错。
    #[test]
    fn write_obj_empty_ok() {
        let path = std::env::temp_dir().join(format!("tangis-editops-test-{}-b.obj", std::process::id()));
        let (nv, nf) = write_obj(&path, "TileX", &[]).unwrap();
        assert_eq!((nv, nf), (0, 0));
        let text = fs::read_to_string(&path).unwrap();
        assert!(text.contains("# vertices: 0 faces: 0"));
        let _ = fs::remove_file(&path);
    }

    /// 源收集：文件直通；目录排序收集 *.osgb；缺失报错。
    #[test]
    fn collect_sources() {
        let dir = std::env::temp_dir().join(format!("tangis-editops-test-{}-src", std::process::id()));
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.join("b.osgb"), b"x").unwrap();
        fs::write(dir.join("a.osgb"), b"x").unwrap();
        fs::write(dir.join("c.txt"), b"x").unwrap();
        let files = collect_osgb_sources(&dir).unwrap();
        assert_eq!(files.len(), 2);
        assert!(files[0].ends_with("a.osgb"));
        assert!(files[1].ends_with("b.osgb"));
        let missing = dir.join("nope.osgb");
        assert!(collect_osgb_sources(&missing).is_err());
        let _ = fs::remove_dir_all(&dir);
    }
}
