/**
 * WMTS GetCapabilities 轻量解析（纯函数，正则提取，不引入 XML 依赖）。
 *
 * 后端 GetCapabilities 由任务金字塔元数据生成：
 *   - Layer 的 Identifier 即任务 ID；
 *   - TileMatrixSet 为 WebMercatorQuad；
 *   - WGS84BoundingBox 为 "经度 纬度"（CRS84 轴序）。
 */

export interface WmtsLayerInfo {
  /** 图层 WGS84 包围盒（度） */
  bbox?: { west: number; south: number; east: number; north: number }
  /** 金字塔可用最大层级（TileMatrixLimits 中的最大 TileMatrix） */
  maxLevel?: number
}

/** 在 GetCapabilities XML 中定位指定图层并提取包围盒 / 最大层级；未找到返回空对象。 */
export function parseCapabilitiesLayer(xml: string, layerId: string): WmtsLayerInfo {
  const blocks = xml.split(/<Layer[\s>]/).slice(1)
  for (const block of blocks) {
    const end = block.indexOf('</Layer>')
    const body = end >= 0 ? block.slice(0, end) : block
    const id = body.match(/<ows:Identifier>([^<]+)<\/ows:Identifier>/)?.[1]
    if (id !== layerId) continue

    const info: WmtsLayerInfo = {}
    const lower = body.match(/<ows:LowerCorner>([^<]+)<\/ows:LowerCorner>/)?.[1]
    const upper = body.match(/<ows:UpperCorner>([^<]+)<\/ows:UpperCorner>/)?.[1]
    if (lower != null && upper != null) {
      const [w, s] = lower.trim().split(/\s+/).map(Number)
      const [e, n] = upper.trim().split(/\s+/).map(Number)
      if ([w, s, e, n].every((v) => Number.isFinite(v))) {
        info.bbox = { west: w, south: s, east: e, north: n }
      }
    }
    const levels = [...body.matchAll(/<TileMatrix>(\d+)<\/TileMatrix>/g)].map((m) => Number(m[1]))
    if (levels.length > 0) info.maxLevel = Math.max(...levels)
    return info
  }
  return {}
}

/**
 * 把后端 wmts_url（GetCapabilities KVP 入口，已含 expires/sig 防盗链参数）
 * 改写为可交给 Cesium WebMapTileServiceImageryProvider 的 KVP base URL：
 * 保留签名相关 query，剥离 GetCapabilities 专用参数（service/version/request，
 * 由 Cesium 以 GetTile 语义自行补充）。
 */
export function wmtsKvpBaseUrl(capabilitiesUrl: string): string {
  const q = capabilitiesUrl.indexOf('?')
  if (q < 0) return capabilitiesUrl
  const base = capabilitiesUrl.slice(0, q)
  const params = new URLSearchParams(capabilitiesUrl.slice(q + 1))
  for (const k of ['service', 'version', 'request']) params.delete(k)
  const qs = params.toString()
  return qs ? `${base}?${qs}` : base
}
