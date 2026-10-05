// PGStore：task.Store 的 PostgreSQL 持久化实现（pgx v5 + pgxpool）。
//
// 表结构见 deploy/initdb/02-tasks.sql；NewPGStore 启动时会执行
// CREATE TABLE IF NOT EXISTS + ALTER TABLE ADD COLUMN IF NOT EXISTS
// （与 initdb 脚本双保险，兼容已有库与已有列）。DATABASE_URL 有值时
// apiserver 使用本实现，为空回退内存实现。
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaDDL 建表 + 兼容性加列，与 deploy/initdb/02-tasks.sql 保持一致。
const schemaDDL = `
CREATE TABLE IF NOT EXISTS tasks (
    id            text PRIMARY KEY,
    type          text NOT NULL,
    source        text NOT NULL,
    output        text NOT NULL,
    status        text NOT NULL,
    manifest_path text,
    error         text,
    minio_prefix  text,
    tenant_id     text NOT NULL DEFAULT '',
    params        jsonb,
    progress_done int  NOT NULL DEFAULT 0,
    progress_total int NOT NULL DEFAULT 0,
    attempts      int  NOT NULL DEFAULT 0,
    approved      boolean NOT NULL DEFAULT false,
    qc_status     text NOT NULL DEFAULT '',
    qc_summary    jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
-- 已有库平滑升级（F-04/F-06/F-21 新增列）
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS params jsonb;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS progress_done int NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS progress_total int NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS attempts int NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS approved boolean NOT NULL DEFAULT false;
-- M2-F08b 质检字段
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS qc_status text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS qc_summary jsonb;
-- M2-F09c 编辑任务字段
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS parent_task_id text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS ops_summary jsonb;
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status);
CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_tenant ON tasks (tenant_id);
CREATE INDEX IF NOT EXISTS idx_tasks_parent ON tasks (parent_task_id);
`

// taskColumns 读取列（可空列用 COALESCE 映射为空值，简化扫描）。
const taskColumns = `id, type, source, output, status,
	COALESCE(manifest_path, ''), COALESCE(error, ''), COALESCE(minio_prefix, ''),
	COALESCE(tenant_id, ''), params, progress_done, progress_total,
	attempts, approved, COALESCE(qc_status, ''), qc_summary,
	COALESCE(parent_task_id, ''), ops_summary, created_at, updated_at`

// PGStore Store 的 PostgreSQL 实现。
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore 建立连接池并确保表结构存在。ctx 建议带超时，
// 建连失败（PG 不可用/鉴权错误）返回 error，由调用方决定回退策略。
func NewPGStore(ctx context.Context, url string) (*PGStore, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("task: pg pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("task: pg ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaDDL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("task: pg ensure schema: %w", err)
	}
	return &PGStore{pool: pool}, nil
}

// Close 释放连接池。
func (s *PGStore) Close() { s.pool.Close() }

