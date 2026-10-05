// Package worker 实现任务消费者：NATS 消息 -> 内核 CLI -> 状态回写。
//
// 处理流程（PRD F-04 任务状态机）：
//  1. 收到 TASKS.created 消息，任务置 RUNNING；
//  2. 确保 manifest.json 存在（不存在则生成最小骨架，供内核 M1 模拟执行）；
//  3. exec 调用内核 CLI 执行清单（默认 `build <manifest> --out <output>`，产物
//     与 manifest.json 均落在 output 目录内，可用 KERNEL_ARGS 透传兜底），
//     执行期间周期解析 manifest 回传分块进度，stdout/stderr 落盘日志；
//  4. 成功后将 output 全部产物上传 MinIO（不可用则告警降级，不阻塞）；
//  5. 置 SUCCEEDED；失败则指数退避自动重试（默认 10s/60s/300s 共 3 次，
//     重试次数记入 Task.Attempts），超限置 FAILED（均为终态，Ack 不重投）。
//
// 外部命令执行抽象为 Executor 接口、产物上传抽象为 Uploader 接口，
// 单测用 fake 替代真实进程/对象存储。
package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tangis/server/internal/objectstore"
	"tangis/server/internal/queue"
	"tangis/server/internal/task"
	"tangis/server/internal/webhook"
)

// 内核 CLI 对接约定：
//   - KERNEL_BIN：内核可执行文件路径，默认 ../kernel/target/debug/tangis-kernel；
//   - 默认 argv：`build <manifest> --out <dir> --task-id <id>`（产出 3DTiles 产物），
//     <dir> 即任务 output 目录本身：产物与 manifest.json 均落在 output 内；
//   - KERNEL_ARGS：完整透传兜底（空格分隔），支持占位符
//     {task_id} {manifest_path} {output_dir}，设置后完全替代默认 argv。
const (
	DefaultKernelBin  = "../kernel/target/debug/tangis-kernel"
	defaultWorkDir    = "" // 继承 apiserver 工作目录，KERNEL_BIN 相对路径以此为基准
	manifestChunkSoft = 4096
)

// 默认失败重试策略（F-04）：最多 3 次重试，指数退避。
var DefaultRetryBackoff = []time.Duration{10 * time.Second, 60 * time.Second, 300 * time.Second}

// 默认进度轮询间隔：内核运行期间周期解析 manifest 回传进度。
const DefaultProgressInterval = 2 * time.Second

// Executor 外部命令执行接口，隔离 os/exec 便于 mock。
type Executor interface {
	// Run 执行命令并返回合并输出（stdout+stderr）；非零退出返回 error。
	Run(name string, args []string) (string, error)
}

// CancelableExecutor Executor 的可选增强：按任务登记运行中的进程并支持中断，
// 供 F-04 任务取消/暂停使用。未实现该接口的执行器不杀进程，只做状态流转。
type CancelableExecutor interface {
	Executor
	// RunKeyed 同 Run，但以 key（任务 ID）登记进程，可被 Kill 中断。
	RunKeyed(key, name string, args []string) (string, error)
	// Kill 中断 key 对应的进程；返回是否命中在跑的进程。
	Kill(key string) bool
}

// ProcessProbe CancelableExecutor 的可选增强：查询进程是否仍在运行，
// 供 B7 协作式取消的宽限期等待使用。未实现时跳过优雅等待，直接 Kill。
type ProcessProbe interface {
	// Running 报告 key 对应的进程是否仍在运行。
	Running(key string) bool
}

// CmdExecutor CancelableExecutor 的真实实现。
// 必须经 NewCmdExecutor 构造：内部登记表需要预初始化。
type CmdExecutor struct {
	mu      sync.Mutex
	running map[string]*os.Process
}

// NewCmdExecutor 构造真实执行器（带进程登记表）。
func NewCmdExecutor() *CmdExecutor {
	return &CmdExecutor{running: map[string]*os.Process{}}
}

// Run 实现 Executor（不登记，无法中断）。
func (e *CmdExecutor) Run(name string, args []string) (string, error) {
	return e.RunKeyed("", name, args)
}

// RunKeyed 实现 CancelableExecutor：用 Start/Wait 替代 Run，以便持有进程句柄。
func (e *CmdExecutor) RunKeyed(key, name string, args []string) (string, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return buf.String(), err
	}
	if key != "" {
		e.mu.Lock()
		if e.running == nil {
			e.running = map[string]*os.Process{}
		}
		e.running[key] = cmd.Process
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			delete(e.running, key)
			e.mu.Unlock()
		}()
	}
	err := cmd.Wait()
	return buf.String(), err
}

// Kill 实现 CancelableExecutor：终止进程；返回是否找到在跑的进程。
func (e *CmdExecutor) Kill(key string) bool {
	e.mu.Lock()
	p := e.running[key]
	e.mu.Unlock()
	if p == nil {
		return false
	}
	return p.Kill() == nil
}

