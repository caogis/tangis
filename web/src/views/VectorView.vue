<script setup lang="ts">
/**
 * 矢量数据（F-10）：本地矢量文件 → 图层注册 → MVT 瓦片 / WFS 要素服务。
 *
 * 桌面单机版走**文件矢量**（后端 internal/vector/filegateway.go）：不依赖 PostGIS，
 * 注册一个 GeoJSON 即得到一个可分发图层。服务端模式同样可用（数据源为 PostGIS）。
 *
 * 导入两条路径：
 *   1) 拖拽/选择文件 → POST /uploads 落盘 → POST /vector/files 注册（浏览器可控）
 *   2) 直接填本机绝对路径 → POST /vector/files（免拷贝，适合大文件）
 */
import { computed, nextTick, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import {
  deleteVectorLayer,
  exportVectorLayer,
  fetchVectorFeatures,
  getSystemInfo,
  listVectorLayers,
  listVectorSources,
  registerVectorFile,
  uploadFiles,
  vectorLayerMetadata,
  vectorTileUrlTemplate,
  vectorWfsCapabilitiesUrl,
  vectorWfsFeatureUrl,
} from '../api/client'
import type { GeoJsonFeature, VectorLayer, VectorLayerMetadata, VectorSource } from '../api/types'
import AppIcon from '../components/AppIcon.vue'
import { useToast } from '../composables/useToast'

const toast = useToast()

/** 预览单次拉取要素上限：足够看清形状，又不至于把浏览器拖死 */
const PREVIEW_COUNT = 3000

/* ---------------- 状态 ---------------- */

const layers = ref<VectorLayer[]>([])
const sources = ref<VectorSource[]>([])
const loading = ref(true)
const listError = ref('')
/** null = 未知（尚未取到运行信息）；false = 当前部署未装配矢量能力 */
const vectorReady = ref<boolean | null>(null)

const selected = ref<VectorLayer | null>(null)
const meta = ref<VectorLayerMetadata | null>(null)

/** 导入区 */
const importing = ref(false)
const importError = ref('')
const pathInput = ref('')
const nameInput = ref('')
const dragOver = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)
const busyName = ref('')

/** 预览画布 */
const canvasRef = ref<HTMLCanvasElement | null>(null)
const previewState = ref<'idle' | 'loading' | 'ok' | 'empty' | 'error'>('idle')
const previewNote = ref('')
const copiedKey = ref('')

const sourceOf = computed(() => {
  const id = selected.value?.sourceId
  return sources.value.find((s) => s.id === id)
})

const bboxText = computed(() => {
  const b = meta.value?.bboxWgs84
  if (!b || b.length !== 4) return '—'
  return `${b[0].toFixed(4)}, ${b[1].toFixed(4)}  →  ${b[2].toFixed(4)}, ${b[3].toFixed(4)}`
})

const tileTemplate = computed(() =>
  selected.value ? vectorTileUrlTemplate(selected.value.name) : '',
)
const wfsUrl = computed(() => (selected.value ? vectorWfsFeatureUrl(selected.value.name) : ''))
const wfsCapsUrl = computed(() => vectorWfsCapabilitiesUrl())

/* ---------------- 载入 ---------------- */

async function loadAll(): Promise<void> {
  loading.value = true
  listError.value = ''
  try {
    const [ls, ss] = await Promise.all([listVectorLayers(), listVectorSources()])
    layers.value = ls
    sources.value = ss
  } catch (e) {
    listError.value = e instanceof Error ? e.message : '读取图层失败'
    layers.value = []
  } finally {
    loading.value = false
  }
}

async function loadRuntime(): Promise<void> {
  try {
    const info = await getSystemInfo()
    // 后端显式声明能力开关：false 表示未实现或当前部署未装配
    vectorReady.value = info.capabilities.vector !== false
  } catch {
    // 运行信息不可得时不阻断页面（列表自身的错误更具体）
    vectorReady.value = true
  }
}

onMounted(async () => {
  await Promise.all([loadRuntime(), loadAll()])
})

/* ---------------- 导入 ---------------- */

/** 后端图层名需符合标识符白名单（^[A-Za-z_][A-Za-z0-9_]*$），这里做前端预清洗。 */
function suggestName(stem: string): string {
  const cleaned = stem.replace(/[^A-Za-z0-9_]/g, '_').replace(/^_+/, '')
  if (!cleaned) return ''
  return /^[A-Za-z_]/.test(cleaned) ? cleaned : `layer_${cleaned}`
}

