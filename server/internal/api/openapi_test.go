// openapi_test.go — OpenAPI 规范防漂移测试（M2-F14）：
// 硬编码路径清单，双向断言 docs/openapi.yaml 与 gin 路由表一致
// （改路由必须同步改 docs/openapi.yaml，否则本测试失败）。
package api

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// specPaths 期望 openapi.yaml 与 router 同时具备的路径清单（与
// internal/api/router.go 现状一一对应；gin :param → OpenAPI {param}，
// *path → {path}）。
var specPaths = []string{
	"/healthz",
	"/api/v1/tasks",
	"/api/v1/tasks/import",
	"/api/v1/tasks/{id}",
	"/api/v1/tasks/{id}/logs",
	"/api/v1/tasks/{id}/qc-report",
	"/api/v1/tasks/{id}/ops-report",
	"/api/v1/tasks/{id}/edit",
	"/api/v1/tasks/{id}/edit-history",
	"/api/v1/tasks/{id}/approve",
	"/api/v1/tasks/{id}/package",
	"/api/v1/services",
	"/services/{task_id}/{path}",
	"/api/v1/wmts",
	"/api/v1/wmts/1.0.0/{layer}/{style}/{tms}/{z}/{row}/{col}",
	"/api/v1/tms/{task_id}/{z}/{x}/{y}",
	"/api/v1/vector/sources",
	"/api/v1/vector/sources/{id}",
	"/api/v1/vector/layers",
	"/api/v1/vector/layers/{name}",
	"/api/v1/vector/layers/{name}/metadata",
	"/api/v1/vector/{layer}/{z}/{x}/{y}",
	"/api/v1/wfs",
}

// specPathRe 提取 openapi.yaml 顶层 path 键（两空格缩进的 "/..." 行）。
var specPathRe = regexp.MustCompile(`(?m)^  (/.+):\s*$`)

// openapiPaths 解析 docs/openapi.yaml 的路径集合。
func openapiPaths(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("../../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	out := map[string]bool{}
	for _, m := range specPathRe.FindAllStringSubmatch(string(data), -1) {
		out[m[1]] = true
	}
	return out
}

// routerPaths 收集 gin 路由表的实际路径（OPTIONS 除外；通配 *path 归一为
// {path}），并校验期望清单中的路径确实可达（有对应 method 注册）。
func routerPaths(r *gin.Engine) map[string]bool {
	out := map[string]bool{}
	for _, rt := range r.Routes() {
		if rt.Method == http.MethodOptions {
			continue
		}
		p := rt.Path
		p = strings.ReplaceAll(p, "*path", "{path}")
		for _, seg := range strings.Split(p, "/") {
			if strings.HasPrefix(seg, ":") {
				p = strings.Replace(p, seg, "{"+seg[1:]+"}", 1)
			}
		}
		out[p] = true
	}
	return out
}

// newOpenapiProbeRouter 最小装配 router（能力全 nil，仅取路由表）。
func newOpenapiProbeRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return NewRouter(nil, nil, nil, AuthOptions{Enabled: false}, nil)
}

// TestOpenAPISpecMatchesRouter 双向一致性：spec 清单 ⊆ router，router ⊆ spec。
func TestOpenAPISpecMatchesRouter(t *testing.T) {
	spec := openapiPaths(t)
	router := routerPaths(newOpenapiProbeRouter(t))

	// 1) 硬编码期望清单必须同时出现在 spec 与 router 中
	for _, p := range specPaths {
		if !spec[p] {
			t.Errorf("path %q missing in docs/openapi.yaml (router has it: %v)", p, router[p])
		}
		if !router[p] {
			t.Errorf("path %q in openapi.yaml/specPaths but NOT registered in router (doc drift)", p)
		}
	}
	// 2) 反向：router 实际路径都应被文档覆盖（防止改路由不改文档）
	var extra []string
	for p := range router {
		if !spec[p] {
			extra = append(extra, p)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("router paths missing in openapi.yaml — update the spec: %v", extra)
	}
}

// TestOpenAPIYAMLParsesBasics 轻量健全性：yaml 必须含 openapi 3.x 声明与
// components 段（完整 YAML 解析不引第三方依赖，此处做结构断言）。
func TestOpenAPIYAMLParsesBasics(t *testing.T) {
	data, err := os.ReadFile("../../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(data)
	for _, want := range []string{"openapi: 3.0.3", "components:", "securitySchemes:", "X-API-Key"} {
		if !strings.Contains(s, want) {
			t.Errorf("openapi.yaml missing %q", want)
		}
	}
	// 429 语义必须在 POST /tasks 上有交代
	if !strings.Contains(s, "429") || !strings.Contains(s, "Retry-After") {
		t.Error("openapi.yaml must document 429 + Retry-After quota semantics")
	}
}
