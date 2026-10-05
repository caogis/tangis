// Package task 定义切片/转换任务的领域模型与存储接口。
//
// M1 MVP 使用内存实现；PRD F-04 要求任务幂等、断点续切，
// 后续将把实现替换为 PostgreSQL 持久化 + NATS JetStream 队列，
// 因此这里先固定 Store 接口，业务层只依赖接口。
//
// F-04/F-06/F-21 补齐后新增字段：
//   - Progress：分块完成数/总数（worker 解析内核 manifest 回传）；
//   - Params：任务参数（JSONB 存储并随任务消息下发内核，参数版本化）；
//   - Attempts：内核已执行次数（失败自动重试上限判断）；
//   - TenantID：任务归属租户（API Key 鉴权下的多租户隔离，F-06）；
//   - Approved：发布审批状态（未审批任务不进入服务列表，F-21）。
package task

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// Status 任务生命周期状态。
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusRunning   Status = "RUNNING"
	StatusSucceeded Status = "SUCCEEDED"
	StatusFailed    Status = "FAILED"
	// StatusCancelled 用户取消（F-04）：排队中的直接标记，运行中的先中断内核进程再标记。
	// worker 的幂等检查只认 PENDING，故取消后仍留在队列里的消息会被自动跳过。
	StatusCancelled Status = "CANCELLED"
	// StatusPaused 用户暂停（F-04）：中断内核进程但保留产物目录与 manifest，
	// resume 时重新入队，内核凭 journal（断点续切）从已完成分块继续。
	StatusPaused Status = "PAUSED"
)

// Terminal 报告状态是否为终态：终态任务不可再被 worker 执行，
// 也不允许被取消/暂停（避免把已成功的产物标成取消）。
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// TypeT3dImport 任务类型：.t3d 包解包导入（M2-F07）。
// 跳过内核：产物即解包内容，任务创建即 SUCCEEDED，进入审批→分发链路。
const TypeT3dImport = "t3d-import"

// TypeTerrain 任务类型：DEM 地形瓦片转换（A5/B2 链路）。
// 内核 `terrain2tiles`：GeoTIFF DEM → Cesium quantized-mesh（layer.json +
// {z}/{x}/{y}.terrain），审批后经 /services/{id}/layer.json 分发。
const TypeTerrain = "terrain->tiles"

// TypePointCloud 任务类型：LAS 点云 → 3D Tiles 点云瓦片（pnts）。
// 内核 `las2pnts`：LAS 1.0–1.4（点格式 0/1/2/3/5/6/7/8）→ tiles/{i}.pnts +
// tileset.json，审批后复用 3D Tiles 分发链路（Cesium3DTileset 可直接加载）。
// 注意：LAZ（压缩）暂不支持；内核对 las2pnts 也**未实现 --cancel-file**。
const TypePointCloud = "las->3dtiles"

// TypeDemDesensitize 任务类型：DEM 区域脱密（F-21 合规）。
// 内核 `desensitize-dem`：按区域 JSON（矩形或多边形）对 DEM 做 flatten/noise
// 处理，产物 desensitized.tif + 脱密留痕 JSON（审批留痕用）。
// 注意：源同为 .tif，创建任务时必须显式声明类型，否则会落到影像切片线。
const TypeDemDesensitize = "dem->desensitized"

// TypeEdit 任务类型：3D Tiles 编辑（M2-F09c）。
// 对已 SUCCEEDED+Approved 的切片任务产物执行内核
// `edit <clip|flatten|ground-align> ...`，产物（b3dm/tileset.json/ops.json）
// 落独立目录并复用审批→分发链路；ParentTaskID 指向被编辑的源任务。
const TypeEdit = "edit"

// OpsReportFile 内核 edit 操作留痕在任务产物目录内的约定文件名（M2-F09c）。
// worker 执行 `edit ... --report <output>/ops.json` 落盘，
// API 下载端点按此约定回读（同 qc-report 模式）。
const OpsReportFile = "ops.json"

// QcReportFile 内核 qc 质检报告在任务产物目录内的约定文件名（M2-F08b）。
// worker 执行 `qc --source <源> --report <output>/qc-report.json` 落盘，
// API 下载端点按此约定回读。
const QcReportFile = "qc-report.json"

// 任务质检状态取值（M2-F08b）。空串表示未请求质检（未检）。
const (
	QcPass    = "pass"    // 质检报告 summary.passed=true
	QcFail    = "fail"    // 质检报告 summary.passed=false（任务仍 SUCCEEDED）
	QcSkipped = "skipped" // 请求了质检但源不适用（如影像任务/源非目录）
)

// QcSummary 质检摘要（M2-F08b）：内核 qc 报告 summary 与 tile_count 的子集，
// 随任务记录持久化，供详情页卡片直接展示（完整报告走下载端点）。
type QcSummary struct {
	TileCount              int `json:"tile_count"`
	TotalDegenerate        int `json:"total_degenerate"`
	TotalFlippedEdges      int `json:"total_flipped_edges"`
	TotalFloating          int `json:"total_floating"`
	TotalCrackSegments     int `json:"total_crack_segments"`
	TotalSelfIntersections int `json:"total_self_intersections"`
}

