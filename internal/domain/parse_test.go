package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// 用例来源：F:\movies 的真实文件名（第 1 条）、R6 §7 的三条脏名字、
// 以及各发布组的标准写法。真实文件名优先——自己编的例子只能证明代码自洽。

func yearOf(p ParsedName) int {
	if p.Year == nil {
		return 0
	}
	return *p.Year
}

func seasonOf(p ParsedName) int {
	if p.Season == nil {
		return 0
	}
	return *p.Season
}

func episodeOf(p ParsedName) int {
	if p.Episode == nil {
		return 0
	}
	return *p.Episode
}

func TestParseFileName(t *testing.T) {
	cases := []struct {
		file   string
		title  string
		year   int
		season int
		ep     int
		res    string
		source string
		group  string
		code   ParseCode
	}{
		{
			// 真实库里的那一条（DV P5，框架 §4.2 举的例子）
			file:  "All.Wishes.Come.True.2026.2160p.YK.WEB-DL.H.265.DV.HFR.HQ.DTS5.1-PandaQT.mkv",
			title: "All Wishes Come True", year: 2026,
			res: "2160p", source: "WEB-DL", group: "PandaQT", code: ParseOK,
		},
		{
			// R6 §7 第 3 条：连标准英文发布名都返回 0
			file:  "Interstellar.2014.2160p.UHD.BluRay.x265-TERMiNAL.mkv",
			title: "Interstellar", year: 2014,
			res: "2160p", source: "BluRay", group: "TERMiNAL", code: ParseOK,
		},
		{
			// R6 §7 第 1 条
			file:  "漫长的季节.S01E01.2160p.WEB-DL.H265.DDP5.1.mkv",
			title: "漫长的季节", season: 1, ep: 1,
			res: "2160p", source: "WEB-DL", code: ParseOK,
		},
		{
			// R6 §7 第 2 条：方括号 + 中文 + 「全39集」
			file:  "[压制组] 狂飙 2023 1080p 全39集.mkv",
			title: "狂飙", year: 2023,
			res: "1080p", group: "压制组", code: ParseOK,
		},
		{
			// 片名本身是年份：1917 是片名，2019 是上映年
			file:  "1917.2019.1080p.BluRay.x264-SPARKS.mkv",
			title: "1917", year: 2019,
			res: "1080p", source: "BluRay", group: "SPARKS", code: ParseOK,
		},
		{
			// 片名是《2012》，上映年 2009。只给一个年份会直接把片子筛没
			file:  "2012.2009.1080p.BluRay.x264-SPARKS.mkv",
			title: "2012", year: 2009,
			res: "1080p", source: "BluRay", group: "SPARKS", code: ParseOK,
		},
		{
			// 整个名字只有一个四位数字 → 年份留空
			file: "2012.mkv", title: "2012", code: ParseYearOrTitleAmbiguous,
		},
		{file: "1917.mkv", title: "1917", code: ParseYearOrTitleAmbiguous},
		{
			// 片名里的连字符不能被当成压制组分隔符
			file:  "Spider-Man.2002.1080p.BluRay.x264-GROUP.mkv",
			title: "Spider-Man", year: 2002,
			res: "1080p", source: "BluRay", group: "GROUP", code: ParseOK,
		},
		{file: "Spider-Man.mkv", title: "Spider-Man", code: ParseTitleOnly},
		{
			// 结尾方括号里是组名，不是技术标记
			file:  "Movie (2019) [1080p] [BluRay] [YTS.MX].mp4",
			title: "Movie", year: 2019,
			res: "1080p", source: "BluRay", group: "YTS.MX", code: ParseOK,
		},
		{
			file:  "Breaking.Bad.S02E04.1080p.BluRay.x264-GROUP.mkv",
			title: "Breaking Bad", season: 2, ep: 4,
			res: "1080p", source: "BluRay", group: "GROUP", code: ParseOK,
		},
		{
			// 季集号之后是**单集标题**，不能吃进剧名
			file:  "Show - S01E01 - The Pilot.mkv",
			title: "Show", season: 1, ep: 1, code: ParseOK,
		},
		{
			file:  "The.Matrix.1999.REMASTERED.1080p.BluRay.x264-AMIABLE.mkv",
			title: "The Matrix", year: 1999,
			res: "1080p", source: "BluRay", group: "AMIABLE", code: ParseOK,
		},
		{
			// 中文字幕标记在片名后面，不能污染标题
			file:  "让子弹飞.2010.1080p.BluRay.国语中字.mkv",
			title: "让子弹飞", year: 2010,
			res: "1080p", source: "BluRay", code: ParseOK,
		},
		{
			file:  "狂飙.第02集.2023.2160p.WEB-DL.mkv",
			title: "狂飙", year: 2023, ep: 2,
			res: "2160p", source: "WEB-DL", code: ParseOK,
		},
		{
			// 多集连放
			file:  "Frieren.S01E01E02.1080p.mkv",
			title: "Frieren", season: 1, ep: 1, res: "1080p", code: ParseOK,
		},
		{
			file:  "The.100.S01E01.1080p.WEB-DL.mkv",
			title: "The 100", season: 1, ep: 1,
			res: "1080p", source: "WEB-DL", code: ParseOK,
		},
		{
			// 1x02 写法
			file:  "Show.1x02.HDTV.mkv",
			title: "Show", season: 1, ep: 2, source: "HDTV", code: ParseOK,
		},
		{
			// 只有季 → 整季包；没有集号但季集信息足够定位，仍算 OK
			file:  "Show.S01.2160p.WEB-DL.mkv",
			title: "Show", season: 1, res: "2160p", source: "WEB-DL", code: ParseOK,
		},
		{
			// 有季集号但没剧名
			file: "S02E04.mkv", season: 2, ep: 4, code: ParseEpisodeWithoutSeries,
		},
		{
			// 括号里的年份与上映年不一致 → 提醒"这是压制年"
			file:  "Movie.2023.1080p.BluRay.[2019].mkv",
			title: "Movie", year: 2023,
			res: "1080p", source: "BluRay", code: ParseOK,
		},
		{
			// 动漫绝对集号：`- 12` 后面紧跟分辨率括号
			file:  "[SubsPlease] Sousou no Frieren - 12 (1080p) [ABCD1234].mkv",
			title: "Sousou no Frieren", ep: 12,
			res: "1080p", group: "SubsPlease", code: ParseOK,
		},
		{
			// 分卷盘：DDP5.1 不能被切成 DDP5 + 1
			file:  "Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.Atmos.H.265-FLUX.mkv",
			title: "Dune Part Two", year: 2024,
			res: "2160p", source: "WEB-DL", group: "FLUX", code: ParseOK,
		},
		{file: "1080p.mkv", code: ParseEmpty},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			got := ParseFileName(c.file)
			p := got.Parsed

			if p.Title != c.title {
				t.Errorf("标题 = %q，期望 %q", p.Title, c.title)
			}
			if yearOf(p) != c.year {
				t.Errorf("年份 = %d，期望 %d", yearOf(p), c.year)
			}
			if seasonOf(p) != c.season {
				t.Errorf("季 = %d，期望 %d", seasonOf(p), c.season)
			}
			if episodeOf(p) != c.ep {
				t.Errorf("集 = %d，期望 %d", episodeOf(p), c.ep)
			}
			if c.res != "" && p.Resolution != c.res {
				t.Errorf("分辨率 = %q，期望 %q", p.Resolution, c.res)
			}
			if c.source != "" && p.Source != c.source {
				t.Errorf("片源 = %q，期望 %q", p.Source, c.source)
			}
			if p.ReleaseGroup != c.group {
				t.Errorf("压制组 = %q，期望 %q", p.ReleaseGroup, c.group)
			}
			if got.Code != c.code {
				t.Errorf("code = %s，期望 %s（params=%v）", got.Code, c.code, got.Params)
			}
		})
	}
}

