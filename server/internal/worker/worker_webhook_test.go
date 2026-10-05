package worker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"tangis/server/internal/task"
	"tangis/server/internal/webhook"
)

// waitFor 轮询等待条件成立（webhook 为异步 goroutine，需等待投递完成）。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestWorkerNotifiesWebhookOnSuccess(t *testing.T) {
	var calls int32
	var gotBody []byte
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Tangis-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	tk := newPendingTask(msg)
	tk.Params = map[string]any{"webhook_url": srv.URL}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	w := newTestWorker(t, store, "", &fakeExecutor{}, nil)
	w.Notifier = &webhook.Notifier{Secret: "whsec", Backoff: []time.Duration{time.Millisecond}}

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&calls) == 1 })

	var ev webhook.Event
	if err := json.Unmarshal(gotBody, &ev); err != nil {
		t.Fatalf("event not json: %v", err)
	}
	if ev.TaskID != msg.TaskID || ev.Status != string(task.StatusSucceeded) || ev.Error != "" {
		t.Fatalf("event = %+v", ev)
	}
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write(gotBody)
	if gotSig != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature mismatch: %q", gotSig)
	}
}

func TestWorkerNotifiesWebhookOnFailure(t *testing.T) {
	var calls int32
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	tk := newPendingTask(msg)
	tk.Params = map[string]any{"webhook_url": srv.URL}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	w := newTestWorker(t, store, "/fake/kernel",
		&fakeExecutor{err: os.ErrPermission}, nil)
	w.RetryBackoff = nil // Publisher 为 nil，直接终态 FAILED
	w.Notifier = &webhook.Notifier{Backoff: []time.Duration{time.Millisecond}}

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := store.Get(msg.TaskID, "")
	if got.Status != task.StatusFailed {
		t.Fatalf("status = %q, want FAILED", got.Status)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&calls) == 1 })

	var ev webhook.Event
	if err := json.Unmarshal(gotBody, &ev); err != nil {
		t.Fatalf("event not json: %v", err)
	}
	if ev.Status != string(task.StatusFailed) || ev.Error == "" {
		t.Fatalf("event = %+v, want FAILED with error", ev)
	}
}

func TestWorkerSkipsWebhookWithoutURL(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}
	// Notifier 可用但任务无 webhook_url → 不发送
	w := newTestWorker(t, store, "", &fakeExecutor{}, nil)
	w.Notifier = &webhook.Notifier{Backoff: []time.Duration{time.Millisecond}}
	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("webhook should not fire without params.webhook_url, got %d calls", calls)
	}
}
