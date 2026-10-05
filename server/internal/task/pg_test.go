package task

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"
)

func getenv(key string) string { return os.Getenv(key) }

// runStoreConformance 对任意 Store 实现执行同一组行为断言，
// 保证内存/PG 两种实现对上层语义一致。
func runStoreConformance(t *testing.T, s Store, idPrefix string) {
	t.Helper()

	// Create：ID 为空自动生成
	a := &Task{Type: "osgb->3dtiles", Source: "/data/osgb/a", Output: "/data/out/a", Status: StatusPending}
	if err := s.Create(a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.ID == "" || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Fatalf("Create should fill id/timestamps, got %+v", a)
	}
	t.Cleanup(func() { _ = s.Delete(a.ID, "") })

	// Create：保留指定 ID
	b := &Task{ID: fmt.Sprintf("%s-fixed", idPrefix), Type: "image->tiles", Source: "/data/img/b", Output: "/data/out/b", Status: StatusPending}
	if err := s.Create(b); err != nil {
		t.Fatalf("Create with id: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(b.ID, "") })

	// Get
	got, err := s.Get(a.ID, "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Type != a.Type || got.Status != StatusPending || got.MinioPrefix != "" {
		t.Fatalf("Get mismatch: %+v", got)
	}

	// Get：不存在 -> ErrNotFound
	if _, err := s.Get(fmt.Sprintf("%s-missing", idPrefix), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}

	// Get：租户不匹配 -> ErrNotFound（F-06 租户隔离）
	if _, err := s.Get(a.ID, "other-tenant"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get cross-tenant = %v, want ErrNotFound", err)
	}

	// Update：含 MinioPrefix 全字段覆盖
	a.Status = StatusSucceeded
	a.MinioPrefix = "tasks/" + a.ID
	a.ErrMsg = ""
	if err := s.Update(a); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = s.Get(a.ID, "")
	if got.Status != StatusSucceeded || got.MinioPrefix != "tasks/"+a.ID {
		t.Fatalf("Update mismatch: %+v", got)
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatalf("UpdatedAt %v should not be before CreatedAt %v", got.UpdatedAt, got.CreatedAt)
	}
	// F-04：progress/params/attempts/tenant/approved 全字段覆盖往返
	a.Progress = &Progress{Done: 2, Total: 5}
	a.Params = map[string]any{"lod": float64(2), "crs": "EPSG:4490"}
	a.Attempts = 1
	a.TenantID = "tenant-x"
	a.Approved = true
	if err := s.Update(a); err != nil {
		t.Fatalf("Update extended: %v", err)
	}
	got, _ = s.Get(a.ID, "")
	if got.Progress == nil || got.Progress.Done != 2 || got.Progress.Total != 5 {
		t.Fatalf("progress roundtrip mismatch: %+v", got.Progress)
	}
	if got.Params["crs"] != "EPSG:4490" || got.Params["lod"] != float64(2) {
		t.Fatalf("params roundtrip mismatch: %+v", got.Params)
	}
	if got.Attempts != 1 || got.TenantID != "tenant-x" || !got.Approved {
		t.Fatalf("attempts/tenant/approved roundtrip mismatch: %+v", got)
	}
	// M2-F08b：qc_status/qc_summary 全字段往返
	a.QcStatus = QcPass
	a.QcSummary = &QcSummary{
		TileCount:              80,
		TotalDegenerate:        1,
		TotalFlippedEdges:      2,
		TotalFloating:          3,
		TotalCrackSegments:     4,
		TotalSelfIntersections: 5,
	}
	if err := s.Update(a); err != nil {
		t.Fatalf("Update qc fields: %v", err)
	}
	got, _ = s.Get(a.ID, "")
	if got.QcStatus != QcPass {
		t.Fatalf("qc_status roundtrip = %q, want %q", got.QcStatus, QcPass)
	}
	if got.QcSummary == nil || *got.QcSummary != *a.QcSummary {
		t.Fatalf("qc_summary roundtrip mismatch: %+v vs %+v", got.QcSummary, a.QcSummary)
	}
	// 清空质检状态也应如实落库（回退为未检）
	a.QcStatus = ""
	a.QcSummary = nil
	if err := s.Update(a); err != nil {
		t.Fatalf("Update qc cleared: %v", err)
	}
	got, _ = s.Get(a.ID, "")
	if got.QcStatus != "" || got.QcSummary != nil {
		t.Fatalf("qc cleared roundtrip mismatch: %q %+v", got.QcStatus, got.QcSummary)
	}
	// M2-F09c：编辑任务字段（parent_task_id/ops_summary）全字段往返
	a.ParentTaskID = "parent-t1"
	a.OpsSummary = map[string]any{"op": "clip", "totals": map[string]any{"tiles": float64(3)}}
	if err := s.Update(a); err != nil {
		t.Fatalf("Update edit fields: %v", err)
	}
	got, _ = s.Get(a.ID, "")
	if got.ParentTaskID != "parent-t1" {
		t.Fatalf("parent_task_id roundtrip = %q", got.ParentTaskID)
	}
	if got.OpsSummary == nil || got.OpsSummary["op"] != "clip" {
		t.Fatalf("ops_summary roundtrip mismatch: %+v", got.OpsSummary)
	}
	// 清空编辑字段也应如实落库
	a.ParentTaskID = ""
	a.OpsSummary = nil
	if err := s.Update(a); err != nil {
		t.Fatalf("Update edit cleared: %v", err)
	}
	got, _ = s.Get(a.ID, "")
	if got.ParentTaskID != "" || got.OpsSummary != nil {
		t.Fatalf("edit cleared roundtrip mismatch: %q %+v", got.ParentTaskID, got.OpsSummary)
	}
	// 租户 scope：按 tenant-x 可见，其他租户不可见
	if _, err := s.Get(a.ID, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Get = %v, want ErrNotFound", err)
	}
	scoped, err := s.List("tenant-x")
	if err != nil || len(scoped) == 0 {
		t.Fatalf("tenant scoped list = %v, err %v", scoped, err)
	}

	// Update：不存在 -> ErrNotFound
	if err := s.Update(&Task{ID: fmt.Sprintf("%s-missing", idPrefix)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing = %v, want ErrNotFound", err)
	}

	// List：包含新任务且倒序
	list, err := s.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, tk := range list {
		ids[tk.ID] = true
	}
	if !ids[a.ID] || !ids[b.ID] {
		t.Fatalf("List should contain created tasks, got %d tasks", len(list))
	}
	stamps := make([]time.Time, len(list))
	for i, tk := range list {
		stamps[i] = tk.CreatedAt
	}
	if !sort.SliceIsSorted(stamps, func(i, j int) bool { return stamps[i].After(stamps[j]) }) {
		t.Fatal("List should be ordered by created_at DESC")
	}

	// Delete 后 Get -> ErrNotFound
	if err := s.Delete(b.ID, ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(b.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(b.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreConformance(t *testing.T) {
	runStoreConformance(t, NewMemoryStore(), fmt.Sprintf("mem-%d", rand.Int63()))
}

// openTestPG 连接本机测试库；连不上则跳过（CI/离线环境不阻塞）。
// 库地址可用 TANGIS_TEST_DATABASE_URL 覆盖，默认对齐 deploy/.env.example。
func openTestPG(t *testing.T) *PGStore {
	t.Helper()
	url := envOrStr("TANGIS_TEST_DATABASE_URL",
		"postgres://tangis:tangis_dev_password@127.0.0.1:15432/tangis?sslmode=disable")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := NewPGStore(ctx, url)
	if err != nil {
		t.Skipf("PostgreSQL unavailable at %s, skipping PG tests: %v", url, err)
	}
	t.Cleanup(s.Close)
	return s
}

func envOrStr(key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

func TestPGStoreConformance(t *testing.T) {
	s := openTestPG(t)
	runStoreConformance(t, s, fmt.Sprintf("pgtest-%d", rand.Int63()))
}

func TestPGStoreEnsureSchemaIdempotent(t *testing.T) {
	s := openTestPG(t)
	// 重复执行建表语句应无副作用（幂等）
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(ctx, schemaDDL); err != nil {
		t.Fatalf("re-run schema DDL: %v", err)
	}
}
