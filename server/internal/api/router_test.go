package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// fakePublisher 记录 Publish 调用，可注入失败模拟队列不可用。
type fakePublisher struct {
	published []queue.TaskMessage
	failErr   error
}

func (f *fakePublisher) Publish(msg queue.TaskMessage) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.published = append(f.published, msg)
	return nil
}

func (f *fakePublisher) Close() {}

func setup(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	// 鉴权关闭（TANGIS_AUTH=off 兼容模式），与旧行为一致
	return NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{}, nil)
}

// setupAuth 创建带 X-API-Key 鉴权的路由，返回引擎与内存 KeyStore。
func setupAuth(t *testing.T) (*gin.Engine, *auth.MemoryKeyStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ks := auth.NewMemoryKeyStore("admin-key")
	ks.Add("tenant-key", &auth.APIKey{ID: "k2", Name: "tenant", TenantID: "acme", Role: auth.RoleTenant})
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{Enabled: true, Keys: ks}, nil)
	return r, ks
}

func setupWithPublisher(t *testing.T, p queue.Publisher) (*gin.Engine, *fakePublisher) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fp, _ := p.(*fakePublisher)
	return NewRouter(task.NewMemoryStore(), p, nil, AuthOptions{}, nil), fp
}

func doReq(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHealthz(t *testing.T) {
	r := setup(t)
	w := doReq(r, http.MethodGet, "/healthz", "")

	if w.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz response not json: %v", err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("healthz status field = %q, want %q", resp["status"], "ok")
	}
}

