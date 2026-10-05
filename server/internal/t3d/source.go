// source.go：打包产物来源实现（本地目录 / MinIO / 本地优先回源组合）。
package t3d

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"tangis/server/internal/objectstore"
)

// writeJSON JSON 编码并写出（不带换行差异要求，供 manifest 使用）。
func writeJSON(w io.Writer, v any) error {
	data, err := jsonMarshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// LocalDirSource 本地目录产物来源：Files 遍历目录内全部普通文件（相对路径，
// / 分隔）；Open 按相对路径打开。
type LocalDirSource struct {
	Dir string
}

// Files 实现 FileSource。
func (s LocalDirSource) Files() ([]string, error) {
	var out []string
	err := filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.Dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Open 实现 FileSource。
func (s LocalDirSource) Open(rel string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(s.Dir, filepath.FromSlash(rel)))
}

// MinioSource MinIO 前缀产物来源（worker 上传约定 tasks/{task_id}/ 前缀）。
// Objects 为 nil 时不可用。
type MinioSource struct {
	Objects objectstore.GetterLister
	Prefix  string
	Timeout time.Duration // 单次列表/读取超时，零取 30s
}

// Files 实现 FileSource：列出前缀下对象并剥去 prefix 得相对路径。
func (s MinioSource) Files() ([]string, error) {
	if s.Objects == nil {
		return nil, errf("object storage unavailable")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	infos, err := s.Objects.List(ctx, s.Prefix)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, info := range infos {
		if strings.HasSuffix(info.Key, "/") {
			continue // 目录占位对象
		}
		rel, ok := strings.CutPrefix(info.Key, s.Prefix+"/")
		if !ok || rel == "" {
			continue
		}
		out = append(out, rel)
	}
	if len(out) == 0 {
		return nil, errf("prefix %s has no objects", s.Prefix)
	}
	return out, nil
}

// Open 实现 FileSource：按 prefix+rel 流式读取对象。
func (s MinioSource) Open(rel string) (io.ReadCloser, error) {
	if s.Objects == nil {
		return nil, errf("object storage unavailable")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rc, _, err := s.Objects.Get(ctx, path.Join(s.Prefix, rel))
	return rc, err
}

// ArtifactSource 打包下载的产物来源：本地目录优先，缺失（目录不存在或
// 文件打开失败）时回源 MinIO。两者皆不可用时报错。
type ArtifactSource struct {
	Dir     string                   // 任务 output 本地目录
	Objects objectstore.GetterLister // 可为 nil
	Prefix  string                   // MinIO key 前缀（tasks/{id}）
}

// Files 实现 FileSource：本地目录存在且非空用本地清单，否则 MinIO 列表。
func (s ArtifactSource) Files() ([]string, error) {
	local := LocalDirSource{Dir: s.Dir}
	if files, err := local.Files(); err == nil && len(files) > 0 {
		return files, nil
	}
	return MinioSource{Objects: s.Objects, Prefix: s.Prefix}.Files()
}

// Open 实现 FileSource：本地优先，失败回源 MinIO。
func (s ArtifactSource) Open(rel string) (io.ReadCloser, error) {
	local := LocalDirSource{Dir: s.Dir}
	if rc, err := local.Open(rel); err == nil {
		return rc, nil
	}
	return MinioSource{Objects: s.Objects, Prefix: s.Prefix}.Open(rel)
}
