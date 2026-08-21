#!/usr/bin/env bash
# Build a notification fixture from a real, currently-active Causely diagnosis.
#
#   CAUSELY_CLIENT_ID=... CAUSELY_CLIENT_SECRET=... scripts/make-fixture.sh
#   ... scripts/make-fixture.sh --out tests/fixtures/causely-live.json
#   ... scripts/make-fixture.sh --diagnosis-id <id>
#
# Why this exists: a synthetic fixture describes a problem the tenant does not
# actually have, and the agent correctly refuses to corroborate it — it queries
# Causely, finds nothing, and concludes the alert is bogus. Technically a good
# result, and a completely uninformative test. Driving the flow from a live
# diagnosis id means get_diagnosis_details returns a real causal chain.
#
# Run this shortly before you need it. Diagnoses resolve, and a resolved one puts
# you right back in the "cannot corroborate" hole.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

: "${CAUSELY_CLIENT_ID:?set CAUSELY_CLIENT_ID}"
: "${CAUSELY_CLIENT_SECRET:?set CAUSELY_CLIENT_SECRET}"

out_file="$REPO_ROOT/tests/fixtures/causely-live.json"
diagnosis_id=""
while (( $# )); do
  case "$1" in
    --out) out_file="$2"; shift 2 ;;
    --diagnosis-id) diagnosis_id="$2"; shift 2 ;;
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
info "Fetching active diagnoses"
# ---------------------------------------------------------------------------

if [[ -n "$diagnosis_id" ]]; then
  arguments="$(python3 -c '
import json, sys
print(json.dumps({"diagnosis_id": sys.argv[1]}))
' "$diagnosis_id")"
else
  arguments='{"active_only": true}'
fi

request="$(python3 -c '
import json, sys
print(json.dumps({
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {"name": "get_diagnoses", "arguments": json.loads(sys.argv[1])},
}))
' "$arguments")"

response_file="$(mktemp)"
trap 'rm -f "$response_file"' EXIT
curl -sS --max-time 60 -X POST "$CAUSELY_MCP_ENDPOINT" \
  -H "Authorization: Bearer ${access_token}" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d "$request" >"$response_file" 2>/dev/null || die "get_diagnoses call failed"

# ---------------------------------------------------------------------------
info "Building the fixture"
# ---------------------------------------------------------------------------

python3 - "$response_file" "$out_file" "$CAUSELY_APP_BASE" <<'PY'
import json, re, sys

app_base = sys.argv[3].rstrip("/")
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

diagnoses = payload.get("diagnoses") if isinstance(payload, dict) else None
if not diagnoses:
    sys.exit("no active diagnoses returned — nothing to build a fixture from")

RANK = {"critical": 0, "high": 1, "medium": 2, "minor": 3, "low": 4}
diagnoses.sort(key=lambda d: RANK.get(str(d.get("severity", "")).lower(), 9))
top = diagnoses[0]

entity = top.get("entity") or {}
labels = entity.get("labels") or {}
diagnosis_id = top.get("id")

cluster = labels.get("k8s.cluster.name") or labels.get("causely.ai/cluster") or ""
namespace = labels.get("k8s.namespace.name") or labels.get("causely.ai/namespace") or ""

impacted = [s.get("name") for s in (top.get("impacted_services") or []) if s.get("name")]

# Causely's description is markdown; keep a readable slice rather than all of it.
description = (top.get("description") or "").strip()
body = re.sub(r"^#+ .*$", "", description, flags=re.MULTILINE).strip()
paragraphs = [p.strip() for p in body.split("\n\n") if p.strip()]
summary = paragraphs[0] if paragraphs else (top.get("custom_defect_name") or "Causely diagnosis")
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
    "name": top.get("name") or "Causely diagnosis",
    "objectId": diagnosis_id,
    "object_type": "defect",
    "severity": top.get("severity") or "High",
    "timestamp": top.get("started_at") or "",
    "link": f"{app_base}/diagnoses/{diagnosis_id}",
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

print(f"  diagnosis: {top.get('custom_defect_name') or fixture['name']}")
print(f"  severity:  {fixture['severity']}")
print(f"  entity:    {entity.get('name')} ({cluster}/{namespace})")
print(f"  objectId:  {diagnosis_id}")
print(f"  {len(diagnoses)} active diagnosis/es available")
PY

echo
ok "wrote $out_file"
info "send it with: tests/send-test-notification.sh $out_file"
