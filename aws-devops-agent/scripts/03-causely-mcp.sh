#!/usr/bin/env bash
# Register the Causely MCP server at account level and attach it to the Agent
# Space with an explicit read-only tool allowlist.
#
# Required environment:
#   CAUSELY_CLIENT_ID, CAUSELY_CLIENT_SECRET   OAuth client_credentials for the tenant
#
# The tool allowlist is intersected with what the server actually advertises, so a
# renamed or retired Causely tool surfaces as a warning here instead of a silent
# gap mid-investigation.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account

: "${CAUSELY_CLIENT_ID:?set CAUSELY_CLIENT_ID (Causely MCP OAuth client id)}"
: "${CAUSELY_CLIENT_SECRET:?set CAUSELY_CLIENT_SECRET (Causely MCP OAuth client secret)}"

agent_space_id="$(require_state agent-space-id)"
MCP_NAME="${MCP_NAME:-Causely}"

# Read-only, investigation-relevant tools. Deliberately excludes submit_feedback
# and generate_ticket (both write back into Causely) and the bulk analytics tools,
# which burn the per-space tool quota without helping either flow.
DESIRED_TOOLS=(
  get_diagnoses
  get_diagnosis_details
  get_diagnosis_observable_signals
  get_issues
  get_issue_details
  get_symptoms
  get_service_summary
  get_entity_health
  get_environment_health
  get_topology
  get_entities
  get_incident_impact
  get_slo
  get_logs
  get_metrics
  get_events
  name_lookup
  investigate_alert
  postmortem
)

# ---------------------------------------------------------------------------
info "Checking Causely credentials against $CAUSELY_MCP_ENDPOINT"
# ---------------------------------------------------------------------------

token_response="$(curl -sS --max-time 15 -X POST "$CAUSELY_TOKEN_ENDPOINT" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode "grant_type=client_credentials" \
  --data-urlencode "client_id=${CAUSELY_CLIENT_ID}" \
  --data-urlencode "client_secret=${CAUSELY_CLIENT_SECRET}" 2>/dev/null || true)"

access_token="$(json_get "$token_response" access_token)"
[[ -n "$access_token" ]] ||
  die "client_credentials exchange failed — registration would fail too. Response: ${token_response:0:200}"
ok "obtained an access token"

# Ask the server what it actually exposes, rather than trusting a hardcoded list.
tools_response="$(curl -sS --max-time 25 -X POST "$CAUSELY_MCP_ENDPOINT" \
  -H "Authorization: Bearer ${access_token}" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' 2>/dev/null || true)"

available="$(python3 -c '
import json, re, sys
raw = sys.argv[1]
# Streamable HTTP may answer as SSE; take the first JSON object either way.
match = re.search(r"^data:\s*(\{.*\})\s*$", raw, re.MULTILINE)
try:
    parsed = json.loads(match.group(1) if match else raw)
except Exception:
    raise SystemExit(0)
for tool in (parsed.get("result") or {}).get("tools") or []:
    name = tool.get("name")
    if name:
        print(name)
' "$tools_response")"

[[ -n "$available" ]] || die "could not list tools from the MCP server. Response: ${tools_response:0:200}"
ok "server advertises $(wc -l <<<"$available") tools"

# ---------------------------------------------------------------------------
echo
info "Resolving the tool allowlist"
# ---------------------------------------------------------------------------

