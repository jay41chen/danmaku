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

## W4a — Middleware + Migration + Auth

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
- [ ] Authentication（JWT middleware）
  - [x] `/register`、`/login`（bcrypt、timing 防帳號列舉、rate limit）
  - [x] Access token（JWT HS256，1 小時，`JWT_SECRET` fail fast）
  - [x] Refresh token（opaque、存 SHA-256、7 天、rotation + duplicate use detection、15s grace）、`/refresh`、`/logout`
  - [ ] Bearer middleware（Step 8）
  - [ ] WS 驗證（Step 9）
  - [ ] Client：存 token、401 自動 refresh（Step 10）
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
- [ ] Authorization（RBAC）
  - role **不放進 JWT**：角色是每個房間各自的（塞不進一個 claim），而且降級需要立刻生效（不能等 access token 過期）
  - 做法同 Ban：執行動作時查每台 server 記憶體裡的 role cache，變更時 Pub/Sub 通知 + 定期從 DB 同步
- [ ] Ban（封鎖使用者，含 shadow ban）
  - 背景：access token 在本機驗證、不查 DB，撤銷 refresh token 後，被封鎖的人在 access token 過期前還能繼續動作；效期也壓不到秒等級（refresh 流量會暴增）→ 封鎖要另外一層
  - 每台 server 在**記憶體**維護 ban set（人數極少），每個 request 查記憶體即可，不查 DB / Redis
  - 同步：ban / unban 時用 Redis Pub/Sub 廣播。Pub/Sub 是 at-most-once（subscription 斷線、部署中都會掉）→ DB 是 source of truth：啟動時、subscription 重連時、每 60 秒從 DB 全量重新載入
  - 三層保險：Pub/Sub（秒級）→ 定期同步（≤ 60 秒）→ refresh token 已撤銷，access token 過期後換不到新的（≤ access token 效期）
  - WS：連線建立時才驗 token，之後的訊息不經過 middleware → 收到 ban 時要主動關掉該 user 的連線，或在每則訊息檢查 ban set

## W5 — API 設計強化

- [ ] Rate limiting（middleware）
  - `/login`、`/register` 的部分提前到 W4-3（部署前就要擋撞庫 / 暴力猜密碼），這裡處理其餘 endpoint
  - Limiter 呼叫加短 timeout（例如 `context.WithTimeout` 100ms）：Redis 網路黑洞時 go-redis 預設 dial timeout 5 秒，一次 login 呼叫 limiter 4 次，fail open 最慢要等約 20 秒
  - 全域基本限制（每 IP 每秒 N 個 request，nginx `limit_req` 或 middleware）：目前只有個別 endpoint 的嚴格限制。依「被濫用的代價」決定而不是 HTTP method：`GET /messages?limit=10000` 一樣貴
- [ ] Login CAPTCHA（reCAPTCHA / Turnstile 等，接外部服務前先討論選型）
  - 補限流擋不到的兩個洞：攻擊者分散到大量 IP（每個 IP 都低於上限、credential stuffing 每個帳號只試一次）；per-username 限流可被拿來故意鎖別人的帳號（DoS）
  - 失敗超過門檻時改成要求驗證，而不是直接 429：真人解完照樣登入，bot 每次嘗試都要付成本
- [ ] Trusted proxy 收尾（client IP 不可偽造的前提）
  - 本機 server-1/2 直接開 port（5388/5389）可繞過 nginx 偽造 `X-Real-IP`：拿掉 docker-compose 的 `ports:`，只留 nginx 對外（代價：不能再繞過 nginx 直接 curl 單台 server）
  - Defense in depth：只有 `RemoteAddr` 是 trusted proxy 的 IP / CIDR 時才信任 header，其他來源退回 `RemoteAddr`（概念同 Gin `SetTrustedProxies`）
- [ ] CORS origin canonical 檢查的剩餘缺口（低優先）：port 前導 0（`:08443`）、非 ASCII 的 IDN host（應寫 punycode）目前仍會通過
- [ ] Metrics path cardinality：`path` label 直接用 `r.URL.Path`，被亂掃 URL 時每個 path 都會產生新的時間序列；改用 route pattern（如 Go 1.23+ 的 `r.Pattern`，mux 路由後才有值）或把未知路徑歸成 `other`
- [ ] Pagination
  - `/messages` 加 `before=<id>`（往上捲載入更舊）/ `after=<id>`（重連補漏），store 跟著加方向參數：`WHERE id < $1 ORDER BY id DESC` / `WHERE id > $1 ORDER BY id ASC`
  - 「選哪 N 則」（store，ORDER BY + LIMIT）跟「回傳順序」（API 契約，handler）是兩件事，不要用一個 order 參數同時控制
  - 方向不能用 `$1` 傳（placeholder 只能放值），用 typed enum + switch 對應到寫死的 `ASC` / `DESC`
- [ ] 密碼外洩檢查：register 時查 HIBP Pwned Passwords API（取代 / 補強 W4-3 embed 的常見密碼清單）
  - k-anonymity：只送 SHA-1 前 5 碼，比對在本機做，原密碼與完整 hash 都不離開 server
  - 要決定外部服務慢 / 掛掉時 fail open（放行）還是 fail closed（擋下），並加 timeout
  - 順帶評估 NFKC 正規化（`golang.org/x/text/unicode/norm`，新套件），避免同字不同編碼登不進去
- [ ] API versioning
- [ ] 統一 error response 格式
- [ ] 評估引入 OpenTelemetry（W4 結束後決定）
  - metrics 改走 vendor-neutral API，backend 可替換
  - 評估 tracing 的 trace ID 是否取代 W4a 的 request ID

## W6 — CI/CD + 進階穩定性

- [ ] CI/CD（GitHub Actions）
- [ ] Redis List replay（搭配前端 auto-reconnect）
- [ ] Hub 抽象（分離分發邏輯與 transport），新增 SSE endpoint 與 WS 並存（SSE `Last-Event-ID` 可搭配 replay）
  - lock 內不做 I/O：每個 client 一個 struct（帶 per-client logger + send channel），由自己的 goroutine 寫入；目前的 write deadline 只是減輕，不是根治
  - SSE 要 flush：用 `http.NewResponseController(w).Flush()`（StatusCodeRecorder 沒實作 `http.Flusher`，靠 `Unwrap` 轉到內層）
- [ ] Alerts
- [ ] Migration 成功時 log 目前的 schema version（`m.Version()`）：現在成功時完全沒有 log，只能從 `server listening` 間接推斷，要確認版本得去開 DB
- [ ] `LOG_LEVEL` 環境變數控制 log level（`slog.HandlerOptions.Level`），debug 時不用改 code
- [ ] Chaos testing
