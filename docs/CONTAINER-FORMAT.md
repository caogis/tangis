# TanGIS 容器格式规范（.t3d 包）

版本：1.0（format version `1`）  
状态：公开规范，任何第三方均可据此实现打包/解包。

`.t3d` 是 TanGIS 用于 3DTiles 产物分发的开放容器格式：一个标准 ZIP 归档，
首条目为 `manifest.json`（清单+完整性校验），其余条目为 `tiles/` 目录下
原样收纳的 3DTiles 资源（`tileset.json` 与全部子资源，目录结构保持不变）。
对标私有化打包格式（如 GBP），但完全公开、无专利、可自由实现。

## 1. 总体结构

```text
town.t3d (ZIP)
├── manifest.json                      ← 必须为 ZIP 第一条目
└── tiles/
    ├── tileset.json                   ← 必须存在（3DTiles 入口）
    ├── metadata.json                  ← 可选（2D 金字塔等扩展元数据）
    └── Data/
        └── 0/
            └── 0.b3dm
```

- ZIP 条目名一律使用 `/` 分隔，UTF-8 编码（对应通用位标志 bit 11）。
- 除 `manifest.json` 外，所有条目必须位于 `tiles/` 前缀之下。
- 不依赖 ZIP 加密、注释、数据描述符等扩展特性；解包方应忽略不认识的
  `manifest.json` 字段（向前兼容）。

## 2. manifest.json

```json
{
  "format": "tangis-3dtiles-package",
  "version": 1,
  "created_at": "2026-10-04T08:00:00Z",
  "task": {
    "id": "rWbcKO",
    "type": "osgb->3dtiles",
    "source": "/data/osgb/town",
    "created_at": "2026-10-04T07:00:00Z"
  },
  "origin": [116.397, 39.909, 0],
  "params": {"origin": [116.397, 39.909, 0]},
  "files": [
    {"path": "tiles/tileset.json", "size": 1820, "sha256": "ab12…"},
    {"path": "tiles/Data/0/0.b3dm", "size": 24576, "sha256": "cd34…"}
  ]
}
```

### 2.1 字段语义

| 字段 | 类型 | 必填 | 语义 |
|---|---|---|---|
| `format` | string | 是 | 固定 `"tangis-3dtiles-package"`，标识容器格式 |
| `version` | int | 是 | 清单格式版本，当前固定 `1`；升级只增不减 |
| `created_at` | string | 是 | 打包时间，RFC 3339 / UTC |
| `task.id` | string | 否 | 产生该包的任务 ID（导入方新建任务，不沿用） |
| `task.type` | string | 否 | 原任务类型（如 `osgb->3dtiles`） |
| `task.source` | string | 否 | 原任务源数据引用（仅追溯用） |
| `task.created_at` | string | 否 | 原任务创建时间 |
| `origin` | [number×3] | 否 | 地理定位 `[lon, lat, height]`（度/米），语义同任务参数 `params.origin` |
| `params` | object | 否 | 原任务参数原样携带（导入方恢复，参数版本化） |
| `files` | array | 是 | 文件清单，逐文件完整性条目，语义见下 |

`files[]` 条目：

| 字段 | 类型 | 必填 | 语义 |
|---|---|---|---|
| `path` | string | 是 | ZIP 内条目名（含 `tiles/` 前缀），`/` 分隔相对路径 |
| `size` | int | 是 | 解压后字节数，必须与实际一致 |
| `sha256` | string | 是 | 解压后内容的 SHA-256，小写 hex（64 字符） |

### 2.2 清单校验规则（解包方必须全部执行）

1. `format == "tangis-3dtiles-package"` 且 `version == 1`，否则拒绝。
2. ZIP 第一条目名必须为 `manifest.json`。
3. `files` 非空，且必须包含 `tiles/tileset.json`（3DTiles 包的入口）。
4. `files[].path` 必须与实际 ZIP 条目一一对应：
   - 条目缺失（清单声明但包内没有）→ 拒绝；
   - 多余条目（包内有但清单未声明）→ 拒绝（防夹带）；
   - `manifest.json` 自身不出现在 `files` 中。
5. 逐文件校验：解压内容字节数 == `size`，SHA-256 == `sha256`；
   任一不符即整体拒绝，明确报错指出首个不一致的路径（不做部分导入）。
6. `path` 安全规则（见 §4）：非法路径直接拒绝。

## 3. 打包规则（打包方）

1. `manifest.json` 必须是 ZIP 的第一条目。
2. 遍历产物目录时按相对路径稳定排序（字典序）写入，保证可复现打包顺序。
3. 每个文件的 `size`/`sha256` 按解压后内容计算（流式读取，不要求整文件驻留内存）。
4. `origin` 取任务参数 `params.origin`（存在且为 `[lon, lat, height]` 数值数组时），
   同时保留原 `params`；两者都不强制存在。

## 4. 安全要求（解包方必须实现）

