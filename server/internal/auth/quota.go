// quota.go — 每日任务创建配额（M2-F14）：
//
//   - 用量按 (key_id, UTC 日) 计数，内存 + PG 双实现（PG 持久化，重启不丢）；
//   - 超限由 api 层任务配额中间件拦截：429 + Retry-After（UTC 次日零点秒数）；
//   - Now 支持时间注入，供跨日重置测试。
//
// 语义：
//   - APIKey.DailyTaskLimit > 0 时启用配额；<=0（含 0 默认）不限额；
//   - admin 角色 key 不限额（管理端不受租户配额约束）；
//   - TANGIS_AUTH=off 时整个配额链路跳过。
package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UsageStore 每日用量存储抽象（day 为 UTC 日期，格式 2006-01-02）。
type UsageStore interface {
	// Usage 查询 key 当日已用次数（无记录返回 0）。
	Usage(ctx context.Context, keyID, day string) (int, error)
	// Increment 原子递增并返回递增后的当日次数。
	Increment(ctx context.Context, keyID, day string) (int, error)
}

// MemoryUsage 内存用量存储（无 PG / 测试）。
type MemoryUsage struct {
	mu   sync.Mutex
	used map[string]int // keyID + "|" + day -> count
}

// NewMemoryUsage 创建内存用量存储。
func NewMemoryUsage() *MemoryUsage { return &MemoryUsage{used: map[string]int{}} }

// Usage 实现 UsageStore。
func (m *MemoryUsage) Usage(_ context.Context, keyID, day string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used[keyID+"|"+day], nil
}

// Increment 实现 UsageStore。
func (m *MemoryUsage) Increment(_ context.Context, keyID, day string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.used[keyID+"|"+day]++
	return m.used[keyID+"|"+day], nil
}

// PGUsage UsageStore 的 PostgreSQL 实现（api_key_usage 表，DDL 见 auth.go）。
type PGUsage struct {
	pool *pgxpool.Pool
}

// Usage 每日用量存储（与 KeyStore 共用连接池）。
func (s *PGKeyStore) Usage() *PGUsage { return &PGUsage{pool: s.pool} }

// Usage 实现 UsageStore。
func (u *PGUsage) Usage(ctx context.Context, keyID, day string) (int, error) {
	var n int
	err := u.pool.QueryRow(ctx,
		`SELECT used FROM api_key_usage WHERE key_id=$1 AND day=$2`, keyID, day).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Increment 实现 UsageStore（原子 upsert）。
func (u *PGUsage) Increment(ctx context.Context, keyID, day string) (int, error) {
	var n int
	err := u.pool.QueryRow(ctx, `
INSERT INTO api_key_usage (key_id, day, used) VALUES ($1, $2, 1)
ON CONFLICT (key_id, day) DO UPDATE SET used = api_key_usage.used + 1
RETURNING used`, keyID, day).Scan(&n)
	return n, err
}

// Quota 配额判定/记账入口。Store 为 nil 时整体禁用（Check 恒放行）。
type Quota struct {
	Store UsageStore
	// Now 时间源（默认 time.Now），测试注入做跨日重置。
	Now func() time.Time
}

func (q *Quota) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Day 当前 UTC 日期串（配额重置粒度）。
func (q *Quota) Day() string { return q.now().UTC().Format("2006-01-02") }

// RetryAfterSeconds 距下一 UTC 日零点的秒数（429 Retry-After 取值）。
func (q *Quota) RetryAfterSeconds() int {
	n := q.now().UTC()
	next := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	return int(next.Sub(n).Seconds()) + 1
}

// Check 判定 key 是否还能创建任务。limit<=0 或 Store 为 nil 恒放行。
// 返回 (是否放行, 当日已用, 错误)。
func (q *Quota) Check(ctx context.Context, keyID string, limit int) (bool, int, error) {
	if q.Store == nil || limit <= 0 {
		return true, 0, nil
	}
	used, err := q.Store.Usage(ctx, keyID, q.Day())
	if err != nil {
		return false, 0, err
	}
	return used < limit, used, nil
}

// Record 创建成功后记账（原子递增）。Store 为 nil 时为空操作。
func (q *Quota) Record(ctx context.Context, keyID string) error {
	if q.Store == nil {
		return nil
	}
	_, err := q.Store.Increment(ctx, keyID, q.Day())
	return err
}
