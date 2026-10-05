// Server：矢量发布编排层——注册流程（连通性测试+自动探测）、
// MVT 瓦片生成（含 Redis 缓存）、图层元数据聚合。
package vector

import (
	"context"
	"fmt"
	"math"
	"time"

	"tangis/server/internal/cache"
)

// DefaultMaxFeatures 单 tile 要素数上限默认值（可配 TANGIS_VECTOR_MAX_FEATURES）。
const DefaultMaxFeatures = 100000

// DefaultCacheTTL 瓦片缓存 TTL 默认值（可配 TANGIS_VECTOR_CACHE_TTL）。
const DefaultCacheTTL = 5 * time.Minute

// Server 矢量发布服务。Meta/Gateway 必须非 nil；Cache 允许为 nil（禁用缓存）。
type Server struct {
	Meta        MetaStore
	Gateway     Gateway
	Cache       cache.Cache
	CacheTTL    time.Duration
	MaxFeatures int
	// WFSMax WFS GetFeature 单次要求数上限（env TANGIS_WFS_MAX_FEATURES，
	// M2-F14）；零值取 DefaultWFSMaxFeatures。
	WFSMax int
}

// RegSourceReq 数据源注册请求（API 层转交）。
type RegSourceReq struct {
	Name     string
	DSN      string // 二选一：直接给 DSN
	Host     string // 或给连接参数（密码按项目现状风格入库）
	Port     int
	User     string
	Password string
	DBName   string
	SSLMode  string
}

// BuildDSN 由连接参数拼 DSN；req.DSN 非空则直接用。
func (r *RegSourceReq) BuildDSN() (string, error) {
	if r.DSN != "" {
		return r.DSN, nil
	}
	if r.Host == "" || r.DBName == "" || r.User == "" {
		return "", fmt.Errorf("%w: source requires dsn or host/port/user/dbname", ErrInvalid)
	}
	if r.Port <= 0 {
		r.Port = 5432
	}
	ssl := r.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		r.User, r.Password, r.Host, r.Port, r.DBName, ssl), nil
}

// RegisterSource 注册数据源：拼 DSN → 连通性测试（ping + PostGIS 版本）
// → 落元数据。连不上直接失败（不静默注册死数据源）。
func (s *Server) RegisterSource(ctx context.Context, req *RegSourceReq) (*Source, error) {
	dsn, err := req.BuildDSN()
	if err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, fmt.Errorf("%w: source name required", ErrInvalid)
	}
	ver, err := s.Gateway.Ping(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("source unreachable: %w", err)
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	src := &Source{ID: id, Name: req.Name, DSN: dsn, PostGISVersion: ver}
	if err := s.Meta.CreateSource(ctx, src); err != nil {
		return nil, err
	}
	return src, nil
}

// RegLayerReq 图层注册请求。
type RegLayerReq struct {
	Name           string
	SourceID       string
	Schema         string
	Table          string
	GeometryColumn string   // 空则自动探测
	SRID           int      // 0 则取 geometry_columns 探测值
	IDColumn       string   // 空则取主键
	Fields         []string // 空则自动探测全部普通列
}

