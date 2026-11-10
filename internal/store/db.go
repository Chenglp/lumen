// Package store 是唯一允许出现 SQL 的地方。
//
// api 层不许出现 db.Query —— 否则"counts 与 items 同事务"这类约束
// 会被随手破坏，而这类破坏只会表现为"列表有 20 条但角标写 21"。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go，无 cgo：单二进制跨平台打包的前提
)

// DB 持有两个句柄。
//
// SQLite 在 WAL 下允许并发读、单写。Go 的 database/sql 连接池会自然地并发写，
// 然后撞 database is locked。做法是读写分离：
//
//	写句柄：MaxOpenConns(1)，让写请求在 Go 侧排队，而不是在 SQLite 侧争锁超时。
//	        代价是写吞吐上限 = 单次事务耗时。对本产品足够——
//	        最大写压力是扫描器每 200 文件一次批量插入。
//	读句柄：多连接，query_only 兜底。
type DB struct {
	rw   *sql.DB
	ro   *sql.DB
	path string
}

// pragma 参数逐连接生效，所以必须写进 DSN 而不是 Open 之后 Exec——
// 后者只作用于池里恰好被拿到的那一条连接，新开一条就又回到默认值。
const rwPragmas = "busy_timeout(5000)&_pragma=journal_mode(WAL)" +
	"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)" +
	"&_pragma=mmap_size(268435456)&_pragma=temp_store(MEMORY)"

// 读句柄用 query_only(1) 而不是 mode=ro。
//
// 框架原文写的是 mode=ro&_pragma=query_only(1)。但 WAL 库的只读打开需要
// 读写 -shm 共享内存文件，mode=ro 在部分平台/权限组合下会直接报
// "unable to open database file"，表现为"服务端起不来"而不是"只读"。
// query_only(1) 在 SQLite 层拒绝任何写入，语义相同，且不会打不开库。
const roPragmas = "busy_timeout(5000)&_pragma=query_only(1)" +
	"&_pragma=foreign_keys(ON)&_pragma=mmap_size(268435456)" +
	"&_pragma=temp_store(MEMORY)"

// Open 打开（必要时创建）数据库。
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("store: 数据库路径为空")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: 创建数据目录 %s: %w", dir, err)
	}

	rw, err := sql.Open("sqlite", dsn(path, rwPragmas))
	if err != nil {
		return nil, fmt.Errorf("store: 打开写句柄: %w", err)
	}
	rw.SetMaxOpenConns(1)
	rw.SetMaxIdleConns(1)
	rw.SetConnMaxLifetime(0)

	if err := rw.Ping(); err != nil {
		rw.Close()
		return nil, fmt.Errorf("store: 写句柄不可用: %w", err)
	}

	ro, err := sql.Open("sqlite", dsn(path, roPragmas))
	if err != nil {
		rw.Close()
		return nil, fmt.Errorf("store: 打开读句柄: %w", err)
	}
	ro.SetMaxOpenConns(8)
	ro.SetMaxIdleConns(8)
	ro.SetConnMaxLifetime(0)

	if err := ro.Ping(); err != nil {
		rw.Close()
		ro.Close()
		return nil, fmt.Errorf("store: 读句柄不可用: %w", err)
	}

	return &DB{rw: rw, ro: ro, path: path}, nil
}

// dsn 拼 SQLite URI。路径里的空格/#/? 必须转义，否则会被当成 URI 结构，
// 表现为"库里一片空白但程序不报错"——因为它在另一个位置上建了个新库。
func dsn(path, params string) string {
	p := filepath.ToSlash(path)
	p = strings.NewReplacer(" ", "%20", "#", "%23", "?", "%3F").Replace(p)
	return "file:" + p + "?_pragma=" + params
}

// Close 关闭两个句柄。
func (db *DB) Close() error {
	var err error
	if db.ro != nil {
		if e := db.ro.Close(); e != nil {
			err = e
		}
	}
	if db.rw != nil {
		if e := db.rw.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// Read 返回只读句柄，用于列表/详情/统计。
func (db *DB) Read() *sql.DB { return db.ro }

// Write 返回写句柄。任何写操作都走这里，天然串行。
func (db *DB) Write() *sql.DB { return db.rw }

// Path 返回数据库文件路径。
func (db *DB) Path() string { return db.path }

// Ping 检查两个句柄都还活着。
func (db *DB) Ping(ctx context.Context) error {
	if err := db.rw.PingContext(ctx); err != nil {
		return fmt.Errorf("写句柄: %w", err)
	}
	if err := db.ro.PingContext(ctx); err != nil {
		return fmt.Errorf("读句柄: %w", err)
	}
	return nil
}

// Now 返回项目统一的时间表示（unix 毫秒）。所有落库时间都走它，
// 避免一半 unix 秒一半毫秒。
func Now() int64 { return time.Now().UnixMilli() }
