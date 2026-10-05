package vector

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* ------------------------------------------------------------------ */
/* 导出测试的核心理念：把导出结果喂回本包的解析器验证                     */
/*                                                                     */
/* 「看起来像 Shapefile」不等于「其它 GIS 软件能读懂」。用自家解析器做       */
/* 往返，等价于用一套独立实现交叉验证格式细节（字节序、定长、环方向、       */
/* 字段宽度…），比人工核对十六进制可靠得多。                              */
/* ------------------------------------------------------------------ */

// exportFixture 构造一个覆盖多种情况的要素集：带洞多边形、普通多边形、中文属性、
// 整型/浮点/布尔/日期字段。
func exportFixture() []FileFeature {
	outer := []Point{{116.380, 39.890}, {116.400, 39.890}, {116.400, 39.910}, {116.380, 39.910}, {116.380, 39.890}}
	hole := []Point{{116.386, 39.896}, {116.394, 39.896}, {116.394, 39.904}, {116.386, 39.904}, {116.386, 39.896}}
	sq := []Point{{116.420, 39.890}, {116.440, 39.890}, {116.440, 39.910}, {116.420, 39.910}, {116.420, 39.890}}
	return []FileFeature{
		{
			ID: 1,
			Geom: Geometry{Type: GeomPolygon,
				Lines:    [][]Point{outer, hole},
				Exterior: []bool{true, false}},
			Props: map[string]any{
				"name":   "带洞地块",
				"code":   "BJDC01",
				"area":   float64(1234.5),
				"count":  int64(7),
				"active": true,
				"upd":    "2026-03-15",
			},
		},
		{
			ID:   2,
			Geom: Geometry{Type: GeomPolygon, Lines: [][]Point{sq}, Exterior: []bool{true}},
			Props: map[string]any{
				"name":   "独立地块",
				"code":   "BJXC02",
				"area":   float64(56.25),
				"count":  int64(12),
				"active": false,
				"upd":    "2026-04-01",
			},
		},
	}
}

// findFeature 按属性名找要素（属性键名可能被截断，用前缀匹配）。
func findFeature(t *testing.T, feats []FileFeature, nameKey, want string) *FileFeature {
	t.Helper()
	for i := range feats {
		for k, v := range feats[i].Props {
			if strings.HasPrefix(k, nameKey) && v == want {
				return &feats[i]
			}
		}
	}
	return nil
}

// TestExportGeoJSONRoundTrip GeoJSON 导出 → 重新解析。
func TestExportGeoJSONRoundTrip(t *testing.T) {
	src := exportFixture()
	res, err := exportGeoJSON("blocks", src)
	if err != nil {
		t.Fatalf("exportGeoJSON: %v", err)
	}
	feats, info, err := ParseGeoJSON(res.Data)
	if err != nil {
		t.Fatalf("导出结果无法被解析回来: %v", err)
	}
	if len(feats) != 2 || info.Count != 2 {
		t.Fatalf("要素数=%d", len(feats))
	}
	if info.GeomType != GeomPolygon {
		t.Errorf("几何类型=%v", info.GeomType)
	}
	f := findFeature(t, feats, "name", "带洞地块")
	if f == nil {
		t.Fatal("中文属性丢失")
	}
	if len(f.Geom.Lines) != 2 || !f.Geom.Exterior[0] || f.Geom.Exterior[1] {
		t.Errorf("环角色丢失: lines=%d exterior=%v", len(f.Geom.Lines), f.Geom.Exterior)
	}
	if f.Props["count"] != float64(7) {
		t.Errorf("整型属性=%v (%T)", f.Props["count"], f.Props["count"])
	}
	if f.Props["area"] != 1234.5 {
		t.Errorf("浮点属性=%v", f.Props["area"])
	}
	if f.Props["active"] != true {
		t.Errorf("布尔属性=%v", f.Props["active"])
	}
}

