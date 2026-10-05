// Package license 实现 TanGIS Open Core 的商业授权校验骨架（PRD F-15）。
//
// # 机制
//
//   - 商业 License 为 JSON 文件，路径由环境变量 TANGIS_LICENSE_FILE 指向；
//   - 文件包含 plan / expires / seats / features[] 与 Ed25519 签名字段 sig，
//     sig 为对「不含 sig 字段的规范 JSON」的 Ed25519 签名（hex 编码）；
//   - 公钥以内置常量随程序分发（当前为开发占位公钥，见 devPublicKeyHex）。
//
// # 降级原则（fail-open 到开源版）
//
// 未设置 TANGIS_LICENSE_FILE、文件不存在、JSON 非法或验签失败时，
// 一律按「开源版」处理：全部开源特性可用，启动不报错不退出；
// 仅当调用商业特性（EnsureFeature 对商业特性名）时返回明确错误。
// License 已过期同样回退开源版语义（EnsureFeature 返回错误并说明）。
//
// # 生产发布前必改
//
// devPublicKeyHex 为开发占位公钥，生产发布前必须替换为正式生成的发布
// 公钥（私钥离线保管）。生成命令：
//
//	go run -mod=mod 以下程序片段：
//	  pub, priv, _ := ed25519.GenerateKey(rand.Reader)
//	  fmt.Println(hex.EncodeToString(pub), hex.EncodeToString(priv))
//
// 开源/商业特性分界详见 docs/LICENSE-BOUNDARY.md。
package license

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"
)

// 开源特性（Apache-2.0，永远免费）：EnsureFeature 对这些名称恒通过。
const (
	FeatureSlicing       = "slicing_single_node"    // 单机切片（kernel CLI）
	FeatureTiles3D       = "distribution_3dtiles"   // 3DTiles 服务分发
	FeatureOGCBasic      = "distribution_ogc_basic" // 基础 OGC（WMTS/TMS）
	FeatureAuthBasic     = "auth_basic"             // 最小鉴权（API Key/防盗链）
	FeatureCesium        = "preview_cesium"         // Web 端 Cesium 预览
	FeatureDeployCompose = "deploy_compose"         // Docker Compose 单机部署
)

// 商业特性（需有效 License 解锁，当前多为接口预留，见 docs/LICENSE-BOUNDARY.md）。
const (
	// FeatureDistributed 分布式切片集群 / 多 Worker 弹性（PRD F-16）。
	// 当前挂点：POST /api/v1/tasks 的 params.distributed=true。
	FeatureDistributed = "distributed_slicing"
	// FeatureCollab 协同编辑（CRDT）不限席位（PRD F-17，开源版限 3 席位）。
	FeatureCollab = "collab_unlimited_seats"
	// FeatureSSOAudit 企业 SSO 与操作审计。
	FeatureSSOAudit = "enterprise_sso_audit"
	// FeatureXinchuang 信创离线包支持服务（UOS/麒麟 + ARM64）。
	FeatureXinchuang = "xinchuang_offline_support"
	// FeatureLicenseConsole License 管控台（签发/吊销/Seat 管理）。
	FeatureLicenseConsole = "license_console"
)

// openSourceFeatures 开源特性集合（EnsureFeature 恒通过）。
var openSourceFeatures = map[string]bool{
	FeatureSlicing:       true,
	FeatureTiles3D:       true,
	FeatureOGCBasic:      true,
	FeatureAuthBasic:     true,
	FeatureCesium:        true,
	FeatureDeployCompose: true,
}

// devPublicKeyHex 开发占位公钥（Ed25519，hex 编码）。
// ⚠️ 仅供开发联调，生产发布前必须替换为正式发布公钥。
const devPublicKeyHex = "cbcf6e35fd2f09a2fb4bb9783eb6255c6089abfa6b27affa4dc0ca94bfe6f7de"

// License 商业授权文件结构。
// sig 为对 Payload（同结构去掉 Sig 字段）JSON 序列化结果的 Ed25519 签名（hex）。
type License struct {
	Plan     string   `json:"plan"`     // 套餐名，如 professional / enterprise
	Expires  string   `json:"expires"`  // RFC3339；空串表示永久
	Seats    int      `json:"seats"`    // 席位数（协同等多席位场景；当前仅随文件携带，未强制）
	Features []string `json:"features"` // 解锁的商业特性名
	Sig      string   `json:"sig"`      // Ed25519 签名（hex）
}

