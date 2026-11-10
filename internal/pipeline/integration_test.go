package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Chenglp/lumen/internal/store"
)

// 端到端测试：真的建库、真的落盘、真的遍历。
//
// 上面的用例都用假 store，它们能验证扫描逻辑，但验证不了 SQL——
// 一个写错的 IN 子句、一个漏掉 WHERE library_id 的 UPDATE，
// 在假 store 上都是绿的。这里补上那一半。

func TestScanEndToEnd(t *testing.T) {
	ctx := context.Background()

	dataDir := t.TempDir()
	db, err := store.Open(filepath.Join(dataDir, "lumen.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("迁移: %v", err)
	}

	// 假卷：一个临时目录，根下放 media/movie。
	volRoot := t.TempDir()
	libRoot := filepath.Join(volRoot, "media", "movie")
	if err := os.MkdirAll(filepath.Join(libRoot, "Show"), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO volumes(id, volume_key, kind, mount_hint, watch_mode, status, created_at, updated_at)
		VALUES ('vol_1','vk1','local',?,'native','online',?,?)`, volRoot, now, now); err != nil {
		t.Fatalf("插入卷: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO libraries(id, volume_id, root_rel, path_key, kind, name, enabled, created_at, updated_at)
		VALUES ('lib_1','vol_1','media/movie','pk','movie','电影',1,?,?)`, now, now); err != nil {
		t.Fatalf("插入库: %v", err)
	}

	write := func(rel, body string) string {
		p := filepath.Join(libRoot, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("写 %s: %v", rel, err)
		}
		return p
	}

	write("千与千寻.mkv", "aaaa")
	write("Show/S01E01.mp4", "bb")
	write("poster.jpg", "not-video")

	sc := NewScanner(db)
	res, err := sc.ScanLibrary(ctx, "lib_1", nil)
	if err != nil {
		t.Fatalf("扫描: %v", err)
	}
	if res.Scanned != 2 || res.Added != 2 {
		t.Fatalf("期望 scanned=2 added=2，实际 scanned=%d added=%d", res.Scanned, res.Added)
	}

	// 库里必须有两条，且 rel_path 是库相对路径。
	var n int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM unmatched`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("unmatched 应有 2 条，实际 %d", n)
	}
	var relPath string
	if err := db.Read().QueryRowContext(ctx,
		`SELECT rel_path FROM unmatched WHERE rel_path LIKE '%千与千寻.mkv'`).Scan(&relPath); err != nil {
		t.Fatalf("查 rel_path: %v", err)
	}
	if relPath != "千与千寻.mkv" {
		t.Fatalf("rel_path 应为库相对路径，实际 %q", relPath)
	}

	// 二次扫描：全部跳过，不重复插入。
	res2, err := sc.ScanLibrary(ctx, "lib_1", nil)
	if err != nil {
		t.Fatalf("二次扫描: %v", err)
	}
	if res2.Added != 0 || res2.Unchanged != 2 {
		t.Fatalf("二次扫描应 added=0 unchanged=2，实际 added=%d unchanged=%d", res2.Added, res2.Unchanged)
	}

	// 删掉一个文件：应被标为消失，但记录还在。
	if err := os.Remove(filepath.Join(libRoot, "Show", "S01E01.mp4")); err != nil {
		t.Fatal(err)
	}
	res3, err := sc.ScanLibrary(ctx, "lib_1", nil)
	if err != nil {
		t.Fatalf("三次扫描: %v", err)
	}
	if res3.Removed != 1 {
		t.Fatalf("应标 1 条消失，实际 %d", res3.Removed)
	}

	var total, missing int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(missing_since) FROM unmatched`).Scan(&total, &missing); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("软删不该物理删除记录，应仍为 2 条，实际 %d", total)
	}
	if missing != 1 {
		t.Fatalf("应有 1 条 missing_since 非空，实际 %d", missing)
	}

	// 文件回来了：清标记。
	write("Show/S01E01.mp4", "bb")
	res4, err := sc.ScanLibrary(ctx, "lib_1", nil)
	if err != nil {
		t.Fatalf("四次扫描: %v", err)
	}
	if res4.Restored != 1 {
		t.Fatalf("应恢复 1 条，实际 %d", res4.Restored)
	}
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(missing_since) FROM unmatched`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("标记应被清除，实际仍有 %d 条", missing)
	}

	// last_scan_at 应被写上，否则 28 屏「上次扫描」永远是空的。
	var lastScan int64
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COALESCE(last_scan_at,0) FROM libraries WHERE id='lib_1'`).Scan(&lastScan); err != nil {
		t.Fatal(err)
	}
	if lastScan == 0 {
		t.Fatal("last_scan_at 未写入")
	}
}

