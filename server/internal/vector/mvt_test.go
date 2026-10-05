package vector

import (
	"encoding/binary"
	"math"
	"testing"
)

/* ------------------------------------------------------------------ */
/* 最小 MVT 解码器（测试专用）：证明编码结果可被标准 protobuf 解析       */
/* ------------------------------------------------------------------ */

const (
	wtVarint = 0
	wtI64    = 1
	wtBytes  = 2
)

type pbReader struct {
	b   []byte
	pos int
}

func (r *pbReader) eof() bool { return r.pos >= len(r.b) }

func (r *pbReader) varint() (uint64, bool) {
	var v uint64
	var shift uint
	for {
		if r.pos >= len(r.b) || shift > 63 {
			return 0, false
		}
		c := r.b[r.pos]
		r.pos++
		v |= uint64(c&0x7F) << shift
		if c < 0x80 {
			return v, true
		}
		shift += 7
	}
}

// next 读一个字段，返回 (field, wire, varint 值, bytes 值)。
func (r *pbReader) next() (int, int, uint64, []byte, bool) {
	key, ok := r.varint()
	if !ok {
		return 0, 0, 0, nil, false
	}
	field, wire := int(key>>3), int(key&0x7)
	switch wire {
	case wtVarint:
		v, ok := r.varint()
		if !ok {
			return 0, 0, 0, nil, false
		}
		return field, wire, v, nil, true
	case wtI64:
		if r.pos+8 > len(r.b) {
			return 0, 0, 0, nil, false
		}
		v := binary.LittleEndian.Uint64(r.b[r.pos : r.pos+8])
		r.pos += 8
		return field, wire, v, nil, true
	case wtBytes:
		n, ok := r.varint()
		if !ok || r.pos+int(n) > len(r.b) {
			return 0, 0, 0, nil, false
		}
		b := r.b[r.pos : r.pos+int(n)]
		r.pos += int(n)
		return field, wire, 0, b, true
	default:
		return 0, 0, 0, nil, false
	}
}

type dValue struct {
	kind string // string / double / int / sint / uint / bool
	s    string
	num  float64
}

type dFeature struct {
	id    uint64
	tags  []uint64
	gtype uint64
	cmds  []uint64
}

type dLayer struct {
	name     string
	version  uint64
	extent   uint64
	keys     []string
	values   []dValue
	features []dFeature
}

// decodeMVT 解析瓦片字节（单图层），用于验证编码正确性。
func decodeMVT(t *testing.T, data []byte) *dLayer {
	t.Helper()
	tr := &pbReader{b: data}
	var layerBytes []byte
	for !tr.eof() {
		field, wire, v, b, ok := tr.next()
		if !ok {
			t.Fatalf("tile 解析失败 @%d", tr.pos)
		}
		if field == 3 && wire == wtBytes {
			layerBytes = b
		}
		_ = v
	}
	if layerBytes == nil {
		t.Fatal("瓦片里没有 Layer(3)")
	}

	lr := &pbReader{b: layerBytes}
	layer := &dLayer{}
	for !lr.eof() {
		field, wire, v, b, ok := lr.next()
		if !ok {
			t.Fatalf("layer 解析失败 @%d", lr.pos)
		}
		switch {
		case field == 15 && wire == wtVarint:
			layer.version = v
		case field == 5 && wire == wtVarint:
			layer.extent = v
		case field == 1 && wire == wtBytes:
			layer.name = string(b)
		case field == 3 && wire == wtBytes:
			layer.keys = append(layer.keys, string(b))
		case field == 4 && wire == wtBytes:
			layer.values = append(layer.values, decodeValue(t, b))
		case field == 2 && wire == wtBytes:
			layer.features = append(layer.features, decodeFeature(t, b))
		}
	}
	return layer
}

func decodeValue(t *testing.T, b []byte) dValue {
	t.Helper()
	r := &pbReader{b: b}
	out := dValue{kind: "unknown"}
	for !r.eof() {
		field, _, v, bs, ok := r.next()
		if !ok {
			t.Fatalf("value 解析失败")
		}
		switch field {
		case 1:
			out.kind, out.s = "string", string(bs)
		case 3:
			out.kind, out.num = "double", math.Float64frombits(v)
		case 4:
			out.kind, out.num = "int", float64(int64(v))
		case 5:
			out.kind, out.num = "uint", float64(v)
		case 6:
			out.kind, out.num = "sint", float64(unzigzag64(v))
		case 7:
			out.kind = "bool"
			if v == 1 {
				out.num = 1
			}
		}
	}
	return out
}

