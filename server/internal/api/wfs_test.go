// wfs_test.go — WFS API 全链路测试（M2-F14）：router + mock Gateway，
// 覆盖 GetCapabilities 真实生成、GetFeature GeoJSON、OGC exception、鉴权关闭模式。
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/vector"
)

// wfsGet 便捷请求。
func wfsGet(t *testing.T, r *gin.Engine, url string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w
}

// TestWFSCapabilitiesFromRealLayers Capabilities 必须来自已注册图层。
func TestWFSCapabilitiesFromRealLayers(t *testing.T) {
	r, _ := newVectorRouter(t)
	vectorSetupSourceAndLayer(t, r)

	w := wfsGet(t, r, "/api/v1/wfs?service=WFS&version=2.0.0&request=GetCapabilities")
	if w.Code != http.StatusOK {
		t.Fatalf("capabilities: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/xml") {
		t.Fatalf("content-type = %q", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		`<ows:ServiceType>OGC WFS</ows:ServiceType>`,
		`<ows:Identifier>parcels</ows:Identifier>`, // 真实注册的图层
		`urn:ogc:def:crs:EPSG::4326`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("capabilities missing %q", want)
		}
	}
	// 未注册图层不得出现（不硬编码假 XML）
	if strings.Contains(body, "<ows:Identifier>fake_layer</ows:Identifier>") {
		t.Error("capabilities must not contain fabricated layers")
	}
}

// TestWFSGetFeature GeoJSON 输出与 count 上限收敛。
func TestWFSGetFeature(t *testing.T) {
	r, srv := newVectorRouter(t)
	vectorSetupSourceAndLayer(t, r)
	srv.WFSMax = 2

	// mock gateway 输出两个要素
	srv.Gateway = &featStubGateway{json: []byte(`[{"type":"Feature","id":"1"},{"type":"Feature","id":"2"}]`), n: 2}

	w := wfsGet(t, r, "/api/v1/wfs?service=WFS&version=2.0.0&request=GetFeature&typename=parcels&bbox=116,39,117,40&count=999")
	if w.Code != http.StatusOK {
		t.Fatalf("getfeature: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/geo+json") {
		t.Fatalf("content-type = %q", ct)
	}
	if w.Header().Get("X-Tangis-Feature-Count") != "2" {
		t.Fatalf("feature count header = %q", w.Header().Get("X-Tangis-Feature-Count"))
	}
	if !strings.Contains(w.Body.String(), `"type":"FeatureCollection"`) {
		t.Fatalf("body = %q", w.Body.String())
	}

	// 空 bbox 参数省略也允许（全图层，仍受 count 收敛）
	w = wfsGet(t, r, "/api/v1/wfs?request=GetFeature&typename=parcels")
	if w.Code != http.StatusOK {
		t.Fatalf("getfeature no bbox: %d %s", w.Code, w.Body.String())
	}
}

// TestWFSKvpParameterCaseInsensitive OGC 的 KVP 参数名不区分大小写（WFS 2.0 §7.5）。
// 客户端常见写法 typeName / TypeName / TYPENAME 与全大写 REQUEST 都必须能工作——
// gin 的 c.Query 是精确匹配，早年这里只认小写 typename，实测会让 QGIS 接入失败。
func TestWFSKvpParameterCaseInsensitive(t *testing.T) {
	r, srv := newVectorRouter(t)
	vectorSetupSourceAndLayer(t, r)
	srv.Gateway = &featStubGateway{json: []byte(`[{"type":"Feature","id":"1"}]`), n: 1}

	for _, url := range []string{
		"/api/v1/wfs?SERVICE=WFS&VERSION=2.0.0&REQUEST=GetCapabilities",
		"/api/v1/wfs?Service=Wfs&Version=2.0.0&Request=GetCapabilities",
		"/api/v1/wfs?SERVICE=WFS&REQUEST=GetFeature&TYPENAME=parcels&COUNT=1",
		"/api/v1/wfs?service=WFS&request=GetFeature&typeName=parcels&count=1",
		"/api/v1/wfs?Service=WFS&Request=GetFeature&TypeName=parcels",
	} {
		if w := wfsGet(t, r, url); w.Code != http.StatusOK {
			t.Errorf("%s → %d, want 200: %s", url, w.Code, firstLine(w.Body.String()))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// featStubGateway 只覆盖 WFS 数据面的假 Gateway。
type featStubGateway struct {
	vecMockGateway
	json []byte
	n    int
}

func (g *featStubGateway) FeaturesGeoJSON(context.Context, string, *vector.Layer, bool, float64, float64, float64, float64, int) ([]byte, int, error) {
	return g.json, g.n, nil
}

// TestWFSExceptions 异常路径输出 OGC exception XML。
func TestWFSExceptions(t *testing.T) {
	r, _ := newVectorRouter(t)
	vectorSetupSourceAndLayer(t, r)

	cases := []struct {
		url, codeContains string
		status            int
	}{
		{"/api/v1/wfs", "MissingParameterValue", 400},                                                // 缺 request
		{"/api/v1/wfs?request=Bogus", "InvalidParameterValue", 400},                                  // 未知 request
		{"/api/v1/wfs?request=GetFeature", "MissingParameterValue", 400},                             // 缺 typename
		{"/api/v1/wfs?request=GetFeature&typename=parcels&bbox=1,2,3", "InvalidParameterValue", 400}, // bbox 格式
		{"/api/v1/wfs?request=GetFeature&typename=parcels&bbox=a,b,c,d", "InvalidParameterValue", 400},
		{"/api/v1/wfs?request=GetFeature&typename=nope&bbox=116,39,117,40", "NotFound", 404}, // 未知图层
	}
	for _, tc := range cases {
		w := wfsGet(t, r, tc.url)
		if w.Code != tc.status {
			t.Errorf("GET %s = %d, want %d (%s)", tc.url, w.Code, tc.status, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "ExceptionReport") || !strings.Contains(w.Body.String(), tc.codeContains) {
			t.Errorf("GET %s body missing exception %q: %s", tc.url, tc.codeContains, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/xml") {
			t.Errorf("GET %s content-type = %q, want xml", tc.url, ct)
		}
	}
}

// TestWFS503WhenNotConfigured vector 未装配时 WFS 走 503。
func TestWFS503WhenNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewRouter(nil, nil, nil, AuthOptions{Enabled: false}, nil)
	for _, url := range []string{
		"/api/v1/wfs?request=GetCapabilities",
		"/api/v1/wfs?request=GetFeature&typeName=x",
	} {
		w := wfsGet(t, r, url)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503", url, w.Code)
		}
	}
}
