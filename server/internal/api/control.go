package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// Version 服务版本号，回显在 GET /api/v1/system。
// 发布时可用 -ldflags "-X tangis/server/internal/api.Version=x.y.z" 覆盖。
var Version = "0.1.0-dev"

// ---- F-04 任务控制：取消 / 暂停 / 恢复 / 重试 ----

// TaskInterrupter 中断正在运行的任务内核进程，由 worker 实现。
// 允许为 nil：此时取消/暂停只做状态流转（内核会跑完，但结果不再被写回）。
type TaskInterrupter interface {
	// Interrupt 中断任务当前执行，返回是否命中正在运行的进程。
	Interrupt(taskID string) bool
}

// RuntimeInfo 运行时信息，GET /api/v1/system 返回，前端设置页展示。
// 同时充当能力开关来源（哪些协议/模块在当前模式下可用），
// 使前端不必靠 503 探测或硬编码。
type RuntimeInfo struct {
	Mode         string          `json:"mode"`     // server | desktop
	Version      string          `json:"version"`  // 构建版本
	DataDir      string          `json:"data_dir"` // 数据目录（桌面模式为 ~/TanGIS）
	KernelBin    string          `json:"kernel_bin"`
	Queue        string          `json:"queue"`   // local | nats | none
	Storage      string          `json:"storage"` // localfs | minio | none
	Cache        string          `json:"cache"`   // memory | redis | none
	Workers      int             `json:"workers"`
	AuthEnabled  bool            `json:"auth_enabled"`
	LicensePlan  string          `json:"license_plan"`
	Capabilities map[string]bool `json:"capabilities"`
}

// GetSystemInfo GET /api/v1/system — 运行信息与能力开关（设置页数据源）。
func (h *Handler) GetSystemInfo(c *gin.Context) {
	info := h.Runtime
	if info.Capabilities == nil {
		info.Capabilities = map[string]bool{}
	}
	if info.DataDir == "" {
		info.DataDir = h.dataDir()
	}
	c.JSON(http.StatusOK, info)
}

// taskAction 任务控制动作。
type taskAction string

const (
	taskActionCancel taskAction = "cancel"
	taskActionPause  taskAction = "pause"
	taskActionResume taskAction = "resume"
	taskActionRetry  taskAction = "retry"
)

// CancelTask POST /api/v1/tasks/:id/cancel — 取消任务（F-04）。
func (h *Handler) CancelTask(c *gin.Context) { h.controlTask(c, taskActionCancel) }

// PauseTask POST /api/v1/tasks/:id/pause — 暂停任务（F-04），保留产物与断点。
func (h *Handler) PauseTask(c *gin.Context) { h.controlTask(c, taskActionPause) }

// ResumeTask POST /api/v1/tasks/:id/resume — 恢复暂停任务（F-04）。
func (h *Handler) ResumeTask(c *gin.Context) { h.controlTask(c, taskActionResume) }

// RetryTask POST /api/v1/tasks/:id/retry — 手动重试失败/已取消任务（F-04）。
func (h *Handler) RetryTask(c *gin.Context) { h.controlTask(c, taskActionRetry) }

// controlTask 任务控制的公共实现：
// 校验状态是否允许该动作 → 运行中先中断内核 → 写状态 →（resume/retry）重新入队。
func (h *Handler) controlTask(c *gin.Context, action taskAction) {
	t, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if msg := checkActionAllowed(action, t.Status); msg != "" {
		c.JSON(http.StatusConflict, gin.H{"error": msg, "status": string(t.Status)})
		return
	}

	// 运行中：先中断内核进程。否则进程会继续跑并把状态覆盖回 RUNNING/SUCCEEDED。
	wasRunning := t.Status == task.StatusRunning
	interrupted := false
	if wasRunning && h.Interrupter != nil {
		interrupted = h.Interrupter.Interrupt(t.ID)
	}

	requeue := false
	switch action {
	case taskActionCancel:
		t.Status = task.StatusCancelled
		t.ErrMsg = "cancelled by user"
	case taskActionPause:
		t.Status = task.StatusPaused
		t.ErrMsg = "paused by user"
	case taskActionResume:
		t.Status = task.StatusPending
		t.ErrMsg = ""
		requeue = true
	case taskActionRetry:
		t.Status = task.StatusPending
		t.ErrMsg = ""
		// 显式重试视为新一轮：退避计数清零，否则已耗尽重试的任务会立刻再次 FAILED
		t.Attempts = 0
		requeue = true
	}

	if err := h.Store.Update(t); err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	warning := ""
	if requeue {
		if h.Publisher == nil {
			warning = "queue unavailable, task stays PENDING"
		} else if perr := h.Publisher.Publish(queue.TaskMessage{
			TaskID:       t.ID,
			Type:         t.Type,
			Source:       t.Source,
			Output:       t.Output,
			ManifestPath: t.ManifestPath,
			Params:       t.Params,
		}); perr != nil {
			warning = "queue publish failed, task stays PENDING: " + perr.Error()
		}
	}

	h.invalidateLists(scopeTenant(c))

	resp := gin.H{"task": t}
	if warning != "" {
		resp["warning"] = warning
	}
	if wasRunning {
		// 让前端能区分"确实中断了内核"与"进程已自行结束，仅改了状态"
		resp["interrupted"] = interrupted
	}
	c.JSON(http.StatusOK, resp)
}