// Running 实现 ProcessProbe：报告 key 对应进程是否仍在运行。
func (e *CmdExecutor) Running(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[key] != nil
}

// Worker 任务消费者。
type Worker struct {
	Store      task.Store
	Exec       Executor
	KernelBin  string               // KERNEL_BIN，空则取默认
	KernelArgs []string             // KERNEL_ARGS 透传兜底（已按占位符拆分前的模板），非空则替代默认 argv
	Uploader   objectstore.Uploader // 产物上传器（MinIO），可为 nil：跳过上传，任务仍可成功

	// Publisher 失败重试的重新入队通道，可为 nil（禁用自动重试）。
	Publisher queue.Publisher
	// DataDir 本地数据目录（内核日志落盘 data/logs/{task_id}.log），空取 "data"。
	DataDir string
	// RetryBackoff 失败重试退避序列，长度即最大重试次数；空取 DefaultRetryBackoff。
	RetryBackoff []time.Duration
	// ProgressInterval 内核运行期间的 manifest 进度轮询间隔；零取 DefaultProgressInterval。
	ProgressInterval time.Duration
	// Notifier 任务终态 webhook 通知器（F-14 第一步），可为 nil（禁用）。
	// 任务进入 SUCCEEDED/FAILED 且 params.webhook_url 非空时异步 POST 事件，
	// 结果只记日志，不阻塞、不影响任务状态。
	Notifier *webhook.Notifier
	// CancelGracePeriod B7 协作式取消宽限期：Interrupt 先写内核 --cancel-file
	// 标志文件（内核在分块边界安全退出，journal 保留），等待进程在该时限内
	// 自行退出；超时仍存活才 Kill 兜底。零取 DefaultCancelGracePeriod。
	CancelGracePeriod time.Duration

	// mu / interrupted 跟踪被用户取消或暂停而中断的任务：内核进程被杀后必然
	// 返回非零退出码，若无此标记会被 retryOrFail 误判为执行失败而自动重试，
	// 从而把用户主动取消变成"重跑一遍"。
	mu          sync.Mutex
	interrupted map[string]bool
}

// DefaultCancelGracePeriod B7 协作式取消默认宽限期：内核在分块边界检查
// 取消标志并安全退出的最长等待时间。
const DefaultCancelGracePeriod = 3 * time.Second

// cancelFlagPath 内核 --cancel-file 标志文件的落盘路径
// （data/cancels/{task_id}.flag，与产物目录隔离，避免混入 MinIO 上传）。
func (w *Worker) cancelFlagPath(taskID string) string {
	return filepath.Join(w.dataDir(), "cancels", taskID+".flag")
}

// requestCooperativeCancel 写取消标志文件；返回是否写入成功。
func (w *Worker) requestCooperativeCancel(taskID string) bool {
	flag := w.cancelFlagPath(taskID)
	if err := os.MkdirAll(filepath.Dir(flag), 0o755); err != nil {
		return false
	}
	return os.WriteFile(flag, nil, 0o644) == nil
}

// Interrupt 中断任务正在执行的内核进程（F-04 取消/暂停用）。
//
// B7 协作式取消优先：先写内核 --cancel-file 标志文件，内核在**分块边界**
// 安全退出（已完成分块保留在 journal、断点续切可恢复、exit 130），
// 宽限期内未退出再 Kill 兜底（journal 逐块追加对进程强杀同样崩溃安全）。
// 返回 false 表示当前没有在跑的进程（任务可能仍在排队，或已执行结束）。
func (w *Worker) Interrupt(taskID string) bool {
	ce, ok := w.Exec.(CancelableExecutor)
	if !ok {
		return false
	}

	// 优雅路径：标志文件 → 等待内核自行退出（须先观测到"在跑"再消失，
	// 避免把恰好自行结束的进程误标为被中断）
	graceful := false
	if w.requestCooperativeCancel(taskID) {
		if probe, pok := ce.(ProcessProbe); pok {
			graceful = w.awaitKernelExit(probe, taskID)
		}
	}

	if !graceful && !ce.Kill(taskID) {
		// 无在跑进程：清掉可能残留的标志（resume/重试不得被误取消）
		os.Remove(w.cancelFlagPath(taskID))
		return false
	}

	w.mu.Lock()
	if w.interrupted == nil {
		w.interrupted = map[string]bool{}
	}
	w.interrupted[taskID] = true
	w.mu.Unlock()
	os.Remove(w.cancelFlagPath(taskID))
	return true
}

