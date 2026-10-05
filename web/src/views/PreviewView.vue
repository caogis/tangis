<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, shallowRef, watch } from 'vue'
import { useRoute } from 'vue-router'
import * as Cesium from 'cesium'
import { getTask, listServices } from '../api/client'
import type { ServiceItem, Task } from '../api/types'
import { parseSigExpiry } from '../api/url'
import { parseCapabilitiesLayer, wmtsKvpBaseUrl } from '../utils/wmts'
import { buildSceneTree, tilesetRootOf, volumeCenter, type EcefConvert, type TreeNode } from '../utils/tileset'
import SceneTree from '../components/SceneTree.vue'
import MeasureTool from '../components/MeasureTool.vue'
import AppIcon from '../components/AppIcon.vue'

const route = useRoute()
/** 全屏模式（/preview/full）：不套应用外壳，视口高度直接用满 */
const isBare = computed(() => route.meta.bare === true)

/**
 * 从列表点「预览」新页面打开时带 ?task=<taskId>：
 * 服务列表与 viewer 都就绪后自动加载该任务的服务，无需用户再点一次。
 */
const requestedTaskId = computed(() => String(route.query.task ?? ''))
let autoLoaded = false

const services = ref<ServiceItem[]>([])
const loadingList = ref(true)
const listError = ref('')

const activeService = ref<ServiceItem | null>(null)
const tilesetError = ref('')
const tilesetLoading = ref(false)

/* WMTS 影像叠加：taskId -> 是否勾选 / 加载中 / Cesium 图层实例 */
const overlayOn = reactive<Record<string, boolean>>({})
const overlayBusy = reactive<Record<string, boolean>>({})
const overlayError = ref('')
const overlayLayers = new Map<string, Cesium.ImageryLayer>()

/* 服务详情面板 */
const detailService = ref<ServiceItem | null>(null)
const detailTask = ref<Task | null>(null)
const detailLoading = ref(false)
const nowSec = ref(Date.now() / 1000)
const copiedKey = ref('')
let nowTimer: ReturnType<typeof setInterval> | null = null
let copiedTimer: ReturnType<typeof setTimeout> | null = null

/* 点击拾取信息 */
interface PickRow {
  key: string
  value: string
  /** 完整值：tooltip 与复制用（展示值可能被 CSS 截断） */
  full: string
}
interface PickInfo {
  title: string
  rows: PickRow[]
}
const pickInfo = ref<PickInfo | null>(null)

const PICK_COPY_PREFIX = 'pick:'

/* ---------- 场景树（F-05）---------- */
const sceneTree = ref<TreeNode | null>(null)
const treeLoading = ref(false)
const treeError = ref('')
/** 当前 tileset 实例：取其 modelMatrix 以正确解算 box/sphere 包围盒位置 */
let currentTileset: Cesium.Cesium3DTileset | null = null

/** 加载 tileset.json 并构建场景树（结构完全来自真实 tileset.json） */
async function loadSceneTree(svc: ServiceItem): Promise<void> {
  sceneTree.value = null
  treeError.value = ''
  if (!svc.tilesetUrl) return
  treeLoading.value = true
  try {
    const res = await fetch(svc.tilesetUrl)
    if (!res.ok) throw new Error(`tileset.json 不可读（HTTP ${res.status}）`)
    const json = (await res.json()) as unknown
    sceneTree.value = buildSceneTree(tilesetRootOf(json))
    if (!sceneTree.value) treeError.value = 'tileset.json 结构无法解析'
  } catch (e) {
    treeError.value = e instanceof Error ? e.message : '场景树读取失败'
  } finally {
    treeLoading.value = false
  }
}

/** 定位到场景树节点：解算包围盒中心（region/box/sphere）并飞行 */
function locateNode(node: TreeNode): void {
  if (!viewer) return
  const conv: EcefConvert = {
    cartesianToDegrees(x, y, z) {
      const carto = Cesium.Cartographic.fromCartesian(new Cesium.Cartesian3(x, y, z))
      if (!carto) return null
      return {
        lon: Cesium.Math.toDegrees(carto.longitude),
        lat: Cesium.Math.toDegrees(carto.latitude),
        height: carto.height,
      }
    },
    applyTransform(x, y, z) {
      const p = new Cesium.Cartesian3(x, y, z)
      const m = currentTileset?.modelMatrix ?? Cesium.Matrix4.IDENTITY
      const out = Cesium.Matrix4.multiplyByPoint(m, p, new Cesium.Cartesian3())
      return [out.x, out.y, out.z]
    },
  }
  const c = volumeCenter(node.boundingVolume, conv)
  if (!c) {
    treeError.value = `节点 ${node.name} 的包围盒无法解算定位`
    return
  }
  treeError.value = ''
  const altitude = c.height + Math.max(c.radius * 2.2, 200)
  viewer.camera.flyTo({
    destination: Cesium.Cartesian3.fromDegrees(c.lon, c.lat, altitude),
    orientation: { heading: 0, pitch: -Cesium.Math.PI_OVER_TWO, roll: 0 },
    duration: 1.2,
  })
}

