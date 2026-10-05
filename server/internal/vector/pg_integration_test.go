// 真实 PostGIS 集成测试（M2-F10a）：默认跳过，设置
// TANGIS_VECTOR_TEST_DSN（如 postgres://tangis:tangis_dev_password@127.0.0.1:15432/tangis?sslmode=disable）
// 后启用，连 deploy compose 的 PostGIS。会创建/清理 demo_integration schema。
package vector

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const integrationSchema = "demo_integration"

func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TANGIS_VECTOR_TEST_DSN")
	if dsn == "" {
		t.Skip("TANGIS_VECTOR_TEST_DSN not set, skip real PostGIS integration test")
	}
	return dsn
}

func TestIntegrationTileMVT(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	gw := NewPGGateway()
	defer gw.(interface{ Close() }).Close()

	ver, err := gw.Ping(ctx, dsn)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Logf("PostGIS: %s", ver)

	// 建测试表（2 个多边形 + 属性），srid 4326
	setup := []string{
		`DROP SCHEMA IF EXISTS ` + integrationSchema + ` CASCADE`,
		`CREATE SCHEMA ` + integrationSchema,
		`CREATE TABLE ` + integrationSchema + `.parcels (
			gid serial PRIMARY KEY,
			name text NOT NULL,
			kind text NOT NULL,
			area_ha double precision,
			geom geometry(Polygon, 4326) NOT NULL
		)`,
		`INSERT INTO ` + integrationSchema + `.parcels (name, kind, area_ha, geom) VALUES
		 ('P1','residential', 1.5, ST_GeomFromText('POLYGON((116.30 39.90,116.31 39.90,116.31 39.91,116.30 39.91,116.30 39.90))',4326)),
		 ('P2','industrial',  2.5, ST_GeomFromText('POLYGON((116.32 39.92,116.33 39.92,116.33 39.93,116.32 39.93,116.32 39.92))',4326))`,
		`ANALYZE ` + integrationSchema + `.parcels`,
		`CREATE INDEX ON ` + integrationSchema + `.parcels USING gist (geom)`,
	}
	// 复用 gateway 的连接池执行 setup：借 TileMVT 不合适，直接开临时连接
	execSetup(t, dsn, setup)

	// 自动探测
	geomCol, srid, gtype, err := gw.DetectGeometry(ctx, dsn, integrationSchema, "parcels", "")
	if err != nil || geomCol != "geom" || srid != 4326 || gtype != "POLYGON" {
		t.Fatalf("DetectGeometry = %q %d %q, %v", geomCol, srid, gtype, err)
	}
	pk, err := gw.DetectPrimaryKey(ctx, dsn, integrationSchema, "parcels")
	if err != nil || pk != "gid" {
		t.Fatalf("DetectPrimaryKey = %q, %v", pk, err)
	}
	fields, err := gw.DetectFields(ctx, dsn, integrationSchema, "parcels", "geom", "gid")
	if err != nil || len(fields) != 3 {
		t.Fatalf("DetectFields = %v, %v", fields, err)
	}

	l := &Layer{
		Name: "parcels_it", SourceID: "it", Schema: integrationSchema, Table: "parcels",
		GeometryColumn: geomCol, GeometryType: gtype, SRID: srid, IDColumn: pk, Fields: fields,
	}

	// 北京 116.3 附近 z11：P1 在瓦片内（P2 恰在上一行），应命中 1 要素
	x, y, z := WGS84ToTile(116.305, 39.905, 11)
	bminx, bminy, bmaxx, bmaxy := TileBBox(z, x, y)
	mvt, count, err := gw.TileMVT(ctx, dsn, l, bminx, bminy, bmaxx, bmaxy, 100000)
	if err != nil {
		t.Fatalf("TileMVT: %v", err)
	}
	if count != 1 || len(mvt) == 0 {
		t.Fatalf("z11 count = %d, mvt bytes = %d, want 1 feature", count, len(mvt))
	}
	if !isMVT(mvt) {
		t.Fatal("payload does not start with MVT layer field tag (proto)")
	}
	t.Logf("tile %d/%d/%d: %d features, %d bytes", z, x, y, count, len(mvt))

	// z3 粗瓦片（全球尺度）：两个要素都应命中
	cminx, cminy, cmaxx, cmaxy := TileBBox(3, 6, 3)
	mvt3, count3, err := gw.TileMVT(ctx, dsn, l, cminx, cminy, cmaxx, cmaxy, 100000)
	if err != nil || count3 != 2 || len(mvt3) == 0 {
		t.Fatalf("z3 count = %d, mvt bytes = %d, %v, want 2 features", count3, len(mvt3), err)
	}

	// 远离数据的瓦片 → 空
	eminx, eminy, emaxx, emaxy := TileBBox(5, 0, 0)
	mvt2, count2, err := gw.TileMVT(ctx, dsn, l, eminx, eminy, emaxx, emaxy, 100000)
	if err != nil || count2 != 0 || len(mvt2) != 0 {
		t.Fatalf("empty tile = %d bytes, %d features, %v", len(mvt2), count2, err)
	}

	// 元数据
	minx, miny, maxx, maxy, ok, err := gw.EstimatedExtent(ctx, dsn, l)
	if err != nil || !ok {
		t.Fatalf("EstimatedExtent = %v, ok=%v", err, ok)
	}
	bbox := TransformBBoxToWGS84(minx, miny, maxx, maxy, l.SRID)
	if bbox[0] < 115 || bbox[0] > 118 {
		t.Fatalf("bbox = %v", bbox)
	}
	rows, err := gw.EstimatedRows(ctx, dsn, l)
	if err != nil || rows < 2 {
		t.Fatalf("EstimatedRows = %d, %v", rows, err)
	}
}

// WGS84ToTile 经纬度 → 瓦片坐标（测试辅助）。
func WGS84ToTile(lon, lat float64, z int) (x, y, zOut int) {
	clamped := lon
	if clamped > 179.999 {
		clamped = 179.999
	}
	if clamped < -179.999 {
		clamped = -179.999
	}
	latRad := lat * math.Pi / 180
	n := float64(uint64(1) << uint(z))
	xt := (clamped + 180) / 360 * n
	yt := (1 - math.Log(math.Tan(math.Pi/4+latRad/2))/math.Pi) / 2 * n
	return int(xt), int(yt), z
}

func execSetup(t *testing.T, dsn string, stmts []string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	defer pool.Close()
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
}

func isMVT(b []byte) bool {
	// MVT 是 protobuf 序列化的 layer 序列；首字节应为 field 3 (layers),
	// wire type 2 → 0x1A
	return len(b) > 0 && b[0] == 0x1A
}
