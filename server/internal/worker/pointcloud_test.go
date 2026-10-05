package worker

import (
	"os"
	"path/filepath"
	"testing"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// TestPointCloudArgv LAS 点云任务必须走 las2pnts，且**不得**追加 --cancel-file
// （内核对 las2pnts 未实现该参数，追加会让内核启动即报错）。
func TestPointCloudArgv(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	msg.Type = task.TypePointCloud
	msg.Source = "/data/cloud.las"

	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExecutor{output: "ok"}
	w := newTestWorker(t, store, "", fe, nil)

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(fe.calls))
	}
	args := fe.calls[0][1].([]string)
	want := []string{"las2pnts", "--source", "/data/cloud.las", "--output", msg.Output}
	if len(args) != len(want) {
		t.Fatalf("argv = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("argv = %v, want %v", args, want)
		}
	}
	// 点云任务不应产出 manifest.json（las2pnts 不消费，且会污染产物目录）
	if _, err := os.Stat(filepath.Join(msg.Output, "manifest.json")); err == nil {
		t.Error("point cloud task should not produce manifest.json")
	}
}

// TestPointCloudArgvWithParams 前端表单以**字符串**提交参数（origin、max_points_per_tile），
// 后端需正确解析为内核参数（否则地理定位静默失效）。
func TestPointCloudArgvWithParams(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	msg.Type = task.TypePointCloud
	msg.Source = "/data/cloud.las"
	msg.Params = map[string]any{
		"origin":              "116.39,39.90,12.5",
		"max_points_per_tile": "80000",
	}

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
	got := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--origin" {
			got["origin"] = args[i+1] + "," + args[i+2] + "," + args[i+3]
		}
		if args[i] == "--max-points-per-tile" {
			got["max"] = args[i+1]
		}
	}
	if got["origin"] != "116.39,39.9,12.5" {
		t.Errorf("--origin = %q, want 116.39,39.9,12.5（字符串参数未解析）", got["origin"])
	}
	if got["max"] != "80000" {
		t.Errorf("--max-points-per-tile = %q, want 80000", got["max"])
	}
}

// TestImageryTerrainKeepCancelFlag 回归：影像与地形线路仍需注入 --cancel-file（B7 优雅取消）。
func TestImageryTerrainKeepCancelFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		msgType string
		source  string
		sub     string
	}{
		{"影像", "image->tiles", "/data/a.tif", "raster2tiles"},
		{"地形", "terrain->tiles", "/data/dem.tif", "terrain2tiles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			msg := newTestMsg(dir)
			msg.Type = tc.msgType
			msg.Source = tc.source

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
			if args[0] != tc.sub {
				t.Fatalf("subcommand = %q, want %q", args[0], tc.sub)
			}
			found := false
			for _, a := range args {
				if a == "--cancel-file" {
					found = true
				}
			}
			if !found {
				t.Errorf("%s 线路应注入 --cancel-file（协作式取消），argv=%v", tc.name, args)
			}
		})
	}
}

// TestOriginParam 参数解析：数组 / 逗号字符串 / 非法输入。
func TestOriginParam(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want [3]float64
		ok   bool
	}{
		{"数组", map[string]any{"origin": []any{116.39, 39.9, 0.0}}, [3]float64{116.39, 39.9, 0}, true},
		{"字符串", map[string]any{"origin": "116.39,39.90,12.5"}, [3]float64{116.39, 39.9, 12.5}, true},
		{"字符串含空格", map[string]any{"origin": " 116.39 , 39.9 , 0 "}, [3]float64{116.39, 39.9, 0}, true},
		{"字符串个数不对", map[string]any{"origin": "116.39,39.9"}, [3]float64{}, false},
		{"字符串非数字", map[string]any{"origin": "a,b,c"}, [3]float64{}, false},
		{"数组个数不对", map[string]any{"origin": []any{1.0, 2.0}}, [3]float64{}, false},
		{"数组元素非数字", map[string]any{"origin": []any{"1", 2.0, 3.0}}, [3]float64{}, false},
		{"缺失", map[string]any{}, [3]float64{}, false},
		{"nil", nil, [3]float64{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := originParam(c.in)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (got %v)", ok, c.ok, got)
			}
			if ok && got != c.want {
				t.Errorf("origin = %v, want %v", got, c.want)
			}
		})
	}
}

// TestPositiveIntParam 正整数参数解析（字符串/数字/非法）。
func TestPositiveIntParam(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want int
		ok   bool
	}{
		{"字符串", map[string]any{"max_points_per_tile": "80000"}, 80000, true},
		{"数字", map[string]any{"max_points_per_tile": float64(5000)}, 5000, true},
		{"小数数字", map[string]any{"max_points_per_tile": 1.5}, 0, false},
		{"零", map[string]any{"max_points_per_tile": "0"}, 0, false},
		{"负数", map[string]any{"max_points_per_tile": float64(-1)}, 0, false},
		{"非数字", map[string]any{"max_points_per_tile": "abc"}, 0, false},
		{"缺失", map[string]any{}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := positiveIntParam(c.in, "max_points_per_tile")
			if ok != c.ok || (ok && got != c.want) {
				t.Errorf("got (%d,%v), want (%d,%v)", got, ok, c.want, c.ok)
			}
		})
	}
}

// TestIsPointCloudTask 类型判定（大小写不敏感，不按扩展名推断）。
func TestIsPointCloudTask(t *testing.T) {
	if !isPointCloudTask(queue.TaskMessage{Type: task.TypePointCloud}) {
		t.Error("las->3dtiles 应判定为点云任务")
	}
	if !isPointCloudTask(queue.TaskMessage{Type: "LAS->3DTiles"}) {
		t.Error("类型判定应大小写不敏感")
	}
	if isPointCloudTask(queue.TaskMessage{Type: "osgb->3dtiles", Source: "/a/cloud.las"}) {
		t.Error("不应按扩展名把切片任务误判为点云任务")
	}
}
