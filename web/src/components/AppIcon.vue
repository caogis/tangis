<script setup lang="ts">
/** 内联 SVG 图标组件：路径表见 icon-paths.ts，颜色随 currentColor 继承 */
import { ICON_PATHS, type IconName } from './icon-paths'

withDefaults(defineProps<{ name: IconName; size?: number }>(), { size: 16 })
</script>

<template>
  <svg
    class="icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.6"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path :d="ICON_PATHS[name]" />
  </svg>
</template>

<style scoped>
.icon { display: block; flex: none; }
</style>
