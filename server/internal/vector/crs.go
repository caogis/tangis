// crs.go 矢量文件的坐标系解析与投影换算。
//
// 为什么需要它：文件矢量此前只接受 EPSG:4326/4490/3857 三种"经纬度或已是墨卡托"
// 的坐标系，而国内的矢量数据大量是**投影坐标**（CGCS2000 高斯克吕格 3°/6° 带、
// 北京54、西安80…），一律被拒绝就接不进来。本文件在 Go 侧补齐这一层：
//
//	.prj / gpkg 的 WKT  →  椭球 + 基准 + 投影参数  →  反算成 WGS84 经纬度
//
// 设计取舍：
//   - **从 WKT 解析投影参数，而不是硬编码 EPSG 带号表**。WKT 本身就带着中央经线、
//     尺度因子、假东/假北、椭球参数，照它算最准，也天然覆盖非标准中央经线与
//     自定义坐标系；硬编码一张带号表既臃肿又必然漏。
//   - **基准转换按事实判断**：CGCS2000 与 WGS84 差在厘米级，可直接等同；
//     其他基准（北京54/西安80 等）若 WKT 里给了 TOWGS84 七参数就走七参数，
//     没给就**明确拒绝**而不是静默当 WGS84 —— 后者会引入 50~150m 的系统性偏差。
//   - 七参数转换需要高程，而二维矢量没有高程：按 h=0 处理并在提示里写明
//     （这是二维数据做基准转换的通行做法，代价是可能存在米级误差）。
package vector

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	deg2rad = math.Pi / 180
	rad2deg = 180 / math.Pi
	// arcSec2rad 角秒 → 弧度（七参数旋转角用角秒表示）。
	arcSec2rad = math.Pi / (180 * 3600)
)

/* ---------------- 椭球 ---------------- */

// ellipsoid 参考椭球。
type ellipsoid struct {
	Name string
	A    float64 // 长半轴（米）
	InvF float64 // 扁率倒数（0 表示球体）
}

// 常用椭球（WKT 缺失时的兜底）。
var (
	ellipsoidWGS84      = ellipsoid{"WGS 84", 6378137, 298.257223563}
	ellipsoidCGCS2000   = ellipsoid{"CGCS2000", 6378137, 298.257222101}
	ellipsoidKrassovsky = ellipsoid{"Krassovsky 1940", 6378245, 298.3}
	ellipsoidIAU76      = ellipsoid{"IAG 1975", 6378140, 298.257}
)

func (e ellipsoid) valid() bool { return e.A > 6.3e6 && e.A < 6.5e6 && e.InvF != 0 }

func (e ellipsoid) f() float64 { return 1 / e.InvF }

// e2 第一偏心率平方。
func (e ellipsoid) e2() float64 {
	f := e.f()
	return 2*f - f*f
}

// meridianArc 子午线弧长 M(φ)（Snyder 级数，截断到 e⁶，毫米级精度）。
func (e ellipsoid) meridianArc(latRad float64) float64 {
	e2 := e.e2()
	e4 := e2 * e2
	e6 := e4 * e2
	return e.A * ((1-e2/4-3*e4/64-5*e6/256)*latRad -
		(3*e2/8+3*e4/32+45*e6/1024)*math.Sin(2*latRad) +
		(15*e4/256+45*e6/1024)*math.Sin(4*latRad) -
		(35*e6/3072)*math.Sin(6*latRad))
}

// geodeticToCartesian 大地坐标（度、度、米）→ 空间直角坐标（米）。
func (e ellipsoid) geodeticToCartesian(lonDeg, latDeg, h float64) (X, Y, Z float64) {
	lon, lat := lonDeg*deg2rad, latDeg*deg2rad
	e2 := e.e2()
	sinLat, cosLat := math.Sin(lat), math.Cos(lat)
	N := e.A / math.Sqrt(1-e2*sinLat*sinLat)
	X = (N + h) * cosLat * math.Cos(lon)
	Y = (N + h) * cosLat * math.Sin(lon)
	Z = (N*(1-e2) + h) * sinLat
	return
}

