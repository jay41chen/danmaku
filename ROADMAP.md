# Roadmap

## Milestones（2026-11-03 訂，暫定）

> 主軸：application + architecture。從 2026-11-03 起估 5 ～ 7 週（2026-11-04 重排 M3 之後的順序，日期從當天重算，假設 M7 會做）。估計假設投入時間跟前一個月差不多，沒算做的過程中新長出來的項目 → 實際偏後半段。細項見下方 W4b / W6

| Milestone | 內容 | 估計（週） | 預計完成（快 ～ 慢） |
|---|---|---|---|
| M1 房間可瀏覽 ✅ | 房間列表（`GET /rooms` cursor 分頁、`/me/rooms`）+ 選房間 UI（2026-11-04 完成，[ADR 0008](docs/design/0008-cursor-pagination.md)） | 0.5 ～ 0.75 | 11/6 ～ 11/8 |
| M2 房間有人氣 ✅ | presence + 跨 server 不重複人數（同一套 TTL + heartbeat，從 W6 提前；2026-11-04 完成，[ADR 0010](docs/design/0010-room-presence.md)） | 0.75 ～ 1.25 | 11/12 ～ 11/17 |
| M3 Git hook ✅ | pre-commit：`gofmt` / `go vet` / 前端 lint；pre-push：全部測試（只用 `danmaku_test`）+ 前端 build（2026-11-04 完成） | < 0.1 | 11/4 ～ 11/5 |
| M4 不漏訊息 ✅ | 重連補最新 100 則 + `s-maxage=1` 快取 header + 重連退避修正 + PubSub half-open watchdog + 重建後 `resync` + SLI metrics；不做 `after`、Redis List、client timer（2026-11-05 完成，[ADR 0011](docs/design/0011-message-delivery-recovery.md)） | 1 ～ 1.5 | 11/11 ～ 11/16 |
| M5 房間有秩序 | HTTP 錯誤格式統一 → RBAC → Ban | 1 ～ 1.5 | 11/18 ～ 11/26 |
| M6 壓測基準 | 自己寫腳本在本機模擬大量流量（不打 prod、只用測試環境），記下同步寫入的基準數字，決定 M7 要不要做；同一台 Mac 的數字只用來比較前後，不當容量上限 | 0.5 ～ 0.75 | 11/22 ～ 12/2 |
| M7 寫入路徑 | 彈幕非同步保存（queue 批次寫入、已廣播沒存到的處理）+ SLI metrics（queue 積壓、寫入延遲 / 失敗）；**看 M6 結果決定要不要做** | 1 ～ 1.25 | 11/29 ～ 12/10 |
| M8 依人數排序 | 房間列表依人數排序（從 M2 移出，見下方 W4b） | 0.5 ～ 0.75 | 12/2 ～ 12/16 |
| M9 最小 CI | 精簡 GitHub Actions（只跑測試，不擋部署、不走 PR）+ multi-stage Dockerfile | 0.1 ～ 0.25 | 12/3 ～ 12/17 |
| M10 驗收 | 本機 docker-compose 腳本化故障情境（kill Redis、`kill -9` server、網路丟包 / 黑洞）+ 重跑 M6 的壓測，檢查不漏訊息、人數恢復、重連成功 | 0.5 ～ 0.75 | 12/7 ～ 12/23 |

