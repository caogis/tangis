// vector 包单测：标识符白名单、TileBBox、注册流程（mock Gateway）、
// Tile 编排（缓存/超限/空 tile）、元数据聚合。不依赖真实 PG。
package vector

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// mockGateway 可编程 Gateway 假实现。
type mockGateway struct {
	pingVer  string
	pingErr  error
	geomCol  string
	geomSRID int
	geomType string
	geomErr  error
	pk       string
	fields   []string
	tileFn   func(dsn string, l *Layer, minx, miny, maxx, maxy float64, maxFeatures int) ([]byte, int, error)
	extentOK bool
	extent   [4]float64
	rows     int64
	// featFn WFS GetFeature（M2-F14）：记录 limit 供断言
	featFn   func(limit int)
	featJSON []byte
	featN    int
	featE    error
	// extentNoStats 按图层名模拟统计信息缺失（Capabilities 不输出 bbox）
	extentNoStats map[string]bool
}

func (m *mockGateway) Ping(context.Context, string) (string, error) {
	return m.pingVer, m.pingErr
}

func (m *mockGateway) DetectGeometry(_ context.Context, _, _, _, _ string) (string, int, string, error) {
	return m.geomCol, m.geomSRID, m.geomType, m.geomErr
}

func (m *mockGateway) DetectFields(context.Context, string, string, string, ...string) ([]string, error) {
	return m.fields, nil
}

func (m *mockGateway) DetectPrimaryKey(context.Context, string, string, string) (string, error) {
	return m.pk, nil
}

func (m *mockGateway) TileMVT(_ context.Context, dsn string, l *Layer, minx, miny, maxx, maxy float64, maxFeatures int) ([]byte, int, error) {
	return m.tileFn(dsn, l, minx, miny, maxx, maxy, maxFeatures)
}

func (m *mockGateway) EstimatedExtent(_ context.Context, _ string, l *Layer) (float64, float64, float64, float64, bool, error) {
	if m.extentNoStats != nil && m.extentNoStats[l.Name] {
		return 0, 0, 0, 0, false, nil
	}
	return m.extent[0], m.extent[1], m.extent[2], m.extent[3], m.extentOK, nil
}

func (m *mockGateway) EstimatedRows(context.Context, string, *Layer) (int64, error) {
	return m.rows, nil
}

func (m *mockGateway) FeaturesGeoJSON(_ context.Context, _ string, _ *Layer, _ bool, _, _, _, _ float64, limit int) ([]byte, int, error) {
	if m.featFn != nil {
		m.featFn(limit)
	}
	return m.featJSON, m.featN, m.featE
}

func TestValidIdentifier(t *testing.T) {
	good := []string{"a", "_x", "geom", "T1", "field_2", "z9_"}
	bad := []string{"", "9x", "a-b", `a"b`, "a;b", "drop table", "a.b", "SELECT*", "x y", `a'b`}
	for _, s := range good {
		if !ValidIdentifier(s) {
			t.Errorf("ValidIdentifier(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidIdentifier(s) {
			t.Errorf("ValidIdentifier(%q) = true, want false", s)
		}
	}
}

func TestQuoteIdentPanicsOnInvalid(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("QuoteIdent should panic on invalid identifier")
		}
	}()
	QuoteIdent(`a"b`)
}

func TestTileBBox(t *testing.T) {
	// z0 整幅世界
	minx, miny, maxx, maxy := TileBBox(0, 0, 0)
	const half = 20037508.342789244
	if math.Abs(minx+half) > 1e-6 || math.Abs(maxx-half) > 1e-6 ||
		math.Abs(miny+half) > 1e-6 || math.Abs(maxy-half) > 1e-6 {
		t.Fatalf("z0 bbox = %v", []float64{minx, miny, maxx, maxy})
	}
	// z1/x1/y0：右上象限（x 正、y 正纬度 → mercator 正）
	minx, miny, maxx, maxy = TileBBox(1, 1, 0)
	if math.Abs(minx) > 1e-6 || math.Abs(maxx-half) > 1e-6 ||
		math.Abs(maxy-half) > 1e-6 || math.Abs(miny) > 1e-6 {
		t.Fatalf("z1 bbox = %v", []float64{minx, miny, maxx, maxy})
	}
}

