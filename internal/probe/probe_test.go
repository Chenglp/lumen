package probe

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// TestParseFPSZeroOnly 锁住一个实测出来的坑。
//
// 用 8 秒片段跑硬编，ffmpeg 的统计行里 fps 全是 0.0（进程结束太快，
// 统计行只有收尾那一行）。如果直接取"最后一行的 fps"，测量会得到 0，
// 然后整条探测链路判失败、机器掉到 X 档——而它其实能跑 480 fps。
func TestParseFPSZeroOnly(t *testing.T) {
	out := []byte("frame=  144 fps=0.0 q=-1.0 Lsize=N/A time=00:00:02.91 bitrate=N/A speed=44.7x elapsed=0:00:00.06    \n")
	if got := ParseFPS(out); got != 0 {
		t.Fatalf("ParseFPS = %v, 期望 0（这一行确实没有可用帧率）", got)
	}
	// 有非零值时取最后一个非零值。
	out2 := []byte("frame=  33 fps=0.0 q=23.0\nframe=  89 fps=61.2 q=23.0\nframe= 144 fps=0.0 q=23.0\n")
	if got := ParseFPS(out2); math.Abs(got-61.2) > 0.01 {
		t.Fatalf("ParseFPS = %v, 期望 61.2", got)
	}
}

// TestParseFrames 用真实 ffmpeg 输出的形状。
func TestParseFrames(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"收尾行", "frame=  144\n", 144},
		{"多行取最后", "frame=   33\nframe=   89\nframe=  144\n", 144},
		{"CRLF", "frame=  480\r\n", 480},
		{"没有帧数", "Output #0, null, to 'pipe:'\n", 0},
		{"零帧不算", "frame=    0\n", 0},
	}
	for _, c := range cases {
		if got := ParseFrames([]byte(c.in)); got != c.want {
			t.Errorf("%s: ParseFrames = %d, 期望 %d", c.name, got, c.want)
		}
	}
}

// TestTargetHeight 保证输出高度是偶数。
//
// 奇数高度在 yuv420p 上直接失败（色度平面不能半像素），而失败信息是
// "height not divisible by 2"，看起来像参数写错，其实是宽高比算出来的。
func TestTargetHeight(t *testing.T) {
	// 3840x1608 → 1920 宽时高 804（已经是偶数）
	if got := targetHeight(3840, 1608, 1920); got != 804 {
		t.Errorf("3840x1608→1920 高 = %d, 期望 804", got)
	}
	// 1920x1080 → 1280 宽时高 720
	if got := targetHeight(1920, 1080, 1280); got != 720 {
		t.Errorf("1920x1080→1280 高 = %d, 期望 720", got)
	}
	// 1920x800（2.4:1）→ 1280 时高 533，必须进位成 534
	if got := targetHeight(1920, 800, 1280); got%2 != 0 {
		t.Errorf("奇数高度 %d 未被修正", got)
	}
	if got := targetHeight(0, 0, 1920); got != 0 {
		t.Errorf("未知尺寸应返回 0，实际 %d", got)
	}
}

// TestSeekPoint 保证取样点不会越过片尾。
//
// 越界取样会得到 0 帧，表现成"这个编码器不可用"——错得很隐蔽，
// 因为短片段和测量失败看起来一模一样。
func TestSeekPoint(t *testing.T) {
	cases := []struct {
		dur        float64
		secs, want int
	}{
		{143 * 60, 8, 600}, // 858 秒的 10% 被 600 秒上限截断
		{1000, 8, 100},     // 100 秒
		{20, 8, 0},         // 太短，从头开始
		{10, 8, 0},         // 更短
		{30, 8, 3},         // 30 秒的 10% = 3，且 3+8 仍在片内
		{9, 8, 0},          // 9 秒片源，不 seek
	}
	for _, c := range cases {
		got := seekPoint(c.dur, c.secs)
		if got != c.want {
			t.Errorf("seekPoint(%.0f, %d) = %d, 期望 %d", c.dur, c.secs, got, c.want)
		}
		if float64(got+c.secs) > c.dur && got != 0 {
			t.Errorf("seekPoint(%.0f, %d) = %d 越过了片尾", c.dur, c.secs, got)
		}
	}
}

// TestSecondSeek 第二个取样点必须离第一个足够远，否则等于白测。
func TestSecondSeek(t *testing.T) {
	// 858 秒片源，第一个点在 600 → 第二个点会被压回片尾附近
	if got := secondSeek(8580, 8, 600); got <= 600 || float64(got+8) > 8580 {
		t.Errorf("secondSeek(8580, 8, 600) = %d，越界或没有前进", got)
	}
	// 短片源：两个点会重叠，返回 0 表示"不要第二个点"
	if got := secondSeek(20, 8, 0); got != 0 {
		t.Errorf("短片源不该有第二个取样点，实际 %d", got)
	}
	// 正常情况：两个点必须至少隔开片长的 1/4
	if got := secondSeek(6000, 8, 600); got < 600+1500 {
		t.Errorf("secondSeek(6000, 8, 600) = %d，离第一个点太近", got)
	}
}

// TestPlansForHardwareFirst 硬件解码方案必须排在软件前面。
//
// 排反了不会报错，只会让一台能跑 480 fps 的机器测出 76 fps，
// 然后被判成 M 档——性能白扔一半还找不到原因。
func TestPlansForHardwareFirst(t *testing.T) {
	cases := map[string]string{
		"h264_nvenc":        "cuda",
		"hevc_nvenc":        "cuda",
		"h264_videotoolbox": "videotoolbox",
		"h264_qsv":          "qsv",
		"libx264":           "software",
		"libx265":           "software",
		// 没有实现硬解方案的厂商必须退到软件，而不是给出一个错的方案
		"h264_amf":   "software",
		"h264_vaapi": "software",
	}
	for enc, wantFirst := range cases {
		plans := plansFor(enc)
		if len(plans) == 0 {
			t.Errorf("%s: 没有方案", enc)
			continue
		}
		if plans[0].name != wantFirst {
			t.Errorf("%s: 首选方案 = %s, 期望 %s", enc, plans[0].name, wantFirst)
		}
		// 除纯软件编码器外，都必须有软件兜底
		if wantFirst != "software" && plans[len(plans)-1].name != "software" {
			t.Errorf("%s: 缺少软件兜底方案", enc)
		}
	}
}

// TestPlansForScaleFilterProducesRightArgs 缩放滤镜的参数个数必须和格式串匹配。
//
// scale_cuda 与 scale 的参数语法不同（前者要显式 :format=nv12），
// 写错的话 ffmpeg 会报 "Option 'format' not found"，而这只会表现为
// "硬编不可用"，然后静默退到软编——数字莫名其妙地低。
func TestPlansForScaleFilterProducesRightArgs(t *testing.T) {
	for _, enc := range []string{"h264_nvenc", "h264_qsv", "libx264"} {
		for _, p := range plansFor(enc) {
			if p.name == "qsv" {
				// scale_qsv 用命名参数，不能套 %d:%d
				continue
			}
			vf := fmt.Sprintf(p.scale, 1920, 804)
			if !strings.Contains(vf, "1920") {
				t.Errorf("%s/%s: 缩放参数没被替换: %q", enc, p.name, vf)
			}
		}
	}
}
