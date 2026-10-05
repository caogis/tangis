// fs.go — 服务端本机目录浏览（切片转换页「选择路径」弹窗的数据源）。
//
// 为什么需要：建任务的 source / output 都是**服务端本机路径**，手填易错
// （OSGB 目录层级深、波浪号与相对路径、大小写）。本接口只列目录名与文件名，
// 不返回任何文件内容，也不做写操作；写操作只有 mkdir（建输出目录）。
//
// 路由（均需 admin key；TANGIS_AUTH=off 时按 admin 放行）：
//
//	GET  /api/v1/fs/browse?path=&kind=dir|file|any&ext=.tif,.tiff
//	POST /api/v1/fs/mkdir  {path, name}
//
// 安全边界（默认「能用」，生产可收口）：
//   - 开关：AuthOptions.FSBrowseEnabled（env TANGIS_FS_BROWSE=off 关闭），
//     关闭时两个端点均 404，前端按 capabilities.fs_browse 隐藏入口；
//   - 白名单：AuthOptions.FSBrowseRoots（env TANGIS_FS_ROOTS 逗号分隔），
//     非空时只允许浏览这些根目录之下的路径，越界 403。
package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
)

// fsMaxEntries 单次列举上限（超大目录不拖慢接口，前端按 truncated 提示）。
const fsMaxEntries = 2000

// errPathOutsideRoots 目标路径不在 FSBrowseRoots 白名单之内。
var errPathOutsideRoots = errors.New("path outside allowed roots (TANGIS_FS_ROOTS)")

// fsEntry 目录项。Path 为可直接回填表单的绝对路径。
type fsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// fsBrowseResp GET /api/v1/fs/browse 响应。
type fsBrowseResp struct {
	// Path 规范化后的当前目录；根层（盘符/根列表）为空串。
	Path string `json:"path"`
	// Parent 上一级目录；已在顶层时为空串。
	Parent string `json:"parent"`
	// Writable 当前目录是否可写（输出目录选择时提示用）。
	Writable bool `json:"writable"`
	// Truncated 条目数达到 fsMaxEntries 被截断。
	Truncated bool `json:"truncated"`
	// Roots 非空时为白名单根列表（此时前端不应再往上跳）。
	Roots []string `json:"roots"`
	// Os 服务端系统（darwin/linux/windows），前端据此提示路径分隔符。
	Os      string    `json:"os"`
	Entries []fsEntry `json:"entries"`
}

// BrowseFS GET /api/v1/fs/browse — 列举服务端本机目录。
//
// 查询参数：
//   - path：目录绝对路径；空/缺省返回顶层（配置了白名单时为白名单根，
//     Windows 为盘符列表，其它为 "/"）；
//   - kind：dir（默认，只列目录）｜file（目录 + 文件）｜any（不过滤）；
//   - ext：逗号分隔的后缀（.tif,.tiff），仅 kind=file 时用于过滤文件。
func (h *Handler) BrowseFS(c *gin.Context) {
	if !h.FSBrowseEnabled {
		c.JSON(http.StatusNotFound, gin.H{"error": "fs browse disabled"})
		return
	}
	if !isAuthOff(c) && c.GetString("role") != auth.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin key required"})
		return
	}

	kind := strings.ToLower(strings.TrimSpace(c.Query("kind")))
	if kind == "" {
		kind = "dir"
	}
	if kind != "dir" && kind != "file" && kind != "any" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be dir, file or any"})
		return
	}

	resp := fsBrowseResp{Os: runtime.GOOS, Roots: h.fsRoots()}
	raw := strings.TrimSpace(c.Query("path"))
	if raw == "" {
		resp.Entries, resp.Truncated = h.fsTopEntries()
		c.JSON(http.StatusOK, resp)
		return
	}

	dir, err := h.resolveFSPath(raw)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}
	st, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "path not found: " + dir})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !st.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is not a directory: " + dir})
		return
	}

	entries, truncated := listDir(dir, kind, splitExts(c.Query("ext")))
	resp.Path = dir
	resp.Parent = h.fsParent(dir)
	resp.Writable = dirWritable(dir)
	resp.Entries = entries
	resp.Truncated = truncated
	c.JSON(http.StatusOK, resp)
}

// fsMkdirReq POST /api/v1/fs/mkdir 请求体。
type fsMkdirReq struct {
	Path string `json:"path"` // 父目录（绝对路径）
	Name string `json:"name"` // 待建子目录名（不允许分隔符与 ..）
}

