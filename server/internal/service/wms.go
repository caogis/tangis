// wms.go 实现 OGC WMS 1.3.0 / 1.1.1（服务分发补齐）。
//
// 路由（挂在 /api/v1 下，鉴权语义同 WMTS/TMS：API Key 或防盗链签名任一）：
//
//	KVP GET /api/v1/wms?service=WMS&request=GetCapabilities
//	KVP GET /api/v1/wms?service=WMS&request=GetMap&layers={task}&bbox=...&width=&height=
//	        &crs=EPSG:3857&format=image/png[&transparent=true][&version=1.3.0]
//
// 数据来源：任务产物影像金字塔（tiles/metadata.json + {z}/{x}/{y}.png），
// 与 WMTS/TMS 同一份瓦片——WMS 只是把「一次一个瓦片」变成「一次一张图」。
//
// 真实数据原则：无 2D 产物的任务不产生 Layer；瓦片缺失留透明并在
// X-Tangis-Missing-Tiles 头报数，不返回占位图。
//
// 轴序：WMS 1.3.0 规定 EPSG:4326 的 bbox 顺序是 lat,lon（1.1.1 与 CRS:84 为 lon,lat），
// 这是 WMS 集成中最常见的踩坑点，此处按 version 严格区分。
package service

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/task"
)

// WMS 单边像素上限（与 raster.go 的制图上限一致）。
const wmsMaxSide = maxMosaicPixels

// WMSKvp GET /api/v1/wms — KVP 入口（GetCapabilities / GetMap）。
func (s *Server) WMSKvp(c *gin.Context) {
	switch strings.ToUpper(strings.TrimSpace(kvp(c, "request"))) {
	case "GETCAPABILITIES":
		s.wmsCapabilities(c)
	case "GETMAP":
		s.wmsGetMap(c)
	case "":
		s.wmsException(c, http.StatusBadRequest, "MissingParameter",
			"missing request parameter (GetCapabilities | GetMap)")
	default:
		s.wmsException(c, http.StatusBadRequest, "OperationNotSupported",
			"unsupported request: "+kvp(c, "request"))
	}
}

// wmsException 输出 WMS ServiceExceptionReport（规范规定的错误载体）。
func (s *Server) wmsException(c *gin.Context, status int, code, msg string) {
	c.Header("Content-Type", "application/vnd.ogc.se_xml; charset=UTF-8")
	c.String(status, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
		`<ServiceExceptionReport version="1.3.0" xmlns="http://www.opengis.net/ogc">`+"\n"+
		`  <ServiceException code="`+xmlEscape(code)+`">`+xmlEscape(msg)+`</ServiceException>`+"\n"+
		`</ServiceExceptionReport>`)
}

