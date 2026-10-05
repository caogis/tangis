/**
 * 全局轻量通知（Toast）：操作反馈基建。
 *
 * 用法：
 *   const { success, error, info } = useToast()
 *   success('任务已创建')
 *
 * 状态为模块级共享，由 App.vue 中的 <AppToast /> 统一渲染，
 * 因此在任何组件（含非父子关系）调用都能显示。
 */
import { ref } from 'vue'

export type ToastKind = 'info' | 'ok' | 'warn' | 'err'

export interface ToastItem {
  id: number
  text: string
  kind: ToastKind
}

const items = ref<ToastItem[]>([])
let seq = 0

function push(text: string, kind: ToastKind = 'info', ms = 3000): void {
  if (!text) return
  const id = ++seq
  items.value = [...items.value, { id, text, kind }]
  setTimeout(() => dismiss(id), ms)
}

function dismiss(id: number): void {
  items.value = items.value.filter((t) => t.id !== id)
}

export function useToast() {
  return {
    items,
    dismiss,
    toast: push,
    info: (t: string, ms?: number) => push(t, 'info', ms),
    success: (t: string, ms?: number) => push(t, 'ok', ms),
    warn: (t: string, ms?: number) => push(t, 'warn', ms),
    error: (t: string, ms?: number) => push(t, 'err', ms ?? 4200),
  }
}
