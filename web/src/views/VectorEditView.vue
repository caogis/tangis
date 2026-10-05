<script setup lang="ts">
/**
 * 矢量编辑：画布 + 工具栏 + 属性面板。
 *
 * 数据流：一次性拉**全量**要素（复用导出接口，不受 WFS count 上限约束）
 * → 本地工作副本 → 编辑（不可变替换，撤销只需留一份数组快照）
 * → 保存时与初始快照做差集，得到 create/update/delete 三类操作。
 *
 * 只提交**真正变化**的要素：未改动的要素不进请求体，避免"编辑一个点、回写一万条"。
 * 服务端首次编辑前会自动备份源文件（路径在结果里回显）。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  applyVectorLayerEdits,
  fetchVectorLayerGeoJSON,
  vectorLayerMetadata,
} from '../api/client'
import type { VectorEditOp } from '../api/client'
import type { GeoJsonFeature } from '../api/types'
import AppIcon from '../components/AppIcon.vue'
import type { IconName } from '../components/icon-paths'
import { useToast } from '../composables/useToast'
import {
  closestOnSegment,
  distToSegment,
  findSnap,
  fitViewport,
  gridStep,
  lonLatToMerc,
  mercToLonLat,
  panBy,
  pointInRing,
  projectPoint,
  unprojectPoint,
  zoomAt,
  type SnapResult,
  type SnapTargets,
  type Viewport,
} from '../utils/geoview'

const route = useRoute()
const router = useRouter()
const toast = useToast()

type Kind = 'point' | 'line' | 'polygon'
type Tool = 'pan' | 'select' | 'insert' | 'remove' | 'point' | 'line' | 'polygon'

/** 工作副本里的一个要素。几何按绘制需要拆成 parts：
 *  点：[[点]]；线：[[折线…]]；面：[[环…]…]（环为经纬度点数组）。 */
interface Feature {
  /** 本地唯一键（新要素为负数） */
  key: number
  /** 源要素 ID；0 表示新建 */
  originId: number
  kind: Kind
  parts: number[][][]
  /** 面：每个多边形的环下标分组（保留 MultiPolygon 结构，写回时不丢分组） */
  groups?: number[][]
  props: Record<string, unknown>
}

const TOOLS: { id: Tool; label: string; icon: IconName; hint: string; lineOnly?: boolean }[] = [
  { id: 'pan', label: '平移', icon: 'globe', hint: '拖拽平移画布；滚轮缩放' },
  {
    id: 'select',
    label: '选择',
    icon: 'search',
    hint: '点选要素；拖动顶点改形状；拖动内部整体移动；Delete 删除',
  },
  {
    id: 'insert',
    label: '加点',
    icon: 'import',
    hint: '在选中要素的边上单击插入顶点（鼠标靠近边时会出现待插入的空心方块）',
    lineOnly: true,
  },
  {
    id: 'remove',
    label: '删点',
    icon: 'trash',
    hint: '单击选中要素的顶点将其删除（面环至少保留 3 点、折线至少 2 点）',
    lineOnly: true,
  },
  { id: 'point', label: '画点', icon: 'dashboard', hint: '单击落点' },
  { id: 'line', label: '画线', icon: 'convert', hint: '依次单击加点，双击 / 回车结束，Esc 取消' },
  { id: 'polygon', label: '画面', icon: 'layers', hint: '依次单击加点，双击 / 回车闭合，Esc 取消' },
]

const layerName = computed(() => String(route.params.name ?? ''))
const canvasRef = ref<HTMLCanvasElement | null>(null)
const loading = ref(true)
const loadError = ref('')
const saving = ref(false)

const features = ref<Feature[]>([])
const tool = ref<Tool>('select')
const selectedKey = ref<number | null>(null)
const dirty = ref(false)

/** 初始快照：originId → 几何+属性的指纹，用于算差集与统计待保存数量 */
const initialSnapshot = ref<Map<number, string>>(new Map())
const initialIds = ref<number[]>([])

/** 撤销栈：每次改动前压入一份数组快照（要素对象不可变，浅拷贝即可） */
const history = ref<Feature[][]>([])
const nextKey = ref(-1)

const geomKind = ref<Kind>('polygon')
const geomKindLabel = computed(() => ({ point: '点', line: '线', polygon: '面' })[geomKind.value])

/** 点图层没有顶点/边可编辑，加点与删点工具直接不显示（而不是点不动） */
const visibleTools = computed(() => TOOLS.filter((t) => !t.lineOnly || geomKind.value !== 'point'))

/* ---------------- 画布状态 ---------------- */

const vp = ref<Viewport>({ cx: 0, cy: 0, mpp: 100 })
const canvasSize = ref({ w: 800, h: 520 })
const cursorLL = ref<[number, number] | null>(null)
/** 正在绘制中的草稿（经纬度点序列） */
const draft = ref<number[][]>([])
const draftCursor = ref<[number, number] | null>(null)
/** 拖拽状态：sx/sy 为上一次的屏幕位置；dx/dy 为「抓住顶点时光标相对顶点的偏移」 */
type DragMode = 'none' | 'pan' | 'vertex' | 'feature'
const drag = ref<{
  mode: DragMode
  sx: number
  sy: number
  part: number
  idx: number
  dx: number
  dy: number
}>({ mode: 'none', sx: 0, sy: 0, part: 0, idx: 0, dx: 0, dy: 0 })

/* ---------------- 吸附 ---------------- */

/** 吸附总开关与两类目标开关（顶点对顶点、顶点贴边） */
const snapOn = ref(true)
const snapVertexOn = ref(true)
const snapEdgeOn = ref(true)
/** 当前生效的吸附点（屏幕坐标），用于画吸附指示器 */
const snapMark = ref<SnapResult | null>(null)
/** 删点模式下悬停的顶点 [部分下标, 顶点下标] */
const hoverVertex = ref<[number, number] | null>(null)
/** 加点模式下悬停的边：[部分下标, 边起点下标, 边上最近点屏幕坐标] */
const hoverEdge = ref<{ part: number; seg: number; x: number; y: number } | null>(null)

const vertexCount = computed(() => {
  const f = selected.value
  if (!f) return 0
  let n = 0
  for (const part of f.parts) n += part.length
  return n
})

