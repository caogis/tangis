package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tangis/server/internal/task"
)

// fakeInterrupter 记录被中断的任务，并报告"命中运行中进程"。
type fakeInterrupter struct {
	hits []string
}

func (f *fakeInterrupter) Interrupt(taskID string) bool {
	f.hits = append(f.hits, taskID)
	return true
}

// TestCheckActionAllowed 覆盖任务控制动作与状态的匹配矩阵（F-04）。
func TestCheckActionAllowed(t *testing.T) {
	cases := []struct {
		action taskAction
		status task.Status
		wantOK bool
	}{
		{taskActionCancel, task.StatusPending, true},
		{taskActionCancel, task.StatusRunning, true},
		{taskActionCancel, task.StatusPaused, true},
		{taskActionCancel, task.StatusSucceeded, false},
		{taskActionCancel, task.StatusFailed, false},
		{taskActionCancel, task.StatusCancelled, false},

		{taskActionPause, task.StatusPending, true},
		{taskActionPause, task.StatusRunning, true},
		{taskActionPause, task.StatusPaused, false},
		{taskActionPause, task.StatusSucceeded, false},

		{taskActionResume, task.StatusPaused, true},
		{taskActionResume, task.StatusPending, false},
		{taskActionResume, task.StatusCancelled, false},

		{taskActionRetry, task.StatusFailed, true},
		{taskActionRetry, task.StatusCancelled, true},
		{taskActionRetry, task.StatusSucceeded, false},
		{taskActionRetry, task.StatusPending, false},
	}
	for _, c := range cases {
		gotOK := checkActionAllowed(c.action, c.status) == ""
		if gotOK != c.wantOK {
			t.Errorf("checkActionAllowed(%s, %s) allowed=%v, want %v", c.action, c.status, gotOK, c.wantOK)
		}
	}
}

// TestTaskControlLifecycle 端到端走一遍 暂停 → 恢复 → 取消 → 非法动作拒绝。
func TestTaskControlLifecycle(t *testing.T) {
	store := task.NewMemoryStore()
	pub := &fakePublisher{}
	it := &fakeInterrupter{}
	r := NewRouter(store, pub, nil, AuthOptions{Interrupter: it}, nil)

	tk := &task.Task{
		ID: "t-ctrl", Type: "osgb->3dtiles", Source: "/src", Output: "/out",
		ManifestPath: "/out/manifest.json", Status: task.StatusRunning,
	}
	if err := store.Create(tk); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	do := func(action string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t-ctrl/"+action, nil)
		r.ServeHTTP(w, req)
		return w
	}
	reload := func() *task.Task {
		cur, err := store.Get("t-ctrl", "")
		if err != nil {
			t.Fatalf("reload task: %v", err)
		}
		return cur
	}

	// RUNNING → pause：应中断内核进程并置 PAUSED
	if w := do("pause"); w.Code != http.StatusOK {
		t.Fatalf("pause: code=%d body=%s", w.Code, w.Body.String())
	}
	if cur := reload(); cur.Status != task.StatusPaused {
		t.Fatalf("after pause status=%s, want PAUSED", cur.Status)
	}
	if len(it.hits) != 1 || it.hits[0] != "t-ctrl" {
		t.Fatalf("interrupter hits=%v, want [t-ctrl]", it.hits)
	}

	// PAUSED → resume：应置 PENDING 并重新入队
	if w := do("resume"); w.Code != http.StatusOK {
		t.Fatalf("resume: code=%d body=%s", w.Code, w.Body.String())
	}
	if cur := reload(); cur.Status != task.StatusPending {
		t.Fatalf("after resume status=%s, want PENDING", cur.Status)
	}
	if len(pub.published) != 1 || pub.published[0].TaskID != "t-ctrl" {
		t.Fatalf("requeue published=%v, want one message for t-ctrl", pub.published)
	}

	// PENDING → resume 非法（409）
	if w := do("resume"); w.Code != http.StatusConflict {
		t.Fatalf("resume on PENDING: code=%d, want 409", w.Code)
	}

	// PENDING → cancel：直接置 CANCELLED，不应中断进程（没有在跑的进程）
	before := len(it.hits)
	if w := do("cancel"); w.Code != http.StatusOK {
		t.Fatalf("cancel: code=%d body=%s", w.Code, w.Body.String())
	}
	if cur := reload(); cur.Status != task.StatusCancelled {
		t.Fatalf("after cancel status=%s, want CANCELLED", cur.Status)
	}
	if len(it.hits) != before {
		t.Fatalf("cancel on PENDING should not interrupt kernel, hits=%v", it.hits)
	}

	// CANCELLED → cancel 非法（终态）
	if w := do("cancel"); w.Code != http.StatusConflict {
		t.Fatalf("cancel terminal: code=%d, want 409", w.Code)
	}
}

