#!/usr/bin/env bash
# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
#
# Runs a read-only Kubernetes MCP server locally against the kind cluster's
# kubeconfig, then opens a cloudflared tunnel to it.
#
# Why a tunnel: a Claude Managed Agent runs in Anthropic's cloud, not on this
# machine, so it can't reach localhost or a private kind API server directly.
# The tunnel gives the MCP server a public HTTPS URL the cloud agent can call.
#
# Two modes, picked by whether K8S_MCP_TUNNEL_NAME is set:
#
#   Default (unset) — a cloudflared *quick* tunnel. Zero setup, but the URL
#   is random and rotates every restart — re-copy it into .env (K8S_MCP_URL)
#   each time.
#
#   K8S_MCP_TUNNEL_NAME=<name> — a cloudflared *named* tunnel. The hostname
#   stays fixed across restarts, so K8S_MCP_URL only needs to be set once.
#   Requires a one-time setup against your own Cloudflare account + domain
#   first — same steps as the durable webhook URL in
#   docs/causely-webhook.md (`cloudflared tunnel login` / `create` /
#   `route dns`), just for this tunnel name instead. Also set
#   K8S_MCP_TUNNEL_HOSTNAME to whatever hostname you routed to it, so this
#   script can print the full URL.
#
# For a production deployment instead of a laptop process behind either kind
# of tunnel, see docs/mcp-tunnels.md.
set -euo pipefail

PORT="${K8S_MCP_PORT:-8080}"
KUBECONFIG_PATH="${KUBECONFIG:-$HOME/.kube/config}"
TUNNEL_NAME="${K8S_MCP_TUNNEL_NAME:-}"
TUNNEL_HOSTNAME="${K8S_MCP_TUNNEL_HOSTNAME:-}"

command -v cloudflared >/dev/null || {
  echo "cloudflared not found — https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/" >&2
  exit 1
}

RUNNER=(kubernetes-mcp-server)
command -v kubernetes-mcp-server >/dev/null || RUNNER=(npx -y kubernetes-mcp-server@latest)

echo "Starting kubernetes-mcp-server (read-only) on :$PORT against $KUBECONFIG_PATH"
# The MCP Go SDK's DNS-rebinding protection rejects any request that arrives over a
# loopback connection with a non-loopback Host header — which is exactly what the
# cloudflared tunnel sends (it forwards the public tunnel hostname to localhost).
# Disabling it here is safe: real protection against a rebound DNS name still comes
# from the tunnel only exposing this one server, not from the Host check.
MCPGODEBUG=disablelocalhostprotection=1 "${RUNNER[@]}" --port "$PORT" --read-only --kubeconfig "$KUBECONFIG_PATH" &
MCP_PID=$!

TUNNEL_LOG="$(mktemp)"
cleanup() {
  kill "$MCP_PID" "${TUNNEL_PID:-}" 2>/dev/null || true
  rm -f "$TUNNEL_LOG"
}
trap cleanup EXIT

sleep 2

if [ -n "$TUNNEL_NAME" ]; then
  # Named tunnel — requires "cloudflared tunnel create $TUNNEL_NAME" and
  # "cloudflared tunnel route dns $TUNNEL_NAME <hostname>" to have already
  # been run once. --url here works without a config.yml/ingress rules
  # since there's only the one local service.
  #
  # --ha-connections 1: a named tunnel defaults to 4 simultaneous
  # connections to different Cloudflare edge locations, for redundancy.
  # That's fine for ordinary request/response traffic (webhook.py, plain
  # HTTP), but this server's /mcp endpoint responds over SSE — a real
  # `initialize` call reliably 502'd through the default 4-connection
  # named tunnel (confirmed empirically: identical request, same server,
  # worked over a quick tunnel and over this same named tunnel forced to
  # one connection, failed over the 4-connection default). Quick tunnels
  # use a single connection for the same reason; this makes the named
  # tunnel match that.
  echo "Starting named cloudflared tunnel '$TUNNEL_NAME' -> http://localhost:$PORT"
  cloudflared tunnel --ha-connections 1 run --url "http://localhost:$PORT" "$TUNNEL_NAME" >"$TUNNEL_LOG" 2>&1 &
  TUNNEL_PID=$!

  sleep 3
  if ! kill -0 "$TUNNEL_PID" 2>/dev/null; then
    echo "cloudflared exited immediately — check $TUNNEL_LOG" >&2
    echo "(has '$TUNNEL_NAME' been created and DNS-routed yet?)" >&2
    cat "$TUNNEL_LOG" >&2
    exit 1
  fi

  echo
  if [ -n "$TUNNEL_HOSTNAME" ]; then
    echo "K8S_MCP_URL=https://${TUNNEL_HOSTNAME}/mcp"
    echo "Paste that into .env (only needed once — stable across restarts),"
    echo "then run: streamlit run app.py"
  else
    echo "Named tunnel '$TUNNEL_NAME' is running, but K8S_MCP_TUNNEL_HOSTNAME"
    echo "isn't set, so this script doesn't know the public URL to print."
    echo "Set it to whatever hostname you routed to this tunnel."
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
    echo "K8S_MCP_URL=${URL}/mcp"
    echo "Paste that into .env, then run: streamlit run app.py"
    echo "(this URL rotates every time you restart this script — set"
    echo "K8S_MCP_TUNNEL_NAME + K8S_MCP_TUNNEL_HOSTNAME for a durable one)"
  fi
fi

wait
