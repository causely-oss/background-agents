#!/usr/bin/env bash
# Create the two IAM roles AWS DevOps Agent needs. Safe to re-run.
#
#   DevOpsAgentRole-AgentSpace   the agent assumes this to discover and read
#                                resources in the monitored account
#   DevOpsAgentRole-WebappAdmin  backs operator web app sessions
#
# Both trust policies carry aws:SourceAccount and aws:SourceArn conditions for
# confused-deputy protection, as the service requires.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account

SOURCE_ARN_PATTERN="arn:aws:aidevops:${REGION}:${ACCOUNT_ID}:agentspace/*"
RESOURCE_EXPLORER_SLR="arn:aws:iam::${ACCOUNT_ID}:role/aws-service-role/resource-explorer-2.amazonaws.com/AWSServiceRoleForResourceExplorer"

# --- trust policies --------------------------------------------------------

agentspace_trust() {
  cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "aidevops.amazonaws.com" },
      "Action": "sts:AssumeRole",
      "Condition": {
        "StringEquals": { "aws:SourceAccount": "${ACCOUNT_ID}" },
        "ArnLike": { "aws:SourceArn": "${SOURCE_ARN_PATTERN}" }
      }
    }
  ]
}
EOF
}

# The operator app role additionally needs sts:TagSession, because the service
# scopes web app permissions with an AgentSpaceId principal tag.
webapp_trust() {
  cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "aidevops.amazonaws.com" },
      "Action": ["sts:AssumeRole", "sts:TagSession"],
      "Condition": {
        "StringEquals": { "aws:SourceAccount": "${ACCOUNT_ID}" },
        "ArnLike": { "aws:SourceArn": "${SOURCE_ARN_PATTERN}" }
      }
    }
  ]
}
EOF
}

slr_policy() {
  cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "AllowCreateResourceExplorerServiceLinkedRole",
      "Effect": "Allow",
      "Action": ["iam:CreateServiceLinkedRole"],
      "Resource": ["${RESOURCE_EXPLORER_SLR}"]
    }
  ]
}
EOF
}

# --- idempotent role management -------------------------------------------

upsert_role() {
  local role_name="$1" trust_document="$2" description="$3"
  if aws iam get-role --role-name "$role_name" >/dev/null 2>&1; then
    aws iam update-assume-role-policy \
      --role-name "$role_name" \
      --policy-document "$trust_document" >/dev/null
    skip "$role_name exists — trust policy refreshed"
  else
    aws iam create-role \
      --role-name "$role_name" \
      --description "$description" \
      --assume-role-policy-document "$trust_document" >/dev/null
    ok "created $role_name"
  fi
  aws iam get-role --role-name "$role_name" --query Role.Arn --output text
}

attach_managed() {
  local role_name="$1" policy_arn="$2"
  # attach-role-policy is idempotent, but report accurately for the operator.
  if aws iam list-attached-role-policies --role-name "$role_name" \
      --query 'AttachedPolicies[].PolicyArn' --output text 2>/dev/null | grep -qF "$policy_arn"; then
    skip "$(basename "$policy_arn") already attached to $role_name"
  else
    aws iam attach-role-policy --role-name "$role_name" --policy-arn "$policy_arn" >/dev/null
    ok "attached $(basename "$policy_arn") to $role_name"
  fi
}

# --- agent space role ------------------------------------------------------

info "Agent Space role ($ROLE_AGENTSPACE)"
agentspace_arn="$(upsert_role "$ROLE_AGENTSPACE" "$(agentspace_trust)" \
  "Lets AWS DevOps Agent discover and read resources for investigations")"
attach_managed "$ROLE_AGENTSPACE" "arn:aws:iam::aws:policy/AIDevOpsAgentAccessPolicy"

aws iam put-role-policy \
  --role-name "$ROLE_AGENTSPACE" \
  --policy-name AllowCreateServiceLinkedRoles \
  --policy-document "$(slr_policy)" >/dev/null
ok "inline policy AllowCreateServiceLinkedRoles in place"

state_put agentspace-role-arn "$agentspace_arn"
ok "$agentspace_arn"

# --- operator app role -----------------------------------------------------

echo
info "Operator app role ($ROLE_WEBAPP)"
webapp_arn="$(upsert_role "$ROLE_WEBAPP" "$(webapp_trust)" \
  "Backs AWS DevOps Agent operator web app sessions")"
attach_managed "$ROLE_WEBAPP" "arn:aws:iam::aws:policy/AIDevOpsOperatorAppAccessPolicy"

state_put webapp-role-arn "$webapp_arn"
ok "$webapp_arn"

echo
# IAM is eventually consistent; a brand new role can fail to be assumable for a
# few seconds, which surfaces as a confusing error from create-agent-space.
info "roles ready — next: scripts/02-agent-space.sh"
warn "if 02 fails with a trust or assume-role error, wait ~15s and re-run it"
