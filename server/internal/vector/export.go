// export.go 图层导出：把已注册的图层回流成其它 GIS 软件能直接打开的文件。
//
// 三种格式的取舍：
//
//	GeoJSON      单文件、无属性宽度限制、中文无编码困扰 —— 最省事，但体积大
//	GeoPackage   单文件 SQLite（OGC 标准），属性类型/精度完整保留 —— 现代默认选择
//	Shapefile    国内最通行的交换格式，但是**文件族**且限制多：
//	             · 一个文件只能有一种几何类型（混合类型必须丢或拆）
//	             · 属性字段名上限 10 字符、字符串宽度按字节计（255 上限）
//	             · 因此打包成 .zip 交付（.shp/.shx/.dbf/.prj/.cpg）
//
// 两条贯穿始终的原则：
//  1. **导出与导入互为逆运算**：测试直接把导出结果喂回本包的解析器验证，
//     不靠"看起来像"判断正确性。
//  2. **有损就明说**：Shapefile 丢掉混合几何要素、字段名被截断，都记进
//     ExportResult.Warnings 回显给用户，不静默处理。
package vector

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ExportFormat 导出格式。
type ExportFormat string

const (
	// ExportGeoJSON GeoJSON（单文件）。
	ExportGeoJSON ExportFormat = "geojson"
	// ExportGeoPackage GeoPackage（单文件 SQLite）。
	ExportGeoPackage ExportFormat = "gpkg"
	// ExportShapefile Shapefile（打包为 .zip：.shp/.shx/.dbf/.prj/.cpg）。
	ExportShapefile ExportFormat = "shp"
)

// ParseExportFormat 解析导出格式参数（大小写不敏感，容忍常见别名）。
func ParseExportFormat(s string) (ExportFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "geojson", "json":
		return ExportGeoJSON, nil
	case "gpkg", "geopackage":
		return ExportGeoPackage, nil
	case "shp", "shapefile", "zip":
		return ExportShapefile, nil
	}
	return "", fmt.Errorf("%w: 不支持的导出格式 %q（可选 geojson / gpkg / shp）", ErrInvalid, s)
}

// DatasetSource 可选增强：一次性取**全量**要素（导出用）。
//
// 与 FeaturesGeoJSON 的分工：那个面向 WFS 查询，受 count 上限与 bbox 过滤约束；
// 导出不能截断，必须拿全量。
type DatasetSource interface {
	// Snapshot 返回图层的全部要素与探测信息（返回值只读，调用方不得修改）。
	Snapshot(ctx context.Context, dsn string, l *Layer) ([]FileFeature, FileInfo, error)
}

// Snapshot 实现 DatasetSource：文件矢量本来就把数据集缓存在内存里，直接给引用。
func (g *FileGateway) Snapshot(_ context.Context, dsn string, l *Layer) ([]FileFeature, FileInfo, error) {
	ds, err := g.dataset(dsn, tableOf(l))
	if err != nil {
		return nil, FileInfo{}, err
	}
	return ds.features, ds.info, nil
}

// ExportResult 导出产物。
type ExportResult struct {
	// Filename 建议的下载文件名。
	Filename string
	// MimeType 响应内容类型。
	MimeType string
	// Data 产物字节（GeoJSON 与 Shapefile zip）。
	Data []byte
	// Path 产物路径（GeoPackage 需要真实文件）；TempDir 非空时调用方
	// 应当在响应结束后删除 TempDir。
	Path    string
	TempDir string
	// Features 实际写出的要素数。
	Features int
	// Skipped 因格式限制被跳过的要素数。
	Skipped int
	// Warnings 有损处理的说明（混合几何类型、字段名截断等）。
	Warnings []string
}

// TempDirCleanup 删除导出产生的临时目录（GeoPackage 落盘用）。
// 调用方应在响应写完之后调用；重复调用无副作用。
func (r *ExportResult) TempDirCleanup() {
	if r == nil || r.TempDir == "" {
		return
	}
	_ = os.RemoveAll(r.TempDir)
	r.TempDir = ""
	r.Path = ""
}

// Size 产物字节数。
func (r *ExportResult) Size() int64 {
	if r.Path != "" {
		if st, err := os.Stat(r.Path); err == nil {
			return st.Size()
		}
	}
	return int64(len(r.Data))
}

