<script setup lang="ts">
/**
 * 应用外壳 —— "仪器面板"
 *
 * 结构：顶部状态条（品牌 / 当前页 / 状态 LED 组 / 主题）+ 紧凑侧栏 + 内容区。
 * 设计要点（见 .impeccable.md）：
 *  - 琥珀只表示"正在发生"：活动任务 LED 与计数用它，其余一律青灰；
 *  - 状态条承载全局真实性信息（服务可达性、活动任务数），而非装饰；
 *  - 侧栏用左侧 2px 竖条 + 背景表示选中，不用大色块。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'
import AppIcon from './components/AppIcon.vue'
import AppToast from './components/AppToast.vue'
import type { IconName } from './components/icon-paths'
import { getSystemInfo, listTasks } from './api/client'

const route = useRoute()

const year = new Date().getFullYear()

interface NavItem {
  path: string
  label: string
  icon: IconName
  hint: string
  /** 外部打开（新标签页），用于需要独占整屏的页面（如全屏预览） */
  external?: boolean
  /**
   * 依赖的能力开关（GET /api/v1/system.capabilities）。
   * 后端上报为 false（当前模式未装配/未实现）时侧栏灰显，避免用户点进去才发现不可用。
   */
  requires?: string
}
interface NavGroup { title: string; items: NavItem[] }

const groups: NavGroup[] = [
  { title: '总览', items: [{ path: '/', label: '工作台', icon: 'dashboard', hint: '任务与能力总览' }] },
  {
    title: '数据',
    items: [
      { path: '/import', label: '数据导入', icon: 'import', hint: '拖拽上传或填写本机路径' },
      {
        path: '/vector',
        label: '矢量数据',
        icon: 'vector',
        hint: '本地矢量文件 → 图层 → MVT / WFS 服务',
        requires: 'vector',
      },
    ],
  },
  {
    title: '处理',
    items: [
      { path: '/convert', label: '切片转换', icon: 'convert', hint: '创建切片任务' },
      { path: '/qc', label: '数据质检', icon: 'qc', hint: '几何质检报告' },
      {
        path: '/compliance',
        label: '合规工具',
        icon: 'shield',
        hint: '坐标系识别 / 七参数转换 / DEM 脱密',
      },
      { path: '/edit', label: '模型编辑', icon: 'edit', hint: '抠除 / 压平 / 对齐' },
    ],
  },
  {
    title: '服务',
    items: [
      { path: '/services', label: '服务分发', icon: 'services', hint: '已发布服务与协议入口' },
      { path: '/preview', label: '三维预览', icon: 'globe', hint: 'Cesium 场景、场景树与测量' },
    ],
  },
  {
    title: '系统',
    items: [
      { path: '/tasks', label: '任务中心', icon: 'tasks', hint: '队列与历史' },
      { path: '/settings', label: '系统设置', icon: 'settings', hint: '运行信息与能力' },
      { path: '/edition', label: '版本对照', icon: 'edition', hint: '社区版 / 专业版 / 企业版' },
    ],
  },
]

/** 全屏（无外壳）路由：命中 meta.bare 时不渲染侧栏与顶栏，页面独占整屏 */
const isBare = computed(() => route.meta.bare === true)

const currentTitle = computed(() => {
  if (route.path.startsWith('/tasks/') && route.path !== '/tasks') return '任务详情'
  for (const g of groups) {
    for (const it of g.items) {
      if (it.path === '/' ? route.path === '/' : route.path.startsWith(it.path)) return it.label
    }
  }
  return 'TanGIS'
})

function isActive(path: string): boolean {
  if (path === '/') return route.path === '/'
  if (path === '/tasks') return route.path.startsWith('/tasks')
  return route.path.startsWith(path)
}

/* ---------- 主题 ---------- */
type Theme = 'light' | 'dark'
const theme = ref<Theme>((document.documentElement.getAttribute('data-theme') as Theme) ?? 'light')

function toggleTheme(): void {
  theme.value = theme.value === 'light' ? 'dark' : 'light'
  document.documentElement.setAttribute('data-theme', theme.value)
  try { localStorage.setItem('tangis-theme', theme.value) } catch { /* 隐私模式 */ }
}

/* ---------- 状态条数据：服务可达性 + 活动任务数（真实值，不做假） ---------- */
const healthy = ref<boolean | null>(null)
const activeTasks = ref<number | null>(null)
let timer: ReturnType<typeof setInterval> | null = null

async function poll(): Promise<void> {
  try {
    const res = await fetch('/healthz', { cache: 'no-store' })
    healthy.value = res.ok
  } catch {
    healthy.value = false
  }
  try {
    const tasks = await listTasks()
    activeTasks.value = tasks.filter((t) => t.status === 'RUNNING' || t.status === 'PENDING').length
  } catch {
    activeTasks.value = null
  }
}

