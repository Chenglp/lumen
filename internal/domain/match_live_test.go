package domain

// 真机校验：把 R6 保留下来的 24 条真实剧名重跑一遍，看排序有没有把 TMDB 的排错纠正过来。
//
// 这个测试默认跳过（要外网 + token），它存在的理由不是 CI 覆盖，而是：
// 打分权重是这套系统里唯一"看起来能凭直觉调"的地方。改完权重跑一次这个，
// 比在单测里编一百个例子可信——单测只能证明我实现了我以为的规则，
// 这个能证明那套规则在真实数据上确实更好。
//
//	LUMEN_TMDB_TOKEN=<v4 bearer token> go test ./internal/domain -run TestLiveTMDB -v
//
// 注意它只校验排序，不校验元数据：海报、简介、译文这些是刮削阶段的事。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type liveCase struct {
	query    string
	kind     string
	expected string // 期望命中的原名（R6 结果里的 expected_orig）
}

// 24 条来自 R6 的 probe-zh-CN-20261111.json，expected 是当时人工认定的正确项。
// 其中「信号」当时排在第 2，「昼颜」当时被判定未命中——这两条就是打分的靶子。
var liveCases = []liveCase{
	{"漫长的季节", "tv", "漫长的季节"},
	{"隐秘的角落", "tv", "隐秘的角落"},
	{"狂飙", "tv", "狂飙"},
	{"山海情", "tv", "山海情"},
	{"非自然死亡", "tv", "アンナチュラル"},
	{"半泽直树", "tv", "半沢直樹"},
	{"孤独的美食家", "tv", "孤独のグルメ"},
	{"重启人生", "tv", "ブラッシュアップライフ"},
	{"昼颜", "tv", "昼顔〜平日午後3時の恋人たち〜"},
	{"请回答1988", "tv", "응답하라 1988"},
	{"黑暗荣耀", "tv", "더 글로리"},
	{"信号", "tv", "시그널"},
	{"绝命毒师", "tv", "Breaking Bad"},
	{"权力的游戏", "tv", "Game of Thrones"},
	{"切尔诺贝利", "tv", "Chernobyl"},
	{"怪奇物语", "tv", "Stranger Things"},
	{"神探夏洛克", "tv", "Sherlock"},
	{"进击的巨人", "tv", "進撃の巨人"},
	{"鬼灭之刃", "tv", "鬼滅の刃"},
	{"霸王别姬", "movie", "霸王别姬"},
	{"让子弹飞", "movie", "让子弹飞"},
	{"星际穿越", "movie", "Interstellar"},
	{"千与千寻", "movie", "千と千尋の神隠し"},
	{"寄生虫", "movie", "기생충"},
}

type tmdbResult struct {
	ID               int     `json:"id"`
	Name             string  `json:"name"`
	OriginalName     string  `json:"original_name"`
	Title            string  `json:"title"`
	OriginalTitle    string  `json:"original_title"`
	FirstAirDate     string  `json:"first_air_date"`
	ReleaseDate      string  `json:"release_date"`
	OriginalLanguage string  `json:"original_language"`
	Popularity       float64 `json:"popularity"`
}

type tmdbSearch struct {
	Results []tmdbResult `json:"results"`
}

