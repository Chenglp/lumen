package store

import (
	"context"
	"errors"
	"testing"

	"github.com/Chenglp/lumen/internal/domain"
)

// TestLoadHostProfileEmpty 首次启动时没有探测结果，必须能区分
// "还没探测过"和"读取失败"。
func TestLoadHostProfileEmpty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	_, err := db.LoadHostProfile(ctx)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("空表应返回 ErrNotFound，实际 %v", err)
	}
}

// TestHostProfileRoundTrip 落库再读回，所有决策字段必须一致。
//
// 这个测试存在的理由：这份 profile 是"这台机器能不能转码"的唯一依据，
// 少读一个字段的后果不是报错，而是**静默降级**——比如 chain_fps_1080
// 读成 0，机器就变成 X 档只做 remux，用户看到的是"我的电脑突然不支持转码了"。
func TestHostProfileRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	want := domain.HostProfile{
		HostMeasurements: domain.HostMeasurements{
			HasFFmpeg:      true,
			FFmpegPath:     `C:\tools\ffmpeg.exe`,
			FFmpegVersion:  "ffmpeg version 9.0.2",
			FFmpegFeatures: []string{"libplacebo", "zscale", "tonemap"},
			Encoders: []domain.EncoderInfo{
				{Name: "h264_nvenc", Listed: true, Usable: true, Verified: true, Hardware: true},
				{Name: "h264_qsv", Listed: true, Usable: false, Hardware: true, Why: "no device"},
				{Name: "libx264", Listed: true, Usable: true, Verified: true},
			},
			HWEncoder:     "h264_nvenc",
			SoftEncoder:   "libx264",
			DecodeFPS:     166.0,
			ChainFPS1080:  476.8,
			ChainFPS2160:  172.2,
			TonemapFPS:    85.9,
			SampleKind:    "real",
			SamplePath:    `F:\movies\a.mkv`,
			SampleHeight:  1608,
			SampleHDR:     "dv5",
			Cores:         8,
			MemMB:         32768,
			OnBattery:     false,
			DiskClass:     "hdd",
			DiskWriteMBps: 92.4,
			ProbeMS:       28854,
		},
		Tier:                 "S",
		MaxHeight:            2160,
		TranscodeConcurrency: 3,
		TonemapSlots:         1,
		Confidence:           "measured",
		Reasons:              []string{"tonemap 实测 85.9 fps，槽位锁 1 路"},
		ProbedAt:             1794335017158,
	}

	if err := db.SaveHostProfile(ctx, want); err != nil {
		t.Fatalf("SaveHostProfile: %v", err)
	}
	got, err := db.LoadHostProfile(ctx)
	if err != nil {
		t.Fatalf("LoadHostProfile: %v", err)
	}

	// 逐项比，而不是整体 ==：整体比只会说"不相等"，看不出是哪个字段丢了。
	checks := []struct {
		name      string
		got, want any
	}{
		{"Tier", got.Tier, want.Tier},
		{"MaxHeight", got.MaxHeight, want.MaxHeight},
		{"TranscodeConcurrency", got.TranscodeConcurrency, want.TranscodeConcurrency},
		{"TonemapSlots", got.TonemapSlots, want.TonemapSlots},
		{"Confidence", got.Confidence, want.Confidence},
		{"ChainFPS1080", got.ChainFPS1080, want.ChainFPS1080},
		{"ChainFPS2160", got.ChainFPS2160, want.ChainFPS2160},
		{"DecodeFPS", got.DecodeFPS, want.DecodeFPS},
		{"TonemapFPS", got.TonemapFPS, want.TonemapFPS},
		{"HWEncoder", got.HWEncoder, want.HWEncoder},
		{"SoftEncoder", got.SoftEncoder, want.SoftEncoder},
		{"SampleKind", got.SampleKind, want.SampleKind},
		{"SampleHDR", got.SampleHDR, want.SampleHDR},
		{"SampleHeight", got.SampleHeight, want.SampleHeight},
		{"Cores", got.Cores, want.Cores},
		{"MemMB", got.MemMB, want.MemMB},
		{"DiskClass", got.DiskClass, want.DiskClass},
		{"OnBattery", got.OnBattery, want.OnBattery},
		{"ProbeMS", got.ProbeMS, want.ProbeMS},
		{"ProbedAt", got.ProbedAt, want.ProbedAt},
		{"len(Encoders)", len(got.Encoders), 3},
		{"len(Features)", len(got.FFmpegFeatures), 3},
		{"len(Reasons)", len(got.Reasons), 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, 期望 %v", c.name, c.got, c.want)
		}
	}
	if len(got.Encoders) == 3 {
		if got.Encoders[0].Name != "h264_nvenc" || !got.Encoders[0].Verified {
			t.Errorf("编码器 JSON 读回不对: %+v", got.Encoders[0])
		}
		if got.Encoders[1].Usable || got.Encoders[1].Why != "no device" {
			t.Errorf("不可用编码器的原因没保留: %+v", got.Encoders[1])
		}
	}
}

// TestHostProfileUpsertSingleRow 重复探测必须覆盖而不是追加。
//
// host_profile 是单行表（CHECK id = 1）。如果写成"先删后插"，
// 两次探测之间服务可能读到空表，表现为"启动时档位未知然后恢复"——
// 这种偶发问题几乎不可能被用户描述清楚。
func TestHostProfileUpsertSingleRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	p := domain.HostProfile{HostMeasurements: domain.HostMeasurements{HasFFmpeg: true, ChainFPS1080: 60}}
	p.Tier, p.MaxHeight, p.TranscodeConcurrency = "M", 1080, 2
	if err := db.SaveHostProfile(ctx, p); err != nil {
		t.Fatalf("第一次 Save: %v", err)
	}

	p.Tier, p.MaxHeight, p.TranscodeConcurrency = "S", 2160, 3
	p.ChainFPS1080 = 254
	if err := db.SaveHostProfile(ctx, p); err != nil {
		t.Fatalf("第二次 Save: %v", err)
	}

	var n int
	if err := db.Read().QueryRowContext(ctx, `SELECT count(*) FROM host_profile`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("host_profile 有 %d 行，期望 1 行", n)
	}
	got, err := db.LoadHostProfile(ctx)
	if err != nil {
		t.Fatalf("LoadHostProfile: %v", err)
	}
	if got.Tier != "S" || got.ChainFPS1080 != 254 {
		t.Errorf("第二次探测没覆盖第一次: tier=%s chain=%v", got.Tier, got.ChainFPS1080)
	}
}

// TestHostProfileProbeErrorPersistsX X 档的失败原因必须能读回来。
//
// 32 屏的本机诊断要显示"为什么不能转码"。只存 tier=X 不存原因，
// 用户看到的是一个无法行动的结论。
func TestHostProfileProbeErrorPersistsX(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	p := domain.DecideHost(domain.HostMeasurements{HasFFmpeg: false})
	if p.Tier != "X" {
		t.Fatalf("无 ffmpeg 应为 X 档，实际 %s", p.Tier)
	}
	if err := db.SaveHostProfile(ctx, p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := db.LoadHostProfile(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Err == "" {
		t.Error("X 档必须带上失败原因，否则诊断页无从解释")
	}
	if got.MaxHeight != 720 {
		t.Errorf("X 档最高档位应为 720，实际 %d", got.MaxHeight)
	}
}
