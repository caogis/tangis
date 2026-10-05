import type {
  EditTaskPayload,
  GeoJsonFeatureCollection,
  NewTaskPayload,
  QcStatus,
  QcSummary,
  ServiceItem,
  SystemInfo,
  Task,
  TaskAction,
  TaskControlResult,
  TaskProgress,
  TaskStatus,
  UploadResult,
  VectorLayer,
  VectorLayerMetadata,
  VectorSource,
} from './types'
import { isTerminal } from './types'
import { resolveAssetUrl } from './url'

// 桌面单机版（TANGIS_MODE=desktop）端口由后端动态分配，前端必须走**同源相对地址**；
// 因此未显式配置 VITE_API_BASE 时默认空串（相对当前 origin），而不是写死端口。
// 独立 dev 场景（vite 5173 直连 apiserver 8080）在 .env.local 里显式指定即可。
export const API_BASE: string = import.meta.env.VITE_API_BASE || ''
const API_KEY: string = import.meta.env.VITE_API_KEY || ''

/** 后端不可达 / 非法响应时抛出，供视图层做优雅降级 */
export class ApiError extends Error {
  readonly status?: number
  constructor(message: string, status?: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = {
    Accept: 'application/json',
  }
  if (init?.body != null) headers['Content-Type'] = 'application/json'
  if (API_KEY) headers['X-API-Key'] = API_KEY

  let res: Response
  try {
    res = await fetch(`${API_BASE}${path}`, { ...init, headers })
  } catch {
    throw new ApiError('无法连接 API，请确认后端已启动')
  }
  if (!res.ok) {
    let detail = ''
    try {
      const body = await res.json()
      detail = body?.message ?? body?.error ?? ''
    } catch {
      /* 忽略非 JSON 响应体 */
    }
    throw new ApiError(detail || `请求失败（HTTP ${res.status}）`, res.status)
  }
  // 204 No Content（删除任务等）没有响应体，不能按 JSON 解析
  if (res.status === 204) return undefined as T
  try {
    return (await res.json()) as T
  } catch {
    throw new ApiError('响应不是合法 JSON')
  }
}

/* ------------------------------------------------------------------ */
/* 字段宽容处理：兼容 camelCase / snake_case / 上游字段演进            */
/* ------------------------------------------------------------------ */

function pickStr(src: Record<string, unknown>, ...keys: string[]): string {
  for (const k of keys) {
    const v = src[k]
    if (typeof v === 'string' && v.length > 0) return v
  }
  return ''
}

function pickNum(src: Record<string, unknown>, ...keys: string[]): number {
  for (const k of keys) {
    const v = src[k]
    if (typeof v === 'number' && Number.isFinite(v)) return v
  }
  return 0
}

function normalizeStatus(raw: unknown): TaskStatus {
  const s = String(raw ?? '').toUpperCase()
  if (s === 'PENDING' || s === 'QUEUED' || s === 'WAITING') return 'PENDING'
  if (s === 'RUNNING' || s === 'PROCESSING') return 'RUNNING'
  if (s === 'SUCCEEDED' || s === 'SUCCESS' || s === 'DONE' || s === 'COMPLETED') return 'SUCCEEDED'
  if (s === 'FAILED' || s === 'ERROR') return 'FAILED'
  if (s === 'CANCELLED' || s === 'CANCELED') return 'CANCELLED'
  if (s === 'PAUSED') return 'PAUSED'
  return 'PENDING'
}

function normalizeProgress(src: Record<string, unknown>): TaskProgress {
  const p = (src.progress ?? src.task_progress ?? null) as Record<string, unknown> | number | null
  if (typeof p === 'number' && Number.isFinite(p)) {
    // 0~1 视为比例，>1 视为百分比
    const ratio = p <= 1 ? p : p / 100
    return { done: Math.round(ratio * 100), total: 100 }
  }
  if (p && typeof p === 'object') {
    let done = pickNum(p, 'done', 'completed', 'finished', 'current')
    let total = pickNum(p, 'total', 'count', 'expected')
    // 上游直接给百分比的情况：total 缺失但存在 percent
    const percent = pickNum(p, 'percent', 'percentage', 'progress')
    if (total <= 0 && percent > 0) return { done: Math.min(percent, 100), total: 100 }
    if (total <= 0) return { done: 0, total: 0 }
    if (done > total && total > 0) done = total
    return { done, total }
  }
  return { done: 0, total: 0 }
}

function normalizeTask(raw: Record<string, unknown>): Task {
  return {
    id: pickStr(raw, 'id', 'task_id', 'taskId', 'uuid'),
    type: pickStr(raw, 'type', 'task_type', 'taskType', 'kind') || 'unknown',
    status: normalizeStatus(raw.status),
    progress: normalizeProgress(raw),
    error: pickStr(raw, 'error', 'error_message', 'errorMessage', 'message', 'reason') || undefined,
    source: pickStr(raw, 'source', 'source_path', 'src') || undefined,
    output: pickStr(raw, 'output', 'output_path', 'output_dir', 'dest') || undefined,
    createdAt: pickStr(raw, 'created_at', 'createdAt', 'created') || undefined,
    updatedAt: pickStr(raw, 'updated_at', 'updatedAt', 'updated') || undefined,
    qcStatus: normalizeQcStatus(raw),
    qcSummary: normalizeQcSummary(raw.qc_summary),
    approved: typeof raw.approved === 'boolean' ? raw.approved : undefined,
    parentTaskId: pickStr(raw, 'parent_task_id', 'parentTaskId') || undefined,
    opsSummary: normalizeOpsSummary(raw.ops_summary),
    params: normalizeParams(raw.params),
  }
}

const QC_STATUSES: readonly QcStatus[] = ['pass', 'fail', 'skipped']

function normalizeQcStatus(raw: Record<string, unknown>): QcStatus | undefined {
  const s = String(raw.qc_status ?? raw.qcStatus ?? '').toLowerCase()
  return (QC_STATUSES as readonly string[]).includes(s) ? (s as QcStatus) : undefined
}

function normalizeQcSummary(raw: unknown): QcSummary | undefined {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return undefined
  const s = raw as Record<string, unknown>
  return {
    tile_count: pickNum(s, 'tile_count', 'tileCount'),
    total_degenerate: pickNum(s, 'total_degenerate', 'totalDegenerate'),
    total_flipped_edges: pickNum(s, 'total_flipped_edges', 'totalFlippedEdges'),
    total_floating: pickNum(s, 'total_floating', 'totalFloating'),
    total_crack_segments: pickNum(s, 'total_crack_segments', 'totalCrackSegments'),
    total_self_intersections: pickNum(s, 'total_self_intersections', 'totalSelfIntersections'),
  }
}

/** ops.json 留痕摘要（M2-F09c）：整体透传，内容结构由内核定义 */
function normalizeOpsSummary(raw: unknown): Record<string, unknown> | undefined {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return undefined
  return raw as Record<string, unknown>
}

/** 任务参数（F-04）：编辑任务展示 op/参数用 */
function normalizeParams(raw: unknown): Record<string, unknown> | undefined {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return undefined
  return raw as Record<string, unknown>
}

/* ------------------------------------------------------------------ */
/* API 端点                                                            */
/* ------------------------------------------------------------------ */

export async function listTasks(): Promise<Task[]> {
  const raw = await request<unknown>('/api/v1/tasks')
  const arr = Array.isArray(raw) ? raw : (raw as { tasks?: unknown[]; items?: unknown[]; data?: unknown[] })?.tasks
    ?? (raw as { items?: unknown[] })?.items ?? (raw as { data?: unknown[] })?.data ?? []
  if (!Array.isArray(arr)) return []
  return arr
    .filter((t): t is Record<string, unknown> => t != null && typeof t === 'object')
    .map(normalizeTask)
}

export async function getTask(id: string): Promise<Task> {
  const raw = await request<Record<string, unknown>>(`/api/v1/tasks/${encodeURIComponent(id)}`)
  // 兼容 {task: {...}} 包装
  const inner = (raw.task && typeof raw.task === 'object' ? raw.task : raw) as Record<string, unknown>
  return normalizeTask(inner)
}

export async function createTask(payload: NewTaskPayload): Promise<Task> {
  const body: Record<string, unknown> = {
    type: payload.type,
    source: payload.source,
    output: payload.output,
  }
  if (payload.params && Object.keys(payload.params).length > 0) body.params = payload.params
  const raw = await request<Record<string, unknown>>('/api/v1/tasks', {
    method: 'POST',
    body: JSON.stringify(body),
  })
  const inner = (raw.task && typeof raw.task === 'object' ? raw.task : raw) as Record<string, unknown>
  const task = normalizeTask(inner)
  // 兼容仅返回 {id: "..."} 的最小实现
  if (!task.id) task.id = pickStr(raw, 'id', 'task_id', 'taskId')
  return task
}

export async function listServices(): Promise<ServiceItem[]> {
  const raw = await request<unknown>('/api/v1/services')
  const arr = Array.isArray(raw) ? raw : (raw as { services?: unknown[]; items?: unknown[] })?.services
    ?? (raw as { items?: unknown[] })?.items ?? []
  if (!Array.isArray(arr)) return []
  return arr
    .filter((s): s is Record<string, unknown> => s != null && typeof s === 'object')
    .map((s) => ({
      taskId: pickStr(s, 'task_id', 'taskId', 'id'),
      type: pickStr(s, 'type') || undefined,
      status: pickStr(s, 'status') || undefined,
      tilesetUrl: pickStr(s, 'tileset_url', 'tilesetUrl', 'url'),
      wmtsUrl: pickStr(s, 'wmts_url', 'wmtsUrl'),
      tmsUrl: pickStr(s, 'tms_url', 'tmsUrl'),
      wmsUrl: pickStr(s, 'wms_url', 'wmsUrl'),
      wcsUrl: pickStr(s, 'wcs_url', 'wcsUrl'),
      terrainUrl: pickStr(s, 'terrain_url', 'terrainUrl'),
    }))
    .filter(
      (s) =>
        s.tilesetUrl.length > 0 ||
        s.wmtsUrl.length > 0 ||
        s.wmsUrl.length > 0 ||
        s.terrainUrl.length > 0,
    )
    // 后端返回相对路径（如 /services/t1/tileset.json），浏览器会解析到
    // Vite 源拿到 HTML；此处统一拼 API_BASE 指向后端源
    .map((s) => ({
      ...s,
      tilesetUrl: s.tilesetUrl ? resolveAssetUrl(s.tilesetUrl, API_BASE) : '',
      wmtsUrl: s.wmtsUrl ? resolveAssetUrl(s.wmtsUrl, API_BASE) : '',
      tmsUrl: s.tmsUrl ? resolveAssetUrl(s.tmsUrl, API_BASE) : '',
      wmsUrl: s.wmsUrl ? resolveAssetUrl(s.wmsUrl, API_BASE) : '',
      wcsUrl: s.wcsUrl ? resolveAssetUrl(s.wcsUrl, API_BASE) : '',
      terrainUrl: s.terrainUrl ? resolveAssetUrl(s.terrainUrl, API_BASE) : '',
    }))
}

/** 发起编辑任务（M2-F09c）：返回新建的编辑任务 */
export async function createEditTask(sourceTaskId: string, payload: EditTaskPayload): Promise<Task> {
  const raw = await request<Record<string, unknown>>(
    `/api/v1/tasks/${encodeURIComponent(sourceTaskId)}/edit`,
    { method: 'POST', body: JSON.stringify(payload) },
  )
  const inner = (raw.task && typeof raw.task === 'object' ? raw.task : raw) as Record<string, unknown>
  const task = normalizeTask(inner)
  if (!task.id) task.id = pickStr(raw, 'id', 'task_id', 'taskId')
  return task
}

/** 审批发布（F-21）：审批通过后任务才进入服务分发列表 */
export async function approveTask(id: string): Promise<Task> {
  const raw = await request<Record<string, unknown>>(
    `/api/v1/tasks/${encodeURIComponent(id)}/approve`,
    { method: 'POST' },
  )
  const inner = (raw.task && typeof raw.task === 'object' ? raw.task : raw) as Record<string, unknown>
  return normalizeTask(inner)
}

/** 编辑链查询（M2-F09c）：同 parent 链的编辑任务列表（含 ops 摘要） */
export async function listEditHistory(taskId: string): Promise<{ parentTaskId: string; tasks: Task[] }> {
  const raw = await request<Record<string, unknown>>(
    `/api/v1/tasks/${encodeURIComponent(taskId)}/edit-history`,
  )
  const arr = Array.isArray(raw.tasks) ? raw.tasks : []
  return {
    parentTaskId: pickStr(raw, 'parent_task_id', 'parentTaskId'),
    tasks: arr
      .filter((t): t is Record<string, unknown> => t != null && typeof t === 'object')
      .map(normalizeTask),
  }
}

/* ------------------------------------------------------------------ */
/* 带鉴权下载与原始请求                                                 */
/* ------------------------------------------------------------------ */

/**
 * 带鉴权的原始 fetch（返回 Response，不解析 JSON）。
 *
 * 为什么需要它：后端 /api/v1/* 只认 X-API-Key 请求头，**不接受 query 传 key**。
 * 而 `<a href download>` 无法携带自定义请求头，因此日志/报告下载必须走
 * fetch + blob；读 JSON 报告同理。
 */
export async function fetchWithAuth(path: string, init?: RequestInit): Promise<Response> {
  const headers: Record<string, string> = {
    ...((init?.headers as Record<string, string> | undefined) ?? {}),
  }
  if (API_KEY) headers['X-API-Key'] = API_KEY
  let res: Response
  try {
    res = await fetch(`${API_BASE}${path}`, { ...init, headers })
  } catch {
    throw new ApiError('无法连接 API，请确认后端已启动')
  }
  if (!res.ok) {
    let detail = ''
    try {
      const body = await res.json()
      detail = body?.error ?? body?.message ?? ''
    } catch {
      /* 非 JSON 响应体 */
    }
    throw new ApiError(detail || `请求失败（HTTP ${res.status}）`, res.status)
  }
  return res
}

/** 任务内核日志路径（F-04） */
export function taskLogsPath(id: string): string {
  return `/api/v1/tasks/${encodeURIComponent(id)}/logs`
}

/** 质检完整报告路径（M2-F08b） */
export function taskQcReportPath(id: string): string {
  return `/api/v1/tasks/${encodeURIComponent(id)}/qc-report`
}

/** 编辑操作留痕 ops.json 路径（M2-F09c） */
export function taskOpsReportPath(id: string): string {
  return `/api/v1/tasks/${encodeURIComponent(id)}/ops-report`
}

/** 触发浏览器保存一个 Blob */
function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

/** 带鉴权下载文件（触发浏览器保存） */
export async function downloadFile(path: string, filename: string): Promise<void> {
  const res = await fetchWithAuth(path)
  saveBlob(await res.blob(), filename)
}

/** 从 Content-Disposition 解析建议文件名（取不到返回 null） */
function filenameFromDisposition(v: string | null): string | null {
  if (!v) return null
  const m = /filename\*?=(?:UTF-8'')?"?([^";]+)"?/i.exec(v)
  if (!m) return null
  try {
    return decodeURIComponent(m[1])
  } catch {
    return m[1]
  }
}