// ExportLayer 把已注册图层导出为指定格式。
func (s *Server) ExportLayer(ctx context.Context, layerName string, format ExportFormat) (*ExportResult, error) {
	l, err := s.Meta.GetLayer(ctx, layerName)
	if err != nil {
		return nil, err
	}
	src, err := s.Meta.GetSource(ctx, l.SourceID)
	if err != nil {
		return nil, err
	}
	ds, ok := s.Gateway.(DatasetSource)
	if !ok {
		return nil, fmt.Errorf("%w: 当前装配的数据源不支持导出（仅本地文件矢量数据源可导出；"+
			"PostGIS 数据源可改用 WFS GetFeature 取 GeoJSON）", ErrInvalid)
	}
	feats, info, err := ds.Snapshot(ctx, src.DSN, l)
	if err != nil {
		return nil, err
	}
	if len(feats) == 0 {
		return nil, fmt.Errorf("%w: 图层没有可导出的要素", ErrInvalid)
	}
	// 导出坐标系取图层登记的实际坐标系（文件源在解析阶段已换算成 WGS84 系）
	srid := l.SRID
	if srid == 0 {
		srid = info.SRID
	}
	name := sanitizeIdent(l.Name)
	if name == "" {
		name = "layer"
	}

	switch format {
	case ExportGeoJSON:
		return exportGeoJSON(name, feats)
	case ExportGeoPackage:
		return exportGeoPackage(name, feats, srid)
	case ExportShapefile:
		return exportShapefile(name, feats, srid)
	}
	return nil, fmt.Errorf("%w: 不支持的导出格式 %q", ErrInvalid, format)
}

/* ---------------- GeoJSON ---------------- */

// marshalGeoJSONFeatures 把要素集序列化为 GeoJSON FeatureCollection 字节。
// 导出与"就地编辑写回 GeoJSON 源文件"共用同一份实现，保证两条路径产物一致。
func marshalGeoJSONFeatures(feats []FileFeature) ([]byte, int, error) {
	arr := make([]map[string]any, 0, len(feats))
	for i := range feats {
		g := geometryToGeoJSON(feats[i].Geom)
		if g == nil {
			continue
		}
		f := map[string]any{
			"type":       "Feature",
			"geometry":   g,
			"properties": feats[i].Props,
		}
		if feats[i].ID > 0 {
			f["id"] = feats[i].ID
		}
		arr = append(arr, f)
	}
	doc := map[string]any{"type": "FeatureCollection", "features": arr}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, 0, fmt.Errorf("vector: 生成 GeoJSON 失败: %w", err)
	}
	return data, len(arr), nil
}

func exportGeoJSON(name string, feats []FileFeature) (*ExportResult, error) {
	data, n, err := marshalGeoJSONFeatures(feats)
	if err != nil {
		return nil, err
	}
	return &ExportResult{
		Filename: name + ".geojson",
		MimeType: "application/geo+json",
		Data:     data,
		Features: n,
	}, nil
}

/* ---------------- 坐标系 WKT（导出用） ---------------- */

// canonicalWKT 给出导出时写入的坐标系 WKT（ESRI 风格，兼容性最好）。
//
// 只支持文件矢量链路实际会产生的三种；其它坐标系明确报错而不是写一个
// 空的或猜的 .prj——那会让下游软件按错误坐标系打开数据。
func canonicalWKT(srid int) string {
	switch srid {
	case 4326:
		return `GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984",SPHEROID["WGS_1984",6378137.0,298.257223563]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]]`
	case 4490:
		return `GEOGCS["GCS_China_Geodetic_Coordinate_System_2000",DATUM["D_China_2000",SPHEROID["CGCS2000",6378137.0,298.257222101]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]]`
	case 3857:
		return `PROJCS["WGS_1984_Web_Mercator_Auxiliary_Sphere",GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984",SPHEROID["WGS_1984",6378137.0,298.257223563]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]],PROJECTION["Mercator_Auxiliary_Sphere"],PARAMETER["False_Easting",0.0],PARAMETER["False_Northing",0.0],PARAMETER["Central_Meridian",0.0],PARAMETER["Standard_Parallel_1",0.0],PARAMETER["Auxiliary_Sphere_Type",0.0],UNIT["Meter",1.0]]`
	}
	return ""
}

/* ---------------- 属性列推断（DBF / GeoPackage 共用） ---------------- */

// attrColumn 属性列定义。
type attrColumn struct {
	Name string
	// Src 原始字段名。DBF 字段名上限 10 字节会被截断，取值时必须按原名找，
	// 否则属性会全部落空（这是最容易踩的一个坑）。
	Src string
	// Kind 归一化类型：bool / date / int / float / text
	Kind string
	// Type dBASE 字段类型字符（C/N/D/L），仅 DBF 写出时使用。
	Type byte
	// Width DBF 用：字节宽度；Dec 小数位。
	Width int
	Dec   int
	// SQLType GeoPackage 列类型。
	SQLType string
}

