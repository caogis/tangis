<script setup lang="ts">
/**
 * 合规工具（F-21）：坐标系识别 / CGCS2000 七参数转换 / DEM 区域脱密。
 *
 * 三条能力的执行方式不同（对应后端设计）：
 *   - 识别与七参数是秒级算子 → 同步接口，结果直接回显；
 *   - DEM 脱密产物是 GeoTIFF + 留痕、可能跑很久 → 创建任务，跳任务详情跟进。
 */
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  bursaTransform,
  createTask,
  crsIdentify,
  taskArtifactPath,
  type BursaResult,
  type CrsIdentifyResult,
} from '../api/client'
import AppIcon from '../components/AppIcon.vue'
import { useToast } from '../composables/useToast'

const router = useRouter()
const toast = useToast()

/* ---------------- 1. 坐标系识别 ---------------- */

const crsPath = ref('/Users/yangtanfang/project/2026/AI/tangis/testdata/geoimage/small_rgb_4326.tif')
const crsBusy = ref(false)
const crsResult = ref<CrsIdentifyResult | null>(null)

const crsRows = computed(() => {
  const info = crsResult.value?.info
  if (!info) return [] as { k: string; v: string }[]
  return Object.entries(info).map(([k, v]) => ({
    k,
    v: v != null && typeof v === 'object' ? JSON.stringify(v) : String(v ?? '—'),
  }))
})

async function runCrsIdentify(): Promise<void> {
  const p = crsPath.value.trim()
  if (!p) {
    toast.info('请填写数据路径（GeoTIFF 或 .prj）')
    return
  }
  crsBusy.value = true
  try {
    crsResult.value = await crsIdentify(p)
    toast.success('识别完成')
  } catch (e) {
    crsResult.value = null
    toast.error(e instanceof Error ? e.message : '识别失败')
  } finally {
    crsBusy.value = false
  }
}

/* ---------------- 2. CGCS2000 七参数转换 ---------------- */

const bursaParams = ref({
  dx: '',
  dy: '',
  dz: '',
  rx: '',
  ry: '',
  rz: '',
  scale_ppm: '',
  source: 'EPSG:4490',
  target: 'EPSG:4326',
})
const pointsText = ref('116.39,39.90,50\n116.40,39.91,52')
const bursaBusy = ref(false)
const bursaResult = ref<BursaResult | null>(null)

const NUMERIC_BURSA_KEYS = ['dx', 'dy', 'dz', 'rx', 'ry', 'rz', 'scale_ppm'] as const

/** 解析点集文本：每行 `lon,lat,h`（逗号或空格分隔）。 */
function parsePoints(): number[][] {
  const lines = pointsText.value
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.length > 0)
  const out: number[][] = []
  lines.forEach((line, i) => {
    const parts = line.split(/[,\s]+/).filter((s) => s.length > 0).map(Number)
    if (parts.length < 3 || parts.slice(0, 3).some((n) => !Number.isFinite(n))) {
      throw new Error(`第 ${i + 1} 行格式错误，应为 lon,lat,h`)
    }
    out.push(parts.slice(0, 3))
  })
  return out
}

async function runBursa(): Promise<void> {
  let points: number[][]
  try {
    points = parsePoints()
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '点集格式错误')
    return
  }
  if (points.length === 0) {
    toast.info('请填写待转换点集')
    return
  }
  const params: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(bursaParams.value)) {
    if (String(v).trim() === '') continue
    if ((NUMERIC_BURSA_KEYS as readonly string[]).includes(k)) {
      const n = Number(v)
      if (!Number.isFinite(n)) {
        toast.error(`参数 ${k} 需为数字`)
        return
      }
      params[k] = n
    } else {
      params[k] = String(v).trim()
    }
  }
  if (params.dx === undefined && params.dy === undefined && params.dz === undefined) {
    toast.error('至少填写平移量 dx/dy/dz')
    return
  }

  bursaBusy.value = true
  try {
    bursaResult.value = await bursaTransform(params, points)
    toast.success(`转换完成（${points.length} 点）`)
  } catch (e) {
    bursaResult.value = null
    toast.error(e instanceof Error ? e.message : '转换失败')
  } finally {
    bursaBusy.value = false
  }
}

/* ---------------- 3. DEM 区域脱密 ---------------- */

const demPath = ref('/Users/yangtanfang/project/2026/AI/tangis/testdata/geo/dem_cgcs2000_u16.tif')
const demMode = ref<'flatten' | 'noise'>('noise')
const demDelta = ref('2')
const demSeed = ref('20261005')
const regionMode = ref<'bbox' | 'ring'>('bbox')
const regionBox = ref({ min_x: '116.0', min_y: '39.96', max_x: '116.02', max_y: '39.98' })
const ringText = ref('')
const demBusy = ref(false)

