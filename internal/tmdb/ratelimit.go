package tmdb

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"
)

// 限流参数。这些数字来自 R6 检查单 §2，不是拍脑袋。
//
// 为什么不能写死一个「每秒 N 次」：R6 实测本机最高 14.7 次/秒未 429，
// 而并发从 20 加到 50 吞吐不涨、延迟不涨——说明瓶颈在出网代理，
// 不在 TMDB。也就是说**这台机器上测到的上限是被代理截断的值**，
// 拿它当常量写进代码，换一台机器就是错的（目标机是家宽或 NAS 直连）。
//
// 所以用 AIMD：成功就慢慢提速，被限流就对半砍。
const (
	// StartRate 是起始速率：5 次/秒。保守，因为首扫可能在用户正在用的机器上跑。
	StartRate = 5.0
	// MaxRate 是上限：20 次/秒。TMDB 在 2019 年取消了「40 次/10 秒」旧限制，
	// 但 R6 检查单明确要求不写死新值，所以这里只是一个「不要失控」的天花板。
	MaxRate = 20.0
	// MinRate 是下限：0.5 次/秒。再低就不如停下来等下一轮了。
	MinRate = 0.5
	// rateRampInterval / rateRampFactor：连续成功时每分钟 +10%。
	rateRampInterval = time.Minute
	rateRampFactor   = 1.1
)

// limiter 是自适应令牌桶。
//
// 桶容量 = 当前速率（1 秒的量），所以速率降低时桶也随之缩小——
// 否则被限流之后桶里还攒着一把令牌，会立刻又打一波出去，然后把 429 变成连续 429。
type limiter struct {
	mu       sync.Mutex
	rate     float64 // 每秒令牌
	tokens   float64
	last     time.Time
	lastBump time.Time

	now   func() time.Time
	sleep Sleeper
	rnd   *lockedRand
}

func newLimiter(rate float64, now func() time.Time, sleep Sleeper) *limiter {
	rate = math.Max(MinRate, math.Min(MaxRate, rate))
	t := now()
	return &limiter{
		rate:     rate,
		tokens:   rate, // 起步桶满，第一波并发不会被无谓地串行化
		last:     t,
		lastBump: t,
		now:      now,
		sleep:    sleep,
		rnd:      newLockedRand(1),
	}
}

// Wait 阻塞直到拿到一个令牌，或 ctx 结束。
func (l *limiter) Wait(ctx context.Context) error {
	for tries := 0; ; tries++ {
		l.mu.Lock()
		l.refill(l.now())
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		need := time.Duration(float64(time.Second) * (1 - l.tokens) / l.rate)
		// 时钟是注入的，测试里可能不前进；不设上限的话这里会永远循环下去。
		// 正常路径上 sleeps 一次就够（睡够 need 一定攒到令牌），
		// 所以「睡了两轮还没令牌」只可能是时钟不动——放行，别把 goroutine 挂死。
		if tries >= 2 {
			l.tokens = 0
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()

		if need < time.Millisecond {
			need = time.Millisecond
		}
		if err := l.sleep(ctx, need); err != nil {
			return err
		}
	}
}

func (l *limiter) refill(now time.Time) {
	if now.Before(l.last) { // 时钟被往回拨，别把令牌算成负的
		l.last = now
		return
	}
	elapsed := now.Sub(l.last).Seconds()
	l.last = now
	if elapsed <= 0 {
		return
	}
	l.tokens = math.Min(l.burst(), l.tokens+elapsed*l.rate)
}

// burst 是桶容量：1 秒的量，但至少 1 个，否则永远攒不满一个令牌。
func (l *limiter) burst() float64 { return math.Max(1, l.rate) }

// Success 记一次成功，最多每分钟提速 10%。
//
// 为什么不是「每次成功都提速」：一次扫描里成功的请求占绝大多数，
// 每次都提会让速率在几十个请求内顶到 MaxRate，然后被 429 打回来——
// 反复震荡比稳定在略低的速率更慢。
func (l *limiter) Success() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastBump) < rateRampInterval {
		return
	}
	l.lastBump = now
	if l.rate < MaxRate {
		l.rate = math.Min(MaxRate, l.rate*rateRampFactor)
	}
}

// Throttled 记一次 429：速率对半砍，桶清空并重置提速计时。
//
// 清空桶是关键：被限流说明「刚才那一波已经把配额用完了」，
// 留着令牌等于把这次限流当成一次噪声。
func (l *limiter) Throttled() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate = math.Max(MinRate, l.rate/2)
	l.tokens = 0
	l.last = l.now()
	l.lastBump = l.last
}

// Rate 当前速率，给诊断与测试用。
func (l *limiter) Rate() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// lockedRand 是加锁的 rand，供退避抖动使用。
//
// 为什么要抖动：限流是按秒计的，8 个 goroutine 同时撞上 429 时若退避完全同步，
// 它们会在同一毫秒一起重试，把下一次配额又打成一次 429。
type lockedRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

func newLockedRand(seed int64) *lockedRand {
	return &lockedRand{r: rand.New(rand.NewSource(seed))}
}

func (lr *lockedRand) Float64() float64 {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.r.Float64()
}

// jitter 给退避加 ±20% 抖动。
func jitter(d time.Duration, r *lockedRand) time.Duration {
	if d <= 0 {
		return 0
	}
	f := 0.8 + 0.4*r.Float64()
	return time.Duration(float64(d) * f)
}
