/**
 * 3D Tiles tileset.json 子树统计（纯函数）。
 * 只统计 tileset.json 中真实存在的字段，不做任何估算（真实数据原则）。
 */

export interface TilesetStats {
  /** 子树节点总数（含无内容的中间节点） */
  totalNodes: number
  /** 携带内容（content / contents）的节点数，如 b3dm 瓦片 */
  contentNodes: number
  /** 子树最大深度（根节点记 1） */
  maxDepth: number
}

/** 遍历 root 起的子树并统计节点数 / 内容节点数 / 最大深度。 */
export function computeTilesetStats(root: unknown): TilesetStats {
  const stats: TilesetStats = { totalNodes: 0, contentNodes: 0, maxDepth: 0 }
  const visit = (node: unknown, depth: number): void => {
    if (node == null || typeof node !== 'object') return
    stats.totalNodes += 1
    if (depth > stats.maxDepth) stats.maxDepth = depth
    const n = node as { content?: unknown; contents?: unknown[]; children?: unknown[] }
    if (n.content != null || (Array.isArray(n.contents) && n.contents.length > 0)) {
      stats.contentNodes += 1
    }
    if (Array.isArray(n.children)) {
      for (const child of n.children) visit(child, depth + 1)
    }
  }
  visit(root, 1)
  return stats
}

/** 从 tileset.json 响应体中取 root 节点（兼容 1.0 顶层 root 与直接传子树）。 */
export function tilesetRootOf(json: unknown): unknown {
  if (json != null && typeof json === 'object' && 'root' in (json as Record<string, unknown>)) {
    return (json as { root?: unknown }).root
  }
  return json
}

/* ------------------------------------------------------------------ */
/* 场景树（F-05）：tileset.json → 可展示的层级树                        */
/* ------------------------------------------------------------------ */

export interface TreeNode {
  /** 展示名：优先 content.uri 文件名，其次瓦片序号 */
  name: string
  /** content.uri（3D Tiles 1.0）或 contents[0].uri（1.1），可能为空 */
  uri: string
  depth: number
  geometricError: number
  /** 子节点数（用于折叠提示） */
  children: TreeNode[]
  /** 包围盒（原样保留，定位时由 Cesium 侧解算） */
  boundingVolume: unknown
}

interface RawNode {
  content?: { uri?: unknown; url?: unknown }
  contents?: { uri?: unknown; url?: unknown }[]
  children?: unknown[]
  geometricError?: unknown
  boundingVolume?: unknown
}

function uriOf(node: RawNode): string {
  const fromContent = node.content?.uri ?? node.content?.url
  if (typeof fromContent === 'string' && fromContent.length > 0) return fromContent
  const first = Array.isArray(node.contents) ? node.contents[0] : undefined
  const fromContents = first?.uri ?? first?.url
  return typeof fromContents === 'string' ? fromContents : ''
}

/** 取 URI 的文件名部分（去掉目录与 query），作为树节点展示名 */
export function tileDisplayName(uri: string, fallback: string): string {
  if (!uri) return fallback
  const noQuery = uri.split('?')[0] ?? uri
  const parts = noQuery.split('/')
  const last = parts[parts.length - 1]
  return last && last.length > 0 ? last : fallback
}

/**
 * 构建场景树（深度优先，按 tileset.json 中的顺序）。
 * 不读取任何运行时状态：树结构完全来自 tileset.json（真实数据原则）。
 */
export function buildSceneTree(root: unknown): TreeNode | null {
  let counter = 0
  const visit = (raw: unknown, depth: number): TreeNode | null => {
    if (raw == null || typeof raw !== 'object') return null
    const n = raw as RawNode
    counter += 1
    const uri = uriOf(n)
    const children: TreeNode[] = []
    if (Array.isArray(n.children)) {
      for (const c of n.children) {
        const node = visit(c, depth + 1)
        if (node) children.push(node)
      }
    }
    const ge = typeof n.geometricError === 'number' && Number.isFinite(n.geometricError) ? n.geometricError : 0
    return {
      name: tileDisplayName(uri, depth === 0 ? 'root' : `节点 ${counter}`),
      uri,
      depth,
      geometricError: ge,
      children,
      boundingVolume: n.boundingVolume,
    }
  }
  return visit(root, 0)
}

