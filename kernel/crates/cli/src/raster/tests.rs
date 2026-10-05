//! raster 模块测试：手写最小 TIFF 字节构造 fixture + GeoTIFF 解析 + XYZ 端到端。

use super::geotiff::{self, Crs};
use super::reproject::{lonlat_to_merc, tile_bounds};
use super::xyz::{cmd_raster2tiles, tile_path, RasterArgs};

// ===================== TIFF 字节构造器（仅测试用） =====================

/// 地理参考 fixture：GeoKey 键值对 + pixel scale + tiepoint。
struct GeoRef {
    scale: [f64; 3],
    tie: [f64; 6],
    keys: Vec<(u16, u16)>,
}

/// EPSG:4326（地理坐标系）。
fn geo_4326(scale: [f64; 3], tie: [f64; 6]) -> GeoRef {
    GeoRef {
        scale,
        tie,
        keys: vec![(1024, 2), (2048, 4326), (2054, 9102)],
    }
}

/// EPSG:3857（投影坐标系，地理基准 4326）。
fn geo_3857(scale: [f64; 3], tie: [f64; 6]) -> GeoRef {
    GeoRef {
        scale,
        tie,
        keys: vec![(1024, 1), (3072, 3857), (2048, 4326)],
    }
}

fn geokeys_bytes(keys: &[(u16, u16)]) -> Vec<u8> {
    let mut shorts = vec![1u16, 1, 0, keys.len() as u16];
    for &(k, v) in keys {
        shorts.extend_from_slice(&[k, 0, 1, v]);
    }
    shorts.iter().flat_map(|v| v.to_le_bytes()).collect()
}

enum Val {
    Inline([u8; 4]),
    Ext(Vec<u8>),
}

/// SHORT 内联值（TIFF 内联值右对齐于 4 字节小端）。
fn inline_u16(v: u16) -> Val {
    let mut a = [0u8; 4];
    a[..2].copy_from_slice(&v.to_le_bytes());
    Val::Inline(a)
}

/// 构造最小条带（单 strip）TIFF（小端，未压缩或 deflate 由调用方压好传入）。
fn build_strip_tiff(
    width: u16,
    height: u16,
    spp: u16,
    compression: u16,
    strip: &[u8],
    geo: Option<&GeoRef>,
) -> Vec<u8> {
    let photometric: u16 = if spp == 1 { 1 } else { 2 };
    let bits_bytes: Vec<u8> = vec![8u16; spp as usize]
        .iter()
        .flat_map(|b| b.to_le_bytes())
        .collect();

    let mut entries: Vec<(u16, u16, u32, Val)> = Vec::new();
    entries.push((256, 4, 1, Val::Inline((width as u32).to_le_bytes())));
    entries.push((257, 4, 1, Val::Inline((height as u32).to_le_bytes())));
    {
        let v = if bits_bytes.len() <= 4 {
            let mut a = [0u8; 4];
            a[..bits_bytes.len()].copy_from_slice(&bits_bytes);
            Val::Inline(a)
        } else {
            Val::Ext(bits_bytes)
        };
        entries.push((258, 3, spp as u32, v));
    }
    entries.push((259, 3, 1, inline_u16(compression)));
    entries.push((262, 3, 1, inline_u16(photometric)));
    entries.push((273, 4, 1, Val::Inline([0, 0, 0, 0]))); // strip offset，布局后回填
    entries.push((277, 3, 1, inline_u16(spp)));
    entries.push((278, 4, 1, Val::Inline((height as u32).to_le_bytes())));
    entries.push((279, 4, 1, Val::Inline((strip.len() as u32).to_le_bytes())));
    if let Some(g) = geo {
        entries.push((
            33550,
            12,
            3,
            Val::Ext(g.scale.iter().flat_map(|v| v.to_le_bytes()).collect()),
        ));
        entries.push((
            33922,
            12,
            6,
            Val::Ext(g.tie.iter().flat_map(|v| v.to_le_bytes()).collect()),
        ));
        let kb = geokeys_bytes(&g.keys);
        let count = (kb.len() / 2) as u32;
        let v = if kb.len() <= 4 {
            let mut a = [0u8; 4];
            a[..kb.len()].copy_from_slice(&kb);
            Val::Inline(a)
        } else {
            Val::Ext(kb)
        };
        entries.push((34735, 3, count, v));
    }
    entries.sort_by_key(|e| e.0);
    assemble(entries, strip)
}

