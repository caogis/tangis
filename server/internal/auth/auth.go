// Package auth 实现 TanGIS M1 最小鉴权（PRD F-06）：
//
//   - API Key 校验：客户端携带 X-API-Key，服务端以 SHA-256 哈希比对
//     api_keys 表（PG）或内置默认 key（内存 store 模式）；
//   - HMAC-SHA256 签名防盗链：/services/{task_id}/{file} 分发地址需携带
//     sig + expires 查询参数，密钥来自 env TANGIS_SIGN_SECRET。
//
// 开发兼容：TANGIS_AUTH=off 时调用方跳过鉴权与签名校验（默认 on）。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 角色：admin 可执行发布审批等管理动作；tenant 为普通租户 key。
const (
	RoleAdmin  = "admin"
	RoleTenant = "tenant"
)

// ErrInvalidKey key 不存在或不匹配。
var ErrInvalidKey = errors.New("auth: invalid api key")

// ErrDisabledKey key 已被禁用（api_keys.disabled）。
var ErrDisabledKey = errors.New("auth: api key disabled")

// APIKey 校验通过后的身份信息。
type APIKey struct {
	ID       string // api_keys.id
	Name     string // 便于审计的名称
	TenantID string // 归属租户
	Role     string // admin / tenant
	// DailyTaskLimit 每日（UTC 日）任务创建配额；0=不限额（M2-F14）。
	DailyTaskLimit int
	// Disabled 为 true 时校验直接拒绝（ErrDisabledKey）。
	Disabled bool
}

// KeyStore API Key 校验接口。
type KeyStore interface {
	// Verify 原始 key 校验，通过返回身份，失败返回 ErrInvalidKey。
	Verify(rawKey string) (*APIKey, error)
}

// HashKey 原始 key 的存储哈希（SHA-256 hex）。
func HashKey(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}

// MemoryKeyStore KeyStore 的内存实现：哈希 -> 身份。
// 内存 store 模式（DATABASE_URL 为空）预置一个默认 admin key 便于本地测试。
type MemoryKeyStore struct {
	keys map[string]*APIKey // HashKey(raw) -> identity
}

// NewMemoryKeyStore 创建内存 KeyStore 并预置默认 admin key
// （tenant 为 default，便于本地无 PG 时联调）。
func NewMemoryKeyStore(defaultKey string) *MemoryKeyStore {
	ks := &MemoryKeyStore{keys: make(map[string]*APIKey)}
	ks.keys[HashKey(defaultKey)] = &APIKey{
		ID:       "dev-default",
		Name:     "local development default key",
		TenantID: "default",
		Role:     RoleAdmin,
	}
	return ks
}

// Add 注册一个 key（测试/运维工具用）。
func (s *MemoryKeyStore) Add(rawKey string, key *APIKey) {
	s.keys[HashKey(rawKey)] = key
}

// Verify 实现 KeyStore；disabled key 返回 ErrDisabledKey。
func (s *MemoryKeyStore) Verify(rawKey string) (*APIKey, error) {
	if rawKey == "" {
		return nil, ErrInvalidKey
	}
	key, ok := s.keys[HashKey(rawKey)]
	if !ok {
		return nil, ErrInvalidKey
	}
	if key.Disabled {
		return nil, ErrDisabledKey
	}
	return key, nil
}

// apiKeysDDL api_keys 表结构，与 deploy/initdb/02-auth.sql 一致；
// M2-F14 追加 daily_task_limit/disabled 两列（ADD COLUMN IF NOT EXISTS
// 自动迁移，老库无痛升级）。
const apiKeysDDL = `
CREATE TABLE IF NOT EXISTS api_keys (
    id        text PRIMARY KEY,
    key_hash  text NOT NULL UNIQUE,
    name      text NOT NULL,
    tenant_id text NOT NULL,
    role      text NOT NULL DEFAULT 'tenant',
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS daily_task_limit int NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS disabled boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS api_key_usage (
    key_id text NOT NULL,
    day    date NOT NULL,
    used   int  NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, day)
);
`

