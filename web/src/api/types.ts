/** 任务状态（F-04 状态机：取消与暂停为用户可控的额外状态） */
export type TaskStatus = 'PENDING' | 'RUNNING' | 'SUCCEEDED' | 'FAILED' | 'CANCELLED' | 'PAUSED'

export interface TaskProgress {
  done: number
  total: number
}

/** 质检状态（M2-F08b）：空/缺省=未检 */
export type QcStatus = 'pass' | 'fail' | 'skipped'

/** 质检摘要（来自内核 qc 报告 summary 的子集，随任务记录返回） */
export interface QcSummary {
  tile_count: number
  total_degenerate: number
  total_flipped_edges: number
  total_floating: number
  total_crack_segments: number
  total_self_intersections: number
}

export interface Task {
  id: string
  type: string
  status: TaskStatus
  progress: TaskProgress
  /** 失败信息（若有） */
  error?: string
  source?: string
  output?: string
  createdAt?: string
  updatedAt?: string
  /** 质检状态：undefined=未检 */
  qcStatus?: QcStatus
  qcSummary?: QcSummary
  /** 发布审批状态（F-21）：编辑入口等场景使用 */
  approved?: boolean
  /** 编辑任务源任务 ID（M2-F09c）：非空表示这是编辑任务 */
  parentTaskId?: string
  /** 内核 ops.json 留痕摘要（M2-F09c，编辑任务成功后返回） */
  opsSummary?: Record<string, unknown>
  /** 任务参数（F-04）：编辑任务展示 op/参数用 */
  params?: Record<string, unknown>
}

/** 编辑操作（M2-F09c） */
export type EditOp = 'clip' | 'flatten' | 'ground-align'

/** 发起编辑请求体（POST /api/v1/tasks/:id/edit） */
export interface EditTaskPayload {
  op: EditOp
  /** 瓦片名，省略默认 all */
  tile?: string
  /** 四至 "minx,miny,maxx,maxy" */
  bbox?: string
  /** 切割面 "ax+by+cz=d" */
  plane?: string
  elevation?: number
  feather?: number
}

export interface ServiceItem {
  taskId: string
  /** 任务类型（如 osgb->3dtiles / image->tiles），后端可能缺省 */
  type?: string
  /** 任务状态（发布服务均为 SUCCEEDED，后端可能缺省） */
  status?: string
  /** 3DTiles 入口（已解析为可请求的绝对 URL，含防盗链签名 query） */
  tilesetUrl: string
  /** 2D 影像 WMTS GetCapabilities 入口（已解析为绝对 URL，仅影像任务有） */
  wmtsUrl?: string
  /** TMS URL 模板（如 /api/v1/tms/{task}/{z}/{x}/{y}.png，仅影像任务有） */
  tmsUrl?: string
  /** WMS 1.3.0 GetCapabilities 入口（仅影像任务有，已解析为绝对 URL 含签名） */
  wmsUrl?: string
  /** WCS 1.0.0 GetCapabilities 入口（仅影像任务有，已解析为绝对 URL 含签名） */
  wcsUrl?: string
  /** 地形服务入口 layer.json（仅地形任务有，Cesium TerrainProvider 直接消费） */
  terrainUrl?: string
}

export interface NewTaskPayload {
  type: string
  source: string
  /** 产物输出目录；省略时后端自动分配 <dataDir>/outputs/{task_id}（桌面版免填） */
  output?: string
  /**
   * 任务参数。多数线路用字符串（表单直传），但结构化参数（如 DEM 脱密的
   * `region` 矩形/多边形对象）需原样传给内核，故允许任意 JSON 值。
   */
  params?: Record<string, unknown>
}

/** 系统运行信息与能力开关（GET /api/v1/system，设置页数据源） */
export interface SystemInfo {
  /** server | desktop */
  mode: string
  version: string
  dataDir: string
  kernelBin: string
  /** local | nats | none */
  queue: string
  /** localfs | minio | none */
  storage: string
  /** memory | redis | none */
  cache: string
  workers: number
  authEnabled: boolean
  licensePlan: string
  /** 能力开关：false 表示当前模式下不可用（未实现或未装配） */
  capabilities: Record<string, boolean>
}

