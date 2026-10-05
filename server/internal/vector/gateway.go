// Gateway：矢量数据源的数据面操作抽象 + PostGIS 实现。
// pgGateway 内部按 DSN 维护 pgxpool 连接池注册表（懒建、复用），
// 连接池带生命周期/空闲超时与建连超时（connect_timeout）。
package vector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Gateway 数据面操作（对接已注册数据源的 PostGIS）。
// 抽象成接口便于单测 mock（不依赖真实 PG）。
type Gateway interface {
	// Ping 连通性测试：建连 + ping + 返回 PostGIS 版本串。
	Ping(ctx context.Context, dsn string) (postgisVersion string, err error)
	// DetectGeometry 从 geometry_columns 探测几何列的 srid 与类型。
	// geomCol 为空串时自动探测该表唯一的几何列。
	DetectGeometry(ctx context.Context, dsn, schema, table, geomCol string) (geomColFound string, srid int, geomType string, err error)
	// DetectFields 从 information_schema 探测普通属性列（排除排除项）。
	DetectFields(ctx context.Context, dsn, schema, table string, exclude ...string) ([]string, error)
	// DetectPrimaryKey 探测表主键列；无主键返回空串。
	DetectPrimaryKey(ctx context.Context, dsn, schema, table string) (string, error)
	// TileMVT 查询一瓦片 MVT。dsn 为图层所属数据源连接串；layer 提供全部
	// 标识符（已白名单校验），bbox 为 Web Mercator 3857 包围盒。
	// 返回 (mvt 字节, 要素数)。
	TileMVT(ctx context.Context, dsn string, l *Layer, minx, miny, maxx, maxy float64, maxFeatures int) (mvt []byte, count int, err error)
	// EstimatedExtent ST_EstimatedExtent 探测图层 bbox（图层原生 SRID）。
	// 统计信息缺失（返回 NULL）时 ok=false。
	EstimatedExtent(ctx context.Context, dsn string, l *Layer) (minx, miny, maxx, maxy float64, ok bool, err error)
	// EstimatedRows 估算要素数（pg_class.reltuples）。
	EstimatedRows(ctx context.Context, dsn string, l *Layer) (int64, error)
	// FeaturesGeoJSON WFS GetFeature 数据面（M2-F14）：bbox 内要素输出
	// GeoJSON（ST_AsGeoJSON）。hasBBox=false 输出全图层；bbox 以 WGS84
	// CRS84（经度、纬度序）给出，内部经 ST_Transform 到图层 SRID 做 &&
	// 索引过滤；limit 硬截断。返回 (features JSON 数组字节, 要素数)。
	FeaturesGeoJSON(ctx context.Context, dsn string, l *Layer, hasBBox bool, minx, miny, maxx, maxy float64, limit int) (features []byte, count int, err error)
}

// dialTimeout 建连与 Ping 的超时。
const dialTimeout = 5 * time.Second

// pgGateway Gateway 的 PostGIS 实现。
type pgGateway struct {
	mu    sync.Mutex
	pools map[string]*pgxpool.Pool
}

// NewPGGateway 创建 PostGIS 网关。
func NewPGGateway() Gateway {
	return &pgGateway{pools: map[string]*pgxpool.Pool{}}
}

// pool 按 DSN 取（或懒建）连接池。池参数：最大 4 连接、连接寿命 30 分钟、
// 空闲 5 分钟回收；建连超时经 connect_timeout=5 注入 DSN。
func (g *pgGateway) pool(dsn string) (*pgxpool.Pool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.pools[dsn]; ok {
		return p, nil
	}
	cfg, err := pgxpool.ParseConfig(withConnectTimeout(dsn))
	if err != nil {
		return nil, fmt.Errorf("vector: parse dsn: %w", err)
	}
	cfg.MaxConns = 4
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("vector: pg pool: %w", err)
	}
	g.pools[dsn] = p
	return p, nil
}

