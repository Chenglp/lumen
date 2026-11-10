package domain

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 文件名解析 —— 对应技术框架 §4.2 第 3 级。
//
// R6 的实测把这件事从"优化"变成了"硬前置"：
//
//	漫长的季节.S01E01.2160p.WEB-DL.H265.DDP5.1   → total_results = 0
//	[压制组] 狂飙 2023 1080p 全39集                → total_results = 0
//	Interstellar.2014.2160p.UHD.BluRay.x265-…     → total_results = 0
//
// 连纯英文的标准发布名都是 0，原因是点号与方括号把 TMDB 的分词彻底打碎。
// 所以**没有解析就没有任何兜底路径**：UNMATCHED 行只能显示文件名本身。
//
// 输出按契约的 reasonCode 组织（不是按"成功/失败"两分），因为每行副标那句话
// 由它决定，而服务端不拼中文。

// ParseCode 是解析器自己的判定。
//
// 前四个与契约 `UnmatchedReasonCode` 同名同义，可以直接往外抛；
// `OK` / `TITLE_ONLY` / `EMPTY` 不是契约 code——它们是"解析完成、等匹配"的状态，
// 由匹配阶段换成 `STRONG_MATCH` / `TITLE_YEAR_OVERLAP` 等。
type ParseCode string

const (
	// ParseOK 片名与年份都拿到了，可以直接去搜。
	ParseOK ParseCode = "OK"
	// ParseTitleOnly 只有片名没有年份。年份缺失会让中文短标题被综艺抢走（R6 §5 坑二），
	// 所以这是个需要人看一眼的状态，不是错误。
	ParseTitleOnly ParseCode = "TITLE_ONLY"
	// ParseEmpty 什么都没解析出来，连标题都没有。
	ParseEmpty ParseCode = "EMPTY"

	// ParseYearOrTitleAmbiguous 整个名字只有一个四位数字：《2012》《1917》。
	// 它既是年份也可能是片名，判错会直接把片子筛没。
	ParseYearOrTitleAmbiguous ParseCode = "YEAR_OR_TITLE_AMBIGUOUS"
	// ParseEpisodeWithoutSeries 有季集号但文件名里没有剧名。
	ParseEpisodeWithoutSeries ParseCode = "EPISODE_WITHOUT_SERIES"
	// ParseYearIsReleaseTag 括号里的那个年份是压制年，不是上映年。
	ParseYearIsReleaseTag ParseCode = "YEAR_IS_RELEASE_TAG"
	// ParseMultiSeasonPack 整季包，需要拆集。
	ParseMultiSeasonPack ParseCode = "MULTI_SEASON_PACK"
)

// ParseWarning 是不改变主判定、但 28 屏要提一句的情况。
//
// 为什么要和 Code 分开：`Interstellar.2014…` 解析得干干净净（OK），
// 但如果括号里还挂了个 `[2019]`，用户就该看到"括号里的 2019 是压制年份"。
// 合成一个 code 会让"解析成功"和"年份可疑"互斥，而它们本来是并存的。
type ParseWarning struct {
	Code   ParseCode
	Params map[string]any
}

// ParsedName 对应契约 `ParsedName`，字段名即 JSON 键，不能再加。
//
// 想加字段先改契约：这个对象会原样进 `unmatched.parsed` 并出现在 A1 响应里，
// 多一个键就是一次契约破坏。
type ParsedName struct {
	Title        string `json:"title"`
	Year         *int   `json:"year"`
	Season       *int   `json:"season"`
	Episode      *int   `json:"episode"`
	Resolution   string `json:"resolution"`
	Source       string `json:"source"`
	ReleaseGroup string `json:"releaseGroup"`

	// 以下不进 JSON，只在进程内使用。
	//
	// 它们要么是 28 屏之外的用途（搜索打分、拆集），要么已经在 probe 里有权威值
	// （编码）。放进 JSON 就等于往契约里塞私有字段——所以一律 `json:"-"`。

	EpisodeEnd    int      `json:"-"` // S01E01-E03 的 03
	SeasonCount   int      `json:"-"` // S01-S03 的 3；单季为 1
	TotalEpisodes int      `json:"-"` // 「全39集」的 39
	SeasonPack    bool     `json:"-"` // 文件名指向一季/整剧，不是单集
	VideoCodec    string   `json:"-"` // H.265 / x264 / AV1
	AudioCodec    string   `json:"-"` // DTS / EAC3 / TrueHD
	AudioChannels string   `json:"-"` // 5.1 / 7.1 / 2.0
	Tags          []string `json:"-"` // DV / HDR / HFR / REMUX…给 28 屏「已识别」行
	LooksSerial   bool     `json:"-"` // 有季集痕迹，倾向 tv
}

// ParseResult 是解析器的完整输出。
type ParseResult struct {
	Parsed   ParsedName
	Code     ParseCode
	Params   map[string]any
	Warnings []ParseWarning

	// TitleFrom 记录标题是从文件名还是上层目录得来的（"file" / "dir" / ""）。
	// 排障时要回答"这个名字凭什么能搜到"，没这一列只能猜。
	TitleFrom string
}

