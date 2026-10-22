-- 欄位上的 CHECK constraint 會跟著欄位一起被 drop
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS revoked_reason;
