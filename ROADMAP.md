# Roadmap

## W1 — 基礎彈幕系統 ✅

- [x] Go HTTP server（net/http）
- [x] WebSocket 即時廣播（gorilla/websocket）
- [x] PostgreSQL 訊息持久化（pgx）
- [x] Redis Pub/Sub 跨 instance 廣播
- [x] REST API（POST /messages、GET /messages、GET /healthcheck）
- [x] React 前端（TypeScript + Vite）
- [x] 部署：Railway（server）+ GitHub Pages（前端）
- [x] CORS 處理

## W2 — 架構改善 & 多 instance 驗證 ✅

- [x] DI 重構（interface 抽取、依賴注入）
- [x] Dockerfile（layer caching 優化）
- [x] Docker Compose 編排（PostgreSQL、Redis、server x2、nginx）
- [x] Nginx reverse proxy + load balancer（least_conn）
- [x] WebSocket 透過 nginx 轉發（upgrade headers）
- [x] 跨 instance WebSocket 廣播驗證

## W3 — Structured Logging + Metrics + Error Handling ✅

- [x] Structured logging（slog，JSON format）
- [x] Log aggregation（Promtail → Loki → Grafana） ← local dev only
- [x] Metrics（Prometheus + Grafana） ← local dev only
- [x] Production error handling 強化
  - [x] Sanitize error responses（不洩漏內部錯誤訊息）
  - [x] Panic recovery middleware
  - [x] Graceful shutdown（SIGTERM/SIGINT + 10s timeout）

## Pre-W4 — 重構

- [x] 移除 ApplicationStore，改用 function params DI（Go 慣例）
- [x] WebSocket race condition 修復（upgrade 先、lock 內 check + add）
- [x] SetReadLimit 防止過大訊息
- [x] API method validation（405 Method Not Allowed）
- [x] Redis Subscribe exponential backoff retry

## W4a — Middleware + Migration + Auth ✅

- [x] HTTP middleware pattern（request logging + request ID）
  - [x] Middleware chain + Request ID（`X-Request-ID`，沿用合法的 client header，否則自動產生）
  - [x] Request logging（依 status 決定 level，略過 `/metrics`）
  - [x] ContextHandler：handler 的 log 透過 ctx 自動帶 request_id（WS 同一條連線共用同一個 ID）
  - [x] `http.Server.ErrorLog` 接 slog（ERROR level）
  - [x] CORS middleware（取代 handler 內的 `setCors`）：allowlist + `Vary: Origin`、preflight 回 204、`CORS_ORIGINS` 設定錯誤時 fail fast
  - [x] Panic recovery 修正（變數改名、`ErrorContext`、`http.ErrAbortHandler` re-panic）
  - [x] StatusCodeRecorder 修正：記錄 client 實際收到的 status（只記第一次 `WriteHeader`、`Write` 隱含 200）
  - [x] Nginx 傳遞 `X-Request-ID`（原樣轉發，驗證只在 server；沒帶才補 `$request_id`；access log 記 server 回傳的 ID）
  - [x] Review 修正
    - [x] StatusCodeRecorder：Hijack 後維持 101，WS 連線期間的 panic 不會被誤記成 500
    - [x] WS connection gauge：broadcast 寫入失敗與讀取失敗都會 remove，只減一次
    - [x] CORS origin 必須是 canonical 寫法（大寫、預設 port、空 port、query / fragment / userinfo 啟動失敗）
    - [x] Broadcast 加 10s write deadline，避免單一卡住的 client 無限期佔住 lock
    - [x] 補 test：Chain 順序、RequestID middleware、RecoverPanic（含 `ErrAbortHandler`）