func TestValidTile(t *testing.T) {
	if !ValidTile(0, 0, 0) || !ValidTile(10, 1023, 511) {
		t.Fatal("valid tiles rejected")
	}
	for _, tt := range [][3]int{{-1, 0, 0}, {23, 0, 0}, {1, 2, 0}, {1, 0, 2}, {2, -1, 0}} {
		if ValidTile(tt[0], tt[1], tt[2]) {
			t.Errorf("ValidTile(%v) should be false", tt)
		}
	}
}

func newTestServer(gw Gateway) *Server {
	return &Server{Meta: NewMemoryMetaStore(), Gateway: gw, MaxFeatures: 10}
}

func registerFixture(t *testing.T, s *Server) *Layer {
	t.Helper()
	src, err := s.RegisterSource(context.Background(), &RegSourceReq{
		Name: "demo", Host: "127.0.0.1", Port: 15432, User: "tangis", Password: "p", DBName: "tangis",
	})
	if err != nil {
		t.Fatalf("RegisterSource: %v", err)
	}
	l, err := s.RegisterLayer(context.Background(), &RegLayerReq{
		Name: "parcels", SourceID: src.ID, Schema: "demo", Table: "parcels",
	})
	if err != nil {
		t.Fatalf("RegisterLayer: %v", err)
	}
	return l
}

