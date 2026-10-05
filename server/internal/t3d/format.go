// Package t3d 实现 TanGIS 开放容器格式 .t3d（M2-F07）：
// 3DTiles 产物的打包（流式 zip）与解包导入（manifest + SHA256 逐文件校验）。
//
// 规范见 docs/CONTAINER-FORMAT.md：ZIP 容器，首条目 manifest.json，
// 其余条目位于 tiles/ 前缀下原样收纳 tileset.json 与全部子资源。
// 安全面：防 Zip Slip、条目数/解压总量上限、逐文件 SHA256 校验，
// 任一校验失败整体拒绝（不做部分导入）。
package t3d

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

// jsonMarshal JSON 编码（清单写出用）。
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// 容器格式常量（与 docs/CONTAINER-FORMAT.md 对齐）。
const (
	// FormatName manifest 的 format 字段固定值。
	FormatName = "tangis-3dtiles-package"
	// FormatVersion manifest 格式版本，升级只增不减。
	FormatVersion = 1
	// ManifestEntry ZIP 内清单条目名（必须为第一条目）。
	ManifestEntry = "manifest.json"
	// TilesPrefix 除清单外全部条目的强制前缀。
	TilesPrefix = "tiles/"
	// RequiredEntry 3DTiles 包入口文件（manifest.files 必须包含）。
	RequiredEntry = TilesPrefix + "tileset.json"
)

// manifest 校验失败等可预期错误（HTTP 400 语义）。
var ErrInvalid = errors.New("t3d: invalid package")

// errf 构造带上下文的 ErrInvalid 错误。
func errf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// FileEntry manifest.files[] 条目：单个文件的完整性信息。
type FileEntry struct {
	Path   string `json:"path"`   // ZIP 内条目名（含 tiles/ 前缀，/ 分隔）
	Size   int64  `json:"size"`   // 解压后字节数
	SHA256 string `json:"sha256"` // 解压后内容 SHA-256，小写 hex
}

// TaskInfo manifest.task：产生该包的任务元信息（仅追溯用，导入方新建任务）。
type TaskInfo struct {
	ID        string `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Source    string `json:"source,omitempty"`
	CreatedAt string `json:"created_at,omitempty"` // RFC 3339
}

// Manifest .t3d 包的清单（manifest.json schema）。
// 解包方必须忽略不认识的字段（向前兼容）。
type Manifest struct {
	Format    string         `json:"format"`  // 固定 FormatName
	Version   int            `json:"version"` // 固定 FormatVersion
	CreatedAt string         `json:"created_at,omitempty"`
	Task      TaskInfo       `json:"task,omitempty"`
	Origin    []float64      `json:"origin,omitempty"` // [lon, lat, height]
	Params    map[string]any `json:"params,omitempty"`
	Files     []FileEntry    `json:"files"`
}

// Validate 清单静态校验（不含与 ZIP 条目的对照，见 Extract 阶段）。
func (m *Manifest) Validate() error {
	return m.validate()
}

// validate 内部校验入口。
func (m *Manifest) validate() error {
	if m.Format != FormatName {
		return errf("manifest format = %q, want %q", m.Format, FormatName)
	}
	if m.Version != FormatVersion {
		return errf("manifest version = %d, want %d", m.Version, FormatVersion)
	}
	if len(m.Files) == 0 {
		return errf("manifest files empty")
	}
	seen := make(map[string]bool, len(m.Files))
	hasRequired := false
	for i, f := range m.Files {
		if _, perr := ValidateEntryPath(f.Path); perr != nil {
			return errf("files[%d] path: %v", i, perr)
		}
		if f.SHA256 == "" {
			return errf("files[%d] %s: missing sha256", i, f.Path)
		}
		if seen[f.Path] {
			return errf("manifest duplicate file path %q", f.Path)
		}
		seen[f.Path] = true
		if f.Path == RequiredEntry {
			hasRequired = true
		}
	}
	if !hasRequired {
		return errf("manifest files missing required entry %s", RequiredEntry)
	}
	return nil
}

// ValidateEntryPath 校验 zip 内条目相对路径的安全性（防 Zip Slip）：
// 拒绝绝对路径、反斜杠/NUL、空段、`..` 穿越；返回规范化后的干净路径。
func ValidateEntryPath(p string) (string, error) {
	if p == "" {
		return "", errf("empty path")
	}
	if strings.ContainsRune(p, '\x00') || strings.ContainsRune(p, '\\') {
		return "", errf("path %q contains illegal character", p)
	}
	if strings.HasPrefix(p, "/") {
		return "", errf("absolute path %q not allowed", p)
	}
	clean := path.Clean("/" + p) // 锚定根消化 ..
	clean = strings.TrimPrefix(clean, "/")
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errf("path %q contains empty/dot segment", p)
		}
	}
	if clean != p {
		return "", errf("path %q is not canonical (want %q)", p, clean)
	}
	return clean, nil
}
