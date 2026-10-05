package worker

import (
	"path/filepath"
	"strings"
	"testing"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// F-03：影像任务（.tif/.tiff 源或 image->tiles 类型）调用内核时须携带
// 影像瓦片金字塔参数；内核不支持这些参数时会以非零退出失败，
// 任务 FAILED 带明确原因（不伪造产物）。

func TestIsImageTask(t *testing.T) {
	cases := []struct {
		msg  queue.TaskMessage
		want bool
	}{
		{queue.TaskMessage{Type: "image->tiles", Source: "/d/img"}, true},
		{queue.TaskMessage{Type: "osgb->3dtiles", Source: "/d/dem.tif"}, true},
		{queue.TaskMessage{Type: "osgb->3dtiles", Source: "/d/dem.TIFF"}, true},
		{queue.TaskMessage{Type: "osgb->3dtiles", Source: "/d/town"}, false},
		{queue.TaskMessage{Type: "osgb->3dtiles", Source: "/d/tiff.bak"}, false},
	}
	for i, c := range cases {
		if got := isImageTask(c.msg); got != c.want {
			t.Fatalf("case %d: isImageTask(%+v) = %v, want %v", i, c.msg, got, c.want)
		}
	}
}

func TestImageTaskKernelArgs(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	msg.Type = "image->tiles"
	msg.Source = "/data/img/dem.tif"
	store := task.NewMemoryStore()
	tk := newPendingTask(msg)
	tk.Type = msg.Type
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{output: "image tiling not supported yet"}
	w := newTestWorker(t, store, "", fe, nil)
	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(fe.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(fe.calls))
	}
	args := fe.calls[0][1].([]string)
	// 影像任务走 raster2tiles 子命令（rhWkvH），而非 build
	if args[0] != "raster2tiles" {
		t.Fatalf("image task should invoke raster2tiles, got %v", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--source-type image", "--pyramid-layout xyz"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("image kernel args missing %q: %v", want, args)
		}
	}
	// 金字塔元数据路径约定：output/tiles/metadata.json
	if !strings.Contains(joined, filepath.Join(msg.Output, "tiles", "metadata.json")) {
		t.Fatalf("metadata path arg wrong: %v", args)
	}
	// 源与输出直传
	if !strings.Contains(joined, "--source "+msg.Source) || !strings.Contains(joined, "--output "+msg.Output) {
		t.Fatalf("source/output args missing: %v", args)
	}
}

func TestOsgbTaskHasNoImageArgs(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{output: "ok"}
	w := newTestWorker(t, store, "", fe, nil)
	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	args := fe.calls[0][1].([]string)
	for _, a := range args {
		if strings.HasPrefix(a, "--source-type") || strings.HasPrefix(a, "--pyramid") {
			t.Fatalf("osgb task should not carry image args: %v", args)
		}
	}
}

func TestKernelArgsOverrideWins(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	msg.Type = "image->tiles"
	msg.Source = "/data/img/dem.tif"
	store := task.NewMemoryStore()
	tk := newPendingTask(msg)
	tk.Type = msg.Type
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	fe := &fakeExecutor{output: "ok"}
	w := New(store, fe, "", "tile {manifest_path} --out {output_dir}", nil)
	w.DataDir = t.TempDir()
	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	args := fe.calls[0][1].([]string)
	// KERNEL_ARGS 完全接管：不追加影像参数
	for _, a := range args {
		if a == "--source-type" {
			t.Fatalf("KERNEL_ARGS override should suppress image args: %v", args)
		}
	}
	if args[0] != "tile" || args[1] != msg.ManifestPath {
		t.Fatalf("KERNEL_ARGS not applied: %v", args)
	}
}