// TestTaskRetryResetsAttempts 重试应清零退避计数，否则耗尽重试的任务会立即再失败。
func TestTaskRetryResetsAttempts(t *testing.T) {
	store := task.NewMemoryStore()
	pub := &fakePublisher{}
	r := NewRouter(store, pub, nil, AuthOptions{}, nil)

	tk := &task.Task{
		ID: "t-retry", Type: "osgb->3dtiles", Source: "/src", Output: "/out",
		Status: task.StatusFailed, Attempts: 3, ErrMsg: "kernel exit: signal: killed",
	}
	if err := store.Create(tk); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t-retry/retry", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("retry: code=%d body=%s", w.Code, w.Body.String())
	}
	cur, err := store.Get("t-retry", "")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cur.Status != task.StatusPending {
		t.Errorf("status=%s, want PENDING", cur.Status)
	}
	if cur.Attempts != 0 {
		t.Errorf("attempts=%d, want 0 (显式重试=新一轮)", cur.Attempts)
	}
	if cur.ErrMsg != "" {
		t.Errorf("err_msg=%q, want empty", cur.ErrMsg)
	}
	if len(pub.published) != 1 {
		t.Errorf("published=%d, want 1", len(pub.published))
	}
}

// TestTaskControlNotFound 未知任务返回 404。
func TestTaskControlNotFound(t *testing.T) {
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{}, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/tasks/nope/cancel", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", w.Code)
	}
}

// ---- 通用上传（F-01 导入体验）----

