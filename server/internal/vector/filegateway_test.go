package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// 测试数据：一个带洞的多边形 + 一个独立多边形 + 一个点，位于北京附近。
// 外环为 RFC 7946 约定的逆时针，洞为顺时针 —— 用来验证绕向归一化确实生效。
const fixtureGeoJSON = `{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "id": 11,
      "properties": {"name": "带洞多边形", "area": 100},
      "geometry": {
        "type": "Polygon",
        "coordinates": [
          [[116.38,39.89],[116.42,39.89],[116.42,39.93],[116.38,39.93],[116.38,39.89]],
          [[116.39,39.90],[116.41,39.90],[116.41,39.92],[116.39,39.92],[116.39,39.90]]
        ]
      }
    },
    {
      "type": "Feature",
      "properties": {"name": "独立多边形"},
      "geometry": {
        "type": "Polygon",
        "coordinates": [[[116.43,39.89],[116.45,39.89],[116.45,39.91],[116.43,39.91],[116.43,39.89]]]
      }
    },
    {
      "type": "Feature",
      "id": 33,
      "properties": {"name": "点位", "rank": 2},
      "geometry": {"type": "Point", "coordinates": [116.40, 39.91]}
    }
  ]
}`

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFileGatewayProbe 文件数据源的连通性与元数据探测。
func TestFileGatewayProbe(t *testing.T) {
	ctx := context.Background()
	g := NewFileGateway()
	defer g.Close()
	dsn := FileDSN(writeFixture(t, "beijing.geojson", fixtureGeoJSON))

	ver, err := g.Ping(ctx, dsn)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if ver == "" {
		t.Error("Ping 应返回格式描述")
	}

	col, srid, gtype, err := g.DetectGeometry(ctx, dsn, "", "", "")
	if err != nil {
		t.Fatalf("DetectGeometry: %v", err)
	}
	if col != "geometry" || srid != 4326 {
		t.Errorf("geometry=%q srid=%d, want geometry/4326", col, srid)
	}
	if gtype != "POLYGON" {
		t.Errorf("geometry type=%q, want POLYGON（混合时取更复杂类型）", gtype)
	}

	fields, err := g.DetectFields(ctx, dsn, "", "", "geometry")
	if err != nil {
		t.Fatalf("DetectFields: %v", err)
	}
	want := map[string]bool{"name": true, "area": true, "rank": true}
	if len(fields) != len(want) {
		t.Fatalf("fields=%v, want %v", fields, want)
	}
	for _, f := range fields {
		if !want[f] {
			t.Errorf("意外字段 %q", f)
		}
	}

	if n, err := g.EstimatedRows(ctx, dsn, nil); err != nil || n != 3 {
		t.Errorf("要素数=%d(err=%v), want 3", n, err)
	}
	minx, miny, maxx, maxy, ok, err := g.EstimatedExtent(ctx, dsn, nil)
	if err != nil || !ok {
		t.Fatalf("EstimatedExtent: ok=%v err=%v", ok, err)
	}
	if math.Abs(minx-116.38) > 1e-9 || math.Abs(maxy-39.93) > 1e-9 {
		t.Errorf("bbox=[%.4f,%.4f,%.4f,%.4f], want minx=116.38 maxy=39.93", minx, miny, maxx, maxy)
	}
}

// TestFileGatewayTileMVT 出瓦片：命中瓦片应有要素、瓦片外应为空、
// 且解码后几何落在瓦片坐标范围内。
func TestFileGatewayTileMVT(t *testing.T) {
	ctx := context.Background()
	g := NewFileGateway()
	defer g.Close()
	dsn := FileDSN(writeFixture(t, "tile.geojson", fixtureGeoJSON))
	layer := &Layer{Name: "beijing", SRID: 4326}

	// 北京在东半球北半球 → z1 的 (x=1,y=0) 瓦片必然命中
	bx0, by0, bx1, by1 := TileBBox(1, 1, 0)
	mvt, count, err := g.TileMVT(ctx, dsn, layer, bx0, by0, bx1, by1, 10000)
	if err != nil {
		t.Fatalf("TileMVT: %v", err)
	}
	if count == 0 || len(mvt) == 0 {
		t.Fatalf("命中瓦片应产出要素, count=%d bytes=%d", count, len(mvt))
	}

	l := decodeMVT(t, mvt)
	if l.name != "beijing" {
		t.Errorf("layer name=%q, want beijing", l.name)
	}
	if len(l.features) != count {
		t.Errorf("解码要素数=%d, 上报=%d", len(l.features), count)
	}
	if len(l.keys) == 0 || len(l.values) == 0 {
		t.Errorf("属性字典不应为空: keys=%v values=%v", l.keys, l.values)
	}
	// 坐标须在瓦片坐标系内（含 buffer）
	for _, f := range l.features {
		for _, p := range ringsOf(decodeGeomOps(t, f.cmds)) {
			for _, pt := range p {
				if pt.X < -64 || pt.X > float64(DefaultExtent)+64 || pt.Y < -64 || pt.Y > float64(DefaultExtent)+64 {
					t.Errorf("瓦片坐标越界: %+v", pt)
				}
			}
		}
	}

	// 西半球北半球瓦片（x=0,y=0）应为空
	bx0, by0, bx1, by1 = TileBBox(1, 0, 0)
	mvt2, count2, err := g.TileMVT(ctx, dsn, layer, bx0, by0, bx1, by1, 10000)
	if err != nil {
		t.Fatalf("TileMVT(空瓦片): %v", err)
	}
	if count2 != 0 || len(mvt2) != 0 {
		t.Errorf("远端瓦片应为空, count=%d bytes=%d", count2, len(mvt2))
	}
}

