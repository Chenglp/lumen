package tmdb

import (
	"context"
	"net/http"
	"testing"

	"github.com/Chenglp/lumen/internal/domain"
)

func q(t *testing.T, name, libKind string) domain.MatchQuery {
	t.Helper()
	return domain.MatchQuery{Result: domain.ParsePath(name), LibKind: libKind, Lang: "zh-CN"}
}

const tvDetailBody = `{
  "id": 100, "name": "Signal", "original_name": "Signal",
  "first_air_date": "2016-01-22", "original_language": "ko",
  "overview": "一台对讲机连接了过去与现在。",
  "poster_path": "/sig.jpg",
  "number_of_episodes": 16,
  "seasons": [{"season_number":1,"name":"第 1 季","episode_count":16,"air_date":"2016-01-22"}],
  "external_ids": {"imdb_id": "tt5320180"},
  "credits": {"cast":[{"name":"李帝勋","character":"朴海英","order":0}],
              "crew":[{"name":"金元锡","job":"Director"}]}
}`

// nfo/tag 里有 id 时不该再搜：那是 R6 记账里「0 次请求」的那一档。
func TestResolveUsesHintWithoutSearch(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tv/100" {
			_, _ = w.Write([]byte(tvDetailBody))
			return
		}
		t.Errorf("不该打 %s", r.URL.Path)
	})
	c := s.client(t, "k")

	res, err := c.Resolve(context.Background(), Lookup{
		Query: q(t, "Signal.S01E01.1080p.mkv", "tv"),
		Hint:  Hint{TMDBID: 100, Type: TV, Source: "nfo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "hint" {
		t.Errorf("Source = %q，期望 hint", res.Source)
	}
	if s.count() != 1 {
		t.Errorf("请求 %d 次，期望 1（详情）", s.count())
	}
	if res.Detail == nil || res.Detail.TMDBID != 100 {
		t.Fatalf("Detail 没带回来：%+v", res.Detail)
	}
	if len(res.Hits) != 1 || res.Hits[0].TMDBID != 100 {
		t.Fatalf("候选 = %+v", res.Hits)
	}
	if res.Disagreement {
		t.Error("片名吻合却报了不一致")
	}
	if res.Detail.IMDBID != "tt5320180" {
		t.Errorf("external_ids 没解析：%q", res.Detail.IMDBID)
	}
	if len(res.Detail.Seasons) != 1 || res.Detail.TotalEpisodes != 16 {
		t.Errorf("季信息没解析：%+v", res.Detail.Seasons)
	}
}

// id 在 TMDB 没了（合并/下架）时必须降级搜索。凭据或网络问题则不能降级——
// 那会把同一个故障变成两次失败，还把原因盖掉。
func TestResolveHintNotFoundFallsBackToSearch(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/100":
			writeJSON(w, http.StatusNotFound, map[string]any{"status_code": 34})
		case "/search/tv":
			_, _ = w.Write([]byte(tvSearchBody))
		default:
			t.Errorf("不该打 %s", r.URL.Path)
		}
	})
	c := s.client(t, "k")

	res, err := c.Resolve(context.Background(), Lookup{
		Query: q(t, "Signal.S01E01.1080p.mkv", "tv"),
		Hint:  Hint{TMDBID: 100, Type: TV, Source: "nfo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "search" {
		t.Errorf("Source = %q，期望 search", res.Source)
	}
	if len(res.Hits) == 0 {
		t.Fatal("降级后没有候选")
	}
}

func TestResolveHintUnauthorizedDoesNotFallBack(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tv/100" {
			t.Errorf("凭据失效时不该继续打 %s", r.URL.Path)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"status_code": 7})
	})
	c := s.client(t, "k")

	_, err := c.Resolve(context.Background(), Lookup{
		Query: q(t, "Signal.S01E01.1080p.mkv", "tv"),
		Hint:  Hint{TMDBID: 100, Type: TV, Source: "nfo"},
	})
	if err == nil {
		t.Fatal("应把凭据错误报出来")
	}
	if s.count() != 1 {
		t.Errorf("请求 %d 次，期望 1", s.count())
	}
}

