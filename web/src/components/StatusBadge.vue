<script setup lang="ts">
import { computed } from 'vue'
import type { TaskStatus } from '../api/types'

const props = defineProps<{ status: TaskStatus | string }>()

const cls = computed(() => {
  const s = String(props.status).toUpperCase()
  return `badge badge-${s.toLowerCase()}`
})

const label = computed(() => {
  const map: Record<string, string> = {
    PENDING: '排队中',
    RUNNING: '运行中',
    SUCCEEDED: '已完成',
    FAILED: '失败',
    // F-04 任务控制新增状态
    CANCELLED: '已取消',
    PAUSED: '已暂停',
  }
  return map[String(props.status).toUpperCase()] ?? String(props.status)
})
</script>

<template>
  <span class="badge" :class="cls">{{ label }}</span>
</template>