選做（infra 向）：Railway multi-replica metrics / service discovery、alerts、trusted proxy + IPv6 /64、全域限流、測試 DB 隔離、Hub + SSE。其餘見最下方 Backlog

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
    - [x] 移除已棄用的 `/messages`：改成 tombstone route，任何 method 回 `410 Gone` + 指路（`routes.removed` 不收 handler，避免被拿去開公開 API）
    - ~~Known issue：WS 仍是全站一個 Redis channel，在房間 X 的觀眾會收到所有房間的彈幕~~ → 已由下一項修掉
  - [x] Pub/Sub 每個房間一個 channel（`room:{public_id}`，過濾交給 Redis）+ WS `join` / `leave`：store a33e3f5、前端 c4eedc0、backend e0eac93（含 ws.go 長函式拆小）。本機 2 台 server 實測 NUMSUB / MONITOR 行為符合預期（見 go-notes 3.29）
    - 每台 server 只看自己身上的連線：某房間的 `viewers` 0→1 時 `SUBSCRIBE`、1→0 時 `UNSUBSCRIBE`（lazy subscribe），全部走同一條 Redis Pub/Sub 連線
    - 進房走 WS frame（不用 `?room=`、不用 HTTP API：要改變某條連線的狀態，資訊就要從那條連線進來；只留一套進房機制）：`join` 只負責加入，一條連線最多一間房，已在房間內再 `join` 回 error；成功回 `join_ok`（同 `auth_ok` 模式）；換房 = client 先 `leave` 再 `join`（以後做多房只要放寬這條限制，不改語意，舊前端不受影響）。代價：`join` 失敗後連線不在任何房間，前端要能顯示
    - `join` 查房間存不存在（沿用 `GetRoom` 的 store method）帶 200 ms timeout（含等 DB pool 的時間）；錯誤只分兩種：`ws_room_not_found`（≈ 404，含 soft delete，重試沒用）、`ws_join_unavailable`（≈ 503，DB / Redis 出問題或 timeout，真正原因只寫 log）。前端收到後者用現有的 `equalJitter` backoff 重試
    - channel 名稱 `room:{public_id}`（client 送來的、`createMessage` 手上的都是 public_id，不用轉換）
    - 一把鎖（`roomSubscriptions.mu`）保護 `viewers map[public_id]int` + 0↔1 時的 SUBSCRIBE / UNSUBSCRIBE，降到 0 啟動 `unsubscribeDelay` timer、expire 時才刪 entry（debounce 之後；原本是直接刪）。原本設計的 per-room keyed mutex + refcount 換不到平行度：整台 server 只有一個 go-redis `PubSub`，它的 `Subscribe` / `Unsubscribe` 本來就在內部 `c.mu` 排隊（見 go-notes）。跟 `WebsocketHandler.mutex` 永不巢狀：拿著全域鎖時不呼叫 `Join` / `Leave`
    - 連線目前在哪間房由讀取迴圈的 goroutine 自己記（join / leave / 斷線清理都在它身上），結束時在鎖外 `Leave`；`wsClient.room` 另存一份給廣播過濾
    - 部署分兩次 push：先上前端（舊 backend 對 `join` 回 `ws_write_forbidden`，前端要當無害忽略），再上 backend；backend 上線後還開著舊頁面的人要重新整理
  - [x] Pub/Sub flapping debounce（`unsubscribeDelay` 30 秒，generation check 擋過期 timer；fakeClock 測情境 A / B）：冷門房 `viewers` 在 0↔1 之間跳，每次都送 SUBSCRIBE / UNSUBSCRIBE；20,000 間房 × 10 台 server 會超過單台 Redis。做法：延遲 UNSUBSCRIBE（例如 30 秒，SUBSCRIBE 不延遲）；timer 觸發時要重新上鎖再檢查 `viewers == 0`（`timer.Stop()` 回 `false` 時取消不了）；有 timer 在等的 entry 不能刪；test 要能注入時間
  - [x] `join_ok` 等 Redis 確認 SUBSCRIBE 才回（`ChannelWithSubscriptions` 的 `*redis.Subscription` → entry 的 `ready` channel，鎖外等）：補「join_ok 之後補撈歷史」的洞（JoinRoom 原本只寫出指令就回傳，緊接著的訊息可能兩邊都漏）；順便修好 `TestRoomPubSub` 的 flaky（30 次 fail 5 次 → 50/50）
  - [ ] W4a + Room 概念 Judge 審查（2026-10-30，3 個 Judge：W4a、Room、時序洞；無 high、無 977b0e6 造成的退步）後的修正
    - [x] Refresh token：duplicate use 撤銷全部之後，15s grace 內剛 rotate 的 token 仍能換出新的有效 token（撤銷全部沒有立即生效；攻擊者每 14 秒 rotate 一次就能穩定繞過）
      - 做法：新表 `user_credential_state(user_id, valid_after)`（migration 000012）；撤銷全部 = 寫 `valid_after`（`clock_timestamp()`），`created_at <= valid_after` 的 token 一律無效、**不再觸發撤銷**（攻擊者拿舊 token 不能一直把使用者登出），也不再 UPDATE `refresh_tokens`（不用列舉狀態就不會漏列）。`RotateRefreshToken`：查 user_id → `lockUser`（users 列當同一個 user 的排隊點，`lockOwner` 通用化）→ 鎖之後才檢查（TOCTOU）。舊的 `duplicate_use_detected` 資料 backfill 成 `valid_after`。被撤銷的 token 再出現記 Warn（`RefreshTokenRevokedError`）
      - 3 輪 Judge 通過（mutation 都有 test 擋）。設計過程見 go-notes 3.31
      - Known issue（接受）：部署期間舊版 server 不讀新表；撤銷進行中開始的登入 / rotate，新 token 可能一發出就失效（誤殺、不是繞過）；(a) 查 user_id 與 (b) 拿鎖之間 user 被刪 → invalid 那條沒 test；handler log test 的 fake `rotateUserID` 跟 error 的 UserID 都是 7，分不出 log 用錯變數（改成 0 即可）
      - 之後：`lockUser` 用 `FOR UPDATE` 會擋住這個 user 的 FK INSERT（登入、發彈幕）幾 ms，可改 `FOR NO KEY UPDATE`；token family（RFC 9700：只撤銷同一次登入衍生的那串，不踢掉其他裝置）
    - [x] 補 mutation 沒擋住的 test：WS 過期 timer 拿鎖後重查 exp（ws.go）、`handleLeave` 清 `c.room`（leave 後不再收到廣播）、未知帳號的 dummy bcrypt（timing 防列舉；`AuthHandler.checkPassword` 可替換成 spy）
    - [x] `Publish` 改用 `context.WithoutCancel(r.Context())` + 短 timeout：INSERT 成功後 client 斷線 → 已存 DB 但沒廣播。只改 `broadcast()`（`publishTimeout` 500ms），INSERT 維持 request ctx（INSERT 前 / 中取消 = 沒存也沒廣播，一致）
      - 同類（低優先，未修）：login 的 `CreateRefreshToken` commit 之後，`limiter.Reset` / `Refund` 還用 `r.Context()` → client 中途斷線時失敗計數沒 reset、IP 沒退還（client 沒拿到 token，說得過去）
      - Known issue：D1 test 靠 100ms token + 250ms sleep 的時序（沿用既有 race test），CI 很慢時可能誤報失敗（不會假通過）；真的 flaky 再放寬
    - [x] Redis 出狀況時不要拖垮整台 server（2026-11-02，`roomSubscriptions` 改成 reconciliation loop，見 go-notes 3.32）
      - 做法：事件端（Join / Leave / expire / confirmSubscribed）在 `mu` 底下只改記憶體、標 dirty；整台 server 一個 worker goroutine 比對 desired（`viewers > 0 || timer != nil`）/ actual（`state`），在鎖外送 SUB / UNSUB。`room_subscriptions.go`（事件端）+ `room_subscriptions_worker.go`（worker）
      - [x] `rooms.mu` 底下呼叫 go-redis `Subscribe`：Redis 黑洞時整台 Join / Leave 卡 5–15 秒 → I/O 移到 worker、鎖外做；`TestRoomSubscriptionsStuckSubscribeDoesNotBlockJoinLeave` 鎖住
      - [x] 排隊等鎖時 ctx 已過期 → 寫入 timeout → 整條 PubSub 重連 → worker 送指令當下才建 ctx（`redisCommandTimeout` 500ms），排隊時間不吃 timeout
      - [x] 確認遺失後這間房永遠 `ws_join_unavailable` → SUB 送出 500ms（`confirmResendDelay`）沒確認就改 `subRetry`、worker 重送（SUBSCRIBE 冪等）；SUB 寫入失敗 backoff 重試（100ms 起、翻倍、上限 5s）
      - SUB 失敗 / 確認逾時用 `subRetry`，不退回 `subIdle`：go-redis 寫入失敗也會記住 channel、重連時自己重訂 → 「可能有訂」，沒人要時必須 UNSUB，不能直接刪 entry
    - [x] 慢 client 卡住廣播（B3，2026-11-03，見 go-notes 3.33）：原本 consumer goroutine 在全域鎖底下同步寫每個 client（最多 `writeWait` 10s）→ 其他房間的廣播、SUB 確認、所有回覆一起卡，「第一個進房」的 join timeout
      - 做法：每條連線一個 `writeLoop`（唯一寫 conn 的 goroutine）+ outbox `send chan []byte`（`wsSendBuffer` 256）。廣播和回覆都只在鎖底下非阻塞排進 outbox（`enqueueLocked`），滿了就 `Close` 踢掉、讓 client 重連補撈；全域鎖底下不再寫 conn
      - 清理 owner 仍是讀取迴圈：writeLoop 寫失敗 / outbox 滿 / ping 失敗都只 Close；outbox 不 close（多個 sender），writeLoop 跟 pingLoop 共用 `done` 結束
      - test：`TestWebsocketSlowClientKickedWithoutBlockingBroadcast`、`TestWebsocketWriteFailureClosesConnection`；mutation 6 個全抓到
      - Known（無害）：被踢 / 正常斷線時可能多一行 `write frame failed` warn（見 `writeLoop` 註解）；突發超過 256 則的爆量，正常 client 也會被踢（設計如此，靠重連補撈）
    - Known limitation（接受）：只有一個 worker，一個 SUB / UNSUB 卡住（最多 `redisCommandTimeout`）時其他房間的指令排在後面（head-of-line blocking）。已有觀眾的房間不受影響，只影響「第一個進房」；go-redis 一條 PubSub 本來就一次處理一個指令，多 worker 換不到平行度
    - [x] 前端 `ws_room_not_found` 只有 `console.warn`，畫面沒顯示（2026-11-03：`realtime/room.ts` store + `useRoomStatus`，not_found 時只顯示「房間不存在」、隱藏列表和 Composer；只看 WS，GET 404 不算；重連不重設狀態避免閃爍）
    - [x] join frame 剛好是 1 秒內第 6 個 frame 會被 WS 限流靜默丟掉，client 也沒有 join 回應的 timeout（2026-11-03：後端不改，限流靜默丟是刻意的（回應也要拿 Hub 鎖）；前端 `JOIN_TIMEOUT_MS` 5 秒逾時 → 走既有 `scheduleJoinRetry` 退避；重送後晚到的 `ws_already_in_room` 不當 client bug。auth 不做 timeout：WS 唯讀，auth 只影響觀眾數統計。Known：舊版 backend 回 `ws_write_forbidden` 時每 ≤30 秒重送一次 join，只在部署空窗）
    - Known issue（低優先）：`CreateMessage` 的 `deleted_at IS NULL` 是自己寫的（為了單一 statement 的 atomicity），沒走共用 method，soft delete 條件改的時候要一起改
  - [x] 列出房間的 API（M1 後端，2026-11-04）：`GET /rooms`（公開）、`GET /me/rooms`（要登入，`no-store`）
    - Cursor pagination（keyset）：`ORDER BY created_at DESC, public_id DESC`，`public_id` 當 tiebreaker；撈 `limit+1` 判斷有沒有下一頁，cursor = 最後一筆回傳的；migration 000013 partial index `rooms(created_at, public_id) WHERE deleted_at IS NULL`（見 go-notes 3.37）
    - Page token：base64url 的 JSON `{v, created_at, public_id}`，對 client opaque；decode 嚴格（不認識的欄位、後面多接資料、版本、零值時間、public_id 格式 → `400 invalid cursor`）。`limit` 預設 20、最多 50（`model.MaxRoomListLimit`）
    - `GET /me/rooms` 也走同一套分頁（雖然未刪除的最多 3 筆）：之後列出已刪除房間（恢復功能）時不用改格式
    - [ ] Viewer 數排序 → M8（2026-11-04 從 M2 移過來）。要先解決：目前人數只推給房內觀眾，`GET /rooms` 拿不到；沒有觀眾的 server 不會算那間房，要有人清掉過期的數字；人數一直變，現在的 `(created_at, public_id)` cursor 不能直接沿用
    - Known issue（先不處理，2026-11-04）：`/me/rooms` 沒有 `(owner_id, created_at, public_id)` index。`danmaku_test` 強制關掉 seq scan 跑 `EXPLAIN`：planner 沿著 `rooms_created_at_public_id_idx` 倒著走、逐筆 `Filter: owner_id`，資料多、自己的房間又很舊時要掃過大部分的表；另一條路 `rooms_owner_id_idx` 只照 owner_id 排，還要 Sort。未刪除的最多 3 間，但被刪除的會一直累積（見下方 soft delete 的 known issue）。等有資料量能用 `EXPLAIN ANALYZE` 量出差別再加；加的時候要考慮：partial `WHERE deleted_at IS NULL` 與否、`rooms_owner_id_idx` 能不能 drop（`ON DELETE RESTRICT` 的 FK 檢查也會用到它，而且包含已刪除的房間）
  - [x] 選房間的 UI（M1 前端，2026-11-04）：「所有房間」+ 登入後的「我的房間」，每個房間是 `?room=<public_id>` 連結（整頁重載，沿用原本換房方式）；「載入更多」用 `next_page_token`；房名用 `<bdi>` + overflow 隔離
    - 建立房間的 UI 還沒有（只能打 `POST /rooms`）→ 之後需要時再做
    - 彈幕分頁不在 A：`after` → W6 replay，`before` → Backlog（Q167）
  - schema（見 go-notes 3.28）：`rooms(id, public_id, owner_id → users ON DELETE RESTRICT, room_name, created_at, deleted_at)` + `owner_id` index；名稱可重複，空白 / 長度由程式檢查
  - 每人房間數上限：transaction 內 `SELECT ... FROM users WHERE id = $1 FOR UPDATE` → 數 → INSERT，只能透過共用 method（恢復房間也走同一條）；超過回 403
  - 彈幕綁房間：`messages.room_id BIGINT NOT NULL REFERENCES rooms(id)`；舊彈幕歸進 legacy room（`public_id = 'legacy00000'`、名稱 `Legacy`，owner 是 migration 建的系統帳號 `admin`，`password_hash = '!'` 不能登入）。**部署前確認 prod 沒人註冊 `admin`**
  - 部署分兩次（expand / contract）：第一次 migration 加 `room_id` 時帶 `DEFAULT 1`，讓新舊版同時在線時舊版 INSERT 不會撞 NOT NULL；全部換成新版後，下一次部署再 `DROP DEFAULT`
    - ✅ 000010（expand）、000011 `DROP DEFAULT`（contract，34ab09f）都已上線。退版注意：後端要退回 `2c17ac8` 之前的版本，得先手動跑 000011 的 down
  - 房間 soft delete（`deleted_at`）：已刪除的不算上限、連結回 404、所有讀取只走會過濾 `deleted_at IS NULL` 的共用 method。Known issue：可無限「建立 → 刪除」累積資料，靠 rate limit
    - 之後再做：房主看自己已刪除的房間要能分辨「已刪除」（例如之後的恢復功能），觀眾一律只看到 404
  - Backlog：不公開但知道連結就能進的房間（YouTube 不公開影片、Google Docs「知道連結的任何人」= capability URL）。目前沒需求；要做時新增 `visibility` 欄位（`public` / `unlisted`）即可，`public_id` 已經猜不到，id 設計不用動
  - [x] 房間人數（M2，2026-11-04，[ADR 0010](docs/design/0010-room-presence.md)、go-notes 3.27）：每個房間一個 Redis Sorted Set `room:<public_id>:members`，member = `conn_id:user_id`（綁連線不綁 user；匿名 user_id = 0，依 conn_id 算），score = 最後續期時間（Redis `TIME`）。每台 server 一個 worker：Hub 有變動只寫 diff，每 10 秒全部續期 + `EXPIRE 30`，每 5 秒算人數推給自己的觀眾；算不到就不推，client 沿用上一次的數字
