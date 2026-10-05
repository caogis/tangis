package worker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// qcFakeExecutor 按子命令分流：build 用 buildErr，qc 用 qcErr，
// qc 调用时先执行 onQC 副作用（如落一份报告文件，模拟真实内核行为）。
type qcFakeExecutor struct {
	calls    [][2]interface{} // [name, args]
	buildErr error
	qcErr    error
	onQC     func(args []string)
}

func (f *qcFakeExecutor) Run(name string, args []string) (string, error) {
	f.calls = append(f.calls, [2]interface{}{name, args})
	if len(args) > 0 && args[0] == "qc" {
		if f.onQC != nil {
			f.onQC(args)
		}
		return "qc done", f.qcErr
	}
	return "build done", f.buildErr
}

// writeQCReport 生成一份内核 qc 报告 JSON（对齐 docs/qc-report-sample schema）。
func writeQCReport(path string, passed bool) error {
	report := map[string]any{
		"schema_version":    "1",
		"generated_at_unix": 1791082984.7,
		"source":            "/data/osgb/town",
		"tile_count":        80,
		"tiles":             []any{},
		"cracks":            []any{},
		"summary": map[string]any{
			"tiles_with_degenerate":     0,
			"total_degenerate":          2,
			"tiles_with_normal_flip":    0,
			"total_flipped_edges":       3,
			"tiles_with_floating":       0,
			"total_floating_components": 4,
			"crack_pairs_checked":       0,
			"crack_pairs_flagged":       0,
			"total_crack_segments":      5,
			"tiles_with_self_intersect": 0,
			"total_self_intersections":  6,
			"passed":                    passed,
		},
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// newQCMsg 带源目录与 qc 参数的任务消息。
// 源目录落在临时目录内并真实创建（worker 对非目录源会记 skipped）。
func newQCMsg(dir string) queue.TaskMessage {
	msg := newTestMsg(dir)
	msg.Source = filepath.Join(dir, "src")
	if err := os.MkdirAll(msg.Source, 0o755); err != nil {
		panic(err)
	}
	msg.Params = map[string]any{"qc": true}
	return msg
}

// newPendingTaskWithParams 落库带 params 的 PENDING 任务
// （真实链路中 CreateTask 会把 params 存入任务记录）。
func newPendingTaskWithParams(msg queue.TaskMessage) *task.Task {
	tk := newPendingTask(msg)
	tk.Params = msg.Params
	return tk
}

// TestHandleQCPass qc 报告 PASS：任务 SUCCEEDED，QcStatus=pass，摘要正确解析。
func TestHandleQCPass(t *testing.T) {
	dir := t.TempDir()
	msg := newQCMsg(dir)
	// 源目录必须真实存在（worker 对非目录源会记 skipped）
	if err := os.MkdirAll(msg.Source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(msg.Output, 0o755); err != nil {
		t.Fatal(err)
	}

	store := task.NewMemoryStore()
	if err := store.Create(newPendingTaskWithParams(msg)); err != nil {
		t.Fatal(err)
	}

	fe := &qcFakeExecutor{
		onQC: func(args []string) {
			// 断言 qc 参数后落报告（模拟真实内核行为）
			report := args[len(args)-1]
			if err := writeQCReport(report, true); err != nil {
				t.Errorf("write qc report: %v", err)
			}
		},
	}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 两次 exec：先 build 后 qc
	if len(fe.calls) != 2 {
		t.Fatalf("exec calls = %d, want 2 (build + qc)", len(fe.calls))
	}
	for i, sub := range []string{"build", "qc"} {
		args := fe.calls[i][1].([]string)
		if args[0] != sub {
			t.Fatalf("call %d argv = %v, want subcommand %q", i, args, sub)
		}
	}
	qcName := fe.calls[1][0].(string)
	if qcName != "/fake/kernel" {
		t.Fatalf("qc kernel bin = %q, want /fake/kernel", qcName)
	}
	qcArgs := fe.calls[1][1].([]string)
	want := []string{"qc", "--source", msg.Source, "--report", filepath.Join(msg.Output, task.QcReportFile)}
	if strings.Join(qcArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("qc argv = %v, want %v", qcArgs, want)
	}

	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusSucceeded {
		t.Fatalf("status = %q, want SUCCEEDED", got.Status)
	}
	if got.QcStatus != task.QcPass {
		t.Fatalf("qc_status = %q, want %q", got.QcStatus, task.QcPass)
	}
	wantSummary := task.QcSummary{
		TileCount: 80, TotalDegenerate: 2, TotalFlippedEdges: 3,
		TotalFloating: 4, TotalCrackSegments: 5, TotalSelfIntersections: 6,
	}
	if got.QcSummary == nil || *got.QcSummary != wantSummary {
		t.Fatalf("qc_summary = %+v, want %+v", got.QcSummary, &wantSummary)
	}
	// 报告文件确实落在产物目录（上传/下载端点的数据来源）
	if _, err := os.Stat(filepath.Join(msg.Output, task.QcReportFile)); err != nil {
		t.Fatalf("qc report not written to output: %v", err)
	}
	if got.ErrMsg != "" {
		t.Fatalf("pass task should have empty error, got %q", got.ErrMsg)
	}
}

// TestHandleQCReportFail 报告 FAIL（几何质量问题）：任务仍 SUCCEEDED，QcStatus=fail。
func TestHandleQCReportFail(t *testing.T) {
	dir := t.TempDir()
	msg := newQCMsg(dir)
	if err := os.MkdirAll(msg.Source, 0o755); err != nil {
		t.Fatal(err)
	}
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTaskWithParams(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &qcFakeExecutor{
		onQC: func(args []string) {
			if err := writeQCReport(args[len(args)-1], false); err != nil {
				t.Errorf("write qc report: %v", err)
			}
		},
	}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusSucceeded {
		t.Fatalf("report FAIL is a data-quality verdict, not a task failure; status = %q", got.Status)
	}
	if got.QcStatus != task.QcFail {
		t.Fatalf("qc_status = %q, want %q", got.QcStatus, task.QcFail)
	}
}

// TestHandleQCKernelError 内核 qc 报错：任务置 FAILED 带原因（不静默）。
func TestHandleQCKernelError(t *testing.T) {
	dir := t.TempDir()
	msg := newQCMsg(dir)
	if err := os.MkdirAll(msg.Source, 0o755); err != nil {
		t.Fatal(err)
	}
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTaskWithParams(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &qcFakeExecutor{qcErr: errors.New("exit status 1")}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("task failure is terminal, Handle should ack: %v", err)
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusFailed {
		t.Fatalf("status = %q, want FAILED", got.Status)
	}
	if !strings.Contains(got.ErrMsg, "qc kernel exit") {
		t.Fatalf("error should mention qc kernel exit, got %q", got.ErrMsg)
	}
	if got.QcStatus == task.QcPass {
		t.Fatal("qc failed must not be recorded as pass")
	}
}

// TestHandleQCUnreadableReport qc 成功但报告缺失/不可解析：置 FAILED（不伪造状态）。
func TestHandleQCUnreadableReport(t *testing.T) {
	dir := t.TempDir()
	msg := newQCMsg(dir)
	if err := os.MkdirAll(msg.Source, 0o755); err != nil {
		t.Fatal(err)
	}
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTaskWithParams(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &qcFakeExecutor{} // 不写报告文件
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusFailed {
		t.Fatalf("unreadable report must fail the task, status = %q", got.Status)
	}
	if !strings.Contains(got.ErrMsg, "qc report unreadable") {
		t.Fatalf("error should mention unreadable report, got %q", got.ErrMsg)
	}
}

// TestHandleQCDisabled 未请求质检：不执行 qc，QcStatus 保持空（未检）。
func TestHandleQCDisabled(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir) // 无 params.qc
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTaskWithParams(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &qcFakeExecutor{}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1 (build only)", len(fe.calls))
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.QcStatus != "" || got.QcSummary != nil {
		t.Fatalf("no-qc task should keep qc fields empty, got %q %+v", got.QcStatus, got.QcSummary)
	}
}

// TestHandleQCSkippedForImageTask 影像任务请求 qc：源不适用，记 skipped 不报错。
func TestHandleQCSkippedForImageTask(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	msg.Type = "image->tiles"
	msg.Source = "/data/img/town.tif"
	msg.Params = map[string]any{"qc": true}
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTaskWithParams(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &qcFakeExecutor{}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// build（raster2tiles）+ 无 qc 调用
	if len(fe.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1 (no qc for image task)", len(fe.calls))
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.QcStatus != task.QcSkipped {
		t.Fatalf("qc_status = %q, want %q", got.QcStatus, task.QcSkipped)
	}
}

// TestParseQCReportMalformed 非法报告 JSON 解析报错。
func TestParseQCReportMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseQCReport(p); err == nil {
		t.Fatal("malformed report should error")
	}
	if _, err := parseQCReport(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing report should error")
	}
}

// TestQCEnabled qc 参数判定。
func TestQCEnabled(t *testing.T) {
	if qcEnabled(map[string]any{"qc": true}) != true {
		t.Fatal("qc=true should be enabled")
	}
	for _, p := range []map[string]any{
		{"qc": false},
		{"qc": "true"},
		{},
		nil,
	} {
		if qcEnabled(p) {
			t.Fatalf("params %v should not enable qc", p)
		}
	}
}
