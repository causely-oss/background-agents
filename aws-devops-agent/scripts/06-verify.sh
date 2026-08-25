#!/usr/bin/env bash
# Report the current state of the deployment. Read-only; safe to run any time.
#
# Python blocks use quoted heredocs rather than `python3 -c '...'`, so the embedded
# code can contain quotes freely.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account

agent_space_id="$(state_get agent-space-id)"
[[ -n "$agent_space_id" ]] || die "no agent space recorded — run scripts/02-agent-space.sh"

# ---------------------------------------------------------------------------
info "Agent Space"
# ---------------------------------------------------------------------------
space="$(agent get-agent-space --agent-space-id "$agent_space_id" 2>/dev/null || echo '{}')"
python3 - "$space" <<'PY'
import json, sys
data = json.loads(sys.argv[1] or "{}")
space = data.get("agentSpace") or data
if not space:
    print("  could not read the agent space")
for key in ("agentSpaceId", "name", "status", "state", "locale", "createdAt"):
    if space.get(key):
        print(f"  {key}: {space[key]}")
PY

# ---------------------------------------------------------------------------
echo
info "Associations"
# ---------------------------------------------------------------------------
associations="$(agent list-associations --agent-space-id "$agent_space_id" 2>/dev/null || echo '{}')"
python3 - "$associations" <<'PY'
import json, sys
data = json.loads(sys.argv[1] or "{}")
items = next((v for v in data.values() if isinstance(v, list)), [])
if not items:
    print("  (none)")
for item in items:
    if not isinstance(item, dict):
        continue
    # list-associations omits serviceType, so infer the kind from the configuration.
    config = item.get("configuration") or {}
    kind = next(iter(config), None) or item.get("serviceType") or "unknown"
    status = item.get("status") or item.get("state") or "?"
    detail = ""
    if kind == "mcpserver":
        tools = (config.get("mcpserver") or {}).get("tools") or []
        detail = f" ({len(tools)} tools allowlisted)"
    elif kind == "aws":
        aws_config = config.get("aws") or {}
        detail = f" ({aws_config.get('accountId')} as {aws_config.get('accountType')})"
    print(f"  {kind}: {status}{detail}")
PY

# ---------------------------------------------------------------------------
echo
info "Registered services"
# ---------------------------------------------------------------------------
services="$(agent list-services 2>/dev/null || echo '{}')"
python3 - "$services" <<'PY'
import json, sys
data = json.loads(sys.argv[1] or "{}")
items = data.get("services") or []
if not items:
    print("  (none registered)")
for item in items:
    if not isinstance(item, dict):
        continue
    kind = item.get("serviceType") or "?"
    name = item.get("name") or ""
    details = (item.get("additionalServiceDetails") or {}).get(kind) or {}
    endpoint = details.get("endpoint") or ""
    auth = details.get("authorizationMethod") or ""
    line = f"  {kind}: {name} {item.get('serviceId', '')}".rstrip()
    print(line)
    if endpoint:
        print(f"      {endpoint} ({auth})")
PY

if [[ -z "$(state_get causely-service-id)" ]]; then
  warn "Causely MCP not registered yet — run scripts/03-causely-mcp.sh"
fi

# ---------------------------------------------------------------------------
echo
info "Investigations"
# ---------------------------------------------------------------------------
tasks="$(agent list-backlog-tasks --agent-space-id "$agent_space_id" 2>/dev/null || echo '{}')"
python3 - "$tasks" <<'PY'
import json, sys
data = json.loads(sys.argv[1] or "{}")
items = next((v for v in data.values() if isinstance(v, list)), [])
if not items:
    print("  (none yet)")
for item in items[:10]:
    if not isinstance(item, dict):
        continue
    title = item.get("title") or item.get("name") or "(untitled)"
    print(f"  [{item.get('status', '?')}] {title}")
if len(items) > 10:
    print(f"  ... and {len(items) - 10} more")
PY

# ---------------------------------------------------------------------------
echo
info "Relay"
# ---------------------------------------------------------------------------
relay_endpoint="$(state_get relay-endpoint)"
if [[ -z "$relay_endpoint" ]]; then
  skip "not deployed (scripts/05-deploy-relay.sh)"
else
  ok "endpoint: $relay_endpoint"
  config="$(aws lambda get-function-configuration --function-name "$LAMBDA_NAME" \
    --region "$REGION" 2>/dev/null || echo '{}')"
  python3 - "$config" <<'PY'
import json, sys
data = json.loads(sys.argv[1] or "{}")
env = (data.get("Environment") or {}).get("Variables") or {}
print(f"  runtime: {data.get('Runtime', '?')}   state: {data.get('State', '?')}")
dry = str(env.get("DRY_RUN", "false")).lower()
suffix = "   <- translating only, no investigations" if dry in ("1", "true", "yes") else ""
print(f"  DRY_RUN: {dry}{suffix}")
PY
fi

# ---------------------------------------------------------------------------
echo
info "Cross-region discovery"
# ---------------------------------------------------------------------------
aggregator="$(aws resource-explorer-2 list-indexes --type AGGREGATOR --region "$REGION" \
  --query 'Indexes[0].Arn' --output text 2>/dev/null || echo None)"
if [[ "$aggregator" == "None" || -z "$aggregator" ]]; then
  fail "no AGGREGATOR index — resources outside $REGION stay invisible to the agent"
  warn "fix with: scripts/00-preflight.sh --fix"
else
  ok "aggregator present in $REGION"
fi

echo
info "console: https://${REGION}.console.aws.amazon.com/aidevops/home?region=${REGION}"
info "open the web app from the Agent Space page via 'Operator access'"
