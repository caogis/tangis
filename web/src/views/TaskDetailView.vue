<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  approveTask,
  controlTask,
  createEditTask,
  deleteTask,
  downloadFile,
  getTask,
  listServices,
  taskArtifactPath,
  taskLogsPath,
  taskOpsReportPath,
  taskQcReportPath,
} from '../api/client'
import {
  canCancel,
  canPause,
  canResume,
  canRetry,
  isTerminal,
  type EditOp,
  type EditTaskPayload,
  type Task,
  type TaskAction,
} from '../api/types'
import { computeTilesetStats, tilesetRootOf, type TilesetStats } from '../utils/tileset'
import StatusBadge from '../components/StatusBadge.vue'
import AppIcon from '../components/AppIcon.vue'
import { useToast } from '../composables/useToast'
import { taskPreviewUrl } from '../utils/preview'

const route = useRoute()
const router = useRouter()
const task = ref<Task | null>(null)
const loading = ref(true)
const errorMsg = ref('')
const POLL_MS = 5000
let timer: ReturnType<typeof setInterval> | null = null

/* 切片产物统计（真实数据：来自任务产物 tileset.json，不估算） */
const stats = ref<TilesetStats | null>(null)
const statsState = ref<'idle' | 'loading' | 'ok' | 'unavailable' | 'error'>('idle')
const statsNote = ref('')

async function loadTilesetStats(taskId: string) {
  statsState.value = 'loading'
  statsNote.value = ''
  try {
    const services = await listServices()
    const svc = services.find((s) => s.taskId === taskId)
    if (!svc || !svc.tilesetUrl) {
      statsState.value = 'unavailable'
      statsNote.value = svc
        ? '影像瓦片任务无 tileset 产物，后端也未提供统计接口'
        : '该任务未发布 tileset 产物，后端未提供统计接口'
      return
    }
    const res = await fetch(svc.tilesetUrl)
    if (!res.ok) throw new Error(`HTTP ${res.status}`)
    stats.value = computeTilesetStats(tilesetRootOf(await res.json()))
    statsState.value = 'ok'
  } catch (e) {
    statsState.value = 'error'
    statsNote.value = e instanceof Error ? e.message : '读取 tileset.json 失败'
  }
}

/* 下载入口：后端只认 X-API-Key 请求头，`<a download>` 带不上头会 401，
   因此统一走 client.downloadFile（fetch + blob）。 */
async function downloadLogs(): Promise<void> {
  if (!task.value) return
  try {
    await downloadFile(taskLogsPath(task.value.id), `tangis-${task.value.id.slice(0, 8)}.log`)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '日志下载失败')
  }
}

/* 质检卡片（M2-F08b）：徽标 + 摘要 + 完整报告下载 */
const qcBadge = computed(() => {
  switch (task.value?.qcStatus) {
    case 'pass': return { cls: 'badge-qc-pass', label: 'PASS' }
    case 'fail': return { cls: 'badge-qc-fail', label: 'FAIL' }
    case 'skipped': return { cls: 'badge-qc-none', label: '已跳过' }
    default: return { cls: 'badge-qc-none', label: '未检' }
  }
})
/** 仅真实跑过质检（pass/fail）才有报告文件；skipped/未检不提供下载入口 */
const hasQcReport = computed(
  () => task.value?.qcStatus === 'pass' || task.value?.qcStatus === 'fail',
)

async function downloadQcReport(): Promise<void> {
  if (!task.value) return
  try {
    await downloadFile(taskQcReportPath(task.value.id), `qc-report-${task.value.id.slice(0, 8)}.json`)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '质检报告下载失败')
  }
}

/* ------------------------------------------------------------------ */
/* 编辑任务（M2-F09c）                                                  */
/* ------------------------------------------------------------------ */

/** 是否为编辑任务（parent_task_id 非空） */
const isEditTask = computed(() => !!task.value?.parentTaskId)
const parentHref = computed(() =>
  task.value?.parentTaskId ? `/tasks/${task.value.parentTaskId}` : '',
)

