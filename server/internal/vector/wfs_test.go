// wfs_test.go — WFS 2.0 简单子集（M2-F14）：Features 编排与 Capabilities
// 真实生成（mock Gateway，不依赖真实 PG）。
package vector

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

func wfsTestServer(t *testing.T) (*Server, *mockGateway) {
	t.Helper()
	gw := &mockGateway{
		pingVer:  "POSTGIS=\"3.4.3\"",
		pk:       "gid",
		fields:   []string{"name"},
		geomSRID: 4326,
		extentOK: true,
		extent:   [4]float64{116, 39, 117, 40},
	}
	srv := &Server{Meta: NewMemoryMetaStore(), Gateway: gw}
	return srv, gw
}

func wfsRegisterLayer(t *testing.T, s *Server, name, schema, table string, srid int) {
	t.Helper()
	src, err := s.RegisterSource(context.Background(), &RegSourceReq{Name: "src-" + name, DSN: "postgres://u:p@127.0.0.1:1/db"})
	if err != nil {
		t.Fatalf("register source: %v", err)
	}
	l, err := s.RegisterLayer(context.Background(), &RegLayerReq{
		Name: name, SourceID: src.ID, Schema: schema, Table: table, SRID: srid,
	})
	if err != nil {
		t.Fatalf("register layer: %v", err)
	}
	_ = l
}

func TestWFSFeaturesValidationAndCap(t *testing.T) {
	s, gw := wfsTestServer(t)
	ctx := context.Background()
	wfsRegisterLayer(t, s, "parcels", "demo", "parcels", 4326)

	// 非法 typeName
	if _, _, err := s.Features(ctx, "a;b", false, 0, 0, 0, 0, 0); err == nil {
		t.Fatal("invalid typeName should error")
	}
	// bbox 顺序错
	if _, _, err := s.Features(ctx, "parcels", true, 117, 39, 116, 40, 0); err == nil {
		t.Fatal("reversed bbox should error")
	}
	// NaN bbox
	if _, _, err := s.Features(ctx, "parcels", true, math.NaN(), 39, 117, 40, 0); err == nil {
		t.Fatal("NaN bbox should error")
	}
	// count 超上限收敛（WFSMax=7）
	s.WFSMax = 7
	var gotLimit int
	gw.featFn = func(limit int) { gotLimit = limit }
	gw.featJSON = []byte(`[{"type":"Feature"}]`)
	gw.featN = 1
	_, n, err := s.Features(ctx, "parcels", true, 116, 39, 117, 40, 999)
	if err != nil || n != 1 {
		t.Fatalf("features = %d, err %v", n, err)
	}
	if gotLimit != 7 {
		t.Fatalf("limit passed to gateway = %d, want 7 (capped)", gotLimit)
	}
	// count<=0 → 上限
	gw.featFn = func(limit int) { gotLimit = limit }
	if _, _, err := s.Features(ctx, "parcels", true, 116, 39, 117, 40, 0); err != nil || gotLimit != 7 {
		t.Fatalf("count<=0 → cap, got %d err %v", gotLimit, err)
	}
	// 未知图层 → ErrNotFound
	if _, _, err := s.Features(ctx, "nope", true, 116, 39, 117, 40, 0); err != ErrNotFound {
		t.Fatalf("unknown layer err = %v", err)
	}
}

func TestWFSFeaturesCache(t *testing.T) {
	s, gw := wfsTestServer(t)
	s.Cache = newMemCacheForTest()
	s.CacheTTL = time.Minute
	ctx := context.Background()
	wfsRegisterLayer(t, s, "parcels", "demo", "parcels", 4326)

	calls := 0
	gw.featFn = func(int) { calls++ }
	gw.featJSON = []byte(`[{"type":"Feature"}]`)
	for i := 0; i < 2; i++ {
		if _, _, err := s.Features(ctx, "parcels", true, 116, 39, 117, 40, 10); err != nil {
			t.Fatalf("features: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("gateway calls = %d, want 1 (cache hit on 2nd)", calls)
	}
}

func TestWFSCapabilitiesRealLayers(t *testing.T) {
	s, gw := wfsTestServer(t)
	ctx := context.Background()
	wfsRegisterLayer(t, s, "parcels", "demo", "parcels", 4326)
	// 3857 图层：bbox 应换算为 WGS84
	wfsRegisterLayer(t, s, "parcels_m", "demo", "parcels_m", 3857)
	// 无统计信息：不输出 WGS84BoundingBox（不造假）
	gw.extentNoStats = map[string]bool{"nostats": true}
	wfsRegisterLayer(t, s, "nostats", "demo", "nostats", 4326)

	xml, err := s.CapabilitiesXML(ctx, "http://test")
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	for _, want := range []string{
		`<ows:ServiceType>OGC WFS</ows:ServiceType>`,
		`version="2.0.0"`,
		`<ows:Identifier>parcels</ows:Identifier>`,
		`<ows:Identifier>parcels_m</ows:Identifier>`,
		`<ows:Identifier>nostats</ows:Identifier>`,
		`urn:ogc:def:crs:EPSG::4326`,
		`urn:ogc:def:crs:EPSG::3857`,
		`<OutputFormat>application/geo+json</OutputFormat>`,
		`http://test/api/v1/wfs?`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("capabilities missing %q\n%s", want, xml)
		}
	}
	// 3857 bbox 换算（116,39 → 约 12912xxx/4747xxx 米）
	if !strings.Contains(xml, "<ows:LowerCorner>116 ") {
		t.Errorf("3857 layer WGS84 lower corner missing:\n%s", xml)
	}
	// nostats 图层段内不应有 WGS84BoundingBox（从该 Identifier 截取到 FeatureType 结束）
	i := strings.Index(xml, "<ows:Identifier>nostats</ows:Identifier>")
	seg := xml[i:]
	if strings.Contains(seg[:strings.Index(seg, "</FeatureType>")], "WGS84BoundingBox") {
		t.Error("layer without stats must not fabricate a bbox")
	}
	// XML 注入转义
	wfsRegisterLayer(t, s, "x<b", "demo", "inj", 4326)
	xml, _ = s.CapabilitiesXML(ctx, "http://test")
	if strings.Contains(xml, "x<b") || !strings.Contains(xml, "x&lt;b") {
		t.Error("layer name must be XML-escaped")
	}
}

func TestWFSExceptionReport(t *testing.T) {
	status, xml := ExceptionReport(400, "InvalidParameterValue", "bbox", `bad <input> & stuff`)
	if status != 400 {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{
		`xmlns="http://www.opengis.net/ows/1.1"`,
		`exceptionCode="InvalidParameterValue"`,
		`locator="bbox"`,
		`bad &lt;input&gt; &amp; stuff`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("exception xml missing %q:\n%s", want, xml)
		}
	}
}
