package domain

import (
	"math"
	"testing"
)

// 打分的测试全部对着 R6 的真实失败案例写，不对着「我觉得合理的例子」写。
// 24 条真实剧名里只有 2 条排错，这两条的成因决定了整套权重；
// 其余 22 条只要不发生回归就够了。

// tvQ / movieQ 构造查询。年份为 nil 表示文件名里没有可信年份。
func tvQ(title string, year *int) MatchQuery {
	return MatchQuery{
		Result:  ParseResult{Parsed: ParsedName{Title: title, Year: year}, Code: ParseTitleOnly},
		LibKind: "tv",
		Lang:    "zh-CN",
	}
}

func movieQ(title string, year *int) MatchQuery {
	return MatchQuery{
		Result:  ParseResult{Parsed: ParsedName{Title: title, Year: year}, Code: ParseOK},
		LibKind: "movie",
		Lang:    "zh-CN",
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// R6 坑二：查询「信号」时 TMDB 的 top1 是综艺《心动的信号》。
//
// 这不是「搜不到」——正确项在 rank 2。两字的短标题被长标题的子串命中，
// 而 TMDB 按 popularity 把综艺顶到了前面（88.5 vs 30.1）。
// 唯一能翻盘的地方是精确命中与子串命中的差别。
func TestRankBeatsPopularityOnShortTitle(t *testing.T) {
	q := tvQ("信号", nil)
	got := Rank(q, []Candidate{
		{TMDBID: 100, MediaType: "tv", Title: "心动的信号", OriginalTitle: "心动的信号",
			Year: intp(2018), OriginalLanguage: "zh", Popularity: 88.5},
		{TMDBID: 200, MediaType: "tv", Title: "信号", OriginalTitle: "시그널",
			Year: intp(2016), OriginalLanguage: "ko", Popularity: 30.1},
	})

	if len(got) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(got))
	}
	if got[0].TMDBID != 200 {
		t.Fatalf("第一名 = %d《%s》，期望 200《信号》——精确命中必须压过子串命中", got[0].TMDBID, got[0].Title)
	}
	if got[0].Breakdown.TitleSim != 1 {
		t.Errorf("《信号》的片名相似度 = %v，期望 1", got[0].Breakdown.TitleSim)
	}
	if got[1].Breakdown.TitleSim != 0.7 {
		t.Errorf("《心动的信号》的片名相似度 = %v，期望 0.7（子串命中）", got[1].Breakdown.TitleSim)
	}
	if !got[0].Recommended {
		t.Errorf("正确项应当被标推荐：score=%v 第二名=%v", got[0].Breakdown.Score, got[1].Breakdown.Score)
	}
}

// R6 坑一：查询「昼颜」，top1 的 zh-CN 名是《昼颜：工作日下午3点的恋人们》。
//
// 契约里「切片长度差异不得作为降权理由」说的就是这条：
// 译名比俗称长是常态。如果拿字符串相等或长度比去打分，这个正确项会被判成未匹配。
func TestTitleSimDoesNotPenalizeLongerTranslation(t *testing.T) {
	q := tvQ("昼颜", nil)
	c := Candidate{
		TMDBID: 1, MediaType: "tv",
		Title:         "昼颜：工作日下午3点的恋人们",
		OriginalTitle: "昼顔〜平日午後3時の恋人たち〜",
		Year:          intp(2014), OriginalLanguage: "ja", Popularity: 20,
	}
	b := Score(q, c)
	if b.TitleSim != 1 {
		t.Fatalf("片名相似度 = %v，期望 1——候选比查询长得多，但查询被完整覆盖了", b.TitleSim)
	}
	// 反过来：候选比查询短，就是没覆盖，该扣分。
	short := Candidate{TMDBID: 2, MediaType: "tv", Title: "昼", Year: intp(2014)}
	if b2 := Score(q, short); b2.TitleSim >= b.TitleSim {
		t.Errorf("候选只覆盖一半时相似度 = %v，不该不低于完整覆盖的 %v", b2.TitleSim, b.TitleSim)
	}
}

