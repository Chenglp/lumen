package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Chenglp/lumen/internal/domain"
)

// SaveHostProfile 写入探测结果（单行表，id 恒为 1）。
//
// 用 upsert 而不是"先删后插"：两次探测之间服务可能正在读这一行，
// 删了再插会有一个窗口读到空表，表现为"刚启动时档位未知"，然后才恢复正常。
func (db *DB) SaveHostProfile(ctx context.Context, p domain.HostProfile) error {
	encoders, err := json.Marshal(p.Encoders)
	if err != nil {
		return fmt.Errorf("store: 序列化编码器列表: %w", err)
	}
	features, err := json.Marshal(p.FFmpegFeatures)
	if err != nil {
		return fmt.Errorf("store: 序列化 ffmpeg 能力: %w", err)
	}
	reasons, err := json.Marshal(p.Reasons)
	if err != nil {
		return fmt.Errorf("store: 序列化决策依据: %w", err)
	}

	_, err = db.rw.ExecContext(ctx, `
		INSERT INTO host_profile (
			id, tier, encoders, cores, mem_mb, transcode_concurrency,
			probed_at, probe_error,
			ffmpeg_path, ffmpeg_version, ffmpeg_features, hw_encoder, soft_encoder,
			decode_fps, chain_fps_1080, chain_fps_2160, tonemap_fps, tonemap_slots,
			sample_kind, sample_path, sample_height, sample_hdr, confidence, max_height,
			disk_class, disk_write_mbps, on_battery, probe_ms, reasons
		) VALUES (
			1, ?, ?, ?, ?, ?,
			?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?, ?
		)
		ON CONFLICT(id) DO UPDATE SET
			tier=excluded.tier, encoders=excluded.encoders, cores=excluded.cores,
			mem_mb=excluded.mem_mb, transcode_concurrency=excluded.transcode_concurrency,
			probed_at=excluded.probed_at, probe_error=excluded.probe_error,
			ffmpeg_path=excluded.ffmpeg_path, ffmpeg_version=excluded.ffmpeg_version,
			ffmpeg_features=excluded.ffmpeg_features, hw_encoder=excluded.hw_encoder,
			soft_encoder=excluded.soft_encoder, decode_fps=excluded.decode_fps,
			chain_fps_1080=excluded.chain_fps_1080, chain_fps_2160=excluded.chain_fps_2160,
			tonemap_fps=excluded.tonemap_fps, tonemap_slots=excluded.tonemap_slots,
			sample_kind=excluded.sample_kind, sample_path=excluded.sample_path,
			sample_height=excluded.sample_height, sample_hdr=excluded.sample_hdr,
			confidence=excluded.confidence,
			max_height=excluded.max_height, disk_class=excluded.disk_class,
			disk_write_mbps=excluded.disk_write_mbps, on_battery=excluded.on_battery,
			probe_ms=excluded.probe_ms, reasons=excluded.reasons`,
		p.Tier, string(encoders), p.Cores, p.MemMB, p.TranscodeConcurrency,
		p.ProbedAt, nullIfEmpty(p.Err),
		nullIfEmpty(p.FFmpegPath), nullIfEmpty(p.FFmpegVersion), string(features),
		nullIfEmpty(p.HWEncoder), nullIfEmpty(p.SoftEncoder),
		p.DecodeFPS, p.ChainFPS1080, p.ChainFPS2160, p.TonemapFPS, p.TonemapSlots,
		nullIfEmpty(p.SampleKind), nullIfEmpty(p.SamplePath), p.SampleHeight,
		nullIfEmpty(p.SampleHDR), nullIfEmpty(p.Confidence), p.MaxHeight,
		nullIfEmpty(p.DiskClass), p.DiskWriteMBps, boolToInt(p.OnBattery), p.ProbeMS,
		string(reasons),
	)
	if err != nil {
		return fmt.Errorf("store: 写入 host_profile: %w", err)
	}
	return nil
}

