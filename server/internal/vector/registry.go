// registry.go 文件持久化的矢量注册表（MetaStore 实现）。
//
// 为什么需要它：桌面单机版没有 PostgreSQL，而内存注册表在重启后会丢失已接入的
// 图层，用户每次重启都要重新导入。这里把数据源/图层注册信息持久化到数据目录下的
// JSON 文件（默认 <dataDir>/vector/registry.json），并保证写盘原子性
// （临时文件 + rename），避免断电留下半个 JSON。
//
// 语义与 MemoryMetaStore 保持一致：名称唯一、被图层引用的数据源不可删。
package vector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// registryDoc 内存结构（带版本号便于将来迁移）。
type registryDoc struct {
	Version int       `json:"version"`
	Sources []*Source `json:"sources"`
	Layers  []*Layer  `json:"layers"`
}

// persistedSource Source 的落盘形态。
//
// 为什么需要它：Source.DSN 标了 `json:"-"`（避免 API 回显里带出 PostgreSQL 密码），
// 但注册表**必须**保存 DSN —— 否则重启后数据源变成空 DSN，文件矢量会被判成
// 「未装配 PostGIS」而整条链路失效（实测踩到过）。这里用外层同名字段承载：
// encoding/json 对同名字段取「更外层」的那个，内层的 `-` 仍然生效。
type persistedSource struct {
	*Source
	DSN string `json:"dsn"`
}

// onDiskDoc 实际写盘/读取的文档结构。
type onDiskDoc struct {
	Version int               `json:"version"`
	Sources []persistedSource `json:"sources"`
	Layers  []*Layer          `json:"layers"`
}

// RegistryVersion 当前注册表结构版本。
const RegistryVersion = 1

// JSONMetaStore 持久化到 JSON 文件的 MetaStore。
type JSONMetaStore struct {
	mu   sync.RWMutex
	path string
	doc  registryDoc
}

// NewJSONMetaStore 打开（或初始化）注册表文件；父目录会自动创建。
func NewJSONMetaStore(path string) (*JSONMetaStore, error) {
	m := &JSONMetaStore{path: path, doc: registryDoc{Version: RegistryVersion}}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) > 0 {
			var disk onDiskDoc
			if jerr := json.Unmarshal(data, &disk); jerr != nil {
				return nil, fmt.Errorf("vector: 注册表损坏 %s: %w", path, jerr)
			}
			m.doc.Version = disk.Version
			if m.doc.Version == 0 {
				m.doc.Version = RegistryVersion
			}
			m.doc.Layers = disk.Layers
			m.doc.Sources = make([]*Source, 0, len(disk.Sources))
			for _, ps := range disk.Sources {
				if ps.Source == nil {
					continue
				}
				ps.Source.DSN = ps.DSN // 还原落盘的外层 DSN
				m.doc.Sources = append(m.doc.Sources, ps.Source)
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// 首次运行
	default:
		return nil, fmt.Errorf("vector: 读取注册表 %s: %w", path, err)
	}
	if m.doc.Sources == nil {
		m.doc.Sources = []*Source{}
	}
	if m.doc.Layers == nil {
		m.doc.Layers = []*Layer{}
	}
	return m, nil
}

// Path 注册表文件路径。
func (m *JSONMetaStore) Path() string { return m.path }

// flushLocked 原子落盘（调用方须持有写锁）。
func (m *JSONMetaStore) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return fmt.Errorf("vector: 创建注册表目录: %w", err)
	}
	data, err := json.MarshalIndent(m.snapshotLocked(), "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("vector: 写注册表: %w", err)
	}
	return os.Rename(tmp, m.path)
}

// snapshotLocked 组装落盘文档（把 DSN 显式搬到外层字段）。
func (m *JSONMetaStore) snapshotLocked() onDiskDoc {
	doc := onDiskDoc{Version: RegistryVersion, Layers: m.doc.Layers, Sources: make([]persistedSource, 0, len(m.doc.Sources))}
	for _, s := range m.doc.Sources {
		doc.Sources = append(doc.Sources, persistedSource{Source: s, DSN: s.DSN})
	}
	return doc
}

func (m *JSONMetaStore) CreateSource(_ context.Context, s *Source) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.doc.Sources {
		if e.Name == s.Name {
			return conflictf("source name %q already exists", s.Name)
		}
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	m.doc.Sources = append(m.doc.Sources, s)
	return m.flushLocked()
}

func (m *JSONMetaStore) ListSources(_ context.Context) ([]*Source, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Source, len(m.doc.Sources))
	copy(out, m.doc.Sources)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *JSONMetaStore) GetSource(_ context.Context, id string) (*Source, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.doc.Sources {
		if s.ID == id {
			cp := *s
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("%w: source %s", ErrNotFound, id)
}

func (m *JSONMetaStore) DeleteSource(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.doc.Layers {
		if l.SourceID == id {
			return conflictf("source %s still referenced by layer %q", id, l.Name)
		}
	}
	kept := m.doc.Sources[:0]
	found := false
	for _, s := range m.doc.Sources {
		if s.ID == id {
			found = true
			continue
		}
		kept = append(kept, s)
	}
	if !found {
		return fmt.Errorf("%w: source %s", ErrNotFound, id)
	}
	m.doc.Sources = kept
	return m.flushLocked()
}

func (m *JSONMetaStore) CountLayersBySource(_ context.Context, sourceID string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, l := range m.doc.Layers {
		if l.SourceID == sourceID {
			n++
		}
	}
	return n, nil
}

func (m *JSONMetaStore) CreateLayer(_ context.Context, l *Layer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.doc.Layers {
		if e.Name == l.Name {
			return conflictf("layer name %q already exists", l.Name)
		}
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now().UTC()
	}
	m.doc.Layers = append(m.doc.Layers, l)
	return m.flushLocked()
}

func (m *JSONMetaStore) GetLayer(_ context.Context, name string) (*Layer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, l := range m.doc.Layers {
		if l.Name == name {
			cp := *l
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("%w: layer %s", ErrNotFound, name)
}

func (m *JSONMetaStore) ListLayers(_ context.Context) ([]*Layer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Layer, len(m.doc.Layers))
	copy(out, m.doc.Layers)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *JSONMetaStore) DeleteLayer(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.doc.Layers[:0]
	found := false
	for _, l := range m.doc.Layers {
		if l.Name == name {
			found = true
			continue
		}
		kept = append(kept, l)
	}
	if !found {
		return fmt.Errorf("%w: layer %s", ErrNotFound, name)
	}
	m.doc.Layers = kept
	return m.flushLocked()
}
