// Package webui 把前端构建产物（默认 web/dist 复制到本包 dist/ 目录）嵌入
// apiserver 二进制，使桌面单机版无需额外的静态文件托管即可提供 Web 控制台。
//
// 用 `make ui`（或 scripts）在构建前刷新 dist/：
//
//	rm -rf server/internal/webui/dist && cp -R web/dist server/internal/webui/dist
package webui

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// placeholderMarker dist/index.html 中的占位标记：dist 未被真实构建产物覆盖时，
// Handler 仍能工作但会在启动日志提示。
const placeholderMarker = "TanGIS Web UI placeholder"

//go:embed all:dist
var assets embed.FS

// Handler 返回 SPA 静态资源 handler：
//   - 命中真实静态文件则直出（带 http.FileServer 的缓存/Range 支持）；
//   - 未命中且路径无扩展名时回落到 index.html（支持 vue-router history 模式）；
//   - 未命中且带扩展名（如缺失的 .js/.css）返回 404，避免掩盖资源错误。
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		return nil, err
	}
	hfs := http.FS(sub)
	return &spa{fs: hfs, files: http.FileServer(hfs)}, nil
}

// Stub 报告嵌入的是否为占位 dist（前端尚未构建/复制）。
func Stub() bool {
	b, err := assets.ReadFile("dist/index.html")
	if err != nil {
		return true
	}
	return strings.Contains(string(b), placeholderMarker)
}

type spa struct {
	fs    http.FileSystem
	files http.Handler
}

func (s *spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.files.ServeHTTP(w, r)
		return
	}
	clean := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/")), "/")
	if exists(s.fs, clean) {
		s.files.ServeHTTP(w, r)
		return
	}
	// 无扩展名视为前端路由 → SPA 回落
	if path.Ext(clean) == "" {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		s.files.ServeHTTP(w, r2)
		return
	}
	http.NotFound(w, r)
}

func exists(fsys http.FileSystem, name string) bool {
	if name == "" {
		return false
	}
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return !st.IsDir()
}

// ErrNoUI dist 缺失时的显式错误（正常构建下不会发生）。
var ErrNoUI = errors.New("webui: embedded dist not found")
