// edits.go 矢量编辑的就地写回。
//
// 三种源格式的写回策略不同，选型依据是**别动不该动的东西**：
//
//	GeoJSON    一文件一图层 → 整体重写（原子替换：临时文件 + rename）
//	GeoPackage 一文件多图层 → **只对目标表做 SQL 增删改**，绝不动同库其它表
//	Shapefile  一文件族的单图层 → 重写 .shp/.shx/.dbf/.prj/.cpg
//
// 安全前提：**首次编辑前自动备份原文件**（`<文件名>.orig`，只备份一次），
// 备份路径随结果回显给用户——编辑是不可逆操作，必须有退路。
//
// 几何类型约束：Shapefile 与 GeoPackage 都是「一个图层一种几何类型」的存储，
// 往面图层里画点会写出非法文件，因此**明确拒绝**而不是将就写入。
package vector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EditOp 单条编辑操作（前端提交的 JSON 形状）。
type EditOp struct {
	// Op create | update | delete
	Op string `json:"op"`
	// ID 目标要素 ID（update / delete 必填）
	ID uint64 `json:"id,omitempty"`
	// Geometry 新几何，GeoJSON 几何对象（create 必填；update 可选）
	Geometry json.RawMessage `json:"geometry,omitempty"`
	// Properties 属性；update 时整体替换（前端提交完整字典）
	Properties map[string]any `json:"properties,omitempty"`
}

// EditResult 编辑结果。
type EditResult struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Deleted int `json:"deleted"`
	// Total 编辑后图层要素总数。
	Total int `json:"total"`
	// NewIDs 新建要素的 ID（与请求里 create 的顺序一致）。
	NewIDs []uint64 `json:"new_ids,omitempty"`
	// Backup 本次（或此前）建立的源文件备份路径；空表示未备份。
	Backup string `json:"backup,omitempty"`
	// Warnings 写回过程中需要用户知道的取舍。
	Warnings []string `json:"warnings,omitempty"`
}

// FeatureWriter 可选增强：就地应用要素编辑。
type FeatureWriter interface {
	ApplyEdits(ctx context.Context, dsn string, l *Layer, ops []EditOp) (*EditResult, error)
}

// ParseGeoJSONGeometry 解析单个 GeoJSON 几何对象（编辑接口用）。
func ParseGeoJSONGeometry(raw []byte) (Geometry, error) {
	if len(raw) == 0 {
		return Geometry{}, fmt.Errorf("%w: 缺少 geometry", ErrInvalid)
	}
	var g gjGeometry
	if err := json.Unmarshal(raw, &g); err != nil {
		return Geometry{}, fmt.Errorf("%w: geometry 不是合法 GeoJSON 几何: %v", ErrInvalid, err)
	}
	geom, err := convertGeometry(&g)
	if err != nil {
		return Geometry{}, err
	}
	if geom.Type == GeomUnknown || len(geom.Lines) == 0 {
		return Geometry{}, fmt.Errorf("%w: geometry 为空", ErrInvalid)
	}
	return geom, nil
}

// parsedOp 解析后的编辑操作。
type parsedOp struct {
	op       string
	id       uint64
	geom     Geometry
	hasGeom  bool
	props    map[string]any
	hasProps bool
}