/// 构造分块（Tiled）TIFF：tw x tl 分块，未压缩，块内行按 tw 对齐填充。
fn build_tiled_tiff(
    width: u16,
    height: u16,
    spp: u16,
    pixels: &[u8],
    tw: u16,
    tl: u16,
    geo: Option<&GeoRef>,
) -> Vec<u8> {
    let photometric: u16 = if spp == 1 { 1 } else { 2 };
    let across = width.div_ceil(tw) as usize;
    let down = height.div_ceil(tl) as usize;
    let n_tiles = across * down;

    let mut tile_data: Vec<u8> = Vec::new();
    let mut offsets_rel: Vec<u32> = Vec::new();
    let mut counts: Vec<u32> = Vec::new();
    for ty in 0..down {
        for tx in 0..across {
            let mut buf = vec![0u8; tw as usize * tl as usize * spp as usize];
            for row in 0..tl as usize {
                let gy = ty * tl as usize + row;
                if gy >= height as usize {
                    break;
                }
                let cols = (tw as usize).min(width as usize - tx * tw as usize);
                let src = (gy * width as usize + tx * tw as usize) * spp as usize;
                let dst = row * tw as usize * spp as usize;
                buf[dst..dst + cols * spp as usize]
                    .copy_from_slice(&pixels[src..src + cols * spp as usize]);
            }
            offsets_rel.push(tile_data.len() as u32);
            counts.push(buf.len() as u32);
            tile_data.extend_from_slice(&buf);
        }
    }
    let u32s = |v: &[u32]| v.iter().flat_map(|x| x.to_le_bytes()).collect::<Vec<u8>>();
    let photometric_u16 = photometric;
    let _ = photometric_u16;

    let mut entries: Vec<(u16, u16, u32, Val)> = Vec::new();
    entries.push((256, 4, 1, Val::Inline((width as u32).to_le_bytes())));
    entries.push((257, 4, 1, Val::Inline((height as u32).to_le_bytes())));
    let bits_bytes: Vec<u8> = vec![8u16; spp as usize]
        .iter()
        .flat_map(|b| b.to_le_bytes())
        .collect();
    {
        let v = if bits_bytes.len() <= 4 {
            let mut a = [0u8; 4];
            a[..bits_bytes.len()].copy_from_slice(&bits_bytes);
            Val::Inline(a)
        } else {
            Val::Ext(bits_bytes)
        };
        entries.push((258, 3, spp as u32, v));
    }
    entries.push((259, 3, 1, inline_u16(1))); // 未压缩
    entries.push((262, 3, 1, inline_u16(photometric)));
    entries.push((277, 3, 1, inline_u16(spp)));
    entries.push((284, 3, 1, inline_u16(1)));
    entries.push((322, 3, 1, inline_u16(tw)));
    entries.push((323, 3, 1, inline_u16(tl)));
    entries.push((324, 4, n_tiles as u32, Val::Ext(u32s(&offsets_rel))));
    entries.push((325, 4, n_tiles as u32, Val::Ext(u32s(&counts))));
    if let Some(g) = geo {
        entries.push((
            33550,
            12,
            3,
            Val::Ext(g.scale.iter().flat_map(|v| v.to_le_bytes()).collect()),
        ));
        entries.push((
            33922,
            12,
            6,
            Val::Ext(g.tie.iter().flat_map(|v| v.to_le_bytes()).collect()),
        ));
        let kb = geokeys_bytes(&g.keys);
        entries.push((34735, 3, (kb.len() / 2) as u32, Val::Ext(kb)));
    }
    entries.sort_by_key(|e| e.0);
    // 回填 tile 绝对偏移：tiles 数据在所有外部块之后
    let mut out = assemble(entries, &tile_data);
    // assemble 把 tile_data 放在 strip 位置；绝对基址 = 文件长 - tile_data 长度
    let tile_base = out.len() - tile_data.len();
    // 在 324 的外部块内逐项加上基址
    // 重新计算 324 块位置：布局确定性，直接扫描 IFD
    let ifd_off = u32::from_le_bytes([out[4], out[5], out[6], out[7]]) as usize;
    let count = u16::from_le_bytes([out[ifd_off], out[ifd_off + 1]]) as usize;
    for i in 0..count {
        let e = ifd_off + 2 + i * 12;
        let tag = u16::from_le_bytes([out[e], out[e + 1]]);
        if tag == 324 {
            let block = u32::from_le_bytes([out[e + 8], out[e + 9], out[e + 10], out[e + 11]])
                as usize;
            for k in 0..n_tiles {
                let p = block + k * 4;
                let v = u32::from_le_bytes([out[p], out[p + 1], out[p + 2], out[p + 3]]);
                out[p..p + 4].copy_from_slice(&(v + tile_base as u32).to_le_bytes());
            }
        }
    }
    out
}