// TestExportShapefileRoundTrip Shapefile 导出 → 解压 → 重新解析。
func TestExportShapefileRoundTrip(t *testing.T) {
	res, err := exportShapefile("blocks", exportFixture(), 4326)
	if err != nil {
		t.Fatalf("exportShapefile: %v", err)
	}
	if res.Skipped != 0 || len(res.Warnings) == 0 {
		t.Errorf("Skipped=%d warnings=%v", res.Skipped, res.Warnings)
	}
	dir := unzipToTemp(t, res.Data)
	shp := filepath.Join(dir, "blocks.shp")

	feats, info, err := ParseShapefile(shp)
	if err != nil {
		t.Fatalf("导出的 Shapefile 无法被解析回来: %v", err)
	}
	if len(feats) != 2 {
		t.Fatalf("要素数=%d, want 2", len(feats))
	}
	// .prj 写的是 WGS84，解析后应回到 4326
	if info.SRID != 4326 {
		t.Errorf("SRID=%d, want 4326（.prj 往返失败）", info.SRID)
	}
	if info.GeomType != GeomPolygon {
		t.Errorf("几何类型=%v", info.GeomType)
	}
	f := findFeature(t, feats, "name", "带洞地块")
	if f == nil {
		t.Fatalf("中文属性丢失（.dbf 编码或宽度问题）; 实际属性=%v", feats[0].Props)
	}
	if len(f.Geom.Lines) != 2 {
		t.Errorf("环数=%d, want 2（洞丢失）", len(f.Geom.Lines))
	}
	// 洞必须仍被判为内环（导出时按 ESRI 约定反了绕向，靠包含关系才能正确重判）
	if f.Geom.Exterior[1] {
		t.Error("洞被读成了外环（绕向/角色问题）")
	}
	if f.Props["count"] != float64(7) {
		t.Errorf("整型字段=%v (%T)", f.Props["count"], f.Props["count"])
	}
	if f.Props["area"] != 1234.5 {
		t.Errorf("浮点字段=%v", f.Props["area"])
	}
	if f.Props["active"] != true {
		t.Errorf("布尔字段=%v", f.Props["active"])
	}
	if f.Props["upd"] != "2026-03-15" {
		t.Errorf("日期字段=%v", f.Props["upd"])
	}
	// 坐标保真：多边形范围应一致
	if math.Abs(info.BBox.MinX-116.38) > 1e-9 || math.Abs(info.BBox.MaxX-116.44) > 1e-9 {
		t.Errorf("bbox=%+v", info.BBox)
	}
}

// TestExportGeoPackageRoundTrip GeoPackage 导出 → 重新解析。
func TestExportGeoPackageRoundTrip(t *testing.T) {
	res, err := exportGeoPackage("blocks", exportFixture(), 4326)
	if err != nil {
		t.Fatalf("exportGeoPackage: %v", err)
	}
	defer res.TempDirCleanup()

	if res.Path == "" || res.Filename != "blocks.gpkg" {
		t.Fatalf("产物=%+v", res)
	}
	// 文件应是合法 GeoPackage（能列出要素表）
	tabs, err := ListGeoPackageTables(res.Path)
	if err != nil {
		t.Fatalf("导出的 GeoPackage 无法被识别: %v", err)
	}
	if len(tabs) != 1 || tabs[0].Name != "blocks" || tabs[0].SRID != 4326 {
		t.Fatalf("要素表=%+v", tabs)
	}
	if tabs[0].GeometryCol != "geom" {
		t.Errorf("几何列=%q, want geom", tabs[0].GeometryCol)
	}

	feats, info, err := ReadGeoPackageTable(res.Path, "blocks", "")
	if err != nil {
		t.Fatalf("ReadGeoPackageTable: %v", err)
	}
	if len(feats) != 2 || info.SRID != 4326 {
		t.Fatalf("要素数=%d srid=%d", len(feats), info.SRID)
	}
	f := findFeature(t, feats, "name", "带洞地块")
	if f == nil {
		t.Fatalf("属性丢失: %v", feats[0].Props)
	}
	if len(f.Geom.Lines) != 2 || f.Geom.Exterior[1] {
		t.Errorf("环角色丢失: lines=%d exterior=%v", len(f.Geom.Lines), f.Geom.Exterior)
	}
	if f.Props["count"] != int64(7) {
		t.Errorf("整型字段=%v (%T)", f.Props["count"], f.Props["count"])
	}
	if f.Props["area"] != 1234.5 {
		t.Errorf("浮点字段=%v", f.Props["area"])
	}
	// 坐标保真
	p := f.Geom.Lines[0][0]
	if math.Abs(p.X-116.38) > 1e-9 || math.Abs(p.Y-39.89) > 1e-9 {
		t.Errorf("坐标=%.9f,%.9f", p.X, p.Y)
	}
}

