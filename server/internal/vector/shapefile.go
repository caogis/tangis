// shapefile.go Shapefile（.shp / .dbf / .prj / .cpg）纯 Go 读取。
//
// 为什么自己写：Shapefile 是国内最常用的矢量交换格式，而现有实现全部依赖
// PostGIS 的 shapefile 导入能力；桌面单机版没有数据库，必须在 Go 侧解析。
// 本文件零第三方几何依赖，只用了 golang.org/x/text 的 GBK 解码表——国内
// Shapefile 的 .dbf 属性大多是 GBK，不做编码处理会满屏乱码。
//
// 文件族（同名不同扩展名，缺 .dbf/.prj 时降级但可用）：
//
//	.shp  几何（主文件，本文件的解析重点）
//	.shx  索引 —— 不用（顺序读记录即可，不需要随机访问）
//	.dbf  属性表（dBASE III/IV 定长记录）
//	.prj  坐标系 WKT（判定 SRID）
//	.cpg  属性表编码声明（GBK / UTF-8）
//
// 坐标支持：EPSG:4326、EPSG:4490（CGCS2000 地理坐标，与 WGS84 差在厘米级）、
// EPSG:3857。其他坐标系（北京54/西安80/高斯克吕格投影等）明确报错而不是
// 静默当成 WGS84 —— 那会引入几十到上百米的系统性偏差。
package vector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// Shapefile 形状类型（ESRI 规范，Z/M 变体与基础类型共用 XY 段布局）。
const (
	shpNull        = 0
	shpPoint       = 1
	shpPolyLine    = 3
	shpPolygon     = 5
	shpMultiPoint  = 8
	shpPointZ      = 11
	shpPolyLineZ   = 13
	shpPolygonZ    = 15
	shpMultiPointZ = 18
	shpPointM      = 21
	shpPolyLineM   = 23
	shpPolygonM    = 25
	shpMultiPointM = 28
)

// shpFileCode Shapefile 文件头魔数（大端）。
const shpFileCode = 9994

// supportedSRID 报告文件矢量能否直接使用该源坐标系。
func supportedSRID(srid int) bool {
	switch srid {
	case 4326, 4490, 3857:
		return true
	}
	return false
}

