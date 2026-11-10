package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Chenglp/lumen/internal/domain"
	"github.com/Chenglp/lumen/internal/lib"
)

// EnsureVolume 按 volume_key 幂等注册卷，返回卷与「本次是否新建」。
//
// 幂等是按 volume_key 而不是按 id 做的：同一块盘被拔下来又插回去、
// 或者用户把已经加过的目录再加一次，都不该产生第二条卷记录。
// 盘符变了则更新 mount_hint——它只是提示，身份仍然是 volume_key。
func (db *DB) EnsureVolume(ctx context.Context, volumeKey, kind, mountHint string) (Volume, bool, error) {
	if volumeKey == "" {
		return Volume{}, false, errors.New("store: volume_key 不能为空")
	}
	if kind == "" {
		kind = "local"
	}
	now := Now()

	var id string
	err := db.ro.QueryRowContext(ctx,
		`SELECT id FROM volumes WHERE volume_key = ?`, volumeKey).Scan(&id)

	switch {
	case err == nil:
		// 已存在：刷新挂载提示与在线状态，不动其它列。
		if _, err := db.rw.ExecContext(ctx,
			`UPDATE volumes SET mount_hint = ?, status = 'online', updated_at = ?
			 WHERE id = ?`, mountHint, now, id); err != nil {
			return Volume{}, false, fmt.Errorf("store: 刷新卷 %s: %w", id, err)
		}
		v, err := db.Volume(ctx, id)
		return v, false, err

	case errors.Is(err, sql.ErrNoRows):
		id = lib.NewID("vol")
		watch := "native"
		if kind == "network" {
			// 网络盘收不到内核事件，只能轮询（框架 §1.3）。
			watch = "poll"
		}
		if _, err := db.rw.ExecContext(ctx, `
			INSERT INTO volumes (id, volume_key, kind, mount_hint, watch_mode, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'online', ?, ?)`,
			id, volumeKey, kind, mountHint, watch, now, now); err != nil {
			return Volume{}, false, fmt.Errorf("store: 注册卷 %s: %w", volumeKey, err)
		}
		v, err := db.Volume(ctx, id)
		return v, true, err

	default:
		return Volume{}, false, fmt.Errorf("store: 查询卷 %s: %w", volumeKey, err)
	}
}

// EnsureLibrary 按 (volume_id, root_rel) 幂等注册库。
//
// 唯一键不用 root_rel 本身：两个卷上都可以有 movies 目录。
func (db *DB) EnsureLibrary(ctx context.Context, volumeID, rootRel, kind, name string) (Library, bool, error) {
	if volumeID == "" {
		return Library{}, false, errors.New("store: volume_id 不能为空")
	}
	rel, ok := domain.CleanRel(rootRel)
	if !ok {
		return Library{}, false, fmt.Errorf("store: 库根 %q 不是合法的相对路径", rootRel)
	}
	if kind == "" {
		kind = "movie"
	}
	if name == "" {
		name = rel
	}
	now := Now()

	var id string
	err := db.ro.QueryRowContext(ctx,
		`SELECT id FROM libraries WHERE volume_id = ? AND root_rel = ?`,
		volumeID, rel).Scan(&id)

	switch {
	case err == nil:
		l, err := db.Library(ctx, id)
		return l, false, err

	case errors.Is(err, sql.ErrNoRows):
		id = lib.NewID("lib")
		if _, err := db.rw.ExecContext(ctx, `
			INSERT INTO libraries (id, volume_id, root_rel, path_key, kind, name, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			id, volumeID, rel, domain.PathKey(volumeID, rel), kind, name, now, now); err != nil {
			return Library{}, false, fmt.Errorf("store: 注册库 %s: %w", rel, err)
		}
		l, err := db.Library(ctx, id)
		return l, true, err

	default:
		return Library{}, false, fmt.Errorf("store: 查询库 %s: %w", rel, err)
	}
}
