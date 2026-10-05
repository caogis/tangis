// geojson.go 文件矢量的 GeoJSON 读取。
//
// 支持 FeatureCollection / 单个 Feature / 裸 Geometry；坐标按 RFC 7946 视为
// WGS84（EPSG:4326，经纬度序）。Z 值忽略（MVT 是二维格式）。
//
// 环角色：GeoJSON 明确区分外环与内环，这里如实标注到 Geometry.Exterior，
// 供编码前按 MVT 要求归一化绕向——不能靠绕向反推，因为真实数据里
// 「外环顺时针」（旧 Shapefile 习惯）与 RFC 7946 的逆时针约定都大量存在。
package vector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// FileFeature 文件矢量中的一个要素。
type FileFeature struct {
	ID    uint64
	Geom  Geometry
	Props map[string]any
}

// FileInfo 文件矢量探测摘要（注册图层时落元数据）。
type FileInfo struct {
	// Count 要素总数。
	Count int
	// BBox 原生坐标系下的包围盒（GeoJSON 为经纬度）。
	BBox Box
	// SRID 原生坐标系：GeoJSON 固定 4326（Shapefile 由 .prj 判定）。
	SRID int
	// GeomType 图层主几何类型（混合时取更复杂的一类）。
	GeomType GeomType
	// Fields 属性字段名（去重后排序，保证元数据稳定）。
	Fields []string
	// TypeCounts 各几何类型要素数（用于提示混合类型数据）。
	TypeCounts map[GeomType]int
	// Notes 解析提示（属性表编码回退、缺 .dbf 等），如实告知而不静默。
	// 任何需要用户知道的解析降级都记在这里，由注册接口回显。
	Notes []string
}

// gjGeometry GeoJSON 几何（递归结构）。
type gjGeometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
	Geometries  []gjGeometry    `json:"geometries"`
}

// gjFeature GeoJSON 要素。
type gjFeature struct {
	Type        string          `json:"type"`
	ID          any             `json:"id"`
	Geometry    *gjGeometry     `json:"geometry"`
	Properties  map[string]any  `json:"properties"`
	Features    []gjFeature     `json:"features"`
	Coordinates json.RawMessage `json:"coordinates"`
}

// ParseGeoJSON 解析 GeoJSON 字节为要素集与探测摘要。
func ParseGeoJSON(data []byte) ([]FileFeature, FileInfo, error) {
	info := FileInfo{SRID: 4326, BBox: Box{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}, TypeCounts: map[GeomType]int{}}
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}))
	if len(trimmed) == 0 {
		return nil, info, fmt.Errorf("%w: empty geojson", ErrInvalid)
	}

	var doc gjFeature
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return nil, info, fmt.Errorf("%w: invalid geojson: %v", ErrInvalid, err)
	}

	var rawGeoms []*gjGeometry
	switch doc.Type {
	case "FeatureCollection":
		rawGeoms = make([]*gjGeometry, 0, len(doc.Features))
		ids := make([]any, 0, len(doc.Features))
		props := make([]map[string]any, 0, len(doc.Features))
		for _, f := range doc.Features {
			rawGeoms = append(rawGeoms, f.Geometry)
			ids = append(ids, f.ID)
			props = append(props, f.Properties)
		}
		return buildFeatures(rawGeoms, ids, props, &info)
	case "Feature":
		return buildFeatures([]*gjGeometry{doc.Geometry}, []any{doc.ID}, []map[string]any{doc.Properties}, &info)
	default:
		// 裸几何（直接按几何结构再解一次，避免与 Feature 结构纠缠）
		var bare gjGeometry
		if err := json.Unmarshal(trimmed, &bare); err != nil {
			return nil, info, fmt.Errorf("%w: invalid geojson geometry: %v", ErrInvalid, err)
		}
		return buildFeatures([]*gjGeometry{&bare}, []any{nil}, []map[string]any{nil}, &info)
	}
}

