// geom.go 文件矢量的几何模型、墨卡托投影与瓦片裁剪。
//
// 与 PG 路线的区别：PostGIS 用 ST_AsMVTGeom 在库里把几何归一化到瓦片坐标并裁剪；
// 文件路线没有数据库，这一步必须在 Go 里自己做。本文件只负责「几何 → 瓦片坐标」，
// MVT 二进制编码在 mvt.go。
//
// 裁剪策略：按瓦片 bbox + buffer 做真实裁剪（多边形 Sutherland–Hodgman、
// 折线 Liang–Barsky），而不是仅仅按范围过滤要素——否则跨界要素会把超范围坐标
// 写进瓦片，渲染端在瓦片边界会出现错误连线。
package vector

import "math"

// GeomType MVT 几何类型（数值与 MVT 规范一致）。
type GeomType int

const (
	GeomUnknown GeomType = 0
	GeomPoint   GeomType = 1
	GeomLine    GeomType = 2
	GeomPolygon GeomType = 3
)

// Point 平面点（图层原生坐标系，或投影后的墨卡托米）。
type Point struct{ X, Y float64 }

// Geometry 简单几何：按 MVT 三类几何归并存储。
//
//	GeomPoint   Lines 每项含 1 个点（多点即多个单项）
//	GeomLine    Lines 每项为一条折线
//	GeomPolygon Lines 为全部环的序列（环不要求显式闭合，编码时按 ClosePath 处理）；
//	            MultiPolygon 即「外环、其内环…、下一个外环、其内环…」的拼接
type Geometry struct {
	Type  GeomType
	Lines [][]Point
	// Exterior 与 Lines 平行，标记该环是否为外环（仅 GeomPolygon 使用）。
	// MVT 规范要求：瓦片坐标下外环面积为正、内环（洞）为负——裁剪后必须按此
	// 角色重新归一化缠绕方向，否则渲染端会把外环当洞挖掉。
	Exterior []bool
}

// isExterior 报告第 i 个环是否外环（缺省按外环处理）。
func (g Geometry) isExterior(i int) bool {
	if len(g.Exterior) != len(g.Lines) {
		return i == 0 || g.Exterior == nil
	}
	return g.Exterior[i]
}

// signedArea 测量公式计算环的有向面积（瓦片坐标系，Y 轴向下）。
func signedArea(ring []Point) float64 {
	n := len(ring)
	if n < 3 {
		return 0
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		a, b := ring[i], ring[(i+1)%n]
		sum += a.X*b.Y - b.X*a.Y
	}
	return sum / 2
}

// normalizeRing 按角色强制环方向：外环正向面积，内环反向（MVT 约定）。
// 退化环（面积≈0）保持原样，由上层按顶点数决定是否丢弃。
//
// 注意两套约定并存：MVT / GeoPackage(OGC 简单要素) 要求外环**逆时针（正面积）**，
// 而 Shapefile(ESRI) 要求外环**顺时针（负面积）**——导出 Shapefile 时走
// orientRing(ring, false)。方向弄反会让洞被画成实体（见 export.go）。
func normalizeRing(ring []Point, exterior bool) []Point {
	return orientRing(ring, exterior)
}

// Box 轴对齐包围盒。
type Box struct{ MinX, MinY, MaxX, MaxY float64 }

// Empty 报告盒是否无效（未初始化）。
func (b Box) Empty() bool { return b.MinX > b.MaxX || b.MinY > b.MaxY }

// Union 合并另一个盒。
func (b Box) Union(o Box) Box {
	if b.Empty() {
		return o
	}
	if o.Empty() {
		return b
	}
	return Box{
		MinX: math.Min(b.MinX, o.MinX), MinY: math.Min(b.MinY, o.MinY),
		MaxX: math.Max(b.MaxX, o.MaxX), MaxY: math.Max(b.MaxY, o.MaxY),
	}
}

// Expand 按 d（正数）向外扩张。
func (b Box) Expand(d float64) Box {
	return Box{b.MinX - d, b.MinY - d, b.MaxX + d, b.MaxY + d}
}

// Intersects 报告两盒是否相交（接触视为相交）。
func (b Box) Intersects(o Box) bool {
	return !(o.MinX > b.MaxX || o.MaxX < b.MinX || o.MinY > b.MaxY || o.MaxY < b.MinY)
}