/** 解析多边形文本：每行 `x,y`，至少 3 个顶点。 */
function parseRing(): number[][] {
  const lines = ringText.value
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.length > 0)
  const ring: number[][] = []
  lines.forEach((line, i) => {
    const parts = line.split(/[,\s]+/).filter((s) => s.length > 0).map(Number)
    if (parts.length < 2 || !Number.isFinite(parts[0]) || !Number.isFinite(parts[1])) {
      throw new Error(`第 ${i + 1} 行格式错误，应为 x,y`)
    }
    ring.push([parts[0], parts[1]])
  })
  if (ring.length < 3) throw new Error('多边形至少需要 3 个顶点')
  return ring
}

async function createDesensitizeTask(): Promise<void> {
  const src = demPath.value.trim()
  if (!src) {
    toast.info('请填写 DEM 路径')
    return
  }

  let region: Record<string, unknown>
  if (regionMode.value === 'bbox') {
    const b = regionBox.value
    const nums = [b.min_x, b.min_y, b.max_x, b.max_y].map((v) => Number(v))
    if (nums.some((n) => !Number.isFinite(n))) {
      toast.error('区域四至需为数字')
      return
    }
    if (nums[2] <= nums[0] || nums[3] <= nums[1]) {
      toast.error('区域范围不合法（max 必须大于 min）')
      return
    }
    region = { min_x: nums[0], min_y: nums[1], max_x: nums[2], max_y: nums[3] }
  } else {
    try {
      region = { ring: parseRing() }
    } catch (e) {
      toast.error(e instanceof Error ? e.message : '多边形格式错误')
      return
    }
  }

  if (demMode.value === 'noise' && Number(demDelta.value) <= 0) {
    toast.error('noise 模式必须给出大于 0 的噪声界 delta')
    return
  }

  const params: Record<string, unknown> = { region, mode: demMode.value }
  if (String(demDelta.value).trim() !== '') params.delta = String(demDelta.value).trim()
  if (String(demSeed.value).trim() !== '') params.seed = String(demSeed.value).trim()

  demBusy.value = true
  try {
    const t = await createTask({ type: 'dem->desensitized', source: src, params })
    toast.success('脱密任务已创建，可在任务详情下载产物与留痕')
    void router.push(`/tasks/${encodeURIComponent(t.id)}`)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '创建脱密任务失败')
  } finally {
    demBusy.value = false
  }
}

