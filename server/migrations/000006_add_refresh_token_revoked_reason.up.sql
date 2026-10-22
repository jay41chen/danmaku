-- 為什麼作廢：只有 rotated 才適用 grace（回應掉了重送）；logout 掉的 token 再出現就是 reuse
--   rotated：/refresh 換新的   logout：/logout   reuse：偵測到外洩，整個 user 一起撤銷
-- 不改 000005 而是新增：migration 跑過就不能改（golang-migrate 只記版本號、不比對內容，
-- 已經在 v5 的 DB 會直接跳過改過的 000005 → schema 跟 code 對不上）
ALTER TABLE refresh_tokens
    ADD COLUMN revoked_reason TEXT CHECK (revoked_reason IN ('rotated', 'logout', 'reuse'));

-- 已經作廢的舊資料不知道原因：補成 logout（比較嚴格，不給 grace），不然下面的 CHECK 加不上去
UPDATE refresh_tokens SET revoked_reason = 'logout' WHERE revoked_at IS NOT NULL;

-- 兩個欄位要嘛都是 NULL（還有效）、要嘛都有值：不會出現「作廢了但不知道原因」
ALTER TABLE refresh_tokens
    ADD CONSTRAINT refresh_tokens_revoked_reason_check_pair
    CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL));