// 换了片子却沿用旧目录与旧 nfo 是真实的操作事故。静默采用错的 id，
// 用户点开播放才发现海报是另一部片。
func TestResolveHintDisagreementTriggersSearch(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/100":
			_, _ = w.Write([]byte(`{"id":100,"name":"教父","original_name":"The Godfather","first_air_date":"1972-03-14"}`))
		case "/search/tv":
			_, _ = w.Write([]byte(tvSearchBody))
		default:
			t.Errorf("不该打 %s", r.URL.Path)
		}
	})
	c := s.client(t, "k")

	res, err := c.Resolve(context.Background(), Lookup{
		Query: q(t, "Signal.S01E01.1080p.mkv", "tv"),
		Hint:  Hint{TMDBID: 100, Type: TV, Source: "nfo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Disagreement {
		t.Error("片名完全不符却没有标记不一致")
	}
	if res.Source != "hint" {
		t.Errorf("Source = %q：自带的那条应仍然可点", res.Source)
	}
	// 自带的那条不应被丢掉，它仍然是一个选项。
	var found bool
	for _, h := range res.Hits {
		if h.TMDBID == 100 {
			found = true
		}
	}
	if !found {
		t.Error("合并候选时丢了 nfo 自带的那条")
	}
}

// 年份过滤会误杀：发布名里的年份是压制/发行年，与首播年差一岁是常态。
// 带年份搜到 0 条时必须去掉年份重搜，否则整条记录变成「无候选」。
func TestResolveDropsYearWhenSearchEmpty(t *testing.T) {
	var calls []string
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/tv" {
			t.Errorf("不该打 %s", r.URL.Path)
			return
		}
		y := r.URL.Query().Get("first_air_date_year")
		calls = append(calls, y)
		if y != "" {
			_, _ = w.Write([]byte(`{"total_results":0,"results":[]}`))
			return
		}
		_, _ = w.Write([]byte(tvSearchBody))
	})
	c := s.client(t, "k")

	res, err := c.Resolve(context.Background(), Lookup{
		Query: q(t, "Signal.2015.S01E01.1080p.mkv", "tv"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.YearDropped {
		t.Error("应标记 YearDropped")
	}
	if len(calls) != 2 || calls[0] != "2015" || calls[1] != "" {
		t.Errorf("请求序列 = %v，期望 [2015 \"\"]", calls)
	}
	if len(res.Hits) == 0 {
		t.Fatal("去年份后应有候选")
	}
	if res.Requests != 2 {
		t.Errorf("Requests = %d，期望 2", res.Requests)
	}
}

// 没有片名就没有可搜的东西。发一次必然为空的请求，然后把「未识别」显示给用户，
// 比不搜更糟——那时该显示的是「文件名里没有剧名」。
func TestResolveWithoutTitleSkipsNetwork(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("没有片名不该发请求，实际打了 %s", r.URL.Path)
	})
	c := s.client(t, "k")

	res, err := c.Resolve(context.Background(), Lookup{
		Query: q(t, "S01E01.mkv", "tv"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("不该有候选：%+v", res.Hits)
	}
	if res.Code != domain.ParseEpisodeWithoutSeries {
		t.Errorf("Code = %q，期望 %q", res.Code, domain.ParseEpisodeWithoutSeries)
	}
	if s.count() != 0 {
		t.Errorf("请求 %d 次，期望 0", s.count())
	}
}

// Enrich 只为前 N 个补详情：28 屏右栏只放 3 个。
func TestResolveEnrichOnlyTouchesTop(t *testing.T) {
	var detailCalls int
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/tv" {
			_, _ = w.Write([]byte(tvSearchBody))
			return
		}
		detailCalls++
		_, _ = w.Write([]byte(tvDetailBody))
	})
	c := s.client(t, "k")

	if _, err := c.Resolve(context.Background(), Lookup{
		Query:  q(t, "Signal.S01E01.1080p.mkv", "tv"),
		Enrich: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 {
		t.Errorf("补了 %d 条详情，期望 1", detailCalls)
	}
}

// nfo/tag 里有 id 时，把握度不是"猜得准不准"，而是"答案本来就写在文件里"。
//
// 真实的例子：`All.Wishes.Come.True.2026.2160p…PandaQT.mkv` 的容器 tag 写着
// TMDB=movie/1633056，而 TMDB 的中文名是《八仙！》——片名一个词都对不上，
// titleSim 是 0，分数只有 0.445。若照分数报"把握 45%"，28 屏会显示一个
// 明明已经知道答案却显得没底的数字。
func TestResolveHintIsConfirmedRegardlessOfTitleMatch(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		// 注意 original_title 也是中文：这部片的原名就是《八仙！》，
		// 英文名是发行方另起的。所以文件名里的 All Wishes Come True
		// 与两个标题都零重合——这才是真机上看到的情形。
		_, _ = w.Write([]byte(`{"id":1633056,"title":"八仙！","original_title":"八仙！",
		  "release_date":"2026-02-10","original_language":"zh"}`))
	})
	c := s.client(t, "k")

	res, err := c.Resolve(context.Background(), Lookup{
		Query: q(t, "All.Wishes.Come.True.2026.2160p.WEB-DL.H.265.mkv", "movie"),
		Hint:  Hint{TMDBID: 1633056, Type: Movie, Source: "tag"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Confirmed {
		t.Error("自带的 id 应标记 Confirmed")
	}
	if res.Confidence != 1 {
		t.Errorf("Confirmed 时把握应为 1，实际 %v", res.Confidence)
	}
	// 但每条的 score 仍是真实吻合度，不能一起改成 1——
	// 它要能回答"NFO 说 A、文件名也像 A 吗"这个问题。
	if len(res.Hits) != 1 || res.Hits[0].Score >= 0.9 {
		t.Errorf("score 不该被抬高：%+v", res.Hits)
	}
	t.Logf("片名零重合时 score=%.3f，而 Confirmed=true → 把握 1.000", res.Hits[0].Score)
	if res.Disagreement {
		t.Error("0.445 > 0.35，不该触发重搜")
	}
}
