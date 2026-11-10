package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/Chenglp/lumen/internal/lib"
)

// 一次 IN 查询最多带多少个占位符。
//
// SQLite 的 SQLITE_MAX_VARIABLE_NUMBER 老版本是 999，新版本才 32766。
// 取 400 是为了两边都能跑，且远低于上限——这个数不需要抠，扫描的瓶颈在磁盘 IO。
const inChunk = 400

// KnownFile 是扫描比对时需要的已有记录，只取四列。
//
// 刻意不取整行：5 万文件的比对如果连 title/probe 一起读出来，
// 内存和 IO 都会翻几倍，而这里唯一要判断的只是「在不在、变没变」。
type KnownFile struct {
	Table     string // unmatched | media
	ID        string
	SizeBytes int64
	MTime     int64
	Missing   bool // missing_since 非空
}

// KnownFiles 批量查 path_key 是否已存在。
//
// 返回 map 而不是 slice：调用方是按 key 逐个判定的，用 map 可以 O(1) 命中，
// 也顺手解决了「同一个 key 在 unmatched 和 media 里都有」的歧义——
// 正常不会发生（匹配成功会从 unmatched 移走），真发生了以 media 为准。
func (db *DB) KnownFiles(ctx context.Context, keys []string) (map[string]KnownFile, error) {
	out := make(map[string]KnownFile, len(keys))
	if len(keys) == 0 {
		return out, nil
	}

	// 顺序很重要：先写 unmatched，再写 media，后者覆盖前者。
	for _, tbl := range []string{"unmatched", "media"} {
		for start := 0; start < len(keys); start += inChunk {
			end := start + inChunk
			if end > len(keys) {
				end = len(keys)
			}
			chunk := keys[start:end]

			q := fmt.Sprintf(`
				SELECT id, path_key, size_bytes, mtime, missing_since IS NOT NULL
				FROM %s WHERE path_key IN (%s)`,
				tbl, placeholders(len(chunk)))

			args := make([]any, len(chunk))
			for i, k := range chunk {
				args[i] = k
			}

			rows, err := db.ro.QueryContext(ctx, q, args...)
			if err != nil {
				return nil, fmt.Errorf("store: 比对 %s: %w", tbl, err)
			}
			for rows.Next() {
				var kf KnownFile
				var key string
				if err := rows.Scan(&kf.ID, &key, &kf.SizeBytes, &kf.MTime, &kf.Missing); err != nil {
					rows.Close()
					return nil, err
				}
				kf.Table = tbl
				out[key] = kf
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// UnmatchedRow 是待插入的一条待整理记录。
type UnmatchedRow struct {
	VolumeID  string
	LibraryID string
	RelPath   string // 相对库根，给 UI 显示
	PathKey   string // 由卷相对路径算，见 domain.VolumePathKey
	SizeBytes int64
	MTime     int64
	Derived
}

// Derived 是扫描阶段就能算出、但需要落库的派生信息。
//
// 全部以 JSON 或枚举的形式原样存：这一层不认识它们的语义，也不该认识——
// store 只负责存取，改文案/改判定都不该动到这里。
type Derived struct {
	Parsed       string  // JSON，domain.ParsedName
	MediaType    string  // movie | tv | unknown
	ReasonCode   string  // 契约 UnmatchedReasonCode；空表示等匹配阶段填
	ReasonParams string  // JSON
	Confidence   float64 // 只在匹配成功后才有意义，扫描阶段恒 0
}

func (d Derived) mediaTypeOrUnknown() string {
	if d.MediaType == "" {
		return "unknown"
	}
	return d.MediaType
}

// InsertUnmatchedBatch 在一个事务里批量插入待整理记录。
//
// ON CONFLICT DO NOTHING 而不是 REPLACE：并发扫描（定时任务 + 用户手动触发）
// 撞上同一批文件时，第二个写入者应当静默让路，而不是把第一个刚写进去的
// parsed/probe 结果冲掉。
func (db *DB) InsertUnmatchedBatch(ctx context.Context, rows []UnmatchedRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	// 单事务 ≤ 500 行的落地：扫描器按 200 行一批调用，
	// 这里再兜一道，防止未来有人一次塞几万条把写锁占住（R4 实测结论）。
	if len(rows) > 500 {
		return 0, fmt.Errorf("store: 单事务插入 %d 行，超过 500 行上限", len(rows))
	}

	tx, err := db.rw.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: 批量插入待整理: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := Now()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO unmatched (id, volume_id, library_id, rel_path, path_key,
		                       size_bytes, mtime, media_type, reason_code, reason_params,
		                       parsed, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path_key) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("store: 预编译插入: %w", err)
	}
	defer stmt.Close()

	var inserted int
	for _, r := range rows {
		res, err := stmt.ExecContext(ctx, lib.NewID("um"), r.VolumeID, r.LibraryID,
			r.RelPath, r.PathKey, r.SizeBytes, r.MTime,
			r.mediaTypeOrUnknown(), nullStr(r.ReasonCode), nullStr(r.ReasonParams),
			nullStr(r.Parsed), now, now)
		if err != nil {
			return 0, fmt.Errorf("store: 插入待整理 %s: %w", r.RelPath, err)
		}
		n, _ := res.RowsAffected()
		inserted += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: 提交待整理批次: %w", err)
	}
	return inserted, nil
}

// TouchUnmatched 更新一条已存在记录的 stat；d 非 nil 时同时重写派生字段。
//
// 传 nil 表示文件只是回来了（stat 没变），解析结果仍然有效，不该重算；
// 传 d 表示内容变了，之前解析出的片名/季集号都失效，必须重跑——
// 留着旧结果会让 28 屏显示一部已经不存在的片子。
//
// probe 不在这里清：ffprobe 必须在写锁外跑（R4 结论），由扫描器把路径
// 收集起来交给探测阶段。
func (db *DB) TouchUnmatched(ctx context.Context, id string, size, mtime int64, d *Derived) error {
	var err error
	if d != nil {
		_, err = db.rw.ExecContext(ctx, `
			UPDATE unmatched
			SET size_bytes = ?, mtime = ?, parsed = ?, media_type = ?,
			    reason_code = ?, reason_params = ?, probe = NULL,
			    confidence = 0, missing_since = NULL, updated_at = ?
			WHERE id = ?`,
			size, mtime, nullStr(d.Parsed), d.mediaTypeOrUnknown(),
			nullStr(d.ReasonCode), nullStr(d.ReasonParams), Now(), id)
	} else {
		_, err = db.rw.ExecContext(ctx, `
			UPDATE unmatched
			SET size_bytes = ?, mtime = ?, missing_since = NULL, updated_at = ?
			WHERE id = ?`, size, mtime, Now(), id)
	}
	if err != nil {
		return fmt.Errorf("store: 更新待整理 %s: %w", id, err)
	}
	return nil
}

// nullStr 把空串写成 SQL NULL。
//
// 不用 sql.NullString：这几列全部是「有值就是有，没值就空」，空串与 NULL
// 在这里没有语义差别，但 NULL 能让 `IS NULL` 的查询和后续迁移简单得多。
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TouchMedia 更新一条已入库记录的 stat。
//
// 不在这里清 probe：media 的编解码信息要重跑 ffprobe 才能填，
// 那一步在锁外做（R4 结论），由扫描器把路径收集起来交给后续阶段。
func (db *DB) TouchMedia(ctx context.Context, id string, size, mtime int64) error {
	_, err := db.rw.ExecContext(ctx, `
		UPDATE media
		SET size_bytes = ?, mtime = ?, missing_since = NULL, updated_at = ?
		WHERE id = ?`, size, mtime, Now(), id)
	if err != nil {
		return fmt.Errorf("store: 更新媒体 %s: %w", id, err)
	}
	return nil
}

// ClearMissing 把指定记录标为「重新可见」。
//
// 单独一个方法而不是塞进 Touch*：文件没变但之前被标成消失（拔盘后插回）
// 这种情况，stat 完全没变化，走不到 Touch 分支，只能显式清标记。
func (db *DB) ClearMissing(ctx context.Context, refs []PathRef) (int, error) {
	return db.setMissing(ctx, refs, 0)
}

// MarkMissing 把指定记录标为消失（missing_since = at）。
func (db *DB) MarkMissing(ctx context.Context, refs []PathRef, at int64) (int, error) {
	return db.setMissing(ctx, refs, at)
}

func (db *DB) setMissing(ctx context.Context, refs []PathRef, at int64) (int, error) {
	if len(refs) == 0 {
		return 0, nil
	}

	byTable := map[string][]string{}
	for _, r := range refs {
		byTable[r.Table] = append(byTable[r.Table], r.ID)
	}

	total := 0
	for tbl, ids := range byTable {
		if tbl != "unmatched" && tbl != "media" {
			return total, fmt.Errorf("store: 未知表 %q", tbl)
		}
		for start := 0; start < len(ids); start += inChunk {
			end := start + inChunk
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]

			var arg any
			if at == 0 {
				arg = nil
			} else {
				arg = at
			}

			q := fmt.Sprintf(`UPDATE %s SET missing_since = ? WHERE id IN (%s)`,
				tbl, placeholders(len(chunk)))
			args := make([]any, 0, len(chunk)+1)
			args = append(args, arg)
			for _, id := range chunk {
				args = append(args, id)
			}

			res, err := db.rw.ExecContext(ctx, q, args...)
			if err != nil {
				return total, fmt.Errorf("store: 标记 %s 消失状态: %w", tbl, err)
			}
			n, _ := res.RowsAffected()
			total += int(n)
		}
	}
	return total, nil
}

// PathRef 指向一条记录，用于标记消失/恢复。
type PathRef struct {
	Table   string
	ID      string
	PathKey string
	RelPath string
}

// ListLibraryPaths 返回某个库下所有记录的 path_key。
//
// 这是「哪些文件消失了」这个问题的另一半：扫描器手里有见过的 key 集合，
// 库里有全量的 key 集合，差集就是消失的。反过来（把见过的 key 塞进 SQL）
// 在 5 万文件时要传几万个占位符，不可行。
func (db *DB) ListLibraryPaths(ctx context.Context, libraryID string) ([]PathRef, error) {
	var out []PathRef
	for _, tbl := range []string{"unmatched", "media"} {
		rows, err := db.ro.QueryContext(ctx, fmt.Sprintf(`
			SELECT id, path_key, rel_path FROM %s WHERE library_id = ?`, tbl), libraryID)
		if err != nil {
			return nil, fmt.Errorf("store: 列出 %s 路径: %w", tbl, err)
		}
		for rows.Next() {
			var r PathRef
			if err := rows.Scan(&r.ID, &r.PathKey, &r.RelPath); err != nil {
				rows.Close()
				return nil, err
			}
			r.Table = tbl
			out = append(out, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CountLibraryFiles 返回某库下的记录数，用于「总数未知时估算进度」与自检。
func (db *DB) CountLibraryFiles(ctx context.Context, libraryID string) (int, error) {
	var n int
	err := db.ro.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM unmatched WHERE library_id = ?)
		     + (SELECT COUNT(*) FROM media     WHERE library_id = ?)`,
		libraryID, libraryID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: 统计库文件数: %w", err)
	}
	return n, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
