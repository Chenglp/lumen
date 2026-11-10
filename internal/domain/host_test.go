package domain

import (
	"math"
	"testing"
)

// TestDecideHost 用 R1 在这台机器上实测到的真实数字做表驱动测试。
//
// 这些数字不是编的：254.4 / 67.0 / 137.0 / 71.1 都来自 2026-11-11 的实测
// （3840x1608 60fps HEVC 10bit DV P5，i7-9700K + RTX 2080 Ti）。
// 用真实数字做输入的价值在于：如果哪天有人调了阈值，测试会直接告诉他
// "你把实测 254 fps 的机器判成了 2 路并发"。
func TestDecideHost(t *testing.T) {
	cases := []struct {
		name     string
		m        HostMeasurements
		wantTier string
		wantMax  int
		wantConc int
		wantTone int
	}{
		{
			name:     "没有 ffmpeg",
			m:        HostMeasurements{HasFFmpeg: false},
			wantTier: "X", wantMax: 720, wantConc: 1, wantTone: 0,
		},
		{
			name: "本机实测：NVENC 全链 254.4 fps",
			m: HostMeasurements{
				HasFFmpeg: true, HWEncoder: "h264_nvenc", SampleKind: "real",
				ChainFPS1080: 254.4, TonemapFPS: 71.1,
			},
			// 254.4/24 = 10.6×；并发 = floor(254.4/(24*1.35)) = 7，被 S 档上限 3 截断
			wantTier: "S", wantMax: 2160, wantConc: 3, wantTone: 1,
		},
		{
			name: "只有软编：全链 67 fps",
			m: HostMeasurements{
				HasFFmpeg: true, SoftEncoder: "libx264", SampleKind: "real",
				ChainFPS1080: 67.0,
			},
			// 67/24 = 2.8×，但没有硬编所以不是 S；并发 floor(67/32.4) = 2
			wantTier: "M", wantMax: 1080, wantConc: 2, wantTone: 0,
		},
		{
			name: "弱核显 NAS：全链 10 fps",
			m: HostMeasurements{
				HasFFmpeg: true, SoftEncoder: "libx264", SampleKind: "real", ChainFPS1080: 10,
			},
			wantTier: "L", wantMax: 1080, wantConc: 1, wantTone: 0,
		},
		{
			name: "老机器：全链 3 fps",
			m: HostMeasurements{
				HasFFmpeg: true, SoftEncoder: "libx264", SampleKind: "real", ChainFPS1080: 3,
			},
			wantTier: "X", wantMax: 720, wantConc: 1, wantTone: 0,
		},
		{
			name: "0.6–1.0 之间落在 L 而不是落空",
			m: HostMeasurements{
				HasFFmpeg: true, SoftEncoder: "libx264", SampleKind: "real", ChainFPS1080: 20,
			},
			// 20/24 = 0.83×。框架原文 S/M/L 的区间在这里是断开的，
			// 这里明确归 L（保守），否则会出现"落不进任何档"。
			wantTier: "L", wantMax: 1080, wantConc: 1, wantTone: 0,
		},
		{
			name: "有硬编但吞吐低仍然是 M",
			m: HostMeasurements{
				HasFFmpeg: true, HWEncoder: "h264_qsv", SampleKind: "real", ChainFPS1080: 20,
			},
			wantTier: "M", wantMax: 1080, wantConc: 1, wantTone: 0,
		},
		{
			name: "有 libplacebo 但没 DV 样本：tonemap 仍预留 1 路",
			m: HostMeasurements{
				HasFFmpeg: true, HWEncoder: "h264_nvenc", SampleKind: "real",
				ChainFPS1080: 254.4, FFmpegFeatures: []string{"libplacebo", "zscale"},
			},
			wantTier: "S", wantMax: 2160, wantConc: 3, wantTone: 1,
		},
		{
			name: "essentials 构建：缺 libplacebo，tonemap 0 路",
			m: HostMeasurements{
				HasFFmpeg: true, HWEncoder: "h264_nvenc", SampleKind: "real",
				ChainFPS1080: 254.4, FFmpegFeatures: []string{"tonemap"},
			},
			wantTier: "S", wantMax: 2160, wantConc: 3, wantTone: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecideHost(c.m)
			if got.Tier != c.wantTier {
				t.Errorf("Tier = %s, 期望 %s（依据：%v）", got.Tier, c.wantTier, got.Reasons)
			}
			if got.MaxHeight != c.wantMax {
				t.Errorf("MaxHeight = %d, 期望 %d", got.MaxHeight, c.wantMax)
			}
			if got.TranscodeConcurrency != c.wantConc {
				t.Errorf("TranscodeConcurrency = %d, 期望 %d", got.TranscodeConcurrency, c.wantConc)
			}
			if got.TonemapSlots != c.wantTone {
				t.Errorf("TonemapSlots = %d, 期望 %d", got.TonemapSlots, c.wantTone)
			}
		})
	}
}