// 标题必须是一条能直接丢给 /search 的查询串。
//
// 这条测试是 R6 的直接复现：那三条 0 命中的共同点是"名字里还带着点号和方括号"。
func TestParseTitleIsSearchable(t *testing.T) {
	dirty := []string{
		"漫长的季节.S01E01.2160p.WEB-DL.H265.DDP5.1.mkv",
		"[压制组] 狂飙 2023 1080p 全39集.mkv",
		"Interstellar.2014.2160p.UHD.BluRay.x265-TERMiNAL.mkv",
	}
	for _, f := range dirty {
		title := ParseFileName(f).Parsed.Title
		if title == "" {
			t.Fatalf("%s: 标题为空", f)
		}
		if strings.ContainsAny(title, ".[]()_") {
			t.Errorf("%s: 标题 %q 仍含发布名符号", f, title)
		}
		if strings.Contains(title, "  ") {
			t.Errorf("%s: 标题 %q 有连续空格", f, title)
		}
	}
}

// 年份不能出现在标题里：`Movie 2019` 会把年份当成片名的一部分去搜。
func TestParseYearNotInTitle(t *testing.T) {
	cases := map[string]string{
		"Interstellar.2014.1080p.mkv":          "Interstellar",
		"让子弹飞.2010.1080p.mkv":                  "让子弹飞",
		"The.Matrix.1999.REMASTERED.1080p.mkv": "The Matrix",
		"1917.2019.1080p.mkv":                  "1917",
		"2012.2009.1080p.mkv":                  "2012",
	}
	for f, want := range cases {
		if got := ParseFileName(f).Parsed.Title; got != want {
			t.Errorf("%s: 标题 = %q，期望 %q", f, got, want)
		}
	}
}

