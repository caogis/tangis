// 编辑任务 API（M2-F09c）：
//   - POST /api/v1/tasks/:id/edit：对已 SUCCEEDED+Approved 的 3D 切片任务
//     发起内核 edit（clip/flatten/ground-align），创建独立编辑任务并入队；
//   - GET /api/v1/tasks/:id/edit-history：查同 parent 链的编辑任务列表；
//   - GET /api/v1/tasks/:id/ops-report：下载内核 ops.json 留痕（同 qc-report 模式）。
//
// 严禁造假：参数校验失败直接 400 带明确原因，不静默修正后放行。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
	"tangis/server/internal/worker"
)

// editTaskReq POST /api/v1/tasks/:id/edit 请求体。
// op 必填；tile 省略默认 all（全量瓦片）；
// clip 需 plane 或 bbox（二者互斥）；flatten/ground-align 需 bbox+elevation。
type editTaskReq struct {
	Op         string   `json:"op" binding:"required"`
	Tile       string   `json:"tile"`
	BBox       string   `json:"bbox"`
	Plane      string   `json:"plane"`
	Elevation  *float64 `json:"elevation"`
	Feather    *float64 `json:"feather"`
	WebhookURL string   `json:"webhook_url"`
}

// is3DEditSource 任务产物是否为可编辑的 3D Tiles（b3dm/tileset）：
// 3D 切片线（*3dtiles*）、.t3d 导入、既有编辑任务（可链式再编辑）。
// 影像瓦片（image->tiles）产物为 PNG 金字塔，不适用几何编辑。
func is3DEditSource(t *task.Task) bool {
	switch t.Type {
	case task.TypeEdit, task.TypeT3dImport:
		return true
	}
	return strings.Contains(t.Type, "3dtiles")
}

// planePattern 内核 --plane ax+by+cz=d 的宽松形态（系数允许小数与负号，
// y/z 项符号并入系数）。
var planePattern = regexp.MustCompile(`^\s*([+-]?(?:\d+\.?\d*|\.\d+))x\s*([+-](?:\d+\.?\d*|\.\d+))y\s*([+-](?:\d+\.?\d*|\.\d+))z\s*=\s*([+-]?(?:\d+\.?\d*|\.\d+))\s*$`)

// formatF 数值规整为内核友好的最短十进制（避免科学计数法/多余零）。
func formatF(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// parseBBox 校验并规整 bbox 四至 "minx,miny,maxx,maxy"。
// 非法返回 error（原样透传给前端提示），合法返回规范形式。
func parseBBox(raw string) (string, error) {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) != 4 {
		return "", fmt.Errorf("bbox must be minx,miny,maxx,maxy (4 numbers), got %q", raw)
	}
	v := make([]float64, 4)
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return "", fmt.Errorf("bbox item %d is not a number: %q", i+1, p)
		}
		v[i] = f
	}
	if v[0] >= v[2] || v[1] >= v[3] {
		return "", fmt.Errorf("bbox requires minx<maxx and miny<maxy, got %q", raw)
	}
	return strings.Join([]string{formatF(v[0]), formatF(v[1]), formatF(v[2]), formatF(v[3])}, ","), nil
}

// parsePlane 校验并规整切割面 "ax+by+cz=d"，合法返回规范形式
// （系数符号规范化，如 "-1x+2.5y-3z=0"）。
func parsePlane(raw string) (string, error) {
	m := planePattern.FindStringSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("plane must be ax+by+cz=d (e.g. 0x+0y+1z=10), got %q", raw)
	}
	a, _ := strconv.ParseFloat(m[1], 64)
	b, _ := strconv.ParseFloat(m[2], 64)
	c, _ := strconv.ParseFloat(m[3], 64)
	d, _ := strconv.ParseFloat(m[4], 64)
	return formatF(a) + "x" + signF(b) + "y" + signF(c) + "z=" + formatF(d), nil
}

// signF 带符号数值（y/z 项系数需显式 +/-）。
func signF(f float64) string {
	if f >= 0 {
		return "+" + formatF(f)
	}
	return formatF(f)
}

// CreateEditTask POST /api/v1/tasks/:id/edit — 对源任务发起编辑（M2-F09c）。
// 前置：源任务 SUCCEEDED + 已审批 + 3D Tiles 产物；op 与参数互斥校验失败
// 返回 400（前端同步拦截，此处为最终防线）。创建 type=edit 任务
// （params 存完整编辑参数 + 源任务 id，source=源任务 output 目录）并入队；
// NATS 不可用时降级 PENDING（与 CreateTask 同语义）。
func (h *Handler) CreateEditTask(c *gin.Context) {
	src, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if src.Status != task.StatusSucceeded {
		c.JSON(http.StatusConflict, gin.H{"error": "source task not finished: " + string(src.Status)})
		return
	}
	if !src.Approved {
		c.JSON(http.StatusForbidden, gin.H{"error": "source task not approved for distribution"})
		return
	}
	if !is3DEditSource(src) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source task is not an editable 3D tiles task: " + src.Type})
		return
	}

	var req editTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if bad := validateEditReq(&req); bad != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": bad})
		return
	}

	tenantID := c.GetString("tenant_id")
	if isAuthOff(c) {
		tenantID = "default"
	}

	id, err := task.NewID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	output := filepath.Join(h.dataDir(), "tasks", id)

	params := worker.EditParamsFromReq(req.Op, req.Tile, req.BBox, req.Plane, req.Elevation, req.Feather)
	params["parent_task_id"] = src.ID
	if req.WebhookURL != "" {
		params["webhook_url"] = req.WebhookURL
	}

	t := &task.Task{
		ID:   id,
		Type: task.TypeEdit,
		// 编辑源 = 原任务原始 OSGB 源目录：内核 edit 读 OSGB 几何做算子
		// 再经 --format b3dm 产出编辑后 tileset（产物目录里是 b3dm，不可再编辑）
		Source:       src.Source,
		Output:       output,
		Status:       task.StatusPending,
		Params:       params,
		TenantID:     tenantID,
		ParentTaskID: src.ID,
	}
	if err := h.Store.Create(t); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	resp := gin.H{}
	if h.Publisher == nil {
		resp["warning"] = "queue unavailable, task stays PENDING (NATS not connected)"
	} else if err := h.Publisher.Publish(queue.TaskMessage{
		TaskID: t.ID,
		Type:   t.Type,
		Source: t.Source,
		Output: t.Output,
		Params: t.Params,
	}); err != nil {
		resp["warning"] = "queue publish failed, task stays PENDING: " + err.Error()
	}
	h.invalidateLists(tenantID)

	resp["task"] = t
	c.JSON(http.StatusCreated, resp)
}

