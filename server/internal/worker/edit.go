// worker 编辑接线（M2-F09c）：type=edit 的任务对源任务切片产物执行内核
// `edit <clip|flatten|ground-align> ...`（CLI 契约见任务 rsJTtY）：
//
//	tangis-kernel edit <op> --source <tile源目录> --tile <tile名|all> \
//	  --out <dir> --format b3dm [--plane ax+by+cz=d] [--bbox minx,miny,maxx,maxy] \
//	  [--elevation N] [--feather N] [--report <out>/ops.json]
//
// 语义约定（严禁造假）：
//   - 内核执行报错：沿用既有指数退避重试（retryOrFail），超限 FAILED；
//   - 成功后 ops.json 留痕缺失/不可解析：任务置 FAILED（宁可失败也不伪造留痕）；
//   - ops.json 解析成功：整体存入 Task.OpsSummary（web 展示 totals 等摘要），
//     完整原文随产物落盘并由 /tasks/:id/ops-report 下载端点提供。
package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// 编辑操作与参数的 params 键名（API 层写入、worker 读取，两端契约）。
const (
	editParamOp        = "edit_op"
	editParamTile      = "edit_tile"
	editParamBBox      = "edit_bbox"      // "minx,miny,maxx,maxy"
	editParamPlane     = "edit_plane"     // "ax+by+cz=d"
	editParamElevation = "edit_elevation" // 数字
	editParamFeather   = "edit_feather"   // 数字
)

// editOps 支持的编辑操作集合。
var editOps = map[string]bool{"clip": true, "flatten": true, "ground-align": true}

// EditOpValid 校验 op 是否为支持的编辑操作（api 层校验复用）。
func EditOpValid(op string) bool { return editOps[op] }

// buildEditArgv 按 CLI 契约拼装 edit 子命令 argv。
// 参数由 API 层校验过（op 合法、互斥满足）；worker 侧再兜底校验，
// 非法返回 error（调用方置任务 FAILED，不把非法参数发给内核）。
func buildEditArgv(params map[string]any, source, output string) ([]string, error) {
	op, _ := params[editParamOp].(string)
	if !editOps[op] {
		return nil, fmt.Errorf("edit: invalid op %q", op)
	}
	if source == "" || output == "" {
		return nil, fmt.Errorf("edit: source/output dir required")
	}

	tile, _ := params[editParamTile].(string)
	if tile == "" {
		tile = "all"
	}

	argv := []string{
		"edit", op,
		"--source", source,
		"--tile", tile,
		"--out", output,
		"--format", "b3dm",
	}
	if v, _ := params[editParamPlane].(string); v != "" {
		argv = append(argv, "--plane", v)
	}
	if v, _ := params[editParamBBox].(string); v != "" {
		argv = append(argv, "--bbox", v)
	}
	if v, ok := numParam(params, editParamElevation); ok {
		argv = append(argv, "--elevation", strconv.FormatFloat(v, 'f', -1, 64))
	}
	if v, ok := numParam(params, editParamFeather); ok {
		argv = append(argv, "--feather", strconv.FormatFloat(v, 'f', -1, 64))
	}
	argv = append(argv, "--report", filepath.Join(output, task.OpsReportFile))
	return argv, nil
}

// numParam 提取数值参数（JSON 数字；宽容接受字符串数字以便重放场景）。
func numParam(params map[string]any, key string) (float64, bool) {
	switch v := params[key].(type) {
	case float64:
		return v, true
	case string:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// parseOpsReport 读取并解析 ops.json 留痕（整体作为摘要透传，
// 内容结构由内核定义，server 不臆造字段）。
func parseOpsReport(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse ops report %s: %w", path, err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("ops report %s is empty", path)
	}
	return m, nil
}

// runEdit 执行编辑任务（Handle 的 edit 分支）：跳过 manifest/进度轮询，
// 直接按契约调内核 edit；成功后解析 ops.json 回写 OpsSummary。
// 返回 retriable=true（内核执行失败）时由调用方走 retryOrFail 既有退避；
// retriable=false（参数非法/留痕缺失不可解析）直接 FAILED，重试无意义。
func (w *Worker) runEdit(t *task.Task, msg queue.TaskMessage) (retriable bool, err error) {
	argv, err := buildEditArgv(t.Params, msg.Source, msg.Output)
	if err != nil {
		return false, fmt.Errorf("invalid edit params: %v", err)
	}
	out, execErr := w.runKernel(msg.TaskID, w.KernelBin, argv)
	if lerr := w.appendLog(msg.TaskID, t.Attempts+1, "[edit] "+out, execErr); lerr != nil {
		fmt.Printf("worker: write edit log for %s: %v\n", msg.TaskID, lerr)
	}
	if execErr != nil {
		return true, fmt.Errorf("edit kernel exit: %v; output: %s", execErr, tail(out, 1024))
	}
	summary, err := parseOpsReport(filepath.Join(msg.Output, task.OpsReportFile))
	if err != nil {
		return false, fmt.Errorf("ops report unreadable: %v", err)
	}
	t.OpsSummary = summary
	return false, nil
}

// isEditTask 任务是否为编辑任务。
func isEditTask(t *task.Task) bool { return t != nil && t.Type == task.TypeEdit }

// EditParamsFromReq 把 API 请求体整理为 edit 任务 params（API 与 worker 的
// 共享契约；API 层负责互斥校验，这里只做规整存储）。导出供 api 层复用，
// 保证两端键名一致。
func EditParamsFromReq(op, tile, bbox, plane string, elevation, feather *float64) map[string]any {
	p := map[string]any{editParamOp: op}
	if tile != "" {
		p[editParamTile] = tile
	}
	if bbox != "" {
		p[editParamBBox] = bbox
	}
	if plane != "" {
		p[editParamPlane] = plane
	}
	if elevation != nil {
		p[editParamElevation] = *elevation
	}
	if feather != nil {
		p[editParamFeather] = *feather
	}
	return p
}
