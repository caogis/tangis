package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
	"tangis/server/internal/cache"
	"tangis/server/internal/license"
	"tangis/server/internal/objectstore"
	"tangis/server/internal/queue"
	"tangis/server/internal/service"
	"tangis/server/internal/t3d"
	"tangis/server/internal/task"
	"tangis/server/internal/vector"
	"tangis/server/internal/webhook"
)

// Handler 承载全部 HTTP handler，依赖注入 task.Store 与 queue.Publisher。
// Publisher 允许为 nil（NATS 不可用降级运行：任务保持 PENDING）；
// Services 允许为 nil（3DTiles 发布服务降级：路由返回 503）；
// Cache 允许为 nil（F-03 列表缓存禁用，直连存储）。
type Handler struct {
	Store     task.Store
	Publisher queue.Publisher
	Services  *service.Server
	Cache     cache.Cache
	// DataDir 本地数据目录（worker 内核日志落盘处），空取 "data"，
	// 与 worker.Worker.DataDir 保持一致（TANGIS_DATA_DIR）。
	DataDir string

	// ListCacheTTL 任务列表缓存 TTL（env TANGIS_LIST_CACHE_TTL，默认 5s；
	// 负数禁用）。服务列表缓存 TTL 由 service.Server 自管。
	ListCacheTTL int

	// License 商业授权校验器（F-15），允许为 nil（开源版）。
	// 商业特性入口据此门禁：nil 或校验失败按开源版处理。
	License *license.Checker

	// Objects 打包下载的 MinIO 回源（M2-F07），可为 nil（仅本地产物）。
	Objects objectstore.GetterLister
	// Uploader 导入任务产物的 MinIO 冗余上传器，可为 nil（跳过上传）。
	Uploader objectstore.Uploader
	// Notifier 导入任务终态 webhook 通知器，可为 nil（禁用，F-14 第一步）。
	Notifier *webhook.Notifier

	// ImportMaxBytes .t3d 上传体积上限（env TANGIS_IMPORT_MAX_BYTES），
	// 零值取默认 2 GiB。
	ImportMaxBytes int64
	// ImportMaxFiles .t3d ZIP 条目数上限（env TANGIS_IMPORT_MAX_FILES），
	// 零值取默认 65536。
	ImportMaxFiles int
	// ImportMaxTotalBytes .t3d 解压总量上限（env TANGIS_IMPORT_MAX_TOTAL_BYTES），
	// 零值取默认 8 GiB。
	ImportMaxTotalBytes int64

	// Vector 矢量发布服务（M2-F10a，PRD F-10），可为 nil
	//（未装配时 vector 相关路由返回 503）。
	Vector *vector.Server

	// Quota 每日任务创建配额（M2-F14），可为 nil（不限额）。
	Quota *auth.Quota

	// Interrupter 任务中断器（F-04 取消/暂停），由 worker 实现；
	// 可为 nil：此时取消/暂停只做状态流转，不杀内核进程。
	Interrupter TaskInterrupter

	// Runtime 运行时信息与能力开关（GET /api/v1/system），由装配侧填充。
	Runtime RuntimeInfo

	// KernelBin 内核可执行文件路径，供合规同步算子（crs-identify / bursa）
	// 直接调用；空则退回仓库内 debug 构建（仅便于本地开发）。
	KernelBin string
}

// LogPath 任务内核日志的本地路径（与 worker 侧约定一致：data/logs/{id}.log）。
func (h *Handler) LogPath(taskID string) string {
	dir := h.DataDir
	if dir == "" {
		dir = "data"
	}
	return filepath.Join(dir, "logs", taskID+".log")
}

// scopeTenant 从 gin context 取「查询租户范围」：
// admin（或鉴权关闭）为空串=全租户；普通 tenant key 仅可见本租户。
func scopeTenant(c *gin.Context) string {
	return c.GetString(service.ScopeTenantKey)
}

// isAuthOff 鉴权是否关闭（TANGIS_AUTH=off 的开发兼容模式，
// 此模式下 approve 放行、任务归属 default 租户）。
func isAuthOff(c *gin.Context) bool {
	v, ok := c.Get("auth_off")
	return ok && v.(bool)
}

