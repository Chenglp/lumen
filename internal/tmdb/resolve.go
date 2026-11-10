package tmdb

import (
	"context"
	"errors"
	"strconv"

	"github.com/Chenglp/lumen/internal/domain"
)

// Hint 是「文件自己说的元数据」，优先级高于任何搜索。
//
// 三层的顺序不是偏好，是成本与可靠性同时决定的（R6 检查单 §3）：
//
//	nfo   → 用户自己的刮削结果，一次请求（详情），且通常已缓存
//	tag   → 容器里的 TMDB=movie/1633056（实测 mkv 的 format_tags 直接带）
//	name  → 文件名搜索，1 次搜索 + 至少 1 次详情，且可能搜不到
//
// 每往下一层，请求数与不确定性都翻一倍。所以能读 nfo 就不要读 tag，
// 能读 tag 就不要搜。
type Hint struct {
	TMDBID int
	Type   string // movie | tv；空表示未知，按库类型推断
	Source string // nfo | tag | name，只用于显示与诊断
}

// Lookup 是一次匹配的全部输入。
type Lookup struct {
	Query domain.MatchQuery
	Hint  Hint
	Type  string // 期望端点；空则按 Query 推断，仍推不出就走 multi
	Limit int
	// Enrich 给前 N 个候选补详情（契约 CandidateItem 的 totalSeasons/totalEpisodes
	// 只在详情里有）。默认 0：28 屏右栏只显示片名与年份，不需要为此多打 3 次请求。
	Enrich int
}

// Resolution 是可以直接拿去渲染 28 屏右栏的东西。
//
// 不再返回 []domain.Scored 让调用方自己算副标：`Reason` 与 `Confidence` 的
// 入参是打分结果，而打分结果只有这一层有。多一次转手就多一次拼错的机会，
// 表现为右栏显示「未识别」但旁边明明排着三个候选。
type Resolution struct {
	Hits   []Hit
	Detail *Detail // 仅 hint 命中（或 Enrich 覆盖到）时非空

	Source       string // hint | search
	YearDropped  bool   // 年份过滤导致 0 结果，已去掉年份重搜
	Disagreement bool   // 文件自带的 id 与文件名对不上，已额外搜了一轮
	// Confirmed 表示 id 来自文件自带的元数据（nfo / 容器 tag），不是猜的。
	Confirmed bool
	Requests  int

	Code       domain.ParseCode
	Params     map[string]any
	Confidence float64
}

// Resolve 按 nfo/tag → 文件名搜索的顺序找候选。
func (c *Client) Resolve(ctx context.Context, lk Lookup) (Resolution, error) {
	var res Resolution

	if lk.Hint.TMDBID > 0 {
		mt := normalizeType(firstNonEmpty(lk.Hint.Type, lk.Type))
		if mt != Multi {
			d, cached, err := c.Detail(ctx, mt, lk.Hint.TMDBID)
			if err == nil {
				if !cached {
					res.Requests++
				}
				hit := hitFromDetail(d)
				hits, ranked := rankHits(lk.Query, []Hit{hit})
				res = finish(res, lk.Query, hits, ranked)
				res.Detail = &d
				res.Source = "hint"

				// 自带的 id 与文件名差太多，说明 nfo/tag 可能是陈的
				// （换了片子但沿用了旧目录与旧 nfo 是真实存在的操作事故）。
				// 这时再搜一轮，把搜到的候选与自带的那条并排放：
				// 静默采用错的 id，用户点开播放才会发现海报是另一部片。
				if res.Confidence >= HintMismatchScore {
					res.Confirmed = true
					res.Confidence = 1
					return res, nil
				}
				res.Disagreement = true
				searched, sres, err := c.searchResolve(ctx, lk)
				if err != nil {
					// 搜索失败不该抹掉已有结果：自带的那条仍然可点。
					return res, nil
				}
				res.Requests += sres.Requests
				merged := append(append([]Hit{}, searched...), hit)
				hits, ranked = rankHits(lk.Query, merged)
				res = finish(res, lk.Query, hits, ranked)
				res.Detail = &d
				res.Source = "hint"
				res.Disagreement = true
				// 不一致时不给 Confirmed，也不把把握抬到 1：
				// 这时"文件自己说的"和"文件名说的"是两件事，得让用户选。
				return res, nil
			}
			// 只有 404 才降级：id 在 TMDB 没了（合并、下架）。凭据或网络问题
			// 降级成搜索只会把同一个故障变成两次失败，且把原因掩盖掉。
			if !errors.Is(err, ErrNotFound) {
				return res, err
			}
			res.Requests++
		}
	}

	hits, sres, err := c.searchResolve(ctx, lk)
	if err != nil {
		return res, err
	}
	res = finish(res, lk.Query, hits, nil)
	res.Source = "search"
	res.YearDropped = sres.YearDropped
	res.Requests += sres.Requests

	if lk.Enrich > 0 {
		hits = c.enrich(ctx, hits, lk.Enrich, &res)
		res.Hits = hits
	}
	return res, nil
}

// HintMismatchScore 是「自带 id 与文件名是否同一部作品」的分界。
//
// 0.35 取得很低，因为这道判断只用来抓**明显不是同一部**的情况：
// 片名换了语言、片名被改过、文件名只有集号，都会让分数偏低但不该触发重搜。
// 真正要抓的是「NFO 里是《教父》，文件名是《星际穿越》」——那种分数接近 0。
const HintMismatchScore = 0.35

type searchOutcome struct {
	YearDropped bool
	Requests    int
}

