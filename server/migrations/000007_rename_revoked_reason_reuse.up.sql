-- 'reuse' → 'reuse_detected'：這個值寫在「被連帶撤銷」的列上（例如另一台裝置的 token），
-- 那些 token 本身沒有被 reuse；寫 reuse 會讓查 DB 的人以為是它外洩了
-- 被重送的那個舊 token 維持 rotated（它早就作廢，WHERE revoked_at IS NULL 改不到它）
-- 不直接改 000006：migration 跑過就不改，一律新增
ALTER TABLE refresh_tokens DROP CONSTRAINT refresh_tokens_revoked_reason_check;

UPDATE refresh_tokens SET revoked_reason = 'reuse_detected' WHERE revoked_reason = 'reuse';

-- 沿用 000006 時 PG 自動取的名字，down 才找得到同一個
ALTER TABLE refresh_tokens
    ADD CONSTRAINT refresh_tokens_revoked_reason_check
    CHECK (revoked_reason IN ('rotated', 'logout', 'reuse_detected'));
