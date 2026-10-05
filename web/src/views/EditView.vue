<script setup lang="ts">
/** 模型编辑：左选算子，右填参数，底部执行；下方编辑链历史 */
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { createEditTask, listEditHistory, listTasks } from '../api/client'
import type { EditOp, Task } from '../api/types'
import AppIcon from '../components/AppIcon.vue'
import StatusBadge from '../components/StatusBadge.vue'
import { useToast } from '../composables/useToast'

interface OpDef {
  id: EditOp
  name: string
  desc: string
  fields: { key: string; label: string; ph?: string; hint?: string }[]
}

const OPS: OpDef[] = [
  {
    id: 'clip',
    name: '抠除',
    desc: '按包围盒或切割面移除区域内的几何，面积守恒（被移除与保留部分之和等于原面积）',
    fields: [
      { key: 'bbox', label: '包围盒 minx,miny,maxx,maxy', ph: '0,0,500,500' },
      { key: 'plane', label: '切割面 ax+by+cz=d（可选，与包围盒二选一）', ph: '0,0,1,50' },
    ],
  },
  {
    id: 'flatten',
    name: '压平',
    desc: '区域内高程置平到目标高程，边界带线性羽化过渡，避免硬边',
    fields: [
      { key: 'bbox', label: '包围盒 minx,miny,maxx,maxy', ph: '0,0,500,500' },
      { key: 'elevation', label: '目标高程（米）', ph: '0' },
      { key: 'feather', label: '羽化距离（米）', ph: '20' },
    ],
  },
  {
    id: 'ground-align',
    name: '地面对齐',
    desc: '按区域内最低点做刚性平移（不缩放），避免非刚性变形与接缝错位',
    fields: [
      { key: 'bbox', label: '包围盒 minx,miny,maxx,maxy', ph: '0,0,500,500' },
      { key: 'tile', label: '目标瓦片名（可选，默认 all）', ph: 'Tile_+000_+000' },
    ],
  },
]

const router = useRouter()
const toast = useToast()
const tasks = ref<Task[]>([])
const selectedOp = ref(0)
const sourceId = ref('')
const values = ref<Record<string, string>>({})
const submitting = ref(false)
const errorMsg = ref('')
const history = ref<Task[]>([])
const loading = ref(true)

const op = computed(() => OPS[selectedOp.value]!)
const sources = computed(() => tasks.value.filter((t) => t.status === 'SUCCEEDED' && t.approved === true))
const canSubmit = computed(() => sourceId.value.length > 0 && !submitting.value)

const summary = computed(() => {
  const items: { key: string; label: string; value: string }[] = []
  for (const f of op.value.fields) {
    const v = (values.value[f.key] ?? '').trim()
    if (v) items.push({ key: f.key, label: f.key, value: v })
  }
  return items
})

function selectOp(i: number): void {
  selectedOp.value = i
  values.value = {}
  errorMsg.value = ''
}

function setField(key: string, value: string): void {
  values.value = { ...values.value, [key]: value }
}

async function submit(): Promise<void> {
  if (!canSubmit.value) return
  submitting.value = true
  errorMsg.value = ''
  try {
    const payload: Record<string, unknown> = { op: op.value.id }
    for (const f of op.value.fields) {
      const v = (values.value[f.key] ?? '').trim()
      if (!v) continue
      payload[f.key] = f.key === 'elevation' || f.key === 'feather' ? Number(v) : v
    }
    const t = await createEditTask(sourceId.value, payload as never)
    toast.success('编辑任务已创建')
    router.push(`/tasks/${encodeURIComponent(t.id)}`)
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '创建编辑任务失败'
    toast.error(errorMsg.value)
  } finally {
    submitting.value = false
  }
}

async function loadHistory(): Promise<void> {
  if (!sourceId.value) {
    history.value = []
    return
  }
  try {
    history.value = (await listEditHistory(sourceId.value)).tasks
  } catch {
    history.value = []
  }
}

