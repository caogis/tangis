<script setup lang="ts">
/**
 * 测量工具（F-05）：距离 / 面积 / 高程差。
 *
 * 交互：选择模式后在地球上依次点击采点，双击结束（也可点「结束」按钮）。
 * 计算：距离用测地线（EllipsoidGeodesic），面积用扇形三角化 + 海伦公式，
 * 高程差取两点椭球高之差。所有结果实时显示，清空即移除绘制要素。
 */
import { onBeforeUnmount, ref, watch } from 'vue'
import * as Cesium from 'cesium'

type Mode = 'off' | 'distance' | 'area' | 'height'

const props = defineProps<{ viewer: Cesium.Viewer | null }>()

const mode = ref<Mode>('off')
const points = ref<Cesium.Cartesian3[]>([])
const result = ref('')
const hint = ref('')

let handler: Cesium.ScreenSpaceEventHandler | null = null
let entities: Cesium.Entity[] = []
let movingEntity: Cesium.Entity | null = null

const MODES: { id: Exclude<Mode, 'off'>; label: string }[] = [
  { id: 'distance', label: '距离' },
  { id: 'area', label: '面积' },
  { id: 'height', label: '高程差' },
]

function cartoOf(p: Cesium.Cartesian3): Cesium.Cartographic | null {
  const c = Cesium.Cartographic.fromCartesian(p)
  return c ?? null
}

/** 两点间测地线距离（米） */
function geodesicDistance(a: Cesium.Cartesian3, b: Cesium.Cartesian3): number {
  const ca = cartoOf(a)
  const cb = cartoOf(b)
  if (!ca || !cb) return Cesium.Cartesian3.distance(a, b)
  const geo = new Cesium.EllipsoidGeodesic(ca, cb)
  return geo.surfaceDistance
}

/** 球面三角形面积（海伦公式，边长取测地线） */
function triangleArea(a: Cesium.Cartesian3, b: Cesium.Cartesian3, c: Cesium.Cartesian3): number {
  const ab = geodesicDistance(a, b)
  const bc = geodesicDistance(b, c)
  const ca = geodesicDistance(c, a)
  const s = (ab + bc + ca) / 2
  const v = s * (s - ab) * (s - bc) * (s - ca)
  return v > 0 ? Math.sqrt(v) : 0
}

function fmtDistance(m: number): string {
  if (m >= 1000) return `${(m / 1000).toFixed(3)} km`
  return `${m.toFixed(2)} m`
}

function fmtArea(m2: number): string {
  if (m2 >= 1_000_000) return `${(m2 / 1_000_000).toFixed(3)} km²`
  return `${m2.toFixed(2)} m²`
}

function recompute(): void {
  const pts = points.value
  if (pts.length < 2) {
    result.value = ''
    return
  }
  if (mode.value === 'distance') {
    let total = 0
    for (let i = 1; i < pts.length; i++) total += geodesicDistance(pts[i - 1]!, pts[i]!)
    result.value = `折线距离 ${fmtDistance(total)}（${pts.length} 点，${pts.length - 1} 段）`
  } else if (mode.value === 'area') {
    if (pts.length < 3) {
      result.value = '至少 3 个点才能计算面积'
      return
    }
    let area = 0
    for (let i = 1; i < pts.length - 1; i++) {
      area += triangleArea(pts[0]!, pts[i]!, pts[i + 1]!)
    }
    const perimeter = pts.reduce((acc, p, i) => acc + (i === 0 ? 0 : geodesicDistance(pts[i - 1]!, p)), 0)
    result.value = `面积 ${fmtArea(area)} · 周长 ${fmtDistance(perimeter)}`
  } else if (mode.value === 'height') {
    const a = cartoOf(pts[0]!)
    const b = cartoOf(pts[pts.length - 1]!)
    if (!a || !b) {
      result.value = '高程读取失败'
      return
    }
    result.value = `起点 ${a.height.toFixed(2)} m → 终点 ${b.height.toFixed(2)} m · 高差 ${(b.height - a.height).toFixed(2)} m`
  }
}

function redraw(): void {
  const viewer = props.viewer
  if (!viewer) return
  for (const e of entities) viewer.entities.remove(e)
  entities = []

  const pts = points.value
  for (const p of pts) {
    entities.push(
      viewer.entities.add({
        position: p,
        point: {
          pixelSize: 7,
          color: Cesium.Color.fromCssColorString('#7fb08a'),
          outlineColor: Cesium.Color.fromCssColorString('#0d1211'),
          outlineWidth: 2,
          disableDepthTestDistance: Number.POSITIVE_INFINITY,
        },
      }),
    )
  }

  if (pts.length >= 2 && mode.value === 'distance') {
    entities.push(
      viewer.entities.add({
        polyline: {
          positions: pts,
          width: 2,
          material: Cesium.Color.fromCssColorString('#7fb08a'),
          clampToGround: false,
        },
      }),
    )
  } else if (pts.length >= 3 && mode.value === 'area') {
    entities.push(
      viewer.entities.add({
        polygon: {
          hierarchy: pts,
          material: Cesium.Color.fromCssColorString('#7fb08a').withAlpha(0.25),
          outline: true,
          outlineColor: Cesium.Color.fromCssColorString('#7fb08a'),
        },
      }),
    )
  } else if (pts.length >= 2 && mode.value === 'height') {
    // 高差：起点到终点的连线 + 两端竖直参考线
    entities.push(
      viewer.entities.add({
        polyline: {
          positions: [pts[0]!, pts[pts.length - 1]!],
          width: 2,
          material: Cesium.Color.fromCssColorString('#c8a86b'),
        },
      }),
    )
  }
}

