package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"time"
)

// FormatTags 读容器级的 tag，不做任何解释。
//
// 存在的理由只有一个：R6 实测我们自己的片库里，mkv 的 format tags 直接写着
//
//	TMDB=movie/1633056
//	IMDB=tt43515101
//
// 也就是说**文件自己知道它是哪部片**。有它就完全不必猜片名，一次详情请求
// 就能定下来；没有才回退到文件名搜索（§4.2.3 的三级优先级）。
//
// 这里只取原始 map，不解析 TMDB 的写法——那是 tmdb 包的事（HintFromTags）。
// 放在 probe 是因为它属于「读媒体文件」这一类，和转码探测共用同一个 ffprobe。
func (b *Bin) FormatTags(ctx context.Context, path string) (map[string]string, error) {
	if b.FFprobe == "" {
		return nil, ErrFFmpegMissing
	}
	// 超时给 15 秒：ffprobe 读的是文件头与尾部，机械盘上等寻道可能要几秒。
	// 给太长会让一个坏文件把整轮扫描拖住，给太短会在网络盘上误判成"没有 tag"。
	out, err := b.run(ctx, 15*time.Second, b.FFprobe,
		"-v", "quiet", "-print_format", "json", "-show_format", path)
	if err != nil {
		return nil, fmt.Errorf("probe: ffprobe %s: %w", filepath.Base(path), err)
	}
	tags, perr := ParseFormatTags(out)
	if perr != nil {
		return nil, fmt.Errorf("probe: 解析 %s 的 format: %w", filepath.Base(path), perr)
	}
	return tags, nil
}

// ParseFormatTags 解 ffprobe 的 -show_format 输出。
//
// tags 的值先按 any 解再转字符串：某些封装会把 tag 写成数字
// （`"TMDB": 1633056`），直接解进 map[string]string 会整条失败——
// 于是「这个文件没有 TMDB tag」，而它其实有。
func ParseFormatTags(out []byte) (map[string]string, error) {
	var doc struct {
		Format struct {
			Tags map[string]any `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	tags := make(map[string]string, len(doc.Format.Tags))
	for k, v := range doc.Format.Tags {
		tags[k] = scalarString(v)
	}
	return tags, nil
}

// scalarString 把 JSON 标量转成字符串。
//
// 不能直接用 fmt.Sprint：JSON 的数字解出来是 float64，`fmt.Sprint(1633056)`
// 得到的是 `1.633056e+06`，而不是 `1633056`。下游 Atoi 失败后会静默认为
// 「这个文件没有 TMDB tag」，然后退回按文件名搜索——不报错，只是每次多花
// 一次请求，而且搜索结果还可能不如文件自带的那条准。
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