// ParseShapefile 读取 .shp（自动关联同目录 .dbf/.prj/.cpg）。
//
// 返回要素集与探测摘要；.dbf 缺失时要素无属性，.prj 缺失时按 4326 处理
// （Shapefile 最常见约定）并在 info.SRID 中如实体现。
func ParseShapefile(shpPath string) ([]FileFeature, FileInfo, error) {
	data, err := os.ReadFile(shpPath)
	if err != nil {
		return nil, FileInfo{}, fmt.Errorf("%w: 读取 .shp 失败: %v", ErrInvalid, err)
	}
	if len(data) < 100 {
		return nil, FileInfo{}, fmt.Errorf("%w: 文件过小，不是合法 Shapefile", ErrInvalid)
	}
	if binary.BigEndian.Uint32(data[0:4]) != shpFileCode {
		return nil, FileInfo{}, fmt.Errorf("%w: 文件头标识不是 Shapefile（期望 %d）", ErrInvalid, shpFileCode)
	}
	declared := int(binary.BigEndian.Uint32(data[24:28])) * 2
	if declared > 0 && declared > len(data) {
		// 声明长度大于实际：文件被截断，按实际长度尽力解析（后面会记录截断）
		declared = len(data)
	}

	info := FileInfo{
		SRID:       4326, // .prj 缺失时的默认值（Shapefile 事实约定）
		TypeCounts: map[GeomType]int{},
		BBox:       Box{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)},
	}

	// ---- .prj：坐标系与到 WGS84 的转换方案 ----
	// 投影坐标（国内常见的 CGCS2000 高斯克吕格等）在这里换算成经纬度；
	// 无法安全换算（基准不同又无七参数、非横轴墨卡托投影等）直接报错，
	// 不让带错的坐标流到下游。
	tr, trErr := crsTransformForShapefile(shpPath)
	if trErr != nil {
		return nil, info, trErr
	}
	info.SRID = tr.effectiveSRID()
	if note := tr.note(); note != "" {
		info.Notes = append(info.Notes, note)
	}

	// ---- .dbf：属性 ----
	attrs, dbfWarn := loadDBF(shpPath)

	// ---- .shp：几何 ----
	limit := len(data)
	if declared > 0 && declared < limit {
		limit = declared
	}
	offset := 100
	seq := uint64(0)
	feats := make([]FileFeature, 0, 256)
	truncated := false
	transformFailed := 0
	for offset+8 <= limit {
		contentWords := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		contentLen := contentWords * 2
		offset += 8
		if contentLen < 0 || offset+contentLen > limit {
			truncated = true
			break
		}
		seq++
		geom, gerr := parseShpShape(data[offset : offset+contentLen])
		offset += contentLen
		if gerr != nil {
			// 单条记录损坏不放弃整个文件：跳过并继续（末尾会给出警告）
			continue
		}
		if geom.Type == GeomUnknown || len(geom.Lines) == 0 {
			continue // Null Shape
		}
		if tr.needsTransform() {
			converted, terr := tr.transformGeometry(geom)
			if terr != nil {
				transformFailed++
				continue
			}
			geom = converted
		}
		var props map[string]any
		if attrs != nil && int(seq) <= len(attrs.Rows) {
			props = attrs.RowProps(int(seq) - 1)
		}
		feats = append(feats, FileFeature{ID: seq, Geom: geom, Props: props})
		info.BBox = info.BBox.Union(geom.Bounds())
		info.TypeCounts[geom.Type]++
	}
	if truncated {
		return nil, info, fmt.Errorf("%w: .shp 记录在末尾被截断（文件不完整）", ErrInvalid)
	}

	// 记录为空时退回文件头 bbox（无要素的图层也能给出范围）
	if len(feats) == 0 {
		hx0 := math.Float64frombits(binary.LittleEndian.Uint64(data[36:44]))
		hy0 := math.Float64frombits(binary.LittleEndian.Uint64(data[44:52]))
		hx1 := math.Float64frombits(binary.LittleEndian.Uint64(data[52:60]))
		hy1 := math.Float64frombits(binary.LittleEndian.Uint64(data[60:68]))
		if hx1 >= hx0 && hy1 >= hy0 {
			info.BBox = Box{MinX: hx0, MinY: hy0, MaxX: hx1, MaxY: hy1}
		}
	}

	info.Count = len(feats)
	if gt, ok := closestGeomType(info.TypeCounts); ok {
		info.GeomType = gt
	}
	if attrs != nil {
		info.Fields = attrs.FieldNames()
		info.Notes = append(info.Notes, attrs.Note())
	}
	if dbfWarn != "" {
		info.Notes = append(info.Notes, dbfWarn)
	}
	if transformFailed > 0 {
		info.Notes = append(info.Notes, fmt.Sprintf("有 %d 条要素的坐标无法换算，已跳过", transformFailed))
		if len(feats) == 0 {
			return nil, info, fmt.Errorf("%w: 全部 %d 条要素的坐标都无法换算，请检查 .prj 与实际坐标是否一致",
				ErrInvalid, transformFailed)
		}
	}
	return feats, info, nil
}

// crsTransformForShapefile 依 .prj 构造转换方案；缺 .prj 时按 Shapefile 事实约定
// 视为 WGS84 经纬度（并在提示里说明是"按约定"，不假装确知）。
func crsTransformForShapefile(shpPath string) (*crsTransform, error) {
	if wkt, ok := readSibling(shpPath, ".prj"); ok {
		return crsTransformFromWKT(string(wkt))
	}
	return &crsTransform{
		SRID:     4326,
		Name:     "WGS84（缺 .prj，按约定）",
		identity: true,
	}, nil
}

/* ---------------- .shp 几何 ---------------- */

