#!/usr/bin/env bash
# Grant the Agent Space role read-only kubectl access to an EKS cluster.
#
#   EKS_CLUSTER=my-cluster EKS_CLUSTER_REGION=us-east-2 scripts/eks-access.sh
#   EKS_CLUSTER=my-cluster scripts/eks-access.sh --revoke
#
# Worth doing. Without kubectl the agent cannot inspect ConfigMaps, Helm release
# metadata, or workload specs, which is exactly where the "what changed" half of
# a root cause usually lives — investigations say so themselves when it is missing.
#
# The cluster name Causely displays is not necessarily the EKS cluster name; it
# comes from however the cluster identified itself at install time. If you are not
# sure of the mapping, resolve one of the cluster's ingress hostnames to its load
# balancer and read the balancer's kubernetes.io/cluster/<name> tag, or match on
# the cluster's OIDC issuer.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account

: "${EKS_CLUSTER:?set EKS_CLUSTER to the EKS cluster name (see the header of this script)}"

role_arn="$(state_get agentspace-role-arn)"
[[ -n "$role_arn" ]] || role_arn="arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_AGENTSPACE}"

# The `view` ClusterRole equivalent: read-only across namespaces, excludes Secrets.
POLICY_ARN="arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"

if [[ "${1:-}" == "--revoke" ]]; then
  info "Revoking access to $EKS_CLUSTER"
  aws eks delete-access-entry --cluster-name "$EKS_CLUSTER" --region "$EKS_CLUSTER_REGION" \
    --principal-arn "$role_arn" >/dev/null 2>&1 &&
    ok "access entry removed" || skip "no access entry to remove"
  exit 0
fi

info "Cluster $EKS_CLUSTER ($EKS_CLUSTER_REGION)"

auth_mode="$(aws eks describe-cluster --name "$EKS_CLUSTER" --region "$EKS_CLUSTER_REGION" \
  --query 'cluster.accessConfig.authenticationMode' --output text 2>/dev/null || true)"
[[ -n "$auth_mode" && "$auth_mode" != "None" ]] || die "cannot describe cluster $EKS_CLUSTER"
ok "authentication mode: $auth_mode"

# Access entries require the API to be an accepted auth source.
if [[ "$auth_mode" == "CONFIG_MAP" ]]; then
  fail "cluster only accepts the aws-auth ConfigMap; access entries are unavailable"
  warn "switch it with: aws eks update-cluster-config --name $EKS_CLUSTER \\"
  warn "  --region $EKS_CLUSTER_REGION --access-config authenticationMode=API_AND_CONFIG_MAP"
  exit 1
fi

info "Principal: $role_arn"

if aws eks describe-access-entry --cluster-name "$EKS_CLUSTER" --region "$EKS_CLUSTER_REGION" \
    --principal-arn "$role_arn" >/dev/null 2>&1; then
  skip "access entry already exists"
else
  aws eks create-access-entry --cluster-name "$EKS_CLUSTER" --region "$EKS_CLUSTER_REGION" \
    --principal-arn "$role_arn" --type STANDARD \
    --tags Purpose=aws-devops-agent-investigations >/dev/null
  ok "access entry created"
fi

# associate-access-policy is idempotent for an already-associated policy+scope.
aws eks associate-access-policy --cluster-name "$EKS_CLUSTER" --region "$EKS_CLUSTER_REGION" \
  --principal-arn "$role_arn" --access-scope type=cluster \
  --policy-arn "$POLICY_ARN" >/dev/null
ok "AmazonEKSViewPolicy associated at cluster scope (read-only, excludes Secrets)"

echo
info "Verifying"
aws eks list-associated-access-policies --cluster-name "$EKS_CLUSTER" \
  --region "$EKS_CLUSTER_REGION" --principal-arn "$role_arn" \
  --query 'associatedAccessPolicies[].{policy:policyArn,scope:accessScope.type}' --output table 2>&1

echo
info "the agent can now use kubectl against $EKS_CLUSTER during investigations"
info "confirm on the next run with: scripts/show-tool-calls.sh"
