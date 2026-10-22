-- 'reuse_detected' → 'duplicate_use_detected'：reuse 在中文容易讀成「復用」，
-- 改成描述觀察到的事實（同一張被用了兩次），跟 Go 的 tokenDuplicateUse / log 訊息同一個詞
-- 不直接改 000007：migration 跑過就不改，一律新增
ALTER TABLE refresh_tokens DROP CONSTRAINT refresh_tokens_revoked_reason_check;

UPDATE refresh_tokens SET revoked_reason = 'duplicate_use_detected' WHERE revoked_reason = 'reuse_detected';

-- 沿用 000006 時 PG 自動取的名字，down 才找得到同一個
ALTER TABLE refresh_tokens
    ADD CONSTRAINT refresh_tokens_revoked_reason_check
    CHECK (revoked_reason IN ('rotated', 'logout', 'duplicate_use_detected'));
