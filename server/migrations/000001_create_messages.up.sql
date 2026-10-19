-- 照「導入 migration 之前」手動建的 schema 寫（本機與 Railway 一致，2026-10-19 確認）
-- IF NOT EXISTS：全新 DB 會建表；已經有表的 DB 跳過，只把版本記成 1
-- 型別問題（SERIAL、TIMESTAMP、created_at 可為 NULL）刻意不在這裡修，交給 000002
CREATE TABLE IF NOT EXISTS messages (
    id         SERIAL PRIMARY KEY,
    content    TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT now()
);
