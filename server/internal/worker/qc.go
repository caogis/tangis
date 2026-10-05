// worker 质检接线（M2-F08b）：params.qc=true 的任务在切片成功后，
// 对源目录追加执行内核 `qc --source <源> --report <output>/qc-report.json`。
//
// 语义约定（严禁造假）：
//   - 内核 qc 执行报错：任务置 FAILED 并带明确原因（不静默、不降级为成功）；
//   - 报告可解析：按 summary.passed 写 QcStatus=pass/fail 与 QcSummary 摘要；
//     报告 FAIL 只代表几何质量问题，任务本身仍 SUCCEEDED（切片是成功的）；
//   - 报告缺失/不可解析：按 qc 失败处理置 FAILED（宁可失败也不伪造状态）；
//   - 源不适用（影像任务/源非目录）：QcStatus=skipped，不执行不报错。
package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// qcEnabled 任务是否请求质检：params.qc === true。
func qcEnabled(params map[string]any) bool {
	b, ok := params["qc"].(bool)
	return ok && b
}

// qcReport 内核 qc 报告 JSON 的最小解析结构（对齐 docs/qc-report-sample）。
type qcReport struct {
	TileCount int `json:"tile_count"`
	Summary   struct {
		TotalDegenerate         int  `json:"total_degenerate"`
		TotalFlippedEdges       int  `json:"total_flipped_edges"`
		TotalFloatingComponents int  `json:"total_floating_components"`
		TotalCrackSegments      int  `json:"total_crack_segments"`
		TotalSelfIntersections  int  `json:"total_self_intersections"`
		Passed                  bool `json:"passed"`
	} `json:"summary"`
}

// summary 摘要转任务持久化结构（字段映射：total_floating_components -> total_floating）。
func (r *qcReport) summary() *task.QcSummary {
	return &task.QcSummary{
		TileCount:              r.TileCount,
		TotalDegenerate:        r.Summary.TotalDegenerate,
		TotalFlippedEdges:      r.Summary.TotalFlippedEdges,
		TotalFloating:          r.Summary.TotalFloatingComponents,
		TotalCrackSegments:     r.Summary.TotalCrackSegments,
		TotalSelfIntersections: r.Summary.TotalSelfIntersections,
	}
}

// parseQCReport 读取并解析 qc 报告文件。
func parseQCReport(path string) (*qcReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r qcReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse qc report %s: %w", path, err)
	}
	return &r, nil
}

// runQC 切片成功后的质检步骤（M2-F08b）。返回 error 表示质检失败
// （内核报错/报告不可解析），调用方据此置任务 FAILED；
// 返回 nil 时 t.QcStatus/QcSummary 已按报告写好。
func (w *Worker) runQC(t *task.Task, msg queue.TaskMessage) error {
	// 源不适用（影像单文件线、源非目录）：跳过而非报错，状态如实记 skipped
	if isImageTask(msg) {
		t.QcStatus = task.QcSkipped
		return nil
	}
	if st, err := os.Stat(msg.Source); err != nil || !st.IsDir() {
		t.QcStatus = task.QcSkipped
		return nil
	}

	reportPath := filepath.Join(msg.Output, task.QcReportFile)
	out, execErr := w.runKernel(msg.TaskID, w.KernelBin, []string{
		"qc", "--source", msg.Source, "--report", reportPath,
	})
	// qc 输出追加进同一份任务日志，保留完整历史（失败原因可追溯）
	if lerr := w.appendLog(msg.TaskID, t.Attempts+1, "[qc] "+out, execErr); lerr != nil {
		fmt.Printf("worker: write qc log for %s: %v\n", msg.TaskID, lerr)
	}
	if execErr != nil {
		return fmt.Errorf("qc kernel exit: %v; output: %s", execErr, tail(out, 1024))
	}

	report, err := parseQCReport(reportPath)
	if err != nil {
		return fmt.Errorf("qc report unreadable: %v", err)
	}
	t.QcSummary = report.summary()
	if report.Summary.Passed {
		t.QcStatus = task.QcPass
	} else {
		t.QcStatus = task.QcFail
	}
	return nil
}
