# 王国保卫战 Dove 版 更新器
#
# 常用：
#   make              编译当前平台 -> dist/krd-updater
#   make windows      编译 Windows x64 -> dist/krd-updater.exe
#   make dist         打包 Windows 压缩包（含 exe 与示例配置）
#   make deps         拉取依赖（仅在升级依赖时需要；仓库自带 vendor/，平时完全离线）
#   make test         跑单元测试
#   make gui          本机编译并直接打开图形界面（调试用）
#
# Go 版本锁在 1.27.3：GOTOOLCHAIN 会强制使用这个版本，本机没有就自动下载。
# 拉不到模块时代理顺序：国内镜像在前，官方在后。
# 也可以手动覆盖：make deps GOPROXY=https://goproxy.io,direct

APP        := krd-updater
VERSION    ?= 1.0.0
GO_VERSION := 1.27.3
BIN_DIR    := dist
GO         ?= go
LDFLAGS    := -s -w -X main.Version=$(VERSION)

# ---- 依赖代理：国内镜像在前，逐个试到能通 ----
# goproxy.cn            七牛云
# goproxy.io            国内备用
# mirrors.aliyun.com    阿里云
# mirrors.cloud.tencent.com   腾讯云
# goproxy.bj.bcebos.com 百度云
# proxy.golang.com.cn   官方 CN
# direct                直连（配合 HTTPS_PROXY 或 ghproxy 使用）
GOPROXY_LIST := https://goproxy.cn|https://goproxy.io|https://mirrors.aliyun.com/goproxy/|https://mirrors.cloud.tencent.com/go/|https://goproxy.bj.bcebos.com|https://mirrors.huaweicloud.com/repository/goproxy/|https://proxy.golang.com.cn|https://proxy.golang.org|direct

export GOTOOLCHAIN ?= go$(GO_VERSION)
export GOPROXY ?= https://goproxy.cn,https://goproxy.io,https://mirrors.aliyun.com/goproxy/,https://mirrors.cloud.tencent.com/go/,https://proxy.golang.org,direct
export GOSUMDB  ?= off
export CGO_ENABLED ?= 0

ifeq ($(OS),Windows_NT)
SHELL := cmd.exe
endif

.PHONY: all build windows windows-arm64 linux darwin dist test vet fmt tidy deps run gui clean help check-go

all: build

# 打印实际使用的 Go 版本，版本不对时直接停下
check-go:
	@$(GO) version

build: check-go
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP) .

windows:
	GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP).exe .

windows-arm64:
	GOOS=windows GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP)-arm64.exe .

linux:
	GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP)-linux .

darwin:
	GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP)-macos .

# 生成一个可以整个丢给别人的 Windows 包
dist: windows
	cp -f krd-updater.example.jsonc $(BIN_DIR)/
	cd $(BIN_DIR) && zip -q -r $(APP)-$(VERSION)-windows.zip $(APP).exe krd-updater.example.jsonc

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

# 拉依赖：脚本会依次尝试上面那串镜像
deps:
	bash scripts/deps.sh

run:
	$(GO) run . wizard

gui:
	$(GO) run . gui

clean:
	rm -rf $(BIN_DIR)

help:
	@sed -n '1,20p' Makefile
