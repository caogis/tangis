package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newUnreachableClient 指向保留地址的客户端（测试内不实际发起长连接，
// go-redis 读超时按 miss 处理）。
func newUnreachableClient() *redis.Client {
	return redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, ReadTimeout: 100 * time.Millisecond})
}

func TestMemoryCacheSetGet(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()

	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("empty cache should miss")
	}
	c.Set(ctx, "k", []byte("v1"), time.Minute)
	v, ok := c.Get(ctx, "k")
	if !ok || string(v) != "v1" {
		t.Fatalf("got %q ok=%v, want v1", v, ok)
	}
	c.Delete(ctx, "k")
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("deleted key should miss")
	}
}

func TestMemoryCacheTTLExpiry(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()
	c.Set(ctx, "k", []byte("v"), 10*time.Millisecond)
	if _, ok := c.Get(ctx, "k"); !ok {
		t.Fatal("fresh key should hit")
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("expired key should miss")
	}
}

func TestRedisCacheWithMiniredis(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	rc, err := Dial(ctx, mr.Addr())
	if err != nil {
		t.Fatalf("dial miniredis: %v", err)
	}
	defer rc.Close()

	// key 前缀 tangis: 且 TTL 生效
	rc.Set(ctx, "tile:1", []byte("PNGDATA"), time.Minute)
	if !mr.Exists("tangis:tile:1") {
		t.Fatal("redis key should be prefixed with tangis:")
	}
	if v, ok := rc.Get(ctx, "tile:1"); !ok || string(v) != "PNGDATA" {
		t.Fatalf("get = %q ok=%v", v, ok)
	}

	// TTL 过期
	rc.Set(ctx, "tile:2", []byte("x"), 10*time.Millisecond)
	mr.FastForward(50 * time.Millisecond)
	if _, ok := rc.Get(ctx, "tile:2"); ok {
		t.Fatal("expired key should miss")
	}

	rc.Delete(ctx, "tile:1")
	if _, ok := rc.Get(ctx, "tile:1"); ok {
		t.Fatal("deleted key should miss")
	}
}

func TestRedisCacheMissIsNotDegradation(t *testing.T) {
	// redis.Nil（键不存在）是正常 miss，不得触发降级态（不刷 WARNING 日志）。
	mr := miniredis.RunT(t)
	ctx := context.Background()

	rc, err := Dial(ctx, mr.Addr())
	if err != nil {
		t.Fatalf("dial miniredis: %v", err)
	}
	defer rc.Close()

	if _, ok := rc.Get(ctx, "never-set"); ok {
		t.Fatal("missing key should miss")
	}
	if rc.failing.Load() {
		t.Fatal("a normal miss must NOT mark the cache as failing")
	}

	// miss 后回填、再取应命中
	rc.Set(ctx, "k", []byte("v"), time.Minute)
	if v, ok := rc.Get(ctx, "k"); !ok || string(v) != "v" {
		t.Fatalf("get after set = %q ok=%v", v, ok)
	}
}

func TestRedisCacheGracefulDegradation(t *testing.T) {
	// 连不上的地址：Get 按 miss、Set/Delete 静默，绝不 panic/error 泄漏
	rc := &RedisCache{rdb: newUnreachableClient()}
	ctx := context.Background()

	if _, ok := rc.Get(ctx, "k"); ok {
		t.Fatal("unreachable redis should miss")
	}
	rc.Set(ctx, "k", []byte("v"), time.Minute) // 不应 panic
	rc.Delete(ctx, "k")
}

func TestCacheInterfaceCompliance(t *testing.T) {
	var _ Cache = (*RedisCache)(nil)
	var _ Cache = (*MemoryCache)(nil)
}