- [x] Database migration tool（golang-migrate）：SQL embed 進 binary、啟動時自動跑；DB URL 用 pgx 解析、Redis URL 錯誤時不洩漏密碼
- [x] Authentication（JWT middleware）
  - [x] `/register`、`/login`（bcrypt、timing 防帳號列舉、rate limit）
  - [x] Access token（JWT HS256，1 小時，`JWT_SECRET` fail fast）
  - [x] Refresh token（opaque、存 SHA-256、7 天、rotation + duplicate use detection、15s grace）、`/refresh`、`/logout`
  - [x] `requireAuth` + route helper（`post` 預設要登入、`publicGet` 公開）、加回 `POST /messages`、廣播改 JSON；WS 寫入一律拒絕、回 error frame 不斷線（Step 8）
  - [x] WS 驗證：token 可選，連線內送 auth frame（失敗 / 過期退回匿名、不斷線；過期 timer 拿鎖後重查 exp）；會員 / 非會員分開統計（`websocket_viewers`，只算單台 server；跨 server 去重見 W6）（Step 9）
  - [x] Client：存 token、401 自動 refresh（Step 10）
    - 發彈幕改走 `POST /messages`；WS 訊息格式 `{type, payload}`（`message` / `error`，error 的 `code` 如 `ws_write_forbidden` 可接 popup）
    - POST 回 201 + payload，WS 廣播也會再收到一次 → 用 `payload.id` 去重；依 `id` 插入而不是依到達順序 append（response / 廣播都可能晚到）
    - `username: null` → client 顯示「匿名」（000009 之前的匿名彈幕和已刪除的 user 目前不區分，見 W4b）
    - Token 存放（Step 10 定案）：access token 只放 memory；refresh token 放 `localStorage`（F5 / 重開頁面時打 `/refresh` 換新的）。防 XSS 靠源頭：彈幕一律純文字渲染（禁用 `dangerouslySetInnerHTML`）+ `index.html` 的 CSP meta；損害由 rotation + duplicate use 限制
    - 新增 `GET /me`（要登入）：F5 後取得目前 user
    - 上線檢查（2026-10-29 已驗證：replicas = 2，不同台的 A 發、B 收）：前端部署後在 Railway 開 replicas = 2，實測跨 instance（不需要 sticky session：WS 建立後固定在一台、廣播走 Redis、限流 / refresh 狀態在 Redis / Postgres）
  - [ ] 跨分頁登出同步：refresh token 在 `localStorage`，同一個瀏覽器的分頁共用。A 分頁登出後，B 分頁的 access token 還在 memory（JWT stateless，server 撤不掉）→ B 還能發彈幕，最多到 B 下一次 refresh（約 55 分鐘）才發現 refresh token 不見而登出
    - 方向：監聽 `window` 的 `storage` 事件（只有**其他**分頁改 `localStorage` 才會觸發），refresh token 被刪 → 本分頁 `clearAll()`（WS 跟著降回訪客）
    - 也可用 `BroadcastChannel`；兩者都只在同一個瀏覽器內有效，跨裝置登出要靠 server（W4b「登出所有裝置」）
  - [ ] Refresh token 改 HttpOnly cookie：前端 `github.io`、後端 `railway.app` 是 cross-site → third-party cookie，Safari 預設擋。等有自訂網域（同 site）再改，同時處理 CSRF（`SameSite`）、CORS `Allow-Credentials`、WS `CheckOrigin`
  - WS `CheckOrigin` 目前全部放行：若改用 cookie 驗證，要依 `CORS_ORIGINS` 檢查 Origin，防 CSWSH（跨站 WebSocket 劫持）；token 放在 header / query 則不受影響
  - Refresh token 延後的項目：
    - 清理 job：作廢 / 過期的列保留到 `expires_at` 之後再定期 `DELETE`（量大時改成依日期 partition、整個 drop）
    - 「登出所有裝置」endpoint：store 已有 `WHERE user_id` 撤銷全部的 SQL（duplicate use 用），要再決定要不要驗證密碼
      - 到時候 `revoked_reason` 會變成 Go 的參數（例如 `revokeAll(userID, reason)`）→ 改成 typed string const。目前值只出現在 SQL 字串裡：寫入打錯有 CHECK 擋，讀取端（r2 的 `in_grace`）打錯 DB 不會報錯，只靠 store 測試擋
    - client（Step 10）：refresh 要 single-flight；登出前要等進行中的 refresh 結束，否則登出後那個 refresh 會被當成 duplicate use、把所有裝置一起登出
      - refresh 的重試總時間必須小於 server grace（15s）：照一般 API 的 5s timeout × 3 retry，第 3 次約在 15s 才到 → 被當成 duplicate use、所有裝置登出。改 client，不加大 grace（grace = 攻擊者拿舊 token 換新的不被抓的時間）
      - 最外層加總 deadline，但要比 grace 小並留緩衝（例如 10s）：client 從「送出」起算、grace 從 server 的 `revoked_at` 起算，而且取消 request 不會讓已送出的封包消失
      - deadline 到期後丟掉舊 token、跳登入頁：舊 token 可能已被 rotate，下次再拿它 refresh 會被當成 duplicate use，把所有裝置登出（猜錯的代價：只有這台要重新登入 vs 全部裝置）
      - client deadline 與 server grace 各寫一個 const、註解互相指向；有 mobile app（舊版殘留）時才值得改成 remote config，原則是 server 為準、放寬 grace 安全、縮短要等舊 client 淘汰

