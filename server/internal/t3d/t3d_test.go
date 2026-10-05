package t3d

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// makeSourceDir 构造一个最小 3DTiles 产物目录并返回 LocalDirSource。
func makeSourceDir(t *testing.T) (LocalDirSource, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		"tileset.json":  []byte(`{"asset":{"version":"1.0"},"geometricError":0,"root":{}}`),
		"Data/0/0.b3dm": bytes.Repeat([]byte{0x62, 0x33, 0x64, 0x6d}, 512),
		"Data/0/1.b3dm": bytes.Repeat([]byte("xyz"), 333),
	}
	for rel, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return LocalDirSource{Dir: dir}, files
}

// buildManifest 枚举来源并组装合法 Manifest。
func buildManifest(t *testing.T, src FileSource) *Manifest {
	t.Helper()
	files, err := HashSource(src)
	if err != nil {
		t.Fatalf("HashSource: %v", err)
	}
	return &Manifest{
		Format:  FormatName,
		Version: FormatVersion,
		Task:    TaskInfo{ID: "t1", Type: "osgb->3dtiles"},
		Files:   files,
	}
}

// packToTemp 打包到临时文件，返回文件路径与字节。
func packToTemp(t *testing.T, src FileSource, m *Manifest) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := Pack(&buf, src, m); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	p := filepath.Join(t.TempDir(), "out.t3d")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, buf.Bytes()
}

func mustSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPackManifestFirstEntryAndLayout(t *testing.T) {
	src, files := makeSourceDir(t)
	m := buildManifest(t, src)
	_, data := packToTemp(t, src, m)

	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	if r.File[0].Name != ManifestEntry {
		t.Fatalf("first entry = %q, want %q", r.File[0].Name, ManifestEntry)
	}
	seen := map[string]bool{}
	for _, f := range r.File[1:] {
		seen[f.Name] = true
		if len(f.Name) < len(TilesPrefix) || f.Name[:len(TilesPrefix)] != TilesPrefix {
			t.Fatalf("entry %q missing %q prefix", f.Name, TilesPrefix)
		}
	}
	for rel := range files {
		if !seen[TilesPrefix+rel] {
			t.Fatalf("zip missing entry %q", TilesPrefix+rel)
		}
	}
}

func TestRoundTripSHA256AllMatch(t *testing.T) {
	src, files := makeSourceDir(t)
	m := buildManifest(t, src)
	p, _ := packToTemp(t, src, m)

	dest := t.TempDir()
	res, err := Extract(p, dest, DefaultExtractOptions)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Files != len(files) {
		t.Fatalf("extracted %d files, want %d", res.Files, len(files))
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("content mismatch for %s", rel)
		}
		if mustSHA(got) != mustSHA(want) {
			t.Fatalf("sha mismatch for %s", rel)
		}
	}
	// 结果统计与实际一致
	var total int64
	for _, b := range files {
		total += int64(len(b))
	}
	if res.Bytes != total {
		t.Fatalf("extracted bytes = %d, want %d", res.Bytes, total)
	}
}

func TestExtractRejectsTamperedSHA256(t *testing.T) {
	src, _ := makeSourceDir(t)
	m := buildManifest(t, src)
	// 篡改一个文件的哈希
	for i := range m.Files {
		if m.Files[i].Path == TilesPrefix+"Data/0/0.b3dm" {
			m.Files[i].SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
		}
	}
	p, _ := packToTemp(t, src, m)
	dest := t.TempDir()
	_, err := Extract(p, dest, DefaultExtractOptions)
	if err == nil {
		t.Fatal("tampered sha256 must fail extraction")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("sha256 mismatch")) {
		t.Fatalf("error should mention sha256 mismatch, got: %v", err)
	}
}

func TestExtractRejectsZipSlip(t *testing.T) {
	// 手工构造带穿越条目的恶意包：manifest 声明 tiles/../evil.txt
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.Create(ManifestEntry)
	content := []byte("evil")
	m := Manifest{
		Format:  FormatName,
		Version: FormatVersion,
		Files: []FileEntry{
			{Path: TilesPrefix + "../evil.txt", Size: 4, SHA256: mustSHA(content)},
		},
	}
	mj, _ := json.Marshal(m)
	mw.Write(mj)
	// 恶意 zip 条目名（zip 标准允许任意名字）
	h := &zip.FileHeader{Name: TilesPrefix + "../evil.txt", Method: zip.Deflate}
	fw, _ := zw.CreateHeader(h)
	fw.Write(content)
	zw.Close()

	p := filepath.Join(t.TempDir(), "evil.t3d")
	os.WriteFile(p, buf.Bytes(), 0o644)
	dest := t.TempDir()
	if _, err := Extract(p, dest, DefaultExtractOptions); err == nil {
		t.Fatal("zip slip entry must be rejected")
	}
	// 绝不允许写出目标目录之外的文件
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil.txt")); err == nil {
		t.Fatal("zip slip file was written outside dest")
	}
}

