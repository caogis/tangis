// Package desktop 实现 TanGIS 桌面单机版（交付形态：双击即用，零外部依赖）。
//
// 与服务端模式的差异（全部通过替换已有接口实现达成，业务层零改动）：
//
//	PostgreSQL  → SQLite（internal/localdb，modernc.org/sqlite 纯 Go 无 cgo）
//	MinIO       → 本地文件系统（internal/localfs）
//	NATS        → 进程内队列（internal/localqueue）
//	Redis       → 进程内内存缓存（internal/cache.MemoryCache）
//	Web 控制台   → go:embed 静态资源（internal/webui）
//
// 数据全部落在单一用户目录（默认 ~/TanGIS），服务启动后自动打开浏览器。
//
// 矢量发布在桌面模式走**本地文件矢量**（GeoJSON，纯 Go 投影/裁剪/MVT 编码），
// 不依赖 PostGIS：注册表持久化到 <dataDir>/vector/registry.json。
package desktop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"tangis/server/internal/api"
	"tangis/server/internal/auth"
	"tangis/server/internal/cache"
	"tangis/server/internal/config"
	"tangis/server/internal/license"
	"tangis/server/internal/localdb"
	"tangis/server/internal/localfs"
	"tangis/server/internal/localqueue"
	"tangis/server/internal/service"
	"tangis/server/internal/vector"
	"tangis/server/internal/webhook"
	"tangis/server/internal/webui"
	"tangis/server/internal/worker"
)

// Fatalf/Open 等之外的启动打印统一走 log，便于 Windows 无控制台环境下重定向到文件。

// Layout 用户数据目录下的路径布局。
type Layout struct {
	Home      string // 数据根目录（默认 ~/TanGIS）
	DB        string // SQLite 数据库文件
	Artifacts string // 产物仓库（替代 MinIO bucket）
	Data      string // 内核日志等运行数据（data/logs/{task_id}.log）
}

// HomeDir 解析数据根目录：TANGIS_HOME > ~/TanGIS。
//
// 目录名唯一且固定（TanGIS）：不探测、不沿用任何历史目录名，
// 避免同一台机器上出现「数据到底在哪个目录」的歧义。
func HomeDir() (string, error) {
	if v := os.Getenv("TANGIS_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("desktop: resolve home dir: %w", err)
	}
	return filepath.Join(home, "TanGIS"), nil
}

// EnsureLayout 创建数据目录布局（幂等）。
func EnsureLayout() (Layout, error) {
	home, err := HomeDir()
	if err != nil {
		return Layout{}, err
	}
	l := Layout{
		Home:      home,
		DB:        filepath.Join(home, "tangis.db"),
		Artifacts: filepath.Join(home, "artifacts"),
		Data:      filepath.Join(home, "data"),
	}
	for _, d := range []string{home, l.Artifacts, filepath.Join(l.Data, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Layout{}, fmt.Errorf("desktop: create dir %s: %w", d, err)
		}
	}
	return l, nil
}