const hint = computed(() => {
  if (loading.value) return '正在载入要素…'
  if (loadError.value) return loadError.value
  const snapTip = snapOn.value ? ' · 已开吸附' : ''
  switch (tool.value) {
    case 'point':
      return '单击落点' + snapTip
    case 'line':
      return draft.value.length
        ? `已加 ${draft.value.length} 个点：双击结束，Esc 取消${snapTip}`
        : '依次单击加点' + snapTip
    case 'polygon':
      return draft.value.length
        ? `已加 ${draft.value.length} 个点：双击闭合，Esc 取消${snapTip}`
        : '依次单击加点' + snapTip
    case 'insert':
      return selected.value
        ? `在边上单击插入顶点（当前 ${vertexCount.value} 点）${snapTip}`
        : '先在选择工具下点选一个要素'
    case 'remove':
      return selected.value
        ? `单击顶点删除（当前 ${vertexCount.value} 点）`
        : '先在选择工具下点选一个要素'
    case 'select':
      return selectedKey.value == null
        ? '点选要素后可改形状与属性'
        : `拖动顶点改形状 · 拖动内部整体移动 · Delete 删除${snapTip}`
    default:
      return '拖拽平移 · 滚轮缩放'
  }
})

const cursorText = computed(() => {
  if (!cursorLL.value) return '—'
  const [lon, lat] = cursorLL.value
  return `${lon.toFixed(6)}, ${lat.toFixed(6)}`
})

const selected = computed(() =>
  selectedKey.value == null ? null : features.value.find((f) => f.key === selectedKey.value) ?? null,
)

const pendingCount = computed(() => {
  const cur = new Set<number>()
  let n = 0
  for (const f of features.value) {
    if (f.originId === 0) {
      n++
    } else {
      cur.add(f.originId)
      if (initialSnapshot.value.get(f.originId) !== snapshotOf(f)) n++
    }
  }
  for (const id of initialIds.value) if (!cur.has(id)) n++
  return n
})

/* ---------------- 载入与保存 ---------------- */

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    // 元数据给出图层几何类型（决定能画什么）与实际要素范围
    const md = await vectorLayerMetadata(layerName.value)
    const t = md.geometryType.toUpperCase()
    geomKind.value = t.startsWith('POINT') ? 'point' : t.startsWith('LINE') ? 'line' : 'polygon'
    tool.value = 'select'

    const fc = await fetchVectorLayerGeoJSON(layerName.value)
    const list: Feature[] = []
    const snap = new Map<number, string>()
    const ids: number[] = []
    let key = 1
    for (const f of fc.features ?? []) {
      const feat = fromGeoJSON(f, key)
      if (!feat) continue
      key++
      list.push(feat)
      if (feat.originId > 0) {
        snap.set(feat.originId, snapshotOf(feat))
        ids.push(feat.originId)
      }
    }
    features.value = list
    initialSnapshot.value = snap
    initialIds.value = ids
    history.value = []
    selectedKey.value = null
    dirty.value = false

    // 初始视图：优先按图层范围，其次按实际要素
    const bbox = (md.bboxWgs84 as [number, number, number, number] | null) ?? boundsOf(list)
    const { w, h } = canvasSize.value
    if (bbox) vp.value = fitViewport(bbox, w || 800, h || 520)
    draw()
  } catch (e) {
    loadError.value = e instanceof Error ? e.message : '载入失败'
  } finally {
    loading.value = false
  }
}

function snapshotOf(f: Feature): string {
  return JSON.stringify({ k: f.kind, p: f.parts, g: f.groups ?? null, v: f.props })
}

function toGeoJSON(f: Feature): unknown {
  if (f.kind === 'point') {
    if (f.parts.length === 1 && f.parts[0].length === 1) {
      return { type: 'Point', coordinates: f.parts[0][0] }
    }
    return { type: 'MultiPoint', coordinates: f.parts.map((p) => p[0]) }
  }
  if (f.kind === 'line') {
    return f.parts.length === 1
      ? { type: 'LineString', coordinates: f.parts[0] }
      : { type: 'MultiLineString', coordinates: f.parts }
  }
  // 面：多环但只有一个多边形时用 Polygon（首环外环、其余洞）
  if (f.parts.length === 1) return { type: 'Polygon', coordinates: f.parts }
  if (f.groups && f.groups.length > 1) {
    return { type: 'MultiPolygon', coordinates: f.groups.map((g) => g.map((i) => f.parts[i])) }
  }
  return { type: 'Polygon', coordinates: f.parts }
}

function fromGeoJSON(f: GeoJsonFeature, key: number): Feature | null {
  const g = f.geometry
  const props = (f.properties ?? {}) as Record<string, unknown>
  const originId = typeof f.id === 'number' ? f.id : Number(f.id) || 0
  if (!g || g.coordinates == null) return null
  const c = g.coordinates as number[] | number[][] | number[][][] | number[][][][]
  switch (g.type) {
    case 'Point':
      return { key, originId, kind: 'point', parts: [[c as number[]]], props }
    case 'MultiPoint':
      return { key, originId, kind: 'point', parts: (c as number[][]).map((p) => [p]), props }
    case 'LineString':
      return { key, originId, kind: 'line', parts: [c as number[][]], props }
    case 'MultiLineString':
      return { key, originId, kind: 'line', parts: c as number[][][], props }
    case 'Polygon':
      return { key, originId, kind: 'polygon', parts: c as number[][][], props }
    case 'MultiPolygon': {
      const polys = c as number[][][][]
      const parts: number[][][] = []
      const groups: number[][] = []
      for (const poly of polys) {
        const idx: number[] = []
        for (const ring of poly) {
          idx.push(parts.length)
          parts.push(ring)
        }
        groups.push(idx)
      }
      return { key, originId, kind: 'polygon', parts, groups, props }
    }
  }
  return null
}

async function save(): Promise<void> {
  if (saving.value || pendingCount.value === 0) return
  saving.value = true
  try {
    const ops: VectorEditOp[] = []
    const curIds = new Set<number>()
    for (const f of features.value) {
      if (f.originId === 0) {
        ops.push({ op: 'create', geometry: toGeoJSON(f), properties: f.props })
        continue
      }
      curIds.add(f.originId)
      if (initialSnapshot.value.get(f.originId) !== snapshotOf(f)) {
        ops.push({
          op: 'update',
          id: f.originId,
          geometry: toGeoJSON(f),
          properties: f.props,
        })
      }
    }
    for (const id of initialIds.value) {
      if (!curIds.has(id)) ops.push({ op: 'delete', id })
    }
    if (ops.length === 0) {
      toast.info('没有需要保存的变更')
      return
    }
    const res = await applyVectorLayerEdits(layerName.value, ops)
    const parts = [`新增 ${res.created}`, `修改 ${res.updated}`, `删除 ${res.deleted}`]
    toast.success(`已保存：${parts.join(' / ')}（现有 ${res.total} 个要素）`, 5000)
    if (res.backup) {
      toast.info(`源文件已备份：${res.backup}`, 8000)
    }
    for (const w of res.warnings) toast.info(w, 8000)
    dirty.value = false
    await load()
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '保存失败')
  } finally {
    saving.value = false
  }
}

