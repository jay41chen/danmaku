#!/bin/sh
# 跑 `loadtest -scenario redis`，中途把 fault 環境的 Redis stop 或 pause 一段時間，再恢復。
# 用法：redis.sh <stop|pause> <outage_seconds> [額外的 loadtest 參數，例如 -duration 180s -redis-viewers 10]
# 流程：背景啟動 loadtest（跑在 danmaku-fault 網路裡）→ 等 30s → 記 T_down → stop/pause redis → 等 outage 秒 → start/unpause → 記 T_up → 等 loadtest 結束。
# 注意：stop + start 會丟掉 Redis 資料（tmpfs、沒開持久化），這是刻意的；pause 則保留資料、連線只是被凍住。
# 環境變數：OUT_DIR（輸出目錄，預設 deploy/fault/out/）、OUT_NAME（檔名，預設 redis-<mode>-<秒數>-<時間>.json）
# 注意：loadtest 的 -duration 預設 120s，必須大於 30 + outage + 恢復後想觀察的時間
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <stop|pause> <outage_seconds> [extra loadtest flags]" >&2
  exit 2
fi
mode="$1"
outage="$2"
shift 2
case "$mode" in stop|pause) ;; *) echo "redis.sh: mode must be stop or pause, got '$mode'" >&2; exit 2 ;; esac
case "$outage" in ''|*[!0-9]*) echo "redis.sh: outage_seconds must be a non-negative integer, got '$outage'" >&2; exit 2 ;; esac

out_dir="${OUT_DIR:-$here/out}"
out_name="${OUT_NAME:-redis-$mode-$outage-$(date +%Y%m%d-%H%M%S).json}"
mkdir -p "$here/.bin" "$out_dir"

(cd "$here/../../server" && GOOS=linux CGO_ENABLED=0 go build -o "$here/.bin/loadtest" ./cmd/loadtest)

servers_arg="http://server-1:8080,http://server-2:8080"
targets="server-1 server-2"

# 不論怎麼結束（正常、失敗、Ctrl-C）都把 Redis 救回來；只處理 danmaku-fault 的 redis
down=0
lt_pid=""
restore() {
  if [ "$down" = "1" ]; then
    if [ "$mode" = "stop" ]; then
      docker compose -p danmaku-fault start redis >/dev/null 2>&1 || echo "redis.sh: WARNING: failed to start redis" >&2
    else
      docker compose -p danmaku-fault unpause redis >/dev/null 2>&1 || echo "redis.sh: WARNING: failed to unpause redis" >&2
    fi
    down=0
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
  /bin-loadtest/loadtest -in-compose -scenario redis -servers "$servers_arg" \
  -require-machine-check=false -docker-containers "" -out "/out/$out_name" "$@" &
lt_pid=$!

sleep 30
t_down="n/a"
t_up="n/a"
if kill -0 "$lt_pid" 2>/dev/null; then
  t_down="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  down=1
  docker compose -p danmaku-fault "$mode" redis
  sleep "$outage"
  if [ "$mode" = "stop" ]; then
    docker compose -p danmaku-fault start redis
  else
    docker compose -p danmaku-fault unpause redis
  fi
  down=0
  t_up="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
else
  echo "redis.sh: loadtest exited before the outage, skipping" >&2
fi

rc=0
wait "$lt_pid" || rc=$?
lt_pid=""

printf 'run start: %s\nT_down (redis %s): %s\nT_up: %s\nloadtest exit code: %s\n' "$start_ts" "$mode" "$t_down" "$t_up" "$rc"

# 各種 log 行數（不是事件數：一個事件可能印多行）；logs 失敗就印 n/a；loadtest 失敗時也照印
for s in $targets; do
  if logs="$(docker compose -p danmaku-fault logs --since "$start_ts" "$s" 2>/dev/null)"; then
    for pat in "publish to redis failed" "rate limiter failed" "pubsub judged deaf, rebuilding" "publish heartbeat failed" "subscribe room failed, will retry" "unsubscribe room failed" "create message failed"; do
      n="$(printf '%s\n' "$logs" | grep -c "$pat" || true)"
      printf 'log lines "%s" (%s, since %s): %s\n' "$pat" "$s" "$start_ts" "$n"
    done
  else
    printf 'log lines (%s, since %s): n/a (logs failed)\n' "$s" "$start_ts"
  fi
done
exit "$rc"