// TestExportShapefileMixedGeometryWarns 混合几何类型：必须跳过并明确告知。
func TestExportShapefileMixedGeometryWarns(t *testing.T) {
	feats := append(exportFixture(),
		FileFeature{ID: 3, Geom: Geometry{Type: GeomPoint, Lines: [][]Point{{{X: 116.4, Y: 39.9}}}},
			Props: map[string]any{"name": "点位"}})
	res, err := exportShapefile("mixed", feats, 4326)
	if err != nil {
		t.Fatalf("exportShapefile: %v", err)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped=%d, want 1", res.Skipped)
	}
	if res.Features != 2 {
		t.Errorf("Features=%d, want 2", res.Features)
	}
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, "跳过 1 条") {
		t.Errorf("应提示跳过了要素: %v", res.Warnings)
	}
	// 导出的文件里确实只有面
	dir := unzipToTemp(t, res.Data)
	_, info, err := ParseShapefile(filepath.Join(dir, "mixed.shp"))
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if info.GeomType != GeomPolygon {
		t.Errorf("几何类型=%v", info.GeomType)
	}
}

// TestExportShapefileLongFieldNames 字段名超 10 字符需截断，但**取值不能丢**。
//
// 回归用例：DBF 头里写的是截断后的名字，取值时若也按截断名查属性，
// 所有属性都会落空——导出看起来正常，打开才发现整张表是空的。
func TestExportShapefileLongFieldNames(t *testing.T) {
	feats := []FileFeature{{
		ID:   1,
		Geom: Geometry{Type: GeomPolygon, Lines: [][]Point{{{0, 0}, {1, 0}, {1, 1}, {0, 0}}}, Exterior: []bool{true}},
		Props: map[string]any{
			"very_long_field_name_a": "值甲", // 22 字符 → 截断
			"another_long_field_bbb": int64(42),
		},
	}}
	res, err := exportShapefile("longname", feats, 4326)
	if err != nil {
		t.Fatalf("exportShapefile: %v", err)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "字段名") {
		t.Errorf("应提示字段名被截断: %v", res.Warnings)
	}
	dir := unzipToTemp(t, res.Data)
	got, _, err := ParseShapefile(filepath.Join(dir, "longname.shp"))
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("要素数=%d", len(got))
	}
	// 字段名被截断到 10 字节，但值必须还在
	var foundText, foundNum bool
	for k, v := range got[0].Props {
		if strings.HasPrefix("very_long_field_name_a", k) && v == "值甲" {
			foundText = true
		}
		if strings.HasPrefix("another_long_field_bbb", k) && v == float64(42) {
			foundNum = true
		}
	}
	if !foundText || !foundNum {
		t.Errorf("截断后属性取值丢失: %v", got[0].Props)
	}
}