// validateEditReq op 与参数互斥校验（严禁造假：校验失败返回明确原因，
// 不静默修正）。返回空串表示合法。
func validateEditReq(req *editTaskReq) string {
	if !worker.EditOpValid(req.Op) {
		return fmt.Sprintf("op must be one of clip|flatten|ground-align, got %q", req.Op)
	}
	if req.Tile != "" && req.Tile != "all" && strings.ContainsAny(req.Tile, "/\\") {
		return fmt.Sprintf("tile must be a tile name or 'all', got %q", req.Tile)
	}

	bbox, plane := strings.TrimSpace(req.BBox), strings.TrimSpace(req.Plane)
	switch req.Op {
	case "clip":
		// 互斥：plane 与 bbox 二选一
		if bbox != "" && plane != "" {
			return "clip accepts either plane or bbox, not both"
		}
		if bbox == "" && plane == "" {
			return "clip requires plane or bbox"
		}
	case "flatten", "ground-align":
		if plane != "" {
			return fmt.Sprintf("%s does not accept plane (bbox+elevation required)", req.Op)
		}
		if bbox == "" {
			return fmt.Sprintf("%s requires bbox", req.Op)
		}
		if req.Elevation == nil {
			return fmt.Sprintf("%s requires elevation", req.Op)
		}
	}

	var err error
	if bbox != "" {
		if req.BBox, err = parseBBox(bbox); err != nil {
			return err.Error()
		}
	}
	if plane != "" {
		if req.Plane, err = parsePlane(plane); err != nil {
			return err.Error()
		}
	}
	if req.Feather != nil && *req.Feather < 0 {
		return fmt.Sprintf("feather must be >= 0, got %v", *req.Feather)
	}
	return ""
}

// EditHistory GET /api/v1/tasks/:id/edit-history — 查同 parent 链的编辑任务
// 列表（含 ops.json 摘要，随任务记录 ops_summary 字段返回）。
// :id 可为切片任务或链上任意编辑任务：统一上溯到链根（首个非 edit 祖先），
// 返回该根下全部编辑任务（按创建时间正序）。源任务本身不存在返回 404。
func (h *Handler) EditHistory(c *gin.Context) {
	t, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	scope := scopeTenant(c)

	// 上溯链根：edit -> parent，直到 parent 非编辑任务或缺失（被删则止于当前）
	root := t
	for root.Type == task.TypeEdit && root.ParentTaskID != "" {
		parent, perr := h.Store.Get(root.ParentTaskID, scope)
		if perr != nil {
			break
		}
		root = parent
	}

	tasks, err := h.Store.List(scope)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 编辑链 = 链根的全部 edit 后代（含编辑的编辑）：按 parent_task_id
	// 建子代表，从根 BFS 收集，再按创建时间正序输出。
	children := map[string][]*task.Task{}
	for _, e := range tasks {
		if e.Type == task.TypeEdit && e.ParentTaskID != "" {
			children[e.ParentTaskID] = append(children[e.ParentTaskID], e)
		}
	}
	chain := make([]*task.Task, 0)
	queue := []*task.Task{root}
	seen := map[string]bool{root.ID: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ch := range children[cur.ID] {
			if !seen[ch.ID] {
				seen[ch.ID] = true
				chain = append(chain, ch)
				queue = append(queue, ch)
			}
		}
	}
	sort.Slice(chain, func(i, j int) bool { return chain[i].CreatedAt.Before(chain[j].CreatedAt) })
	c.JSON(http.StatusOK, gin.H{"parent_task_id": root.ID, "tasks": chain})
}

// GetTaskOpsReport GET /api/v1/tasks/:id/ops-report — 下载编辑操作留痕
// ops.json 原文（M2-F09c，同 qc-report 模式）。worker 落盘于产物目录
// （<output>/ops.json，task.OpsReportFile）；非编辑任务或留痕缺失返回 404
// （不伪造内容）。
func (h *Handler) GetTaskOpsReport(c *gin.Context) {
	t, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	reportPath := filepath.Join(t.Output, task.OpsReportFile)
	data, err := os.ReadFile(reportPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no ops report yet"})
		return
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=\""+t.ID+"-ops.json\"")
	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}
