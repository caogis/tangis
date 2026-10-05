// filesource.go 文件矢量的注册入口与数据面路由。
//
// 两条注册路径并存，互不干扰：
//
//	PostGIS（服务端模式）  POST /vector/sources（DSN）+ /vector/layers（表名）
//	文件矢量（桌面单机版）  POST /vector/files（本地路径）—— 一步到位：
//	                       探测格式/几何类型/字段/范围 → 自动建数据源与图层
//
// 数据面由 RouterGateway 按 DSN scheme 分派：file:// → FileGateway，
// 其余（postgres://）→ pgGateway。桌面模式只装文件网关，服务端可两者共存。
package vector

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

/* ---------------- 数据面路由 ---------------- */

// RouterGateway 按 DSN scheme 把数据面请求分派给文件网关或 PostGIS 网关。
type RouterGateway struct {
	File *FileGateway
	PG   Gateway // 可为 nil（桌面模式无 PostGIS）
}

// NewRouterGateway 构造路由网关；pg 允许为 nil。
func NewRouterGateway(file *FileGateway, pg Gateway) *RouterGateway {
	if file == nil {
		file = NewFileGateway()
	}
	return &RouterGateway{File: file, PG: pg}
}

// pick 选择处理该 DSN 的网关。
func (g *RouterGateway) pick(dsn string) (Gateway, error) {
	if IsFileDSN(dsn) {
		return g.File, nil
	}
	if g.PG == nil {
		return nil, fmt.Errorf("%w: 当前部署未装配 PostGIS 数据源（桌面单机版请使用 file:// 本地矢量文件）", ErrInvalid)
	}
	return g.PG, nil
}

func (g *RouterGateway) Ping(ctx context.Context, dsn string) (string, error) {
	gw, err := g.pick(dsn)
	if err != nil {
		return "", err
	}
	return gw.Ping(ctx, dsn)
}

func (g *RouterGateway) DetectGeometry(ctx context.Context, dsn, schema, table, geomCol string) (string, int, string, error) {
	gw, err := g.pick(dsn)
	if err != nil {
		return "", 0, "", err
	}
	return gw.DetectGeometry(ctx, dsn, schema, table, geomCol)
}

func (g *RouterGateway) DetectFields(ctx context.Context, dsn, schema, table string, exclude ...string) ([]string, error) {
	gw, err := g.pick(dsn)
	if err != nil {
		return nil, err
	}
	return gw.DetectFields(ctx, dsn, schema, table, exclude...)
}

func (g *RouterGateway) DetectPrimaryKey(ctx context.Context, dsn, schema, table string) (string, error) {
	gw, err := g.pick(dsn)
	if err != nil {
		return "", err
	}
	return gw.DetectPrimaryKey(ctx, dsn, schema, table)
}

func (g *RouterGateway) TileMVT(ctx context.Context, dsn string, l *Layer, minx, miny, maxx, maxy float64, maxFeatures int) ([]byte, int, error) {
	gw, err := g.pick(dsn)
	if err != nil {
		return nil, 0, err
	}
	return gw.TileMVT(ctx, dsn, l, minx, miny, maxx, maxy, maxFeatures)
}

func (g *RouterGateway) EstimatedExtent(ctx context.Context, dsn string, l *Layer) (float64, float64, float64, float64, bool, error) {
	gw, err := g.pick(dsn)
	if err != nil {
		return 0, 0, 0, 0, false, err
	}
	return gw.EstimatedExtent(ctx, dsn, l)
}

func (g *RouterGateway) EstimatedRows(ctx context.Context, dsn string, l *Layer) (int64, error) {
	gw, err := g.pick(dsn)
	if err != nil {
		return 0, err
	}
	return gw.EstimatedRows(ctx, dsn, l)
}

func (g *RouterGateway) FeaturesGeoJSON(ctx context.Context, dsn string, l *Layer, hasBBox bool, minx, miny, maxx, maxy float64, limit int) ([]byte, int, error) {
	gw, err := g.pick(dsn)
	if err != nil {
		return nil, 0, err
	}
	return gw.FeaturesGeoJSON(ctx, dsn, l, hasBBox, minx, miny, maxx, maxy, limit)
}

// Snapshot 实现 DatasetSource：按 DSN 分派全量取数（导出用）。
//
// PostGIS 数据源暂不支持导出：那需要把任意 SRID 的几何连同坐标系定义
// 完整写进目标文件（含投影参数），而文件矢量链路只保证 4326/4490/3857
// 三种；与其导出一份坐标系可疑的数据，不如明确告知改用 WFS 取 GeoJSON。
func (g *RouterGateway) Snapshot(ctx context.Context, dsn string, l *Layer) ([]FileFeature, FileInfo, error) {
	if IsFileDSN(dsn) {
		return g.File.Snapshot(ctx, dsn, l)
	}
	if g.PG == nil {
		return nil, FileInfo{}, fmt.Errorf("%w: 当前部署未装配 PostGIS 数据源", ErrInvalid)
	}
	return nil, FileInfo{}, fmt.Errorf("%w: PostGIS 数据源暂不支持导出（可改用 WFS GetFeature 取 GeoJSON，或先落成文件矢量）", ErrInvalid)
}

