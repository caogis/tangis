package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// retryPublisher 记录重新入队的消息（失败重试用）。
type retryPublisher struct {
	published []queue.TaskMessage
	failErr   error
}

func (p *retryPublisher) Publish(msg queue.TaskMessage) error {
	if p.failErr != nil {
		return p.failErr
	}
	p.published = append(p.published, msg)
	return nil
}

func (p *retryPublisher) Close() {}

// writeManifest 写入带指定分块状态的 manifest。
func writeManifest(t *testing.T, path string, statuses []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	type chunk struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	chunks := make([]chunk, len(statuses))
	for i, s := range statuses {
		chunks[i] = chunk{ID: fmt.Sprintf("c%d", i), Status: s}
	}
	data, _ := json.Marshal(map[string]any{"task_id": "t1", "chunks": chunks})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFailureRetriesWithBackoffThenPublishes(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{output: "boom", err: errors.New("exit status 1")}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)
	pub := &retryPublisher{}
	w.Publisher = pub
	w.RetryBackoff = []time.Duration{0, 0, 0} // 测试零退避

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle should ack on retryable failure: %v", err)
	}

	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusPending {
		t.Fatalf("status = %q, want PENDING (awaiting retry)", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.Attempts)
	}
	if !strings.Contains(got.ErrMsg, "retrying in 0s") {
		t.Fatalf("retry msg = %q, want mention of retry", got.ErrMsg)
	}
	if len(pub.published) != 1 || pub.published[0].TaskID != msg.TaskID {
		t.Fatalf("re-publish = %+v, want 1 message for %s", pub.published, msg.TaskID)
	}
	// 日志应已落盘（含本次失败输出）
	data, err := os.ReadFile(w.LogPath(msg.TaskID))
	if err != nil {
		t.Fatalf("log file: %v", err)
	}
	if !strings.Contains(string(data), "boom") || !strings.Contains(string(data), "attempt 1") {
		t.Fatalf("log content = %q", string(data))
	}
}

func TestFailureExceedsRetryLimitMarksFailed(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	tk := newPendingTask(msg)
	tk.Attempts = 3 // 已重试 3 次（上限）
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{err: errors.New("exit status 1")}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil)
	w.Publisher = &retryPublisher{}
	w.RetryBackoff = []time.Duration{0, 0, 0}

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusFailed {
		t.Fatalf("status = %q, want FAILED after retry limit", got.Status)
	}
	if got.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4", got.Attempts)
	}
	if !strings.Contains(got.ErrMsg, "exit status 1") {
		t.Fatalf("error msg = %q", got.ErrMsg)
	}
}

func TestNoPublisherMeansDirectFailure(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{err: errors.New("exit status 1")}
	w := newTestWorker(t, store, "/fake/kernel", fe, nil) // Publisher 未设置

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusFailed {
		t.Fatalf("status = %q, want FAILED (no publisher)", got.Status)
	}
}

func TestParseManifestProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	writeManifest(t, path, []string{"done", "done", "pending", "failed"})

	done, total, err := ParseManifestProgress(path)
	if err != nil {
		t.Fatalf("ParseManifestProgress: %v", err)
	}
	if done != 2 || total != 4 {
		t.Fatalf("progress = %d/%d, want 2/4", done, total)
	}

	// 文件不存在
	if _, _, err := ParseManifestProgress(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("missing manifest should error")
	}
}

func TestHandleParsesProgressFromManifest(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	// 预置带进度的 manifest（内核执行前部分分块已完成）
	if err := os.MkdirAll(msg.Output, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, msg.ManifestPath, []string{"done", "done", "done", "pending"})

	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}
	w := newTestWorker(t, store, "", &fakeExecutor{}, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Progress == nil {
		t.Fatal("progress should be parsed from manifest")
	}
	if got.Progress.Done != 3 || got.Progress.Total != 4 {
		t.Fatalf("progress = %+v, want 3/4", got.Progress)
	}
	if got.Status != task.StatusSucceeded {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestHandleRunningProgressUpdatesViaPoll(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	// 慢 executor：执行期间 manifest 被更新为部分完成，轮询协程应捕获
	fe := &slowExecutor{delay: 300 * time.Millisecond, manifest: msg.ManifestPath}
	w := newTestWorker(t, store, "", fe, nil)
	w.ProgressInterval = 50 * time.Millisecond

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Progress == nil || got.Progress.Done != 1 || got.Progress.Total != 2 {
		t.Fatalf("polled progress = %+v, want 1/2", got.Progress)
	}
}

// slowExecutor 执行期间把 manifest 更新为 1/2 done，模拟内核分块推进。
type slowExecutor struct {
	delay    time.Duration
	manifest string
}

func (s *slowExecutor) Run(name string, args []string) (string, error) {
	// 半程时更新 manifest
	go func() {
		time.Sleep(s.delay / 2)
		_ = os.WriteFile(s.manifest, []byte(`{"task_id":"t1","chunks":[{"id":"a","status":"done"},{"id":"b","status":"pending"}]}`), 0o644)
	}()
	time.Sleep(s.delay)
	return "ok", nil
}

func TestLogPathLayout(t *testing.T) {
	w := New(task.NewMemoryStore(), &fakeExecutor{}, "", "", nil)
	w.DataDir = "/tmp/tangis-data"
	want := filepath.Join("/tmp/tangis-data", "logs", "abc.log")
	if w.LogPath("abc") != want {
		t.Fatalf("LogPath = %q, want %q", w.LogPath("abc"), want)
	}
	// DataDir 空时默认 data 目录
	w2 := New(task.NewMemoryStore(), &fakeExecutor{}, "", "", nil)
	if !strings.HasSuffix(w2.LogPath("abc"), filepath.Join("data", "logs", "abc.log")) {
		t.Fatalf("default LogPath = %q", w2.LogPath("abc"))
	}
}