/* ---------------- 编辑操作（全部走不可变更新，便于撤销） ---------------- */

function pushHistory(): void {
  history.value.push(features.value.slice())
  if (history.value.length > 60) history.value.shift()
  dirty.value = true
}

function undo(): void {
  const prev = history.value.pop()
  if (!prev) return
  features.value = prev
  dirty.value = true
  if (selectedKey.value != null && !features.value.some((f) => f.key === selectedKey.value)) {
    selectedKey.value = null
  }
  draw()
}

function replaceFeature(key: number, next: Partial<Feature>): void {
  features.value = features.value.map((f) => (f.key === key ? { ...f, ...next } : f))
}

function deleteSelected(): void {
  const f = selected.value
  if (!f) return
  pushHistory()
  features.value = features.value.filter((x) => x.key !== f.key)
  selectedKey.value = null
  draw()
}

function createFeature(kind: Kind, parts: number[][][], props: Record<string, unknown> = {}): void {
  pushHistory()
  const key = nextKey.value--
  features.value = [...features.value, { key, originId: 0, kind, parts, props }]
  selectedKey.value = key
  draw()
}

/** 属性面板改动 */
function updateProp(key: number, name: string, value: unknown): void {
  const f = features.value.find((x) => x.key === key)
  if (!f) return
  pushHistory()
  replaceFeature(key, { props: { ...f.props, [name]: value } })
}

function renameProp(key: number, from: string, to: string): void {
  const f = features.value.find((x) => x.key === key)
  if (!f || from === to) return
  pushHistory()
  const props: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(f.props)) props[k === from ? to : k] = v
  replaceFeature(key, { props })
}

function removeProp(key: number, name: string): void {
  const f = features.value.find((x) => x.key === key)
  if (!f) return
  pushHistory()
  const props: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(f.props)) if (k !== name) props[k] = v
  replaceFeature(key, { props })
}

function onPropInput(key: number, name: string, text: string): void {
  updateProp(key, name, coercePropValue(text))
}

/** 属性值类型推断：数字与 true/false 按类型存，其余按文本。 */
function coercePropValue(text: string): unknown {
  const t = text.trim()
  if (t === '') return ''
  if (t === 'true') return true
  if (t === 'false') return false
  if (/^-?\d+(\.\d+)?$/.test(t)) return Number(t)
  return text
}

function addProp(key: number): void {
  const f = features.value.find((x) => x.key === key)
  if (!f) return
  let name = 'field'
  let i = 1
  while (name in f.props) name = `field${++i}`
  updateProp(key, name, '')
}

/* ---------------- 画布绘制 ---------------- */

function resizeCanvas(): void {
  const canvas = canvasRef.value
  if (!canvas) return
  const rect = canvas.getBoundingClientRect()
  const dpr = window.devicePixelRatio || 1
  canvas.width = Math.max(1, Math.round(rect.width * dpr))
  canvas.height = Math.max(1, Math.round(rect.height * dpr))
  canvasSize.value = { w: rect.width, h: rect.height }
}

function themeColor(name: string, fallback: string): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return v || fallback
}

/** 经纬度点 → 屏幕坐标 */
function screen(part: number[], vpNow: Viewport): { x: number; y: number } {
  return projectPoint(lonLatToMerc(part[0], part[1]), vpNow, canvasSize.value.w, canvasSize.value.h)
}

