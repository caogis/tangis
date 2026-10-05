package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// TestDemDesensitizeArgv DEM 脱密任务（F-21 合规）：
// 走 desensitize-dem、区域 JSON 先落盘、不注入 --cancel-file（内核未实现该参数）。
func TestDemDesensitizeArgv(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	msg.Type = task.TypeDemDesensitize
	msg.Source = "/data/dem.tif"
	msg.Params = map[string]any{
		"region": map[string]any{"min_x": 116.0, "min_y": 39.0, "max_x": 116.1, "max_y": 39.1},
		"mode":   "noise",
		"delta":  "2.5", // 前端表单以字符串提交
		"seed":   "42",
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
	if args[0] != "desensitize-dem" {
		t.Fatalf("subcommand=%q, want desensitize-dem（argv=%v）", args[0], args)
	}
	flags := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			flags[args[i]] = args[i+1]
		}
	}
	for flag, want := range map[string]string{
		"--input":  "/data/dem.tif",
		"--mode":   "noise",
		"--delta":  "2.5",
		"--seed":   "42",
		"--out":    filepath.Join(msg.Output, "desensitized.tif"),
		"--record": filepath.Join(msg.Output, "desensitize-record.json"),
		"--region": filepath.Join(msg.Output, "region.json"),
	} {
		if flags[flag] != want {
			t.Errorf("%s = %q, want %q", flag, flags[flag], want)
		}
	}
	for _, a := range args {
		if a == "--cancel-file" {
			t.Error("desensitize-dem 不支持 --cancel-file，不得注入")
		}
	}

	// 区域 JSON 必须落盘且内容与请求一致
	data, err := os.ReadFile(filepath.Join(msg.Output, "region.json"))
	if err != nil {
		t.Fatalf("region.json 未落盘: %v", err)
	}
	var region map[string]any
	if err := json.Unmarshal(data, &region); err != nil {
		t.Fatalf("region.json 非法: %v", err)
	}
	if region["min_x"] != 116.0 || region["max_y"] != 39.1 {
		t.Errorf("region 内容不符: %v", region)
	}

	// 脱密任务不应产出 manifest.json
	if _, err := os.Stat(filepath.Join(msg.Output, "manifest.json")); err == nil {
		t.Error("脱密任务不应产出 manifest.json")
	}
}

// TestDemDesensitizeRequiresRegion 缺 params.region 时任务 FAILED 且原因可读（不静默跑错范围）。
func TestDemDesensitizeRequiresRegion(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	msg.Type = task.TypeDemDesensitize
	msg.Source = "/data/dem.tif"
	msg.Params = map[string]any{"mode": "flatten"}

	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExecutor{output: "should not run"}
	w := newTestWorker(t, store, "", fe, nil)
	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	cur, err := store.Get(msg.TaskID, "")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != task.StatusFailed {
		t.Fatalf("status=%s, want FAILED", cur.Status)
	}
	if !strings.Contains(cur.ErrMsg, "region") {
		t.Errorf("err_msg=%q, 应说明 region 缺失", cur.ErrMsg)
	}
	if len(fe.calls) != 0 {
		t.Errorf("内核不应被调用（参数不完整）: %v", fe.calls)
	}
}

// TestImageTaskNotHijackedByDemType 脱密任务源同为 .tif，
// isImageTask 必须靠显式类型把它排除，否则会落到影像切片线。
func TestImageTaskNotHijackedByDemType(t *testing.T) {
	if isImageTask(queue.TaskMessage{Type: task.TypeDemDesensitize, Source: "/a/dem.tif"}) {
		t.Error("dem->desensitized 不得被判为影像任务")
	}
	if isImageTask(queue.TaskMessage{Type: "terrain->tiles", Source: "/a/dem.tif"}) {
		t.Error("terrain->tiles 不得被判为影像任务")
	}
	if !isImageTask(queue.TaskMessage{Type: "image->tiles", Source: "/a/img.tif"}) {
		t.Error("image->tiles 应判为影像任务")
	}
	// 历史行为：未声明类型时按 .tif 扩展名推断
	if !isImageTask(queue.TaskMessage{Source: "/a/legacy.tif"}) {
		t.Error("无类型但源为 .tif 时应按影像处理（历史兼容）")
	}
	if !isDemDesensitizeTask(queue.TaskMessage{Type: "DEM->DESENSITIZED"}) {
		t.Error("类型判定应大小写不敏感")
	}
}

// TestFloatParam 浮点参数解析（字符串/数字/非法）。
func TestFloatParam(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want float64
		ok   bool
	}{
		{"字符串", map[string]any{"delta": "2.5"}, 2.5, true},
		{"数字", map[string]any{"delta": 3.25}, 3.25, true},
		{"负数", map[string]any{"delta": "-1.5"}, -1.5, true},
		{"非数字", map[string]any{"delta": "abc"}, 0, false},
		{"缺失", map[string]any{}, 0, false},
		{"nil", nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := floatParam(c.in, "delta")
			if ok != c.ok || (ok && got != c.want) {
				t.Errorf("got (%v,%v), want (%v,%v)", got, ok, c.want, c.ok)
			}
		})
	}
}
