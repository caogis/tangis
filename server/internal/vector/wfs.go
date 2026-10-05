// wfs.go — WFS 2.0 简单子集（M2-F14）：
//
//   - GetCapabilities：从 vector 已注册图层真实生成 XML
//     （ServiceIdentification + FeatureTypeList：图层名/DefaultCRS/WGS84 bbox），
//     不硬编码假图层；
//   - GetFeature：typeName=图层名、bbox（WGS84 CRS84）过滤、count 上限
//     （可配 TANGIS_WFS_MAX_FEATURES，默认 10000）、输出 GeoJSON FeatureCollection。
//
// 鉴权与缓存沿用 vector MVT 同一语义（路由层 API Key/签名 URL，Redis 缓存）。
// 异常统一输出 OGC（ows 1.1）ExceptionReport XML。
package vector

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// DefaultWFSMaxFeatures 单次 GetFeature 要素数上限默认值
// （可配 TANGIS_WFS_MAX_FEATURES）。
const DefaultWFSMaxFeatures = 10000

// wfsVersion WFS 简单子集实现的规范版本号。
const wfsVersion = "2.0.0"

// WFSMaxFeatures 单次 GetFeature 要素数上限（含默认值兜底）。
func (s *Server) WFSMaxFeatures() int {
	if s.WFSMax > 0 {
		return s.WFSMax
	}
	return DefaultWFSMaxFeatures
}

// Features GetFeature 编排：图层解析 → bbox/count 校验 → 缓存 → PostGIS 查询。
// hasBBox=false（请求未带 bbox）时输出全图层（LIMIT 收敛），bbox 参数仅在
// 提供时生效（WGS84 CRS84 经纬度序）。count<=0 视为取上限；count 超上限时
// 收敛到上限（不报错，避免正常客户端被拒）。
// 返回 (FeatureCollection JSON, 要素数, 错误)。
func (s *Server) Features(ctx context.Context, typeName string, hasBBox bool, minx, miny, maxx, maxy float64, count int) ([]byte, int, error) {
	if !ValidIdentifier(typeName) || typeName == "" {
		return nil, 0, fmt.Errorf("%w: typeName %q invalid", ErrInvalid, typeName)
	}
	if hasBBox {
		// bbox 顺序与有限性校验（CRS84 经纬度）
		if !finite(minx) || !finite(miny) || !finite(maxx) || !finite(maxy) ||
			minx >= maxx || miny >= maxy {
			return nil, 0, fmt.Errorf("%w: bbox must be minx,miny,maxx,maxy (CRS84, lower-left first)", ErrInvalid)
		}
	}
	capN := s.WFSMaxFeatures()
	if count <= 0 || count > capN {
		count = capN
	}
	l, err := s.Meta.GetLayer(ctx, typeName)
	if err != nil {
		return nil, 0, err
	}
	key := fmt.Sprintf("tile:wfs:%s:%t:%f,%f,%f,%f:%d", l.Name, hasBBox, minx, miny, maxx, maxy, count)
	if s.Cache != nil {
		if v, ok := s.Cache.Get(ctx, key); ok {
			return v, -1, nil // -1：缓存命中，要素数未知
		}
	}
	src, err := s.Meta.GetSource(ctx, l.SourceID)
	if err != nil {
		return nil, 0, err
	}
	feats, n, err := s.Gateway.FeaturesGeoJSON(ctx, src.DSN, l, hasBBox, minx, miny, maxx, maxy, count)
	if err != nil {
		return nil, 0, err
	}
	fc := featureCollection(feats)
	if s.Cache != nil && s.CacheTTL > 0 {
		s.Cache.Set(ctx, key, fc, s.CacheTTL)
	}
	return fc, n, nil
}

// featureCollection 把 features JSON 数组包装为 FeatureCollection。
func featureCollection(features []byte) []byte {
	return []byte(`{"type":"FeatureCollection","features":` + string(features) + `}`)
}