/** 矢量图层导出结果（含服务端如实回传的有损处理提示） */
export interface VectorExportInfo {
  filename: string
  features: number
  /** Shapefile 因几何类型不兼容被跳过的要素数 */
  skipped: number
  /** 其它有损处理提示（字段名截断等） */
  warnings: string[]
}

/**
 * 导出矢量图层并触发下载（GET /api/v1/vector/layers/{name}/export）。
 *
 * 有损处理通过响应头回传（HTTP 头只能是 ASCII，故中文按 URL 编码），
 * 这里解码后交给界面提示——避免"导出成功了但少了一堆要素"却不知原因。
 */
export async function exportVectorLayer(
  layer: string,
  format: 'geojson' | 'gpkg' | 'shp',
): Promise<VectorExportInfo> {
  const res = await fetchWithAuth(
    `/api/v1/vector/layers/${encodeURIComponent(layer)}/export?format=${format}`,
  )
  const warnings = (res.headers.get('X-Tangis-Warnings') ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => {
      try {
        return decodeURIComponent(s)
      } catch {
        return s
      }
    })
  const info: VectorExportInfo = {
    filename:
      filenameFromDisposition(res.headers.get('Content-Disposition')) ?? `${layer}.${format}`,
    features: Number(res.headers.get('X-Tangis-Features') ?? 0),
    skipped: Number(res.headers.get('X-Tangis-Skipped') ?? 0),
    warnings,
  }
  saveBlob(await res.blob(), info.filename)
  return info
}

