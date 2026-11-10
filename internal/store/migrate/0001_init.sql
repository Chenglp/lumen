-- 0001_init.sql — 一期表结构（技术框架 §3.1）
--
-- 三条与本文件相关的约定：
--   1. 只存 volume_id + rel_path，绝对路径不入库（§1.2 ①）。
--   2. 所有路径类索引列用 path_key = sha256(volume_id ‖ \0 ‖ Fold(NFC(rel)))，见 domain.PathKey。
--   3. 时间一律 unix 毫秒 INTEGER；JSON 一律 TEXT；布尔一律 INTEGER 0/1。

PRAGMA foreign_keys = ON;

-- ─────────────────────────────────────────────────────────────
-- volumes — 媒体卷。volume_key 是稳定标识，mount_hint 只是提示。
-- ─────────────────────────────────────────────────────────────
CREATE TABLE volumes (
    id            TEXT PRIMARY KEY,              -- vol_ + ULID
    volume_key    TEXT NOT NULL UNIQUE,          -- Win volume serial / APFS VolumeUUID / blkid UUID / 网络挂载哈希
    kind          TEXT NOT NULL,                 -- local | removable | network
    mount_hint    TEXT,                          -- 最近见到的盘符或挂载点，仅供提示
    watch_mode    TEXT NOT NULL DEFAULT 'poll',  -- native | poll | off
    status        TEXT NOT NULL DEFAULT 'offline', -- online | offline
    read_mbps     REAL,
    first_byte_ms REAL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    CHECK (kind IN ('local','removable','network')),
    CHECK (watch_mode IN ('native','poll','off')),
    CHECK (status IN ('online','offline'))
);

-- ─────────────────────────────────────────────────────────────
-- libraries — 库根。唯一键是 (volume_id, root_rel)，不是 root_rel：
-- 两个卷上都可以有 media/tv。
-- ─────────────────────────────────────────────────────────────
CREATE TABLE libraries (
    id           TEXT PRIMARY KEY,               -- lib_ + ULID
    volume_id    TEXT NOT NULL REFERENCES volumes(id) ON DELETE CASCADE,
    root_rel     TEXT NOT NULL,                  -- 相对卷根，'/' 分隔
    path_key     TEXT NOT NULL,                  -- 同上算法，用于快速定位
    kind         TEXT NOT NULL,                  -- movie | tv | photo | music
    name         TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_scan_at INTEGER,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    CHECK (kind IN ('movie','tv','photo','music')),
    UNIQUE (volume_id, root_rel)
);

CREATE INDEX idx_libraries_volume ON libraries(volume_id, enabled);

-- ─────────────────────────────────────────────────────────────
-- unmatched — 待整理（契约组 A 的数据源）
-- ─────────────────────────────────────────────────────────────
CREATE TABLE unmatched (
    id             TEXT PRIMARY KEY,             -- um_ + ULID
    volume_id      TEXT NOT NULL REFERENCES volumes(id) ON DELETE CASCADE,
    library_id     TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path       TEXT NOT NULL,                -- 相对库根，'/' 分隔，原始大小写
    path_key       TEXT NOT NULL UNIQUE,         -- 去重的唯一依据
    size_bytes     INTEGER NOT NULL DEFAULT 0,
    mtime          INTEGER NOT NULL DEFAULT 0,   -- 增量扫描靠它，不是可选列
    media_type     TEXT NOT NULL DEFAULT 'unknown',
    status         TEXT NOT NULL DEFAULT 'pending',
    confidence     REAL NOT NULL DEFAULT 0,
    reason_code    TEXT,
    reason_params  TEXT,                         -- JSON
    parsed         TEXT,                         -- JSON，文件名解析结果
    probe          TEXT,                         -- JSON，ffprobe 原始结果
    thumb_path     TEXT,                         -- 相对 dataDir，非 URL
    ignored_until  INTEGER,                      -- TTL 预留，一期恒 NULL
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    CHECK (status IN ('pending','matched','ignored'))
);

CREATE INDEX idx_unmatched_status ON unmatched(status, created_at DESC);
CREATE INDEX idx_unmatched_lib    ON unmatched(library_id, status);
CREATE INDEX idx_unmatched_mtime  ON unmatched(mtime);