## W4b — Room + Authorization

> Room 排在 RBAC 前面：有「房主」之後，權限規則才具體（房主刪訊息、踢人）

- [ ] Room 概念（房間 + 房主）
  - 房間 = 長期存在的 channel（跟著擁有者，不是一次直播）；「一次直播」之後需要時再加 `streams` 子實體 + `messages.stream_id`（見 go-notes 3.28）
  - 網址 `/{username}/{public_id}`：只用 `public_id`（11 字元 base64url，`randx.URLText(8)` 產生，跟 refresh token 共用，unique index 擋碰撞）找房間，username 對不上就 301 導向；內部 PK / FK 仍用 `BIGINT id`
    - **暫時做法（待改）**：前端先用 `?room={public_id}`，沒帶就進 `legacy00000`。原因：GitHub Pages 是純靜態 hosting，直接開 `/{username}/{public_id}` 這種深層路徑會 404（沒有 SPA fallback）。等有自訂網域 / 換 hosting，或用 `404.html` 轉址做 SPA fallback 時，改回上面的路徑格式
    - ✅ 彈幕 API 改成 `POST` / `GET /rooms/{public_id}/messages`（2c17ac8），前端已換過去（91b3940）
    - [ ] 移除已棄用的 `/messages`（route、handler、test）：目前固定讀寫 legacy room，暫時保留
    - **Known issue（暫時）**：WS 仍是全站一個 Redis channel，在房間 X 的觀眾會收到所有房間的彈幕 → W4b 即時部分（Pub/Sub channel 切法）修掉
  - [ ] 列出房間的 API + 選房間的 UI：目前只能靠 `POST /rooms` 回傳的 `public_id` 拼網址進房間
  - schema（見 go-notes 3.28）：`rooms(id, public_id, owner_id → users ON DELETE RESTRICT, room_name, created_at, deleted_at)` + `owner_id` index；名稱可重複，空白 / 長度由程式檢查
  - 每人房間數上限：transaction 內 `SELECT ... FROM users WHERE id = $1 FOR UPDATE` → 數 → INSERT，只能透過共用 method（恢復房間也走同一條）；超過回 403
  - 彈幕綁房間：`messages.room_id BIGINT NOT NULL REFERENCES rooms(id)`；舊彈幕歸進 legacy room（`public_id = 'legacy00000'`、名稱 `Legacy`，owner 是 migration 建的系統帳號 `admin`，`password_hash = '!'` 不能登入）。**部署前確認 prod 沒人註冊 `admin`**
  - 部署分兩次（expand / contract）：第一次 migration 加 `room_id` 時帶 `DEFAULT 1`，讓新舊版同時在線時舊版 INSERT 不會撞 NOT NULL；全部換成新版後，下一次部署再 `DROP DEFAULT`
    - ✅ 000010（expand）、000011 `DROP DEFAULT`（contract，34ab09f）都已上線。退版注意：後端要退回 `2c17ac8` 之前的版本，得先手動跑 000011 的 down
  - 房間 soft delete（`deleted_at`）：已刪除的不算上限、連結回 404、所有讀取只走會過濾 `deleted_at IS NULL` 的共用 method。Known issue：可無限「建立 → 刪除」累積資料，靠 rate limit
    - 之後再做：房主看自己已刪除的房間要能分辨「已刪除」（例如之後的恢復功能），觀眾一律只看到 404
  - Backlog：不公開但知道連結就能進的房間（YouTube 不公開影片、Google Docs「知道連結的任何人」= capability URL）。目前沒需求；要做時新增 `visibility` 欄位（`public` / `unlisted`）即可，`public_id` 已經猜不到，id 設計不用動
  - 房間人數（presence，設計已定案，見 go-notes 3.27）：每個房間一個 Redis Sorted Set `room:R:members`，member = `conn_id:user_id`（綁連線不綁 user；訪客用唯一值，不能留空），score = 最後續期時間。每台 server 每 10 秒用 pipeline 續期自己的連線 + 對 key 下 `EXPIRE 30`；正常斷線直接刪。算人數：先刪 score < now − 30s → 讀出 member 依 user_id 去重；每台各自每 5 秒算一次推給自己的觀眾，算不到就沿用上一次的數字