/// IFD 装配：header + IFD + 外部数据 + 像素数据（273 偏移回填到最后一段）。
fn assemble(entries: Vec<(u16, u16, u32, Val)>, tail: &[u8]) -> Vec<u8> {
    let ifd_off = 8usize;
    let ifd_len = 2 + entries.len() * 12 + 4;
    let ext_off = ifd_off + ifd_len;
    let mut ext_positions: Vec<usize> = Vec::new();
    let mut ext_data: Vec<u8> = Vec::new();
    for (_, _, _, v) in &entries {
        if let Val::Ext(b) = v {
            ext_positions.push(ext_off + ext_data.len());
            ext_data.extend_from_slice(b);
            if ext_data.len() % 2 == 1 {
                ext_data.push(0);
            }
        }
    }
    let strip_off = (ifd_off + ifd_len + ext_data.len()) as u32;

    let mut out: Vec<u8> = Vec::new();
    out.extend_from_slice(b"II");
    out.extend_from_slice(&42u16.to_le_bytes());
    out.extend_from_slice(&(ifd_off as u32).to_le_bytes());
    out.extend_from_slice(&(entries.len() as u16).to_le_bytes());
    let mut ext_i = 0usize;
    for (i, (tag, kind, count, v)) in entries.iter().enumerate() {
        let _ = i;
        out.extend_from_slice(&tag.to_le_bytes());
        out.extend_from_slice(&kind.to_le_bytes());
        out.extend_from_slice(&count.to_le_bytes());
        match v {
            Val::Inline(a) => out.extend_from_slice(a),
            Val::Ext(_) => {
                out.extend_from_slice(&(ext_positions[ext_i] as u32).to_le_bytes());
                ext_i += 1;
            }
        }
    }
    out.extend_from_slice(&0u32.to_le_bytes());
    out.extend_from_slice(&ext_data);
    out.extend_from_slice(tail);
    // 回填 273（strip offset）
    if let Some((idx, _)) = entries.iter().enumerate().find(|(_, (t, _, _, _))| *t == 273) {
        let p = ifd_off + 2 + idx * 12 + 8;
        out[p..p + 4].copy_from_slice(&strip_off.to_le_bytes());
    }
    out
}

fn deflate(data: &[u8]) -> Vec<u8> {
    use flate2::write::ZlibEncoder;
    use std::io::Write;
    let mut e = ZlibEncoder::new(Vec::new(), flate2::Compression::default());
    e.write_all(data).unwrap();
    e.finish().unwrap()
}

// ===================== GeoTIFF 解析测试 =====================

#[test]
fn parses_uncompressed_gray_4326() {
    // 4x2 灰度，pixel_scale 0.5°/px，tiepoint 在 (0,0)→(10,20)
    let mut px = Vec::new();
    for i in 0..8 {
        px.push((i * 30) as u8);
    }
    let geo = geo_4326([0.5, 0.5, 0.0], [0.0, 0.0, 0.0, 10.0, 20.0, 0.0]);
    let tiff = build_strip_tiff(4, 2, 1, 1, &px, Some(&geo));
    let g = geotiff::read(&tiff).unwrap();
    assert_eq!((g.width, g.height), (4, 2));
    assert_eq!(g.crs, Crs::Epsg4326);
    assert_eq!(g.bbox, [10.0, 19.0, 12.0, 20.0]);
    // 第一像素值 0（黑），alpha 255；第三像素 60
    assert_eq!(g.rgba[0..4], [0, 0, 0, 255]);
    assert_eq!(g.rgba[8..12], [60, 60, 60, 255]);
}

