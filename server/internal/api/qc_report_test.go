package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/task"
)

// TestTaskQCFieldsInDetail 详情响应携带 qc_status/qc_summary（M2-F08b）。
func TestTaskQCFieldsInDetail(t *testing.T) {
	store := task.NewMemoryStore()
	r := newRouterWithStore(store)

	out := t.TempDir()
	tk := &task.Task{
		ID: "qc-detail-1", Type: "osgb->3dtiles", Source: "/data/osgb/town",
		Output: out, Status: task.StatusSucceeded,
		QcStatus: task.QcPass,
		QcSummary: &task.QcSummary{
			TileCount: 80, TotalDegenerate: 1, TotalFlippedEdges: 2,
			TotalFloating: 3, TotalCrackSegments: 4, TotalSelfIntersections: 5,
		},
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	w := doReq(r, http.MethodGet, "/api/v1/tasks/qc-detail-1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		QcStatus  string          `json:"qc_status"`
		QcSummary *map[string]any `json:"qc_summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if resp.QcStatus != task.QcPass {
		t.Fatalf("qc_status = %q, want %q", resp.QcStatus, task.QcPass)
	}
	if resp.QcSummary == nil || (*resp.QcSummary)["total_degenerate"] != float64(1) {
		t.Fatalf("qc_summary mismatch: %+v", resp.QcSummary)
	}
}

// TestGetTaskQCReport 下载端点：报告存在 200（内容一致），缺失/无任务 404。
func TestGetTaskQCReport(t *testing.T) {
	store := task.NewMemoryStore()
	r := newRouterWithStore(store)

	out := t.TempDir()
	tk := &task.Task{
		ID: "qc-dl-1", Type: "osgb->3dtiles", Source: "/data/osgb/town",
		Output: out, Status: task.StatusSucceeded, QcStatus: task.QcPass,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	// 报告未生成：404（不伪造内容）
	w := doReq(r, http.MethodGet, "/api/v1/tasks/qc-dl-1/qc-report", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing report status = %d, want 404, body=%s", w.Code, w.Body.String())
	}

	// 报告存在：200 + 原文一致 + 下载头
	want := []byte(`{"schema_version":"1","tile_count":80,"summary":{"passed":true}}`)
	if err := os.WriteFile(filepath.Join(out, task.QcReportFile), want, 0o644); err != nil {
		t.Fatal(err)
	}
	w = doReq(r, http.MethodGet, "/api/v1/tasks/qc-dl-1/qc-report", "")
	if w.Code != http.StatusOK {
		t.Fatalf("report status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(want) {
		t.Fatalf("report body = %q, want %q", w.Body.String(), want)
	}
	if cd := w.Header().Get("Content-Disposition"); cd == "" {
		t.Fatal("missing Content-Disposition attachment header")
	}

	// 任务不存在：404
	w = doReq(r, http.MethodGet, "/api/v1/tasks/missing/qc-report", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing task status = %d, want 404", w.Code)
	}
}

// newRouterWithStore 指定 store 的路由（鉴权关闭，与 setup 一致）。
func newRouterWithStore(store task.Store) *gin.Engine {
	gin.SetMode(gin.TestMode)
	return NewRouter(store, nil, nil, AuthOptions{}, nil)
}
