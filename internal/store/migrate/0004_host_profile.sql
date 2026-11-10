-- 0004_host_profile.sql — 探测结果从"倍率"改成"吞吐"
--
-- 框架 §3.1 原来给 host_profile 存的是 soft_x264_ratio / soft_x264_ratio_4k
-- 两个倍率。R1 实测推翻了它：**倍率是相对帧率的，吞吐才是绝对的**。
-- 同一台机器在 24fps 片源上是 6–12×，在 60fps 的 HFR 片源上只剩 4.24×。
-- 只存倍率的话，一部 60fps 的电影会被判成"轻松可转"，两路并发就开始掉帧。
--
-- 所以：删掉两个倍率列（它们是 f(fps, 片源帧率) 的结果，不是独立事实），
-- 改存 fps；倍率在读取时按片源帧率现算。
--
-- 同时补上"为什么是这个数"的证据列：样本、置信度、解码与 tonemap 的实测值。
-- 32 屏的本机诊断要能回答"你凭什么说这台机器行"，
-- 只存一个档位字母是回答不了的。

ALTER TABLE host_profile ADD COLUMN ffmpeg_path       TEXT;
ALTER TABLE host_profile ADD COLUMN ffmpeg_version    TEXT;
ALTER TABLE host_profile ADD COLUMN ffmpeg_features   TEXT;   -- JSON: ["libplacebo","zscale",...]
ALTER TABLE host_profile ADD COLUMN hw_encoder        TEXT;   -- 选中的硬编，空表示只有软编
ALTER TABLE host_profile ADD COLUMN soft_encoder      TEXT;
ALTER TABLE host_profile ADD COLUMN decode_fps        REAL;   -- 仅解码
ALTER TABLE host_profile ADD COLUMN chain_fps_1080    REAL;   -- 4K→1080p 全链
ALTER TABLE host_profile ADD COLUMN chain_fps_2160    REAL;   -- 4K→2160p 全链
ALTER TABLE host_profile ADD COLUMN tonemap_fps       REAL;   -- DV P5→SDR 全链
ALTER TABLE host_profile ADD COLUMN tonemap_slots     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE host_profile ADD COLUMN sample_kind       TEXT;   -- real | synthetic | none
ALTER TABLE host_profile ADD COLUMN sample_path       TEXT;
ALTER TABLE host_profile ADD COLUMN sample_height     INTEGER;
ALTER TABLE host_profile ADD COLUMN sample_hdr        TEXT;   -- sdr | hdr10 | hlg | dv5 ...
ALTER TABLE host_profile ADD COLUMN confidence        TEXT;   -- measured | estimated
ALTER TABLE host_profile ADD COLUMN max_height        INTEGER NOT NULL DEFAULT 1080;
ALTER TABLE host_profile ADD COLUMN disk_class        TEXT;   -- ssd | hdd | network | unknown
ALTER TABLE host_profile ADD COLUMN disk_write_mbps   REAL;
ALTER TABLE host_profile ADD COLUMN on_battery        INTEGER NOT NULL DEFAULT 0;
ALTER TABLE host_profile ADD COLUMN probe_ms          INTEGER;
ALTER TABLE host_profile ADD COLUMN reasons           TEXT;   -- JSON: 决策依据，给 32 屏诊断用

-- 两个倍率列没有其他读者（S2 还没写），直接删掉，避免"两处都能算出倍率、
-- 但算法不同"这种之后一定会分叉的状态。
ALTER TABLE host_profile DROP COLUMN soft_x264_ratio;
ALTER TABLE host_profile DROP COLUMN soft_x264_ratio_4k;
