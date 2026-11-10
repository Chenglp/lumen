package domain

// 候选打分 —— 对应技术框架 §4.2 与契约 §1.2「score 的产生规则」。
//
// 这一层只做一件事：把 TMDB 搜索返回的一堆候选排成序，并给出一个能解释的分数。
// 它不发请求、不读库、不写库——所以它能被表驱动测试完整覆盖，
// 也能在「换一批」之外的地方复用（手动搜索、批量匹配、详情页重排）。
//
// 为什么不能沿用 TMDB 自己的顺序：R6 实测 24 条真实剧名走 zh-CN，hit@1 只有 22/24。
// 两个失败案例的成因不同，但都不是「搜不到」，而是「排序不动脑子」：
//
//   - 查询「信号」，top1 是综艺《心动的信号》（正确项在 rank 2）。
//     两字的短标题被长标题的子串命中，而 TMDB 按 popularity 把综艺顶了上去。
//   - 查询「昼颜」，top1 的 zh-CN 名是《昼颜：工作日下午3点的恋人们》。
//     拿字符串相等去比，这个正确项会被判成未匹配——译名比俗称长是常态。
//
// 所以分数由四个信号加权而成，每个信号都对应上面某一类失败，没有一个是为了「更精细」。

import (
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// 权重之和必须为 1，且刻意不做「缺失信号后重新归一」。
//
// 重新归一看起来更公平（没年份的片子不该被扣分），实际会把「证据不全」伪装成
// 「证据充分」：一部只有片名、其余什么都不确定的片子会算出 1.0，
// 然后在 28 屏上显示 100%，用户点确认进去发现是另一部。
// 缺一个信号就是少一分证据，那就该少一分，这是这套权重的唯一设计原则。
//
// 直接后果：没有年份的候选最高只能到 0.78，够「推荐」不够「强匹配」——
// 而契约里 STRONG_MATCH 的定义本来就是「片名与年份都吻合」。
const (
	weightTitle    = 0.50 // 主信号：token 覆盖率，不是字符串相等
	weightYear     = 0.22 // 高：短标题靠它区分（《信号》vs《心动的信号》同年在播时）
	weightType     = 0.18 // 高：走错端点等于必然搜不到
	weightLanguage = 0.10 // 中：只做微调，永远不是否决项
)

// 判定阈值。数字不是拍的，是从失败案例倒推出来的，改动前先看 match_test.go。
const (
	// StrongMatchScore 「强匹配」的下限：片名与年份都吻合才可能达到。
	//
	// 0.85 这个位置是算出来的，不是拍的。四个信号满分是 1.0，缺一个是：
	// 缺年份 0.78、类型不符 0.82、没有同文种名字 0.90。
	// 门槛卡在 0.85，意味着「缺年份」和「类型不符」两种都够不到——
	// 而契约里 STRONG_MATCH 的文案就是「片名与年份都吻合」，
	// 让它出现在缺年份的行上，等于文案自己打自己的脸。
	StrongMatchScore = 0.85

	// RecommendMinScore 「推荐」角标的下限。
	// 定在 0.65 的理由：片名子串命中(0.7)+年份错(0) 会算到 0.63，
	// 这种「名字像、年份不对」的候选不该被标成推荐——它正是用户最容易被误导的一类。
	RecommendMinScore = 0.65

	// RecommendMargin 第一名与第二名的最小分差。
	// 打平时不标推荐：角标只能有一个，标错一个等于把用户往错的方向推一次，
	// 而让两个候选并列摆着，用户自己看一眼就分得清。
	RecommendMargin = 0.04

	// OverlapMargin 判定「片名部分重合多部」的分差窗口。
	OverlapMargin = 0.06
)

// MaxCandidates 是契约规定的候选单页硬上限（§1.2）。
//
// 放在这里而不是 API 层，是因为它同时也是「不许翻页」这个决定的一部分：
// R6 实测 24 条真实剧名 hit@5 = 96%，正确项几乎总在前 5；
// 再往后翻只会让全库首扫的请求数翻倍（5 万文件已约 27 小时），换不回排序质量。
// 排序问题只能靠更好的信号解决。
const MaxCandidates = 20

// 匹配阶段才产生的 reason code。解析阶段给不出这两个，因为它们需要候选。
const (
	// CodeStrongMatch 片名与年份都吻合，等用户确认。
	CodeStrongMatch ParseCode = "STRONG_MATCH"
	// CodeTitleYearOverlap 片名部分重合多部，选不出唯一解。
	CodeTitleYearOverlap ParseCode = "TITLE_YEAR_OVERLAP"
)

// Candidate 是搜索结果里参与打分的字段。
//
// 刻意只留这几个：TMDB 的 search 响应有二十多个字段，但只把能影响排序的收进来。
// 收得越多，将来换数据源（或 TMDB 改字段）时要动的地方越多，而排序不需要它们。
type Candidate struct {
	TMDBID           int
	MediaType        string // movie | tv
	Title            string // 按库语言取的标题；无该语言译名时 TMDB 回退成原名
	OriginalTitle    string
	Year             *int
	OriginalLanguage string // zh / ja / ko / en…，只用于解释，不参与打分
	Popularity       float64
}

// MatchQuery 是打分的全部输入。
//
// 传 ParseResult 而不是 ParsedName：候选为空时要把解析阶段的 code 原样还回去
// （28 屏副标那时显示的是「文件名里没有剧名」，不是「未识别」），
// 少传一个 Code 就得让调用方自己再拼一遍，拼错了没人发现。
type MatchQuery struct {
	Result  ParseResult
	LibKind string // movie | tv | 其它（未知）
	Lang    string // 库设置的语言，如 zh-CN；本层只用于日志追溯
}

// Breakdown 是分数的分解。
//
// 之所以要留分解而不是只留 Score：`recommended` 和 reason code 都要能解释。
// 「为什么这部被标了推荐」必须能回答成「片名全中、年份对、类型对」，
// 而不是「它 87 分」——后者在排障时等于没说。
type Breakdown struct {
	TitleSim float64
	YearSim  float64
	TypeSim  float64
	LangSim  float64
	Score    float64
}

// Scored 是排好序的候选，字段对应契约 `CandidateItem`。
type Scored struct {
	Candidate
	Breakdown

	Recommended bool
	// RecommendBasis 留空（契约里是 null）。
	//
	// 契约的三种依据 SAME_SEASON / SAME_SERIES / EPISODE_TITLE_MATCH 都需要库内上下文
	// （同季已入库的作品、已知剧集名），本层拿不到。用片名 token 命中硬凑一个
	// 不属于这三类的值，等于往契约里塞一个客户端文案表里没有的枚举。
	RecommendBasis string
}

// Score 给单个候选打分。
//
// 纯函数，同样的输入永远得到同样的分数——「换一批」换的是排序种子，不是分数，
// 否则用户点两次就会看到同一部片子显示两个匹配度。
func Score(q MatchQuery, c Candidate) Breakdown {
	var b Breakdown
	b.TitleSim = titleSim(q.Result.Parsed.Title, c)
	b.YearSim = yearSim(q.Result.Parsed.Year, c.Year)
	b.TypeSim = typeSim(q, c.MediaType)
	b.LangSim = langSim(q.Result.Parsed.Title, c)

	b.Score = round3(
		weightTitle*b.TitleSim +
			weightYear*b.YearSim +
			weightType*b.TypeSim +
			weightLanguage*b.LangSim,
	)
	return b
}

// Rank 打分、去重、排序、截断，并标出唯一的推荐项。
//
// 排序必须完全确定：同一个文件重复匹配要给出同一个顺序。
// 不这样做的代价很具体——用户点「换一批」看到顺序变了，会以为服务在乱猜，
// 然后不再相信右栏任何一个候选。
func Rank(q MatchQuery, cands []Candidate) []Scored {
	out := make([]Scored, 0, len(cands))
	seen := make(map[string]int, len(cands))

	for _, c := range cands {
		if c.TMDBID <= 0 {
			continue
		}
		if strings.TrimSpace(c.Title) == "" && strings.TrimSpace(c.OriginalTitle) == "" {
			continue
		}
		b := Score(q, c)
		key := c.MediaType + ":" + strconv.Itoa(c.TMDBID)
		if i, ok := seen[key]; ok {
			// 同一个 tmdb_id 在 multi 搜索里可能以 movie/tv 各出现一次（同名电影与剧），
			// 所以键里带 mediaType；真正重复的只保留分高的那条。
			if b.Score > out[i].Breakdown.Score {
				out[i] = Scored{Candidate: c, Breakdown: b}
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, Scored{Candidate: c, Breakdown: b})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if si, sj := out[i].Breakdown.Score, out[j].Breakdown.Score; si != sj {
			return si > sj
		}
		// popularity 只在同分时决定次序。
		//
		// 这条不是风格问题：R6 的两个失败案例全是 popularity 造成的。
		// 它是 TMDB 全站的热度，与「用户硬盘上这个文件是不是它」没有因果关系。
		if pi, pj := out[i].Popularity, out[j].Popularity; pi != pj {
			return pi > pj
		}
		return out[i].TMDBID < out[j].TMDBID
	})

	if len(out) > MaxCandidates {
		out = out[:MaxCandidates]
	}
	markRecommended(out)
	return out
}

// markRecommended 标出唯一的推荐项。
func markRecommended(out []Scored) {
	if len(out) == 0 {
		return
	}
	top := out[0].Breakdown.Score
	if top < RecommendMinScore {
		return
	}
	if len(out) > 1 && top-out[1].Breakdown.Score < RecommendMargin {
		return
	}
	out[0].Recommended = true
}

// Confidence 给出 unmatched 行的匹配度（契约 0–1，0 表示未识别）。
//
// 候选为空时必须返回 0 而不是「差一点点的分数」：契约规定 0 渲染成「未识别」，
// 客户端据此区分「没搜到」和「搜到了但不确定」，两者要引导的操作完全不同。
func Confidence(ranked []Scored) float64 {
	if len(ranked) == 0 {
		return 0
	}
	return ranked[0].Breakdown.Score
}

// Reason 给出这一行该显示哪种副标。
//
// 只在「有候选」时才可能升级成 STRONG_MATCH / TITLE_YEAR_OVERLAP；
// 没有候选就把解析阶段的 code 原样还回去——那时用户该看的是「文件名里没有剧名」，
// 这句比「未识别」有用得多。
func Reason(q MatchQuery, ranked []Scored) (ParseCode, map[string]any) {
	if len(ranked) == 0 {
		return q.Result.Code, q.Result.Params
	}
	top := ranked[0]
	py := q.Result.Parsed.Year

	// 强匹配的三个条件缺一不可，其中「年份吻合」是契约里写明的定义，
	// 而「片名几乎完全命中」是防止 `信号` 这种子串命中被当成强匹配：
	// 子串命中 + 年份恰好的分数也能过 0.80，但那正是《心动的信号》的情形。
	if top.Breakdown.Score >= StrongMatchScore &&
		py != nil && top.Year != nil && absInt(*py-*top.Year) <= 1 &&
		top.Breakdown.TitleSim >= 0.85 {
		return CodeStrongMatch, map[string]any{
			"title": top.Title,
			"year":  *top.Year,
			"score": top.Breakdown.Score,
		}
	}

	if len(ranked) > 1 {
		s := ranked[1]
		near := top.Breakdown.Score-s.Breakdown.Score <= OverlapMargin
		partial := top.Breakdown.TitleSim >= 0.30 && top.Breakdown.TitleSim < 0.95
		if near && partial && s.Breakdown.TitleSim >= 0.30 {
			p := map[string]any{
				"guessedTitle": q.Result.Parsed.Title,
				"matchedTitle": top.Title,
				"overlap":      round2(top.Breakdown.TitleSim),
			}
			if py != nil {
				p["guessedYear"] = *py
			} else {
				p["guessedYear"] = nil
			}
			if top.Year != nil {
				p["matchedYear"] = *top.Year
			} else {
				p["matchedYear"] = nil
			}
			return CodeTitleYearOverlap, p
		}
	}

	return q.Result.Code, q.Result.Params
}

// ---------------------------------------------------------------- 四个信号

// titleSim 计算「候选标题覆盖了查询标题的多少」。
//
// 分母只有查询侧的权重，候选标题再长也不进分母——这是契约那条
// 「切片长度差异不得作为降权理由」的实现方式。反过来，候选短于查询时会被扣分，
// 那不是长度歧视：候选没覆盖查询里的词，就是没覆盖。
func titleSim(query string, c Candidate) float64 {
	units := titleUnits(query)
	if len(units) == 0 {
		return 0
	}
	var best float64
	for _, name := range []string{c.Title, c.OriginalTitle} {
		if strings.TrimSpace(name) == "" {
			continue
		}
		idx := newTitleIndex(name)
		var got, total float64
		for _, u := range units {
			total += u.w
			got += u.w * idx.match(u.text)
		}
		if total > 0 {
			if s := got / total; s > best {
				best = s
			}
		}
	}
	return round3(best)
}

// yearSim 计算年份吻合度。
//
// ±1 年给 0.55 而不是 1.0：跨年上映 / 跨时区首映会让上映年差一岁，
// 但差一岁也确实比同年弱，这一点该体现出来。
// ≥2 年直接 0：片名可能撞车，年份是它们之间唯一的分界线。
func yearSim(q, c *int) float64 {
	if q == nil || c == nil {
		return 0
	}
	switch d := absInt(*q - *c); {
	case d == 0:
		return 1
	case d == 1:
		return 0.55
	case d == 2:
		return 0.15
	default:
		return 0
	}
}

// typeSim 计算媒体类型是否一致。
//
// 期望类型优先看库：用户把目录加成「电影库」，里面的文件就是电影，
// 哪怕文件名带了 S01E01（花絮、彩蛋、命名事故都常见）。
// 库类型未知时才看文件名里的季集痕迹。
func typeSim(q MatchQuery, mediaType string) float64 {
	want := expectedType(q)
	if want == "" || mediaType == "" {
		return 0
	}
	if mediaType == want {
		return 1
	}
	return 0
}

func expectedType(q MatchQuery) string {
	switch q.LibKind {
	case "movie", "tv":
		return q.LibKind
	}
	p := q.Result.Parsed
	if p.Season != nil || p.Episode != nil || p.SeasonPack || p.LooksSerial {
		return "tv"
	}
	return ""
}

// langSim 判断候选有没有「与文件名同文种」的名字。
//
// 注意这不是「语言偏好」：中文文件名配英文原名（美剧）不作扣分处理，
// 因为那是常态。它真正的两个作用：
//
//   - 候选只有中文名 + 文件名是中文 → 1.0，正常；
//   - 候选只有英文名 + 文件名是中文（说明 TMDB 上根本没有中文译名）→ 0.45，
//     这是个弱信号：一部连中文译名都没有的作品，被中文文件名指到的概率更低。
//
// 顺带兜住繁简/异体这类「同文种但字形不同」的情况——那种情况下 token 覆盖率会掉，
// 但文种一致，不该掉到底。
func langSim(query string, c Candidate) float64 {
	qs := scriptOf(query)
	if qs == scriptNone {
		return 0
	}
	if scriptOf(c.Title) == qs || scriptOf(c.OriginalTitle) == qs {
		return 1
	}
	return 0.45
}

// ---------------------------------------------------------------- token

type titleUnit struct {
	text string
	w    float64
}

// enStopwords 只丢冠词。
//
// 清单必须短：`de` / `la` / `the` 在别的语言里是实词（《La La Land》），
// 丢错等于把片名的一部分删掉。冠词丢掉是安全的，因为两头都会丢，不影响相对次序。
var enStopwords = map[string]struct{}{"the": {}, "a": {}, "an": {}}

// titleUnits 把标题切成用于比对的单元。
//
// 切分只发生在**分隔符**处（空格、标点、括号），不发生在字类边界：
// 「海边电影院2」整串是一个单元，于是查询「海边电影院」只能拿到子串命中（0.7）——
// 如果按字类切，`2` 会被拆走，剩下的「海边电影院」就与正确片名精确相等，
// 续集把正传顶掉，而且不报错。
//
// 中日韩文没有词边界，所以整段连续的文字算一个单元。这带来一个正好的性质：
// 《昼颜》在《昼颜：工作日下午3点的恋人们》里是冒号界定的独立单元（精确命中 1.0），
// 而《信号》在《心动的信号》里只是单段文字的一部分（子串命中 0.7）。
// 两者的差别就是打分能翻盘的全部依据。
func titleUnits(s string) []titleUnit {
	s = stripMarks(s)
	var (
		out  []titleUnit
		buf  []rune
		seen = map[string]struct{}{}
	)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		text := string(buf)
		buf = buf[:0]
		if isLatinWord(text) {
			if _, drop := enStopwords[text]; drop {
				return
			}
		}
		if _, dup := seen[text]; dup {
			return
		}
		seen[text] = struct{}{}
		out = append(out, titleUnit{text: text, w: float64(len([]rune(text)))})
	}
	for _, r := range s {
		if !isWordRune(r) {
			flush()
			continue
		}
		buf = append(buf, r)
	}
	flush()
	return out
}

