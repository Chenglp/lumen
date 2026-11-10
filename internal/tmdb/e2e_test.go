package tmdb

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Chenglp/lumen/internal/store"
)

// 用真 SQLite 跑一遍缓存路径。
//
// 理由和 TestScanPersistsParseResult 一样：假缓存只能证明「我调了缓存」，
// 证明不了「列名对得上、payload 真的落了、TTL 真的按毫秒算」。
// 上一轮 media 表缺 mtime 列就是假 store 全绿、真库直接报 no such column。
func TestCacheAgainstRealSQLite(t *testing.T) {
	ctx := context.Background()

	db, err := store.Open(filepath.Join(t.TempDir(), "lumen.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/tv" {
			_, _ = w.Write([]byte(tvSearchBody))
			return
		}
		_, _ = w.Write([]byte(tvDetailBody))
	})
	c := s.client(t, "k", WithCache(db))

	first, err := c.Resolve(ctx, Lookup{Query: q(t, "Signal.S01E01.1080p.mkv", "tv")})
	if err != nil {
		t.Fatal(err)
	}
	if first.Requests != 1 {
		t.Errorf("Requests = %d，期望 1", first.Requests)
	}

	// 缓存必须真的写进去了。
	rows, bytes, err := db.TMDBStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 || bytes == 0 {
		t.Fatalf("缓存没落库：rows=%d bytes=%d", rows, bytes)
	}

	second, err := c.Resolve(ctx, Lookup{Query: q(t, "Signal.S01E02.1080p.mkv", "tv")})
	if err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Errorf("第二集又打了请求（共 %d 次）——同一部剧的 12 集本该只付一次", s.count())
	}
	if len(second.Hits) != len(first.Hits) {
		t.Errorf("缓存命中后候选数不同：%d → %d", len(first.Hits), len(second.Hits))
	}
	// 命中缓存时 Requests 应为 0：这不是省事，是「27 小时」那笔账里
	// 唯一能被用户看见的进度分子。
	if second.Requests != 0 {
		t.Errorf("缓存命中时 Requests = %d，期望 0", second.Requests)
	}

	// 换语言必须当 miss（契约 §5.2 的渐进重抓），且旧行不能被删——
	// 重抓失败时要能回退到旧 payload。
	en := New("k", WithBaseURL(s.URL), WithCache(db), WithLanguage("en-US"))
	if _, err := en.Search(ctx, TV, "signal", nil, 10); err != nil {
		t.Fatal(err)
	}
	if s.count() != 2 {
		t.Errorf("换语言后应重抓，请求共 %d 次", s.count())
	}
	if rows, _, _ := db.TMDBStats(ctx); rows != 2 {
		t.Errorf("换语言后应有 2 行（旧语言保留），实际 %d", rows)
	}
}

// payload 存的是投影结果，不是 TMDB 原始响应。这条不是洁癖：
// 原始响应一条搜索约 2KB，投影后约 200 字节，5 万文件量级下是 GB 与百 MB 的差别。
func TestCacheStoresProjectionNotRawResponse(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "lumen.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// 一份带满无用字段的原始响应。
	raw := `{"page":1,"total_results":1,"results":[{"id":7,"name":"信号",
	  "adult":false,"backdrop_path":"/b.jpg","genre_ids":[18,9648],
	  "origin_country":["KR"],"popularity":30.1,"vote_average":8.5,"vote_count":900,
	  "first_air_date":"2016-01-22","original_language":"ko","original_name":"Signal",
	  "overview":"一台对讲机。","poster_path":"/p.jpg"}]}`
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(raw)) })
	c := s.client(t, "k", WithCache(db))

	if _, err := c.Search(ctx, TV, "信号", nil, 10); err != nil {
		t.Fatal(err)
	}

	e, ok, err := db.GetTMDBCache(ctx, CacheKey(store.TMDBKindSearch, TV, "zh-CN", 0, "-", hashQuery("信号")), 0)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	for _, unwanted := range []string{"genre_ids", "vote_average", "origin_country", "backdrop_path"} {
		if strings.Contains(e.Payload, unwanted) {
			t.Errorf("缓存里带了没用到的字段 %q", unwanted)
		}
	}
	if len(e.Payload) >= len(raw) {
		t.Errorf("投影后没有变小：%d vs 原始 %d", len(e.Payload), len(raw))
	}
}
