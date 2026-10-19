-- 回到 000001 的 schema（TIMESTAMP、可為 NULL）
-- ⚠️ 只還原 schema：up 把 NULL 補成 now() 的那幾筆，已經分不出原本是 NULL，不會變回 NULL
BEGIN;
-- 把改型別的operation改回來
-- USING 一樣寫 'UTC'：把時間點換回 UTC 的時鐘時間，跟 up 的假設對稱
-- 不寫的話會用當下 session 的 TimeZone 換算，在非 UTC 的環境跑 down，時間會偏掉
ALTER TABLE messages
    ALTER COLUMN created_at TYPE TIMESTAMP USING created_at AT TIME ZONE 'UTC',
    ALTER COLUMN created_at DROP NOT NULL;

COMMIT;
