#!/bin/sh
# 啟動 fault 環境（compose project: danmaku-fault）。JWT secret 第一次自動產生，存在 .env（已被 gitignore），不印出
set -eu
cd "$(dirname "$0")"

if [ ! -f .env ]; then
  # umask 077：檔案一建立就是 600，沒有 644 的空窗
  (
    umask 077
    secret="$(openssl rand -base64 48)"
    if [ -z "$secret" ]; then
      echo "up.sh: openssl produced an empty JWT secret; aborting" >&2
      exit 1
    fi
    printf 'JWT_SECRET=%s\n' "$secret" > .env
  ) || { rm -f .env; echo "up.sh: failed to generate .env" >&2; exit 1; }
fi

docker compose -p danmaku-fault up -d --build --wait

# --wait 只代表 container healthy；再等 server-1 真的回 200
i=0
while [ "$i" -lt 30 ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' http://127.0.0.1:5488/healthcheck || true)"
  if [ "$code" = "200" ]; then
    echo "fault environment ready (server-1 healthcheck 200)"
    exit 0
  fi
  i=$((i + 1))
  sleep 1
done
echo "up.sh: server-1 /healthcheck did not return 200 within 30s (last: ${code:-none})" >&2
exit 1
