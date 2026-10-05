package service

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/task"
)

// 测试金字塔的瓦片边长（小图便于逐像素断言拼接与采样方向是否正确）。
const testTileSize = 4

// 世界范围 Web Mercator 边界（米）。
const mercWorld = "-20037508.342789244,-20037508.342789244,20037508.342789244,20037508.342789244"

var (
	tileRed    = color.RGBA{R: 255, A: 255}
	tileGreen  = color.RGBA{G: 255, A: 255}
	tileBlue   = color.RGBA{B: 255, A: 255}
	tileYellow = color.RGBA{R: 255, G: 255, A: 255}
	tileWhite  = color.RGBA{R: 255, G: 255, B: 255, A: 255}
)

// writePNGTile 写一张纯色 PNG 瓦片（XYZ 布局：{z}/{x}/{y}.png）。
func writePNGTile(t *testing.T, dir string, z, x, y int, c color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, testTileSize, testTileSize))
	fillRGBA(img, c)
	p := filepath.Join(dir, "tiles", strconv.Itoa(z), strconv.Itoa(x), strconv.Itoa(y)+".png")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func writePyramidMeta(t *testing.T, dir, meta string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "tiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tiles", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wmsFixture 构造带影像金字塔的已发布任务：z0 一张红瓦片，z1 四象限四色。
// 四色便于验证「拼瓦片 + 采样」的行列方向是否与 XYZ 约定一致。
func wmsFixture(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	writePyramidMeta(t, dir, `{"tile_matrix_set":"WebMercatorQuad",`+
		`"extent":[-20037508.342789244,-20037508.342789244,20037508.342789244,20037508.342789244],`+
		`"min_zoom":0,"max_zoom":1,"tile_size":4,"format":"png"}`)
	writePNGTile(t, dir, 0, 0, 0, tileRed)
	writePNGTile(t, dir, 1, 0, 0, tileGreen)  // 西北
	writePNGTile(t, dir, 1, 1, 0, tileBlue)   // 东北
	writePNGTile(t, dir, 1, 0, 1, tileYellow) // 西南
	writePNGTile(t, dir, 1, 1, 1, tileWhite)  // 东南

	store := task.NewMemoryStore()
	if err := store.Create(&task.Task{
		ID: "img-task-1", Type: "image->tiles", Output: dir,
		Status: task.StatusSucceeded, Approved: true, MinioPrefix: "tasks/img-task-1",
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, AuthEnabled: false}
	return wmsRouter(s), "img-task-1"
}

func wmsRouter(s *Server) *gin.Engine {
	r := gin.New()
	r.GET("/api/v1/wms", s.WMSKvp)
	r.GET("/api/v1/wcs", s.WCSKvp)
	// WMTS 与 WMS/WCS 共用同一套 KVP 取值器，一并挂上以便覆盖大小写不敏感测试
	r.GET("/api/v1/wmts", s.WMTSKvp)
	return r
}

func doGET(r *gin.Engine, url string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w
}

// TestWMSGetCapabilities 能力文档必须反映真实已发布图层，并声明三种坐标系。
func TestWMSGetCapabilities(t *testing.T) {
	r, id := wmsFixture(t)
	w := doGET(r, "/api/v1/wms?service=WMS&request=GetCapabilities")
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("content-type=%q, want xml", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<Name>" + id + "</Name>",
		"<CRS>EPSG:3857</CRS>",
		"<CRS>EPSG:4326</CRS>",
		"<CRS>CRS:84</CRS>",
		"<Format>image/png</Format>",
		"<Format>image/jpeg</Format>",
		"<EX_GeographicBoundingBox>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("capabilities 缺少 %q", want)
		}
	}
	// WMS 1.3.0 规定 EPSG:4326 的 BoundingBox 轴序为 lat,lon：
	// 世界范围下 minx/maxx 是纬度 ±85.05…，miny/maxy 是经度 ±180
	if !strings.Contains(body, `CRS="EPSG:4326" minx="-85.05112878" miny="-180.00000000" maxx="85.05112878" maxy="180.00000000"`) {
		t.Errorf("EPSG:4326 BoundingBox 轴序错误（应 lat,lon）:\n%s", firstLines(body, 60))
	}
	if !strings.Contains(body, `CRS="CRS:84" minx="-180.00000000" miny="-85.05112878"`) {
		t.Errorf("CRS:84 BoundingBox 应为 lon,lat")
	}
}

// TestWMSGetMapMosaicAndSampling 验证「选层级 → 拼瓦片 → 重采样」全链路：
// 世界范围 z1 的四个象限应分别取到四张不同颜色的瓦片。
func TestWMSGetMapMosaicAndSampling(t *testing.T) {
	r, id := wmsFixture(t)
	url := "/api/v1/wms?service=WMS&request=GetMap&layers=" + id +
		"&bbox=" + mercWorld + "&width=8&height=8&crs=EPSG:3857&format=image/png"
	w := doGET(r, url)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "png") {
		t.Errorf("content-type=%q, want png", ct)
	}
	if got := w.Header().Get("X-Tangis-Zoom"); got != "1" {
		t.Errorf("X-Tangis-Zoom=%q, want 1", got)
	}
	if got := w.Header().Get("X-Tangis-Missing-Tiles"); got != "0" {
		t.Errorf("X-Tangis-Missing-Tiles=%q, want 0（不应有缺瓦片）", got)
	}

	img, _, err := image.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("image size = %dx%d, want 8x8", b.Dx(), b.Dy())
	}
	// 8x8 输出被 4 张瓦片均分（每张 4x4）
	for _, c := range []struct {
		x, y int
		want color.RGBA
		name string
	}{
		{2, 2, tileGreen, "西北"},
		{5, 2, tileBlue, "东北"},
		{2, 5, tileYellow, "西南"},
		{5, 5, tileWhite, "东南"},
	} {
		got := color.RGBAModel.Convert(img.At(c.x, c.y)).(color.RGBA)
		if got != c.want {
			t.Errorf("%s 像素(%d,%d)=%v, want %v（瓦片拼接/采样方向错误）", c.name, c.x, c.y, got, c.want)
		}
	}
}