- [ ] Authorization（RBAC）
  - role **不放進 JWT**：角色是每個房間各自的（塞不進一個 claim），而且降級需要立刻生效（不能等 access token 過期）
  - 做法同 Ban：執行動作時查每台 server 記憶體裡的 role cache，變更時 Pub/Sub 通知 + 定期從 DB 同步
  - 房主 / curator 可以透過 WS 直送彈幕（W4-3 起一般 user 只能走 `POST /rooms/{public_id}/messages`，WS 寫入一律回 error）
- [ ] 彈幕內容過濾：`POST /rooms/{public_id}/messages` 只用 `strings.TrimSpace` 擋全空白（含全形空白），零寬空白（U+200B 等）仍可送出看不見的彈幕；之後跟內容審核一起處理。`room_name` 同樣只 trim 頭尾，中間可以有換行 / 控制字元，一起處理
- [ ] 刪除帳號時區分「匿名舊彈幕」和「已刪除的使用者」：`messages.user_id = NULL` 目前有兩種來源（000009 之前的匿名彈幕、`ON DELETE SET NULL`），前端一律顯示「匿名」。做刪帳號功能時決定要不要區分（例如 migration 標記 legacy 列，或刪帳號時改寫 username 快照）
- [ ] Ban（封鎖使用者，含 shadow ban）
  - 背景：access token 在本機驗證、不查 DB，撤銷 refresh token 後，被封鎖的人在 access token 過期前還能繼續動作；效期也壓不到秒等級（refresh 流量會暴增）→ 封鎖要另外一層
  - 每台 server 在**記憶體**維護 ban set（人數極少），每個 request 查記憶體即可，不查 DB / Redis
  - 同步：ban / unban 時用 Redis Pub/Sub 廣播。Pub/Sub 是 at-most-once（subscription 斷線、部署中都會掉）→ DB 是 source of truth：啟動時、subscription 重連時、每 60 秒從 DB 全量重新載入
  - 三層保險：Pub/Sub（秒級）→ 定期同步（≤ 60 秒）→ refresh token 已撤銷，access token 過期後換不到新的（≤ access token 效期）
  - WS：連線建立時才驗 token，之後的訊息不經過 middleware → 收到 ban 時要主動關掉該 user 的連線，或在每則訊息檢查 ban set

## W5 — API 設計強化

- [ ] HTTP 錯誤格式統一：目前 `http.Error` 只回純文字，同一個 status 分不出原因（例如 403 可能是「被 ban」或「房間數到上限」）。改成 status + 機器判斷用的 `code` + 給人看的 `message`（WS 已經是 `{code, message}`；可參考 RFC 9457 Problem Details），所有 endpoint 一次換
- [ ] Rate limiting（middleware）
  - `/login`、`/register` 的部分提前到 W4-3（部署前就要擋撞庫 / 暴力猜密碼），這裡處理其餘 endpoint
  - Limiter 呼叫加短 timeout（例如 `context.WithTimeout` 100ms）：Redis 網路黑洞時 go-redis 預設 dial timeout 5 秒，一次 login 呼叫 limiter 4 次，fail open 最慢要等約 20 秒
  - 全域基本限制（每 IP 每秒 N 個 request，nginx `limit_req` 或 middleware）：目前只有個別 endpoint 的嚴格限制。依「被濫用的代價」決定而不是 HTTP method：`GET /messages?limit=10000` 一樣貴
  - WS 連線數限制：WS 不用登入就能連，現在只有每條連線 5 frame/秒（`listenConnection`），擋不住「開很多條」
    - 開 100 條閒置連線就佔滿 `maxClients`，正常使用者連不上；keepalive 之後閒置連線會回 pong，不會被 read deadline 踢掉
    - 開 100 條、每條每秒送 5 個 → 合計每秒 500 次搶全域鎖回應，廣播變慢
    - 方向：每 IP 最多 N 條 WS（nginx `limit_conn` 或 upgrade 前在 middleware 擋），多台 server 時要跨 instance 計數（Redis）
