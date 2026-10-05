// wfs_handlers.go — WFS 2.0 简单子集 API（M2-F14）。
// KVP 入口挂在 tile 组（鉴权语义同 vector MVT：API Key 或签名 URL）；
// 异常统一输出 OGC（ows 1.1）ExceptionReport XML。
package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/vector"
)

// WFSKvp GET /api/v1/wfs — KVP encoding 入口（GetCapabilities | GetFeature）。
func (h *Handler) WFSKvp(c *gin.Context) {
	switch strings.ToUpper(kvp(c, "request")) {
	case "GETCAPABILITIES":
		h.wfsCapabilities(c)
	case "GETFEATURE":
		h.wfsGetFeature(c)
	case "":
		wfsException(c, 400, "MissingParameterValue", "request",
			"missing request parameter (GetCapabilities | GetFeature)")
	default:
		wfsException(c, 400, "InvalidParameterValue", "request",
			"unsupported request: "+kvp(c, "request"))
	}
}

// wfsCapabilities GetCapabilities：从已注册图层真实生成 XML。
func (h *Handler) wfsCapabilities(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	base := requestBaseURL(c)
	xml, err := h.Vector.CapabilitiesXML(c.Request.Context(), base)
	if err != nil {
		wfsException(c, 502, "NoApplicableCode", "", err.Error())
		return
	}
	c.Header("Content-Type", "application/xml; charset=UTF-8")
	c.String(http.StatusOK, xml)
}

// wfsGetFeature GetFeature：typeName/bbox/count → GeoJSON FeatureCollection。
// bbox 按 WFS 2.0 约定取 CRS84（经度、纬度序）：minx,miny,maxx,maxy。
// count 收敛到 TANGIS_WFS_MAX_FEATURES（默认 10000）；缓存同 vector 语义。
func (h *Handler) wfsGetFeature(c *gin.Context) {
	if h.Vector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vector service not configured"})
		return
	}
	if v := kvp(c, "version"); v != "" && v != "2.0.0" {
		wfsException(c, 400, "InvalidParameterValue", "version", "unsupported version: "+v+" (only 2.0.0)")
		return
	}
	typeName := kvp(c, "typename")
	if typeName == "" {
		wfsException(c, 400, "MissingParameterValue", "typename", "missing typename parameter")
		return
	}
	var minx, miny, maxx, maxy float64
	var hasBBox bool
	var err error
	if b := kvp(c, "bbox"); b != "" {
		hasBBox = true
		parts := strings.Split(b, ",")
		if len(parts) != 4 {
			wfsException(c, 400, "InvalidParameterValue", "bbox",
				"bbox must be minx,miny,maxx,maxy (CRS84)")
			return
		}
		vals := make([]float64, 4)
		for i, p := range parts {
			if vals[i], err = strconv.ParseFloat(strings.TrimSpace(p), 64); err != nil {
				wfsException(c, 400, "InvalidParameterValue", "bbox", "bbox value "+p+" is not numeric")
				return
			}
		}
		minx, miny, maxx, maxy = vals[0], vals[1], vals[2], vals[3]
	}
	count := 0
	if v := kvp(c, "count"); v != "" {
		if count, err = strconv.Atoi(v); err != nil || count < 0 {
			wfsException(c, 400, "InvalidParameterValue", "count", "count must be a non-negative integer")
			return
		}
	}
	fc, n, err := h.Vector.Features(c.Request.Context(), typeName, hasBBox, minx, miny, maxx, maxy, count)
	if err != nil {
		switch {
		case errors.Is(err, vector.ErrInvalid):
			wfsException(c, 400, "InvalidParameterValue", "typename/bbox", err.Error())
		case errors.Is(err, vector.ErrNotFound):
			wfsException(c, 404, "NotFound", "typename", "unknown feature type: "+typeName)
		default:
			wfsException(c, 502, "NoApplicableCode", "", err.Error())
		}
		return
	}
	c.Header("Content-Type", "application/geo+json; charset=UTF-8")
	if n < 0 {
		c.Header("X-Cache", "HIT") // 缓存命中（要素数未知）
	} else {
		c.Header("X-Cache", "MISS")
		c.Header("X-Tangis-Feature-Count", strconv.Itoa(n))
	}
	c.Data(http.StatusOK, "application/geo+json; charset=UTF-8", fc)
}

// requestBaseURL 从请求还原对外基地址（scheme://host），
// 反代场景取 X-Forwarded-Proto（与 service.baseURL 同语义）。
func requestBaseURL(c *gin.Context) string {
	scheme := c.GetHeader("X-Forwarded-Proto")
	if scheme == "" {
		if c.Request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + c.Request.Host
}

// wfsException 输出 OGC ExceptionReport 并终止请求链。
func wfsException(c *gin.Context, status int, code, locator, text string) {
	st, body := vector.ExceptionReport(status, code, locator, text)
	c.Header("Content-Type", "application/xml; charset=UTF-8")
	c.String(st, body)
}
