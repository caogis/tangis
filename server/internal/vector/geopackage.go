// geopackage.go GeoPackage（.gpkg）纯 Go 读取。
//
// 为什么复用 modernc.org/sqlite：GeoPackage 本质是一个带规范表的 SQLite 库，
// 而项目已经为桌面版的元数据库引入了纯 Go 的 modernc.org/sqlite（internal/localdb），
// 复用它**不增加任何二进制体积**，也不需要 cgo——保持了桌面版三平台交叉编译能力。
//
// GeoPackage 结构（OGC 12-128r17）：
//
//	gpkg_spatial_ref_sys   坐标系定义（srs_id → EPSG 码 / WKT）
//	gpkg_contents          数据集清单（data_type = 'features' 为要素表）
//	gpkg_geometry_columns  要素表的几何列（列名、几何类型、srs_id）
//	<用户表>                要素数据；几何列是 GeoPackage 二进制（GP 头 + WKB）
//
// 与单图层格式（GeoJSON/Shapefile）的关键差异：**一个 .gpkg 可含多个要素表**，
// 因此这里列出表清单，由上层为每个表注册一个图层。
//
// 安全边界：表名/列名来自文件内容，进入 SQL 前一律用 SQLite 标准的双引号转义
// （内部双引号翻倍），并拒绝含 NUL 的名字——不做白名单（GeoPackage 表名允许
// 中文/点号等），但必须杜绝拼接注入。
package vector

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // 注册 database/sql 驱动名 "sqlite"
)

// GeoPackageTable 一个要素表的探测结果。
type GeoPackageTable struct {
	// Name 表名（SQLite 标识符，可能含中文/点号）。
	Name string
	// GeometryCol 几何列名。
	GeometryCol string
	// GeomType gpkg_geometry_columns 声明的几何类型（含 Z/M 后缀）。
	GeomType string
	// SRID 坐标系（由 gpkg_spatial_ref_sys 判定为 EPSG 码）。
	SRID int
}

// openGeoPackage 以只读方式打开 .gpkg（避免在用户数据旁生成 -wal/-journal）。
func openGeoPackage(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("%w: 打开 GeoPackage 失败: %v", ErrInvalid, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: 不是合法的 SQLite 文件（或缺少读取权限）: %v", ErrInvalid, err)
	}
	return db, nil
}