// TestExportShapefilePointLayers 点图层（含多点）导出类型选择。
func TestExportShapefilePointLayers(t *testing.T) {
	single := []FileFeature{
		{ID: 1, Geom: Geometry{Type: GeomPoint, Lines: [][]Point{{{X: 1, Y: 1}}}}, Props: map[string]any{"n": "a"}},
		{ID: 2, Geom: Geometry{Type: GeomPoint, Lines: [][]Point{{{X: 2, Y: 2}}}}, Props: map[string]any{"n": "b"}},
	}
	res, err := exportShapefile("pts", single, 4326)
	if err != nil {
		t.Fatalf("exportShapefile: %v", err)
	}
	dir := unzipToTemp(t, res.Data)
	got, info, err := ParseShapefile(filepath.Join(dir, "pts.shp"))
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(got) != 2 || info.GeomType != GeomPoint {
		t.Fatalf("要素数=%d 类型=%v", len(got), info.GeomType)
	}

	// 含多点 → 整文件用 MultiPoint，单点要素也要能往返
	multi := append(single, FileFeature{ID: 3,
		Geom:  Geometry{Type: GeomPoint, Lines: [][]Point{{{X: 3, Y: 3}}, {{X: 4, Y: 4}}}},
		Props: map[string]any{"n": "c"}})
	res2, err := exportShapefile("multi_pts", multi, 4326)
	if err != nil {
		t.Fatalf("exportShapefile(多点): %v", err)
	}
	dir2 := unzipToTemp(t, res2.Data)
	got2, _, err := ParseShapefile(filepath.Join(dir2, "multi_pts.shp"))
	if err != nil {
		t.Fatalf("解析(多点): %v", err)
	}
	if len(got2) != 3 {
		t.Fatalf("要素数=%d, want 3", len(got2))
	}
	var pts int
	for _, f := range got2 {
		for _, l := range f.Geom.Lines {
			pts += len(l)
		}
	}
	if pts != 4 {
		t.Errorf("总点数=%d, want 4（多点展开失败）", pts)
	}
}

// TestExportUnsupportedSRID 不支持的坐标系必须报错而不是写个猜的 .prj。
func TestExportUnsupportedSRID(t *testing.T) {
	if _, err := exportShapefile("x", exportFixture(), 4547); err == nil {
		t.Error("投影坐标系导出为 Shapefile 应报错（无法生成正确的 .prj）")
	} else if !strings.Contains(err.Error(), "4547") {
		t.Errorf("错误信息应带上坐标系码: %v", err)
	}
	if _, err := exportGeoPackage("x", exportFixture(), 4547); err == nil {
		t.Error("投影坐标系导出为 GeoPackage 应报错")
	}
	// 3857 / 4490 应可用
	if _, err := exportShapefile("x", exportFixture(), 3857); err != nil {
		t.Errorf("3857 应可导出: %v", err)
	}
	if _, err := exportGeoPackage("x", exportFixture(), 4490); err != nil {
		t.Errorf("4490 应可导出: %v", err)
	}
}

