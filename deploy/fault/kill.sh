#!/bin/sh
# 跑 `loadtest -scenario kill`：觀眾與 poster 都經 nginx 進來，中途對 server-1 下 SIGKILL，觀察觀眾重連、補撈、presence 與 POST 結果。
# 用法：kill.sh [額外的 loadtest 參數，例如 -duration 180s -kill-viewers 20]
# 流程：背景啟動 loadtest（跑在 danmaku-fault 網路裡）→ 等 30s → 記 T_kill → kill -s SIGKILL server-1 → 等 loadtest 結束 → 不論如何都把 server-1 start 回來（還原時也會重啟 nginx）。
# 實驗期間不重啟 server-1（loadtest 結束後才 start，只是還原環境）。
# 環境變數：OUT_DIR（輸出目錄，預設 deploy/fault/out/）、OUT_NAME（檔名，預設 kill-<時間>.json）
# 注意：loadtest 的 -duration 預設 120s，必須大於 30 + 想觀察的恢復時間；觀眾退避最久 32s，presence stale 成員最久 30s 才剔除
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

out_dir="${OUT_DIR:-$here/out}"
out_name="${OUT_NAME:-kill-$(date +%Y%m%d-%H%M%S).json}"
mkdir -p "$here/.bin" "$out_dir"

(cd "$here/../../server" && GOOS=linux CGO_ENABLED=0 go build -o "$here/.bin/loadtest" ./cmd/loadtest)

# -servers 只用來 seed 與取樣各台的 /metrics（直連）；觀眾與 poster 走 loadtest 預設的 -kill-entry（http://nginx:80）
servers_arg="http://server-1:8080,http://server-2:8080"

# 不論怎麼結束（正常、失敗、Ctrl-C）都把 server-1 救回來；只處理 danmaku-fault 的 server-1
killed=0
lt_pid=""
restore() {
  if [ "$killed" = "1" ]; then
    docker compose -p danmaku-fault start server-1 >/dev/null 2>&1 || echo "kill.sh: WARNING: failed to start server-1" >&2
    # nginx resolved server-1's old IP at startup and may hold a stale upstream address; restart it so it re-resolves
    docker compose -p danmaku-fault restart nginx >/dev/null 2>&1 || echo "kill.sh: WARNING: failed to restart nginx" >&2
    killed=0
  fi
}
on_exit() {
  rc=$?
  trap - EXIT INT TERM
  if [ -n "$lt_pid" ] && kill -0 "$lt_pid" 2>/dev/null; then kill "$lt_pid" 2>/dev/null || true; fi
  restore
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT TERM

start_ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# 容器內用目前使用者的 uid:gid 寫輸出（distroless 預設是 nonroot 65532，寫不進掛載目錄）
docker run --rm --user "$(id -u):$(id -g)" --network danmaku-fault_default --env-file "$here/.env" \
  -e TEST_DATABASE_URL='postgres://fault:fault@postgres:5432/danmaku_test?sslmode=disable' \
  -e TEST_REDIS_URL='redis://redis:6379/15' \
  -v "$here/.bin:/bin-loadtest:ro" -v "$out_dir:/out" \
  gcr.io/distroless/static-debian12:nonroot \
  /bin-loadtest/loadtest -in-compose -scenario kill -servers "$servers_arg" \
  -require-machine-check=false -docker-containers "" -out "/out/$out_name" "$@" &
lt_pid=$!

sleep 30
t_kill="n/a"
if kill -0 "$lt_pid" 2>/dev/null; then
  t_kill="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  killed=1
  docker compose -p danmaku-fault kill -s SIGKILL server-1
else
  echo "kill.sh: loadtest exited before the kill, skipping" >&2
fi

rc=0
wait "$lt_pid" || rc=$?
lt_pid=""

printf 'run start: %s\nT_kill (SIGKILL server-1): %s\nloadtest exit code: %s\n' "$start_ts" "$t_kill" "$rc"

# 各種 log 行數（不是事件數：一個事件可能印多行）；logs 失敗就印 n/a；loadtest 失敗時也照印
# server-2：redis.sh 用的那組，加上 WS join / leave / presence 相關的 log
if logs="$(docker compose -p danmaku-fault logs --since "$start_ts" server-2 2>/dev/null)"; then
  for pat in "publish to redis failed" "rate limiter failed" "pubsub judged deaf, rebuilding" "publish heartbeat failed" "subscribe room failed, will retry" "unsubscribe room failed" "create message failed" \
    "new client connected" "client disconnected" "ws upgrade failed" "ws join room failed" "ws join get room failed" "ws auth get user failed" "presence sync failed" "presence count failed" "too many connections"; do
    n="$(printf '%s\n' "$logs" | grep -c "$pat" || true)"
    printf 'log lines "%s" (server-2, since %s): %s\n' "$pat" "$start_ts" "$n"
  done
else
  printf 'log lines (server-2, since %s): n/a (logs failed)\n' "$start_ts"
fi

# nginx：官方 image 的 /var/log/nginx/access.log 與 error.log 是指到 stdout / stderr 的 symlink，所以 docker compose logs 看得到
# （config/nginx.conf 的 access_log 寫 /var/log/nginx/access.log、沒設 error_log，用預設路徑）。
# access log 格式是 `... "METHOD path HTTP/1.1" <status> ...`，所以用 `" 502 ` 比對 status
if logs="$(docker compose -p danmaku-fault logs --since "$start_ts" nginx 2>/dev/null)"; then
  for pat in "connect() failed" "upstream prematurely closed" "no live upstreams"; do
    n="$(printf '%s\n' "$logs" | grep -c "$pat" || true)"
    printf 'log lines "%s" (nginx, since %s): %s\n' "$pat" "$start_ts" "$n"
  done
  n="$(printf '%s\n' "$logs" | grep -c '" 502 ' || true)"
  printf 'access log lines with status 502 (nginx, since %s): %s\n' "$start_ts" "$n"
else
  printf 'log lines (nginx, since %s): n/a (logs failed)\n' "$start_ts"
fi
exit "$rc"