// Close 关闭文件网关缓存（PostGIS 网关由装配侧单独关闭）。
func (g *RouterGateway) Close() {
	if g.File != nil {
		g.File.Close()
	}
}

/* ---------------- 文件矢量一步注册 ---------------- */

// RegFileReq 本地矢量文件注册请求。
type RegFileReq struct {
	// Path 本地矢量文件绝对路径（GeoJSON；Shapefile/GeoPackage 见文档）。
	Path string
	// Name 数据源与图层名；留空取文件名（去扩展名）。
	Name string
}

// RegisterFileSource 一步注册本地矢量文件为可分发图层。
//
// 与 PostGIS 的两步注册相比，文件矢量无需再挑表：**单图层格式**（GeoJSON /
// Shapefile）一个文件即一个图层；**多图层容器**（GeoPackage）则为文件内每个
// 要素表各注册一个图层。探测（几何列/类型/坐标系/字段）与建源建层合并为一次调用，
// 中途失败会回滚已建的数据源与图层，不留半成品。
func (s *Server) RegisterFileSource(ctx context.Context, req RegFileReq) (*Source, []*Layer, error) {
	path := strings.TrimSpace(req.Path)
	if path == "" {
		return nil, nil, fmt.Errorf("%w: path 不能为空", ErrInvalid)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: 路径不合法: %v", ErrInvalid, err)
	}
	if !SupportedFileExt(abs) {
		return nil, nil, fmt.Errorf("%w: 暂不支持的矢量格式 %q（当前支持 %s）",
			ErrInvalid, filepath.Ext(abs), SupportedFormats)
	}
	base := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = base
	}
	if !ValidIdentifier(name) {
		return nil, nil, fmt.Errorf("%w: 名称 %q 不合法（需字母/下划线开头，仅含字母数字下划线），请通过 name 参数指定", ErrInvalid, name)
	}

	// 容器格式先列出表清单（GeoPackage 可含多个要素表）
	type layerTarget struct {
		table string // 容器内表名（非容器格式为文件主干名，仅作记录）
		name  string // 图层名（进 URL，须过标识符白名单）
	}
	var targets []layerTarget
	if strings.EqualFold(filepath.Ext(abs), ".gpkg") {
		tabs, lerr := ListGeoPackageTables(abs)
		if lerr != nil {
			return nil, nil, lerr
		}
		if len(tabs) == 1 {
			targets = append(targets, layerTarget{table: tabs[0].Name, name: name})
		} else {
			seen := map[string]bool{}
			for i, t := range tabs {
				ln := sanitizeIdent(t.Name)
				if ln == "" || seen[ln] {
					ln = fmt.Sprintf("%s_t%d", name, i+1)
				}
				seen[ln] = true
				targets = append(targets, layerTarget{table: t.Name, name: ln})
			}
		}
	} else {
		targets = append(targets, layerTarget{table: base, name: name})
	}

	dsn := FileDSN(abs)
	// 连通性/可解析性测试（不通过直接失败，不注册死数据源）
	ver, err := s.Gateway.Ping(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}

	id, err := NewID()
	if err != nil {
		return nil, nil, err
	}
	src := &Source{ID: id, Name: name, DSN: dsn, PostGISVersion: ver}
	if err := s.Meta.CreateSource(ctx, src); err != nil {
		return nil, nil, err
	}
	// rollback 回滚已建的图层与数据源，避免半注册状态
	created := make([]string, 0, len(targets))
	rollback := func() {
		for _, ln := range created {
			_ = s.Meta.DeleteLayer(ctx, ln)
		}
		_ = s.Meta.DeleteSource(ctx, src.ID)
	}

	layers := make([]*Layer, 0, len(targets))
	for _, t := range targets {
		geomCol, srid, gtype, derr := s.Gateway.DetectGeometry(ctx, dsn, "", t.table, "")
		if derr != nil {
			rollback()
			return nil, nil, derr
		}
		fields, ferr := s.Gateway.DetectFields(ctx, dsn, "", t.table, geomCol)
		if ferr != nil {
			rollback()
			return nil, nil, ferr
		}
		layer := &Layer{
			Name:           t.name,
			SourceID:       src.ID,
			Schema:         "",
			Table:          t.table,
			GeometryColumn: geomCol,
			GeometryType:   gtype,
			SRID:           srid,
			IDColumn:       "",
			Fields:         fields,
		}
		if cerr := s.Meta.CreateLayer(ctx, layer); cerr != nil {
			rollback()
			return nil, nil, cerr
		}
		created = append(created, layer.Name)
		layers = append(layers, layer)
	}
	return src, layers, nil
}

// sanitizeIdent 把任意名字转成通过标识符白名单的形式（图层名要进 URL 路径）。
// 中文表名/含空格或点号的表名会被替换掉，全部非法时返回空串，由调用方改用序号命名。
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "t_" + out
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}
