<script setup lang="ts">
/**
 * 切片转换：左侧选线路，右侧设参数，底部确认后创建任务。
 * 参数分组用 section-head + 细线，不使用卡片嵌套。
 */
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { createTask, getSystemInfo, listTasks } from '../api/client'
import type { Task } from '../api/types'
import AppIcon from '../components/AppIcon.vue'
import PathPickerDialog from '../components/PathPickerDialog.vue'
import StatusBadge from '../components/StatusBadge.vue'
import { useToast } from '../composables/useToast'

type LineStatus = 'ready' | 'planned'
type ParamKind = 'text' | 'number' | 'switch' | 'range'

interface ParamDef {
  key: string
  label: string
  kind: ParamKind
  ph?: string
  hint?: string
  min?: number
  max?: number
  step?: number
  def?: string | boolean | number
}
interface Line {
  id: string
  name: string
  desc: string
  accept: string
  status: LineStatus
  icon: 'convert' | 'import' | 'globe'
  /** 源数据形态：目录（OSGB）还是单个文件（影像/DEM/LAS/OBJ） */
  srcKind: 'dir' | 'file'
  /** srcKind=file 时路径选择器展示的后缀（首个后缀用于内核校验提示） */
  srcExts?: string[]
  params: ParamDef[]
}

const LINES: Line[] = [
  {
    id: 'osgb->3dtiles',
    name: '倾斜摄影 → 3DTiles',
    desc: '真实解析 OSGB，输出带纹理与地理定位的 3D Tiles，支持断点续切',
    accept: 'OSGB 目录（含 Tile_*.osgb）',
    status: 'ready',
    icon: 'convert',
    srcKind: 'dir',
    params: [
      { key: 'origin', label: '地理原点（经度,纬度,高程）', kind: 'text', ph: '116.39,39.90,0', hint: '写入 root.transform，使模型落到真实地理位置' },
      { key: 'simplify', label: '轻量化比例', kind: 'range', min: 0.1, max: 1, step: 0.1, def: 0.5, hint: '1.0 不简化；QEM 简化并保留瓦片接缝' },
      { key: 'qc', label: '切片后自动质检', kind: 'switch', def: false, hint: '检查退化面、法线翻转、悬浮块、裂缝与自相交' },
    ],
  },
  {
    id: 'image->tiles',
    name: '影像 → 瓦片金字塔',
    desc: 'GeoTIFF 切片为 XYZ/WMTS 金字塔，无数据区透明，层级按分辨率自动推导',
    accept: '.tif / .tiff',
    status: 'ready',
    icon: 'globe',
    srcKind: 'file',
    srcExts: ['.tif', '.tiff'],
    // 层级由内核按源分辨率自动推导（内核 raster2tiles 不接收层级参数），
    // 故此处不提供无效表单项，避免"填了没效果"。
    params: [],
  },
  {
    id: 'model->3dtiles',
    name: '通用模型 → 3DTiles',
    desc: 'OBJ 等通用模型切片，保留材质纹理',
    accept: '.obj（及同目录贴图）',
    status: 'ready',
    icon: 'convert',
    srcKind: 'file',
    srcExts: ['.obj'],
    params: [{ key: 'origin', label: '地理原点（经度,纬度,高程）', kind: 'text', ph: '116.39,39.90,0' }],
  },
  {
    // 注意：类型必须与后端 isTerrainTask 判定一致（terrain->tiles），
    // 否则任务会落到默认切片分支。
    id: 'terrain->tiles',
    name: '地形 → Terrain',
    desc: 'DEM 切片为 Quantized-Mesh 地形瓦片，可直接被 Cesium TerrainProvider 加载',
    accept: '.tif（单波段 DEM）',
    status: 'ready',
    icon: 'globe',
    srcKind: 'file',
    srcExts: ['.tif', '.tiff'],
    params: [
      { key: 'min_zoom', label: '最小层级', kind: 'number', ph: '0', hint: '留空由内核按分辨率推导' },
      { key: 'max_zoom', label: '最大层级', kind: 'number', ph: '15' },
    ],
  },
  {
    id: 'las->3dtiles',
    name: '点云 → 3DTiles',
    desc: 'LAS 点云切片为 3D Tiles 点云瓦片（pnts + tileset.json）',
    accept: '.las（LAS 1.0–1.4；LAZ 暂不支持）',
    status: 'ready',
    icon: 'convert',
    srcKind: 'file',
    srcExts: ['.las'],
    params: [
      {
        key: 'origin',
        label: '地理原点（经度,纬度,高程）',
        kind: 'text',
        ph: '116.39,39.90,0',
        hint: '写入 root.transform，使点云落到真实地理位置',
      },
      {
        key: 'max_points_per_tile',
        label: '每瓦片点数上限',
        kind: 'number',
        ph: '50000',
        hint: '默认 50000，范围 1000–2000000',
      },
    ],
  },
]