function stemOf(filename: string): string {
  const i = filename.lastIndexOf('.')
  return i > 0 ? filename.slice(0, i) : filename
}

async function register(path: string, name?: string): Promise<void> {
  const res = await registerVectorFile({ path, name: name?.trim() || undefined })
  // GeoPackage 可能一次注册出多个图层（每个要素表一个）
  const created = res.layers.map((l) => l.name)
  const first = created[0] ?? res.source.name
  toast.success(
    created.length > 1
      ? `已导入 ${created.length} 个图层：${created.join('、')}`
      : `已导入图层 ${first}`,
  )
  await loadAll()
  if (first) await selectLayer(first)
}

/**
 * 拖拽/选择文件：先上传落盘，再按落盘路径注册。
 *
 * Shapefile 是**文件族**（.shp 几何 + .dbf 属性 + .prj 坐标系 + .cpg 编码），
 * 因此选中 .shp 时要把整批文件一起上传（同名同目录），再按 .shp 注册；
 * 其它格式只取第一个文件。
 */
async function importFiles(files: File[]): Promise<void> {
  if (files.length === 0 || importing.value) return
  const shp = files.find((f) => f.name.toLowerCase().endsWith('.shp'))
  const batch = shp ? files : [files[0]]
  const target = shp ?? files[0]
  importing.value = true
  importError.value = ''
  try {
    const up = await uploadFiles(batch)
    // 上传保留原文件名（服务端以 part.FileName() 作相对路径）
    const root = up.root.endsWith('/') ? up.root.slice(0, -1) : up.root
    await register(`${root}/${target.name}`, nameInput.value.trim() || suggestName(stemOf(target.name)))
  } catch (e) {
    importError.value = e instanceof Error ? e.message : '导入失败'
  } finally {
    importing.value = false
    if (fileInput.value) fileInput.value.value = ''
  }
}

function onDrop(e: DragEvent): void {
  dragOver.value = false
  const files = Array.from(e.dataTransfer?.files ?? [])
  void importFiles(files)
}

function onPick(e: Event): void {
  const input = e.target as HTMLInputElement
  void importFiles(Array.from(input.files ?? []))
}

/** 按本机绝对路径注册（桌面版服务端就在本机，免上传拷贝） */
async function importByPath(): Promise<void> {
  const p = pathInput.value.trim()
  if (!p || importing.value) return
  importing.value = true
  importError.value = ''
  try {
    await register(p, nameInput.value)
    pathInput.value = ''
    nameInput.value = ''
  } catch (e) {
    importError.value = e instanceof Error ? e.message : '导入失败'
  } finally {
    importing.value = false
  }
}

/* ---------------- 图层详情与预览 ---------------- */

async function selectLayer(name: string): Promise<void> {
  const layer = layers.value.find((l) => l.name === name)
  if (!layer) return
  selected.value = layer
  meta.value = null
  previewState.value = 'loading'
  previewNote.value = '读取元数据…'
  try {
    // 元数据失败不阻断预览：拿不到范围就退回按要素实际范围自适应
    meta.value = await vectorLayerMetadata(name)
  } catch {
    meta.value = null
  }
  await loadPreview(layer)
}

async function loadPreview(layer: VectorLayer): Promise<void> {
  previewState.value = 'loading'
  previewNote.value = '读取要素…'
  const bbox = meta.value?.bboxWgs84
  const box =
    bbox && bbox.length === 4 ? (bbox as [number, number, number, number]) : undefined
  try {
    const fc = await fetchVectorFeatures(layer.name, { bbox: box, count: PREVIEW_COUNT })
    const feats = fc.features ?? []
    if (feats.length === 0) {
      previewState.value = 'empty'
      previewNote.value = '该图层没有可预览的要素'
      return
    }
    previewState.value = 'ok'
    previewNote.value = ''
    await nextTick()
    drawPreview(feats, box)
  } catch (e) {
    previewState.value = 'error'
    previewNote.value = e instanceof Error ? e.message : '要素读取失败'
  }
}

/** 读取主题色（canvas 无法直接使用 CSS 变量） */
function themeColor(name: string, fallback: string): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return v || fallback
}

