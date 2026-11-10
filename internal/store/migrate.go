package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrate/*.sql
var migrationFS embed.FS

// migration 是一个已注册的迁移脚本。
type migration struct {
	version string // 文件名去掉 .sql，如 0001_init
	sql     string
}

// Migrate 按文件名顺序执行未应用的迁移。
//
// 每个脚本在一个事务里跑完：SQLite 的 DDL 是事务性的，所以中断不会留下半张表。
// 版本用文件名而不是序号——序零填充到 4 位，字符串排序即执行顺序，
// 而且新增迁移永远不会因为改号而冲突（已有库里记的是文件名）。
func (db *DB) Migrate(ctx context.Context) (applied []string, err error) {
	if _, err := db.rw.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return nil, fmt.Errorf("store: 初始化迁移表: %w", err)
	}

	done, err := db.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}

	all, err := loadMigrations()
	if err != nil {
		return nil, err
	}

	for _, m := range all {
		if _, ok := done[m.version]; ok {
			continue
		}
		if err := db.applyOne(ctx, m); err != nil {
			return applied, err
		}
		applied = append(applied, m.version)
	}
	return applied, nil
}

// SchemaVersion 返回当前库已应用的最大迁移版本，空库返回 ""。
func (db *DB) SchemaVersion(ctx context.Context) (string, error) {
	var v sql.NullString
	err := db.ro.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return "", err
	}
	return v.String, nil
}

func (db *DB) appliedVersions(ctx context.Context) (map[string]struct{}, error) {
	rows, err := db.ro.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: 读取已应用迁移: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = struct{}{}
	}
	return out, rows.Err()
}

func (db *DB) applyOne(ctx context.Context, m migration) error {
	tx, err := db.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 迁移 %s 开启事务: %w", m.version, err)
	}
	defer tx.Rollback() //nolint:errcheck // 成功路径下面显式 Commit

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: 迁移 %s 执行失败: %w", m.version, err)
	}

	// 用 ULID 而不是自增：迁移记录本身也要能按时间读。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
		m.version, Now()); err != nil {
		return fmt.Errorf("store: 迁移 %s 记账失败: %w", m.version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 迁移 %s 提交失败: %w", m.version, err)
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrate")
	if err != nil {
		return nil, fmt.Errorf("store: 读取迁移目录: %w", err)
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile("migrate/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: 读取迁移 %s: %w", e.Name(), err)
		}
		out = append(out, migration{
			version: strings.TrimSuffix(e.Name(), ".sql"),
			sql:     string(b),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}
