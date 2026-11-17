#!/bin/sh
# 拆掉 fault 環境：只動 danmaku-fault 這個 compose project（含它的 volume）
set -eu
cd "$(dirname "$0")"
docker compose -p danmaku-fault down -v
