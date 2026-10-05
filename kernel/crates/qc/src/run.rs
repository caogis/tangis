//! 质检编排：加载 tile → per-tile 四项检测 → 裂缝跨 tile 配对 → 汇总。

use crate::checks::crack::{check_cracks, CrackTile};
use crate::checks::{degenerate, floating, normal_flip, self_intersect};
use crate::config::QcConfig;
use crate::load::LoadedTile;
use crate::report::{
    QcReport, Summary, TileReport,
};

/// 对已加载 tile 执行全部检测（`source` 仅作报告溯源记录）。
pub fn run_qc(tiles: Vec<LoadedTile>, config: &QcConfig, source: &str) -> QcReport {
    let mut tiles = tiles;
    tiles.sort_by(|a, b| a.name.cmp(&b.name));

    let mut tile_reports = Vec::with_capacity(tiles.len());
    let mut crack_inputs: Vec<CrackTile> = Vec::new();

    for t in &tiles {
        let faces = t.mesh.indices.len() / 3;
        let degenerate_r = config
            .degenerate
            .then(|| degenerate::check_degenerate(&t.mesh, config.degenerate_area_eps));
        let flip_r = config
            .normal_flip
            .then(|| normal_flip::check_normal_flip(&t.mesh, config.normal_flip_max_angle_deg));
        let float_r = config.floating.then(|| {
            floating::check_floating(
                &t.mesh,
                config.floating_voxel_size,
                config.floating_height_above,
                config.floating_max_volume_ratio,
            )
        });
        let intersect_r = config.self_intersect.then(|| self_intersect::check_self_intersect(&t.mesh));

        // 裂缝配对输入：原生坐标包围盒 + 名后缀下标 + 原生顶点
        let (bmin, bmax) = t.mesh.bounds().map(|(a, b)| (degenerate::f64p(a), degenerate::f64p(b)))
            .unwrap_or(([0.0; 3], [0.0; 3]));
        crack_inputs.push(CrackTile::new(
            &t.name,
            bmin,
            bmax,
            t.mesh.vertices.iter().map(|v| degenerate::f64p(*v)).collect(),
        ));

        tile_reports.push(TileReport {
            name: t.name.clone(),
            source_file: t.path.to_string_lossy().into_owned(),
            vertices: t.mesh.vertices.len(),
            faces,
            degenerate: degenerate_r,
            normal_flip: flip_r,
            floating: float_r,
            self_intersect: intersect_r,
        });
    }

    let (cracks, unpaired) = if config.crack {
        check_cracks(&crack_inputs, config.crack_gap)
    } else {
        (Vec::new(), 0)
    };

    let summary = summarize(&tile_reports, &cracks, config.crack);
    QcReport {
        schema_version: "1".to_string(),
        generated_at_unix: std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs_f64())
            .unwrap_or(0.0),
        source: source.to_string(),
        tile_count: tiles.len(),
        thresholds: config.clone(),
        tiles: tile_reports,
        crack_tiles_unpaired: unpaired,
        cracks,
        summary,
    }
}

fn summarize(tiles: &[TileReport], cracks: &[crate::report::CrackPairReport], crack_enabled: bool) -> Summary {
    let total_degenerate: usize =
        tiles.iter().filter_map(|t| t.degenerate.as_ref()).map(|d| d.total).sum();
    let total_flipped: usize =
        tiles.iter().filter_map(|t| t.normal_flip.as_ref()).map(|n| n.flipped_edges).sum();
    let total_floating: usize =
        tiles.iter().filter_map(|t| t.floating.as_ref()).map(|f| f.flagged.len()).sum();
    let total_intersect: usize =
        tiles.iter().filter_map(|t| t.self_intersect.as_ref()).map(|s| s.intersections).sum();
    let total_crack_segments: usize =
        cracks.iter().map(|c| c.gaps_over_threshold).sum();

    let mut s = Summary {
        tiles_with_degenerate: tiles
            .iter()
            .filter(|t| t.degenerate.as_ref().is_some_and(|d| d.total > 0))
            .count(),
        total_degenerate,
        tiles_with_normal_flip: tiles
            .iter()
            .filter(|t| t.normal_flip.as_ref().is_some_and(|n| n.flipped_edges > 0))
            .count(),
        total_flipped_edges: total_flipped,
        tiles_with_floating: tiles
            .iter()
            .filter(|t| t.floating.as_ref().is_some_and(|f| !f.flagged.is_empty()))
            .count(),
        total_floating_components: total_floating,
        crack_pairs_checked: if crack_enabled { cracks.len() } else { 0 },
        crack_pairs_flagged: cracks.iter().filter(|c| c.gaps_over_threshold > 0).count(),
        total_crack_segments,
        tiles_with_self_intersect: tiles
            .iter()
            .filter(|t| t.self_intersect.as_ref().is_some_and(|x| x.intersections > 0))
            .count(),
        total_self_intersections: total_intersect,
        passed: false,
    };
    s.passed = s.total_degenerate == 0
        && s.total_flipped_edges == 0
        && s.total_floating_components == 0
        && s.total_crack_segments == 0
        && s.total_self_intersections == 0;
    s
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::QcConfig;
    use std::path::PathBuf;
    use tangis_osgb::Mesh;

    fn tile(name: &str, mesh: Mesh) -> LoadedTile {
        LoadedTile { name: name.to_string(), path: PathBuf::from(format!("/{name}")), mesh }
    }

    fn ground() -> Mesh {
        Mesh {
            vertices: vec![[0.0; 3], [10.0, 0.0, 0.0], [10.0, 10.0, 0.0], [0.0, 10.0, 0.0]],
            indices: vec![0, 1, 2, 0, 2, 3],
            ..Mesh::default()
        }
    }

    #[test]
    fn clean_scene_passes() {
        let rep = run_qc(vec![tile("Tile_+000_+000", ground())], &QcConfig::default(), "mem");
        assert!(rep.summary.passed, "{:?}", rep.summary);
        assert_eq!(rep.tile_count, 1);
        assert!(rep.tiles[0].degenerate.is_some());
    }

    #[test]
    fn disabled_check_is_null_not_zero() {
        let cfg = QcConfig { floating: false, crack: false, ..QcConfig::default() };
        let rep = run_qc(vec![tile("t", ground())], &cfg, "mem");
        assert!(rep.tiles[0].floating.is_none());
        assert!(rep.cracks.is_empty());
        assert_eq!(rep.summary.crack_pairs_checked, 0);
    }
}
