<script setup lang="ts">
/**
 * 版本对照：社区版 / 专业版（商业版）/ 企业版（商业版）功能对照
 *
 * 设计要点：延续"工业精密仪表"风格 —— 无阴影、1px 细线、
 * 社区版列高亮（当前部署版本），差异单元格用等宽小字标注，不堆图形。
 */
type Cell = true | false | string

interface Row { feature: string; community: Cell; pro: Cell; enterprise: Cell }
interface Section { title: string; rows: Row[] }

const year = new Date().getFullYear()

const EDITIONS = [
  {
    key: 'community',
    name: '社区版',
    tag: '开源免费',
    current: true,
    desc: '面向个人与学习场景：核心切片管线全开放，Apache-2.0 协议，桌面单机双击即用。',
  },
  {
    key: 'pro',
    name: '专业版',
    tag: '商业版',
    current: false,
    desc: '面向中小团队：解除并发与规模限制，补齐矢量发布与团队协作，含标准技术支持。',
  },
  {
    key: 'enterprise',
    name: '企业版',
    tag: '商业版',
    current: false,
    desc: '面向企业级生产：私有化集群高可用、多租户与审计、定制开发与专属服务。',
  },
] as const

const SECTIONS: Section[] = [
  {
    title: '数据处理',
    rows: [
      { feature: '格式导入（OSGB / OBJ / SHP 等）', community: true, pro: true, enterprise: true },
      { feature: '切片转换（OSGB → 3DTiles）', community: true, pro: true, enterprise: true },
      { feature: '切片并发任务数', community: '1', pro: '4', enterprise: '不限' },
      { feature: '单任务数据量上限', community: '10 GB', pro: '100 GB', enterprise: '不限' },
      { feature: '数据质检（几何）', community: true, pro: true, enterprise: true },
      { feature: '批量质检与自定义规则', community: false, pro: true, enterprise: true },
      { feature: '模型编辑（抠除 / 压平 / 对齐）', community: true, pro: true, enterprise: true },
    ],
  },
  {
    title: '服务与分发',
    rows: [
      { feature: '3DTiles / 地形服务发布', community: true, pro: true, enterprise: true },
      { feature: '矢量发布（MVT / WFS，需 PostGIS）', community: false, pro: true, enterprise: true },
      { feature: '全开放 REST API', community: true, pro: true, enterprise: true },
      { feature: '多协议网关与负载均衡', community: false, pro: false, enterprise: true },
      { feature: 'CDN / 边缘分发对接', community: false, pro: true, enterprise: true },
    ],
  },
  {
    title: '协同与管控',
    rows: [
      { feature: '使用人数', community: '单用户', pro: '≤ 20 人', enterprise: '不限' },
      { feature: '多租户 / RBAC 权限', community: false, pro: false, enterprise: true },
      { feature: '操作审计日志', community: false, pro: false, enterprise: true },
      { feature: '配额与用量统计', community: false, pro: true, enterprise: true },
    ],
  },
  {
    title: '部署与支持',
    rows: [
      { feature: '部署形态', community: '桌面单机', pro: '服务器 / Docker', enterprise: '私有化集群·高可用' },
      { feature: '开源许可', community: 'Apache-2.0', pro: '商业授权', enterprise: '商业授权' },
      { feature: '技术支持', community: '社区 / GitHub Issues', pro: '工单（工作时间）', enterprise: '专属服务 + SLA' },
      { feature: '定制开发与私有特性', community: false, pro: false, enterprise: true },
    ],
  },
]
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">版本说明</h1>
        <p class="page-sub">社区版 / 专业版 / 企业版 功能对照 —— 当前部署为社区版</p>
      </div>
    </header>

    <!-- 三版定位卡片 -->
    <div class="edition-cards block">
      <div
        v-for="e in EDITIONS"
        :key="e.key"
        class="card edition-card"
        :class="{ 'is-current': e.current }"
      >
        <div class="edition-head">
          <span class="edition-name">{{ e.name }}</span>
          <span class="badge" :class="e.current ? 'badge-succeeded' : ''">{{ e.tag }}</span>
        </div>
        <p class="edition-desc">{{ e.desc }}</p>
        <span v-if="e.current" class="edition-current-tag">当前部署</span>
      </div>
    </div>

    <!-- 功能对照表 -->
    <div v-for="s in SECTIONS" :key="s.title" class="panel block">
      <div class="panel-head">
        <span class="panel-title">{{ s.title }}</span>
        <span class="panel-meta">{{ s.rows.length }} 项</span>
      </div>
      <table class="cmp-table">
        <thead>
          <tr>
            <th class="col-feature">功能</th>
            <th class="col-edition col-community">社区版</th>
            <th class="col-edition">专业版</th>
            <th class="col-edition">企业版</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in s.rows" :key="r.feature">
            <td class="col-feature">{{ r.feature }}</td>
            <td class="col-edition col-community">
              <span v-if="typeof r.community === 'boolean'" class="mono cell-bool" :class="r.community ? 'yes' : 'no'">{{ r.community ? '✓' : '—' }}</span>
              <span v-else class="mono cell-text">{{ r.community }}</span>
            </td>
            <td class="col-edition">
              <span v-if="typeof r.pro === 'boolean'" class="mono cell-bool" :class="r.pro ? 'yes' : 'no'">{{ r.pro ? '✓' : '—' }}</span>
              <span v-else class="mono cell-text">{{ r.pro }}</span>
            </td>
            <td class="col-edition">
              <span v-if="typeof r.enterprise === 'boolean'" class="mono cell-bool" :class="r.enterprise ? 'yes' : 'no'">{{ r.enterprise ? '✓' : '—' }}</span>
              <span v-else class="mono cell-text">{{ r.enterprise }}</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <footer class="page-foot block">
      © {{ year }} 武汉草果科技有限公司 · TanGIS —— 开源内核、云原生、可私有化的新一代 GIS 数据处理与分发平台
    </footer>
  </section>
