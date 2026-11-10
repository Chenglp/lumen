-- 0003_media_mtime.sql — media 也需要 mtime
--
-- 框架 §4.1 说增量扫描「只认 mtime + size 两列」，但 §3.1 的 media 表
-- 只列了 size_bytes，没有 mtime；unmatched 有。
-- 缺了它，已入库的文件内容变化就检测不到——只能靠 size 判断，
-- 而重压一遍的片子 size 完全可能一致，于是永远不更新。
--
-- 默认 0 而不是 NOT NULL 无默认：已有行填 0 会在下一次扫描时被判为"变化"，
-- 触发一次重探。这是一次性的、可接受的代价，比让它们永远不再被检查好。

ALTER TABLE media ADD COLUMN mtime INTEGER NOT NULL DEFAULT 0;

-- 增量扫描会按库 + mtime 比对，补一个覆盖索引避免全表扫。
CREATE INDEX idx_media_mtime ON media(library_id, mtime);