/** 容错判定 3D Tiles 要素：除 instanceof 外兼容类实例跨模块引用的情况 */
function isTileFeature(p: unknown): p is Cesium.Cesium3DTileFeature {
  if (!Cesium.defined(p)) return false
  if (p instanceof Cesium.Cesium3DTileFeature) return true
  const f = p as { getPropertyIds?: unknown; getProperty?: unknown }
  return typeof f.getPropertyIds === 'function' && typeof f.getProperty === 'function'
}

/** 读取 feature 全部 batch table 属性 + BATCHID + 命中 tile 资源 uri；读不到的不编造 */
function buildFeatureInfo(feature: Cesium.Cesium3DTileFeature): PickInfo {
  const rows: PickRow[] = []
  const fx = feature as unknown as {
    featureId?: unknown
    _featureId?: unknown
    content?: {
      url?: string
      tile?: { content?: { uri?: string }; resource?: { getUrl?: () => string } }
    }
  }

  let ids: string[] = []
  try {
    ids = (feature.getPropertyIds() ?? []) as string[]
  } catch {
    ids = []
  }
  for (const id of ids) {
    let v: unknown
    try {
      v = feature.getProperty(id)
    } catch {
      v = undefined
    }
    const text = v === undefined || v === null ? '（读取失败）' : String(v)
    rows.push({ key: id, value: text, full: text })
  }

  // BATCHID：属性里没有时从 featureId 补充
  const batchId = fx.featureId ?? fx._featureId
  if (batchId !== undefined && batchId !== null && !ids.includes('BATCHID')) {
    const text = String(batchId)
    rows.unshift({ key: 'BATCHID', value: text, full: text })
  }

  // 命中 tile 的资源 uri：batch table 已带 _tile 属性时不重复展示
  const tileUri =
    fx.content?.url ?? fx.content?.tile?.content?.uri ?? fx.content?.tile?.resource?.getUrl?.()
  if (tileUri && !ids.includes('_tile')) {
    const text = String(tileUri)
    rows.unshift({ key: 'tile.uri', value: text, full: text })
  }

  if (rows.length === 0) {
    rows.push({ key: '属性', value: '（无属性）', full: '（无属性）' })
  }
  return { title: '3D Tiles 要素', rows }
}

let viewer: Cesium.Viewer | null = null
/**
 * 模板可用的 viewer 引用：MeasureTool 需要响应式传入。
 * 用 shallowRef：Viewer 是含大量 DOM 类型的巨型对象，深响应式
 * 既无意义又会让类型被 UnwrapRef 展开成畸变结构（导致赋值类型报错）。
 */
const viewerRef = shallowRef<Cesium.Viewer | null>(null)
let handler: Cesium.ScreenSpaceEventHandler | null = null
/** 监听 html[data-theme] 变化，使三维场景底色跟随主题切换 */
let themeObserver: MutationObserver | null = null

async function loadServiceList() {
  try {
    services.value = await listServices()
    listError.value = ''
  } catch (e) {
    listError.value = e instanceof Error ? e.message : '无法连接 API'
  } finally {
    loadingList.value = false
  }
}

/** 三维场景底色跟随 data-theme，主题切换时同步 */
function applySceneTheme(scene: Cesium.Scene): void {
  const dark = document.documentElement.getAttribute('data-theme') === 'dark'
  scene.globe.baseColor = Cesium.Color.fromCssColorString(dark ? '#101614' : '#e4e9e7')
  scene.backgroundColor = Cesium.Color.fromCssColorString(dark ? '#0d1211' : '#edf1ef')
}

function svcKind(svc: ServiceItem): 'image' | '3d' {
  return svc.wmtsUrl ? 'image' : '3d'
}

function onSvcClick(svc: ServiceItem) {
  if (svc.tilesetUrl) {
    void selectService(svc)
  } else {
    openDetail(svc)
  }
}

/* ------------------------------------------------------------------ */
/* 全屏模式（/preview/full）：3D 占满整页，左侧栏改为悬浮控制台        */
/* 功能不丢——服务选择、场景树、检查信息改为浮层，不占布局              */
/* ------------------------------------------------------------------ */
const showTree = ref(false)
const showInfo = ref(true)

/** 全屏模式下从悬浮下拉切换服务 */
function onHudSelect(taskId: string): void {
  const svc = services.value.find((s) => s.taskId === taskId)
  if (svc) onSvcClick(svc)
}