/* ---------- 能力开关：侧栏据此灰显"当前模式不可用"的入口 ---------- */
const capabilities = ref<Record<string, boolean>>({})

async function loadCapabilities(): Promise<void> {
  try {
    capabilities.value = (await getSystemInfo()).capabilities
  } catch {
    // 读不到就保持空表：不灰显任何入口（后端不可达时问题在别处）
    capabilities.value = {}
  }
}

/** 该导航项在当前运行模式下是否不可用（后端明确上报 false） */
function navDisabled(item: NavItem): boolean {
  return item.requires != null && capabilities.value[item.requires] === false
}

function navTitle(item: NavItem): string {
  return navDisabled(item) ? `${item.hint} —— 当前运行模式不可用` : item.hint
}

onMounted(() => {
  void poll()
  void loadCapabilities()
  timer = setInterval(poll, 10000)
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <!-- 全屏路由（meta.bare）：不套外壳，页面独占整屏，适合在新窗口打开预览 -->
  <div v-if="isBare" class="bare-shell">
    <RouterView />
  </div>

  <div v-else class="shell">
    <header class="statusbar">
      <div class="brand">
        <span class="brand-mark" aria-hidden="true" />
        <span class="brand-name">TanGIS</span>
        <span class="brand-tag mono">v0.1</span>
      </div>

      <span class="divider-v" aria-hidden="true" />
      <span class="crumb">{{ currentTitle }}</span>

      <!-- 状态 LED 组：把"系统是否正常"变成一眼可读的仪表读数 -->
      <div class="leds">
        <span class="led" :class="{ ok: healthy === true, err: healthy === false }" title="服务进程">
          <span class="led-dot" />
          {{ healthy === true ? '服务在线' : healthy === false ? '服务离线' : '检测中' }}
        </span>
        <span
          class="led"
          :class="{ active: (activeTasks ?? 0) > 0 }"
          title="进行中的切片任务"
        >
          <span class="led-dot" />
          活动任务
          <b class="led-num mono">{{ activeTasks ?? '—' }}</b>
        </span>
      </div>

      <button
        type="button"
        class="icon-btn"
        :title="theme === 'light' ? '切换到深色主题' : '切换到浅色主题'"
        @click="toggleTheme"
      >
        <AppIcon :name="theme === 'light' ? 'moon' : 'sun'" :size="15" />
      </button>
    </header>

    <div class="body">
      <aside class="sidebar">
        <nav class="nav">
          <div v-for="g in groups" :key="g.title" class="nav-group">
            <div class="nav-group-title">{{ g.title }}</div>
            <template v-for="item in g.items" :key="item.path">
              <a
                v-if="item.external"
                class="nav-item"
                :class="{ active: isActive(item.path) }"
                :href="item.path"
                target="_blank"
                rel="noopener"
                :title="item.hint"
              >
                <AppIcon :name="item.icon" :size="15" />
                <span class="nav-label">{{ item.label }}</span>
                <AppIcon name="external" :size="11" class="nav-ext" />
              </a>
              <RouterLink
                v-else
                class="nav-item"
                :class="{ active: isActive(item.path), disabled: navDisabled(item) }"
                :to="item.path"
                :title="navTitle(item)"
              >
                <AppIcon :name="item.icon" :size="15" />
                <span class="nav-label">{{ item.label }}</span>
                <span v-if="navDisabled(item)" class="nav-off">不可用</span>
              </RouterLink>
            </template>
          </div>
        </nav>
      </aside>

      <main class="main">
        <RouterView />
      </main>
    </div>

    <footer class="footer">
      <span class="footer-copy">© {{ year }} 武汉草果科技有限公司 · TanGIS 新一代 GIS 数据处理与分发平台</span>
      <div class="footer-editions">
        <RouterLink to="/edition" class="edition edition-current" title="当前部署为社区版（开源免费），点击查看版本功能对照">社区版</RouterLink>
        <RouterLink to="/edition" class="footer-link">版本功能对照</RouterLink>
      </div>
    </footer>

    <AppToast />
  </div>
</template>

<style scoped>
.shell { display: flex; flex-direction: column; height: 100%; }

/* 全屏页容器：占满视口，无内边距，交给页面自己掌控布局 */
.bare-shell { height: 100%; overflow: hidden; }
.nav-ext { margin-left: auto; opacity: 0.45; }

/* ---------- 顶部状态条 ---------- */
.statusbar {
  height: var(--topbar-h);
  flex: none;
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  padding: 0 var(--sp-4);
  background: var(--surface);
  border-bottom: 1px solid var(--line);
  z-index: 10;
}

.brand { display: flex; align-items: center; gap: var(--sp-2); }
.brand-mark {
  width: 15px;
  height: 15px;
  border-radius: var(--radius-sm);
  background: var(--primary);
  box-shadow: inset 0 0 0 1px rgb(255 255 255 / 14%);
}
.brand-name { font-size: var(--fs-base); font-weight: 600; letter-spacing: -0.01em; }
.brand-tag { font-size: var(--fs-xs); color: var(--ink-faint); }

.divider-v { width: 1px; height: 18px; background: var(--line); }
.crumb { font-size: var(--fs-sm); color: var(--ink-muted); }

.leds { margin-left: auto; display: flex; align-items: center; gap: var(--sp-2); }
.led {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 2px 8px;
  font-size: var(--fs-xs);
  color: var(--ink-muted);
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
  background: var(--bg-sunken);
}
.led-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--ink-faint);
  flex: none;
}
.led.ok .led-dot { background: var(--ok); }
.led.err { color: var(--err); border-color: color-mix(in oklch, var(--err) 35%, var(--line)); }
.led.err .led-dot { background: var(--err); }
/* 琥珀 = 正在发生 */
.led.active {
  color: var(--accent-ink);
  border-color: color-mix(in oklch, var(--accent) 40%, var(--line));
  background: var(--accent-soft);
}
.led.active .led-dot { background: var(--accent); animation: pulse 1.5s var(--ease) infinite; }
.led-num { font-weight: 600; }

