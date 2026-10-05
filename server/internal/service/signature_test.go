package service

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tangis/server/internal/auth"
)

// signedFixture 带签名校验的 Server（AuthEnabled=true）。
func signedFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.srv.AuthEnabled = true
	f.srv.SignSecret = "test-sign-secret"
	f.srv.SignTTL = time.Hour
	return f
}

func TestServeTileRequiresSignature(t *testing.T) {
	f := signedFixture(t)

	// 无签名 -> 403
	if w := f.do(http.MethodGet, "/services/svc-task-1/tileset.json"); w.Code != http.StatusForbidden {
		t.Fatalf("no signature status = %d, want 403", w.Code)
	}
	// 错误签名 -> 403
	if w := f.do(http.MethodGet, "/services/svc-task-1/tileset.json?expires=99999999999&sig=deadbeef"); w.Code != http.StatusForbidden {
		t.Fatalf("bad signature status = %d, want 403", w.Code)
	}
	// 有效签名 -> 200
	path := "/services/svc-task-1/tileset.json"
	q := auth.SignURL(path, time.Now().Add(time.Hour), "test-sign-secret")
	if w := f.do(http.MethodGet, path+"?"+q); w.Code != http.StatusOK {
		t.Fatalf("valid signature status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	// 过期签名 -> 403
	expired := auth.SignURL(path, time.Now().Add(-time.Minute), "test-sign-secret")
	if w := f.do(http.MethodGet, path+"?"+expired); w.Code != http.StatusForbidden {
		t.Fatalf("expired signature status = %d, want 403", w.Code)
	}
	// 换 path 签名 -> 403
	other := auth.SignURL("/services/svc-task-1/Data/0.b3dm", time.Now().Add(time.Hour), "test-sign-secret")
	if w := f.do(http.MethodGet, "/services/svc-task-1/tileset.json?"+other); w.Code != http.StatusForbidden {
		t.Fatalf("cross-path signature status = %d, want 403", w.Code)
	}
	// OPTIONS 预检不受签名限制
	if w := f.do(http.MethodOptions, "/services/svc-task-1/tileset.json"); w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS with no signature status = %d, want 204", w.Code)
	}
}

func TestListServicesSignedTilesetURL(t *testing.T) {
	f := signedFixture(t)

	w := f.do(http.MethodGet, "/api/v1/services")
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	body := w.Body.String()
	// tileset_url 应自带 expires + sig（JSON 中 & 会转义为 \u0026，故只断言参数名）
	if !strings.Contains(body, "tileset.json?expires=") || !strings.Contains(body, "sig=") {
		t.Fatalf("tileset_url missing signature: %s", body)
	}
	// 生成出的带签名 URL 应可通过分发校验
	var resp struct {
		Services []ServiceEntry `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(resp.Services))
	}
	u := resp.Services[0].TilesetURL
	if w := f.do(http.MethodGet, u); w.Code != http.StatusOK {
		t.Fatalf("signed tileset_url fetch = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestListServicesNoSignatureWhenAuthOff(t *testing.T) {
	f := newFixture(t) // AuthEnabled=false（TANGIS_AUTH=off 兼容）
	w := f.do(http.MethodGet, "/api/v1/services")
	if strings.Contains(w.Body.String(), "?expires=") {
		t.Fatalf("auth off should not sign URLs: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/services/svc-task-1/tileset.json") {
		t.Fatalf("plain tileset_url missing: %s", w.Body.String())
	}
}

// signedNestedTilesetFixture 在 signedFixture 基础上布置嵌套 tileset 产物：
// 根 tileset.json → content: Data/0.b3dm + 嵌套 Data/inner.json；
// inner.json（位于 Data/ 子目录）→ content: ../Data/1.b3dm（MinIO 回源）。
func signedNestedTilesetFixture(t *testing.T) *fixture {
	t.Helper()
	f := signedFixture(t)
	root := `{
	  "asset": {"version": "1.1"},
	  "geometricError": 500,
	  "root": {
	    "content": {"uri": "Data/0.b3dm"},
	    "children": [
	      {"content": {"uri": "Data/inner.json"}},
	      {"content": {"uri": "https://cdn.example.com/external/tileset.json"}}
	    ]
	  }
	}`
	inner := `{"asset":{"version":"1.1"},"root":{"content":{"uri":"../Data/1.b3dm"}}}`
	if err := os.WriteFile(filepath.Join(f.outputDir, "tileset.json"), []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.outputDir, "Data", "inner.json"), []byte(inner), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// queryValues 解析 uri 的 query 部分（uri 可能含相对路径字符，仅取 ? 之后）。
func queryValues(uri string) url.Values {
	i := strings.Index(uri, "?")
	if i < 0 {
		return url.Values{}
	}
	v, err := url.ParseQuery(uri[i+1:])
	if err != nil {
		return url.Values{}
	}
	return v
}

// assertSignedURI 校验改写后的 uri：expires/sig 齐全且对期望 path 验签通过，
// 并回填 expires/sig 供拼接请求。
func assertSignedURI(t *testing.T, uri, wantPath string) url.Values {
	t.Helper()
	if !strings.Contains(uri, "?expires=") || !strings.Contains(uri, "sig=") {
		t.Fatalf("uri %q not rewritten with signature", uri)
	}
	v := queryValues(uri)
	exp, _ := strconv.ParseInt(v.Get("expires"), 10, 64)
	if !auth.VerifySignature(wantPath, exp, v.Get("sig"), "test-sign-secret") {
		t.Fatalf("signature invalid for uri %q (path %s)", uri, wantPath)
	}
	return v
}

// resolveRef 模拟浏览器/Cesium 的相对引用解析：ref（可含 ../ 与 query）
// 以当前 tileset.json 的绝对 URL path 为 base 解析为规范请求路径。
func resolveRef(t *testing.T, base, ref string) string {
	t.Helper()
	u, err := url.Parse(ref)
	if err != nil {
		t.Fatal(err)
	}
	return path.Join(path.Dir(base), u.Path) + "?" + u.RawQuery
}

// TestServeTilesetRewritesContentURIs C 致命缺陷修复验证：
// tileset.json 输出时 content.uri（含嵌套 tileset）按自身 path 重签，
// Cesium 用改写后 URL 请求子资源应逐层验签通过。
func TestServeTilesetRewritesContentURIs(t *testing.T) {
	f := signedNestedTilesetFixture(t)

	// 1) 入口 tileset.json（带签名）→ 改写后的 Data/0.b3dm uri 可验签且可请求
	w := f.do(http.MethodGet, "/services/svc-task-1/tileset.json?"+
		auth.SignURL("/services/svc-task-1/tileset.json", time.Now().Add(time.Hour), "test-sign-secret"))
	if w.Code != http.StatusOK {
		t.Fatalf("tileset.json status = %d, body=%s", w.Code, w.Body.String())
	}
	var doc struct {
		Root struct {
			Content struct {
				URI string `json:"uri"`
			} `json:"content"`
			Children []struct {
				Content struct {
					URI string `json:"uri"`
				} `json:"content"`
			} `json:"children"`
		} `json:"root"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("tileset.json not valid json: %v\n%s", err, w.Body.String())
	}
	b3dmURI := doc.Root.Content.URI
	assertSignedURI(t, b3dmURI, "/services/svc-task-1/Data/0.b3dm")
	if !strings.HasPrefix(b3dmURI, "Data/0.b3dm?") {
		t.Fatalf("rewritten uri should keep original relative path: %q", b3dmURI)
	}
	// Cesium 行为模拟：按改写后 URL（含 query）请求子瓦片 → 200
	w = f.do(http.MethodGet, resolveRef(t, "/services/svc-task-1/tileset.json", b3dmURI))
	if w.Code != http.StatusOK || w.Body.String() != "\x00\x00\x00\x00b3dm" {
		t.Fatalf("rewritten b3dm uri fetch = %d body=%s", w.Code, w.Body.String())
	}

	// 2) 嵌套 tileset uri 按根目录解析签名，且请求后其内部 uri 按
	//    inner.json 自身目录（Data/）解析：../Data/1.b3dm → Data/1.b3dm
	if len(doc.Root.Children) < 1 {
		t.Fatalf("children missing: %s", w.Body.String())
	}
	innerURI := doc.Root.Children[0].Content.URI
	assertSignedURI(t, innerURI, "/services/svc-task-1/Data/inner.json")
	innerBase := resolveRef(t, "/services/svc-task-1/tileset.json", innerURI)
	w = f.do(http.MethodGet, innerBase)
	if w.Code != http.StatusOK {
		t.Fatalf("rewritten inner.json uri fetch = %d body=%s", w.Code, w.Body.String())
	}
	var inner struct {
		Root struct {
			Content struct {
				URI string `json:"uri"`
			} `json:"content"`
		} `json:"root"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &inner); err != nil {
		t.Fatal(err)
	}
	assertSignedURI(t, inner.Root.Content.URI, "/services/svc-task-1/Data/1.b3dm")
	// 嵌套 tileset 的子瓦片（MinIO 回源）按改写后 URL 可请求
	w = f.do(http.MethodGet, resolveRef(t, innerBase, inner.Root.Content.URI))
	if w.Code != http.StatusOK || w.Body.String() != "REMOTE-B3DM" {
		t.Fatalf("rewritten remote b3dm uri fetch = %d body=%s", w.Code, w.Body.String())
	}

	// 3) 外部地址（https://）不改写
	if len(doc.Root.Children) < 2 || doc.Root.Children[1].Content.URI != "https://cdn.example.com/external/tileset.json" {
		t.Fatalf("external uri should be untouched: %+v", doc.Root.Children)
	}

	// 4) 改写后的 JSON 响应禁缓存（expires 随签发时刻变化，缓存会持有过期签名）
	w = f.do(http.MethodGet, "/services/svc-task-1/tileset.json?"+
		auth.SignURL("/services/svc-task-1/tileset.json", time.Now().Add(time.Hour), "test-sign-secret"))
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("rewritten tileset.json should set Cache-Control no-store, got %q", cc)
	}
}

// TestServeTilesetNoRewriteWhenAuthOff TANGIS_AUTH=off：content.uri 原样返回。
func TestServeTilesetNoRewriteWhenAuthOff(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.outputDir, "tileset.json"), []byte(
		`{"asset":{"version":"1.1"},"root":{"content":{"uri":"Data/0.b3dm"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w := f.do(http.MethodGet, "/services/svc-task-1/tileset.json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sig=") || strings.Contains(w.Body.String(), "expires=") {
		t.Fatalf("auth off should not rewrite content.uri: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"uri":"Data/0.b3dm"`) {
		t.Fatalf("content.uri should be untouched: %s", w.Body.String())
	}
}

// TestServeTileB3dmNeverRewritten b3dm 等非 JSON 资源维持原样（仅验签，不改写）。
func TestServeTileB3dmNeverRewritten(t *testing.T) {
	f := signedFixture(t)
	q := auth.SignURL("/services/svc-task-1/Data/0.b3dm", time.Now().Add(time.Hour), "test-sign-secret")
	w := f.do(http.MethodGet, "/services/svc-task-1/Data/0.b3dm?"+q)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Body.String() != "\x00\x00\x00\x00b3dm" {
		t.Fatalf("b3dm body must be untouched: %q", w.Body.String())
	}
}

// TestServeNonTilesetJSONUntouched 非 tileset 的 .json 产物原样分发（不报错、不改写）。
func TestServeNonTilesetJSONUntouched(t *testing.T) {
	f := signedFixture(t)
	q := auth.SignURL("/services/svc-task-1/Data/app.json", time.Now().Add(time.Hour), "test-sign-secret")
	w := f.do(http.MethodGet, "/services/svc-task-1/Data/app.json?"+q)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"meta":"app"}` {
		t.Fatalf("non-tileset json body changed: %s", w.Body.String())
	}
}
