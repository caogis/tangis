# TanGIS 矢量发布 API（M2-F10a，PRD F-10）

PostGIS → MVT（Mapbox Vector Tile）发布。矢量数据留在用户的 PostGIS 库中，
TanGIS 只登记「数据源连接 + 图层映射」元数据，按需实时切片（`ST_AsMVT`），
不做数据拷贝。

- 管理接口（`/api/v1/vector/sources|layers`）：走 `X-API-Key` 鉴权（F-06，`TANGIS_AUTH=off` 时放行）。
- MVT 分发（`/api/v1/vector/{layer}/{z}/{x}/{y}.pbf`）：`X-API-Key` 或防盗链签名 URL 任一（与 WMTS/TMS 分发同语义）。
- WFS 2.0（`/api/v1/wfs`，M2-F14）：鉴权与缓存沿用 MVT 同一语义。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `TANGIS_VECTOR_CACHE_TTL` | `300` | MVT 瓦片 Redis 缓存 TTL（秒），key 含 layer/z/x/y |
| `TANGIS_VECTOR_MAX_FEATURES` | `100000` | 单 tile 要素数上限，超出返回 413（不静默截断） |
| `TANGIS_WFS_MAX_FEATURES` | `10000` | WFS GetFeature 单次要求数上限，`count` 超出时收敛到该值（M2-F14） |

## API 一览

### 数据源管理

#### `POST /api/v1/vector/sources` — 注册 PostGIS 数据源

注册时立即做连通性测试（ping + `PostGIS_Full_Version()`），不可达直接拒绝（502）。

```jsonc
// 方式一：连接参数（密码按项目现状明文入库，见「安全说明」）
{"name": "local-postgis", "host": "127.0.0.1", "port": 15432,
 "user": "tangis", "password": "***", "dbname": "tangis", "sslmode": "disable"}
// 方式二：直接给 DSN
{"name": "local-postgis", "dsn": "postgres://tangis:***@127.0.0.1:15432/tangis?sslmode=disable"}
```

`201` 返回 `{id, name, dsn(打码), postgis_version, created_at}`。
`400` 参数非法；`409` 名称重复；`502` 连不上。

#### `GET /api/v1/vector/sources` — 数据源列表（DSN 打码回显）

#### `DELETE /api/v1/vector/sources/:id` — 删除数据源

仍被图层引用时 `409`（需先删图层）；成功 `204`；不存在 `404`。

### 图层管理

#### `POST /api/v1/vector/layers` — 注册图层

```jsonc
{
  "name": "parcels",          // 全局唯一，同时是 MVT source-layer 名与 URL 标识
  "source_id": "7b7ea67469f1a96c",
  "schema": "demo",
  "table": "parcels",
  "geometry_column": "geom",  // 可省略：表仅有一个几何列时自动探测（geometry_columns）
  "srid": 0,                  // 可省略：默认取 geometry_columns；>0 显式覆盖
  "id_column": "",            // 可省略：默认取主键（information_schema）
  "fields": []                // 可省略：自动探测全部普通列（进 MVT 属性）
}
```

`201` 返回图层全量定义（含探测结果）。探测不到表/几何列 `400`。

#### `GET /api/v1/vector/layers` — 图层列表

#### `GET /api/v1/vector/layers/:name/metadata` — 图层元数据

```jsonc
{
  "name": "parcels", "source_id": "...", "schema": "demo", "table": "parcels",
  "geometry_column": "geom", "geometry_type": "POLYGON", "srid": 4326,
  "id_column": "gid", "fields": ["name", "kind", "area_ha"],
  "bbox_wgs84": [116.299995, 39.899998, 116.355003, 39.940002], // ST_EstimatedExtent → WGS84
  "features_estimated": 12,     // pg_class.reltuples
  "minzoom": 12, "maxzoom": 16  // 启发式建议（bbox 宽度推 minzoom，maxzoom=minzoom+8 夹在 [10,16]）
}
```

> `bbox_wgs84`/`features_estimated` 依赖统计信息：表从未 `ANALYZE` 时
> `ST_EstimatedExtent` 返回 NULL，此时二者为 `null`/`0`，执行 `ANALYZE <表>` 即可。

#### `DELETE /api/v1/vector/layers/:name` — 删除图层（`204`）

### MVT 分发

#### `GET /api/v1/vector/{layer}/{z}/{x}/{y}.pbf`

