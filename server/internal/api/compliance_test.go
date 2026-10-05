package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tangis/server/internal/task"
)

// artifactFixture 造一个带非瓦片产物的已发布任务（模拟合规脱密产物）。
func artifactFixture(t *testing.T) (*httptest.ResponseRecorder, func(method, path string) *httptest.ResponseRecorder) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "desensitized.tif"), []byte("TIFF-DATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "desensitize-record.json"), []byte(`{"mode":"noise"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := task.NewMemoryStore()
	if err := store.Create(&task.Task{
		ID: "dem-1", Type: task.TypeDemDesensitize, Output: dir,
		Status: task.StatusSucceeded, Approved: true, MinioPrefix: "tasks/dem-1",
	}); err != nil {
		t.Fatal(err)
	}
	r := NewRouter(store, nil, nil, AuthOptions{DataDir: t.TempDir()}, nil)
	do := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	return nil, do
}

// TestDownloadTaskArtifact 合规产物（脱密 GeoTIFF / 留痕 JSON）可按相对路径下载。
func TestDownloadTaskArtifact(t *testing.T) {
	_, do := artifactFixture(t)

	for _, name := range []string{"desensitized.tif", "desensitize-record.json"} {
		w := do(http.MethodGet, "/api/v1/tasks/dem-1/artifact/"+name)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code=%d body=%s", name, w.Code, w.Body.String())
		}
		if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, name) {
			t.Errorf("%s: Content-Disposition=%q", name, cd)
		}
	}
	if w := do(http.MethodGet, "/api/v1/tasks/dem-1/artifact/desensitized.tif"); !strings.Contains(w.Body.String(), "TIFF-DATA") {
		t.Errorf("产物内容不符: %q", w.Body.String())
	}
}

// TestDownloadTaskArtifactErrors 不存在 / 路径逃逸 / 未知任务。
func TestDownloadTaskArtifactErrors(t *testing.T) {
	_, do := artifactFixture(t)

	if w := do(http.MethodGet, "/api/v1/tasks/dem-1/artifact/nope.txt"); w.Code != http.StatusNotFound {
		t.Errorf("不存在产物 code=%d, want 404", w.Code)
	}
	// 逃逸尝试：URL 编码的 .. 与绝对路径
	for _, p := range []string{"..%2F..%2Fetc%2Fpasswd", "%2Fetc%2Fpasswd", "sub%2F..%2F..%2Fsecret"} {
		w := do(http.MethodGet, "/api/v1/tasks/dem-1/artifact/"+p)
		if w.Code == http.StatusOK {
			t.Errorf("路径逃逸 %q 未被拒绝（code=200）", p)
		}
	}
	if w := do(http.MethodGet, "/api/v1/tasks/nope/artifact/x.tif"); w.Code != http.StatusNotFound {
		t.Errorf("未知任务 code=%d, want 404", w.Code)
	}
}

// TestCRSIdentifyRejectsMissingPath 缺路径/文件不存在时明确报错（不调用内核）。
func TestCRSIdentifyRejectsMissingPath(t *testing.T) {
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{DataDir: t.TempDir()}, nil)
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/compliance/crs-identify", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	if w := post(`{}`); w.Code != http.StatusBadRequest {
		t.Errorf("缺 path: code=%d, want 400", w.Code)
	}
	if w := post(`{"path":"/no/such/file.tif"}`); w.Code != http.StatusNotFound {
		t.Errorf("文件不存在: code=%d, want 404", w.Code)
	}
}

// TestBursaRejectsEmptyPoints 点集为空/超限时拒绝（不落盘、不调内核）。
func TestBursaRejectsEmptyPoints(t *testing.T) {
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{DataDir: t.TempDir()}, nil)
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/compliance/bursa", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	if w := post(`{"params":{"dx":1},"points":[]}`); w.Code != http.StatusBadRequest {
		t.Errorf("空点集: code=%d, want 400", w.Code)
	}
	// 上限保护：构造超限点数会被拒绝（用 1 个点 + 伪造上限不现实，这里只验证空集与缺字段）
	if w := post(`{"params":{"dx":1}}`); w.Code != http.StatusBadRequest {
		t.Errorf("缺 points: code=%d, want 400", w.Code)
	}
}