/** 从要素反推范围（元数据缺失时兜底） */
function boundsOf(features: GeoJsonFeature[]): [number, number, number, number] | null {
  let minx = Infinity
  let miny = Infinity
  let maxx = -Infinity
  let maxy = -Infinity
  const walk = (c: unknown): void => {
    if (!Array.isArray(c)) return
    if (typeof c[0] === 'number' && typeof c[1] === 'number') {
      const [x, y] = c as [number, number]
      if (Number.isFinite(x) && Number.isFinite(y)) {
        minx = Math.min(minx, x)
        miny = Math.min(miny, y)
        maxx = Math.max(maxx, x)
        maxy = Math.max(maxy, y)
      }
      return
    }
    for (const child of c) walk(child)
  }
  for (const f of features) walk(f.geometry?.coordinates)
  if (!Number.isFinite(minx)) return null
  return [minx, miny, maxx, maxy]
}

/**
 * 把 GeoJSON 画到 canvas：等距圆柱投影 + 按范围自适应（保持纵横比）。
 * 仅用于"看清数据长什么样"，不做地图投影精度承诺——真实渲染交给 MVT 客户端。
 */
function drawPreview(features: GeoJsonFeature[], bbox?: [number, number, number, number]): void {
  const canvas = canvasRef.value
  if (!canvas) return
  const box = bbox ?? boundsOf(features)
  if (!box) {
    previewState.value = 'empty'
    previewNote.value = '要素没有可用坐标'
    return
  }
  const dpr = window.devicePixelRatio || 1
  const w = canvas.clientWidth || 520
  const h = canvas.clientHeight || 300
  canvas.width = Math.round(w * dpr)
  canvas.height = Math.round(h * dpr)
  const ctx = canvas.getContext('2d')
  if (!ctx) return
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
  ctx.clearRect(0, 0, w, h)

  const [minx, miny, maxx, maxy] = box
  const spanX = Math.max(maxx - minx, 1e-9)
  const spanY = Math.max(maxy - miny, 1e-9)
  const pad = 10
  const scale = Math.min((w - pad * 2) / spanX, (h - pad * 2) / spanY)
  const ox = pad + ((w - pad * 2) - spanX * scale) / 2
  const oy = pad + ((h - pad * 2) - spanY * scale) / 2
  const px = (lon: number): number => ox + (lon - minx) * scale
  const py = (lat: number): number => oy + (maxy - lat) * scale // 纬度向上

  const line = themeColor('--primary', '#3d7f96')
  const fill = themeColor('--accent', '#c9812a')
  const grid = themeColor('--line', '#dddddd')

  ctx.strokeStyle = grid
  ctx.lineWidth = 1
  ctx.strokeRect(pad, pad, w - pad * 2, h - pad * 2)

  ctx.lineWidth = 1
  ctx.fillStyle = fill
  ctx.strokeStyle = line

  // 描边路径：把一条环/折线画进当前 path（不 stroke，由调用方决定何时落笔）
  const traceLine = (coords: unknown): boolean => {
    if (!Array.isArray(coords)) return false
    let started = false
    for (const pt of coords) {
      if (!Array.isArray(pt) || typeof pt[0] !== 'number' || typeof pt[1] !== 'number') continue
      const x = px(pt[0] as number)
      const y = py(pt[1] as number)
      if (!started) {
        ctx.moveTo(x, y)
        started = true
      } else {
        ctx.lineTo(x, y)
      }
    }
    if (started) ctx.closePath()
    return started
  }

  // 面：一条 path 装全部环后按 even-odd 填充，洞才会被正确挖掉；
  // 半透明用 globalAlpha——oklch 变量不能简单拼 alpha 后缀。
  const tracePolygon = (rings: unknown[]): boolean => {
    ctx.beginPath()
    let any = false
    for (const r of rings) any = traceLine(r) || any
    if (!any) return false
    ctx.globalAlpha = 0.22
    ctx.fill('evenodd')
    ctx.globalAlpha = 1
    ctx.stroke()
    return true
  }

  const paintPoint = (pt: unknown): void => {
    if (!Array.isArray(pt) || typeof pt[0] !== 'number' || typeof pt[1] !== 'number') return
    ctx.beginPath()
    ctx.arc(px(pt[0] as number), py(pt[1] as number), 2.4, 0, Math.PI * 2)
    ctx.fill()
  }

  for (const f of features) {
    const g = f.geometry
    if (!g) continue
    const c = g.coordinates
    switch (g.type) {
      case 'Point':
        paintPoint(c)
        break
      case 'MultiPoint':
        for (const pt of (c as unknown[]) ?? []) paintPoint(pt)
        break
      case 'LineString':
        ctx.beginPath()
        if (traceLine(c)) ctx.stroke()
        break
      case 'MultiLineString':
        for (const l of (c as unknown[]) ?? []) {
          ctx.beginPath()
          if (traceLine(l)) ctx.stroke()
        }
        break
      case 'Polygon':
        tracePolygon((c as unknown[]) ?? [])
        break
      case 'MultiPolygon':
        // 每个多边形单独成 path（跨多边形 even-odd 会互相挖洞）
        for (const poly of (c as unknown[]) ?? []) tracePolygon((poly as unknown[]) ?? [])
        break
      case 'GeometryCollection':
        for (const sub of g.geometries ?? []) {
          ctx.beginPath()
          if (traceLine(sub.coordinates)) ctx.stroke()
        }
        break
    }
  }
}