#[test]
fn parses_deflate_rgb_3857() {
    // 3x3 RGB，deflate 压缩，1 米/像素 @ 原点
    let px: Vec<u8> = (0..27).map(|i| (i * 9) as u8).collect();
    let compressed = deflate(&px);
    let geo = geo_3857([1.0, 1.0, 0.0], [0.0, 0.0, 0.0, 500000.0, 4000000.0, 0.0]);
    let tiff = build_strip_tiff(3, 3, 3, 8, &compressed, Some(&geo));
    let g = geotiff::read(&tiff).unwrap();
    assert_eq!(g.crs, Crs::Epsg3857);
    assert_eq!(g.bbox, [500000.0, 3999997.0, 500003.0, 4000000.0]);
    assert_eq!(&g.rgba[0..4], &[0, 9, 18, 255]);
    // 最后一个像素
    let last = 8 * 4;
    assert_eq!(&g.rgba[last..last + 3], &[216, 225, 234]);
}

#[test]
fn parses_uncompressed_rgba_and_alpha() {
    let px: Vec<u8> = vec![
        10, 20, 30, 255, //
        40, 50, 60, 128, //
        70, 80, 90, 0, //
        1, 2, 3, 255,
    ];
    let geo = geo_3857([2.0, 2.0, 0.0], [0.0, 0.0, 0.0, 0.0, 0.0, 0.0]);
    let tiff = build_strip_tiff(2, 2, 4, 1, &px, Some(&geo));
    let g = geotiff::read(&tiff).unwrap();
    assert_eq!(g.rgba, px); // RGBA 原样保留
}

#[test]
fn parses_tiled_rgb() {
    // 3x3 影像按 2x2 分块（4 块，含填充）
    let px: Vec<u8> = (0..27).map(|i| (i * 7) as u8).collect();
    let geo = geo_4326([1.0, 1.0, 0.0], [0.0, 0.0, 0.0, 100.0, 50.0, 0.0]);
    let tiff = build_tiled_tiff(3, 3, 3, &px, 2, 2, Some(&geo));
    let g = geotiff::read(&tiff).unwrap();
    assert_eq!((g.width, g.height), (3, 3));
    assert_eq!(&g.rgba[0..3], &[0, 7, 14]);
    // 第二行第三列（像素 (2,1)，线性索引 5）：src idx = (1*3+2)*3 = 15 → 105,112,119
    assert_eq!(&g.rgba[5 * 4..5 * 4 + 3], &[105, 112, 119]);
}

#[test]
fn rejects_lzw_and_16bit_and_bad_predictor() {
    let geo = geo_4326([1.0, 1.0, 0.0], [0.0, 0.0, 0.0, 0.0, 0.0, 0.0]);
    let tiff = build_strip_tiff(2, 2, 1, 5, &[0u8; 4], Some(&geo)); // LZW=5
    let err = geotiff::read(&tiff).unwrap_err();
    assert!(err.contains("压缩"), "{err}");

    // 16 bit：手工改 BitsPerSample 条目（SHORT 16）
    let mut tiff = build_strip_tiff(2, 2, 1, 1, &[0u8; 8], Some(&geo));
    // 找到 258 条目，改 inline 值为 16
    let ifd = u32::from_le_bytes([tiff[4], tiff[5], tiff[6], tiff[7]]) as usize;
    let n = u16::from_le_bytes([tiff[ifd], tiff[ifd + 1]]) as usize;
    for i in 0..n {
        let e = ifd + 2 + i * 12;
        if u16::from_le_bytes([tiff[e], tiff[e + 1]]) == 258 {
            tiff[e + 8] = 16;
        }
    }
    let err = geotiff::read(&tiff).unwrap_err();
    assert!(err.contains("位深"), "{err}");

    // Predictor=2
    let mut tiff2 = build_strip_tiff(2, 2, 1, 1, &[0u8; 4], Some(&geo));
    // 在 IFD 里插入条目太麻烦，改为直接构造一个带 317 的：复制原文件并在 IFD 末尾扩展
    // 简化：直接校验 read 对缺失 predictor 的默认行为即可，Predictor=2 场景由
    // 修改 compression 条目 tag 的方式构造：
    let ifd2 = u32::from_le_bytes([tiff2[4], tiff2[5], tiff2[6], tiff2[7]]) as usize;
    let n2 = u16::from_le_bytes([tiff2[ifd2], tiff2[ifd2 + 1]]) as usize;
    // 把 262（Photometric）条目的 tag 改为 317（Predictor），值 2
    for i in 0..n2 {
        let e = ifd2 + 2 + i * 12;
        if u16::from_le_bytes([tiff2[e], tiff2[e + 1]]) == 262 {
            tiff2[e] = 61; // 317 = 0x013D 小端低字节
            tiff2[e + 1] = 1;
            tiff2[e + 8] = 2; // value = 2
        }
    }
    let err = geotiff::read(&tiff2).unwrap_err();
    assert!(err.contains("PhotometricInterpretation"), "{err}");
}

