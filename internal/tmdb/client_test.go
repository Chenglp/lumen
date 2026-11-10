package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Chenglp/lumen/internal/store"
)

// fakeClock 让限流与退避的测试不需要真的等。
//
// 关键点：sleep 里必须**推进时钟**。限流器睡醒后会重新按 (now - last) 补令牌，
// 时钟不动就永远补不上，Wait 只能靠「睡两轮就放行」的保护退出——
// 那样测出来的时序全是假的。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func (f *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	f.Advance(d)
	return nil
}

// srv 是一个记录所有请求的假 TMDB。
type srv struct {
	*httptest.Server
	mu   sync.Mutex
	seen []*http.Request
	body []url.Values
	n    int32
}

func newSrv(t *testing.T, h http.HandlerFunc) *srv {
	t.Helper()
	s := &srv{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.n, 1)
		s.mu.Lock()
		s.seen = append(s.seen, r.Clone(context.Background()))
		s.body = append(s.body, r.URL.Query())
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *srv) count() int { return int(atomic.LoadInt32(&s.n)) }

func (s *srv) last() (*http.Request, url.Values) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		return nil, nil
	}
	return s.seen[len(s.seen)-1], s.body[len(s.body)-1]
}

func (s *srv) client(t *testing.T, token string, opts ...Option) *Client {
	t.Helper()
	base := []Option{WithBaseURL(s.URL)}
	return New(token, append(base, opts...)...)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

const tvSearchBody = `{
  "page": 1, "total_results": 2,
  "results": [
    {"id": 1, "name": "信号", "original_name": "Signal", "first_air_date": "2016-01-22",
     "original_language": "ko", "popularity": 30.1, "poster_path": "/sig.jpg"},
    {"id": 2, "name": "心动的信号", "original_name": "心动的信号", "first_air_date": "2018-08-26",
     "original_language": "zh", "popularity": 88.5, "poster_path": "/hb.jpg"}
  ]
}`

// 凭据形状决定认证方式。搞错的后果是 401，而 401 会被误读成「key 过期」。
func TestAuthV4UsesBearerHeader(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"total_results":0}`))
	})
	c := s.client(t, "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig")

	if _, err := c.Search(context.Background(), TV, "信号", nil, 10); err != nil {
		t.Fatal(err)
	}
	req, q := s.last()
	if got := req.Header.Get("Authorization"); got != "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig" {
		t.Errorf("Authorization = %q", got)
	}
	// v4 token 不能同时出现在 query 里，否则等于把 JWT 写进 URL 日志。
	if v := q.Get("api_key"); v != "" {
		t.Errorf("v4 凭据不该出现在 query，实际 api_key=%q", v)
	}
}

func TestAuthV3UsesAPIKeyQuery(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"total_results":0}`))
	})
	c := s.client(t, "0123456789abcdef0123456789abcdef")

	if _, err := c.Search(context.Background(), TV, "信号", nil, 10); err != nil {
		t.Fatal(err)
	}
	req, q := s.last()
	if q.Get("api_key") != "0123456789abcdef0123456789abcdef" {
		t.Errorf("v3 凭据应拼进 query，实际 %v", q)
	}
	if h := req.Header.Get("Authorization"); h != "" {
		t.Errorf("v3 凭据不该发 Authorization，实际 %q", h)
	}
}

// 端点与年份参数名的对应关系是最容易写错、且错了不报错的地方。
func TestSearchYearParamNameDiffersByEndpoint(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"total_results":0}`))
	})
	c := s.client(t, "k")

	year := 2016
	if _, err := c.Search(context.Background(), TV, "信号", &year, 10); err != nil {
		t.Fatal(err)
	}
	path, q := s.last()
	if path.URL.Path != "/search/tv" {
		t.Errorf("path = %q", path.URL.Path)
	}
	if got := q.Get("first_air_date_year"); got != "2016" {
		t.Errorf("/search/tv 应发 first_air_date_year，实际 %q", got)
	}
	if got := q.Get("year"); got != "" {
		t.Errorf("/search/tv 的 year 会被 TMDB 忽略，不该发；实际发了 %q", got)
	}

	if _, err := c.Search(context.Background(), Movie, "降临", &year, 10); err != nil {
		t.Fatal(err)
	}
	path, q = s.last()
	if path.URL.Path != "/search/movie" {
		t.Errorf("path = %q", path.URL.Path)
	}
	if got := q.Get("year"); got != "2016" {
		t.Errorf("/search/movie 应发 year，实际 %q", got)
	}
	if got := q.Get("first_air_date_year"); got != "" {
		t.Errorf("/search/movie 不该发 first_air_date_year；实际发了 %q", got)
	}

	// multi 两个参数都不接，发了也是白发——所以一个都不该有。
	if _, err := c.Search(context.Background(), Multi, "信号", &year, 10); err != nil {
		t.Fatal(err)
	}
	path, q = s.last()
	if path.URL.Path != "/search/multi" {
		t.Errorf("path = %q", path.URL.Path)
	}
	if q.Get("year") != "" || q.Get("first_air_date_year") != "" {
		t.Errorf("/search/multi 不接受年份参数，实际 %v", q)
	}
}

// /search/multi 会混进 media_type=person 的人物条目，它们没有 title，
// 放进候选会变成右栏里点不动的空行。
func TestSearchMultiDropsPeople(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
		  "total_results": 2,
		  "results": [
		    {"id": 9, "media_type": "person", "name": "汤唯", "popularity": 50},
		    {"id": 11, "media_type": "tv", "name": "信号", "first_air_date": "2016-01-22"}
		  ]}`))
	})
	c := s.client(t, "k")
	out, err := c.Search(context.Background(), Multi, "信号", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Hits) != 1 || out.Hits[0].TMDBID != 11 {
		t.Fatalf("应只留下剧集条目，实际 %+v", out.Hits)
	}
}

