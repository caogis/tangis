.PHONY: build test run tidy vet clean ui desktop bench

# 前端构建产物目录：webui 包以 go:embed 把它嵌入二进制
WEB_DIST := ../web/dist
EMBED_DIST := internal/webui/dist

build:
	go build ./...

test:
	go test ./... -v

run:
	go run ./cmd/apiserver

tidy:
	go mod tidy

vet:
	go vet ./...

clean:
	rm -f apiserver bin/apiserver bin/tangis-desktop

# ui：构建前端并把产物同步到embed 目录（桌面单机版必须步骤）
# 采用覆盖式复制而非删除重建：既避免误删，也兼容只读/受保护目录。
ui:
	cd ../web && npm run build
	mkdir -p $(EMBED_DIST)
	cp -R $(WEB_DIST)/. $(EMBED_DIST)/
	@echo "web dist -> $(EMBED_DIST)"

# desktop：桌面单机版单二进制（SQLite + 本地存储 + embed 前端，零外部依赖）
# 用法：make desktop && TANGIS_MODE=desktop ./bin/tangis-desktop
desktop: ui
	go build -ldflags "-s -w" -o bin/tangis-desktop ./cmd/apiserver
	@echo "build ok: bin/tangis-desktop"

# desktop-fast：跳过前端重建，复用已存在的 embed dist
desktop-fast:
	go build -ldflags "-s -w" -o bin/tangis-desktop ./cmd/apiserver

# bench：并发跑测试多轮，便于捕捉 SQLite/并发相关偶发问题
bench:
	go test ./... -count=3

# ---- 桌面单机版打包（无需签名：解压后运行 start 脚本，浏览器访问固定端口）----
GOOS_CUR := $(shell go env GOOS)
GOARCH_CUR := $(shell go env GOARCH)
EXE := $(if $(filter windows,$(GOOS_CUR)),.exe,)
DIST := ../dist/tangis-$(GOOS_CUR)-$(GOARCH_CUR)

# kernel：Rust 切片内核 release 构建（打包时随主二进制同目录分发）
kernel:
	cd ../kernel && cargo build --release

# dist：前端 + 主二进制 + 内核 + 启停脚本 → dist/tangis-<os>-<arch>/
# 目标目录结构（用户只需运行 start 脚本）：
#   tangis            # 主二进制（ embed 前端，自带 Web 控制台）
#   tangis-kernel     # 切片内核（sidecar）
#   start.sh / .command / .bat
#   stop.sh / .bat
#   README-DESKTOP.md
dist: ui kernel
	rm -rf $(DIST)
	mkdir -p $(DIST)
	go build -ldflags "-s -w" -o $(DIST)/tangis$(EXE) ./cmd/apiserver
	cp ../kernel/target/release/tangis-kernel$(EXE) $(DIST)/
	cp ../deploy/desktop/start.sh ../deploy/desktop/stop.sh $(DIST)/
	cp ../deploy/desktop/start.bat ../deploy/desktop/stop.bat $(DIST)/
	cp ../deploy/desktop/start.sh $(DIST)/start.command
	cp ../deploy/desktop/stop.sh $(DIST)/stop.command
	chmod +x $(DIST)/start.sh $(DIST)/stop.sh $(DIST)/start.command $(DIST)/stop.command
	cp ../deploy/desktop/README-DESKTOP.md $(DIST)/README.md
	@echo "package ready: $(DIST)"

# dist-go：只交叉编译主二进制（不含 Rust 内核，用于 CI 分平台构建）
dist-go: ui
	rm -rf ../dist/tangis-$(GOOS_TARGET)-$(GOARCH_TARGET)
	mkdir -p ../dist/tangis-$(GOOS_TARGET)-$(GOARCH_TARGET)
	GOOS=$(GOOS_TARGET) GOARCH=$(GOARCH_TARGET) CGO_ENABLED=0 \
		go build -ldflags "-s -w" -o ../dist/tangis-$(GOOS_TARGET)-$(GOARCH_TARGET)/tangis$(if $(filter windows,$(GOOS_TARGET)),.exe,) ./cmd/apiserver
	@echo "go binary ready: ../dist/tangis-$(GOOS_TARGET)-$(GOARCH_TARGET)"