// uploadRequest 构造多文件 + paths 数组的 multipart 请求体。
// paths 必须先于文件 part（服务端流式顺序消费）。
func uploadRequest(t *testing.T, paths []string, files map[string]string, order []string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if paths != nil {
		raw, err := json.Marshal(paths)
		if err != nil {
			t.Fatalf("marshal paths: %v", err)
		}
		fw, err := mw.CreateFormField("paths")
		if err != nil {
			t.Fatalf("create paths field: %v", err)
		}
		if _, err := fw.Write(raw); err != nil {
			t.Fatalf("write paths: %v", err)
		}
	}
	for _, name := range order {
		fw, err := mw.CreateFormFile("files", filepath.Base(name))
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := fw.Write([]byte(files[name])); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &body, mw.FormDataContentType()
}

// TestUploadFilesPreservesStructure 上传 OSGB 目录结构：落盘保留层级，
// root 收敛到公共顶层目录（可直接作为任务 source）。
func TestUploadFilesPreservesStructure(t *testing.T) {
	dir := t.TempDir()
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{DataDir: dir}, nil)

	paths := []string{"Proj/Data/a.osgb", "Proj/Data/b.osgb"}
	files := map[string]string{"Proj/Data/a.osgb": "AAA", "Proj/Data/b.osgb": "BBBB"}
	body, ctype := uploadRequest(t, paths, files, paths)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", body)
	req.Header.Set("Content-Type", ctype)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var res struct {
		UploadID string `json:"upload_id"`
		Root     string `json:"root"`
		Files    int    `json:"files"`
		Bytes    int64  `json:"bytes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Files != 2 || res.Bytes != 7 {
		t.Errorf("files=%d bytes=%d, want 2/7", res.Files, res.Bytes)
	}
	wantRoot := filepath.Join(dir, "uploads", res.UploadID, "Proj")
	if res.Root != wantRoot {
		t.Errorf("root=%s, want %s（应收敛到公共顶层目录）", res.Root, wantRoot)
	}
	got, err := os.ReadFile(filepath.Join(res.Root, "Data", "a.osgb"))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(got) != "AAA" {
		t.Errorf("content=%q, want AAA", got)
	}
}

// TestUploadFilesRejectsEscape 路径逃逸必须被拒绝，且不落盘。
func TestUploadFilesRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{DataDir: dir}, nil)

	body, ctype := uploadRequest(t, []string{"../evil.txt"}, map[string]string{"../evil.txt": "x"}, []string{"../evil.txt"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", body)
	req.Header.Set("Content-Type", ctype)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); err == nil {
		t.Fatalf("escape path was written outside data dir")
	}
}

// TestUploadFilesNoFiles 没有任何文件 part 时返回 400。
func TestUploadFilesNoFiles(t *testing.T) {
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{DataDir: t.TempDir()}, nil)
	body, ctype := uploadRequest(t, []string{"a.txt"}, nil, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", body)
	req.Header.Set("Content-Type", ctype)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", w.Code)
	}
}

// TestSafeRelPath 路径清洗表驱动。
func TestSafeRelPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"Proj/Data/a.osgb", filepath.FromSlash("Proj/Data/a.osgb"), false},
		{"./a.txt", "a.txt", false},
		{"/abs/a.txt", filepath.FromSlash("abs/a.txt"), false},
		{"../evil", "", true},
		{"a/../../evil", "", true},
		{"", "", true},
		{"C:/win.txt", "", true},
		{"a/b/../c.txt", filepath.FromSlash("a/c.txt"), false},
	}
	for _, c := range cases {
		got, err := safeRelPath(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("safeRelPath(%q) err=nil, want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("safeRelPath(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("safeRelPath(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestCommonRoot 公共顶层目录推导。
func TestCommonRoot(t *testing.T) {
	base := "/data/uploads/u1"
	cases := []struct {
		rels []string
		want string
	}{
		{[]string{"Proj/Data/a.osgb", "Proj/Data/b.osgb"}, filepath.Join(base, "Proj")},
		{[]string{"a.txt"}, base},              // 单层：无顶层目录可收敛
		{[]string{"A/a.txt", "B/b.txt"}, base}, // 顶层不同
		{[]string{"Proj/a.txt", "Proj/sub/b.txt"}, filepath.Join(base, "Proj")},
	}
	for _, c := range cases {
		if got := commonRoot(base, c.rels); got != c.want {
			t.Errorf("commonRoot(%v)=%q, want %q", c.rels, got, c.want)
		}
	}
}

// TestGetSystemInfo 系统信息接口回显装配侧填入的运行时信息与能力开关。
func TestGetSystemInfo(t *testing.T) {
	rt := RuntimeInfo{
		Mode: "desktop", Version: "test", Queue: "local", Storage: "localfs",
		Cache: "memory", Workers: 2, AuthEnabled: true, LicensePlan: "community",
		Capabilities: map[string]bool{"wms": false, "wmts": true},
	}
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{DataDir: "/tmp/tangis-data", Runtime: rt}, nil)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/system", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var got RuntimeInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != "desktop" || got.Workers != 2 || got.DataDir != "/tmp/tangis-data" {
		t.Errorf("unexpected runtime info: %+v", got)
	}
	if got.Capabilities["wms"] || !got.Capabilities["wmts"] {
		t.Errorf("capabilities not echoed: %+v", got.Capabilities)
	}
}

// TestSystemInfoDataDirFallback DataDir 为空时回落到 dataDir()（默认 "data"）。
func TestSystemInfoDataDirFallback(t *testing.T) {
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{}, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/system", nil))
	if !strings.Contains(w.Body.String(), `"data_dir":"data"`) {
		t.Fatalf("body=%s, want data_dir fallback to \"data\"", w.Body.String())
	}
}