// RegisterLayer 注册图层：源存在性 → 标识符白名单校验 → 几何列/srid/主键/
// 字段自动探测 → 落元数据。探测不到表/几何列返回 ErrInvalid。
func (s *Server) RegisterLayer(ctx context.Context, req *RegLayerReq) (*Layer, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("%w: layer name required", ErrInvalid)
	}
	if err := validateIdentifierField("schema", req.Schema); err != nil {
		return nil, err
	}
	if err := validateIdentifierField("table", req.Table); err != nil {
		return nil, err
	}
	src, err := s.Meta.GetSource(ctx, req.SourceID)
	if err != nil {
		return nil, err
	}
	dsn := src.DSN

	geomCol, srid, gtype, err := s.Gateway.DetectGeometry(ctx, dsn, req.Schema, req.Table, req.GeometryColumn)
	if err != nil {
		return nil, err
	}
	if req.SRID > 0 {
		srid = req.SRID // 显式覆盖（geometry_columns 视图信息滞后时）
	}

	idCol := req.IDColumn
	if idCol == "" {
		idCol, err = s.Gateway.DetectPrimaryKey(ctx, dsn, req.Schema, req.Table)
		if err != nil {
			return nil, err
		}
	}
	if err := validateIdentifierField("id_column", idCol); err != nil {
		return nil, err
	}

	fields := req.Fields
	if len(fields) == 0 {
		fields, err = s.Gateway.DetectFields(ctx, dsn, req.Schema, req.Table, geomCol, idCol)
		if err != nil {
			return nil, err
		}
	}
	for _, f := range fields {
		if err := validateIdentifierField("field", f); err != nil {
			return nil, err
		}
		if f == geomCol || f == idCol {
			return nil, fmt.Errorf("%w: field %q duplicates geometry/id column", ErrInvalid, f)
		}
	}

	l := &Layer{
		Name:           req.Name,
		SourceID:       req.SourceID,
		Schema:         req.Schema,
		Table:          req.Table,
		GeometryColumn: geomCol,
		GeometryType:   gtype,
		SRID:           srid,
		IDColumn:       idCol,
		Fields:         fields,
	}
	if err := s.Meta.CreateLayer(ctx, l); err != nil {
		return nil, err
	}
	return l, nil
}

// validateIdentifierField 标识符白名单校验（空值报错）。
func validateIdentifierField(kind, v string) error {
	if !ValidIdentifier(v) {
		return fmt.Errorf("%w: %s %q fails identifier whitelist %s", ErrInvalid, kind, v, identifierRe)
	}
	return nil
}

// ErrLayerNotFound / ErrSourceNotFound 供 API 层映射 404。
var (
	ErrLayerNotFound  = ErrNotFound
	ErrSourceNotFound = ErrNotFound
)

// Tile 生成一瓦片 MVT：图层解析 → 缓存 → PostGIS 查询 → 回填缓存。
// 返回 (mvt 字节, 要素数, 错误)。mvt 为 nil 表示空 tile（零要素）。
// 错误可能为 ErrLayerNotFound / ErrTooManyFeatures / 数据面错误。
func (s *Server) Tile(ctx context.Context, layerName string, z, x, y int) ([]byte, int, error) {
	if !ValidTile(z, x, y) {
		return nil, 0, fmt.Errorf("%w: tile z/x/y out of range", ErrInvalid)
	}
	l, err := s.Meta.GetLayer(ctx, layerName)
	if err != nil {
		return nil, 0, err
	}
	key := fmt.Sprintf("tile:vector:%s:%d:%d:%d", l.Name, z, x, y)
	if s.Cache != nil {
		if v, ok := s.Cache.Get(ctx, key); ok {
			return v, -1, nil // -1：缓存命中，要素数未知
		}
	}
	src, err := s.Meta.GetSource(ctx, l.SourceID)
	if err != nil {
		return nil, 0, err
	}
	minx, miny, maxx, maxy := TileBBox(z, x, y)
	maxf := s.MaxFeatures
	if maxf <= 0 {
		maxf = DefaultMaxFeatures
	}
	mvt, count, err := s.Gateway.TileMVT(ctx, src.DSN, l, minx, miny, maxx, maxy, maxf)
	if err != nil {
		return nil, 0, err
	}
	if len(mvt) == 0 {
		return nil, count, nil // 空 tile：204（见 docs/VECTOR-API.md 策略）
	}
	if s.Cache != nil && s.CacheTTL > 0 {
		s.Cache.Set(ctx, key, mvt, s.CacheTTL)
	}
	return mvt, count, nil
}

// Metadata 图层元数据聚合：bbox（ST_EstimatedExtent → WGS84）、字段、
// 要素数估算、minzoom/maxzoom 建议。
type Metadata struct {
	Layer
	// BboxWGS84 WGS84 (EPSG:4326) 包围盒 [minx, miny, maxx, maxy]；
	// 统计信息缺失时为 nil（图层从未 VACUUM/ANALYZE，ST_EstimatedExtent 无值）。
	BboxWGS84 []float64 `json:"bbox_wgs84"`
	// FeaturesEstimated 要素数估算（pg_class.reltuples，可能为 0=未知）。
	FeaturesEstimated int64 `json:"features_estimated"`
	// MinZoom/MaxZoom 建议值（启发式，见 SuggestZooms）。
	MinZoom int `json:"minzoom"`
	MaxZoom int `json:"maxzoom"`
}