// TV 条目的年份只在 first_air_date 里；读错字段会让年份信号恒为 0，
// 表现是「剧集永远够不到强匹配」——不报错，只是排序一直不对。
func TestTVHitYearComesFromFirstAirDate(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(tvSearchBody))
	})
	c := s.client(t, "k")
	out, err := c.Search(context.Background(), TV, "信号", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Hits) != 2 {
		t.Fatalf("hits = %d", len(out.Hits))
	}
	for _, h := range out.Hits {
		if h.Year == nil {
			t.Fatalf("TV 条目 %d 的年份为 nil——first_air_date 没读到", h.TMDBID)
		}
	}
	if *out.Hits[0].Year != 2016 {
		t.Errorf("year = %d，期望 2016", *out.Hits[0].Year)
	}
	// 片名也必须取 name 而不是 title（TV 条目的 title 字段根本不存在）。
	if out.Hits[0].Title != "信号" || out.Hits[0].OriginalTitle != "Signal" {
		t.Errorf("片名 = %q / %q", out.Hits[0].Title, out.Hits[0].OriginalTitle)
	}
}

func TestThrottleHalvesRateAndGivesUp(t *testing.T) {
	clk := newFakeClock()
	var hits int32
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"status_code": 25, "status_message": "rate limited"})
	})
	c := s.client(t, "k", WithClock(clk.Now), WithSleeper(clk.Sleep), WithRate(8))

	_, err := c.Search(context.Background(), TV, "信号", nil, 10)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v，期望 ErrRateLimited", err)
	}
	// 429 最多重试 2 次：1 次原始 + 2 次重试。
	if got := atomic.LoadInt32(&hits); got != int32(maxThrottleRetries+1) {
		t.Errorf("请求次数 = %d，期望 %d", got, maxThrottleRetries+1)
	}
	// 8 → 4 → 2 → 1：每次 429 对半砍。
	if r := c.lim.Rate(); r != 1 {
		t.Errorf("速率 = %v，期望 1（8 连续两次减半）", r)
	}
}

func TestUnauthorizedIsNotRetried(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"status_code": 7, "status_message": "Invalid API key"})
	})
	c := s.client(t, "bad", WithRate(8))

	_, err := c.Search(context.Background(), TV, "信号", nil, 10)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if s.count() != 1 {
		t.Errorf("401 不该重试，实际请求 %d 次", s.count())
	}
	var te *Error
	if errors.As(err, &te) && te.Code != 7 {
		t.Errorf("TMDB status_code 没带出来：%+v", te)
	}
	// 契约 §5.2：unauothorized 与 network 的处置动作完全不同。
	if r := Reason(err); r != "unauthorized" {
		t.Errorf("Reason = %q，期望 unauthorized", r)
	}
}

func TestNetworkErrorMapsToUnavailable(t *testing.T) {
	// 关掉的端口：连接立即被拒。
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {})
	c := New("k", WithBaseURL(s.URL), WithClock(newFakeClock().Now), WithSleeper(newFakeClock().Sleep), WithRate(8))
	s.Close()

	_, err := c.Search(context.Background(), TV, "信号", nil, 10)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v，期望 ErrUnavailable", err)
	}
	if r := Reason(err); r != "network" {
		t.Errorf("Reason = %q，期望 network", r)
	}
}