const router = useRouter()
const toast = useToast()

const selectedId = ref(LINES[0]!.id)
const source = ref('')
const output = ref('')
const values = ref<Record<string, string | boolean | number>>({})
const submitting = ref(false)
const errorMsg = ref('')
const recent = ref<Task[]>([])
/** 后端是否开放本机目录浏览（capabilities.fs_browse）；关闭时不显示选择入口 */
const fsBrowse = ref(false)

/** 路径选择器：'source' 选源数据（目录或文件），'output' 选输出目录 */
const picker = ref<'source' | 'output' | null>(null)
const pickerStart = ref('')

function openPicker(target: 'source' | 'output'): void {
  const raw = (target === 'source' ? source.value : output.value).trim()
  // 文件类线路：现有值是一个文件路径，选择器应停在它所在目录
  const start = target === 'source' && line.value.srcKind === 'file' ? dirOf(raw) : raw
  pickerStart.value = raw ? start : ''
  picker.value = target
}

function onPick(path: string): void {
  if (picker.value === 'source') source.value = path
  else if (picker.value === 'output') output.value = path
  picker.value = null
}

/** 取路径的父目录（兼容 / 与 \ 两种分隔符） */
function dirOf(p: string): string {
  const i = Math.max(p.lastIndexOf('/'), p.lastIndexOf('\\'))
  return i > 0 ? p.slice(0, i) : p
}

const line = computed(() => LINES.find((l) => l.id === selectedId.value) ?? LINES[0]!)
const canSubmit = computed(() => line.value.status === 'ready' && source.value.trim().length > 0 && !submitting.value)

const summary = computed(() => {
  const items: { key: string; label: string; value: string }[] = []
  for (const p of line.value.params) {
    const v = values.value[p.key]
    if (v === undefined || v === '' || v === false) continue
    items.push({ key: p.key, label: p.label, value: v === true ? '开启' : String(v) })
  }
  return items
})

function selectLine(l: Line): void {
  if (l.status === 'planned') {
    toast.info(`${l.name} 尚在规划中`)
    return
  }
  selectedId.value = l.id
  values.value = {}
  errorMsg.value = ''
}

function paramValue(p: ParamDef): string | boolean | number {
  const v = values.value[p.key]
  if (v !== undefined) return v
  return p.def ?? (p.kind === 'switch' ? false : p.kind === 'range' ? 0.5 : '')
}

function setParam(p: ParamDef, v: string | boolean | number): void {
  values.value = { ...values.value, [p.key]: v }
}

/** 各线路的本机示例数据（指向仓库 testdata，便于直接试跑） */
const SAMPLE_SOURCES: Record<string, string> = {
  'osgb->3dtiles': '/Users/yangtanfang/project/2026/AI/tangis/testdata/osgb-real/osgb',
  'image->tiles': '/Users/yangtanfang/project/2026/AI/tangis/testdata/geoimage/small_rgb_4326.tif',
  'terrain->tiles': '/Users/yangtanfang/project/2026/AI/tangis/testdata/geo/dem_cgcs2000_u16.tif',
  'las->3dtiles': '/path/to/cloud.las',
  'model->3dtiles': '/Users/yangtanfang/project/2026/AI/tangis/testdata/osgb/src',
}

function useSample(): void {
  source.value = SAMPLE_SOURCES[line.value.id] ?? ''
}

async function submit(): Promise<void> {
  if (!canSubmit.value) return
  submitting.value = true
  errorMsg.value = ''
  try {
    const params: Record<string, string> = {}
    for (const p of line.value.params) {
      const v = values.value[p.key]
      if (v === undefined || v === '' || v === false) continue
      params[p.key] = v === true ? 'true' : String(v)
    }
    const t = await createTask({
      type: line.value.id,
      source: source.value.trim(),
      output: output.value.trim(),
      params,
    })
    toast.success('任务已创建，开始切片')
    router.push(`/tasks/${encodeURIComponent(t.id)}`)
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '创建任务失败'
    toast.error(errorMsg.value)
  } finally {
    submitting.value = false
  }
}

