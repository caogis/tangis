package vector

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* ------------------------------------------------------------------ */
/* 测试用 GeoPackage 生成器（真建一个 SQLite 库，不提交二进制语料）     */
/* ------------------------------------------------------------------ */

// wkb 写入小工具（统一小端，另有专门的大端/Z 变体用例）。
func wkbAppendU32(b []byte, v uint32) []byte {
	var t [4]byte
	binary.LittleEndian.PutUint32(t[:], v)
	return append(b, t[:]...)
}

func wkbAppendF64(b []byte, v float64) []byte {
	var t [8]byte
	binary.LittleEndian.PutUint64(t[:], math.Float64bits(v))
	return append(b, t[:]...)
}

// wkbPoint 小端点。
func wkbPointLE(p Point) []byte {
	b := []byte{1}
	b = wkbAppendU32(b, 1)
	return wkbAppendF64(wkbAppendF64(b, p.X), p.Y)
}

// wkbLineString 小端折线。
func wkbLineStringLE(pts []Point) []byte {
	b := append([]byte{1}, wkbAppendU32(nil, 2)...)
	b = wkbAppendU32(b, uint32(len(pts)))
	for _, p := range pts {
		b = wkbAppendF64(wkbAppendF64(b, p.X), p.Y)
	}
	return b
}

// wkbPolygon 小端多边形（首环外环，其余洞）。
func wkbPolygonLE(rings [][]Point) []byte {
	b := append([]byte{1}, wkbAppendU32(nil, 3)...)
	b = wkbAppendU32(b, uint32(len(rings)))
	for _, r := range rings {
		b = wkbAppendU32(b, uint32(len(r)))
		for _, p := range r {
			b = wkbAppendF64(wkbAppendF64(b, p.X), p.Y)
		}
	}
	return b
}

// gpkgBlob 组装 GeoPackage 几何 BLOB（GP 头 + WKB，无包络）。
func gpkgBlob(wkb []byte, srsID int) []byte {
	b := []byte{'G', 'P', 0, 0x01} // version 0；flags：小端、无包络、非空
	b = wkbAppendU32(b, uint32(srsID))
	return append(b, wkb...)
}

// gpkgTableSpec 测试用要素表定义。
type gpkgTableSpec struct {
	Name     string
	GeomCol  string
	GeomType string // 如 POLYGON / POINT / MULTIPOLYGON Z
	SRSID    int
	Attrs    []string // 属性列（一律 TEXT 或 INTEGER，按名称猜）
	Geoms    [][]byte // 每行的几何 BLOB
	AttrRows [][]any  // 与 Geoms 对齐的属性值
}

