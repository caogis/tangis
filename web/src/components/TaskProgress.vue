<script setup lang="ts">
import { computed } from 'vue'
import type { TaskProgress } from '../api/types'

const props = defineProps<{
  progress: TaskProgress
  status?: string
}>()

const percent = computed(() => {
  const { done, total } = props.progress
  if (total <= 0) return 0
  return Math.min(Math.round((done / total) * 100), 100)
})

const barCls = computed(() => {
  const s = String(props.status ?? '').toUpperCase()
  if (s === 'SUCCEEDED') return 'is-done'
  if (s === 'FAILED') return 'is-failed'
  return ''
})

const text = computed(() => {
  const { done, total } = props.progress
  if (total <= 0) return '— / —'
  return `${done} / ${total} · ${percent.value}%`
})
</script>

<template>
  <div class="progress-wrap">
    <div class="progress">
      <div class="progress-bar" :class="barCls" :style="{ width: percent + '%' }" />
    </div>
    <div class="progress-text">{{ text }}</div>
  </div>
</template>

<style scoped>
.progress-wrap {
  display: flex;
  align-items: center;
  gap: 12px;
}
.progress { flex: 1; }
.progress-text { flex: none; }
</style>
