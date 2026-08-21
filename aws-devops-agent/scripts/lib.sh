#!/usr/bin/env bash
# Shared configuration and helpers for the AWS DevOps Agent + Causely example.
# Sourced by every script in this directory; not meant to be run directly.

set -euo pipefail

# The AWS CLI opens a pager for some output, which wedges non-interactive runs.
export AWS_PAGER=""

# ---------------------------------------------------------------------------
# Configuration (override any of these via the environment)
# ---------------------------------------------------------------------------

# Pick a region where DevOps Agent is available. It does not have to be the
# region your clusters run in: an Agent Space discovers resources across every
# region of an associated account, so one space can cover workloads elsewhere.
# us-east-1 is the safe default — it gets every feature first, including previews.
REGION="${DEVOPS_AGENT_REGION:-us-east-1}"

# The AWS account the Agent Space lives in. Left empty, confirm_account resolves
# it from your current credentials. Set DEVOPS_AGENT_ACCOUNT_ID to pin it, and
# every script will refuse to run against any other account — worth doing if you
# have more than one profile in play.
ACCOUNT_ID="${DEVOPS_AGENT_ACCOUNT_ID:-}"

AGENT_SPACE_NAME="${AGENT_SPACE_NAME:-CauselyDemo}"
AGENT_SPACE_DESCRIPTION="${AGENT_SPACE_DESCRIPTION:-Causely + AWS DevOps Agent showcase}"

ROLE_AGENTSPACE="${ROLE_AGENTSPACE:-DevOpsAgentRole-AgentSpace}"
ROLE_WEBAPP="${ROLE_WEBAPP:-DevOpsAgentRole-WebappAdmin}"
ROLE_RELAY="${ROLE_RELAY:-CauselyRelayLambdaRole}"

LAMBDA_NAME="${LAMBDA_NAME:-causely-devops-agent-relay}"

# Ingress for the relay is an API Gateway HTTP API rather than a Lambda Function
# URL. Some accounts deny anonymous invocation of Function URLs outright — even
# with AuthType NONE and a correct Principal "*" resource policy, every request
# comes back 403 from Lambda's own auth layer with nothing in the function logs.
# An HTTP API is reliably reachable and passes the Authorization header through
# untouched, and the function still enforces the bearer token itself.
RELAY_API_NAME="${RELAY_API_NAME:-causely-relay-api}"

# Secrets Manager ids. The agent webhook secret is returned exactly once at
# creation, so it is stored here rather than in a Lambda environment variable.
SECRET_WEBHOOK="${SECRET_WEBHOOK:-causely/devops-agent-webhook}"
SECRET_INBOUND="${SECRET_INBOUND:-causely/causely-notif-token}"

# Your Causely tenant's MCP endpoint. Override CAUSELY_MCP_ENDPOINT if you are on
# a dedicated or non-production tenant; the OAuth token endpoint is derived from it.
CAUSELY_MCP_ENDPOINT="${CAUSELY_MCP_ENDPOINT:-https://api.causely.app/mcp}"
CAUSELY_BASE="${CAUSELY_MCP_ENDPOINT%/mcp}"
CAUSELY_TOKEN_ENDPOINT="${CAUSELY_TOKEN_ENDPOINT:-${CAUSELY_BASE}/mcp/oauth/token}"

# The Causely web portal, used only to build human-clickable links inside the
# notifications that make-fixture.sh writes. Derived from the API host by
# convention (api.* -> app.*); override if your tenant does not follow it.
CAUSELY_APP_BASE="${CAUSELY_APP_BASE:-${CAUSELY_BASE/\/\/api./\/\/app.}}"

# Optional: Grafana MCP endpoint for the same cluster (see scripts/07-grafana-mcp.sh).
# No default — this is specific to your deployment.
GRAFANA_MCP_ENDPOINT="${GRAFANA_MCP_ENDPOINT:-}"

# Optional: the EKS cluster to grant the agent read-only kubectl access to
# (see scripts/eks-access.sh). No defaults — set both to your own cluster.
# Note the name Causely shows you may not be the EKS cluster name; see that
# script's header for how to confirm the mapping.
EKS_CLUSTER="${EKS_CLUSTER:-}"
EKS_CLUSTER_REGION="${EKS_CLUSTER_REGION:-$REGION}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="$REPO_ROOT/.state"
BUILD_DIR="$REPO_ROOT/.build"
mkdir -p "$STATE_DIR"

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