// quoteIdent SQLite 标识符安全引用：双引号包裹 + 内部双引号翻倍。
// 表名/列名来自用户文件，不能走白名单（合法 GeoPackage 允许中文表名），
// 因此必须用标准的引用转义，并拒绝含 NUL 的异常名字。
func quoteIdent(s string) (string, error) {
	if s == "" || len(s) > 128 || strings.ContainsRune(s, 0) {
		return "", fmt.Errorf("%w: 非法的 SQL 标识符 %q", ErrInvalid, s)
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`, nil
}

// pickTable 在多要素表容器里选表：table 为空且只有一个表时取该表。
func pickTable(tabs []GeoPackageTable, table string) *GeoPackageTable {
	if len(tabs) == 0 {
		return nil
	}
	if table == "" {
		if len(tabs) == 1 {
			return &tabs[0]
		}
		return nil
	}
	for i := range tabs {
		if tabs[i].Name == table {
			return &tabs[i]
		}
	}
	return nil
}

// normalizeGeomTypeName 把 GeoPackage 声明的几何类型统一成内部写法。
//
// gpkg 规范里 Z/M 变体写作「POLYGON Z」「POLYGON ZM」（**带空格**），
// 而 GeoJSON/Shapefile 路线的几何类型来自实际要素推断，结果为 POINT/
// LINESTRING/POLYGON。这里统一口径：去 MULTI 前缀与 Z/M 后缀、
// GEOMETRYCOLLECTION 归并为 GEOMETRY，使三种来源在前端呈现一致。
func normalizeGeomTypeName(s string) string {
	u := strings.ToUpper(strings.TrimSpace(s))
	u = strings.TrimSuffix(u, "ZM")
	u = strings.TrimSuffix(u, "Z")
	u = strings.TrimSuffix(u, "M")
	u = strings.TrimSpace(u)
	switch u {
	case "GEOMETRYCOLLECTION":
		return "GEOMETRY"
	}
	return strings.TrimPrefix(u, "MULTI")
}

// ListGeoPackageTables 列出 .gpkg 中的要素表（不含纯属性表与栅格表）。
func ListGeoPackageTables(path string) ([]GeoPackageTable, error) {
	db, err := openGeoPackage(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT c.table_name, g.column_name, g.geometry_type_name, c.srs_id
		FROM gpkg_contents AS c
		JOIN gpkg_geometry_columns AS g ON g.table_name = c.table_name
		WHERE c.data_type = 'features'
		ORDER BY c.table_name`)
	if err != nil {
		return nil, fmt.Errorf("%w: 不是合法 GeoPackage（缺少 gpkg_contents / gpkg_geometry_columns）: %v", ErrInvalid, err)
	}
	defer rows.Close()

	var out []GeoPackageTable
	for rows.Next() {
		var t GeoPackageTable
		var srsID any
		if err := rows.Scan(&t.Name, &t.GeometryCol, &t.GeomType, &srsID); err != nil {
			return nil, fmt.Errorf("%w: 读取 GeoPackage 清单失败: %v", ErrInvalid, err)
		}
		t.SRID = srsFromValue(srsID)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 读取 GeoPackage 清单失败: %v", ErrInvalid, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: GeoPackage 里没有要素表（可能只含栅格或纯属性表）", ErrInvalid)
	}
	// srs_id 为 -1/0 或无法映射时，尝试从 gpkg_spatial_ref_sys 的定义里解析
	if err := resolveGPKGSRS(db, out); err != nil {
		return nil, err
	}
	return out, nil
}

// srsFromValue 把 srs_id 列值转 int（SQLite 可能给 int64 或 []byte）。
func srsFromValue(v any) int {
	switch t := v.(type) {
	case int64:
		return int(t)
	case float64:
		return int(t)
	case []byte:
		n := 0
		for _, c := range t {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			}
		}
		return n
	}
	return 0
}

// resolveGPKGSRS 为无法直接映射成 EPSG 码的 srs_id 解析 gpkg_spatial_ref_sys。
//
// GeoPackage 允许 srs_id 是任意整数（organization 可能是 EPSG、NONE 或自定义），
// 因此先用 organization='EPSG' 的 organization_coordsys_id，再退回解析 WKT 定义。
func resolveGPKGSRS(db *sql.DB, tables []GeoPackageTable) error {
	need := false
	for _, t := range tables {
		if !supportedSRID(t.SRID) {
			need = true
			break
		}
	}
	if !need {
		return nil
	}
	rows, err := db.Query(`SELECT srs_id, COALESCE(organization,''), COALESCE(organization_coordsys_id,0), COALESCE(definition,'') FROM gpkg_spatial_ref_sys`)
	if err != nil {
		return nil // 表缺失不算致命：下面按"不支持坐标系"处理
	}
	defer rows.Close()
	byID := map[int]int{}
	for rows.Next() {
		var id, orgID int64
		var org, def string
		if err := rows.Scan(&id, &org, &orgID, &def); err != nil {
			continue
		}
		if strings.EqualFold(org, "EPSG") && orgID != 0 {
			byID[int(id)] = int(orgID)
			continue
		}
		if def != "" {
			if code, _, perr := sridFromPRJ(def); perr == nil {
				byID[int(id)] = code
			}
		}
	}
	for i := range tables {
		if code, ok := byID[tables[i].SRID]; ok {
			tables[i].SRID = code
		}
	}
	return nil
}