func decodeFeature(t *testing.T, b []byte) dFeature {
	t.Helper()
	r := &pbReader{b: b}
	f := dFeature{}
	for !r.eof() {
		field, wire, v, bs, ok := r.next()
		if !ok {
			t.Fatalf("feature 解析失败")
		}
		switch {
		case field == 1 && wire == wtVarint:
			f.id = v
		case field == 2 && wire == wtBytes:
			f.tags = append(f.tags, unpackVarints(bs)...)
		case field == 3 && wire == wtVarint:
			f.gtype = v
		case field == 4 && wire == wtBytes:
			f.cmds = append(f.cmds, unpackVarints(bs)...)
		}
	}
	return f
}

func unpackVarints(b []byte) []uint64 {
	r := &pbReader{b: b}
	var out []uint64
	for !r.eof() {
		v, ok := r.varint()
		if !ok {
			break
		}
		out = append(out, v)
	}
	return out
}

func unzigzag64(v uint64) int64 {
	return int64(v>>1) ^ -int64(v&1)
}

// geomOp 解码后的几何命令事件。
type geomOp struct {
	op   int
	pt   Point
	ring int // 所属 part 序号（MoveTo 递增）
}

// decodeGeomOps 把几何命令流解回事件序列。
func decodeGeomOps(t *testing.T, cmds []uint64) []geomOp {
	t.Helper()
	var ops []geomOp
	var cx, cy int64
	ring := -1
	i := 0
	for i < len(cmds) {
		cmd := cmds[i]
		id := int(cmd & 0x7)
		count := int(cmd >> 3)
		i++
		switch id {
		case cmdMoveTo:
			for k := 0; k < count; k++ {
				if i+2 > len(cmds) {
					t.Fatalf("MoveTo 参数不足")
				}
				dx := unzigzag64(cmds[i])
				dy := unzigzag64(cmds[i+1])
				i += 2
				cx += dx
				cy += dy
				ring++
				ops = append(ops, geomOp{op: cmdMoveTo, pt: Point{X: float64(cx), Y: float64(cy)}, ring: ring})
			}
		case cmdLineTo:
			for k := 0; k < count; k++ {
				if i+2 > len(cmds) {
					t.Fatalf("LineTo 参数不足")
				}
				dx := unzigzag64(cmds[i])
				dy := unzigzag64(cmds[i+1])
				i += 2
				cx += dx
				cy += dy
				ops = append(ops, geomOp{op: cmdLineTo, pt: Point{X: float64(cx), Y: float64(cy)}, ring: ring})
			}
		case cmdClosePath:
			ops = append(ops, geomOp{op: cmdClosePath, ring: ring})
		default:
			t.Fatalf("未知几何命令 %d", id)
		}
	}
	return ops
}

// ringsOf 从事件序列还原各环（ClosePath 之前的部分）。
func ringsOf(ops []geomOp) [][]Point {
	var rings [][]Point
	var cur []Point
	for _, o := range ops {
		switch o.op {
		case cmdMoveTo:
			if len(cur) > 0 {
				rings = append(rings, cur)
			}
			cur = []Point{o.pt}
		case cmdLineTo:
			cur = append(cur, o.pt)
		case cmdClosePath:
			if len(cur) > 0 {
				rings = append(rings, cur)
				cur = nil
			}
		}
	}
	if len(cur) > 0 {
		rings = append(rings, cur)
	}
	return rings
}

/* ------------------------------------------------------------------ */
/* 编码测试                                                            */
/* ------------------------------------------------------------------ */