// ListServices GET /api/v1/services — 列出已发布 3DTiles/影像瓦片服务（按租户隔离）。
func (h *Handler) ListServices(c *gin.Context) {
	h.Services.ListServices(c)
}

// WMTSKvp GET /api/v1/wmts — WMTS KVP 入口（GetCapabilities/GetTile，F-03）。
func (h *Handler) WMTSKvp(c *gin.Context) {
	h.Services.WMTSKvp(c)
}

// WMSKvp GET /api/v1/wms — WMS 1.3.0/1.1.1 KVP 入口（GetCapabilities/GetMap，F-03）。
func (h *Handler) WMSKvp(c *gin.Context) {
	h.Services.WMSKvp(c)
}

// WCSKvp GET /api/v1/wcs — WCS 1.0.0 KVP 入口（GetCapabilities/DescribeCoverage/GetCoverage）。
func (h *Handler) WCSKvp(c *gin.Context) {
	h.Services.WCSKvp(c)
}

// WMTSRestTile GET /api/v1/wmts/1.0.0/... — WMTS RESTful GetTile（F-03）。
func (h *Handler) WMTSRestTile(c *gin.Context) {
	h.Services.WMTSRestTile(c)
}

// TMSTile GET /api/v1/tms/{task_id}/{z}/{x}/{y}.{ext} — TMS 瓦片（F-03）。
func (h *Handler) TMSTile(c *gin.Context) {
	h.Services.TMSTile(c)
}

// ServeTile GET /services/:task_id/*path — 读取已发布产物文件。
func (h *Handler) ServeTile(c *gin.Context) {
	h.Services.ServeTile(c)
}

// Healthz GET /healthz — 存活探针。
func (h *Handler) Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// createTaskReq POST /api/v1/tasks 请求体。
// type/source 必填；output 可选——省略时按 <dataDir>/outputs/{task_id}
// 自动分配（桌面单机版免填路径，服务端模式仍可显式指定）。
// params 可选（F-04 参数版本化）。
type createTaskReq struct {
	Type   string         `json:"type" binding:"required"`   // 转换类型，如 osgb->3dtiles
	Source string         `json:"source" binding:"required"` // 源数据引用
	Output string         `json:"output"`                    // 产物输出引用，省略则自动分配
	Params map[string]any `json:"params"`                    // 任务参数（JSONB 存储，随任务下发内核）
}

// CreateTask POST /api/v1/tasks — 创建切片/转换任务。
// 落库后向 NATS 发布 TASKS.created 驱动 Worker；发布失败时优雅降级：
// 任务保持 PENDING，响应体附 warning 提示，建任务本身不失败。
// 任务归属租户取自 API Key（鉴权关闭时为 default）。
func (h *Handler) CreateTask(c *gin.Context) {
	var req createTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tenantID := c.GetString("tenant_id")
	if isAuthOff(c) {
		tenantID = "default"
	}

	// 商业特性门禁（F-15）：params.distributed=true 请求分布式切片集群
	// （PRD F-16，当前为接口预留），需有效 License 解锁；
	// License 为 nil 或未授权时按开源版拒绝并返回明确错误。
	if b, ok := req.Params["distributed"].(bool); ok && b {
		if err := h.License.EnsureFeature(license.FeatureDistributed); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
	}

	// 输出目录缺省则自动分配：<dataDir>/outputs/{task_id}。
	// 桌面单机版（TANGIS_MODE=desktop）下 dataDir 指向用户数据目录 ~/TanGIS，
	// 用户无需感知路径；服务端模式仍可显式指定 output。
	id, err := task.NewID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	output := req.Output
	if output == "" {
		output = filepath.Join(h.dataDir(), "outputs", id)
	}

	t := &task.Task{
		ID:           id,
		Type:         req.Type,
		Source:       req.Source,
		Output:       output,
		Params:       req.Params,
		Status:       task.StatusPending,
		TenantID:     tenantID,
		ManifestPath: filepath.Join(output, "manifest.json"),
	}
	if err := h.Store.Create(t); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.recordTaskQuota(c) // 每日任务配额记账（M2-F14）

	resp := gin.H{}
	if h.Publisher == nil {
		resp["warning"] = "queue unavailable, task stays PENDING (NATS not connected)"
	} else if err := h.Publisher.Publish(queue.TaskMessage{
		TaskID:       t.ID,
		Type:         t.Type,
		Source:       t.Source,
		Output:       t.Output,
		ManifestPath: t.ManifestPath,
		Params:       t.Params,
	}); err != nil {
		log.Printf("publish task %s failed, degraded to PENDING: %v", t.ID, err)
		resp["warning"] = "queue publish failed, task stays PENDING: " + err.Error()
	}
	h.invalidateLists(tenantID)

	resp["task"] = t
	c.JSON(http.StatusCreated, resp)
}

