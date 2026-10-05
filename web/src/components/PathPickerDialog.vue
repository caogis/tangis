<script setup lang="ts">
/**
 * 路径选择器：逐级浏览服务端本机目录，选中后回填任务表单。
 *
 * 数据面是 GET /api/v1/fs/browse + POST /api/v1/fs/mkdir——浏览的是**服务端
 * （桌面单机版即本机）**的文件系统，与浏览器所在机器无关，故不使用
 * <input type="file">（那只能拿到浏览器侧路径，服务端不可达）。
 *
 * 用法：<PathPickerDialog :open kind="dir" @pick="v => source = v" @close="open = false" />
 */
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { browseFs, mkdirFs } from '../api/client'
import type { FsBrowseResult, FsEntry } from '../api/types'
import AppIcon from './AppIcon.vue'

const props = defineProps<{
  open: boolean
  title: string
  /** dir：选目录（输出目录 / OSGB 数据目录）；file：选文件（按 exts 过滤） */
  kind: 'dir' | 'file'
  /** kind=file 时的后缀白名单，如 ['.tif', '.tiff'] */
  exts?: string[]
  /** 打开时的起始路径（一般取当前表单值或其父目录） */
  initialPath?: string
  /** 确认按钮文案 */
  confirmText?: string
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'pick', path: string): void
}>()

const loading = ref(false)
const errorMsg = ref('')
const result = ref<FsBrowseResult | null>(null)
const pathInput = ref('')
const newName = ref('')
const creating = ref(false)
const selectedFile = ref<FsEntry | null>(null)

const current = computed(() => result.value?.path ?? '')
const canConfirm = computed(() =>
  props.kind === 'file' ? selectedFile.value !== null : current.value.length > 0,
)
/** 提示文案：区分"选目录"与"选文件"两种用途 */
const confirmLabel = computed(
  () => props.confirmText ?? (props.kind === 'file' ? '选择该文件' : '选择此目录'),
)

watch(
  () => props.open,
  (v) => {
    if (!v) return
    errorMsg.value = ''
    newName.value = ''
    void load(props.initialPath ?? '')
    window.addEventListener('keydown', onKey)
  },
)
watch(
  () => props.open,
  (v) => {
    if (!v) window.removeEventListener('keydown', onKey)
  },
)
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))

function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape') {
    emit('close')
  }
}

async function load(p?: string): Promise<void> {
  loading.value = true
  errorMsg.value = ''
  selectedFile.value = null
  try {
    result.value = await browseFs({
      path: p ?? '',
      kind: props.kind,
      ext: props.exts && props.exts.length > 0 ? props.exts.join(',') : undefined,
    })
    pathInput.value = result.value.path
  } catch (e) {
    result.value = null
    errorMsg.value = e instanceof Error ? e.message : '读取目录失败'
  } finally {
    loading.value = false
  }
}

/** 目录进入下一级；文件（kind=file）则选中 */
function enter(entry: FsEntry): void {
  if (entry.is_dir) {
    void load(entry.path)
    return
  }
  if (props.kind === 'file') selectedFile.value = entry
}

function goUp(): void {
  const parent = result.value?.parent
  if (parent) void load(parent)
}

function goInput(): void {
  void load(pathInput.value.trim())
}

async function createDir(): Promise<void> {
  const name = newName.value.trim()
  if (!name || !current.value) return
  creating.value = true
  errorMsg.value = ''
  try {
    const created = await mkdirFs(current.value, name)
    newName.value = ''
    await load(created)
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '创建目录失败'
  } finally {
    creating.value = false
  }
}

function confirm(): void {
  if (!canConfirm.value) return
  emit('pick', props.kind === 'file' ? (selectedFile.value?.path ?? '') : current.value)
  emit('close')
}

