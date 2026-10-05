// apiserver 是 TanGIS M1 MVP 的业务 API 服务入口（PRD 3.1 服务层）。
//
// 两种运行模式（TANGIS_MODE）：
//   - server（默认）：完整服务端，外部依赖 PostgreSQL / MinIO / NATS / Redis；
//   - desktop：桌面单机版（双击即用），依赖退化为 SQLite + 本地文件系统 +
//     进程内队列 + 内存缓存 + embed 前端，全部实现均在 internal/desktop 装配。
//
// 两种模式复用同一套业务层（api / service / worker / task），差别只在装配。
//   - 启动流程：加载配置 -> 任务存储（DATABASE_URL 有值用 PostgreSQL，
//
// 否则内存）-> MinIO（不可用降级）-> NATS JetStream（不可用则降级）->
// Worker 消费任务调内核 CLI 并上传产物 -> 路由 -> 监听信号优雅退出。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tangis/server/internal/api"
	"tangis/server/internal/auth"
	"tangis/server/internal/cache"
	"tangis/server/internal/config"
	"tangis/server/internal/desktop"
	"tangis/server/internal/license"
	"tangis/server/internal/objectstore"
	"tangis/server/internal/queue"
	"tangis/server/internal/service"
	"tangis/server/internal/task"
	"tangis/server/internal/vector"
	"tangis/server/internal/webhook"
	"tangis/server/internal/worker"
)

