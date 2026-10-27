# Danmaku

即時彈幕系統，支援 WebSocket 即時廣播與訊息持久化。

練習用的 full-stack side project，目的是熟悉後端開發與部署流程。

## Tech Stack

- **Backend**: Go + gorilla/websocket + pgx
- **Frontend**: React + TypeScript + Vite
- **Database**: PostgreSQL 17
- **Cache**: Redis 7 (Pub/Sub)
- **Reverse Proxy**: Nginx 1.27
- **Infrastructure**: Docker Compose
- **Observability (local dev)**: Promtail → Loki → Grafana, Prometheus → Grafana
- **Middleware**: Request ID → Request logging → Metrics → Panic recovery → CORS（`net/http` + 自製 chain）
- **Logging**: slog JSON，透過 context 自動帶 `request_id`
- **Reliability**: Panic recovery middleware, graceful shutdown (SIGTERM/SIGINT)
- **Schema Migration**: golang-migrate，server 啟動時自動套用，SQL embed 進 binary
- **Auth**: JWT access token（HS256，1 小時）+ opaque refresh token（rotation + duplicate use detection）；route 預設要登入，公開的要明寫 `publicGet` / `publicPost`

## Architecture

```
Client → Nginx (load balancer, least_conn)
           ├── Server-1 (Go :8080)
           └── Server-2 (Go :8080)
                ├── PostgreSQL (持久化)
                └── Redis (Pub/Sub 跨 instance 廣播)
```

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
docker run -d --name danmaku-db -p 3192:5432 -e POSTGRES_USER=danmaku -e POSTGRES_PASSWORD=danmaku -e POSTGRES_DB=danmaku postgres:17
docker run -d --name danmaku-redis -p 2907:6379 redis:7
```

2. 啟動 Go server（啟動時會自動建立 / 更新資料表，見 [Database Migration](#database-migration)）：

```bash
cd server
go run .
```

3. 啟動前端：

```bash
cd client
npm install
npm run dev
```

4. 開啟 http://localhost:5173/danmaku/

## API

| Method | Path | 要登入 | 說明 |
|---|---|---|---|
| `GET` | `/messages` | | 歷史訊息；發送者已刪除的訊息 `username` 是 `null` |
| `POST` | `/messages` | ✓ | 發彈幕 `{"content": "..."}`：最多 100 字、不可全空白；按 user 限流（3 則 / 1 秒 + 20 則 / 10 秒）。成功回 `201` + 訊息，並廣播給所有 WS 連線 |
| `GET` | `/ws` | | WebSocket，接收即時廣播。連上是匿名，可在連線內送 auth frame 登入（見下方） |
| `POST` | `/register`、`/login` | | 註冊 / 登入，`/login` 回 access token + refresh token |
| `POST` | `/refresh`、`/logout` | | 用 refresh token 換新的一組 / 作廢 |
| `GET` | `/healthcheck`、`/metrics` | | 健康檢查、Prometheus metrics |

- 要登入的 endpoint 帶 `Authorization: Bearer <access token>`。沒帶、token 無效或過期一律回同一個 `401`（`WWW-Authenticate: Bearer`），client 用 refresh token 換新的再重送
- WS 訊息格式是 `{"type": ..., "payload": ...}`：
  - `message`：新彈幕，payload 跟 `POST /messages` 的回應相同
  - `error`：只送給那條連線，連線不會斷，`code` 有：
    - `ws_write_forbidden`：client 從 WS 送訊息（發彈幕要走 `POST /messages`）
    - `ws_auth_failed`：auth 失敗（token 無效、user 不存在），連線退回匿名
    - `ws_auth_expired`：token 到期，連線退回匿名
  - `auth_ok`：auth 成功，payload `{"user": {"id", "username"}, "expires_in": 秒數}`
- WS 登入：client 送 `{"type": "auth", "payload": {"token": "<access token>"}}`。快到期時在同一條連線再送一次新的 token 續期；送別人的 token 會直接換身分

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
- 設定格式錯誤（含 path、缺 scheme、`*` 與其他 origin 混用）時 server 啟動失敗，log 會指出是哪一項
- 比對是字串完全相等，所以 origin 必須跟瀏覽器送的寫法一樣（全小寫、不寫預設 port `:443` / `:80`、沒有 query / fragment / 帳號），否則啟動失敗並提示正確寫法

GitHub Pages 前端的 origin 是 `https://jay41chen.github.io`（不含 `/danmaku/` 路徑）。

```bash
curl -si -X OPTIONS localhost:5388/messages \
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
| Server | Go x2 + Nginx LB | Go x1 |
| Database | PostgreSQL container | Railway PostgreSQL |
| Cache | Redis container | Railway Redis |
| Logs | Promtail → Loki → Grafana | Railway built-in logs |
| Metrics | Prometheus + Grafana | — |

Observability stack（Loki、Promtail、Grafana、Prometheus）只存在於 local dev 環境，不部署到 production。

## Environment Variables

Docker Compose 模式下由 `docker-compose.yml` 的 `environment` 覆蓋，以下為 Local Development 的預設值。

| Variable | Description | Local Default |
|---|---|---|
| `REDIS_URL` | Redis 連線 URL（`redis://` 或 `rediss://`）；密碼有特殊字元時要 percent-encode（如 `@` → `%40`） | `redis://localhost:2907` |
| `DATABASE_URL` | PostgreSQL 連線字串（URL 或 `host=... user=...` 格式）；URL 的密碼有特殊字元時要 percent-encode（如 `@` → `%40`、`#` → `%23`） | `postgres://danmaku:danmaku@localhost:3192/danmaku` |
| `PORT` | Server 監聽 port | `8080` |
| `CLIENT_IP_HEADER` | 限流用的 client IP 從哪個 header 讀（如 `X-Real-IP`）；只能設成前面那層 proxy 會**覆蓋**的 header，否則 client 可以偽造。沒設時用連線的 RemoteAddr，在 proxy 後面會變成所有人共用同一個計數 | 空（用 RemoteAddr） |
| `JWT_SECRET` | 簽 access token（JWT，HS256）用的 secret，**至少 32 bytes**，所有 server instance 要用同一把；沒設或太短時拒絕啟動。產生方式：`openssl rand -base64 48` | **必填**，無預設值 |
| `CORS_ORIGINS` | 允許的 CORS origin，逗號分隔（如 `https://a.com,http://localhost:5173`）；只含 scheme + host + port，不含 path 和結尾 `/`；全小寫、不寫預設 port；不可與 `*` 混用 | `*` |
| `VITE_API_URL` | 前端 API 位址 | `http://localhost:8080` |
| `VITE_WS_URL` | 前端 WebSocket 位址 | `ws://localhost:8080/ws` |
