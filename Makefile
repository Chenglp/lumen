# Lumen — 开发入口
#
# 约定：所有目标都在仓库根执行；GOFLAGS 统一关闭 cgo，
# 保证构建产物是纯 Go 单二进制（modernc.org/sqlite 是纯 Go 实现）。

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
# 本机需要 C 编译器（Windows 上是 mingw-w64，已装 WinLibs POSIX UCRT）。
# 发布产物不受影响：build 目标仍然 CGO_ENABLED=0，输出纯 Go 单文件。
race:
	CGO_ENABLED=1 go test ./... -race -count=1

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
