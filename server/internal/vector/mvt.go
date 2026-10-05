// mvt.go 纯 Go 的 Mapbox Vector Tile（MVT 2.1）编码器。
//
// 为什么需要它：PostGIS 路线把 MVT 编码交给 ST_AsMVT/ST_AsMVTGeom，文件矢量
// 没有数据库，必须在 Go 侧自行编码——本文件即那一层的实现（零第三方依赖，
// 手写 protobuf wire 格式）。
//
// 规范要点（MVT 2.1）：
//   - Tile.layers(3) → Layer{version(15)=2, name(1), features(2), keys(3),
//     values(4), extent(5)}
//   - Feature{id(1), tags(2)=[key_idx,val_idx,…], type(3), geometry(4)}
//   - Value 用 oneof 的 7 种字段承载 string/float/double/int/uint/sint/bool
//   - 几何：CommandInteger = (id & 0x7) | (count << 3)，id∈{MoveTo=1,
//     LineTo=2, ClosePath=7}；参数为相对光标的**增量**，且做 zigzag 编码；
//     光标在整个要素内共享（跨环/跨线延续）。
//
// 确定性：keys 排序、values 按内容去重后排序，保证同一输入产出同一字节
// —— 否则瓦片缓存与测试都无法稳定。
package vector

import (
	"encoding/binary"
	"math"
	"sort"
	"strconv"
)

// MVT 几何命令 ID。
const (
	cmdMoveTo    = 1
	cmdLineTo    = 2
	cmdClosePath = 7
)

// DefaultExtent MVT 默认瓦片内部坐标范围（与 ST_AsMVT 的 4096 一致）。
const DefaultExtent = 4096

// pbWriter 极简 protobuf wire 写入器（只实现 MVT 用到的三种 wire type）。
type pbWriter struct{ buf []byte }

func (w *pbWriter) varint(v uint64) {
	for v >= 0x80 {
		w.buf = append(w.buf, byte(v)|0x80)
		v >>= 7
	}
	w.buf = append(w.buf, byte(v))
}

// tag 写字段头：field_number << 3 | wire_type。
func (w *pbWriter) tag(field, wire int) { w.varint(uint64(field)<<3 | uint64(wire)) }

// fieldVarint 写 varint 字段（wire type 0）。
func (w *pbWriter) fieldVarint(field int, v uint64) {
	w.tag(field, 0)
	w.varint(v)
}

// fieldBytes 写 length-delimited 字段（wire type 2）。
func (w *pbWriter) fieldBytes(field int, b []byte) {
	w.tag(field, 2)
	w.varint(uint64(len(b)))
	w.buf = append(w.buf, b...)
}

// fieldString 写字符串字段。
func (w *pbWriter) fieldString(field int, s string) {
	w.tag(field, 2)
	w.varint(uint64(len(s)))
	w.buf = append(w.buf, s...)
}

// fieldFixed64 写 fixed64（double）。
func (w *pbWriter) fieldFixed64(field int, v float64) {
	w.tag(field, 1)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	w.buf = append(w.buf, b[:]...)
}

// zigzag 有符号整数编码（int32 → uint32）。
func zigzag(n int32) uint32 { return uint32((n << 1) ^ (n >> 31)) }

// MVTFeature 一个待编码要素（几何须已是**瓦片坐标**，即 clipGeometry 的输出）。
type MVTFeature struct {
	ID    uint64
	Geom  Geometry
	Props map[string]any
}