// Metadata 聚合图层元数据。DSN 来自注册的数据源。
func (s *Server) Metadata(ctx context.Context, layerName string) (*Metadata, error) {
	l, err := s.Meta.GetLayer(ctx, layerName)
	if err != nil {
		return nil, err
	}
	src, err := s.Meta.GetSource(ctx, l.SourceID)
	if err != nil {
		return nil, err
	}
	md := &Metadata{Layer: *l}

	if minx, miny, maxx, maxy, ok, err := s.Gateway.EstimatedExtent(ctx, src.DSN, l); err == nil && ok {
		md.BboxWGS84 = TransformBBoxToWGS84(minx, miny, maxx, maxy, l.SRID)
		md.MinZoom, md.MaxZoom = SuggestZooms(md.BboxWGS84)
	}
	if n, err := s.Gateway.EstimatedRows(ctx, src.DSN, l); err == nil {
		md.FeaturesEstimated = n
	}
	return md, nil
}

// TransformBBoxToWGS84 把图层原生 SRID 的 bbox 转 WGS84（经纬度）。
// 仅支持 3857（解析式换算）与 4326（恒等）；其他 SRID 返回原值并保持 srid
// 语义（元数据接口同时返回 srid，消费方自行判断）——已知限制见文档。
func TransformBBoxToWGS84(minx, miny, maxx, maxy float64, srid int) []float64 {
	switch srid {
	case 4326:
		return []float64{minx, miny, maxx, maxy}
	case 4490:
		// CGCS2000 地理坐标：单位同为度，与 WGS84 差在厘米级，数值可直接使用。
		// 显式列出而不是靠兜底分支，避免将来"整理兜底逻辑"时把它误改。
		return []float64{minx, miny, maxx, maxy}
	case 3857:
		return []float64{
			mercXToLon(minx), mercYToLat(miny), mercXToLon(maxx), mercYToLat(maxy),
		}
	}
	// 其他 SRID：原样返回（调用方以 srid 字段为准）
	return []float64{minx, miny, maxx, maxy}
}

func mercXToLon(x float64) float64 { return x / 20037508.342789244 * 180 }

func mercYToLat(y float64) float64 {
	return math.Atan(math.Sinh(y/20037508.342789244*math.Pi)) * 180 / math.Pi
}

// SuggestZooms 由 WGS84 bbox 宽度启发式建议 minzoom/maxzoom：
// minzoom = 整个图层恰好铺满约 1 个瓦片所需的最低 zoom（减 1 留裕量）；
// maxzoom = minzoom + 8，夹在 [10, 16]（地块级细节足够，可按需覆盖）。
// 纯启发式，仅供前端初始化相机参考。
func SuggestZooms(bboxWGS84 []float64) (minZoom, maxZoom int) {
	const defMin, defMax = 0, 14
	if len(bboxWGS84) != 4 {
		return defMin, defMax
	}
	w := math.Abs(bboxWGS84[2] - bboxWGS84[0])
	h := math.Abs(bboxWGS84[3] - bboxWGS84[1])
	span := math.Max(w, h)
	if span <= 0 {
		return defMin, defMax
	}
	// 需要 360/span * 2^z >= 1 个瓦片跨度
	z := math.Ceil(math.Log2(360/span)) - 1
	minZoom = int(math.Max(0, math.Min(z, MaxZoomCap)))
	maxZoom = minZoom + 8
	if maxZoom < 10 {
		maxZoom = 10
	}
	if maxZoom > 16 {
		maxZoom = 16
	}
	return minZoom, maxZoom
}

// Close 释放网关持有的连接池（PG 网关时）。
func (s *Server) Close() {
	if c, ok := s.Gateway.(interface{ Close() }); ok {
		c.Close()
	}
}
