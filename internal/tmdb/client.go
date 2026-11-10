package tmdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Chenglp/lumen/internal/store"
)

// 错误分类。调用方用 errors.Is 判断，不要看字符串。
//
// 分成这五类是因为**处置动作不同**，这也正是契约 §5.2 要求把
// unauthorized 与 network 分开的理由：
//
//	ErrDisabled     → 去设置页填 token
//	ErrUnauthorized → 重新申请 token（或去验证邮箱，见下）
//	ErrNotFound     → 这条 id 在 TMDB 没了，标记后跳过，不是故障
//	ErrRateLimited  → 等下一轮，不要重试
//	ErrUnavailable  → 检查网络或代理配置
var (
	ErrDisabled     = errors.New("tmdb: 未配置凭据")
	ErrUnauthorized = errors.New("tmdb: 凭据无效")
	ErrNotFound     = errors.New("tmdb: 条目不存在")
	ErrRateLimited  = errors.New("tmdb: 被限流")
	ErrUnavailable  = errors.New("tmdb: 服务不可达")
	ErrBadResponse  = errors.New("tmdb: 响应无法解析")
)

// Error 带上足够定位问题、但不足以泄露凭据的信息。
//
// 刻意不带请求 URL：v3 key 是放在 query 里的，把 URL 写进错误日志
// 等于把它写进诊断包。Token 永远不进任何 Error 的字段。
type Error struct {
	Op        string // 端点，如 /search/tv
	Status    int    // HTTP 状态码，0 表示没拿到响应
	Code      int    // TMDB 的 status_code（如 7 = 无效 key，32 = 无法认证）
	Message   string // TMDB 的 status_message
	kind      error
	Retriable bool
}

func (e *Error) Error() string {
	switch {
	case e.Status == 0:
		return fmt.Sprintf("tmdb: %s: %v", e.Op, e.kind)
	case e.Message != "":
		return fmt.Sprintf("tmdb: %s: %d (%d %s)", e.Op, e.Status, e.Code, e.Message)
	default:
		return fmt.Sprintf("tmdb: %s: %d", e.Op, e.Status)
	}
}

func (e *Error) Unwrap() error { return e.kind }

// Reason 把错误翻成契约 §5.2 的 detail.reason 枚举。
//
// 这一层必须放在 tmdb 包里而不是 api 层：判断依据是 TMDB 的状态码，
// 而状态码只在这里存在。api 层若自己猜（比如看 error 字符串里有没有
// "timeout"），换一版 Go 的 net 错误文案就会失效。
func Reason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrDisabled):
		return "unauthorized"
	default:
		return "network"
	}
}

// Cache 是本包需要的存储能力。*store.DB 直接满足它。
type Cache interface {
	GetTMDBCache(ctx context.Context, key string, maxAge time.Duration) (store.TMDBEntry, bool, error)
	PutTMDBCache(ctx context.Context, e store.TMDBEntry) error
}

// Sleeper 让等待可注入，否则限流与退避的测试只能靠真实 sleep。
type Sleeper func(ctx context.Context, d time.Duration) error

func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Client 是并发安全的。
type Client struct {
	http     *http.Client
	baseURL  string
	token    string
	language string
	cache    Cache
	lim      *limiter
	flight   *flightGroup
	sleep    Sleeper
	now      func() time.Time

	detailTTL time.Duration
	searchTTL time.Duration
	startRate float64
}

// Option 配置客户端。
type Option func(*Client)

// WithBaseURL 换根地址（测试用 httptest，或自建缓存代理）。
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithHTTPClient 换底层客户端（配代理时用，见契约 §5.2 的 proxy 设置）。
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithLanguage 设置 language 参数。缓存按语言分隔，换语言后旧行为 miss。
func WithLanguage(lang string) Option { return func(c *Client) { c.language = lang } }

// WithCache 挂上缓存。不挂也能用：每次请求都真的发出去。
func WithCache(cc Cache) Option { return func(c *Client) { c.cache = cc } }

// WithTTL 覆盖默认 TTL。两者都会被夹到 MaxCacheAge 以内。
func WithTTL(detail, search time.Duration) Option {
	return func(c *Client) { c.detailTTL, c.searchTTL = clampAge(detail), clampAge(search) }
}

// WithSleeper 注入等待实现（测试）。
func WithSleeper(s Sleeper) Option { return func(c *Client) { c.sleep = s } }

// WithClock 注入时钟（测试）。
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// WithRate 指定起始速率（测试用，避免每条断言都等令牌）。
//
// 只记参数、不在这里构造：WithClock / WithSleeper 可能排在它后面，
// 提前 newLimiter 会让限流器攥着旧时钟，测试里表现为「注入的 sleep 不被调用」。
func WithRate(perSec float64) Option { return func(c *Client) { c.startRate = perSec } }

