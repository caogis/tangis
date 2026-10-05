// filegateway.go 文件矢量数据面：Gateway 的本地文件实现。
//
// 定位：PostGIS 路线把「投影 + 裁剪 + MVT 编码」交给数据库（ST_Transform /
// ST_AsMVTGeom / ST_AsMVT）；桌面单机版没有数据库，本文件用纯 Go 补齐同一条链路：
//
//	读文件（GeoJSON / Shapefile / GeoPackage）
//	  → 加载进内存（要素 + 墨卡托包围盒 + 属性）
//	  → 瓦片查询：bbox 粗筛 → clipGeometry 投影/裁剪/绕向归一化 → EncodeMVT
//
// DSN 约定：`file:///abs/path/to/data.geojson`（file:// 前缀 + 绝对路径）。
// 与 pgGateway 由路由网关按 scheme 分派，二者可共存（服务端模式）。
//
// 已知限制（诚实标注，不假装支持）：
//   - 数据整体载入内存，超大文件（>数百万要素）需换分块索引方案；
//   - 只支持 EPSG:4326 / EPSG:3857 两种源坐标系，其他 SRID 明确报错。
package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileScheme 文件矢量 DSN 前缀。
const FileScheme = "file://"

// FileGateway Gateway 的本地文件实现（含解析缓存）。
type FileGateway struct {
	mu    sync.Mutex
	cache map[string]*fileDataset
}

// NewFileGateway 构造文件矢量网关。
func NewFileGateway() *FileGateway {
	return &FileGateway{cache: map[string]*fileDataset{}}
}

// fileDataset 已加载的文件矢量数据集。
type fileDataset struct {
	path     string
	info     FileInfo
	features []FileFeature
	// mercBounds 与 features 平行：Web Mercator 下的包围盒，用于瓦片粗筛。
	mercBounds []Box
	modTime    time.Time
	size       int64
}

// FilePathOf 从 file:// DSN 解析本地绝对路径。
func FilePathOf(dsn string) (string, bool) {
	if !strings.HasPrefix(dsn, FileScheme) {
		return "", false
	}
	p := strings.TrimPrefix(dsn, FileScheme)
	if p == "" {
		return "", false
	}
	// file:///abs → /abs；也容忍 file://abs 的写法（补前导斜杠）
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return filepath.Clean(p), true
}

// IsFileDSN 报告 DSN 是否为文件矢量。
func IsFileDSN(dsn string) bool {
	_, ok := FilePathOf(dsn)
	return ok
}

// FileDSN 由本地路径构造 file:// DSN。
func FileDSN(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return FileScheme + filepath.ToSlash(abs)
}

// SupportedFileExt 报告扩展名是否受支持（用于注册时的友好报错）。
//
// Shapefile 只需给 .shp 路径：.dbf/.prj/.cpg 由解析器按同名规则自动关联。
func SupportedFileExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".geojson", ".json", ".shp", ".gpkg":
		return true
	}
	return false
}

// SupportedFormats 支持的扩展名清单（拼错误提示用）。
const SupportedFormats = ".geojson / .json / .shp / .gpkg"

// load 读取并缓存数据集（按 mtime+size 判断失效）。
//
// table 仅对多图层容器（GeoPackage）有意义：同一文件的不同要素表是不同数据集，
// 因此参与缓存键。其余格式传空串。
func (g *FileGateway) load(path, table string) (*fileDataset, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: 文件不存在或不可读: %s", ErrInvalid, path)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%w: 目录不受支持（请指定矢量文件）: %s", ErrInvalid, path)
	}

	// 只有多图层容器（GeoPackage）才按表分键；单图层格式忽略 table，
	// 使同一文件被注册成多个图层名时仍共用一份内存数据集。
	key := path
	if table != "" && strings.EqualFold(filepath.Ext(path), ".gpkg") {
		key = path + "\x00" + table
	}
	g.mu.Lock()
	if ds, ok := g.cache[key]; ok && ds.modTime.Equal(st.ModTime()) && ds.size == st.Size() {
		g.mu.Unlock()
		return ds, nil
	}
	g.mu.Unlock()

	if !SupportedFileExt(path) {
		return nil, fmt.Errorf("%w: 暂不支持的矢量格式 %q（当前支持 %s）",
			ErrInvalid, filepath.Ext(path), SupportedFormats)
	}
	// 按扩展名分派解析器。注意：Shapefile 是文件族（.shp/.dbf/.prj/.cpg），
	// 这里以 .shp 为主键；同级文件变更不会让缓存失效（缓存由 .shp 的
	// mtime+size 决定），重新导出属性表后重注册一次即可。
	var (
		feats []FileFeature
		info  FileInfo
	)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".geojson", ".json":
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("%w: 读取失败: %v", ErrInvalid, rerr)
		}
		feats, info, err = ParseGeoJSON(data)
	case ".shp":
		feats, info, err = ParseShapefile(path)
	case ".gpkg":
		feats, info, err = ReadGeoPackageTable(path, table, "")
	default:
		return nil, fmt.Errorf("%w: 不支持的格式 %q", ErrInvalid, filepath.Ext(path))
	}
	if err != nil {
		return nil, err
	}
	// 源坐标系白名单与各解析器保持一致（supportedSRID：4326/4490/3857）。
	// 曾在这里留着一份"只认 4326/3857"的旧白名单，把 Shapefile 阶段新增的
	// 4490(CGCS2000) 挡在门外（E2E 才暴露）——现在统一走同一个判据。
	if !supportedSRID(info.SRID) {
		return nil, unsupportedCRSErr(info.SRID, "")
	}
	ds := &fileDataset{
		path:       path,
		info:       info,
		features:   feats,
		mercBounds: make([]Box, len(feats)),
		modTime:    st.ModTime(),
		size:       st.Size(),
	}
	for i := range feats {
		ds.mercBounds[i] = projectGeometry(feats[i].Geom, info.SRID).Bounds()
	}

	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[string]*fileDataset{}
	}
	g.cache[key] = ds
	g.mu.Unlock()
	return ds, nil
}