// TestWMSAxisOrderEquivalence 同一地理区域三种写法必须渲染出完全相同的图：
// 1.3.0 的 EPSG:4326（lat,lon）、1.1.1 的 srs=EPSG:4326（lon,lat）、CRS:84（lon,lat）。
func TestWMSAxisOrderEquivalence(t *testing.T) {
	r, id := wmsFixture(t)
	latLon13 := "/api/v1/wms?service=WMS&request=GetMap&layers=" + id +
		"&bbox=0,0,85.05112877980659,180&width=8&height=8&crs=EPSG:4326&version=1.3.0&format=image/png"
	lonLat84 := "/api/v1/wms?service=WMS&request=GetMap&layers=" + id +
		"&bbox=0,0,180,85.05112877980659&width=8&height=8&crs=CRS:84&format=image/png"
	lonLat111 := "/api/v1/wms?service=WMS&request=GetMap&layers=" + id +
		"&bbox=0,0,180,85.05112877980659&width=8&height=8&srs=EPSG:4326&version=1.1.1&format=image/png"

	a, b, c := doGET(r, latLon13), doGET(r, lonLat84), doGET(r, lonLat111)
	if a.Code != http.StatusOK || b.Code != http.StatusOK || c.Code != http.StatusOK {
		t.Fatalf("codes = %d/%d/%d", a.Code, b.Code, c.Code)
	}
	if !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Errorf("1.3.0(lat,lon) 与 CRS:84(lon,lat) 渲染结果不一致 —— 1.3.0 轴序处理有误")
	}
	if !bytes.Equal(c.Body.Bytes(), b.Body.Bytes()) {
		t.Errorf("1.1.1(srs, lon,lat) 与 CRS:84 渲染结果应一致")
	}
}

// TestWMSErrors 错误必须走 ServiceExceptionReport（规范载体）并给出可读原因。
func TestWMSErrors(t *testing.T) {
	r, id := wmsFixture(t)
	base := "/api/v1/wms?service=WMS&request=GetMap&width=4&height=4"
	cases := []struct {
		name string
		url  string
		code int
		ex   string
	}{
		{"未知图层", base + "&layers=nope&bbox=0,0,1,1&crs=EPSG:3857", http.StatusNotFound, "LayerNotDefined"},
		{"多图层", base + "&layers=" + id + ",other&bbox=0,0,1,1&crs=EPSG:3857", http.StatusBadRequest, "InvalidParameterValue"},
		{"不支持的 CRS", base + "&layers=" + id + "&bbox=0,0,1,1&crs=EPSG:4490", http.StatusBadRequest, "InvalidCRS"},
		{"缺 bbox", base + "&layers=" + id + "&crs=EPSG:3857", http.StatusBadRequest, "InvalidParameterValue"},
		{"bbox 元素不足", base + "&layers=" + id + "&bbox=1,2,3&crs=EPSG:3857", http.StatusBadRequest, "InvalidParameterValue"},
		{"不支持的操作", "/api/v1/wms?service=WMS&request=GetFeatureInfo", http.StatusBadRequest, "OperationNotSupported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doGET(r, c.url)
			if w.Code != c.code {
				t.Errorf("code=%d, want %d (body=%s)", w.Code, c.code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), c.ex) {
				t.Errorf("body 缺少 %s: %s", c.ex, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "se_xml") {
				t.Errorf("content-type=%q, want ServiceException XML", ct)
			}
		})
	}
}

