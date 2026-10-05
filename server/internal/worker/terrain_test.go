package worker

import (
	"path/filepath"
	"strings"
	"testing"

	"tangis/server/internal/queue"
)

// A5 地形任务派发测试：terrain->tiles 显式类型走 terrain2tiles 子命令，
// 且不被影像线（同为 .tif 源）误判接管。
func TestBuildArgvTerrain(t *testing.T) {
	dir := t.TempDir()
	msg := queue.TaskMessage{
		TaskID:       "t-terrain",
		Type:         "terrain->tiles",
		Source:       "/data/dem/mountain.tif",
		Output:       filepath.Join(dir, "out"),
		ManifestPath: filepath.Join(dir, "out", "manifest.json"),
	}
	w := newTestWorker(t, nil, "", &fakeExecutor{}, nil)

	if isImageTask(msg) {
		t.Fatal("terrain task must not be dispatched to the image line")
	}
	if !isTerrainTask(msg) {
		t.Fatal("terrain task should match the terrain line")
	}

	argv := w.buildArgv(msg)
	wantPrefix := []string{
		"terrain2tiles",
		"--source", "/data/dem/mountain.tif",
		"--output", msg.Output,
		"--layer-metadata", filepath.Join(msg.Output, "layer.json"),
	}
	if len(argv) < len(wantPrefix) {
		t.Fatalf("argv = %v, want prefix %v", argv, wantPrefix)
	}
	for i, a := range wantPrefix {
		if argv[i] != a {
			t.Fatalf("argv = %v, want prefix %v", argv, wantPrefix)
		}
	}
	// B7：末尾必须带取消标志参数
	if lastFlagArg(argv) != w.cancelFlagPath(msg.TaskID) {
		t.Errorf("terrain argv missing --cancel-file: %v", argv)
	}
	if strings.Join(argv, " ") != strings.Join(append(wantPrefix, "--cancel-file", w.cancelFlagPath(msg.TaskID)), " ") {
		t.Errorf("terrain argv = %v, want %v", argv, append(wantPrefix, "--cancel-file", w.cancelFlagPath(msg.TaskID)))
	}
}