/* ---------------- 操作 ---------------- */

async function removeLayer(name: string): Promise<void> {
  if (busyName.value) return
  if (!window.confirm(`删除图层 ${name}？\n注册信息会被移除，源文件保留在磁盘上。`)) return
  busyName.value = name
  try {
    await deleteVectorLayer(name)
    toast.success('图层已删除')
    if (selected.value?.name === name) {
      selected.value = null
      meta.value = null
      previewState.value = 'idle'
    }
    await loadAll()
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '删除失败')
  } finally {
    busyName.value = ''
  }
}

/** 导出中标记（同一时刻只允许一个导出请求） */
const exporting = ref('')

/**
 * 导出图层为文件（回流给 QGIS / ArcGIS 等）。
 * 服务端会把有损处理（Shapefile 跳过的要素、字段名截断）通过响应头回传，
 * 这里如实提示给用户，而不是默默丢掉一批数据。
 */
async function doExport(format: 'geojson' | 'gpkg' | 'shp'): Promise<void> {
  const layer = selected.value
  if (!layer || exporting.value) return
  exporting.value = format
  try {
    const info = await exportVectorLayer(layer.name, format)
    if (info.skipped > 0) {
      toast.info(`已导出 ${info.filename}（${info.features} 个要素，跳过 ${info.skipped} 个）`, 6000)
    } else {
      toast.success(`已导出 ${info.filename}（${info.features} 个要素）`)
    }
    if (info.warnings.length > 0) {
      toast.info(info.warnings.join('；'), 9000)
    }
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '导出失败')
  } finally {
    exporting.value = ''
  }
}

async function copy(text: string, key: string): Promise<void> {
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    copiedKey.value = key
    setTimeout(() => {
      if (copiedKey.value === key) copiedKey.value = ''
    }, 1600)
  } catch {
    toast.error('复制失败：浏览器未授权剪贴板')
  }
}