- **防 Zip Slip**：条目路径规范化后必须仍位于目标目录内。拒绝：
  绝对路径、含 `\` 或 NUL 字节、`path.Clean` 后为空/`..`/以 `../` 开头、
  空路径段。实现上建议「先校验 `path.Clean("/"+p)` 再拼接」。
- **落盘映射**：解包落盘时剥离 `tiles/` 前缀——`tiles/tileset.json`
  写入目标目录根的 `tileset.json`，任务工作目录即 3DTiles 产物目录，
  分发服务可直接按产物相对路径读取（TanGIS 导入语义）。
- **条目数上限**：ZIP 条目总数（含 `manifest.json`）不得超过配置上限
  （TanGIS 默认 65536，`TANGIS_IMPORT_MAX_FILES` 可调）。
- **解压总量上限**：全部条目解压后字节数之和不得超过配置上限
  （TanGIS 默认 8 GiB，`TANGIS_IMPORT_MAX_TOTAL_BYTES` 可调），边解压边计数，
  超限立即中止。
- **上传大小上限**：HTTP 上传体积受 `TANGIS_IMPORT_MAX_BYTES`（默认 2 GiB）限制。
- 仅创建普通文件（目录按需创建）；不还原 ZIP 条目的权限位、符号链接等元数据。

## 5. HTTP API

### 5.1 打包下载

```
GET /api/v1/tasks/{id}/package
```

| 项 | 说明 |
|---|---|
| 鉴权 | `X-API-Key` 或防盗链签名 URL（`?expires=&sig=`，签名覆盖完整请求 path）任一 |
| 前置条件 | 任务 `SUCCEEDED` 且已审批（`Approved`）；产物中存在 `tileset.json` |
| 成功响应 | `200`，`Content-Type: application/zip`，`Content-Disposition: attachment; filename="{id}.t3d"`，zip 流式输出（不全量驻留内存） |
| 失败响应 | `403` 未审批；`404` 任务不存在/无产物；`409` 任务未完成 |
| 审计 | 每次打包记录审计日志（操作者租户、时间、任务 ID、来源 IP） |

签名 URL 生成示例（服务端 `SignURL` 同款算法）：

```
sig = hex(HMAC-SHA256(secret, "/api/v1/tasks/{id}/package\n" + expires))
GET /api/v1/tasks/{id}/package?expires=1770000000&sig=…
```

### 5.2 解包导入

```
POST /api/v1/tasks/import
Content-Type: multipart/form-data
字段：file=<.t3d 文件>（必填）；webhook_url=<URL>（可选）
```

| 项 | 说明 |
|---|---|
| 鉴权 | `X-API-Key`（租户隔离：导入任务归属该 key 的租户） |
| 行为 | 按 §2.2/§4 全量校验 → 落盘到任务工作目录 `data/tasks/{id}/` → 创建任务：`type=t3d-import`、状态直接 `SUCCEEDED`（跳过内核，进入审批→分发链路）、`params` 恢复自 manifest |
| 上传冗余 | 配置了 MinIO 时后台异步上传产物（失败仅告警，不影响任务） |
| 成功响应 | `201`，`{"task": {...}}` |
| 失败响应 | `400` 校验失败（响应体含具体原因，如 `sha256 mismatch: tiles/…`）；`413` 超上传上限 |
| 审计 | 每次导入记录审计日志（操作者租户、时间、文件名、解出文件数/字节数） |

导入成功且请求携带 `webhook_url`（或 manifest `params` 中含 `webhook_url`）时，
按 F-14 语义发送任务事件（见 §6）。

## 6. Webhook 任务事件（F-14 第一步）

任务进入终态（`SUCCEEDED`/`FAILED`）时，若任务参数 `params.webhook_url` 非空，
向该 URL 发送：

```
POST {webhook_url}
Content-Type: application/json
X-Tangis-Signature: hex(HMAC-SHA256(secret, request_body))
```

```json
{
  "task_id": "rWbcKO",
  "status": "SUCCEEDED",
  "timestamp": "2026-10-04T08:10:00Z",
  "error": ""
}
```

- 签名密钥来自 `TANGIS_WEBHOOK_SECRET`；未配置时不发送签名头。
- 失败重试 3 次，指数退避（默认 1s/2s/4s，共 4 次请求含首次）。
- webhook 结果只记日志，不阻塞、不影响任务状态。
- 接收方校验建议：用共享密钥对原始 body 重算 HMAC 并恒定时间比较。

## 7. 配置项汇总

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `TANGIS_IMPORT_MAX_BYTES` | `2147483648`（2 GiB） | 导入上传体积上限（字节） |
| `TANGIS_IMPORT_MAX_FILES` | `65536` | 导入包 ZIP 条目数上限 |
| `TANGIS_IMPORT_MAX_TOTAL_BYTES` | `8589934592`（8 GiB） | 导入解压总量上限（字节） |
| `TANGIS_WEBHOOK_SECRET` | 空 | webhook HMAC 签名密钥，空则不签名 |
