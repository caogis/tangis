//! TanGIS 合规算子 crate（F-21 kernel 侧，纯 Rust，无 GDAL C 依赖）。
//!
//! 模块：
//! - [`crs`]：坐标系识别（GeoTIFF GeoKey / ESRI WKT .prj）→ 结构化 [`crs::CrsInfo`]；
//! - [`bursa`]：CGCS2000 七参数（Bursa-Wolf，Position Vector 约定）坐标转换
//!   （BLH ↔ XYZ + 七参数空间直角坐标转换）；
//! - [`dem`]：DEM 区域脱密（flatten / noise），输出 GeoTIFF + 脱密记录 JSON；
//! - [`tiff`]：内部最小 TIFF/GeoTIFF 读写器（无压缩，单页），供 crs/dem 复用。
//!
//! 合规原则（测绘合规 P0）：
//! - **禁止静默降级**：CRS 识别失败、参数缺失/非法、格式不支持时一律返回
//!   带原因的错误，绝不猜测、绝不伪造默认坐标系；
//! - 脱密操作产出可复现（seed 参数化）并生成审批留痕记录（前后统计）。

pub mod bursa;
pub mod crs;
pub mod dem;
pub mod tiff;