// New 构造客户端。
//
// token 同时接受 v3（32 位十六进制 key）与 v4（JWT）。两者认证方式不同：
// v4 走 Authorization 头，v3 只能拼进 query —— 也就是说**用 v3 key 时，
// 凭据会出现在 URL 里**，进而可能进访问日志、诊断包、issue 附件。
// 所以文档推荐 v4（R6 检查单 §1 已按此执行），但我们不能拒绝 v3：
// 用户手上可能只有它。鉴权方式由 token 形状判断，见 authorize。
func New(token string, opts ...Option) *Client {
	c := &Client{
		http:      &http.Client{Timeout: 20 * time.Second},
		baseURL:   DefaultBaseURL,
		token:     strings.TrimSpace(token),
		language:  "zh-CN",
		sleep:     realSleep,
		now:       time.Now,
		detailTTL: DefaultDetailTTL,
		searchTTL: DefaultSearchTTL,
		flight:    newFlightGroup(),
	}
	for _, o := range opts {
		o(c)
	}
	rate := c.startRate
	if rate <= 0 {
		rate = StartRate
	}
	c.lim = newLimiter(rate, c.now, c.sleep)
	return c
}

// Language 返回当前语言，给缓存键与诊断用。
func (c *Client) Language() string { return c.language }

// HasToken 表示是否配置了凭据。未配置时所有请求返回 ErrDisabled，
// **不发任何网络请求**——没 token 还去请求等于把 401 当成网络故障报给用户。
func (c *Client) HasToken() bool { return c.token != "" }

func clampAge(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultDetailTTL
	}
	if d > MaxCacheAge {
		return MaxCacheAge
	}
	return d
}

// isJWT 判断 token 形状。
//
// 只看前缀与两段点：**不做签名验证**。验证签名需要密钥，而这里只需要
// 决定把它放在 header 还是 query；真伪由 TMDB 回答，它的答案才是权威。
func isJWT(t string) bool {
	return strings.HasPrefix(t, "eyJ") && strings.Count(t, ".") == 2
}

// get 发一次 GET 并把 JSON 解到 out。返回值里的 fromCache 只在有缓存时非 nil。
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if c.token == "" {
		return &Error{Op: path, kind: ErrDisabled}
	}

	// 认证方式决定之后才能编码 query：v3 key 要拼进去。
	authByHeader := isJWT(c.token)

	var (
		backoff  = time.Second
		netTries int
		rateHits int
	)
	for {
		if err := c.lim.Wait(ctx); err != nil {
			return err
		}

		// 每轮复制一份 query：url.Values 是 map，重试时复用同一份没有问题，
		// 但把 api_key 写进去之后就不该再被外层看见。
		qq := url.Values{}
		for k, v := range q {
			qq[k] = v
		}
		if !authByHeader {
			qq.Set("api_key", c.token)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			c.baseURL+path+"?"+qq.Encode(), nil)
		if err != nil {
			return &Error{Op: path, kind: ErrBadResponse}
		}
		req.Header.Set("Accept", "application/json")
		if authByHeader {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			netTries++
			if netTries > maxNetworkRetries {
				return &Error{Op: path, kind: ErrUnavailable, Retriable: true}
			}
			if err := c.sleep(ctx, jitter(backoff, c.lim.rnd)); err != nil {
				return err
			}
			backoff = nextBackoff(backoff)
			continue
		}

		body, readErr := readLimited(resp.Body)
		resp.Body.Close()
		if errors.Is(readErr, errTooLarge) {
			// 确定性的失败，重试没有意义：body 大小不会因为重试而变小，
			// 只会白等三次退避（1+2+4 秒）并再下三次 8MB。
			return &Error{Op: path, Status: resp.StatusCode, kind: ErrBadResponse}
		}
		if readErr != nil {
			netTries++
			if netTries > maxNetworkRetries {
				return &Error{Op: path, Status: resp.StatusCode, kind: ErrUnavailable, Retriable: true}
			}
			if err := c.sleep(ctx, jitter(backoff, c.lim.rnd)); err != nil {
				return err
			}
			backoff = nextBackoff(backoff)
			continue
		}

		switch st := resp.StatusCode; {
		case st == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return &Error{Op: path, Status: st, kind: ErrBadResponse}
			}
			// 一次成功不代表速率可以上调——Success 内部按「每分钟最多 +10%」自己节流。
			c.lim.Success()
			return nil

		case st == http.StatusUnauthorized, st == http.StatusForbidden:
			// 不重试：重试一个错的 key 只会把 401 变成一堆 401，
			// 还会把「凭据失效」拖成「网络超时」的样子。
			e := &Error{Op: path, Status: st, kind: ErrUnauthorized}
			var er errResp
			if json.Unmarshal(body, &er) == nil {
				e.Code, e.Message = er.StatusCode, er.StatusMessage
			}
			return e

		case st == http.StatusNotFound:
			return &Error{Op: path, Status: st, kind: ErrNotFound}

		case st == http.StatusTooManyRequests:
			// 检查清单 §2 的结论：429 最多重试 2 次就放弃，交给下一轮。
			// 在限流期间死磕会把整个刮削任务卡在这一条上，
			// 而 5 万个文件里卡住一条的代价是「进度条不动」。
			c.lim.Throttled()
			rateHits++
			if rateHits > maxThrottleRetries {
				return &Error{Op: path, Status: st, kind: ErrRateLimited, Retriable: true}
			}
			wait := retryAfter(resp, c.now())
			if wait <= 0 {
				wait = backoff
			}
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
			backoff = nextBackoff(backoff)
			continue

		case st >= 500:
			netTries++
			if netTries > maxNetworkRetries {
				return &Error{Op: path, Status: st, kind: ErrUnavailable, Retriable: true}
			}
			if err := c.sleep(ctx, jitter(backoff, c.lim.rnd)); err != nil {
				return err
			}
			backoff = nextBackoff(backoff)
			continue

		default:
			e := &Error{Op: path, Status: st, kind: ErrBadResponse}
			var er errResp
			if json.Unmarshal(body, &er) == nil {
				e.Code, e.Message = er.StatusCode, er.StatusMessage
			}
			return e
		}
	}
}