// 年份是短标题之间唯一的分界线，所以它的档位必须陡：
// 差 1 年还有救（跨年首映），差 2 年基本可以排除，差 3 年以上是另一部片。
func TestYearSignalSteps(t *testing.T) {
	q := movieQ("1917", intp(2019))
	base := Score(q, Candidate{TMDBID: 1, MediaType: "movie", Title: "1917", Year: intp(2019)})
	if base.YearSim != 1 {
		t.Fatalf("同年 YearSim = %v，期望 1", base.YearSim)
	}

	want := map[int]float64{2020: 0.55, 2017: 0.15, 2016: 0}
	for y, w := range want {
		got := Score(q, Candidate{TMDBID: 2, MediaType: "movie", Title: "1917", Year: intp(y)}).YearSim
		if !near(got, w) {
			t.Errorf("年份差 %d 时 YearSim = %v，期望 %v", absInt(2019-y), got, w)
		}
	}

	// 文件名没年份时不给分，也不许别的信号替它补上——
	// 缺证据就是少一分证据，重新归一等于把「不确定」伪装成「确定」。
	noYear := Score(movieQ("1917", nil), Candidate{TMDBID: 1, MediaType: "movie", Title: "1917", Year: intp(2019)})
	if noYear.YearSim != 0 {
		t.Errorf("查询无年份时 YearSim = %v，期望 0", noYear.YearSim)
	}
	if noYear.Score >= StrongMatchScore {
		t.Errorf("无年份的分数 = %v，不该够到强匹配门槛 %v", noYear.Score, StrongMatchScore)
	}
}

// 类型信号：走错端点等于必然搜不到，所以同名电影与剧同时出现时必须分开。
func TestTypeSignalSeparatesMovieAndTV(t *testing.T) {
	q := movieQ("Interstellar", intp(2014))
	got := Rank(q, []Candidate{
		{TMDBID: 10, MediaType: "tv", Title: "Interstellar", Year: intp(2014), Popularity: 99},
		{TMDBID: 11, MediaType: "movie", Title: "Interstellar", Year: intp(2014), Popularity: 1},
	})
	if got[0].TMDBID != 11 {
		t.Fatalf("第一名 = %d(%s)，期望电影 —— 类型不符不该靠 popularity 顶上来", got[0].TMDBID, got[0].MediaType)
	}
	if got[0].TypeSim != 1 || got[1].TypeSim != 0 {
		t.Errorf("TypeSim = %v/%v，期望 1/0", got[0].TypeSim, got[1].TypeSim)
	}
	if got[1].Breakdown.Score >= StrongMatchScore {
		t.Errorf("类型不符的分数 = %v，不该够到强匹配门槛 %v", got[1].Breakdown.Score, StrongMatchScore)
	}
}

// 库类型优先于文件名痕迹：用户把目录加成「电影库」，里面的 S01E01 是命名事故。
func TestExpectedTypePrefersLibraryKind(t *testing.T) {
	q := movieQ("Show", nil)
	q.Result.Parsed.Season = intp(1)
	q.Result.Parsed.Episode = intp(1)
	if got := expectedType(q); got != "movie" {
		t.Errorf("expectedType = %q，期望 movie（跟随库类型）", got)
	}
	q.LibKind = ""
	if got := expectedType(q); got != "tv" {
		t.Errorf("库类型未知时 expectedType = %q，期望 tv（跟随季集痕迹）", got)
	}
}

// popularity 只能决定同分次序，不能决定其它任何事。
func TestPopularityOnlyBreaksExactTies(t *testing.T) {
	q := movieQ("寄生虫", intp(2019))
	got := Rank(q, []Candidate{
		{TMDBID: 20, MediaType: "movie", Title: "寄生虫", Year: intp(2019), Popularity: 3},
		{TMDBID: 21, MediaType: "movie", Title: "寄生虫", Year: intp(2019), Popularity: 80},
	})
	if got[0].Breakdown.Score != got[1].Breakdown.Score {
		t.Fatalf("同名片同年应当同分，实际 %v / %v", got[0].Breakdown.Score, got[1].Breakdown.Score)
	}
	if got[0].TMDBID != 21 {
		t.Errorf("同分时第一名 = %d，期望 popularity 高的 21", got[0].TMDBID)
	}
}

// 打平时不标推荐：角标只能有一个，标错等于把用户往错的方向推一次。
func TestNoRecommendationWhenClose(t *testing.T) {
	q := tvQ("海边电影院", nil)
	got := Rank(q, []Candidate{
		{TMDBID: 30, MediaType: "tv", Title: "海边电影院的故事", Year: intp(2020), Popularity: 9},
		{TMDBID: 31, MediaType: "tv", Title: "海边电影院2", Year: intp(2020), Popularity: 8},
	})
	for _, s := range got {
		if s.Recommended {
			t.Fatalf("分不开的两条里标了推荐：%d score=%v", s.TMDBID, s.Breakdown.Score)
		}
	}
}