// buildGPKG 生成一个合法的 GeoPackage（含规范表 + 若干要素表）。
//
// srsDefs 可额外补充 gpkg_spatial_ref_sys 记录（用于测试自定义 srs_id → WKT 解析）。
func buildGPKG(t *testing.T, path string, specs []gpkgTableSpec, srsDefs map[int]string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	ddl := []string{
		`CREATE TABLE gpkg_spatial_ref_sys (
			srs_name TEXT NOT NULL, srs_id INTEGER PRIMARY KEY,
			organization TEXT NOT NULL, organization_coordsys_id INTEGER NOT NULL,
			definition TEXT NOT NULL, description TEXT)`,
		`CREATE TABLE gpkg_contents (
			table_name TEXT NOT NULL PRIMARY KEY, data_type TEXT NOT NULL,
			identifier TEXT UNIQUE, description TEXT DEFAULT '',
			last_change DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			min_x DOUBLE, min_y DOUBLE, max_x DOUBLE, max_y DOUBLE, srs_id INTEGER)`,
		`CREATE TABLE gpkg_geometry_columns (
			table_name TEXT NOT NULL, column_name TEXT NOT NULL,
			geometry_type_name TEXT NOT NULL, srs_id INTEGER NOT NULL, z TINYINT NOT NULL, m TINYINT NOT NULL,
			PRIMARY KEY (table_name, column_name))`,
	}
	for _, s := range ddl {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("ddl: %v (%s)", err, s)
		}
	}
	// 标准要求的三条基础 SRS + 用例自定义
	base := []struct {
		name string
		id   int
		org  string
		ocid int
		def  string
	}{
		{"Undefined cartesian SRS", -1, "NONE", -1, "undefined"},
		{"Undefined geographic SRS", 0, "NONE", 0, "undefined"},
		{"WGS 84 geodetic", 4326, "EPSG", 4326, prjWGS84},
	}
	for _, s := range base {
		if _, err := db.Exec(
			`INSERT INTO gpkg_spatial_ref_sys VALUES (?,?,?,?,?,NULL)`,
			s.name, s.id, s.org, s.ocid, s.def); err != nil {
			t.Fatalf("insert srs: %v", err)
		}
	}
	for id, def := range srsDefs {
		if _, err := db.Exec(
			`INSERT INTO gpkg_spatial_ref_sys VALUES (?,?,?,?,?,NULL)`,
			"custom", id, "NONE", id, def); err != nil {
			t.Fatalf("insert custom srs: %v", err)
		}
	}

	for _, s := range specs {
		cols := []string{"fid INTEGER PRIMARY KEY AUTOINCREMENT", s.GeomCol + " BLOB"}
		for _, a := range s.Attrs {
			cols = append(cols, a+" TEXT")
		}
		if _, err := db.Exec("CREATE TABLE " + quoteMust(s.Name) + " (" + strings.Join(cols, ", ") + ")"); err != nil {
			t.Fatalf("create table %s: %v", s.Name, err)
		}
		if _, err := db.Exec(
			`INSERT INTO gpkg_contents (table_name, data_type, identifier, srs_id) VALUES (?,'features',?,?)`,
			s.Name, s.Name, s.SRSID); err != nil {
			t.Fatalf("insert contents: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO gpkg_geometry_columns VALUES (?,?,?,?,0,0)`,
			s.Name, s.GeomCol, s.GeomType, s.SRSID); err != nil {
			t.Fatalf("insert geometry_columns: %v", err)
		}
		for i, g := range s.Geoms {
			names := append([]string{s.GeomCol}, s.Attrs...)
			ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
			vals := []any{g}
			if i < len(s.AttrRows) {
				vals = append(vals, s.AttrRows[i]...)
			} else {
				for range s.Attrs {
					vals = append(vals, nil)
				}
			}
			q := "INSERT INTO " + quoteMust(s.Name) + " (" + strings.Join(names, ",") + ") VALUES (" + ph + ")"
			if _, err := db.Exec(q, vals...); err != nil {
				t.Fatalf("insert row: %v", err)
			}
		}
	}
}

/* ------------------------------------------------------------------ */
/* 解析测试                                                            */
/* ------------------------------------------------------------------ */

// TestGeoPackageListTables 列出要素表（含几何列/类型/坐标系）。
func TestGeoPackageListTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.gpkg")
	sq := [][]Point{{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}}
	buildGPKG(t, path, []gpkgTableSpec{
		{Name: "roads", GeomCol: "geom", GeomType: "LINESTRING", SRSID: 4326,
			Attrs: []string{"name"}, Geoms: [][]byte{gpkgBlob(wkbLineStringLE([]Point{{0, 0}, {1, 1}}), 4326)}, AttrRows: [][]any{{"一号线"}}},
		{Name: "blocks", GeomCol: "geom", GeomType: "POLYGON", SRSID: 4326,
			Attrs: []string{"name", "area"}, Geoms: [][]byte{gpkgBlob(wkbPolygonLE(sq), 4326)}, AttrRows: [][]any{{"甲块", "100"}}},
	}, nil)

	tabs, err := ListGeoPackageTables(path)
	if err != nil {
		t.Fatalf("ListGeoPackageTables: %v", err)
	}
	if len(tabs) != 2 {
		t.Fatalf("要素表数=%d, want 2", len(tabs))
	}
	if tabs[0].Name != "blocks" || tabs[1].Name != "roads" {
		t.Errorf("表名排序=%v", []string{tabs[0].Name, tabs[1].Name})
	}
	for _, tb := range tabs {
		if tb.GeometryCol != "geom" || tb.SRID != 4326 {
			t.Errorf("表 %s: geomCol=%q srid=%d", tb.Name, tb.GeometryCol, tb.SRID)
		}
	}
	if got := normalizeGeomTypeName("MULTIPOLYGON Z"); got != "POLYGON" {
		t.Errorf("normalizeGeomTypeName(MULTIPOLYGON Z)=%q, want POLYGON", got)
	}
}