// isWordRune 判断一个字符是否属于片名正文。
//
// 汉字/假名/谚文、字母、数字都算；其余（空格、冒号、括号、连字符、中点）一律当分隔符。
func isWordRune(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hangul, r):
		return true
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isLatinWord 判断单元是不是纯拉丁字母（用来决定要不要查停用词表）。
func isLatinWord(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) {
			return false
		}
	}
	return s != ""
}

// titleIndex 是候选标题的比对索引：单元集合 + 去分隔符的整串。
type titleIndex struct {
	set     map[string]struct{}
	compact string
}

func newTitleIndex(s string) titleIndex {
	units := titleUnits(s)
	idx := titleIndex{set: make(map[string]struct{}, len(units))}
	var sb strings.Builder
	for _, u := range units {
		idx.set[u.text] = struct{}{}
		sb.WriteString(u.text)
	}
	idx.compact = sb.String()
	return idx
}

// match 返回一个查询单元在候选标题里的命中强度。
//
// 0.7 这个中间档是整套打分的支点：
// 《信号》对《心动的信号》是子串命中，对《信号》是精确命中，
// 其它一切相同的情况下两者差 0.15 分（0.5 × 0.3），足以把正确项顶到前面，
// 又不足以让一个精确命中但年份离谱的候选压过它——那正是年份那一档的职责。
func (ti titleIndex) match(u string) float64 {
	if _, ok := ti.set[u]; ok {
		return 1
	}
	if ti.compact == "" {
		return 0
	}
	if strings.Contains(ti.compact, u) {
		if isCJKText(u) {
			return 0.7
		}
		return 0.6
	}
	if isCJKText(u) {
		// 字形不同（繁简、异体）时字面覆盖率仍有意义。
		// 封顶 0.5：字重合远不等于片名相同。
		return 0.5 * charOverlap(u, ti.compact)
	}
	return 0
}