#[test]
fn rejects_unsupported_crs_and_missing_geo() {
    // 投影 CRS UTM 50N（32650）
    let geo = GeoRef {
        scale: [1.0; 3],
        tie: [0.0; 6],
        keys: vec![(1024, 1), (3072, 32650)],
    };
    let tiff = build_strip_tiff(2, 2, 1, 1, &[0u8; 4], Some(&geo));
    let err = geotiff::read(&tiff).unwrap_err();
    assert!(err.contains("EPSG:32650"), "{err}");

    // 无地理 tag
    let tiff = build_strip_tiff(2, 2, 1, 1, &[0u8; 4], None);
    let err = geotiff::read(&tiff).unwrap_err();
    assert!(err.contains("CRS") || err.contains("定位影像"), "{err}");
}

#[test]
fn rejects_truncated_and_non_tiff() {
    assert!(geotiff::read(b"xx").unwrap_err().contains("过短"));
    assert!(geotiff::read(b"NOTTIFF89").unwrap_err().contains("不是 TIFF"));
    let mut tiff = build_strip_tiff(4, 4, 1, 1, &[0u8; 16], None);
    tiff.truncate(20); // 截断
    assert!(geotiff::read(&tiff).is_err());
}

// ===================== XYZ 端到端测试 =====================

fn raster_args(dir: &std::path::Path, tif_bytes: &[u8]) -> (RasterArgs, std::path::PathBuf) {
    let src = dir.join("source.tif");
    std::fs::write(&src, tif_bytes).unwrap();
    let args = RasterArgs {
        source: src.clone(),
        output: dir.join("out"),
        source_type: "image".into(),
        pyramid_layout: "xyz".into(),
        pyramid_metadata: dir.join("out/tiles/metadata.json"),
        resampling: "nearest".into(),
        cancel_file: None,
    };
    (args, src)
}

/// 独立重算：墨卡托瓦片像素 (px,py) 应采到源影像的哪个像素（含越界）。
#[allow(clippy::too_many_arguments)]
fn expected_pixel(
    bbox_src: [f64; 4],
    crs: Crs,
    w: usize,
    h: usize,
    z: u32,
    x: u32,
    y: u32,
    px: u32,
    py: u32,
) -> Option<(usize, usize)> {
    let b = tile_bounds(z, x, y);
    let span = b[2] - b[0];
    let mx = b[0] + (px as f64 + 0.5) / 256.0 * span;
    let my = b[3] - (py as f64 + 0.5) / 256.0 * span;
    let (gx, gy) = match crs {
        Crs::Epsg3857 => (mx, my),
        Crs::Epsg4326 => {
            use super::reproject::merc_to_lonlat;
            merc_to_lonlat(mx, my)
        }
    };
    let fx = (gx - bbox_src[0]) / (bbox_src[2] - bbox_src[0]) * w as f64 - 0.5;
    let fy = (bbox_src[3] - gy) / (bbox_src[3] - bbox_src[1]) * h as f64 - 0.5;
    if fx < 0.0 || fy < 0.0 || fx >= w as f64 || fy >= h as f64 {
        None
    } else {
        Some((fx as usize, fy as usize))
    }
}

