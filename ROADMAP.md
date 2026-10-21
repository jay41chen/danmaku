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
  - WS `CheckOrigin` 目前全部放行：若改用 cookie 驗證，要依 `CORS_ORIGINS` 檢查 Origin，防 CSWSH（跨站 WebSocket 劫持）；token 放在 header / query 則不受影響

## W4b — Room + Authorization

> Room 排在 RBAC 前面：有「房主」之後，權限規則才具體（房主刪訊息、踢人）

- [ ] Room 概念（房間 + 房主）
- [ ] Authorization（RBAC）

## W5 — API 設計強化

- [ ] Rate limiting（middleware）
  - `/login`、`/register` 的部分提前到 W4-3（部署前就要擋撞庫 / 暴力猜密碼），這裡處理其餘 endpoint
  - Limiter 呼叫加短 timeout（例如 `context.WithTimeout` 100ms）：Redis 網路黑洞時 go-redis 預設 dial timeout 5 秒，一次 login 呼叫 limiter 4 次，fail open 最慢要等約 20 秒
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
- [ ] `LOG_LEVEL` 環境變數控制 log level（`slog.HandlerOptions.Level`），debug 時不用改 code
- [ ] Chaos testing