-- ─────────────────────────────────────────────────────────────
-- media — 已入库
--
-- 与框架原文的一处差异（有意为之）：原文写 "library_id + file_path 文件唯一索引"，
-- 但 §1.2 ① 明确"不存绝对路径"。两者冲突，按 §1.2 执行：
-- 用 volume_id + rel_path 作定位，path_key 作唯一键。
-- file_path 保留一列，仅作最近一次解析结果供 UI/排障，不参与任何唯一性判断。
-- ─────────────────────────────────────────────────────────────
CREATE TABLE media (
    id             TEXT PRIMARY KEY,             -- md_ + ULID
    kind           TEXT NOT NULL,                -- movie | episode | track | photo
    tmdb_id        INTEGER,
    title          TEXT NOT NULL,
    original_title TEXT,
    year           INTEGER,
    season         INTEGER,
    episode        INTEGER,
    library_id     TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    volume_id      TEXT NOT NULL REFERENCES volumes(id) ON DELETE CASCADE,
    rel_path       TEXT NOT NULL,
    path_key       TEXT NOT NULL UNIQUE,
    file_path      TEXT,                         -- 最近一次解析出的绝对路径，仅用于展示/排障
    duration_ms    INTEGER NOT NULL DEFAULT 0,   -- 播放决策与 ETA 全靠这三列
    size_bytes     INTEGER NOT NULL DEFAULT 0,
    bitrate_kbps   INTEGER NOT NULL DEFAULT 0,
    width          INTEGER NOT NULL DEFAULT 0,
    height         INTEGER NOT NULL DEFAULT 0,
    video_codec    TEXT,
    audio_codec    TEXT,
    container      TEXT,
    hdr            TEXT,                         -- hdr10 | dv | hlg | NULL
    probe          TEXT,                         -- JSON
    thumb_path     TEXT,
    added_at       INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    CHECK (kind IN ('movie','episode','track','photo'))
);

CREATE INDEX idx_media_tmdb ON media(tmdb_id) WHERE tmdb_id IS NOT NULL;
CREATE INDEX idx_media_ep   ON media(tmdb_id, season, episode);
CREATE INDEX idx_media_lib  ON media(library_id, added_at DESC);

