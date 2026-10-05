// Package service 实现 3DTiles 服务发布（M1 简化版）：
//
// 发布审批制（F-21）——任务 SUCCEEDED 且产物上传完成后，还需管理员审批
// （Task.Approved=true）才对外发布。提供两组能力：
//   - GET /api/v1/services：列出已审批发布的服务（tileset_url 自带防盗链签名）；
//   - GET /services/{task_id}/{file}：读取产物文件（含 tileset.json），
//     本地 output 目录优先，缺失时从 MinIO 流式回源。
//
// 安全面：
//   - file 路径做规范化校验，拒绝目录穿越（../、URL 编码 ../、绝对路径）；
//   - 鉴权开启时分发请求须携带有效 HMAC-SHA256 签名（sig + expires，
//     密钥 env TANGIS_SIGN_SECRET），见 internal/auth 的 SignURL/VerifySignature；
//   - 所有响应带 CORS 头（Cesium 跨域加载必需）。
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
	"tangis/server/internal/cache"
	"tangis/server/internal/objectstore"
	"tangis/server/internal/task"
)

// ScopeTenantKey gin context 中「查询租户范围」的 key：
// 中间件按 API Key 角色计算（admin 为空串=全租户，tenant 为其租户 ID）。
const ScopeTenantKey = "scope_tenant"

// Getter 服务回源所需的按 key 读取接口（*objectstore.Client 实现）。
type Getter = objectstore.Getter

// Server 3DTiles 发布服务。
type Server struct {
	Store   task.Store
	Objects Getter // MinIO 回源，可为 nil（不可用时仅走本地）
	Bucket  string // MinIO bucket（记录用途，key 由 minio_prefix 推导）

	// AuthEnabled 鉴权开启时分发请求须携带有效防盗链签名（TANGIS_AUTH=off 时 false 跳过）。
	AuthEnabled bool
	// SignSecret HMAC 签名密钥（env TANGIS_SIGN_SECRET）。
	SignSecret string
	// SignTTL ListServices 返回的 tileset_url 签名有效期；零值取默认 1 小时。
	SignTTL time.Duration
	// Cache 瓦片/列表缓存（Redis），可为 nil（缓存禁用，直读存储）。
	Cache cache.Cache
	// CacheTTL 瓦片缓存 TTL（env TANGIS_CACHE_TTL）；零值取默认 5 分钟。
	CacheTTL time.Duration
	// ListCacheTTL 服务列表缓存 TTL（env TANGIS_LIST_CACHE_TTL）；零值取默认 5 秒，
	// 负数禁用列表缓存。
	ListCacheTTL time.Duration
}

// ServiceEntry GET /api/v1/services 列表项。
// 3D（3DTiles）任务返回 tileset_url；2D 影像瓦片任务（F-03）返回
// wmts_url（GetCapabilities 入口，签名模式已带防盗链签名）与
// tms_url（URL 模板，签名模式下瓦片需逐条签名或改用 API Key/WMTS 入口）。
type ServiceEntry struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Output      string `json:"output"`
	MinioPrefix string `json:"minio_prefix,omitempty"`
	TilesetURL  string `json:"tileset_url,omitempty"` // 已带 sig + expires 防盗链签名（仅 3D 产物真实存在时返回）
	WmtsURL     string `json:"wmts_url,omitempty"`    // WMTS GetCapabilities 入口（F-03）
	TmsURL      string `json:"tms_url,omitempty"`     // TMS URL 模板（F-03）
	WmsURL      string `json:"wms_url,omitempty"`     // WMS 1.3.0 GetCapabilities 入口（带签名）
	WcsURL      string `json:"wcs_url,omitempty"`     // WCS 1.0.0 GetCapabilities 入口（带签名）
	TerrainURL  string `json:"terrain_url,omitempty"` // Cesium TerrainProvider 入口 layer.json（A5，带签名）
}

// ListServices GET /api/v1/services — 列出已发布服务
// （= SUCCEEDED 且产物已上传且审批通过的任务，F-21）。
// 结果走短 TTL 列表缓存（TANGIS_LIST_CACHE_TTL，默认 5s），写路径主动失效。
func (s *Server) ListServices(c *gin.Context) {
	if s == nil || s.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service registry unavailable"})
		return
	}
	scope := c.GetString(ScopeTenantKey)
	data, hit, err := cache.CachedJSON(c.Request.Context(), s.Cache, cache.ListKeys("services", scope),
		s.listCacheTTL(), func() (any, error) { return s.buildServiceList(scope) })
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