// cartesianToGeodetic 空间直角坐标 → 大地坐标（Bowring 直接解，毫米级）。
func (e ellipsoid) cartesianToGeodetic(X, Y, Z float64) (lonDeg, latDeg float64) {
	lon := math.Atan2(Y, X)
	p := math.Hypot(X, Y)
	e2 := e.e2()
	b := e.A * (1 - e.f())
	ep2 := (e.A*e.A - b*b) / (b * b)
	theta := math.Atan2(Z*e.A, p*b)
	sinT, cosT := math.Sin(theta), math.Cos(theta)
	lat := math.Atan2(Z+ep2*b*sinT*sinT*sinT, p-e2*e.A*cosT*cosT*cosT)
	return lon * rad2deg, lat * rad2deg
}

/* ---------------- 横轴墨卡托（高斯克吕格） ---------------- */

// tmParams 横轴墨卡托 / 高斯克吕格投影参数（角度单位为度，长度单位为米）。
type tmParams struct {
	Ell  ellipsoid
	Lon0 float64 // 中央经线
	Lat0 float64 // 投影原点纬度（国内多为 0）
	K0   float64 // 尺度因子（国内多为 1）
	FE   float64 // 假东（国内 3°/6° 带为 500000）
	FN   float64 // 假北
}

// forward 正算：经纬度 → 投影平面坐标（EPSG Guidance Note 7-2 公式）。
func (p tmParams) forward(lonDeg, latDeg float64) (x, y float64) {
	e2 := p.Ell.e2()
	ep2 := e2 / (1 - e2)
	lat, lon := latDeg*deg2rad, lonDeg*deg2rad
	lat0 := p.Lat0 * deg2rad
	sinLat, cosLat, tanLat := math.Sin(lat), math.Cos(lat), math.Tan(lat)

	nu := p.Ell.A / math.Sqrt(1-e2*sinLat*sinLat)
	T := tanLat * tanLat
	C := ep2 * cosLat * cosLat
	A := (lon - p.Lon0*deg2rad) * cosLat
	M := p.Ell.meridianArc(lat)
	M0 := p.Ell.meridianArc(lat0)

	x = p.FE + p.K0*nu*(A+(1-T+C)*A*A*A/6+(5-18*T+T*T+72*C-58*ep2)*math.Pow(A, 5)/120)
	y = p.FN + p.K0*(M-M0+nu*tanLat*(A*A/2+(5-T+9*C+4*C*C)*math.Pow(A, 4)/24+
		(61-58*T+T*T+600*C-330*ep2)*math.Pow(A, 6)/720))
	return
}

// inverse 反算：投影平面坐标 → 经纬度（EPSG Guidance Note 7-2 公式）。
func (p tmParams) inverse(x, y float64) (lonDeg, latDeg float64) {
	e2 := p.Ell.e2()
	e4 := e2 * e2
	e6 := e4 * e2
	ep2 := e2 / (1 - e2)

	M := p.Ell.meridianArc(p.Lat0*deg2rad) + (y-p.FN)/p.K0
	mu := M / (p.Ell.A * (1 - e2/4 - 3*e4/64 - 5*e6/256))
	e1 := (1 - math.Sqrt(1-e2)) / (1 + math.Sqrt(1-e2))
	e1_2, e1_3, e1_4 := e1*e1, e1*e1*e1, e1*e1*e1*e1

	phi1 := mu +
		(3*e1/2-27*e1_3/32)*math.Sin(2*mu) +
		(21*e1_2/16-55*e1_4/32)*math.Sin(4*mu) +
		(151*e1_3/96)*math.Sin(6*mu) +
		(1097*e1_4/512)*math.Sin(8*mu)

	sinP, cosP, tanP := math.Sin(phi1), math.Cos(phi1), math.Tan(phi1)
	C1 := ep2 * cosP * cosP
	T1 := tanP * tanP
	nu1 := p.Ell.A / math.Sqrt(1-e2*sinP*sinP)
	rho1 := p.Ell.A * (1 - e2) / math.Pow(1-e2*sinP*sinP, 1.5)
	D := (x - p.FE) / (nu1 * p.K0)

	lat := phi1 - (nu1*tanP/rho1)*(D*D/2-
		(5+3*T1+10*C1-4*C1*C1-9*ep2)*math.Pow(D, 4)/24+
		(61+90*T1+298*C1+45*T1*T1-252*ep2-3*C1*C1)*math.Pow(D, 6)/720)
	lon := (D-(1+2*T1+C1)*math.Pow(D, 3)/6+
		(5-2*C1+28*T1-3*C1*C1+8*ep2+24*T1*T1)*math.Pow(D, 5)/120)/cosP + p.Lon0*deg2rad
	return lon * rad2deg, lat * rad2deg
}

