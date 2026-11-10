# Lumen — 开发入口
#
# 约定：所有目标都在仓库根执行；GOFLAGS 统一关闭 cgo，
# 保证构建产物是纯 Go 单二进制（modernc.org/sqlite 是纯 Go 实现）。
#
# Windows 上 test / race 都会偶发 `Access is denied` 或
# `The process cannot access the file ... being used by another process`：
# 杀软在扫描刚编译出来的测试二进制，不是测试失败。原样重跑一次即可，
# 别去改代码。详见 race 目标的注释。

SHELL := /bin/bash
BINARY  := lumen
MODULE  := github.com/Chenglp/lumen

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildTime=$(BUILD_TIME)

export CGO_ENABLED := 0

.PHONY: all build test fmt vet lint tidy run migrate clean check

all: check build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)$(if $(filter windows,$(shell go env GOOS)),.exe,) ./cmd/lumen

test:
	go test ./... -count=1

# 竞态检测：store 的双句柄设计必须验，否则并发写会静默丢数据。
#
# -race 需要 cgo（ThreadSanitizer 是 C++ runtime），与上面的 CGO_ENABLED=0 相反。
# 本机需要 C 编译器：Windows 上是 WinLibs mingw-w64（gcc 16.2.0 POSIX UCRT），
# 解压在 E:\WB\tools\winlibs\mingw64，需要手动加进 PATH 才能编：
#
#	export PATH="/e/WB/tools/winlibs/mingw64/bin:$PATH"
#
# 不加 PATH 的报错是 "cgo: C compiler "gcc" not found"，不是测试失败——
# 别把它读成"race 测出问题了"。
# 发布产物不受影响：build 目标仍然 CGO_ENABLED=0，输出纯 Go 单文件。
#
# -p 1 是必须的，不是保守：Windows 上并行编包会同时产出多个带 race 插桩的
# 测试二进制（每个都比普通二进制大得多），杀软实时扫描会在 Go 删除它们时
# 抓住文件句柄，报 "Access is denied" 而不是测试失败。串行大幅降低概率
# 并更快（实测 13 秒 vs 40 秒，因为不再有扫描争用）。
#
# 但它是「降低概率」不是「消除」：串行跑完整个 ./... 时仍偶发命中一次
# （实测 2026-11-11 首次全新编译时 pipeline.test.exe 被拦住，原样重跑即过）。
# 看到 Access is denied 先重跑一次再判断，别去改代码。Linux 上无此问题。
race:
	CGO_ENABLED=1 go test ./... -race -count=1 -p 1

fmt:
	gofmt -w -s .

vet:
	go vet ./...

tidy:
	go mod tidy

# 一键自检：格式、静态检查、测试
check: fmt vet test

run:
	go run ./cmd/lumen -data ./data

migrate:
	go run ./cmd/lumen -data ./data -migrate

clean:
	rm -rf bin dist