/* ------------------------------------------------------------------ */
/* F-04 任务控制：删除 / 取消 / 暂停 / 恢复 / 重试                      */
/* ------------------------------------------------------------------ */

/** 删除任务（产物与日志保留在磁盘，仅移除任务记录） */
export async function deleteTask(id: string): Promise<void> {
  await request<void>(`/api/v1/tasks/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/**
 * 任务控制动作（F-04）。
 * 取消/暂停在任务运行中会先中断内核进程；恢复/重试会重新入队。
 */
export async function controlTask(id: string, action: TaskAction): Promise<TaskControlResult> {
  const raw = await request<Record<string, unknown>>(
    `/api/v1/tasks/${encodeURIComponent(id)}/${action}`,
    { method: 'POST' },
  )
  const inner = (raw.task && typeof raw.task === 'object' ? raw.task : raw) as Record<string, unknown>
  return {
    task: normalizeTask(inner),
    interrupted: typeof raw.interrupted === 'boolean' ? raw.interrupted : undefined,
    warning: pickStr(raw, 'warning') || undefined,
  }
}

export function cancelTask(id: string): Promise<TaskControlResult> {
  return controlTask(id, 'cancel')
}

export function pauseTask(id: string): Promise<TaskControlResult> {
  return controlTask(id, 'pause')
}

export function resumeTask(id: string): Promise<TaskControlResult> {
  return controlTask(id, 'resume')
}

export function retryTask(id: string): Promise<TaskControlResult> {
  return controlTask(id, 'retry')
}

/* ------------------------------------------------------------------ */
/* F-01 导入：文件/目录上传                                             */
/* ------------------------------------------------------------------ */

/**
 * 上传文件或整个目录（POST /api/v1/uploads），返回可直接建任务的 root。
 *
 * 用 XHR 而非 fetch：需要上传进度（大 OSGB 目录可能数百 MB）。
 * relPaths 与 files **顺序对齐**（浏览器 webkitRelativePath），用于保留目录结构；
 * 后端要求 paths 字段排在文件之前，此处按此顺序 append。
 */
export function uploadFiles(
  files: File[],
  relPaths: string[] = [],
  onProgress?: (loaded: number, total: number) => void,
): Promise<UploadResult> {
  return new Promise((resolve, reject) => {
    const form = new FormData()
    if (relPaths.length > 0) form.append('paths', JSON.stringify(relPaths))
    for (const f of files) form.append('files', f)

    const xhr = new XMLHttpRequest()
    xhr.open('POST', `${API_BASE}/api/v1/uploads`)
    if (API_KEY) xhr.setRequestHeader('X-API-Key', API_KEY)
    xhr.upload.onprogress = (e) => {
      if (onProgress && e.lengthComputable) onProgress(e.loaded, e.total)
    }
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          const body = JSON.parse(xhr.responseText) as Record<string, unknown>
          resolve({
            uploadId: pickStr(body, 'upload_id', 'uploadId'),
            root: pickStr(body, 'root'),
            files: pickNum(body, 'files'),
            bytes: pickNum(body, 'bytes'),
          })
        } catch {
          reject(new ApiError('上传响应不是合法 JSON'))
        }
        return
      }
      let detail = ''
      try {
        const body = JSON.parse(xhr.responseText) as { error?: string }
        detail = body?.error ?? ''
      } catch {
        /* 非 JSON 错误体 */
      }
      reject(new ApiError(detail || `上传失败（HTTP ${xhr.status}）`, xhr.status))
    }
    xhr.onerror = () => reject(new ApiError('上传失败：无法连接 API'))
    xhr.onabort = () => reject(new ApiError('上传已取消'))
    xhr.send(form)
  })
}

/* ------------------------------------------------------------------ */
/* F-21 测绘合规算子                                                     */
/* ------------------------------------------------------------------ */

/** 坐标系识别结果 */
export interface CrsIdentifyResult {
  /** 内核识别的坐标系信息（CRS / 投影 / 数据源等） */
  info?: Record<string, unknown>
  /** 像元尺度与 tiepoint 原点（GeoTIFF 才有） */
  georef?: string
  /** 内核原始输出（排障用） */
  output: string
}

/** 识别数据坐标系（内核 crs-identify，秒级同步） */
export async function crsIdentify(path: string): Promise<CrsIdentifyResult> {
  const raw = await request<Record<string, unknown>>('/api/v1/compliance/crs-identify', {
    method: 'POST',
    body: JSON.stringify({ path }),
  })
  return {
    info:
      raw.info && typeof raw.info === 'object' && !Array.isArray(raw.info)
        ? (raw.info as Record<string, unknown>)
        : undefined,
    georef: pickStr(raw, 'georef') || undefined,
    output: pickStr(raw, 'output'),
  }
}

/** 七参数转换结果 */
export interface BursaResult {
  /** 内核摘要（点数与源/目标坐标系） */
  summary: string
  /** 内核输出的逐点结果与统计 */
  result?: Record<string, unknown>
  /** 落盘的参数/点集/结果文件路径 */
  files?: Record<string, string>
}

/** CGCS2000 七参数（Bursa-Wolf）坐标转换 */
export async function bursaTransform(
  params: Record<string, unknown>,
  points: number[][],
): Promise<BursaResult> {
  const raw = await request<Record<string, unknown>>('/api/v1/compliance/bursa', {
    method: 'POST',
    body: JSON.stringify({ params, points }),
  })
  return {
    summary: pickStr(raw, 'summary'),
    result:
      raw.result && typeof raw.result === 'object' && !Array.isArray(raw.result)
        ? (raw.result as Record<string, unknown>)
        : undefined,
    files:
      raw.files && typeof raw.files === 'object' && !Array.isArray(raw.files)
        ? (raw.files as Record<string, string>)
        : undefined,
  }
}

/** 任务产物下载路径（非瓦片类：合规脱密 GeoTIFF、留痕 JSON 等） */
export function taskArtifactPath(id: string, name: string): string {
  const enc = name
    .split('/')
    .filter((s) => s.length > 0)
    .map(encodeURIComponent)
    .join('/')
  return `/api/v1/tasks/${encodeURIComponent(id)}/artifact/${enc}`
}

/* ------------------------------------------------------------------ */
/* 运行信息（设置页）                                                   */
/* ------------------------------------------------------------------ */

/** 读取运行信息与能力开关（GET /api/v1/system） */
export async function getSystemInfo(): Promise<SystemInfo> {
  const raw = await request<Record<string, unknown>>('/api/v1/system')
  const rawCaps =
    raw.capabilities && typeof raw.capabilities === 'object'
      ? (raw.capabilities as Record<string, unknown>)
      : {}
  const capabilities: Record<string, boolean> = {}
  for (const [k, v] of Object.entries(rawCaps)) capabilities[k] = v === true
  return {
    mode: pickStr(raw, 'mode') || 'server',
    version: pickStr(raw, 'version'),
    dataDir: pickStr(raw, 'data_dir', 'dataDir'),
    kernelBin: pickStr(raw, 'kernel_bin', 'kernelBin'),
    queue: pickStr(raw, 'queue'),
    storage: pickStr(raw, 'storage'),
    cache: pickStr(raw, 'cache'),
    workers: pickNum(raw, 'workers'),
    authEnabled: raw.auth_enabled === true || raw.authEnabled === true,
    licensePlan: pickStr(raw, 'license_plan', 'licensePlan'),
    capabilities,
  }
}

/* ------------------------------------------------------------------ */
/* 矢量发布（F-10）：数据源 / 图层注册与分发地址                        */
/* ------------------------------------------------------------------ */

function pickStrArr(src: Record<string, unknown>, ...keys: string[]): string[] {
  for (const k of keys) {
    const v = src[k]
    if (Array.isArray(v)) return v.map((x) => String(x))
  }
  return []
}

function normalizeVectorLayer(raw: Record<string, unknown>): VectorLayer {
  return {
    name: pickStr(raw, 'name'),
    sourceId: pickStr(raw, 'source_id', 'sourceId'),
    schema: pickStr(raw, 'schema'),
    table: pickStr(raw, 'table'),
    geometryColumn: pickStr(raw, 'geometry_column', 'geometryColumn'),
    geometryType: pickStr(raw, 'geometry_type', 'geometryType'),
    srid: pickNum(raw, 'srid'),
    idColumn: pickStr(raw, 'id_column', 'idColumn'),
    fields: pickStrArr(raw, 'fields'),
    createdAt: pickStr(raw, 'created_at', 'createdAt'),
  }
}

function normalizeVectorSource(raw: Record<string, unknown>): VectorSource {
  return {
    id: pickStr(raw, 'id'),
    name: pickStr(raw, 'name'),
    dsn: pickStr(raw, 'dsn'),
    // 文件源的格式摘要与 PostGIS 版本串共用一个字段
    format: pickStr(raw, 'format', 'postgis_version', 'postgisVersion'),
    createdAt: pickStr(raw, 'created_at', 'createdAt'),
  }
}

/** 图层列表（GET /api/v1/vector/layers） */
export async function listVectorLayers(): Promise<VectorLayer[]> {
  const raw = await request<Record<string, unknown>>('/api/v1/vector/layers')
  const list = Array.isArray(raw?.layers) ? (raw.layers as Record<string, unknown>[]) : []
  return list.map(normalizeVectorLayer)
}

/** 数据源列表（GET /api/v1/vector/sources，连接串已打码） */
export async function listVectorSources(): Promise<VectorSource[]> {
  const raw = await request<Record<string, unknown>>('/api/v1/vector/sources')
  const list = Array.isArray(raw?.sources) ? (raw.sources as Record<string, unknown>[]) : []
  return list.map(normalizeVectorSource)
}

/** 图层元数据：范围、要素数、缩放建议（GET /api/v1/vector/layers/{name}/metadata） */
export async function vectorLayerMetadata(name: string): Promise<VectorLayerMetadata> {
  const raw = await request<Record<string, unknown>>(
    `/api/v1/vector/layers/${encodeURIComponent(name)}/metadata`,
  )
  const bboxRaw = raw?.bbox_wgs84 ?? raw?.bboxWgs84
  return {
    ...normalizeVectorLayer(raw),
    bboxWgs84: Array.isArray(bboxRaw) ? (bboxRaw as unknown[]).map((v) => Number(v)) : null,
    featuresEstimated: pickNum(raw, 'features_estimated', 'featuresEstimated'),
    minzoom: pickNum(raw, 'minzoom'),
    maxzoom: pickNum(raw, 'maxzoom'),
  }
}

/**
 * 一步注册本地矢量文件（POST /api/v1/vector/files）。
 * 桌面单机版主路径：传本地绝对路径（可先经 uploadFiles 落到 uploads/ 目录）。
 */
export async function registerVectorFile(payload: {
  path: string
  name?: string
}): Promise<{ source: VectorSource; layers: VectorLayer[] }> {
  const raw = await request<Record<string, unknown>>('/api/v1/vector/files', {
    method: 'POST',
    body: JSON.stringify({ path: payload.path, name: payload.name || undefined }),
  })
  const srcRaw = (raw?.source ?? {}) as Record<string, unknown>
  const layersRaw = Array.isArray(raw?.layers) ? (raw.layers as Record<string, unknown>[]) : []
  return { source: normalizeVectorSource(srcRaw), layers: layersRaw.map(normalizeVectorLayer) }
}

/** 删除图层（POST 无，DELETE /api/v1/vector/layers/{name}） */
export async function deleteVectorLayer(name: string): Promise<void> {
  await request<void>(`/api/v1/vector/layers/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

/** 删除数据源（仍被图层引用时后端返回 409） */
export async function deleteVectorSource(id: string): Promise<void> {
  await request<void>(`/api/v1/vector/sources/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** MVT 瓦片地址模板（供 MapLibre / OpenLayers / Cesium 直接使用） */
export function vectorTileUrlTemplate(layer: string, ext = '.pbf'): string {
  return `${API_BASE}/api/v1/vector/${encodeURIComponent(layer)}/{z}/{x}/{y}${ext}`
}

/** 单个 MVT 瓦片地址 */
export function vectorTileUrl(layer: string, z: number, x: number, y: number): string {
  return `${API_BASE}/api/v1/vector/${encodeURIComponent(layer)}/${z}/${x}/${y}.pbf`
}

/** WFS GetCapabilities 地址 */
export function vectorWfsCapabilitiesUrl(): string {
  return `${API_BASE}/api/v1/wfs?service=WFS&version=2.0.0&request=GetCapabilities`
}

/**
 * WFS GetFeature 地址（返回 GeoJSON）。
 * bbox 为 WGS84 经纬度序（minx,miny,maxx,maxy），与 WFS 2.0 的 CRS84 约定一致。
 */
export function vectorWfsFeatureUrl(
  layer: string,
  opts?: { bbox?: [number, number, number, number]; count?: number },
): string {
  const q = new URLSearchParams({
    service: 'WFS',
    version: '2.0.0',
    request: 'GetFeature',
    typeName: layer,
  })
  if (opts?.bbox) q.set('bbox', opts.bbox.map((v) => v.toFixed(6)).join(','))
  if (opts?.count) q.set('count', String(opts.count))
  return `${API_BASE}/api/v1/wfs?${q.toString()}`
}

/** 拉取图层要素（GeoJSON FeatureCollection），供列表预览绘制 */
export async function fetchVectorFeatures(
  layer: string,
  opts?: { bbox?: [number, number, number, number]; count?: number },
): Promise<GeoJsonFeatureCollection> {
  const res = await fetchWithAuth(vectorWfsFeatureUrl(layer, opts))
  return (await res.json()) as GeoJsonFeatureCollection
}

export { isTerminal }