</template>

<style scoped>
/* ---------- 三版定位卡片 ---------- */
.edition-cards {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: var(--sp-3);
}
.edition-card {
  position: relative;
  padding: var(--sp-4);
}
.edition-card.is-current {
  border-color: color-mix(in oklch, var(--primary) 45%, var(--line));
  background: color-mix(in oklch, var(--primary-soft) 40%, var(--surface));
}
.edition-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
  margin-bottom: var(--sp-2);
}
.edition-name { font-size: var(--fs-lg); font-weight: 600; letter-spacing: -0.01em; }
.edition-desc { margin: 0; font-size: var(--fs-sm); color: var(--ink-muted); line-height: var(--lh-base); }
.edition-current-tag {
  display: inline-block;
  margin-top: var(--sp-3);
  padding: 1px 8px;
  font-size: var(--fs-xs);
  font-weight: 550;
  color: var(--primary);
  border: 1px solid color-mix(in oklch, var(--primary) 40%, var(--line));
  border-radius: var(--radius-sm);
  background: var(--primary-soft);
}

/* ---------- 对照表 ---------- */
.cmp-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--fs-sm);
}
.cmp-table th,
.cmp-table td {
  padding: 7px var(--sp-4);
  text-align: left;
  border-bottom: 1px solid var(--line);
}
.cmp-table tbody tr:last-child td { border-bottom: none; }
.cmp-table tbody tr:hover td { background: var(--surface-hover); }

.cmp-table thead th {
  font-size: var(--fs-xs);
  font-weight: 600;
  letter-spacing: 0.06em;
  color: var(--ink-faint);
  background: var(--bg-sunken);
  border-bottom: 1px solid var(--line);
}

.col-feature { width: 46%; color: var(--ink); }
.col-edition { width: 18%; text-align: center; }

/* 社区版列整列弱高亮，与页脚徽章呼应 */
.col-community { background: color-mix(in oklch, var(--primary-soft) 30%, transparent); }

.cell-bool { font-size: var(--fs-base); }
.cell-bool.yes { color: var(--ok); }
.cell-bool.no { color: var(--ink-faint); }
.cell-text { font-size: var(--fs-xs); color: var(--ink); }

/* ---------- 页脚说明 ---------- */
.page-foot {
  padding: var(--sp-3) var(--sp-1);
  font-size: var(--fs-xs);
  color: var(--ink-faint);
}

@media (max-width: 900px) {
  .edition-cards { grid-template-columns: 1fr; }
}
</style>
