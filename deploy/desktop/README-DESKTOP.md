# TanGIS 桌面单机版

> 打开即用：**不需要 Docker、不需要 PostgreSQL/Redis/MinIO/NATS**，也不需要安装数据库。
> 本目录自带 Web 控制台（已嵌入主二进制），运行后浏览器访问固定端口即可。

## 一、启动

| 系统 | 操作 |
|---|---|
| macOS | **双击 `TanGIS.app`**（推荐）；或双击 `start.command`（带终端输出） |
| Linux | 终端执行 `./start.sh` |
| Windows | 双击 `start.bat` |

默认端口 **8080**，启动后自动打开浏览器：

```
http://127.0.0.1:8080/
```

停止：`stop.command` / `stop.sh` / `stop.bat`。

`.app` 也可以直接拖进「应用程序」使用 —— 数据目录与安装位置无关，放在哪里都不丢数据。

> **macOS 首次打开被 Gatekeeper 拦住**（本发行版未做 Apple 公证，只做了临时签名）：
>
> - `TanGIS.app`：在 Finder 里**右键 → 打开**，或
>   ```bash
>   xattr -dr com.apple.quarantine TanGIS.app
>   ```
> - 目录形态：`xattr -dr com.apple.quarantine start.command stop.command`
>
> 命令行启动还可以直接执行 `./TanGIS.app/Contents/Resources/start.sh`。

## 二、目录说明

macOS 形态：

| 路径 | 作用 |
|---|---|
| `TanGIS.app` | **双击即用的应用包**（含主二进制、切片内核、图标） |
| `TanGIS.app/Contents/Resources/tangis` | 主服务：HTTP API + Web 控制台（内置） |
| `TanGIS.app/Contents/Resources/tangis-kernel` | Rust 切片内核（主程序在同目录查找） |
| `start.sh` / `start.command` | 命令行/双击启动（转调 `.app` 内的启动脚本） |
| `stop.sh` / `stop.command` | 停止 |

Linux / Windows 形态（二进制与脚本平铺）：

| 文件 | 作用 |
|---|---|
| `tangis` / `tangis.exe` | 主服务 |
| `tangis-kernel` / `.exe` | 切片内核 |
| `start.*` / `stop.*` | 启停脚本 |

## 三、数据落在哪里

默认 `~/TanGIS/`（目录名唯一且固定，不探测也不沿用任何历史目录；如需换位置，用 `TANGIS_HOME`）：

```
~/TanGIS/
├── tangis.db          # SQLite：任务 / API Key / 配额用量 / 矢量图层注册表
├── tangis.pid         # 进程号（stop 脚本使用）
├── tangis.log         # 服务运行日志（启动脚本生成）
├── start.log          # 启动脚本自身的输出（macOS .app 形态下排障入口）
├── artifacts/         # 产物仓库（瓦片、b3dm、tileset.json…）
├── data/
│   ├── logs/          # 每个任务的切片日志（可下载）
│   ├── outputs/       # 建任务未指定 output 时的自动产物目录
│   ├── uploads/       # 页面上传/导入的源文件（含首次编辑自动备份的 *.orig）
│   └── vector/        # 矢量图层注册表（registry.json）
└── exports/           # 矢量导出产物
```

> pid 与日志都在数据目录，不在安装目录 —— 安装目录可能被覆盖升级，且 macOS 的
> `.app` 内部本应只读。

## 四、环境变量（可选）

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | 8080 | 监听端口（脚本可覆盖：`PORT=9000 ./start.sh`） |
| `TANGIS_HOME` | `~/TanGIS` | 数据目录 |
| `TANGIS_API_KEY` | `tangis-dev-key` | 内置管理员 Key（浏览器请求头使用） |
| `TANGIS_AUTH` | `on` | 设为 `off` 关闭鉴权（仅开发用） |
| `TANGIS_DESKTOP_WORKERS` | 1 | 切片并发数 |
| `TANGIS_PORT_AUTO` | 未设置 | 为 `1` 时端口被占用自动顺延（默认占用即报错，避免访问错端口） |
| `TANGIS_NO_BROWSER` | 未设置 | 为 `1` 时不自动打开浏览器 |
| `KERNEL_BIN` | 同目录内核 | 手动指定内核路径 |

