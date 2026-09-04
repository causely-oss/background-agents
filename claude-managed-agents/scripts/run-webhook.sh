#!/usr/bin/env bash
# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
#
# Runs webhook.py locally, then opens a cloudflared quick tunnel to it so
# Causely (a public SaaS) can reach it. Same pattern as run-k8s-mcp.sh, just
# in the opposite direction: there the cloud agent needed to reach a local
# MCP server; here an external service needs to reach this local receiver.
#
# The URL is random and rotates every time this script restarts — re-paste
# it into Causely's notification config (Settings -> Notifications) after
# each restart, same caveat as K8S_MCP_URL.
set -euo pipefail

PORT="${WEBHOOK_PORT:-8082}"

command -v cloudflared >/dev/null || {
  echo "cloudflared not found — https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/" >&2
  exit 1
}

echo "Starting webhook.py on :$PORT"
uvicorn webhook:app --host 0.0.0.0 --port "$PORT" &
WEBHOOK_PID=$!

TUNNEL_LOG="$(mktemp)"
cleanup() {
  kill "$WEBHOOK_PID" "${TUNNEL_PID:-}" 2>/dev/null || true
  rm -f "$TUNNEL_LOG"
}
trap cleanup EXIT

sleep 2

echo "Starting cloudflared quick tunnel -> http://localhost:$PORT"
cloudflared tunnel --url "http://localhost:$PORT" >"$TUNNEL_LOG" 2>&1 &
TUNNEL_PID=$!

echo "Waiting for the tunnel URL..."
URL=""
for _ in $(seq 1 30); do
  URL="$(grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' "$TUNNEL_LOG" | head -n1 || true)"
  [ -n "$URL" ] && break
  sleep 1
done

if [ -z "$URL" ]; then
  echo "Could not find the tunnel URL — check $TUNNEL_LOG" >&2
else
  echo
  echo "Causely webhook URL: ${URL}/webhook/causely"
  echo "Paste that into Causely: Settings -> Notifications -> Generic,"
  echo "with Authorization token = CAUSELY_WEBHOOK_SECRET from .env."
  echo "(this URL rotates every time you restart this script)"
fi

wait