async function selectService(svc: ServiceItem) {
  if (!viewer || tilesetLoading.value) return
  activeService.value = svc
  tilesetError.value = ''
  pickInfo.value = null
  tilesetLoading.value = true

  const scene = viewer.scene
  // 清理上一个 tileset
  const old = scene.primitives.get(0)
  if (old instanceof Cesium.Cesium3DTileset) {
    scene.primitives.remove(old)
  }

  try {
    const tileset = await Cesium.Cesium3DTileset.fromUrl(svc.tilesetUrl, {
      maximumScreenSpaceError: 16,
    })
    // 用户在加载期间可能切换了服务
    if (activeService.value !== svc || !viewer) {
      return
    }
    viewer.scene.primitives.add(tileset)
    currentTileset = tileset
    await viewer.zoomTo(tileset)
    void loadSceneTree(svc)
  } catch (e) {
    tilesetError.value = e instanceof Error ? e.message : 'Tileset 加载失败'
  } finally {
    tilesetLoading.value = false
  }
}

/* ---------- WMTS 影像叠加 ---------- */

async function toggleOverlay(svc: ServiceItem, on: boolean) {
  if (!viewer || !svc.wmtsUrl || overlayBusy[svc.taskId]) return
  overlayError.value = ''

  if (!on) {
    const layer = overlayLayers.get(svc.taskId)
    if (layer) {
      viewer.imageryLayers.remove(layer, true)
      overlayLayers.delete(svc.taskId)
    }
    overlayOn[svc.taskId] = false
    return
  }

  overlayBusy[svc.taskId] = true
  try {
    // 先取 GetCapabilities：最大层级用于约束请求（超出金字塔层级的瓦片后端直接 400）
    let maxLevel: number | undefined
    try {
      const capRes = await fetch(svc.wmtsUrl)
      if (capRes.ok) {
        maxLevel = parseCapabilitiesLayer(await capRes.text(), svc.taskId).maxLevel
      }
    } catch {
      /* 元数据读取失败不阻塞叠加 */
    }
    const provider = new Cesium.WebMapTileServiceImageryProvider({
      url: wmtsKvpBaseUrl(svc.wmtsUrl),
      layer: svc.taskId,
      style: 'default',
      format: 'image/png',
      tileMatrixSetID: 'WebMercatorQuad',
      tilingScheme: new Cesium.WebMercatorTilingScheme(),
      maximumLevel: maxLevel,
    })
    const layer = viewer.imageryLayers.addImageryProvider(provider)
    overlayLayers.set(svc.taskId, layer)
    overlayOn[svc.taskId] = true
    await flyToWmtsLayer(svc)
  } catch (e) {
    overlayError.value = e instanceof Error ? e.message : 'WMTS 图层叠加失败'
    overlayOn[svc.taskId] = false
  } finally {
    overlayBusy[svc.taskId] = false
  }
}

/** 读取 GetCapabilities，定位到图层真实包围盒（定位失败不影响叠加本身） */
async function flyToWmtsLayer(svc: ServiceItem) {
  if (!viewer || !svc.wmtsUrl) return
  try {
    const res = await fetch(svc.wmtsUrl)
    if (!res.ok) return
    const info = parseCapabilitiesLayer(await res.text(), svc.taskId)
    if (!info.bbox || !viewer) return
    const { west, south, east, north } = info.bbox
    const padLon = Math.max((east - west) * 0.5, 0.0005)
    const padLat = Math.max((north - south) * 0.5, 0.0005)
    viewer.camera.flyTo({
      destination: Cesium.Rectangle.fromDegrees(
        west - padLon,
        south - padLat,
        east + padLon,
        north + padLat,
      ),
      duration: 1.2,
    })
  } catch {
    /* 包围盒解析失败：保持当前视角 */
  }
}

/* ---------- 服务详情面板 ---------- */

function openDetail(svc: ServiceItem) {
  detailService.value = svc
  detailTask.value = null
  if (!nowTimer) {
    nowSec.value = Date.now() / 1000
    nowTimer = setInterval(() => {
      nowSec.value = Date.now() / 1000
    }, 1000)
  }
  void loadDetailTask(svc)
}

function closeDetail() {
  detailService.value = null
  detailTask.value = null
  if (nowTimer) {
    clearInterval(nowTimer)
    nowTimer = null
  }
}

async function loadDetailTask(svc: ServiceItem) {
  detailLoading.value = true
  try {
    const task = await getTask(svc.taskId)
    // 面板可能已切换到其他服务
    if (detailService.value?.taskId === svc.taskId) detailTask.value = task
  } catch {
    /* 任务详情读取失败：面板对应行显示 — */
  } finally {
    detailLoading.value = false
  }
}