// parseEditOps 解析并校验编辑操作（几何先解出来，避免写回途中才发现非法）。
func parseEditOps(ops []EditOp) ([]parsedOp, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("%w: 没有要应用的编辑", ErrInvalid)
	}
	if len(ops) > 10000 {
		return nil, fmt.Errorf("%w: 单次编辑操作过多（上限 10000 条）", ErrInvalid)
	}
	out := make([]parsedOp, 0, len(ops))
	for i, o := range ops {
		p := parsedOp{op: strings.ToLower(strings.TrimSpace(o.Op)), id: o.ID}
		switch p.op {
		case "create":
			g, err := ParseGeoJSONGeometry(o.Geometry)
			if err != nil {
				return nil, fmt.Errorf("第 %d 条操作：%w", i+1, err)
			}
			p.geom, p.hasGeom = g, true
			if o.Properties != nil {
				p.props, p.hasProps = o.Properties, true
			}
		case "update":
			if o.ID == 0 {
				return nil, fmt.Errorf("%w: 第 %d 条 update 缺少 id", ErrInvalid, i+1)
			}
			if len(o.Geometry) > 0 {
				g, err := ParseGeoJSONGeometry(o.Geometry)
				if err != nil {
					return nil, fmt.Errorf("第 %d 条操作：%w", i+1, err)
				}
				p.geom, p.hasGeom = g, true
			}
			if o.Properties != nil {
				p.props, p.hasProps = o.Properties, true
			}
			if !p.hasGeom && !p.hasProps {
				return nil, fmt.Errorf("%w: 第 %d 条 update 既没有 geometry 也没有 properties", ErrInvalid, i+1)
			}
		case "delete":
			if o.ID == 0 {
				return nil, fmt.Errorf("%w: 第 %d 条 delete 缺少 id", ErrInvalid, i+1)
			}
		default:
			return nil, fmt.Errorf("%w: 第 %d 条操作类型 %q 不支持（create/update/delete）", ErrInvalid, i+1, o.Op)
		}
		out = append(out, p)
	}
	return out, nil
}

// checkEditGeomType 校验几何类型与图层一致。
//
// Shapefile / GeoPackage 的存储都是「一图层一种几何类型」：往面图层写点会产出
// 非法文件（GeoPackage 规范还要求列类型与声明一致），因此拒绝而非将就。
func checkEditGeomType(l *Layer, got GeomType) error {
	want := normalizeGeomTypeName(l.GeometryType)
	if want == "" || want == "GEOMETRY" {
		return nil
	}
	if want != geomTypeName(got) {
		return fmt.Errorf("%w: 该图层是 %s 类型，不能写入 %s 要素"+
			"（Shapefile / GeoPackage 一个图层只能存一种几何类型）",
			ErrInvalid, want, geomTypeName(got))
	}
	return nil
}

// applyOps 在内存中应用编辑，返回新要素集与统计。
//
// 语义：create 追加（ID 取当前最大值 +1，保持稳定不重号）；update 按 ID 替换
// 几何和/或属性（属性**整体替换**，前端提交完整字典）；delete 按 ID 移除。
func applyOps(feats []FileFeature, l *Layer, ops []parsedOp) ([]FileFeature, *EditResult, error) {
	res := &EditResult{}
	out := make([]FileFeature, len(feats))
	copy(out, feats)

	nextID := uint64(1)
	index := make(map[uint64]int, len(out))
	for i := range out {
		if out[i].ID >= nextID {
			nextID = out[i].ID + 1
		}
		if _, dup := index[out[i].ID]; dup {
			// 同 ID 重复（理论上不该出现）：后续按首个匹配处理
			continue
		}
		index[out[i].ID] = i
	}
	removed := map[uint64]bool{}

	locate := func(id uint64) (int, error) {
		idx, ok := index[id]
		if !ok || removed[id] {
			return 0, fmt.Errorf("%w: 要素 %d 不存在", ErrNotFound, id)
		}
		return idx, nil
	}

	for _, p := range ops {
		switch p.op {
		case "create":
			if err := checkEditGeomType(l, p.geom.Type); err != nil {
				return nil, nil, err
			}
			id := nextID
			nextID++
			out = append(out, FileFeature{ID: id, Geom: p.geom, Props: p.props})
			index[id] = len(out) - 1
			res.Created++
			res.NewIDs = append(res.NewIDs, id)
		case "update":
			idx, err := locate(p.id)
			if err != nil {
				return nil, nil, err
			}
			if p.hasGeom {
				if err := checkEditGeomType(l, p.geom.Type); err != nil {
					return nil, nil, err
				}
				out[idx].Geom = p.geom
			}
			if p.hasProps {
				out[idx].Props = p.props
			}
			res.Updated++
		case "delete":
			idx, err := locate(p.id)
			if err != nil {
				return nil, nil, err
			}
			removed[p.id] = true
			_ = idx
			res.Deleted++
		}
	}
	if len(removed) > 0 {
		kept := make([]FileFeature, 0, len(out))
		for i := range out {
			if removed[out[i].ID] {
				continue
			}
			kept = append(kept, out[i])
		}
		out = kept
	}
	res.Total = len(out)
	return out, res, nil
}

