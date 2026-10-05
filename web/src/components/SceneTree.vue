<script setup lang="ts">
/**
 * 场景树（F-05）：展示 tileset.json 的真实层级结构。
 * 支持展开/折叠、点击定位（可定位的节点显示定位按钮）、全展开/收起。
 * 结构数据完全来自 tileset.json，不做任何估算或伪造。
 */
import { computed, ref } from 'vue'
import { treeStats, type TreeNode } from '../utils/tileset'

const props = defineProps<{
  root: TreeNode | null
  loading?: boolean
  error?: string
}>()

const emit = defineEmits<{
  (e: 'locate', node: TreeNode): void
}>()

/** 折叠状态：以「节点路径」为 key（同名节点可区分） */
const collapsed = ref<Record<string, boolean>>({})

const rows = computed<{ node: TreeNode; path: string; hasChildren: boolean; collapsed: boolean }[]>(() => {
  const out: { node: TreeNode; path: string; hasChildren: boolean; collapsed: boolean }[] = []
  const walk = (n: TreeNode, path: string): void => {
    const key = `${path}/${n.name}`
    const hasChildren = n.children.length > 0
    const isCollapsed = collapsed.value[key] === true
    out.push({ node: n, path: key, hasChildren, collapsed: isCollapsed })
    if (hasChildren && !isCollapsed) {
      for (const c of n.children) walk(c, key)
    }
  }
  if (props.root) walk(props.root, '')
  return out
})

const stats = computed(() => treeStats(props.root))

function toggle(key: string): void {
  collapsed.value = { ...collapsed.value, [key]: !collapsed.value[key] }
}

function expandAll(): void {
  collapsed.value = {}
}
function collapseAll(): void {
  const next: Record<string, boolean> = {}
  const walk = (n: TreeNode, path: string): void => {
    const key = `${path}/${n.name}`
    if (n.children.length > 0) {
      next[key] = true
      for (const c of n.children) walk(c, key)
    }
  }
  if (props.root) walk(props.root, '')
  collapsed.value = next
}

function fmtGe(n: number): string {
  if (!Number.isFinite(n)) return '—'
  if (n === 0) return '0'
  return n >= 1000 ? n.toFixed(0) : n.toFixed(1)
}
</script>

<template>
  <div class="scene-tree">
    <div class="tree-head">
      <span class="label">场景树</span>
      <span v-if="root" class="tree-meta">{{ stats.nodes }} 节点 · {{ stats.maxDepth }} 层</span>
      <span v-if="root" class="tree-actions">
        <button type="button" class="mini" @click="expandAll">展开</button>
        <button type="button" class="mini" @click="collapseAll">收起</button>
      </span>
    </div>

    <div v-if="loading" class="tree-state">正在读取 tileset…</div>
    <div v-else-if="error" class="tree-state tree-error">{{ error }}</div>
    <div v-else-if="!root" class="tree-state">未加载场景</div>

    <ul v-else class="tree-list">
      <li
        v-for="r in rows"
        :key="r.path"
        class="tree-row"
        :style="{ paddingLeft: `${6 + r.node.depth * 12}px` }"
      >
        <button
          v-if="r.hasChildren"
          type="button"
          class="tree-caret"
          :title="r.collapsed ? '展开' : '折叠'"
          @click="toggle(r.path)"
        >
          {{ r.collapsed ? '▸' : '▾' }}
        </button>
        <span v-else class="tree-caret placeholder">·</span>

        <span class="tree-name" :title="r.node.uri || r.node.name">{{ r.node.name }}</span>
        <span class="tree-ge" :title="'geometricError'">ge {{ fmtGe(r.node.geometricError) }}</span>
        <button type="button" class="mini locate" title="定位到该节点" @click="emit('locate', r.node)">
          定位
        </button>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.scene-tree { display: flex; flex-direction: column; }
.tree-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding-bottom: 8px;
  border-bottom: 1px solid var(--border);
  margin-bottom: 6px;
}
.label { font-size: var(--fs-xs); letter-spacing: 0.12em; color: var(--ink-faint); text-transform: uppercase; }
.tree-meta { font-size: var(--fs-xs); color: var(--ink-faint); }
.tree-actions { margin-left: auto; display: flex; gap: 6px; }

.mini {
  padding: 2px 7px;
  border: 1px solid var(--border);
  border-radius: 2px;
  background: var(--bg-sunken);
  color: var(--ink-faint);
  font-size: var(--fs-xs);
  cursor: pointer;
}
.mini:hover { color: var(--ink); }

.tree-state { padding: 12px 0; font-size: var(--fs-xs); color: var(--ink-faint); }
.tree-error { color: var(--danger, #a75d5d); }

.tree-list { list-style: none; margin: 0; padding: 0; max-height: 320px; overflow-y: auto; }
.tree-row {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 3px 4px;
  font-size: var(--fs-xs);
  color: var(--ink-muted);
}
.tree-row:hover { background: var(--surface); }
.tree-caret {
  width: 14px;
  flex: none;
  text-align: center;
  border: none;
  background: none;
  color: var(--ink-faint);
  cursor: pointer;
  font-size: var(--fs-xs);
  padding: 0;
}
.tree-caret.placeholder { cursor: default; }
.tree-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--mono, monospace);
  font-size: var(--fs-xs);
}
.tree-ge { flex: none; font-size: var(--fs-xs); color: var(--ink-faint); }
.locate { flex: none; opacity: 0; transition: opacity var(--t-fast); }
.tree-row:hover .locate { opacity: 1; }
</style>
