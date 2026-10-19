-- username 不分大小寫唯一：Jay 跟 jay 視為同一個 user
--   理由：不然有人可以註冊 Admin 冒充 admin
--   做法：unique index 建在 lower(username) 上，由 DB 保證，不管哪段 code 寫入都擋得住
--     - CREATE TABLE 裡的 UNIQUE 只能放欄位、不能放運算式，所以要另外 CREATE UNIQUE INDEX
--     - username 本身照原樣存，顯示時 Jay 還是 Jay；只有「比對是否重複」時轉小寫
--     - 查詢時要寫 WHERE lower(username) = lower($1) 才會用到這個 index
--   index 名稱 users_username_lower_key：沿用 PG 自動命名的慣例 <表>_<欄位>_key
--     （像 UNIQUE constraint 會自動叫 users_username_key），中間加 lower 表示是轉小寫後的唯一
--     重複註冊時錯誤訊息會帶這個名字（SQLSTATE 23505），Go 靠它判斷要回 409 而不是 500
--
-- password_hash 存 bcrypt 結果（固定 60 字元），不是密碼本身
-- id 有空號是正常的：sequence 不受 transaction 控制，INSERT 失敗（例如重複註冊）也會用掉一個號
CREATE TABLE users(
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_username_lower_key ON users(lower(username));
