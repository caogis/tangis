package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/license"
	"tangis/server/internal/task"
)

// TestCreateTaskDistributedRequiresLicense 商业特性门禁（F-15）：
// params.distributed=true 请求分布式切片，开源版（License 为 nil）必须 403，
// 且任务不落库；普通任务不受影响。
func TestCreateTaskDistributedRequiresLicense(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewRouter(task.NewMemoryStore(), nil, nil, AuthOptions{License: license.OpenSource()}, nil)

	body := `{"type":"osgb->3dtiles","source":"s3://in","output":"s3://out","params":{"distributed":true}}`
	w := doReq(r, "POST", "/api/v1/tasks", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("distributed task on open-source plan: got %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), license.FeatureDistributed) {
		t.Fatalf("error should name the feature: %s", w.Body.String())
	}

	// 不带 distributed 参数的普通任务照常创建（NATS 降级仅附 warning）
	w = doReq(r, "POST", "/api/v1/tasks", `{"type":"osgb->3dtiles","source":"s3://in","output":"s3://out"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("plain task should be created: got %d; body=%s", w.Code, w.Body.String())
	}
}
