-- 這張表放「這個 user 的憑證狀態」。
-- 目前只有 valid_after：這個時間點（含）以前發的 refresh token 一律無效（撤銷全部 = 只寫這一欄）。
-- 之後 Ban / 強制登出也放這裡。
-- 沒有那一列 = 這個 user 從來沒被撤銷過。
--
-- 鎖不在這張表：不存在的列鎖不到，所以同一個 user 的請求是用 users 那一列排隊（store.lockUser），
-- 這張表只放資料。
--
-- 只新增表、舊版 server 不會讀它 → 部署一次就好，不需要 expand / contract。
-- Known issue：部署期間新舊版同時在線的那段時間，舊版處理 refresh 不知道這張表。
CREATE TABLE user_credential_state(
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    valid_after TIMESTAMPTZ NOT NULL
);

-- backfill：這個 migration 之前的撤銷全部是把 token 標成 duplicate_use_detected、沒有 valid_after。
-- 不補的話，攻擊者手上的舊 token 部署後再送來，會再觸發一次撤銷，把正牌使用者重新登入的 session 踢掉。
-- 用每個 user 最後一次撤銷的時間當 valid_after（那次撤銷的所有列 revoked_at 相同），
-- 之後重新登入發的 token 不受影響。
-- 加 max(created_at) 取較晚者：舊版撤銷用 now()（撤銷 tx 開始的時間），撤銷 tx 開始之後、UPDATE 之前
-- commit 的 token，created_at 會晚於 revoked_at 卻被標成 duplicate_use_detected；
-- 取兩者較晚的，才能讓所有被舊版標記的 token 都落在 valid_after 之前。
INSERT INTO user_credential_state(user_id, valid_after)
SELECT user_id, GREATEST(max(revoked_at), max(created_at))
FROM refresh_tokens
WHERE revoked_reason = 'duplicate_use_detected'
GROUP BY user_id;