function draw(): void {
  const canvas = canvasRef.value
  if (!canvas) return
  const ctx = canvas.getContext('2d')
  if (!ctx) return
  const dpr = window.devicePixelRatio || 1
  const { w, h } = canvasSize.value
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
  ctx.clearRect(0, 0, w, h)

  const line = themeColor('--primary', '#3d7f96')
  const accent = themeColor('--accent', '#c9812a')
  const okColor = themeColor('--ok', '#2f9e6f')
  const faint = themeColor('--line', '#e2e2e2')
  const vpNow = vp.value

  // 网格（经纬度整数间隔 + 米制刻度混排：直接按米画，读起来是等距的）
  const step = gridStep(vpNow.mpp)
  const sw = unprojectPoint(0, h, vpNow, w, h)
  const ne = unprojectPoint(w, 0, vpNow, w, h)
  ctx.strokeStyle = faint
  ctx.lineWidth = 1
  ctx.fillStyle = faint
  ctx.font = '10px ui-monospace, monospace'
  const x0 = Math.ceil(sw.x / step) * step
  for (let x = x0; x <= ne.x; x += step) {
    const p = projectPoint({ x, y: 0 }, vpNow, w, h)
    ctx.beginPath()
    ctx.moveTo(p.x, 0)
    ctx.lineTo(p.x, h)
    ctx.stroke()
    const ll = mercToLonLat({ x, y: 0 })[0]
    ctx.fillText(`${ll.toFixed(2)}°`, p.x + 3, 12)
  }
  const y0 = Math.ceil(sw.y / step) * step
  for (let y = y0; y <= ne.y; y += step) {
    const p = projectPoint({ x: 0, y }, vpNow, w, h)
    ctx.beginPath()
    ctx.moveTo(0, p.y)
    ctx.lineTo(w, p.y)
    ctx.stroke()
    const ll = mercToLonLat({ x: 0, y })[1]
    ctx.fillText(`${ll.toFixed(2)}°`, 3, p.y - 3)
  }

  // 要素
  for (const f of features.value) {
    const isSel = f.key === selectedKey.value
    ctx.lineWidth = isSel ? 2.2 : 1.2
    ctx.strokeStyle = isSel ? accent : line
    ctx.fillStyle = isSel ? accent : line
    if (f.kind === 'point') {
      for (const part of f.parts) {
        const p = screen(part[0], vpNow)
        ctx.beginPath()
        ctx.arc(p.x, p.y, isSel ? 5.5 : 4, 0, Math.PI * 2)
        ctx.globalAlpha = isSel ? 1 : 0.85
        ctx.fill()
        ctx.globalAlpha = 1
      }
      continue
    }
    if (f.kind === 'line') {
      for (const part of f.parts) drawPolyline(ctx, part, vpNow)
      continue
    }
    // 面：所有环装进一条 path 用 even-odd 填充，洞才会被挖掉
    ctx.beginPath()
    for (const part of f.parts) traceRing(ctx, part, vpNow)
    ctx.globalAlpha = isSel ? 0.3 : 0.18
    ctx.fill('evenodd')
    ctx.globalAlpha = 1
    for (const part of f.parts) {
      ctx.beginPath()
      traceRing(ctx, part, vpNow)
      ctx.stroke()
    }
  }

  // 选中要素的顶点（可拖动）
  const sel = selected.value
  if (sel) {
    ctx.fillStyle = '#fff'
    ctx.strokeStyle = accent
    ctx.lineWidth = 1.5
    for (const part of sel.parts) {
      for (const pt of part) {
        const p = screen(pt, vpNow)
        ctx.beginPath()
        ctx.rect(p.x - 3.5, p.y - 3.5, 7, 7)
        ctx.fill()
        ctx.stroke()
      }
    }
  }

  // 绘制草稿
  if (draft.value.length > 0) {
    ctx.strokeStyle = accent
    ctx.lineWidth = 2
    ctx.setLineDash([5, 4])
    if (tool.value === 'polygon') {
      ctx.beginPath()
      traceRing(ctx, draft.value, vpNow)
      if (draftCursor.value) {
        const c = screen(draftCursor.value, vpNow)
        ctx.lineTo(c.x, c.y)
      }
      ctx.stroke()
    } else {
      const pts = [...draft.value]
      if (draftCursor.value) pts.push(draftCursor.value)
      drawPolyline(ctx, pts, vpNow)
    }
    ctx.setLineDash([])
    ctx.fillStyle = accent
    for (const pt of draft.value) {
      const p = screen(pt, vpNow)
      ctx.beginPath()
      ctx.arc(p.x, p.y, 3.5, 0, Math.PI * 2)
      ctx.fill()
    }
  }

  /* ---- 编辑辅助指示（一律画在最上层） ---- */

  // 加点模式：待插入位置的空心方块。有吸附时吸到目标，否则落在边上的投影点。
  if (tool.value === 'insert') {
    const target: { x: number; y: number } | null = snapMark.value ?? hoverEdge.value
    if (target) {
      ctx.strokeStyle = snapMark.value ? okColor : accent
      ctx.lineWidth = 2
      ctx.beginPath()
      ctx.rect(target.x - 4, target.y - 4, 8, 8)
      ctx.stroke()
    }
  }

  // 删点模式：悬停顶点高亮成实心方块 + 外圈
  if (tool.value === 'remove' && hoverVertex.value) {
    const f = selected.value
    const pt = f?.parts[hoverVertex.value[0]]?.[hoverVertex.value[1]]
    if (pt) {
      const p = screen(pt, vpNow)
      ctx.fillStyle = okColor
      ctx.fillRect(p.x - 4, p.y - 4, 8, 8)
      ctx.strokeStyle = okColor
      ctx.lineWidth = 2
      ctx.beginPath()
      ctx.arc(p.x, p.y, 9, 0, Math.PI * 2)
      ctx.stroke()
    }
  }

  // 吸附指示：顶点吸附画菱形（表示会精确落到那个顶点），边吸附画圆点
  const sm = snapMark.value
  if (sm) {
    ctx.strokeStyle = okColor
    ctx.fillStyle = okColor
    ctx.lineWidth = 2
    ctx.beginPath()
    if (sm.kind === 'vertex') {
      ctx.moveTo(sm.x, sm.y - 6)
      ctx.lineTo(sm.x + 6, sm.y)
      ctx.lineTo(sm.x, sm.y + 6)
      ctx.lineTo(sm.x - 6, sm.y)
      ctx.closePath()
    } else {
      ctx.arc(sm.x, sm.y, 4, 0, Math.PI * 2)
    }
    ctx.stroke()
    ctx.globalAlpha = 0.35
    ctx.beginPath()
    ctx.arc(sm.x, sm.y, 9, 0, Math.PI * 2)
    ctx.stroke()
    ctx.globalAlpha = 1
  }
}

function traceRing(ctx: CanvasRenderingContext2D, ring: number[][], vpNow: Viewport): void {
  if (ring.length === 0) return
  const first = screen(ring[0], vpNow)
  ctx.moveTo(first.x, first.y)
  for (let i = 1; i < ring.length; i++) {
    const p = screen(ring[i], vpNow)
    ctx.lineTo(p.x, p.y)
  }
  ctx.closePath()
}

function drawPolyline(ctx: CanvasRenderingContext2D, line: number[][], vpNow: Viewport): void {
  if (line.length === 0) return
  ctx.beginPath()
  const first = screen(line[0], vpNow)
  ctx.moveTo(first.x, first.y)
  for (let i = 1; i < line.length; i++) {
    const p = screen(line[i], vpNow)
    ctx.lineTo(p.x, p.y)
  }
  ctx.stroke()
}

/* ---------------- 交互 ---------------- */

function localXY(e: PointerEvent | WheelEvent | MouseEvent): { x: number; y: number } {
  const canvas = canvasRef.value
  if (!canvas) return { x: 0, y: 0 }
  const rect = canvas.getBoundingClientRect()
  return { x: e.clientX - rect.left, y: e.clientY - rect.top }
}

/** 屏幕点 → 经纬度 */
function toLL(x: number, y: number): [number, number] {
  return mercToLonLat(unprojectPoint(x, y, vp.value, canvasSize.value.w, canvasSize.value.h))
}

/* ---------------- 吸附取点 ---------------- */

/**
 * 要素经纬度范围缓存。
 * 要素对象是**不可变**的（每次改动都替换新对象），因此用 WeakMap 以对象身份
 * 为键即可天然失效——不需要额外的版本号管理。
 */
const boundsCache = new WeakMap<Feature, [number, number, number, number]>()

function featureBounds(f: Feature): [number, number, number, number] {
  const cached = boundsCache.get(f)
  if (cached) return cached
  let minx = Infinity
  let miny = Infinity
  let maxx = -Infinity
  let maxy = -Infinity
  for (const part of f.parts) {
    for (const pt of part) {
      if (pt[0] < minx) minx = pt[0]
      if (pt[1] < miny) miny = pt[1]
      if (pt[0] > maxx) maxx = pt[0]
      if (pt[1] > maxy) maxy = pt[1]
    }
  }
  const b: [number, number, number, number] = [minx, miny, maxx, maxy]
  boundsCache.set(f, b)
  return b
}

