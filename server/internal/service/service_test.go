package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/task"
)

// fakeGetter 按 key 返回内容，记录调用（模拟 MinIO 回源）。
type fakeGetter struct {
	objects map[string]string // key -> content
	gets    []string
}

func (f *fakeGetter) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	f.gets = append(f.gets, key)
	data, ok := f.objects[key]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(data)), int64(len(data)), nil
}

type fixture struct {
	r         *gin.Engine
	srv       *Server
	store     *task.MemoryStore
	getter    *fakeGetter
	outputDir string
	taskID    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Data"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"tileset.json":  `{"asset":{"version":"1.1"}}`,
		"Data/0.b3dm":   "\x00\x00\x00\x00b3dm",
		"Data/app.json": `{"meta":"app"}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := task.NewMemoryStore()
	tk := &task.Task{
		ID:          "svc-task-1",
		Type:        "osgb->3dtiles",
		Source:      "/data/osgb/town",
		Output:      dir,
		Status:      task.StatusSucceeded,
		MinioPrefix: "tasks/svc-task-1",
		Approved:    true, // 已审批发布（F-21）
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	getter := &fakeGetter{objects: map[string]string{
		"tasks/svc-task-1/Data/1.b3dm": "REMOTE-B3DM",
	}}

	s := &Server{Store: store, Objects: getter, Bucket: "tangis"}
	r := gin.New()
	r.GET("/api/v1/services", s.ListServices)
	r.GET("/services/:task_id/*path", s.ServeTile)
	r.OPTIONS("/services/:task_id/*path", s.ServeTile)

	return &fixture{r: r, srv: s, store: store, getter: getter, outputDir: dir, taskID: tk.ID}
}

func (f *fixture) do(method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

func TestListServicesOnlyPublished(t *testing.T) {
	f := newFixture(t)
	// 一个 RUNNING 的任务不应出现在列表
	if err := f.store.Create(&task.Task{
		ID: "svc-task-2", Type: "image->tiles", Source: "/s", Output: "/o",
		Status: task.StatusRunning, MinioPrefix: "tasks/svc-task-2",
	}); err != nil {
		t.Fatal(err)
	}
	// SUCCEEDED 但未上传（无 minio_prefix）也不应出现
	if err := f.store.Create(&task.Task{
		ID: "svc-task-3", Type: "image->tiles", Source: "/s", Output: "/o",
		Status: task.StatusSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
	// SUCCEEDED + 已上传但未审批（F-21）也不应出现
	if err := f.store.Create(&task.Task{
		ID: "svc-task-4", Type: "image->tiles", Source: "/s", Output: "/o",
		Status: task.StatusSucceeded, MinioPrefix: "tasks/svc-task-4",
	}); err != nil {
		t.Fatal(err)
	}

	w := f.do(http.MethodGet, "/api/v1/services")
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "svc-task-1") {
		t.Fatalf("published task missing: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "svc-task-2") || strings.Contains(w.Body.String(), "svc-task-3") ||
		strings.Contains(w.Body.String(), "svc-task-4") {
		t.Fatalf("unpublished tasks leaked into services list: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/services/svc-task-1/tileset.json") {
		t.Fatalf("tileset_url missing: %s", w.Body.String())
	}
}

func TestServeTilesetJSONFromLocal(t *testing.T) {
	f := newFixture(t)
	w := f.do(http.MethodGet, "/services/svc-task-1/tileset.json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if !strings.Contains(w.Body.String(), `"version":"1.1"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	// 本地命中不应触发 MinIO 回源
	if len(f.getter.gets) != 0 {
		t.Fatalf("local hit should not hit MinIO, got %v", f.getter.gets)
	}
	// CORS 头（Cesium 跨域必需）
	if ao := w.Header().Get("Access-Control-Allow-Origin"); ao != "*" {
		t.Fatalf("CORS allow-origin = %q, want *", ao)
	}
}

