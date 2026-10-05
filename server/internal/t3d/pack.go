// pack.go：.t3d 打包（流式 zip 写出）。
//
// manifest.json 要求为 ZIP 首条目，而清单含全部文件的 SHA256——故先对流式
// 读取各文件计算哈希（HashSource，内容不驻留内存），再按「清单首条 + 逐文件
// 流式拷贝」两段写出。任一文件在两段之间发生变化（大小不符）即报错，不造假。
package t3d

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
)

// FileSource 打包产物来源抽象：本地目录 / MinIO 回源由调用方实现。
type FileSource interface {
	// Files 枚举待打包文件的相对路径（/ 分隔，不含 manifest.json）。
	Files() ([]string, error)
	// Open 打开单个文件，返回内容流（调用方负责 Close）。
	Open(rel string) (io.ReadCloser, error)
}

// HashSource 流式遍历来源并计算每个文件的 size 与 SHA256，
// 返回可直接填入 Manifest.Files 的清单（按路径字典序稳定排序）。
// 清单路径带 tiles/ 前缀（zip 内条目名），来源枚举的原始相对路径在写出时剥去。
func HashSource(src FileSource) ([]FileEntry, error) {
	files, err := src.Files()
	if err != nil {
		return nil, fmt.Errorf("t3d: enumerate source: %w", err)
	}
	if len(files) == 0 {
		return nil, errf("source has no files")
	}
	sort.Strings(files)
	out := make([]FileEntry, 0, len(files))
	for _, rel := range files {
		rc, err := src.Open(rel)
		if err != nil {
			return nil, fmt.Errorf("t3d: open %s: %w", rel, err)
		}
		h := sha256.New()
		n, err := io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("t3d: hash %s: %w", rel, err)
		}
		out = append(out, FileEntry{Path: TilesPrefix + rel, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	return out, nil
}

// Pack 将清单与文件流式写入 w（zip 格式）。
// m.Files 须已经 HashSource 填充；manifest.json 写为首条目，
// 其余文件按清单顺序（调用方应传入排序后的清单）以 tiles/ 前缀
// 原样收纳。写出过程逐块拷贝，不全量驻留内存。
func Pack(w io.Writer, src FileSource, m *Manifest) error {
	if err := m.validate(); err != nil {
		return err
	}
	zw := zip.NewWriter(w)

	// 1) manifest.json 首条目
	mw, err := zw.Create(ManifestEntry)
	if err != nil {
		return fmt.Errorf("t3d: create manifest entry: %w", err)
	}
	if err := writeJSON(mw, m); err != nil {
		return err
	}

	// 2) 逐文件流式写入（清单顺序；来源按剥去 tiles/ 前缀的原始路径打开）
	for _, fe := range m.Files {
		raw := strings.TrimPrefix(fe.Path, TilesPrefix)
		fw, err := zw.CreateHeader(&zip.FileHeader{
			Name:   fe.Path,
			Method: zip.Deflate,
		})
		if err != nil {
			return fmt.Errorf("t3d: create entry %s: %w", fe.Path, err)
		}
		rc, err := src.Open(raw)
		if err != nil {
			return fmt.Errorf("t3d: open %s: %w", raw, err)
		}
		n, err := io.Copy(fw, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("t3d: write %s: %w", raw, err)
		}
		// 两段读取之间文件发生变化：拒绝（不造假）
		if n != fe.Size {
			return fmt.Errorf("t3d: file %s changed during pack (size %d, manifest %d)", fe.Path, n, fe.Size)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("t3d: close zip: %w", err)
	}
	return nil
}
