package service

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/task"
)

// A5 地形服务分发测试：terrain->tiles 任务产物（layer.json + {z}/{x}/{y}.terrain）
// 经 /services 通道分发；服务列表暴露 terrain_url 入口。

func newTerrainFixture(t *testing.T) *fixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	files := map[string]string{
		"layer.json":           `{"tilejson":"2.1.0","format":"quantized-mesh-1.0","profile":"tms","tiles":["{z}/{x}/{y}.terrain?v=1.0.0"]}`,
		"12/3367/3368.terrain": "FAKE-TERRAIN-BYTES",
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := task.NewMemoryStore()
	tk := &task.Task{
		ID:          "terrain-task-1",
		Type:        task.TypeTerrain,
		Source:      "/data/dem/mountain.tif",
		Output:      dir,
		Status:      task.StatusSucceeded,
		MinioPrefix: "tasks/terrain-task-1",
		Approved:    true,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	// 未审批的对照任务
	tk2 := *tk
	tk2.ID = "terrain-task-2"
	tk2.Output = dir
	tk2.Approved = false
	if err := store.Create(&tk2); err != nil {
		t.Fatal(err)
	}

	getter := &fakeGetter{objects: map[string]string{
		// MinIO 回源：本地缺失的瓦片
		"tasks/terrain-task-1/12/3369/3370.terrain": "REMOTE-TERRAIN",
	}}

	s := &Server{Store: store, Objects: getter, Bucket: "tangis"}
	r := gin.New()
	r.GET("/api/v1/services", s.ListServices)
	r.GET("/services/:task_id/*path", s.ServeTile)

	f := &fixture{r: r, srv: s, store: store, getter: getter, outputDir: dir, taskID: tk.ID}
	return f
}

// TestTerrainServiceDiscovery 已发布地形任务应出现在服务列表且带 terrain_url，
// 不应误报 tileset/wmts 入口（产物探测不造假）。
func TestTerrainServiceDiscovery(t *testing.T) {
	f := newTerrainFixture(t)
	w := f.do(http.MethodGet, "/api/v1/services")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"terrain_url":"/services/terrain-task-1/layer.json"`) {
		t.Fatalf("services list missing terrain_url: %s", body)
	}
	if strings.Contains(body, `"tileset_url"`) || strings.Contains(body, `"wmts_url"`) {
		t.Errorf("terrain task should not advertise tileset/wmts: %s", body)
	}
	// 未审批任务不应出现
	if strings.Contains(body, "terrain-task-2") {
		t.Errorf("unapproved task must not be listed: %s", body)
	}
}

// TestServeTerrainLayerAndTiles layer.json 与 .terrain 瓦片分发：
// 本地优先、MinIO 回源、未审批 404、缺失瓦片 404。
func TestServeTerrainLayerAndTiles(t *testing.T) {
	f := newTerrainFixture(t)

	// layer.json
	w := f.do(http.MethodGet, "/services/terrain-task-1/layer.json")
	if w.Code != http.StatusOK {
		t.Fatalf("layer.json status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "quantized-mesh-1.0") {
		t.Errorf("layer.json body = %s", w.Body.String())
	}

	// 本地瓦片
	w = f.do(http.MethodGet, "/services/terrain-task-1/12/3367/3368.terrain")
	if w.Code != http.StatusOK {
		t.Fatalf("tile status = %d", w.Code)
	}
	if w.Body.String() != "FAKE-TERRAIN-BYTES" {
		t.Errorf("tile body = %q", w.Body.String())
	}

	// MinIO 回源瓦片（本地缺失）
	w = f.do(http.MethodGet, "/services/terrain-task-1/12/3369/3370.terrain")
	if w.Code != http.StatusOK || w.Body.String() != "REMOTE-TERRAIN" {
		t.Fatalf("remote tile: status=%d body=%q", w.Code, w.Body.String())
	}

	// 不存在的瓦片
	w = f.do(http.MethodGet, "/services/terrain-task-1/12/1/1.terrain")
	if w.Code != http.StatusNotFound {
		t.Errorf("missing tile status = %d, want 404", w.Code)
	}

	// 未审批任务一律 404（不泄露产物存在性）
	w = f.do(http.MethodGet, "/services/terrain-task-2/layer.json")
	if w.Code != http.StatusNotFound {
		t.Errorf("unapproved layer.json status = %d, want 404", w.Code)
	}
}