// ---------------------------------------------------------------- 年份

// yearLike 判断一个 token 是否像上映年。
//
// 下界 1888 是电影史的起点（《朗德海花园场景》），上界留两年余量——
// 压制年可能比上映年晚一两年，压太紧会把新片判成"没有年份"。
func yearLike(s string) (int, bool) {
	if len(s) != 4 {
		return 0, false
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, _ := strconv.Atoi(s)
	if n < 1888 || n > time.Now().Year()+2 {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------- 归类表

type tokKind int

const (
	kindNone tokKind = iota
	kindRes
	kindSource
	kindVideo
	kindAudio
	kindTag
)

// strong 表示"看到它就说明标题已经结束"。
//
// 这是整个解析器唯一真正的判据：标题边界不是靠"哪边词多"猜的，
// 而是靠发布名的固定语法——技术标记永远在片名之后。
func (k tokKind) strong() bool {
	switch k {
	case kindRes, kindSource, kindVideo, kindAudio:
		return true
	}
	return false
}

var (
	// 2160p / 1080i / 720p / 4k；也放行裸数字 2160/1080/720（少部分发布名这么写）。
	reRes = regexp.MustCompile(`^(?:[48]k|(?:2160|1080|720|576|480)[pi]|(?:2160|1080|720))$`)
	// 3840x2160 这种直接给分辨率的写法。
	reResDim = regexp.MustCompile(`^\d{3,4}x\d{3,4}$`)
)

var sourceNames = map[string]string{
	"bluray": "BluRay", "blu-ray": "BluRay", "bdrip": "BDRip", "brrip": "BRRip",
	"web-dl": "WEB-DL", "webdl": "WEB-DL", "web": "WEB", "webrip": "WEBRip", "web-rip": "WEBRip",
	"hdtv": "HDTV", "hdrip": "HDRip", "tvrip": "TVRip", "dvdrip": "DVDRip",
	"dvd": "DVD", "dvd5": "DVD5", "dvd9": "DVD9", "bdremux": "Remux", "remux": "Remux",
	"hddvd": "HDDVD", "hdcam": "HDCam", "hdtc": "HDTC", "hdts": "HDTS", "ppv": "PPV",
	"blu": "BluRay", "bd25": "BD25", "bd50": "BD50",
}

var videoNames = map[string]string{
	"x264": "H.264", "h264": "H.264", "h.264": "H.264", "avc": "AVC",
	"x265": "H.265", "h265": "H.265", "h.265": "H.265", "hevc": "HEVC",
	"xvid": "XviD", "divx": "DivX", "vp9": "VP9", "av1": "AV1", "vvc": "VVC",
	"mpeg2": "MPEG-2", "mpeg4": "MPEG-4",
}

// 音轨带声道：dts5.1 / ddp5.1 / aac2.0 / ac3 都可能连写，也可能单独出现。
// 三个分组：编码、可选声道数（5.1 / 7 / 2.0）、结尾。
var reAudio = regexp.MustCompile(`^(dts[-.]?hd|dts[-.]?x|dts|truehd|atmos|e[-.]?ac3|ac3|ddp|dd\+|dd|aac|flac|opus|lpcm|pcm|mp3)(?:[.\-]?\d(?:[.\-]\d)?)?$`)

var audioNames = map[string]string{
	"dts-hd": "DTS-HD", "dtshd": "DTS-HD", "dts.hd": "DTS-HD", "dtsx": "DTS:X", "dts.x": "DTS:X",
	"dts": "DTS", "truehd": "TrueHD", "atmos": "Atmos", "eac3": "EAC3", "e-ac3": "EAC3",
	"ac3": "AC3", "ddp": "DD+", "dd+": "DD+", "dd": "DD", "aac": "AAC", "flac": "FLAC",
	"opus": "Opus", "lpcm": "LPCM", "pcm": "PCM", "mp3": "MP3",
}

// hardTags 是无条件从标题里剔掉的：它们不可能出现在片名里。
var hardTags = map[string]struct{}{
	"proper": {}, "repack": {}, "rerip": {}, "internal": {}, "limited": {},
	"nfofix": {}, "readnfo": {}, "dirfix": {}, "remux": {}, "retail": {},
}

// softTags 只在年份之后才剔。
//
// 因为它们本身就是英文单词：`The.Extended.Family.2019` 的 Extended 是片名的一部分，
// 而 `Movie.2019.EXTENDED.1080p` 的 EXTENDED 是版本标记。位置是唯一能分开两者的信号。
var softTags = map[string]struct{}{
	"extended": {}, "uncut": {}, "unrated": {}, "remastered": {}, "theatrical": {},
	"directors": {}, "director's": {}, "imax": {}, "criterion": {}, "special": {},
	"collectors": {}, "anniversary": {},
}

// 其余标签：不参与标题边界判定，只用于 28 屏的「已识别」行与候选打分。
var plainTags = map[string]struct{}{
	"dv": {}, "dovi": {}, "dolbyvision": {}, "hdr": {}, "hdr10": {}, "hdr10+": {}, "sdr": {},
	"10bit": {}, "8bit": {}, "hq": {}, "hfr": {}, "uhd": {}, "3d": {}, "sbs": {}, "hsbs": {},
	"multi": {}, "dual": {}, "sub": {}, "subs": {}, "subbed": {}, "dubbed": {},
	"chs": {}, "cht": {}, "chi": {}, "zho": {}, "eng": {}, "jpn": {}, "kor": {}, "fre": {},
	"ger": {}, "ita": {}, "spa": {}, "rus": {}, "complete": {}, "repack2": {},
}

// 中文字幕/音轨标记。它们几乎只出现在标题之后，但仍按标签处理——
// 因为「国语」这类词也可能出现在片名里（如《国语》），靠位置区分更稳。
var zhTags = map[string]string{
	"国语": "国语", "粤语": "粤语", "国粤": "国粤", "中字": "中字", "中英": "中英",
	"双语": "双语", "简中": "简中", "繁中": "繁中", "简繁": "简繁", "英语": "英语",
	"日语": "日语", "韩语": "韩语", "台配": "台配", "港配": "港配", "内封": "内封",
	"外挂": "外挂", "高清": "高清", "蓝光": "蓝光", "超清": "超清",
}

// classifyToken 给一个 token 定性，并给出展示值。
func classifyToken(t token) (tokKind, string) {
	k := t.key
	if reRes.MatchString(k) || reResDim.MatchString(k) {
		return kindRes, resDisplay(k)
	}
	if disp, ok := sourceNames[k]; ok {
		return kindSource, disp
	}
	if disp, ok := videoNames[k]; ok {
		return kindVideo, disp
	}
	if m := reAudio.FindStringSubmatch(k); m != nil {
		if disp, ok := audioNames[m[1]]; ok {
			return kindAudio, disp
		}
		return kindAudio, strings.ToUpper(m[1])
	}
	if _, ok := hardTags[k]; ok {
		return kindTag, k
	}
	if _, ok := softTags[k]; ok {
		return kindTag, k
	}
	if _, ok := plainTags[k]; ok {
		return kindTag, k
	}
	if disp, ok := zhTags[t.text]; ok {
		return kindTag, disp
	}
	return kindNone, ""
}

func resDisplay(k string) string {
	switch k {
	case "4k", "8k":
		return strings.ToUpper(k)
	}
	if reResDim.MatchString(k) {
		return k
	}
	if strings.HasSuffix(k, "p") || strings.HasSuffix(k, "i") {
		return k
	}
	return k + "p"
}

// ---------------------------------------------------------------- 分词

type token struct {
	text    string
	key     string // 小写 + '@' 还原成 '.'，只用于查表
	bracket byte   // 0 / '[' / '(' / '{'
	start   int    // 在 working string 中的字节偏移，用来切标题
}

func tokenKey(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "@", ".")
}

// tokenize 把名字切成 token，并记住每个 token 前导的是哪种括号。
//
// 之所以不直接 `strings.FieldsFunc`：方括号和圆括号既可能是压制组前缀
// （`[SubsPlease]`），也可能是技术标记（`[1080p]`、`(2019)`），
// 丢掉括号就等于丢掉区分这两者的唯一依据。
//
// 括号内部**不按点号切**：`[YTS.MX]`、`[RARBG]` 这种带点的组名很常见，
// 切开会得到 `YTS` + `MX`，然后把 MX 当组名写进库。
func tokenize(s string) []token {
	var (
		toks  []token
		buf   strings.Builder
		br    byte
		start int
	)
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		text := buf.String()
		buf.Reset()
		toks = append(toks, token{text: text, key: tokenKey(text), bracket: br, start: start})
	}

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '[' || c == '(' || c == '{':
			flush()
			br = c
			i++
		case c == ']' || c == ')' || c == '}':
			flush()
			br = 0
			i++
		case c == ' ' || c == '\t' || (c == '.' || c == '_') && br == 0:
			flush()
			i++
		default:
			if buf.Len() == 0 {
				start = i
			}
			r, size := utf8.DecodeRuneInString(s[i:])
			buf.WriteRune(r)
			i += size
		}
	}
	flush()
	return toks
}