/** 可发起编辑：3D 切片任务（含编辑任务的链式再编辑）+ SUCCEEDED + 已审批 */
const canEdit = computed(() => {
  const t = task.value
  if (!t || t.status !== 'SUCCEEDED' || !t.approved) return false
  if (t.parentTaskId) return true
  return t.type.includes('3dtiles') || t.type === 't3d-import'
})

function paramStr(key: string): string {
  const v = task.value?.params?.[key]
  return v === undefined || v === null ? '' : String(v)
}

/** 编辑任务的 op 与参数行（真实来自任务 params，无则不显示该行） */
const editParamRows = computed(() => {
  const rows: Array<{ k: string; v: string }> = []
  const op = paramStr('edit_op')
  if (op) rows.push({ k: '操作', v: op })
  const tile = paramStr('edit_tile') || 'all'
  rows.push({ k: '瓦片', v: tile })
  const bbox = paramStr('edit_bbox')
  if (bbox) rows.push({ k: 'bbox 四至', v: bbox })
  const plane = paramStr('edit_plane')
  if (plane) rows.push({ k: '切割面', v: plane })
  const elev = task.value?.params?.edit_elevation
  if (elev !== undefined) rows.push({ k: '高程', v: String(elev) })
  const feather = task.value?.params?.edit_feather
  if (feather !== undefined) rows.push({ k: '羽化', v: String(feather) })
  return rows
})

/** ops.json 留痕摘要：优先展示 totals 子对象，否则整体标量键值（不臆造） */
const opsRows = computed(() => {
  const s = task.value?.opsSummary
  if (!s) return []
  const src = (s.totals && typeof s.totals === 'object' && !Array.isArray(s.totals)
    ? s.totals
    : s) as Record<string, unknown>
  return Object.entries(src)
    .filter(([, v]) => ['string', 'number', 'boolean'].includes(typeof v))
    .map(([k, v]) => ({ k, v: String(v) }))
})

const hasOpsReport = computed(() => !!task.value?.opsSummary)

/** DEM 脱密任务（F-21）：产物是 GeoTIFF 与留痕 JSON，走 artifact 下载通道 */
const isDesensitizeTask = computed(() => task.value?.type === 'dem->desensitized')

async function downloadArtifact(name: string): Promise<void> {
  if (!task.value) return
  try {
    await downloadFile(taskArtifactPath(task.value.id, name), name)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '产物下载失败')
  }
}

async function downloadOpsReport(): Promise<void> {
  if (!task.value) return
  try {
    await downloadFile(taskOpsReportPath(task.value.id), `ops-${task.value.id.slice(0, 8)}.json`)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '留痕下载失败')
  }
}

/* 产物预览入口：复用现有 3D 预览页（服务列表中选择本任务） */
// 新页面全屏预览：带 ?task=，预览页就绪后自动加载本任务的服务
const previewHref = computed(() =>
  task.value ? taskPreviewUrl(task.value.id) : '/preview/full',
)

/* ---- 发起编辑表单 ---- */
const EDIT_OPS: Array<{ value: EditOp; label: string; hint: string }> = [
  { value: 'clip', label: 'clip（裁剪）', hint: '需 plane 或 bbox（二者互斥）' },
  { value: 'flatten', label: 'flatten（压平）', hint: '需 bbox + elevation' },
  { value: 'ground-align', label: 'ground-align（贴地）', hint: '需 bbox + elevation' },
]
const showEditForm = ref(false)
const submitting = ref(false)
const formError = ref('')
const form = reactive({
  op: 'clip' as EditOp,
  tile: '',
  clipMode: 'bbox' as 'bbox' | 'plane',
  minx: '', miny: '', maxx: '', maxy: '',
  plane: '',
  elevation: '',
  feather: '',
})

const BBOX_DEFAULTS = { minx: '0', miny: '0', maxx: '100', maxy: '100' }
watch(showEditForm, (on) => {
  if (on && !form.minx) Object.assign(form, BBOX_DEFAULTS)
})

const PLANE_RE = /^[+-]?(?:\d+\.?\d*|\.\d+)x\s*[+-](?:\d+\.?\d*|\.\d+)y\s*[+-](?:\d+\.?\d*|\.\d+)z\s*=\s*[+-]?(?:\d+\.?\d*|\.\d+)$/

