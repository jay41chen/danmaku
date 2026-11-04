-- 因應cursor pagination需求
-- 需要在原本的room table上增加新的index
CREATE INDEX rooms_created_at_public_id_idx
ON rooms(created_at, public_id)
WHERE deleted_at IS NULL;