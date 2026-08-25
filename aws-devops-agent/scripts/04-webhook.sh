#!/usr/bin/env bash
# Create the inbound webhook that external systems use to trigger investigations,
# and store its url + HMAC secret in Secrets Manager.
#
# The webhook belongs to an "eventChannel" service association. The secret is
# returned exactly once, in the associate-service response — there is no API to
# read it back later, so this script captures it immediately. If it is ever lost,
# delete the association and re-run to mint a new one.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account

agent_space_id="$(require_state agent-space-id)"
WEBHOOK_NAME="${WEBHOOK_NAME:-CauselyRelayChannel}"

# ---------------------------------------------------------------------------
info "Registering the event channel"
# ---------------------------------------------------------------------------

existing_service_id="$(agent list-services 2>/dev/null | python3 -c '
import json, sys
data = json.loads(sys.stdin.read() or "{}")
for service in data.get("services") or []:
    if service.get("serviceType") == "eventChannel":
        print(service["serviceId"])
        break
' || true)"

if [[ -n "$existing_service_id" ]]; then
  service_id="$existing_service_id"
  skip "reusing event channel $service_id"
else
  registration="$(agent register-service \
    --service eventChannel \
    --name "$WEBHOOK_NAME" \
    --service-details '{"eventChannel":{"type":"webhook"}}')"
  service_id="$(json_get "$registration" serviceId)"
  [[ -n "$service_id" ]] || die "no serviceId in register-service response: $registration"
  ok "registered event channel $service_id"
fi

state_put webhook-service-id "$service_id"

# ---------------------------------------------------------------------------
echo
info "Associating it with Agent Space $agent_space_id"
# ---------------------------------------------------------------------------

webhook_url=""
webhook_secret=""

if association_output="$(agent associate-service \
    --agent-space-id "$agent_space_id" \
    --service-id "$service_id" \
    --configuration '{"eventChannel":{}}' 2>&1)"; then
  webhook_url="$(json_get "$association_output" webhook.webhookUrl)"
  webhook_secret="$(json_get "$association_output" webhook.webhookSecret)"
  webhook_type="$(json_get "$association_output" webhook.webhookType)"
  association_id="$(json_get "$association_output" association.associationId)"
  [[ -n "$association_id" ]] && state_put webhook-association-id "$association_id"
  ok "created webhook (type ${webhook_type:-unknown})"
elif grep -qiE 'already|conflict|exists' <<<"$association_output"; then
  # The url is retrievable, the secret is not. Without the secret the relay cannot
  # sign, so recreating the association is the only way forward.
  skip "event channel already associated"
  association_id="$(state_get webhook-association-id)"
  if [[ -n "$association_id" ]]; then
    webhook_url="$(agent list-webhooks \
      --agent-space-id "$agent_space_id" \
      --association-id "$association_id" 2>/dev/null |
      python3 -c '
import json, sys
data = json.loads(sys.stdin.read() or "{}")
webhooks = data.get("webhooks") or []
print(webhooks[0].get("webhookUrl", "") if webhooks else "")
' || true)"
  fi
  if aws secretsmanager get-secret-value --secret-id "$SECRET_WEBHOOK" \
      --region "$REGION" >/dev/null 2>&1; then
    ok "existing webhook secret already stored in $SECRET_WEBHOOK — nothing to do"
    [[ -n "$webhook_url" ]] && ok "url: $webhook_url"
    echo
    info "next: scripts/05-deploy-relay.sh"
    exit 0
  fi
  fail "the webhook exists but its secret was never stored, and it cannot be read back"
  warn "disassociate and re-run to mint a fresh webhook:"
  warn "  aws $(agent_cli) disassociate-service --agent-space-id $agent_space_id \\"
  warn "    --association-id ${association_id:-<association-id>} --region $REGION"
  exit 1
else
  fail "associate-service failed:"
  printf '%s\n' "$association_output" >&2
  exit 1
fi

[[ -n "$webhook_url" ]] || die "no webhookUrl in the response — cannot continue"
[[ -n "$webhook_secret" ]] || die "no webhookSecret in the response — cannot sign requests without it"

# ---------------------------------------------------------------------------
echo
info "Storing the webhook config in Secrets Manager"
# ---------------------------------------------------------------------------

# url and secret travel together so the relay needs a single lookup.
secret_json="$(python3 -c '
import json, sys
print(json.dumps({"url": sys.argv[1], "secret": sys.argv[2]}))
' "$webhook_url" "$webhook_secret")"

if aws secretsmanager describe-secret --secret-id "$SECRET_WEBHOOK" --region "$REGION" >/dev/null 2>&1; then
  aws secretsmanager put-secret-value \
    --secret-id "$SECRET_WEBHOOK" --secret-string "$secret_json" --region "$REGION" >/dev/null
  skip "updated $SECRET_WEBHOOK"
else
  aws secretsmanager create-secret \
    --name "$SECRET_WEBHOOK" \
    --description "DevOps Agent webhook url + HMAC secret for the Causely relay" \
    --secret-string "$secret_json" --region "$REGION" >/dev/null
  ok "created $SECRET_WEBHOOK"
fi

echo
info "Webhook ready"
ok "url: $webhook_url"
ok "secret stored in $SECRET_WEBHOOK (not printed; it cannot be retrieved from the API again)"
echo
info "verify the agent side on its own: tests/send-direct-webhook.sh"
info "next: scripts/05-deploy-relay.sh"
