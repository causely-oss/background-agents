#!/usr/bin/env bash
# Register the Grafana MCP server and attach it to the Agent Space with a
# read-only tool allowlist.
#
#   GRAFANA_MCP_ENDPOINT=https://<host>/mcp GRAFANA_TOKEN=<sa token> scripts/07-grafana-mcp.sh
#   GRAFANA_AUTH_MODE=bearer GRAFANA_MCP_ENDPOINT=... GRAFANA_TOKEN=... scripts/07-grafana-mcp.sh
#
# PREREQUISITE: the endpoint must actually enforce authentication. DevOps Agent
# probes it during registration and refuses any MCP server that answers
# unauthenticated:
#
#   ValidationException: MCP Server at '...' does not have any authorization
#   configured. Authorization is required for remote MCP Servers to ensure secure
#   communication.
#
# A default mcp-grafana deployment carries the service-account token in its own
# environment and therefore answers anyone, so registration will fail until you
# make it require a per-request credential — either by removing that token so it
# reads X-Grafana-API-Key from the caller, or by putting a gateway auth policy in
# front of it. This script probes the endpoint first and stops early with that
# explanation rather than letting AWS fail opaquely.
#
# Auth modes: apikey (default) sends the token in X-Grafana-API-Key, which is what
# mcp-grafana reads; bearer sends Authorization: Bearer.
#
# Grafana complements Causely rather than duplicating it: Causely supplies the
# causal chain, Grafana supplies the raw PromQL/LogQL underneath it.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account

: "${GRAFANA_MCP_ENDPOINT:?set GRAFANA_MCP_ENDPOINT to your Grafana MCP server URL, e.g. https://grafana.example.com/mcp}"

agent_space_id="$(require_state agent-space-id)"
GRAFANA_MCP_NAME="${GRAFANA_MCP_NAME:-Grafana}"

# Read-only and investigation-relevant. Every write tool the server exposes is
# deliberately absent: update_dashboard, create_incident, add_activity_to_incident,
# create_annotation, update_annotation, create_folder, alerting_manage_rules,
# alerting_manage_routing.
#
# This is also not the full read-only set. Tools count against a per-Agent-Space
# quota shared with Causely's 19, so this is trimmed to what actually helps an
# investigation.
DESIRED_TOOLS=(
  query_prometheus
  query_prometheus_histogram
  list_prometheus_metric_names
  list_prometheus_label_names
  list_prometheus_label_values
  query_loki_logs
  query_loki_stats
  find_error_pattern_logs
  find_slow_requests
  list_datasources
  search_dashboards
  get_dashboard_summary
  get_dashboard_panel_queries
  get_annotations
  list_alert_groups
  get_assertions
  generate_deeplink
)

# ---------------------------------------------------------------------------
info "Checking $GRAFANA_MCP_ENDPOINT"
# ---------------------------------------------------------------------------

GRAFANA_AUTH_MODE="${GRAFANA_AUTH_MODE:-apikey}"
GRAFANA_API_KEY_HEADER="${GRAFANA_API_KEY_HEADER:-X-Grafana-API-Key}"

auth_header=()
if [[ -n "${GRAFANA_TOKEN:-}" ]]; then
  if [[ "$GRAFANA_AUTH_MODE" == "bearer" ]]; then
    auth_header=(-H "Authorization: Bearer ${GRAFANA_TOKEN}")
  else
    auth_header=(-H "${GRAFANA_API_KEY_HEADER}: ${GRAFANA_TOKEN}")
  fi
fi

# DevOps Agent probes the endpoint and rejects servers that answer anonymously.
# Catch that here, where the message can actually explain what to do about it.
anon_status="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 -X POST "$GRAFANA_MCP_ENDPOINT" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"anon-probe","version":"1"}}}' \
  2>/dev/null || echo 000)"

if [[ "$anon_status" == "200" && "${GRAFANA_FORCE:-}" != "1" ]]; then
  fail "the endpoint answers unauthenticated requests (HTTP 200 with no credentials)"
  warn "DevOps Agent will refuse to register it:"
  warn "  \"does not have any authorization configured. Authorization is required"
  warn "   for remote MCP Servers to ensure secure communication.\""
  echo >&2
  warn "This is also worth fixing on its own merits: the server is reachable from"
  warn "the public internet and exposes write tools (update_dashboard,"
  warn "create_incident, alerting_manage_rules, ...) to anyone who finds it."
  echo >&2
  warn "Make the MCP server require a credential per request — typically by removing"
  warn "the service-account token from its environment so it reads"
  warn "${GRAFANA_API_KEY_HEADER} from the caller instead, or by adding a gateway"
  warn "auth policy in front of it. Then re-run with GRAFANA_TOKEN set."
  die "aborting before registration"
fi
[[ "$anon_status" == "200" ]] && warn "GRAFANA_FORCE=1 set — continuing despite anonymous access"

