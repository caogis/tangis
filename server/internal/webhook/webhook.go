// Package webhook 实现任务终态事件通知（PRD F-14 第一步，M2-F07 顺路）：
//
// 任务 SUCCEEDED/FAILED 时向任务参数 params.webhook_url 指定的地址 POST JSON
// 事件（task_id、status、时间、错误信息），并携带 HMAC-SHA256 签名头
// X-Tangis-Signature = hex(HMAC(secret, body))，密钥 env TANGIS_WEBHOOK_SECRET。
//
// 语义约定：失败重试 3 次（指数退避，默认 1s/2s/4s），全部失败仅记日志，
// 不阻塞、不影响任务状态（调用方在 goroutine 中触发）。
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Event 任务终态事件（POST body schema，见 docs/CONTAINER-FORMAT.md §6）。
type Event struct {
	TaskID    string    `json:"task_id"`
	Status    string    `json:"status"` // SUCCEEDED / FAILED
	Timestamp time.Time `json:"timestamp"`
	Error     string    `json:"error,omitempty"`
}

// Notifier webhook 发送器。零值不可用（Secret 为空仅表示不签名，仍可发送）。
type Notifier struct {
	// Secret HMAC 签名密钥；为空时不发送 X-Tangis-Signature 头。
	Secret string
	// Client HTTP 客户端，nil 时取默认（10s 单次超时）。
	Client *http.Client
	// Backoff 重试退避序列，长度即失败重试次数上限；空取 DefaultBackoff。
	Backoff []time.Duration
}

// DefaultBackoff 默认退避：失败后 1s/2s/4s 各重试一次（共 4 次请求）。
var DefaultBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// DefaultTimeout 单次请求超时。
const DefaultTimeout = 10 * time.Second

// backoff 重试退避序列（含默认值兜底）。
func (n *Notifier) backoff() []time.Duration {
	if len(n.Backoff) > 0 {
		return n.Backoff
	}
	return DefaultBackoff
}

// client HTTP 客户端（含默认值兜底）。
func (n *Notifier) client() *http.Client {
	if n.Client != nil {
		return n.Client
	}
	return &http.Client{Timeout: DefaultTimeout}
}

// Notify 同步发送事件：立即尝试一次，失败按退避序列重试。
// 返回最终 error（全部尝试失败时非 nil）；调用方负责记日志、异步化。
func (n *Notifier) Notify(url string, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("webhook: marshal event: %w", err)
	}
	backoff := n.backoff()
	attempts := len(backoff) + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(backoff[i-1])
		}
		lastErr = n.sendOnce(url, body)
		if lastErr == nil {
			if i > 0 {
				log.Printf("webhook: task %s delivered to %s after %d retries", ev.TaskID, url, i)
			}
			return nil
		}
		log.Printf("webhook: attempt %d/%d for task %s failed: %v", i+1, attempts, ev.TaskID, lastErr)
	}
	return lastErr
}

// sendOnce 单次 POST：2xx 视为成功，其余状态码与非 2xx 一律失败。
func (n *Notifier) sendOnce(url string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Secret != "" {
		mac := hmac.New(sha256.New, []byte(n.Secret))
		mac.Write(body)
		req.Header.Set("X-Tangis-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := n.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// VerifySignature 接收方校验辅助：body 的 HMAC-SHA256 hex 是否等于签名头。
// 使用恒定时间比较；sig 为空返回 false。
func VerifySignature(body []byte, sigHex, secret string) bool {
	if sigHex == "" || secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sigHex)
	if err != nil || len(got) != len(want) {
		return false
	}
	return hmac.Equal(got, want)
}