// searchResolve 是按文件名搜索的那条路，hint 降级时也复用它。
func (c *Client) searchResolve(ctx context.Context, lk Lookup) ([]Hit, searchOutcome, error) {
	var so searchOutcome

	mt := normalizeType(firstNonEmpty(lk.Type, lk.Query.LibKind))
	title := lk.Query.Result.Parsed.Title
	if title == "" {
		// 没有片名就没有可搜的东西。返回空候选，让 Reason 把解析阶段的
		// code 原样还回去——那时副标该显示「文件名里没有剧名」，
		// 而不是发一次必然为空的请求。
		return nil, so, nil
	}

	limit := lk.Limit
	if limit <= 0 {
		limit = domain.MaxCandidates
	}

	out, err := c.Search(ctx, mt, title, lk.Query.Result.Parsed.Year, limit)
	if err != nil {
		return nil, so, err
	}
	// 用 out.Requests 而不是无脑 +1：缓存命中时它本来就是 0。
	// 这个数直接进 28 屏的「还要多久」，算错一次就是给用户一个假的进度。
	so.Requests += out.Requests

	// 年份过滤会误杀：发布名里的年份是压制/发行年，与 TMDB 的首播年
	// 差一岁是常态。带年份搜到 0 条时去掉年份重搜一次——
	// 这是极少数值得付出额外请求的情况，因为「0 条」和「搜到了但不是它」
	// 对用户的含义完全不同（前者无解，后者只需挑一下）。
	if len(out.Hits) == 0 && lk.Query.Result.Parsed.Year != nil {
		if out2, err := c.Search(ctx, mt, title, nil, limit); err == nil && len(out2.Hits) > 0 {
			so.YearDropped = true
			so.Requests += out2.Requests
			out = out2
		}
	}

	cands := make([]domain.Candidate, 0, len(out.Hits))
	for _, h := range out.Hits {
		cands = append(cands, domain.Candidate{
			TMDBID:           h.TMDBID,
			MediaType:        h.MediaType,
			Title:            h.Title,
			OriginalTitle:    h.OriginalTitle,
			Year:             h.Year,
			OriginalLanguage: h.OriginalLanguage,
			Popularity:       h.Popularity,
		})
	}
	ranked := domain.Rank(lk.Query, cands)

	// 把分数与推荐角标贴回 Hit，并保持 Rank 的顺序。
	byKey := make(map[string]Hit, len(out.Hits))
	for _, h := range out.Hits {
		byKey[h.MediaType+":"+strconv.Itoa(h.TMDBID)] = h
	}
	hits := make([]Hit, 0, len(ranked))
	for _, s := range ranked {
		h, ok := byKey[s.MediaType+":"+strconv.Itoa(s.TMDBID)]
		if !ok {
			continue
		}
		h.Score, h.Recommended = s.Score, s.Recommended
		hits = append(hits, h)
	}
	return hits, so, nil
}

// rankHits 用于 hint 那条路径：候选只有一个，但仍走同一套打分，
// 这样「nfo 说的」与「搜索到的」两条来源的分数是可比的。
func rankHits(q domain.MatchQuery, hits []Hit) ([]Hit, []domain.Scored) {
	cands := make([]domain.Candidate, 0, len(hits))
	for _, h := range hits {
		cands = append(cands, domain.Candidate{
			TMDBID: h.TMDBID, MediaType: h.MediaType, Title: h.Title,
			OriginalTitle: h.OriginalTitle, Year: h.Year,
			OriginalLanguage: h.OriginalLanguage, Popularity: h.Popularity,
		})
	}
	ranked := domain.Rank(q, cands)
	byKey := make(map[string]int, len(hits))
	for i, h := range hits {
		byKey[h.MediaType+":"+strconv.Itoa(h.TMDBID)] = i
	}
	out := make([]Hit, 0, len(ranked))
	for _, s := range ranked {
		i, ok := byKey[s.MediaType+":"+strconv.Itoa(s.TMDBID)]
		if !ok {
			continue
		}
		h := hits[i]
		h.Score, h.Recommended = s.Score, s.Recommended
		out = append(out, h)
	}
	return out, ranked
}

func finish(res Resolution, q domain.MatchQuery, hits []Hit, ranked []domain.Scored) Resolution {
	res.Hits = hits
	if ranked == nil {
		_, ranked = rankHits(q, hits)
	}
	res.Code, res.Params = domain.Reason(q, ranked)
	res.Confidence = domain.Confidence(ranked)
	return res
}

func hitFromDetail(d Detail) Hit {
	return Hit{
		TMDBID:           d.TMDBID,
		MediaType:        d.MediaType,
		Title:            d.Title,
		OriginalTitle:    d.OriginalTitle,
		Year:             d.Year,
		OriginalLanguage: d.OriginalLanguage,
		PosterPath:       d.PosterPath,
		Overview:         d.Overview,
	}
}

// enrich 给前 n 个候选补详情。
//
// 只补前 n 个：28 屏右栏只放 3 个候选，给第 4 名之后补详情是纯浪费。
// 详情本身按 (id, language) 缓存，所以同一部作品只在第一次付费。
func (c *Client) enrich(ctx context.Context, hits []Hit, n int, res *Resolution) []Hit {
	if n > len(hits) {
		n = len(hits)
	}
	for i := 0; i < n; i++ {
		d, cached, err := c.Detail(ctx, hits[i].MediaType, hits[i].TMDBID)
		if err != nil {
			continue // 补详情失败不影响已经排好的候选
		}
		if !cached {
			res.Requests++
		}
		if hits[i].PosterPath == "" {
			hits[i].PosterPath = d.PosterPath
		}
		if res.Detail == nil && i == 0 {
			dd := d
			res.Detail = &dd
		}
	}
	return hits
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
