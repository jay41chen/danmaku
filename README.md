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

2. 建立資料表（server 不會自動 migrate）：

```bash
docker exec -it danmaku-db psql -U danmaku
```

```sql
CREATE TABLE messages (
    id SERIAL PRIMARY KEY,
    content TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT now()
);
```

3. 啟動 Go server：

```bash
cd server
go run .
```

4. 啟動前端：

```bash
cd client
npm install
npm run dev
```

5. 開啟 http://localhost:5173/danmaku/

## Request ID & Log 追蹤

每個 request 都有 request ID：client 帶合法的 `X-Request-ID`（`[A-Za-z0-9_-]{1,64}`）就沿用，否則由 server 產生，並回傳在 response header。同一個 request 的所有 log 都帶相同的 `request_id`；WebSocket 連線沿用 upgrade request 的 ID，所以整條連線的 log 可以用同一個 ID 串起來。

```bash
curl -si -H 'X-Request-ID: test-123' localhost:5388/healthcheck   # response header 會帶 X-Request-ID
docker logs danmaku-server-1 2>&1 | grep '"request_id":"test-123"'
```

## CORS

CORS middleware 依 `CORS_ORIGINS` 決定哪些網站的前端可以讀取 API response（只影響瀏覽器，curl / mobile app 不受影響）：

- `*`（預設）：所有網站都允許
- allowlist：Origin 在清單內才回寫 `Access-Control-Allow-Origin`，並加上 `Vary: Origin`
- Preflight（`OPTIONS` + `Access-Control-Request-Method`）由 middleware 直接回 `204`，不進 handler
- 設定格式錯誤（含 path、缺 scheme、`*` 與其他 origin 混用）時 server 啟動失敗，log 會指出是哪一項

GitHub Pages 前端的 origin 是 `https://jay41chen.github.io`（不含 `/danmaku/` 路徑）。

```bash
curl -si -X OPTIONS localhost:5388/messages \
  -H 'Origin: https://a.com' -H 'Access-Control-Request-Method: POST'   # 204 + Access-Control-Allow-* headers
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
| `REDIS_URL` | Redis 連線地址 | `localhost:2907` |
| `DATABASE_URL` | PostgreSQL 連線字串 | `postgres://danmaku:danmaku@localhost:3192/danmaku` |
| `PORT` | Server 監聽 port | `8080` |
| `CORS_ORIGINS` | 允許的 CORS origin，逗號分隔（如 `https://a.com,http://localhost:5173`）；只含 scheme + host + port，不含 path 和結尾 `/`；不可與 `*` 混用 | `*` |
| `VITE_API_URL` | 前端 API 位址 | `http://localhost:8080` |
| `VITE_WS_URL` | 前端 WebSocket 位址 | `ws://localhost:8080/ws` |
