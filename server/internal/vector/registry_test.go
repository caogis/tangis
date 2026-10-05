package vector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJSONMetaStorePersistsDSN 注册表必须把数据源 DSN 落盘并在重开后还原。
//
// 背景：Source.DSN 标了 `json:"-"`（防止 API 回显带出 PostgreSQL 密码），
// 若直接序列化 Source 结构体，DSN 会静默丢失 —— 重启后文件矢量数据源变成
// 空 DSN，被路由网关判成「未装配 PostGIS」，MVT/WFS 全链路失效（实测踩到）。
func TestJSONMetaStorePersistsDSN(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vector", "registry.json")

	m1, err := NewJSONMetaStore(path)
	if err != nil {
		t.Fatalf("NewJSONMetaStore: %v", err)
	}
	src := &Source{ID: "s1", Name: "beijing", DSN: "file:///data/beijing_blocks.geojson"}
	if err := m1.CreateSource(ctx, src); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if err := m1.CreateLayer(ctx, &Layer{Name: "beijing", SourceID: "s1", Table: "beijing", SRID: 4326}); err != nil {
		t.Fatalf("CreateLayer: %v", err)
	}

	// 落盘文件里必须能看到 DSN（而不是被 json:"-" 吃掉）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读注册表: %v", err)
	}
	if !strings.Contains(string(raw), "file:///data/beijing_blocks.geojson") {
		t.Errorf("注册表未保存 DSN：\n%s", raw)
	}

	// 模拟重启：重新打开注册表
	m2, err := NewJSONMetaStore(path)
	if err != nil {
		t.Fatalf("重开注册表: %v", err)
	}
	got, err := m2.GetSource(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if got.DSN != "file:///data/beijing_blocks.geojson" {
		t.Errorf("DSN 未还原: %q（重启后矢量链路会整条失效）", got.DSN)
	}
	layers, err := m2.ListLayers(ctx)
	if err != nil || len(layers) != 1 {
		t.Fatalf("图层未持久化: %v %v", layers, err)
	}
	if layers[0].SourceID != "s1" || layers[0].SRID != 4326 {
		t.Errorf("图层字段丢失: %+v", layers[0])
	}
}

// TestJSONMetaStoreSemantics 名称唯一、被引用的数据源不可删（与 Memory 实现同语义）。
func TestJSONMetaStoreSemantics(t *testing.T) {
	ctx := context.Background()
	m, err := NewJSONMetaStore(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CreateSource(ctx, &Source{ID: "s1", Name: "dup", DSN: "file:///a.geojson"}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateSource(ctx, &Source{ID: "s2", Name: "dup", DSN: "file:///b.geojson"}); err == nil {
		t.Error("重名数据源应报冲突")
	}
	if err := m.CreateLayer(ctx, &Layer{Name: "l1", SourceID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteSource(ctx, "s1"); err == nil {
		t.Error("仍被图层引用的数据源不应允许删除")
	}
	if err := m.DeleteLayer(ctx, "l1"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteSource(ctx, "s1"); err != nil {
		t.Errorf("图层删除后应可删数据源: %v", err)
	}
	if _, err := m.GetSource(ctx, "s1"); err == nil {
		t.Error("删除后应查不到")
	}
}