-- FTS5 走独立虚表 + 触发器同步。
-- 不要用 content=media 直接绑：媒体表结构会变，external content 表在 ALTER 后会读出垃圾。
CREATE VIRTUAL TABLE media_fts USING fts5(
    title,
    original_title,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER media_ai AFTER INSERT ON media BEGIN
    INSERT INTO media_fts(rowid, title, original_title)
    VALUES (new.rowid, new.title, COALESCE(new.original_title, ''));
END;

CREATE TRIGGER media_au AFTER UPDATE OF title, original_title ON media BEGIN
    DELETE FROM media_fts WHERE rowid = old.rowid;
    INSERT INTO media_fts(rowid, title, original_title)
    VALUES (new.rowid, new.title, COALESCE(new.original_title, ''));
END;

CREATE TRIGGER media_ad AFTER DELETE ON media BEGIN
    DELETE FROM media_fts WHERE rowid = old.rowid;
END;

-- ─────────────────────────────────────────────────────────────
-- tasks — 任务（契约组 B）
--
-- progress 落库，但只在 checkpoint 时写；rate/eta 不落库，重启后由进度差重算。
-- can_pause/can_cancel/can_retry 由 kind+state 派生，不存。
-- ─────────────────────────────────────────────────────────────
CREATE TABLE tasks (
    id                  TEXT PRIMARY KEY,        -- tk_ + ULID
    kind                TEXT NOT NULL,
    state               TEXT NOT NULL,
    title               TEXT NOT NULL,
    priority            INTEGER NOT NULL DEFAULT 0,
    target              TEXT,                    -- JSON: libraryId / path / fileCount / sizeBytes
    progress_done       INTEGER NOT NULL DEFAULT 0,
    progress_total      INTEGER NOT NULL DEFAULT 0,
    unit                TEXT NOT NULL DEFAULT 'files',
    percent             REAL NOT NULL DEFAULT 0,
    pause_requested_at  INTEGER,                 -- 协作式暂停的唯一凭据
    result_code         TEXT,
    result_params       TEXT,                    -- JSON
    needs_attention     TEXT,                    -- JSON，29 屏「需处理 ›」
    error_code          TEXT,
    error_message       TEXT,
    engine_log_tail     TEXT,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    CHECK (state IN ('queued','running','paused','done','failed','canceled'))
);

CREATE INDEX idx_tasks_queue ON tasks(state, priority DESC, created_at);

-- ─────────────────────────────────────────────────────────────
-- playback_sessions — 内存为主，此表用于崩溃恢复与 30 屏推断
-- ─────────────────────────────────────────────────────────────
CREATE TABLE playback_sessions (
    id                 TEXT PRIMARY KEY,         -- ps_ + ULID
    media_id           TEXT NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    device_id          TEXT NOT NULL,
    mode               TEXT NOT NULL,            -- direct | remux | transcode
    resume_position_ms INTEGER NOT NULL DEFAULT 0,
    progress_token     TEXT NOT NULL,            -- 防并发乱序回写
    transcode_job_id   TEXT,
    start_segment      INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    last_seen_at       INTEGER NOT NULL,
    CHECK (mode IN ('direct','remux','transcode'))
);

CREATE INDEX idx_sessions_gc ON playback_sessions(last_seen_at);

-- ─────────────────────────────────────────────────────────────
-- playback_progress — 续播点，独立于会话（会话会 GC，进度必须留）
-- 同时是 30 屏「正在使用」的唯一推断来源：
-- playing=1 AND now-updated_at<90s 就是"正在播"。
-- ─────────────────────────────────────────────────────────────
CREATE TABLE playback_progress (
    media_id    TEXT NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    device_id   TEXT NOT NULL,
    position_ms INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    playing     INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (media_id, device_id)
);

CREATE INDEX idx_progress_playing ON playback_progress(playing, updated_at DESC);

-- ─────────────────────────────────────────────────────────────
-- devices — 字段照契约 Device schema
-- ─────────────────────────────────────────────────────────────
CREATE TABLE devices (
    id                    TEXT PRIMARY KEY,      -- dev_ + ULID
    name                  TEXT NOT NULL,
    kind                  TEXT NOT NULL,         -- DeviceKind 枚举
    platform              TEXT NOT NULL,         -- macos|ios|ipados|tvos|windows|linux|android|web
    platform_version      TEXT,
    app_version           TEXT,
    is_self               INTEGER NOT NULL DEFAULT 0,
    online                INTEGER NOT NULL DEFAULT 0,
    last_seen_at          INTEGER,
    network               TEXT,                  -- lan | tailscale | public
    address               TEXT,
    permissions           TEXT NOT NULL DEFAULT '{}', -- JSON
    activity              TEXT,
    last_activity_at      INTEGER,
    last_activity_summary TEXT,
    paired_at             INTEGER,
    revoked_at            INTEGER,
    CHECK (network IS NULL OR network IN ('lan','tailscale','public'))
);

CREATE INDEX idx_devices_seen ON devices(revoked_at, last_seen_at DESC);

-- ─────────────────────────────────────────────────────────────
-- diagnoses — 字段照契约 Diagnosis schema
-- ─────────────────────────────────────────────────────────────
CREATE TABLE diagnoses (
    id                  TEXT PRIMARY KEY,        -- dg_ + ULID
    verdict             TEXT NOT NULL,
    reason_code         TEXT NOT NULL,
    reason_params       TEXT,                    -- JSON
    failed_at_ms        INTEGER NOT NULL,
    resume_position_ms  INTEGER,
    detail              TEXT,                    -- JSON
    actions             TEXT,                    -- JSON
    log_ref             TEXT,
    expires_at          INTEGER NOT NULL,        -- 过期即删，不无限积累
    created_at          INTEGER NOT NULL
);

CREATE INDEX idx_diagnoses_expiry ON diagnoses(expires_at);

-- ─────────────────────────────────────────────────────────────
-- settings — KV + JSON。组 E 的 MetadataSettings 落成几条 key。
-- 凭据不落这里：token 走独立列 + 单独读写路径，避免 GET /settings/metadata 顺手返回。
-- ─────────────────────────────────────────────────────────────
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,                    -- JSON
    updated_at INTEGER NOT NULL
);

CREATE TABLE secrets (
    key        TEXT PRIMARY KEY,                 -- 如 tmdb_token
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- ─────────────────────────────────────────────────────────────
-- host_profile — 单行表（id = 1）。探测很贵，结果落库，启动读库。
-- ─────────────────────────────────────────────────────────────
CREATE TABLE host_profile (
    id                    INTEGER PRIMARY KEY CHECK (id = 1),
    tier                  TEXT NOT NULL,         -- S | M | L | X
    encoders              TEXT,                  -- JSON: [{name, usable, verified}]
    soft_x264_ratio       REAL,
    soft_x264_ratio_4k    REAL,
    cores                 INTEGER,
    mem_mb                INTEGER,
    transcode_concurrency INTEGER NOT NULL DEFAULT 1,
    probed_at             INTEGER,
    probe_error           TEXT,                  -- 非空时按 X 档保守运行
    CHECK (tier IN ('S','M','L','X'))
);