// Run 以桌面模式启动服务：装配本地依赖 → 注册路由 → 选端口 → 打开浏览器 → 等待退出信号。
func Run(cfg *config.Config, lic *license.Checker) error {
	l, err := EnsureLayout()
	if err != nil {
		return err
	}

	// 任务/Key/配额持久化：单个 SQLite 文件（纯 Go 驱动，无 cgo）
	store, err := localdb.Open(l.DB)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SeedKey(cfg.APIKey, "desktop local key", "default", auth.RoleAdmin); err != nil {
		log.Printf("desktop: seed api key failed: %v", err)
	}
	log.Printf("desktop: data dir %s (sqlite %s)", l.Home, filepath.Base(l.DB))

	// 对象存储替代：本地文件系统
	objects, err := localfs.New(l.Artifacts)
	if err != nil {
		return err
	}

	// 缓存替代：进程内（桌面单用户无需 Redis）
	tileCache := cache.NewMemoryCache()

	services := &service.Server{
		Store:        store,
		Objects:      objects,
		Bucket:       "local", // 桌面模式：本地文件系统即"bucket"
		AuthEnabled:  cfg.AuthEnabled,
		SignSecret:   cfg.SignSecret,
		SignTTL:      time.Duration(cfg.SignTTLSeconds) * time.Second,
		Cache:        tileCache,
		CacheTTL:     time.Duration(cfg.CacheTTLSeconds) * time.Second,
		ListCacheTTL: time.Duration(cfg.ListCacheTTLSeconds) * time.Second,
	}

	// 队列替代：进程内队列 + worker 回调
	lq := localqueue.New(cfg.DesktopWorkers)
	notifier := &webhook.Notifier{Secret: cfg.WebhookSecret}
	w := worker.New(store, worker.NewCmdExecutor(), ResolveKernel(os.Getenv("KERNEL_BIN")), os.Getenv("KERNEL_ARGS"), objects)
	w.Publisher = lq
	w.DataDir = l.Data
	w.Notifier = notifier
	if err := lq.Subscribe(w.Handle); err != nil {
		return fmt.Errorf("desktop: start worker: %w", err)
	}
	defer lq.Close()
	log.Printf("desktop: worker ready (%d concurrent, kernel %q)", cfg.DesktopWorkers, w.KernelBin)

	// 矢量发布（M2-F10a）：桌面单机版用**本地文件矢量**替代 PostGIS。
	// 注册表落 <dataDir>/vector/registry.json（重启不丢图层）；
	// 数据面 = 文件网关（读 GeoJSON → 投影/裁剪 → 纯 Go MVT 编码）。
	vectorStore, verr := vector.NewJSONMetaStore(filepath.Join(l.Data, "vector", "registry.json"))
	if verr != nil {
		log.Printf("desktop: 矢量注册表不可用，矢量发布已禁用: %v", verr)
	}
	var vectorServer *vector.Server
	if vectorStore != nil {
		vectorServer = &vector.Server{
			Meta:     vectorStore,
			Gateway:  vector.NewRouterGateway(vector.NewFileGateway(), nil),
			Cache:    tileCache,
			CacheTTL: time.Duration(cfg.CacheTTLSeconds) * time.Second,
		}
		log.Printf("desktop: 文件矢量就绪（注册表 %s）", vectorStore.Path())
	}

	router := api.NewRouter(store, lq, services, api.AuthOptions{
		Enabled:             cfg.AuthEnabled,
		Keys:                store,
		DataDir:             l.Data,
		SignSecret:          cfg.SignSecret,
		License:             lic,
		Uploader:            objects,
		Notifier:            notifier,
		ImportMaxBytes:      cfg.ImportMaxBytes,
		ImportMaxFiles:      cfg.ImportMaxFiles,
		ImportMaxTotalBytes: cfg.ImportMaxTotalBytes,
		Quota:               &auth.Quota{Store: store},
		// 矢量发布：本地文件矢量（GeoJSON），不依赖 PostGIS
		Vector: vectorServer,
		// F-04 任务控制：中断运行中的内核进程（取消/暂停）
		Interrupter: w,
		// F-21 合规同步算子（坐标系识别 / 七参数）复用同一内核
		KernelBin: w.KernelBin,
		// 本机目录浏览（建任务选路径）
		FSBrowseEnabled: cfg.FSBrowse,
		FSBrowseRoots:   cfg.FSRoots,
		Runtime: api.RuntimeInfo{
			Mode:        config.ModeDesktop,
			Version:     api.Version,
			DataDir:     l.Data,
			KernelBin:   w.KernelBin,
			Queue:       "local",
			Storage:     "localfs",
			Cache:       "memory",
			Workers:     cfg.DesktopWorkers,
			AuthEnabled: cfg.AuthEnabled,
			LicensePlan: lic.Plan(),
			Capabilities: map[string]bool{
				"import_upload": true,
				"task_control":  true,
				"3dtiles":       true,
				"wmts":          true,
				"tms":           true,
				// 地形服务：产物 layer.json + quantized-mesh 走本地分发，桌面模式同样可用
				"terrain":    true,
				"pointcloud": true,
				"qc":         true,
				"edit":       true,
				"package":    true,
				"webhook":    true,
				// F-21 合规：坐标系识别 / 七参数同步算子 + DEM 脱密任务线
				"compliance": true,
				// WMS/WCS 与 WMTS/TMS 共用影像金字塔，桌面模式同样可用
				"wms": true,
				"wcs": true,
				// 矢量链路：桌面模式走本地文件矢量（GeoJSON → MVT / WFS）
				"vector": vectorServer != nil,
				"wfs":    vectorServer != nil,
				"mvt":    vectorServer != nil,
				// 本机目录浏览（建任务选路径）：桌面版即本机使用，默认开放
				"fs_browse": cfg.FSBrowse,
			},
		},
	}, tileCache)

	// Web 控制台托管（embed 静态资源 + SPA 回落）
	ui, err := webui.Handler()
	if err != nil {
		return fmt.Errorf("desktop: web ui: %w", err)
	}
	if webui.Stub() {
		log.Printf("desktop: WARNING embedded web UI is a placeholder; run `make ui` to refresh server/internal/webui/dist")
	}
	router.NoRoute(gin.WrapH(ui))

	// 一次 Listen 并复用该 listener：既避免"探测成功但绑定失败"的竞态，
	// 也消除 IPv4/IPv6 双栈导致的端口可用性误判。
	// 端口策略（安装后访问指定端口即可用）：默认**固定**监听 cfg.Port；
	// 被占用时按 TANGIS_PORT_AUTO 决定——为 1 则向后顺延并打印醒目警告，
	// 否则直接报错退出，避免用户访问了错误端口而不自知。
	ln, port, err := listenPort(cfg.Port, cfg.PortAuto)
	if err != nil {
		return err
	}
	if port != cfg.Port {
		log.Printf("desktop: WARNING port %s was busy, using %s instead (access via http://127.0.0.1:%s/)", cfg.Port, port, port)
	}
	url := fmt.Sprintf("http://127.0.0.1:%s/", port)

	srv := &http.Server{Handler: router}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("desktop: http server error: %v", err)
		}
	}()

	printBanner(url, l, cfg)

	if os.Getenv("TANGIS_NO_BROWSER") != "1" {
		OpenBrowser(url)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("desktop: shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("desktop: forced shutdown: %v", err)
	}
	log.Println("desktop: exited")
	return nil
}

