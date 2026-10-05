// 编辑任务 API 测试（M2-F09c）：参数互斥校验（非法 400）、任务创建与入队、
// parent 链查询、ops-report 下载、审批/状态前置条件。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tangis/server/internal/task"
)

// seedSourceTask 落一个 SUCCEEDED+Approved 的 3D 切片源任务。
func seedSourceTask(t *testing.T, store task.Store, id, typ string, approved bool, status task.Status) *task.Task {
	t.Helper()
	src := &task.Task{
		ID: id, Type: typ, Source: "/data/osgb/town",
		Output: filepath.Join(t.TempDir(), "town-out"),
		Status: status, Approved: approved, TenantID: "default",
	}
	if err := store.Create(src); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestCreateEditTaskSuccess(t *testing.T) {
	store := task.NewMemoryStore()
	seedSourceTask(t, store, "src-1", "osgb->3dtiles", true, task.StatusSucceeded)
	pub := &fakePublisher{}
	r := NewRouter(store, pub, nil, AuthOptions{DataDir: t.TempDir()}, nil)

	body := `{"op":"clip","bbox":"0,0,10,10","tile":"tile-3"}`
	w := doReq(r, http.MethodPost, "/api/v1/tasks/src-1/edit", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create edit status = %d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Task struct {
			ID           string         `json:"id"`
			Type         string         `json:"type"`
			Source       string         `json:"source"`
			Output       string         `json:"output"`
			ParentTaskID string         `json:"parent_task_id"`
			Params       map[string]any `json:"params"`
		} `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	got := resp.Task
	if got.Type != task.TypeEdit || got.ParentTaskID != "src-1" {
		t.Fatalf("type/parent = %q/%q", got.Type, got.ParentTaskID)
	}
	// 源 = 原任务产物目录；产物落独立目录（不污染源任务）
	if got.Source == "" || got.Output == "" || got.Source == got.Output {
		t.Fatalf("source/output = %q/%q", got.Source, got.Output)
	}
	// params 存完整编辑参数 + 源任务 id
	if got.Params["edit_op"] != "clip" || got.Params["edit_bbox"] != "0,0,10,10" {
		t.Fatalf("edit params = %+v", got.Params)
	}
	if got.Params["edit_tile"] != "tile-3" || got.Params["parent_task_id"] != "src-1" {
		t.Fatalf("edit params = %+v", got.Params)
	}
	// 已入队 worker
	if len(pub.published) != 1 || pub.published[0].Type != task.TypeEdit {
		t.Fatalf("published = %+v", pub.published)
	}
}

func TestCreateEditTaskValidation(t *testing.T) {
	store := task.NewMemoryStore()
	seedSourceTask(t, store, "src-1", "osgb->3dtiles", true, task.StatusSucceeded)
	seedSourceTask(t, store, "src-pending", "osgb->3dtiles", true, task.StatusRunning)
	seedSourceTask(t, store, "src-noapprove", "osgb->3dtiles", false, task.StatusSucceeded)
	seedSourceTask(t, store, "src-image", "image->tiles", true, task.StatusSucceeded)
	r := NewRouter(store, &fakePublisher{}, nil, AuthOptions{DataDir: t.TempDir()}, nil)

	cases := []struct {
		name    string
		taskID  string
		body    string
		status  int
		wantSub string
	}{
		{"未知任务", "missing", `{"op":"clip","bbox":"0,0,1,1"}`, http.StatusNotFound, ""},
		{"未终态", "src-pending", `{"op":"clip","bbox":"0,0,1,1"}`, http.StatusConflict, "not finished"},
		{"未审批", "src-noapprove", `{"op":"clip","bbox":"0,0,1,1"}`, http.StatusForbidden, "not approved"},
		{"影像任务不可编辑", "src-image", `{"op":"clip","bbox":"0,0,1,1"}`, http.StatusBadRequest, "not an editable 3D tiles"},
		{"非法 op", "src-1", `{"op":"explode","bbox":"0,0,1,1"}`, http.StatusBadRequest, "op must be"},
		{"clip 缺 plane/bbox", "src-1", `{"op":"clip"}`, http.StatusBadRequest, "requires plane or bbox"},
		{"clip plane/bbox 互斥", "src-1", `{"op":"clip","bbox":"0,0,1,1","plane":"0x+0y+1z=5"}`, http.StatusBadRequest, "not both"},
		{"flatten 缺 bbox", "src-1", `{"op":"flatten","elevation":1}`, http.StatusBadRequest, "requires bbox"},
		{"flatten 缺 elevation", "src-1", `{"op":"flatten","bbox":"0,0,1,1"}`, http.StatusBadRequest, "requires elevation"},
		{"flatten 带 plane", "src-1", `{"op":"flatten","bbox":"0,0,1,1","elevation":1,"plane":"0x+0y+1z=5"}`, http.StatusBadRequest, "does not accept plane"},
		{"ground-align 同规则", "src-1", `{"op":"ground-align","elevation":1}`, http.StatusBadRequest, "requires bbox"},
		{"bbox 项数不足", "src-1", `{"op":"clip","bbox":"0,0,1"}`, http.StatusBadRequest, "bbox must be"},
		{"bbox 非数字", "src-1", `{"op":"clip","bbox":"a,0,1,1"}`, http.StatusBadRequest, "not a number"},
		{"bbox min>=max", "src-1", `{"op":"clip","bbox":"5,0,1,1"}`, http.StatusBadRequest, "minx<maxx"},
		{"plane 非法格式", "src-1", `{"op":"clip","plane":"x+y=z"}`, http.StatusBadRequest, "plane must be"},
		{"feather 负数", "src-1", `{"op":"clip","bbox":"0,0,1,1","feather":-1}`, http.StatusBadRequest, "feather"},
		{"tile 非法字符", "src-1", `{"op":"clip","bbox":"0,0,1,1","tile":"../etc"}`, http.StatusBadRequest, "tile must be"},
	}
	for _, tc := range cases {
		w := doReq(r, http.MethodPost, "/api/v1/tasks/"+tc.taskID+"/edit", tc.body)
		if w.Code != tc.status {
			t.Fatalf("%s: status = %d, want %d, body=%s", tc.name, w.Code, tc.status, w.Body.String())
		}
		if tc.wantSub != "" {
			var resp map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if msg, _ := resp["error"].(string); !strings.Contains(msg, tc.wantSub) {
				t.Fatalf("%s: error = %q, want contains %q", tc.name, msg, tc.wantSub)
			}
		}
	}
}

// TestCreateEditTaskPlaneNormalized 合法 plane/bbox 规范化后写入 params
// （前端拦截之外，服务端最终防线：非法拒绝、合法规整）。
func TestCreateEditTaskPlaneNormalized(t *testing.T) {
	store := task.NewMemoryStore()
	seedSourceTask(t, store, "src-1", "t3d-import", true, task.StatusSucceeded)
	r := NewRouter(store, &fakePublisher{}, nil, AuthOptions{DataDir: t.TempDir()}, nil)

	w := doReq(r, http.MethodPost, "/api/v1/tasks/src-1/edit",
		`{"op":"clip","plane":"-1.50x +0y +2z = 10.0"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Task struct {
			Params map[string]any `json:"params"`
		} `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got, _ := resp.Task.Params["edit_plane"].(string); got != "-1.5x+0y+2z=10" {
		t.Fatalf("edit_plane = %q, want normalized", got)
	}
}

// TestEditHistoryChain 同 parent 链查询：编辑的编辑上溯到链根，
// 返回链根下全部编辑任务（正序），不含其他任务。
func TestEditHistoryChain(t *testing.T) {
	store := task.NewMemoryStore()
	seedSourceTask(t, store, "t0", "osgb->3dtiles", true, task.StatusSucceeded)
	e1 := &task.Task{ID: "e1", Type: task.TypeEdit, Source: "x", Output: "o1",
		Status: task.StatusSucceeded, ParentTaskID: "t0", TenantID: "default"}
	e2 := &task.Task{ID: "e2", Type: task.TypeEdit, Source: "o1", Output: "o2",
		Status: task.StatusPending, ParentTaskID: "e1", TenantID: "default"}
	other := &task.Task{ID: "other", Type: task.TypeEdit, Source: "y", Output: "o3",
		Status: task.StatusPending, ParentTaskID: "another-root", TenantID: "default"}
	for _, tk := range []*task.Task{e1, e2, other} {
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRouter(store, nil, nil, AuthOptions{}, nil)

	for _, entry := range []string{"t0", "e1", "e2"} {
		w := doReq(r, http.MethodGet, "/api/v1/tasks/"+entry+"/edit-history", "")
		if w.Code != http.StatusOK {
			t.Fatalf("edit-history(%s) status = %d, body=%s", entry, w.Code, w.Body.String())
		}
		var resp struct {
			ParentTaskID string `json:"parent_task_id"`
			Tasks        []struct {
				ID           string         `json:"id"`
				ParentTaskID string         `json:"parent_task_id"`
				OpsSummary   map[string]any `json:"ops_summary"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.ParentTaskID != "t0" {
			t.Fatalf("entry %s: root = %q, want t0", entry, resp.ParentTaskID)
		}
		if len(resp.Tasks) != 2 || resp.Tasks[0].ID != "e1" || resp.Tasks[1].ID != "e2" {
			t.Fatalf("entry %s: chain = %+v", entry, resp.Tasks)
		}
	}
}

// TestEditHistoryMissing 任务不存在返回 404。
func TestEditHistoryMissing(t *testing.T) {
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{}, nil)
	w := doReq(r, http.MethodGet, "/api/v1/tasks/missing/edit-history", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestGetTaskOpsReport 留痕下载：缺失 404（不伪造），存在 200 + 原文一致。
func TestGetTaskOpsReport(t *testing.T) {
	store := task.NewMemoryStore()
	src := seedSourceTask(t, store, "e1", task.TypeEdit, true, task.StatusSucceeded)
	r := NewRouter(store, nil, nil, AuthOptions{}, nil)

	w := doReq(r, http.MethodGet, "/api/v1/tasks/e1/ops-report", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing report status = %d, want 404", w.Code)
	}

	want := []byte(`{"op":"clip","totals":{"tiles":3}}`)
	if err := os.MkdirAll(src.Output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src.Output, task.OpsReportFile), want, 0o644); err != nil {
		t.Fatal(err)
	}
	w = doReq(r, http.MethodGet, "/api/v1/tasks/e1/ops-report", "")
	if w.Code != http.StatusOK {
		t.Fatalf("report status = %d, want 200", w.Code)
	}
	if w.Body.String() != string(want) {
		t.Fatalf("body = %q, want %q", w.Body.String(), want)
	}
	if w.Header().Get("Content-Disposition") == "" {
		t.Fatal("missing Content-Disposition attachment header")
	}
}

// TestCreateEditTaskQueueDegraded NATS 不可用时建任务不失败（降级 PENDING）。
func TestCreateEditTaskQueueDegraded(t *testing.T) {
	store := task.NewMemoryStore()
	seedSourceTask(t, store, "src-1", "osgb->3dtiles", true, task.StatusSucceeded)
	r := NewRouter(store, nil, nil, AuthOptions{DataDir: t.TempDir()}, nil)

	w := doReq(r, http.MethodPost, "/api/v1/tasks/src-1/edit", `{"op":"clip","bbox":"0,0,1,1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Warning string `json:"warning"`
		Task    struct {
			Status string `json:"status"`
		} `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Warning == "" || resp.Task.Status != "PENDING" {
		t.Fatalf("degraded resp = %+v", resp)
	}
}
