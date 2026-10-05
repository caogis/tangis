package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
	"tangis/server/internal/cache"
	"tangis/server/internal/service"
	"tangis/server/internal/task"
)

// F-03：WMTS/TMS 路由鉴权（API Key 或签名任一）与列表缓存行为。

func setupTiles(t *testing.T) (*gin.Engine, *auth.MemoryKeyStore, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ks := auth.NewMemoryKeyStore("admin-key")
	svc := &service.Server{Store: task.NewMemoryStore()}
	secret := "test-sign-secret"
	r := NewRouter(svc.Store, nil, svc, AuthOptions{
		Enabled: true, Keys: ks, SignSecret: secret,
	}, nil)
	return r, ks, secret
}

func TestTileEndpointsRequireAuth(t *testing.T) {
	r, _, _ := setupTiles(t)

	paths := []string{
		"/api/v1/wmts?request=GetCapabilities",
		"/api/v1/wmts/1.0.0/some-task/default/WebMercatorQuad/0/0/0.png",
		"/api/v1/tms/some-task/0/0/0.png",
	}
	for _, p := range paths {
		if w := doReq(r, http.MethodGet, p, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without auth = %d, want 401, body=%s", p, w.Code, w.Body.String())
		}
	}
}

func TestTileEndpointsWithAPIKey(t *testing.T) {
	r, _, _ := setupTiles(t)
	w := doReqKey(r, http.MethodGet, "/api/v1/wmts?request=GetCapabilities", "admin-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("api key GetCapabilities = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "<Capabilities") {
		t.Fatalf("not a capabilities doc: %s", w.Body.String())
	}
}

func TestTileEndpointsWithSignature(t *testing.T) {
	r, _, secret := setupTiles(t)

	// 对 KVP path 签名（签名覆盖 path，不含 query）
	sig := auth.SignURL("/api/v1/wmts", time.Now().Add(time.Hour), secret)
	p := "/api/v1/wmts?request=GetCapabilities&" + sig
	if w := doReq(r, http.MethodGet, p, ""); w.Code != http.StatusOK {
		t.Fatalf("signed GetCapabilities = %d, body=%s", w.Code, w.Body.String())
	}

	// RESTful path 签名
	sig = auth.SignURL("/api/v1/wmts/1.0.0/t1/default/WebMercatorQuad/0/0/0.png", time.Now().Add(time.Hour), secret)
	p = "/api/v1/wmts/1.0.0/t1/default/WebMercatorQuad/0/0/0.png?" + sig
	if w := doReq(r, http.MethodGet, p, ""); w.Code == http.StatusUnauthorized {
		t.Fatalf("signed REST tile should pass auth, got 401 body=%s", w.Body.String())
	}

	// 过期签名 → 401
	sig = auth.SignURL("/api/v1/wmts", time.Now().Add(-time.Hour), secret)
	p = "/api/v1/wmts?request=GetCapabilities&" + sig
	if w := doReq(r, http.MethodGet, p, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired signature = %d, want 401", w.Code)
	}
}

func TestTileAuthOffAllowsAll(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &service.Server{Store: task.NewMemoryStore()}
	r := NewRouter(svc.Store, nil, svc, AuthOptions{Enabled: false}, nil)
	if w := doReq(r, http.MethodGet, "/api/v1/wmts?request=GetCapabilities", ""); w.Code != http.StatusOK {
		t.Fatalf("auth-off GetCapabilities = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestTaskListCacheWithInvalidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cc := cache.NewMemoryCache()
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{}, cc)

	// 第一次列表：MISS
	w := doReq(r, http.MethodGet, "/api/v1/tasks", "")
	if w.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first list = %s, want MISS", w.Header().Get("X-Cache"))
	}
	// 第二次：HIT
	w = doReq(r, http.MethodGet, "/api/v1/tasks", "")
	if w.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second list = %s, want HIT", w.Header().Get("X-Cache"))
	}
	// 新建任务触发失效：再次列表应 MISS 且包含新任务
	if w := doReq(r, http.MethodPost, "/api/v1/tasks", `{"type":"t","source":"/s","output":"/o"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	w = doReq(r, http.MethodGet, "/api/v1/tasks", "")
	if w.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("list after create = %s, want MISS (write should invalidate)", w.Header().Get("X-Cache"))
	}
	if !strings.Contains(w.Body.String(), `"type":"t"`) {
		t.Fatalf("fresh list should contain new task: %s", w.Body.String())
	}
}