func main() {
	cfg := config.Load()

	// 商业授权（F-15）：TANGIS_LICENSE_FILE 未设置或校验失败时
	// 按开源版运行（fail-open 到开源版，商业特性调用时才报错）
	lic := license.LoadFromEnv()
	log.Printf("license plan: %s (licensed=%v, env TANGIS_LICENSE_FILE=%q)",
		lic.Plan(), lic.Licensed(), os.Getenv("TANGIS_LICENSE_FILE"))

	// 桌面单机版：零外部依赖，自动打开浏览器（见 internal/desktop）
	if cfg.Mode == config.ModeDesktop {
		if err := desktop.Run(cfg, lic); err != nil {
			log.Fatalf("desktop mode failed: %v", err)
		}
		return
	}

	// 任务存储：DATABASE_URL 有值用 PostgreSQL，失败/为空回退内存
	var store task.Store = task.NewMemoryStore()
	if cfg.DatabaseURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pgStore, err := task.NewPGStore(ctx, cfg.DatabaseURL)
		cancel()
		if err != nil {
			log.Printf("PostgreSQL unavailable (%v), falling back to in-memory store", err)
		} else {
			store = pgStore
			defer pgStore.Close()
			log.Printf("task store: PostgreSQL (%s)", cfg.DatabaseURL)
		}
	} else {
		log.Println("DATABASE_URL empty, task store: in-memory")
	}

	// MinIO 对象存储：不可用则降级（worker 跳过上传、服务发布仅走本地，均不阻塞）
	var minioClient *objectstore.Client
	{
		ctx, cancel := context.WithTimeout(context.Background(), objectstore.DialTimeout)
		mc, err := objectstore.New(ctx, objectstore.ConfigFromEnv())
		cancel()
		if err != nil {
			log.Printf("MinIO unavailable (%v), artifact upload disabled (degraded)", err)
		} else {
			minioClient = mc
			log.Printf("MinIO ready: endpoint %s, bucket %s", objectstore.ConfigFromEnv().Endpoint, mc.Bucket())
		}
	}

	// 连接 NATS 并确保 Stream/Consumer；失败不阻断启动，走降级（任务保持 PENDING）
	var publisher queue.Publisher
	var q *queue.Queue
	// 提到外层作用域：API 层需要它中断运行中的内核进程（F-04 取消/暂停）
	var taskWorker *worker.Worker
	// 任务终态 webhook 通知器（F-14 第一步）：密钥为空仅不签名，通知仍开启
	notifier := &webhook.Notifier{Secret: cfg.WebhookSecret}
	if conn, err := queue.Connect(cfg.NatsURL); err != nil {
		log.Printf("NATS unavailable (%v), running degraded: tasks stay PENDING", err)
	} else {
		q = conn
		publisher = q

		// 启动 Worker 消费任务；同进程内以 goroutine 回调执行
		w := worker.New(store, worker.NewCmdExecutor(),
			os.Getenv("KERNEL_BIN"), os.Getenv("KERNEL_ARGS"), minioClient)
		taskWorker = w
		w.Publisher = publisher // 失败重试的重新入队（F-04）
		w.DataDir = cfg.DataDir // 内核日志落盘（F-04）
		w.Notifier = notifier   // 终态 webhook 通知（F-14 第一步）
		if err := q.Subscribe(w.Handle); err != nil {
			log.Printf("worker subscribe failed (%v), running degraded", err)
			publisher = nil
			q.Close()
			q = nil
		} else {
			log.Printf("worker consuming subject TASKS.* (durable %s), kernel bin %q",
				queue.DurableWorker, w.KernelBin)
		}
	}

	// Redis 瓦片/列表缓存（F-03）：不可用则降级为无缓存（直读存储），不阻塞启动
	var tileCache cache.Cache
	{
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		rc, err := cache.Dial(ctx, cfg.RedisAddr)
		cancel()
		if err != nil {
			log.Printf("Redis unavailable (%v), tile/list cache DISABLED (degraded to direct storage reads)", err)
		} else {
			defer rc.Close()
			tileCache = rc
			log.Printf("redis cache ready: %s (tile ttl %ds, list ttl %ds)",
				cfg.RedisAddr, cfg.CacheTTLSeconds, cfg.ListCacheTTLSeconds)
		}
	}

	// 3DTiles 服务发布：本地产物优先，MinIO 流式回源；
	// 鉴权开启时分发请求须携带防盗链签名（F-06）
	services := &service.Server{
		Store:        store,
		Objects:      minioClient,
		Bucket:       cfg.MinioBucket,
		AuthEnabled:  cfg.AuthEnabled,
		SignSecret:   cfg.SignSecret,
		SignTTL:      time.Duration(cfg.SignTTLSeconds) * time.Second,
		Cache:        tileCache,
		CacheTTL:     time.Duration(cfg.CacheTTLSeconds) * time.Second,
		ListCacheTTL: time.Duration(cfg.ListCacheTTLSeconds) * time.Second,
	}

	// 矢量发布服务（M2-F10a，F-10）：元数据存 DATABASE_URL（PG），
	// 不可用/未配置降级内存元数据（重启丢失注册，不阻塞启动）；
	// 数据面（MVT 查询）由 pgGateway 按需连用户注册的 PostGIS 数据源。
	var vectorServer *vector.Server
	{
		var vectorMeta vector.MetaStore = vector.NewMemoryMetaStore()
		if cfg.DatabaseURL != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			pgMeta, err := vector.NewPGMetaStore(ctx, cfg.DatabaseURL)
			cancel()
			if err != nil {
				log.Printf("vector meta store unavailable (%v), falling back to in-memory", err)
			} else {
				defer pgMeta.Close()
				vectorMeta = pgMeta
			}
		} else {
			log.Println("DATABASE_URL empty, vector meta store: in-memory")
		}
		gw := vector.NewPGGateway()
		vectorServer = &vector.Server{
			Meta:        vectorMeta,
			Gateway:     gw,
			Cache:       tileCache,
			CacheTTL:    time.Duration(cfg.VectorCacheTTLSeconds) * time.Second,
			MaxFeatures: cfg.VectorMaxFeatures,
			WFSMax:      cfg.WFSMaxFeatures,
		}
		defer vectorServer.Close()
		log.Printf("vector service ready (mvt cache ttl %ds, max features/tile %d)",
			cfg.VectorCacheTTLSeconds, cfg.VectorMaxFeatures)
	}

	// API Key 鉴权装配（F-06）：TANGIS_AUTH=off 时跳过（NewRouter 内打警告）；
	// PG 可用时 key 走 api_keys 表，否则内存默认 key（便于本地测试）。
	// M2-F14：多 Key 表（daily_task_limit/disabled 列自动迁移）+ 每日任务配额。
	var pgKeyStore *auth.PGKeyStore
	authOpts := api.AuthOptions{Enabled: cfg.AuthEnabled}
	if cfg.AuthEnabled {
		if cfg.DatabaseURL != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			ks, err := auth.NewPGKeyStore(ctx, cfg.DatabaseURL)
			cancel()
			if err != nil {
				log.Printf("api_keys store unavailable (%v), falling back to in-memory key store", err)
				authOpts.Keys = auth.NewMemoryKeyStore(cfg.APIKey)
			} else {
				pgKeyStore = ks
				defer ks.Close()
				authOpts.Keys = ks
				log.Printf("api key store: PostgreSQL (api_keys table, multi-key + daily quota)")
			}
		} else {
			authOpts.Keys = auth.NewMemoryKeyStore(cfg.APIKey)
			log.Printf("api key store: in-memory (dev default key, tenant default)")
		}
		// 兼容迁移：既有 TANGIS_API_KEY（回退 TANGIS_DEV_API_KEY）确保以
		// admin 身份存在（多 Key 表下老部署平滑升级；已存在则不动）
		if ks, ok := authOpts.Keys.(interface {
			EnsureRawKey(context.Context, string, string, string, string) error
		}); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := ks.EnsureRawKey(ctx, cfg.APIKey, "migrated env admin key", "default", auth.RoleAdmin); err != nil {
				log.Printf("env api key migration skipped: %v", err)
			}
			cancel()
		}
		// 每日任务创建配额：PG 持久化用量，内存兜底（M2-F14）
		var usage auth.UsageStore = auth.NewMemoryUsage()
		if pgKeyStore != nil {
			usage = pgKeyStore.Usage()
		}
		authOpts.Quota = &auth.Quota{Store: usage}
	}

	// 运行时信息与能力开关（GET /api/v1/system，设置页数据源）：
	// 由实际装配结果推导，避免前端硬编码"哪些能力在服务端模式可用"。
	queueMode := "none"
	if publisher != nil {
		queueMode = "nats"
	}
	storageMode := "none"
	if minioClient != nil {
		storageMode = "minio"
	}
	cacheMode := "none"
	if tileCache != nil {
		cacheMode = "redis"
	}
	capabilities := map[string]bool{
		"import_upload": true,
		"task_control":  true,
		"3dtiles":       true,
		"wmts":          true,
		"tms":           true,
		"terrain":       true,
		"pointcloud":    true,
		"qc":            true,
		"edit":          true,
		"package":       true,
		"webhook":       true,
		"compliance":    true,
		"wms":           true,
		"wcs":           true,
		"vector":        vectorServer != nil,
		"wfs":           vectorServer != nil,
		"mvt":           vectorServer != nil,
	}

	router := api.NewRouter(store, publisher, services, api.AuthOptions{
		Enabled:    cfg.AuthEnabled,
		Keys:       authOpts.Keys,
		DataDir:    cfg.DataDir,
		SignSecret: cfg.SignSecret,
		License:    lic,
		// M2-F07：打包下载 MinIO 回源、导入上传冗余、导入安全限制、webhook
		Objects:             minioClient,
		Uploader:            minioClient,
		Notifier:            notifier,
		ImportMaxBytes:      cfg.ImportMaxBytes,
		ImportMaxFiles:      cfg.ImportMaxFiles,
		ImportMaxTotalBytes: cfg.ImportMaxTotalBytes,
		// M2-F10a：矢量发布（PostGIS → MVT）
		Vector: vectorServer,
		// F-04 任务控制：中断运行中的内核进程
		Interrupter: taskWorker,
		// F-21 合规同步算子（坐标系识别 / 七参数）
		KernelBin: os.Getenv("KERNEL_BIN"),
		Runtime: api.RuntimeInfo{
			Mode:         config.ModeServer,
			Version:      api.Version,
			DataDir:      cfg.DataDir,
			KernelBin:    os.Getenv("KERNEL_BIN"),
			Queue:        queueMode,
			Storage:      storageMode,
			Cache:        cacheMode,
			Workers:      1,
			AuthEnabled:  cfg.AuthEnabled,
			LicensePlan:  lic.Plan(),
			Capabilities: capabilities,
		},
	}, tileCache)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: router,
	}

	// 异步启动 HTTP 服务
	go func() {
		log.Printf("tangis apiserver listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	// 等待中断信号（SIGINT/SIGTERM），随后优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down apiserver...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("forced shutdown: %v", err)
	}
	if q != nil {
		q.Close()
	}
	log.Println("apiserver exited")
}
