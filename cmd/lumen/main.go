// Command lumen 是 Lumen 的自托管媒体库服务端。
//
// 当前阶段：S1（store + 迁移 + 路径归一化）。HTTP 层尚未接入，
// 所以本命令只做装配与自检：打开库、跑迁移、打印版本后退出。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Chenglp/lumen/internal/store"
)

// 版本信息在构建时注入：-ldflags "-X main.version=..."
var (
	version   = "0.1.0-dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "lumen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dataDir     = flag.String("data", defaultDataDir(), "数据目录（lumen.db 所在处，必须在本地盘）")
		showVer     = flag.Bool("version", false, "打印版本后退出")
		migrateOnly = flag.Bool("migrate", false, "只执行迁移后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lumen %s (commit %s, built %s)\n", version, commit, buildTime)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := filepath.Join(*dataDir, "lumen.db")
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Ping(ctx); err != nil {
		return fmt.Errorf("数据库自检失败: %w", err)
	}

	applied, err := db.Migrate(ctx)
	if err != nil {
		return err
	}
	for _, v := range applied {
		fmt.Printf("migrate: 已应用 %s\n", v)
	}

	schemaVer, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("lumen %s | db=%s | schema=%s | 本次迁移 %d 个\n",
		version, dbPath, schemaVer, len(applied))

	if *migrateOnly {
		return nil
	}

	// S1 阶段没有长驻循环；这里留出装配点，S2 起接 HTTP/WS 与 pipeline。
	fmt.Println("服务端尚未接入 HTTP 层（S2）。按 Ctrl-C 退出。")
	<-ctx.Done()
	fmt.Println("已退出。")
	return nil
}

// defaultDataDir 返回默认数据目录。
//
// 硬要求：媒体卷与数据目录必须分开配置（框架 §1.1）。
// lumen.db 只能放在本地盘——SQLite 的 WAL 依赖 POSIX 文件锁与 mmap 一致性，
// 放在 SMB/NFS 上会静默损坏，不是报错而是数据坏掉。
func defaultDataDir() string {
	if v := os.Getenv("LUMEN_DATA_DIR"); v != "" {
		return v
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	return filepath.Join(base, "Lumen")
}
