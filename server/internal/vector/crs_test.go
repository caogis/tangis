package vector

import (
	"math"
	"strings"
	"testing"
)

// OSGB 1936 / British National Grid 的投影参数（EPSG:27700）。
// 选用它是因为 EPSG Guidance Note 7-2 给出了**公开算例**，可作为数学实现的锚点。
var osgb = tmParams{
	Ell:  ellipsoid{Name: "Airy 1830", A: 6377563.396, InvF: 299.32496},
	Lon0: -2, Lat0: 49, K0: 0.9996012717, FE: 400000, FN: -100000,
}

// TestTransverseMercatorAgainstEPSGExample 用 EPSG GN7-2 的公开算例校准横轴墨卡托。
//
// 正算：φ=52°39'27.2531"N λ=1°43'4.5177"E → E=651409.903 N=313177.270
// 反算：E=577274.99 N=69740.50 → φ=50°30'00.000"N λ=0°30'00.000"E
func TestTransverseMercatorAgainstEPSGExample(t *testing.T) {
	lat := 52 + 39/60.0 + 27.2531/3600.0
	lon := 1 + 43/60.0 + 4.5177/3600.0
	x, y := osgb.forward(lon, lat)
	if math.Abs(x-651409.903) > 0.02 || math.Abs(y-313177.270) > 0.02 {
		t.Errorf("正算 = (%.3f, %.3f), want (651409.903, 313177.270)", x, y)
	}

	// 容差 1e-6°（≈0.1m）：级数截断到 e⁶ 的实际精度在厘米级，
	// 而 EPSG 算例本身只给到 0.01m。真正错（中央经线/椭球取值不对）会差上百米。
	lonOut, latOut := osgb.inverse(577274.99, 69740.50)
	if math.Abs(latOut-50.5) > 1e-6 || math.Abs(lonOut-0.5) > 1e-6 {
		t.Errorf("反算 = (%.9f, %.9f), want (0.5, 50.5)", lonOut, latOut)
	}
}

// TestTransverseMercatorCGCS2000 国内常见情形：CGCS2000 3 度带（中央经线 114°E）。
func TestTransverseMercatorCGCS2000(t *testing.T) {
	tm := tmParams{Ell: ellipsoidCGCS2000, Lon0: 114, Lat0: 0, K0: 1, FE: 500000, FN: 0}

	// 中央经线上：x 必须恰为假东，经度反算回中央经线
	fx, y := tm.forward(114, 39.9)
	if math.Abs(fx-500000) > 1e-6 {
		t.Errorf("中央经线正算 x=%.6f, want 500000", fx)
	}
	lonCM, latCM := tm.inverse(500000, y)
	if math.Abs(lonCM-114) > 1e-9 {
		t.Errorf("中央经线反算经度=%.9f, want 114", lonCM)
	}
	// 经度不变 ⇒ 纬度应回到原值（校验反算级数精度）
	if math.Abs(latCM-39.9) > 1e-9 {
		t.Errorf("中央经线上纬度回归=%.9f, want 39.9", latCM)
	}

	// 往返一致性（距中央经线约 2.4°）
	x2, y2 := tm.forward(116.397, 39.908)
	lon2, lat2 := tm.inverse(x2, y2)
	if math.Abs(lon2-116.397) > 1e-9 || math.Abs(lat2-39.908) > 1e-9 {
		t.Errorf("往返 = (%.9f, %.9f), want (116.397, 39.908)", lon2, lat2)
	}
	// 量级合理性：东偏应在 600~750 km 之间（3 度带宽度约 ±1.5°，超带则更大）
	if x2 < 600000 || x2 > 780000 {
		t.Errorf("x=%.0f 超出合理量级（500000 假东 + 东西偏）", x2)
	}
	if y2 < 4_300_000 || y2 > 4_500_000 {
		t.Errorf("y=%.0f 超出北纬 39.9° 应有的量级", y2)
	}

	// 带号前缀（国内常见的 38500000 写法）：识别提示应给出提示而不静默处理
	// —— 这里验证"未加提示时会得到越界经纬度"，说明检测有必要。
	if _, lat3 := tm.inverse(38500000, y2); math.Abs(lat3-39.908) < 1 {
		t.Error("带号前缀坐标不加处理却算出正确纬度，说明越界检测失效")
	}
}

