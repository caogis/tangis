<script setup lang="ts">
/** 数据质检：选择任务 → 读取内核报告 → 指标读数 + 分块明细 */
import { computed, onMounted, ref } from 'vue'
import { ApiError, downloadFile, fetchWithAuth, listTasks, taskQcReportPath } from '../api/client'
import type { Task } from '../api/types'
import AppIcon from '../components/AppIcon.vue'
import { useToast } from '../composables/useToast'

interface Metrics {
  tileCount: number
  degenerate: number
  flipped: number
  floating: number
  cracks: number
  selfIntersect: number
  passed?: boolean
}

const toast = useToast()
const tasks = ref<Task[]>([])
const selectedId = ref('')
const loading = ref(true)
const loadingReport = ref(false)
const errorMsg = ref('')
const report = ref<Metrics | null>(null)
const tiles = ref<Record<string, unknown>[]>([])

const candidates = computed(() => tasks.value.filter((t) => t.status === 'SUCCEEDED'))
const issueTotal = computed(() => {
  const r = report.value
  if (!r) return 0
  return r.degenerate + r.flipped + r.floating + r.cracks + r.selfIntersect
})

function num(src: Record<string, unknown>, ...keys: string[]): number {
  for (const k of keys) {
    const v = src[k]
    if (typeof v === 'number' && Number.isFinite(v)) return v
    if (typeof v === 'string' && v.trim() !== '' && Number.isFinite(Number(v))) return Number(v)
  }
  return 0
}

function pickSummary(doc: Record<string, unknown>): Record<string, unknown> {
  const s = doc.summary ?? doc.Summary
  return s && typeof s === 'object' && !Array.isArray(s) ? (s as Record<string, unknown>) : doc
}

async function loadReport(): Promise<void> {
  const id = selectedId.value
  if (!id) return
  loadingReport.value = true
  errorMsg.value = ''
  report.value = null
  tiles.value = []
  try {
    let res: Response
    try {
      res = await fetchWithAuth(taskQcReportPath(id))
    } catch (err) {
      // 404 = 该任务没做过质检，给出可操作提示而不是裸 HTTP 码
      if (err instanceof ApiError && err.status === 404) {
        throw new Error('该任务没有质检报告，建任务时需开启「切片后自动质检」')
      }
      throw err
    }
    const doc = (await res.json()) as Record<string, unknown>
    const s = pickSummary(doc)
    report.value = {
      tileCount: num(s, 'tile_count', 'tileCount', 'tiles'),
      degenerate: num(s, 'total_degenerate', 'totalDegenerate', 'degenerate'),
      flipped: num(s, 'total_flipped_edges', 'totalFlippedEdges', 'flipped_edges'),
      floating: num(s, 'total_floating', 'totalFloating', 'floating'),
      cracks: num(s, 'total_crack_segments', 'totalCrackSegments', 'crack_segments'),
      selfIntersect: num(s, 'total_self_intersections', 'totalSelfIntersections'),
      passed: typeof s.passed === 'boolean' ? s.passed : undefined,
    }
    const arr = doc.tiles ?? doc.per_tile ?? doc.results
    tiles.value = Array.isArray(arr) ? (arr as Record<string, unknown>[]).slice(0, 50) : []
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '报告读取失败'
    toast.error(errorMsg.value)
  } finally {
    loadingReport.value = false
  }
}

function tileField(t: Record<string, unknown>, ...keys: string[]): string {
  for (const k of keys) {
    const v = t[k]
    if (typeof v === 'number' || typeof v === 'string') return String(v)
  }
  return '0'
}

/** 下载完整报告：必须走带鉴权的 fetch+blob（后端只认 X-API-Key 头） */
async function downloadReport(): Promise<void> {
  if (!selectedId.value) return
  try {
    await downloadFile(taskQcReportPath(selectedId.value), `qc-report-${selectedId.value.slice(0, 8)}.json`)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '下载失败')
  }
}

function selectTask(id: string): void {
  selectedId.value = id
  void loadReport()
}

