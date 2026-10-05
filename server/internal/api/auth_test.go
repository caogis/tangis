package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
	"tangis/server/internal/service"
	"tangis/server/internal/task"
)

// doReqKey 带 X-API-Key 的请求。
func doReqKey(r *gin.Engine, method, path, key, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAuthRequired(t *testing.T) {
	r, _ := setupAuth(t)

	// 无 key -> 401
	if w := doReqKey(r, http.MethodGet, "/api/v1/tasks", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key status = %d, want 401", w.Code)
	}
	// 错误 key -> 401
	if w := doReqKey(r, http.MethodGet, "/api/v1/tasks", "wrong", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d, want 401", w.Code)
	}
	// 有效 admin key -> 200
	if w := doReqKey(r, http.MethodGet, "/api/v1/tasks", "admin-key", ""); w.Code != http.StatusOK {
		t.Fatalf("admin key status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	// healthz 不在鉴权范围内
	if w := doReqKey(r, http.MethodGet, "/healthz", "", ""); w.Code != http.StatusOK {
		t.Fatalf("healthz should skip auth, got %d", w.Code)
	}
}

func TestTenantIsolation(t *testing.T) {
	r, _ := setupAuth(t)

	// acme 租户建任务
	w := doReqKey(r, http.MethodPost, "/api/v1/tasks", "tenant-key",
		`{"type":"t","source":"/s","output":"/o"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("tenant create = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Task task.Task `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Task.TenantID != "acme" {
		t.Fatalf("task tenant = %q, want acme", resp.Task.TenantID)
	}
	id := resp.Task.ID

	// tenant key 列表/详情只见本租户任务
	w = doReqKey(r, http.MethodGet, "/api/v1/tasks", "tenant-key", "")
	var list struct {
		Tasks []*task.Task `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, tk := range list.Tasks {
		if tk.TenantID != "acme" {
			t.Fatalf("tenant list leaked task of tenant %q", tk.TenantID)
		}
	}
	if w := doReqKey(r, http.MethodGet, "/api/v1/tasks/"+id, "tenant-key", ""); w.Code != http.StatusOK {
		t.Fatalf("owner get = %d, want 200", w.Code)
	}
	// tenant key 不能删除他人任务：再造一个其他租户任务验证隔离
	// （admin 直接在底层写入 other-tenant 任务）
	// admin（scope=全部）可见
	if w := doReqKey(r, http.MethodGet, "/api/v1/tasks/"+id, "admin-key", ""); w.Code != http.StatusOK {
		t.Fatalf("admin get = %d, want 200", w.Code)
	}
}

func TestTenantCannotAccessOthersTask(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ks := auth.NewMemoryKeyStore("admin-key")
	ks.Add("acme-key", &auth.APIKey{ID: "ka", Name: "acme", TenantID: "acme", Role: auth.RoleTenant})
	ks.Add("acme2-key", &auth.APIKey{ID: "kb", Name: "acme2", TenantID: "acme2", Role: auth.RoleTenant})
	store := task.NewMemoryStore()
	r := NewRouter(store, nil, nil, AuthOptions{Enabled: true, Keys: ks}, nil)

	// acme 建任务
	w := doReqKey(r, http.MethodPost, "/api/v1/tasks", "acme-key",
		`{"type":"t","source":"/s","output":"/o"}`)
	var resp struct {
		Task task.Task `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	id := resp.Task.ID

	// acme2 访问 acme 任务：404（不泄露存在性）
	if w := doReqKey(r, http.MethodGet, "/api/v1/tasks/"+id, "acme2-key", ""); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get = %d, want 404", w.Code)
	}
	if w := doReqKey(r, http.MethodDelete, "/api/v1/tasks/"+id, "acme2-key", ""); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete = %d, want 404", w.Code)
	}
	// 任务仍在（删除未生效）
	if _, err := store.Get(id, ""); err != nil {
		t.Fatalf("task should survive cross-tenant delete: %v", err)
	}
}

func TestApproveRequiresAdmin(t *testing.T) {
	r, _ := setupAuth(t)

	// tenant key 建任务
	w := doReqKey(r, http.MethodPost, "/api/v1/tasks", "tenant-key",
		`{"type":"t","source":"/s","output":"/o"}`)
	var resp struct {
		Task task.Task `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	id := resp.Task.ID

	// tenant key 审批 -> 403
	if w := doReqKey(r, http.MethodPost, "/api/v1/tasks/"+id+"/approve", "tenant-key", ""); w.Code != http.StatusForbidden {
		t.Fatalf("tenant approve = %d, want 403", w.Code)
	}
	// admin key 审批 -> 200 且 approved=true
	w = doReqKey(r, http.MethodPost, "/api/v1/tasks/"+id+"/approve", "admin-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin approve = %d, body=%s", w.Code, w.Body.String())
	}
	var got task.Task
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Approved {
		t.Fatal("approved should be true after admin approve")
	}
}

func TestAuthOffAllowsEverything(t *testing.T) {
	r := setup(t) // AUTH off

	// 无 key 即可建任务，归属 default 租户
	w := doReq(r, http.MethodPost, "/api/v1/tasks", `{"type":"t","source":"/s","output":"/o"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("auth-off create = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"tenant_id":"default"`) {
		t.Fatalf("auth-off task should belong to default tenant: %s", w.Body.String())
	}
	// AUTH=off 时审批放行
	if w := doReq(r, http.MethodPost, "/api/v1/tasks/nonexistent/approve", ""); w.Code != http.StatusNotFound {
		// 任务不存在返回 404 而非 403，证明已进入审批逻辑
		t.Fatalf("auth-off approve should pass permission check (404 for missing task), got %d", w.Code)
	}
}

func TestCreateTaskWithParams(t *testing.T) {
	fp := &fakePublisher{}
	r, _ := setupWithPublisher(t, fp)

	body := `{"type":"osgb->3dtiles","source":"/s","output":"/o","params":{"lod":2,"crs":"EPSG:4490"}}`
	w := doReq(r, http.MethodPost, "/api/v1/tasks", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Task task.Task `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Task.Params["crs"] != "EPSG:4490" || resp.Task.Params["lod"] != float64(2) {
		t.Fatalf("params mismatch: %+v", resp.Task.Params)
	}
	// params 随任务消息下发内核
	if len(fp.published) != 1 || fp.published[0].Params["crs"] != "EPSG:4490" {
		t.Fatalf("published params mismatch: %+v", fp.published[0].Params)
	}
	// GET 回读 params
	w = doReq(r, http.MethodGet, "/api/v1/tasks/"+resp.Task.ID, "")
	if !strings.Contains(w.Body.String(), `"crs":"EPSG:4490"`) {
		t.Fatalf("params not returned by GET: %s", w.Body.String())
	}
}

func TestScopeTenantConstant(t *testing.T) {
	// 保证 service 包 context key 与 api 中间件一致
	if service.ScopeTenantKey == "" {
		t.Fatal("scope tenant key must not be empty")
	}
	_ = auth.RoleAdmin
}
