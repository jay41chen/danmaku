BEGIN;

-- 拿掉default的id以及他的seq
ALTER TABLE messages 
    ALTER COLUMN id DROP DEFAULT;
DROP SEQUENCE messages_id_seq;

-- 改用新的IDENTITY id
ALTER TABLE messages 
    ALTER COLUMN id TYPE BIGINT,
    -- 這行代表id是由DBgenera，且視為自動編號欄
    ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY;

-- 更改value，
-- `pg_get_serial_sequence`的功用是「問 Postgres「這個欄位現在用的計數器叫什麼名字？」
-- 這邊最後的is_called一定要是false，不然在第一筆插入的時候(null -> 0)會出錯(需從1開始)
SELECT setval(pg_get_serial_sequence('messages','id'), COALESCE(MAX(id), 0) + 1, false)
FROM messages;

COMMIT;