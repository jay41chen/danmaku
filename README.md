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
- **Observability (local dev)**: Promtail → Loki → Grafana

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

啟動後包含：PostgreSQL、Redis、兩個 Go server、Nginx load balancer、Loki + Promtail + Grafana（log aggregation）。

| Service | URL |
|---|---|
| Nginx 入口 | `http://localhost:5390` |
| Grafana | `http://localhost:5392`（admin / admin） |

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

## Local Dev vs Production

| | Local (Docker Compose) | Production (Railway) |
|---|---|---|
| Server | Go x2 + Nginx LB | Go x1 |
| Database | PostgreSQL container | Railway PostgreSQL |
| Cache | Redis container | Railway Redis |
| Logs | Promtail → Loki → Grafana | Railway built-in logs |
| Metrics | Prometheus + Grafana（planned） | — |

Observability stack（Loki、Promtail、Grafana、Prometheus）只存在於 local dev 環境，不部署到 production。

## Environment Variables

Docker Compose 模式下由 `docker-compose.yml` 的 `environment` 覆蓋，以下為 Local Development 的預設值。

| Variable | Description | Local Default |
|---|---|---|
| `REDIS_URL` | Redis 連線地址 | `localhost:2907` |
| `DATABASE_URL` | PostgreSQL 連線字串 | `postgres://danmaku:danmaku@localhost:3192/danmaku` |
| `PORT` | Server 監聽 port | `8080` |
| `CORS_ORIGIN` | 允許的 CORS origin | `*` |
| `VITE_API_URL` | 前端 API 位址 | `http://localhost:8080` |
| `VITE_WS_URL` | 前端 WebSocket 位址 | `ws://localhost:8080/ws` |
