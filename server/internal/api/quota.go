// quota.go — 每日任务创建配额中间件与 handler 记账（M2-F14）。
// 语义见 internal/auth/quota.go：tenant key 且 daily_task_limit>0 时生效，
// 超限 429 + Retry-After（UTC 次日零点秒数）；TANGIS_AUTH=off / admin 不限额。
package api

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// taskQuotaMiddleware POST /tasks 与 POST /tasks/import 的前置配额检查。
// 依赖 apiKeyMiddleware 注入的 key_id / role / daily_task_limit；
// 鉴权关闭（auth_off）或 Quota 未装配时直通。
func taskQuotaMiddleware(h *Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.Quota == nil || isAuthOff(c) {
			c.Next()
			return
		}
		// admin 不限额（管理端不受租户配额约束）
		if c.GetString("role") == "admin" {
			c.Next()
			return
		}
		limit := c.GetInt("daily_task_limit")
		keyID := c.GetString("key_id")
		if keyID == "" || limit <= 0 {
			c.Next()
			return
		}
		ok, used, err := h.Quota.Check(c.Request.Context(), keyID, limit)
		if err != nil {
			// 记账后端故障不阻塞业务（fail-open），仅告警
			c.Next()
			return
		}
		if !ok {
			c.Header("Retry-After", strconv.Itoa(h.Quota.RetryAfterSeconds()))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":     "daily task quota exceeded",
				"limit":     limit,
				"used":      used,
				"resets_at": h.Quota.Day() + "T24:00:00Z (next UTC midnight)",
			})
			return
		}
		c.Next()
	}
}

// recordTaskQuota 任务落库成功后记账（CreateTask/ImportTask 调用）。
// 仅对受限 tenant key 记数；失败仅告警不影响响应。
func (h *Handler) recordTaskQuota(c *gin.Context) {
	if h.Quota == nil || isAuthOff(c) || c.GetString("role") == "admin" {
		return
	}
	keyID := c.GetString("key_id")
	if keyID == "" || c.GetInt("daily_task_limit") <= 0 {
		return
	}
	if err := h.Quota.Record(c.Request.Context(), keyID); err != nil {
		log.Printf("quota record failed for key %s: %v", keyID, err)
	}
}