// buildServiceList 构造服务列表（真实产物探测：瓦片元数据 / tileset.json）。
func (s *Server) buildServiceList(scope string) (any, error) {
	tasks, err := s.Store.List(scope)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceEntry, 0)
	for _, t := range tasks {
		if !s.published(t) {
			continue
		}
		e := ServiceEntry{
			ID:          t.ID,
			Type:        t.Type,
			Status:      string(t.Status),
			Output:      t.Output,
			MinioPrefix: t.MinioPrefix,
		}
		if meta, merr := s.LoadPyramidMeta(t); merr == nil {
			// 2D 影像瓦片服务（F-03）：WMTS/TMS 按瓦片取，WMS/WCS 按 bbox 取整图，
			// 三者共用同一份影像金字塔。
			e.WmtsURL = s.capabilitiesURL()
			e.TmsURL = fmt.Sprintf("/api/v1/tms/%s/{z}/{x}/{y}.%s", t.ID, canonicalFormat(meta.Format))
			e.WmsURL = s.wmsCapabilitiesURL()
			e.WcsURL = s.wcsCapabilitiesURL()
		} else if s.hasLayer(t) {
			// 地形服务（A5）：layer.json + quantized-mesh 经 /services 通道分发，
			// 入口 layer.json 带签名；Cesium TerrainProvider 直接消费。
			e.TerrainURL = s.signedTerrainURL(t.ID)
		} else if s.hasTileset(t) {
			e.TilesetURL = s.signedTilesetURL(t.ID)
		} else {
			continue // 无任何可分发产物：不进服务列表（不造假 URL）
		}
		out = append(out, e)
	}
	return gin.H{"services": out}, nil
}

// capabilitiesURL WMTS GetCapabilities 入口地址；鉴权开启时带防盗链签名
// （签名覆盖 path /api/v1/wmts，与分发路径同一密钥）。
func (s *Server) capabilitiesURL() string {
	p := "/api/v1/wmts?service=WMTS&version=1.0.0&request=GetCapabilities"
	if !s.AuthEnabled || s.SignSecret == "" {
		return p
	}
	return p + "&" + auth.SignURL("/api/v1/wmts", time.Now().Add(s.signTTL()), s.SignSecret)
}

// wmsCapabilitiesURL WMS 1.3.0 GetCapabilities 入口（鉴权开启时带防盗链签名）。
func (s *Server) wmsCapabilitiesURL() string {
	return s.signedKvpEntry("/api/v1/wms", "service=WMS&version=1.3.0&request=GetCapabilities")
}

// wcsCapabilitiesURL WCS 1.0.0 GetCapabilities 入口（鉴权开启时带防盗链签名）。
func (s *Server) wcsCapabilitiesURL() string {
	return s.signedKvpEntry("/api/v1/wcs", "service=WCS&request=GetCapabilities")
}

// signedKvpEntry 生成带防盗链签名的 KVP 入口（签名覆盖 path，与瓦片分发同密钥）。
func (s *Server) signedKvpEntry(path, query string) string {
	p := path + "?" + query
	if !s.AuthEnabled || s.SignSecret == "" {
		return p
	}
	return p + "&" + auth.SignURL(path, time.Now().Add(s.signTTL()), s.SignSecret)
}

// signedTerrainURL 生成带防盗链签名的地形 layer.json 分发入口（A5）。
func (s *Server) signedTerrainURL(taskID string) string {
	p := "/services/" + taskID + "/layer.json"
	if !s.AuthEnabled || s.SignSecret == "" {
		return p
	}
	return p + "?" + auth.SignURL(p, time.Now().Add(s.signTTL()), s.SignSecret)
}

// hasLayer 任务产物是否真实包含地形 layer.json（本地或 MinIO，A5）。
func (s *Server) hasLayer(t *task.Task) bool {
	if _, err := os.Stat(filepath.Join(t.Output, "layer.json")); err == nil {
		return true
	}
	if s.Objects != nil && t.MinioPrefix != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		rc, _, err := s.Objects.Get(ctx, path.Join(t.MinioPrefix, "layer.json"))
		if err == nil {
			rc.Close()
			return true
		}
	}
	return false
}

// hasTileset 任务产物是否真实包含 tileset.json（本地或 MinIO）。
func (s *Server) hasTileset(t *task.Task) bool {
	if _, err := os.Stat(filepath.Join(t.Output, "tileset.json")); err == nil {
		return true
	}
	if s.Objects != nil && t.MinioPrefix != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		rc, _, err := s.Objects.Get(ctx, path.Join(t.MinioPrefix, "tileset.json"))
		if err == nil {
			rc.Close()
			return true
		}
	}
	return false
}

// signTTL 防盗链签名有效期（含默认值兜底）。
func (s *Server) signTTL() time.Duration {
	if s.SignTTL > 0 {
		return s.SignTTL
	}
	return time.Hour
}

// listCacheTTL 列表缓存 TTL：正数生效，0 取默认 5s，负数禁用。
func (s *Server) listCacheTTL() time.Duration {
	if s.ListCacheTTL > 0 {
		return s.ListCacheTTL
	}
	if s.ListCacheTTL == 0 {
		return 5 * time.Second
	}
	return -1
}