// Progress 任务分块进度（F-04）：内核产出 manifest 后由 worker 解析回传。
// 指针语义：nil 表示尚无进度（内核未运行/manifest 不存在）。
type Progress struct {
	Done  int `json:"done"`  // 状态为 done 的分块数
	Total int `json:"total"` // 分块总数
}

// Task 一次切片/转换任务（PRD F-02/F-04）。
type Task struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`   // 如 osgb->3dtiles、image->tiles（对应 F-02 各转换线）
	Source       string         `json:"source"` // 源数据路径/引用（MinIO 对象或本地路径）
	Output       string         `json:"output"` // 产物输出路径/引用
	Status       Status         `json:"status"`
	ManifestPath string         `json:"manifest_path,omitempty"` // 内核分块清单路径，约定位于 output 目录内
	ErrMsg       string         `json:"error,omitempty"`         // 失败原因，仅 FAILED 时非空
	MinioPrefix  string         `json:"minio_prefix,omitempty"`  // 产物上传 MinIO 的 key 前缀（tasks/{id}）
	Progress     *Progress      `json:"progress,omitempty"`      // 分块进度（F-04）
	Params       map[string]any `json:"params,omitempty"`        // 任务参数版本化（F-04，JSONB 存储）
	Attempts     int            `json:"attempts"`                // 内核已执行次数（含失败重试，F-04）
	TenantID     string         `json:"tenant_id"`               // 归属租户（F-06 多租户隔离）
	Approved     bool           `json:"approved"`                // 发布审批状态（F-21：审批通过才对外发布）
	// 质检结果（M2-F08b）：params.qc=true 时 worker 在切片成功后跑内核 qc，
	// QcStatus 为空串=未检；QcSummary 为报告摘要（QcStatus 非空时非 nil）。
	QcStatus  string     `json:"qc_status,omitempty"`
	QcSummary *QcSummary `json:"qc_summary,omitempty"`
	// 编辑任务字段（M2-F09c）：ParentTaskID 指向被编辑的源任务（切片任务
	// 或上游编辑任务，构成编辑链）；OpsSummary 为内核 ops.json 留痕的解析
	// 内容（成功后回写，完整原文走 ops-report 下载端点）。
	ParentTaskID string         `json:"parent_task_id,omitempty"`
	OpsSummary   map[string]any `json:"ops_summary,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// ErrNotFound 任务不存在。
var ErrNotFound = errors.New("task not found")

// Store 任务存储接口。
// M1 为内存实现 + PostgreSQL 双实现；tenantID 为空串表示不按租户过滤
// （管理员、worker 内部回写、签名分发路径使用），非空时仅返回该租户任务。
type Store interface {
	Create(t *Task) error
	Get(id, tenantID string) (*Task, error)
	List(tenantID string) ([]*Task, error)
	Update(t *Task) error
	Delete(id, tenantID string) error
}

// MemoryStore Store 的内存实现，读写锁保证并发安全。
type MemoryStore struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

// NewMemoryStore 创建空的内存任务存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{tasks: make(map[string]*Task)}
}

// Create 写入任务；ID 为空时自动生成 uuid。
func (s *MemoryStore) Create(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		id, err := newID()
		if err != nil {
			return err
		}
		t.ID = id
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	s.tasks[t.ID] = t
	return nil
}

// Get 按 ID 查询，不存在返回 ErrNotFound；
// tenantID 非空时任务租户不匹配同样返回 ErrNotFound（隔离语义不泄露存在性）。
func (s *MemoryStore) Get(id, tenantID string) (*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	if tenantID != "" && t.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return t, nil
}

// List 返回任务，按创建时间倒序（最新在前）；tenantID 非空时仅返回该租户任务。
func (s *MemoryStore) List(tenantID string) ([]*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		if tenantID != "" && t.TenantID != tenantID {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Update 覆盖已存在任务，不存在返回 ErrNotFound；自动刷新 UpdatedAt。
func (s *MemoryStore) Update(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[t.ID]; !ok {
		return ErrNotFound
	}
	t.UpdatedAt = time.Now().UTC()
	s.tasks[t.ID] = t
	return nil
}

// Delete 删除任务，不存在（或租户不匹配）返回 ErrNotFound。
func (s *MemoryStore) Delete(id, tenantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return ErrNotFound
	}
	if tenantID != "" && t.TenantID != tenantID {
		return ErrNotFound
	}
	delete(s.tasks, id)
	return nil
}

// newID 生成 32 位十六进制随机任务 ID。
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewID 导出 ID 生成器：导入接口需在解包前确定任务工作目录（含 ID），
// 再带着 ID 落库。
func NewID() (string, error) {
	return newID()
}