// TestExportLayerThroughServer 走 Server.ExportLayer 全链路（文件数据源）。
func TestExportLayerThroughServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	geojson := `{"type":"FeatureCollection","features":[
		{"type":"Feature","id":1,"properties":{"名称":"甲区","面积":12.5},
		 "geometry":{"type":"Polygon","coordinates":[[[116.38,39.89],[116.40,39.89],[116.40,39.91],[116.38,39.91],[116.38,39.89]]]}}
	]}`
	src := filepath.Join(dir, "areas.geojson")
	if err := os.WriteFile(src, []byte(geojson), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err := NewJSONMetaStore(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Meta: meta, Gateway: NewRouterGateway(NewFileGateway(), nil)}
	if _, _, err := srv.RegisterFileSource(ctx, RegFileReq{Path: src, Name: "areas"}); err != nil {
		t.Fatalf("RegisterFileSource: %v", err)
	}

	for _, format := range []ExportFormat{ExportGeoJSON, ExportShapefile, ExportGeoPackage} {
		res, err := srv.ExportLayer(ctx, "areas", format)
		if err != nil {
			t.Fatalf("ExportLayer(%s): %v", format, err)
		}
		if res.Features != 1 {
			t.Errorf("%s: Features=%d, want 1", format, res.Features)
		}
		if res.Size() == 0 {
			t.Errorf("%s: 产物为空", format)
		}
		if res.Filename == "" || res.MimeType == "" {
			t.Errorf("%s: 缺少文件名/类型: %+v", format, res)
		}
		// 三种产物都要能被读回来（GeoJSON 与 GPKG 直接读，SHP 解压后读）
		switch format {
		case ExportGeoJSON:
			var doc struct {
				Type     string `json:"type"`
				Features []any  `json:"features"`
			}
			if err := json.Unmarshal(res.Data, &doc); err != nil {
				t.Fatalf("GeoJSON 非法: %v", err)
			}
			if doc.Type != "FeatureCollection" || len(doc.Features) != 1 {
				t.Errorf("GeoJSON 结构异常: %+v", doc)
			}
		case ExportGeoPackage:
			feats, _, err := ReadGeoPackageTable(res.Path, "areas", "")
			if err != nil || len(feats) != 1 {
				t.Errorf("GPKG 读回失败: feats=%d err=%v", len(feats), err)
			}
			res.TempDirCleanup()
		case ExportShapefile:
			un := unzipToTemp(t, res.Data)
			feats, info, err := ParseShapefile(filepath.Join(un, "areas.shp"))
			if err != nil || len(feats) != 1 || info.SRID != 4326 {
				t.Errorf("SHP 读回失败: feats=%d srid=%d err=%v", len(feats), info.SRID, err)
			}
			if feats[0].Props["名称"] != "甲区" {
				t.Errorf("中文属性读回失败: %v", feats[0].Props)
			}
		}
	}

	// 不存在的图层 → ErrNotFound
	if _, err := srv.ExportLayer(ctx, "nope", ExportGeoJSON); err == nil {
		t.Error("不存在的图层应报错")
	}
	// 格式非法
	if _, err := ParseExportFormat("kml"); err == nil {
		t.Error("非法格式应报错")
	}
}

// TestExportShapefileWinding 导出 Shapefile 时环方向须符合 ESRI 约定（外环顺时针）。
func TestExportShapefileWinding(t *testing.T) {
	// 构造一个外环逆时针（GeoJSON 约定）的多边形
	ccw := []Point{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}
	if signedArea(ccw) <= 0 {
		t.Fatal("测试数据应为逆时针（正面积）")
	}
	g := Geometry{Type: GeomPolygon, Lines: [][]Point{ccw}, Exterior: []bool{true}}
	content := shpRecordContent(shpPolygon, g)
	// 直接从记录内容里取点，验证写出后面积为负（顺时针）
	if len(content) < 44+16*4 {
		t.Fatalf("记录长度=%d", len(content))
	}
	ptsOff := 44 + 1*4
	var ring []Point
	for i := 0; i < 4; i++ {
		ring = append(ring, Point{X: f64(content, ptsOff+i*16), Y: f64(content, ptsOff+i*16+8)})
	}
	if a := signedArea(ring); a >= 0 {
		t.Errorf("Shapefile 外环面积=%.1f，应为负（ESRI 要求外环顺时针）", a)
	}
	// GeoPackage 走相反约定：外环逆时针
	ng := normalizeForGeoPackage(g)
	if a := signedArea(ng.Lines[0]); a <= 0 {
		t.Errorf("GeoPackage 外环面积=%.1f，应为正（OGC 要求外环逆时针）", a)
	}
}

// unzipToTemp 把 zip 产物解到临时目录，返回目录。
func unzipToTemp(t *testing.T, data []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("产物不是合法 zip: %v", err)
	}
	dir := t.TempDir()
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			rc.Close()
			t.Fatal(err)
		}
		rc.Close()
		if err := os.WriteFile(filepath.Join(dir, f.Name), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
