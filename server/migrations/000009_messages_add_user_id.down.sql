-- index 和 foreign key 會跟著欄位一起被 drop
ALTER TABLE messages DROP COLUMN IF EXISTS user_id;
