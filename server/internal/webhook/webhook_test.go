package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotifySuccessAndSignature(t *testing.T) {
	var gotBody []byte
	var gotSig string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Tangis-Signature")
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Notifier{Secret: "s3cret"}
	ev := Event{TaskID: "t1", Status: "SUCCEEDED", Timestamp: time.Now()}
	if err := n.Notify(srv.URL, ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	// 签名 = HMAC-SHA256(secret, body) hex
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(gotBody)
	if gotSig != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature mismatch: %q", gotSig)
	}
	if !VerifySignature(gotBody, gotSig, "s3cret") {
		t.Fatal("VerifySignature should accept valid signature")
	}
	if VerifySignature(gotBody, gotSig, "wrong") {
		t.Fatal("VerifySignature should reject wrong secret")
	}
}

func TestNotifyRetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Notifier{Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	if err := n.Notify(srv.URL, Event{TaskID: "t1", Status: "FAILED"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (2 failures + 1 success)", calls)
	}
}

func TestNotifyExhaustsRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := &Notifier{Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	if err := n.Notify(srv.URL, Event{TaskID: "t1", Status: "FAILED"}); err == nil {
		t.Fatal("Notify should fail after exhausting retries")
	}
	if calls != 4 { // 首次 + 3 次重试
		t.Fatalf("calls = %d, want 4", calls)
	}
}

func TestNotifyWithoutSecretOmitsSignature(t *testing.T) {
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Tangis-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Notifier{}
	if err := n.Notify(srv.URL, Event{TaskID: "t1", Status: "SUCCEEDED"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotSig != "" {
		t.Fatalf("no secret should omit signature header, got %q", gotSig)
	}
}

func TestNotifyConnectionRefused(t *testing.T) {
	// 关闭的服务器 → 连接失败，重试耗尽返回错误
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	n := &Notifier{Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	if err := n.Notify(srv.URL, Event{TaskID: "t1", Status: "SUCCEEDED"}); err == nil {
		t.Fatal("Notify to dead server should fail")
	}
}