/** 前端校验（与 server 同规则）：合法返回请求体，非法返回错误文案。严禁静默放行 */
function buildEditPayload(): EditTaskPayload | string {
  const num = (s: string) => s.trim() !== '' && Number.isFinite(Number(s))
  let bbox = ''
  if (form.op === 'clip' && form.clipMode === 'bbox' || form.op !== 'clip') {
    if (![form.minx, form.miny, form.maxx, form.maxy].every(num)) {
      return 'bbox 四至必须为 4 个数字'
    }
    if (Number(form.minx) >= Number(form.maxx) || Number(form.miny) >= Number(form.maxy)) {
      return 'bbox 要求 minx<maxx 且 miny<maxy'
    }
    bbox = [form.minx, form.miny, form.maxx, form.maxy].map((s) => s.trim()).join(',')
  }
  let plane = ''
  if (form.op === 'clip' && form.clipMode === 'plane') {
    plane = form.plane.trim()
    if (!PLANE_RE.test(plane)) {
      return '切割面格式应为 ax+by+cz=d（如 0x+0y+1z=10）'
    }
  }
  if (form.op !== 'clip') {
    if (!num(form.elevation)) return 'flatten / ground-align 需要数字高程 elevation'
  }
  if (form.feather.trim() !== '') {
    const f = Number(form.feather)
    if (!Number.isFinite(f) || f < 0) return 'feather 必须 ≥ 0'
  }
  const tile = form.tile.trim()
  if (tile && tile !== 'all' && /[/\\]/.test(tile)) {
    return 'tile 应为瓦片名或 all'
  }
  const payload: EditTaskPayload = { op: form.op }
  if (tile) payload.tile = tile
  if (bbox) payload.bbox = bbox
  if (plane) payload.plane = plane
  if (form.op !== 'clip') payload.elevation = Number(form.elevation)
  if (form.feather.trim() !== '') payload.feather = Number(form.feather)
  return payload
}

async function submitEdit() {
  formError.value = ''
  const result = buildEditPayload()
  if (typeof result === 'string') {
    formError.value = result
    return
  }
  if (!task.value) return
  submitting.value = true
  try {
    const created = await createEditTask(task.value.id, result)
    router.push(`/tasks/${created.id}`)
  } catch (e) {
    formError.value = e instanceof Error ? e.message : '提交失败'
  } finally {
    submitting.value = false
  }
}

/* ------------------------------------------------------------------ */
/* 任务加载与轮询                                                       */
/* ------------------------------------------------------------------ */

function stopPolling() {
  if (timer != null) {
    clearInterval(timer)
    timer = null
  }
}

function schedulePolling() {
  stopPolling()
  if (task.value && !isTerminal(task.value.status)) {
    timer = setInterval(fetchTask, POLL_MS)
  }
}

async function fetchTask() {
  const id = String(route.params.id ?? '')
  if (!id) {
    errorMsg.value = '缺少任务 ID'
    loading.value = false
    return
  }
  try {
    task.value = await getTask(id)
    errorMsg.value = ''
    // 仅成功任务且首次到达时统计产物（签名 URL 有时效，避免重复拉取）
    if (task.value.status === 'SUCCEEDED' && statsState.value === 'idle') {
      void loadTilesetStats(id)
    }
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '无法连接 API'
  } finally {
    loading.value = false
    schedulePolling()
  }
}

watch(() => route.params.id, fetchTask)
onMounted(fetchTask)
onBeforeUnmount(stopPolling)

/* ------------------------------------------------------------------ */
/* 审批发布（F-21）+ 展示派生值                                        */
/* 此前前端没有审批入口，发布链路在界面上是断的（只能靠 curl）           */
/* ------------------------------------------------------------------ */

const toast = useToast()
const approving = ref(false)

const TASK_TYPES: Record<string, string> = {
  'osgb->3dtiles': '倾斜摄影切片',
  'image->tiles': '影像金字塔',
  'model->3dtiles': '模型切片',
  edit: '模型编辑',
  't3d-import': '.t3d 导入',
}

