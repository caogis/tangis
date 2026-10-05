// compliance.go 实现测绘合规算子的 HTTP 接线（F-21）。
//
// 三类能力的定位不同，故走不同通道：
//
//	坐标系识别（crs-identify）  秒级查询 → 同步 API
//	七参数转换（bursa）         秒级点集计算 → 同步 API
//	DEM 区域脱密（desensitize-dem）产物是 GeoTIFF + 留痕，可能跑很久 → 任务线（worker）
//
// 任务产物（非瓦片类，如脱密后的 GeoTIFF 与留痕 JSON）由
// GET /api/v1/tasks/{id}/artifact/{name} 取回。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/task"
)

// ---- 内核同步执行（合规算子）----

// kernelBin 内核可执行文件路径：装配侧注入（KERNEL_BIN），
// 空则退回仓库内 debug 构建（仅便于本地开发）。
func (h *Handler) kernelBin() string {
	if h.KernelBin != "" {
		return h.KernelBin
	}
	return "../kernel/target/debug/tangis-kernel"
}

// runKernelSync 同步执行内核子命令并返回合并输出。
// 仅用于秒级算子（crs-identify / bursa）；重活走任务线。
func (h *Handler) runKernelSync(args ...string) (string, error) {
	cmd := exec.Command(h.kernelBin(), args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("kernel %s failed: %w", args[0], err)
	}
	return buf.String(), nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---- 坐标系识别 ----

// CRSIdentify POST /api/v1/compliance/crs-identify — 识别数据坐标系（F-21）。
//
// 请求：{"path": "/data/dem.tif"}（也支持 .prj）。
// 响应：{info: {...}, georef: "georef: pixel_scale=… tiepoint_origin=…", output: "原始输出"}。
// 内核 stdout 首块是 pretty JSON，其后可能追加 georef 行 —— 用流式 Decoder
// 只取第一个 JSON 对象，避免被尾随文本干扰。
func (h *Handler) CRSIdentify(c *gin.Context) {
	var req struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := strings.TrimSpace(req.Path)
	if p == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path 不能为空"})
		return
	}
	if !fileExists(p) {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在或不可读: " + p})
		return
	}

	out, err := h.runKernelSync("crs-identify", p)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "output": out})
		return
	}
	var info any
	_ = json.NewDecoder(strings.NewReader(out)).Decode(&info)
	resp := gin.H{"output": out}
	if info != nil {
		resp["info"] = info
	}
	if line := georefSummary(out); line != "" {
		resp["georef"] = line
	}
	c.JSON(http.StatusOK, resp)
}

// georefSummary 提取 crs-identify 输出中的 georef 行（像元尺度与 tiepoint 原点）。
func georefSummary(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "georef:") {
			return strings.TrimPrefix(t, "georef:")
		}
	}
	return ""
}

// ---- 七参数（Bursa-Wolf）转换 ----

// bursaMaxPoints 单次转换的点数上限（防超大请求打爆内存/磁盘）。
const bursaMaxPoints = 200000

// BursaTransform POST /api/v1/compliance/bursa — CGCS2000 七参数转换（F-21）。
//
// 请求：{"params": {"dx":…,"dy":…,"dz":…,"rx":…,"ry":…,"rz":…,"scale_ppm":…,
// "source":"EPSG:4490","target":"EPSG:4326"}, "points": [[lon,lat,h], ...]}
// 响应：{summary: "内核摘要", result: <内核输出的转换结果 JSON>, files: {...}}
//
// 参数与点集落盘到 <dataDir>/compliance/bursa/{runID}/，便于复核与追溯。
func (h *Handler) BursaTransform(c *gin.Context) {
	var req struct {
		Params map[string]any `json:"params" binding:"required"`
		Points [][3]float64   `json:"points" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Points) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "points 不能为空"})
		return
	}
	if len(req.Points) > bursaMaxPoints {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("点数超过上限 %d", bursaMaxPoints)})
		return
	}

	runID, err := task.NewID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	dir := filepath.Join(h.dataDir(), "compliance", "bursa", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	paramsPath := filepath.Join(dir, "params.json")
	pointsPath := filepath.Join(dir, "points.json")
	outPath := filepath.Join(dir, "result.json")

	paramsJSON, err := json.MarshalIndent(req.Params, "", "  ")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "params 不是合法 JSON 对象: " + err.Error()})
		return
	}
	if err := os.WriteFile(paramsPath, paramsJSON, 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	pointsJSON, err := json.Marshal(map[string]any{"points": req.Points})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := os.WriteFile(pointsPath, pointsJSON, 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	out, kerr := h.runKernelSync("bursa", "--params", paramsPath, "--points", pointsPath, "--out", outPath)
	if kerr != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": kerr.Error(), "output": out})
		return
	}

	var result any
	if data, rerr := os.ReadFile(outPath); rerr == nil {
		_ = json.Unmarshal(data, &result)
	} else {
		log.Printf("[compliance] bursa result unreadable: %v", rerr)
	}
	c.JSON(http.StatusOK, gin.H{
		"summary": out,
		"result":  result,
		"files": gin.H{
			"params": paramsPath,
			"points": pointsPath,
			"result": outPath,
		},
	})
}

// ---- 任务产物下载（非瓦片类）----

// DownloadTaskArtifact GET /api/v1/tasks/{id}/artifact/{name} — 下载任务产物文件。
//
// 用于合规脱密产物（desensitized.tif）与脱密留痕（record JSON）等不走
// /services 瓦片通道的文件；路径做了目录逃逸防护。
func (h *Handler) DownloadTaskArtifact(c *gin.Context) {
	t, err := h.Store.Get(c.Param("id"), scopeTenant(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	name := strings.TrimPrefix(c.Param("name"), "/")
	rel, rerr := safeRelPath(name)
	if rerr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": rerr.Error()})
		return
	}
	absOut, err1 := filepath.Abs(t.Output)
	absFull, err2 := filepath.Abs(filepath.Join(t.Output, rel))
	if err1 != nil || err2 != nil || !strings.HasPrefix(absFull, absOut+string(os.PathSeparator)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "artifact path escapes task output dir"})
		return
	}

	if st, serr := os.Stat(absFull); serr == nil && !st.IsDir() {
		c.Header("Content-Disposition", "attachment; filename=\""+path.Base(filepath.ToSlash(rel))+"\"")
		c.File(absFull)
		return
	}
	if h.Objects != nil && t.MinioPrefix != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rc, _, gerr := h.Objects.Get(ctx, path.Join(t.MinioPrefix, filepath.ToSlash(rel)))
		if gerr == nil {
			defer rc.Close()
			data, rerr := io.ReadAll(io.LimitReader(rc, 512<<20))
			if rerr == nil {
				c.Header("Content-Disposition", "attachment; filename=\""+path.Base(filepath.ToSlash(rel))+"\"")
				c.Data(http.StatusOK, "application/octet-stream", data)
				return
			}
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "artifact not found: " + rel})
}
