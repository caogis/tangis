import { createRouter, createWebHistory } from 'vue-router'
import DashboardView from '../views/DashboardView.vue'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'dashboard', component: DashboardView },

    // 数据
    { path: '/import', name: 'import', component: () => import('../views/ImportView.vue') },
    { path: '/vector', name: 'vector', component: () => import('../views/VectorView.vue') },

    // 处理
    { path: '/convert', name: 'convert', component: () => import('../views/ConvertView.vue') },
    { path: '/qc', name: 'qc', component: () => import('../views/QcView.vue') },
    { path: '/compliance', name: 'compliance', component: () => import('../views/ComplianceView.vue') },
    { path: '/edit', name: 'edit', component: () => import('../views/EditView.vue') },

    // 服务
    { path: '/services', name: 'services', component: () => import('../views/ServicesView.vue') },
    { path: '/preview', name: 'preview', component: () => import('../views/PreviewView.vue') },
    // 全屏预览：不加应用外壳（侧栏/顶栏），用于在新窗口/新标签页中打开
    { path: '/preview/full', name: 'preview-full', component: () => import('../views/PreviewView.vue'), meta: { bare: true } },

    // 系统
    { path: '/tasks', name: 'tasks', component: () => import('../views/TaskListView.vue') },
    // 建任务入口已统一到「切片转换」，保留旧路径做重定向（避免死链）
    { path: '/tasks/new', redirect: '/convert' },
    { path: '/tasks/:id', name: 'task-detail', component: () => import('../views/TaskDetailView.vue') },
    { path: '/settings', name: 'settings', component: () => import('../views/SettingsView.vue') },
    { path: '/edition', name: 'edition', component: () => import('../views/EditionView.vue') },

    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

export default router
