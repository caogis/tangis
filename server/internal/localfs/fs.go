// Package localfs 提供桌面单机版的对象存储替代实现：
// 用本地文件系统承担 MinIO 的角色（MinIO 在企业/集群模式下仍保留）。
//
// 目录约定：root 为用户数据目录下的 artifacts/，占位前缀保持
// tasks/{task_id}/... 不变，因此 service 包的分发逻辑（本地优先、
// 缺失回源）与 PostgreSQL/MinIO 模式完全一致，无需分支。
package localfs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Store 本地文件系统对象存储，同时实现 objectstore.Uploader 与 Getter。
type Store struct {
	Root string
}

// New 创建本地对象存储并确保根目录存在。
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("localfs: create root %s: %w", root, err)
	}
	return &Store{Root: root}, nil
}

// UploadDir 把 localDir 下全部文件复制到 root/prefix 之下（保留相对结构）。
// 语义对齐 objectstore.Client.UploadDir：任一文件失败即返回错误。
func (s *Store) UploadDir(_ context.Context, localDir, prefix string) error {
	return filepath.WalkDir(localDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(localDir, p)
		if err != nil {
			return err
		}
		key := path.Join(prefix, filepath.ToSlash(rel))
		return s.copyFile(p, key)
	})
}

// Get 按 key 流式读取对象；不存在返回 error（返回前已关闭句柄，不留惰性流）。
func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	p, ok := s.resolve(key)
	if !ok {
		return nil, 0, fmt.Errorf("localfs: invalid key %q", key)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, fmt.Errorf("localfs: get %s: %w", key, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("localfs: stat %s: %w", key, err)
	}
	return f, st.Size(), nil
}

// LocalPath 返回 key 对应的本地绝对路径（供调试与服务分发复用）。
func (s *Store) LocalPath(key string) (string, bool) { return s.resolve(key) }

func (s *Store) copyFile(src, key string) error {
	dst, ok := s.resolve(key)
	if !ok {
		return fmt.Errorf("localfs: invalid key %q", key)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("localfs: mkdir for %s: %w", dst, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("localfs: read %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("localfs: write %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("localfs: copy %s -> %s: %w", src, dst, err)
	}
	return out.Close()
}

// resolve 把对象 key 解析为 root 下的绝对路径并做目录穿越防护：
// 任何逃逸出 root 的 key 一律拒绝（对齐 service.safeRelPath 的安全语义）。
func (s *Store) resolve(key string) (string, bool) {
	if strings.ContainsRune(key, '\x00') {
		return "", false
	}
	clean := path.Clean("/" + strings.ReplaceAll(key, "\\", "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", false
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return filepath.Join(s.Root, filepath.FromSlash(clean)), true
}