// TestWMSRejectsNonWebMercatorPyramid WorldCRS84Quad 金字塔不做静默近似，直接报错。
func TestWMSRejectsNonWebMercatorPyramid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	writePyramidMeta(t, dir, `{"tile_matrix_set":"WorldCRS84Quad","extent":[-180,-90,180,90],`+
		`"min_zoom":0,"max_zoom":0,"tile_size":4,"format":"png"}`)
	writePNGTile(t, dir, 0, 0, 0, tileRed)

	store := task.NewMemoryStore()
	if err := store.Create(&task.Task{
		ID: "crs84-task", Type: "image->tiles", Output: dir,
		Status: task.StatusSucceeded, Approved: true, MinioPrefix: "tasks/crs84-task",
	}); err != nil {
		t.Fatal(err)
	}
	r := wmsRouter(&Server{Store: store})

	w := doGET(r, "/api/v1/wms?service=WMS&request=GetMap&layers=crs84-task"+
		"&bbox=-180,-90,180,90&width=4&height=4&crs=CRS:84&format=image/png")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "WebMercatorQuad") {
		t.Errorf("错误信息应说明仅支持 WebMercatorQuad: %s", w.Body.String())
	}
}

// TestWMSRequiresPublishedLayer 未审批任务不得作为图层对外分发。
func TestWMSRequiresPublishedLayer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	writePyramidMeta(t, dir, `{"tile_matrix_set":"WebMercatorQuad","extent":[0,0,1000,1000],`+
		`"min_zoom":0,"max_zoom":1,"tile_size":4,"format":"png"}`)
	writePNGTile(t, dir, 0, 0, 0, tileRed)

	store := task.NewMemoryStore()
	if err := store.Create(&task.Task{
		ID: "unapproved", Type: "image->tiles", Output: dir,
		Status: task.StatusSucceeded, Approved: false, MinioPrefix: "tasks/unapproved",
	}); err != nil {
		t.Fatal(err)
	}
	r := wmsRouter(&Server{Store: store})

	w := doGET(r, "/api/v1/wms?service=WMS&request=GetMap&layers=unapproved"+
		"&bbox=0,0,1000,1000&width=4&height=4&crs=EPSG:3857")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s, want 404", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "LayerNotDefined") {
		t.Errorf("body=%s", w.Body.String())
	}
}

// TestWCSLifecycle WCS：能力文档 → 覆盖描述 → 取图；TIFF 明确拒绝而非产出假 GeoTIFF。
func TestWCSLifecycle(t *testing.T) {
	r, id := wmsFixture(t)

	w := doGET(r, "/api/v1/wcs?service=WCS&request=GetCapabilities")
	if w.Code != http.StatusOK {
		t.Fatalf("capabilities code=%d", w.Code)
	}
	for _, want := range []string{"<name>" + id + "</name>", "CoverageOfferingBrief", "lonLatEnvelope"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("WCS capabilities 缺少 %q:\n%s", want, firstLines(w.Body.String(), 30))
		}
	}

	d := doGET(r, "/api/v1/wcs?service=WCS&request=DescribeCoverage&coverage="+id)
	if d.Code != http.StatusOK {
		t.Fatalf("describe code=%d", d.Code)
	}
	for _, want := range []string{"<CoverageOffering>", "<formats>image/png</formats>", "EPSG:3857"} {
		if !strings.Contains(d.Body.String(), want) {
			t.Errorf("DescribeCoverage 缺少 %q", want)
		}
	}

	g := doGET(r, "/api/v1/wcs?service=WCS&request=GetCoverage&coverage="+id+
		"&bbox=-180,-85.05112877980659,180,85.05112877980659&crs=EPSG:4326&width=8&height=8&format=image/png")
	if g.Code != http.StatusOK {
		t.Fatalf("getcoverage code=%d body=%s", g.Code, g.Body.String())
	}
	if ct := g.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type=%q, want image/png", ct)
	}
	img, _, err := image.Decode(bytes.NewReader(g.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Errorf("size=%dx%d, want 8x8", b.Dx(), b.Dy())
	}

	tf := doGET(r, "/api/v1/wcs?service=WCS&request=GetCoverage&coverage="+id+
		"&bbox=0,0,180,85&crs=EPSG:4326&width=4&height=4&format=image/tiff")
	if tf.Code != http.StatusNotImplemented {
		t.Errorf("TIFF code=%d, want 501", tf.Code)
	}
	if !strings.Contains(tf.Body.String(), "image/png") {
		t.Errorf("TIFF 拒绝信息应提示改用 PNG: %s", tf.Body.String())
	}
}