// LoadHostProfile 读回探测结果。没有结果时返回 ErrNotFound。
//
// 调用方应当把 ErrNotFound 当作"还没探测过"，而不是失败——
// 首次启动就是这个状态。
func (db *DB) LoadHostProfile(ctx context.Context) (domain.HostProfile, error) {
	var (
		p                                                         domain.HostProfile
		encoders, features, reasons                               sql.NullString
		probedAt, probeMS                                         sql.NullInt64
		cores, memMB, conc, tonemapSlots, maxHeight               sql.NullInt64
		sampleHeight                                              sql.NullInt64
		onBattery                                                 sql.NullInt64
		decodeFPS, chain1080, chain2160, tonemapFPS               sql.NullFloat64
		diskMBps                                                  sql.NullFloat64
		ffmpegPath, ffmpegVersion, hwEnc, softEnc                 sql.NullString
		sampleKind, samplePath, sampleHDR, confidence, disk, tier sql.NullString
		probeErr                                                  sql.NullString
	)
	err := db.ro.QueryRowContext(ctx, `
		SELECT tier, encoders, cores, mem_mb, transcode_concurrency, probed_at, probe_error,
		       ffmpeg_path, ffmpeg_version, ffmpeg_features, hw_encoder, soft_encoder,
		       decode_fps, chain_fps_1080, chain_fps_2160, tonemap_fps, tonemap_slots,
		       sample_kind, sample_path, sample_height, sample_hdr, confidence, max_height,
		       disk_class, disk_write_mbps, on_battery, probe_ms, reasons
		FROM host_profile WHERE id = 1`).
		Scan(&tier, &encoders, &cores, &memMB, &conc, &probedAt, &probeErr,
			&ffmpegPath, &ffmpegVersion, &features, &hwEnc, &softEnc,
			&decodeFPS, &chain1080, &chain2160, &tonemapFPS, &tonemapSlots,
			&sampleKind, &samplePath, &sampleHeight, &sampleHDR, &confidence, &maxHeight,
			&disk, &diskMBps, &onBattery, &probeMS, &reasons)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.HostProfile{}, fmt.Errorf("host_profile: %w", ErrNotFound)
	}
	if err != nil {
		return domain.HostProfile{}, fmt.Errorf("store: 读取 host_profile: %w", err)
	}

	p.Tier = tier.String
	p.Cores = int(cores.Int64)
	p.MemMB = int(memMB.Int64)
	p.TranscodeConcurrency = int(conc.Int64)
	p.ProbedAt = probedAt.Int64
	p.Err = probeErr.String
	p.FFmpegPath = ffmpegPath.String
	p.FFmpegVersion = ffmpegVersion.String
	p.HWEncoder = hwEnc.String
	p.SoftEncoder = softEnc.String
	p.DecodeFPS = decodeFPS.Float64
	p.ChainFPS1080 = chain1080.Float64
	p.ChainFPS2160 = chain2160.Float64
	p.TonemapFPS = tonemapFPS.Float64
	p.TonemapSlots = int(tonemapSlots.Int64)
	p.SampleKind = sampleKind.String
	p.SamplePath = samplePath.String
	p.SampleHeight = int(sampleHeight.Int64)
	p.SampleHDR = sampleHDR.String
	p.Confidence = confidence.String
	p.MaxHeight = int(maxHeight.Int64)
	p.DiskClass = disk.String
	p.DiskWriteMBps = diskMBps.Float64
	p.OnBattery = onBattery.Int64 != 0
	p.ProbeMS = probeMS.Int64

	if encoders.String != "" {
		_ = json.Unmarshal([]byte(encoders.String), &p.Encoders)
	}
	if features.String != "" {
		_ = json.Unmarshal([]byte(features.String), &p.FFmpegFeatures)
	}
	if reasons.String != "" {
		_ = json.Unmarshal([]byte(reasons.String), &p.Reasons)
	}
	return p, nil
}

// LargestMediaFile 返回已入库媒体里"最高、最大"的一个绝对路径。
//
// 探测拿它当样本。挑最高而不是随便一个：4K HEVC 10bit 是最难的解码组合，
// 用它测出来的数对其它片源都是保守的；拿 720p 老片测会把机器估强。
func (db *DB) LargestMediaFile(ctx context.Context) (path string, width, height int, size int64, err error) {
	var pathCol sql.NullString
	err = db.ro.QueryRowContext(ctx, `
		SELECT file_path, width, height, size_bytes
		FROM media
		WHERE file_path IS NOT NULL AND file_path <> ''
		ORDER BY height DESC, size_bytes DESC
		LIMIT 1`).Scan(&pathCol, &width, &height, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, 0, 0, fmt.Errorf("media: %w", ErrNotFound)
	}
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("store: 查询探测样本: %w", err)
	}
	return pathCol.String, width, height, size, nil
}

// LargestUnmatchedFile 返回扫描过但还没入库的文件里最大的一个。
//
// 这是 S1 阶段的取样回退：扫描器把文件写进 unmatched，匹配/刮削（S2）才会
// 生成 media 行。在 S2 之前 LargestMediaFile 一定是空的，而"探测需要样本"
// 这件事不能等 S2——用户装完就要知道这台机器能不能转码。
//
// 高度未知（unmatched 还没探测），所以只能按大小排。等 S2 落地后
// media 路径优先，那里有真实的高度信息。
func (db *DB) LargestUnmatchedFile(ctx context.Context) (path string, size int64, err error) {
	var mount, rootRel, rel sql.NullString
	err = db.ro.QueryRowContext(ctx, `
		SELECT v.mount_hint, l.root_rel, u.rel_path, u.size_bytes
		FROM unmatched u
		JOIN libraries l ON l.id = u.library_id
		JOIN volumes v ON v.id = u.volume_id
		WHERE u.missing_since IS NULL AND v.mount_hint IS NOT NULL
		ORDER BY u.size_bytes DESC
		LIMIT 1`).Scan(&mount, &rootRel, &rel, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("unmatched: %w", ErrNotFound)
	}
	if err != nil {
		return "", 0, fmt.Errorf("store: 查询探测样本: %w", err)
	}
	parts := []string{mount.String}
	if r := rootRel.String; r != "" && r != "." {
		parts = append(parts, filepath.FromSlash(r))
	}
	parts = append(parts, filepath.FromSlash(rel.String))
	return filepath.Join(parts...), size, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