// awaitKernelExit 在宽限期内轮询进程状态；须先观测到"在跑"再消失才视为
// 内核已响应协作取消退出。
func (w *Worker) awaitKernelExit(probe ProcessProbe, taskID string) bool {
	grace := w.CancelGracePeriod
	if grace <= 0 {
		grace = DefaultCancelGracePeriod
	}
	deadline := time.Now().Add(grace)
	sawRunning := false
	for time.Now().Before(deadline) {
		if probe.Running(taskID) {
			sawRunning = true
		} else if sawRunning {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// takeInterrupted 消费中断标记：报告该任务是否刚被用户中断（取出即清除）。
func (w *Worker) takeInterrupted(taskID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.interrupted[taskID] {
		delete(w.interrupted, taskID)
		return true
	}
	return false
}

// runKernel 调用内核执行器：支持中断的执行器按任务 ID 登记进程句柄。
func (w *Worker) runKernel(taskID, bin string, argv []string) (string, error) {
	if ce, ok := w.Exec.(CancelableExecutor); ok {
		return ce.RunKeyed(taskID, bin, argv)
	}
	return w.Exec.Run(bin, argv)
}

// New 创建 Worker；kernelBin/kernelArgs 由调用方从 env 解析注入，便于测试；
// uploader 允许为 nil（MinIO 不可用降级：不上传，任务照常 SUCCEEDED）。
// 其余可选依赖（Publisher/DataDir/RetryBackoff）在 New 之后按需赋值。
func New(store task.Store, exec Executor, kernelBin string, kernelArgs string, uploader objectstore.Uploader) *Worker {
	if kernelBin == "" {
		kernelBin = DefaultKernelBin
	}
	return &Worker{Store: store, Exec: exec, KernelBin: kernelBin, KernelArgs: splitArgs(kernelArgs), Uploader: uploader}
}

// splitArgs 按空白拆分 KERNEL_ARGS（不支持引号嵌套，M1 够用）；空串返回 nil。
func splitArgs(s string) []string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// buildArgv 组装内核命令行：KERNEL_ARGS 优先（占位符替换），否则默认
// `build <manifest> --out <dir> --task-id <id>`。
// 路径约定：output 目录即产物目录，--out 直接传 output 本身（不取父目录），
// manifest.json 也位于 output 目录内。
// 影像任务（F-03：.tif/.tiff 源或 image->tiles 类型）追加 --source-type image
// 与瓦片金字塔参数；内核暂不支持时会以非零退出失败，任务 FAILED 带明确原因，
// worker 不伪造产物。
func (w *Worker) buildArgv(msg queue.TaskMessage) []string {
	if len(w.KernelArgs) > 0 {
		argv := make([]string, len(w.KernelArgs))
		for i, a := range w.KernelArgs {
			a = strings.ReplaceAll(a, "{task_id}", msg.TaskID)
			a = strings.ReplaceAll(a, "{manifest_path}", msg.ManifestPath)
			a = strings.ReplaceAll(a, "{output_dir}", msg.Output)
			a = strings.ReplaceAll(a, "{cancel_file}", w.cancelFlagPath(msg.TaskID))
			argv[i] = a
		}
		return argv
	}
	argv := []string{"build", msg.ManifestPath, "--out", msg.Output, "--task-id", msg.TaskID}
	if o, ok := originParam(msg.Params); ok {
		// D1：地理定位。params.origin = [lon, lat, height]（度/米），
		// 内核生成 root.transform（ENU→ECEF），Cesium 中瓦片落到真实位置。
		argv = append(argv, "--origin",
			strconv.FormatFloat(o[0], 'f', -1, 64),
			strconv.FormatFloat(o[1], 'f', -1, 64),
			strconv.FormatFloat(o[2], 'f', -1, 64),
		)
	}
	if isImageTask(msg) {
		// 影像任务走 raster2tiles 子命令（rhWkvH）：GeoTIFF → XYZ 金字塔，
		// 产物 {output}/tiles/{z}/{x}/{y}.png + metadata.json（F-03 server 约定）。
		argv = []string{
			"raster2tiles",
			"--source", msg.Source,
			"--output", msg.Output,
			"--source-type", "image",
			"--pyramid-layout", "xyz", // {z}/{x}/{y}，Y 原点在顶部
			"--pyramid-metadata", filepath.Join(msg.Output, "tiles", "metadata.json"),
		}
	} else if isTerrainTask(msg) {
		// 地形任务走 terrain2tiles 子命令（A5/B2）：GeoTIFF DEM →
		// Cesium quantized-mesh，产物 {output}/{z}/{x}/{y}.terrain + layer.json，
		// 审批后经 /services/{id}/layer.json 分发。
		argv = []string{
			"terrain2tiles",
			"--source", msg.Source,
			"--output", msg.Output,
			"--layer-metadata", filepath.Join(msg.Output, "layer.json"),
		}
		// 层级范围由前端表单提供（内核支持，缺省由内核按分辨率推导）
		if n, ok := positiveIntParam(msg.Params, "min_zoom"); ok {
			argv = append(argv, "--min-zoom", strconv.Itoa(n))
		}
		if n, ok := positiveIntParam(msg.Params, "max_zoom"); ok {
			argv = append(argv, "--max-zoom", strconv.Itoa(n))
		}
	} else if isPointCloudTask(msg) {
		// LAS 点云任务走 las2pnts 子命令：LAS 1.0–1.4 → tiles/{i}.pnts +
		// tileset.json，复用 3D Tiles 审批与分发链路。
		//
		// ⚠️ 内核对 las2pnts **未实现 --cancel-file**（仅 build/run/raster2tiles/
		// terrain2tiles 有此参数），故此处**提前返回**：若走到下面的取消参数追加，
		// 内核会因未知参数直接报错。代价是取消点云任务只能靠 Kill 兜底，
		// 无法像其它线路那样在分块边界优雅退出。
		argv = []string{
			"las2pnts",
			"--source", msg.Source,
			"--output", msg.Output,
		}
		if o, ok := originParam(msg.Params); ok {
			argv = append(argv, "--origin",
				strconv.FormatFloat(o[0], 'f', -1, 64),
				strconv.FormatFloat(o[1], 'f', -1, 64),
				strconv.FormatFloat(o[2], 'f', -1, 64),
			)
		}
		if n, ok := positiveIntParam(msg.Params, "max_points_per_tile"); ok {
			argv = append(argv, "--max-points-per-tile", strconv.Itoa(n))
		}
		return argv
	} else if isDemDesensitizeTask(msg) {
		// DEM 区域脱密（F-21 合规）：产物 desensitized.tif + 脱密留痕 JSON。
		// 区域 JSON 由 runBuild 事先落盘（见 writeRegionFile）。
		// 与地形/点云同理，内核对该子命令未实现 --cancel-file，故提前返回。
		mode := "flatten"
		if m, ok := msg.Params["mode"].(string); ok && strings.TrimSpace(m) != "" {
			mode = strings.TrimSpace(m)
		}
		argv = []string{
			"desensitize-dem",
			"--input", msg.Source,
			"--region", filepath.Join(msg.Output, "region.json"),
			"--mode", mode,
			"--out", filepath.Join(msg.Output, "desensitized.tif"),
			"--record", filepath.Join(msg.Output, "desensitize-record.json"),
		}
		if f, ok := floatParam(msg.Params, "delta"); ok {
			argv = append(argv, "--delta", strconv.FormatFloat(f, 'f', -1, 64))
		}
		if n, ok := positiveIntParam(msg.Params, "seed"); ok {
			argv = append(argv, "--seed", strconv.Itoa(n))
		}
		return argv
	}
	// B7 协作式取消：注入标志文件路径（仅上述支持该参数的子命令会走到这里），
	// Interrupt 优雅取消优先于 Kill。
	argv = append(argv, "--cancel-file", w.cancelFlagPath(msg.TaskID))
	return argv
}

// isImageTask 任务是否为影像瓦片转换线（F-03）：
// type 为 image->tiles，或源文件扩展名为 .tif/.tiff。
func isImageTask(msg queue.TaskMessage) bool {
	if strings.EqualFold(msg.Type, "terrain->tiles") {
		return false // 地形任务显式类型优先（源同为 .tif，避免误入影像线）
	}
	if strings.EqualFold(msg.Type, task.TypeDemDesensitize) {
		return false // DEM 脱密任务源同为 .tif，同样不得误入影像线
	}
	if strings.EqualFold(msg.Type, "image->tiles") {
		return true
	}
	switch strings.ToLower(filepath.Ext(msg.Source)) {
	case ".tif", ".tiff":
		return true
	}
	return false
}

// isTerrainTask 任务是否为 DEM 地形转换线（A5/B2）：显式类型 terrain->tiles。
// 刻意不按扩展名推断（.tif 也可能是影像），创建任务时必须显式声明类型。
func isTerrainTask(msg queue.TaskMessage) bool {
	return strings.EqualFold(msg.Type, "terrain->tiles")
}

// isPointCloudTask 任务是否为 LAS 点云转换线：显式类型 las->3dtiles。
// 与地形同理，不按扩展名推断（避免与模型/影像线混淆）。
func isPointCloudTask(msg queue.TaskMessage) bool {
	return strings.EqualFold(msg.Type, "las->3dtiles")
}

// isDemDesensitizeTask 任务是否为 DEM 区域脱密线（F-21 合规）：显式类型 dem->desensitized。
// 源同样是 .tif，必须靠显式类型与影像线区分。
func isDemDesensitizeTask(msg queue.TaskMessage) bool {
	return strings.EqualFold(msg.Type, task.TypeDemDesensitize)
}

// floatParam 提取浮点参数（兼容前端字符串提交与 API 数字提交）。
func floatParam(params map[string]any, key string) (float64, bool) {
	if params == nil {
		return 0, false
	}
	switch v := params[key].(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// writeRegionFile 把 params.region 落盘为内核 desensitize-dem 需要的区域 JSON。
// 支持矩形 {"min_x","min_y","max_x","max_y"} 与多边形 {"ring":[[x,y],...]}。
func writeRegionFile(msg queue.TaskMessage) error {
	raw, ok := msg.Params["region"]
	if !ok || raw == nil {
		return fmt.Errorf("params.region 缺失（矩形 {min_x,min_y,max_x,max_y} 或多边形 {ring:[[x,y],...]}）")
	}
	// 兼容两种提交形态：结构化对象（推荐）与 JSON 字符串（表单直传）
	var region any = raw
	if s, isStr := raw.(string); isStr {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			return fmt.Errorf("region 字符串不是合法 JSON: %w", err)
		}
		region = parsed
	}
	data, err := json.Marshal(region)
	if err != nil {
		return fmt.Errorf("region 不是合法 JSON: %w", err)
	}
	p := filepath.Join(msg.Output, "region.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// originParam 提取 params.origin 地理定位参数。
//
// 接受两种写法（前端表单只提交字符串，API 调用方常用数组）：
//
//	[lon, lat, height]            JSON 数组
//	"lon,lat,height"              逗号分隔字符串（前端 ConvertView 提交形态）
//
// 缺失/格式非法返回 ok=false，由内核侧对越界值做最终校验。
func originParam(params map[string]any) ([3]float64, bool) {
	var out [3]float64
	if params == nil {
		return out, false
	}
	switch raw := params["origin"].(type) {
	case []any:
		if len(raw) != 3 {
			return out, false
		}
		for i, v := range raw {
			f, ok := v.(float64)
			if !ok {
				return out, false
			}
			out[i] = f
		}
		return out, true
	case string:
		parts := strings.Split(raw, ",")
		if len(parts) != 3 {
			return out, false
		}
		for i, p := range parts {
			f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil {
				return out, false
			}
			out[i] = f
		}
		return out, true
	}
	return out, false
}

// positiveIntParam 提取正整数参数（如 max_points_per_tile）。
// 兼容前端字符串提交与 API 数字提交两种形态；非正数/非整数返回 ok=false
// （交给内核用默认值，不传非法参数）。
func positiveIntParam(params map[string]any, key string) (int, bool) {
	if params == nil {
		return 0, false
	}
	var n int
	var err error
	switch v := params[key].(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		n = int(v)
	case string:
		n, err = strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}

// Handle queue.Subscribe 的回调：处理一条任务消息。
// 返回 nil 即 Ack；仅消息处理过程中的暂时性错误返回 error 触发 Nak 重投。
// 任务本身执行失败是终态（FAILED 或重试调度），同样 Ack。
func (w *Worker) Handle(msg queue.TaskMessage) error {
	t, err := w.Store.Get(msg.TaskID, "")
	if err != nil {
		if err == task.ErrNotFound {
			// 任务已被删除：丢弃消息
			return nil
		}
		return fmt.Errorf("worker: load task %s: %w", msg.TaskID, err)
	}
	// 幂等：仅 PENDING 才执行，重复投递/已终结任务直接跳过
	if t.Status != task.StatusPending {
		return nil
	}

	t.Status = task.StatusRunning
	t.Progress = nil
	if err := w.Store.Update(t); err != nil {
		return fmt.Errorf("worker: mark running %s: %w", msg.TaskID, err)
	}

	// 编辑任务分支（M2-F09c）：无 manifest/进度概念，直接按 CLI 契约执行
	// 内核 edit；内核失败沿用 retryOrFail 既有退避，参数非法/ops.json 不可
	// 解析直接 FAILED（重试无意义）。产物上传与成功回写走下方公共链路。
	if isEditTask(t) {
		retriable, eerr := w.runEdit(t, msg)
		if eerr != nil {
			// 用户取消/暂停导致的中断：状态已由 API 侧写入，不重试不置 FAILED
			if w.takeInterrupted(t.ID) {
				return nil
			}
			if retriable {
				w.retryOrFail(t, msg, eerr.Error())
			} else {
				w.fail(t, eerr.Error())
			}
			return nil
		}
	} else if !w.runBuild(t, msg) {
		// 准备失败或内核执行失败：终态已由 fail/retryOrFail 回写
		return nil
	}

	// 质检接线（M2-F08b）：params.qc=true 时切片成功后对源目录跑内核 qc，
	// 结果写入 QcStatus/QcSummary；qc 报告与产物一并上传 MinIO。
	// qc 失败（内核报错/报告不可解析）任务置 FAILED 带原因，不静默。
	if qcEnabled(t.Params) {
		if qerr := w.runQC(t, msg); qerr != nil {
			if w.takeInterrupted(t.ID) {
				return nil
			}
			w.fail(t, qerr.Error())
			return nil
		}
	}

	// 内核成功后上传产物到 MinIO（key 前缀 tasks/{task_id}/...）。
	// 上传失败仅告警降级：任务仍 SUCCEEDED，不阻塞主链路。
	if w.Uploader != nil {
		prefix := path.Join("tasks", msg.TaskID)
		if uerr := w.Uploader.UploadDir(context.Background(), msg.Output, prefix); uerr != nil {
			fmt.Printf("worker: upload artifacts for %s failed (degraded, task still SUCCEEDED): %v\n",
				msg.TaskID, uerr)
		} else {
			t.MinioPrefix = prefix
		}
	}

	// 成功后重读任务再回写：保留进度协程/其他协程可能已写入的字段，
	// 并将 Attempts 记为本次执行次数
	if cur, gerr := w.Store.Get(msg.TaskID, ""); gerr == nil {
		// 用户中断优先：kill 内核与内核正常结束可能几乎同时发生（小任务尤其明显），
		// 此时不得把用户写入的 CANCELLED/PAUSED 覆盖成 SUCCEEDED。
		if cur.Status == task.StatusCancelled || cur.Status == task.StatusPaused {
			return nil
		}
		t.Progress = cur.Progress
	}
	// 兜底：中断标记存在说明进程被杀，即使退出码为 0 也不按成功处理
	if w.takeInterrupted(t.ID) {
		return nil
	}
	t.Status = task.StatusSucceeded
	t.Attempts++
	if uerr := w.Store.Update(t); uerr != nil && uerr != task.ErrNotFound {
		return fmt.Errorf("worker: mark succeeded %s: %w", msg.TaskID, uerr)
	}
	w.notifyTerminal(t, string(task.StatusSucceeded), "")
	return nil
}

// runBuild 切片/转换任务的既有执行路径（F-04）：确保 manifest → 周期解析
// 进度 → exec 调内核 build/raster2tiles → 日志落盘 → 最终进度回写；
// 内核执行失败时走 retryOrFail 既有退避（含重新入队）。
// 返回 false 表示任务已进入失败/重试终态，调用方不得再走成功回写。
func (w *Worker) runBuild(t *task.Task, msg queue.TaskMessage) bool {
	if isTerrainTask(msg) || isPointCloudTask(msg) || isDemDesensitizeTask(msg) {
		// 地形（A5）、LAS 点云与 DEM 脱密任务无 manifest/进度概念：跳过 ensureManifest，
		// 避免向产物目录写入内核不消费的 manifest.json，也避免污染产物目录。
		msg.ManifestPath = ""
		if isDemDesensitizeTask(msg) {
			// 合规脱密：区域 JSON 必须先落盘，内核据此裁剪处理范围
			if rerr := writeRegionFile(msg); rerr != nil {
				w.fail(t, fmt.Sprintf("prepare desensitize region: %v", rerr))
				return false
			}
		}
	} else {
		manifestPath, err := w.ensureManifest(msg)
		if err != nil {
			w.fail(t, fmt.Sprintf("prepare manifest: %v", err))
			return false
		}
		msg.ManifestPath = manifestPath
	}

	// B7：清掉上一次中断可能残留的取消标志（pause→resume 场景），
	// 防止本轮执行被历史标志误取消。
	os.Remove(w.cancelFlagPath(msg.TaskID))

	// 内核运行期间周期解析 manifest 回传分块进度（F-04）；
	// exec 返回后先停轮询再写终态，避免进度协程与终态回写竞态。
	stopPoll := make(chan struct{})
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		ticker := time.NewTicker(w.progressInterval())
		defer ticker.Stop()
		for {
			select {
			case <-stopPoll:
				return
			case <-ticker.C:
				w.updateProgress(msg, t.ID)
			}
		}
	}()

	argv := w.buildArgv(msg)
	output, execErr := w.runKernel(t.ID, w.KernelBin, argv)

	close(stopPoll)
	<-pollDone

	// 日志落盘（F-04）：本次执行的全部输出写入 data/logs/{task_id}.log（追加）
	if lerr := w.appendLog(msg.TaskID, t.Attempts+1, output, execErr); lerr != nil {
		fmt.Printf("worker: write log for %s: %v\n", msg.TaskID, lerr)
	}

	// 结束后最终解析一次 manifest，确保进度为最新值
	w.updateProgress(msg, t.ID)

	if execErr != nil {
		if w.takeInterrupted(t.ID) {
			// 用户取消/暂停：内核是被我们杀掉的，状态已由 API 侧写入
			// CANCELLED/PAUSED —— 既不重试，也不覆盖为 FAILED。
			return false
		}
		w.retryOrFail(t, msg, fmt.Sprintf("kernel exit: %v; output: %s", execErr, tail(output, 1024)))
		return false
	}
	return true
}

// retryOrFail 失败处理（F-04）：未超重试上限且队列可用时，Attempts+1、
// 任务置回 PENDING 并按指数退避重新入队；否则置 FAILED。
func (w *Worker) retryOrFail(t *task.Task, msg queue.TaskMessage, reason string) {
	backoff := w.retryBackoff()
	// t.Attempts 为已执行次数：<= len(backoff) 时仍有重试机会
	if w.Publisher != nil && t.Attempts < len(backoff) {
		delay := backoff[t.Attempts]
		t.Attempts++
		t.Status = task.StatusPending
		t.ErrMsg = fmt.Sprintf("attempt %d failed, retrying in %s: %s", t.Attempts, delay, reason)
		if err := w.Store.Update(t); err != nil {
			fmt.Printf("worker: mark retry %s: %v\n", t.ID, err)
		}
		publish := func() {
			if err := w.Publisher.Publish(msg); err != nil {
				fmt.Printf("worker: re-publish retry for %s failed (task stays PENDING): %v\n",
					t.ID, err)
			}
		}
		if delay == 0 {
			// 零退避同步发布：Handle 返回前完成，语义确定（测试友好）
			publish()
		} else {
			go func() {
				time.Sleep(delay)
				publish()
			}()
		}
		return
	}
	t.Attempts++
	t.Status = task.StatusFailed
	t.ErrMsg = reason
	if err := w.Store.Update(t); err != nil && err != task.ErrNotFound {
		// 回写失败只能记日志：状态机不因存储抖动丢失终态语义
		fmt.Printf("worker: mark failed %s: %v\n", t.ID, err)
	}
	w.notifyTerminal(t, string(task.StatusFailed), reason)
}

// fail 置任务 FAILED（不重试的准备阶段失败）；任务已被删除时静默。
func (w *Worker) fail(t *task.Task, msg string) {
	t.Status = task.StatusFailed
	t.ErrMsg = msg
	if err := w.Store.Update(t); err != nil && err != task.ErrNotFound {
		fmt.Printf("worker: mark failed %s: %v\n", t.ID, err)
	}
	w.notifyTerminal(t, string(task.StatusFailed), msg)
}

// notifyTerminal 任务进入终态后的 webhook 通知（F-14 第一步）：
// params.webhook_url 非空且 Notifier 可用时异步 POST 事件，失败重试由
// Notifier 负责，结果只记日志——绝不阻塞、不影响任务状态。
func (w *Worker) notifyTerminal(t *task.Task, status, errMsg string) {
	if w.Notifier == nil || t.Params == nil {
		return
	}
	url, ok := t.Params["webhook_url"].(string)
	if !ok || url == "" {
		return
	}
	ev := webhook.Event{
		TaskID:    t.ID,
		Status:    status,
		Timestamp: time.Now().UTC(),
		Error:     errMsg,
	}
	go func() {
		if err := w.Notifier.Notify(url, ev); err != nil {
			fmt.Printf("worker: webhook for task %s to %s failed after retries: %v\n", t.ID, url, err)
		}
	}()
}

// progressInterval 进度轮询间隔（含默认值兜底）。
func (w *Worker) progressInterval() time.Duration {
	if w.ProgressInterval > 0 {
		return w.ProgressInterval
	}
	return DefaultProgressInterval
}

// retryBackoff 重试退避序列（含默认值兜底）。
func (w *Worker) retryBackoff() []time.Duration {
	if len(w.RetryBackoff) > 0 {
		return w.RetryBackoff
	}
	return DefaultRetryBackoff
}

// dataDir 本地数据目录（含默认值兜底）。
func (w *Worker) dataDir() string {
	if w.DataDir != "" {
		return w.DataDir
	}
	return "data"
}

// updateProgress 解析 manifest 的分块状态并回写进度；仅更新进度字段，
// 其余字段以存储侧当前值为准，避免覆盖并发写入（进度协程场景）。
func (w *Worker) updateProgress(msg queue.TaskMessage, taskID string) {
	done, total, err := ParseManifestProgress(msg.ManifestPath)
	if err != nil || total == 0 {
		return // manifest 不存在/不完整：保持无进度
	}
	cur, err := w.Store.Get(taskID, "")
	if err != nil {
		return
	}
	cur.Progress = &task.Progress{Done: done, Total: total}
	if err := w.Store.Update(cur); err != nil {
		fmt.Printf("worker: update progress for %s: %v\n", taskID, err)
	}
}

// appendLog 将一次内核执行的输出落盘到 data/logs/{task_id}.log（F-04 日志下载）。
// 多次执行（重试）按分隔行追加，保留完整历史。
func (w *Worker) appendLog(taskID string, attempt int, output string, execErr error) error {
	logPath := w.LogPath(taskID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	wr := bufio.NewWriter(f)
	fmt.Fprintf(wr, "=== attempt %d @ %s ===\n", attempt, time.Now().UTC().Format(time.RFC3339))
	if strings.TrimSpace(output) != "" {
		wr.WriteString(strings.TrimRight(output, "\n") + "\n")
	}
	if execErr != nil {
		fmt.Fprintf(wr, "exit error: %v\n", execErr)
	}
	return wr.Flush()
}

// LogPath 任务内核日志的本地落盘路径。
func (w *Worker) LogPath(taskID string) string {
	return filepath.Join(w.dataDir(), "logs", taskID+".log")
}

// manifestFile manifest 分块文件（与内核 ChunkManifest schema 对齐的最小子集）。
type manifestFile struct {
	TaskID string `json:"task_id"`
	Chunks []struct {
		ID     string `json:"id"`
		Status string `json:"status"` // pending / done / failed（内核 ChunkStatus）
	} `json:"chunks"`
}

// ParseManifestProgress 解析 manifest.json 返回分块进度
// （done 数与总数）；文件不存在或非法返回 error。
func ParseManifestProgress(manifestPath string) (done, total int, err error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return 0, 0, err
	}
	var m manifestFile
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, 0, fmt.Errorf("worker: parse manifest %s: %w", manifestPath, err)
	}
	total = len(m.Chunks)
	for _, c := range m.Chunks {
		if c.Status == "done" {
			done++
		}
	}
	return done, total, nil
}

// ensureManifest 保证 manifest_path 存在；缺失时生成最小骨架清单
// （task_id + 两层 LOD 分块，与内核 ChunkManifest schema 对齐）。
// 若 task.source 是目录且包含 .osgb/.obj 几何文件，则每个文件生成一个
// 带 source 的叶子分块（真实几何模式），bounds 占位由内核按真实包围盒回写。
// manifest 约定落在 output 目录内；output 目录不存在则创建。
func (w *Worker) ensureManifest(msg queue.TaskMessage) (string, error) {
	manifestPath := msg.ManifestPath
	if manifestPath == "" {
		manifestPath = filepath.Join(msg.Output, "manifest.json")
	}
	if st, err := os.Stat(manifestPath); err == nil && !st.IsDir() {
		return manifestPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	chunks, err := chunksFromSource(msg.Source)
	if err != nil {
		return "", err
	}
	if chunks == nil {
		chunks = skeletonChunks()
	}
	manifest := map[string]any{
		"task_id": msg.TaskID,
		"chunks":  chunks,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}
	return manifestPath, nil
}

// maxSourceChunks 限制单任务几何分块数，防止超大目录生成失控清单。
const maxSourceChunks = 512

// chunksFromSource 扫描 source 目录中的 .osgb/.obj 文件，每个文件一个叶子分块。
// source 不是目录（或为空）返回 nil, nil（调用方回退骨架）。
func chunksFromSource(source string) ([]map[string]any, error) {
	if source == "" {
		return nil, nil
	}
	st, err := os.Stat(source)
	if err != nil || !st.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return nil, fmt.Errorf("scan source dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".osgb" || ext == ".obj" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) > maxSourceChunks {
		names = names[:maxSourceChunks]
	}
	chunks := make([]map[string]any, 0, len(names))
	for _, name := range names {
		id := strings.TrimSuffix(name, filepath.Ext(name))
		if id == "" {
			continue
		}
		chunks = append(chunks, map[string]any{
			"id":  id,
			"lod": 1,
			// bounds 占位：内核解析真实几何后回写真实包围盒（断点续切友好）
			"bounds":   map[string]any{"min": []float64{0, 0, 0}, "max": []float64{1, 1, 1}},
			"status":   "pending",
			"attempts": 0,
			"source":   filepath.Join(source, name),
		})
	}
	return chunks, nil
}

// skeletonChunks 无几何源时的两层 LOD 骨架分块。
func skeletonChunks() []map[string]any {
	return []map[string]any{
		{
			"id":  "chunk-lod0",
			"lod": 0,
			// bounds 与内核 Bounds{min,max}（[f64;3]）对齐；占位值由内核后续按真实数据填充
			"bounds":   map[string]any{"min": []float64{0, 0, 0}, "max": []float64{1, 1, 1}},
			"status":   "pending",
			"attempts": 0,
			"children": []string{"chunk-lod1"},
		},
		{
			"id":       "chunk-lod1",
			"lod":      1,
			"bounds":   map[string]any{"min": []float64{0, 0, 0}, "max": []float64{1, 1, 1}},
			"status":   "pending",
			"attempts": 0,
		},
	}
}

// tail 截取输出末尾 n 字节，避免超长内核输出撑爆任务记录。
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