func TestParseWarnings(t *testing.T) {
	// 括号年份 ≠ 上映年
	r := ParseFileName("Movie.2023.1080p.BluRay.[2019].mkv")
	if len(r.Warnings) == 0 || r.Warnings[0].Code != ParseYearIsReleaseTag {
		t.Fatalf("期望 YEAR_IS_RELEASE_TAG，得到 %+v", r.Warnings)
	}
	if r.Warnings[0].Params["bracketYear"] != 2019 || r.Warnings[0].Params["year"] != 2023 {
		t.Errorf("params 不对：%v", r.Warnings[0].Params)
	}

	// 整季包
	r = ParseFileName("[压制组] 狂飙 2023 1080p 全39集.mkv")
	found := false
	for _, w := range r.Warnings {
		if w.Code == ParseMultiSeasonPack {
			found = true
			if w.Params["episodeCount"] != 39 {
				t.Errorf("episodeCount = %v，期望 39", w.Params["episodeCount"])
			}
		}
	}
	if !found {
		t.Errorf("期望 MULTI_SEASON_PACK，得到 %+v", r.Warnings)
	}

	// S01-S03 多季包
	r = ParseFileName("Show.S01-S03.1080p.BluRay.mkv")
	if seasonOf(r.Parsed) != 1 || r.Parsed.SeasonCount != 3 {
		t.Errorf("多季包解析错：season=%d count=%d", seasonOf(r.Parsed), r.Parsed.SeasonCount)
	}
}

// EPISODE_WITHOUT_SERIES 的 params 必须带齐 season/episode/candidates：
// 缺键会让客户端的文案模板渲染出空字符串。
func TestParseEpisodeWithoutSeriesParams(t *testing.T) {
	r := ParseFileName("S02E04.mkv")
	if r.Code != ParseEpisodeWithoutSeries {
		t.Fatalf("code = %s", r.Code)
	}
	for _, k := range []string{"season", "episode", "candidates"} {
		if _, ok := r.Params[k]; !ok {
			t.Errorf("params 缺 %q", k)
		}
	}
}

func TestParsePathUsesDirForSeries(t *testing.T) {
	cases := []struct {
		rel    string
		title  string
		season int
		ep     int
		from   string
	}{
		// 剧集库的常见结构：文件名里根本没有剧名
		{"Breaking Bad/Season 2/S02E04.mkv", "Breaking Bad", 2, 4, "dir"},
		{"Breaking Bad (2008)/Season 2/S02E04.mkv", "Breaking Bad", 2, 4, "dir"},
		{"狂飙/第2季/第02集.mkv", "狂飙", 2, 2, "dir"},
		// 文件名自己有剧名时，目录不覆盖
		{"Movies/Interstellar.2014.2160p.mkv", "Interstellar", 0, 0, "file"},
		// 季度目录要跳过，不能被当成剧名
		{"The Wire/Specials/S01E01.mkv", "The Wire", 1, 1, "dir"},
	}
	for _, c := range cases {
		r := ParsePath(c.rel)
		if r.Parsed.Title != c.title {
			t.Errorf("%s: 标题 = %q，期望 %q", c.rel, r.Parsed.Title, c.title)
		}
		if seasonOf(r.Parsed) != c.season || episodeOf(r.Parsed) != c.ep {
			t.Errorf("%s: S%dE%d，期望 S%dE%d", c.rel,
				seasonOf(r.Parsed), episodeOf(r.Parsed), c.season, c.ep)
		}
		if r.TitleFrom != c.from {
			t.Errorf("%s: 标题来自 %q，期望 %q", c.rel, r.TitleFrom, c.from)
		}
	}
}

// parsed 会原样进 unmatched.parsed 与 A1 响应，JSON 形状必须是契约的样子：
// 没有年份时是 null，不是 0。
func TestParsedJSONShape(t *testing.T) {
	b, err := json.Marshal(ParseFileName("Interstellar.2014.2160p.mkv").Parsed)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"year":2014`) {
		t.Errorf("年份应序列化为数字：%s", s)
	}
	for _, k := range []string{"title", "year", "season", "episode", "resolution", "source", "releaseGroup"} {
		if !strings.Contains(s, `"`+k+`"`) {
			t.Errorf("缺契约字段 %q：%s", k, s)
		}
	}
	// 内部字段不能外泄
	for _, k := range []string{"LooksSerial", "SeasonPack", "VideoCodec", "SeasonCount", "Tags"} {
		if strings.Contains(s, k) {
			t.Errorf("内部字段 %q 出现在 JSON 里：%s", k, s)
		}
	}

	b, _ = json.Marshal(ParseFileName("2012.mkv").Parsed)
	if !strings.Contains(string(b), `"year":null`) {
		t.Errorf("歧义年份应为 null：%s", b)
	}
}