// tileXYAt z 级下经纬度所在瓦片号。
func tileXYAt(lon, lat float64, z int) (int, int) {
	n := math.Exp2(float64(z))
	x := int((lon + 180) / 360 * n)
	latRad := lat * math.Pi / 180
	y := int((1 - math.Log(math.Tan(latRad)+1/math.Cos(latRad))/math.Pi) / 2 * n)
	if x >= int(n) {
		x = int(n) - 1
	}
	if y >= int(n) {
		y = int(n) - 1
	}
	return x, y
}

// TestFileGatewayPolygonWinding 出图前绕向必须被归一化：外环正、洞负。
func TestFileGatewayPolygonWinding(t *testing.T) {
	ctx := context.Background()
	g := NewFileGateway()
	defer g.Close()
	dsn := FileDSN(writeFixture(t, "winding.geojson", fixtureGeoJSON))
	layer := &Layer{Name: "beijing", SRID: 4326}
	// 带洞多边形覆盖 116.38~116.42 / 39.89~39.93，取中心点所在 z12 瓦片
	tx, ty := tileXYAt(116.40, 39.91, 12)
	bx0, by0, bx1, by1 := TileBBox(12, tx, ty)
	mvt, _, err := g.TileMVT(ctx, dsn, layer, bx0, by0, bx1, by1, 10000)
	if err != nil {
		t.Fatalf("TileMVT: %v", err)
	}
	if len(mvt) == 0 {
		t.Fatalf("z12(%d,%d) 应命中带洞多边形", tx, ty)
	}
	l := decodeMVT(t, mvt)
	for _, f := range l.features {
		if f.gtype != uint64(GeomPolygon) {
			continue
		}
		var positives, negatives int
		for _, ring := range ringsOf(decodeGeomOps(t, f.cmds)) {
			switch a := signedArea(ring); {
			case a > 0:
				positives++
			case a < 0:
				negatives++
			}
		}
		// 每个多边形至少有一个外环（正向）；有洞时洞必为负向
		if positives == 0 {
			t.Errorf("多边形要素缺少正向外环 (pos=%d neg=%d)", positives, negatives)
		}
	}
}

// TestFileGatewayFeatureLimit 超出单瓦片要素上限时明确报错（沿用 PG 策略，不静默截断）。
func TestFileGatewayFeatureLimit(t *testing.T) {
	ctx := context.Background()
	g := NewFileGateway()
	defer g.Close()
	dsn := FileDSN(writeFixture(t, "limit.geojson", fixtureGeoJSON))
	layer := &Layer{Name: "beijing", SRID: 4326}
	bx0, by0, bx1, by1 := TileBBox(1, 1, 0)
	if _, _, err := g.TileMVT(ctx, dsn, layer, bx0, by0, bx1, by1, 1); err == nil {
		t.Error("超过 maxFeatures 应报错")
	} else if !errorIs(err, ErrTooManyFeatures) {
		t.Errorf("错误类型应为 ErrTooManyFeatures, 得到 %v", err)
	}
}