selected=()
missing=()
for tool in "${DESIRED_TOOLS[@]}"; do
  if grep -qxF "$tool" <<<"$available"; then
    selected+=("$tool")
    # The service caps MCP tool names at 64 characters.
    (( ${#tool} <= 64 )) || die "tool name exceeds the 64-character limit: $tool"
  else
    missing+=("$tool")
  fi
done

(( ${#selected[@]} > 0 )) || die "none of the desired tools exist on this server"
ok "allowlisting ${#selected[@]} tools"

if (( ${#missing[@]} > 0 )); then
  warn "not advertised by this tenant, skipping: ${missing[*]}"
fi

# Surface anything read-only we are leaving on the table, so the choice stays deliberate.
unselected="$(grep -vxF -f <(printf '%s\n' "${selected[@]}") <<<"$available" || true)"
if [[ -n "$unselected" ]]; then
  skip "not allowlisted: $(tr '\n' ' ' <<<"$unselected")"
fi

tools_json="$(printf '%s\n' "${selected[@]}" | python3 -c '
import json, sys
print(json.dumps([line.strip() for line in sys.stdin if line.strip()]))
')"

# ---------------------------------------------------------------------------
echo
info "Registering the MCP server at account level"
# ---------------------------------------------------------------------------

# Reuse an existing registration for the same endpoint. Credentials cannot be
# updated in place — rotation means deregister then register again.
existing_service_id="$(agent list-services 2>/dev/null | python3 -c '
import json, sys
data = json.loads(sys.stdin.read() or "{}")
endpoint = sys.argv[1]
for service in data.get("services") or []:
    if service.get("serviceType") != "mcpserver":
        continue
    details = (service.get("additionalServiceDetails") or {}).get("mcpserver") or {}
    if details.get("endpoint") == endpoint:
        print(service["serviceId"])
        break
' "$CAUSELY_MCP_ENDPOINT" || true)"

if [[ -n "$existing_service_id" ]]; then
  service_id="$existing_service_id"
  skip "reusing registration $service_id (deregister first to rotate credentials)"
else
  service_details="$(python3 -c '
import json, sys
name, endpoint, client_id, client_secret, exchange_url = sys.argv[1:6]
print(json.dumps({
    "mcpserver": {
        "name": name,
        "endpoint": endpoint,
        "description": "Causely causal analysis for Kubernetes workloads",
        "authorizationConfig": {
            "oAuthClientCredentials": {
                "clientName": name,
                "clientId": client_id,
                "clientSecret": client_secret,
                "exchangeUrl": exchange_url,
                "scopes": ["openid", "profile", "email"],
            }
        },
    }
}))
' "$MCP_NAME" "$CAUSELY_MCP_ENDPOINT" "$CAUSELY_CLIENT_ID" "$CAUSELY_CLIENT_SECRET" "$CAUSELY_TOKEN_ENDPOINT")"

  registration="$(agent register-service \
    --service mcpserver \
    --name "$MCP_NAME" \
    --service-details "$service_details")"

  service_id="$(json_get "$registration" serviceId)"
  [[ -n "$service_id" ]] || die "no serviceId in register-service response: $registration"
  ok "registered $MCP_NAME as $service_id"

  # client_credentials needs no browser step, but report one if the API asks.
  auth_url="$(python3 -c '
import json, sys
data = json.loads(sys.argv[1] or "{}")
print(((data.get("additionalStep") or {}).get("oauth") or {}).get("authorizationUrl") or "")
' "$registration")"
  [[ -n "$auth_url" ]] && warn "additional OAuth step required, open: $auth_url"
fi

state_put causely-service-id "$service_id"

# ---------------------------------------------------------------------------
echo
info "Attaching to Agent Space $agent_space_id"
# ---------------------------------------------------------------------------

configuration="$(python3 -c '
import json, sys
print(json.dumps({"mcpserver": {"tools": json.loads(sys.argv[1])}}))
' "$tools_json")"

if association_output="$(agent associate-service \
    --agent-space-id "$agent_space_id" \
    --service-id "$service_id" \
    --configuration "$configuration" 2>&1)"; then
  association_id="$(json_get "$association_output" association.associationId)"
  status="$(json_get "$association_output" association.status)"
  ok "attached (association $association_id, status ${status:-unknown})"
  [[ -n "$association_id" ]] && state_put causely-association-id "$association_id"
  [[ "$status" == "invalid" ]] && warn "status is 'invalid' — the service could not validate the connection"
elif grep -qiE 'already|conflict|exists' <<<"$association_output"; then
  # Re-running with a changed allowlist needs update-association, not associate-service.
  skip "already attached — updating the tool allowlist"
  existing_association_id="$(agent list-associations --agent-space-id "$agent_space_id" 2>/dev/null | python3 -c '
import json, sys
data = json.loads(sys.stdin.read() or "{}")
wanted = sys.argv[1]
for association in data.get("associations") or []:
    if association.get("serviceId") == wanted:
        print(association.get("associationId") or "")
        break
' "$service_id" || true)"

  if [[ -n "$existing_association_id" ]]; then
    agent update-association \
      --agent-space-id "$agent_space_id" \
      --association-id "$existing_association_id" \
      --configuration "$configuration" >/dev/null
    state_put causely-association-id "$existing_association_id"
    ok "allowlist updated on association $existing_association_id"
  else
    warn "could not locate the existing association to update"
  fi
else
  fail "associate-service failed:"
  printf '%s\n' "$association_output" >&2
  exit 1
fi

echo
info "Causely MCP wired up"
ok "tools: ${selected[*]}"
echo
info "next: scripts/04-webhook.sh"
