BEGIN;

-- 房間：對外用 public_id（11 字 base64url，網址 /{username}/{public_id}），內部 FK 用 id
--   owner_id ON DELETE RESTRICT：有房間的 user 不能被直接刪掉（房間和裡面的訊息不該默默消失）
--   deleted_at：soft delete，所有讀取都要加 deleted_at IS NULL
CREATE TABLE rooms(
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id TEXT NOT NULL UNIQUE,
    owner_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    room_name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);
-- Postgres 不會自動幫 foreign key 建 index：查「某 user 的房間」（含額度計算）要用
CREATE INDEX rooms_owner_id_idx ON rooms(owner_id);

-- 系統帳號 admin：不是平台管理員，只是用來擁有舊資料（既有訊息所屬的 legacy 房間）
-- password_hash 是 '!'：bcrypt 的輸出固定 60 字元且以 $2 開頭，'!' 永遠不是合法的 hash，
-- 所以任何密碼都比對不上、這個帳號無法登入（跟 Linux /etc/shadow 的做法一樣）
-- 刻意不用 ON CONFLICT：如果已經有人註冊了 admin，migration 要直接失敗，
-- 而不是默默把 legacy 房間掛到陌生人名下
INSERT INTO users(username, password_hash) VALUES ('admin', '!');

-- legacy 房間：公開房間，不需要猜不到；值符合 11 字 base64url 的 public_id 格式
INSERT INTO rooms(public_id, owner_id, room_name)
SELECT 'legacy00000', id, 'Legacy' FROM users WHERE username = 'admin';

-- 安全檢查：下一步的 DEFAULT 寫死 1，所以 legacy 房間的 id 一定要是 1
-- （它是這個 migration 裡剛建的表的第一列，正常就是 1）
DO $$
BEGIN
    IF (SELECT id FROM rooms WHERE public_id = 'legacy00000') IS DISTINCT FROM 1 THEN
        RAISE EXCEPTION 'legacy room id must be 1';
    END IF;
END
$$;

-- 既有的訊息全部填成 1（legacy 房間）
-- DEFAULT 1 的用途：Railway 部署時新舊版本會同時跑一小段時間，
--   舊版 server 的 INSERT 不帶 room_id，沒有 DEFAULT 的話舊版會全部寫入失敗
-- DEFAULT 長期會掩蓋「忘了帶 room_id」的 bug，所以等 messages API 開始寫 room_id 之後，
--   會在之後的 migration（000011）拿掉（expand / contract）
-- DEFAULT 不能是 subquery，所以才寫死 1，再靠上面的檢查保證它是對的
ALTER TABLE messages ADD COLUMN room_id BIGINT NOT NULL DEFAULT 1 REFERENCES rooms(id) ON DELETE RESTRICT;

-- 服務 WHERE room_id = ? ORDER BY id DESC LIMIT 50：不用再排序
-- 最左前綴 room_id 也涵蓋 FK 檢查，所以不另外建 (room_id) 的 index
CREATE INDEX messages_room_id_id_idx ON messages(room_id, id);

COMMIT;
