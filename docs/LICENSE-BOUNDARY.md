# TanGIS 开源/商业边界（LICENSE-BOUNDARY）

> 版本：v1.0 ｜ 日期：2026-10-04 ｜ 对应 PRD：《GISBox竞品分析与PRD》v1.1 的 F-15（商业化前置）、2.7（合规与开源许可策略）、3.4（定价表）
> 核对方式：本文所有「当前实现状态」均基于 2026-10-04 对仓库源码的实际核对（server/ Go 源码、kernel/ Rust 源码、web/src、deploy/），未实现的功能如实标注，无编造。
> 主 License：**Apache-2.0**（见仓库根 [LICENSE](../LICENSE)，Copyright 2026 草果软件（武汉草果科技有限公司））。

---

## 1. 总原则（Open Core）

1. **开源内核永远免费**：kernel 切片核心、单机任务、基础 OGC/3DTiles 分发、最小鉴权、Cesium 预览、Docker Compose 单机部署，全部 Apache-2.0，任何人不需 License Key 即可使用（对应 PRD 3.4「开源版 ¥0」）。
2. **商业能力以 License Key 解锁**：分布式集群、协同不限席位、企业 SSO/审计、信创离线包支持、License 管控台等，通过 Ed25519 签名的 License 文件开启（对应「专业版 ¥99/月」「企业版 ¥20k 起」）。**当前代码只落了校验骨架与特性挂点，商业特性本身尚未实现**——挂点先行是为了后续实现商业特性时无需再改核心接口。
3. **fail-open 到开源版**：无 License 文件、验签失败或授权过期，系统一律按开源版正常运行，绝不因 License 问题阻塞开源特性（校验逻辑见 `server/internal/license`）。
4. **GPL 隔离**（引用 PRD 2.7-2）：LibreDWG（GPLv3）只能以**独立进程 + 管道通信**方式调用，主程序不得包含/链接 GPL 代码；商业客户推荐 ODA SDK 授权版。当前仓库未引入任何 GPL 依赖。

---

## 2. 开源边界（Apache-2.0，永远免费）

| 特性 | PRD | 当前实现状态（2026-10-04 代码核对） | 关键代码 |
|---|---|---|---|
| 切片内核核心：OSGB/OBJ 解析 → 3DTiles | F-01/F-02 | **已实现（M1 骨架）**：OSGB 二进制解析、OBJ（tobj）、分块 Manifest + LOD 拓扑排序 + **断点续切**、Rayon 分块并行、真实几何 b3dm（含纹理）与 tileset.json。注意：影像/地形/点云切片线、LOD 重建/纹理 atlas 深水区尚未实现 | `kernel/crates/osgb`、`kernel/crates/manifest`、`kernel/crates/cli/src/main.rs`、`cli/src/tiles/` |
| 单机任务编排 | F-04 | **已实现**：NATS JetStream（Stream `TANGIS`/durable consumer）、Worker 状态机（幂等 + 失败重试）、PG/内存双 Store、任务参数版本化（params）、内核日志下载、产物自动上传 MinIO（降级容忍） | `server/internal/queue`、`worker`、`task`、`objectstore` |
| 基础 OGC/3DTiles 分发 | F-03 | **已实现（子集）**：3DTiles 静态分发（本地优先 + MinIO 流式回源、CORS、目录穿越防护）、WMTS（KVP + RESTful）、TMS、已发布服务列表。**WMS/WFS/WCS/MVT/Terrain 未实现** | `server/internal/service`（service.go / wmts.go / tiles.go） |
| 最小鉴权 | F-06 | **已实现**：X-API-Key 多租户（admin/tenant 角色、租户隔离）、HMAC 防盗链签名 URL（sig/expires）、`TANGIS_AUTH=off` 开发兼容开关。注册/登录、配额未实现 | `server/internal/auth`、`api/router.go` |
| 发布审批 | F-21（子集） | **已实现（仅审批开关）**：`POST /api/v1/tasks/:id/approve`（仅 admin），审批通过才进入服务列表与分发路径。**脱密、七参数托管、水印、坐标系转换等其余 F-21 子项未实现** | `api/handlers.go` ApproveTask |
| Web 端 Cesium 预览 | F-05（子集） | **已实现（基础）**：Vue3 + Cesium Viewer 加载 tileset.json、服务列表、点击拾取。场景树/属性查询/测量未实现 | `web/src/views/PreviewView.vue` |
| Docker Compose 单机部署 | 3.2 | **已实现**：PostgreSQL+PostGIS / Redis / MinIO / NATS 四服务 + healthcheck | `deploy/docker-compose.yml` |
| License 校验骨架本身 | F-15 | **已实现**：Ed25519 验签、开源降级、`EnsureFeature` API（见 §4）。校验代码本身随开源版分发 | `server/internal/license` |

---

## 3. 商业边界（未来 License Key 解锁；当前代码可留接口）