// TestEncodeMVTRoundTrip 要素编码 → 标准 protobuf 解码，字段与属性应完整还原。
func TestEncodeMVTRoundTrip(t *testing.T) {
	feats := []MVTFeature{
		{
			ID:   7,
			Geom: Geometry{Type: GeomPoint, Lines: [][]Point{{{X: 10, Y: 20}}, {{X: 30, Y: 40}}}},
			Props: map[string]any{
				"name": "站点A",
				"rank": float64(3),
				"ok":   true,
			},
		},
	}
	data := EncodeMVT("poi", DefaultExtent, feats)
	if len(data) == 0 {
		t.Fatal("编码结果为空")
	}
	layer := decodeMVT(t, data)
	if layer.name != "poi" {
		t.Errorf("layer name=%q, want poi", layer.name)
	}
	if layer.version != 2 {
		t.Errorf("version=%d, want 2", layer.version)
	}
	if layer.extent != 4096 {
		t.Errorf("extent=%d, want 4096", layer.extent)
	}
	if len(layer.features) != 1 {
		t.Fatalf("features=%d, want 1", len(layer.features))
	}

	f := layer.features[0]
	if f.id != 7 {
		t.Errorf("feature id=%d, want 7", f.id)
	}
	if f.gtype != uint64(GeomPoint) {
		t.Errorf("geometry type=%d, want %d", f.gtype, GeomPoint)
	}
	// tags 成对出现且指向正确字典项
	if len(f.tags)%2 != 0 {
		t.Fatalf("tags 长度应为偶数: %v", f.tags)
	}
	got := map[string]dValue{}
	for i := 0; i+1 < len(f.tags); i += 2 {
		got[layer.keys[f.tags[i]]] = layer.values[f.tags[i+1]]
	}
	if got["name"].kind != "string" || got["name"].s != "站点A" {
		t.Errorf("name 属性 = %+v", got["name"])
	}
	if got["rank"].num != 3 {
		t.Errorf("rank 属性 = %+v", got["rank"])
	}
	if got["ok"].kind != "bool" || got["ok"].num != 1 {
		t.Errorf("ok 属性 = %+v", got["ok"])
	}

	// 几何：多点用「一个 MoveTo(count=2)」表达，解码后为两个 MoveTo 事件
	ops := decodeGeomOps(t, f.cmds)
	if len(ops) != 2 {
		t.Fatalf("点几何应产生 2 个点事件: %+v", ops)
	}
	for i, o := range ops {
		if o.op != cmdMoveTo {
			t.Errorf("第 %d 个事件应为 MoveTo: %+v", i, o)
		}
	}
	if ops[0].pt != (Point{10, 20}) || ops[1].pt != (Point{30, 40}) {
		t.Errorf("坐标 = %+v / %+v", ops[0].pt, ops[1].pt)
	}
	// 首个命令必须是 MoveTo(count=2)
	if got := f.cmds[0]; int(got&0x7) != cmdMoveTo || int(got>>3) != 2 {
		t.Errorf("首命令 = %d（应为 MoveTo count=2）", got)
	}
}

// TestEncodeMVTPolygonCommands 多边形：MoveTo + LineTo + ClosePath 结构正确。
func TestEncodeMVTPolygonCommands(t *testing.T) {
	sq := []Point{{X: 0, Y: 0}, {X: 0, Y: 10}, {X: 10, Y: 10}, {X: 10, Y: 0}}
	feats := []MVTFeature{{Geom: Geometry{Type: GeomPolygon, Lines: [][]Point{sq}, Exterior: []bool{true}}}}
	layer := decodeMVT(t, EncodeMVT("poly", DefaultExtent, feats))
	ops := decodeGeomOps(t, layer.features[0].cmds)

	var moves, closes int
	for _, o := range ops {
		if o.op == cmdMoveTo {
			moves++
		}
		if o.op == cmdClosePath {
			closes++
		}
	}
	if moves != 1 || closes != 1 {
		t.Fatalf("MoveTo=%d ClosePath=%d, want 1/1（ops=%+v）", moves, closes, ops)
	}
	// 编码器**不改写**顶点顺序（绕向归一化由 clipGeometry 负责），
	// 因此往返后顶点序列应与输入完全一致。
	ring := ringsOf(ops)[0]
	if len(ring) != 4 {
		t.Fatalf("环点数=%d, want 4", len(ring))
	}
	for i, want := range sq {
		if ring[i] != want {
			t.Errorf("顶点 %d = %+v, want %+v（编码器不应改写顶点顺序）", i, ring[i], want)
		}
	}
}

// TestPolygonWindingNormalized 外环/内环绕向必须按 MVT 规范归一化——
// 无论输入是逆时针（RFC 7946）还是顺时针（旧 Shapefile 习惯）。
func TestPolygonWindingNormalized(t *testing.T) {
	// 逆时针外环 + 顺时针内环（RFC 7946 约定的写法）
	ccw := []Point{{X: 0, Y: 0}, {X: 20, Y: 0}, {X: 20, Y: 20}, {X: 0, Y: 20}}
	holeCW := []Point{{X: 5, Y: 5}, {X: 5, Y: 10}, {X: 10, Y: 10}, {X: 10, Y: 5}}
	if signedArea(ccw) <= 0 || signedArea(holeCW) >= 0 {
		t.Fatalf("测试数据绕向不符合预期: ext=%.1f hole=%.1f", signedArea(ccw), signedArea(holeCW))
	}

	// 用覆盖整个数据范围的瓦片，避免裁剪干扰；瓦片 bbox 为全球
	box := Box{-20037508.342789244, -20037508.342789244, 20037508.342789244, 20037508.342789244}
	g := Geometry{Type: GeomPolygon, Lines: [][]Point{ccw, holeCW}, Exterior: []bool{true, false}}
	clipped := clipGeometry(g, 4326, box, DefaultExtent, 64)
	if len(clipped.Lines) != 2 {
		t.Fatalf("环数=%d, want 2", len(clipped.Lines))
	}
	if a := signedArea(clipped.Lines[0]); a <= 0 {
		t.Errorf("瓦片坐标下外环面积应为正，得到 %.6f", a)
	}
	if a := signedArea(clipped.Lines[1]); a >= 0 {
		t.Errorf("瓦片坐标下内环面积应为负，得到 %.6f", a)
	}
}

