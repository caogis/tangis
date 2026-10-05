// vector_handlers.go — 矢量发布 API（M2-F10a，PRD F-10）。
// 管理接口（数据源/图层注册）走 /api/v1 组的 X-API-Key 鉴权；
// MVT 分发走 tile 组的「API Key 或签名 URL」鉴权（与 WMTS/TMS 同语义）。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/vector"
)

// CreateVectorSource POST /api/v1/vector/sources — 注册 PostGIS 数据源。
// 请求体：{name, dsn} 或 {name, host, port, user, password, dbname, sslmode}。
// 注册时做连通性测试（ping + PostGIS 版本），不可达直接拒绝（502）。
func (h *Handler) CreateVectorSource(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	var req struct {
		Name     string `json:"name" binding:"required"`
		DSN      string `json:"dsn"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		DBName   string `json:"dbname"`
		SSLMode  string `json:"sslmode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src, err := h.Vector.RegisterSource(c.Request.Context(), &vector.RegSourceReq{
		Name: req.Name, DSN: req.DSN,
		Host: req.Host, Port: req.Port, User: req.User,
		Password: req.Password, DBName: req.DBName, SSLMode: req.SSLMode,
	})
	if err != nil {
		writeVectorErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":              src.ID,
		"name":            src.Name,
		"dsn":             src.MaskedDSN(),
		"postgis_version": src.PostGISVersion,
		"created_at":      src.CreatedAt,
	})
}

// CreateVectorFileSource POST /api/v1/vector/files — 一步注册本地矢量文件。
//
// 请求体：{path, name?}；name 缺省取文件名（去扩展名）。
// 与 PostGIS 的两步注册相比，文件矢量「一个文件即一个图层」，因此把
// 连通性测试 + 几何/字段/范围探测 + 建源 + 建图层合并为一次调用。
// 返回的图层名可直接用于 MVT 分发（/api/v1/vector/{layer}/{z}/{x}/{y}）
// 与 WFS GetFeature。
func (h *Handler) CreateVectorFileSource(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	var req struct {
		Path string `json:"path" binding:"required"`
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src, layers, err := h.Vector.RegisterFileSource(c.Request.Context(),
		vector.RegFileReq{Path: req.Path, Name: req.Name})
	if err != nil {
		writeVectorErr(c, err)
		return
	}
	ls := make([]gin.H, 0, len(layers))
	for _, l := range layers {
		ls = append(ls, gin.H{
			"name": l.Name, "source_id": l.SourceID, "geometry_type": l.GeometryType,
			"srid": l.SRID, "fields": l.Fields, "created_at": l.CreatedAt,
		})
	}
	c.JSON(http.StatusCreated, gin.H{
		"source": gin.H{
			"id": src.ID, "name": src.Name, "dsn": src.MaskedDSN(),
			// 文件数据源没有 PostGIS 版本，此字段承载「格式 + 要素数」摘要
			"format": src.PostGISVersion, "created_at": src.CreatedAt,
		},
		"layers": ls,
	})
}

// ExportVectorLayer GET /api/v1/vector/layers/{name}/export?format=geojson|gpkg|shp
//
// 把图层回流成其它 GIS 软件能直接打开的文件：
//
//	geojson  单个 .geojson
//	gpkg     单个 .gpkg（GeoPackage，SQLite）
//	shp      .zip 包含 .shp/.shx/.dbf/.prj/.cpg（Shapefile 是文件族）
//
// 有损处理（Shapefile 的混合几何类型、字段名截断等）通过响应头回传：
// X-Tangis-Features / X-Tangis-Skipped / X-Tangis-Warnings（URL 编码，逗号分隔），
// 前端据此提示用户，避免"导出成功了但少了一堆要素"却不知原因。
func (h *Handler) ExportVectorLayer(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	format, err := vector.ParseExportFormat(c.Query("format"))
	if err != nil {
		writeVectorErr(c, err)
		return
	}
	res, err := h.Vector.ExportLayer(c.Request.Context(), c.Param("name"), format)
	if err != nil {
		writeVectorErr(c, err)
		return
	}
	// GeoPackage 走临时文件，响应写完即清理
	if res.TempDir != "" {
		defer res.TempDirCleanup()
	}

	c.Header("X-Tangis-Features", strconv.Itoa(res.Features))
	if res.Skipped > 0 {
		c.Header("X-Tangis-Skipped", strconv.Itoa(res.Skipped))
	}
	if len(res.Warnings) > 0 {
		// HTTP 头只能是 ASCII：中文提示按 URL 编码传递，前端解码后展示
		encoded := make([]string, 0, len(res.Warnings))
		for _, w := range res.Warnings {
			encoded = append(encoded, url.QueryEscape(w))
		}
		c.Header("X-Tangis-Warnings", strings.Join(encoded, ","))
	}

	if res.Path != "" {
		// 先显式声明类型：http.ServeContent 只在未设置时才嗅探，
		// 否则 .gpkg 会被识别成 application/octet-stream
		c.Header("Content-Type", res.MimeType)
		c.FileAttachment(res.Path, res.Filename)
		return
	}
	c.Header("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", res.Filename))
	c.Data(http.StatusOK, res.MimeType, res.Data)
}

