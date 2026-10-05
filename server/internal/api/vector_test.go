// 矢量发布 API 测试（M2-F10a）：router + mock Gateway 走 HTTP 全链路，
// 覆盖鉴权关闭模式下的 CRUD、MVT 分发状态码与响应头。不依赖真实 PG。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/cache"
	"tangis/server/internal/vector"
)

// vecMockGateway vector.Gateway 假实现（可编程）。
type vecMockGateway struct {
	ver   string
	pk    string
	tileM []byte
	tileN int
	tileE error
	// WFS GetFeature（M2-F14）
	featJSON []byte
	featN    int
	featE    error
}

func (m *vecMockGateway) Ping(context.Context, string) (string, error) { return m.ver, nil }

func (m *vecMockGateway) DetectGeometry(context.Context, string, string, string, string) (string, int, string, error) {
	return "geom", 4326, "POLYGON", nil
}

func (m *vecMockGateway) DetectFields(context.Context, string, string, string, ...string) ([]string, error) {
	return []string{"name", "kind"}, nil
}

func (m *vecMockGateway) DetectPrimaryKey(context.Context, string, string, string) (string, error) {
	return m.pk, nil
}

func (m *vecMockGateway) TileMVT(context.Context, string, *vector.Layer, float64, float64, float64, float64, int) ([]byte, int, error) {
	return m.tileM, m.tileN, m.tileE
}

func (m *vecMockGateway) EstimatedExtent(context.Context, string, *vector.Layer) (float64, float64, float64, float64, bool, error) {
	return 116, 39, 117, 40, true, nil
}

func (m *vecMockGateway) EstimatedRows(context.Context, string, *vector.Layer) (int64, error) {
	return 42, nil
}

func (m *vecMockGateway) FeaturesGeoJSON(context.Context, string, *vector.Layer, bool, float64, float64, float64, float64, int) ([]byte, int, error) {
	return m.featJSON, m.featN, m.featE
}

// newVectorRouter 组装 TANGIS_AUTH=off 风格的测试路由（vector 已装配）。
func newVectorRouter(t *testing.T) (*gin.Engine, *vector.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	srv := &vector.Server{
		Meta:        vector.NewMemoryMetaStore(),
		Gateway:     &vecMockGateway{ver: `POSTGIS="3.4.3"`, pk: "gid", tileM: []byte("FAKEMVT"), tileN: 3},
		Cache:       cache.NewMemoryCache(),
		CacheTTL:    time.Minute,
		MaxFeatures: 100,
	}
	r := NewRouter(nil, nil, nil, AuthOptions{Enabled: false, Vector: srv}, nil)
	return r, srv
}

func vectorSetupSourceAndLayer(t *testing.T, r *gin.Engine) {
	t.Helper()
	body := `{"name":"demo","host":"127.0.0.1","port":15432,"user":"tangis","password":"p","dbname":"tangis"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vector/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create source: %d %s", w.Code, w.Body.String())
	}

	body = `{"name":"parcels","source_id":"` + vectorSourceID(t, r) + `","schema":"demo","table":"parcels"}`
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/vector/layers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create layer: %d %s", w.Code, w.Body.String())
	}
}

func vectorSourceID(t *testing.T, r *gin.Engine) string {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/sources", nil))
	var resp struct {
		Sources []struct {
			ID string `json:"id"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Sources) != 1 {
		t.Fatalf("list sources: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tangis_dev") || strings.Contains(w.Body.String(), ":p@") {
		t.Fatalf("dsn not masked: %s", w.Body.String())
	}
	return resp.Sources[0].ID
}

func TestVectorSourceLifecycle(t *testing.T) {
	r, _ := newVectorRouter(t)
	vectorSetupSourceAndLayer(t, r)

	// 删除被引用的数据源 → 409
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/vector/sources/"+vectorSourceID(t, r), nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete referenced source: %d %s", w.Code, w.Body.String())
	}
	// 先删图层再删源 → 204
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/vector/layers/parcels", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete layer: %d", w.Code)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/vector/sources/"+vectorSourceID(t, r), nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete source: %d", w.Code)
	}
}

