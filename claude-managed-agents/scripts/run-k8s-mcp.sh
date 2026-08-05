#!/usr/bin/env bash
# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
#
# Runs a read-only Kubernetes MCP server locally against the kind cluster's
# kubeconfig, then opens a cloudflared quick tunnel to it.
#
# Why a tunnel: a Claude Managed Agent runs in Anthropic's cloud, not on this
# machine, so it can't reach localhost or a private kind API server directly.
# The tunnel gives the MCP server a public HTTPS URL the cloud agent can call.
# The URL is random and rotates every time this script restarts — re-copy it
# into .env (K8S_MCP_URL) after each restart.
set -euo pipefail

PORT="${K8S_MCP_PORT:-8080}"
KUBECONFIG_PATH="${KUBECONFIG:-$HOME/.kube/config}"

command -v cloudflared >/dev/null || {
  echo "cloudflared not found — https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/" >&2
  exit 1
}

RUNNER=(kubernetes-mcp-server)
command -v kubernetes-mcp-server >/dev/null || RUNNER=(npx -y kubernetes-mcp-server@latest)

echo "Starting kubernetes-mcp-server (read-only) on :$PORT against $KUBECONFIG_PATH"
"${RUNNER[@]}" --port "$PORT" --read-only --kubeconfig "$KUBECONFIG_PATH" &
MCP_PID=$!

TUNNEL_LOG="$(mktemp)"
cleanup() {
  kill "$MCP_PID" "${TUNNEL_PID:-}" 2>/dev/null || true
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
  echo "K8S_MCP_URL=${URL}/mcp"
  echo "Paste that into .env, then run: streamlit run app.py"
  echo "(this URL rotates every time you restart this script)"
fi

wait
