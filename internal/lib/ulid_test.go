package lib

import (
	"strings"
	"testing"
	"time"
)

func TestULIDLength(t *testing.T) {
	id := NewULID()
	if len(id) != 26 {
		t.Fatalf("ULID 长度应为 26，实际 %d: %q", len(id), id)
	}
	for _, c := range id {
		if !strings.ContainsRune(crockford, c) {
			t.Fatalf("ULID 含非法字符 %q: %q", c, id)
		}
	}
}

func TestULIDSortableByTime(t *testing.T) {
	// 任务队列依赖字典序 == 入队序，这样 ORDER BY id 就够了。
	base := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	a := NewULIDAt(base)
	b := NewULIDAt(base.Add(time.Second))
	if !(a < b) {
		t.Fatalf("时间更晚的 ULID 字典序反而更小: %s >= %s", a, b)
	}
}

func TestULIDMonotonicWithinSameMillisecond(t *testing.T) {
	ms := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	prev := NewULIDAt(ms)
	for i := 0; i < 5000; i++ {
		cur := NewULIDAt(ms)
		if cur <= prev {
			t.Fatalf("同毫秒内未单调递增: %s <= %s (第 %d 次)", cur, prev, i)
		}
		prev = cur
	}
}

func TestULIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id := NewULID()
		if _, dup := seen[id]; dup {
			t.Fatalf("ULID 重复: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewIDPrefix(t *testing.T) {
	id := NewID("md")
	if !strings.HasPrefix(id, "md_") {
		t.Fatalf("前缀缺失: %q", id)
	}
	if len(id) != 3+26 {
		t.Fatalf("长度错: %q", id)
	}
	if NewID("") == "" {
		t.Fatal("空前缀应退回裸 ULID")
	}
}