// ---------------------------------------------------------------- 预处理

var (
	// H.265 / x.264 里的点不是分隔符，先换成 '@' 保住它（长度不变，偏移不乱）。
	reCodecDot = regexp.MustCompile(`(?i)\b([hx])\.(\d{3})\b`)
	// 声道数 5.1 / 7.1 / 2.0 同理。第三组吃掉后一个字符，
	// 这就避开了 `2012.1080p`（`2.1` 后面跟的是数字 0）这类误伤。
	reChannelDot = regexp.MustCompile(`(\d)\.(\d)(\D|$)`)
)

func protectDots(s string) string {
	s = reCodecDot.ReplaceAllString(s, "$1@$2")
	s = reChannelDot.ReplaceAllString(s, "$1@$2$3")
	return s
}

// ---------------------------------------------------------------- 季集

var (
	// S01E02 / S01.E02 / S01E01E02 / S01E01-E03 / S01EP02。
	//
	// 结尾必须是「非字母数字」：否则 `S01E01-1080p` 会把 1080 当成第二集的集号。
	reSE = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,2})[\s._\-]*(?:e|ep)(\d{1,4})((?:[\s._\-]*(?:e|ep)?\d{1,4})?)(?:$|[^0-9a-z])`)
	// 1x02。
	reX = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(\d{1,2})x(\d{2,4})(?:$|[^0-9a-z])`)
	// 只有季：S01 / Season 2 / Series 3。
	reS = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:s|season|series)[\s._\-]*(\d{1,2})(?:$|[^0-9a-z])`)
	// S01-S03 多季包。
	reSRange = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,2})[\s._\-]*-[\s._\-]*s?(\d{1,2})(?:$|[^0-9])`)

	// 第 2 季 / 第二季
	reZhSeason = regexp.MustCompile(`第\s*([0-9一二三四五六七八九十]{1,3})\s*[季部]`)
	// 第 2 集 / 第二话
	reZhEp = regexp.MustCompile(`第\s*([0-9一二三四五六七八九十]{1,4})\s*[集话話期]`)
	// 第 1-10 集
	reZhEpRange = regexp.MustCompile(`第\s*(\d{1,4})\s*[-~—]\s*(\d{1,4})\s*[集话話]`)
	// 全39集 / 共12话
	reZhTotal = regexp.MustCompile(`(?:全|共)\s*(\d{1,4})\s*[集话話]`)

	// 动漫绝对集号：`[SubsPlease] Sousou no Frieren - 12 (1080p) [ABCD1234]`。
	//
	// 只在后面紧跟括号时才认：否则 `Mr. & Mrs. Smith - 2 (2019)` 之类会把年份前的
	// 数字误当集号。这个约束来自真实发布格式，不是权宜之计。
	reAnimeEp = regexp.MustCompile(`\s-\s+(\d{1,4})\s*(?:[\(\[])`)
)

