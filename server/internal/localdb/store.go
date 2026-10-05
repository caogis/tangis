// Package localdb 提供 TanGIS 桌面单机版（Go + SQLite）的本地持久化实现。
//
// 桌面零依赖模式下替代 PostgreSQL：同一个进程、一个 SQLite 文件，
// 同时承载三张表：
//   - tasks：任务记录（F-04，对应 internal/task.Store）；
//   - api_keys：API Key 身份（F-06，对应 internal/auth.KeyStore）；
//   - api_key_usage：每日配额用量（M2-F14，对应 internal/auth.UsageStore）。
//
// 驱动选型：modernc.org/sqlite（纯 Go，无 cgo）——牺牲少量性能换取
// macOS/Windows/Linux 三平台交叉编译无障碍，这是「双击即用」交付的前提。
//
// 时间列统一存 RFC3339Nano 文本，避免驱动差异导致的时间解析歧义；
// 所有写操作经单连接串行化（SetMaxOpenConns(1)），天然规避 SQLite 写锁竞争。
package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动（纯 Go，无 cgo）

	"tangis/server/internal/auth"
	"tangis/server/internal/task"
)

// ddl 建表语句（SQLite 语法，占位符为 ?）。
const ddl = `
CREATE TABLE IF NOT EXISTS tasks (
    id            text PRIMARY KEY,
    type          text NOT NULL,
    source        text NOT NULL,
    output        text NOT NULL,
    status        text NOT NULL,
    manifest_path text,
    error         text,
    minio_prefix  text,
    progress_done integer,
    progress_total integer,
    params        text,
    attempts      integer NOT NULL DEFAULT 0,
    tenant_id     text NOT NULL DEFAULT '',
    approved      integer NOT NULL DEFAULT 0,
    qc_status     text NOT NULL DEFAULT '',
    qc_summary    text,
    parent_task_id text NOT NULL DEFAULT '',
    ops_summary   text,
    created_at    text NOT NULL,
    updated_at    text NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks (created_at DESC);

CREATE TABLE IF NOT EXISTS api_keys (
    id               text PRIMARY KEY,
    key_hash         text NOT NULL UNIQUE,
    name             text NOT NULL,
    tenant_id        text NOT NULL DEFAULT '',
    role             text NOT NULL DEFAULT 'tenant',
    daily_task_limit integer NOT NULL DEFAULT 0,
    disabled         integer NOT NULL DEFAULT 0,
    created_at       text NOT NULL
);

CREATE TABLE IF NOT EXISTS api_key_usage (
    key_id text NOT NULL,
    day    text NOT NULL,
    used   integer NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, day)
);
`

