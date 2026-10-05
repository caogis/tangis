// Package config 提供 apiserver 的环境变量配置加载。
// M1 阶段仅支持 env 读取 + 默认值，后续可扩展文件/配置中心来源。
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config 汇总 apiserver 运行所需的全部配置项。
// 各项均可通过环境变量覆盖，未设置时使用默认值。
type Config struct {
	// HTTP 监听端口，环境变量 PORT，默认 8080
	Port string
	// PostgreSQL 连接串（PostGIS），环境变量 DATABASE_URL；
	// 有值使用 PG 任务存储，为空回退内存存储
	DatabaseURL string
	// Redis 地址（F-03 瓦片/列表缓存），环境变量 TANGIS_REDIS_ADDR，
	// 未设置回退 REDIS_ADDR，再回退 127.0.0.1:16379（对齐 deploy）；
	// 不可用时缓存整体降级（直读存储），不阻塞启动
	RedisAddr string
	// MinIO 对象存储端点，环境变量 MINIO_ENDPOINT（默认对齐 deploy/.env.example）
	MinioEndpoint string
	// MinIO 访问密钥，环境变量 MINIO_ACCESS_KEY
	MinioAccessKey string
	// MinIO 秘密密钥，环境变量 MINIO_SECRET_KEY
	MinioSecretKey string
	// MinIO bucket，环境变量 MINIO_BUCKET
	MinioBucket string
	// NATS 消息队列地址（任务队列选型见 PRD 3.2：NATS JetStream 唯一选型），环境变量 NATS_URL
	NatsURL string
	// 鉴权开关（F-06）：环境变量 TANGIS_AUTH，on/off，默认 on；
	// off 时跳过 API Key 校验与防盗链签名（开发兼容，启动打警告）
	AuthEnabled bool
	// API Key 校验存储；Enabled 时必须非 nil。
	// 防盗链签名密钥（F-06），环境变量 TANGIS_SIGN_SECRET，默认取开发值
	SignSecret string
	// 防盗链签名有效期（秒），环境变量 TANGIS_SIGN_TTL，默认 3600
	SignTTLSeconds int
	// 本地数据目录（内核日志落盘），环境变量 TANGIS_DATA_DIR，默认 data
	DataDir string
	// 瓦片响应缓存 TTL（秒），环境变量 TANGIS_CACHE_TTL，默认 300（F-03）
	CacheTTLSeconds int
	// 任务/服务列表缓存 TTL（秒），环境变量 TANGIS_LIST_CACHE_TTL，默认 5；
	// 写路径失效 + 短 TTL 自然过期保证一致性（F-03）
	ListCacheTTLSeconds int

	// .t3d 导入上传体积上限（字节），环境变量 TANGIS_IMPORT_MAX_BYTES，默认 2 GiB（F-07）
	ImportMaxBytes int64
	// .t3d 导入 ZIP 条目数上限，环境变量 TANGIS_IMPORT_MAX_FILES，默认 65536（F-07）
	ImportMaxFiles int
	// .t3d 导入解压总量上限（字节），环境变量 TANGIS_IMPORT_MAX_TOTAL_BYTES，默认 8 GiB（F-07）
	ImportMaxTotalBytes int64
	// Webhook 事件 HMAC 签名密钥（F-14 第一步），环境变量 TANGIS_WEBHOOK_SECRET，
	// 空则不发送签名头
	WebhookSecret string

	// 矢量 MVT 瓦片缓存 TTL（秒），环境变量 TANGIS_VECTOR_CACHE_TTL，
	// 默认 300（M2-F10a，F-10；key 含 layer/z/x/y）
	VectorCacheTTLSeconds int
	// 矢量单 tile 要素数上限，环境变量 TANGIS_VECTOR_MAX_FEATURES，
	// 默认 100000；超出返回 413（不静默截断）
	VectorMaxFeatures int
	// WFS GetFeature 单次要求数上限，环境变量 TANGIS_WFS_MAX_FEATURES，
	// 默认 10000（M2-F14；count 参数超出时收敛到该值）
	WFSMaxFeatures int
	// 兼容迁移用的管理 API Key，环境变量 TANGIS_API_KEY（未设置回退
	// TANGIS_DEV_API_KEY，默认 tangis-dev-key）；启动时确保该 key 在
	// KeyStore 中以 admin 身份存在（M2-F14 多 Key 迁移兼容）
	APIKey string
	// 租户 Key 默认每日任务创建配额，环境变量 TANGIS_KEYSTORE_DAILY_LIMIT，
	// 默认 0=不限额；key 行 daily_task_limit>0 时按 key 覆盖（M2-F14）
	KeyDailyLimit int

	// Mode 运行模式（桌面单机版路线）：
	//   - server（默认）：依赖 PostgreSQL/MinIO/NATS/Redis 的完整服务端；
	//   - desktop：Go + SQLite + 本地文件系统 + 进程内队列 + embed 前端，
	//     零外部依赖，双击即用（数据落在 ~/TanGIS）。
	// 环境变量 TANGIS_MODE。
	Mode string
	// DesktopWorkers 桌面模式下切片任务的并发 worker 数，
	// 环境变量 TANGIS_DESKTOP_WORKERS，默认 1（串行更安全，SQLite 写锁粗粒度）。
	DesktopWorkers int
	// PortAuto 桌面模式端口被占用时是否自动顺延（TANGIS_PORT_AUTO=1）。
	// 默认 false：固定监听 PORT（安装后访问指定端口即可），占用时报错退出，
	// 避免用户访问到非预期端口。
	PortAuto bool

	// FSBrowse 是否开放本机目录浏览（建任务选路径，替代手填），
	// 环境变量 TANGIS_FS_BROWSE，默认 on（=off 关闭，端点 404）。
	FSBrowse bool
	// FSRoots 目录浏览白名单（逗号分隔的绝对路径），环境变量
	// TANGIS_FS_ROOTS。空=不限制（桌面单机版本机使用即默认语义）；
	// 服务端多租户部署建议收紧到数据目录与上传目录。
	FSRoots []string
}

