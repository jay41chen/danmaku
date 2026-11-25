# Danmaku

[English](README.md) · 繁體中文

直播房間的即時彈幕系統：觀眾送出的訊息會即時飛過所有人的畫面。後端用 Go 寫，設計成可以水平擴充（多台 server）；前端是 React。

**線上 Demo**：暫停中，後端主機的試用期已結束。想試用請在本機啟動（見 [Getting Started](#getting-started)）。

我原本做 mobile（Kotlin / Flutter），這個專案是用來學後端的。重點放在教學文章通常不會講的部分：server 不只一台時、Redis 卡住時、client 很慢時、新舊版本同時在線時，會壞在哪裡、要怎麼設計。比較重要的設計決策都寫在 [`docs/design/`](docs/design/README.md)（目前只有英文）。

## 功能

- **房間**：有房主，用猜不到的 public id 當網址；soft delete；每人房間數上限
- **即時廣播**：WebSocket + Redis Pub/Sub（每個房間一個 channel），跨多台 server
- **帳號**：註冊 / 登入、JWT access token、會 rotate 的 refresh token（偵測 token 被偷）
- 不登入也能看；在已經連上的 WebSocket 裡登入就會升級身分
- **自動恢復**：client 重連後補上最新訊息；server 收不到 Redis 訊息時會自己發現並恢復
- **房間人數**：跨 server 計算，多個分頁算一人；掛掉的 server 上的觀眾會自己過期。首頁把人最多的房間排在前面
- **防濫用**：按 user、IP、username（登入）限流；登入防帳號列舉（timing）；認得 Unicode 的輸入驗證
- **可維運**：JSON 結構化 log + request ID（WebSocket 也串得起來）、Prometheus metrics、panic recovery、graceful shutdown、啟動時自動跑 migration

## 設計重點

| 問題 | 決策 | 設計文件 |
|---|---|---|
| 全站一個 channel 時，每台 server 都收全站的流量，加機器沒有用 | 每個房間一個 Redis channel；每台 server 只訂閱自己身上有觀眾的房間，人走光後延遲 30 秒才取消訂閱（避免 0↔1 一直跳）；`join_ok` 等 Redis 確認訂閱好才回，中間不漏訊息 | [0001](docs/design/0001-per-room-pubsub.md) |
| Redis 卡住時，整台 server 的進房 / 離房卡 5–15 秒，一個 timeout 還會把所有房間的連線一起重置 | Reconciliation loop：事件只在記憶體標 dirty，單一 worker 比對 desired / actual，在鎖外跟 Redis 溝通 | [0002](docs/design/0002-reconciliation-loop.md) |
| 一支網路很慢的手機，就能讓所有房間的廣播卡 10 秒 | 每條連線自己的 outbox + 寫入 goroutine；outbox 滿了就斷開那條連線，client 重連後補抓 | [0003](docs/design/0003-slow-client-outbox.md) |
| token 被偷之後的「撤銷全部」，攻擊者可以刻意繞過 | 改成每個 user 一個 `valid_after` 時間點，不再列舉 token 狀態；明確處理鎖、TOCTOU、`clock_timestamp()` | [0004](docs/design/0004-refresh-token-revocation.md) |
| 加 `NOT NULL` 欄位會讓部署期間還在跑的舊版壞掉 | Expand / contract，分兩次部署 | [0005](docs/design/0005-expand-contract-migration.md) |
| 彈幕、房名裡的零寬字元、RLO、Zalgo | 用 PRECIS（RFC 8264）只擋不合法的文字、不改寫；版面問題交給前端隔離 | [0006](docs/design/0006-input-validation.md) |
| 任何人亂打 URL 就能讓 Prometheus 記憶體爆掉 | Metrics label 只用已知的值（route pattern、標準 method），並用走真實 middleware chain 的 test 守住 | [0007](docs/design/0007-metrics-cardinality.md) |
| 用 offset 分頁的話，列表在翻頁期間有增減就會重複或漏掉房間，而且越後面越慢 | 用 `(created_at, public_id)` 做 keyset 分頁並建 partial index；多撈一筆判斷有沒有下一頁；page token 對 client 是 opaque，server 嚴格解析（不認識的欄位、後面多接資料、版本、id 格式不對都回 400） | [0008](docs/design/0008-cursor-pagination.md) |
| 一個 13 個方法的 store interface：每個 handler 都依賴用不到的方法，test 的 fake 少實作方法也能編譯，handler 還為了錯誤值 import Postgres 的 package | 每個 handler 自己宣告只含它會呼叫的方法的小 interface（Go 的 interface 是隱式符合）；domain error 放在不依賴任何東西的 `model`；Postgres 和 Redis 分成兩個 package，只有 `main.go` 知道底下用什麼 | [0009](docs/design/0009-consumer-side-interfaces.md) |
| 跨 server 的人數要把同一個人的多個分頁算成一人，還要撐得過 server 沒清理就掛掉 | 每個房間一個 Redis Sorted Set，每條連線一筆；沒續期就過期，時間一律用 Redis 的；每台 server 只有一個 worker 比對 Hub 和上次寫入的內容，舊快照不會蓋掉新的；寫入失敗時寧可短暫少算，也不要留 30 秒的幽靈 | [0010](docs/design/0010-room-presence.md) |
| PubSub 連線可能 half-open，整台 server 的所有房間靜靜收不到訊息，但 ping 和 HTTP 都正常 | 每台 server 定期發心跳給自己，再從那條連線收回；連續 2 次沒收到就重建連線，並通知 client 重新撈訊息。用「不斷線但不轉發」的 proxy 實測：35～44 秒恢復 | [0011](docs/design/0011-message-delivery-recovery.md) |
| 房主擋不了搗亂的人，而舊計畫的全站記憶體封鎖又和每間房各自的角色對不上 | 房間級 shadow ban，讀取時才過濾，誤 ban 可以完全復原；`room_roles` 一人一列；階級規則集中在一個預設拒絕的純函式；檢查順序讓一般觀眾無法探測 user；所有 API 都不會告訴他被 ban。用兩台 server + 真 Redis 和瀏覽器實測 | [0012](docs/design/0012-room-moderation.md) |
| 業務規則散在 handler 和 store；HTTP 和 WebSocket 各自直接呼叫 store 查「房間存在」「這個 user 是誰」，timing 防護的測試還得透過 HTTP 才測得到 | 每個領域一個 use case，不碰 HTTP、SQL、WebSocket 格式；handler 只留傳輸層的事（client IP、連線狀態、保護讀取迴圈的 timeout）。行為不變靠三件事：handler 斷言不改、先在舊 code 上跑過的 characterization test、mutation testing | [0013](docs/design/0013-use-case-layer.md) |
| 聊天類系統常把寫入改成非同步，代價是可能「已廣播但沒存到」 | 先量再決定：100 則／秒時 `POST` p99 約 6 ms，到 2,500 人 × 100 則／秒寫入都在 3 ms 以下、不隨負載變慢，CPU 花在發送給每個觀眾。維持同步寫入。5,000 人 × 100 則時測試用的 Mac 先卡住（證據指向核心網路記憶體耗盡），其中一台 server 因為卡住的寫入用光 thread 而 crash | [0014](docs/design/0014-load-baseline-keep-sync-writes.md) |
| 依人數排序要把人數存起來，而且沒人看的房間要能自己從榜上消失，不需要有人發現「它沒人了」 | 一個 Redis Sorted Set 存人數，每次算完就寫；房間只要 presence key 還在就留在榜上，最後一個人離開，或持有最後觀眾的 server 掛掉時，這個 key 都會自己消失，所以由讀的人順手清。只排前 N 名：人數一直在變，用它當 cursor 會漏或重複 | [0015](docs/design/0015-popular-rooms-ranking.md) |
| 測試只在本機 hook 跑；integration test 缺 DB 環境變數時會 skip，CI 設錯也是全綠、但什麼都沒測 | CI 起真的 Postgres 和 Redis；有 `CI` 環境變數時缺測試變數直接 fail，本機照舊 skip。第一次跑就抓到一個測試 package 只是靠別的 package 先 migrate 了本機 DB 才會過。image 從 1.87 GB（整套 toolchain、root）降到 39.8 MB（distroless 上的靜態執行檔、non-root） | [0016](docs/design/0016-minimal-ci.md) |
| client 連著但不讀資料；macOS 上幾千個卡在 kernel 的寫入曾讓 server crash | 故障實驗用獨立的 compose project，不讀資料的 client 跑在 Docker 網路裡面。1 條卡住時由 75 秒的 pong 逾時踢掉；3,000 條時，核心的 TCP 記憶體（跟同一台 VM 裡的測試 client 共用）超過 high 線之後才出現第一次 outbox 踢人（只是時間上吻合，沒證明因果），44 秒內全部踢掉。Linux 上的 server 最多 22 條 thread（Docker Desktop VM、兩次各單次） | [0017](docs/design/0017-stuck-clients-fault-env.md) |
| Redis 重啟時 go-redis 自己重連，停機期間的訊息全部遺失、而且沒有任何人被通知（停 10 秒漏 52 則、60 秒漏 303 則） | 彈幕是當下的反應，送達只做到盡力而為。已訂閱的房間又收到訂閱確認時視為重連、送 `resync`，client 隨機等 0～2 秒再補撈。停 60 秒補回最新 99 則；停 10 秒全部補回，或只缺最後 1～3 則：發送比接收晚恢復，最多約 0.6 秒（3 次單次實驗，每次不同） | [0018](docs/design/0018-redis-restart-best-effort-resync.md) |
| server 被 kill 時，連到它的 IP 會卡住；nginx 等 60 秒、比 client 的 10 秒還長，所以從沒發現、一直轉給它。重連最久 25.7 秒、35 個 POST 失敗，一個 viewer 撈歷史 timeout 又沒重試，永久缺 105 則 | nginx 連線 2 秒沒成功就放棄，同一個請求改送另一台（還沒送出去，所以連 POST 也安全），並 10 秒內不再用死掉那台；client 撈歷史失敗最多重試 3 次。修改後（單次）：0.6～0.9 秒重連、漏的 18 則全補回、600 個 POST 全部成功 | [0019](docs/design/0019-server-kill-fast-connect-failover.md) |
| 只有一台 server 連不到 Redis（封包被丟、沒有 RST），Redis 本身正常：HTTP 看起來健康，所以流量照樣進來，但限流卡住 POST、訂閱也斷了。60 秒內 128 個 POST 等到 client 放棄；這台的觀眾連另一台發的訊息都收不到，永久缺 1140 則；另一台的觀眾缺 13 則沒廣播出去的訊息，而且沒有任何人通知它們 | 讓 ctx 期限真的套到 Redis 的 socket 上；限流每次 200 ms、presence worker 每次 1 s，超過就放棄（限流 fail open）。修正後（單次）POST 全部 201、最慢約 0.9 秒；presence 2.6 秒內就記錯誤、不再卡 55 秒。代價：失敗從發文的人轉到看的人，存了但沒廣播的訊息從 13 則變 154 則，另一台的觀眾默默漏掉。接著做延後補發 `resync`：publish 失敗的 server 在記憶體記下房間（dirty flag），下次 publish 成功時經 Redis 補發一則 `resync`。重跑：另一台的觀眾在恢復後約 1 秒全都收到 `resync`，每人補回 46 則；其餘超出「最新 100 則」的歷史範圍，以彈幕盡力而為的定位接受。最後解開 55 秒：go-redis 每次撥號會自己重撥 5 次，沒設期限的呼叫卡多久要看連線池狀態（實測 20 秒到超過 100 秒）。改用自己的 client 預設（讀 1 秒、撥號 1 秒、每次只撥一次）壓到約 4 秒，剩下沒期限的 request 路徑也補上期限（`GET /rooms?sort=viewers`：81 秒 → 0.5 秒） | [0020](docs/design/0020-redis-blackhole-gray-failure.md) |
| Redis 恢復後，所有觀眾同時重撈歷史，而且每人會收到 3 個 `resync`（自己 server 重連 1 個，加上每台 server 的延後補發各 1 個），平均每人撈約 2 次。在一台筆電上，5,000 人變慢（GET p99 351 ms），10,000 人有 6,195 個 GET timeout | 先量再改。client 改成先固定等 2.5 秒、再加隨機 0～2 秒，讓 resync 合併：每人剛好撈 1 次，5,000 人 GET p99 21 ms，10,000 人零失敗。不加快取也不加 singleflight。pgx 連線池原本沒設上限（預設 8）；16 條比 8 條和 32 條都好（CPU 已經滿時，同時跑更多查詢會讓每個查詢都變慢），所以改成每個部署在 `DATABASE_URL` 自己設。偶發的過載查到是測試用筆電只有 4 顆快核心，不是 GC、開新連線或過熱。系統過載時限流會 fail open | [0021](docs/design/0021-resync-get-spike-and-pool-size.md) |
| 限流查不到結果時一律放行，所以 server 過載時限流會自己關掉；攻擊者可以先灌便宜的 API 讓 server 過載，再趁機猜密碼。login / register 還會跑 bcrypt，放行只會讓負載更重 | 每條規則自己決定：login / register fail closed，回 503 `auth_unavailable`、不送 `Retry-After`（server 不知道多久會恢復，不亂給數字）；refresh 和發彈幕維持 fail open，Redis 掛掉時已登入的人照常使用。另外加一個 counter 計所有 limiter 錯誤（放行或擋下都算） | [0022](docs/design/0022-auth-limiter-fails-closed.md) |

## 架構

```mermaid
flowchart LR
    sender["Browser"] -- "POST /rooms/{id}/messages" --> A["Server A"]
    A -- INSERT --> pg[("PostgreSQL")]
    A -- "PUBLISH room:{id}" --> ps[["Redis Pub/Sub<br/>每個房間一個 channel"]]
    ps -- "只有身上有該房間觀眾的<br/>server 會訂閱" --> B["Server B"]
    B -- "WebSocket<br/>進房 · 收彈幕" --> viewer["Browser"]
```

- 前面有 load balancer：本機是 Nginx（`least_conn`），production 是 Railway。server 沒有不能重建的狀態（token 用共用的 secret 驗、refresh token 在 PostgreSQL、限流計數和廣播在 Redis），所以**不需要 sticky session**
- 發彈幕走 HTTP；WebSocket 只用來收訊息、進房和登入
- 本機兩台 server + Nginx、Railway replicas = 2 都驗證過

每台 server 只訂閱自己身上有人的房間：第一個人進房時 `SUBSCRIBE`，最後一個人離開後**等 30 秒**才 `UNSUBSCRIBE`（這段時間內有人回來就不用重新訂閱，避免冷門房人數在 0↔1 之間跳時一直送指令）。所以用 `redis-cli PUBSUB CHANNELS 'room:*'` 排查時，房間沒人後 channel 還會留約 30 秒，不是訂閱洩漏。

### server 內部

request 怎麼走（箭頭是**呼叫**）：

```mermaid
flowchart TD
    req(["HTTP request / WebSocket frame"]) --> handler
    handler["<b>handler</b><br/>decode · 算 client IP · 決定 status code · 管 WebSocket 連線"]
    handler -- "呼叫 roleUseCase、authUseCase…<br/><i>interface 寫在 handler 裡</i>" --> usecase
    usecase["<b>usecase</b><br/>規則：先 400 再 429 · 限流規則 · timing 防護"]
    usecase -- "呼叫 roomStore、authLimiter…<br/><i>interface 寫在 usecase 裡</i>" --> stores
    usecase -- "呼叫 roomNotifier、messageNotifier<br/><i>interface 寫在 usecase 裡</i>" --> notifier
    stores["<b>pgstore / redisstore</b><br/>SQL · Redis"]
    notifier["<b>handler.RoomNotifier</b><br/>WebSocket 訊息格式 · PUBLISH"]
```

同一批 package 對照 Clean Architecture 的同心圓。這張圖的箭頭是**「需要知道誰」**：哪一方得認識另一方的合約，全部往內指。所以上一張圖裡在 usecase「下面」的 store，這裡在 usecase「外面」：use case 定義自己需要什麼，store 接上去。在 Go 裡只有 `usecase → model` 是真的 import，其他箭頭都靠 interface 接上（見最後一條）

```mermaid
flowchart LR
    subgraph FD["Frameworks & drivers（做 I/O 的框架）"]
        libs["net/http · gorilla/websocket · pgx · go-redis"]
        subgraph IA["Interface adapters（轉接層）"]
            handler["<b>handler + ws</b><br/>HTTP / WebSocket 進來"]
            notifier["<b>handler.RoomNotifier</b><br/>廣播出去"]
            stores["<b>pgstore / redisstore</b><br/>存取資料（出去）"]
            subgraph UC["Use cases（業務規則）"]
                usecase["<b>usecase</b>"]
                subgraph EN["Entities（領域型別）"]
                    model["<b>model</b>"]
                end
            end
        end
    end
    handler -- 呼叫 --> usecase
    notifier -. "實作 usecase 的 port" .-> usecase
    stores -. "實作 usecase 的 port" .-> usecase
    usecase -- 使用 --> model
```

- **Entities：`model`**。資料型別、domain error（`ErrRoomNotFound`…）和小規則（`ValidPublicID`），只依賴標準函式庫
- **Use cases：`usecase`**。業務規則，一個領域一個 struct（房間、角色、彈幕、auth），不碰 HTTP、SQL、WebSocket 格式。每個 use case 自己宣告 **port**，也就是「需要外面提供什麼」的 interface（`roomStore`、`authLimiter`、`roomNotifier`…）。另外會用到不做 I/O 的純函式工具：`auth`（bcrypt、JWT）、`textrule`、`ratelimit`。見 [ADR 0013](docs/design/0013-use-case-layer.md)
- **Interface adapters（轉接層）**。進來：`handler`（加上它的 HTTP 工具 `httpx`、`middleware`）把 HTTP / WebSocket 轉成 use case 呼叫、把結果轉成回應；它也管 WebSocket 的連線狀態（`ws` 的 Hub、每間房的訂閱），這是傳輸層的事，不是業務規則。出去：`pgstore`、`redisstore`、`handler.RoomNotifier` 實作 use case 的 port。handler 自己傳輸層的需求（`wsPubSub`、`healthChecker`…）也是小 interface，由 store 直接符合
- **Frameworks & drivers**。只有轉接層會碰做 I/O 的套件（net/http、gorilla/websocket、pgx、go-redis）。例外是 metrics：`internal/prometheus` 是 process 全域的 facade，哪一層都可以呼叫
- **`cmd/server`**（`main.go`）在同心圓外面：composition root，唯一看得到所有具體型別、也唯一知道後端是 Postgres 和 Redis 的地方
- 多數語言裡，adapter 要 import use case 才能實作它的 port；Go 的 interface 是隱式實作，所以 `handler` 和 store 都不用 import `usecase`（測試除外）。見 [ADR 0009](docs/design/0009-consumer-side-interfaces.md)
- 其他 package：`config`、`logging`、`randx`

## 怎麼做的

- 每個改動都走「設計 → 實作 → 獨立審查」；修正用 **mutation testing** 驗證：故意把 code 改壞，確認有 test 會 fail
- Integration test 打真的 PostgreSQL 和 Redis，並開 `-race`；push 前在本機跑，push 後在 GitHub Actions 用全新的 DB、不吃測試快取再跑一次，CI 上缺測試環境變數會 fail 而不是 skip
- 問題先在本機重現再修：讓 Redis 暫停（`CLIENT PAUSE`）、用 proxy 丟包、砍掉 Pub/Sub 連線、永遠不讀的 client

## 進度

2026-10-08 開始。已完成：跨多台 server 的即時彈幕、refresh token rotation 的登入機制、房間與分頁的房間列表、跨 server 的房間人數和熱門排行、漏訊息的自動恢復、房間角色和 shadow ban、限流、可觀測性、壓測基準（寫入維持同步）、server CI、腳本化的故障情境和各自的修正，以及上面那些強化。接下來：client 的彈幕層，然後讓 prod 重新上線並加上最小監控。詳見 [ROADMAP 的 Milestones](ROADMAP.md#milestones2026-11-03-訂暫定)。

## Tech Stack

- **Backend**: Go + gorilla/websocket + pgx
- **Frontend**: React + TypeScript + Vite
- **Database**: PostgreSQL 17
- **Cache**: Redis 7 (Pub/Sub)
- **Reverse Proxy**: Nginx 1.27
- **Infrastructure**: Docker Compose；GitHub Actions（server CI）
- **Observability (local dev)**: Promtail → Loki → Grafana, Prometheus → Grafana
- **Middleware**: Request ID → Request logging → Metrics → Panic recovery → CORS（`net/http` + 自製 chain）
- **Metrics labels**：`http_requests_total` / `http_request_duration_seconds` 的 `path` 是 route pattern（如 `/rooms/{public_id}`，沒比對到 route 記成 `unmatched`），`method` 只收 HTTP 標準的九個（其他記成 `other`）；label 值不由 client 決定，避免被亂打 URL 撐爆 time series
- **Logging**: slog JSON，透過 context 自動帶 `request_id`
- **Reliability**: Panic recovery middleware, graceful shutdown (SIGTERM/SIGINT)
- **Schema Migration**: golang-migrate，server 啟動時自動套用，SQL embed 進 binary
- **Auth**: JWT access token（HS256，1 小時）+ opaque refresh token（rotation + duplicate use detection）；route 預設要登入，公開的要明寫 `publicGet` / `publicPost`

## Getting Started

### Prerequisites

- Go 1.27+
- Node.js 18+
- Docker

### Docker Compose（多 instance）

```bash
docker compose up --build
```

啟動後包含：PostgreSQL、Redis、兩個 Go server、Nginx load balancer、Loki + Promtail + Grafana（log aggregation）、Prometheus（metrics）。

| Service | URL |
|---|---|
| Nginx 入口 | `http://localhost:5390` |
| Server-1 / Server-2（直連，方便對 log） | `http://localhost:5388` / `http://localhost:5389` |
| Grafana | `http://localhost:5392`（admin / admin） |
| Prometheus | `http://localhost:5393` |

前端連線：

```bash
VITE_WS_URL=ws://localhost:5390/ws VITE_API_URL=http://localhost:5390 npm run dev
```

### Local Development（單機開發）

1. 單獨啟動 PostgreSQL 和 Redis：

```bash
docker run -d --name danmaku-db -p 3192:5432 -e POSTGRES_USER=danmaku -e POSTGRES_PASSWORD=danmaku -e POSTGRES_DB=danmaku -v danmaku_pgdata:/var/lib/postgresql/data postgres:17
docker run -d --name danmaku-redis -p 2907:6379 --tmpfs /data redis:7 --save ""
```

2. 啟動 Go server（啟動時會自動建立 / 更新資料表，見 [Database Migration](#database-migration)）：

```bash
cd server
go run ./cmd/server
```

3. 啟動前端：

```bash
cd client
npm install
npm run dev
```

4. 開啟 http://localhost:5173/danmaku/

### Git hooks

每個 clone 設定一次。pre-commit 檢查 `gofmt`、`go vet` 和前端 lint；pre-push 依 `server/`、`client/` 有沒有變動，跑全部測試和前端 build。測試用獨立的 DB，以及被 gitignore 的 `server/.env.test`（從 `.env.test.example` 複製），也要設定一次：

```bash
git config core.hooksPath .githooks
docker exec danmaku-db createdb -U danmaku danmaku_test
cp server/.env.test.example server/.env.test
```

跑測試用 `server/test.sh`：它會載入 `.env.test`，再跑 `go test -race -p 1`（預設 `./...`，參數會原樣傳下去，例如 `server/test.sh ./internal/pgstore -run X -count=1`）。直接跑 `go test` 而沒設這兩個變數時，integration test 會 skip。

### 壓測工具

`server/cmd/loadtest` 用來在本機判斷「寫入是不是第一個瓶頸」。答案是不是，基準數字見 [ADR 0014](docs/design/0014-load-baseline-keep-sync-writes.md)。同一台 Mac 的數字只能比較前後，不代表容量上限；本機每秒 25 萬個 frame 正常、50 萬個時整台機器的連線一起卡住（證據指向核心網路記憶體耗盡）。

- **只打本機：** 只接受 `localhost` / `127.0.0.1`、`danmaku_test` 和 Redis db 15，其他一律拒絕，結束時只清掉自己這次建立的資料。
- **情境：** `write`（純寫入）、`mixed`（單間熱門房，流量逐步加大）、`fanout`（大量觀眾、少量彈幕）、`rooms`（500 間小房）。
- **延遲：** 以固定速率發送，從排程時間開始算，所以 server 變慢不會讓延遲被低估；timeout 也計入 p99。
- **有效性：** 機器不夠閒、工具自己來不及、錯誤達 1% 時，那一級會標成無效。要在 Mac 閒置時跑。

先起 2 台 server，都連 `danmaku_test`，並設 `LOADTEST_TIMING_LOG=true`、`WS_MAX_CLIENTS=6000` 和同一把 `JWT_SECRET`。工具用同樣的 `JWT_SECRET`、`TEST_DATABASE_URL`、`TEST_REDIS_URL`：

```bash
cd server && go build -o /tmp/loadtest ./cmd/loadtest
/tmp/loadtest -scenario write -write-rate 100 \
  -servers http://127.0.0.1:6388,http://127.0.0.1:6389 \
  -server-pids "$PID1,$PID2" -server-logs s1.log,s2.log -out write.json
```

## API

| Method | Path | 要登入 | 說明 |
|---|---|---|---|
| `GET` | `/rooms/{public_id}/messages` | | 該房間的歷史訊息；房間不存在或已刪除回 `404`，沒有訊息回 `[]`。發送者已刪除的訊息 `username` 是 `null` |
| `POST` | `/rooms/{public_id}/messages` | ✓ | 在該房間發彈幕 `{"content": "..."}`：最多 100 字、不可全空白、不可含控制字元 / 零寬字元 / 雙向控制字元（PRECIS Freeform，RFC 8264；換行也不行）。只檢查不改寫：存的是原樣；按 user 限流（3 則 / 1 秒 + 20 則 / 10 秒）。房間不存在或已刪除回 `404`。成功回 `201` + 訊息，並廣播給已 `join` 這個房間的 WS 連線 |
| 任何 method | `/messages` | | **已移除**：回 `410 Gone`，請改用 `/rooms/{public_id}/messages` |
| `POST` | `/rooms` | ✓ | 建立房間 `{"room_name": "..."}`：1～30 字（彈幕的 100 字也一樣：以 Unicode code point 計，❤️ = U+2764 + U+FE0F 算 2 個），字元規則同彈幕，另外**頭尾不可有空白**（不幫忙 trim，直接回 `400`）、不可有連續空白、不可有超過 2 個連續的 combining mark；每個 user 最多 3 個未刪除的房間，超過回 `403`。成功回 `201` + 房間 `{"public_id", "room_name", "owner_username", "created_at"}` |
| `GET` | `/rooms/{public_id}` | | 查房間；不存在或已刪除一律回 `404` |
| `GET` | `/rooms` | | 房間列表（未刪除，新的在前），cursor 分頁：`?limit=`（預設 20、最多 50）、`?page_token=`（上一頁回的 `next_page_token`，原樣帶回）。回 `{"rooms": [{"room_name", "public_id"}], "next_page_token"}`，沒有下一頁時不帶 `next_page_token`；`limit` 不合法回 `400`（`invalid_limit`），token 不合法回 `400`（`invalid_cursor`）。`?sort=viewers`：人最多的房間在前，每筆多 `viewer_count`，`limit` 規則同上，不分頁（帶 `page_token` 回 `400 page_token_not_supported`），`sort` 其他值回 `400 invalid_sort`，讀不到排行回 `503 viewer_ranking_unavailable` |
| `GET` | `/me/rooms` | ✓ | 自己的房間列表，參數和回應同 `GET /rooms`。`Cache-Control: no-store` |
| `GET` | `/rooms/{public_id}/me` | ✓ | 自己在這間房的角色 `{"role": "owner" \| "moderator" \| "viewer"}`；被 ban 的人也回 `viewer`。`Cache-Control: no-store` |
| `GET` | `/rooms/{public_id}/roles` | ✓ | 房管和被 ban 的人（只有房主和房管能看，否則 `403 room_forbidden`），分頁參數同 `GET /rooms`。回 `{"roles": [{"user_id", "username", "role", "assigned_by", "assigned_by_username", "created_at"}], "next_page_token"}`。`Cache-Control: no-store` |
| `PUT` | `/rooms/{public_id}/roles/{user_id}` | ✓ | 指派房管或 ban：`{"role": "moderator" \| "banned"}`，回 `204`，可重送。規則見下方 |
| `DELETE` | `/rooms/{public_id}/roles/{user_id}` | ✓ | 撤銷房管或解除 ban，回 `204`，可重送 |
| `GET` | `/ws` | | WebSocket，接收即時廣播。連上後要送 `join` 進房才收得到該房間的彈幕；連上是匿名，可在連線內送 auth frame 登入（見下方） |
| `POST` | `/register`、`/login` | | 註冊 / 登入，`/login` 回 access token + refresh token |
| `GET` | `/me` | ✓ | 目前登入的 user `{"id", "username"}`；F5 後 client 用它取得身分。`Cache-Control: no-store` |
| `POST` | `/refresh`、`/logout` | | 用 refresh token 換新的一組 / 作廢 |
| `GET` | `/healthcheck`、`/metrics` | | 健康檢查、Prometheus metrics |

- 要登入的 endpoint 帶 `Authorization: Bearer <access token>`。沒帶、token 無效或過期一律回同一個 `401`（`WWW-Authenticate: Bearer`），client 用 refresh token 換新的再重送
- HTTP 錯誤一律回 `{"error": {"code": "...", "message": "..."}}`，`Cache-Control: no-store`。`code` 給程式判斷、client 用來對應翻譯，`message` 是後備文字。例外：打錯路徑或用錯 method 時，mux 內建的 `404` / `405` 是純文字
- `/login` 失敗（格式不合法、帳號不存在、密碼錯）一律回同一個 `400`（`invalid_credentials`），不是 `401`：讓 `401` 只代表 access token 失效，client 的自動 refresh 不用排除 `/login`
- 角色：房主 > 房管 > 一般觀眾，只能上對下；只有房主能指派或撤銷房管；房管能解除房主 ban 的以外的 ban。沒有權限回 `403 room_forbidden`，動不了這個對象回 `403 room_target_forbidden`，指派被 ban 的人回 `409 room_role_conflict`，對象不存在回 `404 user_not_found`，`role` 不合法回 `400 invalid_role`
- Ban 是房間級的 shadow ban：被 ban 的人一樣回 `201`，但不廣播，`GET` 也會濾掉；解除後全部回來。不影響登入
- WS 訊息格式是 `{"type": ..., "payload": ...}`：
  - `message`：新彈幕，payload 跟 `POST /rooms/{public_id}/messages` 的回應相同
  - `user_hidden`：`{"user_id": ...}`，有人剛被 ban，client 移除他的彈幕。不發給本人，不保證送達
  - `error`：只送給那條連線，連線不會斷，`code` 有：
    - `ws_write_forbidden`：client 從 WS 送訊息（發彈幕要走 `POST /rooms/{public_id}/messages`）
    - `ws_auth_failed`：auth 失敗（token 無效、user 不存在），連線退回匿名
    - `ws_auth_expired`：token 到期，連線退回匿名
    - `ws_room_not_found`：`join` 的房間不存在或已刪除（payload 格式錯也是這個），重試沒用
    - `ws_join_unavailable`：server 暫時無法進房（DB / Redis 出問題或逾時），client 用 backoff + jitter 重試
    - `ws_already_in_room`：已經在房間裡又送 `join`（要先 `leave`）
    - `ws_not_in_room`：不在任何房間卻送 `leave`
  - `auth_ok`：auth 成功，payload `{"user": {"id", "username"}, "expires_in": 秒數}`
  - `join_ok` / `leave_ok`：進房 / 離房成功，payload `{"room": "<public_id>"}`
  - `pong`：回應 client 的 `ping`
- WS keepalive：client 每 25 秒送 `{"type": "ping"}`，server 回 `{"type": "pong"}`；server 也每 25 秒送 WebSocket protocol ping（瀏覽器自動回 pong），75 秒沒收到任何東西就斷線。目的是讓閒置連線不被 nginx `proxy_read_timeout`（60s）切掉，並偵測半開連線
- WS 進房：client 送 `{"type": "join", "payload": {"room": "<public_id>"}}`，收到 `join_ok` 之後才會收到該房間的 `message`。一條連線同時最多在一個房間；換房 = 先送 `{"type": "leave"}` 再 `join`。斷線時 server 自動離房。`join_ok` 之前發出的彈幕不會補送（Redis Pub/Sub 不存訊息），client 收到 `join_ok` 後要再抓一次歷史訊息（server 等 Redis 確認訂閱好了才回 `join_ok`，所以兩者中間不會有漏掉的訊息）
- WS 登入：client 送 `{"type": "auth", "payload": {"token": "<access token>"}}`。快到期時在同一條連線再送一次新的 token 續期；送別人的 token 會直接換身分
- WS 限流：每條連線每秒最多 5 個 frame（`auth` / `join` / `leave` / `ping` 都算），超過的**直接丟掉、不回應**（回應本身也要佔 server 資源，所以刻意不回）。client 不能假設送出的 frame 一定有回應：送 `join` 後要自己設 timeout（前端是 5 秒），逾時就用 backoff 重送；重送後收到 `ws_already_in_room` 代表第一次的 `join` 其實成功了

## Request ID & Log 追蹤

每個 request 都有 request ID：client 帶合法的 `X-Request-ID`（`[A-Za-z0-9_-]{1,64}`）就沿用，否則由 server 產生，並回傳在 response header。同一個 request 的所有 log 都帶相同的 `request_id`；WebSocket 連線沿用 upgrade request 的 ID，所以整條連線的 log 可以用同一個 ID 串起來。

```bash
curl -si -H 'X-Request-ID: test-123' localhost:5388/healthcheck   # response header 會帶 X-Request-ID
docker logs danmaku-server-1 2>&1 | grep '"request_id":"test-123"'
```

經過 nginx（`localhost:5390`）時，nginx 不驗證 ID（規則只寫在 server，避免兩邊不同步）：client 有帶就原樣轉給 server，沒帶才補上 nginx 的 `$request_id`。access log 記的是 server 回傳的 `X-Request-ID`，也就是 server 驗證後最後採用的值，所以 nginx 和 server 的 log 一定一致：

```bash
curl -si -H 'X-Request-ID: test-456' localhost:5390/healthcheck
docker logs danmaku-nginx 2>&1 | grep 'request_id=test-456'          # 也會顯示轉發到哪台 upstream
docker compose logs server-1 server-2 | grep '"request_id":"test-456"'
```

## CORS

CORS middleware 依 `CORS_ORIGINS` 決定哪些網站的前端可以讀取 API response（只影響瀏覽器，curl / mobile app 不受影響）：

- `*`（預設）：所有網站都允許
- allowlist：Origin 在清單內才回寫 `Access-Control-Allow-Origin`，並加上 `Vary: Origin`
- Preflight（`OPTIONS` + `Origin` + `Access-Control-Request-Method`）由 middleware 直接回 `204`，不進 handler
- 允許的實際 response 會加 `Access-Control-Expose-Headers: Retry-After`：瀏覽器預設只讓 JS 讀少數幾個 response header，前端要讀 `429` 的 `Retry-After` 必須 expose（DevTools / curl 一律看得到，不受影響）
- 設定格式錯誤（含 path、缺 scheme、`*` 與其他 origin 混用）時 server 啟動失敗，log 會指出是哪一項
- 比對是字串完全相等，所以 origin 必須跟瀏覽器送的寫法一樣（全小寫、不寫預設 port `:443` / `:80`、沒有 query / fragment / 帳號），否則啟動失敗並提示正確寫法

GitHub Pages 前端的 origin 是 `https://jay41chen.github.io`（不含 `/danmaku/` 路徑）。

```bash
curl -si -X OPTIONS localhost:5388/rooms/legacy00000/messages \
  -H 'Origin: https://a.com' -H 'Access-Control-Request-Method: POST'   # 204 + Access-Control-Allow-* headers
```

## Database Migration

Schema 由 [golang-migrate](https://github.com/golang-migrate/migrate) 管理。server 啟動時，在建立 DB 連線池之前先執行 `m.Up()`，把還沒跑過的 migration 依序套用；已經是最新版本時直接略過。

- SQL 檔在 `server/migrations/`，compile 時 embed 進 binary，部署只需要執行檔
- 檔名格式 `{version}_{title}.{up|down}.sql`：`up` 套用變更，`down` 撤銷變更
- DB 目前的版本記在 `schema_migrations` 表（`version`、`dirty`）

| Version | 內容 |
|---|---|
| `000001_create_messages` | 導入 migration 前手動建的 schema（baseline）。用 `IF NOT EXISTS`，已有資料表的 DB 只會記成 version 1 |
| `000002_messages_created_at_timestamptz` | `created_at` 改為 `TIMESTAMPTZ NOT NULL`（舊值當成 UTC） |
| `000003_messages_id_identity` | `id` 從 `SERIAL` 改為 `BIGINT GENERATED ALWAYS AS IDENTITY`，編號接續原本的最大值 |
| `000004_create_users` | `users` 表；`lower(username)` unique index（不分大小寫唯一） |
| `000005_create_refresh_tokens` | `refresh_tokens` 表（存 SHA-256 hash、`expires_at`、`revoked_at`），`user_id` FK + index |
| `000006_add_refresh_token_revoked_reason` | 加 `revoked_reason`，並 CHECK 它跟 `revoked_at` 要同時有值或同時是 NULL |
| `000007_rename_revoked_reason_reuse` | `revoked_reason` 的值 `reuse` 改名為 `reuse_detected` |
| `000008_rename_revoked_reason_duplicate_use` | `reuse_detected` 改名為 `duplicate_use_detected`（目前的值：`rotated` / `logout` / `duplicate_use_detected`） |
| `000009_messages_add_user_id` | `messages` 加 `user_id`（nullable FK → `users.id`，`ON DELETE SET NULL`，訊息保留）+ index |
| `000010_create_rooms` | `rooms` 表（`public_id` unique、soft delete）；種下系統帳號 `admin`（不能登入）和 `legacy00000` 房間（id 1）；`messages` 加 `room_id`（`DEFAULT 1`，由 000011 拿掉）+ index |
| `000011_messages_room_id_drop_default` | 拿掉 `messages.room_id` 的 `DEFAULT 1`（expand / contract 的 contract 步驟），INSERT 一定要帶 `room_id`。上線後若要把後端退回 `2c17ac8` 之前的版本（INSERT 不帶 `room_id`），要先手動跑 000011 的 down |
| `000012_create_user_credential_state` | `user_credential_state` 表（`user_id` PK/FK，`valid_after`）：refresh token 「撤銷全部」改成只寫 `valid_after`，`created_at` 在這之前（含）的 token 一律無效，不再 UPDATE `refresh_tokens`。並依既有的 `duplicate_use_detected` 資料 backfill `valid_after`（每個 user 取最晚的 `revoked_at` / `created_at`），舊 token 部署後再出現不會再觸發撤銷。只新增表，部署一次就好 |
| `000013_rooms_created_at_public_id_idx` | `rooms(created_at, public_id) WHERE deleted_at IS NULL` 的 partial index，給房間列表的 cursor 分頁用（`ORDER BY created_at DESC, public_id DESC`，`public_id` 當 tiebreaker） |
| `000014_create_room_roles` | `room_roles` 表（`room_id` + `user_id` PK，`role` 是 `moderator` / `banned`，`assigned_by`）：房管和房間級 ban。房主看 `rooms.owner_id` |

### 新增 migration

1. 在 `server/migrations/` 建立下一號的 `.up.sql` 和 `.down.sql`
2. 整份 SQL 用 `BEGIN;` / `COMMIT;` 包起來，失敗時整個 rollback
   - pgx driver 把整份檔案用一次 `Exec` 送出，Postgres 會自動當成一個 transaction（implicit transaction），所以 `000004`～`000009` 沒包也是 atomic；仍然明確寫出來，避免日後開 `x-multi-statement`（改成逐句送）或換 driver 時失去保護。已套用的不回頭補
   - 例外：`CREATE INDEX CONCURRENTLY` 不能放在 transaction 裡，要單獨一份 migration、不包 `BEGIN` / `COMMIT`
   - 不管有沒有 rollback，失敗後版本都會被標成 `dirty`，要手動處理（見下方「查詢版本 / 手動退版」）
3. 重新 build / 重啟 server 即套用（SQL 是 embed 的，只改 SQL 不重新 build 不會生效）

注意：

- **已經套用（部署）過的 migration 不能修改**：DB 記錄該版本已執行，改了也不會重跑。要修正請新增下一號
- **不要留空的 migration 檔**：空檔一樣會被執行並記錄版本，之後補上的內容永遠不會跑
- 寫完先在臨時 DB 測 up → down → up，並確認結果（例如下一筆的 `id`），不要只看有沒有報錯

### 查詢版本 / 手動退版

目前沒有引入 migrate CLI，down 用 psql 手動執行（以 local DB 為例）：

```bash
# 查目前版本
docker exec -i danmaku-db psql -U danmaku -c "SELECT * FROM schema_migrations"

# 退回一版：執行該版的 down，再把版本改成前一版
docker exec -i danmaku-db psql -U danmaku -v ON_ERROR_STOP=1 < server/migrations/000003_messages_id_identity.down.sql
docker exec -i danmaku-db psql -U danmaku -c "UPDATE schema_migrations SET version = 2"
```

`000001` 的 down 會 `DROP TABLE`，刪除所有訊息，只在本機重建 DB 時使用。

如果 migration 執行到一半失敗，`dirty` 會變成 `true`，之後每次啟動都會報錯。先確認 DB 實際停在哪個狀態並手動修好，再把版本設成「確實已完成」的那一版：

```sql
UPDATE schema_migrations SET version = <已完成的版本>, dirty = false;
```

## Local Dev vs Production

| | Local (Docker Compose) | Production (Railway) |
|---|---|---|
| Server | Go x2 + Nginx LB | Go x1（replicas 可調，見下方 Deployment） |
| Database | PostgreSQL container | Railway PostgreSQL |
| Cache | Redis container | Railway Redis |
| Logs | Promtail → Loki → Grafana | Railway built-in logs |
| Metrics | Prometheus + Grafana | — |

Observability stack（Loki、Promtail、Grafana、Prometheus）只存在於 local dev 環境，不部署到 production。

## Deployment

| | 平台 | 觸發方式 |
|---|---|---|
| Frontend | GitHub Pages | push 到 `dev` 且有改到 `client/**`（`.github/workflows/deploy-frontend.yml`），也可在 Actions 手動跑 `workflow_dispatch`；`VITE_API_URL` / `VITE_WS_URL` 放在 GitHub Secrets |
| Backend | Railway（`danmaku` service + PostgreSQL + Redis） | push 到 `dev` 由 Railway 自動 build `server/Dockerfile` 並部署；migration 在 server 啟動時自動套用。目前停用（試用期已過）；image 入口是 `/server`，重新啟用時若有自訂 start command 要跟著改 |
| Server CI | GitHub Actions | push 到 `dev` 且有改到 `server/**`（`.github/workflows/server-ci.yml`）：測試（Postgres、Redis 用 service container，`-count=1` 不吃測試快取）+ `docker build`，不擋部署 |

Railway 上的 env（`DATABASE_URL`、`REDIS_URL`、`JWT_SECRET`、`CORS_ORIGINS`、`CLIENT_IP_HEADER=X-Real-IP`）都在 dashboard 設定，**不進 repo**。`JWT_SECRET` 沒設時新版會 fail fast 起不來，部署前先確認。

### 開關 replicas

Railway dashboard → `danmaku` service → **Settings** → **Replicas**（Scale 區塊）調整台數 → 按上方出現的 **Deploy** 套用變更。

- 平常維持 **1** 台（省額度）。
- 要驗證跨 instance 行為時開 **2** 台。server 本身沒有不能重建的狀態（JWT 各台用同一把 `JWT_SECRET` 驗、refresh token 在 PostgreSQL、限流計數在 Redis、彈幕透過 Redis Pub/Sub 廣播），所以**不需要 sticky session**。
- 看某個 request 落在哪一台：Railway **HTTP Logs** 的 `upstreamAddress`（replica 的內網 IPv6）。WebSocket 看 `/ws` 那筆 101 handshake。
- 2026-10-29 已用 replicas = 2 驗證：落在不同台的 A（登入）發彈幕、B（訪客）收得到。
- 已知限制：多台時從外部打 `/metrics` 只會落在其中一台（見 ROADMAP W6）。

### 換部署平台（backend）

server 只依賴 Dockerfile、env、Postgres / Redis 標準協定，沒有平台專屬設定檔，換平台照下面清單做即可。前端固定在 GitHub Pages，只需要改它指向的 API 位址。

1. **建 service**：從 `server/Dockerfile` build（build context 是 `server/`），image 入口 `/server`、會讀平台給的 `PORT`。平台必須支援 WebSocket；health check 設 `/healthcheck`。
2. **Postgres**：開新的空 DB 即可，server 啟動時會自動套 migration。要保留資料時用 `pg_dump --no-owner --no-acl` 匯出再匯入。託管服務常要求 TLS，`DATABASE_URL` 加 `sslmode=require`；`pool_max_conns` 在新主機上重新決定（[ADR 0021](docs/design/0021-resync-get-spike-and-pool-size.md)）。
3. **Redis**：**不用搬資料**（只放 Pub/Sub 和限流計數，都是暫時的），開一個新的即可。
4. **env**：照下方 [Environment Variables](#environment-variables) 填 `DATABASE_URL`、`REDIS_URL`、`JWT_SECRET`、`CORS_ORIGINS`、`CLIENT_IP_HEADER`。`JWT_SECRET` 沿用舊值，已登入的使用者就不會被登出；換新值也只是大家要重新登入。
5. **`CLIENT_IP_HEADER`（最容易錯，而且設錯不會報錯）**：查新平台的 proxy 會**覆蓋**哪個 header，而且值要是**單一 IP**。目前程式只接受單一 IP；值是 `X-Forwarded-For` 那種逗號分隔的清單時會解析失敗、退回 RemoteAddr，所有人共用同一個限流計數；設錯時 log 會出現一筆 `client ip header unusable` 開頭的 warn（每個 process 一次），`/metrics` 的 `client_ip_fallback_total` 也會增加。
6. **前端**：改 GitHub Secrets 的 `VITE_API_URL`、`VITE_WS_URL`（`wss://`），在 Actions 手動跑 Deploy Frontend（`workflow_dispatch`）。新的 API 位址要在 `CORS_ORIGINS` 允許的前端 origin 下測。
7. **驗證**：
   - `curl https://<新位址>/healthcheck` 回 200。
   - 前端登入、發彈幕，另一個瀏覽器（訪客）收得到。
   - 限流讀到真實 IP：用同一個網路連續註冊直到 429，再換另一個網路（如手機熱點）註冊，應該不受影響。
   - 不能偽造：帶 `-H "<CLIENT_IP_HEADER>: 203.0.113.1"` 連打註冊，每次換不同的假 IP；如果一直不會 429，代表平台沒有覆蓋這個 header，client 可以偽造。
8. **收尾**：驗證通過後再關掉舊平台的 service 和 DB；更新本文件的 [Deployment](#deployment) 表和 [Local Dev vs Production](#local-dev-vs-production)。

## Environment Variables

Docker Compose 模式下由 `docker-compose.yml` 的 `environment` 覆蓋，以下為 Local Development 的預設值。

| Variable | Description | Local Default |
|---|---|---|
| `REDIS_URL` | Redis 連線 URL（`redis://` 或 `rediss://`）；密碼有特殊字元時要 percent-encode（如 `@` → `%40`） | `redis://localhost:2907` |
| `DATABASE_URL` | PostgreSQL 連線字串（URL 或 `host=... user=...` 格式）；URL 的密碼有特殊字元時要 percent-encode（如 `@` → `%40`、`#` → `%23`）。連線池上限用 `pool_max_conns=N` 加在 URL 上；沒設時是 pgx 預設的 max(4, CPU 數)。上線時從 CPU 數 × 2 開始，在目標主機上實測（[ADR 0021](docs/design/0021-resync-get-spike-and-pool-size.md)） | `postgres://danmaku:danmaku@localhost:3192/danmaku` |
| `PORT` | Server 監聽 port | `8080` |
| `CLIENT_IP_HEADER` | 限流用的 client IP 從哪個 header 讀（如 `X-Real-IP`）；只能設成前面那層 proxy 會**覆蓋**的 header，否則 client 可以偽造。沒設時用連線的 RemoteAddr，在 proxy 後面會變成所有人共用同一個計數。有設但 request 沒帶或值不是單一 IP 時也會退回 RemoteAddr，此時 metric `client_ip_fallback_total{reason="missing"\|"unparsable"}` 會增加，每個 process 第一次發生時記一筆 warn log | 空（用 RemoteAddr） |
| `JWT_SECRET` | 簽 access token（JWT，HS256）用的 secret，**至少 32 bytes**，所有 server instance 要用同一把；沒設或太短時拒絕啟動。產生方式：`openssl rand -base64 48` | **必填**，無預設值 |
| `CORS_ORIGINS` | 允許的 CORS origin，逗號分隔（如 `https://a.com,http://localhost:5173`）；只含 scheme + host + port，不含 path 和結尾 `/`；全小寫、不寫預設 port；不可與 `*` 混用 | `*` |
| `WS_MAX_CLIENTS` | 每台 server 的 WS 連線上限，1 到 1,000,000 的整數；壓測時才需要調高 | `100` |
| `LOADTEST_TIMING_LOG` | 設成 true 時，每 5 秒輸出一行 `loadtest_timing`（DB 連線池、`CreateMessage`、限流、`PUBLISH` 的耗時）。**只給壓測用**；關閉時沒有額外的 wrapper 和 goroutine | `false` |
| `VITE_API_URL` | 前端 API 位址 | `http://localhost:8080` |
| `VITE_WS_URL` | 前端 WebSocket 位址 | `ws://localhost:8080/ws` |
