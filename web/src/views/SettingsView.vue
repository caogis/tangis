<script setup lang="ts">
/** 系统设置：运行信息（真实数据来自 GET /api/v1/system）、能力矩阵、环境变量 */
import { computed, onMounted, ref } from 'vue'
import { API_BASE, getSystemInfo } from '../api/client'
import type { SystemInfo } from '../api/types'
import { countByStatus, groupByModule, STATUS_LABEL } from '../capabilities'

const modules = groupByModule()
const capCount = countByStatus()

/* ---------------- 运行时信息（后端上报，非写死） ---------------- */

const sys = ref<SystemInfo | null>(null)
const sysError = ref('')
const sysLoading = ref(true)

onMounted(async () => {
  try {
    sys.value = await getSystemInfo()
  } catch (e) {
    sysError.value = e instanceof Error ? e.message : '无法读取运行信息'
  } finally {
    sysLoading.value = false
  }
})

const MODE_LABEL: Record<string, string> = {
  desktop: '桌面单机版（零外部依赖）',
  server: '服务端 / 集群模式',
}
const IMPL_LABEL: Record<string, string> = {
  local: '进程内队列（local）',
  nats: 'NATS JetStream',
  none: '未启用',
  localfs: '本地文件系统',
  minio: 'MinIO',
  memory: '内存缓存',
  redis: 'Redis',
}

const runtime = computed(() => {
  const s = sys.value
  return [
    { key: '控制台版本', value: s?.version || '—' },
    { key: 'API 基址', value: API_BASE || '同源（相对地址）' },
    { key: '运行模式', value: s ? (MODE_LABEL[s.mode] ?? s.mode) : '—' },
    { key: '数据目录', value: s?.dataDir || '—' },
    { key: '切片内核', value: s?.kernelBin || '同目录 tangis-kernel' },
    { key: '任务队列', value: s ? (IMPL_LABEL[s.queue] ?? s.queue) : '—' },
    { key: '产物存储', value: s ? (IMPL_LABEL[s.storage] ?? s.storage) : '—' },
    { key: '缓存', value: s ? (IMPL_LABEL[s.cache] ?? s.cache) : '—' },
    { key: '切片并发', value: s ? String(s.workers) : '—' },
    { key: '鉴权', value: s ? (s.authEnabled ? '开启（X-API-Key）' : '已关闭') : '—' },
    { key: '许可', value: s?.licensePlan || '—' },
  ]
})

/* ---------------- 当前运行模式的能力开关（来自后端） ---------------- */

const CAP_LABELS: Record<string, string> = {
  '3dtiles': '3D Tiles 分发',
  wmts: 'WMTS 影像瓦片',
  tms: 'TMS 影像瓦片',
  wms: 'WMS 地图服务',
  wcs: 'WCS 栅格服务',
  terrain: '地形 Terrain（quantized-mesh）',
  pointcloud: 'LAS 点云 → 3D Tiles',
  wfs: 'WFS 矢量服务',
  mvt: 'MVT 矢量瓦片',
  vector: '矢量发布（PostGIS 数据源）',
  qc: '几何质检',
  edit: '模型编辑',
  package: '.t3d 打包 / 解包导入',
  import_upload: '文件 / 目录上传',
  task_control: '任务取消 / 暂停 / 重试',
  compliance: '合规算子（脱密 / 七参数 / 坐标系识别）',
  webhook: 'Webhook 通知',
}

const capEntries = computed(() => {
  const caps = sys.value?.capabilities ?? {}
  return Object.entries(CAP_LABELS).map(([key, label]) => ({
    key,
    label,
    on: caps[key] === true,
    known: caps[key] !== undefined,
  }))
})
const capReady = computed(() => capEntries.value.filter((c) => c.on).length)