function geomLabel(t: string): string {
  const map: Record<string, string> = {
    POINT: '点',
    LINESTRING: '线',
    POLYGON: '面',
    GEOMETRY: '混合几何',
  }
  return map[t] ?? t ?? '—'
}
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">矢量数据</h1>
        <p class="page-sub">本地矢量文件 → 图层注册 → MVT 瓦片 / WFS 要素服务</p>
      </div>
      <div class="toolbar">
        <span v-if="vectorReady === false" class="chip chip-partial">未装配</span>
        <span v-else class="chip chip-ready">{{ layers.length }} 个图层</span>
        <button class="btn btn-sm" :disabled="loading" @click="loadAll">
          <AppIcon name="refresh" :size="12" /> 刷新
        </button>
      </div>
    </header>

    <!-- 未装配：保留原因说明，不假装可用 -->
    <div v-if="vectorReady === false" class="notice panel block">
      <span class="notice-bar" aria-hidden="true" />
      <div>
        <div class="notice-title">当前部署未装配矢量能力</div>
        <p class="notice-body">
          <code class="code">GET /api/v1/system</code> 返回 <code class="code">capabilities.vector = false</code>。
          桌面单机版默认启用<strong>文件矢量</strong>（GeoJSON，不依赖 PostGIS）；
          服务端模式需配置 PostGIS 数据源。
        </p>
      </div>
    </div>

    <template v-else>
      <!-- 导入 -->
      <div class="panel block">
        <div class="panel-head">
          <span class="panel-title">导入矢量文件</span>
          <span class="panel-meta">
            GeoJSON（.geojson/.json）· Shapefile（.shp/.dbf/.prj）· GeoPackage（.gpkg）· 投影坐标自动换算
          </span>
        </div>
        <div class="import">
          <label
            class="drop"
            :class="{ over: dragOver, busy: importing }"
            @dragover.prevent="dragOver = true"
            @dragleave="dragOver = false"
            @drop.prevent="onDrop"
          >
            <input
              ref="fileInput"
              type="file"
              accept=".geojson,.json,.shp,.dbf,.prj,.cpg,.shx,.gpkg,application/geo+json,application/json"
              multiple
              hidden
              @change="onPick"
            />
            <AppIcon name="import" :size="20" />
            <span class="drop-title">
              {{ importing ? '正在导入…' : '把矢量文件拖到这里，或点击选择文件' }}
            </span>
            <span class="drop-note">
              Shapefile 请连同 .dbf / .prj / .cpg 一起选中（可多选）；GeoPackage 单个文件即可，内含的每个要素表会各注册成一个图层。
              高斯克吕格等投影坐标会按 .prj / 库内的坐标系定义自动换算到 WGS84；
              选择文件会先上传到数据目录再注册，大文件建议用右侧「本机路径」直接引用
            </span>
          </label>

          <div class="import-side">
            <label class="field">
              <span class="field-label">本机路径</span>
              <input
                v-model="pathInput"
                class="v-input"
                type="text"
                placeholder="/Users/…/beijing_blocks.geojson 或 roads.shp"
                @keyup.enter="importByPath"
              />
            </label>
            <label class="field">
              <span class="field-label">图层名（可选，留空取文件名）</span>
              <input
                v-model="nameInput"
                class="v-input"
                type="text"
                placeholder="beijing_blocks"
                @keyup.enter="importByPath"
              />
            </label>
            <button
              class="btn btn-solid btn-sm"
              :disabled="importing || !pathInput.trim()"
              @click="importByPath"
            >
              按路径导入
            </button>
            <p v-if="importError" class="import-error">{{ importError }}</p>
          </div>
        </div>
      </div>

      <div v-if="listError" class="error-box block">{{ listError }}</div>

      <div class="cols">
        <!-- 图层列表 -->
        <div class="panel">
          <div class="panel-head">
            <span class="panel-title">图层</span>
            <span class="panel-meta">{{ loading ? '读取中…' : `${layers.length} 个` }}</span>
          </div>
          <div v-if="!loading && layers.length === 0" class="empty">
            还没有图层
            <span class="hint">导入一个 GeoJSON 文件即可获得 MVT 瓦片与 WFS 服务</span>
          </div>
          <ul v-else class="layer-list">
            <li
              v-for="l in layers"
              :key="l.name"
              class="layer-item"
              :class="{ active: selected?.name === l.name }"
              role="button"
              tabindex="0"
              @click="selectLayer(l.name)"
              @keyup.enter="selectLayer(l.name)"
            >
              <div class="layer-main">
                <span class="layer-name">{{ l.name }}</span>
                <span class="layer-meta">
                  {{ geomLabel(l.geometryType) }} · EPSG:{{ l.srid }} · {{ l.fields.length }} 字段
                </span>
              </div>
              <button
                class="btn btn-icon btn-ghost"
                type="button"
                title="删除图层"
                :disabled="busyName === l.name"
                @click.stop="removeLayer(l.name)"
              >
                <AppIcon name="trash" :size="13" />
              </button>
            </li>
          </ul>
        </div>

        <!-- 详情 + 预览 -->
        <div class="panel">
          <div class="panel-head">
            <span class="panel-title">图层详情</span>
            <span v-if="meta" class="panel-meta">{{ meta.featuresEstimated }} 要素</span>
          </div>

          <div v-if="!selected" class="empty">
            选择左侧图层
            <span class="hint">查看元数据、预览要素并复制分发地址</span>
          </div>

          <template v-else>
            <div class="meta-grid">
              <div class="meta-kv">
                <span class="meta-k">几何类型</span>
                <b class="meta-v">{{ geomLabel(selected.geometryType) }}</b>
              </div>
              <div class="meta-kv">
                <span class="meta-k">坐标系</span>
                <b class="meta-v">EPSG:{{ selected.srid }}</b>
              </div>
              <div class="meta-kv">
                <span class="meta-k">要素数</span>
                <b class="meta-v">{{ meta ? meta.featuresEstimated : '—' }}</b>
              </div>
              <div class="meta-kv">
                <span class="meta-k">建议层级</span>
                <b class="meta-v">{{ meta ? `${meta.minzoom} – ${meta.maxzoom}` : '—' }}</b>
              </div>
              <div class="meta-kv wide">
                <span class="meta-k">范围（WGS84 经纬度）</span>
                <b class="meta-v mono">{{ bboxText }}</b>
              </div>
              <div class="meta-kv wide">
                <span class="meta-k">属性字段</span>
                <b class="meta-v">{{ selected.fields.join('、') || '—' }}</b>
              </div>
              <div class="meta-kv wide">
                <span class="meta-k">数据源</span>
                <b class="meta-v mono">{{ sourceOf ? sourceOf.format || sourceOf.dsn : '—' }}</b>
              </div>
            </div>

            <div class="preview">
              <canvas ref="canvasRef" class="preview-canvas" />
              <div v-if="previewState !== 'ok'" class="preview-note">
                {{ previewNote || '预览准备中…' }}
              </div>
            </div>

            <div class="urls">
              <div class="url-row">
                <span class="url-label">编辑</span>
                <div class="export-btns">
                  <RouterLink class="btn btn-sm" :to="`/vector/${selected.name}/edit`">
                    <AppIcon name="edit" :size="12" /> 打开编辑器
                  </RouterLink>
                  <span class="export-note">就地写回源文件，首次编辑前自动备份</span>
                </div>
              </div>
              <div class="url-row">
                <span class="url-label">导出</span>
                <div class="export-btns">
                  <button
                    class="btn btn-sm"
                    type="button"
                    :disabled="exporting !== ''"
                    title="GeoPackage：单文件、属性类型完整，推荐"
                    @click="doExport('gpkg')"
                  >
                    GeoPackage
                  </button>
                  <button
                    class="btn btn-sm"
                    type="button"
                    :disabled="exporting !== ''"
                    title="Shapefile：打包为 zip（.shp/.shx/.dbf/.prj/.cpg）"
                    @click="doExport('shp')"
                  >
                    Shapefile (.zip)
                  </button>
                  <button
                    class="btn btn-sm"
                    type="button"
                    :disabled="exporting !== ''"
                    title="GeoJSON：文本格式，便于交换与检查"
                    @click="doExport('geojson')"
                  >
                    GeoJSON
                  </button>
                  <span v-if="exporting" class="export-note">正在导出…</span>
                </div>
              </div>
              <div class="url-row">
                <span class="url-label">MVT 模板</span>
                <code class="url-code">{{ tileTemplate }}</code>
                <button
                  class="btn btn-sm btn-ghost"
                  type="button"
                  @click="copy(tileTemplate, 'tile')"
                >
                  <AppIcon name="copy" :size="12" />
                  {{ copiedKey === 'tile' ? '已复制' : '复制' }}
                </button>
              </div>
              <div class="url-row">
                <span class="url-label">WFS 要素</span>
                <code class="url-code">{{ wfsUrl }}</code>
                <button class="btn btn-sm btn-ghost" type="button" @click="copy(wfsUrl, 'wfs')">
                  <AppIcon name="copy" :size="12" />
                  {{ copiedKey === 'wfs' ? '已复制' : '复制' }}
                </button>
              </div>
              <p class="url-note">
                MVT 模板可直接填进 MapLibre / OpenLayers 的矢量源；WFS 返回 GeoJSON。
                桌面版默认关闭鉴权，第三方客户端可直接访问；开启鉴权时需带
                <code class="code">X-API-Key</code> 请求头或签名参数。
                <a :href="wfsCapsUrl" target="_blank" rel="noreferrer">查看 GetCapabilities</a>
              </p>
            </div>
          </template>
        </div>
      </div>
    </template>
  </section>