// ReadGeoPackageTable 读取一个要素表的全部要素与探测摘要。
func ReadGeoPackageTable(path, table, geomCol string) ([]FileFeature, FileInfo, error) {
	info := FileInfo{
		SRID:       0,
		TypeCounts: map[GeomType]int{},
		BBox:       Box{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)},
	}
	db, err := openGeoPackage(path)
	if err != nil {
		return nil, info, err
	}
	defer db.Close()

	// 表清单：拿到几何列名与坐标系（未显式给出时）
	tabs, err := ListGeoPackageTables(path)
	if err != nil {
		return nil, info, err
	}
	var meta *GeoPackageTable
	if table == "" && len(tabs) == 1 {
		meta = &tabs[0]
	} else {
		for i := range tabs {
			if tabs[i].Name == table {
				meta = &tabs[i]
				break
			}
		}
	}
	if meta == nil {
		if table == "" {
			names := make([]string, 0, len(tabs))
			for _, t := range tabs {
				names = append(names, t.Name)
			}
			return nil, info, fmt.Errorf("%w: GeoPackage 含多个要素表（%s），请指定表名",
				ErrInvalid, strings.Join(names, "、"))
		}
		return nil, info, fmt.Errorf("%w: GeoPackage 里没有要素表 %q", ErrInvalid, table)
	}
	table = meta.Name
	if geomCol == "" {
		geomCol = meta.GeometryCol
	}
	// 坐标系：优先用 gpkg_spatial_ref_sys 的 WKT 定义（带完整投影参数），
	// 缺定义时退回 srs_id 数值。投影坐标在这里换算成经纬度。
	tr, trErr := geoPackageCRSTransformDB(db, meta.SRID)
	if trErr != nil {
		return nil, info, trErr
	}
	if note := tr.note(); note != "" {
		info.Notes = append(info.Notes, note)
	}

	// 列信息：几何列固定放第一列（便于定位）；整型主键只作要素 ID、不进属性
	cols, err := tableColumns(db, table)
	if err != nil {
		return nil, info, err
	}
	hasGeom := false
	pkName := ""
	for _, c := range cols {
		if c.Name == geomCol {
			hasGeom = true
		}
		if pkName == "" && c.PK == 1 && strings.Contains(strings.ToUpper(c.Type), "INT") {
			pkName = c.Name
		}
	}
	if !hasGeom {
		return nil, info, fmt.Errorf("%w: 表 %q 里找不到几何列 %q", ErrInvalid, table, geomCol)
	}
	selects := []string{quoteMust(geomCol)}
	var propNames []string
	for _, c := range cols {
		if c.Name == geomCol || c.Name == pkName {
			continue
		}
		selects = append(selects, quoteMust(c.Name))
		propNames = append(propNames, c.Name)
	}
	fields := append([]string(nil), propNames...)
	sort.Strings(fields)

	tbl, err := quoteIdent(table)
	if err != nil {
		return nil, info, err
	}
	rows, err := db.Query("SELECT " + strings.Join(selects, ", ") + " FROM " + tbl)
	if err != nil {
		return nil, info, fmt.Errorf("%w: 查询表 %q 失败: %v", ErrInvalid, table, err)
	}
	defer rows.Close()

	feats := make([]FileFeature, 0, 1024)
	var scanned, skipped uint64
	for rows.Next() {
		scanned++
		vals := make([]any, len(selects))
		ptrs := make([]any, len(selects))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, info, fmt.Errorf("%w: 读取表 %q 失败: %v", ErrInvalid, table, err)
		}
		blob, _ := vals[0].([]byte)
		geom, gerr := parseGeoPackageGeometry(blob)
		if gerr != nil || geom.Type == GeomUnknown || len(geom.Lines) == 0 {
			skipped++ // 空几何/损坏几何跳过，不放弃整表
			continue
		}
		if tr.needsTransform() {
			converted, terr := tr.transformGeometry(geom)
			if terr != nil {
				skipped++
				continue
			}
			geom = converted
		}
		var props map[string]any
		for i, name := range propNames {
			v := normalizeSQLValue(vals[i+1])
			if v == nil {
				continue
			}
			if s, ok := v.(string); ok && s == "" {
				continue
			}
			if props == nil {
				props = make(map[string]any, len(propNames))
			}
			props[name] = v
		}
		feats = append(feats, FileFeature{ID: scanned, Geom: geom, Props: props})
		info.BBox = info.BBox.Union(geom.Bounds())
		info.TypeCounts[geom.Type]++
	}
	if err := rows.Err(); err != nil {
		return nil, info, fmt.Errorf("%w: 读取表 %q 失败: %v", ErrInvalid, table, err)
	}

	info.Count = len(feats)
	// 记录换算后的实际坐标系（不是源坐标系码）
	info.SRID = tr.effectiveSRID()
	info.Fields = fields
	if gt, ok := closestGeomType(info.TypeCounts); ok {
		info.GeomType = gt
	}
	if skipped > 0 {
		info.Notes = append(info.Notes, fmt.Sprintf("跳过 %d 条空/损坏/无法换算几何记录", skipped))
	}
	info.Notes = append(info.Notes, fmt.Sprintf("GeoPackage 表 %s（几何列 %s）", table, geomCol))
	return feats, info, nil
}

