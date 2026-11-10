// Package lib 提供无业务含义的基础件：ULID、时间、签名。
package lib

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// Crockford Base32（去掉了 I/L/O/U，避免人工抄写歧义）。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu       sync.Mutex
	lastMS   int64
	lastTail [10]byte // 80 bit 随机部分，同毫秒内自增保证单调
)

// NewULID 返回当前时刻的 ULID。
func NewULID() string { return NewULIDAt(time.Now()) }

// NewULIDAt 返回指定时刻的 ULID。
//
// 同一毫秒内连续调用时，随机部分按大端自增，保证字典序 == 生成序。
// 这一点对任务队列很重要：`ORDER BY id` 就是入队顺序，不必再加时间列。
func NewULIDAt(t time.Time) string {
	ms := t.UnixMilli()
	if ms < 0 {
		ms = 0
	}

	mu.Lock()
	defer mu.Unlock()

	var tail [10]byte
	if ms == lastMS {
		incrementTail(&lastTail)
		tail = lastTail
	} else {
		if _, err := rand.Read(tail[:]); err != nil {
			// crypto/rand 失败是系统级故障，退化成时间戳填充也比 panic 好：
			// 至少 ID 仍然唯一（同毫秒内靠自增兜底）。
			binary.BigEndian.PutUint64(tail[2:], uint64(ms))
		}
		lastMS = ms
		lastTail = tail
	}

	var raw [16]byte
	putUint48(raw[0:6], uint64(ms))
	copy(raw[6:], tail[:])
	return encode128(raw)
}

// NewID 返回带前缀的业务 ID，如 NewID("md") -> "md_01JC...".
func NewID(prefix string) string {
	if prefix == "" {
		return NewULID()
	}
	return prefix + "_" + NewULID()
}

func incrementTail(t *[10]byte) {
	for i := len(t) - 1; i >= 0; i-- {
		t[i]++
		if t[i] != 0 {
			return
		}
	}
	// 整体溢出（同一毫秒内 2^80 次调用），退化为全零，实践上不可能发生。
}

func putUint48(b []byte, v uint64) {
	b[0] = byte(v >> 40)
	b[1] = byte(v >> 32)
	b[2] = byte(v >> 24)
	b[3] = byte(v >> 16)
	b[4] = byte(v >> 8)
	b[5] = byte(v)
}

// encode128 把 128 bit 编成 26 个 Base32 字符（130 bit，前 2 bit 补零）。
func encode128(b [16]byte) string {
	var out [26]byte
	for i := 0; i < 26; i++ {
		var v byte
		for j := 0; j < 5; j++ {
			v <<= 1
			logical := i*5 + j - 2 // 前 2 个逻辑位是补零
			if logical >= 0 {
				v |= (b[logical/8] >> (7 - uint(logical%8))) & 1
			}
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}