</template>

<style scoped>
.notice {
  display: flex;
  gap: var(--sp-3);
  padding: var(--sp-4);
  border-left: none;
}
.notice-bar {
  width: 2px;
  flex: none;
  background: var(--accent);
  border-radius: 1px;
}
.notice-title {
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--ink);
  margin-bottom: var(--sp-1);
}
.notice-body {
  margin: 0;
  font-size: var(--fs-sm);
  color: var(--ink-muted);
  line-height: 1.7;
}

/* ---------- 导入区 ---------- */
.import {
  display: grid;
  grid-template-columns: 1.4fr 1fr;
  gap: var(--sp-4);
  padding: var(--sp-4);
}
@media (max-width: 980px) {
  .import {
    grid-template-columns: 1fr;
  }
}
.drop {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--sp-2);
  min-height: 148px;
  padding: var(--sp-4);
  text-align: center;
  border: 1px dashed var(--line-strong);
  border-radius: var(--radius);
  background: var(--bg-sunken);
  color: var(--ink-muted);
  cursor: pointer;
  transition: border-color var(--t-fast), background var(--t-fast), color var(--t-fast);
}
.drop:hover,
.drop.over {
  border-color: var(--primary);
  background: var(--primary-soft);
  color: var(--ink);
}
.drop.busy {
  opacity: 0.7;
  cursor: progress;
}
.drop-title {
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--ink);
}
.drop-note {
  font-size: var(--fs-xs);
  color: var(--ink-faint);
  max-width: 34em;
  line-height: 1.6;
}
.import-side {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
}
.import-side .field {
  width: 100%;
}
.v-input {
  width: 100%;
  padding: 6px 9px;
  font-family: inherit;
  font-size: var(--fs-sm);
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
.import-error {
  margin: var(--sp-3) 0 0;
  font-size: var(--fs-xs);
  color: var(--danger, #c0392b);
  line-height: 1.6;
}

/* ---------- 布局 ---------- */
.cols {
  display: grid;
  grid-template-columns: 340px 1fr;
  gap: var(--sp-4);
  align-items: start;
}
@media (max-width: 1040px) {
  .cols {
    grid-template-columns: 1fr;
  }
}

/* ---------- 图层列表 ---------- */
.layer-list {
  list-style: none;
  margin: 0;
  padding: 0;
}
.layer-item {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
  border-bottom: 1px solid var(--line);
  cursor: pointer;
  transition: background var(--t-fast);
}
.layer-item:last-child {
  border-bottom: none;
}
.layer-item:hover {
  background: var(--surface-hover);
}
.layer-item.active {
  background: var(--accent-soft);
  box-shadow: inset 2px 0 0 var(--accent);
}
.layer-item:focus-visible {
  outline: 2px solid var(--primary);
  outline-offset: -2px;
}
.layer-main {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
  flex: 1;
}
.layer-name {
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--ink);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.layer-meta {
  font-size: var(--fs-xs);
  color: var(--ink-faint);
}

/* ---------- 元数据 ---------- */
.meta-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--sp-2) var(--sp-4);
  padding: var(--sp-4);
  border-bottom: 1px solid var(--line);
}
.meta-kv {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.meta-kv.wide {
  grid-column: 1 / -1;
  flex-direction: row;
  align-items: baseline;
  gap: var(--sp-3);
}
.meta-k {
  font-size: var(--fs-xs);
  color: var(--ink-faint);
}
.meta-v {
  font-size: var(--fs-sm);
  color: var(--ink);
  font-weight: 500;
  overflow-wrap: anywhere;
}
.meta-v.mono {
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: var(--fs-xs);
  font-weight: 400;
}

/* ---------- 预览 ---------- */
.preview {
  position: relative;
  padding: var(--sp-4);
  border-bottom: 1px solid var(--line);
}
.preview-canvas {
  display: block;
  width: 100%;
  height: 300px;
  border-radius: var(--radius-sm);
  background: var(--bg-sunken);
}
.preview-note {
  position: absolute;
  inset: var(--sp-4);
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: var(--fs-xs);
  color: var(--ink-faint);
  text-align: center;
  padding: 0 var(--sp-4);
}

/* ---------- 分发地址 ---------- */
.urls {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-4);
}
.url-row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.url-label {
  flex: none;
  width: 68px;
  font-size: var(--fs-xs);
  color: var(--ink-faint);
}
.export-btns {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex-wrap: wrap;
}
.export-note {
  font-size: var(--fs-xs);
  color: var(--ink-faint);
}
.url-code {
  flex: 1;
  min-width: 0;
  padding: 4px 7px;
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: var(--fs-xs);
  color: var(--ink-muted);
  background: var(--bg-sunken);
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.url-note {
  margin: var(--sp-2) 0 0;
  font-size: var(--fs-xs);
  color: var(--ink-faint);
  line-height: 1.7;
}
</style>