- `.pbf` 后缀可选（也接受无后缀路径）。
- `z` 取值 0–22，`x/y` 须在 `0 ≤ v < 2^z` 内，否则 `400`。
- 图层不存在 `404`。
- **空 tile（查询范围内零要素）→ `204 No Content`**（本项目选定策略）。
- 要素数超 `TANGIS_VECTOR_MAX_FEATURES` → `413`，客户端应提高 zoom 级别。
- 上游 PostGIS 故障 → `502`。

响应头：

| 头 | 说明 |
| --- | --- |
| `Content-Type: application/vnd.mapbox-vector-tile` | |
| `X-Cache: HIT/MISS` | Redis 瓦片缓存命中标记 |
| `X-Tangis-Feature-Count: N` | 本 tile 要素数（缓存命中时不返回） |

切片实现：`ST_AsMVTGeom` + `ST_AsMVT`（extent 4096，buffer 64，裁剪开启）；
bbox 恒以 WebMercator(3857) 计算，图层 `srid != 3857` 时经 `ST_Transform` 投影到
图层 SRID 做 `&&` 索引过滤与裁剪，输出统一为 3857 瓦片坐标。

## WFS 2.0 简单子集（M2-F14）

`GET /api/v1/wfs`（KVP encoding）。GetCapabilities 从**已注册图层真实生成**
（无假图层），GetFeature 复用图层注册的源/白名单/缓存链路。

### GetCapabilities

```
GET /api/v1/wfs?service=WFS&version=2.0.0&request=GetCapabilities
```

输出 `wfs:WFS_Capabilities`（version 2.0.0）：

- `ows:ServiceIdentification`（Title/ServiceType=OGC WFS/ServiceTypeVersion）
- `ows:OperationsMetadata`（GetCapabilities / GetFeature 的 KVP 入口）
- `FeatureTypeList`：每个已注册矢量图层一个 `FeatureType`
  - `ows:Title` / `ows:Identifier` = 图层名
  - `DefaultCRS` = `urn:ogc:def:crs:EPSG::{srid}`
  - `OutputFormat` = `application/geo+json`
  - `ows:WGS84BoundingBox`（CRS84）：来自 `ST_EstimatedExtent` 真实统计并
    换算 WGS84；**统计缺失或图层 SRID 非 4326/3857 时省略该元素（不编造范围）**。

### GetFeature

```
GET /api/v1/wfs?service=WFS&version=2.0.0&request=GetFeature
    &typename=parcels&bbox=116,39,117,40&count=1000
```

| 参数 | 说明 |
| --- | --- |
| `typename` | 必填，图层名（须过标识符白名单）；未知图层 → 404 异常 |
| `bbox` | 可选，`minx,miny,maxx,maxy`（**CRS84 经度、纬度序**）；经 `ST_Transform` 到图层 SRID 走 `&&` 索引过滤；省略 = 全图层 |
| `count` | 可选，返回要素数上限；`<=0` 或超出 `TANGIS_WFS_MAX_FEATURES` 时收敛到该值 |

输出 GeoJSON `FeatureCollection`（`application/geo+json`），几何经
`ST_AsGeoJSON` 统一为 EPSG:4326；响应头 `X-Tangis-Feature-Count` 为要素数
（缓存命中时不返回）。查询结果按 (图层, bbox, count) 维度走 Redis 缓存
（TTL 同 MVT，`tile:wfs:` 前缀）。

实现：单条 SQL 聚合（`json_agg` + `count`），标识符全部白名单 + 双引号引用，
bbox 数值走 `$1..$4` 占位符——与 MVT 同样的注入防护。

### 异常（OGC ExceptionReport）

所有错误返回 ows 1.1 `ExceptionReport` XML（`application/xml`）：

| HTTP | exceptionCode | 场景 |
| --- | --- | --- |
| 400 | MissingParameterValue / InvalidParameterValue | 缺 request/typename、bbox/count 非法、version 不支持 |
| 404 | NotFound | typename 无匹配图层 |
| 502 | NoApplicableCode | 上游 PostGIS 连接/查询失败 |
| 503 | —（JSON） | 服务端未装配 vector |

```xml
<?xml version="1.0" encoding="UTF-8"?>
<ExceptionReport xmlns="http://www.opengis.net/ows/1.1" version="2.0.0">
  <Exception exceptionCode="NotFound" locator="typename">
    <ExceptionText>unknown feature type: nope</ExceptionText>
  </Exception>
</ExceptionReport>
```

