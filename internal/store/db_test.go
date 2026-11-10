package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "lumen.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateCreatesAllTables(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	applied, err := db.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != len(mustLoadMigrations(t)) {
		t.Fatalf("首次迁移应用了 %v，期望全部 %d 个脚本", applied, len(mustLoadMigrations(t)))
	}
	if applied[0] != "0001_init" {
		t.Fatalf("首个迁移应为 0001_init，实际 %v", applied)
	}

	want := []string{
		"volumes", "libraries", "unmatched", "media", "tasks",
		"playback_sessions", "playback_progress", "devices",
		"diagnoses", "settings", "secrets", "host_profile",
		"media_fts", "schema_migrations",
	}
	for _, name := range want {
		var n int
		err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n)
		if err != nil {
			t.Fatalf("查询 %s: %v", name, err)
		}
		if n == 0 {
			t.Errorf("表/虚表 %s 未创建", name)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("首次迁移: %v", err)
	}
	applied, err := db.Migrate(ctx)
	if err != nil {
		t.Fatalf("二次迁移: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("二次迁移不应再应用任何脚本，实际 %v", applied)
	}
}

func TestWALEnabled(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var mode string
	if err := db.Read().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("读 journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q，应为 wal", mode)
	}
}

func TestReadHandleRejectsWrites(t *testing.T) {
	// 读句柄必须真的只读：否则"counts 与 items 同事务"这类约束会被人绕过。
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	_, err := db.Read().ExecContext(ctx,
		`INSERT INTO settings(key, value, updated_at) VALUES ('k','v',0)`)
	if err == nil {
		t.Fatal("读句柄居然能写入——query_only 没生效")
	}
}

func TestPathKeyUniquePerVolume(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Write().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	mustExec(`INSERT INTO volumes(id, volume_key, kind, watch_mode, status, created_at, updated_at)
	          VALUES ('vol_1','vk1','local','native','online',0,0)`)
	mustExec(`INSERT INTO libraries(id, volume_id, root_rel, path_key, kind, name, created_at, updated_at)
	          VALUES ('lib_1','vol_1','media/movie','pk_lib','movie','电影',0,0)`)
	mustExec(`INSERT INTO media(id, kind, title, library_id, volume_id, rel_path, path_key, added_at, updated_at)
	          VALUES ('md_1','movie','A','lib_1','vol_1','a.mkv','pk_same',0,0)`)

	// 同一 path_key 再插一次必须被唯一约束挡住——这是"扫两次不出两条记录"的保证。
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO media(id, kind, title, library_id, volume_id, rel_path, path_key, added_at, updated_at)
		 VALUES ('md_2','movie','A','lib_1','vol_1','a.mkv','pk_same',0,0)`)
	if err == nil {
		t.Fatal("media.path_key 唯一约束未生效")
	}
}

func TestFTSIndexFollowsMedia(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := db.Write().ExecContext(ctx,
		`INSERT INTO volumes(id, volume_key, kind, watch_mode, status, created_at, updated_at)
		 VALUES ('vol_1','vk1','local','native','online',0,0)`); err != nil {
		t.Fatalf("insert volume: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx,
		`INSERT INTO libraries(id, volume_id, root_rel, path_key, kind, name, created_at, updated_at)
		 VALUES ('lib_1','vol_1','m','pk','movie','电影',0,0)`); err != nil {
		t.Fatalf("insert library: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx,
		`INSERT INTO media(id, kind, title, original_title, library_id, volume_id, rel_path, path_key, added_at, updated_at)
		 VALUES ('md_1','movie','千与千寻','Spirited Away','lib_1','vol_1','a.mkv','pk1',0,0)`); err != nil {
		t.Fatalf("insert media: %v", err)
	}

	var n int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media_fts WHERE media_fts MATCH '千与千寻'`).Scan(&n); err != nil {
		t.Fatalf("FTS 查询: %v", err)
	}
	if n != 1 {
		t.Fatalf("FTS 未同步插入，命中 %d 条", n)
	}

	// 删除后索引必须跟着消失，否则搜索结果会指向不存在的媒体。
	if _, err := db.Write().ExecContext(ctx, `DELETE FROM media WHERE id='md_1'`); err != nil {
		t.Fatalf("delete media: %v", err)
	}
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media_fts WHERE media_fts MATCH '千与千寻'`).Scan(&n); err != nil {
		t.Fatalf("FTS 查询: %v", err)
	}
	if n != 0 {
		t.Fatalf("FTS 未同步删除，仍有 %d 条", n)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("空路径应报错")
	}
}

func TestSchemaVersion(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	v, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	all := mustLoadMigrations(t)
	want := all[len(all)-1].version
	if v != want {
		t.Fatalf("SchemaVersion = %q, 期望最后一个脚本 %q", v, want)
	}
}

// mustLoadMigrations 让断言跟着 migrate/ 目录走，
// 而不是把版本号写死在测试里——每加一个迁移就要改测试的话，早晚有人偷懒不写测试。
func mustLoadMigrations(t *testing.T) []migration {
	t.Helper()
	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("加载迁移: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("没有任何迁移脚本")
	}
	return all
}
