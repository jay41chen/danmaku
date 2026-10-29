BEGIN;

DROP INDEX IF EXISTS messages_room_id_id_idx;
ALTER TABLE messages DROP COLUMN IF EXISTS room_id;
DROP TABLE IF EXISTS rooms;
-- 只刪 up 建的系統帳號（password_hash 是 '!'），不會誤刪別人註冊的 admin
DELETE FROM users WHERE username = 'admin' AND password_hash = '!';

COMMIT;