// tableOf 图层在容器内的表名：GeoPackage 为表名，其余格式为文件主干名
// （由 load 忽略，只为统一调用签名）。
func tableOf(l *Layer) string {
	if l == nil {
		return ""
	}
	return l.Table
}

// dataset 按 DSN 取数据集（多图层容器需给出 table）。
func (g *FileGateway) dataset(dsn, table string) (*fileDataset, error) {
	p, ok := FilePathOf(dsn)
	if !ok {
		return nil, fmt.Errorf("%w: 非文件数据源 DSN: %s", ErrInvalid, dsn)
	}
	return g.load(p, table)
}

/* ---------------- Gateway 实现 ---------------- */

// Ping 校验文件可读且可解析，返回格式描述（替代 PostGIS 版本串）。
func (g *FileGateway) Ping(_ context.Context, dsn string) (string, error) {
	p, ok := FilePathOf(dsn)
	if !ok {
		return "", fmt.Errorf("%w: 非文件数据源 DSN: %s", ErrInvalid, dsn)
	}
	// GeoPackage 可能含多个要素表：连通性测试只校验文件合法并列表，
	// 不把整库加载进内存（那是各图层自己的事）。
	if strings.EqualFold(filepath.Ext(p), ".gpkg") {
		tabs, err := ListGeoPackageTables(p)
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(tabs))
		for _, t := range tabs {
			names = append(names, t.Name)
		}
		return fmt.Sprintf("file-vector/gpkg（%d 个要素表：%s）", len(tabs), strings.Join(names, "、")), nil
	}
	ds, err := g.dataset(dsn, "")
	if err != nil {
		return "", err
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(ds.path)), ".")
	summary := fmt.Sprintf("file-vector/%s（%d 要素，SRID:%d）", ext, ds.info.Count, ds.info.SRID)
	// 解析降级提示（缺 .dbf、编码回退、环角色按包含关系判定等）一并回显，
	// 用户从注册结果就能看出数据是怎么被理解的，而不是遇到怪现象再回头查。
	if len(ds.info.Notes) > 0 {
		summary += "｜" + strings.Join(ds.info.Notes, "；")
	}
	return summary, nil
}

// DetectGeometry 探测几何列信息。
func (g *FileGateway) DetectGeometry(_ context.Context, dsn, _, table, geomCol string) (string, int, string, error) {
	// GeoPackage：几何列/类型/坐标系直接来自 gpkg_geometry_columns，
	// 不必为探测而把整表读进内存（大库尤其重要）。
	if p, ok := FilePathOf(dsn); ok && strings.EqualFold(filepath.Ext(p), ".gpkg") {
		tabs, err := ListGeoPackageTables(p)
		if err != nil {
			return "", 0, "", err
		}
		meta := pickTable(tabs, table)
		if meta == nil {
			return "", 0, "", fmt.Errorf("%w: GeoPackage 里没有要素表 %q", ErrInvalid, table)
		}
		if geomCol != "" && geomCol != meta.GeometryCol {
			return "", 0, "", fmt.Errorf("%w: 表 %q 的几何列是 %q，不是 %q",
				ErrInvalid, table, meta.GeometryCol, geomCol)
		}
		// 与 ReadGeoPackageTable 用同一套坐标系判定：投影坐标在读取时已换算成
		// WGS84，图层元数据必须报**换算后**的 SRID，否则下游按"米"解释经纬度会错位。
		tr, terr := geoPackageCRSTransform(p, meta.SRID)
		if terr != nil {
			return "", 0, "", terr
		}
		return meta.GeometryCol, tr.effectiveSRID(), normalizeGeomTypeName(meta.GeomType), nil
	}
	ds, err := g.dataset(dsn, table)
	if err != nil {
		return "", 0, "", err
	}
	if ds.info.Count == 0 {
		return "", 0, "", fmt.Errorf("%w: 文件里没有有效要素", ErrInvalid)
	}
	if geomCol != "" && geomCol != "geometry" {
		return "", 0, "", fmt.Errorf("%w: 文件矢量没有几何列 %q（固定为 geometry）", ErrInvalid, geomCol)
	}
	return "geometry", ds.info.SRID, geomTypeName(ds.info.GeomType), nil
}