// TestCRSParseESRIAndOGC 兼容 ESRI 与 OGC 两种 WKT 写法。
func TestCRSParseESRIAndOGC(t *testing.T) {
	esri := `PROJCS["CGCS2000_3_Degree_GK_CM_114E",GEOGCS["GCS_China_Geodetic_Coordinate_System_2000",DATUM["D_China_2000",SPHEROID["CGCS2000",6378137.0,298.257222101]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]],PROJECTION["Gauss_Kruger"],PARAMETER["False_Easting",500000.0],PARAMETER["False_Northing",0.0],PARAMETER["Central_Meridian",114.0],PARAMETER["Scale_Factor",1.0],PARAMETER["Latitude_Of_Origin",0.0],UNIT["Meter",1.0]]`
	ogc := `PROJCS["CGCS2000 / 3-degree Gauss-Kruger CM 114E",GEOGCS["China Geodetic Coordinate System 2000",DATUM["China_2000",SPHEROID["CGCS2000",6378137,298.257222101,AUTHORITY["EPSG","1024"]]],PRIMEM["Greenwich",0,AUTHORITY["EPSG","8901"]],UNIT["degree",0.0174532925199433],AUTHORITY["EPSG","4490"]],PROJECTION["Transverse_Mercator"],PARAMETER["latitude_of_origin",0],PARAMETER["central_meridian",114],PARAMETER["scale_factor",1],PARAMETER["false_easting",500000],PARAMETER["false_northing",0],UNIT["metre",1],AUTHORITY["EPSG","4547"]]`

	for _, tc := range []struct{ name, wkt string }{{"ESRI", esri}, {"OGC", ogc}} {
		tr, err := crsTransformFromWKT(tc.wkt)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tr.tm == nil {
			t.Fatalf("%s: 未解析出投影参数", tc.name)
		}
		if math.Abs(tr.tm.Lon0-114) > 1e-9 || tr.tm.FE != 500000 || tr.tm.K0 != 1 {
			t.Errorf("%s: 参数 lon0=%.6f FE=%.0f K0=%.3f", tc.name, tr.tm.Lon0, tr.tm.FE, tr.tm.K0)
		}
		if math.Abs(tr.srcEll.A-6378137) > 1e-6 || math.Abs(tr.srcEll.InvF-298.257222101) > 1e-9 {
			t.Errorf("%s: 椭球 a=%.3f 1/f=%.9f", tc.name, tr.srcEll.A, tr.srcEll.InvF)
		}
		if !tr.needsTransform() {
			t.Errorf("%s: 投影坐标系应需要换算", tc.name)
		}
		if tr.note() == "" {
			t.Errorf("%s: 应给出转换说明", tc.name)
		}
		// 反算落点合理（北京一带）
		p, err := tr.transformPoint(Point{X: 0, Y: 0}) // 占位，真正取点见下
		_ = p
		if err != nil && !strings.Contains(err.Error(), "超出经纬度范围") {
			t.Errorf("%s: 原点换算异常: %v", tc.name, err)
		}
	}
}

// TestCRSProjectedPointTransform 投影坐标 → WGS84 的实际换算。
func TestCRSProjectedPointTransform(t *testing.T) {
	wkt := `PROJCS["CGCS2000 / 3-degree Gauss-Kruger CM 114E",GEOGCS["China Geodetic Coordinate System 2000",DATUM["China_2000",SPHEROID["CGCS2000",6378137,298.257222101]],PRIMEM["Greenwich",0],UNIT["degree",0.0174532925199433]],PROJECTION["Transverse_Mercator"],PARAMETER["latitude_of_origin",0],PARAMETER["central_meridian",114],PARAMETER["scale_factor",1],PARAMETER["false_easting",500000],PARAMETER["false_northing",0],UNIT["metre",1],AUTHORITY["EPSG","4547"]]`
	tr, err := crsTransformFromWKT(wkt)
	if err != nil {
		t.Fatalf("crsTransformFromWKT: %v", err)
	}
	if tr.SRID != 4547 {
		t.Errorf("SRID=%d, want 4547", tr.SRID)
	}
	// 用标准公式造一个投影坐标，再验证换算回来
	tm := tr.tm
	px, py := tm.forward(116.397, 39.908)
	got, err := tr.transformPoint(Point{X: px, Y: py})
	if err != nil {
		t.Fatalf("transformPoint: %v", err)
	}
	if math.Abs(got.X-116.397) > 1e-8 || math.Abs(got.Y-39.908) > 1e-8 {
		t.Errorf("换算 = (%.9f, %.9f), want (116.397, 39.908)", got.X, got.Y)
	}
	// 几何换算（多边形）
	ring := []Point{{px, py}, {px + 1000, py}, {px + 1000, py + 1000}, {px, py + 1000}}
	g := Geometry{Type: GeomPolygon, Lines: [][]Point{ring}, Exterior: []bool{true}}
	gg, err := tr.transformGeometry(g)
	if err != nil {
		t.Fatalf("transformGeometry: %v", err)
	}
	// 1000 米 ≈ 0.009~0.012 度
	dLon := gg.Lines[0][1].X - gg.Lines[0][0].X
	if dLon < 0.008 || dLon > 0.015 {
		t.Errorf("1000m 对应的经度差=%.6f，量级不合理", dLon)
	}
	if gg.Lines[0][0] != got {
		t.Errorf("几何换算与单点换算不一致")
	}
}

