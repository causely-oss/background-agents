#!/usr/bin/env bash
# Exercise the relay end to end: POST a realistic Causely notification to the
# relay endpoint exactly as Causely would, then confirm the relay accepted it.
#
#   tests/send-test-notification.sh [payload.json] [--fresh]
#
# The agent deduplicates on incidentId, which the relay maps from Causely's
# objectId — so re-sending the same fixture folds into the existing investigation
# instead of starting a new one. Pass --fresh to rewrite objectId to a new UUID and
# force a genuinely new investigation. That is what you want when rehearsing.
#
# Also asserts that a bad bearer token is rejected, since a public endpoint
# that ignores its token would be a real problem.

source "$(dirname "${BASH_SOURCE[0]}")/../scripts/lib.sh"

fresh=0
args=()
for arg in "$@"; do
  if [[ "$arg" == "--fresh" ]]; then fresh=1; else args+=("$arg"); fi
done

payload_file="${args[0]:-$REPO_ROOT/tests/fixtures/causely-problem-detected.json}"
[[ -f "$payload_file" ]] || die "payload not found: $payload_file"

if (( fresh )); then
  rewritten="$(mktemp)"
  trap 'rm -f "$rewritten"' EXIT
  python3 - "$payload_file" "$rewritten" <<'PY'
import json, sys, uuid
from datetime import datetime, timezone

payload = json.load(open(sys.argv[1]))
payload["objectId"] = str(uuid.uuid4())
payload["timestamp"] = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"
link = payload.get("link")
if link:
    payload["link"] = link.rsplit("/", 1)[0] + "/" + payload["objectId"]
json.dump(payload, open(sys.argv[2], "w"))
print(payload["objectId"])
PY
  payload_file="$rewritten"
  ok "rewrote objectId to force a new investigation"
fi

function_url="$(require_state relay-endpoint)"
notif_token="$(require_state causely-notif-token)"

info "relay: $function_url"
info "payload: $payload_file"

# --- negative test first: the token must actually be enforced ---------------

echo
info "rejecting an invalid bearer token"
status="$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -X POST "$function_url" \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer definitely-not-the-right-token' \
  --data-binary @"$payload_file")"

if [[ "$status" == "401" ]]; then
  ok "invalid token rejected with 401"
else
  die "expected 401 for a bad token but got $status — the relay is not enforcing auth"
fi

# --- the real call ---------------------------------------------------------

echo
info "sending the notification with the correct token"
response_file="$(mktemp)"
trap 'rm -f "$response_file"' EXIT

status="$(curl -s -o "$response_file" -w '%{http_code}' --max-time 30 -X POST "$function_url" \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${notif_token}" \
  --data-binary @"$payload_file")"

body="$(cat "$response_file")"
echo
info "HTTP $status"
python3 -m json.tool <"$response_file" 2>/dev/null || printf '%s\n' "$body"
echo

case "$status" in
  200)
    if [[ "$(json_get "$body" dryRun)" == "True" ]]; then
      warn "relay is in DRY_RUN mode — payload translated but no investigation started"
      warn "redeploy with DRY_RUN=false to trigger real investigations"
    else
      ok "relay accepted and forwarded the notification"
      ok "agent webhook returned $(json_get "$body" agentStatus)"
      echo
      info "an investigation should now appear in the Agent Space console"
    fi
    ;;
  502)
    fail "the agent webhook rejected the incident"
    warn "most likely the stored webhook url/secret is wrong — re-run 03-deploy-relay.sh"
    warn "isolate the agent side with: tests/send-direct-webhook.sh"
    exit 1
    ;;
  504)
    die "the relay could not reach the agent webhook — check the stored url"
    ;;
  *)
    fail "unexpected status $status"
    warn "check logs: aws logs tail /aws/lambda/${LAMBDA_NAME} --region ${REGION} --since 5m"
    exit 1
    ;;
esac

echo
info "relay logs:"
info "  aws logs tail /aws/lambda/${LAMBDA_NAME} --region ${REGION} --since 5m"