/**
 * 收集吸附目标（屏幕坐标 + 经纬度）。
 *
 * 先用**经纬度范围**粗筛，只投影光标附近的要素——投影是这里最贵的一步，
 * 大图层下全量投影会让指针移动明显掉帧。
 */
function collectSnapTargets(cursor: [number, number], radiusPx = 12): SnapTargets {
  const out: SnapTargets = { vertices: [], edges: [] }
  const radiusGeo = radiusPx * vp.value.mpp
  const dLat = radiusGeo / 110540
  const dLon = radiusGeo / (111320 * Math.max(0.05, Math.cos((cursor[1] * Math.PI) / 180)))
  const { w, h } = canvasSize.value

  for (const f of features.value) {
    const [minx, miny, maxx, maxy] = featureBounds(f)
    if (
      maxx < cursor[0] - dLon ||
      minx > cursor[0] + dLon ||
      maxy < cursor[1] - dLat ||
      miny > cursor[1] + dLat
    ) {
      continue
    }
    for (const part of f.parts) {
      const sc = part.map((pt) => projectPoint(lonLatToMerc(pt[0], pt[1]), vp.value, w, h))
      if (snapVertexOn.value) {
        for (let i = 0; i < sc.length; i++) {
          out.vertices.push({ x: sc[i].x, y: sc[i].y, lon: part[i][0], lat: part[i][1] })
        }
      }
      if (snapEdgeOn.value && f.kind !== 'point') {
        for (let i = 0; i + 1 < sc.length; i++) {
          out.edges.push({
            x1: sc[i].x,
            y1: sc[i].y,
            x2: sc[i + 1].x,
            y2: sc[i + 1].y,
            lon1: part[i][0],
            lat1: part[i][1],
            lon2: part[i + 1][0],
            lat2: part[i + 1][1],
          })
        }
        // 面是闭合环：补上「末点 → 首点」那条边，否则贴不到收口处
        if (f.kind === 'polygon' && sc.length >= 3) {
          const n = sc.length - 1
          out.edges.push({
            x1: sc[n].x,
            y1: sc[n].y,
            x2: sc[0].x,
            y2: sc[0].y,
            lon1: part[n][0],
            lat1: part[n][1],
            lon2: part[0][0],
            lat2: part[0][1],
          })
        }
      }
    }
  }
  return out
}

/** 查询吸附（关闭时返回 null） */
function querySnap(x: number, y: number): SnapResult | null {
  if (!snapOn.value) return null
  return findSnap(x, y, collectSnapTargets(toLL(x, y)))
}

/** 屏幕点 → 经纬度；开启吸附时优先取吸附点的**原始经纬度** */
function toLLSnapped(x: number, y: number): [number, number] {
  const s = querySnap(x, y)
  return s ? [s.lon, s.lat] : toLL(x, y)
}

/** 命中判定：返回最上层命中的要素（按绘制逆序） */
function hitTest(x: number, y: number): Feature | null {
  for (let i = features.value.length - 1; i >= 0; i--) {
    const f = features.value[i]
    if (f.kind === 'point') {
      for (const part of f.parts) {
        const p = screen(part[0], vp.value)
        if (Math.hypot(p.x - x, p.y - y) <= 7) return f
      }
      continue
    }
    if (f.kind === 'line') {
      for (const part of f.parts) {
        const pts = part.map((pt) => screen(pt, vp.value))
        for (let k = 0; k + 1 < pts.length; k++) {
          if (distToSegment(x, y, pts[k].x, pts[k].y, pts[k + 1].x, pts[k + 1].y) <= 6) return f
        }
      }
      continue
    }
    // 面：内部（even-odd：外环内且不在洞里）或靠近边界
    const rings = f.parts.map((part) => part.map((pt) => {
      const p = screen(pt, vp.value)
      return [p.x, p.y]
    }))
    let insideCount = 0
    for (const ring of rings) if (pointInRing(x, y, ring)) insideCount++
    if (insideCount % 2 === 1) return f
    for (const ring of rings) {
      for (let k = 0; k + 1 < ring.length; k++) {
        if (distToSegment(x, y, ring[k][0], ring[k][1], ring[k + 1][0], ring[k + 1][1]) <= 6) return f
      }
    }
  }
  return null
}

/** 命中选中要素的顶点：返回 [部分下标, 顶点下标] */
function hitVertex(x: number, y: number): [number, number] | null {
  const f = selected.value
  if (!f) return null
  for (let pi = 0; pi < f.parts.length; pi++) {
    const part = f.parts[pi]
    for (let vi = 0; vi < part.length; vi++) {
      const p = screen(part[vi], vp.value)
      if (Math.hypot(p.x - x, p.y - y) <= 7) return [pi, vi]
    }
  }
  return null
}

/** 选中要素上离光标最近的边（用于插入顶点）：[部分下标, 边起点下标, 边上最近点] */
function hitEdge(x: number, y: number): { part: number; seg: number; x: number; y: number } | null {
  const f = selected.value
  if (!f || f.kind === 'point') return null
  let best: { part: number; seg: number; x: number; y: number; dist: number } | null = null
  for (let pi = 0; pi < f.parts.length; pi++) {
    const part = f.parts[pi]
    const sc = part.map((pt) => screen(pt, vp.value))
    // 面是闭合环：最后一段回到首点；折线只到倒数第二段
    const segCount = f.kind === 'polygon' ? sc.length : sc.length - 1
    for (let i = 0; i < segCount; i++) {
      const j = (i + 1) % sc.length
      const c = closestOnSegment(x, y, sc[i].x, sc[i].y, sc[j].x, sc[j].y)
      if (!best || c.dist < best.dist) {
        best = { part: pi, seg: i, x: c.x, y: c.y, dist: c.dist }
      }
    }
  }
  if (!best || best.dist > 10) return null
  return { part: best.part, seg: best.seg, x: best.x, y: best.y }
}

/** 在选中要素第 part 个环/折线的 seg 边之后插入顶点 */
function insertVertex(part: number, seg: number, ll: [number, number]): void {
  const f = selected.value
  if (!f) return
  pushHistory()
  const parts = f.parts.map((line, pi) =>
    pi === part ? [...line.slice(0, seg + 1), ll, ...line.slice(seg + 1)] : line,
  )
  replaceFeature(f.key, { parts })
}

/**
 * 删除选中要素的一个顶点。
 * 几何退化时拒绝（面环 < 3 点、折线 < 2 点），并如实说明原因——静默失败
 * 会让人以为点击没生效而反复点。
 */
