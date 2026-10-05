package vector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* ------------------------------------------------------------------ */
/* 编辑写回测试                                                        */
/*                                                                     */
/* 编辑是不可逆的原地写入，所以测试的重点不只是"改动生效"，还有：          */
/*   ① 备份是否存在且保存的是**编辑前**的内容（第二、三次编辑不得覆盖它）  */
/*   ② GeoPackage 同库的其它要素表是否安然无恙（整文件重写会毁掉它们）     */
/* ------------------------------------------------------------------ */

func editPair() []FileFeature {
	sq := []Point{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}
	return []FileFeature{
		{ID: 1, Geom: Geometry{Type: GeomPolygon, Lines: [][]Point{sq}, Exterior: []bool{true}},
			Props: map[string]any{"name": "甲"}},
		{ID: 2, Geom: Geometry{Type: GeomPolygon, Lines: [][]Point{sq}, Exterior: []bool{true}},
			Props: map[string]any{"name": "乙"}},
	}
}

// editOpJSON 便捷构造编辑操作。
func editOpJSON(op string, id uint64, geom map[string]any, props map[string]any) EditOp {
	e := EditOp{Op: op, ID: id, Properties: props}
	if geom != nil {
		raw, _ := json.Marshal(geom)
		e.Geometry = raw
	}
	return e
}

func polyGeom(minx, miny, size float64) map[string]any {
	return map[string]any{
		"type": "Polygon",
		"coordinates": []any{[]any{
			[]float64{minx, miny}, []float64{minx + size, miny},
			[]float64{minx + size, miny + size}, []float64{minx, miny + size},
			[]float64{minx, miny},
		}},
	}
}

// TestEditGeoJSON 增删改 + 备份内容校验。
func TestEditGeoJSON(t *testing.T) {
	ctx := context.Background()
	path := writeFixture(t, "areas.geojson", fixtureFromFeatures(t, editPair()))
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	g := NewFileGateway()
	defer g.Close()
	layer := &Layer{Name: "areas", Table: "areas", SRID: 4326, GeometryType: "POLYGON"}

	ops := []EditOp{
		editOpJSON("create", 0, polyGeom(20, 20, 5), map[string]any{"name": "丙"}),
		editOpJSON("update", 1, nil, map[string]any{"name": "甲改"}),
		editOpJSON("delete", 2, nil, nil),
	}
	res, err := g.ApplyEdits(ctx, FileDSN(path), layer, ops)
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if res.Created != 1 || res.Updated != 1 || res.Deleted != 1 {
		t.Errorf("统计=%+v", res)
	}
	if res.Total != 2 {
		t.Errorf("编辑后要素数=%d, want 2", res.Total)
	}
	if len(res.NewIDs) != 1 || res.NewIDs[0] != 3 {
		t.Errorf("新要素 ID=%v, want [3]", res.NewIDs)
	}
	if res.Backup == "" {
		t.Error("应给出备份路径")
	}

	// 备份内容必须是**编辑前**的原样
	bak, err := os.ReadFile(res.Backup)
	if err != nil {
		t.Fatalf("读备份: %v", err)
	}
	if string(bak) != string(orig) {
		t.Errorf("备份内容与编辑前不一致（备份时机不对）")
	}

	// 重新读回源文件核对
	data, _ := os.ReadFile(path)
	feats, _, err := ParseGeoJSON(data)
	if err != nil {
		t.Fatalf("编辑后的文件解析失败: %v", err)
	}
	if len(feats) != 2 {
		t.Fatalf("要素数=%d, want 2", len(feats))
	}
	var names []string
	for _, f := range feats {
		names = append(names, f.Props["name"].(string))
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "甲改") || !strings.Contains(joined, "丙") || strings.Contains(joined, "乙") {
		t.Errorf("属性=%v, want 含 甲改 与 丙、不含 乙", names)
	}
	// 新建要素的几何要落盘（坐标范围应覆盖到 20~25）
	var maxX float64
	for _, f := range feats {
		if f.Geom.Bounds().MaxX > maxX {
			maxX = f.Geom.Bounds().MaxX
		}
	}
	if maxX < 24 {
		t.Errorf("新建要素几何未写入（maxX=%.1f）", maxX)
	}

	// 第二次编辑：备份不得被覆盖
	if _, err := g.ApplyEdits(ctx, FileDSN(path), layer,
		[]EditOp{editOpJSON("update", 1, nil, map[string]any{"name": "甲改2"})}); err != nil {
		t.Fatalf("第二次编辑: %v", err)
	}
	bak2, _ := os.ReadFile(res.Backup)
	if string(bak2) != string(orig) {
		t.Error("第二次编辑覆盖了备份 —— 备份必须始终是编辑前的原始内容")
	}
}

