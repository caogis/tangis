<script setup lang="ts">
/** 数据导入：拖拽/选择文件或目录上传（或直接填本机路径），识别后建任务 */
import { computed, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { createTask, uploadFiles } from '../api/client'
import AppIcon from '../components/AppIcon.vue'
import { useToast } from '../composables/useToast'
import { CAPABILITIES, STATUS_LABEL } from '../capabilities'

interface FormatRule {
  ext: string[]
  label: string
  taskType: string
  hint: string
  ready: boolean
}

const RULES: FormatRule[] = [
  { ext: ['.osgb'], label: 'OSGB 倾斜摄影', taskType: 'osgb->3dtiles', hint: '整目录导入（含 Tile_*.osgb 与贴图）', ready: true },
  { ext: ['.obj'], label: 'OBJ 模型', taskType: 'osgb->3dtiles', hint: '同目录需含 .mtl 与贴图', ready: true },
  { ext: ['.tif', '.tiff'], label: 'GeoTIFF 影像 / DEM', taskType: 'image->tiles', hint: '支持 GeoTIFF 标签与坐标系识别', ready: true },
  { ext: ['.gltf', '.glb', '.fbx'], label: 'GLTF / FBX 模型', taskType: 'model->3dtiles', hint: '待接入解析（当前 model->3dtiles 仅支持 OBJ）', ready: false },
  { ext: ['.las'], label: 'LAS 点云', taskType: 'las->3dtiles', hint: 'LAS 1.0–1.4，切片为 3D Tiles 点云（pnts）', ready: true },
  { ext: ['.laz'], label: 'LAZ 压缩点云', taskType: 'las->3dtiles', hint: 'LAZ 暂不支持，请先解压为 .las', ready: false },
  { ext: ['.geojson'], label: 'GeoJSON 矢量', taskType: 'vector->mvt', hint: '无需切片任务：到「矢量数据」页导入即可发布 MVT / WFS', ready: false },
  { ext: ['.shp'], label: 'Shapefile 矢量', taskType: 'vector->mvt', hint: '无需切片任务：到「矢量数据」页导入 .shp（连同 .dbf/.prj）', ready: false },
  { ext: ['.gpkg'], label: 'GeoPackage 矢量', taskType: 'vector->mvt', hint: '无需切片任务：到「矢量数据」页导入 .gpkg（内含多表则各注册一层）', ready: false },
  { ext: ['.dxf', '.dwg'], label: 'DXF / DWG', taskType: 'dwg->mvt', hint: 'DXF 优先；DWG 走 ODA 授权', ready: false },
]

const CRS_OPTIONS = [
  { value: '', label: '自动识别（推荐）' },
  { value: 'EPSG:4326', label: 'EPSG:4326 · WGS84 经纬度' },
  { value: 'EPSG:4490', label: 'EPSG:4490 · CGCS2000 经纬度' },
  { value: 'EPSG:3857', label: 'EPSG:3857 · Web 墨卡托' },
  { value: 'EPSG:4479', label: 'EPSG:4479 · CGCS2000 三维地心' },
]

const router = useRouter()
const toast = useToast()
const source = ref('')
const crs = ref('')
const submitting = ref(false)
const errorMsg = ref('')
const formats = CAPABILITIES.filter((c) => c.module === '数据导入')

/* ---------------- 上传（拖拽 / 选择文件 / 选择目录） ---------------- */

interface PickedFile {
  file: File
  /** 相对路径（含目录层级），用于后端还原目录结构 */
  relPath: string
}

const fileInput = ref<HTMLInputElement | null>(null)
const dirInput = ref<HTMLInputElement | null>(null)
const dragOver = ref(false)
const uploading = ref(false)
const uploadPercent = ref(0)
const uploadCount = ref(0)
const uploadLoaded = ref(0)
/** 上传代表文件名：路径是目录时靠它识别格式（如 uploads/x/dem.tif 的 dem.tif） */
const uploadHint = ref('')

const detected = computed<FormatRule | null>(() => {
  // 优先用上传代表名识别：上传目录后 source 是无扩展名的目录路径
  const p = (uploadHint.value || source.value).trim().toLowerCase()
  if (!p) return null
  for (const r of RULES) if (r.ext.some((e) => p.endsWith(e))) return r
  if (!p.includes('.')) return RULES[0]
  return null
})

const canSubmit = computed(
  () => detected.value != null && detected.value.ready && !submitting.value && !uploading.value,
)

/** 各转换线路的本机示例路径 */
const SAMPLE_SOURCES: Record<string, string> = {
  'osgb->3dtiles': '/Users/yangtanfang/project/2026/AI/tangis/testdata/osgb-real/osgb',
  'model->3dtiles': '/Users/yangtanfang/project/2026/AI/tangis/testdata/osgb/src',
  'image->tiles': '/Users/yangtanfang/project/2026/AI/tangis/testdata/geoimage/small_rgb_4326.tif',
  'las->3dtiles': '/path/to/cloud.las',
}

function fillSample(rule: FormatRule): void {
  if (!rule.ready) {
    toast.info(`${rule.label} 尚在规划中`)
    return
  }
  uploadHint.value = ''
  source.value = SAMPLE_SOURCES[rule.taskType] ?? ''
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}

/** 递归遍历拖入的目录项（Drag&Drop 需要手动展开层级） */
function walkEntry(entry: FileSystemEntry, prefix: string, out: PickedFile[]): Promise<void> {
  return new Promise((resolve) => {
    if (entry.isFile) {
      ;(entry as FileSystemFileEntry).file(
        (f) => {
          out.push({ file: f, relPath: prefix + entry.name })
          resolve()
        },
        () => resolve(),
      )
      return
    }
    const reader = (entry as FileSystemDirectoryEntry).createReader()
    const dirPrefix = `${prefix}${entry.name}/`
    // readEntries 每次最多返回一批，必须反复读直到返回空
    const readBatch = (): void => {
      reader.readEntries(
        (batch) => {
          if (batch.length === 0) {
            resolve()
            return
          }
          void (async () => {
            for (const child of batch) await walkEntry(child, dirPrefix, out)
            readBatch()
          })()
        },
        () => resolve(),
      )
    }
    readBatch()
  })
}

async function filesFromDrop(dt: DataTransfer): Promise<PickedFile[]> {
  const entries = Array.from(dt.items ?? [])
    .filter((it) => it.kind === 'file')
    .map((it) => (it.webkitGetAsEntry ? (it.webkitGetAsEntry() as FileSystemEntry | null) : null))
    .filter((e): e is FileSystemEntry => e != null)
  if (entries.length === 0) {
    return Array.from(dt.files ?? []).map((f) => ({ file: f, relPath: f.name }))
  }
  const out: PickedFile[] = []
  for (const en of entries) await walkEntry(en, '', out)
  return out
}

function filesFromInput(input: HTMLInputElement | null): PickedFile[] {
  const list = input?.files
  if (!list) return []
  return Array.from(list).map((f) => ({
    // 选择目录时浏览器提供 webkitRelativePath（含顶层目录名）
    file: f,
    relPath: f.webkitRelativePath || f.name,
  }))
}

function onDrop(e: DragEvent): void {
  dragOver.value = false
  if (!e.dataTransfer || uploading.value) return
  void (async () => {
    const picked = await filesFromDrop(e.dataTransfer as DataTransfer)
    await startUpload(picked)
  })()
}

function onPickFiles(e: Event): void {
  const input = e.target as HTMLInputElement
  const picked = filesFromInput(input)
  input.value = '' // 允许重复选择同一批文件
  void startUpload(picked)
}

async function startUpload(picked: PickedFile[]): Promise<void> {
  if (picked.length === 0) {
    toast.info('没有可上传的文件')
    return
  }
  uploading.value = true
  uploadPercent.value = 0
  uploadCount.value = picked.length
  uploadLoaded.value = 0
  errorMsg.value = ''
  try {
    const res = await uploadFiles(
      picked.map((p) => p.file),
      picked.map((p) => p.relPath),
      (loaded, total) => {
        uploadLoaded.value = loaded
        uploadPercent.value = total > 0 ? Math.min(99, Math.round((loaded / total) * 100)) : 0
      },
    )
    source.value = res.root
    uploadHint.value = picked[0].relPath
    uploadPercent.value = 100
    toast.success(`已上传 ${res.files} 个文件（${formatBytes(res.bytes)}）`)
  } catch (err) {
    errorMsg.value = err instanceof Error ? err.message : '上传失败'
    toast.error(errorMsg.value)
  } finally {
    uploading.value = false
  }
}

async function submit(): Promise<void> {
  if (!canSubmit.value || !detected.value) return
  submitting.value = true
  errorMsg.value = ''
  try {
    const params: Record<string, string> = {}
    if (crs.value) params.crs = crs.value
    const t = await createTask({ type: detected.value.taskType, source: source.value.trim(), params })
    toast.success('导入任务已创建')
    router.push(`/tasks/${encodeURIComponent(t.id)}`)
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '创建失败'
    toast.error(errorMsg.value)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">数据导入</h1>
        <p class="page-sub">选择本机数据，自动识别格式并创建处理任务</p>
      </div>
      <div class="toolbar">
        <RouterLink class="btn btn-sm" to="/convert">改用切片转换</RouterLink>
      </div>
    </header>

    <div class="workspace">
      <!-- 左：格式选择 -->
      <div class="panel pick-list">
        <div class="pick-list-head">支持格式</div>
        <button
          v-for="r in RULES"
          :key="r.label"
          type="button"
          class="pick-item"
          :class="{ disabled: !r.ready }"
          @click="fillSample(r)"
        >
          <AppIcon :name="r.ready ? 'import' : 'convert'" :size="14" />
          <span class="grow">
            <span class="rule-label">{{ r.label }}</span>
            <span class="rule-ext mono">{{ r.ext.join(' / ') }}</span>
          </span>
          <span class="chip" :class="r.ready ? 'chip-ready' : 'chip-planned'">
            {{ r.ready ? '可用' : '规划中' }}
          </span>
        </button>
      </div>

      <!-- 右：识别与提交 -->
      <div>
        <div class="panel block">
          <div class="panel-head">
            <span class="panel-title">数据源与坐标系</span>
            <span v-if="detected" class="chip chip-active">{{ detected.label }}</span>
          </div>
          <div class="form-body">
            <div class="field">
              <label class="label">上传数据</label>
              <div
                class="dropzone"
                :class="{ over: dragOver, busy: uploading }"
                @dragover.prevent="dragOver = true"
                @dragleave.prevent="dragOver = false"
                @drop.prevent="onDrop"
              >
                <!-- 两个隐藏输入：普通文件（可多选）/ 目录（webkitdirectory，保留层级） -->
                <input ref="fileInput" type="file" multiple class="hidden-input" @change="onPickFiles" />
                <input
                  ref="dirInput"
                  type="file"
                  webkitdirectory
                  multiple
                  class="hidden-input"
                  @change="onPickFiles"
                />

                <template v-if="!uploading">
                  <div class="dz-title">拖拽文件或文件夹到此处</div>
                  <div class="dz-actions">
                    <button class="btn btn-sm" type="button" @click="fileInput?.click()">选择文件</button>
                    <button class="btn btn-sm" type="button" @click="dirInput?.click()">选择目录</button>
                  </div>
                  <div class="dz-hint">OSGB 请选到含 Tile_*.osgb 的目录（自动保留层级）</div>
                </template>

                <template v-else>
                  <div class="dz-title">上传中 {{ uploadPercent }}%</div>
                  <div class="progress"><div class="progress-bar" :style="{ width: uploadPercent + '%' }" /></div>
                  <div class="dz-hint">{{ uploadCount }} 个文件 · {{ formatBytes(uploadLoaded) }}</div>
                </template>
              </div>
            </div>

            <div class="field">
              <label class="label">数据路径</label>
              <input v-model="source" class="input mono" placeholder="上传后自动填入；也可直接填本机目录或文件" />
              <span class="hint">
                上传的目录<b>可直接导入</b>；直接填本机路径则不上传文件内容（适合超大数据）
              </span>
            </div>

            <div class="field">
              <label class="label">坐标系</label>
              <select v-model="crs" class="select">
                <option v-for="o in CRS_OPTIONS" :key="o.value" :value="o.value">{{ o.label }}</option>
              </select>
              <span class="hint">留空时由内核依据 GeoTIFF GeoKey / .prj 自动识别</span>
            </div>

            <div class="detect">
              <template v-if="detected">
                <div class="detect-line">
                  <span class="detect-key">识别结果</span>
                  <span class="detect-val">{{ detected.label }}</span>
                  <span class="chip" :class="detected.ready ? 'chip-ready' : 'chip-planned'">
                    {{ detected.ready ? '可导入' : '规划中' }}
                  </span>
                </div>
                <div class="detect-hint">{{ detected.hint }} · 转换线路 <span class="mono">{{ detected.taskType }}</span></div>
              </template>
              <div v-else class="detect-hint">输入路径后自动识别格式</div>
            </div>

            <div v-if="errorMsg" class="field-error">{{ errorMsg }}</div>

            <div class="form-actions">
              <button type="button" class="btn btn-solid" :disabled="!canSubmit" @click="submit">
                <AppIcon name="import" :size="13" />
                {{ submitting ? '导入中…' : '导入并处理' }}
              </button>
              <span class="hint">
                将按识别出的转换线路创建任务；DEM 地形请改用「切片转换 → 地形 → Terrain」（需显式声明类型，避免与影像线混淆）
              </span>
            </div>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head"><span class="panel-title">导入能力状态</span></div>
          <div class="thead data-grid cap-grid">
            <span>格式</span><span>说明</span><span>状态</span>
          </div>
          <div v-for="f in formats" :key="f.name" class="data-row data-grid cap-grid">
            <span class="cap-name">{{ f.name }}</span>
            <span class="cap-note">{{ f.note }}</span>
            <span>
              <span class="chip" :class="f.status === 'ready' ? 'chip-ready' : f.status === 'partial' ? 'chip-partial' : 'chip-planned'">
                {{ STATUS_LABEL[f.status] }}
              </span>
            </span>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.form-body { padding: var(--sp-4); }
.rule-label { display: block; color: inherit; }
.rule-ext { display: block; font-size: var(--fs-xs); color: var(--ink-faint); }

/* 上传区：拖拽目标 + 两个入口按钮，上传中切换为进度条 */
.hidden-input { display: none; }
.dropzone {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-4);
  border: 1px dashed var(--line-strong);
  border-radius: var(--radius);
  background: var(--bg-sunken);
  text-align: center;
  transition: border-color var(--t-fast), background var(--t-fast);
}
.dropzone.over { border-color: var(--primary); background: var(--accent-soft); }
.dropzone.busy { border-style: solid; }
.dz-title { font-size: var(--fs-sm); color: var(--ink); }
.dz-actions { display: flex; gap: var(--sp-2); }
.dz-hint { font-size: var(--fs-xs); color: var(--ink-faint); }
.progress {
  width: 100%;
  height: 6px;
  overflow: hidden;
  background: var(--surface);
  border: 1px solid var(--line);
  border-radius: 999px;
}
.progress-bar { height: 100%; background: var(--primary); transition: width 160ms linear; }

.detect {
  padding: var(--sp-3) var(--sp-4);
  background: var(--bg-sunken);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  margin-bottom: var(--sp-4);
}
.detect-line { display: flex; align-items: center; gap: var(--sp-2); }
.detect-key { font-size: var(--fs-xs); color: var(--ink-faint); }
.detect-val { font-size: var(--fs-sm); color: var(--ink); }
.detect-hint { margin-top: var(--sp-1); font-size: var(--fs-xs); color: var(--ink-faint); }

.cap-grid { grid-template-columns: 190px 1fr 90px; }
.cap-name { color: var(--ink); }
.cap-note { font-size: var(--fs-xs); color: var(--ink-faint); }
</style>