/* ---------------- FileGateway 实现 ---------------- */

// ApplyEdits 实现 FeatureWriter：按源格式就地写回。
func (g *FileGateway) ApplyEdits(_ context.Context, dsn string, l *Layer, ops []EditOp) (*EditResult, error) {
	path, ok := FilePathOf(dsn)
	if !ok {
		return nil, fmt.Errorf("%w: 非文件数据源 DSN: %s", ErrInvalid, dsn)
	}
	parsed, err := parseEditOps(ops)
	if err != nil {
		return nil, err
	}
	// 首次编辑前备份原文件（只备份一次）
	backup, err := ensureSourceBackup(path)
	if err != nil {
		return nil, err
	}

	var res *EditResult
	switch strings.ToLower(filepath.Ext(path)) {
	case ".geojson", ".json":
		res, err = g.applyGeoJSONEdits(path, l, parsed)
	case ".gpkg":
		res, err = g.applyGeoPackageEdits(path, l, parsed)
	case ".shp":
		res, err = g.applyShapefileEdits(path, l, parsed)
	default:
		err = fmt.Errorf("%w: 不支持的编辑格式 %q", ErrInvalid, filepath.Ext(path))
	}
	if err != nil {
		return nil, err
	}
	res.Backup = backup
	// 写回后清缓存：mtime+size 判定在秒级粒度下可能漏掉，直接清更稳
	g.invalidate(path)
	return res, nil
}

// invalidate 清除某文件的全部缓存（含各要素表）。
func (g *FileGateway) invalidate(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k := range g.cache {
		if k == path || strings.HasPrefix(k, path+"\x00") {
			delete(g.cache, k)
		}
	}
}

// applyGeoJSONEdits 重写 GeoJSON 源文件（原子替换）。
func (g *FileGateway) applyGeoJSONEdits(path string, l *Layer, ops []parsedOp) (*EditResult, error) {
	// 直接从磁盘读，不用缓存：避免在过期副本上叠加编辑
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取源文件失败: %v", ErrInvalid, err)
	}
	feats, info, err := ParseGeoJSON(data)
	if err != nil {
		return nil, err
	}
	next, res, err := applyOps(feats, l, ops)
	if err != nil {
		return nil, err
	}
	out, n, err := marshalGeoJSONFeatures(next)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, out); err != nil {
		return nil, fmt.Errorf("vector: 写回 GeoJSON 失败: %w", err)
	}
	res.Total = n
	_ = info
	return res, nil
}

// applyShapefileEdits 重写 Shapefile 文件族。
func (g *FileGateway) applyShapefileEdits(path string, l *Layer, ops []parsedOp) (*EditResult, error) {
	feats, info, err := ParseShapefile(path)
	if err != nil {
		return nil, err
	}
	next, res, err := applyOps(feats, l, ops)
	if err != nil {
		return nil, err
	}
	srid := l.SRID
	if srid == 0 {
		srid = info.SRID
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	files, n, skipped, warnings, err := buildShapefileSet(base, next, srid)
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		return nil, fmt.Errorf("%w: 有 %d 条要素的几何类型与图层不一致，无法写回 Shapefile", ErrInvalid, skipped)
	}
	dir := filepath.Dir(path)
	for suffix, content := range files {
		if err := writeFileAtomic(filepath.Join(dir, base+suffix), content); err != nil {
			return nil, fmt.Errorf("vector: 写回 Shapefile %s 失败: %w", suffix, err)
		}
	}
	res.Total = n
	res.Warnings = append(res.Warnings, warnings...)
	return res, nil
}