// TestEditGeoPackageOtherTablesUntouched 编辑一个表不得损伤同库其它表。
//
// 这是最关键的一条：GeoPackage 是「一个文件多个图层」，若用"整库重写"实现
// 编辑，同库的其它要素表会被直接抹掉——数据丢失且用户很难察觉。
func TestEditGeoPackageOtherTablesUntouched(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "city.gpkg")
	sq := [][]Point{{{116.38, 39.89}, {116.40, 39.89}, {116.40, 39.91}, {116.38, 39.91}, {116.38, 39.89}}}
	buildGPKG(t, path, []gpkgTableSpec{
		{Name: "blocks", GeomCol: "geom", GeomType: "POLYGON", SRSID: 4326,
			Attrs: []string{"name"}, Geoms: [][]byte{gpkgBlob(wkbPolygonLE(sq), 4326), gpkgBlob(wkbPolygonLE(sq), 4326)},
			AttrRows: [][]any{{"甲"}, {"乙"}}},
		{Name: "poi", GeomCol: "geom", GeomType: "POINT", SRSID: 4326,
			Attrs: []string{"name"}, Geoms: [][]byte{gpkgBlob(wkbPointLE(Point{116.39, 39.90}), 4326)},
			AttrRows: [][]any{{"监测站"}}},
	}, nil)

	g := NewFileGateway()
	defer g.Close()
	layer := &Layer{Name: "blocks", Table: "blocks", SRID: 4326,
		GeometryType: "POLYGON", GeometryColumn: "geom"}

	res, err := g.ApplyEdits(ctx, FileDSN(path), layer, []EditOp{
		editOpJSON("create", 0, polyGeom(116.41, 39.89, 0.01), map[string]any{"name": "丙"}),
		editOpJSON("delete", 1, nil, nil),
	})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if res.Created != 1 || res.Deleted != 1 {
		t.Errorf("统计=%+v", res)
	}

	// blocks 表：乙 保留、甲 删除、丙 新增（自增 fid）
	feats, _, err := ReadGeoPackageTable(path, "blocks", "")
	if err != nil {
		t.Fatalf("读回 blocks: %v", err)
	}
	if len(feats) != 2 {
		t.Fatalf("blocks 要素数=%d, want 2", len(feats))
	}
	names := map[string]bool{}
	for _, f := range feats {
		if s, ok := f.Props["name"].(string); ok {
			names[s] = true
		}
	}
	if names["甲"] || !names["乙"] || !names["丙"] {
		t.Errorf("blocks 属性=%v, want 乙+丙", names)
	}

	// poi 表必须原封不动
	poi, _, err := ReadGeoPackageTable(path, "poi", "")
	if err != nil {
		t.Fatalf("poi 表被破坏了: %v", err)
	}
	if len(poi) != 1 || poi[0].Props["name"] != "监测站" {
		t.Errorf("同库其它表未保持原样: %+v", poi)
	}

	// 编辑后仍是合法 GeoPackage（规范表齐全、坐标系定义在）
	tabs, err := ListGeoPackageTables(path)
	if err != nil || len(tabs) != 2 {
		t.Errorf("编辑后 GeoPackage 结构异常: tabs=%d err=%v", len(tabs), err)
	}
}

// TestEditShapefile 重写文件族并保留坐标系与属性。
func TestEditShapefile(t *testing.T) {
	ctx := context.Background()
	sq := []Point{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}
	shp := buildSHP(shpPolygon, [][]byte{
		shpPartsContent(shpPolygon, [][]Point{sq}),
		shpPartsContent(shpPolygon, [][]Point{sq}),
	})
	dbf := buildDBF([]dbfFieldSpec{{"name", 'C', 10, 0}}, [][]string{{"甲"}, {"乙"}})
	path := writeShpSet(t, "blocks", shp, dbf, prjWGS84, "")

	g := NewFileGateway()
	defer g.Close()
	layer := &Layer{Name: "blocks", Table: "blocks", SRID: 4326, GeometryType: "POLYGON"}
	res, err := g.ApplyEdits(ctx, FileDSN(path), layer, []EditOp{
		editOpJSON("create", 0, polyGeom(20, 20, 5), map[string]any{"name": "丙"}),
		editOpJSON("delete", 2, nil, nil),
	})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if res.Created != 1 || res.Deleted != 1 || res.Total != 2 {
		t.Errorf("统计=%+v", res)
	}
	// 文件族齐全
	dir := filepath.Dir(path)
	for _, ext := range []string{".shp", ".shx", ".dbf", ".prj", ".cpg"} {
		if _, err := os.Stat(filepath.Join(dir, "blocks"+ext)); err != nil {
			t.Errorf("缺少 %s: %v", ext, err)
		}
	}
	// 内容核对（坐标系要从 .prj 读回来）
	feats, info, err := ParseShapefile(path)
	if err != nil {
		t.Fatalf("编辑后的 Shapefile 解析失败: %v", err)
	}
	if len(feats) != 2 || info.SRID != 4326 {
		t.Fatalf("要素数=%d srid=%d", len(feats), info.SRID)
	}
	var got []string
	for _, f := range feats {
		got = append(got, f.Props["name"].(string))
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "甲") || !strings.Contains(joined, "丙") || strings.Contains(joined, "乙") {
		t.Errorf("属性=%v", got)
	}
	// 备份文件族也在
	if _, err := os.Stat(path + ".orig"); err != nil {
		t.Errorf("应有 .shp 备份: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blocks.dbf.orig")); err != nil {
		t.Errorf("应有 .dbf 备份: %v", err)
	}
}

