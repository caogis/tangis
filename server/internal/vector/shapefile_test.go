package vector

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* ------------------------------------------------------------------ */
/* 测试用 Shapefile / DBF 生成器（手写字节，避免提交二进制语料）        */
/* ------------------------------------------------------------------ */

// buildSHP 组装一个合法的 .shp：文件头 + 若干记录。
func buildSHP(shapeType int32, contents [][]byte) []byte {
	body := make([]byte, 0, 512)
	minx, miny := math.Inf(1), math.Inf(1)
	maxx, maxy := math.Inf(-1), math.Inf(-1)
	for i, c := range contents {
		// 记录头：记录号（BE，1 起）+ 内容长度（BE，单位 16 位字）
		rec := make([]byte, 8)
		binary.BigEndian.PutUint32(rec[0:4], uint32(i+1))
		binary.BigEndian.PutUint32(rec[4:8], uint32(len(c)/2))
		body = append(body, rec...)
		body = append(body, c...)
		// 从内容里收集 bbox（简化：解析已知布局）
		b := shapeBounds(c)
		if !b.Empty() {
			minx, miny = math.Min(minx, b.MinX), math.Min(miny, b.MinY)
			maxx, maxy = math.Max(maxx, b.MaxX), math.Max(maxy, b.MaxY)
		}
	}
	header := make([]byte, 100)
	binary.BigEndian.PutUint32(header[0:4], shpFileCode)
	binary.BigEndian.PutUint32(header[24:28], uint32((100+len(body))/2))
	binary.LittleEndian.PutUint32(header[28:32], 1000) // version
	binary.LittleEndian.PutUint32(header[32:36], uint32(shapeType))
	putF64(header, 36, minx)
	putF64(header, 44, miny)
	putF64(header, 52, maxx)
	putF64(header, 60, maxy)
	return append(header, body...)
}

// shapeBounds 从记录内容里取 bbox（点/多点/折线/多边形都带 Box，点型除外）。
func shapeBounds(c []byte) Box {
	if len(c) < 4 {
		return Box{}
	}
	switch int(binary.LittleEndian.Uint32(c[0:4])) {
	case shpPoint, shpPointZ, shpPointM:
		if len(c) < 20 {
			return Box{}
		}
		x, y := f64(c, 4), f64(c, 12)
		return Box{x, y, x, y}
	case shpMultiPoint, shpMultiPointZ, shpMultiPointM, shpPolyLine, shpPolyLineZ, shpPolyLineM,
		shpPolygon, shpPolygonZ, shpPolygonM:
		if len(c) < 36 {
			return Box{}
		}
		return Box{f64(c, 4), f64(c, 12), f64(c, 20), f64(c, 28)}
	}
	return Box{}
}

// shpPointContent 点记录：type + X + Y。
func shpPointContent(x, y float64) []byte {
	c := make([]byte, 20)
	binary.LittleEndian.PutUint32(c[0:4], shpPoint)
	putF64(c, 4, x)
	putF64(c, 12, y)
	return c
}