// CapabilitiesXML 从已注册图层生成 WFS GetCapabilities XML。
// base 为对外基地址（scheme://host）。bbox 来自 ST_EstimatedExtent（真实统计），
// 统计缺失或 SRID 无法换算时该 FeatureType 不输出 bbox（不造假）。
func (s *Server) CapabilitiesXML(ctx context.Context, base string) (string, error) {
	layers, err := s.Meta.ListLayers(ctx)
	if err != nil {
		return "", err
	}
	// 稳定输出：按图层名排序
	sort.Slice(layers, func(i, j int) bool { return layers[i].Name < layers[j].Name })

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<wfs:WFS_Capabilities xmlns:wfs="http://www.opengis.net/wfs/2.0" xmlns:ows="http://www.opengis.net/ows/1.1" xmlns:xlink="http://www.w3.org/1999/xlink" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="http://www.opengis.net/wfs/2.0 http://schemas.opengis.net/wfs/2.0/wfs.xsd" version="` + wfsVersion + `">` + "\n")

	b.WriteString("  <ows:ServiceIdentification>\n")
	b.WriteString("    <ows:Title>TanGIS WFS Service</ows:Title>\n")
	b.WriteString("    <ows:ServiceType>OGC WFS</ows:ServiceType>\n")
	b.WriteString("    <ows:ServiceTypeVersion>" + wfsVersion + "</ows:ServiceTypeVersion>\n")
	b.WriteString("  </ows:ServiceIdentification>\n")

	kvpURL := base + "/api/v1/wfs?"
	b.WriteString("  <ows:OperationsMetadata>\n")
	writeWFSOperation(&b, "GetCapabilities", kvpURL)
	writeWFSOperation(&b, "GetFeature", kvpURL)
	b.WriteString("  </ows:OperationsMetadata>\n")

	b.WriteString("  <FeatureTypeList>\n")
	for _, l := range layers {
		s.writeFeatureType(ctx, &b, l)
	}
	b.WriteString("  </FeatureTypeList>\n")

	b.WriteString("</wfs:WFS_Capabilities>\n")
	return b.String(), nil
}

// writeFeatureType 输出单个 FeatureType：标识/标题/DefaultCRS/输出格式/WGS84 bbox。
func (s *Server) writeFeatureType(ctx context.Context, b *strings.Builder, l *Layer) {
	fmt.Fprintf(b, "    <FeatureType>\n")
	fmt.Fprintf(b, "      <ows:Title>%s</ows:Title>\n", xmlEscWFS(l.Name))
	fmt.Fprintf(b, "      <ows:Identifier>%s</ows:Identifier>\n", xmlEscWFS(l.Name))
	fmt.Fprintf(b, "      <DefaultCRS>urn:ogc:def:crs:EPSG::%d</DefaultCRS>\n", l.SRID)
	fmt.Fprintf(b, "      <OutputFormat>application/geo+json</OutputFormat>\n")
	if bbox := s.layerWGS84BBox(ctx, l); bbox != nil {
		fmt.Fprintf(b, "      <ows:WGS84BoundingBox crs=\"urn:ogc:def:crs:OGC:1.3:CRS84\">\n"+
			"        <ows:LowerCorner>%s</ows:LowerCorner>\n"+
			"        <ows:UpperCorner>%s</ows:UpperCorner>\n"+
			"      </ows:WGS84BoundingBox>\n",
			xmlEscWFS(fmtCoordWFS(bbox[0], bbox[1])), xmlEscWFS(fmtCoordWFS(bbox[2], bbox[3])))
	}
	b.WriteString("    </FeatureType>\n")
}

// layerWGS84BBox 图层真实 bbox（ST_EstimatedExtent → WGS84）。
// 统计缺失、查询失败或 SRID 不在 4326/3857 换算范围内 → nil（Capabilities
// 中省略该元素，不编造范围）。
func (s *Server) layerWGS84BBox(ctx context.Context, l *Layer) []float64 {
	if l.SRID != 4326 && l.SRID != 3857 {
		return nil
	}
	src, err := s.Meta.GetSource(ctx, l.SourceID)
	if err != nil {
		return nil
	}
	minx, miny, maxx, maxy, ok, err := s.Gateway.EstimatedExtent(ctx, src.DSN, l)
	if err != nil || !ok {
		return nil
	}
	return TransformBBoxToWGS84(minx, miny, maxx, maxy, l.SRID)
}

// writeWFSOperation 输出单个 ows:Operation（KVP encoding）。
func writeWFSOperation(b *strings.Builder, name, kvpURL string) {
	fmt.Fprintf(b, "    <ows:Operation name=\"%s\">\n      <ows:DCP>\n        <ows:HTTP>\n", xmlEscWFS(name))
	fmt.Fprintf(b, "          <ows:Get xlink:href=\"%s\"/>\n        </ows:HTTP>\n      </ows:DCP>\n    </ows:Operation>\n", xmlEscWFS(kvpURL))
}

// ExceptionReport 输出 OGC（ows 1.1）异常 XML；status 由调用方决定
// （参数错 400 / 图层不存在 404 / 上游故障 502 等）。
func ExceptionReport(status int, code, locator, text string) (int, string) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<ExceptionReport xmlns="http://www.opengis.net/ows/1.1" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="http://www.opengis.net/ows/1.1 http://schemas.opengis.net/ows/1.1.0/owsExceptionReport.xsd" version="` + wfsVersion + `">` + "\n")
	b.WriteString("  <Exception exceptionCode=\"" + xmlEscWFS(code) + "\"")
	if locator != "" {
		b.WriteString(" locator=\"" + xmlEscWFS(locator) + "\"")
	}
	b.WriteString(">\n")
	b.WriteString("    <ExceptionText>" + xmlEscWFS(text) + "</ExceptionText>\n")
	b.WriteString("  </Exception>\n</ExceptionReport>\n")
	return status, b.String()
}

func fmtCoordWFS(a, b float64) string {
	return fmt.Sprintf("%g %g", a, b)
}

func xmlEscWFS(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