// geoPackageCRSTransformDB 依 srs_id 构造转换方案（db 已打开）。
//
// 优先用 definition 里的 WKT：它带着中央经线、尺度因子、假东假北、椭球，
// 照它算最准，也天然覆盖非标准中央经线与自定义坐标系。定义缺失或为
// "undefined" 时退回 EPSG 码（只认经纬度/墨卡托三种）。
func geoPackageCRSTransformDB(db *sql.DB, srsID int) (*crsTransform, error) {
	var org, def string
	var orgID int64
	row := db.QueryRow(`SELECT COALESCE(organization,''), COALESCE(organization_coordsys_id,0),
		COALESCE(definition,'') FROM gpkg_spatial_ref_sys WHERE srs_id = ?`, srsID)
	if err := row.Scan(&org, &orgID, &def); err == nil {
		trimmed := strings.TrimSpace(def)
		if trimmed != "" && !strings.EqualFold(trimmed, "undefined") {
			tr, terr := crsTransformFromWKT(trimmed)
			if terr != nil {
				// 定义本身有问题就如实报错，不悄悄降级成"按码猜"
				return nil, terr
			}
			if tr.SRID == 0 && strings.EqualFold(org, "EPSG") && orgID != 0 {
				tr.SRID = int(orgID)
			}
			return tr, nil
		}
		if strings.EqualFold(org, "EPSG") && orgID != 0 {
			srsID = int(orgID)
		}
	}
	switch srsID {
	case 4326, 4490:
		return &crsTransform{SRID: srsID, identity: true}, nil
	case 3857:
		return &crsTransform{SRID: 3857, mercator: true}, nil
	}
	return nil, unsupportedCRSErr(srsID, "")
}

// geoPackageCRSTransform 按路径构造转换方案（自行开关数据库）。
func geoPackageCRSTransform(path string, srsID int) (*crsTransform, error) {
	db, err := openGeoPackage(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return geoPackageCRSTransformDB(db, srsID)
}

// tableColumn SQLite 列描述（PRAGMA table_info）。
type tableColumn struct {
	Name string
	Type string
	PK   int
}

// tableColumns 读表结构。
func tableColumns(db *sql.DB, table string) ([]tableColumn, error) {
	tbl, err := quoteIdent(table)
	if err != nil {
		return nil, err
	}
	// PRAGMA 不接受参数占位符，但标识符已安全引用
	rows, err := db.Query("PRAGMA table_info(" + tbl + ")")
	if err != nil {
		return nil, fmt.Errorf("%w: 读取表结构失败: %v", ErrInvalid, err)
	}
	defer rows.Close()
	var out []tableColumn
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("%w: 读取表结构失败: %v", ErrInvalid, err)
		}
		out = append(out, tableColumn{Name: name, Type: typ, PK: pk})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: 表 %q 不存在或没有列", ErrInvalid, table)
	}
	return out, nil
}