// shpPartsContent 折线/多边形记录：type + Box + NumParts + NumPoints + Parts + Points。
func shpPartsContent(shapeType int32, rings [][]Point) []byte {
	var pts []Point
	parts := make([]int32, 0, len(rings))
	for _, r := range rings {
		parts = append(parts, int32(len(pts)))
		pts = append(pts, r...)
	}
	c := make([]byte, 44+len(parts)*4+len(pts)*16)
	binary.LittleEndian.PutUint32(c[0:4], uint32(shapeType))
	b := Box{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for _, p := range pts {
		b.MinX, b.MinY = math.Min(b.MinX, p.X), math.Min(b.MinY, p.Y)
		b.MaxX, b.MaxY = math.Max(b.MaxX, p.X), math.Max(b.MaxY, p.Y)
	}
	putF64(c, 4, b.MinX)
	putF64(c, 12, b.MinY)
	putF64(c, 20, b.MaxX)
	putF64(c, 28, b.MaxY)
	binary.LittleEndian.PutUint32(c[36:40], uint32(len(parts)))
	binary.LittleEndian.PutUint32(c[40:44], uint32(len(pts)))
	for i, p := range parts {
		binary.LittleEndian.PutUint32(c[44+i*4:48+i*4], uint32(p))
	}
	off := 44 + len(parts)*4
	for i, p := range pts {
		putF64(c, off+i*16, p.X)
		putF64(c, off+i*16+8, p.Y)
	}
	return c
}

// dbfFieldSpec 测试用字段定义。
type dbfFieldSpec struct {
	Name string
	Type byte
	Len  int
	Dec  int
}

// buildDBF 组装 dBASE III 属性表（定长字段，右补空格）。
func buildDBF(fields []dbfFieldSpec, rows [][]string) []byte {
	headerSize := 32 + 32*len(fields) + 1
	recordSize := 1
	for _, f := range fields {
		recordSize += f.Len
	}
	out := make([]byte, headerSize+recordSize*len(rows))
	out[0] = 0x03
	out[1], out[2], out[3] = 126, 10, 5 // 日期（无关紧要）
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(rows)))
	binary.LittleEndian.PutUint16(out[8:10], uint16(headerSize))
	binary.LittleEndian.PutUint16(out[10:12], uint16(recordSize))
	for i, f := range fields {
		off := 32 + i*32
		copy(out[off:off+11], f.Name)
		out[off+11] = f.Type
		out[off+16] = byte(f.Len)
		out[off+17] = byte(f.Dec)
	}
	out[headerSize-1] = 0x0D
	pos := headerSize
	for _, r := range rows {
		out[pos] = 0x20 // 未删除
		off := pos + 1
		for i, f := range fields {
			val := ""
			if i < len(r) {
				val = r[i]
			}
			cell := make([]byte, f.Len)
			for k := range cell {
				cell[k] = ' '
			}
			if f.Type == 'N' || f.Type == 'F' {
				// 数值右对齐
				copy(cell[maxInt(0, f.Len-len(val)):], val)
			} else {
				copy(cell, val) // 字符左对齐
			}
			copy(out[off:off+f.Len], cell)
			off += f.Len
		}
		pos += recordSize
	}
	return out
}