// ListTasks GET /api/v1/tasks — 任务列表（按租户隔离）。
// 结果走短 TTL 缓存（TANGIS_LIST_CACHE_TTL，默认 5s），写路径主动失效，
// worker 侧状态回写依赖 TTL 自然过期。
func (h *Handler) ListTasks(c *gin.Context) {
	scope := scopeTenant(c)
	data, hit, err := cache.CachedJSON(c.Request.Context(), h.Cache, cache.ListKeys("tasks", scope),
		h.listCacheTTL(), func() (any, error) {
			tasks, err := h.Store.List(scope)
			if err != nil {
				return nil, err
			}
			return gin.H{"tasks": tasks}, nil
		})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if hit {
		c.Header("X-Cache", "HIT")
	} else {
		c.Header("X-Cache", "MISS")
	}
	c.Data(http.StatusOK, "application/json", data)
}

// listCacheTTL 任务列表缓存 TTL：0 取默认 5s，负数禁用。
func (h *Handler) listCacheTTL() time.Duration {
	if h.ListCacheTTL > 0 {
		return time.Duration(h.ListCacheTTL) * time.Second
	}
	if h.ListCacheTTL == 0 {
		return 5 * time.Second
	}
	return -1
}

// invalidateLists 写路径一致性（F-03）：任务创建/删除/审批后失效相关
// 租户与全租户（admin）两个维度的列表缓存。
func (h *Handler) invalidateLists(tenantIDs ...string) {
	if h.Cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	scopes := map[string]bool{"": true}
	for _, id := range tenantIDs {
		if id != "" {
			scopes[id] = true
		}
	}
	for scope := range scopes {
		h.Cache.Delete(ctx, cache.ListKeys("tasks", scope))
		h.Cache.Delete(ctx, cache.ListKeys("services", scope))
	}
}

// GetTask GET /api/v1/tasks/:id — 查询单个任务（含 progress，按租户隔离）。
func (h *Handler) GetTask(c *gin.Context) {
	t, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, t)
}

// DeleteTask DELETE /api/v1/tasks/:id — 删除任务（按租户隔离）。
func (h *Handler) DeleteTask(c *gin.Context) {
	err := h.Store.Delete(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.invalidateLists(scopeTenant(c))
	c.Status(http.StatusNoContent)
}

// GetTaskLogs GET /api/v1/tasks/:id/logs — 下载内核执行日志（F-04）。
// 日志由 worker 落盘（data/logs/{task_id}.log）；尚无日志返回 404。
func (h *Handler) GetTaskLogs(c *gin.Context) {
	t, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logPath := h.LogPath(t.ID)
	data, err := os.ReadFile(logPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no kernel log yet"})
		return
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=\""+t.ID+".log\"")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", data)
}

// GetTaskQCReport GET /api/v1/tasks/:id/qc-report — 下载完整质检报告 JSON（M2-F08b）。
// 报告由 worker 落盘于产物目录（<output>/qc-report.json，task.QcReportFile）；
// 未跑质检或报告缺失返回 404（不伪造内容）。鉴权同 logs（v1 组 API Key 中间件）。
func (h *Handler) GetTaskQCReport(c *gin.Context) {
	t, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	reportPath := filepath.Join(t.Output, task.QcReportFile)
	data, err := os.ReadFile(reportPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no qc report yet"})
		return
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=\""+t.ID+"-qc-report.json\"")
	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}

// ApproveTask POST /api/v1/tasks/:id/approve — 发布审批（F-21）。
// 仅管理员 key 可调用（TANGIS_AUTH=off 时放行）；审批通过后任务才进入
// /api/v1/services 与分发路径。
func (h *Handler) ApproveTask(c *gin.Context) {
	if !isAuthOff(c) && c.GetString("role") != auth.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin key required"})
		return
	}
	t, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	t.Approved = true
	if err := h.Store.Update(t); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.invalidateLists(t.TenantID)
	c.JSON(http.StatusOK, t)
}