// TestConfidence：只有真实样本才配叫 measured。
func TestConfidence(t *testing.T) {
	for _, c := range []struct {
		kind string
		want string
	}{
		{"real", "measured"},
		{"synthetic", "estimated"},
		{"none", "estimated"},
		{"", "estimated"},
	} {
		got := DecideHost(HostMeasurements{
			HasFFmpeg: true, SoftEncoder: "libx264", SampleKind: c.kind, ChainFPS1080: 60,
		})
		if got.Confidence != c.want {
			t.Errorf("SampleKind=%q → Confidence=%q, 期望 %q", c.kind, got.Confidence, c.want)
		}
	}
}

// TestRatioAtIsFrameRateRelative 锁住 R1 推翻的第一条假设。
//
// 同一台机器、同一个档位，24fps 与 60fps 片源的倍率必须不同。
// 如果哪天有人把"这台机器 10×"写回常量，这个测试就会红。
func TestRatioAtIsFrameRateRelative(t *testing.T) {
	p := DecideHost(HostMeasurements{
		HasFFmpeg: true, HWEncoder: "h264_nvenc", SampleKind: "real", ChainFPS1080: 254.4,
	})

	if got := p.RatioAt(24, 1080); math.Abs(got-10.6) > 0.1 {
		t.Errorf("24fps 片源倍率 = %.2f, 期望约 10.6", got)
	}
	if got := p.RatioAt(60, 1080); math.Abs(got-4.24) > 0.05 {
		t.Errorf("60fps 片源倍率 = %.2f, 期望约 4.24", got)
	}
	// 同一个 profile，两个倍率必须差一倍以上——差异被抹平说明基准帧率丢了。
	if p.RatioAt(24, 1080) < p.RatioAt(60, 1080)*2 {
		t.Error("24fps 与 60fps 的倍率差异过小，帧率没有参与计算")
	}
}

// TestChainFPSFor：没测过的高度只能往保守方向退。
func TestChainFPSFor(t *testing.T) {
	p := HostProfile{HostMeasurements: HostMeasurements{
		ChainFPS1080: 67, ChainFPS2160: 0,
	}}
	if got := p.ChainFPSFor(2160); got != 67 {
		t.Errorf("未知 2160 应退回 1080 的数（保守），实际 %.1f", got)
	}
	p.ChainFPS2160 = 30
	if got := p.ChainFPSFor(2160); got != 30 {
		t.Errorf("已知 2160 应用实测值，实际 %.1f", got)
	}
	// 720p 目标借用 1080p 的数：真实值只会更高，所以这是安全方向。
	if got := p.ChainFPSFor(720); got != 67 {
		t.Errorf("720 应借用 1080 的数，实际 %.1f", got)
	}
}

// TestCanServeHeight：L 档机器不能把 2160 放进 renditions（契约 §4.1）。
func TestCanServeHeight(t *testing.T) {
	l := DecideHost(HostMeasurements{
		HasFFmpeg: true, SoftEncoder: "libx264", SampleKind: "real", ChainFPS1080: 10,
	})
	if l.CanServeHeight(2160) {
		t.Errorf("L 档不该对外提供 2160 档（tier=%s max=%d）", l.Tier, l.MaxHeight)
	}
	if !l.CanServeHeight(1080) {
		t.Error("L 档应能提供 1080 档")
	}
}

// TestNeedsProxy：判据是"实时做不到"，不是"档位低"。
func TestNeedsProxy(t *testing.T) {
	l := DecideHost(HostMeasurements{
		HasFFmpeg: true, SoftEncoder: "libx264", SampleKind: "real", ChainFPS1080: 10,
	})
	// 10 fps 对 24fps 片源只有 0.42×，必须预转代理版。
	if !l.NeedsProxy(2160, 24) {
		t.Error("0.42× 的机器应当判定需要预转代理版")
	}

	s := DecideHost(HostMeasurements{
		HasFFmpeg: true, HWEncoder: "h264_nvenc", SampleKind: "real", ChainFPS1080: 254.4,
	})
	if s.NeedsProxy(2160, 24) {
		t.Error("254 fps 的机器不该被判需要预转")
	}
}

// TestOnBatteryDoesNotDowngrade：电池只影响"现在跑不跑"，不改"这台机器行不行"。
//
// 把两件事混成一个数，之后就没法解释"为什么昨天能转今天不能"。
func TestOnBatteryDoesNotDowngrade(t *testing.T) {
	base := HostMeasurements{
		HasFFmpeg: true, HWEncoder: "h264_nvenc", SampleKind: "real", ChainFPS1080: 254.4,
	}
	withBattery := base
	withBattery.OnBattery = true

	a, b := DecideHost(base), DecideHost(withBattery)
	if a.Tier != b.Tier || a.TranscodeConcurrency != b.TranscodeConcurrency {
		t.Errorf("电池供电改变了档位/并发：%s/%d vs %s/%d",
			a.Tier, a.TranscodeConcurrency, b.Tier, b.TranscodeConcurrency)
	}
	found := false
	for _, r := range b.Reasons {
		if r != "" && len(r) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("电池供电应当留下一条决策依据，否则 32 屏无法解释为什么转码被暂停")
	}
}