// DetectFields 探测属性字段（文件矢量的字段来自要素属性并集）。
func (g *FileGateway) DetectFields(_ context.Context, dsn, _, table string, exclude ...string) ([]string, error) {
	ds, err := g.dataset(dsn, table)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{"geometry": true}
	for _, e := range exclude {
		skip[e] = true
	}
	out := make([]string, 0, len(ds.info.Fields))
	for _, f := range ds.info.Fields {
		if !skip[f] {
			out = append(out, f)
		}
	}
	return out, nil
}

// DetectPrimaryKey 文件矢量没有主键，返回空串（MVT 用要素序号兜底）。
func (g *FileGateway) DetectPrimaryKey(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}

// TileMVT 生成一瓦片 MVT：bbox 粗筛 → 投影/裁剪/绕向归一化 → 编码。
func (g *FileGateway) TileMVT(_ context.Context, dsn string, l *Layer, minx, miny, maxx, maxy float64, maxFeatures int) ([]byte, int, error) {
	ds, err := g.dataset(dsn, tableOf(l))
	if err != nil {
		return nil, 0, err
	}
	tileBox := Box{MinX: minx, MinY: miny, MaxX: maxx, MaxY: maxy}

	// 先数一遍命中要素：超过上限时按既有策略直接报错（不静默截断）
	hits := make([]int, 0, 64)
	for i := range ds.features {
		if ds.mercBounds[i].Empty() || !tileBox.Intersects(ds.mercBounds[i]) {
			continue
		}
		hits = append(hits, i)
		if maxFeatures > 0 && len(hits) > maxFeatures {
			return nil, 0, fmt.Errorf("%w: 瓦片要素数超过上限 %d，请提高 zoom 级别",
				ErrTooManyFeatures, maxFeatures)
		}
	}
	if len(hits) == 0 {
		return nil, 0, nil // 空瓦片
	}

	feats := make([]MVTFeature, 0, len(hits))
	for _, idx := range hits {
		f := ds.features[idx]
		clipped := clipGeometry(f.Geom, ds.info.SRID, tileBox, DefaultExtent, 64)
		if !hasGeometry(clipped) {
			continue
		}
		props := f.Props
		if props == nil {
			props = map[string]any{}
		}
		feats = append(feats, MVTFeature{ID: f.ID, Geom: clipped, Props: props})
	}
	if len(feats) == 0 {
		return nil, 0, nil
	}
	layerName := l.Name
	if layerName == "" {
		layerName = "layer"
	}
	return EncodeMVT(layerName, DefaultExtent, feats), len(feats), nil
}

// EstimatedExtent 精确计算图层包围盒（文件矢量在加载时就已算出，非估算）。
func (g *FileGateway) EstimatedExtent(_ context.Context, dsn string, l *Layer) (float64, float64, float64, float64, bool, error) {
	ds, err := g.dataset(dsn, tableOf(l))
	if err != nil {
		return 0, 0, 0, 0, false, err
	}
	if ds.info.BBox.Empty() {
		return 0, 0, 0, 0, false, nil
	}
	return ds.info.BBox.MinX, ds.info.BBox.MinY, ds.info.BBox.MaxX, ds.info.BBox.MaxY, true, nil
}

// EstimatedRows 要素总数（文件矢量可精确给出）。
func (g *FileGateway) EstimatedRows(_ context.Context, dsn string, l *Layer) (int64, error) {
	ds, err := g.dataset(dsn, tableOf(l))
	if err != nil {
		return 0, err
	}
	return int64(ds.info.Count), nil
}