// ---- M2-F07：.t3d 打包下载与解包导入 ----

// dataDir 任务工作目录根（与 worker.DataDir 同一 env TANGIS_DATA_DIR），空取 "data"。
func (h *Handler) dataDir() string {
	if h.DataDir != "" {
		return h.DataDir
	}
	return "data"
}

// importMaxBytes 上传体积上限（含默认值兜底）。
func (h *Handler) importMaxBytes() int64 {
	if h.ImportMaxBytes > 0 {
		return h.ImportMaxBytes
	}
	return 2 << 30
}

// importExtractOptions 解包安全限制（含默认值兜底）。
func (h *Handler) importExtractOptions() t3d.ExtractOptions {
	opts := t3d.ExtractOptions{MaxFiles: h.ImportMaxFiles, MaxTotalBytes: h.ImportMaxTotalBytes}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 65536
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = 8 << 30
	}
	return opts
}

// PackageTask GET /api/v1/tasks/:id/package — 把任务 3DTiles 产物打包为
// .t3d（zip）流式下载（M2-F07，规范见 docs/CONTAINER-FORMAT.md）。
// 前置条件：SUCCEEDED + 已审批 + 产物含 tileset.json（本地优先，MinIO 回源）；
// 鉴权：API Key 或防盗链签名任一（路由层 tileAuthMiddleware）。
// zip 流式写出（manifest.json 首条），不全量驻留内存；每次打包记审计日志。
func (h *Handler) PackageTask(c *gin.Context) {
	t, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if t.Status != task.StatusSucceeded {
		c.JSON(http.StatusConflict, gin.H{"error": "task not finished: " + string(t.Status)})
		return
	}
	if !t.Approved {
		c.JSON(http.StatusForbidden, gin.H{"error": "task not approved for distribution"})
		return
	}

	// 产物来源：本地 output 优先，缺失回源 MinIO（tasks/{id} 前缀）
	src := t3d.ArtifactSource{Dir: t.Output, Objects: h.Objects, Prefix: t.MinioPrefix}

	// 先流式哈希得清单并静态校验（确保是 3DTiles 产物），再写响应头
	files, herr := t3d.HashSource(src)
	if herr != nil || len(files) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no artifacts to package"})
		return
	}
	manifest := &t3d.Manifest{
		Format:  t3d.FormatName,
		Version: t3d.FormatVersion,
		Task: t3d.TaskInfo{
			ID:        t.ID,
			Type:      t.Type,
			Source:    t.Source,
			CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		},
		Origin: t3dOrigin(t.Params),
		Params: t.Params,
		Files:  files,
	}
	if verr := manifest.Validate(); verr != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "artifacts are not a 3D Tiles package: " + verr.Error()})
		return
	}

	// 审计日志（谁、何时、哪个任务）
	log.Printf("[audit] package_download task=%s tenant=%s role=%s ip=%s files=%d",
		t.ID, c.GetString("tenant_id"), c.GetString("role"), c.ClientIP(), len(files))

	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.t3d\"", t.ID))
	c.Status(http.StatusOK)
	if perr := t3d.Pack(c.Writer, src, manifest); perr != nil {
		// 头已发出，无法改状态码：记错误日志（客户端将得到截断的 zip）
		log.Printf("[audit] package_download task=%s FAILED mid-stream: %v", t.ID, perr)
	}
}

// t3dOrigin 从任务参数提取 origin（[lon, lat, height] 数值数组），非法返回 nil。
func t3dOrigin(params map[string]any) []float64 {
	raw, ok := params["origin"].([]any)
	if !ok || len(raw) != 3 {
		return nil
	}
	out := make([]float64, 3)
	for i, v := range raw {
		f, ok := v.(float64)
		if !ok {
			return nil
		}
		out[i] = f
	}
	return out
}

