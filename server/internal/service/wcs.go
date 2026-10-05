// wcs.go 实现 OGC WCS 1.0.0 子集（服务分发补齐）。
//
// 路由（鉴权语义同 WMTS/WMS：API Key 或防盗链签名任一）：
//
//	KVP GET /api/v1/wcs?service=WCS&request=GetCapabilities
//	KVP GET /api/v1/wcs?service=WCS&request=DescribeCoverage&coverage={task}
//	KVP GET /api/v1/wcs?service=WCS&request=GetCoverage&coverage={task}
//	        &bbox=minx,miny,maxx,maxy&crs=EPSG:4326&width=&height=&format=image/png
//
// 说明：上游产物是「影像瓦片金字塔」而非原始栅格，因此 GetCoverage 的语义是
// 「按请求 bbox/尺寸重新镶嵌出该区域影像」——与 WMS GetMap 共用 raster.go 的制图核心，
// 差别只在描述文档（覆盖元数据）与参数名。
//
// 格式取舍：只声明并输出 **image/png**。TIFF 需要地理标签（GeoKey）才有意义，
// 而本服务不对产物做重投影写入，产出"看着像 GeoTIFF 却没有地理信息"的文件
// 比不支持更糟——故明确拒绝并提示改用 PNG。
package service

import (
	"bytes"
	"fmt"
	"image/png"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// WCSKvp GET /api/v1/wcs — KVP 入口。
func (s *Server) WCSKvp(c *gin.Context) {
	switch strings.ToUpper(strings.TrimSpace(kvp(c, "request"))) {
	case "GETCAPABILITIES":
		s.wcsCapabilities(c)
	case "DESCRIBECOVERAGE":
		s.wcsDescribeCoverage(c)
	case "GETCOVERAGE":
		s.wcsGetCoverage(c)
	case "":
		s.wcsException(c, http.StatusBadRequest, "MissingParameter",
			"missing request parameter (GetCapabilities | DescribeCoverage | GetCoverage)")
	default:
		s.wcsException(c, http.StatusBadRequest, "OperationNotSupported",
			"unsupported request: "+kvp(c, "request"))
	}
}

// wcsException 输出 WCS ServiceExceptionReport。
func (s *Server) wcsException(c *gin.Context, status int, code, msg string) {
	c.Header("Content-Type", "application/vnd.ogc.se_xml; charset=UTF-8")
	c.String(status, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
		`<ServiceExceptionReport version="1.0.0" xmlns="http://www.opengis.net/ogc">`+"\n"+
		`  <ServiceException code="`+xmlEscape(code)+`">`+xmlEscape(msg)+`</ServiceException>`+"\n"+
		`</ServiceExceptionReport>`)
}

// wcsCapabilities 生成 GetCapabilities：Coverage 完全来自真实影像金字塔。
func (s *Server) wcsCapabilities(c *gin.Context) {
	if s == nil || s.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}
	layers := s.publishedImageTasks(c.GetString(ScopeTenantKey))
	base := s.kvpBaseURL(c, "/api/v1/wcs")

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<WCS_Capabilities version="1.0.0"` +
		` xmlns="http://www.opengis.net/wcs"` +
		` xmlns:xlink="http://www.w3.org/1999/xlink"` +
		` xmlns:gml="http://www.opengis.net/gml"` +
		` xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` +
		` xsi:schemaLocation="http://www.opengis.net/wcs http://schemas.opengis.net/wcs/1.0.0/getCapabilities.xsd">` + "\n")
	b.WriteString("  <Service>\n")
	b.WriteString("    <description>TanGIS 栅格覆盖服务</description>\n")
	b.WriteString("    <name>TanGIS WCS</name>\n")
	b.WriteString("    <label>TanGIS WCS 1.0.0</label>\n")
	b.WriteString("    <fees>NONE</fees>\n")
	b.WriteString("    <accessConstraints>NONE</accessConstraints>\n")
	b.WriteString("  </Service>\n")
	b.WriteString("  <Capability>\n")
	b.WriteString("    <Request>\n")
	for _, op := range []string{"GetCapabilities", "DescribeCoverage", "GetCoverage"} {
		b.WriteString("      <" + op + ">\n")
		b.WriteString("        <DCPType><HTTP><Get><OnlineResource xlink:type=\"simple\" xlink:href=\"" +
			xmlEscape(base+"?") + "\"/></Get></HTTP></DCPType>\n")
		b.WriteString("      </" + op + ">\n")
	}
	b.WriteString("    </Request>\n")
	b.WriteString("    <Exception><Format>application/vnd.ogc.se_xml</Format></Exception>\n")
	b.WriteString("  </Capability>\n")
	b.WriteString("  <ContentMetadata>\n")
	sort.Slice(layers, func(i, j int) bool { return layers[i].Task.CreatedAt.Before(layers[j].Task.CreatedAt) })
	for _, l := range layers {
		lonMin, latMin := mercToLonLat(l.Meta.Extent[0], l.Meta.Extent[1])
		lonMax, latMax := mercToLonLat(l.Meta.Extent[2], l.Meta.Extent[3])
		f := func(v float64) string { return strconv.FormatFloat(v, 'f', 8, 64) }
		b.WriteString("    <CoverageOfferingBrief>\n")
		b.WriteString("      <name>" + xmlEscape(l.Task.ID) + "</name>\n")
		b.WriteString("      <label>" + xmlEscape(wmsLayerTitle(l.Task)) + "</label>\n")
		b.WriteString(`      <lonLatEnvelope srsName="urn:ogc:def:crs:OGC:1.3:CRS84">` + "\n")
		b.WriteString("        <gml:pos>" + f(lonMin) + " " + f(latMin) + "</gml:pos>\n")
		b.WriteString("        <gml:pos>" + f(lonMax) + " " + f(latMax) + "</gml:pos>\n")
		b.WriteString("      </lonLatEnvelope>\n")
		b.WriteString("    </CoverageOfferingBrief>\n")
	}
	b.WriteString("  </ContentMetadata>\n")
	b.WriteString("</WCS_Capabilities>\n")

	c.Data(http.StatusOK, "application/xml; charset=UTF-8", []byte(b.String()))
}

// wcsDescribeCoverage 描述单个覆盖：范围、支持坐标系与格式。
func (s *Server) wcsDescribeCoverage(c *gin.Context) {
	if s == nil || s.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}
	name := strings.TrimSpace(kvp(c, "coverage"))
	if name == "" {
		s.wcsException(c, http.StatusBadRequest, "MissingParameter", "missing coverage parameter")
		return
	}
	l, errMsg := s.wcsCoverage(c, name)
	if errMsg != "" {
		s.wcsException(c, http.StatusNotFound, "CoverageNotDefined", errMsg)
		return
	}
	ext := l.Meta.Extent
	lonMin, latMin := mercToLonLat(ext[0], ext[1])
	lonMax, latMax := mercToLonLat(ext[2], ext[3])
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 8, 64) }

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<CoverageDescription version="1.0.0"` +
		` xmlns="http://www.opengis.net/wcs"` +
		` xmlns:gml="http://www.opengis.net/gml"` +
		` xmlns:xlink="http://www.w3.org/1999/xlink"` +
		` xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` +
		` xsi:schemaLocation="http://www.opengis.net/wcs http://schemas.opengis.net/wcs/1.0.0/describeCoverage.xsd">` + "\n")
	b.WriteString("  <CoverageOffering>\n")
	b.WriteString("    <name>" + xmlEscape(l.Task.ID) + "</name>\n")
	b.WriteString("    <label>" + xmlEscape(wmsLayerTitle(l.Task)) + "</label>\n")
	b.WriteString("    <lonLatEnvelope srsName=\"urn:ogc:def:crs:OGC:1.3:CRS84\">\n")
	b.WriteString("      <gml:pos>" + f(lonMin) + " " + f(latMin) + "</gml:pos>\n")
	b.WriteString("      <gml:pos>" + f(lonMax) + " " + f(latMax) + "</gml:pos>\n")
	b.WriteString("    </lonLatEnvelope>\n")
	b.WriteString("    <domainSet>\n")
	b.WriteString("      <spatialDomain>\n")
	b.WriteString(`        <gml:Envelope srsName="urn:ogc:def:crs:OGC:1.3:CRS84">` + "\n")
	b.WriteString("          <gml:pos>" + f(lonMin) + " " + f(latMin) + "</gml:pos>\n")
	b.WriteString("          <gml:pos>" + f(lonMax) + " " + f(latMax) + "</gml:pos>\n")
	b.WriteString("        </gml:Envelope>\n")
	b.WriteString("      </spatialDomain>\n")
	b.WriteString("    </domainSet>\n")
	b.WriteString("    <supportedCRSs>\n")
	b.WriteString("      <requestResponseCRSs>EPSG:4326</requestResponseCRSs>\n")
	b.WriteString("      <requestResponseCRSs>EPSG:3857</requestResponseCRSs>\n")
	b.WriteString("      <requestResponseCRSs>CRS:84</requestResponseCRSs>\n")
	b.WriteString("    </supportedCRSs>\n")
	b.WriteString("    <supportedFormats>\n")
	b.WriteString("      <formats>image/png</formats>\n")
	b.WriteString("    </supportedFormats>\n")
	b.WriteString("  </CoverageOffering>\n")
	b.WriteString("</CoverageDescription>\n")

	c.Data(http.StatusOK, "application/xml; charset=UTF-8", []byte(b.String()))
}

