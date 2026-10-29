-- 還原 000010 的 DEFAULT 1（只還原 default，不影響已寫入的資料）
ALTER TABLE messages ALTER COLUMN room_id SET DEFAULT 1;