func TestLiveTMDBSearchRanking(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("LUMEN_TMDB_TOKEN"))
	if token == "" {
		t.Skip("未设置 LUMEN_TMDB_TOKEN，跳过真机校验")
	}
	cli := &http.Client{Timeout: 20 * time.Second}

	var (
		hit1, hit5  int
		rawHit1     int // 未排序前（TMDB 自己的顺序）的 hit@1，作为对照
		improved    int
		regressed   int
		misses      []string
		queryFailed int
	)

	for _, tc := range liveCases {
		raw, err := liveSearch(cli, token, tc.query, tc.kind)
		if err != nil {
			t.Logf("%-10s 请求失败：%v", tc.query, err)
			queryFailed++
			continue
		}
		want := identityKey(tc.expected)

		// 对照：TMDB 原始顺序里正确项排第几。
		rawRank := indexOfExpected(raw, want)

		// 打分后：原始顺序只是输入，候选池不变。
		q := MatchQuery{
			Result:  ParseFileName(tc.query + ".mkv"),
			LibKind: tc.kind,
			Lang:    "zh-CN",
		}
		ranked := Rank(q, raw)

		gotRank := -1
		for i, s := range ranked {
			if identityKey(s.OriginalTitle) == want || identityKey(s.Title) == want {
				gotRank = i + 1
				break
			}
		}

		if rawRank == 1 {
			rawHit1++
		}
		switch {
		case gotRank == 1:
			hit1++
			if rawRank != 1 {
				improved++
			}
		case gotRank > 1 && gotRank <= 5:
			hit5++
			if rawRank == 1 {
				regressed++
			}
		default:
			misses = append(misses, fmt.Sprintf("%s（原始第 %d）", tc.query, rawRank))
			if rawRank == 1 {
				regressed++
			}
		}

		top := ""
		if len(ranked) > 0 {
			top = fmt.Sprintf("%s/%s(%.2f)", ranked[0].Title, ranked[0].OriginalTitle, ranked[0].Breakdown.Score)
		}
		t.Logf("%-10s 原始#%d → 打分#%d  top=%s", tc.query, rawRank, gotRank, top)

		time.Sleep(120 * time.Millisecond) // 别把限流打满，测试不该成为压力源
	}

	n := len(liveCases) - queryFailed
	t.Logf("hit@1 %d/%d（TMDB 原始顺序 %d/%d）· 提升 %d · 回归 %d",
		hit1, n, rawHit1, n, improved, regressed)
	if len(misses) > 0 {
		t.Logf("未命中：%s", strings.Join(misses, "、"))
	}

	// R6 的基线是 hit@1 22/24。排序器的存在意义就是不低于它，
	// 并且不能有任何一条从第 1 掉下去——那说明某个信号在真实数据上是反的。
	if regressed > 0 {
		t.Errorf("%d 条从原始第 1 名掉了下来，权重有问题", regressed)
	}
	if hit1 < rawHit1 {
		t.Errorf("hit@1 %d 低于未排序的 %d", hit1, rawHit1)
	}
}

func liveSearch(cli *http.Client, token, query, kind string) ([]Candidate, error) {
	u := "https://api.themoviedb.org/3/search/" + kind +
		"?language=zh-CN&include_adult=false&page=1&query=" + url.QueryEscape(query)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var s tmdbSearch
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}

	out := make([]Candidate, 0, len(s.Results))
	for _, r := range s.Results {
		title, orig := r.Title, r.OriginalTitle
		if kind == "tv" {
			title, orig = r.Name, r.OriginalName
		}
		c := Candidate{
			TMDBID:           r.ID,
			MediaType:        kind,
			Title:            title,
			OriginalTitle:    orig,
			OriginalLanguage: r.OriginalLanguage,
			Popularity:       r.Popularity,
		}
		date := r.ReleaseDate
		if kind == "tv" {
			date = r.FirstAirDate
		}
		if len(date) >= 4 {
			var y int
			if _, err := fmt.Sscanf(date[:4], "%d", &y); err == nil && y > 1800 {
				c.Year = &y
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// identityKey 把标题压成只含正文字符的串，用来跨语言/跨标点比对。
//
// 不能直接比较原串：「昼顔〜平日午後3時の恋人たち〜」在两边可能一个带波浪线、
// 一个带空格，那属于标点差异，不是不同的作品。
func identityKey(s string) string {
	var sb strings.Builder
	for _, u := range titleUnits(s) {
		sb.WriteString(u.text)
	}
	return sb.String()
}

func indexOfExpected(cands []Candidate, want string) int {
	for i, c := range cands {
		if identityKey(c.OriginalTitle) == want || identityKey(c.Title) == want {
			return i + 1
		}
	}
	return -1
}
