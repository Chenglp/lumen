-- 0005_tmdb_cache.sql — TMDB 元数据与搜索缓存
--
-- 框架 §4.2 原写的是 tmdb_cache(tmdb_id, payload, fetched_at)。写到这里发现
-- 这个签名装不下第二类缓存：
--
--   详情缓存按 tmdb_id 索引（一部作品的多个版本、多集共用一条）；
--   搜索缓存按 (query, year) 索引，**根本没有单一 tmdb_id**——
--   一次搜索返回 10~20 条，正是一个文件在还没有 id 时才需要缓存的东西。
--
-- 而搜索缓存不是可选项：剧集库里同一部剧的 12 集会产生 12 次一模一样的
-- search/tv 请求（文件名里都没有剧名，解析结果当然一样）。R6 实测单次 p50
-- 647ms，这 11 次重复请求纯属浪费，而且全打在速率桶上。
--
-- 所以主键改成 cache_key，tmdb_id 降级为可空的辅助列（给「按 id 清缓存」用）。
-- 换语言时旧语言的行视为 miss 而不是删——契约 §5.2 要求渐进重抓、不阻塞界面，
-- 留着重抓失败还能回退到旧 payload。

CREATE TABLE tmdb_cache (
    cache_key  TEXT PRIMARY KEY,             -- 'detail:tv:128420' / 'search:tv:zh-CN:1f2a...'
    kind       TEXT NOT NULL,                -- detail | search | season | config
    media_type TEXT,                         -- movie | tv；搜索缓存按端点固定
    tmdb_id    INTEGER,                      -- 详情/季缓存有；搜索缓存为空
    language   TEXT NOT NULL,                -- 换语言后旧行自然 miss
    payload    TEXT NOT NULL,                -- 投影后的 JSON，不是 TMDB 原始响应
    fetched_at INTEGER NOT NULL              -- 毫秒；TTL 在读取时按当前设置现算
);

-- 清扫用：删 fetched_at 早于硬上界（180 天，条款要求不超 6 个月）的行。
CREATE INDEX idx_tmdb_cache_fetched ON tmdb_cache (fetched_at);

-- 「按作品清缓存」用：契约 §5.3 的 scope=metadata 是清全部，但单条重刮时
-- 要能只丢这一部。带 language 的复合索引，因为换语言后重抓是按语言的。
CREATE INDEX idx_tmdb_cache_id ON tmdb_cache (tmdb_id, language)
    WHERE tmdb_id IS NOT NULL;