- [ ] Login CAPTCHA（reCAPTCHA / Turnstile 等，接外部服務前先討論選型）
  - 補限流擋不到的兩個洞：攻擊者分散到大量 IP（每個 IP 都低於上限、credential stuffing 每個帳號只試一次）；per-username 限流可被拿來故意鎖別人的帳號（DoS）
  - 失敗超過門檻時改成要求驗證，而不是直接 429：真人解完照樣登入，bot 每次嘗試都要付成本
- [ ] Trusted proxy 收尾（client IP 不可偽造的前提）
  - 本機 server-1/2 直接開 port（5388/5389）可繞過 nginx 偽造 `X-Real-IP`：拿掉 docker-compose 的 `ports:`，只留 nginx 對外（代價：不能再繞過 nginx 直接 curl 單台 server）
  - Defense in depth：只有 `RemoteAddr` 是 trusted proxy 的 IP / CIDR 時才信任 header，其他來源退回 `RemoteAddr`（概念同 Gin `SetTrustedProxies`）
- [ ] CORS origin canonical 檢查的剩餘缺口（低優先）：port 前導 0（`:08443`）、非 ASCII 的 IDN host（應寫 punycode）目前仍會通過
- [ ] Metrics path cardinality：`path` label 直接用 `r.URL.Path`，被亂掃 URL 時每個 path 都會產生新的時間序列；改用 route pattern（如 Go 1.23+ 的 `r.Pattern`，mux 路由後才有值）或把未知路徑歸成 `other`
- [ ] Pagination
  - `GET /rooms/{public_id}/messages` 加 `before=<id>`（往上捲載入更舊）/ `after=<id>`（重連補漏），store 跟著加方向參數：`WHERE id < $1 ORDER BY id DESC` / `WHERE id > $1 ORDER BY id ASC`
  - 「選哪 N 則」（store，ORDER BY + LIMIT）跟「回傳順序」（API 契約，handler）是兩件事，不要用一個 order 參數同時控制
  - 方向不能用 `$1` 傳（placeholder 只能放值），用 typed enum + switch 對應到寫死的 `ASC` / `DESC`
- [ ] 密碼外洩檢查：register 時查 HIBP Pwned Passwords API（取代 / 補強 W4-3 embed 的常見密碼清單）
  - k-anonymity：只送 SHA-1 前 5 碼，比對在本機做，原密碼與完整 hash 都不離開 server
  - 要決定外部服務慢 / 掛掉時 fail open（放行）還是 fail closed（擋下），並加 timeout
  - 順帶評估 NFKC 正規化（`golang.org/x/text/unicode/norm`，新套件），避免同字不同編碼登不進去
- [ ] API versioning
- [ ] 評估引入 OpenTelemetry（W4 結束後決定）
  - metrics 改走 vendor-neutral API，backend 可替換
  - 評估 tracing 的 trace ID 是否取代 W4a 的 request ID

## W6 — CI/CD + 進階穩定性

- [ ] CI/CD（GitHub Actions）
- [ ] Dockerfile 改 multi-stage build：目前單一 stage（`FROM golang`），最終 image 帶著整套 Go toolchain，約 400 MB（實際只需要 `main` 執行檔，約 20 MB）。改成 build stage 編譯、runtime stage 用小 base image 只複製執行檔 → push / 啟動更快、攻擊面更小。注意 runtime image 要有 CA 憑證（連外部 TLS）和時區資料（若有用到）
- [ ] Redis List replay（搭配前端 auto-reconnect）
- [ ] 彈幕非同步保存（量大時）：目前 `POST /messages` 同步 INSERT 成功才廣播，每則彈幕最貴的是寫入（WAL + fsync），不是查 username。改成送達路徑（pub/sub）不等 DB、保存路徑走 queue 批次寫入；要處理 queue 掛掉、已廣播但沒存到的情況
- [ ] Hub 抽象（分離分發邏輯與 transport），新增 SSE endpoint 與 WS 並存（SSE `Last-Event-ID` 可搭配 replay）
  - 每條連線改成自己的寫入 goroutine 後，WS 連線的身分（user、exp、timer）改用每條連線各自的 mutex：Step 9 先用全站那把鎖，因為寫 frame 本來就要拿全站鎖，拆身分鎖省不到東西；要拆時上鎖順序固定「全站 → 單條連線」