// Geometry 计算几何的包围盒。
func (g Geometry) Bounds() Box {
	b := Box{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	for _, line := range g.Lines {
		for _, p := range line {
			b.MinX = math.Min(b.MinX, p.X)
			b.MinY = math.Min(b.MinY, p.Y)
			b.MaxX = math.Max(b.MaxX, p.X)
			b.MaxY = math.Max(b.MaxY, p.Y)
		}
	}
	return b
}

/* ---------------- 投影 ---------------- */

// lonLatToMerc 经纬度 → Web Mercator 米（纬度截断到 ±85.051…）。
func lonLatToMerc(lon, lat float64) (x, y float64) {
	const half = 20037508.342789244
	const latLimit = 85.05112877980659
	if lat > latLimit {
		lat = latLimit
	}
	if lat < -latLimit {
		lat = -latLimit
	}
	x = lon * half / 180
	y = math.Log(math.Tan((90+lat)*math.Pi/360)) * half / math.Pi
	return
}

// projectToMerc 把图层原生坐标转到 Web Mercator。
//
// 支持三种源坐标系（与 supportedSRID 一致）：
//
//	4326 WGS84      经纬度 → 墨卡托
//	4490 CGCS2000   经纬度 → 墨卡托（与 WGS84 差在厘米级，可直接按经纬度处理）
//	3857 WebMercator 已是墨卡托米，原样返回
//
// ⚠️ 4490 必须显式列出：早先这里只判 `srid == 4326`、其余一律"当墨卡托米用"，
// 于是 4490 的经纬度被当成米，瓦片范围（百万米量级）与要素坐标（百量级）
// 完全对不上，出图恒为空 tile（测试暴露）。
func projectToMerc(p Point, srid int) Point {
	switch srid {
	case 4326, 4490:
		x, y := lonLatToMerc(p.X, p.Y)
		return Point{x, y}
	default:
		return p // 3857：已是墨卡托米
	}
}

// projectGeometry 整几何投影到墨卡托（保留环角色）。
func projectGeometry(g Geometry, srid int) Geometry {
	if srid == 3857 {
		return g
	}
	out := Geometry{Type: g.Type, Lines: make([][]Point, 0, len(g.Lines)), Exterior: g.Exterior}
	for _, line := range g.Lines {
		nl := make([]Point, len(line))
		for i, p := range line {
			nl[i] = projectToMerc(p, srid)
		}
		out.Lines = append(out.Lines, nl)
	}
	return out
}

/* ---------------- 瓦片坐标 ---------------- */

// tileTransform 瓦片 bbox（墨卡托米）→ MVT 瓦片坐标（0..extent，Y 轴向下）。
type tileTransform struct {
	box    Box
	extent float64
}

// toTile 墨卡托米 → 瓦片坐标。
func (t tileTransform) toTile(p Point) Point {
	span := t.box.MaxX - t.box.MinX
	return Point{
		X: (p.X - t.box.MinX) / span * t.extent,
		Y: (t.box.MaxY - p.Y) / span * t.extent, // Y 翻转：墨卡托北为上，瓦片北为 0
	}
}

/* ---------------- 裁剪 ---------------- */

// clipGeometry 把几何裁剪到瓦片盒（含 buffer），返回可能为空的几何。
// 同时完成墨卡托 → 瓦片坐标的转换。
func clipGeometry(g Geometry, srid int, box Box, extent, buffer float64) Geometry {
	merc := projectGeometry(g, srid)
	// 裁剪前先按 buffer 扩张瓦片盒：让跨界要素在相邻瓦片间自然重叠，
	// 避免渲染端在瓦片接缝处出现断裂（与 ST_AsMVTGeom 的 buffer 语义一致）。
	clipBox := box.Expand(box.MaxX - box.MinX)
	if buffer > 0 && extent > 0 {
		clipBox = box.Expand((box.MaxX - box.MinX) * buffer / extent)
	}
	if !clipBox.Intersects(merc.Bounds()) {
		return Geometry{Type: g.Type}
	}

	tr := tileTransform{box: box, extent: extent}
	out := Geometry{Type: g.Type}
	switch g.Type {
	case GeomPoint:
		for _, line := range merc.Lines {
			for _, p := range line {
				if p.X < clipBox.MinX || p.X > clipBox.MaxX || p.Y < clipBox.MinY || p.Y > clipBox.MaxY {
					continue
				}
				out.Lines = append(out.Lines, []Point{tr.toTile(p)})
			}
		}
	case GeomLine:
		for _, line := range merc.Lines {
			for _, seg := range clipLine(line, clipBox) {
				if len(seg) >= 2 {
					out.Lines = append(out.Lines, toTileLine(seg, tr))
				}
			}
		}
	case GeomPolygon:
		for i, ring := range merc.Lines {
			clipped := clipRing(ring, clipBox)
			if len(clipped) < 3 {
				out.Lines = append(out.Lines, nil)
				out.Exterior = append(out.Exterior, g.isExterior(i))
				continue
			}
			tiled := toTileLine(clipped, tr)
			// 裁剪与 Y 翻转都会改变绕向，必须按角色重新归一化
			tiled = normalizeRing(tiled, g.isExterior(i))
			out.Lines = append(out.Lines, tiled)
			out.Exterior = append(out.Exterior, g.isExterior(i))
		}
	}
	return out
}

// toTileLine 批量转换坐标。
func toTileLine(line []Point, tr tileTransform) []Point {
	out := make([]Point, len(line))
	for i, p := range line {
		out[i] = tr.toTile(p)
	}
	return out
}

const clipEps = 1e-9

// clipRing Sutherland–Hodgman：对凸裁剪窗口逐边裁剪多边形环。
// 输入环可为开环；输出为开环（编码时闭合）。
func clipRing(ring []Point, box Box) []Point {
	if len(ring) < 3 {
		return nil
	}
	// 去掉重复的闭合点，统一按开环处理
	if ring[0] == ring[len(ring)-1] {
		ring = ring[:len(ring)-1]
	}
	edges := [][4]float64{
		{box.MinX, 0, 1, 0},  // x >= MinX
		{box.MaxX, 0, -1, 0}, // x <= MaxX
		{0, box.MinY, 0, 1},  // y >= MinY
		{0, box.MaxY, 0, -1}, // y <= MaxY
	}
	out := ring
	for _, e := range edges {
		if len(out) == 0 {
			return nil
		}
		out = clipPolygonEdge(out, e[0], e[1], e[2], e[3])
	}
	if len(out) < 3 {
		return nil
	}
	return out
}

// clipPolygonEdge 对一条无限直线（点 + 法线）做半平面裁剪，内部为法线侧。
func clipPolygonEdge(poly []Point, px, py, nx, ny float64) []Point {
	inside := func(p Point) bool { return (p.X-px)*nx+(p.Y-py)*ny >= -clipEps }
	out := make([]Point, 0, len(poly)+2)
	for i := 0; i < len(poly); i++ {
		cur := poly[i]
		prev := poly[(i+len(poly)-1)%len(poly)]
		curIn, prevIn := inside(cur), inside(prev)
		switch {
		case curIn && prevIn:
			out = append(out, cur)
		case curIn && !prevIn:
			if ip, ok := intersectLine(prev, cur, px, py, nx, ny); ok {
				out = append(out, ip)
			}
			out = append(out, cur)
		case !curIn && prevIn:
			if ip, ok := intersectLine(prev, cur, px, py, nx, ny); ok {
				out = append(out, ip)
			}
		}
	}
	return out
}

// intersectLine 线段与直线（点 + 法线）求交。
func intersectLine(a, b Point, px, py, nx, ny float64) (Point, bool) {
	da := (a.X-px)*nx + (a.Y-py)*ny
	db := (b.X-px)*nx + (b.Y-py)*ny
	if math.Abs(da-db) < clipEps {
		return Point{}, false
	}
	t := da / (da - db)
	return Point{X: a.X + t*(b.X-a.X), Y: a.Y + t*(b.Y-a.Y)}, true
}

// clipLine 折线裁剪（Liang–Barsky 逐段），返回若干条连续子折线。
func clipLine(line []Point, box Box) [][]Point {
	if len(line) < 2 {
		return nil
	}
	var out [][]Point
	var cur []Point
	flush := func() {
		if len(cur) >= 2 {
			out = append(out, cur)
		}
		cur = nil
	}
	for i := 0; i+1 < len(line); i++ {
		p0, p1 := line[i], line[i+1]
		a, b, ok := clipSegment(p0, p1, box)
		if !ok {
			flush()
			continue
		}
		if len(cur) == 0 {
			cur = append(cur, a, b)
			continue
		}
		// 与上一段首尾相接则延续，否则断开
		if cur[len(cur)-1] == a {
			cur = append(cur, b)
		} else {
			flush()
			cur = append(cur, a, b)
		}
	}
	flush()
	return out
}

// clipSegment Liang–Barsky 单段裁剪。
func clipSegment(a, b Point, box Box) (Point, Point, bool) {
	dx, dy := b.X-a.X, b.Y-a.Y
	t0, t1 := 0.0, 1.0
	for _, e := range [4][3]float64{
		{-dx, a.X - box.MinX, 1}, // x >= MinX
		{dx, box.MaxX - a.X, 1},  // x <= MaxX
		{-dy, a.Y - box.MinY, 1}, // y >= MinY
		{dy, box.MaxY - a.Y, 1},  // y <= MaxY
	} {
		p, q := e[0], e[1]
		if math.Abs(p) < clipEps {
			if q < 0 {
				return Point{}, Point{}, false // 平行且在外
			}
			continue
		}
		r := q / p
		if p < 0 {
			if r > t1 {
				return Point{}, Point{}, false
			}
			if r > t0 {
				t0 = r
			}
		} else {
			if r < t0 {
				return Point{}, Point{}, false
			}
			if r < t1 {
				t1 = r
			}
		}
	}
	return Point{X: a.X + t0*dx, Y: a.Y + t0*dy}, Point{X: a.X + t1*dx, Y: a.Y + t1*dy}, true
}

// Contains 报告 b 是否被本盒包含（相等不算"严格包含"，用于环嵌套判定）。
func (b Box) Contains(o Box) bool {
	if b.Empty() || o.Empty() {
		return false
	}
	if o.MinX <= b.MinX || o.MaxX >= b.MaxX || o.MinY <= b.MinY || o.MaxY >= b.MaxY {
		return false
	}
	return true
}

// ringBounds 计算环的包围盒。
func ringBounds(ring []Point) Box {
	b := Box{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	for _, p := range ring {
		b.MinX = math.Min(b.MinX, p.X)
		b.MinY = math.Min(b.MinY, p.Y)
		b.MaxX = math.Max(b.MaxX, p.X)
		b.MaxY = math.Max(b.MaxY, p.Y)
	}
	return b
}

// pointInRing 射线法判断点是否在环内（环视为闭合）。
func pointInRing(ring []Point, p Point) bool {
	inside := false
	n := len(ring)
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		pi, pj := ring[i], ring[j]
		if (pi.Y > p.Y) != (pj.Y > p.Y) {
			x := (pj.X-pi.X)*(p.Y-pi.Y)/(pj.Y-pi.Y) + pi.X
			if p.X < x {
				inside = !inside
			}
		}
	}
	return inside
}

// classifyRings 按**包含关系**判定多边形的环角色（true = 外环，false = 内环/洞）。
//
// 为什么不看绕向：ESRI 规范要求外环顺时针、内环逆时针，但实际数据里约定常常
// 不被遵守（不同导出工具、经手工编辑的数据都可能反），按绕向判会把整个多边形
// 判反。包含关系与绕向无关，只依赖几何事实。
//
// 复杂度 O(k²)（k 为单个要素的环数，通常个位数），判定前先用包围盒排除。
func classifyRings(rings [][]Point) []bool {
	boxes := make([]Box, len(rings))
	for i, r := range rings {
		boxes[i] = ringBounds(r)
	}
	ext := make([]bool, len(rings))
	for i := range rings {
		inside := false
		for j := range rings {
			if i == j || len(rings[i]) == 0 {
				continue
			}
			// 严格包含（排除相等包围盒，避免互相判为洞）
			if boxes[j].Contains(boxes[i]) && pointInRing(rings[j], rings[i][0]) {
				inside = true
				break
			}
		}
		ext[i] = !inside
	}
	return ext
}

// closestGeomType 汇总一组几何类型（混合时取更"复杂"的一种，用于图层类型标注）。
func closestGeomType(types map[GeomType]int) (GeomType, bool) {
	if len(types) == 0 {
		return GeomUnknown, false
	}
	// 优先级：Polygon > Line > Point（与 OGR 的图层类型归并直觉一致）
	order := []GeomType{GeomPolygon, GeomLine, GeomPoint}
	for _, t := range order {
		if types[t] > 0 {
			return t, true
		}
	}
	return GeomUnknown, false
}
