-- created_at：TIMESTAMP → TIMESTAMPTZ，並加上 NOT NULL
--   TIMESTAMP 只存時鐘時間、不記時區（像 Kotlin 的 LocalDateTime），TIMESTAMPTZ 存的是時間點（像 Instant）
--   model.Message.CreatedAt 是 time.Time 不是指標，只要有一筆 NULL，QueryMessage 的 Scan 就會失敗
BEGIN;
-- 先把null的欄位都改成now，不然下面的 SET NOT NULL 會失敗
UPDATE messages SET created_at = now() WHERE created_at IS NULL;
-- 把原本的欄位改型別
-- USING：舊值沒有時區，當成 UTC 解讀（Railway / docker postgres 的 TimeZone 都是 UTC，2026-10-19 確認）
ALTER TABLE messages
    ALTER COLUMN created_at TYPE TIMESTAMPTZ USING created_at AT TIME ZONE 'UTC',
    ALTER COLUMN created_at SET NOT NULL;

COMMIT;
