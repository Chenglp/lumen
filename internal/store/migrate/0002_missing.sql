-- 0002_missing.sql — 软删除标记（扫描器需要区分「文件消失」与「读不到」）
--
-- 为什么不直接 DELETE：
--   playback_progress 的外键是 ON DELETE CASCADE，删 media 会连带删掉续播点。
--   拔一次移动硬盘 = 整卷观影记录清空，这是最不可接受的一种数据损失。
--   所以消失只打标记，真正的清理由用户在 29 屏「需处理」里显式确认。
--
-- 为什么必须能区分「读不到」：
--   权限不足、网络盘断链、卷未就绪，都会让 stat 失败，但文件还在。
--   只有遍历完整跑完（Partial=false）时，没见到的才算真消失。

ALTER TABLE media     ADD COLUMN missing_since INTEGER;
ALTER TABLE unmatched ADD COLUMN missing_since INTEGER;

-- 部分索引：绝大多数行 missing_since 为 NULL，不该为它们占索引空间。
CREATE INDEX idx_media_missing     ON media(missing_since)     WHERE missing_since IS NOT NULL;
CREATE INDEX idx_unmatched_missing ON unmatched(missing_since) WHERE missing_since IS NOT NULL;
