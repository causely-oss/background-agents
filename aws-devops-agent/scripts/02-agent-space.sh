#!/usr/bin/env bash
# Create the Agent Space, associate this AWS account for topology discovery, and
# enable the operator web app. Safe to re-run: an existing space with the same
# name is reused rather than duplicated.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account

agentspace_role_arn="$(require_state agentspace-role-arn)"
webapp_role_arn="$(require_state webapp-role-arn)"
cli="$(agent_cli)"
info "using: aws $cli (region $REGION)"

# ---------------------------------------------------------------------------
info "Agent Space '$AGENT_SPACE_NAME'"
# ---------------------------------------------------------------------------

# Field names are read defensively: the id may surface as agentSpaceId or id
# depending on CLI version, and the list key as agentSpaces or agentSpaceSummaries.
find_existing() {
  local listing
  listing="$(agent list-agent-spaces 2>/dev/null || echo '{}')"
  python3 -c '
import json, sys
data = json.loads(sys.argv[1] or "{}")
wanted = sys.argv[2]
items = []
for key, value in data.items():
    if isinstance(value, list):
        items = value
        break
for item in items:
    if not isinstance(item, dict):
        continue
    if item.get("name") == wanted:
        for key in ("agentSpaceId", "id", "agentSpaceArn"):
            if item.get(key):
                print(item[key])
                raise SystemExit(0)
' "$listing" "$AGENT_SPACE_NAME"
}

agent_space_id="$(find_existing || true)"

if [[ -n "$agent_space_id" ]]; then
  skip "reusing existing Agent Space: $agent_space_id"
else
  info "creating Agent Space"
  created="$(agent create-agent-space \
    --name "$AGENT_SPACE_NAME" \
    --description "$AGENT_SPACE_DESCRIPTION")"
  agent_space_id="$(python3 -c '
import json, sys
data = json.loads(sys.argv[1] or "{}")
space = data.get("agentSpace") or data
for key in ("agentSpaceId", "id"):
    if space.get(key):
        print(space[key])
        break
' "$created")"
  [[ -n "$agent_space_id" ]] || die "could not read agentSpaceId from create-agent-space response: $created"
  ok "created Agent Space $agent_space_id"
fi

state_put agent-space-id "$agent_space_id"

# ---------------------------------------------------------------------------
echo
info "Associating AWS account $ACCOUNT_ID for topology discovery"
# ---------------------------------------------------------------------------

# accountType "monitor" marks the primary account that hosts the Agent Space and
# is the one used for discovery. Discovery covers every region in the account, so
# this single association also reaches the us-east-2 EKS clusters.
aws_configuration=$(cat <<EOF
{
  "aws": {
    "assumableRoleArn": "${agentspace_role_arn}",
    "accountId": "${ACCOUNT_ID}",
    "accountType": "monitor"
  }
}
EOF
)

if association_output="$(agent associate-service \
    --agent-space-id "$agent_space_id" \
    --service-id aws \
    --configuration "$aws_configuration" 2>&1)"; then
  ok "AWS account associated"
else
  if grep -qiE 'already|conflict|exists' <<<"$association_output"; then
    skip "AWS account already associated"
  else
    fail "associate-service failed:"
    printf '%s\n' "$association_output" >&2
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
echo
info "Enabling the operator web app (IAM auth)"
# ---------------------------------------------------------------------------

# IAM auth is right for a demo; production would use IAM Identity Center (--auth-flow idc).
if operator_output="$(agent enable-operator-app \
    --agent-space-id "$agent_space_id" \
    --auth-flow iam \
    --operator-app-role-arn "$webapp_role_arn" 2>&1)"; then
  ok "operator app enabled"
else
  if grep -qiE 'already|conflict|enabled' <<<"$operator_output"; then
    skip "operator app already enabled"
  else
    fail "enable-operator-app failed:"
    printf '%s\n' "$operator_output" >&2
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
echo
info "Agent Space ready"
ok "id: $agent_space_id"
ok "console: https://${REGION}.console.aws.amazon.com/aidevops/home?region=${REGION}"
echo
warn "topology discovery runs asynchronously and takes several minutes to populate"
info "next: scripts/03-causely-mcp.sh (needs CAUSELY_CLIENT_ID and CAUSELY_CLIENT_SECRET)"
