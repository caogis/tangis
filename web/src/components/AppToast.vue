<script setup lang="ts">
/** 全局 Toast 容器：挂载在 App 根部，渲染 useToast 的共享队列 */
import { useToast } from '../composables/useToast'

const { items, dismiss } = useToast()
</script>

<template>
  <div class="toast-host" aria-live="polite">
    <div
      v-for="t in items"
      :key="t.id"
      class="toast"
      :class="t.kind"
      role="status"
      @click="dismiss(t.id)"
    >
      <span class="toast-mark" aria-hidden="true">
        {{ t.kind === 'ok' ? '✓' : t.kind === 'err' ? '!' : t.kind === 'warn' ? '△' : 'i' }}
      </span>
      <span class="toast-text">{{ t.text }}</span>
    </div>
  </div>
</template>

<style scoped>
.toast-text { flex: 1; }
.toast-mark {
  flex: none;
  width: 16px;
  height: 16px;
  margin-top: 1px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: var(--fs-xs);
  font-weight: 700;
  border-radius: 50%;
  background: var(--bg-sunken);
  color: var(--ink-muted);
}
.toast.ok .toast-mark { color: var(--ok); }
.toast.err .toast-mark { color: var(--err); }
.toast.warn .toast-mark { color: var(--warn); }
</style>