// buildFeatures 把原始几何/属性统一转换成 FileFeature 并累积探测信息。
func buildFeatures(geoms []*gjGeometry, ids []any, props []map[string]any, info *FileInfo) ([]FileFeature, FileInfo, error) {
	out := make([]FileFeature, 0, len(geoms))
	fieldSet := map[string]bool{}
	var seq uint64
	for i, g := range geoms {
		var id any
		var p map[string]any
		if i < len(ids) {
			id = ids[i]
		}
		if i < len(props) {
			p = props[i]
		}
		if g == nil {
			// 几何为 null 的要素（GeoJSON 允许）跳过，但计数不掩盖
			continue
		}
		geom, err := convertGeometry(g)
		if err != nil {
			return nil, *info, err
		}
		if geom.Type == GeomUnknown || len(geom.Lines) == 0 {
			continue
		}
		seq++
		out = append(out, FileFeature{ID: featureID(id, seq), Geom: geom, Props: p})
		b := geom.Bounds()
		info.BBox = info.BBox.Union(b)
		info.TypeCounts[geom.Type]++
		for k, v := range p {
			if v == nil {
				continue
			}
			if _, ok := scalarize(v); ok {
				fieldSet[k] = true
			}
		}
	}
	info.Count = len(out)
	if gt, ok := closestGeomType(info.TypeCounts); ok {
		info.GeomType = gt
	}
	info.Fields = make([]string, 0, len(fieldSet))
	for k := range fieldSet {
		info.Fields = append(info.Fields, k)
	}
	sort.Strings(info.Fields)
	return out, *info, nil
}

// featureID 取要素 ID：数值型直接用，字符串型可解析为数字则用，否则用序号。
func featureID(raw any, seq uint64) uint64 {
	switch t := raw.(type) {
	case float64:
		if t > 0 && t == math.Trunc(t) {
			return uint64(t)
		}
	case string:
		if n, err := strconv.ParseUint(t, 10, 64); err == nil {
			return n
		}
	}
	return seq
}

// convertGeometry GeoJSON 几何 → 内部几何模型（含环角色标注）。
func convertGeometry(g *gjGeometry) (Geometry, error) {
	switch g.Type {
	case "Point":
		p, err := parsePoint(g.Coordinates)
		if err != nil {
			return Geometry{}, err
		}
		return Geometry{Type: GeomPoint, Lines: [][]Point{{p}}}, nil
	case "MultiPoint":
		pts, err := parseLine(g.Coordinates)
		if err != nil {
			return Geometry{}, err
		}
		lines := make([][]Point, 0, len(pts))
		for _, p := range pts {
			lines = append(lines, []Point{p})
		}
		return Geometry{Type: GeomPoint, Lines: lines}, nil
	case "LineString":
		line, err := parseLine(g.Coordinates)
		if err != nil {
			return Geometry{}, err
		}
		return Geometry{Type: GeomLine, Lines: [][]Point{line}}, nil
	case "MultiLineString":
		lines, err := parseMultiLine(g.Coordinates)
		if err != nil {
			return Geometry{}, err
		}
		return Geometry{Type: GeomLine, Lines: lines}, nil
	case "Polygon":
		rings, err := parseMultiLine(g.Coordinates)
		if err != nil {
			return Geometry{}, err
		}
		return polygonGeometry([][][]Point{rings}), nil
	case "MultiPolygon":
		polys, err := parseMultiPolygon(g.Coordinates)
		if err != nil {
			return Geometry{}, err
		}
		return polygonGeometry(polys), nil
	case "GeometryCollection":
		// 集合内的几何按类型归并（同一要素混合类型时以首个几何类型为准）
		var merged Geometry
		for i := range g.Geometries {
			sub, err := convertGeometry(&g.Geometries[i])
			if err != nil {
				return Geometry{}, err
			}
			if merged.Type == GeomUnknown {
				merged.Type = sub.Type
			}
			if sub.Type != merged.Type {
				continue // 混合类型集合：只保留主类型，避免产出非法 MVT 要素
			}
			merged.Lines = append(merged.Lines, sub.Lines...)
			merged.Exterior = append(merged.Exterior, sub.Exterior...)
		}
		return merged, nil
	}
	return Geometry{}, fmt.Errorf("%w: unsupported geojson geometry %q", ErrInvalid, g.Type)
}