// 运行模式取值（Mode）。
const (
	ModeServer  = "server"
	ModeDesktop = "desktop"
)

// Load 从环境变量加载配置，返回带默认值的 Config。
func Load() *Config {
	return &Config{
		Port:                envOr("PORT", "8080"),
		DatabaseURL:         envOr("DATABASE_URL", ""),
		RedisAddr:           envFallback("127.0.0.1:16379", "TANGIS_REDIS_ADDR", "REDIS_ADDR"),
		MinioEndpoint:       envOr("MINIO_ENDPOINT", "127.0.0.1:19000"),
		MinioAccessKey:      envOr("MINIO_ACCESS_KEY", "tangis"),
		MinioSecretKey:      envOr("MINIO_SECRET_KEY", "tangis_dev_secret"),
		MinioBucket:         envOr("MINIO_BUCKET", "tangis"),
		NatsURL:             envOr("NATS_URL", "nats://127.0.0.1:14222"),
		AuthEnabled:         envOr("TANGIS_AUTH", "on") != "off",
		SignSecret:          envOr("TANGIS_SIGN_SECRET", "tangis-dev-sign-secret"),
		SignTTLSeconds:      envIntOr("TANGIS_SIGN_TTL", 3600),
		DataDir:             envOr("TANGIS_DATA_DIR", "data"),
		CacheTTLSeconds:     envIntOr("TANGIS_CACHE_TTL", 300),
		ListCacheTTLSeconds: envIntOr("TANGIS_LIST_CACHE_TTL", 5),

		ImportMaxBytes:      envInt64Or("TANGIS_IMPORT_MAX_BYTES", 2<<30),
		ImportMaxFiles:      envIntOr("TANGIS_IMPORT_MAX_FILES", 65536),
		ImportMaxTotalBytes: envInt64Or("TANGIS_IMPORT_MAX_TOTAL_BYTES", 8<<30),
		WebhookSecret:       envOr("TANGIS_WEBHOOK_SECRET", ""),

		VectorCacheTTLSeconds: envIntOr("TANGIS_VECTOR_CACHE_TTL", 300),
		VectorMaxFeatures:     envIntOr("TANGIS_VECTOR_MAX_FEATURES", 100000),
		WFSMaxFeatures:        envIntOr("TANGIS_WFS_MAX_FEATURES", 10000),
		APIKey:                envFallback("tangis-dev-key", "TANGIS_API_KEY", "TANGIS_DEV_API_KEY"),
		KeyDailyLimit:         envIntOr("TANGIS_KEYSTORE_DAILY_LIMIT", 0),

		Mode:           strings.ToLower(envOr("TANGIS_MODE", ModeServer)),
		DesktopWorkers: envIntOr("TANGIS_DESKTOP_WORKERS", 1),
		PortAuto:       envOr("TANGIS_PORT_AUTO", "") == "1",

		FSBrowse: envOr("TANGIS_FS_BROWSE", "on") != "off",
		FSRoots:  splitList(os.Getenv("TANGIS_FS_ROOTS")),
	}
}

// splitList 解析逗号分隔列表（去空白、丢空项）。
func splitList(raw string) []string {
	out := []string{}
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// envFallback 依次取多个环境变量（首参为默认值，其余为 env key），均未设置时用默认值。
func envFallback(def string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64Or(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
