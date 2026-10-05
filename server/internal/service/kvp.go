// kvp.go OGC 系列服务（WMTS/WMS/WCS）的 KVP 参数取值。
//
// 为什么不能直接用 gin 的 c.Query：
//
//	OGC 规范明确「KVP 参数名不区分大小写」——WMS 1.3.0 §6.2、WMTS 1.0.0 §6.2.1。
//	而 gin 的 c.Query 走 url.Values 的精确匹配，于是规范里最常写的
//	`SERVICE=WMS&REQUEST=GetCapabilities` 会被判成「缺 request 参数」，
//	QGIS / ArcGIS 等客户端一律用大写或混合大小写，实测必挂。
//
// 本文件提供大小写不敏感的取值器；对 VMTS/WMS/WCS 三套入口统一使用。
package service

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// kvp 大小写不敏感地取 KVP 参数（先精确命中，再回退到忽略大小写扫描）。
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

// kvpDefault 同 kvp，缺省时返回 def。
func kvpDefault(c *gin.Context, name, def string) string {
	if v := kvp(c, name); v != "" {
		return v
	}
	return def
}

// kvpInt 取整数参数：缺失或非法都返回 (0,false)，由调用方给出规范错误。
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
