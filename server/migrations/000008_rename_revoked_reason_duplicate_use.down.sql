ALTER TABLE refresh_tokens DROP CONSTRAINT refresh_tokens_revoked_reason_check;

UPDATE refresh_tokens SET revoked_reason = 'reuse_detected' WHERE revoked_reason = 'duplicate_use_detected';

ALTER TABLE refresh_tokens
    ADD CONSTRAINT refresh_tokens_revoked_reason_check
    CHECK (revoked_reason IN ('rotated', 'logout', 'reuse_detected'));