- [x] HTTP 錯誤格式統一（M5，2026-11-06）：所有 endpoint 回 `{"error": {"code", "message"}}`，`code` 是 client 的翻譯 key、發布後不能改名，`message` 是後備文字。status 不變；錯誤一律 `Cache-Control: no-store`、`nosniff`。不做 RFC 9457、i18n 套件、攔截 mux 內建的 404 / 405（client 碰到非 JSON 只看 status，不顯示 body）。刻意不分細：`invalid_credentials`、`unauthorized`、`room_not_found`、`invalid_refresh_token`
- [ ] Authorization（RBAC，M5 定案，2026-11-06）
  - role **不放進 JWT**：角色是每個房間各自的（塞不進一個 claim），而且降級需要立刻生效（不能等 access token 過期）
  - `room_roles(room_id, user_id, role, assigned_by, created_at, PRIMARY KEY (room_id, user_id))`，`role` 是 `moderator` / `banned`；房主看 `rooms.owner_id`，沒有列就是一般觀眾。一人一房一列，不會同時是房管又被 ban
  - 階級：房主 > 房管 > 一般觀眾，只能上對下；只有房主能指派 / 撤銷房管。房主 ban 房管 = 那一列改成 `banned`，解除後是一般觀眾；指派被 ban 的人回 409
  - 查 DB，不做記憶體 role cache：頻率低，也不用同步。還沒談：API 路徑、client 怎麼知道自己的角色