# Streamable HTTP requires a session: initialize, then reuse the returned id.
headers_file="$(mktemp)"
trap 'rm -f "$headers_file"' EXIT
curl -sS -D "$headers_file" -o /dev/null --max-time 20 -X POST "$GRAFANA_MCP_ENDPOINT" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  "${auth_header[@]}" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"devops-agent-setup","version":"1"}}}' \
  2>/dev/null || die "cannot reach $GRAFANA_MCP_ENDPOINT"

session_id="$(grep -i '^mcp-session-id:' "$headers_file" | tr -d '\r' | awk '{print $2}')"
[[ -n "$session_id" ]] || die "no mcp-session-id returned — is this a Streamable HTTP MCP server?"
ok "session established"

curl -sS --max-time 20 -X POST "$GRAFANA_MCP_ENDPOINT" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: ${session_id}" "${auth_header[@]}" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null 2>&1 || true

tools_response="$(curl -sS --max-time 30 -X POST "$GRAFANA_MCP_ENDPOINT" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: ${session_id}" "${auth_header[@]}" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' 2>/dev/null || true)"

available="$(python3 -c '
import json, re, sys
raw = sys.argv[1]
frames = re.findall(r"^data:\s*(\{.*\})\s*$", raw, re.MULTILINE)
try:
    parsed = json.loads(frames[-1] if frames else raw)
except Exception:
    raise SystemExit(0)
for tool in (parsed.get("result") or {}).get("tools") or []:
    if tool.get("name"):
        print(tool["name"])
' "$tools_response")"

[[ -n "$available" ]] || die "could not list tools. Response: ${tools_response:0:200}"
ok "server advertises $(wc -l <<<"$available") tools"

# ---------------------------------------------------------------------------
echo
info "Resolving the tool allowlist"
# ---------------------------------------------------------------------------

selected=(); missing=()
for tool in "${DESIRED_TOOLS[@]}"; do
  if grep -qxF "$tool" <<<"$available"; then
    prefixed="${GRAFANA_MCP_NAME}_${tool}"
    (( ${#prefixed} <= 64 )) || die "prefixed tool name exceeds 64 chars: $prefixed"
    selected+=("$tool")
  else
    missing+=("$tool")
  fi
done

(( ${#selected[@]} > 0 )) || die "none of the desired tools exist on this server"
ok "allowlisting ${#selected[@]} of $(wc -l <<<"$available") tools"
(( ${#missing[@]} > 0 )) && warn "not advertised, skipping: ${missing[*]}"

# Name the write tools we are refusing, so the omission is visibly deliberate.
write_tools="$(grep -E '^(create_|update_|add_|alerting_manage_)' <<<"$available" | tr '\n' ' ' || true)"
[[ -n "$write_tools" ]] && skip "write tools excluded: $write_tools"

tools_json="$(printf '%s\n' "${selected[@]}" | python3 -c '
import json, sys
print(json.dumps([line.strip() for line in sys.stdin if line.strip()]))
')"

# ---------------------------------------------------------------------------
echo
info "Registering at account level"
# ---------------------------------------------------------------------------

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
' "$GRAFANA_MCP_ENDPOINT" || true)"

if [[ -n "$existing_service_id" ]]; then
  service_id="$existing_service_id"
  skip "reusing registration $service_id"
else
  : "${GRAFANA_TOKEN:?set GRAFANA_TOKEN (a Grafana service account token, Viewer role is enough)}"

  service_details="$(python3 -c '
import json, sys
name, endpoint, token, mode, header = sys.argv[1:6]
if mode == "bearer":
    authorization = {"bearerToken": {"tokenName": name, "tokenValue": token}}
else:
    authorization = {"apiKey": {"apiKeyName": name, "apiKeyValue": token, "apiKeyHeader": header}}
print(json.dumps({
    "mcpserver": {
        "name": name,
        "endpoint": endpoint,
        "description": "Grafana metrics, logs and dashboards",
        "authorizationConfig": authorization,
    }
}))
' "$GRAFANA_MCP_NAME" "$GRAFANA_MCP_ENDPOINT" "$GRAFANA_TOKEN" "$GRAFANA_AUTH_MODE" "$GRAFANA_API_KEY_HEADER")"

  registration="$(agent register-service \
    --service mcpserver \
    --name "$GRAFANA_MCP_NAME" \
    --service-details "$service_details")"

  service_id="$(json_get "$registration" serviceId)"
  [[ -n "$service_id" ]] || die "no serviceId in response: $registration"
  ok "registered $GRAFANA_MCP_NAME as $service_id"
fi

state_put grafana-service-id "$service_id"

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
  [[ -n "$association_id" ]] && state_put grafana-association-id "$association_id"
  [[ "$status" == "invalid" ]] && warn "status 'invalid' — the service could not validate the connection"
elif grep -qiE 'already|conflict|exists' <<<"$association_output"; then
  skip "already attached — updating the allowlist"
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
    state_put grafana-association-id "$existing_association_id"
    ok "allowlist updated"
  else
    warn "could not locate the existing association to update"
  fi
else
  fail "associate-service failed:"
  printf '%s\n' "$association_output" >&2
  exit 1
fi

echo
info "Grafana MCP wired up"
ok "tools: ${selected[*]}"
