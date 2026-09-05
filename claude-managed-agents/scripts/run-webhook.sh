#!/usr/bin/env bash
# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
#
# Runs webhook.py locally, then opens a cloudflared tunnel to it so Causely
# (a public SaaS) can reach it. Same pattern as run-k8s-mcp.sh, just in the
# opposite direction: there the cloud agent needed to reach a local MCP
# server; here an external service needs to reach this local receiver.
#
# Two modes, picked by whether WEBHOOK_TUNNEL_NAME is set:
#
#   Default (unset) — a cloudflared *quick* tunnel. Zero setup, but the URL
#   is random and rotates every restart — re-paste it into Causely's
#   notification config (Settings -> Notifications) each time, same caveat
#   as K8S_MCP_URL.
#
#   WEBHOOK_TUNNEL_NAME=<name> — a cloudflared *named* tunnel. The hostname
#   stays fixed across restarts, but it requires a one-time setup against
#   your own Cloudflare account + domain first — see the "Durable webhook
#   URL" section in docs/causely-webhook.md. Also set WEBHOOK_TUNNEL_HOSTNAME
#   to whatever hostname you routed to the tunnel, so this script can print
#   the full URL for you.
set -euo pipefail

PORT="${WEBHOOK_PORT:-8082}"
TUNNEL_NAME="${WEBHOOK_TUNNEL_NAME:-}"
TUNNEL_HOSTNAME="${WEBHOOK_TUNNEL_HOSTNAME:-}"

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

if [ -n "$TUNNEL_NAME" ]; then
  # Named tunnel — requires "cloudflared tunnel create $TUNNEL_NAME" and
  # "cloudflared tunnel route dns $TUNNEL_NAME <hostname>" to have already
  # been run once (see docs/causely-webhook.md). --url here works without a
  # config.yml/ingress rules since there's only the one local service.
  echo "Starting named cloudflared tunnel '$TUNNEL_NAME' -> http://localhost:$PORT"
  cloudflared tunnel run --url "http://localhost:$PORT" "$TUNNEL_NAME" >"$TUNNEL_LOG" 2>&1 &
  TUNNEL_PID=$!

  sleep 3
  if ! kill -0 "$TUNNEL_PID" 2>/dev/null; then
    echo "cloudflared exited immediately — check $TUNNEL_LOG" >&2
    echo "(has '$TUNNEL_NAME' been created and DNS-routed yet? see docs/causely-webhook.md)" >&2
    cat "$TUNNEL_LOG" >&2
    exit 1
  fi

  echo
  if [ -n "$TUNNEL_HOSTNAME" ]; then
    echo "Causely webhook URL: https://${TUNNEL_HOSTNAME}/webhook/causely"
    echo "Paste that into Causely: Settings -> Notifications -> Generic,"
    echo "with Authorization token = CAUSELY_WEBHOOK_SECRET from .env."
    echo "(stable across restarts — named tunnel '$TUNNEL_NAME')"
  else
    echo "Named tunnel '$TUNNEL_NAME' is running, but WEBHOOK_TUNNEL_HOSTNAME"
    echo "isn't set, so this script doesn't know the public URL to print."
    echo "Set it to whatever hostname you routed to this tunnel — see"
    echo "docs/causely-webhook.md."
  fi
else
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
    echo "(this URL rotates every time you restart this script — set"
    echo "WEBHOOK_TUNNEL_NAME + WEBHOOK_TUNNEL_HOSTNAME for a durable one;"
    echo "see docs/causely-webhook.md)"
  fi
fi

wait
