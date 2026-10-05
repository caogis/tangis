// Package api 组装 HTTP 路由（PRD 3.1 接入层之下的业务 API）。
package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
	"tangis/server/internal/cache"
	"tangis/server/internal/license"
	"tangis/server/internal/objectstore"
	"tangis/server/internal/queue"
	"tangis/server/internal/service"
	"tangis/server/internal/task"
	"tangis/server/internal/vector"
	"tangis/server/internal/webhook"
)

// AuthOptions 鉴权装配项（F-06）。
type AuthOptions struct {
	// Enabled 为 false 时（TANGIS_AUTH=off）跳过 API Key 校验与防盗链签名，
	// 并在启动日志打警告。
	Enabled bool
	// Keys API Key 校验存储；Enabled 时必须非 nil。
	Keys auth.KeyStore
	// DataDir 本地数据目录（内核日志下载接口读取），空取 "data"。
	DataDir string
	// SignSecret HMAC 签名密钥（env TANGIS_SIGN_SECRET），WMTS/TMS 的
	// 「签名放行」分支校验用（F-03）；空则签名分支禁用。
	SignSecret string
	// License 商业授权校验器（F-15），允许为 nil（按开源版处理，
	// internal/license 的方法 nil 安全）。商业特性入口（当前仅
	// CreateTask 的 params.distributed）据此门禁。
	License *license.Checker

	// ---- M2-F07 装配项 ----
	// Objects 打包下载的 MinIO 回源（Getter+Lister），可为 nil。
	Objects objectstore.GetterLister
	// Uploader 导入任务产物的 MinIO 冗余上传器，可为 nil。
	Uploader objectstore.Uploader
	// Notifier 导入任务终态 webhook 通知器，可为 nil。
	Notifier *webhook.Notifier
	// ImportMaxBytes/ImportMaxFiles/ImportMaxTotalBytes 导入安全限制，
	// 零值由 Handler 取默认（见 handlers.go）。
	ImportMaxBytes      int64
	ImportMaxFiles      int
	ImportMaxTotalBytes int64

	// ---- M2-F10a 装配项 ----
	// Vector 矢量发布服务（PostGIS → MVT），可为 nil（相关路由 503）。
	Vector *vector.Server

	// ---- M2-F14 装配项 ----
	// Quota 每日任务创建配额（UTC 日重置），可为 nil（不限额）；
	// 仅对 tenant 角色且 daily_task_limit>0 的 key 生效，超限 429+Retry-After。
	Quota *auth.Quota

	// ---- F-04 任务控制装配项 ----
	// Interrupter 中断运行中任务的内核进程（取消/暂停），可为 nil
	//（只做状态流转，内核进程会自然跑完但结果不再回写）。
	Interrupter TaskInterrupter
	// Runtime 运行时信息与能力开关（GET /api/v1/system），零值亦可。
	Runtime RuntimeInfo
	// KernelBin 内核可执行文件路径（合规同步算子 crs-identify/bursa 调用），可为空。
	KernelBin string

	// ---- 本机目录浏览（建任务选路径，替代手填路径） ----
	// FSBrowseEnabled 是否开放 /api/v1/fs/browse 与 /api/v1/fs/mkdir
	//（env TANGIS_FS_BROWSE，默认 on；off 时端点 404）。
	FSBrowseEnabled bool
	// FSBrowseRoots 允许浏览的根目录白名单（env TANGIS_FS_ROOTS 逗号分隔），
	// 空=不限制。
	FSBrowseRoots []string
}