function pickPosition(movement: { position?: Cesium.Cartesian2 }): Cesium.Cartesian3 | null {
  const viewer = props.viewer
  if (!viewer || !movement.position) return null
  const scene = viewer.scene
  // 优先拾取三维表面（含 3DTiles），失败回落到椭球面
  if (scene.pickPositionSupported) {
    const p = scene.pickPosition(movement.position)
    if (p) return p
  }
  const ray = viewer.camera.getPickRay(movement.position)
  if (!ray) return null
  return viewer.scene.globe.pick(ray, scene) ?? null
}

function clearAll(): void {
  points.value = []
  result.value = ''
  redraw()
}

function finish(): void {
  teardownHandler()
  mode.value = 'off'
  hint.value = ''
  // 保留绘制结果与本次结果文字，便于查看
}

function teardownHandler(): void {
  if (handler) {
    handler.destroy()
    handler = null
  }
  const viewer = props.viewer
  if (viewer && movingEntity) {
    viewer.entities.remove(movingEntity)
    movingEntity = null
  }
}

function setupHandler(): void {
  const viewer = props.viewer
  if (!viewer) return
  teardownHandler()
  // 用 scene.canvas 而非 viewer.canvas：与预览页既有写法一致，
  // 规避 Cesium 自带 DOM 类型与 TS lib.dom 的类型冲突
  handler = new Cesium.ScreenSpaceEventHandler(viewer.scene.canvas)
  handler.setInputAction((movement: { position?: Cesium.Cartesian2 }) => {
    const p = pickPosition(movement)
    if (!p) {
      hint.value = '该位置无法拾取（试试模型表面或已加载的地形）'
      return
    }
    points.value = [...points.value, p]
    hint.value = ''
    redraw()
    recompute()
  }, Cesium.ScreenSpaceEventType.LEFT_CLICK)

  handler.setInputAction(() => {
    if (points.value.length >= 2) finish()
  }, Cesium.ScreenSpaceEventType.LEFT_DOUBLE_CLICK)
}

function selectMode(next: Exclude<Mode, 'off'>): void {
  if (mode.value === next) {
    finish()
    return
  }
  clearAll()
  mode.value = next
  hint.value = next === 'height' ? '点击起点与终点' : '依次点击采点，双击结束'
  setupHandler()
}

function stop(): void {
  teardownHandler()
  mode.value = 'off'
  clearAll()
  hint.value = ''
}

watch(
  () => props.viewer,
  (v) => {
    if (!v) {
      teardownHandler()
      entities = []
    }
  },
)

onBeforeUnmount(() => {
  teardownHandler()
  const viewer = props.viewer
  if (viewer) {
    for (const e of entities) viewer.entities.remove(e)
  }
  entities = []
})
</script>

<template>
  <div class="measure">
    <div class="measure-bar">
      <span class="label">测量</span>
      <button
        v-for="m in MODES"
        :key="m.id"
        type="button"
        class="mini"
        :class="{ active: mode === m.id }"
        @click="selectMode(m.id)"
      >
        {{ m.label }}
      </button>
      <button v-if="mode !== 'off'" type="button" class="mini" @click="clearAll">清空点</button>
      <button v-if="mode !== 'off'" type="button" class="mini" @click="stop">结束</button>
    </div>
    <div v-if="hint" class="measure-hint">{{ hint }}</div>
    <div v-if="result" class="measure-result">{{ result }}</div>
  </div>
</template>

<style scoped>
.measure {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.measure-bar {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.label {
  font-size: var(--fs-xs);
  letter-spacing: 0.12em;
  color: var(--ink-faint);
  text-transform: uppercase;
}
.mini {
  padding: 3px 9px;
  border: 1px solid var(--border);
  border-radius: 2px;
  background: var(--bg-sunken);
  color: var(--ink-faint);
  font-size: var(--fs-xs);
  cursor: pointer;
}
.mini:hover { color: var(--ink); }
.mini.active {
  color: var(--primary);
  border-color: var(--primary-dim);
  background: var(--surface);
}
.measure-hint { font-size: var(--fs-xs); color: var(--ink-faint); }
.measure-result {
  font-size: var(--fs-xs);
  color: var(--primary);
  font-variant-numeric: tabular-nums;
}
</style>