const isPublished = computed(
  () => task.value?.status === 'SUCCEEDED' && task.value.approved === true,
)
const canApprove = computed(
  () => task.value?.status === 'SUCCEEDED' && task.value.approved !== true,
)

const statusText = computed(() => {
  switch (task.value?.status) {
    case 'PENDING': return '排队中'
    case 'RUNNING': return '切片中'
    case 'SUCCEEDED': return '已完成'
    case 'FAILED': return '失败'
    default: return '—'
  }
})

const qcChipCls = computed(() => {
  switch (task.value?.qcStatus) {
    case 'pass': return 'chip-ready'
    case 'fail': return 'chip-partial'
    default: return 'chip-planned'
  }
})

async function approve(): Promise<void> {
  if (!task.value || !canApprove.value) return
  approving.value = true
  try {
    await approveTask(task.value.id)
    await fetchTask()
    toast.success('已审批发布，可在「服务分发」中查看地址')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '审批失败')
  } finally {
    approving.value = false
  }
}

/* ---------------- 任务控制（F-04）：取消 / 暂停 / 恢复 / 重试 / 删除 ---------------- */

const controlling = ref(false)

const ACTION_LABEL: Record<TaskAction, string> = {
  cancel: '取消',
  pause: '暂停',
  resume: '恢复',
  retry: '重试',
}

async function act(action: TaskAction): Promise<void> {
  if (!task.value || controlling.value) return
  controlling.value = true
  try {
    const res = await controlTask(task.value.id, action)
    await fetchTask()
    const note = res.warning ? `（${res.warning}）` : ''
    toast.success(`${ACTION_LABEL[action]}成功${note}`)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : `${ACTION_LABEL[action]}失败`)
  } finally {
    controlling.value = false
  }
}

