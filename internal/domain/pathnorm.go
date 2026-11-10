// Package domain 承载纯业务逻辑。
//
// 铁律：本包不 import api、不 import store，只接受值类型和接口。
// 这样播放决策、匹配、路径归一化才能被单测覆盖。
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// 路径归一化 —— 对应技术框架 §1.2。
//
// 三条规则，缺一条就会在换平台时静默出错（不报错，只是数据对不上）：
//
//	① 不存绝对路径：库里只留 volume_id + rel_path，绝对路径每次由 volume_id 解析。
//	② 大小写折叠：所有用于去重/查找的键先 foldCase 再比较，显示用原始大小写。
//	③ Unicode NFC：macOS 返回 NFD，入库前一律 NFC，比较时也用 NFC。
//
// R7 spike（spikes/r7-path-norm）在 NTFS 上实测到的四类静默差异：
// case_collapsed=6 / win32_trimmed=2 / ads_shadowed=1 / platform_skipped=6。

// foldCaser 是 Unicode 简单大小写折叠。
//
// 不能用 strings.ToLower：对 U+FB01（连字 ﬁ）ToLower 返回自身，Fold 返回 "fi"。
// 两者差一个字符，就足以让同一个文件在两次扫描里算出不同的 path_key。
var foldCaser = cases.Fold()

// NFC 返回 NFC 规范化后的字符串。
func NFC(s string) string {
	if norm.NFC.IsNormalString(s) {
		return s
	}
	return norm.NFC.String(s)
}

// Fold 返回大小写折叠后的字符串，仅用于比较与建键，不用于显示。
func Fold(s string) string {
	return foldCaser.String(s)
}

// CleanRel 把任意平台的路径片段整理成统一的相对路径表示：
// 分隔符一律 '/'，去掉首尾分隔符，折叠连续的 "/"。
//
// 返回 (rel, ok)；ok=false 表示这不是一个合法的相对路径——
// 绝对路径、含 ".." 的路径、空串都该在解析阶段拒绝，
// 而不是等到扫描到一半才失败。
func CleanRel(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	// Windows 分隔符与盘符先归一，交给 path.Clean 处理。
	s := strings.ReplaceAll(p, `\`, "/")

	if strings.HasPrefix(s, "/") {
		return "", false // 绝对路径：路径必须以 volume 为根
	}
	// 盘符形式（C:/...）也是绝对路径。
	if len(s) >= 2 && s[1] == ':' {
		return "", false
	}

	c := path.Clean(s)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// PathKey 计算用于去重与查找的路径键。
//
// 键 = sha256(volume_id ‖ 0x00 ‖ Fold(NFC(rel_path)))
//
// 为什么把 volume_id 拼进去：两个卷上都可以有 media/tv，
// 唯一键必须是 (volume, rel) 的组合，不是 rel 本身。
//
// 为什么用哈希而不是直接存折叠后的字符串：rel_path 可能很长（Windows 深目录），
// 做成定长 64 字节的索引列，比较和索引都更省。
func PathKey(volumeID, relPath string) string {
	rel, ok := CleanRel(relPath)
	if !ok {
		rel = strings.ReplaceAll(relPath, `\`, "/")
	}
	h := sha256.New()
	h.Write([]byte(volumeID))
	h.Write([]byte{0})
	h.Write([]byte(Fold(NFC(rel))))
	return hex.EncodeToString(h.Sum(nil))
}

// 平台差异 ④⑤：扫描时该跳过的目录/文件。
//
// 这些名字在真实媒体库里必然出现，而且量很大——不跳过会让首扫多出几万个
// 无关条目，还会把系统目录当成待整理项塞满 27 屏。
var junkNames = map[string]struct{}{
	".DS_Store":                 {},
	"Thumbs.db":                 {},
	"desktop.ini":               {},
	"@eaDir":                    {}, // Synology 缩略图目录
	"#recycle":                  {}, // Synology 回收站
	"$RECYCLE.BIN":              {},
	"System Volume Information": {},
	".Trashes":                  {},
	".Spotlight-V100":           {},
	".fseventsd":                {},
	".@__thumb":                 {}, // QNAP 缩略图
	".AppleDouble":              {},
}

// IsJunk 判断一个路径片段是否该被扫描器忽略（按名字，不含路径）。
func IsJunk(name string) bool {
	if name == "" {
		return true
	}
	// 以 "." 开头的隐藏文件/目录统一跳过。这条会漏掉用户故意用点开头的库，
	// 但那属于加库时可以显式指定的少数情况，不值得为它放开整条规则。
	if strings.HasPrefix(name, ".") {
		return true
	}
	_, hit := junkNames[name]
	return hit
}

// 平台差异 ⑤：加库时要禁掉的系统路径。
//
// 用户完全可能把整个 C:\Users\me 加进来，那样会扫到几十万个无关文件。
var forbiddenRoots = []string{
	`c:\windows`,
	`c:\program files`,
	`c:\program files (x86)`,
	`c:\programdata`,
	`/system`,
	`/library`,
	`/usr`,
	`/etc`,
	`/var`,
	`/bin`,
	`/sbin`,
	`/private`,
	`~/.ssh`,
}

// IsForbiddenRoot 判断用户想添加的库根是否落在系统目录里。
// 输入是绝对路径，大小写不敏感比较。
func IsForbiddenRoot(abs string) bool {
	p := strings.ToLower(strings.ReplaceAll(abs, `\`, "/"))
	p = strings.TrimSuffix(p, "/")
	for _, f := range forbiddenRoots {
		f = strings.ReplaceAll(f, `\`, "/")
		if p == f || strings.HasPrefix(p, f+"/") {
			return true
		}
	}
	return false
}

// MaxPathWindows 是 Windows 传统 API 的限制；加 \\?\ 后放宽到 32767。
const (
	MaxPathWindows  = 260
	MaxPathExtended = 32767
)

// LongPath 在 Windows 上给绝对路径加 \\?\ 前缀，绕开 260 字符限制。
//
// 非 Windows 平台原样返回——其它平台没有这个问题，加了反而打不开文件。
// UNC 路径（\\server\share）要写成 \\?\UNC\server\share。
func LongPath(goos, abs string) string {
	if goos != "windows" {
		return abs
	}
	if strings.HasPrefix(abs, `\\?\`) {
		return abs
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(abs, `\\`)
	}
	// 只处理绝对路径，相对路径加前缀无意义。
	if len(abs) >= 2 && abs[1] == ':' {
		return `\\?\` + abs
	}
	return abs
}

// CheckPathLength 在解析阶段就拒绝超长路径，而不是扫到一半失败。
// 返回 false 表示该文件应当被跳过并记为 skipped，而不是让整个扫描中断。
func CheckPathLength(abs string) bool {
	n := utf8.RuneCountInString(abs)
	if n > MaxPathExtended {
		return false
	}
	return true
}