// TestClipPolygonCrossingTile 跨瓦片多边形被真实裁剪到瓦片范围（含 buffer）。
func TestClipPolygonCrossingTile(t *testing.T) {
	// 世界范围的大方块，瓦片取 z1 的西北象限
	box := Box{MinX: -20037508.342789244, MinY: 0, MaxX: 0, MaxY: 20037508.342789244}
	big := []Point{{X: -180, Y: -85}, {X: 180, Y: -85}, {X: 180, Y: 85}, {X: -180, Y: 85}}
	g := Geometry{Type: GeomPolygon, Lines: [][]Point{big}, Exterior: []bool{true}}
	clipped := clipGeometry(g, 4326, box, DefaultExtent, 64)
	if len(clipped.Lines) != 1 || len(clipped.Lines[0]) < 3 {
		t.Fatalf("裁剪结果异常: %+v", clipped.Lines)
	}
	// 所有坐标须落在 [-buffer, extent+buffer] 内
	lim := float64(DefaultExtent) + 64 + 1
	for _, p := range clipped.Lines[0] {
		if p.X < -65 || p.X > lim || p.Y < -65 || p.Y > lim {
			t.Errorf("裁剪后坐标越界: %+v", p)
		}
	}
	// 裁剪后的西北象限多边形应覆盖瓦片的大部分
	if a := signedArea(clipped.Lines[0]); a <= 0 {
		t.Errorf("裁剪后外环面积应为正，得到 %.1f", a)
	}
}

// TestClipLineCrossingTile 跨瓦片折线裁剪为若干段。
func TestClipLineCrossingTile(t *testing.T) {
	box := Box{MinX: -20037508.342789244, MinY: 0, MaxX: 0, MaxY: 20037508.342789244}
	// 一条从西南穿到东北的线，横跨整个瓦片
	line := []Point{{X: -170, Y: -80}, {X: 170, Y: 80}}
	g := Geometry{Type: GeomLine, Lines: [][]Point{line}}
	clipped := clipGeometry(g, 4326, box, DefaultExtent, 64)
	if len(clipped.Lines) == 0 {
		t.Fatal("穿越瓦片的线不应被裁掉")
	}
	total := 0
	for _, l := range clipped.Lines {
		total += len(l)
	}
	if total < 2 {
		t.Fatalf("裁剪后点数=%d, want >=2", total)
	}
}

// TestClipDropsFarGeometry 完全在瓦片外的要素应被裁掉（空几何）。
func TestClipDropsFarGeometry(t *testing.T) {
	box := Box{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 1000}
	far := []Point{{X: 5000, Y: 5000}, {X: 5100, Y: 5000}, {X: 5100, Y: 5100}}
	g := Geometry{Type: GeomPolygon, Lines: [][]Point{far}, Exterior: []bool{true}}
	clipped := clipGeometry(g, 3857, box, DefaultExtent, 64)
	if hasGeometry(clipped) {
		t.Errorf("瓦片外几何应被裁空，得到 %+v", clipped.Lines)
	}
	if data := EncodeMVT("empty", DefaultExtent, []MVTFeature{{Geom: clipped}}); data != nil {
		t.Errorf("全部要素为空时应返回空瓦片，得到 %d 字节", len(data))
	}
}

// TestEncodeMVTDeterministic 同输入必须产出同字节（否则瓦片缓存无意义）。
func TestEncodeMVTDeterministic(t *testing.T) {
	mk := func() []MVTFeature {
		return []MVTFeature{
			{ID: 1, Geom: Geometry{Type: GeomPoint, Lines: [][]Point{{{X: 1, Y: 2}}}}, Props: map[string]any{"b": "2", "a": "1", "c": float64(3)}},
			{ID: 2, Geom: Geometry{Type: GeomPoint, Lines: [][]Point{{{X: 3, Y: 4}}}}, Props: map[string]any{"a": "1"}},
		}
	}
	first := EncodeMVT("l", DefaultExtent, mk())
	for i := 0; i < 20; i++ {
		if got := EncodeMVT("l", DefaultExtent, mk()); string(got) != string(first) {
			t.Fatalf("第 %d 次编码结果不同（字典顺序不稳定）", i)
		}
	}
}