// seedAdminKeyHash 内置开发默认 key（tangis-dev-key）的哈希；
// api_keys 表为空时自动播种一个 admin key，保证新建库开箱可用。
// 生产环境应通过 api_keys 表管理正式 key 并删除该默认 key。
const seedAdminKeyHash = "303b67a6146724ec8813d4edc1519352f36c02e93e72e6df2a06b45a0e6d6360"

// PGKeyStore KeyStore 的 PostgreSQL 实现（api_keys 表）。
type PGKeyStore struct {
	pool *pgxpool.Pool
}

// NewPGKeyStore 建立 api_keys 所需连接池并确保表结构存在；
// 表为空时播种默认 admin key（与 initdb 脚本双保险）。
func NewPGKeyStore(ctx context.Context, url string) (*PGKeyStore, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("auth: pg pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("auth: pg ping: %w", err)
	}
	if _, err := pool.Exec(ctx, apiKeysDDL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("auth: pg ensure schema: %w", err)
	}
	ks := &PGKeyStore{pool: pool}
	if err := ks.seedIfEmpty(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("auth: pg seed: %w", err)
	}
	return ks, nil
}

// seedIfEmpty api_keys 无任何记录时插入默认 admin key。
func (s *PGKeyStore) seedIfEmpty(ctx context.Context) error {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO api_keys (id, key_hash, name, tenant_id, role)
VALUES ('dev-default', $1, 'local development default key', 'default', 'admin')
ON CONFLICT (id) DO NOTHING`, seedAdminKeyHash)
	return err
}

// EnsureRawKey 确保原始 key 在表中以指定身份存在（M2-F14 兼容迁移：
// 把既有 TANGIS_API_KEY 单 key 部署平滑迁入多 Key 表；已存在同 hash 时
// 不动，避免覆盖运维手工调整的限额/禁用状态）。
func (s *PGKeyStore) EnsureRawKey(ctx context.Context, rawKey, name, tenantID, role string) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO api_keys (id, key_hash, name, tenant_id, role)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (key_hash) DO NOTHING`,
		"env-"+HashKey(rawKey)[:12], HashKey(rawKey), name, tenantID, role)
	if err != nil {
		return fmt.Errorf("auth: pg ensure raw key: %w", err)
	}
	return nil
}

// Close 释放连接池。
func (s *PGKeyStore) Close() { s.pool.Close() }

// Verify 实现 KeyStore；disabled key 返回 ErrDisabledKey。
func (s *PGKeyStore) Verify(rawKey string) (*APIKey, error) {
	if rawKey == "" {
		return nil, ErrInvalidKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var key APIKey
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, tenant_id, role, daily_task_limit, disabled FROM api_keys WHERE key_hash = $1`,
		HashKey(rawKey)).Scan(&key.ID, &key.Name, &key.TenantID, &key.Role, &key.DailyTaskLimit, &key.Disabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidKey
		}
		return nil, fmt.Errorf("auth: pg verify: %w", err)
	}
	if key.Disabled {
		return nil, ErrDisabledKey
	}
	return &key, nil
}

// SignURL 对分发路径生成防盗链签名参数（F-06）。
// 签名内容为 "<path>\n<expires>"，HMAC-SHA256 后取 hex；
// path 必须与请求 URL path 完全一致（不含 query）。
// 返回形如 "expires=<unix>&sig=<hex>" 的查询串。
func SignURL(path string, expires time.Time, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(path))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(strconv.FormatInt(expires.Unix(), 10)))
	return "expires=" + strconv.FormatInt(expires.Unix(), 10) + "&sig=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature 校验分发请求的防盗链签名。
// expires 已过期或签名不匹配均返回 false；比较使用恒定时间比较防时序侧信道。
func VerifySignature(path string, expires int64, sigHex, secret string) bool {
	if expires < time.Now().Unix() || sigHex == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(path))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(strconv.FormatInt(expires, 10)))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sigHex)
	if err != nil || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}