// checkActionAllowed 校验动作与当前状态是否匹配，返回空串表示允许。
func checkActionAllowed(action taskAction, st task.Status) string {
	switch action {
	case taskActionCancel:
		if st.Terminal() {
			return fmt.Sprintf("task already finished (%s), cannot cancel", st)
		}
		return ""
	case taskActionPause:
		if st != task.StatusPending && st != task.StatusRunning {
			return fmt.Sprintf("only PENDING/RUNNING task can be paused (current: %s)", st)
		}
		return ""
	case taskActionResume:
		if st != task.StatusPaused {
			return fmt.Sprintf("only PAUSED task can be resumed (current: %s)", st)
		}
		return ""
	case taskActionRetry:
		if st != task.StatusFailed && st != task.StatusCancelled {
			return fmt.Sprintf("only FAILED/CANCELLED task can be retried (current: %s)", st)
		}
		return ""
	}
	return "unknown action"
}

// ---- 通用文件/目录上传（导入体验：浏览器选目录即可建任务）----

// UploadFiles POST /api/v1/uploads — 上传文件或整个目录，返回落盘根目录。
//
// 与 POST /tasks/import 的分工：import 针对单个 .t3d 包并**直接建任务**；
// 本接口只负责把用户选的文件落到本机（<dataDir>/uploads/{upload_id}/），
// 返回的 root 作为后续建任务的 source —— 因而能服务 OSGB 目录（数千文件）、
// OBJ/GLTF/影像等任意格式，也允许用户先上传再挑参数。
//
// 请求 multipart/form-data：
//   - paths：JSON 数组（可选，须排在文件 part 之前），第 i 项是第 i 个文件的
//     相对路径（浏览器 webkitRelativePath），用于保留目录结构；
//   - 其余 file part：文件内容，顺序与 paths 对齐。
//
// 响应 201：{upload_id, root, files, bytes}。
func (h *Handler) UploadFiles(c *gin.Context) {
	mr, err := c.Request.MultipartReader()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "multipart/form-data required: " + err.Error()})
		return
	}

	uploadID, err := task.NewID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	base := filepath.Join(h.dataDir(), "uploads", uploadID)

	maxBytes := h.importMaxBytes()
	maxTotal := h.ImportMaxTotalBytes
	if maxTotal <= 0 {
		maxTotal = 8 << 30
	}
	maxFiles := h.ImportMaxFiles
	if maxFiles <= 0 {
		maxFiles = 65536
	}

	var (
		relPaths []string
		written  []string
		files    int
		total    int64
	)
	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read multipart: " + perr.Error()})
			return
		}

		// paths 字段必须在文件之前（流式读取只能顺序消费）
		if part.FormName() == "paths" && part.FileName() == "" {
			raw, rerr := io.ReadAll(io.LimitReader(part, 8<<20))
			part.Close()
			if rerr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "read paths: " + rerr.Error()})
				return
			}
			if uerr := json.Unmarshal(raw, &relPaths); uerr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "paths must be a JSON array: " + uerr.Error()})
				return
			}
			continue
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}

		if files >= maxFiles {
			part.Close()
			c.JSON(http.StatusRequestEntityTooLarge,
				gin.H{"error": fmt.Sprintf("too many files (max %d)", maxFiles)})
			return
		}

		rel := part.FileName()
		if files < len(relPaths) && strings.TrimSpace(relPaths[files]) != "" {
			rel = relPaths[files]
		}
		clean, cerr := safeRelPath(rel)
		if cerr != nil {
			part.Close()
			c.JSON(http.StatusBadRequest, gin.H{"error": cerr.Error()})
			return
		}

		dest := filepath.Join(base, clean)
		if merr := os.MkdirAll(filepath.Dir(dest), 0o755); merr != nil {
			part.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": merr.Error()})
			return
		}
		f, oerr := os.Create(dest)
		if oerr != nil {
			part.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": oerr.Error()})
			return
		}
		n, werr := io.Copy(f, io.LimitReader(part, maxBytes+1))
		f.Close()
		part.Close()
		if werr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write file: " + werr.Error()})
			return
		}
		if n > maxBytes {
			os.Remove(dest)
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": fmt.Sprintf("file %s exceeds per-file limit (%d bytes)", rel, maxBytes),
			})
			return
		}
		total += n
		if total > maxTotal {
			os.Remove(dest)
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": fmt.Sprintf("upload exceeds total limit (%d bytes)", maxTotal),
			})
			return
		}
		written = append(written, filepath.ToSlash(clean))
		files++
	}

	if files == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no file parts received"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"upload_id": uploadID,
		"root":      commonRoot(base, written),
		"files":     files,
		"bytes":     total,
	})
}

// safeRelPath 清洗上传的相对路径：拒绝绝对路径、盘符与 .. 逃逸。
func safeRelPath(p string) (string, error) {
	s := filepath.ToSlash(strings.TrimSpace(p))
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	if s == "" {
		return "", errors.New("empty file path")
	}
	if strings.Contains(s, ":") {
		return "", fmt.Errorf("unsafe file path (drive letter): %s", p)
	}
	clean := path.Clean(s)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("unsafe file path (escape): %s", p)
	}
	if strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("unsafe file path (absolute): %s", p)
	}
	return filepath.FromSlash(clean), nil
}

// commonRoot 取上传文件的公共顶层目录：全部位于同一顶层目录下时返回该目录
// （OSGB 典型的 <工程>/Data/... 结构），否则返回上传根目录本身。
func commonRoot(base string, rels []string) string {
	top := ""
	for _, r := range rels {
		segs := strings.Split(filepath.ToSlash(r), "/")
		if len(segs) < 2 {
			return base
		}
		if top == "" {
			top = segs[0]
			continue
		}
		if segs[0] != top {
			return base
		}
	}
	if top == "" {
		return base
	}
	return filepath.Join(base, top)
}