- [x] 彈幕內容過濾（C1，2026-11-03）：輸入層用 PRECIS Freeform（`golang.org/x/text/secure/precis`，RFC 8264）擋控制字元、零寬字元、雙向控制字元（RLO 等）；**只檢查不改寫**（拿 precis 的 error、丟掉輸出，存原樣）。profile 另外設定：檢查前先移除 VS15 / VS16（U+FE0E / U+FE0F，手機鍵盤送的 ❤️ 帶 U+FE0F，PRECIS 預設會拒絕）+ `DisallowEmpty`（只有選擇符的字串變空 → 拒絕），都是 PRECIS 自己的擴充點（AdditionalMapping），不在 validator 裡加特例。房名比彈幕嚴：頭尾空白直接拒絕（不再 trim）、不可連續空白、combining mark（Mn + Me）最多連續 2 個。驗證集中在 `handler/text_validation.go`，回傳的 sentinel 文字直接給 client。輸出層：前端 `<bdi>` 隔離 + `overflow: hidden` 擋 Zalgo（見 go-notes 3.36）
  - Known：ZWJ emoji 序列（👨‍👩‍👧、👩‍💻）會被拒（PRECIS 的 contextual rule）；PRECIS 拒絕未指派碼位 → Unicode 新增的 emoji 要等 `x/text` 升級才收；舊資料沒有回頭清理；「看起來是空白、但不是 Unicode 空白」的字元擋不到：U+2800（點字空白）、U+1D159、`"\uFE0F "`（選擇符 + 空白，彈幕會通過）；VS15 / VS16 是**任何位置**都移除後才檢查，不限 emoji 後面 → 彈幕可以夾帶最多約 99 個看不見的 U+FE0F（房名受 mark 上限擋，最多連 2 個），選擇符也能插在 PRECIS contextual rule 檢查的兩個字之間讓檢查看不到它（Judge 全掃 U+0000–U+2FFFF：不含選擇符的輸入 0 個行為改變）→ 之後跟內容審核一起處理（要一份「視覺空白」清單）
  - 之後：關鍵字過濾 / 內容審核要「先正規化再比對」（防零寬字元或全形字繞過），正規化只用在比對，不改存的資料