// quoteMust quoteIdent 的 panic 版（仅用于已校验过的名字）。
func quoteMust(s string) string {
	q, err := quoteIdent(s)
	if err != nil {
		panic(err)
	}
	return q
}

// normalizeSQLValue 把 database/sql 的值转成可入 MVT/GeoJSON 的形式。
func normalizeSQLValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	case int64:
		return t
	case float64:
		return t
	case bool:
		return t
	case string:
		return t
	case int:
		return int64(t)
	}
	return fmt.Sprint(v)
}

/* ---------------- 几何：GeoPackage 二进制（GP 头 + WKB） ---------------- */

// parseGeoPackageGeometry 解析 GeoPackage 几何 BLOB。
//
// 结构（OGC 12-128r17 §2.1.3）：
//
//	magic    2 字节 'G','P'
//	version  1 字节
//	flags    1 字节：bit0 字节序（0=大端）、bit1-3 包络类型、bit4 空几何标记
//	srs_id   4 字节（按 flags 的字节序）
//	envelope 0/32/48/48/64 字节（随包络类型）
//	WKB      标准 OGC WKB（每个子几何自带字节序与类型头）
func parseGeoPackageGeometry(blob []byte) (Geometry, error) {
	if len(blob) < 8 || blob[0] != 'G' || blob[1] != 'P' {
		return Geometry{}, fmt.Errorf("%w: 不是 GeoPackage 几何（缺少 GP 标识）", ErrInvalid)
	}
	flags := blob[3]
	if flags&0x10 != 0 {
		return Geometry{}, nil // 空几何
	}
	// 注：blob[4:8] 是 srs_id，这里不用——坐标系统一取 gpkg_geometry_columns.srs_id，
	// 避免同一图层内不同记录携带不同 srs_id 时产生歧义。
	// 包络长度
	envLen := 0
	switch (flags >> 1) & 0x07 {
	case 0:
		envLen = 0
	case 1:
		envLen = 32
	case 2, 3:
		envLen = 48
	case 4:
		envLen = 64
	}
	off := 8 + envLen
	if off > len(blob) {
		return Geometry{}, fmt.Errorf("%w: GeoPackage 几何包头长度异常", ErrInvalid)
	}
	r := &wkbReader{b: blob[off:]}
	return r.geometry()
}

// wkbReader WKB 游标（每个子几何自带字节序，故字节序随解析推进而变）。
type wkbReader struct {
	b   []byte
	pos int
}