function removeVertex(part: number, idx: number): boolean {
  const f = selected.value
  if (!f) return false
  const line = f.parts[part]
  const minPts = f.kind === 'polygon' ? 3 : 2
  if (line.length <= minPts) {
    toast.info(f.kind === 'polygon' ? '面环至少需要 3 个顶点，无法再删' : '折线至少需要 2 个顶点，无法再删')
    return false
  }
  pushHistory()
  const parts = f.parts.map((l, pi) => (pi === part ? l.filter((_, i) => i !== idx) : l))
  replaceFeature(f.key, { parts })
  return true
}

function onPointerDown(e: PointerEvent): void {
  const { x, y } = localXY(e)
  // 指针捕获失败不能中断编辑：某些指针来源（合成事件、部分笔输入）会抛
  // NotFoundError，抛出去后面的落点/插入逻辑就全不执行了。
  try {
    canvasRef.value?.setPointerCapture(e.pointerId)
  } catch {
    /* 忽略：没有捕获也能靠 move/up 完成本次操作 */
  }
  const noDrag = { mode: 'none' as DragMode, sx: 0, sy: 0, part: 0, idx: 0, dx: 0, dy: 0 }

  if (tool.value === 'pan') {
    drag.value = { mode: 'pan', sx: x, sy: y, part: 0, idx: 0, dx: 0, dy: 0 }
    return
  }

  if (tool.value === 'insert') {
    if (!selected.value) {
      toast.info('先在选择工具下点选一个要素，再到边上加点')
      return
    }
    // 落在已有顶点上就不重复插入（否则会造出重合顶点）
    if (hitVertex(x, y)) {
      toast.info('该位置已有顶点')
      return
    }
    const edge = hitEdge(x, y)
    if (!edge) {
      toast.info('请单击选中要素的边线上（靠近边时会出现待插入标记）')
      return
    }
    insertVertex(edge.part, edge.seg, toLLSnapped(x, y))
    hoverEdge.value = null
    draw()
    return
  }

  if (tool.value === 'remove') {
    if (!selected.value) {
      toast.info('先在选择工具下点选一个要素，再到顶点上删点')
      return
    }
    const v = hitVertex(x, y)
    if (!v) {
      toast.info('请单击要素的顶点（靠近顶点时会高亮）')
      return
    }
    if (removeVertex(v[0], v[1])) {
      hoverVertex.value = null
      draw()
    }
    return
  }

  if (tool.value === 'point') {
    createFeature('point', [[toLLSnapped(x, y)]])
    return
  }

  if (tool.value === 'line' || tool.value === 'polygon') {
    const ll = toLLSnapped(x, y)
    draft.value = [...draft.value, ll]
    draftCursor.value = ll
    draw()
    return
  }

  // select
  const v = hitVertex(x, y)
  if (v) {
    const f = selected.value
    if (f) {
      // 记录「抓住顶点时光标相对顶点的偏移」，拖动时不跳位
      const vs = screen(f.parts[v[0]][v[1]], vp.value)
      pushHistory()
      drag.value = { mode: 'vertex', sx: x, sy: y, part: v[0], idx: v[1], dx: vs.x - x, dy: vs.y - y }
      return
    }
  }
  const hit = hitTest(x, y)
  selectedKey.value = hit ? hit.key : null
  if (hit) {
    pushHistory()
    drag.value = { mode: 'feature', sx: x, sy: y, part: 0, idx: 0, dx: 0, dy: 0 }
  } else {
    drag.value = noDrag
  }
  draw()
}

function onPointerMove(e: PointerEvent): void {
  const { x, y } = localXY(e)
  cursorLL.value = toLL(x, y)
  const before = indicatorKey()

  // 吸附指示：选择/加点/绘制/画点模式下都提示落点会被吸到哪里
  const snapTools: Tool[] = ['select', 'insert', 'line', 'polygon', 'point']
  snapMark.value = snapOn.value && snapTools.includes(tool.value) ? querySnap(x, y) : null

  // 加点模式悬停边、删点模式悬停顶点
  hoverEdge.value = tool.value === 'insert' ? hitEdge(x, y) : null
  hoverVertex.value = tool.value === 'remove' ? hitVertex(x, y) : null

  if (tool.value === 'line' || tool.value === 'polygon') {
    if (draft.value.length > 0) {
      // 橡皮筋末端也走吸附：所见落点即保存点
      const s = querySnap(x, y)
      draftCursor.value = s ? [s.lon, s.lat] : toLL(x, y)
    }
  }

  const d = drag.value
  if (d.mode === 'none') {
    // 空闲移动只在指示器变化时重绘，避免大图层下每次移动都全量重画
    if (before !== indicatorKey()) draw()
    return
  }
  if (d.mode === 'pan') {
    vp.value = panBy(vp.value, x - d.sx, y - d.sy)
    drag.value = { ...d, sx: x, sy: y }
    draw()
    return
  }
  const f = selected.value
  if (!f) return
  if (d.mode === 'vertex') {
    // 顶点拖动：**绝对定位**到光标（含吸附），不再用增量累加——
    // 增量会在多次微移后漂移，且无法保证落点落在吸附目标上。
    const s = querySnap(x, y)
    const ll: [number, number] = s ? [s.lon, s.lat] : toLL(x + d.dx, y + d.dy)
    const parts = f.parts.map((part, pi) =>
      part.map((pt, vi) => (pi === d.part && vi === d.idx ? ll : pt)),
    )
    replaceFeature(f.key, { parts })
  } else {
    // 整体移动：按屏幕位移换算成经纬度位移后累加
    const from = toLL(0, 0)
    const to = toLL(x - d.sx, y - d.sy)
    const dLon = to[0] - from[0]
    const dLat = to[1] - from[1]
    const parts = f.parts.map((part) => part.map((pt) => [pt[0] + dLon, pt[1] + dLat]))
    replaceFeature(f.key, { parts })
  }
  drag.value = { ...d, sx: x, sy: y }
  draw()
}

function onPointerUp(): void {
  drag.value = { mode: 'none', sx: 0, sy: 0, part: 0, idx: 0, dx: 0, dy: 0 }
  if (snapMark.value) {
    snapMark.value = null
    draw()
  }
}

/** 指示器指纹：用于判断空闲移动是否需要重绘 */
function indicatorKey(): string {
  const s = snapMark.value
  const v = hoverVertex.value
  const g = hoverEdge.value
  return [
    s ? `${s.kind}:${Math.round(s.x)},${Math.round(s.y)}` : '',
    v ? `${v[0]},${v[1]}` : '',
    g ? `${g.part},${g.seg},${Math.round(g.x)},${Math.round(g.y)}` : '',
  ].join('|')
}

