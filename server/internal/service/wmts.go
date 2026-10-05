// wmts.go 实现 OGC WMTS 1.0.0（KVP + RESTful 两种 encoding）与 TMS 瓦片分发（F-03）。
//
// 路由约定（挂在 /api/v1 之下，鉴权语义：API Key 或防盗链签名任一即可）：
//
//	KVP     GET /api/v1/wmts?service=WMTS&version=1.0.0&request=GetCapabilities
//	KVP     GET /api/v1/wmts?...&request=GetTile&layer={task}&tilematrixset=WebMercatorQuad
//	              &tilematrix={z}&tilerow={y}&tilecol={x}&format=image/png
//	RESTful GET /api/v1/wmts/1.0.0/{layer}/{style}/{tilematrixset}/{z}/{row}/{col}.{ext}
//	TMS     GET /api/v1/tms/{task_id}/{z}/{x}/{y}.{ext}   （Y 原点在左下，标准 TMS）
//
// GetCapabilities 完全由任务产物 metadata.json 生成（真实数据原则）：
// TileMatrixSet、BoundingBox、ResourceURL、TileMatrixSetLimits 均来自真实金字塔，
// 无元数据的任务不产生 Layer。
package service

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// WMTS KVP 参数取值约定。
const (
	wmtsVersion = "1.0.0"
	wmtsStyle   = "default"
)

// WMTSKvp GET /api/v1/wmts — KVP encoding 入口（GetCapabilities / GetTile）。
func (s *Server) WMTSKvp(c *gin.Context) {
	switch strings.ToUpper(kvp(c, "request")) {
	case "GETCAPABILITIES":
		s.wmtsCapabilities(c)
	case "GETTILE":
		s.wmtsKvpTile(c)
	case "":
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing request parameter (GetCapabilities | GetTile)"})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported request: " + kvp(c, "request")})
	}
}

// wmtsKvpTile KVP GetTile：参数映射到 servePyramidTile（KVP tilerow 为左上原点行号）。
func (s *Server) wmtsKvpTile(c *gin.Context) {
	if v := kvp(c, "version"); v != "" && v != wmtsVersion {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version: " + v})
		return
	}
	layer := kvp(c, "layer")
	if layer == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing layer"})
		return
	}
	format := strings.TrimPrefix(strings.ToLower(kvp(c, "format")), "image/")
	if format == "jpeg" {
		format = "jpg"
	}
	z, ok1 := kvpInt(c, "tilematrix")
	y, ok2 := kvpInt(c, "tilerow")
	x, ok3 := kvpInt(c, "tilecol")
	if !ok1 || !ok2 || !ok3 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tilematrix/tilerow/tilecol must be integers"})
		return
	}
	s.servePyramidTile(c, layer, kvp(c, "tilematrixset"), z, x, y, format, false)
}

// WMTSRestTile GET /api/v1/wmts/1.0.0/{layer}/{style}/{tms}/{z}/{row}/{col}.{ext}
func (s *Server) WMTSRestTile(c *gin.Context) {
	if p := c.Param("style"); p != wmtsStyle {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported style: " + p + " (only 'default')"})
		return
	}
	base := c.Param("col") // 形如 "12.png"
	dot := strings.LastIndexByte(base, '.')
	if dot < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing tile format extension"})
		return
	}
	ext, colStr := base[dot+1:], base[:dot]
	z, e1 := strconv.Atoi(c.Param("z"))
	y, e2 := strconv.Atoi(c.Param("row")) // RESTful TileRow（左上原点）
	x, e3 := strconv.Atoi(colStr)
	if e1 != nil || e2 != nil || e3 != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tile coordinates"})
		return
	}
	s.servePyramidTile(c, c.Param("layer"), c.Param("tms"), z, x, y, ext, false)
}

// TMSTile GET /api/v1/tms/{task_id}/{z}/{x}/{y}.{ext} — 标准 TMS（Y 原点左下）。
func (s *Server) TMSTile(c *gin.Context) {
	base := c.Param("y") // 形如 "5.png"
	dot := strings.LastIndexByte(base, '.')
	if dot < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing tile format extension"})
		return
	}
	ext, yStr := base[dot+1:], base[:dot]
	z, e1 := strconv.Atoi(c.Param("z"))
	x, e2 := strconv.Atoi(c.Param("x"))
	y, e3 := strconv.Atoi(yStr)
	if e1 != nil || e2 != nil || e3 != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tile coordinates"})
		return
	}
	// TMS 规范里瓦片集合与金字塔 TileMatrixSet 对应，这里取任务元数据声明的 TMS
	s.servePyramidTile(c, c.Param("task_id"), "", z, x, y, ext, true)
}

