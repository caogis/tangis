package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"tangis/server/internal/objectstore"
	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// fakeExecutor 记录调用参数，按预设返回结果。
type fakeExecutor struct {
	calls  [][2]interface{} // [name, args]
	output string
	err    error
}

func (f *fakeExecutor) Run(name string, args []string) (string, error) {
	f.calls = append(f.calls, [2]interface{}{name, args})
	return f.output, f.err
}

func newTestMsg(dir string) queue.TaskMessage {
	return queue.TaskMessage{
		TaskID:       "t1",
		Type:         "osgb->3dtiles",
		Source:       "/data/osgb/town",
		Output:       filepath.Join(dir, "town"),
		ManifestPath: filepath.Join(dir, "town", "manifest.json"),
	}
}

// newTestWorker 组装测试 Worker：日志落盘到独立临时目录，避免污染工作目录。
func newTestWorker(t *testing.T, store task.Store, kernelBin string, fe Executor, uploader objectstore.Uploader) *Worker {
	t.Helper()
	w := New(store, fe, kernelBin, "", uploader)
	w.DataDir = t.TempDir()
	return w
}

func newPendingTask(msg queue.TaskMessage) *task.Task {
	return &task.Task{
		ID:           msg.TaskID,
		Type:         msg.Type,
		Source:       msg.Source,
		Output:       msg.Output,
		Status:       task.StatusPending,
		ManifestPath: msg.ManifestPath,
	}
}

func TestHandleSuccess(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{output: "完成：共 2 个分块全部 Done"}
	w := newTestWorker(t, store, "", fe, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 默认 argv：build <manifest> --out <output 目录本身> --task-id <id>
	// B7：末尾注入 --cancel-file（协作式取消标志文件）
	if len(fe.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(fe.calls))
	}
	name := fe.calls[0][0].(string)
	args := fe.calls[0][1].([]string)
	if name != DefaultKernelBin {
		t.Fatalf("kernel bin = %q, want %q", name, DefaultKernelBin)
	}
	wantArgv := []string{"build", msg.ManifestPath, "--out", msg.Output, "--task-id", msg.TaskID,
		"--cancel-file", w.cancelFlagPath(msg.TaskID)}
	if len(args) != len(wantArgv) {
		t.Fatalf("default argv = %v, want %v", args, wantArgv)
	}
	for i := range wantArgv {
		if args[i] != wantArgv[i] {
			t.Fatalf("default argv = %v, want %v", args, wantArgv)
		}
	}

	got, err := store.Get(msg.TaskID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusSucceeded {
		t.Fatalf("status = %q, want SUCCEEDED", got.Status)
	}
	if got.ErrMsg != "" {
		t.Fatalf("success task should have empty error, got %q", got.ErrMsg)
	}
}

func TestHandleFailureMarksFailedWithError(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{output: "错误: 清单 JSON 解析失败", err: errors.New("exit status 1")}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)

	if err := w.Handle(msg); err != nil {
		// 任务失败是终态，Handle 不应把执行失败当暂时性错误 Nak
		t.Fatalf("Handle should ack (nil) on task failure, got: %v", err)
	}

	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusFailed {
		t.Fatalf("status = %q, want FAILED", got.Status)
	}
	if !strings.Contains(got.ErrMsg, "清单 JSON 解析失败") || !strings.Contains(got.ErrMsg, "exit status 1") {
		t.Fatalf("error msg should contain kernel output and exit error, got %q", got.ErrMsg)
	}
}

func TestKernelArgsOverrideWithPlaceholders(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{}
	kernelArgs := "build {manifest_path} --out {output_dir} --task-id {task_id}"
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)
	w.KernelArgs = splitArgs(kernelArgs)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	args := fe.calls[0][1].([]string)
	want := []string{
		"build", msg.ManifestPath,
		"--out", msg.Output,
		"--task-id", msg.TaskID,
	}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("KERNEL_ARGS argv = %v, want %v", args, want)
	}
}

