-- refresh token：一個 user 有很多個（每台裝置、每次 rotation 各一列）
CREATE TABLE refresh_tokens(
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- user_id 型別要跟 users.id 一致（BIGINT）才能加 foreign key
    --   ON DELETE CASCADE：刪 user 時一起刪他的 token；不加的話預設 RESTRICT，有 token 的 user 刪不掉
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    -- revoked_at：NULL = 還有效，有值 = 幾點作廢的
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Postgres 不會自動幫 foreign key 建 index（MySQL 會）
-- 用到的地方：reuse 時撤銷整個 user（WHERE user_id = ?）、刪 user 時的 CASCADE
-- expires_at 不加：清理一天一次，這個規模掃整張表不到 1ms，多一個 index 每次 INSERT 都要多更新
CREATE INDEX refresh_tokens_user_id_idx ON refresh_tokens(user_id);