func (r *wkbReader) geometry() (Geometry, error) {
	if r.pos+5 > len(r.b) {
		return Geometry{}, fmt.Errorf("%w: WKB 数据过短", ErrInvalid)
	}
	order, err := r.order()
	if err != nil {
		return Geometry{}, err
	}
	raw := order.Uint32(r.b[r.pos : r.pos+4])
	r.pos += 4
	base := raw % 1000
	extra := raw / 1000 // 0=XY 1=Z 2=M 3=ZM
	stride := 2
	if extra == 1 || extra == 3 {
		stride++
	}
	if extra == 2 || extra == 3 {
		stride++
	}

	switch base {
	case 1: // Point
		p, err := r.point(order, stride)
		if err != nil {
			return Geometry{}, err
		}
		return Geometry{Type: GeomPoint, Lines: [][]Point{{p}}}, nil
	case 2: // LineString
		line, err := r.points(order, stride)
		if err != nil {
			return Geometry{}, err
		}
		return Geometry{Type: GeomLine, Lines: [][]Point{line}}, nil
	case 3: // Polygon：首个环为外环，其余为洞（WKB 规范）
		n, err := r.count(order)
		if err != nil {
			return Geometry{}, err
		}
		g := Geometry{Type: GeomPolygon}
		for i := 0; i < n; i++ {
			line, err := r.points(order, stride)
			if err != nil {
				return Geometry{}, err
			}
			g.Lines = append(g.Lines, line)
			g.Exterior = append(g.Exterior, i == 0)
		}
		return g, nil
	case 4: // MultiPoint
		n, err := r.count(order)
		if err != nil {
			return Geometry{}, err
		}
		g := Geometry{Type: GeomPoint}
		for i := 0; i < n; i++ {
			sub, err := r.geometry()
			if err != nil {
				return Geometry{}, err
			}
			g.Lines = append(g.Lines, sub.Lines...)
		}
		return g, nil
	case 5: // MultiLineString
		n, err := r.count(order)
		if err != nil {
			return Geometry{}, err
		}
		g := Geometry{Type: GeomLine}
		for i := 0; i < n; i++ {
			sub, err := r.geometry()
			if err != nil {
				return Geometry{}, err
			}
			g.Lines = append(g.Lines, sub.Lines...)
		}
		return g, nil
	case 6: // MultiPolygon：环角色由子多边形给出
		n, err := r.count(order)
		if err != nil {
			return Geometry{}, err
		}
		g := Geometry{Type: GeomPolygon}
		for i := 0; i < n; i++ {
			sub, err := r.geometry()
			if err != nil {
				return Geometry{}, err
			}
			g.Lines = append(g.Lines, sub.Lines...)
			g.Exterior = append(g.Exterior, sub.Exterior...)
		}
		return g, nil
	case 7: // GeometryCollection：按首个子几何类型归并（混合类型不产出非法 MVT 要素）
		n, err := r.count(order)
		if err != nil {
			return Geometry{}, err
		}
		var merged Geometry
		for i := 0; i < n; i++ {
			sub, err := r.geometry()
			if err != nil {
				return Geometry{}, err
			}
			if merged.Type == GeomUnknown {
				merged.Type = sub.Type
			}
			if sub.Type != merged.Type {
				continue
			}
			merged.Lines = append(merged.Lines, sub.Lines...)
			merged.Exterior = append(merged.Exterior, sub.Exterior...)
		}
		return merged, nil
	}
	return Geometry{}, fmt.Errorf("%w: 不支持的 WKB 几何类型 %d", ErrInvalid, raw)
}

// order 读一个字节序标记。
func (r *wkbReader) order() (binary.ByteOrder, error) {
	if r.pos >= len(r.b) {
		return nil, fmt.Errorf("%w: WKB 数据过短", ErrInvalid)
	}
	b := r.b[r.pos]
	r.pos++
	if b == 1 {
		return binary.LittleEndian, nil
	}
	return binary.BigEndian, nil
}

// count 读一个 uint32 计数（按当前几何的字节序）。
func (r *wkbReader) count(order binary.ByteOrder) (int, error) {
	if r.pos+4 > len(r.b) {
		return 0, fmt.Errorf("%w: WKB 数据过短", ErrInvalid)
	}
	n := int(int32(order.Uint32(r.b[r.pos : r.pos+4])))
	r.pos += 4
	if n < 0 {
		return 0, fmt.Errorf("%w: WKB 计数非法", ErrInvalid)
	}
	return n, nil
}

// point 读一个坐标（stride 为坐标分量数，Z/M 分量读出后丢弃）。
func (r *wkbReader) point(order binary.ByteOrder, stride int) (Point, error) {
	need := 8 * stride
	if r.pos+need > len(r.b) {
		return Point{}, fmt.Errorf("%w: WKB 坐标数据不完整", ErrInvalid)
	}
	var p Point
	for i := 0; i < stride; i++ {
		v := math.Float64frombits(order.Uint64(r.b[r.pos : r.pos+8]))
		r.pos += 8
		switch i {
		case 0:
			p.X = v
		case 1:
			p.Y = v
		}
	}
	return p, nil
}

// points 读一串坐标。
func (r *wkbReader) points(order binary.ByteOrder, stride int) ([]Point, error) {
	n, err := r.count(order)
	if err != nil {
		return nil, err
	}
	out := make([]Point, 0, n)
	for i := 0; i < n; i++ {
		p, err := r.point(order, stride)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
