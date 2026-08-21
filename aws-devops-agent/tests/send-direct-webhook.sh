#!/usr/bin/env bash
# Post a hand-signed incident straight to the DevOps Agent webhook, bypassing the
# relay entirely. This isolates "is the webhook url + secret + signature scheme
# correct?" from "is the Lambda working?" — run it first when the relay reports 502.
#
#   AGENT_WEBHOOK_URL=... AGENT_WEBHOOK_SECRET=... tests/send-direct-webhook.sh
#
# Falls back to the values stored in Secrets Manager when the environment is unset.

source "$(dirname "${BASH_SOURCE[0]}")/../scripts/lib.sh"

require_cmd openssl

if [[ -z "${AGENT_WEBHOOK_URL:-}" || -z "${AGENT_WEBHOOK_SECRET:-}" ]]; then
  info "reading the webhook config from $SECRET_WEBHOOK"
  stored="$(aws secretsmanager get-secret-value --secret-id "$SECRET_WEBHOOK" \
    --query SecretString --output text --region "$REGION" 2>/dev/null || true)"
  [[ -n "$stored" && "$stored" != "None" ]] ||
    die "no stored webhook config; set AGENT_WEBHOOK_URL and AGENT_WEBHOOK_SECRET"
  AGENT_WEBHOOK_URL="$(json_get "$stored" url)"
  AGENT_WEBHOOK_SECRET="$(json_get "$stored" secret)"
fi

[[ -n "$AGENT_WEBHOOK_URL" && -n "$AGENT_WEBHOOK_SECRET" ]] ||
  die "webhook url or secret is empty"

timestamp="$(python3 -c '
from datetime import datetime, timezone
print(datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z")')"

# Compact separators, matching what the relay sends and signs.
payload="$(python3 -c '
import json, sys
print(json.dumps({
    "eventType": "incident",
    "incidentId": "manual-webhook-probe",
    "action": "created",
    "priority": "LOW",
    "title": "Manual webhook connectivity probe",
    "description": (
        "Synthetic incident sent by tests/send-direct-webhook.sh to verify the "
        "webhook url, HMAC secret, and signature scheme. Safe to close."
    ),
    "timestamp": sys.argv[1],
    "service": "webhook-probe",
    "data": {"source": "manual-test"},
}, separators=(",", ":")))
' "$timestamp")"

# The agent signs "<timestamp>:<exact request body>" with HMAC-SHA256, base64.
signature="$(printf '%s' "${timestamp}:${payload}" |
  openssl dgst -sha256 -hmac "$AGENT_WEBHOOK_SECRET" -binary | base64)"

info "POST $AGENT_WEBHOOK_URL"
info "timestamp: $timestamp"

response_file="$(mktemp)"
trap 'rm -f "$response_file"' EXIT

status="$(curl -s -o "$response_file" -w '%{http_code}' --max-time 30 -X POST "$AGENT_WEBHOOK_URL" \
  -H 'Content-Type: application/json' \
  -H "x-amzn-event-timestamp: ${timestamp}" \
  -H "x-amzn-event-signature: ${signature}" \
  --data-binary "$payload")"

echo
info "HTTP $status"
python3 -m json.tool <"$response_file" 2>/dev/null || cat "$response_file"
echo

if [[ "$status" =~ ^2 ]]; then
  ok "the agent accepted the signed payload — url, secret, and signing are correct"
  info "a low-priority investigation should appear in the console shortly"
else
  fail "the agent rejected the payload (HTTP $status)"
  warn "401/403 means the secret or signature is wrong; the secret is shown only once,"
  warn "so if it was not captured, generate a new webhook and re-run 03-deploy-relay.sh"
  exit 1
fi