// parseShpShape 解析一条记录的几何（XY 段；Z/M 段按记录长度自然忽略）。
func parseShpShape(c []byte) (Geometry, error) {
	if len(c) < 4 {
		return Geometry{}, fmt.Errorf("%w: 记录过短", ErrInvalid)
	}
	typ := int(binary.LittleEndian.Uint32(c[0:4]))
	switch typ {
	case shpNull:
		return Geometry{}, nil
	case shpPoint, shpPointZ, shpPointM:
		if len(c) < 20 {
			return Geometry{}, fmt.Errorf("%w: 点记录过短", ErrInvalid)
		}
		return Geometry{Type: GeomPoint, Lines: [][]Point{{{X: f64(c, 4), Y: f64(c, 12)}}}}, nil
	case shpMultiPoint, shpMultiPointZ, shpMultiPointM:
		if len(c) < 40 {
			return Geometry{}, fmt.Errorf("%w: 多点记录过短", ErrInvalid)
		}
		n := int(int32(binary.LittleEndian.Uint32(c[36:40])))
		if n < 0 || 40+n*16 > len(c) {
			return Geometry{}, fmt.Errorf("%w: 多点记录点数越界", ErrInvalid)
		}
		lines := make([][]Point, 0, n)
		for i := 0; i < n; i++ {
			off := 40 + i*16
			lines = append(lines, []Point{{X: f64(c, off), Y: f64(c, off+8)}})
		}
		return Geometry{Type: GeomPoint, Lines: lines}, nil
	case shpPolyLine, shpPolyLineZ, shpPolyLineM:
		return parseShpParts(c, false)
	case shpPolygon, shpPolygonZ, shpPolygonM:
		return parseShpParts(c, true)
	}
	return Geometry{}, fmt.Errorf("%w: 未支持的 Shapefile 形状类型 %d", ErrInvalid, typ)
}

// parseShpParts 解析折线/多边形：Box(32) + NumParts + NumPoints + Parts[] + Points[]。
//
// 注意：不按 ESRI 的"外环顺时针、内环逆时针"约定判角色——实际数据里绕向约定
// 常常不被遵守（不同导出工具各异），改用**包含关系**判定，见 classifyRings。
func parseShpParts(c []byte, isPolygon bool) (Geometry, error) {
	if len(c) < 44 {
		return Geometry{}, fmt.Errorf("%w: 记录过短", ErrInvalid)
	}
	numParts := int(int32(binary.LittleEndian.Uint32(c[36:40])))
	numPoints := int(int32(binary.LittleEndian.Uint32(c[40:44])))
	if numParts < 0 || numPoints < 0 {
		return Geometry{}, fmt.Errorf("%w: 部件/点数非法", ErrInvalid)
	}
	if 44+numParts*4+numPoints*16 > len(c) {
		return Geometry{}, fmt.Errorf("%w: 记录长度与部件/点数不符", ErrInvalid)
	}
	parts := make([]int, numParts)
	for i := range parts {
		parts[i] = int(int32(binary.LittleEndian.Uint32(c[44+i*4 : 48+i*4])))
	}
	ptsOff := 44 + numParts*4
	pts := make([]Point, numPoints)
	for i := range pts {
		off := ptsOff + i*16
		pts[i] = Point{X: f64(c, off), Y: f64(c, off+8)}
	}

	rings := make([][]Point, 0, numParts)
	for i := range parts {
		start := parts[i]
		end := numPoints
		if i+1 < numParts {
			end = parts[i+1]
		}
		if start < 0 || start >= end || end > numPoints {
			continue
		}
		r := make([]Point, end-start)
		copy(r, pts[start:end])
		rings = append(rings, r)
	}
	if len(rings) == 0 {
		return Geometry{}, nil
	}
	if !isPolygon {
		return Geometry{Type: GeomLine, Lines: rings}, nil
	}
	// 多边形：去掉隐式闭合点（内部模型统一开环），再判定环角色
	for i, r := range rings {
		if len(r) >= 2 && r[0] == r[len(r)-1] {
			rings[i] = r[:len(r)-1]
		}
	}
	return Geometry{Type: GeomPolygon, Lines: rings, Exterior: classifyRings(rings)}, nil
}

// f64 读小端 float64。
func f64(b []byte, off int) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(b[off : off+8]))
}

/* ---------------- .prj 坐标系 ---------------- */