// withConnectTimeout DSN 无 connect_timeout 参数时注入 connect_timeout=5。
func withConnectTimeout(dsn string) string {
	if len(dsn) > 0 && dsn[0] != 'p' { // 非 URL 形式（keyword=value），保守不动
		return dsn
	}
	sep := "?"
	if containsQuery(dsn) {
		sep = "&"
	}
	return dsn + sep + "connect_timeout=5"
}

func containsQuery(dsn string) bool { return indexByte(dsn, '?') >= 0 }

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Close 释放全部连接池。
func (g *pgGateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.pools {
		p.Close()
	}
	g.pools = map[string]*pgxpool.Pool{}
}

// Ping 实现 Gateway。
func (g *pgGateway) Ping(ctx context.Context, dsn string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	p, err := g.pool(dsn)
	if err != nil {
		return "", err
	}
	if err := p.Ping(ctx); err != nil {
		return "", fmt.Errorf("vector: ping: %w", err)
	}
	var ver string
	if err := p.QueryRow(ctx, `SELECT PostGIS_Full_Version()`).Scan(&ver); err != nil {
		return "", fmt.Errorf("vector: postgis version (extension installed?): %w", err)
	}
	return ver, nil
}

// DetectGeometry 实现 Gateway：geometry_columns 探测。
func (g *pgGateway) DetectGeometry(ctx context.Context, dsn, schema, table, geomCol string) (string, int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	p, err := g.pool(dsn)
	if err != nil {
		return "", 0, "", err
	}
	if geomCol == "" {
		// 自动探测：该表全部几何列；要求恰好一个，避免歧义
		var found string
		err := p.QueryRow(ctx, `
SELECT f_geometry_column FROM geometry_columns
WHERE f_table_schema=$1 AND f_table_name=$2
ORDER BY f_geometry_column LIMIT 2`, schema, table).Scan(&found)
		switch {
		case err != nil:
			return "", 0, "", fmt.Errorf("vector: detect geometry column %s.%s: %w", schema, table, prettifyNoTable(err))
		default:
			// LIMIT 2 只取到一个：ok
		}
		// 复查是否唯一
		var cnt int
		if err := p.QueryRow(ctx, `SELECT count(*) FROM geometry_columns WHERE f_table_schema=$1 AND f_table_name=$2`, schema, table).Scan(&cnt); err != nil {
			return "", 0, "", fmt.Errorf("vector: count geometry columns: %w", err)
		}
		if cnt != 1 {
			return "", 0, "", fmt.Errorf("%w: table %s.%s has %d geometry columns, specify one explicitly", ErrInvalid, schema, table, cnt)
		}
		geomCol = found
	}
	var srid int
	var gtype string
	err = p.QueryRow(ctx, `
SELECT srid, type FROM geometry_columns
WHERE f_table_schema=$1 AND f_table_name=$2 AND f_geometry_column=$3`,
		schema, table, geomCol).Scan(&srid, &gtype)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, "", fmt.Errorf("%w: geometry column %q not found in %s.%s (geometry_columns)", ErrInvalid, geomCol, schema, table)
		}
		return "", 0, "", fmt.Errorf("vector: query geometry_columns: %w", err)
	}
	return geomCol, srid, gtype, nil
}