// TestCRSDatumDecisions 基准判断：CGCS2000 直用、无参数的外国基准拒绝、带 TOWGS84 放行。
func TestCRSDatumDecisions(t *testing.T) {
	// ① CGCS2000：与 WGS84 差在厘米级 → 无需基准转换
	t.Run("CGCS2000 直用", func(t *testing.T) {
		tr, err := crsTransformFromWKT(`GEOGCS["China Geodetic Coordinate System 2000",DATUM["China_2000",SPHEROID["CGCS2000",6378137,298.257222101]],PRIMEM["Greenwich",0],UNIT["degree",0.0174532925199433],AUTHORITY["EPSG","4490"]]`)
		if err != nil {
			t.Fatalf("4490 应被接受: %v", err)
		}
		if !tr.identity {
			t.Error("4490 应视为与 WGS84 等价（identity）")
		}
		if tr.SRID != 4490 {
			t.Errorf("SRID=%d, want 4490", tr.SRID)
		}
	})

	// ② 北京54 投影带、无 TOWGS84 → 拒绝（不能静默当 WGS84）
	t.Run("北京54 无七参数拒绝", func(t *testing.T) {
		wkt := `PROJCS["Beijing_1954_3_Degree_GK_CM_114E",GEOGCS["GCS_Beijing_1954",DATUM["D_Beijing_1954",SPHEROID["Krasovsky_1940",6378245.0,298.3]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]],PROJECTION["Gauss_Kruger"],PARAMETER["False_Easting",500000.0],PARAMETER["False_Northing",0.0],PARAMETER["Central_Meridian",114.0],PARAMETER["Scale_Factor",1.0],PARAMETER["Latitude_Of_Origin",0.0],UNIT["Meter",1.0]]`
		_, err := crsTransformFromWKT(wkt)
		if err == nil {
			t.Fatal("北京54 无七参数应报错（静默当 WGS84 会引入 50~150m 偏差）")
		}
		if !strings.Contains(err.Error(), "TOWGS84") {
			t.Errorf("错误信息应指出缺 TOWGS84: %v", err)
		}
		if !errorIs(err, ErrInvalid) {
			t.Errorf("错误类型=%v", err)
		}
	})

	// ③ 同一坐标系带 TOWGS84 → 放行，且确实产生了基准偏移
	t.Run("北京54 带七参数", func(t *testing.T) {
		wkt := `PROJCS["Beijing_1954_3_Degree_GK_CM_114E",GEOGCS["GCS_Beijing_1954",DATUM["D_Beijing_1954",SPHEROID["Krasovsky_1940",6378245.0,298.3],TOWGS84[15.8,-154.4,-82.3,0,0,0,0]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]],PROJECTION["Gauss_Kruger"],PARAMETER["False_Easting",500000.0],PARAMETER["False_Northing",0.0],PARAMETER["Central_Meridian",114.0],PARAMETER["Scale_Factor",1.0],PARAMETER["Latitude_Of_Origin",0.0],UNIT["Meter",1.0]]`
		tr, err := crsTransformFromWKT(wkt)
		if err != nil {
			t.Fatalf("带 TOWGS84 应放行: %v", err)
		}
		if tr.toWGS84 == nil {
			t.Fatal("未解析出七参数")
		}
		if tr.toWGS84.DX != 15.8 || tr.toWGS84.DY != -154.4 || tr.toWGS84.DZ != -82.3 {
			t.Errorf("七参数=%+v", tr.toWGS84)
		}
		if !strings.Contains(tr.note(), "七参数") || !strings.Contains(tr.note(), "高程 0") {
			t.Errorf("提示应写明用了七参数且假定高程为 0: %q", tr.note())
		}

		// 与不做基准转换相比应有可观偏移（几十米量级）
		plain := *tr
		plain.toWGS84 = nil
		plain.direct = false
		tm := tr.tm
		px, py := tm.forward(116.397, 39.908)
		a, err := tr.transformPoint(Point{X: px, Y: py})
		if err != nil {
			t.Fatalf("transformPoint(七参数): %v", err)
		}
		lon0, lat0 := tm.inverse(px, py)
		dist := math.Hypot((a.X-lon0)*111320*math.Cos(lat0*deg2rad), (a.Y-lat0)*110540)
		if dist < 5 || dist > 500 {
			t.Errorf("七参数造成的偏移=%.1fm，量级不合理（应为几十~几百米）", dist)
		}
	})

	// ④ 未知基准、无参数 → 拒绝
	t.Run("未知基准拒绝", func(t *testing.T) {
		if _, err := crsTransformFromWKT(`GEOGCS["Some Local System",DATUM["D_Unknown",SPHEROID["Bessel_1841",6377397.155,299.1528128]],PRIMEM["Greenwich",0],UNIT["Degree",0.0174532925199433]]`); err == nil {
			t.Error("未知基准应拒绝")
		}
	})

	// ⑤ 支持的投影方式之外 → 明确报错
	t.Run("不支持投影方式", func(t *testing.T) {
		_, err := crsTransformFromWKT(`PROJCS["UTM zone 50N",GEOGCS["WGS 84",DATUM["WGS_1984",SPHEROID["WGS 84",6378137,298.257223563]],PRIMEM["Greenwich",0],UNIT["degree",0.0174532925199433]],PROJECTION["Albers_Conic_Equal_Area"],PARAMETER["central_meridian",117],PARAMETER["false_easting",500000],UNIT["metre",1]]`)
		if err == nil {
			t.Fatal("非横轴墨卡托投影应报错")
		}
		if !strings.Contains(err.Error(), "投影方式") {
			t.Errorf("错误信息应指出投影方式不支持: %v", err)
		}
	})
}

