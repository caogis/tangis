package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testKey 生成测试密钥对（正常测试 fixture：License 签发方私钥仅存在于测试内）。
func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

// signLicense 用 priv 对 lic 签名并返回完整 JSON（含 hex sig 字段）。
func signLicense(t *testing.T, priv ed25519.PrivateKey, lic License) []byte {
	t.Helper()
	msg, err := json.Marshal(licensePayload(&lic))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	lic.Sig = hex.EncodeToString(ed25519.Sign(priv, msg))
	data, err := json.Marshal(lic)
	if err != nil {
		t.Fatalf("marshal license: %v", err)
	}
	return data
}

func TestEnsureFeatureOpenSource(t *testing.T) {
	var c *Checker // nil 接收者 = 开源版
	for _, f := range []string{FeatureSlicing, FeatureTiles3D, FeatureOGCBasic, FeatureAuthBasic, FeatureCesium, FeatureDeployCompose} {
		if err := c.EnsureFeature(f); err != nil {
			t.Fatalf("open-source feature %s should pass: %v", f, err)
		}
	}
	err := c.EnsureFeature(FeatureDistributed)
	if !errors.Is(err, ErrFeatureNotLicensed) {
		t.Fatalf("commercial feature on nil checker should be ErrFeatureNotLicensed, got %v", err)
	}
	if !strings.Contains(err.Error(), "open-source") {
		t.Fatalf("error should mention open-source plan: %v", err)
	}
	if p := c.Plan(); p != "open-source" {
		t.Fatalf("plan = %q, want open-source", p)
	}
}

func TestValidLicenseUnlocksCommercialFeature(t *testing.T) {
	pub, priv := testKey(t)
	future := time.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339)
	data := signLicense(t, priv, License{
		Plan: "professional", Expires: future, Seats: 10,
		Features: []string{FeatureDistributed, FeatureCollab},
	})
	c, err := Parse(data, pub)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.Licensed() || c.Plan() != "professional" {
		t.Fatalf("plan = %q licensed = %v", c.Plan(), c.Licensed())
	}
	if err := c.EnsureFeature(FeatureDistributed); err != nil {
		t.Fatalf("licensed feature should pass: %v", err)
	}
	if err := c.EnsureFeature(FeatureSlicing); err != nil {
		t.Fatalf("open-source feature should still pass: %v", err)
	}
	// 授权未包含的商业特性仍拒绝
	err = c.EnsureFeature(FeatureSSOAudit)
	if !errors.Is(err, ErrFeatureNotLicensed) {
		t.Fatalf("feature not in license should fail, got %v", err)
	}
}

func TestTamperedLicenseRejected(t *testing.T) {
	pub, priv := testKey(t)
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	data := signLicense(t, priv, License{Plan: "enterprise", Expires: future, Features: []string{FeatureDistributed}})
	// 篡改 features 后验签必须失败
	var lic License
	if err := json.Unmarshal(data, &lic); err != nil {
		t.Fatal(err)
	}
	lic.Features = append(lic.Features, FeatureSSOAudit)
	tampered, _ := json.Marshal(lic)
	if _, err := Parse(tampered, pub); err == nil {
		t.Fatal("tampered license must fail verification")
	}
	// 错误公钥同样失败
	otherPub, _ := testKey(t)
	if _, err := Parse(data, otherPub); err == nil {
		t.Fatal("wrong public key must fail verification")
	}
}

func TestExpiredLicenseFallsBack(t *testing.T) {
	pub, priv := testKey(t)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	data := signLicense(t, priv, License{Plan: "professional", Expires: past, Features: []string{FeatureDistributed}})
	c, err := Parse(data, pub)
	if err != nil {
		t.Fatalf("Parse (expired sig is still valid): %v", err)
	}
	err = c.EnsureFeature(FeatureDistributed)
	if !errors.Is(err, ErrFeatureNotLicensed) {
		t.Fatalf("expired license should reject commercial feature, got %v", err)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error should mention expiry: %v", err)
	}
	// 过期不影响开源特性
	if err := c.EnsureFeature(FeatureSlicing); err != nil {
		t.Fatalf("open-source feature after expiry: %v", err)
	}
}

func TestLoadMissingFileOpenSource(t *testing.T) {
	t.Setenv("TANGIS_LICENSE_FILE", filepath.Join(t.TempDir(), "nope.json"))
	c := LoadFromEnv()
	if c.Licensed() || c.Plan() != "open-source" {
		t.Fatalf("missing file should fall back to open-source, got plan %q", c.Plan())
	}
	if err := c.EnsureFeature(FeatureSlicing); err != nil {
		t.Fatalf("open-source features must remain available: %v", err)
	}
	if err := c.EnsureFeature(FeatureDistributed); !errors.Is(err, ErrFeatureNotLicensed) {
		t.Fatalf("commercial feature must be rejected: %v", err)
	}
}

func TestLoadInvalidJSONFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid json must fail Load (caller falls back to open-source)")
	}
	// LoadFromEnv 对坏文件同样回退开源版
	t.Setenv("TANGIS_LICENSE_FILE", path)
	if c := LoadFromEnv(); c.Plan() != "open-source" {
		t.Fatalf("bad file should fall back to open-source, got %q", c.Plan())
	}
}

func TestLoadFromEnvEndToEnd(t *testing.T) {
	// 端到端：生成 dev 密钥对替换内置公钥不可行（常量），
	// 这里验证 Parse 全链路签名/验签一致（等价于 Load 的文件内读取分支）。
	pub, priv := testKey(t)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	data := signLicense(t, priv, License{Plan: "enterprise", Expires: future, Seats: 50, Features: []string{FeatureLicenseConsole}})
	c, err := Parse(data, pub)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := c.EnsureFeature(FeatureLicenseConsole); err != nil {
		t.Fatalf("expected FeatureLicenseConsole unlocked: %v", err)
	}
	// 无 Expires（永久授权）
	data = signLicense(t, priv, License{Plan: "perpetual", Features: []string{FeatureXinchuang}})
	c, err = Parse(data, pub)
	if err != nil {
		t.Fatalf("Parse perpetual: %v", err)
	}
	if err := c.EnsureFeature(FeatureXinchuang); err != nil {
		t.Fatalf("perpetual license should pass: %v", err)
	}
}
