# TanGIS

开源内核、云原生、可私有化的新一代 GIS 数据处理与分发平台——切片可横向扩展、全开放 API、协同开源可用、容器化部署（对标 GISBox，详见 [docs/GISBox竞品分析与PRD.md](docs/GISBox竞品分析与PRD.md)）。

## 目录结构

```
tangis/
├── docs/      # PRD、架构设计、ADR 等文档
├── server/    # Go 业务面：用户/租户、任务编排、OGC 服务分发、协同
├── kernel/    # Rust 数据面：切片内核（OSGB→3DTiles 等）、格式解析算子
└── deploy/    # 本地/私有化部署：docker-compose、初始化脚本、环境配置
```

## 交付形态

### 桌面单机版（推荐上手方式，双击即用，零外部依赖）

```bash
cd server && make desktop      # 构建前端 + 打包单二进制
TANGIS_MODE=desktop ./bin/tangis-desktop
```

启动后自动打开浏览器，数据全部落在 `~/TanGIS/`（SQLite + 产物仓库 + 日志），
**不需要 Docker / PostgreSQL / MinIO / NATS / Redis**。

| 能力 | 桌面模式实现 |
|---|---|
| 任务/Key/配额持久化 | SQLite（`~/TanGIS/tangis.db`，纯 Go 驱动无 cgo） |
| 产物仓库（替代 MinIO） | 本地文件系统（`~/TanGIS/artifacts/`） |
| 任务队列（替代 NATS） | 进程内队列 + worker（`internal/localqueue`） |
| 缓存（替代 Redis） | 进程内内存缓存 |
| Web 控制台 | `go:embed` 静态资源，无需独立静态服务 |
| 输出路径 | 建任务可省略 `output`，自动分配 `~/TanGIS/data/outputs/{task_id}` |

环境变量：`TANGIS_MODE=desktop`、`TANGIS_HOME`（数据目录）、`KERNEL_BIN`（内核路径）、
`TANGIS_NO_BROWSER=1`（不自动开浏览器）、`TANGIS_DESKTOP_WORKERS`（并发数，默认 1）。

> 矢量发布（MVT/WFS）依赖 PostGIS，桌面模式暂未装配，相关接口返回 503。

### 服务端 / 集群模式（可选，需外部依赖）

Docker Compose 起 PostGIS / Redis / MinIO / NATS + Go 服务，见下方「快速开始」。

## 快速开始

前置要求：Docker、Go 1.22+、Rust 1.75+

```bash
# 1. 启动基础依赖（PostgreSQL+PostGIS / Redis / MinIO / NATS）
cp deploy/.env.example deploy/.env
cd deploy && docker compose up -d && cd ..

# 2. 启动 Go 业务服务
cd server
go run ./cmd/server
# 服务默认监听 http://localhost:8080

# 3. 运行 Rust 切片内核 CLI
cd kernel
cargo run --bin tangis-kernel -- --help
```

基础设施端口（宿主机，避开常用冲突）：

| 服务 | 端口 | 说明 |
|---|---|---|
| PostgreSQL + PostGIS | 15432 | 库名 `tangis`，已建 `postgis` 扩展 |
| Redis | 16379 | 缓存，AOF 持久化 |
| MinIO API / 控制台 | 19000 / 19001 | 自动创建 bucket `tangis` |
| NATS 客户端 / 监控 | 14222 / 18222 | JetStream 已启用，监控见 http://localhost:18222 |

## M1（MVP）范围

- 数据导入：OSGB / 3DTiles / GLTF / FBX / OBJ / GeoTIFF / DEM / LAS / SHP / GeoJSON / GeoPackage / DXF
- 切片转换：倾斜→3DTiles、影像→瓦片、地形→Terrain、模型/点云→3DTiles（目标：5GB OSGB @16核/32G ≤ 30min，支持断点续切）
- 服务分发：WMS / WMTS / WFS / WCS / TMS / MVT + 3DTiles / Terrain
- 任务管理：队列、进度、失败重试、日志下载
- 3D 预览：Cesium + 场景树 + 属性查询 + 测量
- 用户与权限：多租户、API Key、签名防盗链
- 测绘合规（P0）：CGCS2000 转换与七参数托管、DEM 脱密、发布审批 + 水印
- Open Core / License 分界设计