// ImportTask POST /api/v1/tasks/import — 上传 .t3d 包解包导入（M2-F07）。
// 校验 manifest（SHA256 逐文件，Zip Slip/条目数/解压总量防护）→ 落盘任务
// 工作目录 data/tasks/{id}/ → 创建任务 type=t3d-import、直接 SUCCEEDED
// （跳过内核，进入审批→分发链路）。上传体积受 ImportMaxBytes 限制；
// 导入动作记审计日志。
func (h *Handler) ImportTask(c *gin.Context) {
	// 上传体积上限（请求体整体裁断）
	maxBytes := h.importMaxBytes()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "multipart field 'file' required: " + err.Error()})
		return
	}
	defer file.Close()
	if header.Size > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("file exceeds limit %d bytes", maxBytes)})
		return
	}

	// 流式暂存上传内容（不驻留内存），边拷边计数兜底防伪造 Size
	tmp, err := os.CreateTemp("", "tangis-import-*.t3d")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	n, err := io.Copy(tmp, file)
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read upload: " + err.Error()})
		return
	}
	if n > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("upload %d bytes exceeds limit %d", n, maxBytes)})
		return
	}

	// 任务 ID 与工作目录先于解包确定，失败即清理
	id, err := task.NewID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	destDir := filepath.Join(h.dataDir(), "tasks", id)

	res, xerr := t3d.Extract(tmpPath, destDir, h.importExtractOptions())
	if xerr != nil {
		os.RemoveAll(destDir) // 整体拒绝：不做部分导入
		status := http.StatusBadRequest
		if !errors.Is(xerr, t3d.ErrInvalid) {
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"error": xerr.Error()})
		return
	}

	// 上传原件归档为任务源引用（data/imports/{id}.t3d），便于追溯/重放
	source := filepath.Join(h.dataDir(), "imports", id+".t3d")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		source = header.Filename // 归档失败降级记原始文件名，不阻塞导入
	} else if err := os.Rename(tmpPath, source); err != nil {
		source = header.Filename
	}

	tenantID := c.GetString("tenant_id")
	if isAuthOff(c) {
		tenantID = "default"
	}
	// 恢复 manifest 参数，webhook_url 表单字段优先
	params := res.Manifest.Params
	webhookURL := c.PostForm("webhook_url")
	if webhookURL != "" {
		if params == nil {
			params = map[string]any{}
		}
		params["webhook_url"] = webhookURL
	}

	t := &task.Task{
		ID:       id,
		Type:     task.TypeT3dImport,
		Source:   source,
		Output:   destDir,
		Status:   task.StatusSucceeded, // 跳过内核：产物即解包内容
		Params:   params,
		TenantID: tenantID,
	}
	if err := h.Store.Create(t); err != nil {
		os.RemoveAll(destDir)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.recordTaskQuota(c) // 每日任务配额记账（M2-F14）

	log.Printf("[audit] import task=%s tenant=%s role=%s ip=%s file=%q files=%d bytes=%d",
		id, tenantID, c.GetString("role"), c.ClientIP(), header.Filename, res.Files, res.Bytes)

	// MinIO 冗余上传（异步，失败仅告警，不影响任务状态与分发）
	if h.Uploader != nil {
		prefix := "tasks/" + id
		uploader, store := h.Uploader, h.Store
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if uerr := uploader.UploadDir(ctx, destDir, prefix); uerr != nil {
				log.Printf("[audit] import task=%s minio upload failed (degraded, local distribution intact): %v", id, uerr)
				return
			}
			if cur, gerr := store.Get(id, ""); gerr == nil {
				cur.MinioPrefix = prefix
				if uerr := store.Update(cur); uerr != nil {
					log.Printf("[audit] import task=%s minio_prefix writeback failed: %v", id, uerr)
				}
			}
		}()
	}

	// 导入即终态 SUCCEEDED：与 worker 同语义发送 webhook（F-14）
	if url, _ := params["webhook_url"].(string); url != "" && h.Notifier != nil {
		notifier := h.Notifier
		ev := webhook.Event{
			TaskID:    id,
			Status:    string(task.StatusSucceeded),
			Timestamp: time.Now().UTC(),
		}
		go func() {
			if nerr := notifier.Notify(url, ev); nerr != nil {
				log.Printf("[audit] import task=%s webhook to %s failed after retries: %v", id, url, nerr)
			}
		}()
	}

	h.invalidateLists(tenantID)
	c.JSON(http.StatusCreated, gin.H{"task": t})
}
