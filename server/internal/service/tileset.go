// tileset.go 实现 3DTiles JSON 产物的防盗链签名动态改写（F-06 子瓦片签名）。
//
// 背景：/api/v1/services 仅对 tileset.json 入口 URL 签名（signedTilesetURL）；
// Cesium 加载解析后会按 content.uri 的相对路径（不带 sig/expires query）
// 请求子瓦片与嵌套 tileset，直接命中 403。故 ServeTile 在鉴权开启时对
// *.json 产物动态改写：递归遍历 root/children，对每个 content.uri 按其自身
// 分发 path 用 SignURL（同一密钥 TANGIS_SIGN_SECRET）重签后拼回 query。
// b3dm 等非 JSON 资源维持原样验签，不做改写。
//
// 缓存注意：改写结果依赖签发时刻的 expires，因此该类响应不走 Redis 缓存
// （ServeTile 现状本就不缓存产物），并显式设置 Cache-Control: no-store，
// 防止浏览器/中间层缓存持有过期签名。
package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/auth"
	"tangis/server/internal/task"
)

// tilesetJSONMaxBytes 参与 JSON 改写的产物大小上限（防大文件整读内存）。
const tilesetJSONMaxBytes = 64 << 20

// rewriteEnabled 鉴权开启且配置了签名密钥时才需要改写
// （TANGIS_AUTH=off 跳过，URL 保持干净相对路径）。
func (s *Server) rewriteEnabled() bool {
	return s.AuthEnabled && s.SignSecret != ""
}

// serveSignedTilesetJSON 读取 JSON 产物、改写 content.uri 后输出。
// 读取失败或解析非合法 JSON（含非 tileset 结构的其它 .json 产物）返回
// false，调用方按普通文件走原样分发流程。
func (s *Server) serveSignedTilesetJSON(c *gin.Context, t *task.Task, rel string) bool {
	data, ok := s.readArtifactBytes(c.Request.Context(), t, rel, tilesetJSONMaxBytes)
	if !ok {
		return false
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return false
	}
	s.signTilesetUris(doc, path.Dir(rel), t.ID)
	out, err := json.Marshal(doc)
	if err != nil {
		return false
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json", out)
	return true
}

// readArtifactBytes 读产物完整字节：本地 output 优先，MinIO 回源；
// 超过 maxBytes 视为不可改写（返回 false 走原样分发）。
func (s *Server) readArtifactBytes(ctx context.Context, t *task.Task, rel string, maxBytes int64) ([]byte, bool) {
	if data, err := os.ReadFile(filepath.Join(t.Output, filepath.FromSlash(rel))); err == nil {
		if int64(len(data)) > maxBytes {
			return nil, false
		}
		return data, true
	}
	if s.Objects != nil && t.MinioPrefix != "" {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		rc, size, err := s.Objects.Get(ctx, path.Join(t.MinioPrefix, rel))
		if err != nil {
			return nil, false
		}
		defer rc.Close()
		if size < 0 || size > maxBytes {
			return nil, false
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(rc, buf); err != nil {
			return nil, false
		}
		return buf, true
	}
	return nil, false
}

// signTilesetUris 递归遍历 tileset 文档节点，改写沿途每个 content.uri；
// dir 为当前 tileset.json 所在产物目录（相对 uri 的解析基准）。
func (s *Server) signTilesetUris(node any, dir, taskID string) {
	switch v := node.(type) {
	case map[string]any:
		if content, ok := v["content"]; ok {
			v["content"] = s.signContentValue(content, dir, taskID)
		}
		if root, ok := v["root"]; ok {
			s.signTilesetUris(root, dir, taskID)
		}
		if children, ok := v["children"].([]any); ok {
			for _, ch := range children {
				s.signTilesetUris(ch, dir, taskID)
			}
		}
	case []any:
		for _, item := range v {
			s.signTilesetUris(item, dir, taskID)
		}
	}
}

// signContentValue 改写 content 字段（3D Tiles 1.0/1.1 三种形态）：
//   - string：content 直接是 uri（1.1）；
//   - object：{uri: "..."}；
//   - array：多 content，元素为 string 或 {uri}。
func (s *Server) signContentValue(content any, dir, taskID string) any {
	switch v := content.(type) {
	case string:
		return s.signContentURI(v, dir, taskID)
	case map[string]any:
		if u, ok := v["uri"].(string); ok {
			v["uri"] = s.signContentURI(u, dir, taskID)
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = s.signContentValue(item, dir, taskID)
		}
		return v
	default:
		return content
	}
}

// signContentURI 对单个 content.uri 追加防盗链签名 query。
// 签名覆盖该资源自身的分发 path（与 ServeTile 验签口径一致，即
// c.Request.URL.Path）；外部地址（绝对 URL / 协议相对 / data:）与已带
// 签名的 uri 原样返回。
func (s *Server) signContentURI(uri, dir, taskID string) string {
	if uri == "" || isExternalURI(uri) || strings.Contains(uri, "sig=") {
		return uri
	}
	rel := path.Clean(path.Join(dir, uri))
	if strings.HasPrefix(rel, "/") || rel == ".." || strings.HasPrefix(rel, "../") {
		return uri // 越界引用不签（服务端 safeRelPath 本就会拒绝）
	}
	p := "/services/" + taskID + "/" + rel
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	return uri + sep + auth.SignURL(p, time.Now().Add(s.signTTL()), s.SignSecret)
}

// isExternalURI 判断是否外部引用（不做改写）：
// 带 scheme 的绝对 URL（http://、s3:// 等）、协议相对 //、data: URI。
func isExternalURI(uri string) bool {
	if strings.HasPrefix(uri, "data:") || strings.HasPrefix(uri, "//") {
		return true
	}
	i := strings.Index(uri, "://")
	return i > 0 && !strings.ContainsAny(uri[:i], "/?#")
}