func TestServeB3dmContentType(t *testing.T) {
	f := newFixture(t)
	w := f.do(http.MethodGet, "/services/svc-task-1/Data/0.b3dm")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q, want application/octet-stream", ct)
	}
	if w.Body.String() != "\x00\x00\x00\x00b3dm" {
		t.Fatalf("body mismatch: %q", w.Body.String())
	}
}

func TestServeTileFallsBackToMinIO(t *testing.T) {
	f := newFixture(t)
	w := f.do(http.MethodGet, "/services/svc-task-1/Data/1.b3dm")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "REMOTE-B3DM" {
		t.Fatalf("body = %q, want remote content", w.Body.String())
	}
	if len(f.getter.gets) != 1 || f.getter.gets[0] != "tasks/svc-task-1/Data/1.b3dm" {
		t.Fatalf("MinIO gets = %v, want [tasks/svc-task-1/Data/1.b3dm]", f.getter.gets)
	}
}

func TestServeTilePathTraversal(t *testing.T) {
	f := newFixture(t)
	// 在 output 目录外放置机密文件，验证穿越拿不到
	secret := filepath.Join(f.outputDir, "..", "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []string{
		"/services/svc-task-1/../secret.txt",         // 原始 ..
		"/services/svc-task-1/%2e%2e/secret.txt",     // URL 编码 ..
		"/services/svc-task-1/%2e%2e%2fsecret.txt",   // URL 编码全角
		"/services/svc-task-1/..%2fsecret.txt",       // 半编码
		"/services/svc-task-1/Data/../../secret.txt", // 深层穿越
		"/services/svc-task-1/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
	}
	for _, p := range cases {
		w := f.do(http.MethodGet, p)
		if w.Code == http.StatusOK {
			t.Fatalf("path traversal succeeded for %q: %s", p, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "TOPSECRET") {
			t.Fatalf("secret leaked for %q", p)
		}
	}
}

func TestServeTileErrors(t *testing.T) {
	f := newFixture(t)

	// 任务不存在
	if w := f.do(http.MethodGet, "/services/nope/tileset.json"); w.Code != http.StatusNotFound {
		t.Fatalf("missing task status = %d, want 404", w.Code)
	}
	// 本地与远端都缺文件
	if w := f.do(http.MethodGet, "/services/svc-task-1/Data/missing.b3dm"); w.Code != http.StatusNotFound {
		t.Fatalf("missing file status = %d, want 404", w.Code)
	}
	// 空 path
	if w := f.do(http.MethodGet, "/services/svc-task-1/"); w.Code != http.StatusBadRequest {
		t.Fatalf("empty path status = %d, want 400", w.Code)
	}
	// 未发布任务（无 minio_prefix）
	if err := f.store.Create(&task.Task{
		ID: "svc-task-9", Type: "t", Source: "/s", Output: f.outputDir, Status: task.StatusSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
	if w := f.do(http.MethodGet, "/services/svc-task-9/tileset.json"); w.Code != http.StatusNotFound {
		t.Fatalf("unpublished task status = %d, want 404", w.Code)
	}
	// SUCCEEDED + 已上传但未审批（F-21）同样不可分发
	if err := f.store.Create(&task.Task{
		ID: "svc-task-10", Type: "t", Source: "/s", Output: f.outputDir,
		Status: task.StatusSucceeded, MinioPrefix: "tasks/svc-task-10",
	}); err != nil {
		t.Fatal(err)
	}
	if w := f.do(http.MethodGet, "/services/svc-task-10/tileset.json"); w.Code != http.StatusNotFound {
		t.Fatalf("unapproved task status = %d, want 404", w.Code)
	}
}

func TestServeTileOptionsPreflight(t *testing.T) {
	f := newFixture(t)
	w := f.do(http.MethodOptions, "/services/svc-task-1/tileset.json")
	if w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want 204", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("OPTIONS should carry CORS headers")
	}
}