// inferAttrColumns 依全部要素的属性值推断列定义（列名排序，保证产物确定）。
func inferAttrColumns(feats []FileFeature) []attrColumn {
	names := map[string]bool{}
	for i := range feats {
		for k, v := range feats[i].Props {
			if v == nil {
				continue
			}
			if _, ok := scalarize(v); ok {
				names[k] = true
			}
		}
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	out := make([]attrColumn, 0, len(sorted))
	for _, name := range sorted {
		col := attrColumn{Name: name, Src: name}
		var (
			seenBool, seenDate, seenInt, seenFloat, seenText bool
			intDigits, decimals, maxBytes                    int
		)
		for i := range feats {
			v, ok := feats[i].Props[name]
			if !ok || v == nil {
				continue
			}
			switch t := v.(type) {
			case bool:
				seenBool = true
			case int64:
				seenInt = true
				intDigits = maxInt(intDigits, len(strconv.FormatInt(t, 10)))
			case int:
				seenInt = true
				intDigits = maxInt(intDigits, len(strconv.Itoa(t)))
			case float64:
				if t == math.Trunc(t) && math.Abs(t) < 1e15 {
					seenInt = true
					intDigits = maxInt(intDigits, len(strconv.FormatInt(int64(t), 10)))
				} else {
					seenFloat = true
					// 用最短表示统计宽度与小数位
					s := strconv.FormatFloat(t, 'f', -1, 64)
					if dot := strings.IndexByte(s, '.'); dot >= 0 {
						decimals = maxInt(decimals, len(s)-dot-1)
						intDigits = maxInt(intDigits, maxInt(1, dot))
					} else {
						intDigits = maxInt(intDigits, len(s))
					}
				}
			case string:
				if isISODate(t) {
					seenDate = true
				} else {
					seenText = true
					maxBytes = maxInt(maxBytes, len(t))
				}
			default:
				s := fmt.Sprint(t)
				seenText = true
				maxBytes = maxInt(maxBytes, len(s))
			}
		}
		switch {
		case seenText:
			col.Kind, col.Width = "text", clampInt(maxBytes, 1, 254)
		case seenFloat:
			col.Kind, col.Dec = "float", clampInt(decimals, 0, 15)
			col.Width = clampInt(intDigits+col.Dec+2, 4, 24)
		case seenInt && seenBool:
			col.Kind, col.Width = "text", 8 // 混合布尔与数值：按文本存，避免歧义
		case seenInt:
			col.Kind, col.Width = "int", clampInt(intDigits+1, 2, 20)
		case seenDate:
			col.Kind, col.Width = "date", 8
		case seenBool:
			col.Kind, col.Width = "bool", 1
		default:
			col.Kind, col.Width = "text", 1
		}
		switch col.Kind {
		case "int":
			col.SQLType = "INTEGER"
		case "float":
			col.SQLType = "REAL"
		default:
			col.SQLType = "TEXT"
		}
		out = append(out, col)
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// isISODate 判断是否为 YYYY-MM-DD。
func isISODate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	if !allDigits(s[0:4]) || !allDigits(s[5:7]) || !allDigits(s[8:10]) {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

/* ---------------- Shapefile 导出 ---------------- */

// shpTypeFor 依图层几何类型决定 Shapefile 形状类型，并过滤不兼容要素。
//
// Shapefile 规范要求同一文件内所有非 Null 记录形状类型一致，因此混合类型
// 图层必须取舍：保留主类型、跳过其余，并把跳过数量回显给用户。
func shpTypeFor(gtype GeomType, feats []FileFeature) (int32, []int, int) {
	base := gtype
	if base == GeomUnknown {
		counts := map[GeomType]int{}
		for i := range feats {
			counts[feats[i].Geom.Type]++
		}
		if t, ok := closestGeomType(counts); ok {
			base = t
		}
	}
	kept := make([]int, 0, len(feats))
	skipped := 0
	switch base {
	case GeomPoint:
		multi := false
		for i := range feats {
			if feats[i].Geom.Type != GeomPoint {
				skipped++
				continue
			}
			kept = append(kept, i)
			n := 0
			for _, l := range feats[i].Geom.Lines {
				n += len(l)
			}
			if n > 1 {
				multi = true
			}
		}
		if multi {
			return shpMultiPoint, kept, skipped
		}
		return shpPoint, kept, skipped
	case GeomLine:
		for i := range feats {
			if feats[i].Geom.Type != GeomLine {
				skipped++
				continue
			}
			kept = append(kept, i)
		}
		return shpPolyLine, kept, skipped
	case GeomPolygon:
		for i := range feats {
			if feats[i].Geom.Type != GeomPolygon {
				skipped++
				continue
			}
			kept = append(kept, i)
		}
		return shpPolygon, kept, skipped
	}
	return shpNull, nil, skipped
}

// buildShapefileSet 生成 Shapefile 文件族内容（后缀 → 字节）。
// 导出打包与"就地编辑写回 Shapefile"共用；返回被跳过的要素数与提示。
func buildShapefileSet(name string, feats []FileFeature, srid int) (map[string][]byte, int, int, []string, error) {
	prj := canonicalWKT(srid)
	if prj == "" {
		return nil, 0, 0, nil, fmt.Errorf("%w: 坐标系 EPSG:%d 暂不支持写入 Shapefile（仅 4326 / 4490 / 3857）", ErrInvalid, srid)
	}
	shapeType, kept, skipped := shpTypeFor(GeomType(0), feats)
	if shapeType == shpNull || len(kept) == 0 {
		return nil, 0, 0, nil, fmt.Errorf("%w: 图层没有可写入 Shapefile 的几何", ErrInvalid)
	}
	columns, warn := dbColumnsForShapefile(inferAttrColumns(feats))

	var shpBody, shxBody bytes.Buffer
	shpBody.Grow(len(kept) * 64)
	box := Box{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	for seq, idx := range kept {
		content := shpRecordContent(shapeType, feats[idx].Geom)
		if len(content) < 4 {
			continue
		}
		offset := int32((100 + shpBody.Len()) / 2) // 以 16 位字为单位
		var head [8]byte
		binary.BigEndian.PutUint32(head[0:4], uint32(seq+1))
		binary.BigEndian.PutUint32(head[4:8], uint32(len(content)/2))
		shpBody.Write(head[:])
		shpBody.Write(content)
		shxBody.Write(shxRecord(offset, int32(len(content)/2)))
		box = box.Union(feats[idx].Geom.Bounds())
	}
	files := map[string][]byte{
		".shp": shpFileBytes(shapeType, box, shpBody.Bytes()),
		".shx": shxFileBytes(shapeType, box, shxBody.Bytes()),
		".dbf": dbfFileBytes(columns, feats, kept),
		".prj": []byte(prj),
		// 显式声明 UTF-8：统一写 UTF-8，带上 .cpg 让 ArcGIS 等老软件
		// 也能正确解码中文，不靠猜。
		".cpg": []byte("UTF-8"),
	}
	var warnings []string
	if skipped > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"Shapefile 一个文件只能有一种几何类型，已跳过 %d 条其它类型的要素", skipped))
	}
	if warn != "" {
		warnings = append(warnings, warn)
	}
	return files, len(kept), skipped, warnings, nil
}

// exportShapefile 生成 Shapefile 套件并打包为 zip。
func exportShapefile(name string, feats []FileFeature, srid int) (*ExportResult, error) {
	files, n, skipped, warnings, err := buildShapefileSet(name, feats, srid)
	if err != nil {
		return nil, err
	}
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	for _, suffix := range []string{".shp", ".shx", ".dbf", ".prj", ".cpg"} {
		w, err := zw.Create(name + suffix)
		if err != nil {
			return nil, fmt.Errorf("vector: 打包 Shapefile 失败: %w", err)
		}
		if _, err := w.Write(files[suffix]); err != nil {
			return nil, fmt.Errorf("vector: 打包 Shapefile 失败: %w", err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("vector: 打包 Shapefile 失败: %w", err)
	}
	return &ExportResult{
		Filename: name + "_shp.zip",
		MimeType: "application/zip",
		Data:     zipBuf.Bytes(),
		Features: n,
		Skipped:  skipped,
		Warnings: warnings,
	}, nil
}

// putF64 以小端写入 float64。
func putF64(b []byte, off int, v float64) {
	binary.LittleEndian.PutUint64(b[off:off+8], math.Float64bits(v))
}

// shxRecord 组装 .shx 索引记录（偏移与内容长度，均以 16 位字计，大端）。
func shxRecord(offset, words int32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], uint32(offset))
	binary.BigEndian.PutUint32(b[4:8], uint32(words))
	return b
}

// shpFileBytes 组装 .shp 完整文件（100 字节文件头 + 记录）。
func shpFileBytes(shapeType int32, box Box, records []byte) []byte {
	head := make([]byte, 100)
	binary.BigEndian.PutUint32(head[0:4], shpFileCode)
	binary.BigEndian.PutUint32(head[24:28], uint32((100+len(records))/2))
	binary.LittleEndian.PutUint32(head[28:32], 1000)
	binary.LittleEndian.PutUint32(head[32:36], uint32(shapeType))
	putF64(head, 36, box.MinX)
	putF64(head, 44, box.MinY)
	putF64(head, 52, box.MaxX)
	putF64(head, 60, box.MaxY)
	return append(head, records...)
}

// shxFileBytes 组装 .shx 索引文件（与 .shp 同样的 100 字节文件头 + 每条 8 字节）。
func shxFileBytes(shapeType int32, box Box, records []byte) []byte {
	head := make([]byte, 100)
	binary.BigEndian.PutUint32(head[0:4], shpFileCode)
	binary.BigEndian.PutUint32(head[24:28], uint32((100+len(records))/2))
	binary.LittleEndian.PutUint32(head[28:32], 1000)
	binary.LittleEndian.PutUint32(head[32:36], uint32(shapeType))
	putF64(head, 36, box.MinX)
	putF64(head, 44, box.MinY)
	putF64(head, 52, box.MaxX)
	putF64(head, 60, box.MaxY)
	return append(head, records...)
}

// shpRecordContent 编码一条记录的几何内容（含形状类型字段）。
//
// 环方向按 ESRI 约定归一化：外环顺时针（在正北向上的平面里为负面积）、
// 内环逆时针。数据源的环方向来自别处（GeoJSON 是反的），不纠正会让
// QGIS/ArcGIS 把洞画成实体。
func shpRecordContent(shapeType int32, g Geometry) []byte {
	switch shapeType {
	case shpPoint:
		pts := flattenPoints(g)
		if len(pts) != 1 {
			return nil
		}
		b := make([]byte, 20)
		binary.LittleEndian.PutUint32(b[0:4], uint32(shpPoint))
		putF64(b, 4, pts[0].X)
		putF64(b, 12, pts[0].Y)
		return b
	case shpMultiPoint:
		pts := flattenPoints(g)
		if len(pts) == 0 {
			return nil
		}
		b := make([]byte, 40+len(pts)*16)
		binary.LittleEndian.PutUint32(b[0:4], uint32(shpMultiPoint))
		box := boundsOfPoints(pts)
		putF64(b, 4, box.MinX)
		putF64(b, 12, box.MinY)
		putF64(b, 20, box.MaxX)
		putF64(b, 28, box.MaxY)
		binary.LittleEndian.PutUint32(b[36:40], uint32(len(pts)))
		for i, p := range pts {
			putF64(b, 40+i*16, p.X)
			putF64(b, 40+i*16+8, p.Y)
		}
		return b
	case shpPolyLine:
		parts := nonEmptyLines(g.Lines)
		return shpPartsRecord(shpPolyLine, parts)
	case shpPolygon:
		rings := make([][]Point, 0, len(g.Lines))
		for i, r := range g.Lines {
			if len(r) < 3 {
				continue
			}
			// ESRI 约定：外环顺时针（负面积）、内环逆时针
			rings = append(rings, orientRing(r, !g.isExterior(i)))
		}
		if len(rings) == 0 {
			return nil
		}
		return shpPartsRecord(shpPolygon, rings)
	}
	return nil
}

// shpPartsRecord 组装折线/多边形记录（Box + NumParts + NumPoints + Parts + Points）。
func shpPartsRecord(shapeType int32, parts [][]Point) []byte {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	if total == 0 {
		return nil
	}
	b := make([]byte, 44+len(parts)*4+total*16)
	binary.LittleEndian.PutUint32(b[0:4], uint32(shapeType))
	box := Box{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	off := 44 + len(parts)*4
	partIdx := 0
	for i, p := range parts {
		binary.LittleEndian.PutUint32(b[44+i*4:48+i*4], uint32(partIdx))
		for _, pt := range p {
			putF64(b, off+partIdx*16, pt.X)
			putF64(b, off+partIdx*16+8, pt.Y)
			partIdx++
			box = box.Union(Box{MinX: pt.X, MinY: pt.Y, MaxX: pt.X, MaxY: pt.Y})
		}
	}
	binary.LittleEndian.PutUint32(b[36:40], uint32(len(parts)))
	binary.LittleEndian.PutUint32(b[40:44], uint32(total))
	putF64(b, 4, box.MinX)
	putF64(b, 12, box.MinY)
	putF64(b, 20, box.MaxX)
	putF64(b, 28, box.MaxY)
	return b
}

// flattenPoints 取几何里的全部点（多点要素展开）。
func flattenPoints(g Geometry) []Point {
	var pts []Point
	for _, line := range g.Lines {
		pts = append(pts, line...)
	}
	return pts
}

// boundsOfPoints 计算点集包围盒。
func boundsOfPoints(pts []Point) Box {
	b := Box{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	for _, p := range pts {
		b.MinX = math.Min(b.MinX, p.X)
		b.MinY = math.Min(b.MinY, p.Y)
		b.MaxX = math.Max(b.MaxX, p.X)
		b.MaxY = math.Max(b.MaxY, p.Y)
	}
	return b
}

// orientRing 强制环方向：wantPositive 为真要求正向面积，否则负向。
func orientRing(ring []Point, wantPositive bool) []Point {
	a := signedArea(ring)
	if (wantPositive && a >= 0) || (!wantPositive && a <= 0) {
		return ring
	}
	rev := make([]Point, len(ring))
	for i, p := range ring {
		rev[len(ring)-1-i] = p
	}
	return rev
}

// dbColumnsForShapefile 把属性列转成 DBF 可用的列定义。
//
// dBASE 的限制：字段名 ≤ 10 字符、字符串宽度按**字节**计。字段名会被截断，
// 重名要消解，这些都要如实回显（warn），不能让用户拿着一个字段少了的
// Shapefile 却不知道为什么。
func dbColumnsForShapefile(cols []attrColumn) ([]attrColumn, string) {
	const maxName = 10
	seen := map[string]bool{}
	out := make([]attrColumn, 0, len(cols))
	truncated := 0
	for i, c := range cols {
		name := c.Name
		if len(name) > maxName {
			// 按字节截断但不能把多字节字符劈开
			name = string(truncateUTF8([]byte(name), maxName))
			truncated++
		}
		name = strings.TrimRight(name, " ")
		if name == "" || seen[name] {
			name = fmt.Sprintf("F%d", i+1)
			for seen[name] {
				name = "F" + name
			}
		}
		seen[name] = true
		c.Name = name // Name 给 DBF 头用；Src 保留原名供取值
		switch c.Kind {
		case "bool":
			c.Type = 'L'
			c.Width = 1
			c.Dec = 0
		case "date":
			c.Type = 'D'
			c.Width = 8
			c.Dec = 0
		case "int", "float":
			c.Type = 'N'
		default:
			c.Type = 'C'
		}
		out = append(out, c)
	}
	warn := ""
	if truncated > 0 {
		warn = fmt.Sprintf("DBF 字段名上限 10 字符，已截断 %d 个字段名", truncated)
	}
	return out, warn
}

// dbfCellValue 取属性值：按**原始字段名**查找（DBF 头里写的是截断后的名字）。
func dbfCellValue(props map[string]any, c attrColumn) any {
	if props == nil {
		return nil
	}
	src := c.Src
	if src == "" {
		src = c.Name
	}
	return props[src]
}

// dbfFileBytes 生成 .dbf 属性表（dBASE III，UTF-8 + 后续 .cpg 声明）。
func dbfFileBytes(cols []attrColumn, feats []FileFeature, kept []int) []byte {
	headerSize := 32 + 32*len(cols) + 1
	recordSize := 1
	for _, c := range cols {
		recordSize += c.Width
	}
	out := make([]byte, headerSize+recordSize*len(kept))
	out[0] = 0x03
	now := time.Now()
	out[1] = byte(now.Year() - 1900)
	out[2] = byte(int(now.Month()))
	out[3] = byte(now.Day())
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(kept)))
	binary.LittleEndian.PutUint16(out[8:10], uint16(headerSize))
	binary.LittleEndian.PutUint16(out[10:12], uint16(recordSize))
	for i, c := range cols {
		off := 32 + i*32
		copy(out[off:off+11], c.Name) // 已保证 ≤10 字节
		out[off+11] = c.Type
		out[off+16] = byte(c.Width)
		out[off+17] = byte(c.Dec)
	}
	out[headerSize-1] = 0x0D

	pos := headerSize
	for _, idx := range kept {
		out[pos] = 0x20 // 未删除
		off := pos + 1
		for _, c := range cols {
			cell := make([]byte, c.Width)
			for k := range cell {
				cell[k] = ' '
			}
			writeDBFCell(cell, c, dbfCellValue(feats[idx].Props, c))
			copy(out[off:off+c.Width], cell)
			off += c.Width
		}
		pos += recordSize
	}
	return out
}

// writeDBFCell 把属性值写入定长单元（数值右对齐、字符左对齐）。
func writeDBFCell(cell []byte, c attrColumn, v any) {
	if v == nil {
		return // 全空格
	}
	switch c.Type {
	case 'L':
		switch t := v.(type) {
		case bool:
			if t {
				cell[0] = 'T'
			} else {
				cell[0] = 'F'
			}
		}
	case 'D':
		if s, ok := v.(string); ok && isISODate(s) {
			copy(cell, []byte(strings.ReplaceAll(s, "-", "")))
		}
	case 'N':
		var s string
		switch t := v.(type) {
		case int64:
			s = strconv.FormatInt(t, 10)
		case int:
			s = strconv.Itoa(t)
		case float64:
			if c.Dec > 0 {
				s = strconv.FormatFloat(t, 'f', c.Dec, 64)
			} else {
				s = strconv.FormatFloat(t, 'f', 0, 64)
			}
		case bool:
			if t {
				s = "1"
			} else {
				s = "0"
			}
		default:
			s = fmt.Sprint(t)
		}
		if len(s) > c.Width {
			// 列宽是照实际数据推断的，正常不会溢出。真溢出时按 dBASE 的
			// 标准做法写一排 '*'（表示数值溢出），而不是悄悄截掉数字。
			for i := range cell {
				cell[i] = '*'
			}
			return
		}
		copy(cell[c.Width-len(s):], s)
	default: // 'C'
		s := fmt.Sprint(v)
		b := []byte(s)
		if len(b) > c.Width {
			b = truncateUTF8(b, c.Width)
		}
		copy(cell, b)
	}
}

// truncateUTF8 按字节截断但保证不断开多字节字符。
func truncateUTF8(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return b[:cut]
}

/* ---------------- GeoPackage 导出 ---------------- */

// exportGeoPackage 生成 GeoPackage 文件（真实 SQLite 库）。
func exportGeoPackage(name string, feats []FileFeature, srid int) (*ExportResult, error) {
	wkt := canonicalWKT(srid)
	if wkt == "" {
		return nil, fmt.Errorf("%w: 坐标系 EPSG:%d 暂不支持导出为 GeoPackage（仅 4326 / 4490 / 3857）", ErrInvalid, srid)
	}
	tmpDir, err := os.MkdirTemp("", "tangis-export-")
	if err != nil {
		return nil, fmt.Errorf("vector: 创建导出临时目录失败: %w", err)
	}
	path := filepath.Join(tmpDir, name+".gpkg")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("vector: 创建 GeoPackage 失败: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := writeGeoPackageSchema(db, name, feats, srid, wkt); err != nil {
		db.Close()
		os.RemoveAll(tmpDir)
		return nil, err
	}
	if err := db.Close(); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("vector: 关闭 GeoPackage 失败: %w", err)
	}
	// 不在这里加"二维/WKB"之类的常规说明：那条提示每次都会出现，属于噪音。
	// Warnings 只承载**真正的损失**（跳过的要素、被截断的字段名），
	// 保证前端弹出来的每一条都值得用户看一眼。
	return &ExportResult{
		Filename: name + ".gpkg",
		MimeType: "application/geopackage+sqlite3",
		Path:     path,
		TempDir:  tmpDir,
		Features: len(feats),
	}, nil
}

// gpkgGeomTypeName 依数据推断 GeoPackage 的几何类型名。
func gpkgGeomTypeName(feats []FileFeature) (string, error) {
	var points, lines, polys int
	multi := false
	for i := range feats {
		switch feats[i].Geom.Type {
		case GeomPoint:
			points++
			for _, l := range feats[i].Geom.Lines {
				if len(l) > 1 {
					multi = true
				}
			}
		case GeomLine:
			lines++
			if len(nonEmptyLines(feats[i].Geom.Lines)) > 1 {
				multi = true
			}
		case GeomPolygon:
			polys++
			if len(groupPolygonRings(feats[i].Geom)) > 1 {
				multi = true
			}
		}
	}
	prefix := ""
	if multi {
		prefix = "MULTI"
	}
	switch {
	case points > 0 && lines == 0 && polys == 0:
		return prefix + "POINT", nil
	case lines > 0 && points == 0 && polys == 0:
		return prefix + "LINESTRING", nil
	case polys > 0 && points == 0 && lines == 0:
		return prefix + "POLYGON", nil
	}
	return "", fmt.Errorf("%w: 图层几何类型混合，无法写入 GeoPackage 单列", ErrInvalid)
}

// writeGeoPackageSchema 建规范表 + 要素表并写入数据。
func writeGeoPackageSchema(db *sql.DB, name string, feats []FileFeature, srid int, wkt string) error {
	geomType, err := gpkgGeomTypeName(feats)
	if err != nil {
		return err
	}
	stmts := []string{
		`PRAGMA application_id = 1196444487`, // 'GPKG'
		`PRAGMA user_version = 10200`,        // GeoPackage 1.2.0
		`CREATE TABLE gpkg_spatial_ref_sys (
			srs_name TEXT NOT NULL, srs_id INTEGER PRIMARY KEY,
			organization TEXT NOT NULL, organization_coordsys_id INTEGER NOT NULL,
			definition TEXT NOT NULL, description TEXT)`,
		`CREATE TABLE gpkg_contents (
			table_name TEXT NOT NULL PRIMARY KEY, data_type TEXT NOT NULL,
			identifier TEXT UNIQUE, description TEXT DEFAULT '',
			last_change DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			min_x DOUBLE, min_y DOUBLE, max_x DOUBLE, max_y DOUBLE, srs_id INTEGER,
			CONSTRAINT fk_gc_r_srs_id FOREIGN KEY (srs_id) REFERENCES gpkg_spatial_ref_sys(srs_id))`,
		`CREATE TABLE gpkg_geometry_columns (
			table_name TEXT NOT NULL, column_name TEXT NOT NULL,
			geometry_type_name TEXT NOT NULL, srs_id INTEGER NOT NULL,
			z TINYINT NOT NULL, m TINYINT NOT NULL,
			CONSTRAINT pk_geom_cols PRIMARY KEY (table_name, column_name),
			CONSTRAINT uk_gc_table_name UNIQUE (table_name))`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("vector: 初始化 GeoPackage 失败: %w", err)
		}
	}
	// 规范要求的固定行 + 本图层使用的坐标系
	base := [][]any{
		{"Undefined cartesian SRS", -1, "NONE", -1, "undefined", nil},
		{"Undefined geographic SRS", 0, "NONE", 0, "undefined", nil},
		{"WGS 84 geodetic", 4326, "EPSG", 4326, canonicalWKT(4326), nil},
		{"China Geodetic Coordinate System 2000", 4490, "EPSG", 4490, canonicalWKT(4490), nil},
		{"WGS 84 / Pseudo-Mercator", 3857, "EPSG", 3857, canonicalWKT(3857), nil},
	}
	if srid != 4326 && srid != 4490 && srid != 3857 {
		base = append(base, []any{"Exported SRS", srid, "EPSG", srid, wkt, nil})
	}
	for _, row := range base {
		if _, err := db.Exec(`INSERT INTO gpkg_spatial_ref_sys
			(srs_name, srs_id, organization, organization_coordsys_id, definition, description)
			VALUES (?,?,?,?,?,?)`, row...); err != nil {
			return fmt.Errorf("vector: 写入坐标系失败: %w", err)
		}
	}
	if _, err := db.Exec(`UPDATE gpkg_spatial_ref_sys SET definition = ? WHERE srs_id = ?`,
		wkt, srid); err != nil {
		return fmt.Errorf("vector: 更新坐标系定义失败: %w", err)
	}

	// 要素表：fid + 几何列 + 属性列（列名按原样引用，SQLite 允许中文列名）
	cols := inferAttrColumns(feats)
	quoted := make([]string, 0, len(cols)+2)
	quoted = append(quoted, `fid INTEGER PRIMARY KEY AUTOINCREMENT`, `"geom" BLOB`)
	for _, c := range cols {
		quoted = append(quoted, quoteMust(c.Name)+" "+c.SQLType)
	}
	tbl := quoteMust(name)
	if _, err := db.Exec("CREATE TABLE " + tbl + " (" + strings.Join(quoted, ", ") + ")"); err != nil {
		return fmt.Errorf("vector: 创建要素表失败: %w", err)
	}

	if _, err := db.Exec(`INSERT INTO gpkg_contents
		(table_name, data_type, identifier, srs_id) VALUES (?,'features',?,?)`,
		name, name, srid); err != nil {
		return fmt.Errorf("vector: 写入 gpkg_contents 失败: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO gpkg_geometry_columns VALUES (?,?,?,?,0,0)`,
		name, "geom", geomType, srid); err != nil {
		return fmt.Errorf("vector: 写入 gpkg_geometry_columns 失败: %w", err)
	}

	names := []string{`"geom"`}
	for _, c := range cols {
		names = append(names, quoteMust(c.Name))
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	insert := "INSERT INTO " + tbl + " (" + strings.Join(names, ",") + ") VALUES (" + ph + ")"
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("vector: 开启事务失败: %w", err)
	}
	stmt, err := tx.Prepare(insert)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("vector: 准备写入失败: %w", err)
	}
	var minx, miny, maxx, maxy = math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	written := 0
	for i := range feats {
		geom := normalizeForGeoPackage(feats[i].Geom)
		wkb := wkbEncode(geom)
		if wkb == nil {
			continue
		}
		blob := gpkgBlobBytes(wkb, srid)
		vals := make([]any, 0, len(names))
		vals = append(vals, blob)
		for _, c := range cols {
			vals = append(vals, feats[i].Props[c.Name])
		}
		if _, err := stmt.Exec(vals...); err != nil {
			stmt.Close()
			tx.Rollback()
			return fmt.Errorf("vector: 写入要素失败: %w", err)
		}
		written++
		box := feats[i].Geom.Bounds()
		minx = math.Min(minx, box.MinX)
		miny = math.Min(miny, box.MinY)
		maxx = math.Max(maxx, box.MaxX)
		maxy = math.Max(maxy, box.MaxY)
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("vector: 提交写入失败: %w", err)
	}
	// 回填范围（gpkg_contents 的 bbox 便于其它软件直接定位）
	if written > 0 && !math.IsInf(minx, 1) {
		if _, err := db.Exec(`UPDATE gpkg_contents SET min_x=?, min_y=?, max_x=?, max_y=? WHERE table_name=?`,
			minx, miny, maxx, maxy, name); err != nil {
			return fmt.Errorf("vector: 回填范围失败: %w", err)
		}
	}
	// 外键与完整性检查留在库内（分析统计），便于其它软件打开时直接可用
	if _, err := db.Exec(`ANALYZE`); err != nil {
		return fmt.Errorf("vector: ANALYZE 失败: %w", err)
	}
	return nil
}

// normalizeForGeoPackage 按 GeoPackage（OGC 简单要素）约定归一化环方向：
// 外环逆时针（正面积）、内环顺时针。GeoJSON 源本来就是这个约定，
// 但 Shapefile 源是反的，导出前统一一次。
func normalizeForGeoPackage(g Geometry) Geometry {
	if g.Type != GeomPolygon {
		return g
	}
	out := Geometry{Type: g.Type, Lines: make([][]Point, len(g.Lines)), Exterior: g.Exterior}
	for i, r := range g.Lines {
		if len(r) < 3 {
			out.Lines[i] = r
			continue
		}
		out.Lines[i] = orientRing(r, g.isExterior(i))
	}
	return out
}

// gpkgBlobBytes 组装 GeoPackage 几何 BLOB（GP 头 + WKB，小端、无包络）。
func gpkgBlobBytes(wkb []byte, srsID int) []byte {
	b := make([]byte, 8, 8+len(wkb))
	b[0], b[1] = 'G', 'P'
	b[2] = 0
	b[3] = 0x01 // 小端、无包络、非空
	binary.LittleEndian.PutUint32(b[4:8], uint32(srsID))
	return append(b, wkb...)
}