function fmtSize(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GB`
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(0)} KB`
  return `${n} B`
}
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="mask" @click.self="emit('close')">
      <div class="picker panel" role="dialog" aria-modal="true">
        <div class="picker-head">
          <div>
            <div class="panel-title">{{ title }}</div>
            <div class="picker-sub">
              浏览服务端本机路径{{ kind === 'file' ? '，选中文件后回填' : '，进入目录后选择' }}
            </div>
          </div>
          <button type="button" class="icon-btn" title="关闭" @click="emit('close')">
            <AppIcon name="close" :size="14" />
          </button>
        </div>

        <div class="picker-bar">
          <input
            v-model="pathInput"
            class="input mono"
            placeholder="/ 留空回车回到顶层"
            @keydown.enter.prevent="goInput"
          />
          <button
            type="button"
            class="icon-btn"
            title="上一级"
            :disabled="!result?.parent"
            @click="goUp"
          >
            <AppIcon name="arrow-up" :size="14" />
          </button>
          <button type="button" class="icon-btn" title="刷新" @click="load(current)">
            <AppIcon name="refresh" :size="14" />
          </button>
        </div>

        <div class="picker-list">
          <div v-if="loading" class="picker-tip">读取中…</div>
          <div v-else-if="errorMsg" class="picker-tip err">{{ errorMsg }}</div>
          <div v-else-if="!result || result.entries.length === 0" class="picker-tip">
            该目录下没有可显示的内容
          </div>
          <template v-else>
            <button
              v-for="e in result.entries"
              :key="e.path"
              type="button"
              class="fs-row"
              :class="{ sel: !e.is_dir && selectedFile?.path === e.path }"
              @click="enter(e)"
            >
              <AppIcon :name="e.is_dir ? 'folder' : 'file'" :size="14" class="fs-icon" />
              <span class="fs-name mono">{{ e.name }}</span>
              <span v-if="!e.is_dir && e.size > 0" class="fs-size mono">{{ fmtSize(e.size) }}</span>
              <AppIcon v-if="e.is_dir" name="chevron-right" :size="13" class="fs-arrow" />
            </button>
            <div v-if="result.truncated" class="picker-tip">条目过多已截断，可继续进入子目录</div>
          </template>
        </div>

        <div class="picker-foot">
          <div class="new-dir">
            <input
              v-model="newName"
              class="input mono"
              placeholder="新建文件夹名"
              @keydown.enter.prevent="createDir"
            />
            <button
              type="button"
              class="btn btn-sm"
              :disabled="!newName.trim() || !current || creating"
              @click="createDir"
            >
              <AppIcon name="plus" :size="12" />
              {{ creating ? '创建中…' : '新建' }}
            </button>
          </div>
          <div class="foot-actions">
            <span v-if="result && !result.writable" class="hint">当前目录不可写</span>
            <button type="button" class="btn btn-sm" @click="emit('close')">取消</button>
            <button type="button" class="btn btn-sm btn-solid" :disabled="!canConfirm" @click="confirm">
              {{ confirmLabel }}
            </button>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.mask {
  position: fixed;
  inset: 0;
  z-index: 60;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--sp-4);
  background: color-mix(in oklch, var(--bg) 55%, transparent);
  backdrop-filter: blur(2px);
}

.picker {
  width: 620px;
  max-width: 100%;
  max-height: 78vh;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.picker-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--sp-3);
  padding: var(--sp-3) var(--sp-4);
  border-bottom: 1px solid var(--line);
}
.picker-sub { font-size: var(--fs-xs); color: var(--ink-faint); margin-top: 2px; }

.picker-bar {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
  border-bottom: 1px solid var(--line);
}

.picker-list {
  flex: 1;
  min-height: 220px;
  overflow: auto;
  padding: var(--sp-2) 0;
}
.picker-tip { padding: var(--sp-3) var(--sp-4); font-size: var(--fs-xs); color: var(--ink-faint); }
.picker-tip.err { color: var(--err); }

.fs-row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  width: 100%;
  padding: 5px var(--sp-4);
  border: none;
  background: none;
  color: var(--ink-muted);
  font-family: inherit;
  font-size: var(--fs-sm);
  text-align: left;
  cursor: pointer;
  transition: background var(--t-fast), color var(--t-fast);
}
.fs-row:hover { background: var(--surface-hover); color: var(--ink); }
.fs-row.sel { background: var(--accent-soft); color: var(--accent-ink); }
.fs-icon { color: var(--ink-faint); }
.fs-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.fs-size { font-size: var(--fs-xs); color: var(--ink-faint); }
.fs-arrow { color: var(--ink-faint); }

.picker-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
  padding: var(--sp-3) var(--sp-4);
  border-top: 1px solid var(--line);
  background: var(--bg-sunken);
}
.new-dir { display: flex; align-items: center; gap: var(--sp-2); min-width: 0; }
.new-dir .input { width: 200px; }
.foot-actions { display: flex; align-items: center; gap: var(--sp-2); }
</style>