- [ ] 刪除帳號時區分「匿名舊彈幕」和「已刪除的使用者」：`messages.user_id = NULL` 目前有兩種來源（000009 之前的匿名彈幕、`ON DELETE SET NULL`），前端一律顯示「匿名」。做刪帳號功能時決定要不要區分（例如 migration 標記 legacy 列，或刪帳號時改寫 username 快照）
- [ ] Room ban（M5 定案，2026-11-06）：房間級 shadow ban，只擋別人看到他的彈幕，不撤銷登入、不碰 JWT
  - 發彈幕：照常存，`CreateMessage` 同一條 SQL 查是否被 ban，回 201，被 ban 就不 PUBLISH。不存在 `room_banned` 這種 code
  - 拿彈幕：`NOT EXISTS` 在讀取時過濾，不標記彈幕本身；解除後全部回來，包括被 ban 期間發的（接受，只有還在最新 100 則內的看得到）
  - ban 當下：廣播 `user_hidden {user_id}`，client 移除畫面上他的彈幕；server 不發給本人；掉了不補（退回 F5 或被新彈幕擠掉才消失）
  - 解除：房主全部都能解；房管能解房主 ban 的以外（看 `assigned_by`）。房管重複 ban 不覆蓋房主那一列，房主重複 ban 要覆蓋
  - Known：作者本人 F5、換房或開無痕就會發現（`GET` 是 `public, s-maxage=1`，不能依使用者回不同內容）；刪帳號後 `user_id` 變 NULL，隱藏會失效（現在沒有刪帳號功能）
  - 不做：記憶體 ban set + Pub/Sub 同步（原本是為全站封鎖、每個 request 都要查設計的，現在只有 `POST` 要查，而它本來就會碰 DB）。**M7 如果改成非同步寫入，`POST` 不再經過 DB，到時要加快取，表和讀取過濾不用改**
  - 驗收：重測 `GET` 延遲，尤其是被 ban 的人還在狂發的情況
  - 全站封鎖（site ban）不在 M5：要做時才考慮 `revokeAllCredentials`、`iat <= valid_after` 和記憶體 ban set

