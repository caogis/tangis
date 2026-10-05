package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/cache"
	"tangis/server/internal/task"
)

// F-03 2D 影像瓦片分发测试。
// 测试 fixture 使用自造的合法 PNG（1x1 透明像素），仅用于测试存储通道；
// 生产路径不返回任何 fixture。

// png1x1 合法 1x1 PNG 瓦片。
var png1x1, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

const imageMetaJSON = `{
  "tile_matrix_set": "WebMercatorQuad",
  "extent": [12958000.0, 4792000.0, 12992000.0, 4826000.0],
  "min_zoom": 0,
  "max_zoom": 2,
  "tile_size": 256,
  "format": "png"
}`

type tileFixture struct {
	r      *gin.Engine
	srv    *Server
	store  *task.MemoryStore
	getter *fakeGetter
	cc     *cache.MemoryCache
	outDir string
	taskID string
}

// newTileFixture 已发布影像任务：本地 output/tiles 金字塔（z0 全 1 瓦片 + z1 两瓦片）。
func newTileFixture(t *testing.T) *tileFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	tilesDir := filepath.Join(dir, "tiles")
	if err := os.MkdirAll(filepath.Join(tilesDir, "0", "0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tilesDir, "1", "0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tilesDir, "1", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel string, data []byte) {
		if err := os.WriteFile(filepath.Join(tilesDir, filepath.FromSlash(rel)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("metadata.json", []byte(imageMetaJSON))
	write(filepath.Join("0", "0", "0.png"), png1x1)
	write(filepath.Join("1", "0", "0.png"), png1x1)
	write(filepath.Join("1", "1", "1.png"), png1x1)

	store := task.NewMemoryStore()
	tk := &task.Task{
		ID:          "img-task-1",
		Type:        "image->tiles",
		Source:      "/data/img/dem.tif",
		Output:      dir,
		Status:      task.StatusSucceeded,
		MinioPrefix: "tasks/img-task-1",
		Approved:    true,
		TenantID:    "default",
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	getter := &fakeGetter{objects: map[string]string{
		// MinIO 回源瓦片（本地缺失）：tasks/{id}/tiles/2/2/3.png
		"tasks/img-task-1/tiles/2/2/3.png": string(png1x1),
	}}
	cc := cache.NewMemoryCache()

	s := &Server{Store: store, Objects: getter, Bucket: "tangis", Cache: cc}
	r := gin.New()
	r.GET("/api/v1/wmts", s.WMTSKvp)
	r.GET("/api/v1/wmts/1.0.0/:layer/:style/:tms/:z/:row/:col", s.WMTSRestTile)
	r.GET("/api/v1/tms/:task_id/:z/:x/:y", s.TMSTile)
	r.GET("/api/v1/services", s.ListServices)

	return &tileFixture{r: r, srv: s, store: store, getter: getter, cc: cc, outDir: dir, taskID: tk.ID}
}

func (f *tileFixture) do(method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://test.example"+path, nil)
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

// TestServeTileWMTSLayout metadata.layout=wmts 时按 {z}/{row}/{col} 物理布局读盘（B3）。
func TestServeTileWMTSLayout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	tilesDir := filepath.Join(dir, "tiles")
	if err := os.MkdirAll(filepath.Join(tilesDir, "0", "0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tilesDir, "1", "0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tilesDir, "1", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	wmtsMetaJSON := `{
	  "tile_matrix_set": "WebMercatorQuad",
	  "layout": "wmts",
	  "extent": [12958000.0, 4792000.0, 12992000.0, 4826000.0],
	  "min_zoom": 0,
	  "max_zoom": 1,
	  "tile_size": 256,
	  "format": "png"
	}`
	write := func(rel string, data []byte) {
		if err := os.WriteFile(filepath.Join(tilesDir, filepath.FromSlash(rel)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("metadata.json", []byte(wmtsMetaJSON))
	// 物理布局 {z}/{row}/{col}：xyz(0,0,z0)→0/0/0；xyz(x=0,y=0)→1/0/0；
	// xyz(x=0,y=1)→1/1/0；xyz(x=1,y=1)→1/1/1
	write(filepath.Join("0", "0", "0.png"), png1x1)
	write(filepath.Join("1", "0", "0.png"), png1x1)
	write(filepath.Join("1", "1", "0.png"), png1x1)
	write(filepath.Join("1", "1", "1.png"), png1x1)

	store := task.NewMemoryStore()
	tk := &task.Task{
		ID: "wmts-layout-1", Type: "image->tiles", Source: "/data/img/x.tif",
		Output: dir, Status: task.StatusSucceeded, Approved: true, TenantID: "default",
		MinioPrefix: "tasks/wmts-layout-1",
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, Cache: cache.NewMemoryCache()}
	r := gin.New()
	r.GET("/api/v1/wmts/1.0.0/:layer/:style/:tms/:z/:row/:col", s.WMTSRestTile)
	r.GET("/api/v1/tms/:task_id/:z/:x/:y", s.TMSTile)

	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://test.example"+path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// WMTS REST：TileMatrix=1/TileRow=0/TileCol=1 → 物理 1/0/1.png —— 未写 → 404
	if w := do(http.MethodGet, "/api/v1/wmts/1.0.0/wmts-layout-1/default/WebMercatorQuad/1/0/1.png"); w.Code != http.StatusNotFound {
		t.Fatalf("missing wmts tile = %d, want 404", w.Code)
	}
	// WMTS REST：row=1,col=1 → 物理 1/1/1.png → 200
	if w := do(http.MethodGet, "/api/v1/wmts/1.0.0/wmts-layout-1/default/WebMercatorQuad/1/1/1.png"); w.Code != http.StatusOK {
		t.Fatalf("wmts tile 1/1/1 = %d, want 200", w.Code)
	}
	// TMS：z=1,x=1,y_tms=0 → xyz(1,1) → 物理 1/1/1.png → 200
	if w := do(http.MethodGet, "/api/v1/tms/wmts-layout-1/1/1/0.png"); w.Code != http.StatusOK {
		t.Fatalf("tms→wmts-layout tile = %d, want 200", w.Code)
	}
	// xyz(x=0,y=1) → 物理 1/1/0.png：TMS z=1,x=0,y_tms=0 → 200
	if w := do(http.MethodGet, "/api/v1/tms/wmts-layout-1/1/0/0.png"); w.Code != http.StatusOK {
		t.Fatalf("tms(x=0) = %d, want 200", w.Code)
	}
}

func TestGetCapabilitiesFromRealMetadata(t *testing.T) {
	f := newTileFixture(t)
	w := f.do(http.MethodGet, "/api/v1/wmts?service=WMTS&version=1.0.0&request=GetCapabilities")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/xml") {
		t.Fatalf("content-type = %q", ct)
	}
	body := w.Body.String()
	// Layer 由真实任务生成
	for _, want := range []string{
		"<ows:Identifier>img-task-1</ows:Identifier>",
		"<TileMatrixSet>WebMercatorQuad</TileMatrixSet>",
		`template="http://test.example/api/v1/wmts/1.0.0/img-task-1/default/WebMercatorQuad/{TileMatrix}/{TileRow}/{TileCol}.png"`,
		"image/png",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("capabilities missing %q:\n%s", want, body)
		}
	}
	// KVP + RESTful 两种 encoding 均声明
	if !strings.Contains(body, "<ows:Value>KVP</ows:Value>") || !strings.Contains(body, "<ows:Value>RESTful</ows:Value>") {
		t.Fatal("capabilities must advertise KVP and RESTful encodings")
	}
	// TileMatrixSet 层级定义来自真实 WMTS 标准分母
	if !strings.Contains(body, "<ScaleDenominator>559082264.0287178</ScaleDenominator>") {
		t.Fatal("z0 scale denominator wrong")
	}
	// WGS84BoundingBox 由 extent 真实变换（EPSG:3857→CRS84）
	if !strings.Contains(body, "116.4") || !strings.Contains(body, "39.7") {
		t.Fatalf("WGS84BoundingBox missing (want Beijing-ish coords): %s", body)
	}
	// TileMatrixSetLimits 按元数据层级（0..2）
	if !strings.Contains(body, "<TileMatrix>2</TileMatrix>") {
		t.Fatal("TileMatrixSetLimits missing max zoom")
	}
	// XML 良构性校验（客户端解析前提）
	dec := xml.NewDecoder(strings.NewReader(body))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("capabilities XML not well-formed: %v", err)
		}
	}
}

func TestGetCapabilitiesEmptyWhenNoImageTasks(t *testing.T) {
	f := newTileFixture(t)
	if err := f.store.Delete(f.taskID, ""); err != nil {
		t.Fatal(err)
	}
	w := f.do(http.MethodGet, "/api/v1/wmts?request=GetCapabilities")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "<Layer>") {
		t.Fatal("no image tasks should yield no Layer (no fake layers)")
	}
}

func TestWMTSKvpGetTileLocal(t *testing.T) {
	f := newTileFixture(t)
	w := f.do(http.MethodGet,
		"/api/v1/wmts?service=WMTS&version=1.0.0&request=GetTile&layer=img-task-1&tilematrixset=WebMercatorQuad&tilematrix=0&tilerow=0&tilecol=0&format=image/png")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q", ct)
	}
	if w.Body.String() != string(png1x1) {
		t.Fatal("tile body mismatch")
	}
	// 本地命中不触发 MinIO
	if len(f.getter.gets) != 0 {
		t.Fatalf("local hit should not hit MinIO: %v", f.getter.gets)
	}
	// CORS
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("CORS header missing")
	}
}

func TestWMTSRestGetTile(t *testing.T) {
	f := newTileFixture(t)
	w := f.do(http.MethodGet, "/api/v1/wmts/1.0.0/img-task-1/default/WebMercatorQuad/1/1/1.png")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(png1x1) {
		t.Fatal("tile body mismatch")
	}
}

func TestTMSTileYFlip(t *testing.T) {
	f := newTileFixture(t)
	// 金字塔按 XYZ（Y 原点左上）存储：z1 本地有 xyz(0,0) 与 xyz(1,1) 两瓦片。
	// TMS y 与 XYZ 的换算：y_xyz = 2^z - 1 - y_tms。
	// TMS (x=1, y_tms=0)（最北行）→ xyz(1,1)：本地存在 → 200
	w := f.do(http.MethodGet, "/api/v1/tms/img-task-1/1/1/0.png")
	if w.Code != http.StatusOK {
		t.Fatalf("tms(1,0) → xyz(1,1) status = %d, body=%s", w.Code, w.Body.String())
	}
	// TMS (x=0, y_tms=1)（最南行）→ xyz(0,0)：本地存在 → 200
	w = f.do(http.MethodGet, "/api/v1/tms/img-task-1/1/0/1.png")
	if w.Code != http.StatusOK {
		t.Fatalf("tms(0,1) → xyz(0,0) status = %d, body=%s", w.Code, w.Body.String())
	}
	// TMS (x=1, y_tms=1) → xyz(1,0)：本地与 MinIO 均无 → 404
	w = f.do(http.MethodGet, "/api/v1/tms/img-task-1/1/1/1.png")
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing tile should 404, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestTileServedFromMinIOAndCached(t *testing.T) {
	f := newTileFixture(t)
	// z=2 超出本地，MinIO 回源（max_zoom=2 内）
	w := f.do(http.MethodGet, "/api/v1/tms/img-task-1/2/2/0.png") // TMS y=0 → XYZ y=3? z=2: mh=4, y=4-1-0=3
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if len(f.getter.gets) != 1 || f.getter.gets[0] != "tasks/img-task-1/tiles/2/2/3.png" {
		t.Fatalf("MinIO gets = %v", f.getter.gets)
	}
	// 第二次请求命中缓存，不再回源
	w = f.do(http.MethodGet, "/api/v1/tms/img-task-1/2/2/0.png")
	if w.Code != http.StatusOK {
		t.Fatalf("cached status = %d", w.Code)
	}
	if len(f.getter.gets) != 1 {
		t.Fatalf("second request should hit cache, MinIO gets = %v", f.getter.gets)
	}
	// 缓存 key 含 tenant/task/z/x/y（TMS 翻转后的 XYZ 坐标）
	if _, ok := f.cc.Get(context.Background(), "tile:default:img-task-1:2:2:3:png"); !ok {
		t.Fatal("tile cache key missing (tenant:task:z:x:y:format)")
	}
}

func TestGetTileValidation(t *testing.T) {
	f := newTileFixture(t)
	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/wmts?request=GetTile&layer=img-task-1&tilematrixset=WebMercatorQuad&tilematrix=5&tilerow=0&tilecol=0", http.StatusBadRequest},                   // 超出 max_zoom
		{"/api/v1/wmts?request=GetTile&layer=img-task-1&tilematrixset=WebMercatorQuad&tilematrix=0&tilerow=3&tilecol=0", http.StatusBadRequest},                   // 矩阵外
		{"/api/v1/wmts?request=GetTile&layer=img-task-1&tilematrixset=WebMercatorQuad&tilematrix=0&tilerow=0&tilecol=0&format=image/webp", http.StatusBadRequest}, // 格式不符
		{"/api/v1/wmts/1.0.0/img-task-1/default/WorldCRS84Quad/0/0/0.png", http.StatusBadRequest},                                                                 // TMS 不匹配
		{"/api/v1/wmts/1.0.0/no-such-task/default/WebMercatorQuad/0/0/0.png", http.StatusNotFound},                                                                // 任务不存在
		{"/api/v1/tms/img-task-1/9/0/0.png", http.StatusBadRequest},                                                                                               // TMS 超层
	}
	for _, c := range cases {
		if w := f.do(http.MethodGet, c.path); w.Code != c.want {
			t.Fatalf("GET %s = %d, want %d, body=%s", c.path, w.Code, c.want, w.Body.String())
		}
	}
}

func TestUnpublishedTaskNoTiles(t *testing.T) {
	f := newTileFixture(t)
	// 未审批：不可分发（F-21 语义同样约束 2D）
	tk, _ := f.store.Get(f.taskID, "")
	tk.Approved = false
	if err := f.store.Update(tk); err != nil {
		t.Fatal(err)
	}
	w := f.do(http.MethodGet, "/api/v1/wmts/1.0.0/img-task-1/default/WebMercatorQuad/0/0/0.png")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unapproved task tile = %d, want 404", w.Code)
	}
}

func TestListServices2DEntries(t *testing.T) {
	f := newTileFixture(t)
	// 一个 3D 任务（本地有 tileset.json）验证两类服务并存
	dir3d := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir3d, "tileset.json"), []byte(`{"asset":{"version":"1.1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Create(&task.Task{
		ID: "svc-3d-1", Type: "osgb->3dtiles", Source: "/s", Output: dir3d,
		Status: task.StatusSucceeded, MinioPrefix: "tasks/svc-3d-1", Approved: true,
	}); err != nil {
		t.Fatal(err)
	}

	w := f.do(http.MethodGet, "/api/v1/services")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Services []struct {
			ID         string `json:"id"`
			TilesetURL string `json:"tileset_url"`
			WmtsURL    string `json:"wmts_url"`
			TmsURL     string `json:"tms_url"`
		} `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, s := range resp.Services {
		byID[s.ID] = i
	}
	img, ok := byID["img-task-1"]
	if !ok {
		t.Fatalf("image service missing: %s", w.Body.String())
	}
	sv := resp.Services[img]
	if sv.TilesetURL != "" {
		t.Fatalf("image task should not expose tileset_url: %q", sv.TilesetURL)
	}
	if !strings.Contains(sv.WmtsURL, "request=GetCapabilities") {
		t.Fatalf("wmts_url wrong: %q", sv.WmtsURL)
	}
	if sv.TmsURL != "/api/v1/tms/img-task-1/{z}/{x}/{y}.png" {
		t.Fatalf("tms_url wrong: %q", sv.TmsURL)
	}
	s3d, ok := byID["svc-3d-1"]
	if !ok {
		t.Fatalf("3d service missing: %s", w.Body.String())
	}
	if resp.Services[s3d].TilesetURL == "" || resp.Services[s3d].WmtsURL != "" {
		t.Fatalf("3d entry mismatch: %+v", resp.Services[s3d])
	}
}

func TestServicesListCache(t *testing.T) {
	f := newTileFixture(t)
	w1 := f.do(http.MethodGet, "/api/v1/services")
	if w1.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first list should MISS, got %s", w1.Header().Get("X-Cache"))
	}
	w2 := f.do(http.MethodGet, "/api/v1/services")
	if w2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second list should HIT, got %s", w2.Header().Get("X-Cache"))
	}
	if w2.Body.String() != w1.Body.String() {
		t.Fatal("cached body mismatch")
	}
}

func TestTilesWorkWithoutCache(t *testing.T) {
	// Cache 为 nil（Redis 不可用降级）：分发照常工作
	f := newTileFixture(t)
	f.srv.Cache = nil
	w := f.do(http.MethodGet, "/api/v1/wmts/1.0.0/img-task-1/default/WebMercatorQuad/0/0/0.png")
	if w.Code != http.StatusOK {
		t.Fatalf("no-cache status = %d, body=%s", w.Code, w.Body.String())
	}
}