type span struct{ start, end int }

func findSpan(re *regexp.Regexp, s string) (span, []string, bool) {
	m := re.FindStringSubmatchIndex(s)
	if m == nil {
		return span{}, nil, false
	}
	subs := make([]string, len(m)/2)
	for i := 0; i < len(m)/2; i++ {
		if m[2*i] >= 0 {
			subs[i] = s[m[2*i]:m[2*i+1]]
		}
	}
	return span{m[0], m[1]}, subs, true
}

// mask 把若干区间替换成等长空格。
//
// 必须等长：后面的 token 偏移要能对回原串去切标题。
// 一个 rune 的所有字节都被空格顶掉，结果仍是合法 UTF-8。
func mask(s string, spans []span) string {
	if len(spans) == 0 {
		return s
	}
	b := []byte(s)
	for _, sp := range spans {
		for i := sp.start; i < sp.end && i < len(b); i++ {
			b[i] = ' '
		}
	}
	return string(b)
}

// zhNum 把「二」「十二」「二十一」转成整数；纯数字直接解析。
func zhNum(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	digits := map[rune]int{'一': 1, '二': 2, '三': 3, '四': 4, '五': 5,
		'六': 6, '七': 7, '八': 8, '九': 9}
	rs := []rune(s)
	total, cur := 0, 0
	for _, r := range rs {
		if r == '十' {
			if cur == 0 {
				cur = 1
			}
			total += cur * 10
			cur = 0
			continue
		}
		d, ok := digits[r]
		if !ok {
			return 0, false
		}
		cur = d
	}
	total += cur
	if total == 0 {
		return 0, false
	}
	return total, true
}

// ---------------------------------------------------------------- 主体

type parseCore struct {
	name ParsedName

	// onlyYear 表示整个名字（除掉扩展名与标签）只剩一个四位数字。
	onlyYear  bool
	yearToken string

	hasEp     bool
	hasSeason bool

	bracketYear    int
	bracketYearSet bool
}

// ParseFileName 解析一个文件名（含扩展名，不含目录）。
func ParseFileName(name string) ParseResult {
	return finalize(parseName(name))
}