onMounted(async () => {
  try {
    tasks.value = await listTasks()
    if (candidates.value.length > 0) {
      const withQc = candidates.value.find((t) => t.qcStatus)
      selectedId.value = (withQc ?? candidates.value[0]!).id
      await loadReport()
    }
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '无法连接服务'
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">数据质检</h1>
        <p class="page-sub">倾斜模型几何质检：退化面 / 法线翻转 / 悬浮块 / 跨块裂缝 / 自相交</p>
      </div>
      <div class="toolbar">
        <button v-if="report" class="btn btn-sm" type="button" @click="downloadReport">
          <AppIcon name="external" :size="12" />
          下载完整报告
        </button>
      </div>
    </header>

    <div class="workspace">
      <!-- 左：任务选择 -->
      <div class="panel pick-list">
        <div class="pick-list-head">选择切片任务</div>
        <div v-if="loading" class="pad"><div v-for="i in 4" :key="i" class="skeleton skel" /></div>
        <div v-else-if="candidates.length === 0" class="empty">
          没有可质检的任务
          <span class="hint">切片完成后才能质检</span>
        </div>
        <template v-else>
          <button
            v-for="t in candidates"
            :key="t.id"
            type="button"
            class="pick-item"
            :class="{ active: selectedId === t.id }"
            @click="selectTask(t.id)"
          >
            <AppIcon name="qc" :size="14" />
            <span class="grow">
              <span class="q-id mono">{{ t.id.slice(0, 10) }}</span>
              <span class="q-type">{{ t.type }}</span>
            </span>
            <span
              v-if="t.qcStatus"
              class="chip"
              :class="t.qcStatus === 'pass' ? 'chip-ready' : t.qcStatus === 'fail' ? 'chip-partial' : 'chip-planned'"
            >
              {{ t.qcStatus === 'pass' ? '通过' : t.qcStatus === 'fail' ? '未通过' : '跳过' }}
            </span>
            <span v-else class="chip chip-planned">未检</span>
          </button>
        </template>
      </div>

      <!-- 右：报告 -->
      <div>
        <div v-if="loadingReport" class="panel block pad">
          <div v-for="i in 3" :key="i" class="skeleton skel" />
        </div>

        <div v-else-if="errorMsg" class="error-box panel block">
          {{ errorMsg }}
          <span class="hint">在「切片转换」创建任务时开启「切片后自动质检」，完成后即可在此查看</span>
        </div>

        <template v-else-if="report">
          <div class="readout-bar block">
            <div class="readout">
              <span class="readout-label">质检结论</span>
              <span
                class="readout-value"
                :class="report.passed === false ? 'is-err' : report.passed === true ? 'is-ok' : ''"
                style="font-size: var(--fs-lg)"
              >
                {{ report.passed === true ? '通过' : report.passed === false ? '未通过' : '—' }}
              </span>
            </div>
            <div class="readout">
              <span class="readout-label">分块数</span>
              <span class="readout-value mono">{{ report.tileCount }}</span>
            </div>
            <div class="readout">
              <span class="readout-label">问题总数</span>
              <span class="readout-value mono" :class="{ 'is-err': issueTotal > 0 }">{{ issueTotal }}</span>
            </div>
          </div>

          <div class="panel block">
            <div class="panel-head">
              <span class="panel-title">问题分布</span>
              <span class="panel-meta">检测项关闭时不会伪造 0，而是留空</span>
            </div>
            <div class="thead data-grid metric-grid">
              <span>检测项</span><span class="ta-r">数量</span><span class="ta-r">状态</span>
            </div>
            <div
              v-for="m in [
                { name: '退化三角形', v: report.degenerate },
                { name: '法线翻转边', v: report.flipped },
                { name: '悬浮块', v: report.floating },
                { name: '跨块裂缝段', v: report.cracks },
                { name: '自相交', v: report.selfIntersect },
              ]"
              :key="m.name"
              class="data-row data-grid metric-grid"
            >
              <span class="metric-name">{{ m.name }}</span>
              <span class="num-strong ta-r">{{ m.v }}</span>
              <span class="ta-r">
                <span class="chip" :class="m.v > 0 ? 'chip-partial' : 'chip-ready'">
                  {{ m.v > 0 ? '需关注' : '正常' }}
                </span>
              </span>
            </div>
          </div>

          <div v-if="tiles.length > 0" class="panel">
            <div class="panel-head">
              <span class="panel-title">分块明细</span>
              <span class="panel-meta">前 {{ tiles.length }} 条</span>
            </div>
            <div class="thead data-grid tile-grid">
              <span>分块</span><span class="ta-r">退化</span><span class="ta-r">翻转</span>
              <span class="ta-r">悬浮</span><span class="ta-r">裂缝</span><span class="ta-r">自相交</span>
            </div>
            <div v-for="(t, i) in tiles" :key="i" class="data-row data-grid tile-grid">
              <span class="num">{{ tileField(t, 'tile', 'id', 'name') }}</span>
              <span class="num ta-r">{{ tileField(t, 'degenerate', 'total_degenerate') }}</span>
              <span class="num ta-r">{{ tileField(t, 'flipped_edges', 'total_flipped_edges') }}</span>
              <span class="num ta-r">{{ tileField(t, 'floating', 'total_floating') }}</span>
              <span class="num ta-r">{{ tileField(t, 'crack_segments', 'total_crack_segments') }}</span>
              <span class="num ta-r">{{ tileField(t, 'self_intersections', 'total_self_intersections') }}</span>
            </div>
          </div>
        </template>

        <div v-else class="empty panel">选择左侧任务以查看质检报告</div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.pad { padding: var(--sp-4); display: flex; flex-direction: column; gap: var(--sp-3); }
.skel { height: 16px; }

.q-id { display: block; font-family: var(--font-mono); font-size: var(--fs-xs); color: var(--ink); }
.q-type { display: block; font-size: var(--fs-xs); color: var(--ink-faint); }

.metric-grid { grid-template-columns: 1fr 90px 96px; }
.metric-name { color: var(--ink); }
.tile-grid { grid-template-columns: 1.6fr repeat(5, 1fr); }
</style>