// 缓存必须真的省掉请求，否则「27 小时」那笔账全部回到用户身上。
func TestCacheAvoidsSecondRequest(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(tvSearchBody))
	})
	mem := newMemCache()
	c := s.client(t, "k", WithCache(mem))

	first, err := c.Search(context.Background(), TV, "信号", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if first.FromCache || first.Requests != 1 {
		t.Errorf("首次应打请求，实际 fromCache=%v requests=%d", first.FromCache, first.Requests)
	}

	second, err := c.Search(context.Background(), TV, "信号", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !second.FromCache {
		t.Error("第二次应命中缓存")
	}
	if s.count() != 1 {
		t.Errorf("实际请求 %d 次，期望 1", s.count())
	}
	if len(second.Hits) != len(first.Hits) {
		t.Errorf("缓存命中后候选数变了吗 %d → %d", len(first.Hits), len(second.Hits))
	}

	// 大小写与多余空白必须归一到同一个键：解析器产出的片名大小写不稳定。
	if _, err := c.Search(context.Background(), TV, "  信号  ", nil, 10); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Errorf("只差首尾空白却重发了请求（共 %d 次），键没归一", s.count())
	}

	// 大小写同理。
	if _, err := c.Search(context.Background(), Movie, "Interstellar", nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(context.Background(), Movie, "interstellar", nil, 10); err != nil {
		t.Fatal(err)
	}
	if s.count() != 2 {
		t.Errorf("只差大小写却重发了请求（共 %d 次）", s.count())
	}
}

func TestCacheKeyIncludesLanguage(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"total_results":0}`))
	})
	mem := newMemCache()

	zh := s.client(t, "k", WithCache(mem), WithLanguage("zh-CN"))
	en := s.client(t, "k", WithCache(mem), WithLanguage("en-US"))

	if _, err := zh.Search(context.Background(), TV, "signal", nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := en.Search(context.Background(), TV, "signal", nil, 10); err != nil {
		t.Fatal(err)
	}
	// 换语言必须视为 miss（契约 §5.2 的「渐进重抓」），否则用户切了语言
	// 却看到旧语言的片名，还以为设置没生效。
	if s.count() != 2 {
		t.Errorf("不同语言用了同一条缓存：请求 %d 次", s.count())
	}
}

// 12 集同时进匹配阶段时会发出 12 次一模一样的搜索。缓存拦不住这一波
// （它只拦之后的请求），必须有 single flight。
func TestSingleFlightCollapsesConcurrentSearches(t *testing.T) {
	var hits int32
	gate := make(chan struct{})
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			<-gate // 第一个请求卡住，保证其余并发都还在飞行中
		}
		_, _ = w.Write([]byte(tvSearchBody))
	})
	c := s.client(t, "k")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Search(context.Background(), TV, "信号", nil, 10); err != nil {
				t.Error(err)
			}
		}()
	}
	// 等 8 个 goroutine 都进到 doFlight 里，再放行第一个请求。
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("并发 8 次搜索打了 %d 次请求，期望 1", got)
	}
}

func TestValidateReportsUnauthorized(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/configuration" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"status_code": 7})
	})
	c := s.client(t, "bad")
	if err := c.Validate(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoTokenFailsWithoutRequest(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {})
	c := s.client(t, "")
	if _, err := c.Search(context.Background(), TV, "信号", nil, 10); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v", err)
	}
	if s.count() != 0 {
		t.Errorf("没凭据不该发请求，实际 %d 次", s.count())
	}
}

func TestOversizeBodyIsNotSilentlyTruncated(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[`))
		big := strings.Repeat("x", MaxBody+1024)
		_, _ = w.Write([]byte(`{"id":1,"name":"` + big + `"}]}`))
	})
	c := s.client(t, "k")
	_, err := c.Search(context.Background(), TV, "信号", nil, 10)
	if err == nil {
		t.Fatal("超大响应应报错")
	}
	// 关键：报的必须是「太大」，而不是「解析失败」——后者会把人引到 JSON 上。
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err = %v，期望 ErrBadResponse", err)
	}
	// 而且**不能重试**：响应体大小是确定性的，重试 3 次只是白等 7 秒 + 多下 3 个 8MB。
	if s.count() != 1 {
		t.Errorf("超大响应被重试了 %d 次", s.count())
	}
}

// ---- 内存缓存 ----

type memCache struct {
	mu sync.Mutex
	m  map[string]memEntry
}

type memEntry struct {
	payload string
	at      int64
}

func newMemCache() *memCache { return &memCache{m: map[string]memEntry{}} }

func (c *memCache) GetTMDBCache(_ context.Context, key string, maxAge time.Duration) (store.TMDBEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return store.TMDBEntry{}, false, nil
	}
	if maxAge > 0 && time.Now().Add(-maxAge).UnixMilli() > e.at {
		return store.TMDBEntry{}, false, nil
	}
	return store.TMDBEntry{Key: key, Payload: e.payload, FetchedAt: e.at}, true, nil
}

var _ Cache = (*memCache)(nil)

func (c *memCache) PutTMDBCache(_ context.Context, e store.TMDBEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[e.Key] = memEntry{payload: e.Payload, at: e.FetchedAt}
	return nil
}

func (c *memCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