## 五、当前可用能力

**数据导入**
- 倾斜摄影 `OSGB`（LOD / PagedLOD / 内嵌纹理）、`OBJ`（含材质纹理）、`GeoTIFF` 影像/DEM、`LAS` 点云
- **矢量**：`GeoJSON` / `Shapefile`（.shp/.dbf/.prj/.cpg，GBK 属性自动解码）/ `GeoPackage`（一个文件内每个要素表各注册一个图层）
- 投影坐标自动换算：高斯克吕格 / 横轴墨卡托按 `.prj` 参数反算到 WGS84（基准不同且无 TOWGS84 时**明确拒绝**而非静默近似）
- **矢量编辑**：画布绘制、顶点拖动、加点/删点、属性编辑，顶点/边**吸附到精确坐标**；就地写回源文件（首次编辑前自动备份为 `.orig`）
- **矢量导出**：`GeoPackage` / `Shapefile`(.zip) / `GeoJSON`

**切片转换**
- 倾斜摄影 → 3DTiles（真几何 b3dm + tileset 1.0 + ENU 地理定位）、影像 → 瓦片金字塔（XYZ/WMTS）
- 模型 → 3DTiles、DEM → Quantized-Mesh 地形、点云 → 3DTiles（pnts）
- 断点续切（分块 Manifest + journal）、Rayon 并行、QEM 模型轻量化

**服务分发**（全部本地文件驱动，无外部数据库）
- 3DTiles（签名防盗链 + 发布审批）、WMTS 1.0.0（KVP + RESTful）、TMS、WMS 1.3.0、WCS 1.0.0、Terrain
- **MVT 矢量瓦片**（纯 Go 编码器：投影 / 裁剪 / 绕向归一化）
- **WFS 2.0** 只读（GetCapabilities / GetFeature，支持 bbox 过滤）

**质检与编辑**
- 几何质检 5 项：退化面 / 法线翻转 / 悬浮块 / 跨 tile 裂缝 / 自相交（可下载 JSON 报告）
- 模型编辑算子：扣除 clip / 压平 flatten / 地面对齐 ground-align
- 合规工具：坐标系识别、Bursa 七参数转换、DEM 区域脱密（任务化 + 留痕）

**平台能力**
- 开放 API（OpenAPI 契约 + 漂移守护测试）、API Key / 配额 / 多租户
- 3D 预览（Cesium）：加载、场景树、属性查询、距离/面积/高程测量
- `.t3d` 打包 / 解包（含 Zip Slip 与哈希校验）、Webhook、任务队列与重试

**尚未实现（界面会明确标注）**：`GLTF/FBX` 导入、`DXF/DWG`、`LAZ` 点云、WFS 事务编辑与
DescribeFeatureType、地形/点云之外的高级轻量化（KTX2/DRACO）、OSGB 增量写回、分布式切片集群。

## 六、常见问题

**端口被占用**
```
desktop: port 8080 is already in use
```
换端口：`PORT=9000 ./start.sh`；或允许顺延：`TANGIS_PORT_AUTO=1 ./start.sh`。

**页面打不开**
确认 `tangis` 进程在运行、端口未被防火墙拦截；查看数据目录下的 `tangis.log`
（macOS 双击 `.app` 时，脚本自身的输出在 `~/TanGIS/start.log`）。

**切片任务失败**
任务详情页可下载日志；常见原因是源路径不存在或内核未随包分发（确认 `tangis-kernel`
与主程序同目录 —— macOS 形态下两者都在 `TanGIS.app/Contents/Resources/`）。

**校验下载完整性**
每个压缩包旁附有 `*.sha256`：

```bash
shasum -a 256 -c tangis-darwin-amd64.zip.sha256
```