function onWheel(e: WheelEvent): void {
  const { x, y } = localXY(e)
  const factor = e.deltaY < 0 ? 1.18 : 1 / 1.18
  vp.value = zoomAt(vp.value, factor, x, y, canvasSize.value.w, canvasSize.value.h)
  draw()
}

function onDoubleClick(): void {
  finishDraft()
}

/** 结束线/面绘制 */
function finishDraft(): void {
  const pts = draft.value
  draft.value = []
  draftCursor.value = null
  if (tool.value === 'line' && pts.length >= 2) {
    createFeature('line', [pts])
  } else if (tool.value === 'polygon' && pts.length >= 3) {
    createFeature('polygon', [[...pts, pts[0]]])
  } else {
    draw()
  }
}

function cancelDraft(): void {
  if (draft.value.length === 0) return
  draft.value = []
  draftCursor.value = null
  draw()
}

function onKey(e: KeyboardEvent): void {
  const target = e.target as HTMLElement | null
  if (target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA')) return
  if (e.key === 'Escape') {
    cancelDraft()
    return
  }
  if (e.key === 'Enter') {
    finishDraft()
    return
  }
  if (e.key === 'Delete' || e.key === 'Backspace') {
    if (selected.value) {
      e.preventDefault()
      deleteSelected()
    }
    return
  }
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'z') {
    e.preventDefault()
    undo()
  }
}

function setTool(t: Tool): void {
  if (draft.value.length > 0) cancelDraft()
  tool.value = t
}

function fitToData(): void {
  const b = boundsOf(features.value)
  if (b) vp.value = fitViewport(b, canvasSize.value.w, canvasSize.value.h)
  draw()
}

function boundsOf(list: Feature[]): [number, number, number, number] | null {
  let minx = Infinity
  let miny = Infinity
  let maxx = -Infinity
  let maxy = -Infinity
  for (const f of list) {
    for (const part of f.parts) {
      for (const pt of part) {
        minx = Math.min(minx, pt[0])
        miny = Math.min(miny, pt[1])
        maxx = Math.max(maxx, pt[0])
        maxy = Math.max(maxy, pt[1])
      }
    }
  }
  if (!Number.isFinite(minx)) return null
  return [minx, miny, maxx, maxy]
}

onMounted(async () => {
  resizeCanvas()
  window.addEventListener('resize', onResize)
  window.addEventListener('keydown', onKey)
  window.addEventListener('beforeunload', onBeforeUnload)
  // 画布尺寸会因**布局变化**（不是窗口 resize）而改变——例如侧栏内容变高把
  // grid 行撑大。只听 window.resize 会漏掉这种情况，backing store 与 CSS 尺寸
  // 不一致的后果是：画面被拉伸，且点击落点整体偏移（实测表现为"点不中顶点"、
  // "在边上点击却不插入"）。用 ResizeObserver 盯住元素本身才可靠。
  if (typeof ResizeObserver !== 'undefined' && canvasRef.value) {
    resizeObserver = new ResizeObserver(() => {
      resizeCanvas()
      draw()
    })
    // 转换只为绕开 Cesium 自带 DOM 类型与 lib.dom 的结构冲突（同一份
    // HTMLCanvasElement 被两处声明，结构上互不兼容），运行时就是同一个元素。
    resizeObserver.observe(canvasRef.value as unknown as Element)
  }
  await load()
})

let resizeObserver: ResizeObserver | null = null

onBeforeUnmount(() => {
  resizeObserver?.disconnect()
  resizeObserver = null
  window.removeEventListener('resize', onResize)
  window.removeEventListener('keydown', onKey)
  window.removeEventListener('beforeunload', onBeforeUnload)
})

function onResize(): void {
  resizeCanvas()
  draw()
}

function onBeforeUnload(e: BeforeUnloadEvent): void {
  if (pendingCount.value > 0) {
    e.preventDefault()
    e.returnValue = ''
  }
}

function leave(): void {
  if (pendingCount.value > 0 && !window.confirm('还有未保存的编辑，确定离开？')) return
  void router.push('/vector')
}
</script>