### 已知限制（WFS）

1. 仅 KVP encoding 的 GetCapabilities/GetFeature；不支持 DescribeFeatureType、
   Transaction、StoredQuery、XML POST 等完整 WFS 2.0 能力。
2. bbox 固定按 CRS84（经纬度）解释，不支持 bbox 尾部附加 EPSG 码指定其他 CRS。
3. `count` 超上限时静默收敛到 `TANGIS_WFS_MAX_FEATURES`（不报错、不返回
   numberMatched 分页游标）；如需分页语义待后续版本。
4. 无统计信息的图层（未 `ANALYZE`）在 Capabilities 中无 bbox 元素；
   SRID 非 4326/3857 的图层亦无（不造假换算）。

### 冒烟示例

```bash
curl 'http://127.0.0.1:8080/api/v1/wfs?service=WFS&version=2.0.0&request=GetCapabilities'
curl 'http://127.0.0.1:8080/api/v1/wfs?request=GetFeature&typename=parcels&bbox=116.30,39.90,116.36,39.94'
```

### Key 多租户与配额（M2-F14，同轮交付）

- API Key 从单全局 key 升级为多 Key 表：`api_keys` 增 `daily_task_limit`
  （0=不限）、`disabled`（禁用 403）两列，启动 `ADD COLUMN IF NOT EXISTS`
  自动迁移；`TANGIS_API_KEY`（回退 `TANGIS_DEV_API_KEY`）确保以 admin 身份
  存在（`ON CONFLICT DO NOTHING`，不覆盖手工调整）。
- 每日任务创建配额：`daily_task_limit>0` 的 tenant key 超 UTC 日限额时
  `POST /tasks`、`POST /tasks/import` 返回 `429` + `Retry-After`（距次日
  UTC 零点秒数）；用量 (key_id, UTC 日) 计数，PG `api_key_usage` 持久化
  （内存兜底），跨日自动重置；admin 角色不限额；`TANGIS_AUTH=off` 整链路跳过。
- 全部 REST API 的 OpenAPI 3.0 规范见 [openapi.yaml](openapi.yaml)，
  路径清单与路由表一致性由 `server/internal/api/openapi_test.go` 守护。


### 状态码总览

| 码 | 场景 |
| --- | --- |
| 400 | 标识符不过白名单 / zxy 非法 / 表或几何列探测失败 |
| 404 | 图层或数据源不存在 |
| 409 | 名称重复 / 数据源仍被图层引用 |
| 413 | 单 tile 要素数超上限 |
| 502 | 上游 PostGIS 连接/查询失败 |
| 503 | 服务端未装配 vector（无 DATABASE_URL 且降级异常等） |

## 安全说明

- **SQL 注入防护**：所有进入 SQL 的标识符（schema/table/列名/图层名）必须匹配
  白名单 `^[a-zA-Z_][a-zA-Z0-9_]*$` 并以双引号引用；bbox/limit 等数值一律走参数
  占位符。`information_schema` 探测结果同样过滤，白名单外的列不暴露。
- **密码存储**：按项目现状（任务 `params` 同风格）最小实现，DSN 明文存
  `vector_sources` 表；API 回显一律打码（`postgres://user:****@host/...`）。
  生产部署建议：数据库账号最小权限（只读）、密码走密钥库/环境变量注入，
  网络层限制 apiserver → PostGIS 的可达范围。
- **连接池**：每数据源一个 `pgxpool`（最大 4 连接，连接寿命 30min，空闲 5min
  回收，建连超时 5s），懒建立、复用，进程退出统一释放。
- **缓存**：瓦片走 Redis（`tangis:tile:vector:{layer}:{z}:{x}:{y}`，TTL 可配）；
  Redis 不可用时自动降级直查（与影像瓦片缓存同策略）。

## fixture（demo 数据）

`deploy/initdb/03-vector-fixture.sql`（幂等，可重复执行）建 `demo` schema：
12 个地块多边形（`demo.parcels`：gid/name/kind/area_ha）+ 12 个 POI 点
（`demo.pois`：id/name/cat），北京 116.30–116.36 / 39.90–39.94 一带，EPSG:4326。

```bash
# initdb 首次初始化自动执行；已有库手工导入：
docker exec -i tangis-postgres psql -U tangis -d tangis < deploy/initdb/03-vector-fixture.sql
```