// wmsCapabilities 生成 WMS 1.3.0 GetCapabilities：Layer 完全来自真实影像金字塔。
func (s *Server) wmsCapabilities(c *gin.Context) {
	if s == nil || s.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}
	layers := s.publishedImageTasks(c.GetString(ScopeTenantKey))
	base := s.kvpBaseURL(c, "/api/v1/wms")

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<WMS_Capabilities version="1.3.0"` +
		` xmlns="http://www.opengis.net/wms"` +
		` xmlns:xlink="http://www.w3.org/1999/xlink"` +
		` xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` +
		` xsi:schemaLocation="http://www.opengis.net/wms http://schemas.opengis.net/wms/1.3.0/capabilities_1_3_0.xsd">` + "\n")
	b.WriteString("  <Service>\n")
	b.WriteString("    <Name>WMS</Name>\n")
	b.WriteString("    <Title>TanGIS WMS Service</Title>\n")
	b.WriteString("    <Abstract>TanGIS 影像瓦片服务（WMS 1.3.0）</Abstract>\n")
	b.WriteString(`    <OnlineResource xlink:type="simple" xlink:href="` + xmlEscape(base+"?") + `"/>` + "\n")
	b.WriteString(fmt.Sprintf("    <MaxWidth>%d</MaxWidth>\n    <MaxHeight>%d</MaxHeight>\n", wmsMaxSide, wmsMaxSide))
	b.WriteString("  </Service>\n")
	b.WriteString("  <Capability>\n")
	b.WriteString("    <Request>\n")
	b.WriteString("      <GetCapabilities>\n")
	b.WriteString("        <Format>text/xml</Format>\n")
	b.WriteString(`        <DCPType><HTTP><Get><OnlineResource xlink:type="simple" xlink:href="` + xmlEscape(base+"?") + `"/></Get></HTTP></DCPType>` + "\n")
	b.WriteString("      </GetCapabilities>\n")
	b.WriteString("      <GetMap>\n")
	b.WriteString("        <Format>image/png</Format>\n")
	b.WriteString("        <Format>image/jpeg</Format>\n")
	b.WriteString(`        <DCPType><HTTP><Get><OnlineResource xlink:type="simple" xlink:href="` + xmlEscape(base+"?") + `"/></Get></HTTP></DCPType>` + "\n")
	b.WriteString("      </GetMap>\n")
	b.WriteString("    </Request>\n")
	b.WriteString("    <Exception><Format>XML</Format></Exception>\n")
	b.WriteString("    <Layer>\n")
	b.WriteString("      <Title>TanGIS 影像服务</Title>\n")
	wmsCRSList(&b, "      ")
	// 图层按任务创建顺序输出（与 WMTS 一致，便于客户端稳定对照）
	sort.Slice(layers, func(i, j int) bool { return layers[i].Task.CreatedAt.Before(layers[j].Task.CreatedAt) })
	for _, l := range layers {
		s.wmsLayer(&b, l)
	}
	b.WriteString("    </Layer>\n")
	b.WriteString("  </Capability>\n")
	b.WriteString("</WMS_Capabilities>\n")

	c.Data(http.StatusOK, "application/xml; charset=UTF-8", []byte(b.String()))
}

// wmsCRSList 声明支持的坐标系（三个：Web Mercator 与两种经纬度约定）。
func wmsCRSList(b *strings.Builder, indent string) {
	for _, crs := range []string{"EPSG:3857", "EPSG:4326", "CRS:84"} {
		b.WriteString(indent + "<CRS>" + crs + "</CRS>\n")
	}
}

// wmsLayer 输出单个 Layer：包围盒同时给出 CRS:84（lon,lat）、EPSG:3857 与
// EPSG:4326（1.3.0 轴序为 lat,lon），最大化客户端兼容性。
func (s *Server) wmsLayer(b *strings.Builder, l *layerInfo) {
	ext := l.Meta.Extent
	lonMin, latMin := mercToLonLat(ext[0], ext[1])
	lonMax, latMax := mercToLonLat(ext[2], ext[3])
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 8, 64) }

	b.WriteString(`      <Layer queryable="0">` + "\n")
	b.WriteString("        <Name>" + xmlEscape(l.Task.ID) + "</Name>\n")
	b.WriteString("        <Title>" + xmlEscape(wmsLayerTitle(l.Task)) + "</Title>\n")
	wmsCRSList(b, "        ")
	b.WriteString("        <EX_GeographicBoundingBox>\n")
	b.WriteString("          <westBoundLongitude>" + f(lonMin) + "</westBoundLongitude>\n")
	b.WriteString("          <eastBoundLongitude>" + f(lonMax) + "</eastBoundLongitude>\n")
	b.WriteString("          <southBoundLatitude>" + f(latMin) + "</southBoundLatitude>\n")
	b.WriteString("          <northBoundLatitude>" + f(latMax) + "</northBoundLatitude>\n")
	b.WriteString("        </EX_GeographicBoundingBox>\n")
	b.WriteString(`        <BoundingBox CRS="CRS:84" minx="` + f(lonMin) + `" miny="` + f(latMin) +
		`" maxx="` + f(lonMax) + `" maxy="` + f(latMax) + `"/>` + "\n")
	b.WriteString(`        <BoundingBox CRS="EPSG:3857" minx="` + f(ext[0]) + `" miny="` + f(ext[1]) +
		`" maxx="` + f(ext[2]) + `" maxy="` + f(ext[3]) + `"/>` + "\n")
	// 1.3.0：EPSG:4326 轴序为纬度在前
	b.WriteString(`        <BoundingBox CRS="EPSG:4326" minx="` + f(latMin) + `" miny="` + f(lonMin) +
		`" maxx="` + f(latMax) + `" maxy="` + f(lonMax) + `"/>` + "\n")
	b.WriteString("        <Style><Name>default</Name><Title>默认样式</Title></Style>\n")
	b.WriteString("      </Layer>\n")
}

// wmsLayerTitle 图层标题：任务类型 + 短 ID。
func wmsLayerTitle(t *task.Task) string {
	id := t.ID
	if len(id) > 8 {
		id = id[:8]
	}
	if t.Type == "" {
		return "TanGIS " + id
	}
	return t.Type + " · " + id
}