- [ ] 跨 server 的「不同會員數」：Step 9 的 gauge 只有單台 server 的數字，同一個 user 連到 server-1、server-2 加總會算 2。要共用的地方（Redis）合併，server 被 kill -9 時沒機會清 → 靠 TTL + heartbeat（活著的 server 定期續命，死掉的自然過期），不能靠 server 自己起的 timer。待討論：TTL / heartbeat 間隔（server 死掉後數字錯多久 vs Redis 寫入量）、資料結構、誰來加總、要不要改成獨立的 health checker service（它本身也要被監控，Redis TTL 等於免費的 checker）；要「每個直播間」統計的話等 W4b Room
  - 2026-10-29：TTL / 續期間隔 / 資料結構 / 誰來加總已在 W4b presence 定案（見上方 W4b「Room 概念」）。剩下的 known issue：去重要讀出整個房間的 member，成本 ∝ 房間人數，超大房間（5 萬人）時改用 per-user Sorted Set / HyperLogLog / 分片等（比較見 go-notes 3.27）
  - lock 內不做 I/O：每個 client 一個 struct（帶 per-client logger + send channel），由自己的 goroutine 寫入；目前的 write deadline 只是減輕，不是根治
  - SSE 要 flush：用 `http.NewResponseController(w).Flush()`（StatusCodeRecorder 沒實作 `http.Flusher`，靠 `Unwrap` 轉到內層）
- [ ] Alerts
  - goroutine 洩漏：`go_goroutines` 和 `websocket_connections_active` 一起看，每條 WS 帶 2 個 goroutine（net/http 的 + `pingLoop`），手動驗過 17 → 開 10 條 37 → 關掉回 17。連線數回落但 goroutine 呈階梯狀上升 = 洩漏
- [ ] 多 replica 時 `/metrics` 只收得到其中一台：從外部打 `/metrics` 會經過 Railway 的 load balancer，每次只落在某一台 replica，看到的是半套數字。Prometheus 要能個別找到每台 replica（內網位址 / service discovery）再加總；連線數這類可相加的數字在監控層 `sum` 即可，不需要另寫 aggregate service（不重複會員數不能相加，見上方「不同會員數」）
- [ ] Goroutine 洩漏的自動化檢查（WS keepalive 的 `pingLoop` 有沒有在 `done` 後結束）：目前只有手動驗證，test 沒涵蓋
  - test：`go.uber.org/goleak`（新套件，引入前先討論）在 test 結束時檢查多出來的 goroutine；`runtime.NumGoroutine` 前後比對太不準
  - production 定位：`net/http/pprof` 的 `/debug/pprof/goroutine?debug=1` 依 stack 分組計數，一眼看出卡在哪一行；**不能對外**，開在只有內網能連的 port
  - 同樣沒測的：`pingLoop` 寫 ping 失敗時的 `Close`（難穩定製造寫入失敗）→ 靠 log 追：同一個 `request_id` 的 `write ping failed` 到 `client disconnected` 應該只差幾 ms，差 ~75s 代表 Close 沒生效
- [ ] Graceful shutdown 時對 WS client 送 close frame（`1001 Going Away`）：`server.Shutdown` 不會關已 hijack 的 WS 連線，現在重新部署時 client 只會看到 `1006` 異常斷線。前端判斷要不要重連看的是「是不是 client 自己關的」，不看 `code` / `wasClean`，所以補上後前端不用改
- [ ] Migration 成功時 log 目前的 schema version（`m.Version()`）：現在成功時完全沒有 log，只能從 `server listening` 間接推斷，要確認版本得去開 DB
- [ ] 大表加 index 改用 `CREATE INDEX CONCURRENTLY`：一般的 `CREATE INDEX`（如 000009 的 `messages_user_id_idx`）建立期間會擋住該表的寫入，表小時一瞬間，量大時彈幕會卡住。`CONCURRENTLY` 不能放在 transaction 裡 → 獨立一份 migration、不包 `BEGIN` / `COMMIT`；失敗會留下 `INVALID` 的 index，要先 `DROP INDEX` 再重跑
- [ ] `LOG_LEVEL` 環境變數控制 log level（`slog.HandlerOptions.Level`），debug 時不用改 code
- [ ] Chaos testing