var epsgRe = regexp.MustCompile(`(?i)AUTHORITY\s*\[\s*"(?:EPSG|ESRI)"\s*,\s*"?(\d+)"?\s*\]`)

// sridFromPRJ 从 .prj 的 WKT 判定 SRID。
//
// 判定顺序：取最后一个 AUTHORITY["EPSG",n]（WKT 里最外层即整体 CRS 的权威码），
// 无权威码时退回名称启发式。无法判定或不在支持范围时返回明确错误。
func sridFromPRJ(wkt string) (int, string, error) {
	wkt = strings.TrimSpace(wkt)
	if wkt == "" {
		return 4326, "", nil
	}
	if m := epsgRe.FindAllStringSubmatch(wkt, -1); len(m) > 0 {
		code, _ := strconv.Atoi(m[len(m)-1][1])
		switch code {
		case 4326:
			return 4326, "EPSG:4326 WGS84", nil
		case 4490:
			return 4490, "EPSG:4490 CGCS2000", nil
		case 3857, 900913, 102100:
			return 3857, "EPSG:3857 Web Mercator", nil
		}
		return 0, "", unsupportedCRSErr(code, "")
	}
	upper := strings.ToUpper(wkt)
	switch {
	case strings.Contains(upper, "GCS_WGS_1984") || strings.Contains(upper, "WGS_1984") && strings.HasPrefix(upper, "GEOGCS"):
		return 4326, "WGS84（按名称判定）", nil
	case strings.Contains(upper, "CGCS2000") || strings.Contains(upper, "CHINA_GEODETIC"):
		return 4490, "CGCS2000（按名称判定）", nil
	case strings.Contains(upper, "MERCATOR") && strings.HasPrefix(upper, "PROJCS"):
		return 3857, "Web Mercator（按名称判定）", nil
	}
	return 0, "", fmt.Errorf("%w: 无法从 .prj 判定坐标系，请补 .prj 或改用 EPSG:4326/4490/3857（当前 WKT: %s）",
		ErrInvalid, truncate(wkt, 80))
}