// FeaturesGeoJSON WFS GetFeature 数据面：按 WGS84 bbox 过滤并输出 GeoJSON。
func (g *FileGateway) FeaturesGeoJSON(_ context.Context, dsn string, l *Layer, hasBBox bool, minx, miny, maxx, maxy float64, limit int) ([]byte, int, error) {
	ds, err := g.dataset(dsn, tableOf(l))
	if err != nil {
		return nil, 0, err
	}
	// WFS 的 bbox 是 WGS84 经纬度；文件为 4326 时可直接比较，3857 需换算。
	query := Box{MinX: minx, MinY: miny, MaxX: maxx, MaxY: maxy}
	if ds.info.SRID == 3857 && hasBBox {
		x0, y0 := lonLatToMerc(minx, miny)
		x1, y1 := lonLatToMerc(maxx, maxy)
		query = Box{MinX: math.Min(x0, x1), MinY: math.Min(y0, y1), MaxX: math.Max(x0, x1), MaxY: math.Max(y0, y1)}
	}
	if limit <= 0 {
		limit = 1000
	}

	out := make([]map[string]any, 0, 64)
	for i := range ds.features {
		b := ds.features[i].Geom.Bounds()
		if hasBBox && !query.Intersects(b) {
			continue
		}
		out = append(out, map[string]any{
			"type":       "Feature",
			"id":         ds.features[i].ID,
			"geometry":   geometryToGeoJSON(ds.features[i].Geom),
			"properties": ds.features[i].Props,
		})
		if len(out) >= limit {
			break
		}
	}
	// 契约：返回**要素数组**（与 pgGateway 的 ST_AsGeoJSON 聚合一致），
	// 外层 FeatureCollection 由 WFS 处理器统一封装——曾因这里返回整个
	// FeatureCollection 导致响应嵌套两层 features（实测踩到）。
	data, err := json.Marshal(out)
	if err != nil {
		return nil, 0, err
	}
	return data, len(out), nil
}

// Close 释放缓存（实现 Gateway）。
func (g *FileGateway) Close() {
	g.mu.Lock()
	g.cache = map[string]*fileDataset{}
	g.mu.Unlock()
}

/* ---------------- 辅助 ---------------- */

// geomTypeName 内部几何类型 → OGC 风格类型名（写入图层元数据）。
func geomTypeName(t GeomType) string {
	switch t {
	case GeomPoint:
		return "POINT"
	case GeomLine:
		return "LINESTRING"
	case GeomPolygon:
		return "POLYGON"
	}
	return "GEOMETRY"
}

// geometryToGeoJSON 内部几何 → GeoJSON geometry 对象（保持原坐标系）。
func geometryToGeoJSON(g Geometry) map[string]any {
	coords := func(line []Point) [][]float64 {
		out := make([][]float64, len(line))
		for i, p := range line {
			out[i] = []float64{p.X, p.Y}
		}
		return out
	}
	switch g.Type {
	case GeomPoint:
		if len(g.Lines) == 0 {
			return nil
		}
		if len(g.Lines) == 1 && len(g.Lines[0]) == 1 {
			return map[string]any{"type": "Point", "coordinates": []float64{g.Lines[0][0].X, g.Lines[0][0].Y}}
		}
		flat := make([][]float64, 0, len(g.Lines))
		for _, line := range g.Lines {
			for _, p := range line {
				flat = append(flat, []float64{p.X, p.Y})
			}
		}
		return map[string]any{"type": "MultiPoint", "coordinates": flat}
	case GeomLine:
		if len(g.Lines) == 1 {
			return map[string]any{"type": "LineString", "coordinates": coords(g.Lines[0])}
		}
		multi := make([][][]float64, 0, len(g.Lines))
		for _, l := range g.Lines {
			multi = append(multi, coords(l))
		}
		return map[string]any{"type": "MultiLineString", "coordinates": multi}
	case GeomPolygon:
		// 按环角色切回多边形分组（外环开启新多边形，内环挂到当前多边形）
		var polys [][][][]float64
		var cur [][][]float64
		for i, ring := range g.Lines {
			if g.isExterior(i) || len(cur) == 0 {
				if len(cur) > 0 {
					polys = append(polys, cur)
				}
				cur = [][][]float64{}
			}
			closed := coords(ring)
			if len(closed) >= 3 {
				closed = append(closed, closed[0]) // GeoJSON 环须显式闭合
			}
			cur = append(cur, closed)
		}
		if len(cur) > 0 {
			polys = append(polys, cur)
		}
		if len(polys) == 1 {
			return map[string]any{"type": "Polygon", "coordinates": polys[0]}
		}
		return map[string]any{"type": "MultiPolygon", "coordinates": polys}
	}
	return nil
}