func parseName(name string) parseCore {
	var c parseCore

	base := name
	// 只在扩展名确实是视频时才剥。
	//
	// 不能用 path.Ext 一把梭：`Blade.Runner.2049.2017.mkv` 没问题，但
	// `Movie.2019` 这种（少见但存在）会被剥成 `Movie` 并把年份当扩展名吞掉。
	if ext := path.Ext(name); IsVideoExt(ext) {
		base = name[:len(name)-len(ext)]
	}
	if strings.TrimSpace(base) == "" {
		return c
	}

	w := protectDots(base)
	var masked []span
	markerStart := -1 // 第一个季集/季包标记的起点，标题必须切在它前面

	note := func(sp span) {
		masked = append(masked, sp)
		if markerStart < 0 || sp.start < markerStart {
			markerStart = sp.start
		}
	}

	// ① 多季包 S01-S03：先于单季判定，否则只会拿到 S01。
	if sp, subs, ok := findSpan(reSRange, w); ok {
		lo, _ := strconv.Atoi(subs[1])
		hi, _ := strconv.Atoi(subs[2])
		if hi >= lo && hi-lo < 50 {
			c.hasSeason = true
			c.name.Season = intp(lo)
			c.name.SeasonCount = hi - lo + 1
			c.name.SeasonPack = true
			note(sp)
			w = mask(w, []span{sp})
		}
	}

	// ② S01E02（含 E01E02 / E01-E03）
	if sp, subs, ok := findSpan(reSE, w); ok {
		season, _ := strconv.Atoi(subs[1])
		ep, _ := strconv.Atoi(subs[2])
		c.hasEp, c.hasSeason = true, true
		c.name.Season = intp(season)
		c.name.Episode = intp(ep)
		if c.name.SeasonCount == 0 {
			c.name.SeasonCount = 1
		}
		if tail := strings.TrimSpace(subs[3]); tail != "" {
			tail = strings.TrimLeft(tail, " ._-")
			tail = strings.TrimPrefix(strings.ToLower(tail), "e")
			tail = strings.TrimPrefix(tail, "ep")
			if n, err := strconv.Atoi(tail); err == nil && n > ep {
				c.name.EpisodeEnd = n
			}
		}
		note(sp)
		w = mask(w, []span{sp})
	}

	// ③ 1x02
	if c.name.Episode == nil {
		if sp, subs, ok := findSpan(reX, w); ok {
			season, _ := strconv.Atoi(subs[1])
			ep, _ := strconv.Atoi(subs[2])
			c.hasEp, c.hasSeason = true, true
			c.name.Season = intp(season)
			c.name.Episode = intp(ep)
			if c.name.SeasonCount == 0 {
				c.name.SeasonCount = 1
			}
			note(sp)
			w = mask(w, []span{sp})
		}
	}

	// ④ 中文集号：第2集 / 第1-10集
	if c.name.Episode == nil {
		if sp, subs, ok := findSpan(reZhEpRange, w); ok {
			lo, _ := strconv.Atoi(subs[1])
			hi, _ := strconv.Atoi(subs[2])
			c.hasEp = true
			c.name.Episode = intp(lo)
			c.name.EpisodeEnd = hi
			c.name.SeasonPack = true
			note(sp)
			w = mask(w, []span{sp})
		} else if sp, subs, ok := findSpan(reZhEp, w); ok {
			if n, ok2 := zhNum(subs[1]); ok2 {
				c.hasEp = true
				c.name.Episode = intp(n)
				note(sp)
				w = mask(w, []span{sp})
			}
		}
	}

	// ⑤ 中文季号：第二季
	if c.name.Season == nil {
		if sp, subs, ok := findSpan(reZhSeason, w); ok {
			if n, ok2 := zhNum(subs[1]); ok2 {
				c.hasSeason = true
				c.name.Season = intp(n)
				c.name.SeasonCount = 1
				note(sp)
				w = mask(w, []span{sp})
			}
		}
	}

	// ⑥ 只有季：S01 / Season 2
	if c.name.Season == nil {
		if sp, subs, ok := findSpan(reS, w); ok {
			n, _ := strconv.Atoi(subs[1])
			if n > 0 && n < 100 {
				c.hasSeason = true
				c.name.Season = intp(n)
				c.name.SeasonCount = 1
				if c.name.Episode == nil {
					c.name.SeasonPack = true
				}
				note(sp)
				w = mask(w, []span{sp})
			}
		}
	}

	// ⑦ 动漫绝对集号
	if c.name.Episode == nil {
		if sp, subs, ok := findSpan(reAnimeEp, w); ok {
			n, _ := strconv.Atoi(subs[1])
			c.hasEp = true
			c.name.Episode = intp(n)
			note(sp)
			w = mask(w, []span{sp})
		}
	}

	// ⑧ 全39集 / 共12话
	if sp, subs, ok := findSpan(reZhTotal, w); ok {
		n, _ := strconv.Atoi(subs[1])
		if n > 0 {
			c.name.TotalEpisodes = n
			c.name.SeasonPack = true
			note(sp)
			w = mask(w, []span{sp})
		}
	}

	// ⑨ 尾部 -GROUP
	//
	// 只认「结尾的连字符 + 短串」，且前缀必须是技术标记或年份。
	// 没有这个前置条件，`Spider-Man.mkv` 会把 Man 当压制组，标题变成 Spider。
	//
	// 判定放在分词之后而不是对原串做正则：`WEB-DL`、`DTS-HD`、`Blu-Ray` 这些
	// 复合词里也有连字符，先整词查表就能把它们排除掉，而正则做不到。
	toks := tokenize(w)
	groupIdx := -1
	if g, ok := splitGroupToken(toks); ok {
		c.name.ReleaseGroup = g
	}

	// ⑩ 括号组：开头的前缀（[SubsPlease]）与结尾的后缀（[YTS.MX]）
	// 已经有 `-GROUP` 时不再看括号，避免把两个不同的组名混成一个。
	if c.name.ReleaseGroup == "" {
		for _, i := range []int{0, len(toks) - 1} {
			if i < 0 || i >= len(toks) {
				continue
			}
			t := toks[i]
			if t.bracket == 0 {
				continue
			}
			if _, ok := yearLike(t.text); ok {
				continue // (2019) 是年份，不是组名
			}
			if k, _ := classifyToken(t); k != kindNone {
				continue
			}
			if i == len(toks)-1 && reCRC.MatchString(t.key) {
				continue // 8 位十六进制是校验码，不是组名
			}
			groupIdx = i
			c.name.ReleaseGroup = t.text
			break
		}
	}

	// ⑪ 标题边界
	bound := len(toks)
	for i, t := range toks {
		if k, _ := classifyToken(t); k.strong() {
			bound = i
			break
		}
	}

	// ⑫ 年份：取边界前**最后一个**四位数字。
	//
	// 取最后一个而不是第一个，是《1917》《2012》这类片名的唯一解：
	// 片名里的数字在前，上映年紧随其后。反过来取第一个会把 1917 当成年份，
	// 再去搜「1917 年」的片子，命中列表全错。
	yearIdx := -1
	var year int
	for i := 0; i < bound; i++ {
		if n, ok := yearLike(toks[i].text); ok {
			yearIdx, year = i, n
		}
	}
	if yearIdx < 0 {
		for i := bound; i < len(toks); i++ {
			if n, ok := yearLike(toks[i].text); ok {
				yearIdx, year = i, n
				break
			}
		}
	} else {
		for i := bound; i < len(toks); i++ {
			if n, ok := yearLike(toks[i].text); ok && n != year {
				c.bracketYear, c.bracketYearSet = n, true
				break
			}
		}
	}

	// ⑬ 标题
	//
	// 两个上界：技术标记（bound）与季集标记（markerStart）。
	// 后者的必要性在 `Show - S01E01 - The Pilot` 这类名字上：季集号之后是
	// **单集标题**，不是剧名，吃进标题会让搜索查一个不存在的作品。
	limit := bound
	if markerStart >= 0 {
		for i, t := range toks {
			if t.start >= markerStart {
				if i < limit {
					limit = i
				}
				break
			}
		}
	}

	var parts []string
	skippedYear := false
	for i := 0; i < limit; i++ {
		if i == groupIdx {
			continue
		}
		t := toks[i]
		if i == yearIdx {
			skippedYear = true
			continue
		}
		k, disp := classifyToken(t)
		if k == kindTag {
			if _, hard := hardTags[t.key]; hard {
				continue
			}
			// 软标签只在年份之后剔：`The.Extended.Family.2019` 里的 Extended 是片名。
			if _, soft := softTags[t.key]; soft && yearIdx >= 0 && i > yearIdx {
				c.name.Tags = append(c.name.Tags, disp)
				continue
			}
		}
		parts = append(parts, t.text)
	}

	title := joinTitle(parts)

	// ⑭ 只有一个四位数字：《2012》《1917》。
	//
	// 这时**不给年份**。给了年份，TMDB 的 year 过滤会把正确结果直接筛掉；
	// 不给，搜「1917」反而能命中。这一条是实测踩出来的，不是保守。
	if title == "" && skippedYear && yearIdx >= 0 {
		c.onlyYear = true
		c.yearToken = toks[yearIdx].text
		c.name.Title = c.yearToken
	} else {
		c.name.Title = title
		if yearIdx >= 0 && !c.onlyYear {
			c.name.Year = intp(year)
		}
	}

	c.name.LooksSerial = c.hasEp || c.hasSeason || c.name.SeasonPack

	// ⑮ 技术标记（扫全表，不受标题边界限制）
	for _, t := range toks {
		k, disp := classifyToken(t)
		switch k {
		case kindRes:
			if c.name.Resolution == "" {
				c.name.Resolution = disp
			}
		case kindSource:
			// 取最后一个：`UHD.BluRay` 里 BluRay 更具体，UHD 只是画质前缀。
			c.name.Source = disp
		case kindVideo:
			if c.name.VideoCodec == "" {
				c.name.VideoCodec = disp
			}
		case kindAudio:
			if c.name.AudioCodec == "" {
				c.name.AudioCodec = disp
				c.name.AudioChannels = channelsOf(t.key)
			}
			// Atmos 是叠加在 TrueHD/DD+ 之上的对象音频元数据，不是独立音轨：
			// `DDP5.1.Atmos` 里两支都该被记住，否则 31 屏的音频行会少说一句。
			if disp == "Atmos" {
				c.name.Tags = appendUnique(c.name.Tags, disp)
			}
		case kindTag:
			if disp != "" {
				c.name.Tags = appendUnique(c.name.Tags, disp)
			}
		}
	}

	return c
}