## W5 — 已拆分（2026-11-03）

> 錯誤格式統一、Pagination → W4b（RBAC 前、房間列表內）；Rate limiting、Trusted proxy、IPv6 /64 → W6；其餘 → Backlog

- [x] Metrics path cardinality：`path` label 直接用 `r.URL.Path`，被亂掃 URL 時每個 path 都會產生新的時間序列；改用 route pattern（如 Go 1.23+ 的 `r.Pattern`，mux 路由後才有值）或把未知路徑歸成 `other`
  - **優先度提高**（2026-10-30 Judge）：prod 的 server 公開，任何人掃隨機 URL 就能讓時間序列無限增加 → 記憶體持續成長、可能 OOM，不只是監控難看；`/metrics` 本身也公開可讀
  - ✅ 2026-11-03：`path` = `r.Pattern` 去掉 method（沒比對到 → `unmatched`，404 / 405 / preflight 靠 `status_code` 區分）；Judge 另外抓到 `method` 也是 client 決定的（`FOOBAR123` 一樣能撐爆）→ 白名單：HTTP 標準九個照原樣，其他 `other`。middleware chain 抽成 `serviceHandler`，`main_test` 走同一條 chain 守住「HttpMetrics 和 mux 中間不能複製 request」
  - Known（info）：stdlib 在 CONNECT + 結尾 `/` 的 route 遇到 redirect 時會把 `r.Pattern` 設成 client 的路徑；目前沒有這種 route，新增 subtree route 時要重看
  - `/metrics` 本身仍公開可讀（另議）

## W6 — CI/CD + 進階穩定性

> 順序（2026-11-04 重排，以上方 Milestones 為準）：跨 server 人數（M2）→ 補撈 + half-open watchdog（M4）→ 壓測基準（M6）→ 非同步保存（M7）→ 最小 CI + multi-stage（M9）→ 故障情境 + 壓測驗收（M10）；其餘 W6 項目為選做（見上方 Milestones）

- [ ] CI/CD（GitHub Actions）：範圍見 M9（2026-11-04 定）。自己一個人開發，不走 PR，部署也不等 CI；要改成擋部署時再調整
- [ ] Dockerfile 改 multi-stage build：目前單一 stage（`FROM golang`），最終 image 帶著整套 Go toolchain，約 400 MB（實際只需要 `main` 執行檔，約 20 MB）。改成 build stage 編譯、runtime stage 用小 base image 只複製執行檔 → push / 啟動更快、攻擊面更小。注意 runtime image 要有 CA 憑證（連外部 TLS）和時區資料（若有用到）
- [x] 訊息補撈（M4 定案，2026-11-05）：重連 `GET` 最新 100 則，不做 `after`、不做 Redis List，理由見 [ADR 0011](docs/design/0011-message-delivery-recovery.md)。以下是當時的候選，留作紀錄
  - 補漏的 API 候選：`GET /rooms/{public_id}/messages?after=<id>`（2026-11-03 從 W5 Pagination 移來）跟 Redis List replay 二選一或並用，這裡一起決定
    - store 加方向參數：`WHERE id > $1 ORDER BY id ASC`；方向不能用 `$1` 傳（placeholder 只能放值），用 typed enum + switch 對應到寫死的 `ASC` / `DESC`
    - 「選哪 N 則」（store，ORDER BY + LIMIT）跟「回傳順序」（API 契約，handler）是兩件事，不要用一個 order 參數同時控制
  - 「server 自己的 PubSub 重連、WS 沒斷」（本機 kill pubsub 連線實驗每次漏 1–2 則）：M4 決定接受、不解（Q296）。之後要解的話，重用 M4 的 `resync` frame：已 ready 的房間又收到 subscribe 確認 = 剛重連過
