// Package vector 提供 PRD F-10 的矢量发布能力（M2-F10a，PostGIS → MVT）：
//
//   - 矢量数据源注册（PostGIS 连接，连通性测试 + PostGIS 版本探测）；
//   - 图层注册（schema/table/geom 列/srid/属性字段，自动探测
//     information_schema + geometry_columns）；
//   - MVT 分发（PostGIS ST_AsMVT + ST_AsMVTGeom，WebMercator 3857 动态投影，
//     srid != 3857 时经 ST_Transform）；
//   - 图层元数据（ST_EstimatedExtent bbox、字段、要素数估算、zoom 建议）。
//
// 安全边界：所有进入 SQL 的标识符（schema/table/列名）必须通过
// ValidIdentifier 白名单校验（^[a-zA-Z_][a-zA-Z0-9_]*$）并加双引号引用；
// 数值参数（bbox、limit、srid）走占位符或 int 字面量，杜绝注入。
//
// 存储分两层抽象：
//   - MetaStore：数据源/图层注册元数据（存 apiserver 自己的库，pg 或内存）；
//   - Gateway：对已注册数据源的数据面操作（ping/探测/MVT/extent），
//     内部按 DSN 维护 pgxpool 连接池注册表。
package vector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrNotFound 元数据不存在（图层名/数据源 ID 无匹配）。
var ErrNotFound = errors.New("vector: not found")

// ErrConflict 元数据冲突（数据源仍被图层引用、名称重复等）。
var ErrConflict = errors.New("vector: conflict")

// ErrInvalid 请求参数非法（标识符不过白名单、表不存在等）。
var ErrInvalid = errors.New("vector: invalid")

// ErrTooManyFeatures 单 tile 要素数超出上限（F-10 限制策略：直接报错，
// 提示客户端提高 zoom 级别，绝不静默截断）。
var ErrTooManyFeatures = errors.New("vector: too many features in tile")

// identifierRe 标识符白名单：字母或下划线开头，仅含字母/数字/下划线。
// 杜绝引号逃逸、注释、分号等注入向量。
var identifierRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidIdentifier 校验标识符是否通过白名单。
func ValidIdentifier(s string) bool { return identifierRe.MatchString(s) }

// QuoteIdent 白名单校验通过后加双引号引用，用于安全拼接 SQL 标识符。
// 未通过校验直接 panic——仅限内部已校验路径使用。
func QuoteIdent(s string) string {
	if !ValidIdentifier(s) {
		panic(fmt.Sprintf("vector: invalid identifier %q", s))
	}
	return `"` + s + `"`
}

// MaxZoomCap zoom 上限（Web Mercator 常规到 22 级）。
const MaxZoomCap = 22

// conflictf 包装 ErrConflict 的格式化错误。
func conflictf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrConflict}, args...)...)
}

// Source 已注册的矢量数据源（一个 PostGIS 连接）。
type Source struct {
	// ID 数据源唯一标识（注册时自动生成短 hex）。
	ID string `json:"id"`
	// Name 展示名，全局唯一。
	Name string `json:"name"`
	// DSN PostgreSQL 连接串（postgres://user:pass@host:port/db?sslmode=...）。
	// 按项目现状（任务 params 同风格）明文入库；生产建议密码入 env/密钥库，
	// 见 docs/VECTOR-API.md 安全说明。
	DSN string `json:"-"` // 不回显密码
	// PostGISVersion 注册时探测到的 PostGIS 版本串。
	PostGISVersion string `json:"postgis_version"`
	// CreatedAt 注册时间（UTC）。
	CreatedAt time.Time `json:"created_at"`
}

// MaskedDSN 返回打码后的 DSN（密码段以 **** 代替），供 API 回显。
func (s *Source) MaskedDSN() string {
	// 粗粒度打码：userinfo 密码段替换
	i := strings.Index(s.DSN, "://")
	if i < 0 {
		return s.DSN
	}
	rest := s.DSN[i+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return s.DSN
	}
	cred := rest[:at]
	colon := strings.Index(cred, ":")
	if colon < 0 {
		return s.DSN
	}
	return s.DSN[:i+3] + cred[:colon] + ":****" + rest[at:]
}

// Layer 已注册的矢量图层（一个 PostGIS 空间表 → 一个 MVT source-layer）。
type Layer struct {
	// Name 图层名，全局唯一，同时是 MVT source-layer 名与 URL 标识。
	Name string `json:"name"`
	// SourceID 所属数据源 ID。
	SourceID string `json:"source_id"`
	// Schema / Table 目标表限定名（均过白名单）。
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// GeometryColumn 几何列名。
	GeometryColumn string `json:"geometry_column"`
	// GeometryType 几何类型（geometry_columns 探测，如 POLYGON/POINT）。
	GeometryType string `json:"geometry_type"`
	// SRID 几何列空间参考（ != 3857 时查询内 ST_Transform）。
	SRID int `json:"srid"`
	// IDColumn 要素 ID 列（默认主键），MVT feature id 来源。
	IDColumn string `json:"id_column"`
	// Fields 进 MVT 属性的普通列（白名单校验后存储，几何/ID 列除外）。
	Fields []string `json:"fields"`
	// CreatedAt 注册时间（UTC）。
	CreatedAt time.Time `json:"created_at"`
}

// qualified 拼接安全的限定表名（schema.table，均已白名单+引用）。
func (l *Layer) qualified() string {
	return QuoteIdent(l.Schema) + "." + QuoteIdent(l.Table)
}

// geomExpr 返回图层原生 SRID 下的几何表达式。
func (l *Layer) geomExpr() string { return QuoteIdent(l.GeometryColumn) }

// NewID 生成 16 字符随机 hex ID。
func NewID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("vector: gen id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// MetaStore 数据源/图层注册元数据存储抽象。
// 实现见 pg.go（PostgreSQL，apiserver 自库）与 memory.go（测试/无 PG 兜底）。
type MetaStore interface {
	CreateSource(ctx context.Context, s *Source) error
	ListSources(ctx context.Context) ([]*Source, error)
	GetSource(ctx context.Context, id string) (*Source, error)
	DeleteSource(ctx context.Context, id string) error
	// CountLayersBySource 统计引用某数据源的图层数（删除前置检查）。
	CountLayersBySource(ctx context.Context, sourceID string) (int, error)

	CreateLayer(ctx context.Context, l *Layer) error
	GetLayer(ctx context.Context, name string) (*Layer, error)
	ListLayers(ctx context.Context) ([]*Layer, error)
	DeleteLayer(ctx context.Context, name string) error
}

// TileBBox 计算 Web Mercator（EPSG:3857）下瓦片 (z/x/y) 的包围盒。
// 返回 (minx, miny, maxx, maxy)，单位米。
func TileBBox(z, x, y int) (minx, miny, maxx, maxy float64) {
	const worldSize = 40075016.6855785 // 赤道周长（Web Mercator 全幅）
	n := worldSize / float64(uint64(1)<<uint(z))
	minx = -worldSize/2 + float64(x)*n
	maxx = minx + n
	maxy = worldSize/2 - float64(y)*n
	miny = maxy - n
	return
}

// ValidTile 校验 z/x/y 合法（0 ≤ z ≤ 22，0 ≤ x,y < 2^z）。
func ValidTile(z, x, y int) bool {
	if z < 0 || z > MaxZoomCap {
		return false
	}
	max := 1 << uint(z)
	return x >= 0 && x < max && y >= 0 && y < max
}
