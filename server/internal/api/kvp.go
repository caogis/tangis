// kvp.go api 层的 KVP 参数取值（WFS 等 OGC 入口使用）。
//
// OGC 规范中 KVP 参数名**不区分大小写**（WFS 2.0 §7.5、WMS 1.3.0 §6.2），
// 而 gin 的 c.Query 走 url.Values 精确匹配 —— 于是 `typeName=`、`TYPENAME=`
// 这类客户端常见写法都会取不到值，被判成「缺参数」。本文件统一提供
// 忽略大小写的取值器，避免每个 handler 各自踩坑。
package api

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// kvp 大小写不敏感地取 KVP 参数。
func kvp(c *gin.Context, name string) string {
	if v := c.Query(name); v != "" {
		return v
	}
	for k, vs := range c.Request.URL.Query() {
		if len(vs) > 0 && strings.EqualFold(k, name) {
			return vs[0]
		}
	}
	return ""
}

// kvpInt 取整数参数：缺失或非法都返回 (0,false)。
func kvpInt(c *gin.Context, name string) (int, bool) {
	v := strings.TrimSpace(kvp(c, name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