// ApplyVectorEdits POST /api/v1/vector/layers/{name}/edits — 图层要素增删改（就地写回）。
//
// 请求体：{ops:[{op:"create"|"update"|"delete", id?, geometry?, properties?},…]}
//
// 写回策略按源格式自动选择：GeoJSON 整体重写、GeoPackage 只改目标表（同库其它表
// 不受影响）、Shapefile 重写文件族。**首次编辑前自动备份源文件**（`<文件名>.orig`），
// 备份路径随结果返回。几何类型不符（如往面图层画点）会被明确拒绝。
func (h *Handler) ApplyVectorEdits(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	var req struct {
		Ops []vector.EditOp `json:"ops"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body: " + err.Error()})
		return
	}
	res, err := h.Vector.ApplyEdits(c.Request.Context(), c.Param("name"), req.Ops)
	if err != nil {
		writeVectorErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// ListVectorSources GET /api/v1/vector/sources — 数据源列表（DSN 打码回显）。
func (h *Handler) ListVectorSources(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	srcs, err := h.Vector.Meta.ListSources(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, gin.H{
			"id": s.ID, "name": s.Name, "dsn": s.MaskedDSN(),
			"postgis_version": s.PostGISVersion, "created_at": s.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"sources": out})
}

// DeleteVectorSource DELETE /api/v1/vector/sources/:id — 删除数据源。
// 仍被图层引用时拒绝（409）。
func (h *Handler) DeleteVectorSource(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	n, err := h.Vector.Meta.CountLayersBySource(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "source still referenced by layers", "layers": n})
		return
	}
	if err := h.Vector.Meta.DeleteSource(ctx, id); err != nil {
		writeVectorErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// CreateVectorLayer POST /api/v1/vector/layers — 注册图层。
// 请求体：{name, source_id, schema, table, geometry_column?, srid?, id_column?, fields?}。
// geometry_column/srid/id_column/fields 缺省时自动探测
// （geometry_columns / 显式 SRID 覆盖 / 主键 / information_schema）。
func (h *Handler) CreateVectorLayer(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	var req struct {
		Name           string   `json:"name" binding:"required"`
		SourceID       string   `json:"source_id" binding:"required"`
		Schema         string   `json:"schema" binding:"required"`
		Table          string   `json:"table" binding:"required"`
		GeometryColumn string   `json:"geometry_column"`
		SRID           int      `json:"srid"`
		IDColumn       string   `json:"id_column"`
		Fields         []string `json:"fields"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	l, err := h.Vector.RegisterLayer(c.Request.Context(), &vector.RegLayerReq{
		Name: req.Name, SourceID: req.SourceID, Schema: req.Schema, Table: req.Table,
		GeometryColumn: req.GeometryColumn, SRID: req.SRID,
		IDColumn: req.IDColumn, Fields: req.Fields,
	})
	if err != nil {
		writeVectorErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, l)
}

// ListVectorLayers GET /api/v1/vector/layers — 图层列表。
func (h *Handler) ListVectorLayers(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	layers, err := h.Vector.Meta.ListLayers(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"layers": layers})
}

// VectorLayerMetadata GET /api/v1/vector/layers/:name/metadata — 图层元数据：
// bbox（ST_EstimatedExtent → WGS84）、字段、要素数估算、minzoom/maxzoom 建议。
func (h *Handler) VectorLayerMetadata(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	md, err := h.Vector.Metadata(c.Request.Context(), c.Param("name"))
	if err != nil {
		writeVectorErr(c, err)
		return
	}
	c.JSON(http.StatusOK, md)
}

// DeleteVectorLayer DELETE /api/v1/vector/layers/:name — 删除图层。
func (h *Handler) DeleteVectorLayer(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	if err := h.Vector.Meta.DeleteLayer(c.Request.Context(), c.Param("name")); err != nil {
		writeVectorErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// VectorTile GET /api/v1/vector/:layer/:z/:x/:y[.pbf] — MVT 分发（F-10）。
// 鉴权：API Key 或签名 URL（路由层 tileAuthMiddleware）；Redis 缓存
// key 含 layer/z/x/y（TTL TANGIS_VECTOR_CACHE_TTL）。
// 空 tile → 204 No Content；超要素上限 → 413（策略见 docs/VECTOR-API.md）。
func (h *Handler) VectorTile(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	// 文件名后缀 .pbf 可选（客户端习惯 .../12/3361/1671.pbf）
	yName := c.Param("y")
	if dot := strings.LastIndexByte(yName, '.'); dot >= 0 {
		if yName[dot:] != ".pbf" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "only .pbf extension supported"})
			return
		}
		yName = yName[:dot]
	}
	z, err1 := strconv.Atoi(c.Param("z"))
	x, err2 := strconv.Atoi(c.Param("x"))
	y, err3 := strconv.Atoi(yName)
	if err1 != nil || err2 != nil || err3 != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "z/x/y must be integers"})
		return
	}

	mvt, count, err := h.Vector.Tile(c.Request.Context(), c.Param("layer"), z, x, y)
	if err != nil {
		writeVectorErr(c, err)
		return
	}
	if count < 0 {
		c.Header("X-Cache", "HIT") // 缓存命中（要素数未知）
	} else {
		c.Header("X-Cache", "MISS")
		c.Header("X-Tangis-Feature-Count", strconv.Itoa(count))
	}
	if mvt == nil {
		c.Status(http.StatusNoContent) // 空 tile：无要素
		return
	}
	c.Data(http.StatusOK, "application/vnd.mapbox-vector-tile", mvt)
}

// writeVectorErr vector 错误 → HTTP 状态映射：
// ErrInvalid → 400；ErrNotFound → 404；ErrConflict → 409；
// ErrTooManyFeatures → 413；网关/数据面错误 → 502（上游 PostGIS 故障）。
func writeVectorErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, vector.ErrInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, vector.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, vector.ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, vector.ErrTooManyFeatures):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": err.Error() + " (increase zoom level or raise TANGIS_VECTOR_MAX_FEATURES)",
		})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	}
}