const (
	maxNetworkRetries  = 3
	maxThrottleRetries = 2
	maxBackoff         = 32 * time.Second
)

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// errTooLarge 与 I/O 错误分开，因为处置不同：前者不重试，后者重试。
var errTooLarge = errors.New("响应体超过上限")

// readLimited 读响应体，超限报错而不是静默截断。
//
// io.LimitReader 会把超长 body 截成前半段，而截断后的 JSON 通常解析失败，
// 报出来是「响应无法解析」——一个和真正原因（body 太大）毫无关系的错误。
// 多读 1 字节就能把这两种情况分开。
func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBody {
		return nil, errTooLarge
	}
	return b, nil
}

// retryAfter 读 Retry-After，读不到返回 0。
//
// 检查清单 §2 的原话是「不能当依赖」：官方文档没承诺这个头，那次没触发 429
// 也就无法验证。所以这里只在它存在时用来**延长**等待，缺了就用自己的退避——
// 反过来（缺了就不等）会让一次 429 变成连发。
func retryAfter(resp *http.Response, now time.Time) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		d := time.Duration(n) * time.Second
		if d > maxBackoff {
			d = maxBackoff
		}
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			if d > maxBackoff {
				d = maxBackoff
			}
			return d
		}
	}
	return 0
}

// ---- 缓存 ----

// cacheSchema 是 payload 的形状版本。
//
// 投影后的 JSON 一旦改字段（删字段、改语义），旧缓存读出来会「看起来能用
// 但值是空的」——比如把 year 从对象改成 int 之后，旧缓存解出来 year 恒为 nil，
// 年份信号悄悄失效。带版本号就能让它变成一次干净的 miss。
//
// 时机：改 Hit / Detail / SeasonDetail 的 json tag 就把它 +1。
const cacheSchema = 1

type cacheEnvelope struct {
	V int             `json:"v"`
	D json.RawMessage `json:"data"`
}

// cacheGet 读缓存并解到 out。命中返回 true。
func (c *Client) cacheGet(ctx context.Context, key string, ttl time.Duration, out any) (bool, error) {
	if c.cache == nil {
		return false, nil
	}
	e, ok, err := c.cache.GetTMDBCache(ctx, key, clampAge(ttl))
	if err != nil || !ok {
		return false, err
	}
	var env cacheEnvelope
	if err := json.Unmarshal([]byte(e.Payload), &env); err != nil || env.V != cacheSchema {
		// 坏缓存当 miss，不报错：一条读不出来的缓存不该让整个刮削停下。
		return false, nil
	}
	if err := json.Unmarshal(env.D, out); err != nil {
		return false, nil
	}
	return true, nil
}

// cachePut 写缓存。写失败只记在返回值里，不影响本次结果——
// 缓存是加速手段，不是正确性的一部分。
func (c *Client) cachePut(ctx context.Context, key, kind, mediaType string, id int64, v any) error {
	if c.cache == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	env, err := json.Marshal(cacheEnvelope{V: cacheSchema, D: raw})
	if err != nil {
		return err
	}
	return c.cache.PutTMDBCache(ctx, store.TMDBEntry{
		Key:       key,
		Kind:      kind,
		MediaType: mediaType,
		TMDBID:    id,
		Language:  c.language,
		Payload:   string(env),
		FetchedAt: c.now().UnixMilli(),
	})
}

// CacheKey 拼缓存键。导出是为了让「清缓存」的调用方能算出一模一样的键。
func CacheKey(kind, mediaType, language string, id int, extra ...string) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteByte(':')
	b.WriteString(mediaType)
	b.WriteByte(':')
	b.WriteString(language)
	if id > 0 {
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(id))
	}
	for _, x := range extra {
		b.WriteByte(':')
		b.WriteString(x)
	}
	return b.String()
}

// hashQuery 把查询词折成定长短串。
//
// 不用原串是怕它太长（契约允许 200 字符）且含分隔符；
// 先 ToLower + TrimSpace 再哈希，是为了让 `Dune` 与 `dune` 命中同一条缓存——
// 解析器产出的片名大小写并不稳定（`All.Wishes` vs `all wishes`）。
func hashQuery(q string) string {
	n := strings.ToLower(strings.Join(strings.Fields(q), " "))
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:])
}