// writeShpSet 把 .shp/.dbf/.prj/.cpg 落到临时目录，返回 .shp 路径。
func writeShpSet(t *testing.T, name string, shp []byte, dbf []byte, prj, cpg string) string {
	t.Helper()
	dir := t.TempDir()
	shpPath := filepath.Join(dir, name+".shp")
	if err := os.WriteFile(shpPath, shp, 0o644); err != nil {
		t.Fatal(err)
	}
	if dbf != nil {
		if err := os.WriteFile(filepath.Join(dir, name+".dbf"), dbf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if prj != "" {
		if err := os.WriteFile(filepath.Join(dir, name+".prj"), []byte(prj), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if cpg != "" {
		if err := os.WriteFile(filepath.Join(dir, name+".cpg"), []byte(cpg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return shpPath
}

const prjWGS84 = `GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984",SPHEROID["WGS_1984",6378137.0,298.257223563]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]]`

/* ------------------------------------------------------------------ */
/* 解析测试                                                            */
/* ------------------------------------------------------------------ */

// TestParseShapefilePolygon 多边形：面积要素 + 属性 + bbox。
func TestParseShapefilePolygon(t *testing.T) {
	// 一个 5x5 的方块，环为闭合（首尾点相同，Shapefile 的常规写法）
	sq := []Point{{0, 0}, {5, 0}, {5, 5}, {0, 5}, {0, 0}}
	shp := buildSHP(shpPolygon, [][]byte{shpPartsContent(shpPolygon, [][]Point{sq})})
	dbf := buildDBF(
		[]dbfFieldSpec{{"NAME", 'C', 16, 0}, {"POP", 'N', 10, 0}, {"RATIO", 'N', 8, 3}, {"OK", 'L', 1, 0}, {"DT", 'D', 8, 0}},
		[][]string{{"核心区", "1234", "0.125", "T", "20260315"}},
	)
	path := writeShpSet(t, "poly", shp, dbf, prjWGS84, "")

	feats, info, err := ParseShapefile(path)
	if err != nil {
		t.Fatalf("ParseShapefile: %v", err)
	}
	if info.Count != 1 || len(feats) != 1 {
		t.Fatalf("要素数=%d info.Count=%d, want 1", len(feats), info.Count)
	}
	if info.SRID != 4326 {
		t.Errorf("SRID=%d, want 4326", info.SRID)
	}
	if info.GeomType != GeomPolygon {
		t.Errorf("几何类型=%v, want Polygon", info.GeomType)
	}
	g := feats[0].Geom
	if g.Type != GeomPolygon || len(g.Lines) != 1 {
		t.Fatalf("几何=%+v", g)
	}
	// 内部模型统一开环：闭合点应被去掉
	if len(g.Lines[0]) != 4 {
		t.Errorf("环点数=%d, want 4（去掉闭合点）", len(g.Lines[0]))
	}
	if g.Exterior[0] != true {
		t.Error("唯一环应为外环")
	}
	b := info.BBox
	if b.MinX != 0 || b.MinY != 0 || b.MaxX != 5 || b.MaxY != 5 {
		t.Errorf("bbox=%+v, want 0,0,5,5", b)
	}

	p := feats[0].Props
	if p["NAME"] != "核心区" {
		t.Errorf("NAME=%v", p["NAME"])
	}
	if p["POP"] != float64(1234) {
		t.Errorf("POP=%v (%T), want 1234", p["POP"], p["POP"])
	}
	if p["RATIO"] != 0.125 {
		t.Errorf("RATIO=%v", p["RATIO"])
	}
	if p["OK"] != true {
		t.Errorf("OK=%v, want true", p["OK"])
	}
	if p["DT"] != "2026-03-15" {
		t.Errorf("DT=%v, want 2026-03-15", p["DT"])
	}
}

// TestParseShapefileRingRolesByContainment 环角色按**包含关系**判定，与绕向无关。
func TestParseShapefileRingRolesByContainment(t *testing.T) {
	outer := []Point{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}
	// 洞写成与"外环逆时针、洞顺时针"相反的方向，验证不依赖绕向
	hole := []Point{{2, 2}, {2, 4}, {4, 4}, {4, 2}, {2, 2}}
	shp := buildSHP(shpPolygon, [][]byte{shpPartsContent(shpPolygon, [][]Point{outer, hole})})
	path := writeShpSet(t, "holes", shp, nil, prjWGS84, "")

	feats, _, err := ParseShapefile(path)
	if err != nil {
		t.Fatalf("ParseShapefile: %v", err)
	}
	g := feats[0].Geom
	if len(g.Lines) != 2 || len(g.Exterior) != 2 {
		t.Fatalf("环数=%d 角色数=%d, want 2/2", len(g.Lines), len(g.Exterior))
	}
	if !g.Exterior[0] {
		t.Error("大环应为外环")
	}
	if g.Exterior[1] {
		t.Error("被包含的小环应判为内环（洞）")
	}

	// 反过来：洞的绕向翻转后结论不变
	revHole := []Point{{2, 2}, {4, 2}, {4, 4}, {2, 4}, {2, 2}}
	shp2 := buildSHP(shpPolygon, [][]byte{shpPartsContent(shpPolygon, [][]Point{outer, revHole})})
	path2 := writeShpSet(t, "holes2", shp2, nil, prjWGS84, "")
	feats2, _, err := ParseShapefile(path2)
	if err != nil {
		t.Fatalf("ParseShapefile(2): %v", err)
	}
	if feats2[0].Geom.Exterior[1] {
		t.Error("绕向翻转后仍应判为内环——判定不能依赖绕向")
	}
}

// TestParseShapefileMultiPolygonAndLine 多部件多边形与折线。
func TestParseShapefileMultiPolygonAndLine(t *testing.T) {
	a := []Point{{0, 0}, {1, 0}, {1, 1}, {0, 1}, {0, 0}}
	b := []Point{{5, 5}, {6, 5}, {6, 6}, {5, 6}, {5, 5}}
	shp := buildSHP(shpPolygon, [][]byte{shpPartsContent(shpPolygon, [][]Point{a, b})})
	path := writeShpSet(t, "multi", shp, nil, prjWGS84, "")
	feats, _, err := ParseShapefile(path)
	if err != nil {
		t.Fatalf("ParseShapefile: %v", err)
	}
	g := feats[0].Geom
	if len(g.Lines) != 2 || !g.Exterior[0] || !g.Exterior[1] {
		t.Errorf("互不包含的两个环都应为外环: exterior=%v", g.Exterior)
	}

	line := [][]Point{{{0, 0}, {1, 1}, {2, 0}}, {{5, 5}, {6, 6}}}
	shp2 := buildSHP(shpPolyLine, [][]byte{shpPartsContent(shpPolyLine, line)})
	path2 := writeShpSet(t, "line", shp2, nil, prjWGS84, "")
	feats2, info2, err := ParseShapefile(path2)
	if err != nil {
		t.Fatalf("ParseShapefile(line): %v", err)
	}
	if info2.GeomType != GeomLine || feats2[0].Geom.Type != GeomLine {
		t.Errorf("折线图层类型=%v", info2.GeomType)
	}
	if len(feats2[0].Geom.Lines) != 2 {
		t.Errorf("折线段数=%d, want 2", len(feats2[0].Geom.Lines))
	}
}

// TestParseShapefilePointsAndNoDBF 点图层 + 缺 .dbf（应降级可用并给出提示）。
func TestParseShapefilePointsAndNoDBF(t *testing.T) {
	shp := buildSHP(shpPoint, [][]byte{shpPointContent(116.4, 39.9), shpPointContent(116.5, 39.8)})
	path := writeShpSet(t, "pts", shp, nil, prjWGS84, "")
	feats, info, err := ParseShapefile(path)
	if err != nil {
		t.Fatalf("ParseShapefile: %v", err)
	}
	if len(feats) != 2 || info.GeomType != GeomPoint {
		t.Fatalf("要素数=%d 类型=%v", len(feats), info.GeomType)
	}
	if feats[0].Geom.Lines[0][0].X != 116.4 {
		t.Errorf("坐标=%v", feats[0].Geom.Lines[0][0])
	}
	if feats[0].Props != nil {
		t.Errorf("无 .dbf 时不应有属性: %v", feats[0].Props)
	}
	if len(info.Notes) == 0 {
		t.Error("缺 .dbf 应给出提示（不静默）")
	}
}

// TestParseShapefileGBKAttributes 国内 Shapefile 的 .dbf 多为 GBK，必须正确解码。
func TestParseShapefileGBKAttributes(t *testing.T) {
	// "北京" 的 GBK 字节：B1 B1 BE A9
	gbkName := string([]byte{0xB1, 0xB1, 0xBE, 0xA9})
	shp := buildSHP(shpPoint, [][]byte{shpPointContent(1, 2)})
	dbf := buildDBF([]dbfFieldSpec{{"MC", 'C', 16, 0}}, [][]string{{gbkName}})
	path := writeShpSet(t, "gbk", shp, dbf, prjWGS84, "")

	feats, _, err := ParseShapefile(path)
	if err != nil {
		t.Fatalf("ParseShapefile: %v", err)
	}
	if got := feats[0].Props["MC"]; got != "北京" {
		t.Errorf("GBK 属性解码=%v, want 北京（乱码说明没走 GBK 解码）", got)
	}

	// 同一份字节在 .cpg 声明 UTF-8 时不应被强制按 GBK 解（尊重显式声明）
	path2 := writeShpSet(t, "gbk_utf8cpg", shp, dbf, prjWGS84, "UTF-8")
	feats2, _, err := ParseShapefile(path2)
	if err != nil {
		t.Fatalf("ParseShapefile(cpg=UTF-8): %v", err)
	}
	// 非法 UTF-8 字节仍会回退 GBK 解码（否则用户只能看到替换符）
	if got := feats2[0].Props["MC"]; got != "北京" {
		t.Errorf("非法 UTF-8 应回退 GBK 解码，得到 %v", got)
	}
}

// TestSRIDFromPRJ 坐标系判定与不支持坐标系的明确报错。
func TestSRIDFromPRJ(t *testing.T) {
	cases := []struct {
		name string
		wkt  string
		want int
		fail bool
	}{
		{"esri wgs84", prjWGS84, 4326, false},
		{"ogc 4326 带权威码", `GEOGCS["WGS 84",AUTHORITY["EPSG","4326"]]`, 4326, false},
		{"cgcs2000", `GEOGCS["China Geodetic Coordinate System 2000",AUTHORITY["EPSG","4490"]]`, 4490, false},
		{"cgcs2000 无权威码", `GEOGCS["CGCS2000",DATUM["China_2000"]]`, 4490, false},
		{"web mercator ogc", `PROJCS["WGS 84 / Pseudo-Mercator",AUTHORITY["EPSG","3857"]]`, 3857, false},
		{"web mercator esri", `PROJCS["WGS_1984_Web_Mercator_Auxiliary_Sphere",PROJECTION["Mercator_Auxiliary_Sphere"]]`, 3857, false},
		{"900913 别名", `PROJCS["Google Maps",AUTHORITY["EPSG","900913"]]`, 3857, false},
		{"高斯克吕格 4547", `PROJCS["CGCS2000 / 3-degree Gauss-Kruger CM 114E",AUTHORITY["EPSG","4547"]]`, 0, true},
		{"北京54", `GEOGCS["GCS_Beijing_1954",AUTHORITY["EPSG","4214"]]`, 0, true},
		{"无法判定", `LOCAL_CS["unknown"]`, 0, true},
	}
	for _, c := range cases {
		got, _, err := sridFromPRJ(c.wkt)
		if c.fail {
			if err == nil {
				t.Errorf("%s: 应报错（不支持/无法判定），得到 srid=%d", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: srid=%d, want %d", c.name, got, c.want)
		}
	}
}

// TestUnsupportedCRSRejected 不支持的坐标系必须明确失败，而不是静默当 WGS84。
func TestUnsupportedCRSRejected(t *testing.T) {
	shp := buildSHP(shpPoint, [][]byte{shpPointContent(1, 2)})
	prj := `PROJCS["CGCS2000 / 3-degree Gauss-Kruger CM 114E",AUTHORITY["EPSG","4547"]]`
	path := writeShpSet(t, "gk", shp, nil, prj, "")
	if _, _, err := ParseShapefile(path); err == nil {
		t.Fatal("高斯克吕格投影应报错（静默当 WGS84 会引入上百米系统偏差）")
	} else if !errorIs(err, ErrInvalid) {
		t.Errorf("错误类型应为 ErrInvalid, 得到 %v", err)
	}
}

// TestCGCS2000AcceptedThroughGateway CGCS2000（EPSG:4490）在网关侧必须被接受。
//
// 回归用例：网关里曾留着一份"只认 4326/3857"的旧白名单，而各解析器已支持 4490，
// 导致 4490 数据在注册阶段被拒（单测覆盖不到，E2E 才暴露）。
// 4490 与 WGS84 的差异在厘米级，可直接按经纬度使用。
func TestCGCS2000AcceptedThroughGateway(t *testing.T) {
	ctx := context.Background()
	shp := buildSHP(shpPoint, [][]byte{shpPointContent(116.4, 39.9)})
	prj := `GEOGCS["China Geodetic Coordinate System 2000",DATUM["China_2000",SPHEROID["CGCS2000",6378137,298.257222101]],PRIMEM["Greenwich",0],UNIT["Degree",0.0174532925199433],AUTHORITY["EPSG","4490"]]`
	path := writeShpSet(t, "cgcs", shp, nil, prj, "")

	g := NewFileGateway()
	defer g.Close()
	dsn := FileDSN(path)
	if _, err := g.Ping(ctx, dsn); err != nil {
		t.Fatalf("4490 应被接受: %v", err)
	}
	_, srid, _, err := g.DetectGeometry(ctx, dsn, "", "", "")
	if err != nil {
		t.Fatalf("DetectGeometry(4490): %v", err)
	}
	if srid != 4490 {
		t.Errorf("srid=%d, want 4490", srid)
	}
	// 瓦片要能出（4490 按经纬度处理，与 4326 同一套投影路径）
	layer := &Layer{Name: "cgcs", SRID: 4490}
	tx, ty := tileXYAt(116.4, 39.9, 12)
	bx0, by0, bx1, by1 := TileBBox(12, tx, ty)
	mvt, count, err := g.TileMVT(ctx, dsn, layer, bx0, by0, bx1, by1, 1000)
	if err != nil {
		t.Fatalf("TileMVT(4490): %v", err)
	}
	if count == 0 || len(mvt) == 0 {
		t.Errorf("4490 图层应能出瓦片: count=%d bytes=%d", count, len(mvt))
	}
}

// prjCGCS2000GK 是 CGCS2000 3 度带（中央经线 114°E，EPSG:4547）的 OGC WKT。
const prjCGCS2000GK = `PROJCS["CGCS2000 / 3-degree Gauss-Kruger CM 114E",GEOGCS["China Geodetic Coordinate System 2000",DATUM["China_2000",SPHEROID["CGCS2000",6378137,298.257222101]],PRIMEM["Greenwich",0],UNIT["degree",0.0174532925199433]],PROJECTION["Transverse_Mercator"],PARAMETER["latitude_of_origin",0],PARAMETER["central_meridian",114],PARAMETER["scale_factor",1],PARAMETER["false_easting",500000],PARAMETER["false_northing",0],UNIT["metre",1],AUTHORITY["EPSG","4547"]]`

// TestProjectedShapefileTransform 投影坐标的高斯克吕格数据应被换算成 WGS84 经纬度。
//
// 这是"国内投影坐标数据能不能用"的核心用例：造一份 4547 坐标的 .shp，
// 解析后应回到原始经纬度，且图层 SRID 记为换算后的 4326。
func TestProjectedShapefileTransform(t *testing.T) {
	tm := tmParams{Ell: ellipsoidCGCS2000, Lon0: 114, Lat0: 0, K0: 1, FE: 500000, FN: 0}
	const lon, lat = 116.397, 39.908
	x, y := tm.forward(lon, lat)
	ring := []Point{{x, y}, {x + 2000, y}, {x + 2000, y + 2000}, {x, y + 2000}, {x, y}}
	shp := buildSHP(shpPolygon, [][]byte{shpPartsContent(shpPolygon, [][]Point{ring})})
	path := writeShpSet(t, "proj_gk", shp, nil, prjCGCS2000GK, "")

	feats, info, err := ParseShapefile(path)
	if err != nil {
		t.Fatalf("ParseShapefile: %v", err)
	}
	if info.SRID != 4326 {
		t.Errorf("换算后 SRID=%d, want 4326（不能沿用源坐标系码）", info.SRID)
	}
	if len(feats) != 1 {
		t.Fatalf("要素数=%d", len(feats))
	}
	p := feats[0].Geom.Lines[0][0]
	if math.Abs(p.X-lon) > 1e-8 || math.Abs(p.Y-lat) > 1e-8 {
		t.Errorf("换算后坐标=(%.9f, %.9f), want (%.9f, %.9f)", p.X, p.Y, lon, lat)
	}
	// bbox 应落在经纬度量级（若未换算会是几十万的投影值）
	b := info.BBox
	if b.MinX < 100 || b.MaxX > 130 || b.MinY < 20 || b.MaxY > 50 {
		t.Errorf("bbox=%+v 不像经纬度（未换算？）", b)
	}
	// 提示里要说清源坐标系与换算方式
	note := strings.Join(info.Notes, "；")
	if !strings.Contains(note, "源坐标系") || !strings.Contains(note, "横轴墨卡托") {
		t.Errorf("应给出源坐标系与换算方式提示, 得到 %q", note)
	}
	if !strings.Contains(note, "4547") {
		t.Errorf("提示应带上源坐标系码: %q", note)
	}
}

// TestProjectedShapefileThroughGateway 投影坐标数据经网关出图。
func TestProjectedShapefileThroughGateway(t *testing.T) {
	ctx := context.Background()
	tm := tmParams{Ell: ellipsoidCGCS2000, Lon0: 114, Lat0: 0, K0: 1, FE: 500000, FN: 0}
	x, y := tm.forward(116.39, 39.90)
	ring := []Point{{x, y}, {x + 3000, y}, {x + 3000, y + 3000}, {x, y + 3000}, {x, y}}
	shp := buildSHP(shpPolygon, [][]byte{shpPartsContent(shpPolygon, [][]Point{ring})})
	path := writeShpSet(t, "gk_tile", shp, nil, prjCGCS2000GK, "")

	g := NewFileGateway()
	defer g.Close()
	dsn := FileDSN(path)
	_, srid, _, err := g.DetectGeometry(ctx, dsn, "", "", "")
	if err != nil || srid != 4326 {
		t.Fatalf("DetectGeometry srid=%d err=%v, want 4326", srid, err)
	}
	layer := &Layer{Name: "gk_tile", SRID: 4326}
	tx, ty := tileXYAt(116.39, 39.90, 13)
	bx0, by0, bx1, by1 := TileBBox(13, tx, ty)
	mvt, count, err := g.TileMVT(ctx, dsn, layer, bx0, by0, bx1, by1, 10000)
	if err != nil {
		t.Fatalf("TileMVT: %v", err)
	}
	if count == 0 || len(mvt) == 0 {
		t.Fatal("换算后的投影数据应能在北京所在瓦片出图")
	}
	dl := decodeMVT(t, mvt)
	for _, f := range dl.features {
		for _, r := range ringsOf(decodeGeomOps(t, f.cmds)) {
			for _, pt := range r {
				// 瓦片坐标须落在范围内——若忘了换算，投影值会被映射到离谱位置
				if pt.X < -64 || pt.X > 4160 || pt.Y < -64 || pt.Y > 4160 {
					t.Errorf("瓦片坐标越界: %+v", pt)
				}
			}
		}
	}
}

// TestParseShapefileErrors 非 Shapefile / 截断文件的错误处理。
func TestParseShapefileErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.shp")
	if err := os.WriteFile(bad, []byte("definitely not a shapefile"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseShapefile(bad); err == nil {
		t.Error("非 Shapefile 应报错")
	}
	// 声明长度大于实际 → 截断
	shp := buildSHP(shpPoint, [][]byte{shpPointContent(1, 2)})
	trunc := filepath.Join(dir, "trunc.shp")
	if err := os.WriteFile(trunc, shp[:len(shp)-4], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseShapefile(trunc); err == nil {
		t.Error("截断文件应报错")
	}
	if _, _, err := ParseShapefile(filepath.Join(dir, "missing.shp")); err == nil {
		t.Error("文件不存在应报错")
	}
}

// TestShapefileThroughGateway 经文件网关走完整链路：注册探测 → 出 MVT → WFS。
func TestShapefileThroughGateway(t *testing.T) {
	ctx := context.Background()
	// 在北京一带放两个方块，便于按瓦片命中
	a := []Point{{116.38, 39.89}, {116.40, 39.89}, {116.40, 39.91}, {116.38, 39.91}, {116.38, 39.89}}
	b := []Point{{116.42, 39.89}, {116.44, 39.89}, {116.44, 39.91}, {116.42, 39.91}, {116.42, 39.89}}
	shp := buildSHP(shpPolygon, [][]byte{
		shpPartsContent(shpPolygon, [][]Point{a}),
		shpPartsContent(shpPolygon, [][]Point{b}),
	})
	dbf := buildDBF([]dbfFieldSpec{{"NAME", 'C', 20, 0}}, [][]string{{"甲区"}, {"乙区"}})
	path := writeShpSet(t, "beijing_shp", shp, dbf, prjWGS84, "")

	g := NewFileGateway()
	defer g.Close()
	dsn := FileDSN(path)

	ver, err := g.Ping(ctx, dsn)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !contains(ver, "file-vector/shp") {
		t.Errorf("Ping 摘要=%q, 应标明 shp 格式", ver)
	}

	col, srid, gtype, err := g.DetectGeometry(ctx, dsn, "", "", "")
	if err != nil || col != "geometry" || srid != 4326 || gtype != "POLYGON" {
		t.Fatalf("DetectGeometry=(%q,%d,%q,%v)", col, srid, gtype, err)
	}
	fields, err := g.DetectFields(ctx, dsn, "", "")
	if err != nil || len(fields) != 1 || fields[0] != "NAME" {
		t.Fatalf("DetectFields=%v (%v)", fields, err)
	}
	if n, err := g.EstimatedRows(ctx, dsn, nil); err != nil || n != 2 {
		t.Errorf("要素数=%d (%v), want 2", n, err)
	}

	layer := &Layer{Name: "beijing_shp", SRID: 4326}
	tx, ty := tileXYAt(116.39, 39.90, 12)
	bx0, by0, bx1, by1 := TileBBox(12, tx, ty)
	mvt, count, err := g.TileMVT(ctx, dsn, layer, bx0, by0, bx1, by1, 10000)
	if err != nil {
		t.Fatalf("TileMVT: %v", err)
	}
	if count == 0 || len(mvt) == 0 {
		t.Fatalf("瓦片应命中要素 count=%d bytes=%d", count, len(mvt))
	}
	l := decodeMVT(t, mvt)
	if l.name != "beijing_shp" {
		t.Errorf("MVT 图层名=%q", l.name)
	}
	if len(l.keys) != 1 || l.keys[0] != "NAME" {
		t.Errorf("MVT 属性字典=%v", l.keys)
	}

	data, n, err := g.FeaturesGeoJSON(ctx, dsn, layer, false, 0, 0, 0, 0, 100)
	if err != nil || n != 2 {
		t.Fatalf("FeaturesGeoJSON n=%d (%v)", n, err)
	}
	if !contains(string(data), "甲区") {
		t.Errorf("WFS 输出丢失属性: %s", truncate(string(data), 200))
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