func TestRegisterSourceAutoDSN(t *testing.T) {
	gw := &mockGateway{pingVer: `POSTGIS="3.4.3"`}
	s := newTestServer(gw)
	src, err := s.RegisterSource(context.Background(), &RegSourceReq{
		Name: "demo", Host: "h", Port: 0, User: "u", Password: "p", DBName: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://u:p@h:5432/d?sslmode=disable"
	if src.DSN != want {
		t.Fatalf("DSN = %q, want %q", src.DSN, want)
	}
	if src.MaskedDSN() != "postgres://u:****@h:5432/d?sslmode=disable" {
		t.Fatalf("MaskedDSN = %q", src.MaskedDSN())
	}
}

func TestRegisterSourceUnreachable(t *testing.T) {
	gw := &mockGateway{pingErr: errors.New("connection refused")}
	s := newTestServer(gw)
	_, err := s.RegisterSource(context.Background(), &RegSourceReq{Name: "x", DSN: "postgres://u:p@h/d"})
	if err == nil || !errors.Is(err, gw.pingErr) {
		t.Fatalf("want unreachable error, got %v", err)
	}
	// 不静默注册死数据源
	srcs, _ := s.Meta.ListSources(context.Background())
	if len(srcs) != 0 {
		t.Fatalf("dead source registered: %d", len(srcs))
	}
}

func TestRegisterSourceNameConflict(t *testing.T) {
	s := newTestServer(&mockGateway{pingVer: "v"})
	req := &RegSourceReq{Name: "dup", DSN: "postgres://u:p@h/d"}
	if _, err := s.RegisterSource(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_, err := s.RegisterSource(context.Background(), req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestRegisterLayerAutoDetect(t *testing.T) {
	gw := &mockGateway{
		pingVer: "v", geomCol: "geom", geomSRID: 4326, geomType: "POLYGON",
		pk: "gid", fields: []string{"name", "kind", "extra"},
	}
	s := newTestServer(gw)
	src, _ := s.RegisterSource(context.Background(), &RegSourceReq{Name: "s", DSN: "postgres://u:p@h/d"})
	l, err := s.RegisterLayer(context.Background(), &RegLayerReq{
		Name: "parcels", SourceID: src.ID, Schema: "demo", Table: "parcels",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.GeometryColumn != "geom" || l.SRID != 4326 || l.IDColumn != "gid" {
		t.Fatalf("layer detect wrong: %+v", l)
	}
	if len(l.Fields) != 3 || l.Fields[0] != "name" || l.Fields[1] != "kind" {
		t.Fatalf("fields = %v", l.Fields)
	}
}

func TestRegisterLayerBadIdentifier(t *testing.T) {
	s := newTestServer(&mockGateway{pingVer: "v"})
	src, _ := s.RegisterSource(context.Background(), &RegSourceReq{Name: "s", DSN: "postgres://u:p@h/d"})
	_, err := s.RegisterLayer(context.Background(), &RegLayerReq{
		Name: "evil", SourceID: src.ID, Schema: `demo"; DROP TABLE x;--`, Table: "t",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid for injection schema, got %v", err)
	}
	_, err = s.RegisterLayer(context.Background(), &RegLayerReq{
		Name: "evil2", SourceID: src.ID, Schema: "demo", Table: "t",
		GeometryColumn: "geom", Fields: []string{"a-b"},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid for injection field, got %v", err)
	}
}

func TestRegisterLayerSourceNotFound(t *testing.T) {
	s := newTestServer(&mockGateway{pingVer: "v"})
	_, err := s.RegisterLayer(context.Background(), &RegLayerReq{
		Name: "l", SourceID: "nope", Schema: "s", Table: "t",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestTileOKAndCache(t *testing.T) {
	var calls int
	gw := &mockGateway{pingVer: "v", geomCol: "geom", geomSRID: 4326, pk: "id"}
	gw.tileFn = func(dsn string, l *Layer, minx, miny, maxx, maxy float64, maxFeatures int) ([]byte, int, error) {
		calls++
		if maxFeatures != 10 {
			t.Errorf("maxFeatures = %d, want 10", maxFeatures)
		}
		wminx, wminy, wmaxx, wmaxy := TileBBox(4, 8, 3)
		if minx != wminx || miny != wminy || maxx != wmaxx || maxy != wmaxy {
			t.Errorf("bbox mismatch: got (%v,%v,%v,%v)", minx, miny, maxx, maxy)
		}
		return []byte("MVTBYTES"), 2, nil
	}
	s := newTestServer(gw)
	s.Cache = newMemCacheForTest()
	s.CacheTTL = time.Minute
	registerFixture(t, s)
	mvt, n, err := s.Tile(context.Background(), "parcels", 4, 8, 3)
	if err != nil || n != 2 || string(mvt) != "MVTBYTES" {
		t.Fatalf("tile = %q, %d, %v", mvt, n, err)
	}
	// 二次请求走缓存（缓存命中要素数未知返回 -1）
	mvt2, n2, err := s.Tile(context.Background(), "parcels", 4, 8, 3)
	if err != nil || n2 != -1 || string(mvt2) != "MVTBYTES" {
		t.Fatalf("cached tile = %q, %d, %v", mvt2, n2, err)
	}
	if calls != 1 {
		t.Fatalf("gateway called %d times, want 1 (cache hit expected)", calls)
	}
}

func TestTileEmpty(t *testing.T) {
	gw := &mockGateway{pingVer: "v", geomCol: "geom", geomSRID: 4326, pk: "id"}
	gw.tileFn = func(string, *Layer, float64, float64, float64, float64, int) ([]byte, int, error) {
		return nil, 0, nil
	}
	s := newTestServer(gw)
	registerFixture(t, s)
	mvt, n, err := s.Tile(context.Background(), "parcels", 0, 0, 0)
	if err != nil || mvt != nil || n != 0 {
		t.Fatalf("empty tile = %q, %d, %v", mvt, n, err)
	}
}

func TestTileTooManyFeatures(t *testing.T) {
	gw := &mockGateway{pingVer: "v", geomCol: "geom", geomSRID: 4326, pk: "id"}
	gw.tileFn = func(string, *Layer, float64, float64, float64, float64, int) ([]byte, int, error) {
		return []byte("big"), 11, ErrTooManyFeatures
	}
	s := newTestServer(gw)
	registerFixture(t, s)
	_, _, err := s.Tile(context.Background(), "parcels", 2, 1, 1)
	if !errors.Is(err, ErrTooManyFeatures) {
		t.Fatalf("want ErrTooManyFeatures, got %v", err)
	}
}

func TestTileInvalidAndNotFound(t *testing.T) {
	s := newTestServer(&mockGateway{pingVer: "v", geomCol: "geom", geomSRID: 4326, pk: "id"})
	registerFixture(t, s)
	if _, _, err := s.Tile(context.Background(), "parcels", 30, 0, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid z, got %v", err)
	}
	if _, _, err := s.Tile(context.Background(), "parcels", 3, 9, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid x, got %v", err)
	}
	if _, _, err := s.Tile(context.Background(), "nope", 1, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMetadata(t *testing.T) {
	gw := &mockGateway{
		pingVer: "v", geomCol: "geom", geomSRID: 4326, pk: "id",
		extentOK: true, extent: [4]float64{100, 20, 104, 24}, rows: 120,
	}
	s := newTestServer(gw)
	registerFixture(t, s)
	md, err := s.Metadata(context.Background(), "parcels")
	if err != nil {
		t.Fatal(err)
	}
	if md.BboxWGS84 == nil || len(md.BboxWGS84) != 4 {
		t.Fatalf("bbox = %v", md.BboxWGS84)
	}
	if md.FeaturesEstimated != 120 || md.MinZoom < 0 {
		t.Fatalf("metadata = %+v", md)
	}
	// 3857 bbox → WGS84 换算正确性
	bbox := TransformBBoxToWGS84(0, 0, 20037508.342789244/2, 20037508.342789244/2, 3857)
	if math.Abs(bbox[0]-0) > 1e-9 || math.Abs(bbox[2]-90) > 1e-6 {
		t.Fatalf("mercator->wgs84 = %v", bbox)
	}
}

func TestSuggestZooms(t *testing.T) {
	// 全球跨度 → minzoom 0
	mz, mx := SuggestZooms([]float64{-180, -80, 180, 80})
	if mz != 0 {
		t.Fatalf("global minzoom = %d", mz)
	}
	if mx <= mz || mx > 16 {
		t.Fatalf("maxzoom = %d", mx)
	}
	// 小区域 → minzoom 更高
	mz2, _ := SuggestZooms([]float64{116.3, 39.9, 116.4, 40.0})
	if mz2 <= mz {
		t.Fatalf("small area minzoom = %d, want > %d", mz2, mz)
	}
	// 异常输入兜底
	mz3, mx3 := SuggestZooms(nil)
	if mz3 != 0 || mx3 != 14 {
		t.Fatalf("nil bbox = %d/%d", mz3, mx3)
	}
}

func TestDeleteSourceWithLayerRef(t *testing.T) {
	s := newTestServer(&mockGateway{pingVer: "v", geomCol: "geom", geomSRID: 4326, pk: "id"})
	l := registerFixture(t, s)
	n, err := s.Meta.CountLayersBySource(context.Background(), l.SourceID)
	if err != nil || n != 1 {
		t.Fatalf("layers by source = %d, %v", n, err)
	}
	if err := s.Meta.DeleteSource(context.Background(), l.SourceID); err != nil {
		t.Fatal(err) // 存储层允许删，引用检查在 API 层
	}
}

// newMemCacheForTest 简易内存缓存（Server.Cache 联调用）。
type memCacheForTest struct {
	m map[string][]byte
}

func newMemCacheForTest() *memCacheForTest { return &memCacheForTest{m: map[string][]byte{}} }

func (c *memCacheForTest) Get(_ context.Context, k string) ([]byte, bool) {
	v, ok := c.m[k]
	return v, ok
}

func (c *memCacheForTest) Set(_ context.Context, k string, v []byte, _ time.Duration) {
	c.m[k] = v
}

func (c *memCacheForTest) Delete(_ context.Context, k string) { delete(c.m, k) }
