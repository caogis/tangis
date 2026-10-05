// fs_test.go — 本机目录浏览（建任务选路径）的行为测试：
// 列举/过滤/排序、白名单越界、非目录与不存在路径、mkdir 单层约束。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newFSRouter 装配一个只关心 fs 端点的 router（鉴权关闭 → 按 admin 放行）。
func newFSRouter(enabled bool, roots []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	return NewRouter(nil, nil, nil, AuthOptions{
		FSBrowseEnabled: enabled,
		FSBrowseRoots:   roots,
	}, nil)
}

func doJSON(t *testing.T, r *gin.Engine, method, url string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, url, nil)
	} else {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestBrowseFSListsDirsOnly 默认 kind=dir 只列目录，且目录优先、隐藏项不出现。
func TestBrowseFSListsDirsOnly(t *testing.T) {
	base := t.TempDir()
	mustMkdir(t, filepath.Join(base, "osgb"))
	mustMkdir(t, filepath.Join(base, ".hidden"))
	writeFile(t, filepath.Join(base, "readme.txt"), "x")
	writeFile(t, filepath.Join(base, "a.tif"), "x")

	r := newFSRouter(true, nil)
	w := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse?path="+base, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got fsBrowseResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(got.Entries) != 1 || got.Entries[0].Name != "osgb" {
		t.Fatalf("entries=%+v, want only dir osgb", got.Entries)
	}
	if got.Path != base {
		t.Errorf("path=%q want %q", got.Path, base)
	}
	if got.Parent != filepath.Dir(base) {
		t.Errorf("parent=%q want %q", got.Parent, filepath.Dir(base))
	}
	if !got.Writable {
		t.Error("temp dir should be writable")
	}
}

// TestBrowseFSFileKindAndExt kind=file + ext 过滤出目标数据文件。
func TestBrowseFSFileKindAndExt(t *testing.T) {
	base := t.TempDir()
	mustMkdir(t, filepath.Join(base, "sub"))
	writeFile(t, filepath.Join(base, "dem.TIF"), "x")
	writeFile(t, filepath.Join(base, "notes.md"), "x")

	r := newFSRouter(true, nil)
	w := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse?path="+base+"&kind=file&ext=.tif", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got fsBrowseResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries=%+v, want [sub dem.TIF]", got.Entries)
	}
	if !got.Entries[0].IsDir || got.Entries[0].Name != "sub" {
		t.Errorf("dirs must come first: %+v", got.Entries)
	}
	if got.Entries[1].Name != "dem.TIF" {
		t.Errorf("ext filter should be case-insensitive: %+v", got.Entries)
	}
}

// TestBrowseFSNotADirectory 路径指向文件 → 400（前端应先取父目录再浏览）。
func TestBrowseFSNotADirectory(t *testing.T) {
	base := t.TempDir()
	f := filepath.Join(base, "dem.tif")
	writeFile(t, f, "x")

	r := newFSRouter(true, nil)
	w := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse?path="+f, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", w.Code, w.Body.String())
	}
	w2 := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse?path="+filepath.Join(base, "nope"), "")
	if w2.Code != http.StatusNotFound {
		t.Fatalf("missing dir status=%d want 404", w2.Code)
	}
}

// TestBrowseFSRootsEnforced 白名单外路径 403，白名单内放行，且顶层返回 roots。
func TestBrowseFSRootsEnforced(t *testing.T) {
	allowed := t.TempDir()
	mustMkdir(t, filepath.Join(allowed, "in"))
	outside := t.TempDir()

	r := newFSRouter(true, []string{allowed})

	w := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse?path="+outside, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("outside of roots status=%d want 403, body=%s", w.Code, w.Body.String())
	}

	w2 := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse?path="+allowed, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("inside roots status=%d, body=%s", w2.Code, w2.Body.String())
	}

	// 顶层：白名单根
	w3 := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse", "")
	var top fsBrowseResp
	if err := json.Unmarshal(w3.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode top: %v", err)
	}
	if len(top.Roots) != 1 || top.Roots[0] != allowed {
		t.Errorf("roots=%v want [%s]", top.Roots, allowed)
	}
	if len(top.Entries) != 1 || top.Entries[0].Path != allowed {
		t.Errorf("top entries=%+v want the single root", top.Entries)
	}
	if top.Parent != "" {
		t.Errorf("root parent=%q want empty", top.Parent)
	}
}

// TestBrowseFSTopLevel 未配白名单时顶层给出可浏览的根（非 Windows 为 "/"）。
func TestBrowseFSTopLevel(t *testing.T) {
	r := newFSRouter(true, nil)
	w := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var top fsBrowseResp
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(top.Entries) == 0 {
		t.Fatal("top level must expose at least one entry")
	}
	if runtime.GOOS != "windows" && top.Entries[0].Path != "/" {
		t.Errorf("unix top entry=%q want /", top.Entries[0].Path)
	}
	if top.Os != runtime.GOOS {
		t.Errorf("os=%q want %q", top.Os, runtime.GOOS)
	}
}

// TestBrowseFSDisabled TANGIS_FS_BROWSE=off 时两个端点都不可达（404）。
func TestBrowseFSDisabled(t *testing.T) {
	r := newFSRouter(false, nil)
	if w := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse", ""); w.Code != http.StatusNotFound {
		t.Errorf("browse disabled status=%d want 404", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/fs/mkdir", `{"path":"/tmp","name":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("mkdir disabled status=%d want 404", w.Code)
	}
}

// TestMkdirFS 建目录成功 / 单层约束 / 白名单越界。
func TestMkdirFS(t *testing.T) {
	base := t.TempDir()
	r := newFSRouter(true, nil)

	w := doJSON(t, r, http.MethodPost, "/api/v1/fs/mkdir", `{"path":"`+base+`","name":"out"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st, err := os.Stat(created.Path); err != nil || !st.IsDir() {
		t.Fatalf("mkdir result %q: stat err=%v", created.Path, err)
	}

	// 名字含分隔符 → 400（拒绝 ../ 逃逸与多级创建）
	if w2 := doJSON(t, r, http.MethodPost, "/api/v1/fs/mkdir", `{"path":"`+base+`","name":"../evil"}`); w2.Code != http.StatusBadRequest {
		t.Errorf("escape name status=%d want 400, body=%s", w2.Code, w2.Body.String())
	}
	// 缺 name → 400
	if w3 := doJSON(t, r, http.MethodPost, "/api/v1/fs/mkdir", `{"path":"`+base+`"}`); w3.Code != http.StatusBadRequest {
		t.Errorf("missing name status=%d want 400", w3.Code)
	}
	// 重复创建 → 400（os.Mkdir 语义：已存在即失败，不静默复用）
	if w4 := doJSON(t, r, http.MethodPost, "/api/v1/fs/mkdir", `{"path":"`+base+`","name":"out"}`); w4.Code != http.StatusBadRequest {
		t.Errorf("duplicate mkdir status=%d want 400", w4.Code)
	}

	// 白名单外 → 403
	rr := newFSRouter(true, []string{t.TempDir()})
	if w5 := doJSON(t, rr, http.MethodPost, "/api/v1/fs/mkdir", `{"path":"`+base+`","name":"x"}`); w5.Code != http.StatusForbidden {
		t.Errorf("outside roots mkdir status=%d want 403", w5.Code)
	}
}

// TestBrowseFSKindValidation kind 非法 → 400（显式参数白名单，不猜测）。
func TestBrowseFSKindValidation(t *testing.T) {
	r := newFSRouter(true, nil)
	if w := doJSON(t, r, http.MethodGet, "/api/v1/fs/browse?kind=whatever", ""); w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}