## 端到端示例（本地冒烟）

```bash
# 1. 注册数据源
curl -X POST http://127.0.0.1:8080/api/v1/vector/sources -H 'Content-Type: application/json' \
  -d '{"name":"local-postgis","host":"127.0.0.1","port":15432,"user":"tangis",
       "password":"tangis_dev_password","dbname":"tangis"}'
# → {"id":"7b7ea67469f1a96c", ...}

# 2. 注册图层（自动探测 geom/srid/主键/字段）
curl -X POST http://127.0.0.1:8080/api/v1/vector/layers -H 'Content-Type: application/json' \
  -d '{"name":"parcels","source_id":"<上一步 id>","schema":"demo","table":"parcels"}'

# 3. 取瓦片
curl -o /tmp/tile.pbf 'http://127.0.0.1:8080/api/v1/vector/parcels/13/6743/3103.pbf'
```

## MVT 消费示例

### MapLibre GL JS

```js
const map = new maplibregl.Map({
  container: "map",
  style: {
    version: 8,
    sources: {
      tangis_parcels: {
        type: "vector",
        tiles: [
          // 鉴权开启时改用签名 URL：
          // `http://host:8080/api/v1/vector/parcels/{z}/{x}/{y}.pbf?expires=...&sig=...`
          "http://host:8080/api/v1/vector/parcels/{z}/{x}/{y}.pbf",
        ],
        minzoom: 12,          // 取 GET .../layers/parcels/metadata 的建议值
        maxzoom: 16,
        bounds: [116.30, 39.90, 116.36, 39.94], // 取 metadata.bbox_wgs84
      },
    },
    layers: [
      {
        id: "parcels-fill",
        type: "fill",
        source: "tangis_parcels",
        "source-layer": "parcels", // 图层注册的 name
        paint: {
          "fill-color": [
            "match", ["get", "kind"],
            "residential", "#f4a261",
            "commercial",  "#e76f51",
            "industrial",  "#8d99ae",
            "#2a9d8f",
          ],
          "fill-opacity": 0.6,
        },
      },
      {
        id: "parcels-outline",
        type: "line",
        source: "tangis_parcels",
        "source-layer": "parcels",
        paint: {"line-color": "#264653", "line-width": 1},
      },
    ],
  },
  center: [116.325, 39.92],
  zoom: 13,
});
```

### Cesium（VectorTileLayerImageryProvider，1.1xx+）

```js
const provider = new Cesium.VectorTileLayerImageryProvider({
  url: "http://host:8080/api/v1/vector/parcels/{z}/{x}/{y}.pbf",
  layerName: "parcels",            // MVT source-layer
  style: new Cesium.MapboxStyle({
    "fill": "match(get('kind'), 'residential', color('#f4a261'), color('#2a9d8f'))",
    "stroke-color": "color('#264653')",
    "stroke-width": 1,
  }),
  minimumLevel: 12,
  maximumLevel: 16,
});
viewer.imageryLayers.addImageryProvider(provider);
```

> Cesium 侧 MVT 支持依赖版本（建议 1.104+ 的 `VectorTileLayerImageryProvider`）；
> 旧版本可经 mapbox-vector-tile 前端解析库转 GeoJSON 后用 `GeoJsonDataSource` 加载。

## 已知限制

1. **bbox 换算仅支持 4326/3857**：metadata 的 `bbox_wgs84` 对其他 SRID 图层
   原样返回原生坐标（`srid` 字段照实标注），消费方自行投影；瓦片分发不受影响
   （任意 SRID 经 `ST_Transform` 均可出图）。
2. **实时切片不物化**：每次未命中缓存都直查 PostGIS；超大表建议保证 GiST 索引
   + `ANALYZE`，必要时调大 `TANGIS_VECTOR_CACHE_TTL`。
3. **超限即报错**：`413` 策略对客户端可见（不静默截断），密集图层需靠 zoom
   过滤或数据侧预先简化（后续可扩展 ST_Simplify 策略）。
4. **矢量元数据无租户隔离**：数据源/图层为平台级资源，管理接口要求 API Key
   但不区分租户（与 F-10 最小实现一致，后续版本补）。
5. **无 PG 元数据库时降级**：`DATABASE_URL` 为空时注册信息存内存，重启丢失
   （启动日志有警告）。