// Store 本地持久化入口，同时实现：
//   - task.Store（任务）
//   - auth.KeyStore（API Key 校验）
//   - auth.UsageStore（每日配额用量）
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）SQLite 文件并执行建表。
// file 为数据库文件路径；建议由调用方放在用户数据目录下。
func Open(file string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", file)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("localdb: open %s: %w", file, err)
	}
	// 单连接串行写：SQLite 写锁粗粒度，多连接反而竞争失败；桌面场景单用户足够
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("localdb: ping %s: %w", file, err)
	}
	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		return nil, fmt.Errorf("localdb: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 释放数据库连接。
func (s *Store) Close() { s.db.Close() }

// DB 暴露底层句柄（便于运维/测试直接查询）。
func (s *Store) DB() *sql.DB { return s.db }

// ---------------------------------------------------------------- 任务存储

const taskColumns = `id, type, source, output, status,
	COALESCE(manifest_path,''), COALESCE(error,''), COALESCE(minio_prefix,''),
	progress_done, progress_total, params, attempts, tenant_id, approved,
	COALESCE(qc_status,''), qc_summary, COALESCE(parent_task_id,''), ops_summary,
	created_at, updated_at`

func scanTask(row interface{ Scan(dest ...any) error }) (*task.Task, error) {
	var (
		t            task.Task
		status       string
		mf, er, mp   string
		pDone, pTot  sql.NullInt64
		params       sql.NullString
		approved     int
		qcStatus     string
		qcSummary    sql.NullString
		parentTaskID string
		opsSummary   sql.NullString
		created, upd string
	)
	if err := row.Scan(&t.ID, &t.Type, &t.Source, &t.Output, &status,
		&mf, &er, &mp, &pDone, &pTot, &params, &t.Attempts, &t.TenantID, &approved,
		&qcStatus, &qcSummary, &parentTaskID, &opsSummary,
		&created, &upd); err != nil {
		return nil, err
	}
	t.Status = task.Status(status)
	t.ManifestPath = mf
	t.ErrMsg = er
	t.MinioPrefix = mp
	if pDone.Valid && pTot.Valid {
		t.Progress = &task.Progress{Done: int(pDone.Int64), Total: int(pTot.Int64)}
	}
	if params.Valid && params.String != "" {
		_ = json.Unmarshal([]byte(params.String), &t.Params)
	}
	t.Approved = approved != 0
	t.QcStatus = qcStatus
	if qcSummary.Valid && qcSummary.String != "" {
		var qs task.QcSummary
		if err := json.Unmarshal([]byte(qcSummary.String), &qs); err == nil {
			t.QcSummary = &qs
		}
	}
	t.ParentTaskID = parentTaskID
	if opsSummary.Valid && opsSummary.String != "" {
		_ = json.Unmarshal([]byte(opsSummary.String), &t.OpsSummary)
	}
	t.CreatedAt = parseTime(created)
	t.UpdatedAt = parseTime(upd)
	return &t, nil
}

// Create 写入任务；ID 为空时生成随机 ID，时间戳由存储侧赋值。
func (s *Store) Create(t *task.Task) error {
	if t.ID == "" {
		id, err := task.NewID()
		if err != nil {
			return err
		}
		t.ID = id
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	params, err := marshalJSON(t.Params)
	if err != nil {
		return err
	}
	qcSummary, err := marshalJSON(t.QcSummary)
	if err != nil {
		return err
	}
	opsSummary, err := marshalJSON(t.OpsSummary)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO tasks (`+strings.Join([]string{
		"id", "type", "source", "output", "status", "manifest_path", "error",
		"minio_prefix", "progress_done", "progress_total", "params", "attempts",
		"tenant_id", "approved", "qc_status", "qc_summary", "parent_task_id",
		"ops_summary", "created_at", "updated_at",
	}, ",")+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Type, t.Source, t.Output, string(t.Status),
		nullIfEmpty(t.ManifestPath), nullIfEmpty(t.ErrMsg), nullIfEmpty(t.MinioPrefix),
		progressValue(t.Progress, true), progressValue(t.Progress, false),
		params, t.Attempts, t.TenantID, boolToInt(t.Approved),
		t.QcStatus, qcSummary, t.ParentTaskID, opsSummary,
		formatTime(t.CreatedAt), formatTime(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("localdb: create task %s: %w", t.ID, err)
	}
	return nil
}

// Get 按 ID 查询；tenantID 非空时租户不匹配返回 ErrNotFound（不泄露存在性）。
func (s *Store) Get(id, tenantID string) (*task.Task, error) {
	row := s.db.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, task.ErrNotFound
		}
		return nil, fmt.Errorf("localdb: get task %s: %w", id, err)
	}
	if tenantID != "" && t.TenantID != tenantID {
		return nil, task.ErrNotFound
	}
	return t, nil
}

// List 返回任务，按创建时间倒序；tenantID 非空时仅返回该租户任务。
func (s *Store) List(tenantID string) ([]*task.Task, error) {
	q := `SELECT ` + taskColumns + ` FROM tasks`
	var args []any
	if tenantID != "" {
		q += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("localdb: list tasks: %w", err)
	}
	defer rows.Close()
	out := make([]*task.Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("localdb: scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Update 覆盖已存在任务；不存在返回 ErrNotFound，自动刷新 UpdatedAt。
func (s *Store) Update(t *task.Task) error {
	t.UpdatedAt = time.Now().UTC()
	params, err := marshalJSON(t.Params)
	if err != nil {
		return err
	}
	qcSummary, err := marshalJSON(t.QcSummary)
	if err != nil {
		return err
	}
	opsSummary, err := marshalJSON(t.OpsSummary)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE tasks SET type=?, source=?, output=?, status=?,
		manifest_path=?, error=?, minio_prefix=?, progress_done=?, progress_total=?,
		params=?, attempts=?, tenant_id=?, approved=?, qc_status=?, qc_summary=?,
		parent_task_id=?, ops_summary=?, updated_at=? WHERE id=?`,
		t.Type, t.Source, t.Output, string(t.Status),
		nullIfEmpty(t.ManifestPath), nullIfEmpty(t.ErrMsg), nullIfEmpty(t.MinioPrefix),
		progressValue(t.Progress, true), progressValue(t.Progress, false),
		params, t.Attempts, t.TenantID, boolToInt(t.Approved),
		t.QcStatus, qcSummary, t.ParentTaskID, opsSummary,
		formatTime(t.UpdatedAt), t.ID)
	if err != nil {
		return fmt.Errorf("localdb: update task %s: %w", t.ID, err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return task.ErrNotFound
	}
	return nil
}

// Delete 删除任务；不存在或租户不匹配返回 ErrNotFound。
func (s *Store) Delete(id, tenantID string) error {
	if tenantID != "" {
		if _, err := s.Get(id, tenantID); err != nil {
			return err
		}
	}
	res, err := s.db.Exec(`DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("localdb: delete task %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return task.ErrNotFound
	}
	return nil
}

// ------------------------------------------------------------- API Key 存储

// Verify 实现 auth.KeyStore；disabled key 返回 auth.ErrDisabledKey。
func (s *Store) Verify(rawKey string) (*auth.APIKey, error) {
	if rawKey == "" {
		return nil, auth.ErrInvalidKey
	}
	var k auth.APIKey
	var disabled int
	err := s.db.QueryRow(
		`SELECT id, name, tenant_id, role, daily_task_limit, disabled FROM api_keys WHERE key_hash = ?`,
		auth.HashKey(rawKey)).Scan(&k.ID, &k.Name, &k.TenantID, &k.Role, &k.DailyTaskLimit, &disabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrInvalidKey
		}
		return nil, fmt.Errorf("localdb: verify key: %w", err)
	}
	k.Disabled = disabled != 0
	if k.Disabled {
		return nil, auth.ErrDisabledKey
	}
	return &k, nil
}

// EnsureRawKey 确保原始 key 以指定身份存在（幂等，已存在则不动）。
// 签名与 auth.PGKeyStore 一致，便于 main.go 的通用迁移调用直接生效。
func (s *Store) EnsureRawKey(_ context.Context, rawKey, name, tenantID, role string) error {
	return s.SeedKey(rawKey, name, tenantID, role)
}

// SeedKey 确保原始 key 以指定身份存在（已存在则不动）。
func (s *Store) SeedKey(rawKey, name, tenantID, role string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO api_keys
		(id, key_hash, name, tenant_id, role, created_at) VALUES (?,?,?,?,?,?)`,
		"env-"+auth.HashKey(rawKey)[:12], auth.HashKey(rawKey), name, tenantID, role, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("localdb: seed key: %w", err)
	}
	return nil
}

// KeyStore 返回本 Store 自身的 KeyStore 视图（语义明确，便于注入）。
func (s *Store) KeyStore() auth.KeyStore { return s }

// ------------------------------------------------------------ 配额用量存储

// Usage 实现 auth.UsageStore：查询 key 当日已用次数（无记录返回 0）。
func (s *Store) Usage(_ context.Context, keyID, day string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT used FROM api_key_usage WHERE key_id = ? AND day = ?`, keyID, day).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("localdb: usage: %w", err)
	}
	return n, nil
}

// Increment 实现 auth.UsageStore：原子递增并返回递增后的当日次数。
// 不使用 RETURNING（SQLite ≥3.35），改事务 + update/insert 双路径，兼容较老运行库。
func (s *Store) Increment(_ context.Context, keyID, day string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("localdb: begin usage tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(`UPDATE api_key_usage SET used = used + 1 WHERE key_id = ? AND day = ?`, keyID, day)
	if err != nil {
		return 0, fmt.Errorf("localdb: update usage: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		if _, err := tx.Exec(`INSERT INTO api_key_usage (key_id, day, used) VALUES (?,?,1)`, keyID, day); err != nil {
			return 0, fmt.Errorf("localdb: insert usage: %w", err)
		}
	}
	var used int
	if err := tx.QueryRow(`SELECT used FROM api_key_usage WHERE key_id = ? AND day = ?`, keyID, day).Scan(&used); err != nil {
		return 0, fmt.Errorf("localdb: read usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("localdb: commit usage: %w", err)
	}
	return used, nil
}

// ------------------------------------------------------------------ 工具函数

func marshalJSON(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("localdb: marshal: %w", err)
	}
	s := string(b)
	if s == "null" || s == "{}" {
		return nil, nil
	}
	return s, nil
}

func progressValue(p *task.Progress, wantDone bool) any {
	if p == nil {
		return nil
	}
	if wantDone {
		return p.Done
	}
	return p.Total
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