// NewRouter 创建 gin 引擎并注册全部路由。
// store、publisher 通过参数注入；publisher 允许为 nil（NATS 降级模式）；
// services 允许为 nil（3DTiles 发布服务降级：相关路由返回 503）；
// authOpts 控制 /api/v1/* 管理接口的 X-API-Key 校验（F-06）；
// cch 为瓦片/列表缓存（F-03 Redis），允许为 nil（缓存禁用直连）。
func NewRouter(store task.Store, publisher queue.Publisher, services *service.Server, authOpts AuthOptions, cch cache.Cache) *gin.Engine {
	r := gin.Default()

	h := &Handler{
		Store:               store,
		Publisher:           publisher,
		Services:            services,
		DataDir:             authOpts.DataDir,
		Cache:               cch,
		License:             authOpts.License,
		Objects:             authOpts.Objects,
		Uploader:            authOpts.Uploader,
		Notifier:            authOpts.Notifier,
		ImportMaxBytes:      authOpts.ImportMaxBytes,
		ImportMaxFiles:      authOpts.ImportMaxFiles,
		ImportMaxTotalBytes: authOpts.ImportMaxTotalBytes,
		Vector:              authOpts.Vector,
		Quota:               authOpts.Quota,
		Interrupter:         authOpts.Interrupter,
		Runtime:             authOpts.Runtime,
		KernelBin:           authOpts.KernelBin,
		FSBrowseEnabled:     authOpts.FSBrowseEnabled,
		FSBrowseRoots:       authOpts.FSBrowseRoots,
	}

	// /api/v1 跨域支持（浏览器直连必需）：自定义头 X-API-Key 触发预检，
	// 而预检请求不携带任何鉴权头。gin 的组中间件不作用于未注册的
	// OPTIONS 路由（如 OPTIONS /api/v1/services 会直接 404），故挂全局
	// 中间件并按路径前缀过滤，使预检与 404 响应均带 CORS 头。
	r.Use(apiV1CORS())

	// 健康检查（容器编排探针用）：无鉴权
	r.GET("/healthz", h.Healthz)

	// 任务 API（PRD F-04 任务管理：创建即发布 NATS，Worker 消费驱动状态机）
	v1 := r.Group("/api/v1")
	if authOpts.Enabled {
		v1.Use(apiKeyMiddleware(authOpts.Keys))
	} else {
		log.Println("WARNING: TANGIS_AUTH=off, API key auth is DISABLED (dev compatibility only)")
		v1.Use(authOffMiddleware())
	}
	{
		// 运行信息与能力开关（设置页数据源，不占配额）
		v1.GET("/system", h.GetSystemInfo)

		// 测绘合规算子（F-21）：坐标系识别与七参数为秒级同步算子；
		// DEM 脱密是重活，走任务线（POST /tasks type=dem->desensitized）。
		v1.POST("/compliance/crs-identify", h.CRSIdentify)
		v1.POST("/compliance/bursa", h.BursaTransform)
		// 通用文件/目录上传（导入体验）：落盘后返回 root，可作任务 source
		v1.POST("/uploads", h.UploadFiles)

		v1.GET("/tasks", h.ListTasks)
		// 任务创建走每日配额中间件（M2-F14）：超限 429 + Retry-After，
		// 成功后由 handler 记账（Quota nil 时中间件直通）。
		// 本机目录浏览（建任务选路径用；FSBrowseEnabled=false 时 404）
		v1.GET("/fs/browse", h.BrowseFS)
		v1.POST("/fs/mkdir", h.MkdirFS)

		v1.POST("/tasks", taskQuotaMiddleware(h), h.CreateTask)
		v1.POST("/tasks/import", taskQuotaMiddleware(h), h.ImportTask) // .t3d 解包导入（M2-F07）
		v1.GET("/tasks/:id", h.GetTask)
		v1.DELETE("/tasks/:id", h.DeleteTask)
		// F-04 任务控制：取消 / 暂停 / 恢复 / 重试
		v1.POST("/tasks/:id/cancel", h.CancelTask)
		v1.POST("/tasks/:id/pause", h.PauseTask)
		v1.POST("/tasks/:id/resume", h.ResumeTask)
		v1.POST("/tasks/:id/retry", h.RetryTask)
		v1.GET("/tasks/:id/logs", h.GetTaskLogs)
		// 非瓦片产物下载（合规脱密 GeoTIFF / 留痕 JSON 等）
		v1.GET("/tasks/:id/artifact/*name", h.DownloadTaskArtifact)
		v1.GET("/tasks/:id/qc-report", h.GetTaskQCReport)   // 质检报告下载（M2-F08b）
		v1.POST("/tasks/:id/edit", h.CreateEditTask)        // 发起编辑任务（M2-F09c）
		v1.GET("/tasks/:id/edit-history", h.EditHistory)    // 编辑链查询（M2-F09c）
		v1.GET("/tasks/:id/ops-report", h.GetTaskOpsReport) // 编辑留痕下载（M2-F09c）
		v1.POST("/tasks/:id/approve", h.ApproveTask)
		v1.GET("/services", h.ListServices)

		// 矢量发布管理（M2-F10a，F-10）：数据源/图层注册元数据
		// 文件矢量一步注册（桌面单机版主路径：本地 GeoJSON 文件 → 图层）
		v1.POST("/vector/files", h.CreateVectorFileSource)
		v1.POST("/vector/sources", h.CreateVectorSource)
		v1.GET("/vector/sources", h.ListVectorSources)
		v1.DELETE("/vector/sources/:id", h.DeleteVectorSource)
		v1.POST("/vector/layers", h.CreateVectorLayer)
		v1.GET("/vector/layers", h.ListVectorLayers)
		v1.GET("/vector/layers/:name/metadata", h.VectorLayerMetadata)
		// 导出为其它 GIS 软件可直接打开的文件（GeoJSON / GeoPackage / Shapefile zip）
		v1.GET("/vector/layers/:name/export", h.ExportVectorLayer)
		// 就地编辑：要素增删改（写回数据源，首次编辑前自动备份）
		v1.POST("/vector/layers/:name/edits", h.ApplyVectorEdits)
		v1.DELETE("/vector/layers/:name", h.DeleteVectorLayer)
	}

	// 2D 影像瓦片服务（F-03）：WMTS（KVP + RESTful）与 TMS。
	// 鉴权语义：X-API-Key 或防盗链签名任一通过即可（与 3D 分发同密钥），
	// TANGIS_AUTH=off 时全放行。
	tile := r.Group("/api/v1", tileAuthMiddleware(authOpts, h))
	{
		tile.GET("/wmts", h.WMTSKvp)
		// WMS 1.3.0（GetCapabilities/GetMap）与 WCS 1.0.0（GetCoverage）：
		// 与 WMTS 共用同一份影像金字塔，鉴权语义一致。
		tile.GET("/wms", h.WMSKvp)
		tile.GET("/wcs", h.WCSKvp)
		tile.GET("/wmts/1.0.0/:layer/:style/:tms/:z/:row/:col", h.WMTSRestTile)
		tile.GET("/tms/:task_id/:z/:x/:y", h.TMSTile)
		// .t3d 打包下载（M2-F07）：鉴权语义同上（API Key 或签名 URL），
		// 复用 tileAuthMiddleware（签名覆盖完整请求 path）。
		tile.GET("/tasks/:id/package", h.PackageTask)
		// 矢量 MVT 分发（M2-F10a，F-10）：鉴权语义同上；
		// URL 形如 /api/v1/vector/{layer}/{z}/{x}/{y}.pbf（.pbf 后缀可选）。
		tile.GET("/vector/:layer/:z/:x/:y", h.VectorTile)
		// WFS 2.0 简单子集（M2-F14）：GetCapabilities/GetFeature KVP 入口，
		// 鉴权与缓存沿用 vector MVT 同一语义。
		tile.GET("/wfs", h.WFSKvp)
	}

	// 3DTiles 服务发布（产物读取，含 tileset.json 与瓦片文件；带 CORS；
	// 鉴权开启时由 service.Server 校验防盗链签名，不走 API Key）
	r.GET("/services/:task_id/*path", h.ServeTile)
	r.OPTIONS("/services/:task_id/*path", h.ServeTile)

	return r
}