- [x] PubSub 連線 half-open 偵測（M4，2026-11-05）：每 15 秒 PUBLISH 給自己的心跳 channel，連續 2 次沒收到就重建 PubSub，訂閱確認後送 `resync`；用「不斷線但不轉發」的 proxy 實測 35～44 秒恢復，見 [ADR 0011](docs/design/0011-message-delivery-recovery.md)
- [ ] Redis 不回應時 `POST` 可能卡約 5 秒（2026-11-05 M4 驗收時發現，既有行為）：`publishTimeout` 的 500ms 擋不住 socket I/O，因為沒開 go-redis 的 `ContextTimeoutEnabled`；要開的話影響所有 Redis 呼叫，先確認 pool 和 PubSub 的行為再做
- [ ] 接 CDN（要先有自訂網域；有流量再做）：`GET .../messages` 已帶 `s-maxage=1`，application 不用改。**要另外確認 CDN 有類似 `proxy_cache_use_stale updating` 的設定**，實測只開 `proxy_cache_lock` 時，快取到期的 1,000 個併發全部回源。錯誤回應已是 `no-store`，但也要確認 CDN 對 404 這類錯誤的預設行為
- [ ] `maxClients` 每台 100、被拒的連線還是會先 upgrade：M6 壓測前要決定上限，以及要不要在 upgrade 前就拒絕（在 upgrade 前拒絕的話，`onopen` 不會觸發，滿房的人就看不到歷史，要一起想）
- [ ] Store integration test 不能同時跑兩份：共用 `danmaku_test` / Redis db 15，3 個 Judge 同時跑時每次 fail 的 test 都不一樣（單獨跑都過）。CI 平行化時要每個 runner 各自的 DB / Redis db
- [ ] 彈幕非同步保存（量大時）：目前 `POST /rooms/{public_id}/messages` 同步 INSERT 成功才廣播，每則彈幕最貴的是寫入（WAL + fsync），不是查 username。改成送達路徑（pub/sub）不等 DB、保存路徑走 queue 批次寫入；要處理 queue 掛掉、已廣播但沒存到的情況
- [ ] Hub 抽象（分離分發邏輯與 transport），新增 SSE endpoint 與 WS 並存（SSE `Last-Event-ID` 可搭配 replay）
  - 順便拆 `ws.go`（W4b 已先把長函式拆小，檔案沒拆）：照 Hub 之後的邊界拆（例如 `hub.go` 管連線 / 廣播、`client.go` 管單條連線讀寫），不要先照 auth / room 拆一次再重拆；純搬家和改 code 分兩個 commit，diff 才 review 得了
    - 2026-11-03：決定 B3 之後**先做這個拆分**（提前，不等 W6 其他項；SSE 仍留在 W6）。B3 寫完時觀察到的亂：`WebsocketHandler` 一個 struct 裡擠了四層職責 —— 連線傳輸（`conn` / `send` / `writeLoop` / `writeRaw` / `enqueueLocked`）、協定編碼（`writeEnvelopeLocked` / `reply`）、路由與成員（`clients` / `broadcastToRoomLocked`）、身分（`userID` / `exp` / `timer` / `handleAuth`）；而且函式有的吃 `conn`、有的吃 `*wsClient`，中間靠 `this.clients[conn]` 轉換。方向：gorilla chat 的 Hub / Client，函式一律傳 `*Client`
    - 之後可考慮：Hub / outbox 的 frame 從 `[]byte` 改成 `string`（`chan string`，寫入前才轉）—— 型別本身保證不可變，廣播共用同一份不用靠「只讀不寫」的約定；代價是 `json.Marshal` 結果要多轉一次。拆分時決定先維持 `[]byte`（只搬不改行為）
    - ✅ 2026-11-03 拆分完成：`internal/ws`（`Client` 傳輸層、`Hub` 註冊表，只處理 `[]byte`、鎖底下不做 I/O / 不呼叫外部程式碼）+ `handler/ws.go`（HTTP 入口、Redis / DB / envelope）+ `handler/ws_session.go`（每條連線的業務）+ `handler/ws_protocol.go`；`Client.Close` 用 `sync.Once` 變成冪等（先 close(done) 再關 conn），刪掉 `Shutdown`。Hub 抽象的另一半（SSE endpoint）仍留在 W6
    - 已知、接受：PingLoop「寫入失敗先看 done」沒有 test（要 ticker 剛好在 Close 那瞬間觸發，寫不穩）
  - 每條連線自己的寫入 goroutine 已在 B3 做好；WS 連線的身分（user、exp、timer）之後可改用每條連線各自的 mutex：目前仍用全站那把鎖（B3 之後鎖底下已不做 I/O，拿著的時間很短，拆鎖省不到多少）；要拆時上鎖順序固定「全站 → 單條連線」
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
- [ ] Rate limiting（middleware）
  - `/login`、`/register` 的部分提前到 W4-3（部署前就要擋撞庫 / 暴力猜密碼），這裡處理其餘 endpoint
  - Limiter 呼叫加短 timeout（例如 `context.WithTimeout` 100ms）：Redis 網路黑洞時 go-redis 預設 dial timeout 5 秒，一次 login 呼叫 limiter 4 次，fail open 最慢要等約 20 秒
  - 全域基本限制（每 IP 每秒 N 個 request，nginx `limit_req` 或 middleware）：目前只有個別 endpoint 的嚴格限制。依「被濫用的代價」決定而不是 HTTP method：`GET /rooms/{public_id}/messages?limit=10000` 一樣貴
  - WS 連線數限制：WS 不用登入就能連，現在只有每條連線 5 frame/秒（`listenConnection`），擋不住「開很多條」
    - 開 100 條閒置連線就佔滿 `maxClients`，正常使用者連不上；keepalive 之後閒置連線會回 pong，不會被 read deadline 踢掉
    - 開 100 條、每條每秒送 5 個 → 合計每秒 500 次搶全域鎖回應，廣播變慢
    - 方向：每 IP 最多 N 條 WS（nginx `limit_conn` 或 upgrade 前在 middleware 擋），多台 server 時要跨 instance 計數（Redis）
