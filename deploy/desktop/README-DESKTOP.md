# TanGIS 桌面单机版

> 打开即用：**不需要 Docker、不需要 PostgreSQL/Redis/MinIO/NATS**，也不需要安装数据库。
> 本目录自带 Web 控制台（已嵌入主二进制），运行后浏览器访问固定端口即可。

## 一、启动

| 系统 | 操作 |
|---|---|
| macOS | 双击 `start.command` |
| Linux | 终端执行 `./start.sh` |
| Windows | 双击 `start.bat` |

默认端口 **8080**，启动后自动打开浏览器：

```
http://127.0.0.1:8080/
```

停止：`stop.command` / `stop.sh` / `stop.bat`。

> macOS 首次运行若提示"无法打开，因为它来自身份不明的开发者"（Gatekeeper），
> 在终端执行一次即可（本发行版未做 Apple 开发者签名）：
>
> ```bash
> xattr -dr com.apple.quarantine start.command tangis tangis-kernel
> ```

## 二、目录说明

| 文件 | 作用 |
|---|---|
| `tangis` / `tangis.exe` | 主服务：HTTP API + Web 控制台（内置） |
| `tangis-kernel` / `.exe` | Rust 切片内核（主程序自动在同目录查找） |
| `start.*` / `stop.*` | 启停脚本 |
| `tangis.log` | 运行日志（启动脚本生成） |

## 三、数据落在哪里

默认 `~/TanGIS/`（目录名唯一且固定，不探测也不沿用任何历史目录；如需换位置，用 `TANGIS_HOME`）：

```
~/TanGIS/
├── tangis.db          # SQLite：任务 / API Key / 配额用量
├── artifacts/         # 产物仓库（瓦片、b3dm、tileset.json…）
└── data/
    ├── logs/          # 每个任务的切片日志（可下载）
    └── outputs/       # 建任务未指定 output 时的自动产物目录
```

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

- 数据切片：OSGB → 3DTiles（真实解析 + 真几何 + 纹理）、影像 → XYZ 瓦片金字塔
- 任务管理：队列、进度、失败重试（10s/60s/300s）、日志下载、取消前的幂等保护
- 数据质检：退化面 / 法线翻转 / 悬浮块 / 跨 tile 裂缝 / 自相交（下载 JSON 报告）
- 模型编辑：clip（抠除）/ flatten（压平）/ ground-align（地面对齐）
- 服务分发：3DTiles、WMTS 1.0.0、TMS、WFS 2.0（只读）、防盗链签名、发布审批
- 打包：`.t3d` 开放容器（打包 / 解包导入）

**暂不可用**：矢量瓦片（MVT）/ WFS 事务编辑 —— 依赖 PostGIS，桌面版尚未改造为文件矢量（GeoPackage/SHP），相关接口返回 503。

## 六、常见问题

**端口被占用**
```
desktop: port 8080 is already in use
```
换端口：`PORT=9000 ./start.sh`；或允许顺延：`TANGIS_PORT_AUTO=1 ./start.sh`。

**页面打不开**
确认 `tangis` 进程在运行、端口未被防火墙拦截；查看 `tangis.log`。

**切片任务失败**
任务详情页可下载日志；常见原因是源路径不存在或内核未随包分发（确认 `tangis-kernel` 与主程序同目录）。