// TestGeoPackageReadPolygon 读多边形要素：几何、环角色、属性、bbox。
func TestGeoPackageReadPolygon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poly.gpkg")
	outer := []Point{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}
	hole := []Point{{2, 2}, {4, 2}, {4, 4}, {2, 4}, {2, 2}}
	buildGPKG(t, path, []gpkgTableSpec{{
		Name: "blocks", GeomCol: "geom", GeomType: "POLYGON", SRSID: 4326,
		Attrs:    []string{"name", "cnt"},
		Geoms:    [][]byte{gpkgBlob(wkbPolygonLE([][]Point{outer, hole}), 4326)},
		AttrRows: [][]any{{"带洞地块", "7"}},
	}}, nil)

	feats, info, err := ReadGeoPackageTable(path, "blocks", "")
	if err != nil {
		t.Fatalf("ReadGeoPackageTable: %v", err)
	}
	if info.Count != 1 || len(feats) != 1 {
		t.Fatalf("要素数=%d", len(feats))
	}
	if info.SRID != 4326 || info.GeomType != GeomPolygon {
		t.Errorf("srid=%d geomType=%v", info.SRID, info.GeomType)
	}
	g := feats[0].Geom
	if len(g.Lines) != 2 || !g.Exterior[0] || g.Exterior[1] {
		t.Fatalf("环角色错误: exterior=%v", g.Exterior)
	}
	if feats[0].Props["name"] != "带洞地块" {
		t.Errorf("属性=%v", feats[0].Props)
	}
	if b := info.BBox; b.MinX != 0 || b.MaxY != 10 {
		t.Errorf("bbox=%+v", b)
	}
	if len(info.Fields) != 2 {
		t.Errorf("字段=%v", info.Fields)
	}
	// fid 主键不进属性
	if _, ok := feats[0].Props["fid"]; ok {
		t.Error("fid 主键不应作为属性")
	}
}

// TestGeoPackageGeometryVariants WKB 变体：大端、Z 分量、多面、空几何。
func TestGeoPackageGeometryVariants(t *testing.T) {
	// 大端点：X=12, Y=34
	be := []byte{0} // 大端
	var b4 [4]byte
	binary.BigEndian.PutUint32(b4[:], 1)
	be = append(be, b4[:]...)
	var b8 [8]byte
	binary.BigEndian.PutUint64(b8[:], math.Float64bits(12))
	be = append(be, b8[:]...)
	binary.BigEndian.PutUint64(b8[:], math.Float64bits(34))
	be = append(be, b8[:]...)

	g, err := parseGeoPackageGeometry(gpkgBlob(be, 4326))
	if err != nil {
		t.Fatalf("大端点: %v", err)
	}
	if p := g.Lines[0][0]; p.X != 12 || p.Y != 34 {
		t.Errorf("大端点坐标=%+v, want {12 34}", p)
	}

	// PolygonZ（类型 1003）：每个坐标 3 个分量，Z 需被跳过
	pz := []byte{1}
	pz = wkbAppendU32(pz, 1003)
	pz = wkbAppendU32(pz, 1) // 1 个环
	pz = wkbAppendU32(pz, 3) // 3 个点
	for _, xy := range [][2]float64{{0, 0}, {1, 0}, {1, 1}} {
		pz = wkbAppendF64(wkbAppendF64(pz, xy[0]), xy[1])
		pz = wkbAppendF64(pz, 99) // Z 分量
	}
	gz, err := parseGeoPackageGeometry(gpkgBlob(pz, 4326))
	if err != nil {
		t.Fatalf("PolygonZ: %v", err)
	}
	if len(gz.Lines) != 1 || len(gz.Lines[0]) != 3 {
		t.Fatalf("PolygonZ 解析=%+v", gz.Lines)
	}
	if gz.Lines[0][2].Y != 1 || gz.Lines[0][2].X != 1 {
		t.Errorf("PolygonZ 第三个点=%+v（Z 分量未正确跳过）", gz.Lines[0][2])
	}

	// MultiPolygon（类型 6）：两个子多边形
	sub1 := wkbPolygonLE([][]Point{{{0, 0}, {1, 0}, {1, 1}, {0, 0}}})
	sub2 := wkbPolygonLE([][]Point{{{5, 5}, {6, 5}, {6, 6}, {5, 5}}})
	mp := append([]byte{1}, wkbAppendU32(nil, 6)...)
	mp = wkbAppendU32(mp, 2)
	mp = append(mp, sub1...)
	mp = append(mp, sub2...)
	gm, err := parseGeoPackageGeometry(gpkgBlob(mp, 4326))
	if err != nil {
		t.Fatalf("MultiPolygon: %v", err)
	}
	if len(gm.Lines) != 2 || !gm.Exterior[0] || !gm.Exterior[1] {
		t.Errorf("MultiPolygon 环=%v exterior=%v", len(gm.Lines), gm.Exterior)
	}

	// 空几何（flags bit4）：返回零几何而非报错
	empty := []byte{'G', 'P', 0, 0x11, 0, 0, 0, 0}
	ge, err := parseGeoPackageGeometry(empty)
	if err != nil {
		t.Fatalf("空几何: %v", err)
	}
	if ge.Type != GeomUnknown {
		t.Errorf("空几何应返回零几何, 得到 %+v", ge)
	}

	// 损坏数据：明确报错
	if _, err := parseGeoPackageGeometry([]byte("NOTGPXXX")); err == nil {
		t.Error("非法 GP 头应报错")
	}
	if _, err := parseGeoPackageGeometry([]byte{'G', 'P', 0, 0x01, 0, 0, 0, 0}); err == nil {
		t.Error("缺失 WKB 应报错")
	}
}