/* ---------------- 七参数（Bursa-Wolf，位置矢量约定） ---------------- */

// bursa7 七参数：平移（米）、旋转（角秒）、尺度（ppm）。
// 与 WKT 的 TOWGS84[dX,dY,dZ,rX,rY,rZ,dS] 一一对应。
type bursa7 struct {
	DX, DY, DZ float64
	RX, RY, RZ float64
	DS         float64
}

// apply 把源基准下的大地坐标转到目标基准（位置矢量旋转约定）。
func (b bursa7) apply(lonDeg, latDeg, h float64, src, dst ellipsoid) (lonOut, latOut float64) {
	X, Y, Z := src.geodeticToCartesian(lonDeg, latDeg, h)
	rx, ry, rz := b.RX*arcSec2rad, b.RY*arcSec2rad, b.RZ*arcSec2rad
	s := 1 + b.DS*1e-6
	// 位置矢量法：R = [[1,-rz,ry],[rz,1,-rx],[-ry,rx,1]]
	x2 := b.DX + s*(X-rz*Y+ry*Z)
	y2 := b.DY + s*(rz*X+Y-rx*Z)
	z2 := b.DZ + s*(-ry*X+rx*Y+Z)
	return dst.cartesianToGeodetic(x2, y2, z2)
}

/* ---------------- WKT 解析 ---------------- */

// wktNode WKT 的树节点：KEYWORD[标量…, 子节点…]。
type wktNode struct {
	Keyword  string
	Scalars  []string
	Children []*wktNode
}

// child 取第一个匹配关键字的子节点（关键字大小写不敏感，兼容 WKT1/WKT2 写法）。
func (n *wktNode) child(keywords ...string) *wktNode {
	for _, c := range n.Children {
		for _, k := range keywords {
			if c.Keyword == strings.ToUpper(k) {
				return c
			}
		}
	}
	return nil
}

