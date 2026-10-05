// wkb.go 几何 → WKB（OGC Well-Known Binary）编码。
//
// 与 geopackage.go 里的读取对称：GeoPackage 的几何列就是「GP 头 + WKB」，
// 导出时必须自己拼出来。统一小端、只写 XY（Z/M 信息我们本来就没解析过，
// 导出时如实丢弃而不是补 0 假装三维）。
package vector

import (
	"encoding/binary"
	"math"
)

// wkbWriter 小端 WKB 写入。
type wkbWriter struct{ buf []byte }

func (w *wkbWriter) u32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *wkbWriter) f64(v float64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	w.buf = append(w.buf, b[:]...)
}

// header 写字节序标记 + 几何类型。
func (w *wkbWriter) header(geomType uint32) {
	w.buf = append(w.buf, 1) // 1 = 小端
	w.u32(geomType)
}

// points 写一串坐标（先写计数）。
func (w *wkbWriter) points(pts []Point) {
	w.u32(uint32(len(pts)))
	for _, p := range pts {
		w.f64(p.X)
		w.f64(p.Y)
	}
}

// rings 写一组环（先写计数）。
func (w *wkbWriter) rings(rings [][]Point) {
	w.u32(uint32(len(rings)))
	for _, r := range rings {
		w.points(r)
	}
}

// wkbEncode 几何 → WKB 字节；几何为空返回 nil。
//
// 类型按 OGC 规范选择单/多形式：
//
//	点：单点 Point(1)，多点 MultiPoint(4)
//	线：单线 LineString(2)，多线 MultiLineString(5)
//	面：单面 Polygon(3)，多面 MultiPolygon(6)
func wkbEncode(g Geometry) []byte {
	w := &wkbWriter{}
	switch g.Type {
	case GeomPoint:
		var pts []Point
		for _, line := range g.Lines {
			pts = append(pts, line...)
		}
		if len(pts) == 0 {
			return nil
		}
		if len(pts) == 1 {
			w.header(1)
			w.f64(pts[0].X)
			w.f64(pts[0].Y)
			return w.buf
		}
		w.header(4)
		w.points(pts)
		return w.buf

	case GeomLine:
		lines := nonEmptyLines(g.Lines)
		if len(lines) == 0 {
			return nil
		}
		if len(lines) == 1 {
			w.header(2)
			w.points(lines[0])
			return w.buf
		}
		w.header(5)
		w.u32(uint32(len(lines)))
		for _, l := range lines {
			w.header(2)
			w.points(l)
		}
		return w.buf

	case GeomPolygon:
		polys := groupPolygonRings(g)
		if len(polys) == 0 {
			return nil
		}
		if len(polys) == 1 {
			w.header(3)
			w.rings(polys[0])
			return w.buf
		}
		w.header(6)
		w.u32(uint32(len(polys)))
		for _, p := range polys {
			w.header(3)
			w.rings(p)
		}
		return w.buf
	}
	return nil
}

// nonEmptyLines 过滤掉不足两点的折线。
func nonEmptyLines(lines [][]Point) [][]Point {
	out := make([][]Point, 0, len(lines))
	for _, l := range lines {
		if len(l) >= 2 {
			out = append(out, l)
		}
	}
	return out
}

// groupPolygonRings 把「外环、内环…、外环、内环…」的环序列还原成多边形分组。
//
// 环角色来自解析阶段（Geometry.Exterior）：包含 GIS 语义的判定在那里做过一次，
// 这里只按角色切分，不再重新推断。
func groupPolygonRings(g Geometry) [][][]Point {
	var polys [][][]Point
	var cur [][]Point
	for i, ring := range g.Lines {
		if len(ring) < 3 {
			continue
		}
		if g.isExterior(i) || len(cur) == 0 {
			if len(cur) > 0 {
				polys = append(polys, cur)
			}
			cur = [][]Point{}
		}
		cur = append(cur, ring)
	}
	if len(cur) > 0 {
		polys = append(polys, cur)
	}
	return polys
}