func TestExtractRejectsExtraEntry(t *testing.T) {
	// 包内夹带清单未声明的条目 → 拒绝（手工构造：zip 比清单多一个条目）
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.Create(ManifestEntry)
	m := Manifest{
		Format:  FormatName,
		Version: FormatVersion,
		Files: []FileEntry{
			{Path: RequiredEntry, Size: 2, SHA256: mustSHA([]byte("{}"))},
		},
	}
	mj, _ := json.Marshal(m)
	mw.Write(mj)
	fw, _ := zw.Create(RequiredEntry)
	fw.Write([]byte("{}"))
	// 未声明的夹带条目
	ew, _ := zw.Create(TilesPrefix + "smuggled.bin")
	ew.Write([]byte("sneaky"))
	zw.Close()

	p := filepath.Join(t.TempDir(), "extra.t3d")
	os.WriteFile(p, buf.Bytes(), 0o644)
	_, err := Extract(p, t.TempDir(), DefaultExtractOptions)
	if err == nil {
		t.Fatal("undeclared zip entry must be rejected")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("not declared in manifest")) {
		t.Fatalf("want undeclared-entry error, got: %v", err)
	}
}

func TestExtractRejectsMissingEntry(t *testing.T) {
	// 清单声明了但包内没有 → 拒绝（手工构造：zip 只有 manifest.json）
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.Create(ManifestEntry)
	m := Manifest{
		Format:  FormatName,
		Version: FormatVersion,
		Files: []FileEntry{
			{Path: RequiredEntry, Size: 2, SHA256: mustSHA([]byte("{}"))},
			{Path: TilesPrefix + "ghost.bin", Size: 1, SHA256: mustSHA([]byte("x"))},
		},
	}
	mj, _ := json.Marshal(m)
	mw.Write(mj)
	// tileset.json 真实存在，ghost.bin 缺失
	fw, _ := zw.Create(RequiredEntry)
	fw.Write([]byte("{}"))
	zw.Close()

	p := filepath.Join(t.TempDir(), "missing.t3d")
	os.WriteFile(p, buf.Bytes(), 0o644)
	_, err := Extract(p, t.TempDir(), DefaultExtractOptions)
	if err == nil {
		t.Fatal("missing declared entry must be rejected")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("missing in zip")) {
		t.Fatalf("want missing-entry error, got: %v", err)
	}
}

func TestExtractEnforcesLimits(t *testing.T) {
	src, _ := makeSourceDir(t)
	m := buildManifest(t, src)
	p, _ := packToTemp(t, src, m)

	// 条目数上限
	if _, err := Extract(p, t.TempDir(), ExtractOptions{MaxFiles: 2, MaxTotalBytes: 1 << 30}); err == nil {
		t.Fatal("entry count over limit must be rejected")
	}
	// 解压总量上限
	if _, err := Extract(p, t.TempDir(), ExtractOptions{MaxFiles: 65536, MaxTotalBytes: 10}); err == nil {
		t.Fatal("total bytes over limit must be rejected")
	}
}

func TestExtractRejectsBadManifest(t *testing.T) {
	src, _ := makeSourceDir(t)

	cases := map[string]func(m *Manifest){
		"wrong format":  func(m *Manifest) { m.Format = "other" },
		"wrong version": func(m *Manifest) { m.Version = 99 },
		"no files":      func(m *Manifest) { m.Files = nil },
		"no tileset": func(m *Manifest) {
			out := m.Files[:0]
			for _, f := range m.Files {
				if f.Path != RequiredEntry {
					out = append(out, f)
				}
			}
			m.Files = out
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := buildManifest(t, src)
			mutate(m)
			// 篡改后清单与包不再一致时（no tileset），只测清单静态校验：
			// 直接校验 validate()。
			if err := m.validate(); err == nil {
				t.Fatalf("manifest case %q should fail validation", name)
			}
		})
	}
}

func TestValidateEntryPath(t *testing.T) {
	good := []string{"tiles/tileset.json", "tiles/Data/0/0.b3dm"}
	for _, p := range good {
		if _, err := ValidateEntryPath(p); err != nil {
			t.Fatalf("path %q should be valid, got %v", p, err)
		}
	}
	bad := []string{
		"", "/abs/tileset.json", "tiles/../evil", "..", "a\\b", "a\x00b",
		"./tiles/tileset.json", "tiles//x.json", "tiles/./x.json",
	}
	for _, p := range bad {
		if _, err := ValidateEntryPath(p); err == nil {
			t.Fatalf("path %q should be rejected", p)
		}
	}
}

func TestHashSourceDeterministicOrder(t *testing.T) {
	src, _ := makeSourceDir(t)
	f1, err := HashSource(src)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := HashSource(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(f1) != len(f2) {
		t.Fatal("hash source length mismatch between runs")
	}
	for i := range f1 {
		if f1[i] != f2[i] {
			t.Fatalf("hash source not deterministic at %d: %+v vs %+v", i, f1[i], f2[i])
		}
	}
	// 字典序
	for i := 1; i < len(f1); i++ {
		if f1[i-1].Path >= f1[i].Path {
			t.Fatalf("files not sorted: %s >= %s", f1[i-1].Path, f1[i].Path)
		}
	}
}

func TestExtractRejectsNotZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.t3d")
	os.WriteFile(p, []byte("this is not a zip"), 0o644)
	if _, err := Extract(p, t.TempDir(), DefaultExtractOptions); err == nil {
		t.Fatal("non-zip input must be rejected")
	}
}

var _ io.Reader = (*bytes.Reader)(nil)
