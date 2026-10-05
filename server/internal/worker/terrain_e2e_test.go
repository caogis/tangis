package worker

import (
	"os"
	"path/filepath"
	"testing"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// kernelBinPath 定位 Rust 内核二进制：TANGIS_KERNEL_BIN 环境变量优先，
// 缺省探测仓库内 cargo 构建产物；不存在时测试跳过（CI 装机差异容忍）。
func kernelBinPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("TANGIS_KERNEL_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	candidates := []string{
		"../../../kernel/target/debug/tangis-kernel",
		"../../../kernel/target/release/tangis-kernel",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("kernel binary not found (set TANGIS_KERNEL_BIN or cargo build -p tangis-kernel)")
	return ""
}

// repoDEMPath 定位仓库自带 DEM 语料。
func repoDEMPath(t *testing.T) string {
	t.Helper()
	p := "../../../testdata/geo/dem_cgcs2000_u16.tif"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	t.Skip("test DEM not found")
	return ""
}

// TestTerrainTaskEndToEndWithRealKernel 跨栈契约冒烟（A5 + B2 + B7）：
// terrain->tiles 任务经真实内核 terrain2tiles 执行 → SUCCEEDED，
// 产物 layer.json + {z}/{x}/{y}.terrain 落盘，服务发现可用。
func TestTerrainTaskEndToEndWithRealKernel(t *testing.T) {
	bin := kernelBinPath(t)
	dem := repoDEMPath(t)

	dir := t.TempDir()
	msg := queue.TaskMessage{
		TaskID: "terrain-e2e-1",
		Type:   "terrain->tiles",
		Source: dem,
		Output: filepath.Join(dir, "out"),
	}
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	w := New(store, NewCmdExecutor(), bin, "", nil)
	w.DataDir = dir

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	cur, err := store.Get(msg.TaskID, "")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != task.StatusSucceeded {
		t.Fatalf("status = %s (err=%s), want SUCCEEDED", cur.Status, cur.ErrMsg)
	}
	for _, rel := range []string{"layer.json", "12/3367/2544.terrain"} {
		if _, err := os.Stat(filepath.Join(msg.Output, filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing artifact %s: %v", rel, err)
		}
	}
	// terrain 任务不得产出 manifest.json（跳过 ensureManifest）
	if _, err := os.Stat(filepath.Join(msg.Output, "manifest.json")); err == nil {
		t.Error("terrain task should not produce manifest.json")
	}
}
