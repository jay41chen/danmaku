-- 000010 的 expand / contract 中的 contract 步驟
--   000010 加 room_id 時寫了 DEFAULT 1，只是為了讓 Railway 部署時還在跑的舊版 server
--   （INSERT 不帶 room_id）能繼續寫入；現在所有在跑的程式碼都會明確帶 room_id，
--   所以拿掉 DEFAULT。之後任何 INSERT 忘了帶 room_id，會直接因為 NOT NULL 失敗，
--   而不是默默寫進 legacy 房間。
ALTER TABLE messages ALTER COLUMN room_id DROP DEFAULT;
