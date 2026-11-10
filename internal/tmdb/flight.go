package tmdb

import "sync"

// flightGroup 让「同一个键同时有多个调用者」只发一次请求。
//
// 这不是优化，是这个场景的必需品：剧集库里 12 集的文件名解析出同一个剧名，
// 扫描器的并发会把它们同时送进匹配阶段。没有 single flight 时，12 个
// goroutine 会各自查一次缓存（都 miss），然后**同时**打出 12 次一模一样的
// search/tv。缓存拦不住这一波——它只拦「之后的」请求。
//
// R6 实测单次 p50 647ms：这 11 次多余请求不是 11×带宽，是 11 个并发的
// 限流令牌 + 11 份可能被 429 打回来的风险。
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  any
	err  error
}

func newFlightGroup() *flightGroup { return &flightGroup{m: make(map[string]*flightCall)} }

// do 执行 fn，并把并发的同键调用合并成一次。
func (g *flightGroup) do(key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.val, c.err
	}
	c := &flightCall{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	close(c.done)
	return c.val, c.err
}

// doFlight 是 do 的泛型包装。Go 不允许泛型方法，所以只能做成自由函数。
func doFlight[T any](g *flightGroup, key string, fn func() (T, error)) (T, error) {
	var zero T
	v, err := g.do(key, func() (any, error) {
		x, e := fn()
		if e != nil {
			return nil, e
		}
		return x, nil
	})
	if err != nil {
		return zero, err
	}
	out, ok := v.(T)
	if !ok {
		return zero, ErrBadResponse
	}
	return out, nil
}