onMounted(async () => {
  try {
    tasks.value = await listTasks()
    if (sources.value.length > 0) {
      sourceId.value = sources.value[0]!.id
      await loadHistory()
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
        <h1 class="page-title">模型编辑</h1>
        <p class="page-sub">对已审批发布的切片产物执行抠除 / 压平 / 地面对齐</p>
      </div>
    </header>

    <div class="workspace">
      <!-- 左：算子选择 -->
      <div class="panel pick-list">
        <div class="pick-list-head">编辑算子</div>
        <button
          v-for="(o, i) in OPS"
          :key="o.id"
          type="button"
          class="pick-item"
          :class="{ active: selectedOp === i }"
          @click="selectOp(i)"
        >
          <AppIcon name="edit" :size="14" />
          <span class="grow">{{ o.name }}</span>
          <span class="num">{{ o.id }}</span>
        </button>
      </div>

      <!-- 右：参数 -->
      <div>
        <div class="panel block">
          <div class="panel-head">
            <div>
              <div class="panel-title">{{ op.name }}</div>
              <div class="op-desc">{{ op.desc }}</div>
            </div>
          </div>
          <div class="form-body">
            <div class="field">
              <label class="label">源任务（已审批发布的切片或上游编辑任务）</label>
              <div v-if="loading" class="skeleton skel-wide" />
              <template v-else-if="sources.length > 0">
                <select v-model="sourceId" class="select" @change="loadHistory">
                  <option v-for="t in sources" :key="t.id" :value="t.id">
                    {{ t.id.slice(0, 10) }} · {{ t.type }}
                  </option>
                </select>
                <span class="hint">只有已审批发布的任务可作为编辑源</span>
              </template>
              <div v-else class="empty-inline">
                没有可编辑的任务
                <span class="hint">先在任务详情中完成切片并「审批发布」，再回到此处编辑</span>
              </div>
            </div>

            <div v-for="f in op.fields" :key="f.key" class="field">
              <label class="label">{{ f.label }}</label>
              <input
                class="input mono"
                :placeholder="f.ph"
                :value="values[f.key] ?? ''"
                @input="setField(f.key, ($event.target as HTMLInputElement).value)"
              />
            </div>

            <div v-if="errorMsg" class="field-error">{{ errorMsg }}</div>

            <div class="confirm">
              <div class="confirm-body">
                <div class="confirm-line">
                  <span class="confirm-key">将执行</span>
                  <span class="chip chip-active">{{ op.id }}</span>
                  <span class="num">{{ sourceId ? sourceId.slice(0, 10) : '未选择源任务' }}</span>
                </div>
                <div v-if="summary.length > 0" class="confirm-params">
                  <span v-for="s in summary" :key="s.key" class="chip">{{ s.label }}：{{ s.value }}</span>
                </div>
              </div>
              <button type="button" class="btn btn-solid" :disabled="!canSubmit" @click="submit">
                <AppIcon name="play" :size="13" />
                {{ submitting ? '提交中…' : '执行编辑' }}
              </button>
            </div>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head">
            <span class="panel-title">编辑链历史</span>
            <span class="panel-meta">{{ history.length }} 条</span>
          </div>
          <div v-if="history.length === 0" class="empty">
            该任务暂无编辑记录
            <span class="hint">执行编辑后，产物与留痕 ops.json 会记录在此</span>
          </div>
          <template v-else>
            <div class="thead data-grid hist-grid">
              <span>任务 ID</span><span>操作</span><span>状态</span><span class="ta-r">创建时间</span>
            </div>
            <div v-for="t in history" :key="t.id" class="data-row data-grid hist-grid">
              <span class="num">{{ t.id.slice(0, 10) }}</span>
              <span class="hist-op">{{ String(t.params?.op ?? '—') }}</span>
              <span><StatusBadge :status="t.status" /></span>
              <span class="num ta-r">
                {{ t.createdAt ? new Date(t.createdAt).toLocaleString('zh-CN', { hour12: false, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '—' }}
              </span>
            </div>
          </template>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.op-desc { font-size: var(--fs-xs); color: var(--ink-muted); margin-top: 2px; line-height: 1.6; }
.form-body { padding: var(--sp-4); }
.skel-wide { height: 30px; width: 100%; }
.empty-inline { font-size: var(--fs-sm); color: var(--ink-muted); padding: var(--sp-3) 0; }

.confirm {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-4);
  margin-top: var(--sp-5);
  padding: var(--sp-3) var(--sp-4);
  background: var(--bg-sunken);
  border: 1px solid var(--line);
  border-radius: var(--radius);
}
.confirm-body { min-width: 0; }
.confirm-line { display: flex; align-items: center; gap: var(--sp-2); }
.confirm-key { font-size: var(--fs-xs); color: var(--ink-faint); }
.confirm-params { display: flex; flex-wrap: wrap; gap: 5px; margin-top: var(--sp-2); }

.hist-grid { grid-template-columns: 96px 110px 92px 104px; }
.hist-op { color: var(--ink); }
</style>