// polygonGeometry 把「多边形列表（每个多边形 = 外环 + 内环）」拍平成 MVT 的环序列。
//
// 内部模型统一使用**开环**：GeoJSON 的环通常显式闭合（首尾点相同），这里去掉
// 重复的尾点。否则「闭合点」会被当成真实顶点带着走——编码时靠 ClosePath 表达
// 闭合、导出 GeoJSON 时再补一次，导致导出结果出现两个重复尾点（实测踩到）。
func polygonGeometry(polys [][][]Point) Geometry {
	g := Geometry{Type: GeomPolygon}
	for _, rings := range polys {
		for i, r := range rings {
			if len(r) >= 2 && r[0] == r[len(r)-1] {
				r = r[:len(r)-1]
			}
			if len(r) < 3 {
				continue
			}
			g.Lines = append(g.Lines, r)
			g.Exterior = append(g.Exterior, i == 0)
		}
	}
	return g
}

// parsePoint 解析 [x,y(,z)]。
func parsePoint(raw json.RawMessage) (Point, error) {
	var c []float64
	if err := json.Unmarshal(raw, &c); err != nil || len(c) < 2 {
		return Point{}, fmt.Errorf("%w: invalid coordinates", ErrInvalid)
	}
	return Point{X: c[0], Y: c[1]}, nil
}

// parseLine 解析 [[x,y],…]。
func parseLine(raw json.RawMessage) ([]Point, error) {
	pts, err := parseMultiLine(raw)
	if err != nil {
		return nil, err
	}
	if len(pts) == 0 {
		return nil, fmt.Errorf("%w: empty linestring", ErrInvalid)
	}
	return pts[0], nil
}

// parseMultiLine 解析任意嵌套的坐标数组（深度自适应）。
func parseMultiLine(raw json.RawMessage) ([][]Point, error) {
	var any coordsAny
	if err := json.Unmarshal(raw, &any); err != nil {
		return nil, fmt.Errorf("%w: invalid coordinates: %v", ErrInvalid, err)
	}
	if any.point != nil {
		return [][]Point{{*any.point}}, nil
	}
	out := make([][]Point, 0, len(any.list))
	for _, c := range any.list {
		if c.point != nil {
			out = append(out, []Point{*c.point})
			continue
		}
		line := make([]Point, 0, len(c.list))
		for _, p := range c.list {
			if p.point == nil {
				return nil, fmt.Errorf("%w: malformed coordinates", ErrInvalid)
			}
			line = append(line, *p.point)
		}
		if len(line) > 0 {
			out = append(out, line)
		}
	}
	return out, nil
}

// parseMultiPolygon 解析 [[[x,y],…],…]（多边形列表）。
func parseMultiPolygon(raw json.RawMessage) ([][][]Point, error) {
	var polys []json.RawMessage
	if err := json.Unmarshal(raw, &polys); err != nil {
		return nil, fmt.Errorf("%w: invalid multipolygon: %v", ErrInvalid, err)
	}
	out := make([][][]Point, 0, len(polys))
	for _, p := range polys {
		rings, err := parseMultiLine(p)
		if err != nil {
			return nil, err
		}
		out = append(out, rings)
	}
	return out, nil
}

// coordsAny 自适应坐标树：叶子为点，否则为子节点列表。
type coordsAny struct {
	point *Point
	list  []coordsAny
}

// UnmarshalJSON 递归解析任意深度的数值数组。
func (c *coordsAny) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '[' {
		// 尝试按点解析（全为数值）
		var nums []float64
		if err := json.Unmarshal(b, &nums); err == nil {
			if len(nums) >= 2 {
				p := Point{X: nums[0], Y: nums[1]}
				c.point = &p
			}
			return nil
		}
		var rawList []json.RawMessage
		if err := json.Unmarshal(b, &rawList); err != nil {
			return err
		}
		c.list = make([]coordsAny, 0, len(rawList))
		for _, r := range rawList {
			var child coordsAny
			if err := child.UnmarshalJSON(r); err != nil {
				return err
			}
			c.list = append(c.list, child)
		}
		return nil
	}
	return fmt.Errorf("%w: unexpected coordinates token", ErrInvalid)
}