const ENV = [
  { key: 'PORT', value: '监听端口，默认 8080；被占用时默认报错而非静默顺延' },
  { key: 'TANGIS_HOME', value: '数据目录，默认 ~/TanGIS（目录名唯一，不沿用历史目录）' },
  { key: 'TANGIS_API_KEY', value: '内置管理员 Key，默认 tangis-dev-key' },
  { key: 'TANGIS_AUTH', value: '设为 off 关闭鉴权（仅开发用）' },
  { key: 'TANGIS_DESKTOP_WORKERS', value: '切片并发数，默认 1' },
  { key: 'TANGIS_PORT_AUTO', value: '设为 1 时端口被占用自动顺延' },
  { key: 'KERNEL_BIN', value: '手动指定内核路径（默认同目录 tangis-kernel）' },
]
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">系统设置</h1>
        <p class="page-sub">运行信息、能力覆盖与环境变量</p>
      </div>
    </header>

    <div class="readout-bar tight">
      <div class="readout">
        <span class="readout-label">当前模式可用</span>
        <span class="readout-value mono is-ok">{{ capReady }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">能力矩阵可用</span>
        <span class="readout-value mono is-ok">{{ capCount.ready }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">部分可用</span>
        <span class="readout-value mono" :class="{ 'is-active': capCount.partial > 0 }">{{ capCount.partial }}</span>
      </div>
      <div class="readout">
        <span class="readout-label">规划中</span>
        <span class="readout-value mono">{{ capCount.planned }}</span>
      </div>
    </div>

    <div class="cols block">
      <div class="panel">
        <div class="panel-head">
          <span class="panel-title">运行信息</span>
          <span class="panel-meta">
            <template v-if="sysLoading">读取中…</template>
            <template v-else-if="sysError">无法读取（{{ sysError }}）</template>
            <template v-else>来自 GET /api/v1/system</template>
          </span>
        </div>
        <dl class="kv">
          <template v-for="r in runtime" :key="r.key">
            <dt>{{ r.key }}</dt>
            <dd class="mono">{{ r.value }}</dd>
          </template>
        </dl>
      </div>

      <div class="panel">
        <div class="panel-head">
          <span class="panel-title">环境变量</span>
          <span class="panel-meta">桌面单机版常用项</span>
        </div>
        <dl class="kv">
          <template v-for="e in ENV" :key="e.key">
            <dt class="mono">{{ e.key }}</dt>
            <dd>{{ e.value }}</dd>
          </template>
        </dl>
      </div>
    </div>

    <div class="panel block">
      <div class="panel-head">
        <span class="panel-title">当前运行模式的能力开关</span>
        <span class="panel-meta">
          <template v-if="sysLoading">读取中…</template>
          <template v-else-if="sysError">无法读取</template>
          <template v-else>后端上报 · {{ capReady }} / {{ capEntries.length }} 可用</template>
        </span>
      </div>
      <div class="thead data-grid cap-grid"><span>能力</span><span>标识</span><span>状态</span></div>
      <div v-for="c in capEntries" :key="c.key" class="data-row data-grid cap-grid">
        <span class="cap-name">{{ c.label }}</span>
        <span class="cap-note mono">{{ c.key }}</span>
        <span>
          <span class="chip" :class="c.on ? 'chip-ready' : 'chip-planned'">
            {{ c.on ? '可用' : c.known ? '未装配 / 未实现' : '未上报' }}
          </span>
        </span>
      </div>
    </div>

    <div class="panel block">
      <div class="panel-head">
        <span class="panel-title">能力矩阵</span>
        <span class="panel-meta">对照 GISBox 功能清单</span>
      </div>
      <div v-for="m in modules" :key="m.module" class="cap-module">
        <div class="cap-module-head">
          <span>{{ m.module }}</span>
          <span class="num">{{ m.items.filter((i) => i.status === 'ready').length }} / {{ m.items.length }} 可用</span>
        </div>
        <div class="thead data-grid cap-grid"><span>能力</span><span>说明</span><span>状态</span></div>
        <div v-for="c in m.items" :key="c.name" class="data-row data-grid cap-grid">
          <span class="cap-name">{{ c.name }}</span>
          <span class="cap-note">{{ c.note }}</span>
          <span>
            <span class="chip" :class="c.status === 'ready' ? 'chip-ready' : c.status === 'partial' ? 'chip-partial' : 'chip-planned'">
              {{ STATUS_LABEL[c.status] }}
            </span>
          </span>
        </div>
      </div>
    </div>

    <div class="panel">
      <div class="panel-head"><span class="panel-title">关于</span></div>
      <p class="about">
        TanGIS 是开源内核、可私有化的 GIS 数据处理与分发平台，对照 GISBox 的场景编辑 / 切片转换 / 服务分发三大模块，
        采用 Apache-2.0 许可。当前为桌面单机版形态：零外部依赖，运行后通过浏览器访问固定端口使用；
        数据、切片产物与日志全部落在本机数据目录，不出内网。
      </p>
    </div>
  </section>
</template>

<style scoped>
.cols { display: grid; grid-template-columns: 1fr 1.2fr; gap: var(--sp-4); align-items: start; }
@media (max-width: 1040px) { .cols { grid-template-columns: 1fr; } }

.kv dd { font-size: var(--fs-sm); }
.kv dt { font-size: var(--fs-sm); }

.cap-module { border-top: 1px solid var(--line); }
.cap-module:first-of-type { border-top: none; }
.cap-module-head {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  padding: var(--sp-3) var(--sp-4) var(--sp-1);
  font-size: var(--fs-xs);
  font-weight: 600;
  letter-spacing: 0.06em;
  color: var(--ink-muted);
}
.cap-grid { grid-template-columns: 190px 1fr 90px; }
.cap-name { color: var(--ink); }
.cap-note { font-size: var(--fs-xs); color: var(--ink-faint); }

.about { margin: 0; padding: var(--sp-4); font-size: var(--fs-sm); line-height: 1.8; color: var(--ink-muted); }
</style>