// apiV1CORS /api/v1/* 的 CORS 中间件：允许所有 origin、放行 X-API-Key/X-Role
// 自定义头；OPTIONS 预检直接 204 短路（不进入鉴权与业务处理）。
// /services/* 瓦片分发沿用 service.setCORS，不受此中间件影响。
func apiV1CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if p != "/api/v1" && !strings.HasPrefix(p, "/api/v1/") {
			c.Next()
			return
		}
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Accept, Content-Type, X-API-Key, X-Role")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Content-Range")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// apiKeyMiddleware 校验 X-API-Key（F-06），并把身份注入 context：
//   - key_id：配额记账主体（M2-F14）；
//   - daily_task_limit：该 key 的每日任务创建配额（0=不限）；
//   - tenant_id：任务归属租户（创建任务用）；
//   - role：admin / tenant；
//   - scope_tenant：查询范围（admin 为空串=全租户，tenant 为其租户 ID）。
//   - disabled key 拒绝 403（M2-F14 多 Key 管理）。
func apiKeyMiddleware(keys auth.KeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, err := keys.Verify(c.GetHeader("X-API-Key"))
		if err != nil {
			if errors.Is(err, auth.ErrDisabledKey) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "api key disabled"})
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing X-API-Key"})
			return
		}
		scope := key.TenantID
		if key.Role == auth.RoleAdmin {
			scope = "" // 管理员可见全部租户
		}
		c.Set("key_id", key.ID)
		c.Set("daily_task_limit", key.DailyTaskLimit)
		c.Set("tenant_id", key.TenantID)
		c.Set("role", key.Role)
		c.Set(service.ScopeTenantKey, scope)
		c.Next()
	}
}

// authOffMiddleware TANGIS_AUTH=off 的开发兼容：跳过校验，
// 一律按 admin + default 租户处理。
func authOffMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("auth_off", true)
		c.Set("tenant_id", "default")
		c.Set("role", auth.RoleAdmin)
		c.Set(service.ScopeTenantKey, "")
		c.Next()
	}
}

// tileAuthMiddleware WMTS/TMS 2D 分发鉴权（F-03）：
// API Key 或防盗链签名任一通过即可（WMTS/TMS 客户端不便统一携带 header，
// 故允许签名 URL）；签名覆盖请求 path（不含 query），与 3D 分发同密钥。
// 仅签名放行时 scope_tenant 为空串=可见全部已发布影像服务（与 3D 分发一致）。
func tileAuthMiddleware(opts AuthOptions, h *Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !opts.Enabled {
			authOffMiddleware()(c)
			return
		}
		// 分支一：X-API-Key
		if key, err := opts.Keys.Verify(c.GetHeader("X-API-Key")); err == nil {
			scope := key.TenantID
			if key.Role == auth.RoleAdmin {
				scope = ""
			}
			c.Set("key_id", key.ID)
			c.Set("tenant_id", key.TenantID)
			c.Set("role", key.Role)
			c.Set(service.ScopeTenantKey, scope)
			c.Next()
			return
		}
		// 分支二：防盗链签名（sig + expires，path 维度）
		expires, _ := strconv.ParseInt(c.Query("expires"), 10, 64)
		if opts.SignSecret != "" && auth.VerifySignature(c.Request.URL.Path, expires, c.Query("sig"), opts.SignSecret) {
			c.Set(service.ScopeTenantKey, "")
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "X-API-Key or signed URL required (sig/expires query params)",
		})
	}
}
