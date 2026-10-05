//! 影像切片（GeoTIFF → XYZ 瓦片金字塔）最小实现。
//!
//! - [`geotiff`]：纯 Rust 的最小 GeoTIFF 读取（无 GDAL C 依赖）；
//! - [`reproject`]：EPSG:4326 / EPSG:3857 → WebMercatorQuad 的重投影数值；
//! - [`xyz`]：XYZ 瓦片金字塔生产 + metadata.json。

pub mod geotiff;
pub mod reproject;
pub mod xyz;

#[cfg(test)]
mod tests;