func printBanner(url string, l Layout, cfg *config.Config) {
	log.Printf("--------------------------------------------------")
	log.Printf("TanGIS 已启动")
	log.Printf("  控制台：%s", url)
	log.Printf("  数据目录：%s", l.Home)
	log.Printf("  API Key：%s", cfg.APIKey)
	if !cfg.AuthEnabled {
		log.Printf("  鉴权：已关闭（TANGIS_AUTH=off）")
	}
	log.Printf("--------------------------------------------------")
}

// listenPort 监听端口并返回该 listener 与实际端口（保持持有 listener，
// 消除"探测可用 -> 释放 -> 再绑定"的竞态与 IPv4/IPv6 双栈误判）。
//
// 端口策略：preferred 优先固定监听；当 auto 为 true 时才向后顺延
// （最多 50 个）。固定优先是为了让用户始终访问「指定端口」。
func listenPort(preferred string, auto bool) (net.Listener, string, error) {
	start, err := strconv.Atoi(preferred)
	if err != nil || start <= 0 || start > 65535 {
		start = 8080
	}
	p := strconv.Itoa(start)
	ln, err := net.Listen("tcp", ":"+p)
	if err == nil {
		return ln, p, nil
	}
	lastErr := err
	if !auto {
		return nil, "", fmt.Errorf(
			"desktop: port %s is already in use (%v)\n"+
				"  → 指定其他端口：PORT=%d ./tangis\n"+
				"  → 或允许自动顺延：TANGIS_PORT_AUTO=1 ./tangis",
			p, err, start+1)
	}
	for i := 1; i < 50; i++ {
		p := strconv.Itoa(start + i)
		ln, err := net.Listen("tcp", ":"+p)
		if err == nil {
			return ln, p, nil
		}
		lastErr = err
	}
	return nil, "", fmt.Errorf("desktop: no free port in [%d,%d): %w", start, start+50, lastErr)
}

// OpenBrowser 在默认浏览器中打开地址；失败仅告警（不阻断服务）。
func OpenBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("desktop: cannot open browser automatically (%v), please visit %s", err, url)
	}
}

// ResolveKernel 定位 Rust 切片内核可执行文件：
// KERNEL_BIN > 可执行文件同级目录 > 源码树的 target/debug > PATH。
func ResolveKernel(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), kernelName())
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	if p, err := exec.LookPath("tangis-kernel"); err == nil {
		return p
	}
	return worker.DefaultKernelBin
}

func kernelName() string {
	if runtime.GOOS == "windows" {
		return "tangis-kernel.exe"
	}
	return "tangis-kernel"
}