// MkdirFS POST /api/v1/fs/mkdir — 在白名单内新建目录（输出目录不存在时免手建）。
func (h *Handler) MkdirFS(c *gin.Context) {
	if !h.FSBrowseEnabled {
		c.JSON(http.StatusNotFound, gin.H{"error": "fs browse disabled"})
		return
	}
	if !isAuthOff(c) && c.GetString("role") != auth.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin key required"})
		return
	}
	var req fsMkdirReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	// 目录名不得含分隔符或跳转（只建一层，避免 ../../ 逃逸）
	if strings.ContainsAny(name, `/\\`) || name == "." || name == ".." {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be a single directory name"})
		return
	}
	parent, err := h.resolveFSPath(strings.TrimSpace(req.Path))
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}
	target := filepath.Join(parent, name)
	if err := os.Mkdir(target, 0o755); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"path": target})
}

// fsRoots 白名单根（未配置时为 nil）。
func (h *Handler) fsRoots() []string {
	if len(h.FSBrowseRoots) == 0 {
		return nil
	}
	out := make([]string, 0, len(h.FSBrowseRoots))
	for _, r := range h.FSBrowseRoots {
		r = filepath.Clean(strings.TrimSpace(r))
		if r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveFSPath 规范化并校验路径：相对路径按进程 cwd 取绝对；
// 配置白名单时越界返回错误。
func (h *Handler) resolveFSPath(raw string) (string, error) {
	p := filepath.Clean(raw)
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	roots := h.fsRoots()
	if len(roots) == 0 {
		return p, nil
	}
	sep := string(os.PathSeparator)
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+sep) {
			return p, nil
		}
	}
	return "", errPathOutsideRoots
}

// fsParent 上一级目录；已在根或白名单根时返回空串。
func (h *Handler) fsParent(dir string) string {
	parent := filepath.Dir(dir)
	if parent == dir {
		return ""
	}
	sep := string(os.PathSeparator)
	for _, r := range h.fsRoots() {
		if dir == r {
			return ""
		}
		// 白名单根本身带分隔符结尾时（如 /data/）单独比对
		if strings.TrimSuffix(r, sep) == strings.TrimSuffix(dir, sep) {
			return ""
		}
	}
	return parent
}

// fsTopEntries 顶层条目：白名单根 > Windows 盘符 > "/"。
func (h *Handler) fsTopEntries() ([]fsEntry, bool) {
	if roots := h.fsRoots(); len(roots) > 0 {
		entries := make([]fsEntry, 0, len(roots))
		for _, r := range roots {
			entries = append(entries, fsEntry{Name: r, Path: r, IsDir: true})
		}
		return entries, false
	}
	if runtime.GOOS == "windows" {
		entries := []fsEntry{}
		for b := 'A'; b <= 'Z'; b++ {
			root := string(b) + `:\`
			if st, err := os.Stat(root); err == nil && st.IsDir() {
				entries = append(entries, fsEntry{Name: root, Path: root, IsDir: true})
			}
		}
		return entries, false
	}
	return []fsEntry{{Name: "/", Path: "/", IsDir: true}}, false
}

// listDir 读取目录并按 kind / 后缀过滤：目录优先，名称不区分大小写升序。
func listDir(dir string, kind string, exts []string) ([]fsEntry, bool) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return []fsEntry{}, false
	}
	entries := make([]fsEntry, 0, len(items))
	truncated := false
	for _, it := range items {
		name := it.Name()
		// 隐藏项（以 . 开头）默认不展示：macOS/Linux 下 .ssh/.git 等噪音远多于用途
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, ierr := it.Info()
		if ierr != nil {
			continue
		}
		if info.IsDir() {
			entries = append(entries, fsEntry{Name: name, Path: filepath.Join(dir, name), IsDir: true})
			continue
		}
		if kind == "dir" {
			continue
		}
		if kind == "file" && len(exts) > 0 && !hasExt(name, exts) {
			continue
		}
		entries = append(entries, fsEntry{
			Name:  name,
			Path:  filepath.Join(dir, name),
			IsDir: false,
			Size:  info.Size(),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	if len(entries) > fsMaxEntries {
		entries = entries[:fsMaxEntries]
		truncated = true
	}
	return entries, truncated
}

// splitExts 解析逗号分隔后缀，统一小写并补前导点（".tif,.TIF" → [".tif"]）。
func splitExts(raw string) []string {
	out := []string{}
	for _, s := range strings.Split(raw, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, ".") {
			s = "." + s
		}
		out = append(out, s)
	}
	return out
}

func hasExt(name string, exts []string) bool {
	lower := strings.ToLower(name)
	for _, e := range exts {
		if strings.HasSuffix(lower, e) {
			return true
		}
	}
	return false
}

// dirWritable 目录是否可写（尝试创建临时文件，代价低且比位运算跨平台可靠）。
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".tangis-fs-")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}
