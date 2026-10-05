// MemoryMetaStore：vector.MetaStore 的进程内实现（测试与无 PG 兜底，
// 与 task.NewMemoryStore 同定位）。
package vector

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryMetaStore 内存元数据存储。
type MemoryMetaStore struct {
	mu      sync.RWMutex
	sources map[string]*Source // id -> source
	layers  map[string]*Layer  // name -> layer
}

// NewMemoryMetaStore 创建空内存存储。
func NewMemoryMetaStore() *MemoryMetaStore {
	return &MemoryMetaStore{
		sources: map[string]*Source{},
		layers:  map[string]*Layer{},
	}
}

// CreateSource 实现 MetaStore；名称冲突返回 ErrConflict。
func (m *MemoryMetaStore) CreateSource(_ context.Context, s *Source) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.sources {
		if ex.Name == s.Name {
			return conflictf("source name %q already exists", s.Name)
		}
	}
	cp := *s
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	m.sources[cp.ID] = &cp
	return nil
}

// ListSources 实现 MetaStore（按创建时间倒序，与 PG 实现一致）。
func (m *MemoryMetaStore) ListSources(_ context.Context) ([]*Source, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Source, 0, len(m.sources))
	for _, s := range m.sources {
		cp := *s
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// GetSource 实现 MetaStore。
func (m *MemoryMetaStore) GetSource(_ context.Context, id string) (*Source, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sources[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

// DeleteSource 实现 MetaStore。
func (m *MemoryMetaStore) DeleteSource(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sources[id]; !ok {
		return ErrNotFound
	}
	delete(m.sources, id)
	return nil
}

// CountLayersBySource 实现 MetaStore。
func (m *MemoryMetaStore) CountLayersBySource(_ context.Context, sourceID string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, l := range m.layers {
		if l.SourceID == sourceID {
			n++
		}
	}
	return n, nil
}

// CreateLayer 实现 MetaStore；名称冲突返回 ErrConflict。
func (m *MemoryMetaStore) CreateLayer(_ context.Context, l *Layer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.layers[l.Name]; ok {
		return conflictf("layer name %q already exists", l.Name)
	}
	cp := *l
	cp.Fields = append([]string(nil), l.Fields...)
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	m.layers[cp.Name] = &cp
	return nil
}

// GetLayer 实现 MetaStore。
func (m *MemoryMetaStore) GetLayer(_ context.Context, name string) (*Layer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.layers[name]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *l
	return &cp, nil
}

// ListLayers 实现 MetaStore。
func (m *MemoryMetaStore) ListLayers(_ context.Context) ([]*Layer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Layer, 0, len(m.layers))
	for _, l := range m.layers {
		cp := *l
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// DeleteLayer 实现 MetaStore。
func (m *MemoryMetaStore) DeleteLayer(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.layers[name]; !ok {
		return ErrNotFound
	}
	delete(m.layers, name)
	return nil
}