// applyGeoPackageEdits 只对目标要素表做增删改，绝不动同库其它表。
func (g *FileGateway) applyGeoPackageEdits(path string, l *Layer, ops []parsedOp) (*EditResult, error) {
	table := l.Table
	if table == "" {
		return nil, fmt.Errorf("%w: 图层未记录 GeoPackage 表名", ErrInvalid)
	}
	tbl, err := quoteIdent(table)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("%w: 打开 GeoPackage 失败: %v", ErrInvalid, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	cols, err := tableColumns(db, table)
	if err != nil {
		return nil, err
	}
	geomCol := l.GeometryColumn
	if geomCol == "" {
		geomCol = "geom"
	}
	var (
		geomQuoted string
		pkName     string
		attrCols   []string
	)
	haveGeom := false
	for _, c := range cols {
		if c.Name == geomCol {
			haveGeom = true
			geomQuoted = quoteMust(c.Name)
			continue
		}
		if pkName == "" && c.PK == 1 && strings.Contains(strings.ToUpper(c.Type), "INT") {
			pkName = c.Name
			continue
		}
		attrCols = append(attrCols, c.Name)
	}
	if !haveGeom {
		return nil, fmt.Errorf("%w: 表 %q 里找不到几何列 %q", ErrInvalid, table, geomCol)
	}
	if pkName == "" {
		return nil, fmt.Errorf("%w: 表 %q 没有整型主键，无法定位要素", ErrInvalid, table)
	}

	// 表中没有的属性键会被丢弃（不擅自改表结构），如实提示
	known := map[string]bool{}
	for _, c := range attrCols {
		known[c] = true
	}
	dropped := map[string]bool{}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("vector: 开启事务失败: %w", err)
	}
	rollback := func() { _ = tx.Rollback() }

	res := &EditResult{}
	for _, p := range ops {
		switch p.op {
		case "create":
			if err := checkEditGeomType(l, p.geom.Type); err != nil {
				rollback()
				return nil, err
			}
			names := []string{geomQuoted}
			vals := []any{gpkgBlobBytes(wkbEncode(normalizeForGeoPackage(p.geom)), l.SRID)}
			for _, c := range attrCols {
				names = append(names, quoteMust(c))
				v, ok := p.props[c]
				if !ok {
					v = nil
				}
				vals = append(vals, v)
			}
			for k := range p.props {
				if !known[k] {
					dropped[k] = true
				}
			}
			ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
			r, err := tx.Exec("INSERT INTO "+tbl+" ("+strings.Join(names, ",")+") VALUES ("+ph+")", vals...)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("vector: 新增要素失败: %w", err)
			}
			if id, err := r.LastInsertId(); err == nil {
				res.NewIDs = append(res.NewIDs, uint64(id))
			}
			res.Created++
		case "update":
			sets := []string{}
			vals := []any{}
			if p.hasGeom {
				if err := checkEditGeomType(l, p.geom.Type); err != nil {
					rollback()
					return nil, err
				}
				sets = append(sets, geomQuoted+" = ?")
				vals = append(vals, gpkgBlobBytes(wkbEncode(normalizeForGeoPackage(p.geom)), l.SRID))
			}
			if p.hasProps {
				for _, c := range attrCols {
					sets = append(sets, quoteMust(c)+" = ?")
					vals = append(vals, p.props[c])
				}
				for k := range p.props {
					if !known[k] {
						dropped[k] = true
					}
				}
			}
			if len(sets) == 0 {
				continue
			}
			vals = append(vals, p.id)
			r, err := tx.Exec("UPDATE "+tbl+" SET "+strings.Join(sets, ",")+
				" WHERE "+quoteMust(pkName)+" = ?", vals...)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("vector: 更新要素失败: %w", err)
			}
			if n, _ := r.RowsAffected(); n == 0 {
				rollback()
				return nil, fmt.Errorf("%w: 要素 %d 不存在", ErrNotFound, p.id)
			}
			res.Updated++
		case "delete":
			r, err := tx.Exec("DELETE FROM "+tbl+" WHERE "+quoteMust(pkName)+" = ?", p.id)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("vector: 删除要素失败: %w", err)
			}
			if n, _ := r.RowsAffected(); n == 0 {
				rollback()
				return nil, fmt.Errorf("%w: 要素 %d 不存在", ErrNotFound, p.id)
			}
			res.Deleted++
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("vector: 提交编辑失败: %w", err)
	}

	// 回填要素数与范围（其它软件据此定位图层）
	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&total); err == nil {
		res.Total = total
	}
	_ = refreshGeoPackageExtent(db, table, tbl, geomQuoted, pkName)
	if len(dropped) > 0 {
		keys := make([]string, 0, len(dropped))
		for k := range dropped {
			keys = append(keys, k)
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"这些属性键在当前表中不存在，已忽略：%s", strings.Join(keys, "、")))
	}
	return res, nil
}