// ---- GetCapabilities 生成（真实数据） ----

// wmtsCapabilities 输出 GetCapabilities XML。
// scope 取自鉴权中间件：携带 API Key 时按租户过滤 Layer；
// 仅签名访问（或鉴权关闭）时展示全部已发布影像服务。
func (s *Server) wmtsCapabilities(c *gin.Context) {
	if s == nil || s.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}
	layers := s.publishedImageTasks(c.GetString(ScopeTenantKey))

	base := baseURL(c)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Capabilities xmlns="http://www.opengis.net/wmts/1.0" xmlns:ows="http://www.opengis.net/ows/1.1" xmlns:xlink="http://www.w3.org/1999/xlink" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="http://www.opengis.net/wmts/1.0 http://schemas.opengis.net/wmts/1.0/wmtsGetCapabilities_response.xsd" version="1.0.0">` + "\n")

	b.WriteString("  <ows:ServiceIdentification>\n")
	b.WriteString("    <ows:Title>TanGIS WMTS Service</ows:Title>\n")
	b.WriteString("    <ows:ServiceType>OGC WMTS</ows:ServiceType>\n")
	b.WriteString("    <ows:ServiceTypeVersion>1.0.0</ows:ServiceTypeVersion>\n")
	b.WriteString("  </ows:ServiceIdentification>\n")

	kvpURL := base + "/api/v1/wmts?"
	restBase := base + "/api/v1/wmts/1.0.0"
	b.WriteString("  <ows:OperationsMetadata>\n")
	writeOperation(&b, "GetCapabilities", kvpURL, restBase)
	writeOperation(&b, "GetTile", kvpURL, restBase)
	b.WriteString("  </ows:OperationsMetadata>\n")

	b.WriteString("  <Contents>\n")
	usedTMS := map[string]bool{}
	for _, l := range layers {
		writeLayer(&b, base, l)
		usedTMS[l.Meta.TileMatrixSet] = true
	}
	// TileMatrixSet 定义按名称序输出，保证 XML 稳定
	names := make([]string, 0, len(usedTMS))
	for name := range usedTMS {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeTileMatrixSet(&b, name)
	}
	b.WriteString("  </Contents>\n")

	b.WriteString("  <ServiceMetadataURL xlink:href=\"" + xmlEscape(kvpURL+"service=WMTS&version="+wmtsVersion+"&request=GetCapabilities") + "\"/>\n")
	b.WriteString("</Capabilities>\n")

	c.Header("Content-Type", "application/xml; charset=UTF-8")
	c.String(http.StatusOK, b.String())
}

// baseURL 从请求还原对外基地址（scheme://host），反代场景取 X-Forwarded-Proto。
func baseURL(c *gin.Context) string {
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

// layerResourceURL RESTful GetTile 模板（OGC ResourceURL 语义）。
func layerResourceURL(base string, l *layerInfo) string {
	return fmt.Sprintf("%s/api/v1/wmts/1.0.0/%s/%s/%s/{TileMatrix}/{TileRow}/{TileCol}.%s",
		base, l.Task.ID, wmtsStyle, l.Meta.TileMatrixSet, canonicalFormat(l.Meta.Format))
}

// writeOperation 输出单个 ows:Operation（同时声明 KVP 与 RESTful encoding）。
func writeOperation(b *strings.Builder, name, kvpURL, restBase string) {
	fmt.Fprintf(b, "    <ows:Operation name=\"%s\">\n      <ows:DCP>\n        <ows:HTTP>\n", xmlEscape(name))
	fmt.Fprintf(b, "          <ows:Get xlink:href=\"%s\">\n            <ows:Constraint name=\"GetEncoding\">\n              <ows:AllowedValues><ows:Value>KVP</ows:Value></ows:AllowedValues>\n            </ows:Constraint>\n          </ows:Get>\n", xmlEscape(kvpURL))
	fmt.Fprintf(b, "          <ows:Get xlink:href=\"%s\">\n            <ows:Constraint name=\"GetEncoding\">\n              <ows:AllowedValues><ows:Value>RESTful</ows:Value></ows:AllowedValues>\n            </ows:Constraint>\n          </ows:Get>\n", xmlEscape(restBase+"/"))
	b.WriteString("        </ows:HTTP>\n      </ows:DCP>\n    </ows:Operation>\n")
}

// writeLayer 输出单个 Layer：标题/标识/格式/TMS 链接/ResourceURL/层级限制，
// 全部字段来自任务与真实 metadata.json。
func writeLayer(b *strings.Builder, base string, l *layerInfo) {
	def := tileMatrixSets[l.Meta.TileMatrixSet]
	format := canonicalFormat(l.Meta.Format)
	fmt.Fprintf(b, "    <Layer>\n")
	fmt.Fprintf(b, "      <ows:Title>%s</ows:Title>\n", xmlEscape("Task "+l.Task.ID))
	fmt.Fprintf(b, "      <ows:Identifier>%s</ows:Identifier>\n", xmlEscape(l.Task.ID))
	b.WriteString("      <Style isDefault=\"true\"><ows:Identifier>default</ows:Identifier></Style>\n")
	fmt.Fprintf(b, "      <Format>image/%s</Format>\n", format)
	// WGS84BoundingBox：由元数据 extent 经真实 CRS 变换得到
	lonMin, latMin := def.toWGS84(l.Meta.Extent[0], l.Meta.Extent[1])
	lonMax, latMax := def.toWGS84(l.Meta.Extent[2], l.Meta.Extent[3])
	fmt.Fprintf(b, "      <ows:WGS84BoundingBox crs=\"urn:ogc:def:crs:OGC:1.3:CRS84\">\n        <ows:LowerCorner>%s</ows:LowerCorner>\n        <ows:UpperCorner>%s</ows:UpperCorner>\n      </ows:WGS84BoundingBox>\n",
		xmlEscape(fmtCoord(lonMin, latMin)), xmlEscape(fmtCoord(lonMax, latMax)))
	fmt.Fprintf(b, "      <TileMatrixSetLink><TileMatrixSet>%s</TileMatrixSet></TileMatrixSetLink>\n", xmlEscape(l.Meta.TileMatrixSet))
	b.WriteString("      <TileMatrixSetLimits>\n")
	for z := l.Meta.MinZoom; z <= l.Meta.MaxZoom; z++ {
		mw, mh := def.Matrix(z)
		fmt.Fprintf(b, "        <TileMatrixLimits><TileMatrix>%s</TileMatrix><MinTileRow>0</MinTileRow><MaxTileRow>%d</MaxTileRow><MinTileCol>0</MinTileCol><MaxTileCol>%d</MaxTileCol></TileMatrixLimits>\n",
			xmlEscape(strconv.Itoa(z)), mh-1, mw-1)
	}
	b.WriteString("      </TileMatrixSetLimits>\n")
	fmt.Fprintf(b, "      <ResourceURL format=\"image/%s\" resourceType=\"tile\" template=\"%s\"/>\n", format, xmlEscape(layerResourceURL(base, l)))
	b.WriteString("    </Layer>\n")
}

// writeTileMatrixSet 输出内置 TileMatrixSet 的完整层级定义（0..24）。
func writeTileMatrixSet(b *strings.Builder, name string) {
	def := tileMatrixSets[name]
	fmt.Fprintf(b, "    <TileMatrixSet>\n      <ows:Identifier>%s</ows:Identifier>\n", xmlEscape(name))
	fmt.Fprintf(b, "      <ows:SupportedCRS>%s</ows:SupportedCRS>\n", xmlEscape(def.URN))
	fmt.Fprintf(b, "      <WellKnownScaleSet>urn:ogc:def:wkss:OGC:1.0:%s</WellKnownScaleSet>\n", xmlEscape(name))
	for z := 0; z <= 24; z++ {
		mw, mh := def.Matrix(z)
		denom := def.BaseScale / float64(int(1)<<z)
		fmt.Fprintf(b, "      <TileMatrix>\n        <ows:Identifier>%d</ows:Identifier>\n        <ScaleDenominator>%s</ScaleDenominator>\n        <TopLeftCorner>%s</TopLeftCorner>\n        <TileWidth>256</TileWidth><TileHeight>256</TileHeight>\n        <MatrixWidth>%d</MatrixWidth><MatrixHeight>%d</MatrixHeight>\n      </TileMatrix>\n",
			z, strconv.FormatFloat(denom, 'f', -1, 64),
			xmlEscape(fmtCoord(def.TopLeft[0], def.TopLeft[1])), mw, mh)
	}
	b.WriteString("    </TileMatrixSet>\n")
}

// fmtCoord 坐标对输出（OGC 空格分隔）。
func fmtCoord(a, b float64) string {
	return strconv.FormatFloat(a, 'f', -1, 64) + " " + strconv.FormatFloat(b, 'f', -1, 64)
}

// xmlEscape XML 属性/文本转义。
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