<template>
  <section class="page editor">
    <header class="page-head">
      <div>
        <h1 class="page-title">编辑：{{ layerName }}</h1>
        <p class="page-sub">
          {{ geomKindLabel }}图层 · {{ features.length }} 个要素 ·
          <span :class="{ dirty: pendingCount > 0 }">待保存 {{ pendingCount }}</span>
        </p>
      </div>
      <div class="toolbar">
        <button class="btn btn-sm" type="button" :disabled="saving" @click="load">
          <AppIcon name="refresh" :size="12" /> 重新载入
        </button>
        <button
          class="btn btn-solid btn-sm"
          type="button"
          :disabled="saving || pendingCount === 0"
          @click="save"
        >
          {{ saving ? '保存中…' : '保存' }}
        </button>
        <button class="btn btn-sm" type="button" @click="leave">返回</button>
      </div>
    </header>

    <div v-if="loadError" class="error-box">{{ loadError }}</div>

    <div class="editor-body">
      <!-- 工具栏 -->
      <div class="tool-rail">
        <button
          v-for="t in visibleTools"
          :key="t.id"
          class="rail-btn"
          :class="{ active: tool === t.id }"
          type="button"
          :title="t.hint"
          @click="setTool(t.id)"
        >
          <AppIcon :name="t.icon" :size="14" />
          <span>{{ t.label }}</span>
        </button>
        <div class="rail-sep" />
        <button class="rail-btn" type="button" :disabled="history.length === 0" title="撤销 (Ctrl/Cmd+Z)" @click="undo">
          <AppIcon name="refresh" :size="14" /><span>撤销</span>
        </button>
        <button class="rail-btn" type="button" :disabled="!selected" title="删除选中 (Delete)" @click="deleteSelected">
          <AppIcon name="trash" :size="14" /><span>删除</span>
        </button>
        <button class="rail-btn" type="button" title="缩放至全图" @click="fitToData">
          <AppIcon name="globe" :size="14" /><span>全图</span>
        </button>
      </div>

      <!-- 画布 -->
      <div class="canvas-wrap">
        <canvas
          ref="canvasRef"
          class="canvas"
          @pointerdown="onPointerDown"
          @pointermove="onPointerMove"
          @pointerup="onPointerUp"
          @pointerleave="onPointerUp"
          @wheel.prevent="onWheel"
          @dblclick.prevent="onDoubleClick"
        />
        <div class="canvas-hint">{{ hint }}</div>
        <div class="canvas-coords">{{ cursorText }}</div>
        <div v-if="loading" class="canvas-mask">正在载入要素…</div>
      </div>

      <!-- 属性面板 -->
      <aside class="side">
        <div class="panel">
          <div class="panel-head">
            <span class="panel-title">要素属性</span>
            <span class="panel-meta">{{ selected ? `ID ${selected.originId || '新建'}` : '未选中' }}</span>
          </div>
          <div v-if="!selected" class="empty">
            在画布上点选一个要素
            <span class="hint">选中后可改属性、拖动顶点改形状</span>
          </div>
          <div v-else class="props">
            <div v-for="(val, name) in selected.props" :key="String(name)" class="prop-row">
              <input
                class="v-input prop-key"
                :value="String(name)"
                @change="renameProp(selected.key, String(name), ($event.target as HTMLInputElement).value)"
              />
              <input
                class="v-input prop-val"
                :value="String(val ?? '')"
                @change="onPropInput(selected.key, String(name), ($event.target as HTMLInputElement).value)"
              />
              <button class="btn btn-sm btn-ghost" type="button" title="删除该属性" @click="removeProp(selected.key, String(name))">
                <AppIcon name="close" :size="12" />
              </button>
            </div>
            <button class="btn btn-sm" type="button" @click="addProp(selected.key)">
              <AppIcon name="import" :size="12" /> 添加属性
            </button>
            <p class="props-note">
              数字与 true / false 会按类型保存，其余按文本。属性为**整体替换**：这里改的就是保存后的全部属性。
            </p>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head"><span class="panel-title">吸附</span></div>
          <div class="snap-box">
            <label class="snap-row">
              <input v-model="snapOn" type="checkbox" />
              <span>启用吸附</span>
            </label>
            <label class="snap-row" :class="{ off: !snapOn }">
              <input v-model="snapVertexOn" type="checkbox" :disabled="!snapOn" />
              <span>吸附到顶点（菱形标记，落到同一坐标）</span>
            </label>
            <label class="snap-row" :class="{ off: !snapOn }">
              <input v-model="snapEdgeOn" type="checkbox" :disabled="!snapOn" />
              <span>吸附到边（圆点标记，落到线上）</span>
            </label>
            <p class="snap-note">
              画相邻面时建议两个都开：顶点对顶点才能真正严丝合缝，只贴边仍会留缝。
            </p>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head"><span class="panel-title">操作提示</span></div>
          <div class="tips">
            <p>滚轮缩放 · 平移工具下拖拽移动画布</p>
            <p>画线/画面：依次单击加点，双击或回车结束，Esc 取消</p>
            <p>选择工具：拖动顶点改形状，拖动要素内部整体移动</p>
            <p>加点：在边上单击插入顶点（靠近边出现空心方块）</p>
            <p>删点：单击顶点删除，面环至少留 3 点、折线至少 2 点</p>
            <p>属性键名留空或重复不会被保存</p>
            <p class="warn">
              保存会**就地写回源文件**；服务端首次编辑前会自动备份为
              <code class="code">&lt;文件名&gt;.orig</code>。
            </p>
          </div>
        </div>
      </aside>
    </div>
  </section>
</template>

<style scoped>
.editor-body {
  display: grid;
  grid-template-columns: 80px 1fr 300px;
  gap: var(--sp-3);
  align-items: stretch;
  min-height: 0;
}
@media (max-width: 1100px) {
  .editor-body {
    grid-template-columns: 68px 1fr;
  }
  .side {
    grid-column: 1 / -1;
  }
}
.dirty {
  color: var(--accent-ink);
  font-weight: 600;
}

/* 工具栏 */
.tool-rail {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-2);
  background: var(--surface);
  border: 1px solid var(--line);
  border-radius: var(--radius);
}
.rail-btn {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 3px;
  padding: 7px 4px;
  font-family: inherit;
  font-size: var(--fs-xs);
  color: var(--ink-muted);
  background: none;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: all var(--t-fast);
}
.rail-btn:hover:not(:disabled) {
  color: var(--ink);
  background: var(--surface-hover);
  border-color: var(--line);
}
.rail-btn.active {
  color: var(--accent-ink);
  background: var(--accent-soft);
  border-color: color-mix(in oklch, var(--accent) 40%, var(--line));
}
.rail-btn:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}
.rail-sep {
  height: 1px;
  margin: 2px 0;
  background: var(--line);
}

/* 画布 */
.canvas-wrap {
  position: relative;
  min-height: 560px;
  border: 1px solid var(--line);
  border-radius: var(--radius);
  overflow: hidden;
  background: var(--bg-sunken);
}
.canvas {
  display: block;
  width: 100%;
  height: 100%;
  min-height: 560px;
  cursor: crosshair;
  touch-action: none;
}
.canvas-hint,
.canvas-coords {
  position: absolute;
  padding: 3px 8px;
  font-size: var(--fs-xs);
  color: var(--ink-muted);
  background: var(--overlay-bg);
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
  pointer-events: none;
}
.canvas-hint {
  top: var(--sp-2);
  left: var(--sp-2);
}
.canvas-coords {
  bottom: var(--sp-2);
  right: var(--sp-2);
  font-family: var(--font-mono, ui-monospace, monospace);
}
.canvas-mask {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: var(--fs-sm);
  color: var(--ink-faint);
  background: color-mix(in oklch, var(--bg-sunken) 82%, transparent);
}

/* 属性面板 */
.side {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.props {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3);
}
.prop-row {
  display: grid;
  grid-template-columns: 1fr 1fr auto;
  gap: 4px;
  align-items: center;
}
.v-input {
  width: 100%;
  padding: 4px 7px;
  font-family: inherit;
  font-size: var(--fs-xs);
  color: var(--ink);
  background: var(--surface);
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-sm);
  box-sizing: border-box;
}
.v-input:focus {
  outline: none;
  border-color: var(--primary);
}
.props-note,
.tips p {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--ink-faint);
  line-height: 1.7;
}
.snap-box {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3);
}
.snap-row {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  font-size: var(--fs-xs);
  color: var(--ink-muted);
  cursor: pointer;
}
.snap-row.off {
  opacity: 0.5;
}
.snap-row input {
  margin: 2px 0 0;
  flex: none;
}
.snap-note {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--ink-faint);
  line-height: 1.7;
}
.tips {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3);
}
.tips .warn {
  color: var(--warn);
}
</style>