// wcsGetCoverage 按 bbox/尺寸镶嵌出影像（image/png）。
func (s *Server) wcsGetCoverage(c *gin.Context) {
	if s == nil || s.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}
	name := strings.TrimSpace(kvp(c, "coverage"))
	if name == "" {
		s.wcsException(c, http.StatusBadRequest, "MissingParameter", "missing coverage parameter")
		return
	}
	l, errMsg := s.wcsCoverage(c, name)
	if errMsg != "" {
		s.wcsException(c, http.StatusNotFound, "CoverageNotDefined", errMsg)
		return
	}

	// WCS 1.0.0 的 bbox 轴序为 lon,lat（与 WMS 1.3.0 的 EPSG:4326 相反），
	// 故这里按 "1.0.0" 解析 CRS，不启用 LatFirst。
	rawCRS := kvpDefault(c, "crs", kvpDefault(c, "response_crs", "EPSG:4326"))
	crs, ok := parseCRS(rawCRS, "1.0.0")
	if !ok {
		s.wcsException(c, http.StatusBadRequest, "InvalidCRS",
			"unsupported crs (EPSG:3857 | EPSG:4326 | CRS:84 required): "+rawCRS)
		return
	}
	bbox, err := parseBBox(kvp(c, "bbox"))
	if err != nil {
		s.wcsException(c, http.StatusBadRequest, "InvalidParameterValue", err.Error())
		return
	}

	width, err1 := strconv.Atoi(kvpDefault(c, "width", "512"))
	height, err2 := strconv.Atoi(kvpDefault(c, "height", "512"))
	if err1 != nil || err2 != nil || width <= 0 || height <= 0 {
		s.wcsException(c, http.StatusBadRequest, "InvalidParameterValue",
			"width/height must be positive integers")
		return
	}
	if width > maxMosaicPixels || height > maxMosaicPixels {
		s.wcsException(c, http.StatusBadRequest, "InvalidParameterValue",
			fmt.Sprintf("width/height exceeds limit %d", maxMosaicPixels))
		return
	}

	format := strings.ToLower(strings.TrimSpace(kvpDefault(c, "format", "image/png")))
	switch format {
	case "image/png", "png":
	case "image/tiff", "tiff", "image/geotiff", "geotiff":
		s.wcsException(c, http.StatusNotImplemented, "InvalidFormat",
			"TIFF/GeoTIFF 输出未实现（本服务不写地理标签，避免产出误导性文件）；请使用 format=image/png")
		return
	default:
		s.wcsException(c, http.StatusBadRequest, "InvalidFormat",
			"unsupported format (image/png): "+format)
		return
	}

	img, stats, rerr := s.renderMosaic(l.Task, mosaicRequest{
		TaskID:      l.Task.ID,
		Meta:        l.Meta,
		CRS:         crs,
		BBox:        bbox,
		Width:       width,
		Height:      height,
		Transparent: strings.EqualFold(kvpDefault(c, "transparent", "false"), "true"),
	})
	if rerr != nil {
		s.wcsException(c, http.StatusBadRequest, "InvalidParameterValue", rerr.Error())
		return
	}

	var buf bytes.Buffer
	if e := png.Encode(&buf, img); e != nil {
		s.wcsException(c, http.StatusInternalServerError, "NoApplicableCode", e.Error())
		return
	}

	c.Header("X-Tangis-Zoom", strconv.Itoa(stats.Zoom))
	c.Header("X-Tangis-Mosaic", fmt.Sprintf("%dx%d", stats.Cols, stats.Rows))
	c.Header("X-Tangis-Missing-Tiles", strconv.Itoa(stats.Missing))
	c.Data(http.StatusOK, "image/png", buf.Bytes())
}

// wcsCoverage 取已发布的影像任务与其金字塔元数据；返回错误文案（空串表示成功）。
func (s *Server) wcsCoverage(c *gin.Context, name string) (*layerInfo, string) {
	t, err := s.Store.Get(name, c.GetString(ScopeTenantKey))
	if err != nil {
		return nil, "unknown coverage: " + name
	}
	if !s.published(t) {
		return nil, "coverage not published (task must be SUCCEEDED + approved): " + name
	}
	meta, err := s.LoadPyramidMeta(t)
	if err != nil {
		return nil, "coverage has no 2D tile pyramid (image task required): " + name
	}
	return &layerInfo{Task: t, Meta: meta}, ""
}
