package tmdb

import "testing"

func TestHintFromTags(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		want Hint
	}{
		{
			// R6 在我们自己的片库里实测到的写法。
			name: "movie 前缀",
			tags: map[string]string{"TMDB": "movie/1633056", "IMDB": "tt43515101"},
			want: Hint{TMDBID: 1633056, Type: Movie, Source: "tag"},
		},
		{
			name: "tv 前缀 + 全小写键",
			tags: map[string]string{"tmdb": "tv/1396"},
			want: Hint{TMDBID: 1396, Type: TV, Source: "tag"},
		},
		{
			name: "只有 id，类型留给库类型推断",
			tags: map[string]string{"TMDB": "1633056"},
			want: Hint{TMDBID: 1633056, Type: "", Source: "tag"},
		},
		{
			// 认错端点比不认更糟：会拿 movie id 去查 /tv/{id}，返回 404 后
			// 又被当成"该 id 已下架"降级搜索，原因彻底被掩盖。
			name: "不认识的类型宁可不认",
			tags: map[string]string{"TMDB": "person/287"},
			want: Hint{},
		},
		{
			name: "垃圾值",
			tags: map[string]string{"TMDB": "not-a-number"},
			want: Hint{},
		},
		{
			name: "只有 IMDB",
			tags: map[string]string{"IMDB": "tt43515101"},
			want: Hint{},
		},
		{name: "空 map", tags: nil, want: Hint{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HintFromTags(c.tags); got != c.want {
				t.Errorf("got %+v，期望 %+v", got, c.want)
			}
		})
	}
}