func TestVectorLayerValidation(t *testing.T) {
	r, _ := newVectorRouter(t)
	vectorSetupSourceAndLayer(t, r)

	cases := []struct {
		body string
		code int
	}{
		{`{"name":"evil","source_id":"x","schema":"a;b","table":"t"}`, http.StatusBadRequest},
		{`{"name":"evil2","source_id":"nope","schema":"a","table":"t"}`, http.StatusNotFound},
		{`{"name":"parcels","source_id":"` + vectorSourceID(t, r) + `","schema":"demo","table":"other"}`, http.StatusConflict}, // 名称重复
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/vector/layers", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Errorf("layer %s → %d, want %d (%s)", tc.body, w.Code, tc.code, w.Body.String())
		}
	}
}

func TestVectorTileEndpoint(t *testing.T) {
	r, _ := newVectorRouter(t)
	vectorSetupSourceAndLayer(t, r)

	// 正常瓦片
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/parcels/10/843/388.pbf", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("tile: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/vnd.mapbox-vector-tile" {
		t.Fatalf("content-type = %q", ct)
	}
	if w.Header().Get("X-Tangis-Feature-Count") != "3" {
		t.Fatalf("feature count header = %q", w.Header().Get("X-Tangis-Feature-Count"))
	}
	if w.Body.String() != "FAKEMVT" {
		t.Fatalf("body = %q", w.Body.String())
	}
	// 缓存命中标记（第二次请求）
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/parcels/10/843/388.pbf", nil))
	if w.Code != http.StatusOK || w.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("cache hit: %d X-Cache=%q", w.Code, w.Header().Get("X-Cache"))
	}

	// 空 tile → 204（vecMockGateway 默认 tileM nil → 空）
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/nope/0/0/0.pbf", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown layer: %d", w.Code)
	}
	// 非法 z
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/parcels/30/0/0.pbf", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad z: %d", w.Code)
	}
	// 错误后缀
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/parcels/1/0/0.png", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad ext: %d", w.Code)
	}
	// 无后缀也接受
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/parcels/1/0/0", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("no ext: %d", w.Code)
	}
}

func TestVectorTileEmptyAndTooMany(t *testing.T) {
	gin.SetMode(gin.TestMode)
	srv := &vector.Server{
		Meta:        vector.NewMemoryMetaStore(),
		Gateway:     &vecMockGateway{ver: "v", pk: "id"}, // tileM nil → 空
		MaxFeatures: 100,
	}
	r := NewRouter(nil, nil, nil, AuthOptions{Enabled: false, Vector: srv}, nil)
	vectorSetupSourceAndLayer(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/parcels/5/1/1.pbf", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("empty tile: %d", w.Code)
	}

	// 超限 → 413
	srv.Gateway = &vecMockGateway{ver: "v", pk: "id", tileM: []byte("big"), tileN: 1, tileE: vector.ErrTooManyFeatures}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/parcels/5/1/1.pbf", nil))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too many: %d %s", w.Code, w.Body.String())
	}
}

func TestVectorLayerMetadataEndpoint(t *testing.T) {
	r, _ := newVectorRouter(t)
	vectorSetupSourceAndLayer(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/layers/parcels/metadata", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("metadata: %d %s", w.Code, w.Body.String())
	}
	var md map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &md); err != nil {
		t.Fatal(err)
	}
	if md["bbox_wgs84"] == nil || md["srid"].(float64) != 4326 || md["features_estimated"].(float64) != 42 {
		t.Fatalf("metadata = %s", w.Body.String())
	}
	if _, ok := md["minzoom"]; !ok {
		t.Fatalf("minzoom missing: %s", w.Body.String())
	}
	// 不存在的图层
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vector/layers/nope/metadata", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("metadata 404: %d", w.Code)
	}
}

func TestVectorRoutes503WhenNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewRouter(nil, nil, nil, AuthOptions{Enabled: false}, nil)
	for _, path := range []string{
		"/api/v1/vector/sources", "/api/v1/vector/layers",
		"/api/v1/vector/layers/x/metadata", "/api/v1/vector/x/1/0/0.pbf",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503", path, w.Code)
		}
	}
}
