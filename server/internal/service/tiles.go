// tiles.go 实现 2D 影像瓦片服务分发（F-03）：WMTS 1.0.0（KVP + RESTful）与 TMS。
//
// 瓦片金字塔最小格式约定（内核侧后续对齐）：
//
//	{output}/tiles/metadata.json          金字塔元数据（TilePyramidMeta）
//	{output}/tiles/{z}/{x}/{y}.{ext}      瓦片文件，Y 轴原点在顶部（XYZ/slippy 约定，
//	                                      即 y=0 为最北行；TMS 协议分发时做 y 翻转）
//
// metadata.json 示例：
//
//	{"tile_matrix_set":"WebMercatorQuad","extent":[-20037508.34,-20037508.34,20037508.34,20037508.34],
//	 "min_zoom":0,"max_zoom":3,"tile_size":256,"format":"png"}
//
// 真实数据原则：GetCapabilities 完全由任务产物 metadata.json 生成，
// 无元数据的任务不产生 Layer（不硬编码假 XML）；GetTile 的瓦片必须真实存在于
// 本地 output 或 MinIO，缺失返回 404，绝不返回占位图。
//
// 缓存：瓦片响应经 cache.Cache（Redis，TANGIS_CACHE_TTL）；缓存不可用时
// 优雅降级直读存储，不因缓存故障 500。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/task"
)

// 瓦片金字塔目录/文件名约定。
const (
	tilesDirName = "tiles"
	metaFileName = "metadata.json"
)

// TilePyramidMeta 任务产物瓦片金字塔元数据（metadata.json schema）。
type TilePyramidMeta struct {
	TileMatrixSet string     `json:"tile_matrix_set"` // WebMercatorQuad / WorldCRS84Quad
	Layout        string     `json:"layout"`          // 磁盘布局：xyz = {z}/{x}/{y}（默认）；wmts = {z}/{row}/{col}
	Extent        [4]float64 `json:"extent"`          // [minx,miny,maxx,maxy]，CRS 单位
	MinZoom       int        `json:"min_zoom"`        // 最小层级（>=0）
	MaxZoom       int        `json:"max_zoom"`        // 最大层级（>= MinZoom）
	TileSize      int        `json:"tile_size"`       // 瓦片边长像素，0 取 256
	Format        string     `json:"format"`          // png / jpg
}

// tmsDef 内置 TileMatrixSet 定义（OGC 1.0.0 常用双精度基准）。
type tmsDef struct {
	URN       string                 // CRS URN
	TopLeft   [2]float64             // z0 瓦片左上角坐标
	BaseScale float64                // z0 ScaleDenominator（256px、0.28mm/px 基准）
	Matrix    func(z int) (w, h int) // 该层级矩阵列/行数
	// toWGS84 把 CRS 坐标转 WGS84 经纬度（Capabilities 的 WGS84BoundingBox 用）
	toWGS84 func(x, y float64) (lon, lat float64)
}

// 0.28mm/px + WebMercator 基准分母（OGC WMTS 1.0.0 标准 WebMercatorQuad）。
const (
	webMercatorBaseScale = 559082264.0287178
	crs84BaseScale       = 279541132.0143589
	webMercatorHalf      = 20037508.34278925
)

var tileMatrixSets = map[string]tmsDef{
	"WebMercatorQuad": {
		URN:       "urn:ogc:def:crs:EPSG::3857",
		TopLeft:   [2]float64{-webMercatorHalf, webMercatorHalf},
		BaseScale: webMercatorBaseScale,
		Matrix: func(z int) (int, int) {
			n := 1 << z
			return n, n
		},
		toWGS84: func(x, y float64) (float64, float64) {
			lon := x / webMercatorHalf * 180
			lat := (2*math.Atan(math.Exp(y/webMercatorHalf*math.Pi)) - math.Pi/2) * 180 / math.Pi
			return lon, lat
		},
	},
	"WorldCRS84Quad": {
		URN:       "urn:ogc:def:crs:OGC:1.3:CRS84",
		TopLeft:   [2]float64{-180, 90},
		BaseScale: crs84BaseScale,
		Matrix: func(z int) (int, int) {
			return 1 << (z + 1), 1 << z
		},
		toWGS84: func(x, y float64) (float64, float64) {
			return x, y // CRS84 即经纬度
		},
	},
}