// published 任务是否已对外发布（F-21：SUCCEEDED + 已上传 + 审批通过）。
// t3d-import 导入任务（M2-F07）：产物即本地任务工作目录（跳过内核），
// 分发本地优先，无需以 MinIO 上传为发布前提（后台冗余上传失败不影响分发）。
func (s *Server) published(t *task.Task) bool {
	if t.Status != task.StatusSucceeded || !t.Approved {
		return false
	}
	if t.Type == task.TypeT3dImport {
		return true
	}
	return t.MinioPrefix != ""
}

// signedTilesetURL 生成带防盗链签名的 tileset.json 分发地址。
func (s *Server) signedTilesetURL(taskID string) string {
	p := "/services/" + taskID + "/tileset.json"
	if !s.AuthEnabled || s.SignSecret == "" {
		return p
	}
	return p + "?" + auth.SignURL(p, time.Now().Add(s.signTTL()), s.SignSecret)
}

// ServeTile GET /services/:task_id/*path — 读取已发布产物。
// path 为任务产物目录内的相对路径（如 /tileset.json、/Data/0.b3dm）。
// 鉴权开启时校验 HMAC 防盗链签名（OPTIONS 预检除外）；未审批任务一律 404。
func (s *Server) ServeTile(c *gin.Context) {
	setCORS(c)
	if c.Request.Method == http.MethodOptions {
		c.Status(http.StatusNoContent)
		return
	}
	if s == nil || s.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}

	// 防盗链（F-06）：URL path + expires + sig 的 HMAC-SHA256 校验
	if s.AuthEnabled && s.SignSecret != "" {
		expires, _ := strconv.ParseInt(c.Query("expires"), 10, 64)
		if !auth.VerifySignature(c.Request.URL.Path, expires, c.Query("sig"), s.SignSecret) {
			c.JSON(http.StatusForbidden, gin.H{"error": "invalid or missing signature"})
			return
		}
	}

	t, err := s.Store.Get(c.Param("task_id"), "")
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "service not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 未审批发布（F-21）：仅 SUCCEEDED + 已上传 + 审批通过的任务对外提供
	if !s.published(t) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service not published"})
		return
	}

	rel, ok := safeRelPath(c.Param("path"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file path"})
		return
	}

	// JSON 产物（tileset.json 及嵌套）动态改写 content.uri 签名（F-06 补丁）：
	// Cesium 按改写后 URL（含 query）请求子瓦片/嵌套 tileset，验签通过。
	// 解析失败或非 tileset JSON 时返回 false，按普通文件原样分发。
	if s.rewriteEnabled() && strings.EqualFold(path.Ext(rel), ".json") {
		if s.serveSignedTilesetJSON(c, t, rel) {
			return
		}
	}

	// 1) 本地优先
	if f, err := os.Open(filepath.Join(t.Output, filepath.FromSlash(rel))); err == nil {
		defer f.Close()
		c.Header("Content-Type", objectstore.ContentType(rel))
		var modTime time.Time
		if st, serr := f.Stat(); serr == nil {
			modTime = st.ModTime()
		}
		http.ServeContent(c.Writer, c.Request, "", modTime, f)
		return
	}

	// 2) MinIO 流式回源
	if s.Objects != nil {
		key := path.Join(t.MinioPrefix, rel)
		rc, size, err := s.Objects.Get(c.Request.Context(), key)
		if err == nil {
			defer rc.Close()
			if size >= 0 {
				c.Header("Content-Length", strconv.FormatInt(size, 10))
			}
			c.Header("Content-Type", objectstore.ContentType(rel))
			c.Status(http.StatusOK)
			_, _ = io.Copy(c.Writer, rc)
			return
		}
	}

	c.Header("Content-Type", "application/json")
	c.JSON(http.StatusNotFound, gin.H{"error": "artifact not found"})
}

// safeRelPath 校验并规范化产物相对路径，拒绝目录穿越。
// 入参为 gin 通配符参数（带前导 /，URL 已解码，故 %2e%2e 会还原成 ..）。
// 返回以 / 分隔的干净相对路径；任何可疑形式返回 false。
func safeRelPath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if strings.ContainsRune(p, '\x00') || strings.ContainsRune(p, '\\') {
		return "", false
	}
	clean := path.Clean("/" + p) // 锚定根：消化掉 ..
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		// 允许目录本身？不：必须指向具体文件
		return "", false
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return clean, true
}

// setCORS 设置跨域响应头（Cesium 浏览器端跨域加载必需）。
func setCORS(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "GET, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Origin, Accept, Content-Type, Range")
	c.Header("Access-Control-Expose-Headers", "Content-Length, Content-Range")
}
