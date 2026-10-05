<script setup lang="ts">
/** 服务分发：已发布服务清单 + 协议矩阵（对外交付门面） */
import { computed, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { listServices } from '../api/client'
import type { ServiceItem } from '../api/types'
import AppIcon from '../components/AppIcon.vue'
import { useToast } from '../composables/useToast'
import { taskPreviewUrl } from '../utils/preview'

/** 该服务在新页面的三维预览地址（预览页会自动加载对应任务） */
function previewUrl(taskId: string): string {
  return taskPreviewUrl(taskId)
}

const toast = useToast()
const services = ref<ServiceItem[]>([])
const loading = ref(true)
const errorMsg = ref('')
const kw = ref('')

const stats = computed(() => {
  let vec3d = 0
  let image = 0
  let terrain = 0
  for (const s of services.value) {
    if (s.wmtsUrl) image++
    if (s.tilesetUrl) vec3d++
    if (s.terrainUrl) terrain++
  }
  return { total: services.value.length, vec3d, image, terrain }
})

const visible = computed(() => {
  const k = kw.value.trim().toLowerCase()
  if (!k) return services.value
  return services.value.filter(
    (s) => s.taskId.toLowerCase().includes(k) || (s.type ?? '').toLowerCase().includes(k),
  )
})

const PROTOCOLS = [
  { key: '3D Tiles', status: 'ready', note: 'Cesium / 三维客户端直接加载，含签名防盗链' },
  { key: 'Cesium Terrain', status: 'ready', note: 'Quantized-Mesh + layer.json，TerrainProvider 直接消费' },
  { key: 'WMTS 1.0.0', status: 'ready', note: 'KVP 与 RESTful 两种风格，真实 GetCapabilities' },
  { key: 'TMS', status: 'ready', note: '影像瓦片，Y 轴翻转' },
  { key: 'WFS 2.0', status: 'partial', note: '只读（GetCapabilities / GetFeature）' },
  { key: 'MVT', status: 'ready', note: 'PostGIS 与本地文件矢量双后端，含纯 Go 编码器' },
  { key: 'WMS 1.3.0', status: 'ready', note: 'GetCapabilities / GetMap（3857 / 4326 / CRS:84，含轴序处理）' },
  { key: 'WCS 1.0.0', status: 'ready', note: 'GetCapabilities / DescribeCoverage / GetCoverage（image/png）' },
]

function typeLabel(type?: string): string {
  const map: Record<string, string> = {
    'osgb->3dtiles': '倾斜摄影 3DTiles',
    'image->tiles': '影像瓦片服务',
    'terrain->tiles': '地形 Terrain 服务',
    'las->3dtiles': '点云 3DTiles',
    edit: '编辑产物',
    't3d-import': '.t3d 导入',
  }
  return (type && map[type]) || type || '未标注类型'
}

async function copy(text: string, label: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`${label} 地址已复制`)
  } catch {
    toast.error('复制失败，请手动选择文本')
  }
}

