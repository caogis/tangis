-- TanGIS 矢量发布 fixture（M2-F10a，PRD F-10）
-- demo schema：示例地块多边形 + POI 点，供矢量注册/MVT 冒烟与联调。
-- 幂等：可重复执行（TRUNCATE 后重灌）；initdb 首次初始化时自动执行，
-- 已有库可手工导入：
--   docker exec -i tangis-postgres psql -U tangis -d tangis < deploy/initdb/03-vector-fixture.sql
CREATE SCHEMA IF NOT EXISTS demo;

CREATE TABLE IF NOT EXISTS demo.parcels (
    gid     serial PRIMARY KEY,
    name    text NOT NULL,
    kind    text NOT NULL,
    area_ha double precision,
    geom    geometry(Polygon, 4326) NOT NULL
);

CREATE TABLE IF NOT EXISTS demo.pois (
    id    serial PRIMARY KEY,
    name  text NOT NULL,
    cat   text NOT NULL,
    geom  geometry(Point, 4326) NOT NULL
);

TRUNCATE demo.parcels, demo.pois;

-- 4x3 地块网格（北京城市副中心一带，116.30~116.36 / 39.90~39.94），
-- 面积近似值按 1 度 ≈ 85km（纬向）粗算
INSERT INTO demo.parcels (name, kind, area_ha, geom)
SELECT
    format('P%s-%s', gx, gy),
    (ARRAY['residential','commercial','industrial','green'])[1 + ((gx + gy) % 4)],
    round((0.08 * 85 * 0.06 * 111)::numeric, 1)::double precision,
    ST_MakeEnvelope(
        116.30 + gx * 0.015,
        39.90 + gy * 0.015,
        116.31 + gx * 0.015,
        39.91 + gy * 0.015,
    4326)
FROM generate_series(0, 3) gx, generate_series(0, 2) gy;

-- 12 个 POI（地块中心）
INSERT INTO demo.pois (name, cat, geom)
SELECT
    format('POI-%s', p.name),
    (ARRAY['school','hospital','market','park'])[1 + (gid % 4)],
    ST_Centroid(p.geom)
FROM demo.parcels p;

CREATE INDEX IF NOT EXISTS idx_parcels_geom ON demo.parcels USING gist (geom);
CREATE INDEX IF NOT EXISTS idx_pois_geom ON demo.pois USING gist (geom);

ANALYZE demo.parcels;
ANALYZE demo.pois;
