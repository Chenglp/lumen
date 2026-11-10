package tmdb

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/Chenglp/lumen/internal/domain"
)

// 真机校验。默认跳过：需要外网与真 token，CI 里跑它就是给流水线加一个
// 会被墙 / 会限流的依赖。
//
//	LUMEN_TMDB_TOKEN=<v4 token> go test ./internal/tmdb -run TestLive -v
//
// 它的用途不是覆盖率，是证明**字段映射与认证方式**真的对得上——httptest 的
// 响应是我照文档手写的，手写错了测试就一起错（这正是上一轮 live 测试
// 发现「TMDB 原始顺序只有 23/24」的原因：假数据不会反驳我）。
func TestLiveTMDB(t *testing.T) {
	token := os.Getenv("LUMEN_TMDB_TOKEN")
	if token == "" {
		t.Skip("未设置 LUMEN_TMDB_TOKEN，跳过真机校验")
	}
	ctx := context.Background()
	c := New(token, WithLanguage("zh-CN"))

	if err := c.Validate(ctx); err != nil {
		t.Fatalf("凭据校验失败：%v（reason=%s）", err, Reason(err))
	}

	t.Run("详情字段映射", func(t *testing.T) {
		// 1396 = Breaking Bad
		d, _, err := c.Detail(ctx, TV, 1396)
		if err != nil {
			t.Fatal(err)
		}
		if d.Title == "" {
			t.Error("title 为空")
		}
		if d.Year == nil || *d.Year != 2008 {
			t.Errorf("first_air_date 没解析出年份：%v", d.Year)
		}
		if len(d.Seasons) < 5 {
			t.Errorf("seasons 只有 %d 条，季列表没解析", len(d.Seasons))
		}
		if d.IMDBID == "" {
			t.Error("external_ids.imdb_id 没拿到")
		}
		if len(d.Cast) == 0 || len(d.Crew) == 0 {
			t.Errorf("credits 没解析：cast=%d crew=%d", len(d.Cast), len(d.Crew))
		}
		t.Logf("%s (%s) · %d 季 · %d 集 · imdb=%s · 主演 %s",
			d.Title, yr(d.Year), len(d.Seasons), d.TotalEpisodes, d.IMDBID, d.Cast[0].Name)

		// 第二季的集列表要能拿到集标题。
		sd, _, err := c.Season(ctx, 1396, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(sd.Episodes) == 0 || sd.Episodes[0].Name == "" {
			t.Errorf("季集没解析：%+v", sd.Episodes[:min(1, len(sd.Episodes))])
		}
		t.Logf("S02 共 %d 集，E01 = %s", len(sd.Episodes), sd.Episodes[0].Name)
	})

	t.Run("文件名到候选", func(t *testing.T) {
		res, err := c.Resolve(ctx, Lookup{
			Query: domain.MatchQuery{
				Result:  domain.ParsePath("Breaking.Bad.S02E04.1080p.BluRay.x264.mkv"),
				LibKind: "tv",
				Lang:    "zh-CN",
			},
			Enrich: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Hits) == 0 {
			t.Fatalf("没有候选（code=%s）", res.Code)
		}
		top := res.Hits[0]
		if top.TMDBID != 1396 {
			t.Errorf("top1 = %d %q，期望 1396（Breaking Bad）", top.TMDBID, top.Title)
		}
		if top.Year == nil {
			t.Error("候选年份为 nil——TV 的 first_air_date 没读到")
		}
		if top.PosterPath == "" {
			t.Error("海报路径为空")
		}
		t.Logf("top1 = %d %q (%s) score=%.3f recommended=%v；共 %d 个候选、%d 次请求",
			top.TMDBID, top.Title, yr(top.Year), top.Score, top.Recommended, len(res.Hits), res.Requests)
	})

	t.Run("中文短标题不被综艺抢", func(t *testing.T) {
		// R6 坑二：查「信号」原 top1 是综艺《心动的信号》。
		// 这里带年份，验证年份信号在真机上确实能把剧集顶上来。
		year := 2016
		res, err := c.Resolve(ctx, Lookup{
			Query: domain.MatchQuery{
				Result:  domain.ParseResult{Parsed: domain.ParsedName{Title: "信号", Year: &year}},
				LibKind: "tv",
				Lang:    "zh-CN",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Hits) == 0 {
			t.Fatal("没有候选")
		}
		for i, h := range res.Hits[:min(3, len(res.Hits))] {
			t.Logf("#%d %d %q (%s) score=%.3f", i+1, h.TMDBID, h.Title, yr(h.Year), h.Score)
		}
		if res.Hits[0].MediaType != "tv" {
			t.Errorf("top1 类型 = %q，期望 tv", res.Hits[0].MediaType)
		}
	})
}

func yr(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}
