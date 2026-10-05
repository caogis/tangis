/**
 * 能力矩阵：TanGIS × GISBox 功能对照（界面展示唯一事实来源）
 *
 * 用于工作台/设置/各功能页统一标注每项能力的落地状态，避免各处口径不一致。
 * 状态口径：
 *   - ready   已可用（有真实实现与界面入口）
 *   - partial 部分可用（后端已实现但界面/协议不完整，或需外部依赖）
 *   - planned 规划中（尚未实现，界面明确标注）
 */
export type CapStatus = 'ready' | 'partial' | 'planned'

export interface Capability {
  /** 所属模块（对齐 GISBox：数据导入 / 切片转换 / 服务分发 / 质检与编辑 / 平台能力） */
  module: string
  name: string
  status: CapStatus
  /** 状态说明：为什么是这个状态 / 依赖项 */
  note: string
}

export const STATUS_LABEL: Record<CapStatus, string> = {
  ready: '可用',
  partial: '部分可用',
  planned: '规划中',
}

export const CAPABILITIES: Capability[] = [
  // ---- 数据导入 ----
  { module: '数据导入', name: 'OSGB 倾斜摄影', status: 'ready', note: '真实解析器，支持 LOD/PagedLOD/内嵌纹理' },
  { module: '数据导入', name: 'OBJ 模型', status: 'ready', note: 'tobj 解析，含材质纹理' },
  { module: '数据导入', name: 'GeoTIFF 影像/DEM', status: 'ready', note: '自写 TIFF 解析器，支持 GeoTIFF 标签' },
  { module: '数据导入', name: 'GLTF / FBX 模型', status: 'planned', note: '待接入格式解析（内核插件位已留）' },
  { module: '数据导入', name: 'GeoJSON 矢量', status: 'ready', note: '导入即注册图层，直接提供 MVT 瓦片与 WFS 要素服务（无需切片任务）' },
  { module: '数据导入', name: 'SHP 矢量', status: 'ready', note: '纯 Go 解析 .shp/.dbf/.prj/.cpg；GBK 属性自动解码，环角色按包含关系判定' },
  { module: '数据导入', name: 'GeoPackage 矢量', status: 'ready', note: '复用桌面版自带的纯 Go SQLite 驱动；一个文件内的每个要素表各注册一个图层' },
  { module: '数据导入', name: '投影坐标自动换算', status: 'ready', note: '高斯克吕格/横轴墨卡托按 .prj 参数反算到 WGS84；基准不同且无 TOWGS84 时明确拒绝而非静默近似' },
  { module: '数据导入', name: '矢量导出', status: 'ready', note: 'GeoPackage / Shapefile(.zip) / GeoJSON；Shapefile 的有损处理（混合几何、字段名截断）会明确提示' },
  { module: '数据导入', name: 'LAS / LAZ 点云', status: 'partial', note: 'LAS 1.0–1.4 可切片为 3D Tiles 点云（pnts）；LAZ 暂不支持' },
  { module: '数据导入', name: 'DXF / DWG', status: 'planned', note: 'DXF 优先；DWG 走 ODA 商业授权' },

  // ---- 切片转换 ----
  { module: '切片转换', name: '倾斜摄影 → 3DTiles', status: 'ready', note: '真几何 b3dm + tileset 1.0 + ENU 地理定位' },
  { module: '切片转换', name: '影像 → 瓦片金字塔', status: 'ready', note: 'WebMercatorQuad XYZ/WMTS 布局' },
  { module: '切片转换', name: '模型 → 3DTiles', status: 'ready', note: 'OBJ 等通用模型切片' },
  { module: '切片转换', name: '地形 → Terrain', status: 'ready', note: 'DEM → Quantized-Mesh，产物含 layer.json' },
  { module: '切片转换', name: '点云 → 3DTiles', status: 'ready', note: 'LAS → pnts + tileset.json，支持地理原点定位' },
  { module: '切片转换', name: '断点续切 / 并行切片', status: 'ready', note: '分块 Manifest + journal 增量日志 + Rayon 并行' },
  { module: '切片转换', name: '模型轻量化', status: 'partial', note: 'QEM 简化可用；KTX2/DRACO、重建顶层待做' },

  // ---- 服务分发 ----
  { module: '服务分发', name: '3DTiles 服务', status: 'ready', note: '签名防盗链 + 发布审批' },
  { module: '服务分发', name: 'WMTS 1.0.0', status: 'ready', note: 'KVP + RESTful，真实 GetCapabilities' },
  { module: '服务分发', name: 'TMS', status: 'ready', note: '影像瓦片 TMS 布局' },
  { module: '服务分发', name: 'WFS 2.0（只读）', status: 'partial', note: 'PostGIS 与本地文件矢量双后端；事务与 DescribeFeatureType 待补' },
  { module: '服务分发', name: 'MVT 矢量瓦片', status: 'ready', note: 'PostGIS 与本地文件矢量双后端，含纯 Go 编码器（投影/裁剪/绕向归一化）' },
  { module: '服务分发', name: 'WMS 1.3.0', status: 'ready', note: 'GetCapabilities / GetMap；支持 3857、4326（含 1.3.0 轴序）与 CRS:84' },
  { module: '服务分发', name: 'WCS 1.0.0', status: 'ready', note: 'GetCoverage 按 bbox 镶嵌入图（image/png）' },
  { module: '服务分发', name: 'Terrain 服务', status: 'ready', note: 'layer.json + quantized-mesh，TerrainProvider 直接加载' },

  // ---- 质检与编辑 ----
  { module: '质检与编辑', name: '几何质检（5 项）', status: 'ready', note: '退化/法线翻转/悬浮/裂缝/自相交' },
  { module: '质检与编辑', name: '模型抠除 / 压平 / 对齐', status: 'partial', note: '算子已实现，按参数表单执行；可视刷选待做' },
  { module: '质检与编辑', name: 'OSGB 增量写回', status: 'planned', note: 'PRD 已收缩为试点' },
  { module: '质检与编辑', name: 'DEM 脱密 / 七参数', status: 'ready', note: '合规工具页：坐标系识别、Bursa 七参数转换、DEM 区域脱密（任务化 + 留痕）' },

  // ---- 平台能力 ----
  { module: '平台能力', name: '开放 API', status: 'ready', note: 'OpenAPI 契约 + 漂移守护测试' },
  { module: '平台能力', name: 'API Key / 配额 / 多租户', status: 'ready', note: 'SHA-256 Key、每日配额、租户隔离' },
  { module: '平台能力', name: 'Webhook', status: 'partial', note: '终态事件已投递；事件类型与订阅管理待扩展' },
  { module: '平台能力', name: '.t3d 打包 / 解包', status: 'ready', note: '公开容器格式，含 Zip Slip 与哈希校验' },
  { module: '平台能力', name: '3D 预览（Cesium）', status: 'ready', note: '加载 + 场景树 + 属性查询 + 距离/面积/高程测量' },
  { module: '平台能力', name: '分布式切片集群', status: 'planned', note: 'License 特性位已预留' },
  { module: '平台能力', name: '实时协同 / 插件 / AI', status: 'planned', note: '平台路线已明确延后' },
]

/** 按模块分组（保持 CAPABILITIES 中的出现顺序） */
export function groupByModule(): { module: string; items: Capability[] }[] {
  const out: { module: string; items: Capability[] }[] = []
  for (const c of CAPABILITIES) {
    const last = out[out.length - 1]
    if (last && last.module === c.module) last.items.push(c)
    else out.push({ module: c.module, items: [c] })
  }
  return out
}

export function countByStatus(): Record<CapStatus, number> {
  const acc: Record<CapStatus, number> = { ready: 0, partial: 0, planned: 0 }
  for (const c of CAPABILITIES) acc[c.status]++
  return acc
}