// TestWCSUnknownCoverage 未知覆盖返回 CoverageNotDefined。
func TestWCSUnknownCoverage(t *testing.T) {
	r, _ := wmsFixture(t)
	w := doGET(r, "/api/v1/wcs?service=WCS&request=GetCoverage&coverage=missing"+
		"&bbox=0,0,1,1&crs=EPSG:4326&width=4&height=4")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CoverageNotDefined") {
		t.Errorf("body=%s", w.Body.String())
	}
}

// TestParseCRS 坐标系解析（含 1.3.0 轴序开关）。
func TestParseCRS(t *testing.T) {
	cases := []struct {
		raw     string
		version string
		id      string
		latLon  bool
		lat1st  bool
		ok      bool
	}{
		{"EPSG:3857", "1.3.0", "EPSG:3857", false, false, true},
		{"epsg:900913", "1.3.0", "EPSG:3857", false, false, true},
		{"CRS:84", "1.3.0", "CRS:84", true, false, true},
		{"EPSG:4326", "1.3.0", "EPSG:4326", true, true, true},
		{"EPSG:4326", "1.1.1", "EPSG:4326", true, false, true},
		{"", "1.3.0", "", false, false, false},
		{"EPSG:4490", "1.3.0", "", false, false, false},
	}
	for _, c := range cases {
		got, ok := parseCRS(c.raw, c.version)
		if ok != c.ok {
			t.Errorf("parseCRS(%q,%q) ok=%v, want %v", c.raw, c.version, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.ID != c.id || got.IsLatLon != c.latLon || got.LatFirst != c.lat1st {
			t.Errorf("parseCRS(%q,%q)=%+v, want id=%s latLon=%v latFirst=%v",
				c.raw, c.version, got, c.id, c.latLon, c.lat1st)
		}
	}
}

// TestKVPParameterCaseInsensitive OGC 的 KVP 参数名不区分大小写
// （WMS 1.3.0 §6.2）。规范正文里最常写的就是全大写形式，gin 的 c.Query
// 却精确匹配——不做忽略大小写处理会让多数 OGC 客户端判「缺参数」。
func TestKVPParameterCaseInsensitive(t *testing.T) {
	r, id := wmsFixture(t)

	for _, url := range []string{
		"/api/v1/wms?SERVICE=WMS&REQUEST=GetCapabilities",
		"/api/v1/wms?Service=Wms&Request=GetCapabilities",
		"/api/v1/wms?service=WMS&request=GETCAPABILITIES",
	} {
		if w := doGET(r, url); w.Code != http.StatusOK {
			t.Errorf("%s → %d, want 200（大写参数名不应被判缺参）", url, w.Code)
		}
	}

	up := "/api/v1/wms?SERVICE=WMS&VERSION=1.3.0&REQUEST=GetMap&LAYERS=" + id +
		"&BBOX=" + mercWorld + "&WIDTH=8&HEIGHT=8&CRS=EPSG:3857&FORMAT=image/png"
	w := doGET(r, up)
	if w.Code != http.StatusOK {
		t.Fatalf("全大写 GetMap → %d, want 200: %s", w.Code, firstLines(w.Body.String(), 5))
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "png") {
		t.Errorf("content-type=%q, want png", ct)
	}

	// 混合大小写的 GetTile 参数（WMTS KVP 同一套取值器）
	wc := doGET(r, "/api/v1/wmts?SERVICE=WMTS&REQUEST=GetCapabilities&VERSION=1.0.0")
	if wc.Code != http.StatusOK {
		t.Errorf("WMTS 大写参数 → %d, want 200", wc.Code)
	}
}

// TestMercatorRoundTrip 投影往返一致性。
func TestMercatorRoundTrip(t *testing.T) {
	for _, p := range [][2]float64{{116.39, 39.90}, {-180, 85.05112877980659}, {0, 0}, {180, -85.05112877980659}} {
		x, y := lonLatToMerc(p[0], p[1])
		lon, lat := mercToLonLat(x, y)
		if d := lon - p[0]; d > 1e-9 || d < -1e-9 {
			t.Errorf("lon roundtrip %v → %v", p, lon)
		}
		if d := lat - p[1]; d > 1e-9 || d < -1e-9 {
			t.Errorf("lat roundtrip %v → %v", p, lat)
		}
	}
}

// firstLines 取前 n 行，用于断言失败时的可读输出。
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