const detailUrls = computed(() => {
  const svc = detailService.value
  if (!svc) return []
  return [
    { key: 'tileset', label: 'Tileset URL (3D Tiles)', value: svc.tilesetUrl },
    { key: 'wmts', label: 'WMTS GetCapabilities', value: svc.wmtsUrl ?? '' },
    { key: 'tms', label: 'TMS URL 模板', value: svc.tmsUrl ?? '' },
  ]
})

/** 签名过期时间（unix 秒）：优先 tileset_url，影像服务取 wmts_url */
const detailExpiry = computed<number | null>(() => {
  const svc = detailService.value
  if (!svc) return null
  return parseSigExpiry(svc.tilesetUrl || svc.wmtsUrl || '')
})

const expiryText = computed(() => {
  const exp = detailExpiry.value
  if (exp == null) return { label: '未知', expired: false, has: false }
  const remain = exp - nowSec.value
  if (remain <= 0) return { label: '已过期', expired: true, has: true }
  const h = Math.floor(remain / 3600)
  const m = Math.floor((remain % 3600) / 60)
  const s = Math.floor(remain % 60)
  const pad = (v: number) => String(v).padStart(2, '0')
  const label = h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`
  return { label: `剩余 ${label}`, expired: false, has: true }
})

const expiryDateText = computed(() => {
  const exp = detailExpiry.value
  if (exp == null) return '—'
  return new Date(exp * 1000).toLocaleString('zh-CN', { hour12: false })
})

async function copyText(text: string, key: string) {
  try {
    await navigator.clipboard.writeText(text)
    copiedKey.value = key
    if (copiedTimer) clearTimeout(copiedTimer)
    copiedTimer = setTimeout(() => {
      copiedKey.value = ''
    }, 1600)
  } catch {
    /* 剪贴板不可用（非安全上下文等）：静默忽略 */
  }
}

/* ---------- 点击拾取 ---------- */

function setupPickHandler() {
  if (!viewer) return
  handler = new Cesium.ScreenSpaceEventHandler(viewer.scene.canvas)
  handler.setInputAction((movement: Cesium.ScreenSpaceEventHandler.PositionedEvent) => {
    if (!viewer) return
    pickInfo.value = null
    const scene = viewer.scene

    // drillPick 深入拾取：scene.pick 可能只命中 tileset/primitive 而拿不到要素
    let feature: Cesium.Cesium3DTileFeature | undefined
    try {
      for (const p of scene.drillPick(movement.position, 8) ?? []) {
        if (isTileFeature(p)) {
          feature = p
          break
        }
      }
    } catch {
      /* drillPick 异常时退回坐标拾取 */
    }
    if (feature) {
      pickInfo.value = buildFeatureInfo(feature)
      return
    }
    // 未命中要素：给出点击位置的经纬度/高程
    let cartesian: Cesium.Cartesian3 | undefined
    if (scene.pickPositionSupported) {
      cartesian = scene.pickPosition(movement.position) ?? undefined
    }
    if (!Cesium.defined(cartesian)) {
      cartesian = viewer.camera.pickEllipsoid(movement.position, scene.globe.ellipsoid) ?? undefined
    }
    if (!Cesium.defined(cartesian)) return
    const carto = Cesium.Cartographic.fromCartesian(cartesian)
    pickInfo.value = {
      title: '地表坐标',
      rows: [
        { key: '经度', value: `${Cesium.Math.toDegrees(carto.longitude).toFixed(6)}°`, full: '' },
        { key: '纬度', value: `${Cesium.Math.toDegrees(carto.latitude).toFixed(6)}°`, full: '' },
        { key: '高程', value: `${carto.height.toFixed(2)} m`, full: '' },
      ],
    }
  }, Cesium.ScreenSpaceEventType.LEFT_CLICK)
}

onMounted(async () => {
  void loadServiceList()

  viewer = new Cesium.Viewer('cesium-container', {
    // 不依赖 Cesium ion token：关闭默认影像与需要 ion 的控件
    baseLayer: false,
    baseLayerPicker: false,
    geocoder: false,
    homeButton: false,
    sceneModePicker: false,
    navigationHelpButton: false,
    animation: false,
    timeline: false,
    fullscreenButton: false,
    infoBox: false,
    selectionIndicator: false,
  })

  const scene = viewer.scene
  // 场景底色跟随主题：亮色主题下不再嵌一块纯黑，避免外壳与画布割裂
  applySceneTheme(scene)
  if (scene.skyBox) scene.skyBox.show = false
  if (scene.sun) scene.sun.show = false
  if (scene.moon) scene.moon.show = false

  // 基础相机定位：华北平原上空俯视
  viewer.camera.setView({
    destination: Cesium.Cartesian3.fromDegrees(116.39, 36.0, 6_000_000),
    orientation: {
      heading: 0,
      pitch: -Cesium.Math.PI_OVER_TWO,
      roll: 0,
    },
  })

  viewerRef.value = viewer
  setupPickHandler()
})

/** 服务列表与 viewer 任一就绪后尝试自动加载 ?task= 指定的服务 */
function tryAutoLoadRequested(): void {
  if (autoLoaded) return
  const id = requestedTaskId.value
  if (!id || !viewer || services.value.length === 0) return
  const svc = services.value.find((s) => s.taskId === id && s.tilesetUrl)
  if (!svc) return
  autoLoaded = true
  void selectService(svc)
}

watch([services, viewerRef], tryAutoLoadRequested)

onBeforeUnmount(() => {
  if (themeObserver) {
    themeObserver.disconnect()
    themeObserver = null
  }
  if (nowTimer) {
    clearInterval(nowTimer)
    nowTimer = null
  }
  if (copiedTimer) {
    clearTimeout(copiedTimer)
    copiedTimer = null
  }
  if (handler) {
    handler.destroy()
    handler = null
  }
  if (viewer) {
    viewer.destroy()
    viewer = null
  }
})
</script>

<template>
  <section class="preview-page" :class="{ bare: isBare }">
    <!-- 全屏模式不渲染左侧栏：3D 独占整页，等价功能移到悬浮控制台 -->
    <aside v-if="!isBare" class="svc-side">
      <div class="aside-actions">
        <a v-if="!isBare" class="btn btn-sm" href="/preview/full" target="_blank" rel="noopener">
          <AppIcon name="external" :size="12" />
          新窗口全屏
        </a>
        <a v-else class="btn btn-sm" href="/">
          <AppIcon name="chevron-right" :size="12" />
          返回控制台
        </a>
        <span class="aside-hint">{{ isBare ? '全屏预览模式' : '内嵌预览' }}</span>
      </div>
      <h1 class="page-title">3D 预览</h1>
      <p class="page-sub">已发布服务</p>

      <div v-if="loadingList" class="side-state">正在读取服务列表…</div>
      <div v-else-if="listError" class="side-state side-error">
        无法连接 API
        <span class="hint">{{ listError }}</span>
      </div>
      <div v-else-if="services.length === 0" class="side-state">
        暂无已发布服务
        <span class="hint">任务成功完成后将出现在这里</span>
      </div>

      <ul v-else class="svc-list">
        <li
          v-for="svc in services"
          :key="svc.taskId + (svc.tilesetUrl || svc.wmtsUrl)"
          class="svc-entry"
        >
          <label
            v-if="svc.wmtsUrl"
            class="svc-toggle"
            :title="overlayOn[svc.taskId] ? '移除叠加' : '叠加到地球'"
          >
            <input
              type="checkbox"
              :checked="!!overlayOn[svc.taskId]"
              :disabled="!!overlayBusy[svc.taskId]"
              @change="toggleOverlay(svc, ($event.target as HTMLInputElement).checked)"
            />
          </label>
          <button
            class="svc-item"
            :class="{ active: activeService?.taskId === svc.taskId && !!svc.tilesetUrl }"
            type="button"
            @click="onSvcClick(svc)"
          >
            <span class="svc-head">
              <span class="svc-kind" :class="svcKind(svc) === 'image' ? 'kind-image' : 'kind-3d'">
                {{ svcKind(svc) === 'image' ? '影像' : '3D' }}
              </span>
              <span class="svc-id">{{ svc.taskId || '未命名服务' }}</span>
            </span>
            <span class="svc-url" :title="svc.tilesetUrl || svc.wmtsUrl">
              {{ svc.tilesetUrl || svc.wmtsUrl }}
            </span>
          </button>
          <button
            class="svc-more"
            type="button"
            title="服务详情"
            @click="openDetail(svc)"
          >
            详情
          </button>
        </li>
      </ul>

      <div v-if="overlayError" class="side-state side-error overlay-err">
        WMTS 叠加失败
        <span class="hint">{{ overlayError }}</span>
      </div>

      <SceneTree
        v-if="activeService?.tilesetUrl"
        class="side-tree"
        :root="sceneTree"
        :loading="treeLoading"
        :error="treeError"
        @locate="locateNode"
      />

      <div v-if="pickInfo" class="pick-info">
        <div class="label">{{ pickInfo.title }}</div>
        <div v-for="(row, i) in pickInfo.rows" :key="i" class="pick-row">
          <span class="pick-key">{{ row.key }}</span>
          <span class="pick-val" :title="row.full || row.value">{{ row.value }}</span>
          <button
            v-if="row.full.length > 20"
            class="pick-copy"
            type="button"
            @click="copyText(row.full, PICK_COPY_PREFIX + i)"
          >
            {{ copiedKey === PICK_COPY_PREFIX + i ? '已复制' : '复制' }}
          </button>
        </div>
      </div>
    </aside>

    <div class="viewer-wrap">
      <div id="cesium-container" class="cesium-box" />

      <!-- 全屏悬浮控制台（替代左侧栏，不占布局） -->
      <div v-if="isBare" class="hud">
        <div class="hud-bar">
          <a class="hud-btn" href="/" title="返回控制台">
            <AppIcon name="chevron-right" :size="13" />
          </a>
          <select
            v-if="services.length > 0"
            class="hud-select"
            :value="activeService?.taskId ?? ''"
            title="选择服务"
            @change="onHudSelect(($event.target as HTMLSelectElement).value)"
          >
            <option value="" disabled>选择服务…</option>
            <option
              v-for="svc in services"
              :key="svc.taskId + (svc.tilesetUrl || svc.wmtsUrl)"
              :value="svc.taskId"
            >
              {{ svcKind(svc) === 'image' ? '[影像]' : '[3D]' }} {{ svc.taskId.slice(0, 8) }}
            </option>
          </select>
          <span v-else class="hud-empty">{{ loadingList ? '读取服务…' : '暂无已发布服务' }}</span>

          <button
            v-if="activeService?.tilesetUrl"
            class="hud-btn"
            :class="{ on: showTree }"
            type="button"
            title="场景树"
            @click="showTree = !showTree"
          >
            <AppIcon name="layers" :size="13" />
          </button>
          <button
            v-if="activeService"
            class="hud-btn"
            :class="{ on: showInfo }"
            type="button"
            title="检查信息"
            @click="showInfo = !showInfo"
          >
            <AppIcon name="search" :size="13" />
          </button>
        </div>

        <SceneTree
          v-if="showTree && activeService?.tilesetUrl"
          class="hud-panel"
          :root="sceneTree"
          :loading="treeLoading"
          :error="treeError"
          @locate="locateNode"
        />
      </div>

      <!-- 全屏模式的检查信息：左下浮层 -->
      <div v-if="isBare && showInfo && pickInfo" class="hud hud-info">
        <div class="label">{{ pickInfo.title }}</div>
        <div v-for="(row, i) in pickInfo.rows" :key="i" class="pick-row">
          <span class="pick-key">{{ row.key }}</span>
          <span class="pick-val" :title="row.full || row.value">{{ row.value }}</span>
          <button
            v-if="row.full.length > 20"
            class="pick-copy"
            type="button"
            @click="copyText(row.full, PICK_COPY_PREFIX + i)"
          >
            {{ copiedKey === PICK_COPY_PREFIX + i ? '已复制' : '复制' }}
          </button>
        </div>
      </div>

      <!-- 测量工具（F-05）：距离 / 面积 / 高程差 -->
      <div class="measure-float">
        <MeasureTool :viewer="viewerRef" />
      </div>

      <div v-if="tilesetLoading" class="viewer-banner">Tileset 加载中…</div>
      <div v-if="tilesetError" class="viewer-banner banner-error">
        Tileset 加载失败：{{ tilesetError }}
      </div>
      <div v-if="!activeService && !tilesetLoading && !tilesetError" class="viewer-banner banner-idle">
        {{ isBare ? '从左上角选择服务以加载 3D Tiles' : '点击左侧服务以加载 3D Tiles；勾选影像服务可叠加 WMTS 图层' }}
      </div>

      <!-- 服务详情浮层 -->
      <div v-if="detailService" class="detail-card">
        <div class="detail-head">
          <span class="detail-title serif">服务详情</span>
          <button class="detail-close" type="button" title="关闭" @click="closeDetail">×</button>
        </div>
        <dl class="detail-kv">
          <dt>task_id</dt>
          <dd class="mono">{{ detailService.taskId || '—' }}</dd>
          <dt>类型</dt>
          <dd>{{ detailService.type || (svcKind(detailService) === 'image' ? 'image->tiles' : '3D Tiles') }}</dd>
          <dt>创建时间</dt>
          <dd>{{ detailTask?.createdAt || (detailLoading ? '读取中…' : '—') }}</dd>
          <dt>更新时间</dt>
          <dd>{{ detailTask?.updatedAt || (detailLoading ? '读取中…' : '—') }}</dd>
          <dt>签名有效期</dt>
          <dd :class="{ 'exp-expired': expiryText.expired }">
            {{ expiryDateText }}<template v-if="expiryText.has">（{{ expiryText.label }}）</template>
          </dd>
        </dl>
        <div class="detail-urls">
          <div v-for="u in detailUrls" :key="u.key" class="url-row">
            <div class="url-label">{{ u.label }}</div>
            <div class="url-line">
              <span class="url-text" :title="u.value">{{ u.value || '—' }}</span>
              <button
                v-if="u.value"
                class="url-copy"
                type="button"
                @click="copyText(u.value, u.key)"
              >
                {{ copiedKey === u.key ? '已复制' : '复制' }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.preview-page {
  display: flex;
  gap: 24px;
  height: calc(100vh - 72px);
  min-height: 480px;
}

/* 全屏模式（/preview/full，新窗口打开）：3D 独占整个视口。
   无左侧栏、无内边距、无边框——页面里除了画布只有悬浮控件。 */
.preview-page.bare {
  height: 100vh;
  min-height: 0;
  gap: 0;
  padding: 0;
  box-sizing: border-box;
}
.preview-page.bare .viewer-wrap {
  border: none;
  background: var(--bg-sunken);
}

/* ---------- 全屏悬浮控制台 ---------- */
.hud {
  position: absolute;
  top: var(--sp-3);
  left: var(--sp-3);
  z-index: 6;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  max-width: 340px;
}
.hud-bar {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 5px 7px;
  background: var(--overlay-bg);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
  box-shadow: var(--shadow-pop);
}
.hud-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  flex: none;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  background: none;
  color: var(--ink-muted);
  cursor: pointer;
  transition: color var(--t-fast), background var(--t-fast), border-color var(--t-fast);
}
.hud-btn:hover {
  color: var(--ink);
  background: var(--surface-hover);
  border-color: var(--line);
  text-decoration: none;
}
.hud-btn.on {
  color: var(--accent-ink);
  border-color: color-mix(in oklch, var(--accent) 40%, var(--line));
  background: var(--accent-soft);
}
.hud-select {
  min-width: 170px;
  padding: 4px 8px;
  font-family: inherit;
  font-size: var(--fs-xs);
  color: var(--ink);
  background: var(--surface);
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-sm);
  cursor: pointer;
}
.hud-select:focus { outline: none; border-color: var(--primary); }
.hud-empty { padding: 0 6px; font-size: var(--fs-xs); color: var(--ink-faint); }
.hud-panel {
  padding: var(--sp-3);
  background: var(--overlay-bg);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
  box-shadow: var(--shadow-pop);
}
.hud-info {
  top: auto;
  bottom: var(--sp-3);
  max-width: 300px;
  padding: var(--sp-3);
  background: var(--overlay-bg);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
  box-shadow: var(--shadow-pop);
}

.aside-actions {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  margin-bottom: var(--sp-3);
}
.aside-hint { font-size: var(--fs-xs); color: var(--ink-faint); }

.svc-side {
  width: 300px;
  flex: none;
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.page-sub { margin-bottom: 18px; }

.side-state {
  padding: 16px;
  border: 1px solid var(--border);
  background: var(--surface);
  color: var(--ink-muted);
  font-size: 12px;
}
.side-error { color: var(--err); }
.side-state .hint { display: block; margin-top: 4px; color: var(--ink-faint); font-size: var(--fs-xs); }
.overlay-err { margin-top: 10px; }

.side-tree { margin-top: 14px; }

.measure-float {
  position: absolute;
  top: 14px;
  right: 14px;
  z-index: 5;
  padding: 8px 10px;
  background: var(--overlay-bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  box-shadow: var(--shadow-2);
}

.svc-list {
  list-style: none;
  margin: 0;
  padding: 0;
  overflow-y: auto;
  border: 1px solid var(--border);
  background: var(--surface);
}

.svc-entry {
  display: flex;
  align-items: stretch;
  border-bottom: 1px solid var(--border);
}
.svc-list li:last-child .svc-entry { border-bottom: none; }

.svc-toggle {
  display: flex;
  align-items: center;
  padding: 0 4px 0 10px;
  cursor: pointer;
}
.svc-toggle input {
  accent-color: var(--primary);
  cursor: pointer;
}

.svc-item {
  display: flex;
  flex-direction: column;
  gap: 3px;
  flex: 1;
  min-width: 0;
  padding: 11px 8px;
  text-align: left;
  font-family: var(--font-mono);
  background: transparent;
  border: none;
  border-left: 2px solid transparent;
  cursor: pointer;
  color: var(--ink);
  transition: background var(--t-fast), border-color var(--t-fast);
}
.svc-entry:hover .svc-item { background: var(--surface-hover); }
.svc-item.active {
  border-left-color: var(--primary);
  background: var(--surface-hover);
}
.svc-head {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.svc-id {
  font-size: 12px;
  color: var(--ink);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.svc-item.active .svc-id { color: var(--primary); }
.svc-url {
  font-size: var(--fs-xs);
  color: var(--ink-faint);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 类型徽标：3D / 影像 */
.svc-kind {
  flex: none;
  padding: 0 5px;
  font-size: var(--fs-xs);
  letter-spacing: 0.08em;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  line-height: 16px;
}
.kind-3d { color: var(--primary); border-color: var(--border-accent); }
.kind-image { color: var(--warn); border-color: color-mix(in oklch, var(--warn) 45%, transparent); }

.svc-more {
  flex: none;
  padding: 0 10px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  letter-spacing: 0.06em;
  color: var(--ink-faint);
  background: transparent;
  border: none;
  border-left: 1px solid var(--border);
  cursor: pointer;
  transition: color var(--t-fast), background var(--t-fast);
}
.svc-more:hover { color: var(--primary); background: var(--surface-hover); }

.pick-info {
  margin-top: auto;
  padding: 12px 14px;
  border: 1px solid var(--border-accent);
  background: var(--surface);
  font-size: var(--fs-xs);
  max-height: 46vh;
  overflow-y: auto;
}
.pick-row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  line-height: 1.9;
}
.pick-key {
  flex: none;
  color: var(--ink-faint);
  word-break: break-all;
}
.pick-key::before { content: '▸ '; color: var(--primary-dim); }
.pick-val {
  flex: 1;
  min-width: 0;
  color: var(--ink-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.pick-copy {
  flex: none;
  padding: 1px 8px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  letter-spacing: 0.05em;
  color: var(--primary);
  background: transparent;
  border: 1px solid var(--border-accent);
  border-radius: var(--radius);
  cursor: pointer;
  transition: background var(--t-fast), color var(--t-fast);
}
.pick-copy:hover { background: var(--primary-deep); color: var(--ink); }

.viewer-wrap {
  position: relative;
  flex: 1;
  min-width: 0;
  border: 1px solid var(--border);
  background: var(--bg-sunken);
}

.cesium-box {
  position: absolute;
  inset: 0;
}

/* 收敛 Cesium 自带控件风格 */
.cesium-box :deep(.cesium-viewer-bottom) { display: none; }
.cesium-box :deep(.cesium-widget-credits) { display: none !important; }

.viewer-banner {
  position: absolute;
  left: 50%;
  bottom: 18px;
  transform: translateX(-50%);
  padding: 7px 16px;
  font-size: var(--fs-xs);
  letter-spacing: 0.05em;
  color: var(--ink-muted);
  background: var(--overlay-bg);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-sm);
  pointer-events: none;
}
.banner-error { color: var(--err); border-color: color-mix(in oklch, var(--err) 45%, transparent); }
.banner-idle { color: var(--ink-faint); }

/* ---------- 服务详情浮层 ---------- */
.detail-card {
  position: absolute;
  top: 14px;
  right: 14px;
  width: 420px;
  max-width: calc(100% - 28px);
  max-height: calc(100% - 28px);
  overflow-y: auto;
  background: var(--overlay-bg);
  border: 1px solid var(--border-accent);
  border-radius: var(--radius);
  font-size: var(--fs-xs);
  z-index: 10;
}
.detail-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 14px;
  border-bottom: 1px solid var(--border);
}
.detail-title { font-size: 14px; color: var(--ink); }
.detail-close {
  padding: 0 6px;
  font-size: 16px;
  line-height: 1;
  color: var(--ink-faint);
  background: transparent;
  border: none;
  cursor: pointer;
  transition: color var(--t-fast);
}
.detail-close:hover { color: var(--ink); }

.detail-kv {
  display: grid;
  grid-template-columns: 92px 1fr;
  gap: 6px 14px;
  padding: 12px 14px;
  margin: 0;
}
.detail-kv dt {
  color: var(--ink-faint);
  white-space: nowrap;
}
.detail-kv dt::before { content: '▸ '; color: var(--primary-dim); }
.detail-kv dd {
  margin: 0;
  color: var(--ink);
  word-break: break-all;
}
.exp-expired { color: var(--err); }

.detail-urls {
  padding: 4px 14px 14px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.url-label {
  font-size: var(--fs-xs);
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--ink-faint);
  margin-bottom: 4px;
}
.url-line {
  display: flex;
  align-items: flex-start;
  gap: 8px;
}
.url-text {
  flex: 1;
  min-width: 0;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--ink-muted);
  background: var(--bg-sunken);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 3px 7px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.url-copy {
  flex: none;
  padding: 3px 9px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  letter-spacing: 0.05em;
  color: var(--primary);
  background: transparent;
  border: 1px solid var(--border-accent);
  border-radius: var(--radius);
  cursor: pointer;
  transition: background var(--t-fast), color var(--t-fast);
}
.url-copy:hover { background: var(--primary-deep); color: var(--ink); }
</style>