// TestGeoPackageSRSResolution srs_id 与 EPSG 码的映射（含自定义 srs_id 走 WKT 解析）。
func TestGeoPackageSRSResolution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "srs.gpkg")
	pt := gpkgBlob(wkbPointLE(Point{116.4, 39.9}), 100000)
	buildGPKG(t, path, []gpkgTableSpec{{
		Name: "poi", GeomCol: "geom", GeomType: "POINT", SRSID: 100000,
		Attrs: []string{"name"}, Geoms: [][]byte{pt}, AttrRows: [][]any{{"点"}},
	}}, map[int]string{100000: `GEOGCS["China Geodetic Coordinate System 2000",AUTHORITY["EPSG","4490"]]`})

	tabs, err := ListGeoPackageTables(path)
	if err != nil {
		t.Fatalf("ListGeoPackageTables: %v", err)
	}
	if tabs[0].SRID != 4490 {
		t.Errorf("自定义 srs_id 应经 WKT 解析为 4490, 得到 %d", tabs[0].SRID)
	}
	if _, _, err := ReadGeoPackageTable(path, "poi", ""); err != nil {
		t.Errorf("4490 应被接受: %v", err)
	}

	// 不支持的坐标系：明确报错
	path2 := filepath.Join(t.TempDir(), "bad.gpkg")
	buildGPKG(t, path2, []gpkgTableSpec{{
		Name: "gk", GeomCol: "geom", GeomType: "POINT", SRSID: 4547,
		Attrs: []string{"n"}, Geoms: [][]byte{gpkgBlob(wkbPointLE(Point{1, 2}), 4547)}, AttrRows: [][]any{{"a"}},
	}}, nil)
	if _, _, err := ReadGeoPackageTable(path2, "gk", ""); err == nil {
		t.Error("高斯克吕格应报错（不能静默当 WGS84）")
	} else if !errorIs(err, ErrInvalid) {
		t.Errorf("错误类型=%v", err)
	}
}