// canonicalFormat 归一化瓦片格式（png/jpg/jpeg → png/jpg），非法返回空。
func canonicalFormat(f string) string {
	switch strings.ToLower(strings.TrimPrefix(f, "image/")) {
	case "png":
		return "png"
	case "jpg", "jpeg":
		return "jpg"
	default:
		return ""
	}
}

// imageContentType 瓦片格式的 HTTP Content-Type。
func imageContentType(format string) string {
	if format == "jpg" {
		return "image/jpeg"
	}
	return "image/png"
}

// LoadPyramidMeta 读取任务瓦片金字塔元数据：本地 output 优先，MinIO 回源。
// 无 metadata.json（本地与 MinIO 均无）返回 error——调用方按「无 2D 服务」处理。
func (s *Server) LoadPyramidMeta(t *task.Task) (*TilePyramidMeta, error) {
	data, err := os.ReadFile(filepath.Join(t.Output, tilesDirName, metaFileName))
	if err != nil && s.Objects != nil && t.MinioPrefix != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rc, size, gerr := s.Objects.Get(ctx, path.Join(t.MinioPrefix, tilesDirName, metaFileName))
		if gerr == nil {
			defer rc.Close()
			if size >= 0 && size <= 1<<20 {
				buf := make([]byte, size)
				if _, rerr := io.ReadFull(rc, buf); rerr == nil {
					data = buf
					err = nil
				}
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("tile pyramid metadata not found for task %s", t.ID)
	}
	var m TilePyramidMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse tile pyramid metadata for task %s: %w", t.ID, err)
	}
	return s.normalizeMeta(t.ID, &m)
}

// normalizeMeta 校验并补齐元数据默认值，非法字段明确报错（不静默造假）。
func (s *Server) normalizeMeta(taskID string, m *TilePyramidMeta) (*TilePyramidMeta, error) {
	if m.TileMatrixSet == "" {
		m.TileMatrixSet = "WebMercatorQuad"
	}
	if _, ok := tileMatrixSets[m.TileMatrixSet]; !ok {
		return nil, fmt.Errorf("task %s: unsupported tile_matrix_set %q", taskID, m.TileMatrixSet)
	}
	if m.Layout == "" {
		m.Layout = "xyz" // 历史产物无 layout 字段，按 XYZ 布局读取
	}
	if m.Layout != "xyz" && m.Layout != "wmts" {
		return nil, fmt.Errorf("task %s: unsupported layout %q", taskID, m.Layout)
	}
	if m.TileSize == 0 {
		m.TileSize = 256
	}
	if m.Format == "" {
		return nil, fmt.Errorf("task %s: metadata missing format", taskID)
	}
	if canonicalFormat(m.Format) == "" {
		return nil, fmt.Errorf("task %s: unsupported tile format %q", taskID, m.Format)
	}
	if m.MinZoom < 0 || m.MaxZoom < m.MinZoom || m.MaxZoom > 30 {
		return nil, fmt.Errorf("task %s: invalid zoom range [%d,%d]", taskID, m.MinZoom, m.MaxZoom)
	}
	return m, nil
}

// publishedImageTasks 已发布且带可读瓦片元数据的任务（GetCapabilities/Layer 来源）。
func (s *Server) publishedImageTasks(scope string) []*layerInfo {
	tasks, err := s.Store.List(scope)
	if err != nil {
		return nil
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.Before(tasks[j].CreatedAt) })
	out := make([]*layerInfo, 0, len(tasks))
	for _, t := range tasks {
		if !s.published(t) {
			continue
		}
		meta, merr := s.LoadPyramidMeta(t)
		if merr != nil {
			continue // 无元数据即无 2D 服务，不造假 Layer
		}
		out = append(out, &layerInfo{Task: t, Meta: meta})
	}
	return out
}

// layerInfo GetCapabilities 单 Layer 的真实数据来源。
type layerInfo struct {
	Task *task.Task
	Meta *TilePyramidMeta
}

// ---- 瓦片分发核心 ----

// pyramidTileKey Redis 缓存 key（含 tenant/task/z/x/y 与格式）。
func pyramidTileKey(t *task.Task, z, x, y int, format string) string {
	return fmt.Sprintf("tile:%s:%s:%d:%d:%d:%s", t.TenantID, t.ID, z, x, y, format)
}

