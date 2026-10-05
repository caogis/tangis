package auth

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMemoryKeyStoreVerify(t *testing.T) {
	ks := NewMemoryKeyStore("dev-key")
	ks.Add("acme-key", &APIKey{ID: "k1", Name: "acme", TenantID: "acme", Role: RoleTenant})

	// 默认 admin key
	key, err := ks.Verify("dev-key")
	if err != nil {
		t.Fatalf("Verify default: %v", err)
	}
	if key.Role != RoleAdmin || key.TenantID != "default" {
		t.Fatalf("default key = %+v, want admin@default", key)
	}

	// 租户 key
	key, err = ks.Verify("acme-key")
	if err != nil {
		t.Fatalf("Verify tenant: %v", err)
	}
	if key.TenantID != "acme" || key.Role != RoleTenant {
		t.Fatalf("tenant key = %+v", key)
	}

	// 错误 key / 空 key
	if _, err := ks.Verify("wrong"); err != ErrInvalidKey {
		t.Fatalf("Verify wrong = %v, want ErrInvalidKey", err)
	}
	if _, err := ks.Verify(""); err != ErrInvalidKey {
		t.Fatalf("Verify empty = %v, want ErrInvalidKey", err)
	}
}

func TestHashKeyDeterministic(t *testing.T) {
	if HashKey("abc") != HashKey("abc") {
		t.Fatal("HashKey should be deterministic")
	}
	if HashKey("abc") == HashKey("abd") {
		t.Fatal("different keys should hash differently")
	}
	if len(HashKey("abc")) != 64 {
		t.Fatalf("sha256 hex length = %d, want 64", len(HashKey("abc")))
	}
}

func TestSignURLRoundtrip(t *testing.T) {
	secret := "test-secret"
	path := "/services/abc123/tileset.json"
	expires := time.Now().Add(time.Hour)

	q := SignURL(path, expires, secret)
	if !strings.HasPrefix(q, "expires=") || !strings.Contains(q, "&sig=") {
		t.Fatalf("signed query = %q", q)
	}

	// 解析回 expires/sig 再校验
	var expiresStr, sig string
	for _, kv := range strings.Split(q, "&") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("bad query pair %q", kv)
		}
		if parts[0] == "expires" {
			expiresStr = parts[1]
		} else {
			sig = parts[1]
		}
	}
	exp, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil {
		t.Fatalf("parse expires: %v", err)
	}
	if !VerifySignature(path, exp, sig, secret) {
		t.Fatal("valid signature should verify")
	}
}

func TestVerifySignatureRejects(t *testing.T) {
	secret := "test-secret"
	path := "/services/abc123/tileset.json"
	sig := SignURL(path, time.Now().Add(time.Hour), secret)
	sigHex := sig[strings.Index(sig, "&sig=")+len("&sig="):]

	// 过期
	if VerifySignature(path, time.Now().Add(-time.Second).Unix(), sigHex, secret) {
		t.Fatal("expired signature should be rejected")
	}
	// 路径被篡改
	if VerifySignature("/services/other/tileset.json", time.Now().Add(time.Hour).Unix(), sigHex, secret) {
		t.Fatal("signature must be bound to path")
	}
	// 密钥不匹配
	if VerifySignature(path, time.Now().Add(time.Hour).Unix(), sigHex, "wrong-secret") {
		t.Fatal("signature must be bound to secret")
	}
	// 签名被篡改
	if VerifySignature(path, time.Now().Add(time.Hour).Unix(), strings.Repeat("0", len(sigHex)), secret) {
		t.Fatal("tampered signature should be rejected")
	}
	// 空 sig
	if VerifySignature(path, time.Now().Add(time.Hour).Unix(), "", secret) {
		t.Fatal("empty signature should be rejected")
	}
}

// openTestPGKeyStore 连接本机测试库的 PG KeyStore；连不上则跳过。
func openTestPGKeyStore(t *testing.T) *PGKeyStore {
	t.Helper()
	url := "postgres://tangis:tangis_dev_password@127.0.0.1:15432/tangis?sslmode=disable"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ks, err := NewPGKeyStore(ctx, url)
	if err != nil {
		t.Skipf("PostgreSQL unavailable, skipping PG key store tests: %v", err)
	}
	t.Cleanup(ks.Close)
	return ks
}

func TestPGKeyStoreVerifyAndSeed(t *testing.T) {
	ks := openTestPGKeyStore(t)

	// 默认播种的 admin key 应可校验（hash of tangis-dev-key）
	key, err := ks.Verify("tangis-dev-key")
	if err != nil {
		t.Fatalf("seeded dev key verify: %v", err)
	}
	if key.Role != RoleAdmin || key.TenantID != "default" {
		t.Fatalf("seeded key = %+v, want admin@default", key)
	}

	// 无效 key
	if _, err := ks.Verify("nope"); err != ErrInvalidKey {
		t.Fatalf("invalid key = %v, want ErrInvalidKey", err)
	}
}