// 低分不标推荐，但也不能没有候选——0.65 的下限正是为了挡住
// 「名字像、年份不对」那种最容易被误点的组合（子串 0.7 + 年份错 = 0.63）。
func TestNoRecommendationBelowThreshold(t *testing.T) {
	q := movieQ("海边电影院", intp(2025))
	got := Rank(q, []Candidate{
		{TMDBID: 40, MediaType: "movie", Title: "海边电影院的故事", Year: intp(2019), Popularity: 5},
	})
	if len(got) != 1 {
		t.Fatalf("候选数 = %d", len(got))
	}
	if got[0].Recommended {
		t.Errorf("score=%v 不该标推荐（门槛 %v）", got[0].Breakdown.Score, RecommendMinScore)
	}
}

func TestReasonStrongMatch(t *testing.T) {
	q := movieQ("星际穿越", intp(2014))
	got := Rank(q, []Candidate{
		{TMDBID: 50, MediaType: "movie", Title: "星际穿越", OriginalTitle: "Interstellar",
			Year: intp(2014), OriginalLanguage: "en", Popularity: 70},
	})
	code, params := Reason(q, got)
	if code != CodeStrongMatch {
		t.Fatalf("code = %q，期望 %q", code, CodeStrongMatch)
	}
	if params["title"] != "星际穿越" || params["year"] != 2014 {
		t.Errorf("params = %v", params)
	}
	if _, ok := params["score"].(float64); !ok {
		t.Errorf("params 缺 score：%v", params)
	}
}

// 没有年份就没有强匹配——契约里 STRONG_MATCH 的文案是「片名与年份都吻合」。
func TestReasonStrongMatchNeedsYear(t *testing.T) {
	q := movieQ("星际穿越", nil)
	got := Rank(q, []Candidate{
		{TMDBID: 50, MediaType: "movie", Title: "星际穿越", Year: intp(2014), Popularity: 70},
	})
	code, _ := Reason(q, got)
	if code == CodeStrongMatch {
		t.Fatalf("无年份时不该给 STRONG_MATCH，实际 %q", code)
	}
}

func TestReasonTitleYearOverlap(t *testing.T) {
	q := movieQ("海边电影院", intp(2025))
	got := Rank(q, []Candidate{
		{TMDBID: 60, MediaType: "movie", Title: "海边电影院的故事", Year: intp(2025), Popularity: 9},
		{TMDBID: 61, MediaType: "movie", Title: "海边电影院2", Year: intp(2025), Popularity: 8},
	})
	code, params := Reason(q, got)
	if code != CodeTitleYearOverlap {
		t.Fatalf("code = %q，期望 %q（片名部分重合且前两名分不开）", code, CodeTitleYearOverlap)
	}
	for _, k := range []string{"guessedTitle", "guessedYear", "matchedTitle", "matchedYear", "overlap"} {
		if _, ok := params[k]; !ok {
			t.Errorf("params 缺 %s：%v", k, params)
		}
	}
}

// 没有候选时把解析阶段的 code 原样还回去：那时该显示「文件名里没有剧名」，
// 而不是「未识别」——后者对用户没有任何行动指引。
func TestReasonFallsBackToParseCode(t *testing.T) {
	q := tvQ("", nil)
	q.Result.Code = ParseEpisodeWithoutSeries
	q.Result.Params = map[string]any{"season": 2, "episode": 4, "candidates": 0}

	code, params := Reason(q, nil)
	if code != ParseEpisodeWithoutSeries {
		t.Fatalf("code = %q，期望 %q", code, ParseEpisodeWithoutSeries)
	}
	if params["episode"] != 4 {
		t.Errorf("params 应原样带回：%v", params)
	}
	if c := Confidence(nil); c != 0 {
		t.Errorf("无候选时 confidence = %v，期望 0（客户端据此显示「未识别」）", c)
	}
}

// 缺剧名的行不该出现推荐：所有候选的片名相似度都是 0，
// 分数只来自类型与文种，那种分数没有意义。
func TestEpisodeWithoutSeriesNeverRecommends(t *testing.T) {
	q := tvQ("", nil)
	q.Result.Code = ParseEpisodeWithoutSeries
	got := Rank(q, []Candidate{
		{TMDBID: 70, MediaType: "tv", Title: "狂飙", Year: intp(2023), Popularity: 90},
	})
	if got[0].Recommended {
		t.Errorf("缺剧名时不该标推荐：score=%v", got[0].Breakdown.Score)
	}
}