// TestGeoPackageErrors 非 GeoPackage / 表名歧义 / 表不存在。
func TestGeoPackageErrors(t *testing.T) {
	dir := t.TempDir()

	// 普通 SQLite（没有 gpkg 规范表）
	plain := filepath.Join(dir, "plain.gpkg")
	db, err := sql.Open("sqlite", "file:"+plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (a INTEGER)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ListGeoPackageTables(plain); err == nil {
		t.Error("缺 gpkg_contents 应报错而不是当成空库")
	}

	// 根本不是 SQLite
	notdb := filepath.Join(dir, "not.gpkg")
	if err := os.WriteFile(notdb, []byte("this is not a sqlite database at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ListGeoPackageTables(notdb); err == nil {
		t.Error("非 SQLite 文件应报错")
	}

	// 多表时未指定表名 / 指定不存在的表
	multi := filepath.Join(dir, "multi.gpkg")
	sq := [][]Point{{{0, 0}, {1, 0}, {1, 1}, {0, 0}}}
	buildGPKG(t, multi, []gpkgTableSpec{
		{Name: "a", GeomCol: "geom", GeomType: "POLYGON", SRSID: 4326, Geoms: [][]byte{gpkgBlob(wkbPolygonLE(sq), 4326)}},
		{Name: "b", GeomCol: "geom", GeomType: "POLYGON", SRSID: 4326, Geoms: [][]byte{gpkgBlob(wkbPolygonLE(sq), 4326)}},
	}, nil)
	if _, _, err := ReadGeoPackageTable(multi, "", ""); err == nil {
		t.Error("多要素表且未指定表名应报错（并列出可选表名）")
	} else if !strings.Contains(err.Error(), "多个要素表") {
		t.Errorf("错误信息应说明是表名歧义: %v", err)
	}
	if _, _, err := ReadGeoPackageTable(multi, "nope", ""); err == nil {
		t.Error("表不存在应报错")
	}

	// 单表时允许省略表名
	single := filepath.Join(dir, "single.gpkg")
	buildGPKG(t, single, []gpkgTableSpec{
		{Name: "only", GeomCol: "geom", GeomType: "POLYGON", SRSID: 4326, Geoms: [][]byte{gpkgBlob(wkbPolygonLE(sq), 4326)}},
	}, nil)
	feats, _, err := ReadGeoPackageTable(single, "", "")
	if err != nil || len(feats) != 1 {
		t.Errorf("单表应可省略表名: feats=%d err=%v", len(feats), err)
	}
}

// TestGeoPackageRegisterMultipleLayers 一步注册把每个要素表建成一个图层。
func TestGeoPackageRegisterMultipleLayers(t *testing.T) {
	ctx := context.Background()
	sq := [][]Point{{{116.38, 39.89}, {116.40, 39.89}, {116.40, 39.91}, {116.38, 39.91}, {116.38, 39.89}}}
	path := filepath.Join(t.TempDir(), "city.gpkg")
	buildGPKG(t, path, []gpkgTableSpec{
		{Name: "blocks", GeomCol: "geom", GeomType: "POLYGON", SRSID: 4326,
			Attrs: []string{"name"}, Geoms: [][]byte{gpkgBlob(wkbPolygonLE(sq), 4326)}, AttrRows: [][]any{{"甲"}}},
		{Name: "poi", GeomCol: "geom", GeomType: "POINT", SRSID: 4326,
			Attrs: []string{"name"}, Geoms: [][]byte{gpkgBlob(wkbPointLE(Point{116.39, 39.90}), 4326)}, AttrRows: [][]any{{"点甲"}}},
	}, nil)

	meta, err := NewJSONMetaStore(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Meta: meta, Gateway: NewRouterGateway(NewFileGateway(), nil)}

	src, layers, err := srv.RegisterFileSource(ctx, RegFileReq{Path: path, Name: "city"})
	if err != nil {
		t.Fatalf("RegisterFileSource: %v", err)
	}
	if len(layers) != 2 {
		t.Fatalf("图层数=%d, want 2", len(layers))
	}
	if !strings.Contains(src.PostGISVersion, "2 个要素表") {
		t.Errorf("数据源摘要=%q, 应说明要素表数", src.PostGISVersion)
	}
	byName := map[string]*Layer{}
	for _, l := range layers {
		byName[l.Name] = l
	}
	for _, want := range []string{"blocks", "poi"} {
		l, ok := byName[want]
		if !ok {
			t.Fatalf("缺少图层 %s（实际 %v）", want, namesOf(layers))
		}
		if l.Table != want || l.SourceID != src.ID || l.SRID != 4326 {
			t.Errorf("图层 %s: %+v", want, l)
		}
	}
	if byName["poi"].GeometryType != "POINT" || byName["blocks"].GeometryType != "POLYGON" {
		t.Errorf("几何类型: poi=%s blocks=%s", byName["poi"].GeometryType, byName["blocks"].GeometryType)
	}

	// 两个图层各自出瓦片，且要素不串（图层隔离靠 table）
	tx, ty := tileXYAt(116.39, 39.90, 12)
	for _, name := range []string{"blocks", "poi"} {
		mvt, count, err := srv.Tile(ctx, name, 12, tx, ty)
		if err != nil {
			t.Fatalf("Tile(%s): %v", name, err)
		}
		if count == 0 || len(mvt) == 0 {
			t.Errorf("图层 %s 的瓦片应为空: count=%d", name, count)
			continue
		}
		dl := decodeMVT(t, mvt)
		if dl.name != name {
			t.Errorf("瓦片图层名=%q, want %q", dl.name, name)
		}
	}

	// WFS 也要按图层隔离：只应返回 poi 表的要素（属性值精确比对，不用子串）
	fc, n, err := srv.Features(ctx, "poi", false, 0, 0, 0, 0, 100)
	if err != nil || n != 1 {
		t.Fatalf("Features(poi) n=%d err=%v", n, err)
	}
	var got struct {
		Features []struct {
			Props map[string]any `json:"properties"`
		} `json:"features"`
	}
	if err := json.Unmarshal(fc, &got); err != nil {
		t.Fatalf("WFS 输出不是合法 FeatureCollection: %v (%s)", err, truncate(string(fc), 160))
	}
	if len(got.Features) != 1 || got.Features[0].Props["name"] != "点甲" {
		t.Errorf("WFS 应只返回 poi 表的要素, 得到 %+v", got.Features)
	}
}

// TestGeoPackageRegisterRollback 注册中途失败应回滚，不留半注册状态。
func TestGeoPackageRegisterRollback(t *testing.T) {
	ctx := context.Background()
	sq := [][]Point{{{0, 0}, {1, 0}, {1, 1}, {0, 0}}}
	path := filepath.Join(t.TempDir(), "mixed.gpkg")
	buildGPKG(t, path, []gpkgTableSpec{
		{Name: "good", GeomCol: "geom", GeomType: "POLYGON", SRSID: 4326, Geoms: [][]byte{gpkgBlob(wkbPolygonLE(sq), 4326)}},
		{Name: "bad", GeomCol: "geom", GeomType: "POINT", SRSID: 4547, Geoms: [][]byte{gpkgBlob(wkbPointLE(Point{1, 2}), 4547)}},
	}, nil)

	meta, err := NewJSONMetaStore(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Meta: meta, Gateway: NewRouterGateway(NewFileGateway(), nil)}
	if _, _, err := srv.RegisterFileSource(ctx, RegFileReq{Path: path, Name: "mixed"}); err == nil {
		t.Fatal("含不支持坐标系表时应失败")
	}
	srcs, _ := meta.ListSources(ctx)
	ls, _ := meta.ListLayers(ctx)
	if len(srcs) != 0 || len(ls) != 0 {
		t.Errorf("失败后应回滚干净: sources=%d layers=%d", len(srcs), len(ls))
	}
}

// TestGeoPackageProjectedTable 投影坐标的 GeoPackage 表应被换算成 WGS84。
func TestGeoPackageProjectedTable(t *testing.T) {
	tm := tmParams{Ell: ellipsoidCGCS2000, Lon0: 114, Lat0: 0, K0: 1, FE: 500000, FN: 0}
	const lon, lat = 116.397, 39.908
	x, y := tm.forward(lon, lat)
	ring := [][]Point{{{x, y}, {x + 1000, y}, {x + 1000, y + 1000}, {x, y + 1000}, {x, y}}}

	path := filepath.Join(t.TempDir(), "proj.gpkg")
	buildGPKG(t, path, []gpkgTableSpec{{
		Name: "blocks", GeomCol: "geom", GeomType: "POLYGON", SRSID: 4547,
		Attrs:    []string{"name"},
		Geoms:    [][]byte{gpkgBlob(wkbPolygonLE(ring), 4547)},
		AttrRows: [][]any{{"投影地块"}},
	}}, map[int]string{4547: prjCGCS2000GK})

	feats, info, err := ReadGeoPackageTable(path, "blocks", "")
	if err != nil {
		t.Fatalf("ReadGeoPackageTable: %v", err)
	}
	if info.SRID != 4326 {
		t.Errorf("换算后 SRID=%d, want 4326", info.SRID)
	}
	p := feats[0].Geom.Lines[0][0]
	if math.Abs(p.X-lon) > 1e-8 || math.Abs(p.Y-lat) > 1e-8 {
		t.Errorf("换算后坐标=(%.9f, %.9f), want (%.9f, %.9f)", p.X, p.Y, lon, lat)
	}
	note := strings.Join(info.Notes, "；")
	if !strings.Contains(note, "源坐标系") || !strings.Contains(note, "4547") {
		t.Errorf("应提示源坐标系: %q", note)
	}

	// 网关侧探测的 SRID 也必须与读取结果一致（否则图层元数据与实际坐标错位）
	g := NewFileGateway()
	defer g.Close()
	_, srid, gtype, err := g.DetectGeometry(context.Background(), FileDSN(path), "", "blocks", "")
	if err != nil || srid != 4326 || gtype != "POLYGON" {
		t.Errorf("DetectGeometry=(%d,%q,%v), want 4326/POLYGON", srid, gtype, err)
	}
}

func namesOf(ls []*Layer) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}