- [ ] Trusted proxy 收尾（client IP 不可偽造的前提）
  - 本機 server-1/2 直接開 port（5388/5389）可繞過 nginx 偽造 `X-Real-IP`：拿掉 docker-compose 的 `ports:`，只留 nginx 對外（代價：不能再繞過 nginx 直接 curl 單台 server）
  - Defense in depth：只有 `RemoteAddr` 是 trusted proxy 的 IP / CIDR 時才信任 header，其他來源退回 `RemoteAddr`（概念同 Gin `SetTrustedProxies`）
- [ ] IPv6 限流 key 改用 /64 前綴（PLAUSIBLE，要看 Railway edge 會不會給 v6）：目前用完整位址，持有一個 /64 就能每次換 IP 繞過 IP 限流，可跟 trusted proxy 收尾一起做

## Backlog

> 知道、刻意先不做（2026-11-03 從 W5 移入）

- [ ] 房主 / 房管透過 WS 直送彈幕（2026-11-06 從 RBAC 移出，不一定會做）：好處只有少一次 HTTP，要多一條寫入路徑、自己做 ack 和去重、降級時處理已連上的連線。等 M6 顯示 `POST` 真的太慢再談

- [ ] 彈幕往上捲載入更舊的（`GET /rooms/{public_id}/messages?before=<id>`，`WHERE id < $1 ORDER BY id DESC`）：直播彈幕很少往回翻；前端要處理往上插入時固定捲動位置。若之後做回放（彈幕綁影片時間軸），翻頁依據會變成時間點而不是 id，到時再設計
- [ ] Login CAPTCHA（reCAPTCHA / Turnstile 等，接外部服務前先討論選型）
  - 補限流擋不到的兩個洞：攻擊者分散到大量 IP（每個 IP 都低於上限、credential stuffing 每個帳號只試一次）；per-username 限流可被拿來故意鎖別人的帳號（DoS）
  - 失敗超過門檻時改成要求驗證，而不是直接 429：真人解完照樣登入，bot 每次嘗試都要付成本
- [ ] CORS origin canonical 檢查的剩餘缺口（低優先）：port 前導 0（`:08443`）、非 ASCII 的 IDN host（應寫 punycode）目前仍會通過
- [ ] 小修（2026-10-30 Judge，低優先）：`dummyHash` 產生失敗的 error 被吞掉（應 fail fast，否則 timing 防護無聲失效）；`ContextHandler.WithGroup` 之後 `request_id` 會被放進 group 裡（目前沒人呼叫）；`UserIDFromContext` 多餘的 `if !ok`；receiver 命名 `this` 不符 Go 慣例（staticcheck ST1006，要改就一次機械性 rename）
- [ ] 密碼外洩檢查：register 時查 HIBP Pwned Passwords API（取代 / 補強 W4-3 embed 的常見密碼清單）
  - k-anonymity：只送 SHA-1 前 5 碼，比對在本機做，原密碼與完整 hash 都不離開 server
  - 要決定外部服務慢 / 掛掉時 fail open（放行）還是 fail closed（擋下），並加 timeout
  - 順帶評估 NFKC 正規化（`golang.org/x/text/unicode/norm`，新套件），避免同字不同編碼登不進去
- [ ] API versioning
- [ ] 評估引入 OpenTelemetry（W4 結束後決定）
  - metrics 改走 vendor-neutral API，backend 可替換
  - 評估 tracing 的 trace ID 是否取代 W4a 的 request ID
