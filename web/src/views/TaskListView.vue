<script setup lang="ts">
/** 任务中心：筛选 + 读数条 + 紧凑数据表（高密度作业场景） */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { controlTask, deleteTask, listTasks } from '../api/client'
import {
  canCancel,
  canPause,
  canResume,
  canRetry,
  isTerminal,
  type Task,
  type TaskAction,
  type TaskStatus,
} from '../api/types'
import StatusBadge from '../components/StatusBadge.vue'
import TaskProgress from '../components/TaskProgress.vue'
import AppIcon from '../components/AppIcon.vue'
import { useToast } from '../composables/useToast'
import { taskPreviewUrl } from '../utils/preview'

const router = useRouter()
const toast = useToast()
/** 正在执行控制动作的任务 ID（避免重复点击 + 按钮禁用态） */
const busyId = ref('')

const ACTION_LABEL: Record<TaskAction, string> = {
  cancel: '取消',
  pause: '暂停',
  resume: '恢复',
  retry: '重试',
}

function goDetail(id: string): void {
  void router.push(`/tasks/${encodeURIComponent(id)}`)
}

/** 任务控制（F-04）：取消/暂停/恢复/重试，成功后刷新列表 */
async function act(t: Task, action: TaskAction): Promise<void> {
  if (busyId.value) return
  busyId.value = t.id
  try {
    const res = await controlTask(t.id, action)
    const note = res.warning ? `（${res.warning}）` : ''
    toast.success(`${ACTION_LABEL[action]}成功${note}`)
    await fetchTasks()
  } catch (e) {
    toast.error(e instanceof Error ? e.message : `${ACTION_LABEL[action]}失败`)
  } finally {
    busyId.value = ''
  }
}

/** 删除任务记录（产物文件保留在磁盘上，不随记录删除） */
async function remove(t: Task): Promise<void> {
  if (busyId.value) return
  if (!window.confirm(`删除任务 ${t.id.slice(0, 8)}？\n产物文件仍保留在磁盘上。`)) return
  busyId.value = t.id
  try {
    await deleteTask(t.id)
    toast.success('任务已删除')
    await fetchTasks()
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '删除失败')
  } finally {
    busyId.value = ''
  }
}

/** 仅已审批发布的切片任务有可加载的服务入口 */
function canPreview(t: Task): boolean {
  return t.status === 'SUCCEEDED' && t.approved === true
}

function previewUrl(id: string): string {
  return taskPreviewUrl(id)
}

type Filter = 'all' | 'active' | 'succeeded' | 'failed'

const tasks = ref<Task[]>([])
const loading = ref(true)
const errorMsg = ref('')
const filter = ref<Filter>('all')
const typeFilter = ref('')
const keyword = ref('')
const POLL_MS = 5000
let timer: ReturnType<typeof setInterval> | null = null

const hasActive = computed(() => tasks.value.some((t) => !isTerminal(t.status)))

const counts = computed(() => {
  const c = { total: tasks.value.length, active: 0, succeeded: 0, failed: 0 }
  for (const t of tasks.value) {
    if (!isTerminal(t.status)) c.active++
    else if (t.status === 'SUCCEEDED') c.succeeded++
    else if (t.status === 'FAILED') c.failed++
  }
  return c
})

const types = computed(() => [...new Set(tasks.value.map((t) => t.type))].sort())

const visible = computed(() =>
  tasks.value.filter((t) => {
    if (filter.value === 'active' && isTerminal(t.status)) return false
    if (filter.value === 'succeeded' && t.status !== 'SUCCEEDED') return false
    if (filter.value === 'failed' && t.status !== 'FAILED') return false
    if (typeFilter.value && t.type !== typeFilter.value) return false
    const kw = keyword.value.trim().toLowerCase()
    if (kw && !t.id.toLowerCase().includes(kw)) return false
    return true
  }),
)

function stopPolling(): void {
  if (timer) { clearInterval(timer); timer = null }
}

async function fetchTasks(): Promise<void> {
  try {
    tasks.value = await listTasks()
    errorMsg.value = ''
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '无法连接服务'
  } finally {
    loading.value = false
    stopPolling()
    if (hasActive.value) timer = setInterval(fetchTasks, POLL_MS)
  }
}