// 排序必须完全确定：同一个文件重复匹配给出同一个顺序。
// 不这样做的代价是用户点「换一批」看到顺序乱变，然后不再相信任何一个候选。
func TestRankIsDeterministic(t *testing.T) {
	q := tvQ("请回答1988", nil)
	cands := []Candidate{
		{TMDBID: 81, MediaType: "tv", Title: "请回答1988", Year: intp(2015), Popularity: 40},
		{TMDBID: 82, MediaType: "tv", Title: "请回答1994", Year: intp(2013), Popularity: 40},
		{TMDBID: 83, MediaType: "tv", Title: "请回答1988", Year: intp(2015), Popularity: 40},
		{TMDBID: 84, MediaType: "tv", Title: "回答吧1988", Year: intp(2015), Popularity: 40},
	}
	first := Rank(q, cands)
	// 打乱输入顺序，结果必须一致（同分同热度时按 tmdbId 定序）。
	shuffled := []Candidate{cands[3], cands[1], cands[2], cands[0]}
	second := Rank(q, shuffled)
	for i := range first {
		if first[i].TMDBID != second[i].TMDBID {
			t.Fatalf("第 %d 位不同：%d vs %d", i, first[i].TMDBID, second[i].TMDBID)
		}
	}
	if first[0].TMDBID != 81 {
		t.Errorf("第一名 = %d，期望 81（精确命中且 tmdbId 最小）", first[0].TMDBID)
	}
}

// 去重与截断：同一个 tmdb_id 只留分高的那条；单页硬上限 20，不翻页。
func TestRankDedupAndCap(t *testing.T) {
	q := movieQ("寄生虫", intp(2019))
	got := Rank(q, []Candidate{
		{TMDBID: 90, MediaType: "movie", Title: "寄生虫", Year: intp(2000), Popularity: 1},
		{TMDBID: 90, MediaType: "movie", Title: "寄生虫", Year: intp(2019), Popularity: 1},
	})
	if len(got) != 1 {
		t.Fatalf("去重后候选数 = %d，期望 1", len(got))
	}
	if got[0].Year == nil || *got[0].Year != 2019 {
		t.Errorf("去重应保留分高的那条（year=2019），实际 %v", got[0].Year)
	}

	var many []Candidate
	for i := 0; i < 40; i++ {
		many = append(many, Candidate{
			TMDBID: 1000 + i, MediaType: "movie", Title: "寄生虫", Year: intp(2019),
			Popularity: float64(i),
		})
	}
	if n := len(Rank(q, many)); n != MaxCandidates {
		t.Errorf("截断后 %d 条，期望 %d（契约单页硬上限）", n, MaxCandidates)
	}
}

// 四个信号全中才是 1.0。这条同时锁住权重之和为 1——
// 将来有人调权重忘了归一，这里会直接红。
func TestPerfectScoreIsOne(t *testing.T) {
	q := movieQ("星际穿越", intp(2014))
	b := Score(q, Candidate{
		TMDBID: 1, MediaType: "movie", Title: "星际穿越", OriginalTitle: "Interstellar",
		Year: intp(2014), OriginalLanguage: "en",
	})
	if !near(b.Score, 1) {
		t.Fatalf("满分 = %v，期望 1（title=%v year=%v type=%v lang=%v）",
			b.Score, b.TitleSim, b.YearSim, b.TypeSim, b.LangSim)
	}
}

// 繁简/异体只算字面部分重合，给一点底分但远低于命中。
func TestTraditionalSimplifiedGetsPartialCredit(t *testing.T) {
	q := tvQ("进击的巨人", nil)
	got := Score(q, Candidate{TMDBID: 1, MediaType: "tv", Title: "進撃の巨人"})
	if got.TitleSim <= 0 || got.TitleSim >= 0.7 {
		t.Errorf("繁简差异的片名相似度 = %v，期望在 (0, 0.7) 之间", got.TitleSim)
	}
}

// 重音被 ASCII 化是发布名的常态，不做归一就永远比不上。
func TestAccentInsensitive(t *testing.T) {
	q := movieQ("Amelie", intp(2001))
	b := Score(q, Candidate{TMDBID: 1, MediaType: "movie", Title: "Amélie", Year: intp(2001)})
	if b.TitleSim != 1 {
		t.Errorf("Amelie 对 Amélie 的相似度 = %v，期望 1", b.TitleSim)
	}
}
