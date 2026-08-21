#!/usr/bin/env bash
# Optional: attach a Slack channel so investigation findings are posted there.
#
#   SLACK_CHANNEL_ID=C0123456789 scripts/slack.sh
#
# Slack itself must be registered first through the console, because it uses an
# interactive OAuth install that has no API equivalent:
#
#   Console -> Settings -> Communications -> Register -> authorize the app
#
# This script handles the half that is scriptable: binding the registered
# workspace to a channel for this Agent Space.
#
# Afterwards, invite the agent in Slack:  /invite @AWS DevOps Agent

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account

: "${SLACK_CHANNEL_ID:?set SLACK_CHANNEL_ID (right-click the channel > View channel details)}"

agent_space_id="$(require_state agent-space-id)"

# ---------------------------------------------------------------------------
info "Looking for a registered Slack workspace"
# ---------------------------------------------------------------------------

slack_service="$(agent list-services 2>/dev/null | python3 -c '
import json, sys
data = json.loads(sys.stdin.read() or "{}")
for service in data.get("services") or []:
    if service.get("serviceType") != "slack":
        continue
    details = (service.get("additionalServiceDetails") or {}).get("slack") or {}
    print(json.dumps({
        "serviceId": service.get("serviceId"),
        "teamId": details.get("teamId"),
        "teamName": details.get("teamName"),
    }))
    break
' || true)"

if [[ -z "$slack_service" ]]; then
  fail "no Slack workspace registered"
  warn "register it in the console first: Settings -> Communications -> Register"
  exit 1
fi

service_id="$(json_get "$slack_service" serviceId)"
team_id="$(json_get "$slack_service" teamId)"
team_name="$(json_get "$slack_service" teamName)"

[[ -n "$team_id" ]] || die "the registered Slack service has no teamId"
ok "workspace: ${team_name:-unknown} ($team_id)"

# ---------------------------------------------------------------------------
echo
info "Binding channel $SLACK_CHANNEL_ID to Agent Space $agent_space_id"
# ---------------------------------------------------------------------------

configuration="$(python3 -c '
import json, sys
workspace_id, workspace_name, channel_id = sys.argv[1:4]
print(json.dumps({
    "slack": {
        "workspaceId": workspace_id,
        "workspaceName": workspace_name or workspace_id,
        "transmissionTarget": {"opsOncallTarget": {"channelId": channel_id}},
    }
}))
' "$team_id" "$team_name" "$SLACK_CHANNEL_ID")"

if association_output="$(agent associate-service \
    --agent-space-id "$agent_space_id" \
    --service-id "$service_id" \
    --configuration "$configuration" 2>&1)"; then
  association_id="$(json_get "$association_output" association.associationId)"
  ok "attached (association $association_id)"
  [[ -n "$association_id" ]] && state_put slack-association-id "$association_id"
elif grep -qiE 'already|conflict|exists' <<<"$association_output"; then
  skip "Slack already attached to this Agent Space"
else
  fail "associate-service failed:"
  printf '%s\n' "$association_output" >&2
  exit 1
fi

echo
info "Slack wired up"
warn "the agent cannot post until it is invited — run this in #${SLACK_CHANNEL_ID}:"
warn "  /invite @AWS DevOps Agent"
