//go:build windows

package domain

import (
	"regexp"
	"testing"
)

func TestSplitMount(t *testing.T) {
	cases := []struct {
		in, mount, rel string
		ok             bool
	}{
		{`F:\movies`, "F:\\", "movies", true},
		{`F:\movies\4K`, "F:\\", "movies/4K", true},
		{`f:\Movies`, "F:\\", "Movies", true}, // 盘符大小写归一，路径大小写保留
		{`F:\`, "F:\\", ".", true},
		{`F:\movies\`, "F:\\", "movies", true},
		{`\\nas\media\movies`, `\\nas\media\`, "movies", true},
		{`\\nas\media`, `\\nas\media\`, ".", true},
		{`movies`, "", "", false},      // 相对路径
		{`\\nas`, "", "", false},       // UNC 缺共享名
		{`/mnt/movies`, "", "", false}, // 不是 Windows 路径
	}
	for _, c := range cases {
		mount, rel, ok := SplitMount(c.in)
		if ok != c.ok || mount != c.mount || rel != c.rel {
			t.Errorf("SplitMount(%q) = (%q, %q, %v)，期望 (%q, %q, %v)",
				c.in, mount, rel, ok, c.mount, c.rel, c.ok)
		}
	}
}

// TestVolumeKeyOf_StableAcrossPaths 是这条规则真正要保证的东西：
// 同一块盘上的任何路径都必须算出同一个卷标识。
//
// 如果这条破了，用户从「F:\movies」换成「F:\movies\4K」加库，
// 库里就会出现两个卷、两套 path_key，同一部片子重复入库且互不影响进度。
func TestVolumeKeyOf_StableAcrossPaths(t *testing.T) {
	key, err := VolumeKeyOf(`C:\`)
	if err != nil {
		t.Skipf("无法读取系统卷标识，跳过: %v", err)
	}
	if !regexp.MustCompile(`^[0-9A-F]{8}$`).MatchString(key) {
		t.Fatalf("卷标识 %q 不是 8 位大写十六进制", key)
	}

	for _, p := range []string{`C:\Windows`, `c:\Users`, `C:\`} {
		got, err := VolumeKeyOf(p)
		if err != nil {
			t.Fatalf("VolumeKeyOf(%q): %v", p, err)
		}
		if got != key {
			t.Errorf("VolumeKeyOf(%q) = %q，与 C:\\ 的 %q 不一致", p, got, key)
		}
	}
}

func TestVolumeKeyOf_RejectsRelative(t *testing.T) {
	if _, err := VolumeKeyOf(`movies\4K`); err == nil {
		t.Fatal("相对路径应当报错，而不是猜一个卷")
	}
}