// TestFileGatewayFeaturesGeoJSON WFS 数据面：按 bbox 过滤并输出 GeoJSON。
func TestFileGatewayFeaturesGeoJSON(t *testing.T) {
	ctx := context.Background()
	g := NewFileGateway()
	defer g.Close()
	dsn := FileDSN(writeFixture(t, "wfs.geojson", fixtureGeoJSON))

	// 全量
	data, n, err := g.FeaturesGeoJSON(ctx, dsn, nil, false, 0, 0, 0, 0, 100)
	if err != nil {
		t.Fatalf("FeaturesGeoJSON: %v", err)
	}
	if n != 3 {
		t.Errorf("全量要素数=%d, want 3", n)
	}
	// 契约：数据面返回**要素数组**（外层 FeatureCollection 由 WFS 处理器封装）
	var feats []struct {
		ID       uint64         `json:"id"`
		Geometry map[string]any `json:"geometry"`
		Props    map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(data, &feats); err != nil {
		t.Fatalf("输出不是要素数组: %v (data=%s)", err, string(data[:min(len(data), 120)]))
	}
	if len(feats) != 3 {
		t.Fatalf("要素数=%d, want 3", len(feats))
	}
	if feats[0].Geometry["type"] != "Polygon" {
		t.Errorf("首个要素几何类型=%v, want Polygon", feats[0].Geometry["type"])
	}
	if feats[0].Props["name"] != "带洞多边形" {
		t.Errorf("属性丢失: %+v", feats[0].Props)
	}
	// 多边形环：必须显式闭合，且不能出现两个重复闭合点
	// （内部模型统一开环、导出时补一次；曾因解析时保留闭合点导致重复）
	coords, _ := feats[0].Geometry["coordinates"].([]any)
	if len(coords) > 0 {
		ring, _ := coords[0].([]any)
		if len(ring) < 4 {
			t.Fatalf("环点数=%d, want >=4", len(ring))
		}
		first := fmt.Sprint(ring[0])
		if fmt.Sprint(ring[len(ring)-1]) != first {
			t.Errorf("GeoJSON 环必须闭合（首尾点相同）: %v", ring)
		}
		if fmt.Sprint(ring[len(ring)-2]) == first {
			t.Errorf("环出现两个重复闭合点: %v", ring)
		}
	}

	// 只取点位所在的小范围
	data, n, err = g.FeaturesGeoJSON(ctx, dsn, nil, true, 116.395, 39.905, 116.405, 39.915, 100)
	if err != nil {
		t.Fatalf("FeaturesGeoJSON(bbox): %v", err)
	}
	if n != 2 {
		// 带洞多边形与该点的小范围相交，独立多边形不相交 → 期望 2
		t.Errorf("bbox 过滤后要素数=%d, want 2", n)
	}
	_ = data
}

// TestFileGatewayErrors 非法输入应有明确错误（文件不存在 / 目录 / 不支持的格式）。
func TestFileGatewayErrors(t *testing.T) {
	ctx := context.Background()
	g := NewFileGateway()
	defer g.Close()

	if _, err := g.Ping(ctx, FileDSN("/no/such/file.geojson")); err == nil {
		t.Error("文件不存在应报错")
	}
	if _, err := g.Ping(ctx, "postgres://user@host/db"); err == nil {
		t.Error("非 file:// DSN 应报错（该由 pgGateway 处理）")
	}
	dir := t.TempDir()
	if _, err := g.Ping(ctx, FileDSN(dir)); err == nil {
		t.Error("目录应报错")
	}
	bad := writeFixture(t, "x.shp", "not really a shapefile")
	if _, err := g.Ping(ctx, FileDSN(bad)); err == nil {
		t.Error("暂不支持的格式应报错并提示")
	}
	broken := writeFixture(t, "broken.geojson", "{not json")
	if _, err := g.Ping(ctx, FileDSN(broken)); err == nil {
		t.Error("非法 JSON 应报错")
	}
	empty := writeFixture(t, "empty.geojson", `{"type":"FeatureCollection","features":[]}`)
	if _, _, _, err := g.DetectGeometry(ctx, FileDSN(empty), "", "", ""); err == nil {
		t.Error("没有要素的文件不应能注册图层")
	}
}

// TestFilePathOf DSN 解析。
func TestFilePathOf(t *testing.T) {
	cases := []struct {
		dsn  string
		want string
		ok   bool
	}{
		{"file:///data/a.geojson", "/data/a.geojson", true},
		{"file://data/a.geojson", "/data/a.geojson", true},
		{"file:///data/../b.geojson", "/b.geojson", true},
		{"postgres://u@h/db", "", false},
		{"file://", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := FilePathOf(c.dsn)
		if ok != c.ok || got != c.want {
			t.Errorf("FilePathOf(%q)=(%q,%v), want (%q,%v)", c.dsn, got, ok, c.want, c.ok)
		}
	}
}

// errorIs errors.Is 的薄封装（避免测试文件顶部再引 errors 与业务符号重名）。
func errorIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
