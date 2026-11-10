package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// 缓存种类。字符串落库，所以新增种类要同步迁移里的注释。
const (
	TMDBKindDetail = "detail"
	TMDBKindSearch = "search"
	TMDBKindSeason = "season"
	TMDBKindConfig = "config"
)

// TMDBEntry 是一条缓存记录。
//
// Payload 存的是**投影后的 JSON**，不是 TMDB 的原始响应。原始响应一条搜索
// 就有 20 个对象 × 二十几个字段（含 adult / video / genre_ids 数组等我们
// 永不读取的东西），落库体积会是投影后的 5–10 倍。5 万文件量级下这是
// GB 与百 MB 的差别，而多存的部分没有任何读者。
//
// 代价：将来要用到没投影的字段时，旧缓存必须整体失效。所以 payload 里带
// schema 版本号（见 tmdb.cacheSchema），读出来版本不符就当 miss。
type TMDBEntry struct {
	Key       string
	Kind      string
	MediaType string
	TMDBID    int64 // 搜索缓存为 0
	Language  string
	Payload   string
	FetchedAt int64 // 毫秒
}

// GetTMDBCache 取一条未过期的缓存。
//
// maxAge <= 0 表示不限制；调用方负责把它夹到政策上界（180 天）以内。
// 过期判断放在 SQL 里而不是取回来再比：清扫是批量删，判断是逐行读，
// 两种访问模式的索引不同，混在一起会让 idx_tmdb_cache_fetched 用不上。
func (db *DB) GetTMDBCache(ctx context.Context, key string, maxAge time.Duration) (TMDBEntry, bool, error) {
	var (
		e   TMDBEntry
		cut int64
	)
	if maxAge > 0 {
		cut = time.Now().Add(-maxAge).UnixMilli()
	}

	err := db.ro.QueryRowContext(ctx, `
		SELECT cache_key, kind, COALESCE(media_type,''), COALESCE(tmdb_id,0),
		       language, payload, fetched_at
		FROM tmdb_cache
		WHERE cache_key = ? AND fetched_at >= ?`, key, cut).
		Scan(&e.Key, &e.Kind, &e.MediaType, &e.TMDBID, &e.Language, &e.Payload, &e.FetchedAt)
	if err == sql.ErrNoRows {
		return TMDBEntry{}, false, nil
	}
	if err != nil {
		return TMDBEntry{}, false, fmt.Errorf("store: 读 tmdb 缓存 %s: %w", key, err)
	}
	return e, true, nil
}

// PutTMDBCache 写入或覆盖一条缓存。
//
// 用 UPSERT 而不是 INSERT OR REPLACE：后者是「删除 + 插入」，会连带触发
// 外键的 ON DELETE 行为（现在没有，但将来加引用就会静默清数据），
// 而且 rowid 会变，让「按写入顺序扫」的查询失去意义。
func (db *DB) PutTMDBCache(ctx context.Context, e TMDBEntry) error {
	var id any
	if e.TMDBID > 0 {
		id = e.TMDBID
	}
	var mt any
	if e.MediaType != "" {
		mt = e.MediaType
	}
	if e.FetchedAt == 0 {
		e.FetchedAt = Now()
	}

	_, err := db.rw.ExecContext(ctx, `
		INSERT INTO tmdb_cache (cache_key, kind, media_type, tmdb_id, language, payload, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET
			kind       = excluded.kind,
			media_type = excluded.media_type,
			tmdb_id    = excluded.tmdb_id,
			language   = excluded.language,
			payload    = excluded.payload,
			fetched_at = excluded.fetched_at`,
		e.Key, e.Kind, mt, id, e.Language, e.Payload, e.FetchedAt)
	if err != nil {
		return fmt.Errorf("store: 写 tmdb 缓存 %s: %w", e.Key, err)
	}
	return nil
}

// PurgeTMDBByKey 精确删一条。重刮单条记录时用。
func (db *DB) PurgeTMDBByKey(ctx context.Context, keys ...string) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	tx, err := db.rw.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: 开启事务: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `DELETE FROM tmdb_cache WHERE cache_key = ?`)
	if err != nil {
		return 0, fmt.Errorf("store: 预编译删除: %w", err)
	}
	defer stmt.Close()

	var n int
	for _, k := range keys {
		res, err := stmt.ExecContext(ctx, k)
		if err != nil {
			return n, fmt.Errorf("store: 删除缓存 %s: %w", k, err)
		}
		c, _ := res.RowsAffected()
		n += int(c)
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("store: 提交删除: %w", err)
	}
	return n, nil
}

// PurgeTMDBByID 删掉一部作品的所有语言、所有种类缓存。
func (db *DB) PurgeTMDBByID(ctx context.Context, mediaType string, id int64) (int, error) {
	res, err := db.rw.ExecContext(ctx, `
		DELETE FROM tmdb_cache WHERE tmdb_id = ? AND (media_type = ? OR media_type IS NULL)`,
		id, mediaType)
	if err != nil {
		return 0, fmt.Errorf("store: 按 id 删缓存 %d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PurgeTMDBAll 清空全部缓存，返回删除行数与释放的 payload 字节数。
//
// 契约 §5.3 的 scope=metadata 走这里。**只删 payload，不动 media 表里的
// tmdb_id / season / episode**——否则一次「清空缓存」会把用户手理好的库
// 打回未匹配，那是不可接受的操作语义。
func (db *DB) PurgeTMDBAll(ctx context.Context) (rows int, bytes int64, err error) {
	tx, err := db.rw.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("store: 开启事务: %w", err)
	}
	defer tx.Rollback()

	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(LENGTH(payload)), 0) FROM tmdb_cache`).
		Scan(&rows, &bytes); err != nil {
		return 0, 0, fmt.Errorf("store: 统计缓存: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmdb_cache`); err != nil {
		return 0, 0, fmt.Errorf("store: 清空缓存: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("store: 提交清空: %w", err)
	}
	return rows, bytes, nil
}

// PurgeTMDBOlderThan 删除超过硬上界的行。
//
// 这个函数存在的理由不是省空间，是**条款**：TMDB 要求缓存不得超过 6 个月。
// 调用方传 180 天；传更大的值会被这里拒绝，而不是"信任调用方"——
// 政策一旦分散到多个调用点，迟早有一处写成 365。
func (db *DB) PurgeTMDBOlderThan(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 || maxAge > maxTMDBHardAge {
		return 0, fmt.Errorf("store: 缓存年龄 %v 超出政策上界 %v", maxAge, maxTMDBHardAge)
	}
	cut := time.Now().Add(-maxAge).UnixMilli()
	res, err := db.rw.ExecContext(ctx, `DELETE FROM tmdb_cache WHERE fetched_at < ?`, cut)
	if err != nil {
		return 0, fmt.Errorf("store: 清扫过期缓存: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// TMDBStats 返回缓存条数与 payload 总字节，给 07 屏「缓存占用」用。
func (db *DB) TMDBStats(ctx context.Context) (rows int, bytes int64, err error) {
	err = db.ro.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(LENGTH(payload)), 0) FROM tmdb_cache`).
		Scan(&rows, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("store: 统计 tmdb 缓存: %w", err)
	}
	return rows, bytes, nil
}

// maxTMDBHardAge 是条款允许的缓存上限：6 个月。
//
// 和 tmdb 包里的同名常量重复了一次，故意如此：store 不该 import tmdb
// （那会让存储层依赖一个网络客户端），而这条约束必须在写入侧也能拒绝，
// 否则「有人在 api 层传了 365 天」就没人拦。
const maxTMDBHardAge = 180 * 24 * time.Hour