// Create 写入任务；ID 为空时自动生成，时间戳由存储侧统一赋值。
func (s *PGStore) Create(t *Task) error {
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

	progress := t.Progress
	if progress == nil {
		progress = &Progress{}
	}
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `
INSERT INTO tasks (id, type, source, output, status, manifest_path, error, minio_prefix,
	tenant_id, params, progress_done, progress_total, attempts, approved,
	qc_status, qc_summary, parent_task_id, ops_summary, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		t.ID, t.Type, t.Source, t.Output, string(t.Status),
		emptyToNull(t.ManifestPath), emptyToNull(t.ErrMsg), emptyToNull(t.MinioPrefix),
		t.TenantID, paramsToJSONB(t.Params), progress.Done, progress.Total,
		t.Attempts, t.Approved, t.QcStatus, qcSummaryToJSONB(t.QcSummary),
		t.ParentTaskID, mapToJSONB(t.OpsSummary),
		t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("task: pg create %s: %w", t.ID, err)
	}
	return nil
}

// Get 按 ID 查询；tenantID 非空时限定租户，不存在/不匹配返回 ErrNotFound。
func (s *PGStore) Get(id, tenantID string) (*Task, error) {
	ctx := context.Background()
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE id = $1`
	args := []any{id}
	if tenantID != "" {
		query += ` AND tenant_id = $2`
		args = append(args, tenantID)
	}
	t, err := scanTask(s.pool.QueryRow(ctx, query, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("task: pg get %s: %w", id, err)
	}
	return t, nil
}

// List 返回任务，按创建时间倒序；tenantID 非空时仅返回该租户任务。
func (s *PGStore) List(tenantID string) ([]*Task, error) {
	ctx := context.Background()
	query := `SELECT ` + taskColumns + ` FROM tasks`
	args := []any{}
	if tenantID != "" {
		query += ` WHERE tenant_id = $1`
		args = append(args, tenantID)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("task: pg list: %w", err)
	}
	defer rows.Close()

	out := make([]*Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("task: pg list scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task: pg list rows: %w", err)
	}
	return out, nil
}

// Update 覆盖已存在任务，不存在返回 ErrNotFound；自动刷新 UpdatedAt。
func (s *PGStore) Update(t *Task) error {
	t.UpdatedAt = time.Now().UTC()
	progress := t.Progress
	if progress == nil {
		progress = &Progress{}
	}
	ctx := context.Background()
	tag, err := s.pool.Exec(ctx, `
UPDATE tasks SET type=$2, source=$3, output=$4, status=$5,
	manifest_path=$6, error=$7, minio_prefix=$8,
	tenant_id=$9, params=$10, progress_done=$11, progress_total=$12,
	attempts=$13, approved=$14, qc_status=$15, qc_summary=$16,
	parent_task_id=$17, ops_summary=$18, updated_at=$19
WHERE id=$1`,
		t.ID, t.Type, t.Source, t.Output, string(t.Status),
		emptyToNull(t.ManifestPath), emptyToNull(t.ErrMsg), emptyToNull(t.MinioPrefix),
		t.TenantID, paramsToJSONB(t.Params), progress.Done, progress.Total,
		t.Attempts, t.Approved, t.QcStatus, qcSummaryToJSONB(t.QcSummary),
		t.ParentTaskID, mapToJSONB(t.OpsSummary),
		t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("task: pg update %s: %w", t.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete 删除任务；tenantID 非空时限定租户，不存在返回 ErrNotFound。
func (s *PGStore) Delete(id, tenantID string) error {
	ctx := context.Background()
	query := `DELETE FROM tasks WHERE id=$1`
	args := []any{id}
	if tenantID != "" {
		query += ` AND tenant_id=$2`
		args = append(args, tenantID)
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("task: pg delete %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// scanTask 从一行扫描出 Task（row 兼容 QueryRow 与 Rows）。
func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	var status, qcStatus, parentTaskID string
	var params, qcSummary, opsSummary []byte
	var done, total int
	if err := row.Scan(&t.ID, &t.Type, &t.Source, &t.Output, &status,
		&t.ManifestPath, &t.ErrMsg, &t.MinioPrefix,
		&t.TenantID, &params, &done, &total,
		&t.Attempts, &t.Approved, &qcStatus, &qcSummary,
		&parentTaskID, &opsSummary, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Status = Status(status)
	t.QcStatus = qcStatus
	t.ParentTaskID = parentTaskID
	if len(params) > 0 {
		if err := json.Unmarshal(params, &t.Params); err != nil {
			return nil, fmt.Errorf("task: pg scan params: %w", err)
		}
	}
	if len(qcSummary) > 0 {
		s := &QcSummary{}
		if err := json.Unmarshal(qcSummary, s); err != nil {
			return nil, fmt.Errorf("task: pg scan qc_summary: %w", err)
		}
		t.QcSummary = s
	}
	if len(opsSummary) > 0 {
		m := map[string]any{}
		if err := json.Unmarshal(opsSummary, &m); err != nil {
			return nil, fmt.Errorf("task: pg scan ops_summary: %w", err)
		}
		t.OpsSummary = m
	}
	if done != 0 || total != 0 {
		t.Progress = &Progress{Done: done, Total: total}
	}
	return &t, nil
}

// emptyToNull 空串落库为 NULL（可空列语义）。
func emptyToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// paramsToJSONB params map 转 jsonb；nil/空 map 落 NULL。
func paramsToJSONB(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(data)
}

// qcSummaryToJSONB 质检摘要转 jsonb；nil 落 NULL。
func qcSummaryToJSONB(s *QcSummary) any {
	if s == nil {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return string(data)
}

// mapToJSONB 通用 map（ops_summary 等）转 jsonb；nil/空 map 落 NULL。
func mapToJSONB(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(data)
}