/** 删除任务记录（产物文件保留在磁盘上） */
async function remove(): Promise<void> {
  if (!task.value || controlling.value) return
  if (!window.confirm(`删除任务 ${task.value.id.slice(0, 8)}？\n产物文件仍保留在磁盘上。`)) return
  controlling.value = true
  try {
    await deleteTask(task.value.id)
    toast.success('任务已删除')
    void router.push('/tasks')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '删除失败')
  } finally {
    controlling.value = false
  }
}
</script>
<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">{{ TASK_TYPES[task?.type ?? ''] ?? task?.type ?? '任务详情' }}</h1>
        <p class="page-sub mono">{{ task?.id }}</p>
      </div>
      <div v-if="task" class="toolbar">
        <StatusBadge :status="task.status" />
        <!-- 任务控制（F-04）：按状态给可用动作 -->
        <button
          v-if="canPause(task.status)"
          type="button"
          class="btn btn-sm"
          :disabled="controlling"
          title="暂停任务（保留断点，可继续）"
          @click="act('pause')"
        >
          <AppIcon name="pause" :size="12" />
          暂停
        </button>
        <button
          v-if="canResume(task.status)"
          type="button"
          class="btn btn-sm"
          :disabled="controlling"
          title="恢复任务（从断点续切）"
          @click="act('resume')"
        >
          <AppIcon name="play" :size="12" />
          恢复
        </button>
        <button
          v-if="canRetry(task.status)"
          type="button"
          class="btn btn-sm"
          :disabled="controlling"
          title="重新入队执行"
          @click="act('retry')"
        >
          <AppIcon name="refresh" :size="12" />
          重试
        </button>
        <button
          v-if="canCancel(task.status)"
          type="button"
          class="btn btn-sm"
          :disabled="controlling"
          title="取消任务（中断内核进程）"
          @click="act('cancel')"
        >
          <AppIcon name="close" :size="12" />
          取消
        </button>
        <button
          type="button"
          class="btn btn-sm"
          :disabled="controlling"
          title="删除任务记录（产物保留在磁盘）"
          @click="remove"
        >
          <AppIcon name="trash" :size="12" />
          删除
        </button>
        <button v-if="task" class="btn btn-sm" type="button" @click="downloadLogs">
          <AppIcon name="external" :size="12" />
          日志
        </button>
        <!-- DEM 脱密产物（F-21）：GeoTIFF 与留痕 JSON 走 artifact 下载通道 -->
        <template v-if="isDesensitizeTask && task.status === 'SUCCEEDED'">
          <button class="btn btn-sm" type="button" @click="downloadArtifact('desensitized.tif')">
            <AppIcon name="external" :size="12" />
            脱密 GeoTIFF
          </button>
          <button class="btn btn-sm" type="button" @click="downloadArtifact('desensitize-record.json')">
            <AppIcon name="external" :size="12" />
            脱密留痕
          </button>
        </template>
        <button
          v-if="canApprove"
          type="button"
          class="btn btn-sm btn-solid"
          :disabled="approving"
          @click="approve"
        >
          <AppIcon name="check" :size="12" />
          {{ approving ? '发布中…' : '审批发布' }}
        </button>
        <a
          v-if="isPublished"
          class="btn btn-sm"
          :href="previewHref"
          target="_blank"
          rel="noopener"
        >
          <AppIcon name="globe" :size="12" />
          三维预览
        </a>
      </div>
    </header>

    <div v-if="loading" class="empty panel">正在读取任务…</div>

    <div v-else-if="errorMsg" class="error-box panel">
      无法读取任务
      <span class="hint">{{ errorMsg }}</span>
      <router-link class="btn btn-sm" to="/tasks">返回任务中心</router-link>
    </div>

    <template v-else-if="task">
      <div class="readout-bar">
        <div class="readout">
          <span class="readout-label">状态</span>
          <span class="readout-value" style="font-size: var(--fs-lg)">{{ statusText }}</span>
        </div>
        <div class="readout">
          <span class="readout-label">分块进度</span>
          <span class="readout-value mono">
            {{ task.progress.total > 0 ? task.progress.done + ' / ' + task.progress.total : '—' }}
          </span>
        </div>
        <div class="readout">
          <span class="readout-label">内容瓦片</span>
          <span class="readout-value mono">{{ stats ? stats.contentNodes : '—' }}</span>
        </div>
        <div class="readout">
          <span class="readout-label">节点总数</span>
          <span class="readout-value mono">{{ stats ? stats.totalNodes : '—' }}</span>
        </div>
        <div class="readout">
          <span class="readout-label">最大深度</span>
          <span class="readout-value mono">{{ stats ? stats.maxDepth : '—' }}</span>
        </div>
      </div>

      <p v-if="task.status === 'SUCCEEDED' && statsState !== 'ok'" class="hint block">
        产物统计：{{ statsState === 'loading' ? '正在读取 tileset.json…' : statsState === 'idle' ? '待读取' : statsNote }}
      </p>

      <div v-if="task.status === 'FAILED' && task.error" class="panel fail-block block">
        <div class="fail-head">失败信息</div>
        <pre class="fail-text">{{ task.error }}</pre>
      </div>

      <div class="cols">
        <div>
          <div class="panel block">
            <div class="panel-head"><span class="panel-title">任务信息</span></div>
            <dl class="kv">
              <dt>任务类型</dt>
              <dd>{{ task.type }}</dd>
              <dt>数据源</dt>
              <dd><span class="code">{{ task.source || '—' }}</span></dd>
              <dt>输出路径</dt>
              <dd><span class="code">{{ task.output || '—' }}</span></dd>
              <dt>创建时间</dt>
              <dd class="mono">{{ task.createdAt || '—' }}</dd>
              <dt>更新时间</dt>
              <dd class="mono">{{ task.updatedAt || '—' }}</dd>
              <dt>发布状态</dt>
              <dd>
                {{ isPublished ? '已审批发布' : task.status === 'SUCCEEDED' ? '待审批发布（发布后才出现在服务列表）' : '—' }}
              </dd>
            </dl>
          </div>

          <div v-if="isEditTask" class="panel">
            <div class="panel-head">
              <span class="panel-title">编辑任务</span>
              <span class="panel-meta">parent 链</span>
            </div>
            <dl class="kv">
              <dt>源任务</dt>
              <dd>
                <router-link v-if="parentHref" :to="parentHref" class="num">{{ task.parentTaskId }}</router-link>
                <span v-else>{{ task.parentTaskId }}</span>
              </dd>
              <template v-for="row in editParamRows" :key="row.k">
                <dt>{{ row.k }}</dt>
                <dd><span class="code">{{ row.v }}</span></dd>
              </template>
            </dl>
            <div v-if="opsRows.length" class="readout-bar tight inset">
              <div v-for="row in opsRows" :key="row.k" class="readout">
                <span class="readout-label">{{ row.k }}</span>
                <span class="readout-value mono">{{ row.v }}</span>
              </div>
            </div>
            <div v-if="hasOpsReport" class="block-actions">
              <button class="btn btn-sm" type="button" @click="downloadOpsReport">下载留痕 ops.json</button>
            </div>
          </div>
        </div>

        <div>
          <div class="panel" :class="{ block: canEdit }">
            <div class="panel-head">
              <span class="panel-title">几何质检</span>
              <span class="chip" :class="qcChipCls">{{ qcBadge.label }}</span>
            </div>

            <div v-if="task.qcSummary" class="readout-bar tight inset">
              <div class="readout">
                <span class="readout-label">瓦片数</span>
                <span class="readout-value mono">{{ task.qcSummary.tile_count }}</span>
              </div>
              <div class="readout">
                <span class="readout-label">退化面</span>
                <span class="readout-value mono" :class="{ 'is-err': task.qcSummary.total_degenerate > 0 }">
                  {{ task.qcSummary.total_degenerate }}
                </span>
              </div>
              <div class="readout">
                <span class="readout-label">法线翻转</span>
                <span class="readout-value mono" :class="{ 'is-err': task.qcSummary.total_flipped_edges > 0 }">
                  {{ task.qcSummary.total_flipped_edges }}
                </span>
              </div>
              <div class="readout">
                <span class="readout-label">悬浮块</span>
                <span class="readout-value mono" :class="{ 'is-err': task.qcSummary.total_floating > 0 }">
                  {{ task.qcSummary.total_floating }}
                </span>
              </div>
              <div class="readout">
                <span class="readout-label">裂缝段</span>
                <span class="readout-value mono" :class="{ 'is-err': task.qcSummary.total_crack_segments > 0 }">
                  {{ task.qcSummary.total_crack_segments }}
                </span>
              </div>
              <div class="readout">
                <span class="readout-label">自相交</span>
                <span class="readout-value mono" :class="{ 'is-err': task.qcSummary.total_self_intersections > 0 }">
                  {{ task.qcSummary.total_self_intersections }}
                </span>
              </div>
            </div>
            <div v-else class="empty">
              {{ task.qcStatus === 'skipped' ? '源数据不适用几何质检，已跳过' : '未执行质检' }}
              <span class="hint">创建任务时开启「切片后自动质检」即可</span>
            </div>

            <div v-if="hasQcReport" class="block-actions">
              <button class="btn btn-sm" type="button" @click="downloadQcReport">下载完整报告</button>
              <router-link class="btn btn-sm" to="/qc">打开质检页</router-link>
            </div>
          </div>

          <div v-if="canEdit" class="panel">
            <div class="panel-head">
              <span class="panel-title">发起编辑</span>
              <span class="panel-meta">clip / flatten / ground-align</span>
            </div>

            <div v-if="!showEditForm" class="block-actions">
              <button class="btn btn-sm" type="button" @click="showEditForm = true">
                <AppIcon name="edit" :size="12" />
                选择编辑操作
              </button>
              <span class="hint">提交后创建独立编辑任务（产物需重新审批发布）</span>
            </div>

            <form v-else class="edit-form" @submit.prevent="submitEdit">
              <div class="field">
                <label class="label" for="edit-op">操作</label>
                <select id="edit-op" v-model="form.op" class="select">
                  <option v-for="o in EDIT_OPS" :key="o.value" :value="o.value">{{ o.label }}</option>
                </select>
                <span class="hint">{{ EDIT_OPS.find((o) => o.value === form.op)?.hint }}</span>
              </div>

              <div class="field">
                <label class="label" for="edit-tile">瓦片（可选，默认 all）</label>
                <input id="edit-tile" v-model="form.tile" class="input mono" placeholder="all" />
              </div>

              <template v-if="form.op === 'clip'">
                <div class="field">
                  <label class="label">裁剪依据（互斥）</label>
                  <div class="segmented">
                    <button
                      type="button"
                      :class="{ active: form.clipMode === 'bbox' }"
                      @click="form.clipMode = 'bbox'"
                    >
                      bbox 四至
                    </button>
                    <button
                      type="button"
                      :class="{ active: form.clipMode === 'plane' }"
                      @click="form.clipMode = 'plane'"
                    >
                      plane 切割面
                    </button>
                  </div>
                </div>
                <div v-if="form.clipMode === 'bbox'" class="field">
                  <label class="label">bbox 四至（minx, miny, maxx, maxy）</label>
                  <div class="bbox-grid">
                    <input v-model="form.minx" class="input mono" placeholder="minx" />
                    <input v-model="form.miny" class="input mono" placeholder="miny" />
                    <input v-model="form.maxx" class="input mono" placeholder="maxx" />
                    <input v-model="form.maxy" class="input mono" placeholder="maxy" />
                  </div>
                </div>
                <div v-else class="field">
                  <label class="label" for="edit-plane">切割面 ax+by+cz=d</label>
                  <input id="edit-plane" v-model="form.plane" class="input mono" placeholder="0x+0y+1z=10" />
                </div>
              </template>

              <template v-else>
                <div class="field">
                  <label class="label">bbox 四至（minx, miny, maxx, maxy）</label>
                  <div class="bbox-grid">
                    <input v-model="form.minx" class="input mono" placeholder="minx" />
                    <input v-model="form.miny" class="input mono" placeholder="miny" />
                    <input v-model="form.maxx" class="input mono" placeholder="maxx" />
                    <input v-model="form.maxy" class="input mono" placeholder="maxy" />
                  </div>
                </div>
                <div class="field">
                  <label class="label" for="edit-elevation">高程 elevation</label>
                  <input id="edit-elevation" v-model="form.elevation" class="input mono" placeholder="0" />
                </div>
              </template>

              <div class="field">
                <label class="label" for="edit-feather">羽化 feather（可选，≥0）</label>
                <input id="edit-feather" v-model="form.feather" class="input mono" placeholder="0" />
              </div>

              <p v-if="formError" class="field-error">{{ formError }}</p>

              <div class="form-actions">
                <button class="btn btn-solid" type="submit" :disabled="submitting">
                  {{ submitting ? '提交中…' : '提交编辑任务' }}
                </button>
                <button
                  class="btn btn-ghost"
                  type="button"
                  :disabled="submitting"
                  @click="((showEditForm = false), (formError = ''))"
                >
                  取消
                </button>
              </div>
            </form>
          </div>
        </div>
      </div>
    </template>
  </section>
</template>
<style scoped>
.cols {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--sp-4);
  align-items: start;
}
@media (max-width: 1040px) { .cols { grid-template-columns: 1fr; } }

.readout-bar.inset { margin: var(--sp-3) var(--sp-4); }

.fail-head {
  padding: var(--sp-3) var(--sp-4) var(--sp-1);
  font-size: var(--fs-xs);
  font-weight: 600;
  letter-spacing: 0.06em;
  color: var(--err);
}
.fail-text {
  margin: 0;
  padding: 0 var(--sp-4) var(--sp-4);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  line-height: 1.7;
  color: var(--ink-muted);
  white-space: pre-wrap;
  word-break: break-all;
}

.block-actions {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
  flex-wrap: wrap;
}

.kv dd.mono { font-family: var(--font-mono); font-size: var(--fs-xs); }

.edit-form { padding: var(--sp-4); }
.bbox-grid {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: var(--sp-2);
}
@media (max-width: 700px) { .bbox-grid { grid-template-columns: repeat(2, 1fr); } }
</style>