onMounted(async () => {
  try {
    recent.value = (await listTasks()).slice(0, 5)
  } catch {
    /* 历史失败不阻塞创建 */
  }
  try {
    fsBrowse.value = (await getSystemInfo()).capabilities.fs_browse === true
  } catch {
    /* 后端不可达或版本较旧：退回手填路径 */
    fsBrowse.value = false
  }
})
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">切片转换</h1>
        <p class="page-sub">选择转换线路，设置参数后创建切片任务</p>
      </div>
    </header>

    <div class="workspace">
      <!-- 线路选择：紧凑列表，选中用左侧竖条 -->
      <div class="lines panel">
        <div class="lines-head">转换线路</div>
        <button
          v-for="l in LINES"
          :key="l.id + l.name"
          type="button"
          class="line-item"
          :class="{ active: selectedId === l.id, planned: l.status === 'planned' }"
          @click="selectLine(l)"
        >
          <AppIcon :name="l.icon" :size="15" />
          <span class="line-name">{{ l.name }}</span>
          <span v-if="l.status === 'planned'" class="chip chip-planned">规划中</span>
          <AppIcon v-else name="chevron-right" :size="13" class="line-arrow" />
        </button>
      </div>

      <!-- 参数 -->
      <div class="panel">
        <div class="panel-head">
          <div>
            <div class="panel-title">{{ line.name }}</div>
            <div class="line-desc">{{ line.desc }}</div>
          </div>
          <span class="chip" :class="line.status === 'ready' ? 'chip-ready' : 'chip-planned'">
            {{ line.status === 'ready' ? '可用' : '规划中' }}
          </span>
        </div>

        <div class="form-body">
          <div class="section-head"><span class="section-title">输入</span></div>

          <div class="field">
            <label class="label">源数据路径</label>
            <div class="row">
              <input v-model="source" class="input mono" :placeholder="`本机路径，例：${line.accept}`" />
              <button v-if="fsBrowse" type="button" class="btn btn-sm" @click="openPicker('source')">
                <AppIcon name="folder" :size="12" />
                选择
              </button>
              <button type="button" class="btn btn-sm" @click="useSample">示例</button>
            </div>
            <span class="hint">
              {{ fsBrowse ? '可点「选择」浏览本机目录，也可直接填写' : '直接读取本机路径，不上传文件内容' }}
            </span>
          </div>

          <div class="field">
            <label class="label">输出目录（可留空）</label>
            <div class="row">
              <input v-model="output" class="input mono" placeholder="留空则自动分配到数据目录 outputs/ 下" />
              <button v-if="fsBrowse" type="button" class="btn btn-sm" @click="openPicker('output')">
                <AppIcon name="folder" :size="12" />
                选择
              </button>
            </div>
          </div>

          <template v-if="line.params.length > 0">
            <div class="section-head"><span class="section-title">转换参数</span></div>

            <div v-for="p in line.params" :key="p.key" class="field">
              <label class="label">{{ p.label }}</label>

              <label v-if="p.kind === 'switch'" class="switch">
                <input
                  type="checkbox"
                  :checked="paramValue(p) === true"
                  @change="setParam(p, ($event.target as HTMLInputElement).checked)"
                />
                <span class="track" />
                <span>{{ paramValue(p) === true ? '已开启' : '未开启' }}</span>
              </label>

              <div v-else-if="p.kind === 'range'" class="range-row">
                <input
                  class="slider"
                  type="range"
                  :min="p.min ?? 0"
                  :max="p.max ?? 1"
                  :step="p.step ?? 0.1"
                  :value="Number(paramValue(p)) || 0"
                  @input="setParam(p, Number(($event.target as HTMLInputElement).value))"
                />
                <span class="range-val mono">{{ Number(paramValue(p)).toFixed(1) }}</span>
              </div>

              <input
                v-else
                class="input"
                :class="{ mono: p.kind !== 'number' }"
                :type="p.kind === 'number' ? 'number' : 'text'"
                :placeholder="p.ph"
                :value="String(paramValue(p))"
                @input="setParam(p, ($event.target as HTMLInputElement).value)"
              />

              <span v-if="p.hint" class="hint">{{ p.hint }}</span>
            </div>
          </template>

          <div v-if="errorMsg" class="field-error">{{ errorMsg }}</div>

          <div class="confirm">
            <div class="confirm-body">
              <div class="confirm-line">
                <span class="confirm-key">将创建</span>
                <span class="chip">{{ line.id }}</span>
                <span class="confirm-path mono">{{ source || '（待填源路径）' }}</span>
              </div>
              <div v-if="summary.length > 0" class="confirm-params">
                <span v-for="s in summary" :key="s.key" class="chip">{{ s.label }}：{{ s.value }}</span>
              </div>
            </div>
            <button type="button" class="btn btn-solid" :disabled="!canSubmit" @click="submit">
              <AppIcon name="play" :size="13" />
              {{ submitting ? '提交中…' : '创建任务' }}
            </button>
          </div>
        </div>
      </div>
    </div>

    <PathPickerDialog
      :open="picker !== null"
      :title="picker === 'output' ? '选择输出目录' : '选择源数据路径'"
      :kind="picker === 'output' ? 'dir' : line.srcKind"
      :exts="picker === 'output' ? undefined : line.srcExts"
      :initial-path="pickerStart"
      :confirm-text="picker === 'output' ? '用这个目录' : undefined"
      @pick="onPick"
      @close="picker = null"
    />

    <div v-if="recent.length > 0" class="panel recent">
      <div class="panel-head"><span class="panel-title">最近任务</span></div>
      <div class="thead task-grid">
        <span>ID</span><span>类型</span><span>状态</span><span class="ta-r">创建时间</span>
      </div>
      <div v-for="t in recent" :key="t.id" class="row-line task-grid">
        <span class="mono dim">{{ t.id.slice(0, 8) }}</span>
        <span>{{ t.type }}</span>
        <StatusBadge :status="t.status" />
        <span class="mono dim ta-r">
          {{ t.createdAt ? new Date(t.createdAt).toLocaleString('zh-CN', { hour12: false, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '—' }}
        </span>
      </div>
    </div>
  </section>
</template>

<style scoped>
.workspace { display: grid; grid-template-columns: 296px 1fr; gap: var(--sp-4); align-items: start; }
@media (max-width: 1000px) { .workspace { grid-template-columns: 1fr; } }

/* 线路列表 */
.lines { overflow: hidden; }
.lines-head {
  padding: var(--sp-3) var(--sp-4) var(--sp-2);
  font-size: var(--fs-xs);
  font-weight: 600;
  letter-spacing: 0.08em;
  color: var(--ink-faint);
}
.line-item {
  position: relative;
  width: 100%;
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: 8px var(--sp-4);
  border: none;
  border-top: 1px solid var(--line);
  background: none;
  color: var(--ink-muted);
  font-family: inherit;
  font-size: var(--fs-sm);
  text-align: left;
  cursor: pointer;
  transition: background var(--t-fast), color var(--t-fast);
}
.line-item::before {
  content: '';
  position: absolute;
  left: 0;
  top: 0;
  bottom: 0;
  width: 2px;
  background: transparent;
}
.line-item:hover:not(.planned) { background: var(--surface-hover); color: var(--ink); }
.line-item.active { background: var(--surface-hover); color: var(--ink); font-weight: 550; }
.line-item.active::before { background: var(--accent); }
.line-item.planned { color: var(--ink-faint); cursor: not-allowed; }
.line-name { flex: 1; }
.line-arrow { color: var(--ink-faint); }

.panel-head { align-items: flex-start; }
.line-desc { font-size: var(--fs-xs); color: var(--ink-muted); margin-top: 2px; }
.form-body { padding: var(--sp-4); }
.row { display: flex; gap: var(--sp-2); align-items: center; }

.range-row { display: flex; align-items: center; gap: var(--sp-3); }
.range-val { font-size: var(--fs-sm); color: var(--ink); min-width: 30px; text-align: right; }

/* 确认条：紧凑一行 */
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
.confirm-line { display: flex; align-items: center; gap: var(--sp-2); min-width: 0; }
.confirm-key { font-size: var(--fs-xs); color: var(--ink-faint); }
.confirm-path { font-size: var(--fs-xs); color: var(--ink); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.confirm-params { display: flex; flex-wrap: wrap; gap: 5px; margin-top: var(--sp-2); }

.recent { margin-top: var(--sp-4); overflow: hidden; }
.task-grid {
  display: grid;
  grid-template-columns: 74px 1fr 92px 104px;
  gap: var(--sp-3);
  align-items: center;
  padding: 6px var(--sp-4);
  font-size: var(--fs-sm);
  color: var(--ink-muted);
}
.ta-r { text-align: right; }
.dim { font-size: var(--fs-xs); color: var(--ink-faint); }
</style>
