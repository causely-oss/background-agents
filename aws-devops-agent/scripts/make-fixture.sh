#!/usr/bin/env bash
# Build a notification fixture from a real, currently-active Causely Issue.
#
#   CAUSELY_CLIENT_ID=... CAUSELY_CLIENT_SECRET=... scripts/make-fixture.sh
#   ... scripts/make-fixture.sh --out tests/fixtures/causely-live.json
#   ... scripts/make-fixture.sh --issue-id <id>
#
# Why this exists: a synthetic fixture describes a problem the tenant does not
# actually have, and the agent correctly refuses to corroborate it — it queries
# Causely, finds nothing, and concludes the alert is bogus. Technically a good
# result, and a completely uninformative test. Driving the flow from a live
# Issue id means get_issue_details returns a real causal chain.
#
# Run this shortly before you need it. Issues resolve, and a resolved one puts
# you right back in the "cannot corroborate" hole.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

: "${CAUSELY_CLIENT_ID:?set CAUSELY_CLIENT_ID}"
: "${CAUSELY_CLIENT_SECRET:?set CAUSELY_CLIENT_SECRET}"

out_file="$REPO_ROOT/tests/fixtures/causely-live.json"
issue_id=""
while (( $# )); do
  case "$1" in
    --out) out_file="$2"; shift 2 ;;
    --issue-id) issue_id="$2"; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

# ---------------------------------------------------------------------------
info "Authenticating to $CAUSELY_MCP_ENDPOINT"
# ---------------------------------------------------------------------------

token_response="$(curl -sS --max-time 15 -X POST "$CAUSELY_TOKEN_ENDPOINT" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode "grant_type=client_credentials" \
  --data-urlencode "client_id=${CAUSELY_CLIENT_ID}" \
  --data-urlencode "client_secret=${CAUSELY_CLIENT_SECRET}" 2>/dev/null || true)"

access_token="$(json_get "$token_response" access_token)"
[[ -n "$access_token" ]] || die "token exchange failed: ${token_response:0:200}"
ok "authenticated"

# ---------------------------------------------------------------------------
info "Fetching active issues"
# ---------------------------------------------------------------------------

# --issue-id filters the returned list rather than being passed as a tool
# argument, so this does not depend on the tool's filter parameter names.
request="$(python3 -c '
import json
print(json.dumps({
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {"name": "get_issues", "arguments": {"active_only": True}},
}))
')"

response_file="$(mktemp)"
trap 'rm -f "$response_file"' EXIT
curl -sS --max-time 60 -X POST "$CAUSELY_MCP_ENDPOINT" \
  -H "Authorization: Bearer ${access_token}" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d "$request" >"$response_file" 2>/dev/null || die "get_issues call failed"

# ---------------------------------------------------------------------------
info "Building the fixture"
# ---------------------------------------------------------------------------

python3 - "$response_file" "$out_file" "$CAUSELY_APP_BASE" "$issue_id" <<'PY'
import json, re, sys

app_base = sys.argv[3].rstrip("/")
wanted_id = sys.argv[4] if len(sys.argv) > 4 else ""
raw = open(sys.argv[1]).read()

# Streamable HTTP may answer as SSE. Take the last data: frame, else the whole body.
frames = re.findall(r"^data:\s*(\{.*\})\s*$", raw, re.MULTILINE)
try:
    envelope = json.loads(frames[-1] if frames else raw)
except json.JSONDecodeError:
    sys.exit(f"could not parse the MCP response: {raw[:300]}")

if envelope.get("error"):
    sys.exit(f"MCP error: {envelope['error']}")

# tools/call results arrive as content blocks holding JSON text.
payload = None
for block in (envelope.get("result") or {}).get("content") or []:
    if block.get("type") == "text":
        try:
            payload = json.loads(block.get("text") or "")
            break
        except json.JSONDecodeError:
            continue
if payload is None:
    payload = (envelope.get("result") or {}).get("structuredContent") or {}

# Prefer Issues, the incident-level view. Fall back to a diagnoses key so this
# keeps working if the tool's response shape differs, and record which it was —
# the relay uses object_type to pick the tool it points the agent at.
records, object_type = None, "issue"
if isinstance(payload, dict):
    records = payload.get("issues")
    if not records:
        records = payload.get("diagnoses")
        if records:
            object_type = "defect"
if not records:
    sys.exit("no active issues returned — nothing to build a fixture from")

if wanted_id:
    records = [r for r in records if str(r.get("id")) == wanted_id]
    if not records:
        sys.exit(f"no active issue with id {wanted_id}")

RANK = {"critical": 0, "high": 1, "medium": 2, "minor": 3, "low": 4}
records.sort(key=lambda d: RANK.get(str(d.get("severity", "")).lower(), 9))
top = records[0]

entity = top.get("entity") or {}
labels = entity.get("labels") or {}
object_id = top.get("id")

cluster = labels.get("k8s.cluster.name") or labels.get("causely.ai/cluster") or ""
namespace = labels.get("k8s.namespace.name") or labels.get("causely.ai/namespace") or ""

impacted = [s.get("name") for s in (top.get("impacted_services") or []) if s.get("name")]

# Causely's description is markdown; keep a readable slice rather than all of it.
description = (top.get("description") or "").strip()
body = re.sub(r"^#+ .*$", "", description, flags=re.MULTILINE).strip()
paragraphs = [p.strip() for p in body.split("\n\n") if p.strip()]
summary = paragraphs[0] if paragraphs else (top.get("custom_defect_name") or "Causely issue")
details = "\n\n".join(paragraphs[1:])[:2000] or summary
if impacted:
    details += "\n\nImpacted services: " + ", ".join(impacted)

remediation = (top.get("remediation") or "").strip()
options = []
if remediation:
    # Strip emphasis and code fences but never underscores — they carry meaning in
    # identifiers like STRIPE_URL and SPRING_DATASOURCE_URL.
    def unmark(text):
        return re.sub(r"[*`]", "", text).strip()

    lines = remediation.split("\n")
    title = unmark(lines[0]) or "Suggested remediation"
    rest = re.sub(r"```.*?```", "", "\n".join(lines[1:]), flags=re.S)
    detail = unmark(rest) or title
    options.append({"title": title[:200], "description": detail[:1200]})

fixture = {
    "type": "ProblemDetected",
    "name": top.get("name") or "Causely issue",
    "objectId": object_id,
    "object_type": object_type,
    "severity": top.get("severity") or "High",
    "timestamp": top.get("started_at") or "",
    "link": f"{app_base}/{'issues' if object_type == 'issue' else 'diagnoses'}/{object_id}",
    "entity": {
        "id": entity.get("id"),
        "name": entity.get("name"),
        "type": entity.get("type"),
        "link": f"{app_base}/topology/{entity.get('id')}",
    },
    "labels": {
        "k8s.cluster.name": cluster,
        "k8s.namespace.name": namespace,
        "causely.ai/cluster": cluster,
        "causely.ai/namespace": namespace,
    },
    "description": {"summary": summary, "details": details, "remediationOptions": options},
}
fixture = {k: v for k, v in fixture.items() if v not in (None, "")}

with open(sys.argv[2], "w") as handle:
    json.dump(fixture, handle, indent=2)
    handle.write("\n")

print(f"  {object_type}: {top.get('custom_defect_name') or fixture['name']}")
print(f"  severity:  {fixture['severity']}")
print(f"  entity:    {entity.get('name')} ({cluster}/{namespace})")
print(f"  objectId:  {object_id}")
print(f"  {len(records)} active {object_type}(s) available")
PY

echo
ok "wrote $out_file"
info "send it with: tests/send-test-notification.sh $out_file"