function fmtTime(ts?: string): string {
  if (!ts) return '—'
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ts
  return d.toLocaleString('zh-CN', { hour12: false, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function typeLabel(type: string): string {
  const map: Record<string, string> = {
    'osgb->3dtiles': '倾斜摄影切片',
    'image->tiles': '影像金字塔',
    edit: '模型编辑',
    't3d-import': '.t3d 导入',
  }
  return map[type] ?? type
}

const filters: { id: Filter; label: string }[] = [
  { id: 'all', label: '全部' },
  { id: 'active', label: '进行中' },
  { id: 'succeeded', label: '已完成' },
  { id: 'failed', label: '失败' },
]

onMounted(fetchTasks)
onBeforeUnmount(stopPolling)
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">任务中心</h1>
        <p class="page-sub">切片 / 转换 / 编辑任务的队列与历史</p>
      </div>
      <div class="toolbar">
        <span v-if="hasActive" class="live"><span class="live-dot" />实时刷新</span>
        <button type="button" class="btn btn-sm" @click="fetchTasks">
          <AppIcon name="refresh" :size="13" />
          刷新
        </button>
        <RouterLink class="btn btn-sm btn-solid" to="/convert">新建任务</RouterLink>
      </div>
    </header>

    <div class="readout-bar tight">
      <div class="readout">
        <span class="readout-label">任务总数</span>
        <span class="readout-value mono">{{ counts.total }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">进行中</span>
        <span class="readout-value mono" :class="{ 'is-active': counts.active > 0 }">{{ counts.active }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">已完成</span>
        <span class="readout-value mono is-ok">{{ counts.succeeded }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">失败</span>
        <span class="readout-value mono" :class="{ 'is-err': counts.failed > 0 }">{{ counts.failed }}</span>
      </div>
    </div>

    <div v-if="errorMsg" class="error-box panel block">
      无法连接服务
      <span class="hint">{{ errorMsg }} —— 请确认 TanGIS 服务正在运行</span>
    </div>

    <div class="panel">
      <!-- 筛选栏 -->
      <div class="filter-bar">
        <div class="segmented">
          <button
            v-for="f in filters"
            :key="f.id"
            type="button"
            :class="{ active: filter === f.id }"
            @click="filter = f.id"
          >
            {{ f.label }}
          </button>
        </div>
        <select v-model="typeFilter" class="select select-sm">
          <option value="">全部类型</option>
          <option v-for="t in types" :key="t" :value="t">{{ typeLabel(t) }}</option>
        </select>
        <input v-model="keyword" class="input input-sm mono" placeholder="按任务 ID 过滤" />
        <span class="filter-meta">{{ visible.length }} / {{ counts.total }}</span>
      </div>

      <div v-if="loading" class="pad">
        <div v-for="i in 5" :key="i" class="skeleton skel" />
      </div>
      <div v-else-if="visible.length === 0" class="empty">
        没有匹配的任务
        <span class="hint">调整筛选条件，或前往「切片转换」新建任务</span>
      </div>
      <template v-else>
        <div class="thead data-grid task-grid">
          <span>任务</span><span>状态</span><span>进度</span><span class="ta-r">创建时间</span>
        </div>
        <div
          v-for="t in visible"
          :key="t.id"
          class="data-row data-grid task-grid clickable"
          role="link"
          tabindex="0"
          @click="goDetail(t.id)"
          @keyup.enter="goDetail(t.id)"
        >
          <span class="cell-task">
            <span class="cell-type">{{ typeLabel(t.type) }}</span>
            <span class="num">{{ t.id.slice(0, 10) }}</span>
          </span>
          <span><StatusBadge :status="t.status as TaskStatus" /></span>
          <span class="cell-progress"><TaskProgress :progress="t.progress" :status="t.status" /></span>
          <span class="cell-ops">
            <!-- 任务控制（F-04）：按状态给出可用动作，避免无效按钮 -->
            <span class="row-actions">
              <button
                v-if="canPause(t.status)"
                class="icon-btn"
                type="button"
                title="暂停任务（保留断点，可继续）"
                :disabled="busyId === t.id"
                @click.stop="act(t, 'pause')"
              >
                <AppIcon name="pause" :size="12" />
              </button>
              <button
                v-if="canResume(t.status)"
                class="icon-btn"
                type="button"
                title="恢复任务（从断点续切）"
                :disabled="busyId === t.id"
                @click.stop="act(t, 'resume')"
              >
                <AppIcon name="play" :size="12" />
              </button>
              <button
                v-if="canRetry(t.status)"
                class="icon-btn"
                type="button"
                title="重试任务"
                :disabled="busyId === t.id"
                @click.stop="act(t, 'retry')"
              >
                <AppIcon name="refresh" :size="12" />
              </button>
              <button
                v-if="canCancel(t.status)"
                class="icon-btn"
                type="button"
                title="取消任务"
                :disabled="busyId === t.id"
                @click.stop="act(t, 'cancel')"
              >
                <AppIcon name="close" :size="12" />
              </button>
              <button
                class="icon-btn danger"
                type="button"
                title="删除任务记录（产物保留在磁盘）"
                :disabled="busyId === t.id"
                @click.stop="remove(t)"
              >
                <AppIcon name="trash" :size="12" />
              </button>
            </span>
            <span class="num">{{ fmtTime(t.createdAt) }}</span>
            <a
              v-if="canPreview(t)"
              class="btn btn-sm"
              :href="previewUrl(t.id)"
              target="_blank"
              rel="noopener"
              title="在新页面打开该任务的三维预览"
              @click.stop
            >
              <AppIcon name="globe" :size="12" />
              预览
            </a>
          </span>
        </div>
      </template>
    </div>
  </section>
</template>

<style scoped>
.live {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: var(--fs-xs);
  color: var(--accent-ink);
}
.live-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--accent);
  animation: pulse 1.5s var(--ease) infinite;
}

.filter-bar {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
  border-bottom: 1px solid var(--line);
  flex-wrap: wrap;
}
.select-sm, .input-sm { width: auto; padding: 4px 9px; font-size: var(--fs-xs); }
.input-sm { min-width: 180px; }
.filter-meta { margin-left: auto; font-size: var(--fs-xs); color: var(--ink-faint); }

.task-grid { grid-template-columns: minmax(180px, 1.4fr) 96px minmax(140px, 1fr) 190px; }
.cell-task { display: flex; flex-direction: column; min-width: 0; }
.cell-type { color: var(--ink); }
.cell-progress { min-width: 0; }
.cell-ops {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: var(--sp-2);
}
.clickable { cursor: pointer; }
.pad { padding: var(--sp-4); display: flex; flex-direction: column; gap: var(--sp-3); }
.skel { height: 16px; }
</style>
