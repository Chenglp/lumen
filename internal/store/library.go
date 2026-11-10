package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound 表示按 id 查不到记录。
var ErrNotFound = errors.New("store: 记录不存在")

// Volume 是媒体卷。
//
// mount_hint 只用于提示，**不作为寻址依据**（框架 §1.2 ①）——
// 真正的身份是 volume_key（卷序列号 / VolumeUUID / blkid UUID）。
// 一期用 mount_hint 定位挂载点，S3 前补上按 volume_key 校验的解析器。
type Volume struct {
	ID          string
	VolumeKey   string
	Kind        string // local | removable | network
	MountHint   string
	WatchMode   string // native | poll | off
	Status      string // online | offline
	ReadMBps    float64
	FirstByteMS float64
}

// Library 是媒体库根。
type Library struct {
	ID         string
	VolumeID   string
	RootRel    string // 相对卷根，'/' 分隔
	PathKey    string
	Kind       string // movie | tv | photo | music
	Name       string
	Enabled    bool
	LastScanAt int64
}

// Volume 按 id 取卷。
func (db *DB) Volume(ctx context.Context, id string) (Volume, error) {
	var v Volume
	var readMBps, firstByteMS sql.NullFloat64
	var mountHint sql.NullString
	err := db.ro.QueryRowContext(ctx, `
		SELECT id, volume_key, kind, COALESCE(mount_hint,''), watch_mode, status,
		       COALESCE(read_mbps,0), COALESCE(first_byte_ms,0)
		FROM volumes WHERE id = ?`, id).
		Scan(&v.ID, &v.VolumeKey, &v.Kind, &mountHint, &v.WatchMode, &v.Status, &readMBps, &firstByteMS)
	if errors.Is(err, sql.ErrNoRows) {
		return Volume{}, fmt.Errorf("卷 %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Volume{}, fmt.Errorf("store: 查询卷 %s: %w", id, err)
	}
	v.MountHint = mountHint.String
	v.ReadMBps = readMBps.Float64
	v.FirstByteMS = firstByteMS.Float64
	return v, nil
}

// Library 按 id 取库。
func (db *DB) Library(ctx context.Context, id string) (Library, error) {
	var l Library
	var lastScan sql.NullInt64
	err := db.ro.QueryRowContext(ctx, `
		SELECT id, volume_id, root_rel, path_key, kind, name, enabled, COALESCE(last_scan_at,0)
		FROM libraries WHERE id = ?`, id).
		Scan(&l.ID, &l.VolumeID, &l.RootRel, &l.PathKey, &l.Kind, &l.Name, &l.Enabled, &lastScan)
	if errors.Is(err, sql.ErrNoRows) {
		return Library{}, fmt.Errorf("库 %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Library{}, fmt.Errorf("store: 查询库 %s: %w", id, err)
	}
	l.LastScanAt = lastScan.Int64
	return l, nil
}

// Libraries 返回所有库；enabledOnly 为 true 时只返回启用的。
func (db *DB) Libraries(ctx context.Context, enabledOnly bool) ([]Library, error) {
	q := `SELECT id, volume_id, root_rel, path_key, kind, name, enabled, COALESCE(last_scan_at,0)
	      FROM libraries`
	if enabledOnly {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY id`

	rows, err := db.ro.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: 列出库: %w", err)
	}
	defer rows.Close()

	var out []Library
	for rows.Next() {
		var l Library
		var lastScan sql.NullInt64
		if err := rows.Scan(&l.ID, &l.VolumeID, &l.RootRel, &l.PathKey, &l.Kind,
			&l.Name, &l.Enabled, &lastScan); err != nil {
			return nil, err
		}
		l.LastScanAt = lastScan.Int64
		out = append(out, l)
	}
	return out, rows.Err()
}

// TouchLibraryScan 记录一次扫描完成时间，驱动 28 屏「上次扫描 8 分钟前」。
func (db *DB) TouchLibraryScan(ctx context.Context, libraryID string, at int64) error {
	_, err := db.rw.ExecContext(ctx, `
		UPDATE libraries SET last_scan_at = ?, updated_at = ? WHERE id = ?`,
		at, at, libraryID)
	if err != nil {
		return fmt.Errorf("store: 更新库扫描时间: %w", err)
	}
	return nil
}

// SetVolumeStatus 更新卷在线状态。
//
// 卷离线不是错误，是常态（拔盘、网络断）。所以它落在状态列上，
// 而不是让扫描任务失败——其它卷不受影响。
func (db *DB) SetVolumeStatus(ctx context.Context, volumeID, status string, at int64) error {
	_, err := db.rw.ExecContext(ctx, `
		UPDATE volumes SET status = ?, updated_at = ? WHERE id = ?`,
		status, at, volumeID)
	if err != nil {
		return fmt.Errorf("store: 更新卷状态: %w", err)
	}
	return nil
}