| 特性 | PRD | 当前实现状态 | 代码挂点 |
|---|---|---|---|
| 分布式切片集群 / 多 Worker 弹性（K8s HPA） | F-16 | **接口预留（功能未实现）**：仅落了特性门禁——创建任务时 `params.distributed=true` 需有效授权，开源版返回 403；集群调度、多 Worker 弹性本身**零实现**（当前 Worker 为单进程 goroutine 消费） | `license.FeatureDistributed`；`api/handlers.go` CreateTask 门禁 |
| 协同编辑（CRDT 多席位，开源版限 3 席位/商业版不限） | F-17 | **未实现**：无任何 CRDT/WebSocket 协同代码，仅预留特性常量 `collab_unlimited_seats` | `license.FeatureCollab` |
| 企业 SSO / 操作审计 | 3.4 企业版 | **未实现**：仅预留特性常量 `enterprise_sso_audit` | `license.FeatureSSOAudit` |
| 信创离线包支持服务（UOS/麒麟 + ARM64 离线授权） | 3.4 企业版 / 2.4 信创 | **未实现**：无 ARM64 适配与离线包构建，仅预留特性常量 `xinchuang_offline_support` | `license.FeatureXinchuang` |
| License 管控台（签发/吊销/席位管理） | F-15 | **未实现**：目前签发只能用 Ed25519 私钥手工签名 JSON（见 §4），无签发工具与管控界面，仅预留特性常量 `license_console` | `license.FeatureLicenseConsole` |

> 定价对位（引用 PRD 3.4，建议值，M2 与设计伙伴共创后校准）：开源版 ¥0 = §2 全部；专业版 ¥99/月 = 轻量化高级选项、脱密模块、License 企业能力、不限 API 配额；企业版 ≥¥20k/年 = 不限席位协同、DWG(ODA)、私有化交付、SLA、资质配套。

---

## 4. License 文件与校验机制（已落地的骨架）

- **加载**：环境变量 `TANGIS_LICENSE_FILE` 指向 JSON 文件；未设置/文件缺失/JSON 非法/验签失败/授权过期 → 一律按开源版处理（启动日志提示，不报错退出）。
- **格式**：

```json
{
  "plan": "professional",
  "expires": "2027-10-04T00:00:00Z",
  "seats": 10,
  "features": ["distributed_slicing", "collab_unlimited_seats"],
  "sig": "<hex(ed25519 sign( payload_json ))>"
}
```

  `sig` 为对「去掉 `sig` 字段后的规范 JSON」（字段序 plan/expires/seats/features）的 Ed25519 签名 hex。
- **公钥**：内置常量 `devPublicKeyHex`（`server/internal/license/license.go`），**当前为开发占位公钥，生产发布前必须替换为正式发布公钥**（私钥离线保管）。
- **API**：`EnsureFeature(name string) error`——开源特性恒通过；商业特性在无授权/过期/未包含时返回包裹 `ErrFeatureNotLicensed` 的明确错误。nil Checker 安全，等价开源版。
- **特性名清单**：开源 `slicing_single_node` / `distribution_3dtiles` / `distribution_ogc_basic` / `auth_basic` / `preview_cesium` / `deploy_compose`；商业 `distributed_slicing` / `collab_unlimited_seats` / `enterprise_sso_audit` / `xinchuang_offline_support` / `license_console`（以 `server/internal/license/license.go` 常量为准）。

---

## 5. GPL 隔离与第三方依赖合规

1. **GPL 隔离**（引用 PRD 2.7-2）：LibreDWG（GPLv3）仅允许「独立进程 + 管道通信」集成，主程序不含 GPL 代码；DWG 路线以 DXF 优先，商业客户推荐 ODA SDK。**当前仓库未引入 LibreDWG，F-13 尚未开工**。
2. **GDAL 注意**（引用 PRD 2.7/3.2）：MVP 计划复用 GDAL(rust-bindings) 成熟能力。GDAL 本体为 MIT/X11 风格宽松许可，但其运行时可选插件/驱动（如 FileGDB、部分 DWG 驱动）可能引入 copyleft 或专有许可，**引入 GDAL bindings 前必须逐驱动核对许可清单并在发行物中披露**；当前 kernel 未引入 GDAL（现有 Rust 依赖 serde/serde_json/thiserror/toml/flate2/clap/rayon/tobj 均为 MIT/Apache-2.0 宽松双许可）。
3. **Go 侧依赖**（gin/pgx/minio-go/nats.go/go-redis 等）均为 MIT/Apache-2.0，与 Apache-2.0 主 License 兼容。
4. **防白嫖策略**（引用 PRD 2.7-3 / 2.8）：Apache-2.0 + 商标保护 + CLA；企业能力不开源。**商标政策与 CLA 流程尚未建立，为待办项**（本仓库当前无 CLA 文件与 CONTRIBUTING 协议）。
5. kernel workspace 已在 `kernel/Cargo.toml` 声明 `license = "Apache-2.0"`（各 crate `license.workspace = true`）；`server/go.mod` 为 Go modules 格式，本身无 license 字段，以仓库根 LICENSE 为准（Go 生态惯例，无需修改）。

---

## 6. 已知限制

1. **seats 字段未强制**：License 中的席位数仅随文件携带，无在线席位计数，协同功能落地时需补席位管控。
2. **无吊销与在线校验**：纯离线验签，License 泄露后只能等过期；License 管控台（含吊销列表）为商业特性之一，尚未实现。
3. **fail-open 的边界**：校验 fail-open 到开源版意味着自编译者可自行移除校验——Open Core 的真正防线是**商业特性代码本身不开源**（PRD 2.8），而非客户端校验。
4. **dev 公钥**：内置公钥为开发占位，生产发布前必须替换（见 §4），并配套私钥保管与签发流程。
5. **商业特性挂点目前仅 1 处**（`CreateTask` 的 `distributed` 参数）：其余商业特性（协同/SSO/信创/管控台）实现时应各自接入 `EnsureFeature`。
