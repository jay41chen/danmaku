-- 訊息記下是誰發的
--   nullable：舊資料沒有發送者，補不出來；之後 user 被刪掉也會變成 NULL
--   ON DELETE SET NULL：刪 user 時訊息保留（聊天紀錄不該跟著消失），user_id 變 NULL，
--     client 看到 username 是 null 就顯示「已刪除的用戶」
--     （跟 refresh_tokens 的 CASCADE 不同：token 沒有 user 就沒意義，訊息有）
ALTER TABLE messages ADD COLUMN user_id BIGINT REFERENCES users(id) ON DELETE SET NULL;

-- Postgres 不會自動幫 foreign key 建 index（MySQL 會）
-- 沒有的話：刪 user 時要把他的訊息 SET NULL，得全表掃描 messages
-- 另外 W4b 會按 user 查訊息（WHERE user_id = ?）
CREATE INDEX messages_user_id_idx ON messages(user_id);
