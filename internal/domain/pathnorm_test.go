package domain

import (
	"strings"
	"testing"
)

func TestNFCNormalizesDecomposed(t *testing.T) {
	// macOS 返回 NFD：「é」是 e + U+0301。NFC 后应合成单码点 U+00E9。
	nfd := "Ame\u0301lie"
	nfc := "Amélie"

	if NFC(nfd) != nfc {
		t.Fatalf("NFC 未合成组合字符: got %q, want %q", NFC(nfd), nfc)
	}
	if Fold(NFC(nfd)) != Fold(NFC(nfc)) {
		t.Fatal("NFC 前后折叠结果不一致——跨平台会整库重匹配")
	}
}

func TestFoldLigatureDiffersFromToLower(t *testing.T) {
	// R7 实测：U+FB01（连字 ﬁ）用 ToLower 不变，用 cases.Fold 变 "fi"。
	// 用错一个函数，同一文件两次扫描会算出不同 path_key。
	const ligature = "\uFB01"

	if got := strings.ToLower(ligature); got != ligature {
		t.Fatalf("前提变了：ToLower 现在会展开连字（%q），本测试的前提需要复核", got)
	}
	if got := Fold(ligature); got != "fi" {
		t.Fatalf("Fold 未展开连字: got %q, want \"fi\"", got)
	}
}

func TestPathKeyCaseAndUnicodeInsensitive(t *testing.T) {
	// 同一个文件在 Windows 上是 "Movies/Amélie.mkv"（NFC），
	// 在 macOS 上是 NFD 且可能大小写不同。三者必须算出同一个键。
	a := PathKey("vol_1", "Movies/Amélie.mkv")
	b := PathKey("vol_1", "movies/AME\u0301LIE.MKV") // NFD + 全大写
	if a != b {
		t.Fatalf("大小写/Unicode 差异未归一: %s != %s", a, b)
	}
}

func TestPathKeyVolumeScoped(t *testing.T) {
	// 两个卷上都可能有 media/tv，唯一键必须区分卷。
	if PathKey("vol_1", "media/tv") == PathKey("vol_2", "media/tv") {
		t.Fatal("path_key 未包含 volume_id——两个卷的同名路径会互相覆盖")
	}
}

func TestPathKeySeparatorInsensitive(t *testing.T) {
	if PathKey("v", `Movies\A.mkv`) != PathKey("v", "Movies/A.mkv") {
		t.Fatal("Windows 反斜杠与 POSIX 斜杠未归一")
	}
}

func TestCleanRelRejectsUnsafe(t *testing.T) {
	bad := []string{
		"",               // 空
		"/abs/path",      // 绝对路径
		`C:\Media\movie`, // 盘符绝对路径
		`C:/Media/movie`, // 同上，正斜杠
		"../escape",      // 越界
		"a/../../b",      // 折叠后越界
	}
	for _, in := range bad {
		if got, ok := CleanRel(in); ok {
			t.Errorf("CleanRel(%q) 应被拒绝，却得到 %q", in, got)
		}
	}

	good := map[string]string{
		"media/tv/Show/S01E01.mkv": "media/tv/Show/S01E01.mkv",
		`media\tv\Show\S01E01.mkv`: "media/tv/Show/S01E01.mkv",
		"./media/tv/":              "media/tv",
		"media//tv///Show/":        "media/tv/Show",
	}
	for in, want := range good {
		got, ok := CleanRel(in)
		if !ok {
			t.Errorf("CleanRel(%q) 不该被拒绝", in)
			continue
		}
		if got != want {
			t.Errorf("CleanRel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsJunk(t *testing.T) {
	junk := []string{".DS_Store", "@eaDir", "#recycle", "$RECYCLE.BIN", ".hidden", "Thumbs.db", "System Volume Information"}
	for _, n := range junk {
		if !IsJunk(n) {
			t.Errorf("IsJunk(%q) 应为 true", n)
		}
	}
	keep := []string{"Movies", "S01E01.mkv", "The Office (2005)"}
	for _, n := range keep {
		if IsJunk(n) {
			t.Errorf("IsJunk(%q) 应为 false", n)
		}
	}
}

func TestIsForbiddenRoot(t *testing.T) {
	bad := []string{`C:\Windows`, `C:\Windows\System32`, `C:\Program Files\Foo`, `/System/Library`, `/usr/local`}
	for _, p := range bad {
		if !IsForbiddenRoot(p) {
			t.Errorf("IsForbiddenRoot(%q) 应为 true", p)
		}
	}
	good := []string{`D:\Media`, `E:\WB\MM`, `/Volumes/Media`}
	for _, p := range good {
		if IsForbiddenRoot(p) {
			t.Errorf("IsForbiddenRoot(%q) 应为 false", p)
		}
	}
}

func TestLongPath(t *testing.T) {
	if got := LongPath("windows", `D:\Media\a.mkv`); got != `\\?\D:\Media\a.mkv` {
		t.Fatalf("盘符路径前缀错: %q", got)
	}
	if got := LongPath("windows", `\\nas\media\a.mkv`); got != `\\?\UNC\nas\media\a.mkv` {
		t.Fatalf("UNC 前缀错: %q", got)
	}
	// 非 Windows 平台不得改写，否则打不开文件。
	if got := LongPath("linux", "/media/a.mkv"); got != "/media/a.mkv" {
		t.Fatalf("非 Windows 平台被改写: %q", got)
	}
	// 幂等：已经带前缀不重复加。
	if got := LongPath("windows", `\\?\D:\a.mkv`); got != `\\?\D:\a.mkv` {
		t.Fatalf("重复加前缀: %q", got)
	}
}