// 卷离线时，已有记录不能被标成消失——这条如果破了，
// 用户拔一次移动硬盘，整卷记录全变 missing。
func TestOfflineVolumeDoesNotMarkMissing(t *testing.T) {
	ctx := context.Background()

	db, err := store.Open(filepath.Join(t.TempDir(), "lumen.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	volRoot := t.TempDir()
	libRoot := filepath.Join(volRoot, "m")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libRoot, "a.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	_, _ = db.Write().ExecContext(ctx, `
		INSERT INTO volumes(id, volume_key, kind, mount_hint, watch_mode, status, created_at, updated_at)
		VALUES ('vol_1','vk1','local',?,'native','online',?,?)`, volRoot, now, now)
	_, _ = db.Write().ExecContext(ctx, `
		INSERT INTO libraries(id, volume_id, root_rel, path_key, kind, name, enabled, created_at, updated_at)
		VALUES ('lib_1','vol_1','m','pk','movie','影',1,?,?)`, now, now)

	sc := NewScanner(db)
	if _, err := sc.ScanLibrary(ctx, "lib_1", nil); err != nil {
		t.Fatalf("首扫: %v", err)
	}

	// 卷拔掉：挂载点整个消失。
	if err := os.RemoveAll(volRoot); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.ScanLibrary(ctx, "lib_1", nil); err == nil {
		t.Fatal("卷消失后扫描应返回错误，而不是安静地继续")
	}

	var missing int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(missing_since) FROM unmatched`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("卷离线时不该把任何记录标成消失，实际 %d 条被标", missing)
	}
}

// BenchmarkScanWithDB 用真实 SQLite 测全量首扫吞吐。
//
// 上面那个 BenchmarkScan 用假 store，测出来的只是遍历 + 哈希的成本；
// 真正决定扫描速度的是写入——200 个文件一个事务、每条一次 INSERT。
// 这个数才是「5 万文件要扫多久」能引用得上的那一个。
func BenchmarkScanWithDB(b *testing.B) {
	const files = 2000
	ctx := context.Background()

	db, err := store.Open(filepath.Join(b.TempDir(), "lumen.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Migrate(ctx); err != nil {
		b.Fatal(err)
	}

	volRoot := b.TempDir()
	libRoot := filepath.Join(volRoot, "m")
	for i := 0; i < files; i++ {
		dir := filepath.Join(libRoot, fmt.Sprintf("s%02d", i%26))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("e%04d.mkv", i)), []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	now := time.Now().UnixMilli()
	_, _ = db.Write().ExecContext(ctx, `
		INSERT INTO volumes(id, volume_key, kind, mount_hint, watch_mode, status, created_at, updated_at)
		VALUES ('vol_1','vk1','local',?,'native','online',?,?)`, volRoot, now, now)
	_, _ = db.Write().ExecContext(ctx, `
		INSERT INTO libraries(id, volume_id, root_rel, path_key, kind, name, enabled, created_at, updated_at)
		VALUES ('lib_1','vol_1','m','pk','movie','影',1,?,?)`, now, now)

	sc := NewScanner(db)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 清空记录，测全量首扫而不是二次跳过。
		if _, err := db.Write().ExecContext(ctx, `DELETE FROM unmatched`); err != nil {
			b.Fatal(err)
		}
		if _, err := sc.ScanLibrary(ctx, "lib_1", nil); err != nil {
			b.Fatal(err)
		}
	}
}