// wmsGetMap 渲染请求区域为图片。
func (s *Server) wmsGetMap(c *gin.Context) {
	if s == nil || s.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}
	version := kvpDefault(c, "version", "1.3.0")
	switch version {
	case "1.3.0", "1.1.1", "1.1.0", "1.0.0":
	default:
		s.wmsException(c, http.StatusBadRequest, "InvalidVersion", "unsupported version: "+version)
		return
	}

	// 图层名：只支持单图层（多图层合成语义不同，宁可不做也不出错误的图）
	layerName := strings.TrimSpace(kvp(c, "layers"))
	if layerName == "" {
		layerName = strings.TrimSpace(kvp(c, "layer"))
	}
	if layerName == "" {
		s.wmsException(c, http.StatusBadRequest, "MissingParameter", "missing layers parameter")
		return
	}
	if strings.Contains(layerName, ",") {
		s.wmsException(c, http.StatusBadRequest, "InvalidParameterValue",
			"multiple layers are not supported (request one layer per GetMap)")
		return
	}

	rawCRS := kvp(c, "crs")
	if rawCRS == "" {
		rawCRS = kvp(c, "srs") // 1.1.1 及更早用 srs
	}
	crs, ok := parseCRS(rawCRS, version)
	if !ok {
		s.wmsException(c, http.StatusBadRequest, "InvalidCRS",
			"unsupported crs/srs (EPSG:3857 | EPSG:4326 | CRS:84 required): "+rawCRS)
		return
	}

	bbox, err := parseBBox(kvp(c, "bbox"))
	if err != nil {
		s.wmsException(c, http.StatusBadRequest, "InvalidParameterValue", err.Error())
		return
	}
	// 1.3.0 + EPSG:4326：轴序 lat,lon → 规范化为 lon,lat
	if crs.LatFirst {
		bbox = [4]float64{bbox[1], bbox[0], bbox[3], bbox[2]}
	}

	width, err1 := strconv.Atoi(kvpDefault(c, "width", "256"))
	height, err2 := strconv.Atoi(kvpDefault(c, "height", "256"))
	if err1 != nil || err2 != nil || width <= 0 || height <= 0 {
		s.wmsException(c, http.StatusBadRequest, "InvalidParameterValue",
			"width/height must be positive integers")
		return
	}
	if width > wmsMaxSide || height > wmsMaxSide {
		s.wmsException(c, http.StatusBadRequest, "InvalidParameterValue",
			fmt.Sprintf("width/height exceeds limit %d", wmsMaxSide))
		return
	}

	format := kvpDefault(c, "format", "image/png")
	if canonicalFormat(format) == "" {
		s.wmsException(c, http.StatusBadRequest, "InvalidFormat",
			"unsupported format (image/png | image/jpeg): "+format)
		return
	}
	transparent := strings.EqualFold(strings.TrimSpace(kvp(c, "transparent")), "true")
	if bg := strings.TrimSpace(kvp(c, "bgcolor")); bg != "" && strings.EqualFold(bg, "0xFFFFFF") {
		transparent = false
	}

	// 取任务并核对可分发状态与产物
	t, err := s.Store.Get(layerName, c.GetString(ScopeTenantKey))
	if err != nil {
		s.wmsException(c, http.StatusNotFound, "LayerNotDefined", "unknown layer: "+layerName)
		return
	}
	if !s.published(t) {
		s.wmsException(c, http.StatusNotFound, "LayerNotDefined",
			"layer not published (task must be SUCCEEDED + approved): "+layerName)
		return
	}
	meta, err := s.LoadPyramidMeta(t)
	if err != nil {
		s.wmsException(c, http.StatusNotFound, "LayerNotDefined",
			"layer has no 2D tile pyramid (image task required): "+layerName)
		return
	}

	img, stats, err := s.renderMosaic(t, mosaicRequest{
		TaskID:      t.ID,
		Meta:        meta,
		CRS:         crs,
		BBox:        bbox,
		Width:       width,
		Height:      height,
		Transparent: transparent,
	})
	if err != nil {
		s.wmsException(c, http.StatusBadRequest, "InvalidParameterValue", err.Error())
		return
	}
	data, ctype, err := encodeRaster(img, format)
	if err != nil {
		s.wmsException(c, http.StatusInternalServerError, "NoApplicableCode", err.Error())
		return
	}

	c.Header("X-Tangis-Zoom", strconv.Itoa(stats.Zoom))
	c.Header("X-Tangis-Mosaic", fmt.Sprintf("%dx%d", stats.Cols, stats.Rows))
	c.Header("X-Tangis-Missing-Tiles", strconv.Itoa(stats.Missing))
	c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d", int(s.cacheTTL().Seconds())))
	c.Data(http.StatusOK, ctype, data)
}

// parseBBox 解析 "minx,miny,maxx,maxy"。
func parseBBox(raw string) ([4]float64, error) {
	var out [4]float64
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) != 4 {
		return out, fmt.Errorf("bbox must be 4 comma-separated numbers, got %q", raw)
	}
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return out, fmt.Errorf("invalid bbox value %q", strings.TrimSpace(p))
		}
		out[i] = f
	}
	return out, nil
}

// kvpBaseURL 生成 KVP 入口绝对地址（Capabilities 的 OnlineResource 用）。
func (s *Server) kvpBaseURL(c *gin.Context, path string) string {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host + path
}