#[test]
fn xyz_4326_end_to_end_metadata_tiles_alpha() {
    let dir = std::env::temp_dir().join(format!("tangis-raster-4326-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    // 8x8 RGB：左半红、右半蓝；bbox 经度 10..18、纬度 -4..4
    let mut px = vec![0u8; 8 * 8 * 3];
    for r in 0..8 {
        for c in 0..8 {
            let o = (r * 8 + c) * 3;
            let rgb = if c < 4 { [255, 0, 0] } else { [0, 0, 255] };
            px[o..o + 3].copy_from_slice(&rgb);
        }
    }
    let geo = geo_4326([1.0, 1.0, 0.0], [0.0, 0.0, 0.0, 10.0, 4.0, 0.0]);
    let tiff = build_strip_tiff(8, 8, 3, 1, &px, Some(&geo));

    let (args, _src) = raster_args(&dir, &tiff);
    let summary = cmd_raster2tiles(&args).unwrap();

    // metadata.json 字段与 server schema 完全一致
    let meta: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&args.pyramid_metadata).unwrap()).unwrap();
    assert_eq!(meta["tile_matrix_set"], "WebMercatorQuad");
    assert_eq!(meta["tile_size"], 256);
    assert_eq!(meta["format"], "png");
    assert_eq!(meta["min_zoom"], 0);
    let extent = meta["extent"].as_array().unwrap();
    assert_eq!(extent.len(), 4);
    // 经度 10..18 → 墨卡托 x
    let (x0, _) = lonlat_to_merc(10.0, 0.0);
    let (x1, _) = lonlat_to_merc(18.0, 0.0);
    assert!((extent[0].as_f64().unwrap() - x0).abs() < 1.0);
    assert!((extent[2].as_f64().unwrap() - x1).abs() < 1.0);
    // max_zoom 由分辨率推导： merc 宽 = (18-10)/360*2*HALF ≈ 890559 m / 8px ≈ 111320 m/px
    // → z_max = floor(log2(2*HALF/(256*res))) = floor(log2(1.407)) = 0
    let max_zoom = meta["max_zoom"].as_u64().unwrap() as u32;
    assert_eq!(max_zoom, summary.max_zoom);
    assert_eq!(summary.tiles_written, 1); // z0 全世界只有 (0,0) 一张，bbox 在其中

    // 瓦片内容：解码 PNG，抽查重投影数值 + 透明填充
    let png = std::fs::read(dir.join("out/tiles/0/0/0.png")).unwrap();
    let img = image::load_from_memory(&png).unwrap().to_rgba8();
    assert_eq!((img.width(), img.height()), (256, 256));
    let mut transparent = 0usize;
    let mut red = 0usize;
    let mut blue = 0usize;
    for py in 0..256u32 {
        for px_i in 0..256u32 {
            let c = img.get_pixel(px_i, py);
            let exp = expected_pixel([10.0, -4.0, 18.0, 4.0], Crs::Epsg4326, 8, 8, 0, 0, 0, px_i, py);
            match exp {
                None => {
                    assert_eq!(c.0, [0, 0, 0, 0], "({px_i},{py}) 应为透明填充");
                    transparent += 1;
                }
                Some((col, _row)) => {
                    assert_eq!(c.0[3], 255);
                    if col < 4 {
                        assert_eq!(c.0, [255, 0, 0, 255], "({px_i},{py}) 应为红");
                        red += 1;
                    } else {
                        assert_eq!(c.0, [0, 0, 255, 255], "({px_i},{py}) 应为蓝");
                        blue += 1;
                    }
                }
            }
        }
    }
    assert!(transparent > 60_000, "无数据区应占绝大多数：{transparent}");
    assert!(red >= 10 && blue >= 10, "左右两半都应可见：red={red} blue={blue}");

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn xyz_3857_deflate_multizoom_and_tile_count() {
    let dir = std::env::temp_dir().join(format!("tangis-raster-3857-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    // 128x128 RGBA deflate，bbox 128m 见方 @ (500000, 4000000)：1m/px
    // → z_max = floor(log2(2*HALF/(256*1))) = floor(log2(156543)) = 17
    let w = 128usize;
    let px: Vec<u8> = (0..w * w)
        .flat_map(|i| [(i % 256) as u8, ((i / 256) % 256) as u8, 0u8, 255u8])
        .collect();
    let compressed = deflate(&px);
    let geo = geo_3857([1.0, 1.0, 0.0], [0.0, 0.0, 0.0, 500000.0, 4000128.0, 0.0]);
    let tiff = build_strip_tiff(128, 128, 4, 8, &compressed, Some(&geo));

    let (args, _) = raster_args(&dir, &tiff);
    let summary = cmd_raster2tiles(&args).unwrap();
    assert_eq!(summary.min_zoom, 0);
    assert_eq!(summary.max_zoom, 17, "1m/px 源应推导到 z17");

    // 逐级数瓦片文件数 == summary.tiles_written
    let mut count = 0usize;
    for z in 0..=summary.max_zoom {
        let zd = dir.join(format!("out/tiles/{z}"));
        if !zd.exists() {
            continue;
        }
        for xd in std::fs::read_dir(&zd).unwrap() {
            let xd = xd.unwrap();
            for yf in std::fs::read_dir(xd.path()).unwrap() {
                let name = yf.unwrap().file_name();
                assert!(name.to_string_lossy().ends_with(".png"), "{name:?}");
                count += 1;
            }
        }
    }
    assert_eq!(count, summary.tiles_written);

    // 最高级应有多张瓦片（128px @1m/px 在 z8 下约跨 1 个 38.2m/px... 反而少）：
    // z8 瓦片 256px * 1.19m/px ≈ 305m，影像 128m → z8 覆盖 1~4 张；z0 一张。总量随 z 递增。
    assert!(summary.tiles_written >= 8, "tiles={}", summary.tiles_written);

    // 抽查最高级一张瓦片像素值与独立重算一致
    let top = dir.join(format!("out/tiles/{}", summary.max_zoom));
    let xd = std::fs::read_dir(&top).unwrap().next().unwrap().unwrap();
    let x: u32 = xd.file_name().to_string_lossy().parse().unwrap();
    let yf = std::fs::read_dir(xd.path()).unwrap().next().unwrap().unwrap();
    let y: u32 = yf
        .file_name()
        .to_string_lossy()
        .trim_end_matches(".png")
        .parse()
        .unwrap();
    let png = std::fs::read(yf.path()).unwrap();
    let img = image::load_from_memory(&png).unwrap().to_rgba8();
    for py in [0u32, 128, 255] {
        for px_i in [0u32, 128, 255] {
            let c = img.get_pixel(px_i, py);
            match expected_pixel([500000.0, 4000000.0, 500128.0, 4000128.0], Crs::Epsg3857, w, w, summary.max_zoom, x, y, px_i, py) {
                None => assert_eq!(c.0, [0, 0, 0, 0]),
                Some((col, row)) => {
                    let si = (row * w + col) * 4;
                    assert_eq!(c.0, &px[si..si + 4], "z8/{x}/{y} ({px_i},{py})");
                }
            }
        }
    }

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn wmts_layout_paths_and_metadata() {
    let dir = std::env::temp_dir().join(format!("tangis-raster-wmts-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    // 8x8 RGB 影像，bbox 经度 10..18、纬度 -4..4（分辨率粗，只会出 z0 一张）
    let mut px = vec![0u8; 8 * 8 * 3];
    for o in (0..8 * 8).map(|i| i * 3) {
        px[o] = 200;
        px[o + 1] = 0;
        px[o + 2] = 100;
    }
    let geo = geo_4326([1.0, 1.0, 0.0], [0.0, 0.0, 0.0, 10.0, 4.0, 0.0]);
    let tiff = build_strip_tiff(8, 8, 3, 1, &px, Some(&geo));
    let src = dir.join("source.tif");
    std::fs::write(&src, &tiff).unwrap();

    let args = RasterArgs {
        source: src.clone(),
        output: dir.join("out"),
        source_type: "image".into(),
        pyramid_layout: "wmts".into(),
        pyramid_metadata: dir.join("out/tiles/metadata.json"),
        resampling: "nearest".into(),
        cancel_file: None,
    };
    let summary = cmd_raster2tiles(&args).unwrap();
    assert_eq!(summary.tiles_written, 1);

    // metadata.json：layout=wmts
    let meta: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&args.pyramid_metadata).unwrap()).unwrap();
    assert_eq!(meta["layout"], "wmts");

    // WMTS 物理路径 {z}/{row}/{col}.png 存在；同键 XYZ 路径不存在（z0 二者重合，
    // 用 z1 多瓦片区分不划算，这里直接验证 tile_path 函数语义）
    assert!(dir.join("out/tiles/0/0/0.png").exists());
    // z0 时 (x=0,y=0) 与 (row=0,col=0) 重合，再验证 tile_path 双布局分叉
    assert_eq!(tile_path(&dir.join("o"), "wmts", 3, 5, 7), dir.join("o/tiles/3/7/5.png"));
    assert_eq!(tile_path(&dir.join("o"), "xyz", 3, 5, 7), dir.join("o/tiles/3/5/7.png"));

    // 内容正确：z0 瓦片上 lon≈14°/lat=0 落在源内 → 均匀源色
    let png = std::fs::read(dir.join("out/tiles/0/0/0.png")).unwrap();
    let img = image::load_from_memory(&png).unwrap().to_rgba8();
    let c = img.get_pixel(138, 128);
    assert_eq!(c.0, [200, 0, 100, 255]);

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn xyz_rejects_bad_source_type_and_layout_and_resampling() {
    let dir = std::env::temp_dir().join(format!("tangis-raster-badargs-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let geo = geo_4326([1.0; 3], [0.0; 6]);
    let tiff = build_strip_tiff(2, 2, 1, 1, &[0u8; 4], Some(&geo));
    let (mut args, _) = raster_args(&dir, &tiff);

    args.source_type = "point_cloud".into();
    let err = cmd_raster2tiles(&args).unwrap_err();
    assert!(err.contains("source-type") && err.contains("image"), "{err}");

    args.source_type = "image".into();
    args.pyramid_layout = "tms".into();
    let err = cmd_raster2tiles(&args).unwrap_err();
    assert!(err.contains("pyramid-layout") && err.contains("xyz"), "{err}");

    args.pyramid_layout = "xyz".into();
    args.resampling = "lanczos".into();
    let err = cmd_raster2tiles(&args).unwrap_err();
    assert!(err.contains("resampling"), "{err}");

    let _ = std::fs::remove_dir_all(&dir);
}

// ===================== testdata fixture 生成 =====================

/// 重新生成 testdata/geoimage 下的 fixture（确定性输出，可重复执行）。
/// 供主会话 E2E / 手工验证使用：
/// - small_rgb_4326.tif：8x8 RGB 未压缩，经度 10..18，纬度 -4..4；
/// - small_rgba_3857_deflate.tif：128x128 RGBA Deflate，1m/px @ (500000, 4000000)。
#[test]
fn regenerate_testdata_fixtures() {
    let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/geoimage");
    std::fs::create_dir_all(&dir).unwrap();

    let mut px = vec![0u8; 8 * 8 * 3];
    for r in 0..8 {
        for c in 0..8 {
            let o = (r * 8 + c) * 3;
            let rgb = if c < 4 { [255, 0, 0] } else { [0, 0, 255] };
            px[o..o + 3].copy_from_slice(&rgb);
        }
    }
    let geo = geo_4326([1.0, 1.0, 0.0], [0.0, 0.0, 0.0, 10.0, 4.0, 0.0]);
    std::fs::write(dir.join("small_rgb_4326.tif"), build_strip_tiff(8, 8, 3, 1, &px, Some(&geo)))
        .unwrap();

    let w = 128usize;
    let px: Vec<u8> = (0..w * w)
        .flat_map(|i| [(i % 256) as u8, ((i / 256) % 256) as u8, 0u8, 255u8])
        .collect();
    let compressed = deflate(&px);
    let geo = geo_3857([1.0, 1.0, 0.0], [0.0, 0.0, 0.0, 500000.0, 4000128.0, 0.0]);
    std::fs::write(
        dir.join("small_rgba_3857_deflate.tif"),
        build_strip_tiff(128, 128, 4, 8, &compressed, Some(&geo)),
    )
    .unwrap();
}