// readPyramidTile 读取瓦片字节：本地 output 优先 → MinIO 回源。
// 入参 (z,x,y) 统一为 XYZ 逻辑坐标；物理路径按 meta.Layout：
// xyz = tiles/{z}/{x}/{y}.{format}；wmts = tiles/{z}/{row}/{col}.{format}（即 z/y/x）。
func (s *Server) readPyramidTile(t *task.Task, meta *TilePyramidMeta, z, x, y int, format string) ([]byte, error) {
	ext := strconv.Itoa(y) + "." + format
	if meta.Layout == "wmts" {
		ext = strconv.Itoa(x) + "." + format // WMTS REST 路径第 3 段是 TileCol（x）
	}
	rel := path.Join(tilesDirName, strconv.Itoa(z), strconv.Itoa(x), ext)
	if meta.Layout == "wmts" {
		rel = path.Join(tilesDirName, strconv.Itoa(z), strconv.Itoa(y), ext)
	}
	if data, err := os.ReadFile(filepath.Join(t.Output, filepath.FromSlash(rel))); err == nil {
		return data, nil
	}
	if s.Objects != nil && t.MinioPrefix != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rc, size, err := s.Objects.Get(ctx, path.Join(t.MinioPrefix, rel))
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		if size < 0 || size > 32<<20 {
			return nil, fmt.Errorf("tile %s: unexpected size %d", rel, size)
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(rc, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	return nil, os.ErrNotExist
}

// servePyramidTile WMTS/TMS 三种入口共用的瓦片分发：
// 校验元数据与坐标 → 缓存 → 本地/MinIO 读取 → 回填缓存。
// yIsTMS 为 true 时入参 y 是 TMS（原点左下）行号，内部转 XYZ（原点左上）。
func (s *Server) servePyramidTile(c *gin.Context, taskID, tmsName string, z, x, y int, format string, yIsTMS bool) {
	if s == nil || s.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}
	t, err := s.Store.Get(taskID, c.GetString(ScopeTenantKey))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "service not found"})
		return
	}
	if !s.published(t) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service not published"})
		return
	}
	meta, err := s.LoadPyramidMeta(t)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	format = canonicalFormat(format)
	if format == "" || format != canonicalFormat(meta.Format) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "format not available for this layer"})
		return
	}
	if tmsName != "" && tmsName != meta.TileMatrixSet {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown tile matrix set: " + tmsName})
		return
	}
	def := tileMatrixSets[meta.TileMatrixSet]
	if z < meta.MinZoom || z > meta.MaxZoom {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("tilematrix %d out of range [%d,%d]", z, meta.MinZoom, meta.MaxZoom)})
		return
	}
	if z < 0 || z > 30 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tilematrix"})
		return
	}
	mw, mh := def.Matrix(z)
	if x < 0 || x >= mw || y < 0 || y >= mh {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tile coordinates out of matrix bounds"})
		return
	}
	if yIsTMS {
		y = mh - 1 - y // TMS（左下原点）→ XYZ（左上原点）
	}

	setCORS(c)

	// 1) 缓存命中（Redis 不可用时按 miss 处理，绝不 500）
	if s.Cache != nil {
		if val, ok := s.Cache.Get(c.Request.Context(), pyramidTileKey(t, z, x, y, format)); ok {
			s.serveTileBytes(c, format, val)
			return
		}
	}

	// 2) 本地/MinIO 真实读取
	data, err := s.readPyramidTile(t, meta, z, x, y, format)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tile not found"})
		return
	}
	if s.Cache != nil {
		s.Cache.Set(c.Request.Context(), pyramidTileKey(t, z, x, y, format), data, s.cacheTTL())
	}
	s.serveTileBytes(c, format, data)
}

// serveTileBytes 以正确 Content-Type 与浏览器缓存头输出瓦片字节。
func (s *Server) serveTileBytes(c *gin.Context, format string, data []byte) {
	c.Header("Content-Type", imageContentType(format))
	if ttl := int(s.cacheTTL().Seconds()); ttl > 0 {
		c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d", ttl))
	}
	c.Data(http.StatusOK, imageContentType(format), data)
}

func (s *Server) cacheTTL() time.Duration {
	if s.CacheTTL > 0 {
		return s.CacheTTL
	}
	return 5 * time.Minute
}
