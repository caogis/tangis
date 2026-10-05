// PGMetaStore：vector.MetaStore 的 PostgreSQL 实现（pgx v5 + pgxpool）。
// 元数据存 apiserver 自己的库（DATABASE_URL），表结构见 schemaDDL；
// 与 task.PGStore 同风格：启动建表（IF NOT EXISTS），失败由调用方降级。
package vector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaDDL 数据源/图层注册表。
const schemaDDL = `
CREATE TABLE IF NOT EXISTS vector_sources (
    id              text PRIMARY KEY,
    name            text NOT NULL UNIQUE,
    dsn             text NOT NULL,
    postgis_version text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS vector_layers (
    name            text PRIMARY KEY,
    source_id       text NOT NULL REFERENCES vector_sources(id),
    schema_name     text NOT NULL,
    table_name      text NOT NULL,
    geom_column     text NOT NULL,
    geometry_type   text NOT NULL DEFAULT '',
    srid            int  NOT NULL,
    id_column       text NOT NULL,
    fields          jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_vector_layers_source ON vector_layers (source_id);
`

// PGMetaStore MetaStore 的 PostgreSQL 实现。
type PGMetaStore struct {
	pool *pgxpool.Pool
}

// NewPGMetaStore 建立连接池并确保表结构存在。ctx 建议带超时。
func NewPGMetaStore(ctx context.Context, url string) (*PGMetaStore, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("vector: pg pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("vector: pg ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaDDL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("vector: pg ensure schema: %w", err)
	}
	return &PGMetaStore{pool: pool}, nil
}

// Close 释放连接池。
func (s *PGMetaStore) Close() { s.pool.Close() }

// CreateSource 实现 MetaStore；名称冲突返回 ErrConflict。
func (s *PGMetaStore) CreateSource(ctx context.Context, src *Source) error {
	src.CreatedAt = time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
INSERT INTO vector_sources (id, name, dsn, postgis_version, created_at)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (name) DO NOTHING`,
		src.ID, src.Name, src.DSN, src.PostGISVersion, src.CreatedAt)
	if err != nil {
		return fmt.Errorf("vector: pg create source: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: source name %q already exists", ErrConflict, src.Name)
	}
	return nil
}

// ListSources 实现 MetaStore。
func (s *PGMetaStore) ListSources(ctx context.Context) ([]*Source, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, name, dsn, postgis_version, created_at FROM vector_sources ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("vector: pg list sources: %w", err)
	}
	defer rows.Close()
	out := make([]*Source, 0)
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// GetSource 实现 MetaStore。
func (s *PGMetaStore) GetSource(ctx context.Context, id string) (*Source, error) {
	src, err := scanSource(s.pool.QueryRow(ctx, `
SELECT id, name, dsn, postgis_version, created_at FROM vector_sources WHERE id=$1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("vector: pg get source %s: %w", id, err)
	}
	return src, nil
}

// DeleteSource 实现 MetaStore。
func (s *PGMetaStore) DeleteSource(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM vector_sources WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("vector: pg delete source %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountLayersBySource 实现 MetaStore。
func (s *PGMetaStore) CountLayersBySource(ctx context.Context, sourceID string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM vector_layers WHERE source_id=$1`, sourceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("vector: pg count layers by source: %w", err)
	}
	return n, nil
}

// CreateLayer 实现 MetaStore；名称冲突返回 ErrConflict。
func (s *PGMetaStore) CreateLayer(ctx context.Context, l *Layer) error {
	l.CreatedAt = time.Now().UTC()
	fields, err := json.Marshal(nonNilStrings(l.Fields))
	if err != nil {
		return fmt.Errorf("vector: marshal fields: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `
INSERT INTO vector_layers (name, source_id, schema_name, table_name, geom_column,
	geometry_type, srid, id_column, fields, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (name) DO NOTHING`,
		l.Name, l.SourceID, l.Schema, l.Table, l.GeometryColumn,
		l.GeometryType, l.SRID, l.IDColumn, string(fields), l.CreatedAt)
	if err != nil {
		return fmt.Errorf("vector: pg create layer: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: layer name %q already exists", ErrConflict, l.Name)
	}
	return nil
}

// GetLayer 实现 MetaStore。
func (s *PGMetaStore) GetLayer(ctx context.Context, name string) (*Layer, error) {
	l, err := scanLayer(s.pool.QueryRow(ctx, `SELECT `+layerColumns+` FROM vector_layers WHERE name=$1`, name))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("vector: pg get layer %s: %w", name, err)
	}
	return l, nil
}

// ListLayers 实现 MetaStore。
func (s *PGMetaStore) ListLayers(ctx context.Context) ([]*Layer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+layerColumns+` FROM vector_layers ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("vector: pg list layers: %w", err)
	}
	defer rows.Close()
	out := make([]*Layer, 0)
	for rows.Next() {
		l, err := scanLayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLayer 实现 MetaStore。
func (s *PGMetaStore) DeleteLayer(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM vector_layers WHERE name=$1`, name)
	if err != nil {
		return fmt.Errorf("vector: pg delete layer %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const layerColumns = `name, source_id, schema_name, table_name, geom_column,
	COALESCE(geometry_type, ''), srid, id_column, fields, created_at`

func scanSource(row pgx.Row) (*Source, error) {
	var s Source
	if err := row.Scan(&s.ID, &s.Name, &s.DSN, &s.PostGISVersion, &s.CreatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

func scanLayer(row pgx.Row) (*Layer, error) {
	var l Layer
	var fields []byte
	if err := row.Scan(&l.Name, &l.SourceID, &l.Schema, &l.Table, &l.GeometryColumn,
		&l.GeometryType, &l.SRID, &l.IDColumn, &fields, &l.CreatedAt); err != nil {
		return nil, err
	}
	if len(fields) > 0 {
		if err := json.Unmarshal(fields, &l.Fields); err != nil {
			return nil, fmt.Errorf("vector: pg scan fields: %w", err)
		}
	}
	return &l, nil
}

// nonNilStrings 去掉 nil/空串字段（jsonb 序列化兜底）。
func nonNilStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