func TestTaskCRUD(t *testing.T) {
	r := setup(t)

	// 创建任务
	body := `{"type":"osgb->3dtiles","source":"/data/osgb/town","output":"minio://tiles/town"}`
	w := doReq(r, http.MethodPost, "/api/v1/tasks", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body=%s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp struct {
		Task    task.Task `json:"task"`
		Warning string    `json:"warning"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("create response not json: %v", err)
	}
	created := resp.Task
	if created.ID == "" {
		t.Fatal("created task has empty id")
	}
	if created.Status != task.StatusPending {
		t.Fatalf("created status = %q, want %q", created.Status, task.StatusPending)
	}
	if created.Type != "osgb->3dtiles" || created.Source != "/data/osgb/town" {
		t.Fatalf("created fields mismatch: %+v", created)
	}
	if created.ManifestPath == "" {
		t.Fatal("created task missing manifest_path")
	}

	// 列表应包含该任务
	w = doReq(r, http.MethodGet, "/api/v1/tasks", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d", w.Code, http.StatusOK)
	}
	var list struct {
		Tasks []*task.Task `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("list response not json: %v", err)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].ID != created.ID {
		t.Fatalf("list = %+v, want 1 task with id %s", list.Tasks, created.ID)
	}

	// 按 ID 查询
	w = doReq(r, http.MethodGet, "/api/v1/tasks/"+created.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", w.Code, http.StatusOK)
	}
	var got task.Task
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("get response not json: %v", err)
	}
	if got.ID != created.ID || got.Status != task.StatusPending {
		t.Fatalf("got = %+v, mismatch", got)
	}

	// 删除
	w = doReq(r, http.MethodDelete, "/api/v1/tasks/"+created.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", w.Code, http.StatusNoContent)
	}

	// 删除后再查应 404
	w = doReq(r, http.MethodGet, "/api/v1/tasks/"+created.ID, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestCreateTaskValidation(t *testing.T) {
	r := setup(t)

	// 缺少必填字段应 400
	w := doReq(r, http.MethodPost, "/api/v1/tasks", `{"type":"osgb->3dtiles"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid create status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	r := setup(t)
	w := doReq(r, http.MethodGet, "/api/v1/tasks/nonexistent", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("get missing status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestCreateTaskPublishesToQueue(t *testing.T) {
	fp := &fakePublisher{}
	r, _ := setupWithPublisher(t, fp)

	body := `{"type":"osgb->3dtiles","source":"/data/osgb/town","output":"/data/out/town"}`
	w := doReq(r, http.MethodPost, "/api/v1/tasks", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body=%s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp struct {
		Task    task.Task `json:"task"`
		Warning string    `json:"warning"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if resp.Warning != "" {
		t.Fatalf("unexpected warning: %q", resp.Warning)
	}
	if len(fp.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(fp.published))
	}
	msg := fp.published[0]
	if msg.TaskID != resp.Task.ID {
		t.Fatalf("msg task_id = %q, want %q", msg.TaskID, resp.Task.ID)
	}
	if msg.ManifestPath != resp.Task.ManifestPath {
		t.Fatalf("msg manifest_path = %q, want %q", msg.ManifestPath, resp.Task.ManifestPath)
	}
}

func TestCreateTaskDegradedWhenPublishFails(t *testing.T) {
	fp := &fakePublisher{failErr: errors.New("nats: connection refused")}
	r, _ := setupWithPublisher(t, fp)

	body := `{"type":"osgb->3dtiles","source":"/data/osgb/town","output":"/data/out/town"}`
	w := doReq(r, http.MethodPost, "/api/v1/tasks", body)
	// 降级：创建仍然成功（201），任务保持 PENDING，响应附 warning
	if w.Code != http.StatusCreated {
		t.Fatalf("degraded create status = %d, want %d, body=%s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp struct {
		Task    task.Task `json:"task"`
		Warning string    `json:"warning"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if resp.Warning == "" {
		t.Fatal("degraded create should carry warning")
	}
	if resp.Task.Status != task.StatusPending {
		t.Fatalf("degraded status = %q, want PENDING", resp.Task.Status)
	}
	if len(fp.published) != 0 {
		t.Fatalf("published %d messages on failure, want 0", len(fp.published))
	}
}

func TestCreateTaskDegradedWithoutPublisher(t *testing.T) {
	// publisher 为 nil（启动时 NATS 不可用）：建任务不失败，附 warning
	r := setup(t)
	body := `{"type":"osgb->3dtiles","source":"/data/osgb/town","output":"/data/out/town"}`
	w := doReq(r, http.MethodPost, "/api/v1/tasks", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("degraded create status = %d, want %d", w.Code, http.StatusCreated)
	}
	if !strings.Contains(w.Body.String(), "warning") {
		t.Fatalf("response missing warning: %s", w.Body.String())
	}
}

// TestAPIV1CORS 验证 /api/v1 跨域支持：浏览器自定义头 X-API-Key 触发预检，
// 预检（含未注册路由）必须 204 且带 CORS 头；实际响应也带 CORS 头。
func TestAPIV1CORS(t *testing.T) {
	r := setup(t)

	// OPTIONS 预检 /api/v1/services（该路由未注册 OPTIONS，曾返回 404 且无 CORS 头）
	w := doReq(r, http.MethodOptions, "/api/v1/services", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204, body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("preflight missing Access-Control-Allow-Origin")
	}
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "X-API-Key") {
		t.Fatalf("preflight allow-headers missing X-API-Key: %q", w.Header().Get("Access-Control-Allow-Headers"))
	}
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "X-Role") {
		t.Fatalf("preflight allow-headers missing X-Role: %q", w.Header().Get("Access-Control-Allow-Headers"))
	}

	// GET 实际响应带 CORS 头
	w = doReq(r, http.MethodGet, "/api/v1/tasks", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get tasks status = %d, body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("GET /api/v1/tasks missing CORS allow-origin")
	}

	// WMTS 入口同属 /api/v1，也应带 CORS 头
	w = doReq(r, http.MethodGet, "/api/v1/wmts?request=GetCapabilities", "")
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("GET /api/v1/wmts missing CORS allow-origin")
	}

	// 非 /api/v1 路径不受该中间件影响（healthz 不强加 CORS 头，由各处理器自理）
	w = doReq(r, http.MethodGet, "/healthz", "")
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("healthz should not carry api CORS header")
	}
}

// TestAPIV1CORSPreflightWithAuth 鉴权开启时预检不携带 API Key 也必须放行
// （浏览器预检请求不会带上自定义头）。
func TestAPIV1CORSPreflightWithAuth(t *testing.T) {
	r, _ := setupAuth(t)
	w := doReq(r, http.MethodOptions, "/api/v1/tasks", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight with auth enabled status = %d, want 204 (not 401)", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("preflight with auth missing CORS allow-origin")
	}
	// 真实 GET 仍需 API Key：无 key 401，但响应仍带 CORS 头（浏览器可读错误）
	w = doReq(r, http.MethodGet, "/api/v1/tasks", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("GET without key = %d, want 401", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("401 response missing CORS allow-origin")
	}
}