onMounted(async () => {
  try {
    services.value = await listServices()
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '无法连接服务'
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">服务分发</h1>
        <p class="page-sub">已审批发布的 3DTiles / 影像服务与协议入口</p>
      </div>
      <div class="toolbar">
        <RouterLink class="btn btn-sm" to="/preview">
          <AppIcon name="globe" :size="13" />
          三维预览
        </RouterLink>
      </div>
    </header>

    <div class="readout-bar tight">
      <div class="readout">
        <span class="readout-label">已发布服务</span>
        <span class="readout-value mono">{{ stats.total }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">三维服务</span>
        <span class="readout-value mono">{{ stats.vec3d }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">地形服务</span>
        <span class="readout-value mono">{{ stats.terrain }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">影像服务</span>
        <span class="readout-value mono">{{ stats.image }}</span>
      </div>
    </div>

    <div v-if="errorMsg" class="error-box panel block">
      无法连接服务<span class="hint">{{ errorMsg }}</span>
    </div>

    <div class="panel block">
      <div class="panel-head">
        <span class="panel-title">服务清单</span>
        <input v-model="kw" class="input input-sm mono" placeholder="按任务 ID / 类型过滤" />
      </div>

      <div v-if="loading" class="pad">
        <div v-for="i in 3" :key="i" class="skeleton skel" />
      </div>
      <div v-else-if="visible.length === 0" class="empty">
        {{ services.length === 0 ? '还没有已发布的服务' : '没有匹配的服务' }}
        <span class="hint">切片任务完成后，在任务详情执行「审批发布」即可出现在这里</span>
      </div>
      <template v-else>
        <div class="thead data-grid svc-grid">
          <span>服务</span><span>协议入口</span><span class="ta-r">操作</span>
        </div>
        <div v-for="s in visible" :key="s.taskId" class="data-row data-grid svc-grid">
          <span class="cell-svc">
            <span class="svc-name">{{ typeLabel(s.type) }}</span>
            <span class="num">{{ s.taskId.slice(0, 10) }}</span>
          </span>
          <span class="cell-urls">
            <span v-if="s.tilesetUrl" class="url-line">
              <span class="url-key">3DTiles</span>
              <span class="url-val mono">{{ s.tilesetUrl }}</span>
            </span>
            <span v-if="s.wmtsUrl" class="url-line">
              <span class="url-key">WMTS</span>
              <span class="url-val mono">{{ s.wmtsUrl }}</span>
            </span>
            <span v-if="s.tmsUrl" class="url-line">
              <span class="url-key">TMS</span>
              <span class="url-val mono">{{ s.tmsUrl }}</span>
            </span>
            <span v-if="s.wmsUrl" class="url-line">
              <span class="url-key">WMS</span>
              <span class="url-val mono">{{ s.wmsUrl }}</span>
            </span>
            <span v-if="s.wcsUrl" class="url-line">
              <span class="url-key">WCS</span>
              <span class="url-val mono">{{ s.wcsUrl }}</span>
            </span>
            <span v-if="s.terrainUrl" class="url-line">
              <span class="url-key">Terrain</span>
              <span class="url-val mono">{{ s.terrainUrl }}</span>
            </span>
          </span>
          <span class="cell-ops">
            <button
              v-if="s.tilesetUrl"
              type="button"
              class="btn btn-sm"
              @click="copy(s.tilesetUrl, '3DTiles')"
            >
              <AppIcon name="copy" :size="12" />
              3DTiles
            </button>
            <button v-if="s.wmtsUrl" type="button" class="btn btn-sm" @click="copy(s.wmtsUrl, 'WMTS')">
              <AppIcon name="copy" :size="12" />
              WMTS
            </button>
            <button
              v-if="s.terrainUrl"
              type="button"
              class="btn btn-sm"
              @click="copy(s.terrainUrl, 'Terrain')"
            >
              <AppIcon name="copy" :size="12" />
              Terrain
            </button>
            <button v-if="s.wmsUrl" type="button" class="btn btn-sm" @click="copy(s.wmsUrl, 'WMS')">
              <AppIcon name="copy" :size="12" />
              WMS
            </button>
            <button v-if="s.wcsUrl" type="button" class="btn btn-sm" @click="copy(s.wcsUrl, 'WCS')">
              <AppIcon name="copy" :size="12" />
              WCS
            </button>
            <a
              v-if="s.tilesetUrl"
              class="btn btn-sm"
              :href="previewUrl(s.taskId)"
              target="_blank"
              rel="noopener"
              title="在新页面打开该服务的三维预览"
            >
              <AppIcon name="globe" :size="12" />
              预览
            </a>
            <RouterLink class="btn btn-sm" :to="`/tasks/${encodeURIComponent(s.taskId)}`">详情</RouterLink>
          </span>
        </div>
      </template>
    </div>

    <div class="panel">
      <div class="panel-head">
        <span class="panel-title">协议支持</span>
        <span class="panel-meta">分发地址均带 HMAC 防盗链签名</span>
      </div>
      <div class="thead data-grid proto-grid">
        <span>协议</span><span>说明</span><span>状态</span>
      </div>
      <div v-for="p in PROTOCOLS" :key="p.key" class="data-row data-grid proto-grid">
        <span class="proto-key">{{ p.key }}</span>
        <span class="proto-note">{{ p.note }}</span>
        <span>
          <span class="chip" :class="p.status === 'ready' ? 'chip-ready' : p.status === 'partial' ? 'chip-partial' : 'chip-planned'">
            {{ p.status === 'ready' ? '可用' : p.status === 'partial' ? '部分可用' : '规划中' }}
          </span>
        </span>
      </div>
    </div>
  </section>
</template>

<style scoped>
.input-sm { width: 220px; padding: 4px 9px; font-size: var(--fs-xs); }
.pad { padding: var(--sp-4); display: flex; flex-direction: column; gap: var(--sp-3); }
.skel { height: 16px; }

.svc-grid { grid-template-columns: 200px 1fr 220px; }
.cell-svc { display: flex; flex-direction: column; min-width: 0; }
.svc-name { color: var(--ink); }
.cell-urls { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
.url-line { display: flex; gap: var(--sp-2); align-items: baseline; min-width: 0; }
.url-key {
  flex: none;
  width: 48px;
  font-size: var(--fs-xs);
  color: var(--ink-faint);
}
.url-val {
  font-size: var(--fs-xs);
  color: var(--ink-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.cell-ops { display: flex; gap: var(--sp-2); justify-content: flex-end; flex-wrap: wrap; }

.proto-grid { grid-template-columns: 130px 1fr 90px; }
.proto-key { color: var(--ink); }
.proto-note { font-size: var(--fs-xs); color: var(--ink-faint); }
</style>