var reCRC = regexp.MustCompile(`^[0-9a-f]{8}$`)

var reGroupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,23}$`)

// splitGroupToken 找结尾 token 里的 `-GROUP`，并把那个 token 就地截成前缀。
//
// 就地截断而不是"记个下标跳过"，是因为组名常常直接粘在技术标记上：
// `DTS5.1-PandaQT` 是一个 token，跳过它就等于连音轨信息一起丢了。
//
// 三个前置条件缺一不可：
//  1. 整词不能已经是已知标记——否则 `WEB-DL` 会被拆成 `WEB` + `DL`；
//  2. 前缀必须能独立定性为技术标记或年份——否则 `Spider-Man` 会被拆成 `Spider`；
//  3. 后缀长度像组名（2~24）。
//
// 这三条都是为了同一个原则：**宁可解析不出组名，也不能把片名切坏**。
// 组名只影响 28 屏「已识别」行的显示，片名错了就是搜错片。
func splitGroupToken(toks []token) (string, bool) {
	if len(toks) == 0 {
		return "", false
	}
	last := toks[len(toks)-1]
	i := strings.LastIndex(last.text, "-")
	if i <= 0 || i == len(last.text)-1 {
		return "", false
	}
	if k, _ := classifyToken(last); k != kindNone {
		return "", false
	}
	prefix, suffix := last.text[:i], last.text[i+1:]
	if !reGroupName.MatchString(suffix) {
		return "", false
	}
	if _, ok := yearLike(prefix); !ok {
		pk, _ := classifyToken(token{text: prefix, key: tokenKey(prefix)})
		if !pk.strong() {
			return "", false
		}
	}
	toks[len(toks)-1] = token{text: prefix, key: tokenKey(prefix), bracket: last.bracket, start: last.start}
	return suffix, true
}

func channelsOf(key string) string {
	i := strings.IndexAny(key, "0123456789")
	if i < 0 {
		return ""
	}
	rest := key[i:]
	if strings.Contains(rest, ".") {
		return rest
	}
	return ""
}

// joinTitle 把标题 token 拼成一条可搜索的字符串。
//
// 分隔符统一成一个空格：TMDB 的搜索对空格最友好，点号和下划线会破坏分词
// （R6 §7 那三条 0 命中就是这么来的）。连字符保留在 token 内部，
// `Spider-Man` 不会被拆成两个词。
func joinTitle(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "-" || p == "|" || p == "~" || p == "+" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p)
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	return strings.Trim(s, " -._|~+[](){}")
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// finalize 把解析出的零件变成对外结果。
func finalize(c parseCore) ParseResult {
	res := ParseResult{Parsed: c.name, TitleFrom: "file"}
	if c.name.Title != "" {
		res.Parsed.Title = c.name.Title
	}

	switch {
	case c.onlyYear:
		res.Code = ParseYearOrTitleAmbiguous
		res.Params = map[string]any{"token": c.yearToken}
		// 年份留空，理由见 parseName ⑭。
		res.Parsed.Year = nil

	case c.name.Title == "" && (c.hasEp || c.hasSeason):
		res.Code = ParseEpisodeWithoutSeries
		p := map[string]any{}
		if c.name.Season != nil {
			p["season"] = *c.name.Season
		}
		if c.name.Episode != nil {
			p["episode"] = *c.name.Episode
		}
		// candidates 由 API 层填（要查库内同季作品数），这里先给 0。
		// 不省略这个键：客户端文案表里它是个变量，缺键会渲染出空字符串。
		p["candidates"] = 0
		res.Params = p

	case c.name.Title == "":
		res.Code = ParseEmpty
		res.Params = map[string]any{}

	// 有季集号时年份不是必需品：`Breaking.Bad.S02E04` 没有年份也足以定位。
	// 只有"光有片名"才是真缺信息——TMDB 上同名作品一堆，没有年份排不开。
	case c.name.Year == nil && !c.hasEp && !c.hasSeason:
		res.Code = ParseTitleOnly
		res.Params = map[string]any{}

	default:
		res.Code = ParseOK
		res.Params = map[string]any{}
	}

	if c.bracketYearSet && (res.Parsed.Year == nil || *res.Parsed.Year != c.bracketYear) {
		p := map[string]any{"title": res.Parsed.Title, "bracketYear": c.bracketYear}
		if res.Parsed.Year != nil {
			p["year"] = *res.Parsed.Year
		} else {
			p["year"] = nil
		}
		res.Warnings = append(res.Warnings, ParseWarning{Code: ParseYearIsReleaseTag, Params: p})
	}

	if c.name.SeasonPack && (c.name.TotalEpisodes > 0 || c.name.SeasonCount > 1 || c.name.EpisodeEnd > 0) {
		ep := c.name.TotalEpisodes
		if ep == 0 && c.name.EpisodeEnd > 0 {
			ep = c.name.EpisodeEnd
		}
		sc := c.name.SeasonCount
		if sc == 0 {
			sc = 1
		}
		res.Warnings = append(res.Warnings, ParseWarning{
			Code:   ParseMultiSeasonPack,
			Params: map[string]any{"seasonCount": sc, "episodeCount": ep},
		})
	}

	return res
}

// ---------------------------------------------------------------- 目录补标题

var (
	// `Season 2` / `S02` / `Series 3` / `第2季` / `Specials`
	reSeasonDirNum = regexp.MustCompile(`(?i)^(?:season|series|s)[\s._\-]*(\d{1,2})$|^第\s*([0-9一二三四五六七八九十]{1,3})\s*[季部]$`)
	reSeasonDir    = regexp.MustCompile(`(?i)^(?:season|series|s)[\s._\-]*\d{1,2}$|^第\s*[0-9一二三四五六七八九十]{1,3}\s*[季部]$|^specials?$`)
	// 分类目录：它们只说明"这是什么"，不说明"这是哪部"。
	reNoiseDir = regexp.MustCompile(`(?i)^(?:extras?|featurettes?|samples?|bdmv|certificate|video_ts|movies?|films?|tv|tvshows?|series|anime|documentar(?:y|ies)|4k|2160p|1080p|uhd|remux|collection)$|^(?:电影|影片|电视剧|剧集|动漫|动画|纪录片|综艺|合集|未整理|媒体|影视)$`)
	// `04.mkv` 这种只有集号的文件名。
	reBareNumber = regexp.MustCompile(`^\d{1,3}$`)
)

// seasonFromDir 从「Season 2」「第2季」这样的目录名里取季号。
func seasonFromDir(seg string) (int, bool) {
	m := reSeasonDirNum.FindStringSubmatch(seg)
	if m == nil {
		return 0, false
	}
	tok := m[1]
	if tok == "" {
		tok = m[2]
	}
	return zhNum(tok)
}

// ParsePath 用整条库相对路径解析，文件名拿不到标题时向上找目录。
//
// 这一步不是锦上添花。剧集库的常见结构是
//
//	Breaking Bad/Season 2/S02E04.mkv
//	Breaking Bad/Season 2/04.mkv
//
// 第一种文件名里有剧名，第二种连剧名和季号都在目录里。只看文件名，
// 这类库的解析成功率会掉到接近 0，而它们在剧集库里是多数。
func ParsePath(relPath string) ParseResult {
	rel := strings.ReplaceAll(relPath, `\`, "/")
	c := parseName(path.Base(rel))

	// `Season 2/04.mkv` 里的 `04` 不是片名，是集号。
	// 但只有真的找到季目录才算——`04.mkv` 单独扔在库里，它就是片名。
	bareEp := 0
	if c.name.Episode == nil {
		if n, ok := bareNumber(path.Base(rel)); ok {
			bareEp = n
			c.name.Title = ""
		}
	}
	if c.name.Title != "" {
		return finalize(c)
	}

	if c.name.Title != "" {
		return finalize(c)
	}

	dir := path.Dir(rel)
	season := 0
	titleFromDir := false
	for depth := 0; depth < 3; depth++ {
		if dir == "." || dir == "/" || dir == "" {
			break
		}
		seg := path.Base(dir)
		dir = path.Dir(dir)
		if IsJunk(seg) {
			continue
		}
		if n, ok := seasonFromDir(seg); ok {
			if season == 0 {
				season = n
			}
			continue
		}
		if reSeasonDir.MatchString(seg) || reNoiseDir.MatchString(seg) {
			continue
		}
		dc := parseName(seg)
		if dc.name.Title == "" || dc.onlyYear {
			continue
		}
		c.name.Title = dc.name.Title
		if c.name.Year == nil {
			c.name.Year = dc.name.Year
		}
		c.name.LooksSerial = true
		titleFromDir = true
		break
	}

	if season > 0 {
		if c.name.Season == nil {
			c.name.Season = intp(season)
			c.hasSeason = true
			if c.name.SeasonCount == 0 {
				c.name.SeasonCount = 1
			}
		}
		if c.name.Episode == nil && bareEp > 0 {
			c.name.Episode = intp(bareEp)
			c.hasEp = true
		}
	}

	if c.name.Title == "" {
		// 目录里也没有名字：把裸数字还给文件名，至少别把片名清空。
		if bareEp > 0 {
			c.name.Title = strconv.Itoa(bareEp)
			c.name.Episode = nil
			c.hasEp = false
		}
		return finalize(c)
	}

	res := finalize(c)
	if titleFromDir {
		res.TitleFrom = "dir"
	}
	return res
}

func bareNumber(name string) (int, bool) {
	if ext := path.Ext(name); IsVideoExt(ext) {
		name = name[:len(name)-len(ext)]
	}
	if !reBareNumber.MatchString(name) {
		return 0, false
	}
	n, err := strconv.Atoi(name)
	if err != nil || n == 0 {
		return 0, false
	}
	return n, true
}

func intp(n int) *int { return &n }