func charOverlap(a, b string) float64 {
	bs := make(map[rune]struct{}, len(b))
	for _, r := range b {
		bs[r] = struct{}{}
	}
	as := make(map[rune]struct{}, len(a))
	for _, r := range a {
		as[r] = struct{}{}
	}
	if len(as) == 0 {
		return 0
	}
	var hit int
	for r := range as {
		if _, ok := bs[r]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(as))
}

func isCJKText(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

type script int

const (
	scriptNone script = iota
	scriptHan
	scriptKana
	scriptHangul
	scriptLatin
)

// scriptOf 只看文种，不猜具体语言。
//
// 猜具体语言需要 n-gram 模型，而这里要回答的问题只有「候选有没有同文种的名字」，
// 中文和日文都用汉字，猜错了反而会把中日作品互相判成不同文种。
func scriptOf(s string) script {
	var seenHan, seenLatin bool
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			return scriptKana
		case unicode.Is(unicode.Hangul, r):
			return scriptHangul
		case unicode.Is(unicode.Han, r):
			seenHan = true
		case unicode.IsLetter(r):
			seenLatin = true
		}
	}
	switch {
	case seenHan:
		return scriptHan
	case seenLatin:
		return scriptLatin
	}
	return scriptNone
}

// stripMarks 去掉重音符并把大小写折叠掉。
//
// 欧洲片名在发布名里常被 ASCII 化（Amélie → Amelie），不去掉重音就永远比不上。
// 只处理组合记号（Mn），不动其它字符——NFC 之后中文不受影响。
func stripMarks(s string) string {
	s = norm.NFC.String(s)
	d := norm.NFD.String(s)
	var sb strings.Builder
	sb.Grow(len(d))
	for _, r := range d {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		sb.WriteRune(r)
	}
	return Fold(norm.NFC.String(sb.String()))
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}
