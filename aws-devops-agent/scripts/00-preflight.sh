#!/usr/bin/env bash
# Verify every prerequisite before creating any AWS resources.
#
#   scripts/00-preflight.sh          check only
#   scripts/00-preflight.sh --fix    also promote the Resource Explorer aggregator
#
# Causely credential checks run only when CAUSELY_CLIENT_ID and
# CAUSELY_CLIENT_SECRET are exported.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

FIX=0
[[ "${1:-}" == "--fix" ]] && FIX=1

failures=0
note_failure() { fail "$1"; failures=$((failures + 1)); }

require_cmd aws
require_cmd python3
require_cmd curl

# ---------------------------------------------------------------------------
info "AWS CLI and credentials"
# ---------------------------------------------------------------------------

cli_version="$(aws --version 2>&1 | sed -n 's#^aws-cli/\([0-9.]*\).*#\1#p')"
ok "aws-cli $cli_version"

if agent_cli_name="$(detect_agent_cli)"; then
  state_put agent-cli "$agent_cli_name"
  ok "DevOps Agent commands available as: aws $agent_cli_name"
else
  note_failure "this AWS CLI has no DevOps Agent commands (found $cli_version)"
  warn "upgrade AWS CLI v2 to 2.36+ — see DEPLOYMENT.md step 1"
fi

confirm_account || failures=$((failures + 1))
ok "agent space region: $REGION"

# ---------------------------------------------------------------------------
info "Resource Explorer cross-region discovery"
# ---------------------------------------------------------------------------

# An Agent Space discovers resources across every region of an associated
# account, but cross-region topology discovery goes through Resource Explorer,
# which needs an AGGREGATOR index. Without one the agent is blind to everything
# outside its own region — so if your clusters do not live in $REGION, they are
# invisible to it, silently and with no error.
aggregator_json="$(aws resource-explorer-2 list-indexes --type AGGREGATOR --region "$REGION" 2>/dev/null || echo '{}')"
aggregator_arn="$(python3 -c '
import json, sys
indexes = json.loads(sys.argv[1] or "{}").get("Indexes") or []
print(indexes[0]["Arn"] if indexes else "")
' "$aggregator_json")"

if [[ -n "$aggregator_arn" ]]; then
  ok "aggregator index present: $aggregator_arn"
else
  local_arn="$(aws resource-explorer-2 get-index --region "$REGION" --query Arn --output text 2>/dev/null || true)"
  if [[ -z "$local_arn" || "$local_arn" == "None" ]]; then
    note_failure "no Resource Explorer index in $REGION — create one, then re-run with --fix"
  elif (( FIX )); then
    info "promoting $local_arn to AGGREGATOR"
    aws resource-explorer-2 update-index-type \
      --arn "$local_arn" --type AGGREGATOR --region "$REGION" >/dev/null
    ok "promoted — aggregation across regions takes a few minutes to populate"
  else
    note_failure "no AGGREGATOR index; the agent will not see resources outside $REGION"
    warn "re-run as 'scripts/00-preflight.sh --fix' to promote $local_arn"
  fi
fi

# Report the local indexes so it is obvious which regions are actually covered.
for region in us-east-1 us-east-2; do
  state="$(aws resource-explorer-2 get-index --region "$region" --query State --output text 2>/dev/null || echo NONE)"
  if [[ "$state" == "ACTIVE" ]]; then
    ok "$region index ACTIVE"
  else
    warn "$region has no active Resource Explorer index (state: $state) — resources there stay undiscovered"
  fi
done

# ---------------------------------------------------------------------------
info "Causely MCP endpoint ($CAUSELY_MCP_ENDPOINT)"
# ---------------------------------------------------------------------------

metadata="$(curl -fsS --max-time 10 "${CAUSELY_BASE}/.well-known/oauth-authorization-server" 2>/dev/null || true)"
if [[ -z "$metadata" ]]; then
  note_failure "cannot read OAuth metadata from $CAUSELY_BASE"
else
  grants="$(python3 -c '
import json, sys
print(",".join(json.loads(sys.argv[1]).get("grant_types_supported") or []))
' "$metadata")"
  token_endpoint="$(json_get "$metadata" token_endpoint)"
  ok "grants: $grants"
  ok "token endpoint: $token_endpoint"
  [[ "$grants" == *client_credentials* ]] ||
    note_failure "endpoint does not advertise client_credentials, which DevOps Agent needs"
fi

if [[ -z "${CAUSELY_CLIENT_ID:-}" || -z "${CAUSELY_CLIENT_SECRET:-}" ]]; then
  warn "CAUSELY_CLIENT_ID / CAUSELY_CLIENT_SECRET not set — skipping credential check"
  warn "mint MCP client credentials in Causely staging, then re-run to validate them"
else
  token_response="$(curl -sS --max-time 15 -X POST "$CAUSELY_TOKEN_ENDPOINT" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "grant_type=client_credentials" \
    --data-urlencode "client_id=${CAUSELY_CLIENT_ID}" \
    --data-urlencode "client_secret=${CAUSELY_CLIENT_SECRET}" 2>/dev/null || true)"

  access_token="$(json_get "$token_response" access_token)"
  if [[ -z "$access_token" ]]; then
    note_failure "client_credentials exchange failed — DevOps Agent registration will also fail"
    [[ -n "$token_response" ]] && warn "response: ${token_response:0:200}"
  else
    ok "client_credentials exchange succeeded"

    # Prove the token actually opens the MCP server and lists tools. Registration
    # in the console validates the connection, so a failure here predicts that.
    tools_response="$(curl -sS --max-time 20 -X POST "$CAUSELY_MCP_ENDPOINT" \
      -H "Authorization: Bearer ${access_token}" \
      -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' \
      -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' 2>/dev/null || true)"

    tool_count="$(python3 -c '
import json, re, sys
raw = sys.argv[1]
# Streamable HTTP may answer as SSE; pull the first JSON object out either way.
match = re.search(r"^data:\s*(\{.*\})\s*$", raw, re.MULTILINE)
payload = match.group(1) if match else raw
try:
    tools = (json.loads(payload).get("result") or {}).get("tools") or []
except Exception:
    tools = []
print(len(tools))
' "$tools_response")"

    if [[ "$tool_count" == "0" ]]; then
      warn "token works but tools/list returned nothing parseable — inspect manually"
      [[ -n "$tools_response" ]] && warn "response: ${tools_response:0:200}"
    else
      ok "MCP server exposes $tool_count tools"
    fi
  fi
fi

# ---------------------------------------------------------------------------
echo
if (( failures )); then
  die "$failures precondition(s) failed — resolve them before running 01-iam-roles.sh"
fi
info "preflight passed — next: scripts/01-iam-roles.sh"
