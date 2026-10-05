// raster.go 实现 WMS/WCS 共用的栅格制图核心。
//
// 与瓦片分发（tiles.go/wmts.go）的区别：WMTS/TMS 是「一次一个瓦片」，
// WMS/WCS 是「给我一个 bbox + 目标像素尺寸，还我一张图」。因此这里做三件事：
//
//  1. 把请求 bbox（EPSG:3857 / EPSG:4326 / CRS:84）统一换算到 Web Mercator 米；
//  2. 选一个合适的金字塔层级，把覆盖 bbox 的瓦片拼接成整幅画布；
//  3. 按目标尺寸做最近邻重采样输出（PNG/JPEG）。
//
// 真实数据原则：瓦片缺失就留透明（并在响应头 X-Tangis-Missing-Tiles 报数），
// 绝不返回占位图；金字塔 TileMatrixSet 非 WebMercatorQuad 时明确报错（不做静默近似）。
package service

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"strings"

	"tangis/server/internal/task"
)

// Web Mercator（EPSG:3857）常数：纬度有效范围与 x 半跨度（米）。
const (
	mercLatLimit = 85.05112877980659
	mercHalfX    = 20037508.342789244
	mercSpanX    = 2 * mercHalfX
)

// 制图资源上限：一次请求最多拼接的瓦片数与输出单边像素上限。
// 两者都是「防打爆内存」的硬约束，超限时降层级或直接报错（不静默截断画面）。
const (
	maxMosaicTiles  = 100
	maxMosaicPixels = 4096
)

// mercatorCRS 制图请求的坐标系。
type mercatorCRS struct {
	ID       string // 规范化标识（回显用）：EPSG:3857 / EPSG:4326 / CRS:84
	IsLatLon bool   // 坐标是经纬度（true）还是 Web Mercator 米（false）
	// LatFirst 经纬度轴序是否为 lat,lon。
	// WMS 1.3.0 规定 EPSG:4326 的 bbox 是 lat,lon（规范如此，也是著名的兼容陷阱）；
	// 1.1.1 与 CRS:84 则是 lon,lat。
	LatFirst bool
}

// parseCRS 解析 WMS/WCS 的 crs / srs 参数；不支持的坐标系返回 false
// （调用方按规范回 InvalidCRS 错误，而不是猜一个坐标系）。
func parseCRS(raw, version string) (mercatorCRS, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "epsg:3857", "epsg:900913", "epsg:102100", "urn:ogc:def:crs:epsg::3857":
		return mercatorCRS{ID: "EPSG:3857"}, true
	case "crs:84", "urn:ogc:def:crs:ogc:1.3:crs84", "urn:ogc:def:crs:ogc:2:84":
		return mercatorCRS{ID: "CRS:84", IsLatLon: true}, true
	case "epsg:4326", "urn:ogc:def:crs:epsg::4326":
		return mercatorCRS{ID: "EPSG:4326", IsLatLon: true, LatFirst: version == "1.3.0"}, true
	}
	return mercatorCRS{}, false
}

// lonLatToMerc 经纬度 → Web Mercator 米（纬度截断到 ±85.051…）。
func lonLatToMerc(lon, lat float64) (x, y float64) {
	lat = math.Max(math.Min(lat, mercLatLimit), -mercLatLimit)
	x = lon * mercHalfX / 180
	y = math.Log(math.Tan((90+lat)*math.Pi/360)) * mercHalfX / math.Pi
	return x, y
}

// mercToLonLat Web Mercator 米 → 经纬度。
func mercToLonLat(x, y float64) (lon, lat float64) {
	lon = x * 180 / mercHalfX
	lat = (2*math.Atan(math.Exp(y*math.Pi/mercHalfX)) - math.Pi/2) * 180 / math.Pi
	return lon, lat
}

