-- 房間角色：房管（moderator）和被 ban（banned）；房主看 rooms.owner_id，沒有列就是一般觀眾
-- 一人一房一列，不會同時是房管又被 ban
CREATE TABLE room_roles (
    room_id     BIGINT      NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT        NOT NULL CHECK (role IN ('moderator', 'banned')),
    assigned_by BIGINT      REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, user_id)
);