// DetectFields 实现 Gateway：information_schema 探测普通列（白名单内）。
func (g *pgGateway) DetectFields(ctx context.Context, dsn, schema, table string, exclude ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	p, err := g.pool(dsn)
	if err != nil {
		return nil, err
	}
	rows, err := p.Query(ctx, `
SELECT column_name FROM information_schema.columns
WHERE table_schema=$1 AND table_name=$2
ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("vector: query columns %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	skip := map[string]bool{"geom": true}
	for _, e := range exclude {
		skip[e] = true
	}
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("vector: scan column name: %w", err)
		}
		if !ValidIdentifier(name) || skip[name] {
			continue // 白名单外的列直接不暴露
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vector: list columns: %w", err)
	}
	return out, nil
}

// DetectPrimaryKey 实现 Gateway：information_schema 主键列。
func (g *pgGateway) DetectPrimaryKey(ctx context.Context, dsn, schema, table string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	p, err := g.pool(dsn)
	if err != nil {
		return "", err
	}
	var pk string
	err = p.QueryRow(ctx, `
SELECT ku.column_name
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage ku
  ON tc.constraint_name = ku.constraint_name AND tc.table_schema = ku.table_schema
WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema=$1 AND tc.table_name=$2
ORDER BY ku.ordinal_position LIMIT 1`, schema, table).Scan(&pk)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("vector: detect primary key %s.%s: %w", schema, table, err)
	}
	return pk, nil
}

// TileMVT 实现 Gateway：ST_AsMVT 一瓦片查询。
//
// SQL 骨架（标识符全部 QuoteIdent，数值参数化）：
//
//	WITH src AS (
//	  SELECT "id", "f1", ...,
//	         ST_AsMVTGeom(<geom>, ST_Transform(ST_MakeEnvelope($1,$2,$3,$4,3857), <srid>),
//	                      4096, 64, true) AS geom
//	  FROM "schema"."table"
//	  WHERE <geom> && ST_Transform(ST_MakeEnvelope($1,$2,$3,$4,3857), <srid>)
//	  LIMIT <maxFeatures+1>
//	)
//	SELECT count(*), ST_AsMVT(src, $5, 4096, 'geom') FROM src
//
// 说明：
//   - bbox 恒以 3857 计算，经 ST_Transform 到图层 SRID 与几何对齐；
//     ST_AsMVTGeom 按传入 bbox 归一化，任意 SRID 输出的 MVT 均为瓦片坐标；
//   - && 走 GiST 索引；LIMIT maxFeatures+1 用于探测超限；
//   - count 与 ST_AsMVT 同 SELECT 聚合，零行时 mvt 为 NULL → 空 tile。
func (g *pgGateway) TileMVT(ctx context.Context, dsn string, l *Layer, minx, miny, maxx, maxy float64, maxFeatures int) ([]byte, int, error) {
	p, err := g.pool(dsn)
	if err != nil {
		return nil, 0, err
	}

	selects := make([]string, 0, len(l.Fields)+2)
	selects = append(selects, QuoteIdent(l.IDColumn))
	for _, f := range l.Fields {
		selects = append(selects, QuoteIdent(f))
	}
	bboxExpr := fmt.Sprintf(`ST_Transform(ST_MakeEnvelope($1,$2,$3,$4,3857), %d)`, l.SRID)
	geom := l.geomExpr()

	sql := `WITH src AS (
SELECT ` + strings.Join(selects, ", ") + `,
ST_AsMVTGeom(` + geom + `, ` + bboxExpr + `, 4096, 64, true) AS geom
FROM ` + l.qualified() + `
WHERE ` + geom + ` && ` + bboxExpr + `
LIMIT ` + fmt.Sprint(maxFeatures+1) + `
)
SELECT count(*)::bigint, ST_AsMVT(src, $5, 4096, 'geom') FROM src`

	var count int64
	var mvt []byte
	if err := p.QueryRow(ctx, sql, minx, miny, maxx, maxy, l.Name).Scan(&count, &mvt); err != nil {
		return nil, 0, fmt.Errorf("vector: tile query %s: %w", l.Name, err)
	}
	if count > int64(maxFeatures) {
		return nil, int(count), ErrTooManyFeatures
	}
	return mvt, int(count), nil
}

// EstimatedExtent 实现 Gateway：ST_EstimatedExtent（图层原生 SRID）。
func (g *pgGateway) EstimatedExtent(ctx context.Context, dsn string, l *Layer) (float64, float64, float64, float64, bool, error) {
	p, err := g.pool(dsn)
	if err != nil {
		return 0, 0, 0, 0, false, err
	}
	var minx, miny, maxx, maxy float64
	err = p.QueryRow(ctx, `
SELECT ST_XMin(e), ST_YMin(e), ST_XMax(e), ST_YMax(e) FROM (
  SELECT ST_EstimatedExtent($1,$2,$3) AS e
) s`, l.Schema, l.Table, l.GeometryColumn).Scan(&minx, &miny, &maxx, &maxy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, 0, 0, false, nil
		}
		return 0, 0, 0, 0, false, fmt.Errorf("vector: estimated extent %s: %w", l.Name, err)
	}
	return minx, miny, maxx, maxy, true, nil
}

// EstimatedRows 实现 Gateway：pg_class.reltuples（无 analyze 时为 -1 → 0）。
func (g *pgGateway) EstimatedRows(ctx context.Context, dsn string, l *Layer) (int64, error) {
	p, err := g.pool(dsn)
	if err != nil {
		return 0, err
	}
	var n int64
	err = p.QueryRow(ctx, `
SELECT GREATEST(reltuples::bigint, 0) FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname=$1 AND c.relname=$2`, l.Schema, l.Table).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("vector: estimated rows %s: %w", l.Name, err)
	}
	return n, nil
}

// FeaturesGeoJSON 实现 Gateway：WFS GetFeature 查询（ST_AsGeoJSON）。
//
// SQL 骨架（标识符全部 QuoteIdent，数值参数化）：
//
//	WITH src AS (
//	  SELECT ST_AsGeoJSON(ST_Transform(<geom>, 4326))::jsonb AS geom,
//	         <id>::text AS fid, jsonb_build_object('f1',"f1",...) AS props
//	  FROM "schema"."table"
//	  [WHERE <geom> && ST_Transform(ST_MakeEnvelope($1,$2,$3,$4,4326), <srid>)]
//	  LIMIT <limit>
//	)
//	SELECT COALESCE(json_agg(jsonb_build_object('type','Feature','id',fid,
//	          'geometry',geom,'properties',props)), '[]'::json),
//	       count(*)::bigint
//	FROM src
//
// bbox（hasBBox=true）恒按 WGS84 CRS84（经纬度）解释，经 ST_Transform 到
// 图层 SRID 与几何对齐（&& 走 GiST 索引）；输出统一为 EPSG:4326 GeoJSON 几何。
func (g *pgGateway) FeaturesGeoJSON(ctx context.Context, dsn string, l *Layer, hasBBox bool, minx, miny, maxx, maxy float64, limit int) ([]byte, int, error) {
	p, err := g.pool(dsn)
	if err != nil {
		return nil, 0, err
	}
	geom := l.geomExpr()
	geomOut := fmt.Sprintf(`ST_AsGeoJSON(ST_Transform(%s, 4326))::jsonb`, geom)
	if l.SRID == 4326 {
		geomOut = fmt.Sprintf(`ST_AsGeoJSON(%s)::jsonb`, geom)
	}
	props := "'{}'::jsonb"
	if len(l.Fields) > 0 {
		pairs := make([]string, 0, len(l.Fields)*2)
		for _, f := range l.Fields {
			pairs = append(pairs, `'`+f+`'`, QuoteIdent(f))
		}
		props = "jsonb_build_object(" + strings.Join(pairs, ", ") + ")"
	}

	where, args := "", []any{}
	if hasBBox {
		bboxExpr := fmt.Sprintf(`ST_Transform(ST_MakeEnvelope($1,$2,$3,$4,4326), %d)`, l.SRID)
		where = `WHERE ` + geom + ` && ` + bboxExpr
		args = append(args, minx, miny, maxx, maxy)
	}

	sql := `WITH src AS (
SELECT ` + geomOut + ` AS geom, ` + QuoteIdent(l.IDColumn) + `::text AS fid,
       ` + props + ` AS props
FROM ` + l.qualified() + `
` + where + `
LIMIT ` + fmt.Sprint(limit) + `
)
SELECT COALESCE(json_agg(jsonb_build_object('type','Feature','id',fid,
         'geometry',geom,'properties',props)), '[]'::json)::text,
       count(*)::bigint
FROM src`

	var features string
	var count int64
	if err := p.QueryRow(ctx, sql, args...).Scan(&features, &count); err != nil {
		return nil, 0, fmt.Errorf("vector: wfs features %s: %w", l.Name, err)
	}
	return []byte(features), int(count), nil
}

// prettifyNoTable 把「relation 不存在」类错误转为 ErrInvalid（表不存在）。
func prettifyNoTable(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: table not found or has no geometry column", ErrInvalid)
	}
	return err
}