// refreshGeoPackageExtent 重算并回填 gpkg_contents 的范围与更新时间。
func refreshGeoPackageExtent(db *sql.DB, table, tbl, geomQuoted, pkName string) error {
	rows, err := db.Query("SELECT " + geomQuoted + " FROM " + tbl)
	if err != nil {
		return err
	}
	defer rows.Close()
	minx, miny, maxx, maxy := 0.0, 0.0, 0.0, 0.0
	first := true
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			continue
		}
		g, err := parseGeoPackageGeometry(blob)
		if err != nil || g.Type == GeomUnknown {
			continue
		}
		b := g.Bounds()
		if b.Empty() {
			continue
		}
		if first {
			minx, miny, maxx, maxy = b.MinX, b.MinY, b.MaxX, b.MaxY
			first = false
			continue
		}
		minx = min(minx, b.MinX)
		miny = min(miny, b.MinY)
		maxx = max(maxx, b.MaxX)
		maxy = max(maxy, b.MaxY)
	}
	if first {
		return nil
	}
	_, err = db.Exec(`UPDATE gpkg_contents SET min_x=?, min_y=?, max_x=?, max_y=?,
		last_change=? WHERE table_name=?`,
		minx, miny, maxx, maxy, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), table)
	return err
}

/* ---------------- 备份与原子写 ---------------- */

// sourceSiblingExts 各格式的"文件族"扩展名（备份时要一并复制）。
func sourceSiblingExts(path string) []string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".shp":
		return []string{".shp", ".shx", ".dbf", ".prj", ".cpg"}
	}
	return []string{filepath.Ext(path)}
}

// ensureSourceBackup 首次编辑前备份源文件（已存在备份则跳过，不重复堆副本）。
//
// 编辑是不可逆的原地写入，必须留退路；备份名固定为 `<文件名>.orig`，便于用户
// 一眼认出、也便于重复编辑时复用同一份"编辑前"副本。
func ensureSourceBackup(path string) (string, error) {
	stem := strings.TrimSuffix(path, filepath.Ext(path))
	backupName := filepath.Base(path) + ".orig"
	backupPath := filepath.Join(filepath.Dir(path), backupName)
	if _, err := os.Stat(backupPath); err == nil {
		return backupPath, nil // 已有备份
	}
	copied := false
	for _, ext := range sourceSiblingExts(path) {
		src := stem + ext
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if err := os.WriteFile(src+".orig", data, 0o644); err != nil {
			return "", fmt.Errorf("vector: 备份源文件失败: %w", err)
		}
		copied = true
	}
	if !copied {
		return "", fmt.Errorf("%w: 源文件不可读，无法备份，编辑已中止", ErrInvalid)
	}
	return backupPath, nil
}

// writeFileAtomic 原子写文件（临时文件 + rename）。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ApplyEdits 对已注册图层应用编辑（写回数据源），供 API 层调用。
func (s *Server) ApplyEdits(ctx context.Context, layerName string, ops []EditOp) (*EditResult, error) {
	l, err := s.Meta.GetLayer(ctx, layerName)
	if err != nil {
		return nil, err
	}
	src, err := s.Meta.GetSource(ctx, l.SourceID)
	if err != nil {
		return nil, err
	}
	fw, ok := s.Gateway.(FeatureWriter)
	if !ok {
		return nil, fmt.Errorf("%w: 当前装配的数据源不支持编辑", ErrInvalid)
	}
	return fw.ApplyEdits(ctx, src.DSN, l, ops)
}

/* ---------------- 网关路由 ---------------- */

// ApplyEdits 实现 FeatureWriter：按 DSN 分派。
func (g *RouterGateway) ApplyEdits(ctx context.Context, dsn string, l *Layer, ops []EditOp) (*EditResult, error) {
	if IsFileDSN(dsn) {
		return g.File.ApplyEdits(ctx, dsn, l, ops)
	}
	if g.PG == nil {
		return nil, fmt.Errorf("%w: 当前部署未装配 PostGIS 数据源", ErrInvalid)
	}
	return nil, fmt.Errorf("%w: PostGIS 数据源暂不支持就地编辑（WFS-T 待补）；"+
		"可先把数据导出为 GeoPackage 再编辑", ErrInvalid)
}