// mosaicStats 一次制图的统计信息，回显到响应头便于排障。
type mosaicStats struct {
	Zoom    int // 实际使用的金字塔层级
	Cols    int // 拼接画布列数
	Rows    int // 拼接画布行数
	Tiles   int // 成功读取的瓦片数
	Missing int // 缺失/损坏的瓦片数
}

// mosaicRequest 规范化的制图请求。
type mosaicRequest struct {
	TaskID      string
	Meta        *TilePyramidMeta
	CRS         mercatorCRS
	BBox        [4]float64 // 请求 CRS 下的 [minx,miny,maxx,maxy]，已规范为 x=经度/米、y=纬度/米
	Width       int
	Height      int
	Transparent bool
}

// renderMosaic 按请求把金字塔瓦片渲染成一张图。
func (s *Server) renderMosaic(t *task.Task, req mosaicRequest) (*image.RGBA, mosaicStats, error) {
	var st mosaicStats
	meta := req.Meta
	if meta == nil {
		return nil, st, fmt.Errorf("no tile pyramid metadata")
	}
	// 只支持 WebMercatorQuad：WorldCRS84Quad 的瓦片在经纬度空间宽高不等，
	// 拼接与采样语义不同，这里明确拒绝而不是给出错误的图。
	if meta.TileMatrixSet != "WebMercatorQuad" {
		return nil, st, fmt.Errorf("WMS/WCS 仅支持 WebMercatorQuad 金字塔（当前为 %s）", meta.TileMatrixSet)
	}
	if req.Width <= 0 || req.Height <= 0 {
		return nil, st, fmt.Errorf("width/height must be positive")
	}
	if req.Width > maxMosaicPixels || req.Height > maxMosaicPixels {
		return nil, st, fmt.Errorf("width/height exceeds limit %d", maxMosaicPixels)
	}

	// 请求 bbox → Web Mercator 米
	minX, minY, maxX, maxY := req.BBox[0], req.BBox[1], req.BBox[2], req.BBox[3]
	if req.CRS.IsLatLon {
		minX, minY = lonLatToMerc(minX, minY)
		maxX, maxY = lonLatToMerc(maxX, maxY)
	}
	if !(maxX > minX) || !(maxY > minY) {
		return nil, st, fmt.Errorf("invalid bbox: extent must be positive")
	}
	minX, maxX = clampF(minX, -mercHalfX, mercHalfX), clampF(maxX, -mercHalfX, mercHalfX)
	if !(maxX > minX) {
		return nil, st, fmt.Errorf("bbox outside Web Mercator extent")
	}

	ts := meta.TileSize
	res := (maxX - minX) / float64(req.Width) // 请求分辨率（米/像素）

	// 选层级：从最细层往下找第一个「金字塔分辨率 ≤ 请求分辨率」的层，
	// 即用刚好够清晰、冗余最小的层级（避免不必要的放大与拼接开销）。
	// 若金字塔所有层都比请求粗（请求分辨率更细），则退回最细层 MaxZoom。
	z := meta.MaxZoom
	for zz := meta.MaxZoom; zz >= meta.MinZoom; zz-- {
		spanZ := mercSpanX / float64(int64(1)<<uint(zz))
		if spanZ/float64(ts) <= res*1.0000001 { // 容差：边界上取更清晰的一侧
			z = zz
			break
		}
	}

	// 计算覆盖 bbox 的瓦片范围；超过瓦片数上限则逐级降 z（保证内存可控）
	var span float64
	var col0, col1, row0, row1 int
	for {
		span = mercSpanX / float64(int64(1)<<uint(z))
		col0 = int(math.Floor((minX + mercHalfX) / span))
		col1 = int(math.Floor((maxX + mercHalfX) / span))
		row0 = int(math.Floor((mercHalfX - maxY) / span))
		row1 = int(math.Floor((mercHalfX - minY) / span))
		if (col1-col0+1)*(row1-row0+1) <= maxMosaicTiles || z <= meta.MinZoom {
			break
		}
		z--
	}

	// 收敛到金字塔该层级的有效矩阵范围
	n := int(int64(1) << uint(z))
	col0, col1 = clampInt(col0, 0, n-1), clampInt(col1, 0, n-1)
	row0, row1 = clampInt(row0, 0, n-1), clampInt(row1, 0, n-1)
	cols, rows := col1-col0+1, row1-row0+1
	st.Zoom, st.Cols, st.Rows = z, cols, rows

	// 拼接画布（左上角原点，与 XYZ 瓦片约定一致）
	canvas := image.NewRGBA(image.Rect(0, 0, cols*ts, rows*ts))
	canvasMinX := -mercHalfX + float64(col0)*span
	canvasMaxY := mercHalfX - float64(row0)*span
	format := canonicalFormat(meta.Format)

	for row := row0; row <= row1; row++ {
		for col := col0; col <= col1; col++ {
			data, err := s.readPyramidTile(t, meta, z, col, row, format)
			if err != nil {
				st.Missing++
				continue
			}
			img, _, derr := image.Decode(bytes.NewReader(data))
			if derr != nil {
				st.Missing++
				continue
			}
			st.Tiles++
			drawTile(canvas, img, (col-col0)*ts, (row-row0)*ts)
		}
	}

	// 按目标尺寸重采样（最近邻：WMS 客户端普遍只要"看起来对"，不做重采样算法引入）
	out := image.NewRGBA(image.Rect(0, 0, req.Width, req.Height))
	if !req.Transparent {
		fillRGBA(out, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	}
	dx := (maxX - minX) / float64(req.Width)
	dy := (maxY - minY) / float64(req.Height)
	cw, ch := canvas.Bounds().Dx(), canvas.Bounds().Dy()
	for j := 0; j < req.Height; j++ {
		y := maxY - (float64(j)+0.5)*dy
		py := int((canvasMaxY - y) / span * float64(ts))
		if py < 0 || py >= ch {
			continue
		}
		rowOff := py * canvas.Stride
		for i := 0; i < req.Width; i++ {
			x := minX + (float64(i)+0.5)*dx
			px := int((x - canvasMinX) / span * float64(ts))
			if px < 0 || px >= cw {
				continue
			}
			off := rowOff + px*4
			o := out.PixOffset(i, j)
			out.Pix[o] = canvas.Pix[off]
			out.Pix[o+1] = canvas.Pix[off+1]
			out.Pix[o+2] = canvas.Pix[off+2]
			out.Pix[o+3] = canvas.Pix[off+3]
		}
	}
	return out, st, nil
}

// drawTile 把解码后的瓦片贴到画布指定位置（瓦片间不重叠，直接覆盖写入）。
func drawTile(dst *image.RGBA, src image.Image, ox, oy int) {
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		dy := oy + (y - b.Min.Y)
		if dy < 0 || dy >= dst.Bounds().Dy() {
			continue
		}
		for x := b.Min.X; x < b.Max.X; x++ {
			dx := ox + (x - b.Min.X)
			if dx < 0 || dx >= dst.Bounds().Dx() {
				continue
			}
			r, g, bl, a := src.At(x, y).RGBA()
			o := dst.PixOffset(dx, dy)
			dst.Pix[o] = uint8(r >> 8)
			dst.Pix[o+1] = uint8(g >> 8)
			dst.Pix[o+2] = uint8(bl >> 8)
			dst.Pix[o+3] = uint8(a >> 8)
		}
	}
}

// fillRGBA 用纯色填充整幅图（WMS 非透明请求的白底）。
func fillRGBA(img *image.RGBA, c color.RGBA) {
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
}

// encodeRaster 按请求格式编码图像，返回字节与 Content-Type。
func encodeRaster(img image.Image, format string) ([]byte, string, error) {
	var buf bytes.Buffer
	switch canonicalFormat(format) {
	case "jpg":
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/jpeg", nil
	default:
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/png", nil
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