func TestEnsureManifestCreatesSkeleton(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	// manifest 不存在，Output 目录也不存在
	store := task.NewMemoryStore()
	_ = store.Create(newPendingTask(msg))

	w := newTestWorker(t, store, "", &fakeExecutor{}, nil)
	path, err := w.ensureManifest(msg)
	if err != nil {
		t.Fatalf("ensureManifest: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("manifest not created: %v", err)
	}

	// 内容须与内核 ChunkManifest schema 对齐（task_id + chunks）
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		TaskID string `json:"task_id"`
		Chunks []struct {
			ID     string `json:"id"`
			Bounds struct {
				Min []float64 `json:"min"`
				Max []float64 `json:"max"`
			} `json:"bounds"`
			Status string `json:"status"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest not valid json: %v", err)
	}
	if m.TaskID != msg.TaskID {
		t.Fatalf("manifest task_id = %q, want %q", m.TaskID, msg.TaskID)
	}
	if len(m.Chunks) == 0 {
		t.Fatal("manifest has no chunks")
	}
	// bounds 为内核 Bounds{min,max}（[f64;3]）必填字段
	for _, c := range m.Chunks {
		if len(c.Bounds.Min) != 3 || len(c.Bounds.Max) != 3 {
			t.Fatalf("chunk %q bounds must be [f64;3] min/max, got %+v", c.ID, c.Bounds)
		}
	}

	// 已存在时不覆盖
	if err := os.WriteFile(path, []byte(`{"task_id":"custom","chunks":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path2, err := w.ensureManifest(msg)
	if err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(path2)
	if string(data2) != `{"task_id":"custom","chunks":[]}` {
		t.Fatal("existing manifest should not be overwritten")
	}
}

func TestHandleIdempotentForFinishedTasks(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	tk := newPendingTask(msg)
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	tk.Status = task.StatusSucceeded
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{}
	w := newTestWorker(t, store, "", fe, nil)
	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fe.calls) != 0 {
		t.Fatalf("finished task should not execute kernel, got %d calls", len(fe.calls))
	}
}

func TestHandleDropsDeletedTask(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	fe := &fakeExecutor{}
	w := newTestWorker(t, store, "", fe, nil)
	// 从未落库的任务：消息应被丢弃（Ack），不报错不执行
	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle for deleted task should return nil, got: %v", err)
	}
	if len(fe.calls) != 0 {
		t.Fatalf("deleted task should not execute kernel")
	}
}

func TestSplitArgs(t *testing.T) {
	got := splitArgs("build a b  --flag c")
	want := []string{"build", "a", "b", "--flag", "c"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("splitArgs = %v, want %v", got, want)
	}
	if splitArgs("") != nil {
		t.Fatal("empty KERNEL_ARGS should produce nil slice")
	}
}

// fakeUploader 记录上传调用，可注入失败模拟 MinIO 不可用。
type fakeUploader struct {
	calls  [][2]string // [localDir, prefix]
	failOn string      // 非 nil 时对匹配 prefix 的上传返回错误
	err    error
}

func (f *fakeUploader) UploadDir(_ context.Context, localDir, prefix string) error {
	f.calls = append(f.calls, [2]string{localDir, prefix})
	if f.err != nil && strings.Contains(prefix, f.failOn) {
		return f.err
	}
	return nil
}

func TestHandleUploadsArtifactsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	// 预置产物文件，模拟内核输出
	if err := os.MkdirAll(msg.Output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(msg.Output, "tileset.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}
	fu := &fakeUploader{}
	w := newTestWorker(t, store, "", &fakeExecutor{}, fu)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(fu.calls) != 1 {
		t.Fatalf("upload calls = %d, want 1", len(fu.calls))
	}
	if fu.calls[0][0] != msg.Output {
		t.Fatalf("upload localDir = %q, want output dir %q", fu.calls[0][0], msg.Output)
	}
	wantPrefix := path.Join("tasks", msg.TaskID)
	if fu.calls[0][1] != wantPrefix {
		t.Fatalf("upload prefix = %q, want %q", fu.calls[0][1], wantPrefix)
	}

	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusSucceeded {
		t.Fatalf("status = %q, want SUCCEEDED", got.Status)
	}
	if got.MinioPrefix != wantPrefix {
		t.Fatalf("minio_prefix = %q, want %q", got.MinioPrefix, wantPrefix)
	}
}

func TestHandleSucceedsEvenIfUploadFails(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	if err := os.MkdirAll(msg.Output, 0o755); err != nil {
		t.Fatal(err)
	}

	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}
	fu := &fakeUploader{failOn: "tasks/", err: errors.New("minio: connection refused")}
	w := newTestWorker(t, store, "", &fakeExecutor{}, fu)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusSucceeded {
		t.Fatalf("upload failure must not fail the task, status = %q", got.Status)
	}
	if got.MinioPrefix != "" {
		t.Fatalf("failed upload should leave minio_prefix empty, got %q", got.MinioPrefix)
	}
	if got.ErrMsg != "" {
		t.Fatalf("upload failure should not write task error, got %q", got.ErrMsg)
	}
}
