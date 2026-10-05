// Package cache 提供瓦片/列表响应缓存（F-03 Redis 接入）：
//
//   - Cache 接口：业务层只依赖抽象，便于测试与降级；
//   - RedisCache：go-redis/v9 实现，对接 deploy 里的 Redis(默认 16379)；
//     Redis 不可用时优雅降级——读写错误只记日志并按 miss 处理，
//     绝不向调用方返回 error 导致业务 500；
//   - MemoryCache：进程内 map 实现（测试与无 Redis 环境兜底）。
//
// 环境变量：TANGIS_REDIS_ADDR（默认 127.0.0.1:16379，回退 REDIS_ADDR）、
// TANGIS_CACHE_TTL（瓦片缓存 TTL 秒，默认 300）、
// TANGIS_LIST_CACHE_TTL（列表缓存 TTL 秒，默认 5）。
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache 缓存抽象：字节值 + TTL。
// 实现必须保证任何错误都不向上传播失败语义（按 miss/无操作处理）。
type Cache interface {
	// Get 取缓存值；未命中（含后端不可用）返回 ok=false。
	Get(ctx context.Context, key string) (val []byte, ok bool)
	// Set 写缓存值（含后端不可用时静默丢弃）。
	Set(ctx context.Context, key string, val []byte, ttl time.Duration)
	// Delete 删除缓存（写路径一致性失效用）。
	Delete(ctx context.Context, key string)
}

// RedisCache Cache 的 Redis 实现（go-redis/v9）。
// 降级策略：连接由 Dial 失败直接暴露（启动时决定是否装配）；
// 运行期读写错误记 WARNING 日志（每次故障只记一次，恢复时记一次 INFO），
// Get 按 miss、Set/Delete 按无操作处理。
type RedisCache struct {
	rdb *redis.Client

	failing atomic.Bool // 当前是否处于降级态（用于只记一次日志）
}

// Dial 建立 Redis 连接并 Ping 校验；失败返回 error 由调用方决定降级。
// ctx 建议带超时。
func Dial(ctx context.Context, addr string) (*RedisCache, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, err
	}
	return &RedisCache{rdb: rdb}, nil
}

// Close 释放连接。
func (c *RedisCache) Close() error { return c.rdb.Close() }

// keyPrefix 缓存 key 统一前缀，避免与其他业务混用同一 Redis 实例时冲突。
const keyPrefix = "tangis:"

// Get 实现 Cache。
func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, bool) {
	val, err := c.rdb.Get(ctx, keyPrefix+key).Bytes()
	if err != nil {
		// redis.Nil 是正常未命中，不是故障，不进降级态。
		if !errors.Is(err, redis.Nil) {
			c.degrade(err)
		}
		return nil, false
	}
	c.recovered()
	return val, true
}

// Set 实现 Cache。
func (c *RedisCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	if err := c.rdb.Set(ctx, keyPrefix+key, val, ttl).Err(); err != nil {
		c.degrade(err)
		return
	}
	c.recovered()
}

// Delete 实现 Cache。
func (c *RedisCache) Delete(ctx context.Context, key string) {
	if err := c.rdb.Del(ctx, keyPrefix+key).Err(); err != nil {
		c.degrade(err)
		return
	}
	c.recovered()
}

// degrade 进入/维持降级态：每次故障沿只打一条 WARNING。
func (c *RedisCache) degrade(err error) {
	if c.failing.CompareAndSwap(false, true) {
		log.Printf("WARNING: redis cache unavailable, falling back to direct storage reads: %v", err)
	}
}

// recovered 从降级态恢复：只打一次 INFO。
func (c *RedisCache) recovered() {
	if c.failing.CompareAndSwap(true, false) {
		log.Printf("redis cache recovered")
	}
}

// MemoryCache Cache 的进程内实现（含 TTL 过期），测试与无 Redis 兜底用。
type MemoryCache struct {
	mu      sync.RWMutex
	entries map[string]memoryEntry
}

type memoryEntry struct {
	val     []byte
	expires time.Time // 零值表示永不过期
}

// NewMemoryCache 创建空内存缓存。
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: make(map[string]memoryEntry)}
}

// Get 实现 Cache。
func (c *MemoryCache) Get(_ context.Context, key string) ([]byte, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		return nil, false
	}
	return e.val, true
}

// Set 实现 Cache。
func (c *MemoryCache) Set(_ context.Context, key string, val []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := memoryEntry{val: append([]byte(nil), val...)}
	if ttl > 0 {
		e.expires = time.Now().Add(ttl)
	}
	c.entries[key] = e
}

// Delete 实现 Cache。
func (c *MemoryCache) Delete(_ context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// ListKeys 列表缓存的 key 约定（写路径失效用）：
// scope 为查询租户范围（admin 空串=全租户）。
func ListKeys(kind, scope string) string {
	return "list:" + kind + ":" + scope
}

// CachedJSON 通用「缓存优先的 JSON 列表/对象」助手：
// 命中直接返回缓存字节；未命中执行 produce 并回填。
// cc 为 nil 或 ttl < 0 时禁用缓存（直连 produce）。
// 返回 (响应字节, 是否命中缓存, 业务错误)。
func CachedJSON(ctx context.Context, cc Cache, key string, ttl time.Duration,
	produce func() (any, error)) ([]byte, bool, error) {
	enabled := cc != nil && ttl >= 0
	if enabled {
		if v, ok := cc.Get(ctx, key); ok {
			return v, true, nil
		}
	}
	payload, err := produce()
	if err != nil {
		return nil, false, err
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	if enabled {
		cc.Set(ctx, key, b, ttl)
	}
	return b, false, nil
}
