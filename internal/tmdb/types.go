// Package tmdb 是 TMDB 的最小客户端：搜索、详情、季集，外加缓存与自适应限流。
//
// 它不做两件事，故意：
//
//   - 不决定「用什么词去搜」。那是 domain 解析器的事（§4.2.1）。
//   - 不决定「哪个候选才对」。那是 domain 打分器的事（§4.2.2）。
//
// 这一层只负责：把请求发出去、把结果按投影后的形状拿回来、把重复请求吃掉。
// 边界划在这里的理由是 R6 的实测——真正的失败（脏文件名搜不到、短标题被综艺抢）
// 全部发生在语义层，不在传输层；而传输层的问题（限流、不可达、凭据失效）
// 需要的是完全不同的一套处置。
package tmdb

import (
	"time"
)

// 政策常量。
const (
	// DefaultBaseURL 是 TMDB v3 API 根。
	DefaultBaseURL = "https://api.themoviedb.org/3"

	// MaxCacheAge 是条款允许的缓存硬上界：6 个月。
	// 契约 §5.2 的 ttlDays 上限 180 天就是它，两处必须一致。
	MaxCacheAge = 180 * 24 * time.Hour

	// DefaultDetailTTL 是元数据的默认 TTL（契约 ttlDays 默认 30）。
	DefaultDetailTTL = 30 * 24 * time.Hour

	// DefaultSearchTTL 是搜索结果的 TTL：7 天。
	//
	// 比详情短一个数量级，因为搜索缓存的**失败结果也要存**（R6 的原话是
	// 「避免同一个错名反复搜」）——而 TMDB 库里新增条目的速度远快于元数据修订，
	// 错名在 7 天后再搜一次是有意义的，30 天则太久。
	DefaultSearchTTL = 7 * 24 * time.Hour

	// MaxBody 是响应体硬上限。TMDB 的 detail + credits 约 40–80KB，
	// 给 8MB 是防「代理返回一个 HTML 错误页」这类情况把内存吃满。
	MaxBody = 8 << 20

	// MaxQueryRunes 对齐契约 §1.3 的 q 上限。
	MaxQueryRunes = 200
)

// 媒体类型。字符串而非枚举：它们直接进 URL 路径与缓存键。
const (
	Movie = "movie"
	TV    = "tv"
	Multi = "multi"
)

// Hit 是搜索命中的投影，也是 28 屏右栏的候选。
//
// 只投影这些字段的原因：搜索响应每条有二十几个字段，其中 adult / video /
// genre_ids / video 之类永不参与排序也永不显示。投影后一条约 200 字节，
// 原始响应约 2KB——5 万文件量级下是百 MB 与 GB 的差别（见 store.TMDBEntry）。
type Hit struct {
	TMDBID           int     `json:"tmdbId"`
	MediaType        string  `json:"mediaType"`
	Title            string  `json:"title"`
	OriginalTitle    string  `json:"originalTitle,omitempty"`
	Year             *int    `json:"year,omitempty"`
	OriginalLanguage string  `json:"originalLanguage,omitempty"`
	Popularity       float64 `json:"popularity,omitempty"`
	PosterPath       string  `json:"posterPath,omitempty"`
	Overview         string  `json:"overview,omitempty"`

	// 以下是打分产物，不进缓存：缓存的是「TMDB 说了什么」，
	// 分数是「按当前库设置算出来什么」，换库类型或改权重后必须重算。
	Score       float64 `json:"-"`
	Recommended bool    `json:"-"`
}

// SeasonInfo 是剧集详情里的季条目。
type SeasonInfo struct {
	Number       int    `json:"number"`
	Name         string `json:"name,omitempty"`
	EpisodeCount int    `json:"episodeCount,omitempty"`
	AirDate      string `json:"airDate,omitempty"`
	PosterPath   string `json:"posterPath,omitempty"`
}