if [[ -t 1 ]]; then
  C_RESET=$'\033[0m'; C_BLUE=$'\033[1;34m'; C_GREEN=$'\033[32m'
  C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'; C_DIM=$'\033[2m'
else
  C_RESET=""; C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_DIM=""
fi

# All logging goes to stderr, so helper functions can log freely while still
# returning a value on stdout for command substitution.
info() { printf '%s==>%s %s\n' "$C_BLUE" "$C_RESET" "$*" >&2; }
ok()   { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*" >&2; }
skip() { printf '  %s·%s %s\n' "$C_DIM" "$C_RESET" "$*" >&2; }
warn() { printf '  %s!%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
fail() { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
die()  { fail "$*"; exit 1; }

# ---------------------------------------------------------------------------
# Small state store, so scripts are independently re-runnable
# ---------------------------------------------------------------------------

state_put() { printf '%s' "$2" >"$STATE_DIR/$1"; }
state_get() { [[ -f "$STATE_DIR/$1" ]] && cat "$STATE_DIR/$1" || true; }

require_state() {
  local value
  value="$(state_get "$1")"
  [[ -n "$value" ]] || die "missing state '$1' — run the earlier setup script first"
  printf '%s' "$value"
}

# ---------------------------------------------------------------------------
# DevOps Agent CLI namespace detection
#
# The user guide documents `aws devops-agent` while an AWS blog post uses
# `aws devopsagent`, and the service endpoint is `aidevops`. Rather than betting
# on one, detect which the installed CLI actually exposes.
# ---------------------------------------------------------------------------

_cli_has_service() {
  # `aws <service>` with no operation reports a missing *operation* for a real
  # service and an invalid *choice* for an unknown one. Avoids `help`, which
  # invokes a pager. Lowercased because the wording shifted between CLI releases
  # ("Invalid choice, valid choices are" -> "Found invalid choice").
  local out
  out="$(aws "$1" 2>&1 | tr '[:upper:]' '[:lower:]' || true)"
  [[ "$out" != *"invalid choice"* ]]
}

detect_agent_cli() {
  if [[ -n "${DEVOPS_AGENT_CLI:-}" ]]; then
    printf '%s' "$DEVOPS_AGENT_CLI"
    return 0
  fi
  local candidate
  for candidate in devops-agent devopsagent aidevops; do
    if _cli_has_service "$candidate"; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}

# Resolve once and memoise into state so later scripts agree with preflight.
agent_cli() {
  local cached
  cached="$(state_get agent-cli)"
  if [[ -n "$cached" ]]; then
    printf '%s' "$cached"
    return 0
  fi
  local detected
  if detected="$(detect_agent_cli)"; then
    state_put agent-cli "$detected"
    printf '%s' "$detected"
    return 0
  fi
  die "this AWS CLI has no DevOps Agent commands — upgrade AWS CLI v2 (see DEPLOYMENT.md step 1) then re-run scripts/00-preflight.sh"
}

# Thin wrapper: `agent <operation> [args...]`, region applied automatically.
agent() {
  local cli
  cli="$(agent_cli)"
  aws "$cli" "$@" --region "$REGION" --output json
}

# ---------------------------------------------------------------------------
# Misc helpers
# ---------------------------------------------------------------------------

# Read a JSON value with python3 rather than jq, which is not guaranteed present.
json_get() {
  # usage: json_get <json-string> <dotted.path>
  python3 -c '
import json, sys
data = json.loads(sys.argv[1] or "{}")
for part in sys.argv[2].split("."):
    if data is None:
        break
    data = data.get(part) if isinstance(data, dict) else None
print("" if data is None else data)
' "$1" "$2"
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

confirm_account() {
  local actual
  actual="$(aws sts get-caller-identity --query Account --output text 2>/dev/null || true)"
  [[ -n "$actual" ]] || die "cannot reach AWS STS — check your credentials"
  if [[ -n "$ACCOUNT_ID" && "$actual" != "$ACCOUNT_ID" ]]; then
    die "connected to account $actual but DEVOPS_AGENT_ACCOUNT_ID pins $ACCOUNT_ID"
  fi
  # Not pinned: adopt whatever the caller is authenticated to. Every use of
  # ACCOUNT_ID in these scripts happens after this call, so this is safe.
  ACCOUNT_ID="$actual"
  ok "authenticated to account $actual"
}
