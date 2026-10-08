# Danmaku

即時彈幕系統，支援 WebSocket 即時廣播與訊息持久化。

練習用的 full-stack side project，目的是熟悉後端開發與部署流程。

## Tech Stack

- **Backend**: Go + gorilla/websocket + pgx
- **Frontend**: React + TypeScript + Vite
- **Database**: PostgreSQL 17
- **Cache**: Redis 7 (Pub/Sub)

## Getting Started

### Prerequisites

- Go 1.27+
- Node.js 18+
- Docker

### Setup

1. 啟動 Postgres 和 Redis：

```bash
docker compose up -d
```

2. 建立資料表：

```bash
docker exec -it danmaku-postgres psql -U danmaku
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

5. 開啟 http://localhost:5173

## Environment Variables

| Variable | Description | Default |
|---|---|---|
| `REDIS_URL` | Redis 連線地址 | `localhost:2907` |
| `DATABASE_URL` | PostgreSQL 連線字串 | `postgres://danmaku:danmaku@localhost:3192/danmaku` |
| `PORT` | Server 監聽 port | `8080` |
| `CORS_ORIGIN` | 允許的 CORS origin | `*` |
| `VITE_API_URL` | 前端 API 位址 | `http://localhost:8080` |
| `VITE_WS_URL` | 前端 WebSocket 位址 | `ws://localhost:8080/ws` |