// Credit 是演职人员的一条。
type Credit struct {
	Name      string `json:"name"`
	Character string `json:"character,omitempty"`
	Job       string `json:"job,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Order     int    `json:"order,omitempty"`
}

// Detail 是详情投影。
//
// 同样只留 28 屏与详情页真的会渲染的字段：简介、海报、类型、时长、季列表、
// 主要演职、外部 id。TMDB 的详情响应还有 production_companies / spoken_languages
// / belongs_to_collection 等二三十个字段，本产品不用。
type Detail struct {
	TMDBID           int          `json:"tmdbId"`
	MediaType        string       `json:"mediaType"`
	Title            string       `json:"title"`
	OriginalTitle    string       `json:"originalTitle,omitempty"`
	Overview         string       `json:"overview,omitempty"`
	PosterPath       string       `json:"posterPath,omitempty"`
	BackdropPath     string       `json:"backdropPath,omitempty"`
	Year             *int         `json:"year,omitempty"`
	Genres           []string     `json:"genres,omitempty"`
	RuntimeMinutes   int          `json:"runtimeMinutes,omitempty"`
	VoteAverage      float64      `json:"voteAverage,omitempty"`
	VoteCount        int          `json:"voteCount,omitempty"`
	OriginalLanguage string       `json:"originalLanguage,omitempty"`
	Status           string       `json:"status,omitempty"`
	Seasons          []SeasonInfo `json:"seasons,omitempty"`
	TotalEpisodes    int          `json:"totalEpisodes,omitempty"`
	IMDBID           string       `json:"imdbId,omitempty"`
	Cast             []Credit     `json:"cast,omitempty"`
	Crew             []Credit     `json:"crew,omitempty"`
}

// Episode 是一集的元数据。
type Episode struct {
	Number    int     `json:"number"`
	Name      string  `json:"name,omitempty"`
	Overview  string  `json:"overview,omitempty"`
	AirDate   string  `json:"airDate,omitempty"`
	StillPath string  `json:"stillPath,omitempty"`
	Runtime   int     `json:"runtime,omitempty"`
	Vote      float64 `json:"vote,omitempty"`
}

// SeasonDetail 是一季的集列表。
type SeasonDetail struct {
	TMDBID       int       `json:"tmdbId"`
	SeasonNumber int       `json:"seasonNumber"`
	Name         string    `json:"name,omitempty"`
	Episodes     []Episode `json:"episodes,omitempty"`
}

// Outcome 是一次搜索的结果，附带它是花钱买的还是缓存来的。
//
// Requests 必须暴露出去：契约 §5.1 的连接探测与 32 屏的诊断都靠它，
// 而且「换了一次权重后请求数涨了多少」是这套设计唯一的成本可见性。
type Outcome struct {
	Hits      []Hit
	Total     int
	FromCache bool
	Requests  int
}

// ---- 以下是 TMDB 的线上形状，只在本包内使用 ----

type searchResp struct {
	Page         int          `json:"page"`
	TotalResults int          `json:"total_results"`
	TotalPages   int          `json:"total_pages"`
	Results      []searchItem `json:"results"`
}

type searchItem struct {
	ID               int     `json:"id"`
	MediaType        string  `json:"media_type"` // 只有 multi 端点有
	Title            string  `json:"title"`
	Name             string  `json:"name"`
	OriginalTitle    string  `json:"original_title"`
	OriginalName     string  `json:"original_name"`
	ReleaseDate      string  `json:"release_date"`
	FirstAirDate     string  `json:"first_air_date"`
	OriginalLanguage string  `json:"original_language"`
	Popularity       float64 `json:"popularity"`
	PosterPath       string  `json:"poster_path"`
	Overview         string  `json:"overview"`
	Adult            bool    `json:"adult"`
}

type detailResp struct {
	ID               int     `json:"id"`
	Title            string  `json:"title"`
	Name             string  `json:"name"`
	OriginalTitle    string  `json:"original_title"`
	OriginalName     string  `json:"original_name"`
	Overview         string  `json:"overview"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	ReleaseDate      string  `json:"release_date"`
	FirstAirDate     string  `json:"first_air_date"`
	OriginalLanguage string  `json:"original_language"`
	Status           string  `json:"status"`
	Runtime          int     `json:"runtime"`
	VoteAverage      float64 `json:"vote_average"`
	VoteCount        int     `json:"vote_count"`
	Genres           []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Seasons []struct {
		SeasonNumber int    `json:"season_number"`
		Name         string `json:"name"`
		EpisodeCount int    `json:"episode_count"`
		AirDate      string `json:"air_date"`
		PosterPath   string `json:"poster_path"`
	} `json:"seasons"`
	NumberOfEpisodes int `json:"number_of_episodes"`
	ExternalIDs      struct {
		IMDBID string `json:"imdb_id"`
	} `json:"external_ids"`
	Credits struct {
		Cast []struct {
			Name      string `json:"name"`
			Character string `json:"character"`
			Profile   string `json:"profile_path"`
			Order     int    `json:"order"`
		} `json:"cast"`
		Crew []struct {
			Name    string `json:"name"`
			Job     string `json:"job"`
			Profile string `json:"profile_path"`
		} `json:"crew"`
	} `json:"credits"`
}

type seasonResp struct {
	SeasonNumber int    `json:"season_number"`
	Name         string `json:"name"`
	Episodes     []struct {
		EpisodeNumber int     `json:"episode_number"`
		Name          string  `json:"name"`
		Overview      string  `json:"overview"`
		AirDate       string  `json:"air_date"`
		StillPath     string  `json:"still_path"`
		Runtime       int     `json:"runtime"`
		VoteAverage   float64 `json:"vote_average"`
	} `json:"episodes"`
}

type errResp struct {
	StatusCode    int    `json:"status_code"`
	StatusMessage string `json:"status_message"`
	Success       bool   `json:"success"`
}

// yearOf 从 `2014-11-07` 里取年份。
//
// 两个字段名必须分开处理，这不是洁癖：`/search/tv` 的条目只有 `first_air_date`，
// 若统一读 `release_date`，TV 候选的年份会**全是 nil**。后果不是报错，
// 是年份信号恒为 0、TV 永远够不到强匹配门槛（0.85），以及 R6 那条
// 「狂飙要靠年份才排得开」直接失效。
func yearOf(date string) *int {
	if len(date) < 4 {
		return nil
	}
	y := 0
	for i := 0; i < 4; i++ {
		c := date[i]
		if c < '0' || c > '9' {
			return nil
		}
		y = y*10 + int(c-'0')
	}
	if y < 1870 || y > 2200 {
		return nil
	}
	return &y
}