/** 统计树的节点总数与最大深度（面板头部展示） */
export function treeStats(root: TreeNode | null): { nodes: number; maxDepth: number } {
  let nodes = 0
  let maxDepth = 0
  const walk = (n: TreeNode | null): void => {
    if (!n) return
    nodes += 1
    if (n.depth + 1 > maxDepth) maxDepth = n.depth + 1
    for (const c of n.children) walk(c)
  }
  walk(root)
  return { nodes, maxDepth }
}

/**
 * 解算包围盒中心（用于场景树节点定位）。
 * 支持 region / box / sphere 三种形态；解不出来返回 null（不猜测位置）。
 *
 * 注意：box 与 sphere 定义在 tile 局部坐标系，若 tileset 根节点带 transform
 * 需先按 4x4 列主序矩阵变换，否则定位会偏。这里接受可选的 Cesium 计算回调，
 * 由调用方传入以避免 utils 层直接依赖 Cesium（保持纯函数可测）。
 */
export interface VolumeCenter {
  lon: number
  lat: number
  height: number
  /** 包围盒半径（米），用于相机距离；无法估算时为 0 */
  radius: number
}

export interface EcefConvert {
  /** 局部/ECEF 笛卡尔坐标 → 经纬高（度/米） */
  cartesianToDegrees(x: number, y: number, z: number): { lon: number; lat: number; height: number } | null
  /** 若根节点存在 transform，用它把局部坐标变换到 ECEF；无则原样返回 */
  applyTransform?(x: number, y: number, z: number): [number, number, number]
}

const R2D = 180 / Math.PI

export function volumeCenter(volume: unknown, conv: EcefConvert): VolumeCenter | null {
  if (volume == null || typeof volume !== 'object') return null
  const v = volume as Record<string, unknown>

  // region: [west, south, east, north, minHeight, maxHeight]（弧度 / 米）
  const region = v.region
  if (Array.isArray(region) && region.length >= 6) {
    const nums = region.map(Number)
    if (nums.every((n) => Number.isFinite(n))) {
      const lon = ((nums[0]! + nums[2]!) / 2) * R2D
      const lat = ((nums[1]! + nums[3]!) / 2) * R2D
      const minH = Math.min(nums[4]!, nums[5]!)
      const maxH = Math.max(nums[4]!, nums[5]!)
      // 半径按经纬度跨度估算（米），粗略但足以定位
      const rLat = Math.abs(nums[3]! - nums[1]!) * R2D * 111_320
      const rLon = Math.abs(nums[2]! - nums[0]!) * R2D * 111_320 * Math.cos(((nums[1]! + nums[3]!) / 2))
      const radius = Math.max(Math.max(rLat, rLon) / 2, (maxH - minH) / 2, 50)
      return { lon, lat, height: (minH + maxH) / 2, radius }
    }
  }

  // box: [cx, cy, cz, xx, xy, xz, yx, yy, yz, zx, zy, zz]
  const box = v.box
  if (Array.isArray(box) && box.length >= 12 && box.every((n) => Number.isFinite(Number(n)))) {
    const b = box.map(Number)
    let [cx, cy, cz] = [b[0]!, b[1]!, b[2]!]
    if (conv.applyTransform) [cx, cy, cz] = conv.applyTransform(cx, cy, cz)
    const p = conv.cartesianToDegrees(cx, cy, cz)
    if (!p) return null
    const hx = Math.hypot(b[3]!, b[4]!, b[5]!)
    const hy = Math.hypot(b[6]!, b[7]!, b[8]!)
    const hz = Math.hypot(b[9]!, b[10]!, b[11]!)
    return { lon: p.lon, lat: p.lat, height: p.height, radius: Math.max(hx, hy, hz, 50) }
  }

  // sphere: [cx, cy, cz, radius]
  const sphere = v.sphere
  if (Array.isArray(sphere) && sphere.length >= 4 && sphere.every((n) => Number.isFinite(Number(n)))) {
    const s = sphere.map(Number)
    let [cx, cy, cz] = [s[0]!, s[1]!, s[2]!]
    if (conv.applyTransform) [cx, cy, cz] = conv.applyTransform(cx, cy, cz)
    const p = conv.cartesianToDegrees(cx, cy, cz)
    if (!p) return null
    return { lon: p.lon, lat: p.lat, height: p.height, radius: Math.max(s[3]!, 50) }
  }

  return null
}