// children 取全部匹配子节点。
func (n *wktNode) children(keywords ...string) []*wktNode {
	var out []*wktNode
	for _, c := range n.Children {
		for _, k := range keywords {
			if c.Keyword == strings.ToUpper(k) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// scalar 取第 i 个标量（去引号）。
func (n *wktNode) scalar(i int) string {
	if n == nil || i < 0 || i >= len(n.Scalars) {
		return ""
	}
	return strings.Trim(n.Scalars[i], `"`)
}

// num 取第 i 个标量为浮点。
func (n *wktNode) num(i int) (float64, bool) {
	s := n.scalar(i)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseWKT 解析 WKT 文本。只做到"树 + 标量"这一层——投影换算需要的都能取到，
// 不追求完整的坐标系语义模型。
func parseWKT(s string) (*wktNode, error) {
	p := &wktParser{s: s}
	n, err := p.node()
	if err != nil {
		return nil, err
	}
	return n, nil
}

type wktParser struct {
	s string
	i int
}

func (p *wktParser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

// node 解析 KEYWORD[ 参数… ]。
func (p *wktParser) node() (*wktNode, error) {
	p.ws()
	start := p.i
	for p.i < len(p.s) && isWKTKeyword(p.s[p.i]) {
		p.i++
	}
	if p.i == start {
		return nil, fmt.Errorf("WKT: 位置 %d 处缺少关键字", p.i)
	}
	n := &wktNode{Keyword: strings.ToUpper(p.s[start:p.i])}
	p.ws()
	if p.i >= len(p.s) || p.s[p.i] != '[' {
		return n, nil
	}
	p.i++
	for {
		p.ws()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("WKT: 中括号未闭合")
		}
		if p.s[p.i] == ']' {
			p.i++
			return n, nil
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		// 读到逗号/中括号为止，判断是子节点还是标量
		save := p.i
		for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '[' && p.s[p.i] != ']' {
			p.i++
		}
		tok := strings.TrimSpace(p.s[save:p.i])
		if p.i < len(p.s) && p.s[p.i] == '[' {
			p.i = save
			child, err := p.node()
			if err != nil {
				return nil, err
			}
			n.Children = append(n.Children, child)
			continue
		}
		if tok == "" {
			return nil, fmt.Errorf("WKT: 位置 %d 处出现空参数", save)
		}
		n.Scalars = append(n.Scalars, tok)
	}
}

func isWKTKeyword(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

/* ---------------- 坐标系描述 ---------------- */

// crsTransform 源坐标 → WGS84 经纬度的转换方案。
type crsTransform struct {
	// SRID 源坐标系 EPSG 码（0 表示 WKT 未给出权威码）。
	SRID int
	// Name 源坐标系名称（用于提示）。
	Name string
	// Method 转换方式的人类可读描述（写进解析提示）。
	Method string
	// identity 源坐标已是 WGS84 经纬度（4326/4490），无需换算。
	identity bool
	// mercator 源坐标是 Web Mercator 米（3857），沿用既有在线投影路径。
	mercator bool
	// direct 源为经纬度但基准不同：只做基准转换。
	direct bool

	tm      *tmParams
	toWGS84 *bursa7
	srcEll  ellipsoid
}

// needsTransform 报告是否需要逐点换算。
func (t *crsTransform) needsTransform() bool { return t != nil && !t.identity && !t.mercator }

// effectiveSRID 换算之后数据**实际所处**的坐标系。
//
// 图层元数据必须用这个值而不是源坐标系码：投影坐标换算完就已经是 WGS84 经纬度了，
// 若照抄源码（如 4547），下游会按"米"去解释经纬度，出图必然错位。
// 4490 保持原样（与 WGS84 数值等同，但如实反映来源）。
func (t *crsTransform) effectiveSRID() int {
	if t == nil {
		return 4326
	}
	switch {
	case t.needsTransform():
		return 4326
	case t.mercator:
		return 3857
	case t.identity:
		if t.SRID == 4490 {
			return 4490
		}
		return 4326
	}
	return 4326
}

// transformPoint 源坐标 → WGS84 经纬度。
func (t *crsTransform) transformPoint(p Point) (Point, error) {
	if t == nil || t.identity || t.mercator {
		return p, nil
	}
	lon, lat := p.X, p.Y
	if t.tm != nil {
		lon, lat = t.tm.inverse(p.X, p.Y)
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return Point{}, fmt.Errorf("%w: 坐标反算结果超出经纬度范围（%.3f, %.3f），投影参数可能有误", ErrInvalid, lon, lat)
	}
	if t.toWGS84 != nil {
		// 二维矢量没有高程：按 h=0 处理（提示里会写明）
		lon, lat = t.toWGS84.apply(lon, lat, 0, t.srcEll, ellipsoidWGS84)
	}
	return Point{X: lon, Y: lat}, nil
}

// transformGeometry 整几何换算。
func (t *crsTransform) transformGeometry(g Geometry) (Geometry, error) {
	if !t.needsTransform() {
		return g, nil
	}
	out := Geometry{Type: g.Type, Lines: make([][]Point, len(g.Lines)), Exterior: g.Exterior}
	for i, line := range g.Lines {
		nl := make([]Point, len(line))
		for j, p := range line {
			q, err := t.transformPoint(p)
			if err != nil {
				return Geometry{}, err
			}
			nl[j] = q
		}
		out.Lines[i] = nl
	}
	return out, nil
}

// note 转换说明（写进 FileInfo.Notes，注册结果里直接可见）。
func (t *crsTransform) note() string {
	if t == nil || !t.needsTransform() {
		return ""
	}
	label := t.Name
	if t.SRID != 0 {
		label = fmt.Sprintf("%s（EPSG:%d）", t.Name, t.SRID)
	}
	return "源坐标系 " + label + " 已转换到 WGS84：" + t.Method
}

/* ---------------- 由 WKT 构造转换方案 ---------------- */

// crsTransformFromWKT 解析 WKT 并给出到 WGS84 的转换方案。
//
// 无法安全转换时返回明确的错误（而不是让数据带着错误坐标流下去）。
func crsTransformFromWKT(wkt string) (*crsTransform, error) {
	wkt = strings.TrimSpace(wkt)
	if wkt == "" {
		// 没有 .prj：Shapefile 事实约定是 WGS84 经纬度
		return &crsTransform{SRID: 4326, Name: "WGS84（缺坐标系定义，按约定）", identity: true}, nil
	}
	root, err := parseWKT(wkt)
	if err != nil {
		return nil, fmt.Errorf("%w: 坐标系 WKT 解析失败: %v", ErrInvalid, err)
	}

	t := &crsTransform{Name: root.scalar(0)}
	if a := root.child("AUTHORITY"); a != nil && strings.EqualFold(strings.Trim(a.scalar(0), `"`), "EPSG") {
		if code, err := strconv.Atoi(strings.Trim(a.scalar(1), `"`)); err == nil {
			t.SRID = code
		}
	}

	projected := false
	switch root.Keyword {
	case "GEOGCS", "GEOGCRS":
	case "PROJCS", "PROJCRS":
		projected = true
	default:
		return nil, fmt.Errorf("%w: 不支持的坐标系类型 %s（仅支持地理坐标系与投影坐标系）", ErrInvalid, root.Keyword)
	}

	geo := root
	if projected {
		geo = root.child("GEOGCS", "GEOGCRS", "BASEGEOGCRS")
		if geo == nil {
			return nil, fmt.Errorf("%w: 投影坐标系 WKT 缺少地理坐标系定义", ErrInvalid)
		}
	}
	datum := geo.child("DATUM", "GEODETICDATUM")
	if datum == nil {
		datum = root.child("DATUM", "GEODETICDATUM")
	}
	if datum != nil {
		spheroid := datum.child("SPHEROID", "ELLIPSOID")
		if spheroid != nil {
			a, aok := spheroid.num(1)
			invF, fok := spheroid.num(2)
			if aok && fok {
				t.srcEll = ellipsoid{Name: spheroid.scalar(0), A: a, InvF: invF}
			}
		}
		if tw := datum.child("TOWGS84"); tw != nil && len(tw.Scalars) >= 7 {
			var v [7]float64
			ok := true
			for i := 0; i < 7; i++ {
				n, numOK := tw.num(i)
				if !numOK {
					ok = false
					break
				}
				v[i] = n
			}
			if ok {
				t.toWGS84 = &bursa7{DX: v[0], DY: v[1], DZ: v[2], RX: v[3], RY: v[4], RZ: v[5], DS: v[6]}
			}
		}
	}
	if !t.srcEll.valid() {
		t.srcEll = ellipsoidWGS84
	}
	datumName := ""
	if datum != nil {
		datumName = datum.scalar(0)
	}

	wgs84Like := t.wgs84Equivalent(datumName)
	if t.toWGS84 != nil {
		wgs84Like = false
	}

	if !projected {
		// 已是经纬度：只在基准不同且给了七参数时才需要换算
		switch {
		case t.toWGS84 != nil:
			t.direct = true
			t.Method = fmt.Sprintf("基准转换（七参数，源椭球 %s，假定高程 0）", t.srcEll.Name)
		case wgs84Like:
			t.identity = true
		default:
			return nil, unsupportedDatumErr(datumName, t.SRID)
		}
		return t, nil
	}

	// ---- 投影坐标系：解析投影参数 ----
	if t.SRID == 3857 {
		t.mercator = true
		return t, nil
	}
	proj := root.child("PROJECTION", "METHOD", "CONVERSION")
	method := ""
	if proj != nil {
		method = strings.ToLower(strings.ReplaceAll(proj.scalar(0), " ", "_"))
	} else if conv := root.child("CONVERSION"); conv != nil {
		if m := conv.child("METHOD"); m != nil {
			method = strings.ToLower(strings.ReplaceAll(m.scalar(0), " ", "_"))
		}
	}
	// Web Mercator 的变体名（ESRI 的 Mercator_Auxiliary_Sphere、OGC 的
	// Popular_Visualisation_Pseudo_Mercator）在 WKT 里往往不带 EPSG 权威码，
	// 按名字识别为 3857，沿用既有在线投影路径，别误报"投影方式不支持"。
	lowName := strings.ToLower(t.Name)
	if strings.Contains(method, "pseudo") || strings.Contains(method, "auxiliary_sphere") ||
		strings.Contains(lowName, "web_mercator") || strings.Contains(lowName, "pseudo-mercator") {
		t.mercator = true
		t.SRID = 3857
		return t, nil
	}
	switch strings.Trim(method, "_") {
	case "transverse_mercator", "gauss_kruger", "gausskruger",
		"transverse_mercator_(gauss-kruger)", "transverse_mercator_(gauss_kruger)",
		"transverse_mercator_z", "gauss_kruger_cm", "cgcs2000_3_degree_gk_cm":
	default:
		return nil, fmt.Errorf("%w: 暂不支持的投影方式 %q（当前支持横轴墨卡托 / 高斯克吕格）",
			ErrInvalid, strings.Trim(method, "_"))
	}

	params := map[string]float64{}
	collect := func(n *wktNode) {
		for _, p := range n.children("PARAMETER", "PARAMETERFILE") {
			name := strings.ToLower(strings.Trim(strings.ReplaceAll(p.scalar(0), " ", "_"), `"`))
			if v, ok := p.num(1); ok {
				params[name] = v
			}
		}
	}
	collect(root)
	if conv := root.child("CONVERSION"); conv != nil {
		collect(conv)
	}
	// WKT2 里参数名形如 "Latitude of natural origin"，WKT1 为 "latitude_of_origin"
	get := func(names ...string) (float64, bool) {
		for _, n := range names {
			if v, ok := params[n]; ok {
				return v, true
			}
		}
		return 0, false
	}
	lon0, okLon := get("central_meridian", "longitude_of_natural_origin", "longitude_of_origin",
		"longitude_of_center", "central_meridian_of_zone")
	if !okLon {
		return nil, fmt.Errorf("%w: 投影参数缺少中央经线（central_meridian）", ErrInvalid)
	}
	lat0, _ := get("latitude_of_origin", "latitude_of_natural_origin", "latitude_of_center")
	k0, okK0 := get("scale_factor", "scale_factor_at_natural_origin")
	if !okK0 || k0 == 0 {
		k0 = 1
	}
	fe, _ := get("false_easting", "easting_at_false_origin", "false_easting_of_natural_origin")
	fn, _ := get("false_northing", "northing_at_false_origin", "false_northing_of_natural_origin")

	t.tm = &tmParams{Ell: t.srcEll, Lon0: lon0, Lat0: lat0, K0: k0, FE: fe, FN: fn}
	switch {
	case t.toWGS84 != nil:
		t.Method = fmt.Sprintf("横轴墨卡托反算（中央经线 %.6g°，椭球 %s）+ 基准七参数（假定高程 0）",
			lon0, t.srcEll.Name)
	case wgs84Like:
		t.Method = fmt.Sprintf("横轴墨卡托反算（中央经线 %.6g°，椭球 %s），与 WGS84 无需基准转换",
			lon0, t.srcEll.Name)
	default:
		return nil, unsupportedDatumErr(datumName, t.SRID)
	}
	return t, nil
}

// wgs84Equivalent 判断基准是否可按 WGS84 直接使用。
//
// 依据：基准名关键词（含中文/英文常见写法）或椭球参数与 WGS84 一致。
// CGCS2000 与 WGS84 差异在厘米级，工程上可直接等同。
func (t *crsTransform) wgs84Equivalent(datumName string) bool {
	u := strings.ToUpper(datumName)
	for _, kw := range []string{
		"WGS_1984", "WGS 84", "WGS84", "WGS_1984_", "GCS_WGS",
		"CGCS2000", "CGCS_2000", "CHINA_2000", "CHINA GEODETIC",
		"国家2000", "2000国家大地", "2000 国家大地",
	} {
		if strings.Contains(u, kw) {
			return true
		}
	}
	if t.srcEll.valid() &&
		math.Abs(t.srcEll.A-ellipsoidWGS84.A) < 1e-6 &&
		math.Abs(t.srcEll.InvF-ellipsoidWGS84.InvF) < 1e-9 {
		return true
	}
	return false
}

// unsupportedDatumErr 基准不支持时的可执行提示。
func unsupportedDatumErr(datumName string, srid int) error {
	label := datumName
	if label == "" {
		label = "未知基准"
	}
	if srid != 0 {
		label = fmt.Sprintf("%s（EPSG:%d）", label, srid)
	}
	return fmt.Errorf("%w: 坐标系 %s 的基准与 WGS84 不同，且 WKT 未提供 TOWGS84 七参数，"+
		"直接按 WGS84 处理会引入 50~150m 的系统性偏差。请要么用带 TOWGS84 的 .prj，要么先用「合规工具」的七参数转换把数据转到 WGS84/CGCS2000",
		ErrInvalid, label)
}
