// 编辑任务 worker 全链路测试（M2-F09c）：mock exec 校验 argv 拼装、
// ops.json 解析回写、失败重试沿用既有退避、留痕缺失不伪造成功。
package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// newEditMsg 构造编辑任务消息：source 为源任务产物目录（tile 源），
// output 为编辑任务独立产物目录。
func newEditMsg(dir string) queue.TaskMessage {
	return queue.TaskMessage{
		TaskID: "e1",
		Type:   task.TypeEdit,
		Source: filepath.Join(dir, "src-tiles"),
		Output: filepath.Join(dir, "edit-out"),
		Params: map[string]any{
			"edit_op":   "clip",
			"edit_bbox": "0,0,10,10",
		},
	}
}

func newPendingEditTask(msg queue.TaskMessage) *task.Task {
	return &task.Task{
		ID:           msg.TaskID,
		Type:         task.TypeEdit,
		Source:       msg.Source,
		Output:       msg.Output,
		Status:       task.StatusPending,
		Params:       msg.Params,
		ParentTaskID: "src-t1",
	}
}

// writeOpsReport 预置内核成功后应产出的 ops.json 留痕。
func writeOpsReport(t *testing.T, output string, content string) {
	t.Helper()
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, task.OpsReportFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildEditArgvClipWithBBox(t *testing.T) {
	params := EditParamsFromReq("clip", "", "0,0,10,10", "", nil, nil)
	argv, err := buildEditArgv(params, "/data/src", "/data/out")
	if err != nil {
		t.Fatalf("buildEditArgv: %v", err)
	}
	want := []string{
		"edit", "clip",
		"--source", "/data/src",
		"--tile", "all",
		"--out", "/data/out",
		"--format", "b3dm",
		"--bbox", "0,0,10,10",
		"--report", "/data/out/ops.json",
	}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
}

func TestBuildEditArgvFlattenFull(t *testing.T) {
	elev, feather := 12.5, 2.0
	params := EditParamsFromReq("flatten", "tile-007", "1,2,3,4", "", &elev, &feather)
	argv, err := buildEditArgv(params, "/data/src", "/data/out")
	if err != nil {
		t.Fatalf("buildEditArgv: %v", err)
	}
	want := []string{
		"edit", "flatten",
		"--source", "/data/src",
		"--tile", "tile-007",
		"--out", "/data/out",
		"--format", "b3dm",
		"--bbox", "1,2,3,4",
		"--elevation", "12.5",
		"--feather", "2",
		"--report", "/data/out/ops.json",
	}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
}

func TestBuildEditArgvPlaneAndInvalid(t *testing.T) {
	params := EditParamsFromReq("clip", "", "", "-1.5x+0y+2z=10", nil, nil)
	argv, err := buildEditArgv(params, "/data/src", "/data/out")
	if err != nil {
		t.Fatalf("buildEditArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--plane -1.5x+0y+2z=10") {
		t.Fatalf("argv missing plane: %v", argv)
	}
	// 非法 op / 缺目录：worker 侧兜底拒绝
	if _, err := buildEditArgv(map[string]any{"edit_op": "explode"}, "/s", "/o"); err == nil {
		t.Fatal("invalid op should error")
	}
	if _, err := buildEditArgv(map[string]any{"edit_op": "clip"}, "", "/o"); err == nil {
		t.Fatal("empty source should error")
	}
}

func TestHandleEditSuccessParsesOpsReport(t *testing.T) {
	dir := t.TempDir()
	msg := newEditMsg(dir)
	writeOpsReport(t, msg.Output, `{"op":"clip","totals":{"tiles":3,"removed":1}}`)

	store := task.NewMemoryStore()
	if err := store.Create(newPendingEditTask(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExecutor{}
	fu := &fakeUploader{}
	w := newTestWorker(t, store, "/fake/kernel", fe, fu)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// argv 按契约拼装且不含 manifest 相关参数
	if len(fe.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(fe.calls))
	}
	args := fe.calls[0][1].([]string)
	if args[0] != "edit" || args[1] != "clip" {
		t.Fatalf("argv head = %v, want edit clip", args)
	}

	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusSucceeded {
		t.Fatalf("status = %q, want SUCCEEDED", got.Status)
	}
	// ops.json 摘要如实回写（不臆造字段）
	if got.OpsSummary == nil || got.OpsSummary["op"] != "clip" {
		t.Fatalf("ops_summary = %+v", got.OpsSummary)
	}
	totals, _ := got.OpsSummary["totals"].(map[string]any)
	if totals == nil || totals["tiles"] != float64(3) {
		t.Fatalf("ops totals = %+v", got.OpsSummary["totals"])
	}
	// 产物上传复用既有链路
	if len(fu.calls) != 1 || fu.calls[0][1] != "tasks/"+msg.TaskID {
		t.Fatalf("upload calls = %+v", fu.calls)
	}
}

func TestHandleEditKernelFailureRetriesWithBackoff(t *testing.T) {
	dir := t.TempDir()
	msg := newEditMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingEditTask(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExecutor{err: errors.New("exit status 1")}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)
	pub := &retryPublisher{}
	w.Publisher = pub
	w.RetryBackoff = []time.Duration{0, 0, 0}

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle should ack on retryable failure: %v", err)
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusPending {
		t.Fatalf("status = %q, want PENDING (awaiting retry)", got.Status)
	}
	if len(pub.published) != 1 || pub.published[0].TaskID != msg.TaskID {
		t.Fatalf("re-publish = %+v, want 1 message", pub.published)
	}
	// 编辑执行日志已落盘（含 [edit] 前缀）
	data, err := os.ReadFile(w.LogPath(msg.TaskID))
	if err != nil || !strings.Contains(string(data), "[edit]") {
		t.Fatalf("edit log missing: %v", err)
	}
}

func TestHandleEditMissingOpsReportFails(t *testing.T) {
	dir := t.TempDir()
	msg := newEditMsg(dir)
	// 内核"成功"但未产出 ops.json：宁可失败也不伪造留痕
	store := task.NewMemoryStore()
	if err := store.Create(newPendingEditTask(msg)); err != nil {
		t.Fatal(err)
	}
	w := newTestWorker(t, store, "/fake/kernel", &fakeExecutor{}, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusFailed {
		t.Fatalf("status = %q, want FAILED (ops report missing)", got.Status)
	}
	if !strings.Contains(got.ErrMsg, "ops report") {
		t.Fatalf("error msg = %q", got.ErrMsg)
	}
}

func TestHandleEditInvalidParamsFails(t *testing.T) {
	dir := t.TempDir()
	msg := newEditMsg(dir)
	msg.Params = map[string]any{"edit_op": "explode"}
	store := task.NewMemoryStore()
	if err := store.Create(newPendingEditTask(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExecutor{}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// 参数非法不应发给内核
	if len(fe.calls) != 0 {
		t.Fatalf("invalid params should not exec kernel, got %d calls", len(fe.calls))
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusFailed || !strings.Contains(got.ErrMsg, "invalid edit params") {
		t.Fatalf("status = %q err = %q", got.Status, got.ErrMsg)
	}
}

func TestParseOpsReportRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, task.OpsReportFile)
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseOpsReport(path); err == nil {
		t.Fatal("garbage ops report should error")
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseOpsReport(path); err == nil {
		t.Fatal("empty ops report should error")
	}
}
