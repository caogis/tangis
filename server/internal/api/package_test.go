package api

import (
	"archive/zip"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
	"tangis/server/internal/t3d"
	"tangis/server/internal/task"
	"tangis/server/internal/webhook"
)

// setupWithDir 创建鉴权关闭、DataDir 独立临时目录的路由，返回引擎与任务存储。
func setupWithDir(t *testing.T) (*gin.Engine, task.Store, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := task.NewMemoryStore()
	dataDir := t.TempDir()
	r := NewRouter(store, nil, nil, AuthOptions{DataDir: dataDir}, nil)
	return r, store, dataDir
}

// seedSucceededTask 在存储里预置一个 SUCCEEDED 任务并把产物写入 output 目录。
func seedSucceededTask(t *testing.T, store task.Store, approved bool) *task.Task {
	t.Helper()
	out := t.TempDir()
	files := map[string][]byte{
		"tileset.json":  []byte(`{"asset":{"version":"1.0"}}`),
		"Data/0/0.b3dm": bytes.Repeat([]byte("b3dm"), 100),
	}
	for rel, data := range files {
		p := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tk := &task.Task{
		ID:       "pkg1",
		Type:     "osgb->3dtiles",
		Source:   "/data/osgb/town",
		Output:   out,
		Status:   task.StatusSucceeded,
		TenantID: "default",
		Approved: approved,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestPackageTaskRoundTrip(t *testing.T) {
	r, store, _ := setupWithDir(t)
	seedSucceededTask(t, store, true)

	w := doReq(r, http.MethodGet, "/api/v1/tasks/pkg1/package", "")
	if w.Code != http.StatusOK {
		t.Fatalf("package status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "pkg1.t3d") {
		t.Fatalf("content-disposition = %q", cd)
	}

	// 解包校验：首条目 manifest.json，逐文件 SHA256 与原产物一致
	data := w.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("response not a zip: %v", err)
	}
	if zr.File[0].Name != t3d.ManifestEntry {
		t.Fatalf("first entry = %q, want manifest.json", zr.File[0].Name)
	}
	mr, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	var m t3d.Manifest
	if err := json.NewDecoder(mr).Decode(&m); err != nil {
		t.Fatalf("manifest decode: %v", err)
	}
	mr.Close()
	if m.Format != t3d.FormatName || m.Version != t3d.FormatVersion {
		t.Fatalf("manifest = %s/%d", m.Format, m.Version)
	}
	if m.Task.ID != "pkg1" {
		t.Fatalf("manifest task.id = %q", m.Task.ID)
	}
	if len(m.Files) != 2 {
		t.Fatalf("manifest files = %d, want 2", len(m.Files))
	}
	for _, fe := range m.Files {
		if !strings.HasPrefix(fe.Path, t3d.TilesPrefix) {
			t.Fatalf("file path %q missing prefix", fe.Path)
		}
		ef, ok := func() (*zip.File, bool) {
			for _, f := range zr.File {
				if f.Name == fe.Path {
					return f, true
				}
			}
			return nil, false
		}()
		if !ok {
			t.Fatalf("zip missing %s", fe.Path)
		}
		rc, _ := ef.Open()
		content, _ := io.ReadAll(rc)
		rc.Close()
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != fe.SHA256 {
			t.Fatalf("sha mismatch for %s", fe.Path)
		}
		if int64(len(content)) != fe.Size {
			t.Fatalf("size mismatch for %s", fe.Path)
		}
	}
	// 包内条目与清单一一对应（无夹带）
	if len(zr.File) != len(m.Files)+1 {
		t.Fatalf("zip entries = %d, want %d (+manifest)", len(zr.File), len(m.Files))
	}
}

func TestPackageTaskForbiddenWhenNotApproved(t *testing.T) {
	r, store, _ := setupWithDir(t)
	seedSucceededTask(t, store, false)

	w := doReq(r, http.MethodGet, "/api/v1/tasks/pkg1/package", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("unapproved package status = %d, want 403", w.Code)
	}
}

func TestPackageTaskConflictWhenRunning(t *testing.T) {
	r, store, _ := setupWithDir(t)
	tk := seedSucceededTask(t, store, true)
	tk.Status = task.StatusRunning
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}
	w := doReq(r, http.MethodGet, "/api/v1/tasks/pkg1/package", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("running package status = %d, want 409", w.Code)
	}
}

func TestPackageTaskNotFound(t *testing.T) {
	r, _, _ := setupWithDir(t)
	w := doReq(r, http.MethodGet, "/api/v1/tasks/ghost/package", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing package status = %d, want 404", w.Code)
	}
}

func TestPackageTaskAuthWithSignatureURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ks := auth.NewMemoryKeyStore("admin-key")
	dataDir := t.TempDir()
	store := task.NewMemoryStore()
	const secret = "sign-test-secret"
	r := NewRouter(store, nil, nil, AuthOptions{
		Enabled: true, Keys: ks, DataDir: dataDir, SignSecret: secret,
	}, nil)
	seedSucceededTask(t, store, true)

	// 无鉴权 → 401
	w := doReq(r, http.MethodGet, "/api/v1/tasks/pkg1/package", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth package status = %d, want 401", w.Code)
	}

	// 有效签名 URL → 200（签名覆盖完整请求 path）
	path := "/api/v1/tasks/pkg1/package"
	expires := time.Now().Add(time.Hour).Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s\n%d", path, expires)
	sig := hex.EncodeToString(mac.Sum(nil))
	w = doReq(r, http.MethodGet, fmt.Sprintf("%s?expires=%d&sig=%s", path, expires, sig), "")
	if w.Code != http.StatusOK {
		t.Fatalf("signed package status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	// 错误签名 → 401
	w = doReq(r, http.MethodGet, fmt.Sprintf("%s?expires=%d&sig=deadbeef", path, expires), "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad-signature package status = %d, want 401", w.Code)
	}
}

// buildT3d 用真实产物目录打包一个 .t3d 临时文件，返回路径。
func buildT3d(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		"tileset.json":  []byte(`{"asset":{"version":"1.0"}}`),
		"Data/0/0.b3dm": bytes.Repeat([]byte("x"), 256),
	}
	for rel, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := t3d.LocalDirSource{Dir: dir}
	filesList, err := t3d.HashSource(src)
	if err != nil {
		t.Fatalf("HashSource: %v", err)
	}
	var buf bytes.Buffer
	if err := t3d.Pack(&buf, src, &t3d.Manifest{
		Format:  t3d.FormatName,
		Version: t3d.FormatVersion,
		Files:   filesList,
	}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	p := filepath.Join(t.TempDir(), "import.t3d")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// multipartUpload 构造 multipart/form-data 上传请求体。
func multipartUpload(t *testing.T, field, path string, extra map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if extra != nil {
		for k, v := range extra {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	fw, err := mw.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestImportTaskRoundTrip(t *testing.T) {
	r, store, dataDir := setupWithDir(t)
	pkgPath := buildT3d(t)
	body, ctype := multipartUpload(t, "file", pkgPath, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/import", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Task task.Task `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	tk := resp.Task
	if tk.Type != task.TypeT3dImport {
		t.Fatalf("type = %q, want t3d-import", tk.Type)
	}
	if tk.Status != task.StatusSucceeded {
		t.Fatalf("status = %q, want SUCCEEDED (skip kernel)", tk.Status)
	}
	if tk.ID == "" {
		t.Fatal("task id empty")
	}

	// 产物落盘到任务工作目录
	tileset, err := os.ReadFile(filepath.Join(dataDir, "tasks", tk.ID, "tileset.json"))
	if err != nil {
		t.Fatalf("tileset.json not extracted: %v", err)
	}
	if !strings.Contains(string(tileset), "1.0") {
		t.Fatalf("tileset content unexpected: %s", tileset)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "tasks", tk.ID, "Data", "0", "0.b3dm")); err != nil {
		t.Fatalf("b3dm not extracted: %v", err)
	}

	// 存储中可查到该任务
	got, err := store.Get(tk.ID, "")
	if err != nil {
		t.Fatalf("task not in store: %v", err)
	}
	if got.Output == "" {
		t.Fatal("task output empty")
	}
}

func TestImportTaskRejectsTamperedSHA(t *testing.T) {
	// 构造 SHA256 被篡改的包 → 400 且不做部分导入
	r, _, dataDir := setupWithDir(t)
	mal := buildTamperedT3d(t)
	body, ctype := multipartUpload(t, "file", mal, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/import", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("tampered import status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sha256 mismatch") {
		t.Fatalf("error should mention sha256 mismatch: %s", w.Body.String())
	}
	// 任务目录不应存在（无部分导入）
	entries, _ := os.ReadDir(filepath.Join(dataDir, "tasks"))
	if len(entries) != 0 {
		t.Fatalf("no task dir should remain, got %d", len(entries))
	}
}

// buildTamperedT3d 构造清单哈希被篡改的恶意包。
func buildTamperedT3d(t *testing.T) string {
	t.Helper()
	pkgPath := buildT3d(t)
	data, _ := os.ReadFile(pkgPath)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	var m t3d.Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatal(err)
	}
	rc.Close()
	for i := range m.Files {
		if m.Files[i].Path != t3d.RequiredEntry {
			m.Files[i].SHA256 = strings.Repeat("0", 64)
			break
		}
	}
	mj, _ := json.Marshal(m)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.Create(t3d.ManifestEntry)
	mw.Write(mj)
	for _, fe := range m.Files {
		raw := strings.TrimPrefix(fe.Path, t3d.TilesPrefix)
		ef, oerr := zrOpen(zr, raw)
		if oerr != nil {
			t.Fatal(oerr)
		}
		erc, _ := ef.Open()
		content, _ := io.ReadAll(erc)
		erc.Close()
		fw, _ := zw.Create(fe.Path)
		fw.Write(content)
	}
	zw.Close()
	p := filepath.Join(t.TempDir(), "tampered.t3d")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func zrOpen(zr *zip.Reader, name string) (*zip.File, error) {
	for _, f := range zr.File {
		if f.Name == t3d.TilesPrefix+name || f.Name == name {
			return f, nil
		}
	}
	return nil, fmt.Errorf("entry %q not found", name)
}

func TestImportTaskRejectsZipSlip(t *testing.T) {
	// 恶意包：条目 tiles/../evil.txt（清单同样声明）→ 400 且不落盘
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.Create(t3d.ManifestEntry)
	content := []byte("evil")
	sum := sha256.Sum256(content)
	m := t3d.Manifest{
		Format:  t3d.FormatName,
		Version: t3d.FormatVersion,
		Files: []t3d.FileEntry{
			{Path: t3d.TilesPrefix + "../evil.txt", Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
		},
	}
	mj, _ := json.Marshal(m)
	mw.Write(mj)
	fw, _ := zw.Create(t3d.TilesPrefix + "../evil.txt")
	fw.Write(content)
	zw.Close()

	p := filepath.Join(t.TempDir(), "evil.t3d")
	os.WriteFile(p, buf.Bytes(), 0o644)

	r, _, dataDir := setupWithDir(t)
	body, ctype := multipartUpload(t, "file", p, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/import", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("zip slip import status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	// evil.txt 绝不允许出现在 dataDir 之外（临时目录任意位置都不应有该文件）
	if _, err := os.Stat(filepath.Join(filepath.Dir(dataDir), "evil.txt")); err == nil {
		t.Fatal("zip slip file written outside dest")
	}
}

func TestImportTaskWithoutFile(t *testing.T) {
	r, _, _ := setupWithDir(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("notfile", "x")
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("import without file status = %d, want 400", w.Code)
	}
}

func TestImportTaskWebhookFires(t *testing.T) {
	// 导入成功即 SUCCEEDED → webhook 事件发出
	var gotBody []byte
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		called <- struct{}{}
	}))
	defer srv.Close()

	wh := &webhook.Notifier{Secret: "whsec", Backoff: []time.Duration{time.Millisecond}}
	gin.SetMode(gin.TestMode)
	store := task.NewMemoryStore()
	dataDir := t.TempDir()
	r := NewRouter(store, nil, nil, AuthOptions{DataDir: dataDir, Notifier: wh}, nil)

	pkgPath := buildT3d(t)
	body, ctype := multipartUpload(t, "file", pkgPath, map[string]string{"webhook_url": srv.URL})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/import", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("import status = %d, body=%s", w.Code, w.Body.String())
	}
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("webhook not called after import")
	}
	var ev struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(gotBody, &ev); err != nil {
		t.Fatalf("event not json: %v", err)
	}
	if ev.Status != string(task.StatusSucceeded) || ev.TaskID == "" {
		t.Fatalf("event = %+v", ev)
	}
}
