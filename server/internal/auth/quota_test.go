// quota_test.go — 每日任务配额（M2-F14）：内存用量、跨日重置（时间注入）、
// Retry-After、PG 持久化与多 Key 兼容迁移（PG 不可用跳过）。
package auth

import (
	"context"
	"testing"
	"time"
)

func TestQuotaCheckAndRecord(t *testing.T) {
	q := &Quota{Store: NewMemoryUsage()}
	ctx := context.Background()

	// limit<=0 / Store nil 恒放行
	qNil := &Quota{}
	if ok, _, _ := qNil.Check(ctx, "k1", 5); !ok {
		t.Fatal("nil store should allow")
	}
	if ok, _, _ := q.Check(ctx, "k1", 0); !ok {
		t.Fatal("limit 0 should allow (unlimited)")
	}

	// 限额 2：前两次放行，第三次拒绝
	for i := 1; i <= 2; i++ {
		ok, used, err := q.Check(ctx, "k1", 2)
		if err != nil || !ok {
			t.Fatalf("check %d: ok=%v err=%v", i, ok, err)
		}
		if used != i-1 {
			t.Fatalf("used %d before record %d", used, i)
		}
		if err := q.Record(ctx, "k1"); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	ok, used, _ := q.Check(ctx, "k1", 2)
	if ok || used != 2 {
		t.Fatalf("third create should be denied, ok=%v used=%d", ok, used)
	}
	// 其他 key 不受影响
	if ok, _, _ := q.Check(ctx, "k2", 2); !ok {
		t.Fatal("other key should be independent")
	}
}

func TestQuotaCrossDayReset(t *testing.T) {
	// 时间注入：day1 23:59:59 用满，day2 00:00:01 重置
	now := time.Date(2026, 10, 4, 23, 59, 59, 0, time.UTC)
	q := &Quota{Store: NewMemoryUsage(), Now: func() time.Time { return now }}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_ = q.Record(ctx, "k1")
	}
	if ok, used, _ := q.Check(ctx, "k1", 3); ok || used != 3 {
		t.Fatalf("day1 limit reached, ok=%v used=%d", ok, used)
	}
	// Retry-After ≈ 2 秒（跨零点）
	if ra := q.RetryAfterSeconds(); ra < 1 || ra > 3 {
		t.Fatalf("retry-after = %d, want ~2", ra)
	}
	// 跨日
	now = now.Add(2 * time.Second)
	if ok, used, _ := q.Check(ctx, "k1", 3); !ok || used != 0 {
		t.Fatalf("day2 should reset, ok=%v used=%d", ok, used)
	}
}

func TestQuotaDayFormat(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	q := &Quota{Now: func() time.Time { return now }}
	if q.Day() != "2026-10-04" {
		t.Fatalf("day = %q", q.Day())
	}
	// 非 UTC 时区输入也按 UTC 归日
	now = time.Date(2026, 10, 5, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if q.Day() != "2026-10-05" {
		t.Fatalf("day = %q (want UTC date 2026-10-05, 08:00+08 == 00:00Z)", q.Day())
	}
}

// ---- PG（连不上自动跳过） ----

func openTestPG(t *testing.T) *PGKeyStore {
	t.Helper()
	url := "postgres://tangis:tangis_dev_password@127.0.0.1:15432/tangis?sslmode=disable"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ks, err := NewPGKeyStore(ctx, url)
	if err != nil {
		t.Skipf("PostgreSQL unavailable, skipping PG tests: %v", err)
	}
	t.Cleanup(ks.Close)
	return ks
}

func TestPGKeyStoreQuotaAndMigration(t *testing.T) {
	ks := openTestPG(t)
	ctx := context.Background()

	// 兼容迁移：EnsureRawKey 幂等，老 env key 以 admin 身份入表
	if err := ks.EnsureRawKey(ctx, "legacy-env-key-xyz", "migrated", "default", RoleAdmin); err != nil {
		t.Fatalf("ensure raw key: %v", err)
	}
	if err := ks.EnsureRawKey(ctx, "legacy-env-key-xyz", "migrated", "default", RoleAdmin); err != nil {
		t.Fatalf("ensure raw key idempotent: %v", err)
	}
	key, err := ks.Verify("legacy-env-key-xyz")
	if err != nil {
		t.Fatalf("verify migrated key: %v", err)
	}
	if key.Role != RoleAdmin || key.DailyTaskLimit != 0 || key.Disabled {
		t.Fatalf("migrated key = %+v, want admin/limit0/enabled", key)
	}

	// 每日配额 PG 持久化：递增、跨 key 隔离（先清理上次运行残留，保证幂等）
	if _, err := ks.pool.Exec(ctx, `DELETE FROM api_key_usage WHERE key_id LIKE 'quota-test%'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	t.Cleanup(func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel2()
		_, _ = ks.pool.Exec(ctx2, `DELETE FROM api_key_usage WHERE key_id LIKE 'quota-test%'`)
	})
	u := ks.Usage()
	day := "2026-10-04"
	if n, _ := u.Usage(ctx, "quota-test-key", day); n != 0 {
		t.Fatalf("initial usage = %d", n)
	}
	for i := 1; i <= 3; i++ {
		n, err := u.Increment(ctx, "quota-test-key", day)
		if err != nil || n != i {
			t.Fatalf("increment %d = %d, %v", i, n, err)
		}
	}
	if n, _ := u.Usage(ctx, "quota-test-key", day); n != 3 {
		t.Fatalf("usage after increments = %d", n)
	}
	if n, _ := u.Usage(ctx, "other-key", day); n != 0 {
		t.Fatalf("other key usage = %d, want 0", n)
	}
	if n, _ := u.Usage(ctx, "quota-test-key", "2026-10-05"); n != 0 {
		t.Fatalf("other day usage = %d, want 0", n)
	}
}