// TestEditGeometryTypeRejected 几何类型不符必须拒绝（单几何类型存储）。
func TestEditGeometryTypeRejected(t *testing.T) {
	ctx := context.Background()
	path := writeFixture(t, "poly_only.geojson", fixtureFromFeatures(t, editPair()))
	g := NewFileGateway()
	defer g.Close()
	layer := &Layer{Name: "areas", Table: "areas", SRID: 4326, GeometryType: "POLYGON"}

	point := map[string]any{"type": "Point", "coordinates": []float64{1, 1}}
	_, err := g.ApplyEdits(ctx, FileDSN(path), layer,
		[]EditOp{editOpJSON("create", 0, point, nil)})
	if err == nil {
		t.Fatal("往面图层插入点应被拒绝")
	}
	if !strings.Contains(err.Error(), "不能写入") {
		t.Errorf("错误信息应说明类型不匹配: %v", err)
	}
	// 拒绝时不得留下任何改动（含备份也不该"改坏"原文件）
	data, _ := os.ReadFile(path)
	feats, _, _ := ParseGeoJSON(data)
	if len(feats) != 2 {
		t.Errorf("被拒绝的编辑不应改动数据: 要素数=%d", len(feats))
	}
}

// TestEditValidation 参数校验。
func TestEditValidation(t *testing.T) {
	ctx := context.Background()
	path := writeFixture(t, "v.geojson", fixtureFromFeatures(t, editPair()))
	g := NewFileGateway()
	defer g.Close()
	layer := &Layer{Name: "v", Table: "v", SRID: 4326, GeometryType: "POLYGON"}
	dsn := FileDSN(path)

	cases := []struct {
		name string
		ops  []EditOp
	}{
		{"空操作", nil},
		{"未知操作", []EditOp{{Op: "explode", ID: 1}}},
		{"update 缺 id", []EditOp{{Op: "update", Properties: map[string]any{"a": 1}}}},
		{"delete 缺 id", []EditOp{{Op: "delete"}}},
		{"create 缺几何", []EditOp{{Op: "create", Properties: map[string]any{"a": 1}}}},
		{"update 无内容", []EditOp{{Op: "update", ID: 1}}},
		{"目标不存在", []EditOp{{Op: "delete", ID: 999}}},
		{"几何非法", []EditOp{{Op: "create", Geometry: json.RawMessage(`{"type":"Polygon"}`)}}},
	}
	for _, c := range cases {
		if _, err := g.ApplyEdits(ctx, dsn, layer, c.ops); err == nil {
			t.Errorf("%s: 应报错", c.name)
		}
	}
	// 非法几何不应留下备份（校验在备份之前？——实际顺序是备份在先，
	// 这里只断言数据未被改动）
	data, _ := os.ReadFile(path)
	feats, _, _ := ParseGeoJSON(data)
	if len(feats) != 2 {
		t.Errorf("校验失败的编辑不得改动数据: 要素数=%d", len(feats))
	}
}

// TestEditLayerThroughServer 走 Server 全链路（含 Meta 记录与缓存失效）。
func TestEditLayerThroughServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "areas.geojson")
	if err := os.WriteFile(src, []byte(fixtureFromFeatures(t, editPair())), 0o644); err != nil {
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
	// 编辑前先读一次，把数据集放进网关缓存——编辑后必须读到新数据
	if _, n, err := srv.Features(ctx, "areas", false, 0, 0, 0, 0, 100); err != nil || n != 2 {
		t.Fatalf("编辑前要素数=%d err=%v", n, err)
	}

	srv2, err := srv.ApplyEdits(ctx, "areas", []EditOp{
		editOpJSON("create", 0, polyGeom(30, 30, 1), map[string]any{"name": "新"}),
	})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if srv2.Total != 3 {
		t.Errorf("Total=%d, want 3", srv2.Total)
	}
	// 缓存必须已失效：立刻再读应看到 3 条
	if _, n, err := srv.Features(ctx, "areas", false, 0, 0, 0, 0, 100); err != nil || n != 3 {
		t.Errorf("编辑后要素数=%d（缓存未失效？）err=%v", n, err)
	}
	// 元数据也要跟着变
	md, err := srv.Metadata(ctx, "areas")
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if md.FeaturesEstimated != 3 {
		t.Errorf("元数据要素数=%d, want 3", md.FeaturesEstimated)
	}
}

// fixtureFromFeatures 把要素集写成 GeoJSON 字节（测试语料）。
func fixtureFromFeatures(t *testing.T, feats []FileFeature) string {
	t.Helper()
	data, _, err := marshalGeoJSONFeatures(feats)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
