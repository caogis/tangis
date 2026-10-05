/**
 * 二维矢量编辑的画布投影与命中判定（纯函数，便于单独推敲）。
 *
 * 与预览缩略图不同，编辑要平移缩放、要在屏幕像素与地理坐标间**双向**换算、
 * 要做命中判定，所以统一在 Web Mercator 米空间里算：缩略图那种"按范围自适应
 * 的等距圆柱"在跨大范围时会明显变形，拿来编辑会让用户点不准。
 */

/** 墨卡托全幅半宽（米） */
const WORLD = 20037508.342789244
/** 墨卡托纬度上界 */
const LAT_LIMIT = 85.05112877980659

export interface Merc {
  x: number
  y: number
}

/** 视口：中心（墨卡托米）+ 每像素米数 */
export interface Viewport {
  cx: number
  cy: number
  mpp: number
}

/** 经纬度 → 墨卡托米 */
export function lonLatToMerc(lon: number, lat: number): Merc {
  const safeLat = Math.max(-LAT_LIMIT, Math.min(LAT_LIMIT, lat))
  return {
    x: (lon * WORLD) / 180,
    y: (Math.log(Math.tan(((90 + safeLat) * Math.PI) / 360)) * WORLD) / Math.PI,
  }
}

/** 墨卡托米 → 经纬度 */
export function mercToLonLat(m: Merc): [number, number] {
  return [
    (m.x / WORLD) * 180,
    (Math.atan(Math.exp((m.y / WORLD) * Math.PI)) * 360) / Math.PI - 90,
  ]
}

/** 墨卡托坐标 → 屏幕像素 */
export function projectPoint(m: Merc, vp: Viewport, w: number, h: number): { x: number; y: number } {
  return { x: (m.x - vp.cx) / vp.mpp + w / 2, y: h / 2 - (m.y - vp.cy) / vp.mpp }
}

/** 屏幕像素 → 墨卡托坐标 */
export function unprojectPoint(sx: number, sy: number, vp: Viewport, w: number, h: number): Merc {
  return { x: vp.cx + (sx - w / 2) * vp.mpp, y: vp.cy - (sy - h / 2) * vp.mpp }
}

/** 按 WGS84 范围适配视口（四周留边） */
export function fitViewport(
  bbox: [number, number, number, number],
  w: number,
  h: number,
  pad = 48,
): Viewport {
  const sw = lonLatToMerc(bbox[0], bbox[1])
  const ne = lonLatToMerc(bbox[2], bbox[3])
  const cx = (sw.x + ne.x) / 2
  const cy = (sw.y + ne.y) / 2
  const spanX = Math.max(Math.abs(ne.x - sw.x), 1)
  const spanY = Math.max(Math.abs(ne.y - sw.y), 1)
  const mpp = Math.max(spanX / Math.max(w - pad * 2, 10), spanY / Math.max(h - pad * 2, 10))
  return { cx, cy, mpp }
}

/**
 * 围绕某个屏幕点缩放（该点下的地理坐标保持不动）。
 * factor > 1 放大。
 */
export function zoomAt(
  vp: Viewport,
  factor: number,
  sx: number,
  sy: number,
  w: number,
  h: number,
): Viewport {
  const before = unprojectPoint(sx, sy, vp, w, h)
  const mpp = clamp(vp.mpp / factor, 0.005, WORLD / 128)
  const next: Viewport = { cx: vp.cx, cy: vp.cy, mpp }
  const after = unprojectPoint(sx, sy, next, w, h)
  return { cx: next.cx + (before.x - after.x), cy: next.cy + (before.y - after.y), mpp }
}

/** 平移（按屏幕像素位移换算成墨卡托位移） */
export function panBy(vp: Viewport, dxPx: number, dyPx: number): Viewport {
  return { cx: vp.cx - dxPx * vp.mpp, cy: vp.cy + dyPx * vp.mpp, mpp: vp.mpp }
}

/** 网格间距：让屏幕上的格子不小于 targetPx 像素，且落在 1/2/5×10ⁿ 上 */
export function gridStep(mpp: number, targetPx = 100): number {
  const raw = mpp * targetPx
  const pow = Math.pow(10, Math.floor(Math.log10(Math.max(raw, 1e-9))))
  for (const f of [1, 2, 5, 10]) {
    if (raw <= pow * f) return pow * f
  }
  return pow * 10
}

/** 视口可见范围（WGS84），用于裁剪绘制 */
export function visibleBBox(vp: Viewport, w: number, h: number): [number, number, number, number] {
  const sw = mercToLonLat(unprojectPoint(0, h, vp, w, h))
  const ne = mercToLonLat(unprojectPoint(w, 0, vp, w, h))
  return [sw[0], sw[1], ne[0], ne[1]]
}