// EncodeMVT 把要素编码为单图层 MVT 瓦片字节；要素为空返回 nil（调用方按空瓦片处理）。
//
// extent 为瓦片内部坐标范围（0 取 DefaultExtent）。
//
// **前置条件**：入参几何必须已处于瓦片坐标（0..extent）且多边形的环缠绕方向
// 已按 MVT 规范归一化——这两件事统一由 clipGeometry 完成（它同时负责投影、
// 裁剪与绕向纠正）。本函数只做编码，不改写顶点，以保证「所见即所编」。
func EncodeMVT(layerName string, extent int, feats []MVTFeature) []byte {
	if extent <= 0 {
		extent = DefaultExtent
	}
	// 过滤掉没有有效几何的要素（裁剪后可能为空）
	valid := make([]MVTFeature, 0, len(feats))
	for _, f := range feats {
		if hasGeometry(f.Geom) {
			valid = append(valid, f)
		}
	}
	if len(valid) == 0 {
		return nil
	}

	keys, values, dict := buildDict(valid)

	var layer pbWriter
	layer.fieldVarint(15, 2) // version = 2
	layer.fieldString(1, layerName)
	for i := range valid {
		fb := encodeFeature(&valid[i], keys, dict)
		layer.fieldBytes(2, fb)
	}
	for _, k := range keys {
		layer.fieldString(3, k)
	}
	for _, v := range values {
		layer.fieldBytes(4, encodeValue(v))
	}
	layer.fieldVarint(5, uint64(extent))

	var tile pbWriter
	tile.fieldBytes(3, layer.buf)
	return tile.buf
}

// hasGeometry 报告几何裁剪后是否仍有内容。
func hasGeometry(g Geometry) bool {
	switch g.Type {
	case GeomPoint:
		return len(g.Lines) > 0
	case GeomLine:
		for _, l := range g.Lines {
			if len(l) >= 2 {
				return true
			}
		}
		return false
	case GeomPolygon:
		for _, r := range g.Lines {
			if len(r) >= 3 {
				return true
			}
		}
		return false
	}
	return false
}

// dictIndex 属性字典下标：key 名 → 下标，value 内容 → 下标。
type dictIndex struct {
	keys   map[string]int
	values map[string]int
}

