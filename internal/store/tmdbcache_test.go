package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "lumen.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestTMDBCacheRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	e := TMDBEntry{
		Key: "detail:tv:zh-CN:128420", Kind: TMDBKindDetail,
		MediaType: "tv", TMDBID: 128420, Language: "zh-CN",
		Payload: `{"v":1,"data":{"title":"静默轨道"}}`,
	}
	if err := db.PutTMDBCache(ctx, e); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.GetTMDBCache(ctx, e.Key, 0)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Payload != e.Payload || got.TMDBID != 128420 || got.Kind != TMDBKindDetail {
		t.Errorf("读回来不一致：%+v", got)
	}
	if got.FetchedAt == 0 {
		t.Error("FetchedAt 没被填上")
	}

	// 覆盖写要真的覆盖（新 payload 生效），而不是留下旧行。
	e.Payload = `{"v":1,"data":{"title":"新的"}}`
	if err := db.PutTMDBCache(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, _, _ = db.GetTMDBCache(ctx, e.Key, 0)
	if got.Payload != e.Payload {
		t.Errorf("覆盖写没生效：%q", got.Payload)
	}
	if n, _, _ := db.TMDBStats(ctx); n != 1 {
		t.Errorf("行数 = %d，期望 1（覆盖不该新增）", n)
	}
}

// 过期判断在 SQL 里做：取回来再比会让 idx_tmdb_cache_fetched 用不上，
// 而这张表在 5 万文件量级下会有几十万行。
func TestTMDBCacheTTL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	old := TMDBEntry{
		Key: "detail:movie:zh-CN:1", Kind: TMDBKindDetail, MediaType: "movie",
		TMDBID: 1, Language: "zh-CN", Payload: "{}",
		FetchedAt: time.Now().Add(-40 * 24 * time.Hour).UnixMilli(), // 40 天前
	}
	if err := db.PutTMDBCache(ctx, old); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := db.GetTMDBCache(ctx, old.Key, 30*24*time.Hour); err != nil || ok {
		t.Errorf("40 天前的行在 30 天 TTL 下应 miss：ok=%v err=%v", ok, err)
	}
	// TTL 是读取时现算的，所以把 TTL 放宽就能重新命中——这正是
	// 契约 §5.2 改 ttlDays 时不需要重写任何行的原因。
	if _, ok, _ := db.GetTMDBCache(ctx, old.Key, 60*24*time.Hour); !ok {
		t.Error("TTL 放宽后应命中")
	}
	// maxAge=0 表示不限。
	if _, ok, _ := db.GetTMDBCache(ctx, old.Key, 0); !ok {
		t.Error("maxAge=0 应命中")
	}
}

func TestPurgeTMDBOlderThanRespectsPolicy(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 传 365 天必须被拒绝，不能「信任调用方」——政策一旦分散就会有一处写成 365。
	if _, err := db.PurgeTMDBOlderThan(ctx, 365*24*time.Hour); err == nil {
		t.Fatal("超政策上界应报错")
	}
	if _, err := db.PurgeTMDBOlderThan(ctx, 0); err == nil {
		t.Fatal("maxAge<=0 应报错（防止误删全表）")
	}

	if err := db.PutTMDBCache(ctx, TMDBEntry{
		Key: "detail:tv:zh-CN:9", Kind: TMDBKindDetail, MediaType: "tv", TMDBID: 9,
		Language: "zh-CN", Payload: "{}",
		FetchedAt: time.Now().Add(-200 * 24 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTMDBCache(ctx, TMDBEntry{
		Key: "detail:tv:zh-CN:10", Kind: TMDBKindDetail, MediaType: "tv", TMDBID: 10,
		Language: "zh-CN", Payload: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.PurgeTMDBOlderThan(ctx, 180*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("删了 %d 行，期望 1", n)
	}
	if rows, _, _ := db.TMDBStats(ctx); rows != 1 {
		t.Errorf("剩余 %d 行，期望 1", rows)
	}
}

func TestPurgeTMDBAllReportsBytes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c"} {
		if err := db.PutTMDBCache(ctx, TMDBEntry{
			Key: k, Kind: TMDBKindSearch, MediaType: "tv", Language: "zh-CN",
			Payload: "0123456789",
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, bytes, err := db.PurgeTMDBAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 3 || bytes != 30 {
		t.Errorf("rows=%d bytes=%d，期望 3 / 30", rows, bytes)
	}
	if n, _, _ := db.TMDBStats(ctx); n != 0 {
		t.Errorf("清空后仍有 %d 行", n)
	}
}

// 按 id 删只该删那一部，且不该碰到别的语言里的同 id（那是别的语言，
// 换语言重抓时它们还是有效回退）。
func TestPurgeTMDBByID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	put := func(key, lang string, id int64) {
		t.Helper()
		if err := db.PutTMDBCache(ctx, TMDBEntry{
			Key: key, Kind: TMDBKindDetail, MediaType: "tv", TMDBID: id,
			Language: lang, Payload: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("detail:tv:zh-CN:5", "zh-CN", 5)
	put("detail:tv:zh-CN:6", "zh-CN", 6)

	// 注意：这条在真实流程里是按语言重抓，不该被一起删掉——
	// 所以 PurgeTMDBByID 有意删除该 id 的所有语言，由调用方决定粒度为「整部作品」。
	put("detail:tv:en-US:5", "en-US", 5)

	n, err := db.PurgeTMDBByID(ctx, "tv", 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("删了 %d 行，期望 2（同一部作品的两个语言）", n)
	}
	if rows, _, _ := db.TMDBStats(ctx); rows != 1 {
		t.Errorf("剩余 %d 行，期望 1", rows)
	}
}