// payload 参与签名的规范载荷（字段顺序固定，序列化确定）。
type payload struct {
	Plan     string   `json:"plan"`
	Expires  string   `json:"expires"`
	Seats    int      `json:"seats"`
	Features []string `json:"features"`
}

// ErrFeatureNotLicensed 特性未被当前授权覆盖（含开源版回退情形）。
var ErrFeatureNotLicensed = errors.New("feature not licensed")

// Checker 授权校验器。零值/nil 均按开源版处理（方法 nil 安全）。
type Checker struct {
	lic *License // nil = 开源版
	pub ed25519.PublicKey
	now func() time.Time // 可注入时钟（测试用）
}

// OpenSource 返回开源版校验器（无商业特性）。
func OpenSource() *Checker { return &Checker{now: time.Now} }

// LoadFromEnv 按 TANGIS_LICENSE_FILE 加载授权；任何失败回退开源版并打日志。
// 永不返回 nil。
func LoadFromEnv() *Checker {
	path := os.Getenv("TANGIS_LICENSE_FILE")
	if path == "" {
		return OpenSource()
	}
	c, err := Load(path)
	if err != nil {
		fmt.Printf("license: %v, running as OPEN-SOURCE plan\n", err)
		return OpenSource()
	}
	return c
}

// Load 从文件加载并校验授权（使用内置公钥）。
// 验签失败/JSON 非法返回错误（调用方应回退开源版）。
func Load(path string) (*Checker, error) {
	pub, err := hex.DecodeString(devPublicKeyHex)
	if err != nil {
		return nil, fmt.Errorf("builtin dev public key invalid: %w", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // 路径来自运维配置的环境变量
	if err != nil {
		return nil, fmt.Errorf("read license file: %w", err)
	}
	return Parse(data, pub)
}

// Parse 解析并校验 License 文件内容（验签；过期在 EnsureFeature 时判定，
// 以便过期时刻动态回退）。pub 为公钥原始字节（Ed25519 32 字节）。
func Parse(data []byte, pub ed25519.PublicKey) (*Checker, error) {
	var lic License
	if err := json.Unmarshal(data, &lic); err != nil {
		return nil, fmt.Errorf("parse license json: %w", err)
	}
	sig, err := hex.DecodeString(lic.Sig)
	if err != nil {
		return nil, fmt.Errorf("decode license sig: %w", err)
	}
	msg, err := json.Marshal(licensePayload(&lic))
	if err != nil {
		return nil, fmt.Errorf("marshal license payload: %w", err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return nil, errors.New("license signature verification FAILED (tampered or wrong key)")
	}
	return &Checker{lic: &lic, pub: pub, now: time.Now}, nil
}

// EnsureFeature 校验特性可用性；不可用时返回包裹 ErrFeatureNotLicensed 的
// 明确错误。nil 接收者按开源版处理，恒可用于开源特性。
func (c *Checker) EnsureFeature(name string) error {
	if openSourceFeatures[name] {
		return nil
	}
	lic := c.license()
	if lic == nil {
		return fmt.Errorf("%w: %q requires a commercial license (current plan: open-source); see docs/LICENSE-BOUNDARY.md",
			ErrFeatureNotLicensed, name)
	}
	if exp, err := time.Parse(time.RFC3339, lic.Expires); err == nil && !c.now().Before(exp) {
		return fmt.Errorf("%w: %q (license plan %q expired at %s, falling back to open-source plan)",
			ErrFeatureNotLicensed, name, lic.Plan, lic.Expires)
	}
	if !slices.Contains(lic.Features, name) {
		return fmt.Errorf("%w: %q not included in license plan %q (features: %v)",
			ErrFeatureNotLicensed, name, lic.Plan, lic.Features)
	}
	return nil
}

// Plan 当前套餐名；开源版返回 "open-source"。nil 安全。
func (c *Checker) Plan() string {
	if lic := c.license(); lic != nil {
		return lic.Plan
	}
	return "open-source"
}

// Licensed 是否持有通过验签的商业授权（过期不改变此判定，仅影响 EnsureFeature）。
func (c *Checker) Licensed() bool { return c.license() != nil }

// license nil 安全取授权。
func (c *Checker) license() *License {
	if c == nil {
		return nil
	}
	return c.lic
}

// licensePayload 提取参与签名的载荷。
func licensePayload(l *License) payload {
	return payload{Plan: l.Plan, Expires: l.Expires, Seats: l.Seats, Features: l.Features}
}