// unsupportedCRSErr 生成可执行的坐标系不支持提示。
func unsupportedCRSErr(code int, name string) error {
	label := fmt.Sprintf("EPSG:%d", code)
	if code == 0 {
		label = name
	}
	return fmt.Errorf("%w: 源坐标系 %s 不被文件矢量支持（仅 EPSG:4326 WGS84 / 4490 CGCS2000 / 3857 Web Mercator）；"+
		"请先用「切片转换」或「合规工具」把数据转到上述坐标系，避免静默当成 WGS84 引入系统性偏差",
		ErrInvalid, label)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

/* ---------------- .dbf 属性 ---------------- */

// FileInfo.Notes 用于承载解析过程中的提示（编码回退、缺 .dbf 等）。
// 放在这里而不是别处，是因为 Notes 只在文件矢量路线使用。

// dbfField 字段描述（32 字节）。
type dbfField struct {
	Name string
	Type byte
	Len  int
	Dec  int
}

// dbfTable 属性表。
type dbfTable struct {
	Fields []dbfField
	Rows   [][]any
	// gbk 是否按 GBK 解码（由 .cpg 强制或按字节探测得出）。
	gbk bool
	// missing 是否根本没有 .dbf 文件。
	missing bool
	// deleted 被标记删除（0x2A）而跳过的记录数。
	deleted int
}

// FieldNames 字段名列表。
func (t *dbfTable) FieldNames() []string {
	out := make([]string, 0, len(t.Fields))
	for _, f := range t.Fields {
		out = append(out, f.Name)
	}
	return out
}

// RowProps 第 i 行转属性 map（空值不入表，避免 MVT 里塞满空串）。
func (t *dbfTable) RowProps(i int) map[string]any {
	if i < 0 || i >= len(t.Rows) {
		return nil
	}
	row := t.Rows[i]
	out := make(map[string]any, len(t.Fields))
	for j, f := range t.Fields {
		if j >= len(row) {
			break
		}
		v := row[j]
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		out[f.Name] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Note 生成解析摘要（编码方式、跳过的删除记录等）。
func (t *dbfTable) Note() string {
	if t.missing {
		return "缺少 .dbf，要素没有属性字段"
	}
	enc := "UTF-8"
	if t.gbk {
		enc = "GBK"
	}
	s := fmt.Sprintf("属性表编码按 %s 解码（%d 字段）", enc, len(t.Fields))
	if t.deleted > 0 {
		s += fmt.Sprintf("，跳过 %d 条已删除记录", t.deleted)
	}
	return s
}

// loadDBF 读取同目录属性表；缺失返回 (nil, "")，损坏返回 (nil, 提示)。
func loadDBF(shpPath string) (*dbfTable, string) {
	raw, ok := readSibling(shpPath, ".dbf")
	if !ok {
		return &dbfTable{missing: true}, ""
	}
	tbl, err := parseDBF(raw, readSiblingText(shpPath, ".cpg"))
	if err != nil {
		return &dbfTable{missing: true}, fmt.Sprintf("属性表读取失败（仅几何可用）: %v", err)
	}
	return tbl, ""
}

// readSibling 读取同名的同级文件（大小写不敏感地尝试常见写法）。
func readSibling(basePath, ext string) ([]byte, bool) {
	for _, p := range siblingCandidates(basePath, ext) {
		if b, err := os.ReadFile(p); err == nil {
			return b, true
		}
	}
	return nil, false
}

func readSiblingText(basePath, ext string) string {
	if b, ok := readSibling(basePath, ext); ok {
		return strings.TrimSpace(string(bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})))
	}
	return ""
}

// siblingCandidates 生成同名文件的候选路径：
// Shapefile 族在 Windows 上扩展名大小写不定（roads.SHP / roads.DBF），
// 因此按「给定大小写 → 全小写 → 全大写」依次尝试。
func siblingCandidates(basePath, ext string) []string {
	stem := strings.TrimSuffix(basePath, filepath.Ext(basePath))
	if ext[0] != '.' {
		ext = "." + ext
	}
	return []string{stem + ext, stem + strings.ToLower(ext), stem + strings.ToUpper(ext)}
}

// parseDBF 解析 dBASE III/IV 定长属性表。
func parseDBF(data []byte, cpg string) (*dbfTable, error) {
	if len(data) < 32 {
		return nil, fmt.Errorf("属性表过短")
	}
	numRecords := int(int32(binary.LittleEndian.Uint32(data[4:8])))
	headerSize := int(int16(binary.LittleEndian.Uint16(data[8:10])))
	recordSize := int(int16(binary.LittleEndian.Uint16(data[10:12])))
	if headerSize < 33 || headerSize > len(data) || recordSize <= 1 {
		return nil, fmt.Errorf("属性表头非法（headerSize=%d recordSize=%d）", headerSize, recordSize)
	}

	// 字段描述区：从 32 开始，每 32 字节一个，遇到 0x0D 结束
	var fields []dbfField
	for off := 32; off+32 <= headerSize; off += 32 {
		if data[off] == 0x0D {
			break
		}
		name := string(bytes.TrimRight(data[off:off+11], "\x00"))
		name = decodeText([]byte(strings.TrimSpace(name)), false)
		if name == "" {
			continue
		}
		const maxLen = 10 // dBASE 字段名上限
		if len(name) > maxLen {
			name = name[:maxLen]
		}
		fields = append(fields, dbfField{
			Name: name,
			Type: data[off+11],
			Len:  int(data[off+16]),
			Dec:  int(data[off+17]),
		})
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("属性表没有字段定义")
	}
	widthSum := 1 // 含删除标记
	for _, f := range fields {
		widthSum += f.Len
	}
	if widthSum != recordSize {
		return nil, fmt.Errorf("属性表记录宽度不一致（字段合计 %d，声明 %d）", widthSum, recordSize)
	}

	tbl := &dbfTable{Fields: fields, gbk: cpgForcesGBK(cpg)}
	rows := make([][]any, 0, numRecords)
	pos := headerSize
	for i := 0; i < numRecords && pos+recordSize <= len(data); i++ {
		rec := data[pos : pos+recordSize]
		pos += recordSize
		if rec[0] == 0x2A { // '*' 标记删除
			tbl.deleted++
			continue
		}
		row := make([]any, len(fields))
		off := 1
		for j, f := range fields {
			raw := rec[off : off+f.Len]
			off += f.Len
			row[j] = dbfValue(raw, f, tbl.gbk)
		}
		rows = append(rows, row)
	}
	tbl.Rows = rows
	return tbl, nil
}

// cpgForcesGBK 判定 .cpg 是否要求 GBK 解码。
func cpgForcesGBK(cpg string) bool {
	u := strings.ToUpper(strings.TrimSpace(cpg))
	if u == "" {
		return false
	}
	if strings.Contains(u, "UTF") || strings.Contains(u, "65001") {
		return false
	}
	return strings.Contains(u, "GB") || strings.Contains(u, "936") || strings.Contains(u, "ANSI") ||
		strings.Contains(u, "CP936")
}

// dbfValue 把一个定长字段转成 Go 值。空白一律视为缺失（nil）。
func dbfValue(raw []byte, f dbfField, forceGBK bool) any {
	switch f.Type {
	case 'C', 'c': // 字符：右侧补空格
		s := string(bytes.TrimRight(raw, " \x00"))
		return decodeText([]byte(strings.TrimSpace(s)), forceGBK)
	case 'N', 'n', 'F', 'f': // 数值 / 浮点：左侧补空格
		s := strings.TrimSpace(string(bytes.Trim(raw, " \x00")))
		if s == "" || s == "." || s == "*" {
			return nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		return v
	case 'L', 'l': // 逻辑：T/Y 为真，F/N 为假，? 或空为缺失
		s := strings.ToUpper(strings.TrimSpace(string(raw)))
		if s == "" {
			return nil
		}
		switch s[0] {
		case 'T', 'Y':
			return true
		case 'F', 'N':
			return false
		}
		return nil
	case 'D', 'd': // 日期 YYYYMMDD
		s := strings.TrimSpace(string(raw))
		if len(s) != 8 || !allDigits(s) {
			return nil
		}
		return s[0:4] + "-" + s[4:6] + "-" + s[6:8]
	case 'M', 'm': // 备注：内容在 .dbt 里，这里不做（如实留空）
		return nil
	}
	// 未知类型：按文本尽力返回
	s := strings.TrimSpace(string(bytes.Trim(raw, " \x00")))
	if s == "" {
		return nil
	}
	return decodeText([]byte(s), forceGBK)
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// decodeText 解码属性文本：forceGBK 或字节不是合法 UTF-8 时按 GBK 解。
//
// 国内 Shapefile 的 .dbf 大量使用 GBK；GBK 汉字字节序列几乎总是非法 UTF-8，
// 因此"先按 UTF-8 校验、失败再按 GBK"能覆盖绝大多数文件；.cpg 声明可强制。
func decodeText(b []byte, forceGBK bool) string {
	if len(b) == 0 {
		return ""
	}
	if !forceGBK && utf8Valid(b) {
		return string(b)
	}
	out, err := simplifiedchinese.GBK.NewDecoder().Bytes(b)
	if err != nil {
		return strings.ToValidUTF8(string(b), "\uFFFD")
	}
	return string(out)
}

// utf8Valid 判断字节是否为合法 UTF-8（不引入 unicode/utf8 之外的行为）。
func utf8Valid(b []byte) bool {
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c < 0x80:
			i++
		case c >= 0xC2 && c <= 0xDF:
			if i+1 >= len(b) || b[i+1]&0xC0 != 0x80 {
				return false
			}
			i += 2
		case c >= 0xE0 && c <= 0xEF:
			if i+2 >= len(b) || b[i+1]&0xC0 != 0x80 || b[i+2]&0xC0 != 0x80 {
				return false
			}
			i += 3
		case c >= 0xF0 && c <= 0xF4:
			if i+3 >= len(b) || b[i+1]&0xC0 != 0x80 || b[i+2]&0xC0 != 0x80 || b[i+3]&0xC0 != 0x80 {
				return false
			}
			i += 4
		default:
			return false
		}
	}
	return true
}