/** 点到线段的距离（屏幕像素） */
export function distToSegment(
  px: number,
  py: number,
  x1: number,
  y1: number,
  x2: number,
  y2: number,
): number {
  const dx = x2 - x1
  const dy = y2 - y1
  const lenSq = dx * dx + dy * dy
  if (lenSq === 0) return Math.hypot(px - x1, py - y1)
  let t = ((px - x1) * dx + (py - y1) * dy) / lenSq
  t = Math.max(0, Math.min(1, t))
  return Math.hypot(px - (x1 + t * dx), py - (y1 + t * dy))
}

/* ---------------- 吸附（顶点优先于边） ---------------- */

/** 线段上离给定点最近的点 */
export function closestOnSegment(
  px: number,
  py: number,
  x1: number,
  y1: number,
  x2: number,
  y2: number,
): { x: number; y: number; t: number; dist: number } {
  const dx = x2 - x1
  const dy = y2 - y1
  const lenSq = dx * dx + dy * dy
  let t = lenSq === 0 ? 0 : ((px - x1) * dx + (py - y1) * dy) / lenSq
  t = Math.max(0, Math.min(1, t))
  const x = x1 + t * dx
  const y = y1 + t * dy
  return { x, y, t, dist: Math.hypot(px - x, py - y) }
}

/**
 * 吸附顶点候选：同时带屏幕坐标与**经纬度**。
 *
 * 为什么要带经纬度：顶点吸附必须给出目标顶点的**原始经纬度**，而不是
 * 「屏幕坐标再反算回去」——后者会引入浮点往返误差，相邻面之间会留下
 * 看不出来但真实存在的缝隙（等同没吸附）。
 */
export interface SnapVertex {
  x: number
  y: number
  lon: number
  lat: number
}

/** 吸附边候选（屏幕坐标 + 两端经纬度，命中时按 t 插值） */
export interface SnapEdge {
  x1: number
  y1: number
  x2: number
  y2: number
  lon1: number
  lat1: number
  lon2: number
  lat2: number
}

/** 吸附候选集 */
export interface SnapTargets {
  /** 顶点：优先吸附——顶到顶才能让相邻要素真正严丝合缝 */
  vertices: SnapVertex[]
  /** 边：次优先——顶到边，用于把点贴到别人的边上（如分界线中点） */
  edges: SnapEdge[]
}

/** 吸附结果：屏幕坐标 + 吸附后的经纬度 */
export interface SnapResult {
  kind: 'vertex' | 'edge'
  x: number
  y: number
  lon: number
  lat: number
}

/**
 * 在屏幕空间寻找吸附点（阈值也是屏幕像素，缩放后手感一致）。
 *
 * 优先级：半径内的**顶点**优先于边——顶点对顶点才能让相邻要素严丝合缝，
 * 边吸附只保证落在线上（可能落在别人边的中间，接缝仍是错开的）。
 */
export function findSnap(
  sx: number,
  sy: number,
  targets: SnapTargets,
  opts: { vertexRadius?: number; edgeRadius?: number } = {},
): SnapResult | null {
  const vertexRadius = opts.vertexRadius ?? 12
  const edgeRadius = opts.edgeRadius ?? 10
  let best: SnapResult | null = null
  let bestDist = Infinity
  for (const v of targets.vertices) {
    const d = Math.hypot(v.x - sx, v.y - sy)
    if (d <= vertexRadius && d < bestDist) {
      bestDist = d
      best = { kind: 'vertex', x: v.x, y: v.y, lon: v.lon, lat: v.lat }
    }
  }
  if (best) return best
  for (const e of targets.edges) {
    const c = closestOnSegment(sx, sy, e.x1, e.y1, e.x2, e.y2)
    if (c.dist <= edgeRadius && c.dist < bestDist) {
      bestDist = c.dist
      best = {
        kind: 'edge',
        x: c.x,
        y: c.y,
        lon: e.lon1 + (e.lon2 - e.lon1) * c.t,
        lat: e.lat1 + (e.lat2 - e.lat1) * c.t,
      }
    }
  }
  return best
}

/** 射线法：点是否在环内（屏幕坐标，环可闭合也可不闭合） */
export function pointInRing(px: number, py: number, ring: number[][]): boolean {
  let inside = false
  for (let i = 0, j = ring.length - 1; i < ring.length; j = i++) {
    const xi = ring[i][0]
    const yi = ring[i][1]
    const xj = ring[j][0]
    const yj = ring[j][1]
    if (yi > py !== yj > py) {
      const x = ((xj - xi) * (py - yi)) / (yj - yi) + xi
      if (px < x) inside = !inside
    }
  }
  return inside
}

/** 环的有向面积（屏幕坐标）：用于判断多边形环方向 */
export function ringArea(ring: number[][]): number {
  let sum = 0
  for (let i = 0; i < ring.length; i++) {
    const a = ring[i]
    const b = ring[(i + 1) % ring.length]
    sum += a[0] * b[1] - b[0] * a[1]
  }
  return sum / 2
}

function clamp(v: number, lo: number, hi: number): number {
  return Math.max(lo, Math.min(hi, v))
}
