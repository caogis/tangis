// quota_test.go — 每日任务创建配额全链路（M2-F14）：多 Key 校验、429 语义
// （Retry-After）、admin 不限额、disabled key 403、跨日重置（时间注入）。
package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
	"tangis/server/internal/task"
)

// newQuotaRouter 组装多 Key + 配额的测试路由：
// admin key 不限额；tenant key（acme）每日限 2；disabled key 禁用。
// now 非 nil 时注入配额时钟（跨日重置测试）。
func newQuotaRouter(t *testing.T, now *time.Time) (*gin.Engine, *auth.Quota) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	keys := auth.NewMemoryKeyStore("admin-key")
	keys.Add("acme-key", &auth.APIKey{
		ID: "k-acme", Name: "acme", TenantID: "acme", Role: auth.RoleTenant, DailyTaskLimit: 2,
	})
	keys.Add("nolimit-key", &auth.APIKey{
		ID: "k-nolimit", Name: "nolimit", TenantID: "acme", Role: auth.RoleTenant, DailyTaskLimit: 0,
	})
	keys.Add("off-key", &auth.APIKey{
		ID: "k-off", Name: "off", TenantID: "acme", Role: auth.RoleTenant, Disabled: true,
	})
	usage := auth.NewMemoryUsage()
	quota := &auth.Quota{Store: usage}
	if now != nil {
		quota.Now = func() time.Time { return *now }
	}
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{
		Enabled: true,
		Keys:    keys,
		Quota:   quota,
	}, nil)
	return r, quota
}

func quotaPost(t *testing.T, r *gin.Engine, key string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks",
		strings.NewReader(`{"type":"osgb->3dtiles","source":"/data/x","output":"/data/out"}`))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	r.ServeHTTP(w, req)
	return w
}

// TestQuota429AndRetryAfter 限额 2：前两次 201，第三次 429 + Retry-After。
func TestQuota429AndRetryAfter(t *testing.T) {
	r, _ := newQuotaRouter(t, nil)

	for i := 1; i <= 2; i++ {
		w := quotaPost(t, r, "acme-key")
		if w.Code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	w := quotaPost(t, r, "acme-key")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("create 3: %d %s (want 429)", w.Code, w.Body.String())
	}
	ra := w.Header().Get("Retry-After")
	if n, err := strconv.Atoi(ra); err != nil || n <= 0 || n > 86400+1 {
		t.Fatalf("Retry-After = %q (want seconds to next UTC midnight)", ra)
	}
	if !strings.Contains(w.Body.String(), "quota") {
		t.Fatalf("429 body = %s", w.Body.String())
	}
	// 读接口不受配额影响
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil))
	w.Header().Set("X-API-Key", "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	req.Header.Set("X-API-Key", "acme-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list tasks under quota: %d %s", w.Code, w.Body.String())
	}
}

// TestQuotaAdminAndUnlimited admin 与 daily_task_limit=0 的 key 不限额。
func TestQuotaAdminAndUnlimited(t *testing.T) {
	r, _ := newQuotaRouter(t, nil)
	for i := 0; i < 4; i++ {
		if w := quotaPost(t, r, "admin-key"); w.Code != http.StatusCreated {
			t.Fatalf("admin create %d: %d %s", i, w.Code, w.Body.String())
		}
		if w := quotaPost(t, r, "nolimit-key"); w.Code != http.StatusCreated {
			t.Fatalf("nolimit create %d: %d %s", i, w.Code, w.Body.String())
		}
	}
}

// TestQuotaDisabledKey disabled key → 403（区分于无效 key 的 401）。
func TestQuotaDisabledKey(t *testing.T) {
	r, _ := newQuotaRouter(t, nil)
	w := quotaPost(t, r, "off-key")
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled key: %d %s (want 403)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("body = %s", w.Body.String())
	}
	// 无效 key → 401
	w = quotaPost(t, r, "wrong-key")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key: %d (want 401)", w.Code)
	}
}

// TestQuotaCrossDayReset 跨日重置：day1 用满 → 推进时钟到次日 → 放行。
func TestQuotaCrossDayReset(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	r, _ := newQuotaRouter(t, &now)

	for i := 0; i < 2; i++ {
		if w := quotaPost(t, r, "acme-key"); w.Code != http.StatusCreated {
			t.Fatalf("day1 create %d: %d", i, w.Code)
		}
	}
	if w := quotaPost(t, r, "acme-key"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("day1 third: %d (want 429)", w.Code)
	}
	now = now.Add(14 * time.Hour) // → 2026-10-05T00:00Z
	if w := quotaPost(t, r, "acme-key"); w.Code != http.StatusCreated {
		t.Fatalf("day2 create after reset: %d %s", w.Code, w.Body.String())
	}
}
