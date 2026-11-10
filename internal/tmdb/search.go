package tmdb

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Chenglp/lumen/internal/store"
)

// ErrBadQuery 单独一个哨兵，便于调用方区分「这条记录跳过」与「TMDB 出问题」。
var ErrBadQuery = errors.New("tmdb: 查询为空或不可用")

// normalizeType 把空值归到 multi，其余非法值也归到 multi 而不是报错——
// 库类型可能是 photo/music，那类库根本不进匹配，但一旦进了，多搜一遍比报错好。
func normalizeType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case Movie:
		return Movie
	case TV:
		return TV
	default:
		return Multi
	}
}

// Search 搜一次，返回投影后的候选。
//
// **year 参数名随端点不同，这一条错了会静默失效**：
//
//	/search/movie  接 year（和 primary_release_year）
//	/search/tv     接 first_air_date_year；传 year 会被忽略，**不报错**
//	/search/multi  两个都不接，只能在返回后本地过滤
//
// 如果统一传 year，剧集的年份过滤等于没做；后果不是报错，是 R6 那条
// 「狂飙 total=31，靠文件名里的年份才排得开」直接失效，而看代码的人
// 只会看到「我们传了年份」。
func (c *Client) Search(ctx context.Context, mediaType, query string, year *int, limit int) (Outcome, error) {
	mt := normalizeType(mediaType)
	q := strings.TrimSpace(query)
	if q == "" {
		return Outcome{}, &Error{Op: "/search/" + mt, kind: ErrBadQuery}
	}
	// 超长不报错而是要截断：解析器已经把片名切出来了，超长只可能是
	// 某个文件名的标题部分畸形（比如整条路径被当成了标题）。
	// 为一条脏数据让整个扫描失败不合算。
	if utf8.RuneCountInString(q) > MaxQueryRunes {
		q = truncateRunes(q, MaxQueryRunes)
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 20 {
		limit = 20
	}

	key := CacheKey(store.TMDBKindSearch, mt, c.language, 0, yearToken(year), hashQuery(q))
	var proj searchProjection
	if hit, err := c.cacheGet(ctx, key, c.searchTTL, &proj); err != nil {
		return Outcome{}, err
	} else if hit {
		return Outcome{Hits: clip(proj.Hits, limit), Total: proj.Total, FromCache: true}, nil
	}

	type result struct {
		proj searchProjection
	}
	v, err := doFlight(c.flight, "search|"+key, func() (result, error) {
		p := url.Values{}
		p.Set("query", q)
		p.Set("language", c.language)
		p.Set("include_adult", "false")
		if year != nil {
			switch mt {
			case TV:
				p.Set("first_air_date_year", strconv.Itoa(*year))
			case Movie:
				p.Set("year", strconv.Itoa(*year))
				// multi 端点没有年份参数，这里什么都不加，由调用方在打分层面处理。
			}
		}

		var raw searchResp
		if err := c.get(ctx, "/search/"+mt, p, &raw); err != nil {
			return result{}, err
		}

		_hits := make([]Hit, 0, len(raw.Results))
		for _, it := range raw.Results {
			if h, ok := projectSearchItem(it, mt); ok {
				_hits = append(_hits, h)
			}
		}
		return result{proj: searchProjection{Hits: _hits, Total: raw.TotalResults}}, nil
	})
	if err != nil {
		return Outcome{}, err
	}

	// 写缓存失败不影响本次结果：缓存是加速手段，不是正确性的一部分。
	// 但**空结果也要写**——R6 的原话是「避免同一个错名反复搜」，
	// 而错名搜出来的一定是空。
	_ = c.cachePut(ctx, key, store.TMDBKindSearch, mt, 0, v.proj)

	return Outcome{Hits: clip(v.proj.Hits, limit), Total: v.proj.Total, Requests: 1}, nil
}

// clip 按 limit 截断。
//
// 注意别写成「就地截断后继续用同一个切片」：缓存命中时 proj 是解码出来的副本，
// 截断本身没问题；但缓存里存的是完整列表，若哪天改成共享切片，
// 一次 limit=3 的调用就会让后面 limit=20 的调用只剩 3 条。
// 这里显式返回新切片，把这个隐患掐掉。
func clip(h []Hit, n int) []Hit {
	if n <= 0 || len(h) <= n {
		return h
	}
	out := make([]Hit, n)
	copy(out, h[:n])
	return out
}

// projectSearchItem 把一条原始结果投影成 Hit。第二个返回值为 false 表示丢弃。
func projectSearchItem(it searchItem, want string) (Hit, bool) {
	mt := it.MediaType
	if mt == "" {
		mt = want // 单类型端点不返回 media_type
	}
	// /search/multi 会返回 media_type=person 的人物条目，它们没有 title/name
	// 字段（只有 name），直接放进候选里会让右栏出现一条点不动的空行。
	if mt != Movie && mt != TV {
		return Hit{}, false
	}

	h := Hit{
		TMDBID:           it.ID,
		MediaType:        mt,
		OriginalLanguage: it.OriginalLanguage,
		Popularity:       it.Popularity,
		PosterPath:       it.PosterPath,
		Overview:         it.Overview,
	}
	if mt == TV {
		h.Title, h.OriginalTitle = it.Name, it.OriginalName
		h.Year = yearOf(it.FirstAirDate)
	} else {
		h.Title, h.OriginalTitle = it.Title, it.OriginalTitle
		h.Year = yearOf(it.ReleaseDate)
	}
	if h.TMDBID <= 0 || (h.Title == "" && h.OriginalTitle == "") {
		return Hit{}, false
	}
	return h, true
}

// Detail 取详情。
//
// append_to_response 把 credits 与 external_ids 合并进同一次请求，这是 R6
// 「一条片子 3 次请求」降到 2 次的地方。缓存按 (id, language) 走，
// 所以同一部剧的 12 集只付一次。
func (c *Client) Detail(ctx context.Context, mediaType string, id int) (Detail, bool, error) {
	mt := normalizeType(mediaType)
	if mt == Multi {
		return Detail{}, false, &Error{Op: "/detail", kind: ErrBadQuery}
	}
	if id <= 0 {
		return Detail{}, false, &Error{Op: "/detail", kind: ErrBadQuery}
	}

	key := CacheKey(store.TMDBKindDetail, mt, c.language, id)
	var d Detail
	if hit, err := c.cacheGet(ctx, key, c.detailTTL, &d); err != nil {
		return Detail{}, false, err
	} else if hit {
		return d, true, nil
	}

	type result struct{ d Detail }
	v, err := doFlight(c.flight, "detail|"+key, func() (result, error) {
		p := url.Values{}
		p.Set("language", c.language)
		p.Set("append_to_response", "credits,external_ids")

		var raw detailResp
		if err := c.get(ctx, "/"+mt+"/"+strconv.Itoa(id), p, &raw); err != nil {
			return result{}, err
		}
		return result{d: projectDetail(raw, mt, id)}, nil
	})
	if err != nil {
		return Detail{}, false, err
	}

	_ = c.cachePut(ctx, key, store.TMDBKindDetail, mt, int64(id), v.d)
	return v.d, false, nil
}

func projectDetail(raw detailResp, mt string, id int) Detail {
	d := Detail{
		TMDBID:           id,
		MediaType:        mt,
		Overview:         raw.Overview,
		PosterPath:       raw.PosterPath,
		BackdropPath:     raw.BackdropPath,
		OriginalLanguage: raw.OriginalLanguage,
		Status:           raw.Status,
		VoteAverage:      raw.VoteAverage,
		VoteCount:        raw.VoteCount,
		IMDBID:           raw.ExternalIDs.IMDBID,
	}
	if mt == TV {
		d.Title, d.OriginalTitle = raw.Name, raw.OriginalName
		d.Year = yearOf(raw.FirstAirDate)
		d.TotalEpisodes = raw.NumberOfEpisodes
		for _, s := range raw.Seasons {
			d.Seasons = append(d.Seasons, SeasonInfo{
				Number:       s.SeasonNumber,
				Name:         s.Name,
				EpisodeCount: s.EpisodeCount,
				AirDate:      s.AirDate,
				PosterPath:   s.PosterPath,
			})
		}
		// 剧集没有单一时长，episode_run_time 是数组且经常为空，不投影。
	} else {
		d.Title, d.OriginalTitle = raw.Title, raw.OriginalTitle
		d.Year = yearOf(raw.ReleaseDate)
		d.RuntimeMinutes = raw.Runtime
	}
	for _, g := range raw.Genres {
		if g.Name != "" {
			d.Genres = append(d.Genres, g.Name)
		}
	}
	for i, cst := range raw.Credits.Cast {
		if i >= 10 {
			break
		}
		d.Cast = append(d.Cast, Credit{
			Name: cst.Name, Character: cst.Character, Profile: cst.Profile, Order: cst.Order,
		})
	}
	for _, cw := range raw.Credits.Crew {
		switch cw.Job {
		case "Director", "Writer", "Screenplay", "Creator", "Producer":
			d.Crew = append(d.Crew, Credit{Name: cw.Name, Job: cw.Job, Profile: cw.Profile})
		}
		if len(d.Crew) >= 10 {
			break
		}
	}
	return d
}

// Season 取一季的集列表。
//
// 只在「已知季号且需要集标题」时调用——它不包含在详情里，所以每季单独一次。
// 缓存按 (id, season, language)，同一季的 12 集只付一次。
func (c *Client) Season(ctx context.Context, id, season int) (SeasonDetail, bool, error) {
	if id <= 0 || season < 0 {
		return SeasonDetail{}, false, &Error{Op: "/season", kind: ErrBadQuery}
	}
	key := CacheKey(store.TMDBKindSeason, TV, c.language, id, strconv.Itoa(season))

	var sd SeasonDetail
	if hit, err := c.cacheGet(ctx, key, c.detailTTL, &sd); err != nil {
		return SeasonDetail{}, false, err
	} else if hit {
		return sd, true, nil
	}

	type result struct{ sd SeasonDetail }
	v, err := doFlight(c.flight, "season|"+key, func() (result, error) {
		p := url.Values{}
		p.Set("language", c.language)

		var raw seasonResp
		if err := c.get(ctx, "/tv/"+strconv.Itoa(id)+"/season/"+strconv.Itoa(season), p, &raw); err != nil {
			return result{}, err
		}
		out := SeasonDetail{TMDBID: id, SeasonNumber: raw.SeasonNumber, Name: raw.Name}
		for _, e := range raw.Episodes {
			out.Episodes = append(out.Episodes, Episode{
				Number:    e.EpisodeNumber,
				Name:      e.Name,
				Overview:  e.Overview,
				AirDate:   e.AirDate,
				StillPath: e.StillPath,
				Runtime:   e.Runtime,
				Vote:      e.VoteAverage,
			})
		}
		return result{sd: out}, nil
	})
	if err != nil {
		return SeasonDetail{}, false, err
	}
	_ = c.cachePut(ctx, key, store.TMDBKindSeason, TV, int64(id), v.sd)
	return v.sd, false, nil
}

// Validate 真打一次 /configuration 验证凭据，用于契约 §5.2 的「写入前先验证」。
//
// 不缓存：它的用途就是「现在这一刻通不通」。
func (c *Client) Validate(ctx context.Context) error {
	p := url.Values{}
	var out struct {
		Images struct {
			SecureBaseURL string `json:"secure_base_url"`
		} `json:"images"`
	}
	if err := c.get(ctx, "/configuration", p, &out); err != nil {
		return err
	}
	return nil
}

func truncateRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// yearToken 把年份折成缓存键的一段。nil 用 `-`，与「0 年」区分开。
func yearToken(y *int) string {
	if y == nil {
		return "-"
	}
	return strconv.Itoa(*y)
}

type searchProjection struct {
	Hits  []Hit `json:"hits"`
	Total int   `json:"total"`
}