// TestBursaWolfMath 七参数数学：零参数为恒等，纯平移方向正确。
func TestBursaWolfMath(t *testing.T) {
	var zero bursa7
	lon, lat := zero.apply(116.397, 39.908, 0, ellipsoidWGS84, ellipsoidWGS84)
	if math.Abs(lon-116.397) > 1e-12 || math.Abs(lat-39.908) > 1e-12 {
		t.Errorf("零参数应为恒等变换, 得到 (%.12f, %.12f)", lon, lat)
	}

	// 在 (0°,0°) 处：+X 平移沿赤道径向（只改变大地高，经纬度不变），
	// +Y 平移沿赤道东向（经度增大），用这一对性质验证旋转矩阵的轴向正确。
	radial := bursa7{DX: 100}
	lonR, latR := radial.apply(0, 0, 0, ellipsoidWGS84, ellipsoidWGS84)
	if math.Abs(lonR) > 1e-12 || math.Abs(latR) > 1e-12 {
		t.Errorf("赤道本初子午线上 +X 平移不应改变经纬度, 得到 (%.12f, %.12f)", lonR, latR)
	}
	east := bursa7{DY: 100}
	lon2, lat2 := east.apply(0, 0, 0, ellipsoidWGS84, ellipsoidWGS84)
	dEast := lon2 * 111320
	if math.Abs(dEast-100) > 1 {
		t.Errorf("Y 平移 100m 引起的东向位移=%.2fm, want ≈100m", dEast)
	}
	if math.Abs(lat2) > 1e-12 {
		t.Errorf("赤道上 Y 平移不应改变纬度, 得到 %.12f", lat2)
	}

	// 大地坐标 ↔ 空间直角坐标往返
	X, Y, Z := ellipsoidWGS84.geodeticToCartesian(116.397, 39.908, 50)
	l, b := ellipsoidWGS84.cartesianToGeodetic(X, Y, Z)
	if math.Abs(l-116.397) > 1e-9 || math.Abs(b-39.908) > 1e-9 {
		t.Errorf("坐标往返 = (%.9f, %.9f)", l, b)
	}
}

// TestCRSParseErrors 非法 WKT 与缺参数的报错。
func TestCRSParseErrors(t *testing.T) {
	cases := []struct{ name, wkt string }{
		{"空括号", `PROJCS[]`},
		{"关键字缺失", `["x"]`},
		{"括号未闭合", `PROJCS["a",GEOGCS["b"`},
		{"根节点不支持", `GEOCCS["geocentric",DATUM["D",SPHEROID["S",6378137,298.257223563]]]`},
		{"缺中央经线", `PROJCS["x",GEOGCS["WGS 84",DATUM["WGS_1984",SPHEROID["WGS 84",6378137,298.257223563]],PRIMEM["Greenwich",0],UNIT["degree",0.0174532925199433]],PROJECTION["Transverse_Mercator"],PARAMETER["false_easting",500000],UNIT["metre",1]]`},
	}
	for _, c := range cases {
		if _, err := crsTransformFromWKT(c.wkt); err == nil {
			t.Errorf("%s: 应报错（wkt=%s）", c.name, c.wkt)
		}
	}
	// 缺 .prj 按 WGS84 约定（不报错）
	tr, err := crsTransformFromWKT("")
	if err != nil || !tr.identity || tr.SRID != 4326 {
		t.Errorf("空 WKT 应按 WGS84 约定: tr=%+v err=%v", tr, err)
	}
}