.icon-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
  background: var(--surface);
  color: var(--ink-muted);
  cursor: pointer;
  transition: color var(--t-fast), border-color var(--t-fast), background var(--t-fast);
}
.icon-btn:hover { color: var(--ink); border-color: var(--line-strong); background: var(--surface-hover); }

/* ---------- 主体 ---------- */
.body { flex: 1; display: flex; min-height: 0; }

.sidebar {
  width: var(--sidebar-w);
  flex: none;
  background: var(--bg-sunken);
  border-right: 1px solid var(--line);
  overflow-y: auto;
  padding: var(--sp-2) 0 var(--sp-5);
}

.nav { display: flex; flex-direction: column; }
.nav-group { margin-bottom: var(--sp-2); }
.nav-group-title {
  padding: var(--sp-2) var(--sp-4) var(--sp-1);
  font-size: var(--fs-xs);
  font-weight: 600;
  letter-spacing: 0.08em;
  color: var(--ink-faint);
}

.nav-item {
  position: relative;
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: 5px var(--sp-4);
  color: var(--ink-muted);
  font-size: var(--fs-sm);
  transition: color var(--t-fast), background var(--t-fast);
}
.nav-item::before {
  content: '';
  position: absolute;
  left: 0;
  top: 3px;
  bottom: 3px;
  width: 2px;
  background: transparent;
}
.nav-item:hover { color: var(--ink); background: var(--surface); text-decoration: none; }
.nav-item.active { color: var(--ink); background: var(--surface); font-weight: 550; }
.nav-item.active::before { background: var(--primary); }
/* 当前运行模式不可用（后端 capabilities 上报 false）：灰显 + 角标，仍可点进看说明 */
.nav-item.disabled { color: var(--ink-faint); }
.nav-item.disabled:hover { color: var(--ink-faint); background: transparent; }
.nav-off {
  margin-left: auto;
  padding: 1px 5px;
  font-size: 10px;
  color: var(--ink-faint);
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
}
.nav-label { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.main {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
  padding: var(--sp-5) var(--sp-6) var(--sp-5);
  background: var(--bg);
}

/* ---------- 底部版权页脚 ---------- */
.footer {
  flex: none;
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: var(--sp-2) var(--sp-4);
  padding: var(--sp-2) var(--sp-6);
  background: var(--surface);
  border-top: 1px solid var(--line);
  font-size: var(--fs-xs);
  color: var(--ink-faint);
}

.footer-editions {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-3);
  white-space: nowrap;
}
.edition {
  padding: 1px 7px;
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
  background: var(--bg-sunken);
  cursor: default;
  text-decoration: none;
}
.edition-current {
  color: var(--primary);
  border-color: color-mix(in oklch, var(--primary) 40%, var(--line));
  background: var(--primary-soft);
  font-weight: 550;
}
.footer-link {
  font-size: var(--fs-xs);
  color: var(--ink-faint);
  text-decoration: none;
  transition: color var(--t-fast);
}
.footer-link:hover { color: var(--primary); }
</style>