## Rust 内核能力（CLI 子命令）

```bash
cd kernel && cargo run --bin tangis-kernel -- <COMMAND> --help
```

| 子命令 | 能力 | 产物 |
|---|---|---|
| `build` | OSGB 倾斜 → 3D Tiles（真几何 b3dm + tileset.json，Rayon 并行，journal 断点续切，`--simplify` 减面，`--origin` 地理定位，`--cancel-file` 协作式取消） | `tileset.json` + `*.b3dm` |
| `raster2tiles` | GeoTIFF 影像 → 瓦片金字塔（`--pyramid-layout xyz`（`{z}/{x}/{y}`）或 `wmts`（`{z}/{row}/{col}`，metadata.json 带同名字段，server 按布局读盘；`--cancel-file`） | `tiles/` + `tiles/metadata.json` |
| `terrain2tiles` | GeoTIFF DEM → Cesium Quantized-Mesh 地形瓦片（TMS 布局、gzip、zigzag/HWM 编码、layer.json；EPSG:4326/3857，4490 按 WGS84 近似；`--grid-size` 网格边长，zoom 自动推导，`--cancel-file`） | `{z}/{x}/{y}.terrain` + `layer.json` |
| `las2pnts` | LAS 点云 → 3D Tiles 点云（流式读取；点格式 0/1/2/3/5/6/7/8，LAZ 暂不支持；XY 网格分块 + pnts RTC_CENTER + tileset.json，`--origin` ENU→ECEF） | `tiles/{i}.pnts` + `tileset.json` |
| `qc` | 倾斜模型几何质检（退化/法线翻转/悬浮/裂缝/自相交 → JSON 报告） | 质检报告 JSON |
| `simplify` | QEM 网格简化（OBJ → OBJ + 统计） | OBJ |
| `edit` | OSGB 实时编辑算子（clip / flatten / ground-align，输出 OBJ 或 b3dm+tileset） | OBJ 或 b3dm 产物 + 留痕 JSON |
| `crs-identify` / `bursa` / `desensitize-dem` | 合规算子：CRS 识别 / CGCS2000 七参数 / DEM 区域脱密 | JSON / GeoTIFF |

> **协作式取消（B7）**：`--cancel-file <path>` 指定标志文件，文件出现即安全终止——已完成分块保留在 journal、可断点续切，进程 exit 130（stderr 带 `CANCELED:` 前缀）。server worker 的取消/暂停 API 即基于此：优雅取消优先，宽限期超时才强杀兜底。
>
> **地形服务（A5）**：任务类型 `terrain->tiles` 由 worker 派发 `terrain2tiles`；审批后 `GET /api/v1/services` 返回 `terrain_url`（`/services/{id}/layer.json`），Cesium TerrainProvider 直接消费，`.terrain` 瓦片经同通道分发。

出口标准：跑通「OSGB 进 → 脱密质检 → 3DTiles 服务出 → Cesium 加载」全链路。

## 技术栈

| 层 | 技术 |
|---|---|
| 业务服务端 | Go（Gin/Echo） |
| 切片内核 | Rust（Rayon 多线程，GDAL bindings / las / obj crate） |
| Web 前端 | Vue 3 + TypeScript + Vite |
| 3D 预览 | Cesium（3DTiles 底座）+ Three.js，WebGL2 为主、WebGPU 渐进增强 |
| 空间数据库 | PostgreSQL 16 + PostGIS 3 |
| 缓存 | Redis 7 |
| 对象存储 | MinIO（瓦片/源数据） |
| 任务队列 | NATS JetStream（持久化/重放） |
| 部署 | Docker Compose（单机）/ Helm Chart（集群）/ 离线包（信创 ARM64） |
| 可观测 | OpenTelemetry + Prometheus + Grafana + Loki |
