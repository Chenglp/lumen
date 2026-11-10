package tmdb

import (
	"strconv"
	"strings"
)

// HintFromTags 从容器 tag 里取 TMDB id。
//
// R6 实测的真实写法是 `TMDB=movie/1633056`（也被见过 `TMDB=1633056`、
// `tmdb=tv/1396`）。这里刻意宽松：只要求键名大小写不敏感等于 tmdb，
// 值里认 `movie/123`、`tv/123`、`123` 三种。
//
// 宽松的理由是这一层的错误代价不对称：多认出来一条 → 省一次搜索；
// 认错一条 → id 与文件名对不上，Resolve 的打分会把它标成 Disagreement
// 并另外搜一轮，用户仍能选对。反过来，写得过严会让「文件明明知道答案」
// 的情况退化成搜索，那是每次都要付的钱。
func HintFromTags(tags map[string]string) Hint {
	for k, v := range tags {
		if !strings.EqualFold(strings.TrimSpace(k), "tmdb") {
			continue
		}
		if h, ok := parseTagValue(v); ok {
			h.Source = "tag"
			return h
		}
	}
	return Hint{}
}

func parseTagValue(v string) (Hint, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Hint{}, false
	}
	typ := ""
	if i := strings.IndexByte(v, '/'); i >= 0 {
		typ = strings.ToLower(strings.TrimSpace(v[:i]))
		v = strings.TrimSpace(v[i+1:])
	}
	id, err := strconv.Atoi(v)
	if err != nil || id <= 0 {
		return Hint{}, false
	}
	switch typ {
	case Movie, TV:
	case "":
		typ = "" // 只有 id，类型交给库类型推断
	default:
		// 认不出的类型（比如 "person/123"）宁可不用，也不要让它去查错的端点。
		return Hint{}, false
	}
	return Hint{TMDBID: id, Type: typ}, true
}