/** 脱密产物下载地址构造（供说明文案展示） */
const artifactHint = computed(() => taskArtifactPath('{task_id}', 'desensitized.tif'))
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">合规工具</h1>
        <p class="page-sub">坐标系识别 · CGCS2000 七参数转换 · DEM 区域脱密（F-21）</p>
      </div>
    </header>

    <div class="cols block">
      <!-- 1. 坐标系识别 -->
      <div class="panel">
        <div class="panel-head">
          <span class="panel-title">坐标系识别</span>
          <span class="panel-meta">内核 crs-identify</span>
        </div>
        <div class="form-body">
          <div class="field">
            <label class="label">数据路径</label>
            <input v-model="crsPath" class="input mono" placeholder="/data/dem.tif 或 /data/proj.prj" />
            <span class="hint">支持 GeoTIFF（GeoKey）与 ESRI WKT .prj，只读识别、不改动数据</span>
          </div>
          <div class="form-actions">
            <button class="btn btn-solid" type="button" :disabled="crsBusy" @click="runCrsIdentify">
              <AppIcon name="search" :size="13" />
              {{ crsBusy ? '识别中…' : '识别坐标系' }}
            </button>
          </div>

          <div v-if="crsResult" class="result">
            <div v-if="crsResult.georef" class="geo-line mono">{{ crsResult.georef }}</div>
            <dl v-if="crsRows.length > 0" class="kv">
              <template v-for="r in crsRows" :key="r.k">
                <dt>{{ r.k }}</dt>
                <dd class="mono">{{ r.v }}</dd>
              </template>
            </dl>
            <details class="raw">
              <summary>内核原始输出</summary>
              <pre class="mono">{{ crsResult.output }}</pre>
            </details>
          </div>
        </div>
      </div>

      <!-- 2. 七参数转换 -->
      <div class="panel">
        <div class="panel-head">
          <span class="panel-title">CGCS2000 七参数转换</span>
          <span class="panel-meta">Bursa-Wolf · Position Vector</span>
        </div>
        <div class="form-body">
          <div class="grid-params">
            <div v-for="k in ['dx', 'dy', 'dz']" :key="k" class="field">
              <label class="label">{{ k }}（米）</label>
              <input v-model="bursaParams[k as 'dx']" class="input mono" placeholder="0" />
            </div>
            <div v-for="k in ['rx', 'ry', 'rz']" :key="k" class="field">
              <label class="label">{{ k }}（角秒）</label>
              <input v-model="bursaParams[k as 'rx']" class="input mono" placeholder="0" />
            </div>
            <div class="field">
              <label class="label">scale_ppm</label>
              <input v-model="bursaParams.scale_ppm" class="input mono" placeholder="0" />
            </div>
            <div class="field">
              <label class="label">source</label>
              <input v-model="bursaParams.source" class="input mono" />
            </div>
            <div class="field">
              <label class="label">target</label>
              <input v-model="bursaParams.target" class="input mono" />
            </div>
          </div>

          <div class="field">
            <label class="label">待转换点集（每行 lon,lat,h）</label>
            <textarea v-model="pointsText" class="textarea mono" rows="5" />
            <span class="hint">上限 200000 点；参数与点集会落盘到数据目录便于复核</span>
          </div>

          <div class="form-actions">
            <button class="btn btn-solid" type="button" :disabled="bursaBusy" @click="runBursa">
              <AppIcon name="convert" :size="13" />
              {{ bursaBusy ? '转换中…' : '执行转换' }}
            </button>
          </div>

          <div v-if="bursaResult" class="result">
            <div class="geo-line mono">{{ bursaResult.summary }}</div>
            <details v-if="bursaResult.result" class="raw" open>
              <summary>转换结果 JSON</summary>
              <pre class="mono">{{ JSON.stringify(bursaResult.result, null, 2) }}</pre>
            </details>
          </div>
        </div>
      </div>
    </div>

    <!-- 3. DEM 脱密 -->
    <div class="panel block">
      <div class="panel-head">
        <span class="panel-title">DEM 区域脱密</span>
        <span class="panel-meta">内核 desensitize-dem · 生成任务，产物可在任务详情下载</span>
      </div>
      <div class="form-body">
        <div class="grid-params">
          <div class="field wide">
            <label class="label">DEM 路径</label>
            <input v-model="demPath" class="input mono" placeholder="/data/dem.tif" />
            <span class="hint">单波段 GeoTIFF（uint16 / float32）</span>
          </div>
          <div class="field">
            <label class="label">脱密模式</label>
            <select v-model="demMode" class="select">
              <option value="flatten">flatten · 置平到区域均值 + delta</option>
              <option value="noise">noise · delta 有界受控噪声</option>
            </select>
          </div>
          <div class="field">
            <label class="label">delta（米）</label>
            <input v-model="demDelta" class="input mono" placeholder="2" />
            <span class="hint">flatten：偏移量；noise：噪声界（必填 &gt; 0）</span>
          </div>
          <div class="field">
            <label class="label">seed（noise 必填）</label>
            <input v-model="demSeed" class="input mono" placeholder="20261005" />
            <span class="hint">固定种子保证脱密结果可复现</span>
          </div>
        </div>

        <div class="field">
          <label class="label">脱密区域</label>
          <div class="segmented small">
            <button type="button" :class="{ active: regionMode === 'bbox' }" @click="regionMode = 'bbox'">
              矩形四至
            </button>
            <button type="button" :class="{ active: regionMode === 'ring' }" @click="regionMode = 'ring'">
              多边形
            </button>
          </div>
        </div>

        <div v-if="regionMode === 'bbox'" class="grid-params">
          <div v-for="k in ['min_x', 'min_y', 'max_x', 'max_y']" :key="k" class="field">
            <label class="label mono">{{ k }}</label>
            <input v-model="regionBox[k as 'min_x']" class="input mono" />
          </div>
        </div>
        <div v-else class="field">
          <label class="label">多边形顶点（每行 x,y，至少 3 个）</label>
          <textarea v-model="ringText" class="textarea mono" rows="4" placeholder="116.00,39.96&#10;116.02,39.96&#10;116.02,39.98" />
        </div>

        <div class="form-actions">
          <button class="btn btn-solid" type="button" :disabled="demBusy" @click="createDesensitizeTask">
            <AppIcon name="qc" :size="13" />
            {{ demBusy ? '创建中…' : '创建脱密任务' }}
          </button>
          <span class="hint">
            任务完成后可在任务详情下载 <span class="mono">{{ artifactHint }}</span> 与脱密留痕 JSON
          </span>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.cols { display: grid; grid-template-columns: 1fr 1.25fr; gap: var(--sp-4); align-items: start; }
@media (max-width: 1100px) { .cols { grid-template-columns: 1fr; } }

.form-body { padding: var(--sp-4); }
.grid-params {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: var(--sp-3);
  margin-bottom: var(--sp-4);
}
.field.wide { grid-column: 1 / -1; }
.textarea {
  width: 100%;
  padding: var(--sp-2) var(--sp-3);
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--ink);
  background: var(--surface);
  border: 1px solid var(--line-strong);
  border-radius: var(--radius);
  resize: vertical;
}
.textarea:focus { outline: none; border-color: var(--primary); }

.segmented.small { margin-bottom: var(--sp-2); }

.result {
  margin-top: var(--sp-4);
  padding: var(--sp-3);
  background: var(--bg-sunken);
  border: 1px solid var(--line);
  border-radius: var(--radius);
}
.geo-line { font-size: var(--fs-xs); color: var(--accent-ink); margin-bottom: var(--sp-2); }
.kv dd { font-size: var(--fs-xs); }

.raw { margin-top: var(--sp-2); }
.raw summary { font-size: var(--fs-xs); color: var(--ink-faint); cursor: pointer; }
.raw pre {
  margin: var(--sp-2) 0 0;
  padding: var(--sp-2);
  max-height: 260px;
  overflow: auto;
  font-size: var(--fs-xs);
  line-height: 1.5;
  color: var(--ink-muted);
  background: var(--surface);
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
}
</style>