// buildDict 收集全部属性键值并排序，产出确定性的字典与索引。
func buildDict(feats []MVTFeature) ([]string, []any, *dictIndex) {
	keySet := map[string]bool{}
	valSet := map[string]any{}
	for _, f := range feats {
		for k, v := range f.Props {
			if v == nil {
				continue
			}
			if _, ok := scalarize(v); !ok {
				continue // 不支持的类型（数组/对象）直接跳过，不写进瓦片
			}
			keySet[k] = true
			valSet[valueKey(v)] = v
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	valKeys := make([]string, 0, len(valSet))
	for k := range valSet {
		valKeys = append(valKeys, k)
	}
	sort.Strings(valKeys)
	values := make([]any, len(valKeys))
	for i, k := range valKeys {
		values[i] = valSet[k]
	}

	dict := &dictIndex{keys: map[string]int{}, values: map[string]int{}}
	for i, k := range keys {
		dict.keys[k] = i
	}
	for i, k := range valKeys {
		dict.values[k] = i
	}
	return keys, values, dict
}

// encodeFeature 编码单个要素（tags 按 key 下标升序，保证确定性）。
func encodeFeature(f *MVTFeature, keys []string, dict *dictIndex) []byte {
	var b pbWriter
	if f.ID > 0 {
		b.fieldVarint(1, f.ID)
	}

	// tags：只收录在字典里的键（值类型不支持时已被排除）
	type kv struct{ k, v int }
	pairs := make([]kv, 0, len(f.Props))
	for name, val := range f.Props {
		ki, ok := dict.keys[name]
		if !ok {
			continue
		}
		vi, ok := dict.values[valueKey(val)]
		if !ok {
			continue
		}
		pairs = append(pairs, kv{ki, vi})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })

	var packed pbWriter
	for _, p := range pairs {
		packed.varint(uint64(p.k))
		packed.varint(uint64(p.v))
	}
	if len(packed.buf) > 0 {
		b.fieldBytes(2, packed.buf)
	}

	b.fieldVarint(3, uint64(f.Geom.Type))

	var geom pbWriter
	encodeGeometry(&geom, f.Geom)
	if len(geom.buf) > 0 {
		b.fieldBytes(4, geom.buf)
	}
	return b.buf
}

// encodeGeometry 按 MVT 几何命令流编码（光标跨环/跨线共享）。
func encodeGeometry(w *pbWriter, g Geometry) {
	var cx, cy int32
	emit := func(cmd int, count int) { w.varint(uint64(cmd&0x7) | uint64(count)<<3) }
	move := func(p Point) {
		x, y := int32(math.Round(p.X)), int32(math.Round(p.Y))
		w.varint(uint64(zigzag(x - cx)))
		w.varint(uint64(zigzag(y - cy)))
		cx, cy = x, y
	}
	lineTo := func(pts []Point) {
		if len(pts) == 0 {
			return
		}
		emit(cmdLineTo, len(pts))
		for _, p := range pts {
			move(p)
		}
	}

	switch g.Type {
	case GeomPoint:
		var pts []Point
		for _, line := range g.Lines {
			pts = append(pts, line...)
		}
		if len(pts) == 0 {
			return
		}
		emit(cmdMoveTo, len(pts))
		for _, p := range pts {
			move(p)
		}
	case GeomLine:
		for _, line := range g.Lines {
			if len(line) < 2 {
				continue
			}
			emit(cmdMoveTo, 1)
			move(line[0])
			lineTo(line[1:])
		}
	case GeomPolygon:
		for _, ring := range g.Lines {
			r := ring
			// 去掉显式闭合点：闭合由 ClosePath 表达
			if len(r) >= 2 && r[0] == r[len(r)-1] {
				r = r[:len(r)-1]
			}
			if len(r) < 3 {
				continue
			}
			emit(cmdMoveTo, 1)
			move(r[0])
			lineTo(r[1:])
			emit(cmdClosePath, 1)
		}
	}
}

// encodeValue 编码属性值（按类型选最合适的 oneof 字段）。
func encodeValue(v any) []byte {
	var b pbWriter
	switch t := v.(type) {
	case string:
		b.fieldString(1, t)
	case bool:
		if t {
			b.fieldVarint(7, 1)
		} else {
			b.fieldVarint(7, 0)
		}
	case float64:
		// 整数值用 int/sint 承载（JSON 数字常见），其余用 double
		if t == math.Trunc(t) && !math.IsInf(t, 0) && math.Abs(t) < 1e15 {
			n := int64(t)
			if n >= 0 {
				b.fieldVarint(4, uint64(n))
			} else {
				b.fieldVarint(6, uint64(zigzag(int32(n))))
			}
			break
		}
		b.fieldFixed64(3, t)
	case float32:
		b.fieldFixed64(3, float64(t))
	case int:
		if t >= 0 {
			b.fieldVarint(4, uint64(t))
		} else {
			b.fieldVarint(6, uint64(zigzag(int32(t))))
		}
	case int64:
		if t >= 0 {
			b.fieldVarint(4, uint64(t))
		} else {
			b.fieldVarint(6, uint64(zigzag(int32(t))))
		}
	case uint64:
		b.fieldVarint(5, t)
	default:
		b.fieldString(1, valueKey(v))
	}
	return b.buf
}

// scalarize 报告值是否可入瓦片（数组/对象等复合值不入属性）。
func scalarize(v any) (any, bool) {
	switch v.(type) {
	case string, bool, float64, float32, int, int64, uint64:
		return v, true
	}
	return nil, false
}

// valueKey 值的内容指纹（用于字典去重与索引）。
func valueKey(v any) string {
	switch t := v.(type) {
	case string:
		return "s:" + t
	case bool:
		return "b:" + strconv.FormatBool(t)
	case float64:
		return "f:" + strconv.FormatFloat(t, 'g', -1, 64)
	case float32:
		return "f:" + strconv.FormatFloat(float64(t), 'g', -1, 32)
	case int:
		return "i:" + strconv.Itoa(t)
	case int64:
		return "i:" + strconv.FormatInt(t, 10)
	case uint64:
		return "u:" + strconv.FormatUint(t, 10)
	}
	return "x:" + strconv.Quote(fmtAny(v))
}

func fmtAny(v any) string {
	if s, ok := v.(interface{ String() string }); ok {
		return s.String()
	}
	return "?"
}
