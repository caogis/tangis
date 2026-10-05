<script setup lang="ts">
/**
 * 工作台 —— 仪器读数式总览
 * 刻意不用卡片网格：读数条 + 分组列表承担信息层级，符合"高密度作业"的场景。
 */
import { computed, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { listServices, listTasks } from '../api/client'
import type { Task } from '../api/types'
import StatusBadge from '../components/StatusBadge.vue'
import AppIcon from '../components/AppIcon.vue'
import { countByStatus, groupByModule, STATUS_LABEL } from '../capabilities'

const tasks = ref<Task[]>([])
const serviceCount = ref(0)
const loading = ref(true)
const errorMsg = ref('')

const stats = computed(() => {
  const s = { total: tasks.value.length, active: 0, succeeded: 0, failed: 0 }
  for (const t of tasks.value) {
    if (t.status === 'RUNNING' || t.status === 'PENDING') s.active++
    else if (t.status === 'SUCCEEDED') s.succeeded++
    else if (t.status === 'FAILED') s.failed++
  }
  return s
})

const recent = computed(() => tasks.value.slice(0, 8))
const modules = groupByModule()
const capCount = countByStatus()

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

onMounted(async () => {
  try {
    const [ts, svc] = await Promise.all([listTasks(), listServices()])
    tasks.value = ts
    serviceCount.value = svc.length
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
        <h1 class="page-title">工作台</h1>
        <p class="page-sub">数据导入 → 切片转换 → 服务分发，全部在本机完成</p>
      </div>
      <div class="toolbar">
        <RouterLink class="btn btn-solid" to="/convert">
          <AppIcon name="play" :size="13" />
          新建任务
        </RouterLink>
        <RouterLink class="btn" to="/import">导入数据</RouterLink>
      </div>
    </header>

    <div v-if="errorMsg" class="error-box panel">
      无法连接服务
      <span class="hint">{{ errorMsg }} —— 请确认 TanGIS 服务正在运行</span>
    </div>

    <!-- 读数条：仪器式指标，一行排布，不用卡片 -->
    <div class="readout-bar">
      <div class="readout">
        <span class="readout-label">任务总数</span>
        <span class="readout-value mono">{{ loading ? '—' : stats.total }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">进行中</span>
        <span class="readout-value mono" :class="{ 'is-active': stats.active > 0 }">
          {{ loading ? '—' : stats.active }}
        </span>
      </div>
      <div class="readout">
        <span class="readout-label">已完成</span>
        <span class="readout-value mono is-ok">{{ loading ? '—' : stats.succeeded }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">失败</span>
        <span class="readout-value mono" :class="{ 'is-err': stats.failed > 0 }">
          {{ loading ? '—' : stats.failed }}
        </span>
      </div>
      <div class="readout">
        <span class="readout-label">已发布服务</span>
        <span class="readout-value mono">{{ loading ? '—' : serviceCount }}</span>
      </div>
    </div>

    <div class="cols">
      <!-- 最近任务：紧凑数据表 -->
      <div class="panel">
        <div class="panel-head">
          <span class="panel-title">最近任务</span>
          <RouterLink class="link-more" to="/tasks">全部<AppIcon name="chevron-right" :size="12" /></RouterLink>
        </div>

        <div v-if="loading" class="pad">
          <div v-for="i in 4" :key="i" class="skeleton skel" />
        </div>
        <div v-else-if="recent.length === 0" class="empty">
          还没有任务
          <span class="hint">从「切片转换」新建，或用「数据导入」直接导入数据</span>
        </div>
        <template v-else>
          <div class="thead task-grid">
            <span>任务类型</span><span>ID</span><span>状态</span><span class="ta-r">创建时间</span>
          </div>
          <RouterLink
            v-for="t in recent"
            :key="t.id"
            :to="`/tasks/${encodeURIComponent(t.id)}`"
            class="row-line task-grid"
          >
            <span class="t-type">{{ typeLabel(t.type) }}</span>
            <span class="t-id mono">{{ t.id.slice(0, 8) }}</span>
            <StatusBadge :status="t.status" />
            <span class="t-time mono">{{ fmtTime(t.createdAt) }}</span>
          </RouterLink>
        </template>
      </div>

      <!-- 能力覆盖：分组条形 -->
      <div class="panel">
        <div class="panel-head">
          <span class="panel-title">能力覆盖</span>
          <RouterLink class="link-more" to="/settings">能力矩阵<AppIcon name="chevron-right" :size="12" /></RouterLink>
        </div>
        <div class="cap-summary">
          <span class="chip chip-ready">可用 {{ capCount.ready }}</span>
          <span class="chip chip-partial">部分 {{ capCount.partial }}</span>
          <span class="chip chip-planned">规划 {{ capCount.planned }}</span>
        </div>
        <div class="cap-list">
          <div v-for="m in modules" :key="m.module" class="cap-item">
            <div class="cap-item-head">
              <span>{{ m.module }}</span>
              <span class="mono">{{ m.items.filter((i) => i.status === 'ready').length }}/{{ m.items.length }}</span>
            </div>
            <div class="cap-bar">
              <span
                v-for="c in m.items"
                :key="c.name"
                class="cap-seg"
                :class="c.status"
                :title="`${c.name} · ${STATUS_LABEL[c.status]}`"
              />
            </div>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
/* 读数条：靠间距与竖线分隔，不用卡片 */
.readout-bar {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 0;
  border: 1px solid var(--line);
  border-radius: var(--radius);
  background: var(--surface);
  margin-bottom: var(--sp-4);
  overflow: hidden;
}
.readout { border-left: 1px solid var(--line); }
.readout:first-child { border-left: none; }

.cols { display: grid; grid-template-columns: 1.35fr 1fr; gap: var(--sp-4); align-items: start; }
@media (max-width: 1100px) { .cols { grid-template-columns: 1fr; } }

.link-more {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  font-size: var(--fs-xs);
  color: var(--ink-muted);
}
.link-more:hover { color: var(--primary); text-decoration: none; }

.task-grid {
  display: grid;
  grid-template-columns: 1.1fr 74px 84px 92px;
  gap: var(--sp-3);
  align-items: center;
  padding: 7px var(--sp-4);
  font-size: var(--fs-sm);
  color: var(--ink-muted);
}
a.task-grid:hover { color: var(--ink); text-decoration: none; }
.ta-r { text-align: right; }
.t-type { color: var(--ink); }
.t-id { font-size: var(--fs-xs); color: var(--ink-faint); }
.t-time { font-size: var(--fs-xs); color: var(--ink-faint); text-align: right; }

.pad { padding: var(--sp-4); display: flex; flex-direction: column; gap: var(--sp-3); }
.skel { height: 16px; }

.cap-summary { display: flex; gap: var(--sp-2); padding: var(--sp-3) var(--sp-4) 0; flex-wrap: wrap; }
.cap-list { padding: var(--sp-3) var(--sp-4) var(--sp-4); display: flex; flex-direction: column; gap: var(--sp-3); }
.cap-item-head {
  display: flex;
  justify-content: space-between;
  font-size: var(--fs-xs);
  color: var(--ink-muted);
  margin-bottom: 5px;
}
.cap-item-head .mono { color: var(--ink-faint); }
.cap-bar { display: flex; gap: 2px; }
.cap-seg { height: 5px; flex: 1; border-radius: 1px; background: var(--line); }
.cap-seg.ready { background: var(--primary); }
.cap-seg.partial { background: color-mix(in oklch, var(--primary) 40%, var(--line)); }
.cap-seg.planned { background: var(--line); }
</style>
