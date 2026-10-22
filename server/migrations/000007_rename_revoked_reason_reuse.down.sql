ALTER TABLE refresh_tokens DROP CONSTRAINT refresh_tokens_revoked_reason_check;

UPDATE refresh_tokens SET revoked_reason = 'reuse' WHERE revoked_reason = 'reuse_detected';

ALTER TABLE refresh_tokens
    ADD CONSTRAINT refresh_tokens_revoked_reason_check
    CHECK (revoked_reason IN ('rotated', 'logout', 'reuse'));
