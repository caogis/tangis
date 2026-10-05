// extract.go：.t3d 解包导入（校验优先，失败整体拒绝、不做部分导入）。
//
// 流程：zip.OpenReader（central directory 随机访问，不解压全包）→
// 校验首条目与清单 → 清单静态校验 → 条目集合对照（缺失/多余均拒绝）→
// 逐文件解压：边写边算 SHA256，与清单比对；全程防 Zip Slip 并累计
// 解压总量、条目数上限。任一失败返回 error，由调用方清理已落盘内容。
package t3d

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExtractOptions 解包安全限制（零值取 Default* 兜底）。
type ExtractOptions struct {
	MaxFiles      int   // ZIP 条目总数（含 manifest.json）上限
	MaxTotalBytes int64 // 全部条目解压后字节总量上限
}

// DefaultExtractOptions 默认安全限制（与 docs/CONTAINER-FORMAT.md 对齐）。
var DefaultExtractOptions = ExtractOptions{
	MaxFiles:      65536,
	MaxTotalBytes: 8 << 30,
}

// ExtractResult 解包结果统计。
type ExtractResult struct {
	Manifest *Manifest
	Files    int   // 成功解出的文件数
	Bytes    int64 // 解压总字节数
}

// Extract 将 zipPath 的 .t3d 包校验并解压到 destDir（自动创建）。
// 任何校验失败返回 error（*多个*错误时报告首个），已写出的文件由调用方
// 通过 os.RemoveAll(destDir) 清理。
func Extract(zipPath, destDir string, opts ExtractOptions) (*ExtractResult, error) {
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultExtractOptions.MaxFiles
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = DefaultExtractOptions.MaxTotalBytes
	}
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, errf("not a valid zip: %v", err)
	}
	defer r.Close()

	// 条目数上限（含 manifest.json 本身）
	if len(r.File) < 1 {
		return nil, errf("zip is empty")
	}
	if len(r.File) > opts.MaxFiles {
		return nil, errf("zip has %d entries, exceeds limit %d", len(r.File), opts.MaxFiles)
	}
	// 首条目必须是 manifest.json
	if r.File[0].Name != ManifestEntry {
		return nil, errf("first entry = %q, want %q", r.File[0].Name, ManifestEntry)
	}

	m, err := readManifest(r.File[0])
	if err != nil {
		return nil, err
	}
	if err := m.validate(); err != nil {
		return nil, err
	}

	// 条目集合对照：清单声明与包内条目（除 manifest.json）必须一一对应
	entries := make(map[string]*zip.File, len(r.File))
	for _, f := range r.File[1:] {
		if _, dup := entries[f.Name]; dup {
			return nil, errf("duplicate zip entry %q", f.Name)
		}
		entries[f.Name] = f
	}
	declared := make(map[string]bool, len(m.Files))
	for _, fe := range m.Files {
		declared[fe.Path] = true
		if entries[fe.Path] == nil {
			return nil, errf("entry %s declared in manifest but missing in zip", fe.Path)
		}
	}
	for name := range entries {
		if !declared[name] {
			return nil, errf("entry %s not declared in manifest", name)
		}
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("t3d: create dest dir: %w", err)
	}

	var total int64
	for i, fe := range m.Files {
		n, err := extractOne(entries[fe.Path], destDir, fe)
		if err != nil {
			return nil, err
		}
		total += n
		if total > opts.MaxTotalBytes {
			return nil, errf("uncompressed total %d bytes exceeds limit %d (at files[%d] %s)",
				total, opts.MaxTotalBytes, i, fe.Path)
		}
	}
	return &ExtractResult{Manifest: m, Files: len(m.Files), Bytes: total}, nil
}

// readManifest 读取并解析首条目 manifest.json。
func readManifest(f *zip.File) (*Manifest, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, errf("open %s: %v", ManifestEntry, err)
	}
	defer rc.Close()
	// 清单本身限制 4MB，防超大清单消耗内存
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(rc, 4<<20)).Decode(&m); err != nil {
		return nil, errf("parse %s: %v", ManifestEntry, err)
	}
	return &m, nil
}

// extractOne 解压单个条目到 destDir：容器内条目名带 tiles/ 前缀，
// 落盘时剥离前缀映射到目标目录根（tileset.json → {destDir}/tileset.json）。
// 路径安全校验（防 Zip Slip）→ 边写边算 SHA256 → 比对 size 与 sha256。
// 返回实际写入字节数。
func extractOne(f *zip.File, destDir string, fe FileEntry) (int64, error) {
	// 双重防御：非 tiles/ 前缀条目拒绝（清单校验已保证）
	rel := strings.TrimPrefix(f.Name, TilesPrefix)
	clean, err := ValidateEntryPath(rel)
	if err != nil {
		return 0, err // 防 Zip Slip
	}
	target := filepath.Join(destDir, filepath.FromSlash(clean))
	if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
		return 0, errf("entry %s escapes destination dir", f.Name)
	}

	rc, err := f.Open()
	if err != nil {
		return 0, errf("open entry %s: %v", f.Name, err)
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, fmt.Errorf("t3d: mkdir for %s: %w", clean, err)
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("t3d: create %s: %w", clean, err)
	}
	defer dst.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), rc)
	if err != nil {
		return 0, errf("decompress %s: %v", clean, err)
	}
	if n != fe.Size {
		return 0, errf("%s size = %d, manifest says %d", clean, n, fe.Size)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != fe.SHA256 {
		return 0, errf("sha256 mismatch: %s (got %s, manifest %s)", clean, got, fe.SHA256)
	}
	return n, nil
}