/** 上传结果（POST /api/v1/uploads）：root 可直接作为任务 source */
export interface UploadResult {
  uploadId: string
  root: string
  files: number
  bytes: number
}

/** 任务控制动作（F-04） */
export type TaskAction = 'cancel' | 'pause' | 'resume' | 'retry'

/** 任务控制响应 */
export interface TaskControlResult {
  task: Task
  /** 是否命中并中断了运行中的内核进程（仅取消/暂停且原为 RUNNING 时返回） */
  interrupted?: boolean
  /** 队列不可用等降级提示（任务会停留在 PENDING） */
  warning?: string
}

/** 判断任务是否已进入终态（不再轮询）；CANCELLED 同为终态 */
export function isTerminal(status: TaskStatus | string | undefined): boolean {
  return status === 'SUCCEEDED' || status === 'FAILED' || status === 'CANCELLED'
}

/** 可取消：非终态任务都可以取消（含暂停中的） */
export function canCancel(status: TaskStatus | string | undefined): boolean {
  return status === 'PENDING' || status === 'RUNNING' || status === 'PAUSED'
}

/** 可暂停：排队中或执行中 */
export function canPause(status: TaskStatus | string | undefined): boolean {
  return status === 'PENDING' || status === 'RUNNING'
}

/** 可恢复：仅暂停中的任务 */
export function canResume(status: TaskStatus | string | undefined): boolean {
  return status === 'PAUSED'
}

/** 可重试：失败或已取消 */
export function canRetry(status: TaskStatus | string | undefined): boolean {
  return status === 'FAILED' || status === 'CANCELLED'
}

/* ------------------------------------------------------------------ */
/* 矢量发布（F-10）：数据源 / 图层 / 元数据                             */
/* ------------------------------------------------------------------ */

/**
 * 矢量数据源。
 * 桌面单机版为**本地文件源**（dsn 形如 file:///abs/path.geojson，
 * format 承载「格式 + 要素数」摘要）；服务端模式为 PostGIS 连接串。
 */
export interface VectorSource {
  id: string
  name: string
  /** 打码后的连接串（PostGIS 密码以 **** 代替；文件源为 file:// 路径） */
  dsn: string
  format: string
  createdAt: string
}

/** 矢量图层（一个 PostGIS 空间表，或一个本地矢量文件） */
export interface VectorLayer {
  name: string
  sourceId: string
  schema: string
  table: string
  geometryColumn: string
  /** POINT | LINESTRING | POLYGON（混合类型数据取更复杂的一类） */
  geometryType: string
  srid: number
  /** MVT feature id 来源列；文件矢量无主键时为空串 */
  idColumn: string
  fields: string[]
  createdAt: string
}

/** 图层元数据（含范围与缩放建议） */
export interface VectorLayerMetadata extends VectorLayer {
  /** WGS84 包围盒 [minx, miny, maxx, maxy]；无数据时为 null */
  bboxWgs84: number[] | null
  /** 要素总数（文件矢量为精确值，PostGIS 为统计估算） */
  featuresEstimated: number
  minzoom: number
  maxzoom: number
}

/** 判断图层是否来自本地文件源 */
export function isFileLayer(source: VectorSource | undefined): boolean {
  return !!source && source.dsn.startsWith('file://')
}

/* ------------------------------------------------------------------ */
/* 最小 GeoJSON 类型                                                    */
/* ------------------------------------------------------------------ */
/* 项目未引入 @types/geojson，而 Cesium 自带的 GeoJSON 命名空间并不全局可见； */
/* 这里只声明前端实际用到的字段（几何坐标按 type 解释，UI 仅透传与绘制）。    */

export interface GeoJsonGeometry {
  type: string
  /** Point/LineString/Polygon 等的坐标嵌套，按 type 解释 */
  coordinates?: unknown
  geometries?: GeoJsonGeometry[]
}

export interface GeoJsonFeature {
  type: string
  id?: string | number
  geometry: GeoJsonGeometry | null
  properties: Record<string, unknown> | null
}

export interface GeoJsonFeatureCollection {
  type: string
  features: GeoJsonFeature[]
}
