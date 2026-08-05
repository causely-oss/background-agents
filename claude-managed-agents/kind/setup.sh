#!/usr/bin/env bash
# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
# Creates the demo kind cluster and deploys a small workload to investigate.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER_NAME="managed-agent-demo"

command -v kind >/dev/null || { echo "kind not found — https://kind.sigs.k8s.io/docs/user/quick-start/#installation" >&2; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl not found — https://kubernetes.io/docs/tasks/tools/" >&2; exit 1; }

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  echo "kind cluster '$CLUSTER_NAME' already exists, reusing it"
else
  kind create cluster --config "$DIR/kind-cluster.yaml"
fi

kubectl apply -f "$DIR/workload.yaml"

echo "Waiting for the healthy workloads to roll out (backend is deliberately crash-looping, so it's skipped)..."
kubectl -n demo rollout status deployment/web --timeout=120s
kubectl -n demo rollout status deployment/cache --timeout=120s

cat <<EOF

Cluster ready. namespace "demo" has:
  - web      (nginx, healthy)
  - cache    (redis, healthy)
  - backend  (deliberately crash-looping — something for the agent to find)

Next:
  1. In another terminal: ./scripts/run-k8s-mcp.sh
  2. Paste the printed https://...trycloudflare.com/mcp URL into .env as K8S_MCP_URL
  3. streamlit run app.py
EOF
