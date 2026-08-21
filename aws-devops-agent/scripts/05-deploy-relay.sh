#!/usr/bin/env bash
# Deploy the Causely -> DevOps Agent webhook relay Lambda.
#
# Normally needs no arguments: scripts/04-webhook.sh has already stored the agent
# webhook url and secret in Secrets Manager. Override them explicitly only when
# pointing the relay at a webhook created some other way:
#
#   AGENT_WEBHOOK_URL, AGENT_WEBHOOK_SECRET
#
# Optional:
#   CAUSELY_NOTIF_TOKEN   bearer token Causely must present; generated if unset
#   DRY_RUN=true          deploy in translate-and-log mode (starts no investigations)
#
# Safe to re-run: updates code, configuration, and secrets in place.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account
require_cmd zip

# The webhook config is normally already in Secrets Manager, courtesy of 04.
webhook_already_stored=0
if aws secretsmanager get-secret-value --secret-id "$SECRET_WEBHOOK" \
    --region "$REGION" >/dev/null 2>&1; then
  webhook_already_stored=1
fi

if (( webhook_already_stored )) && [[ -z "${AGENT_WEBHOOK_URL:-}" ]]; then
  skip "using the webhook config already stored in $SECRET_WEBHOOK"
else
  : "${AGENT_WEBHOOK_URL:?no stored webhook config — run scripts/04-webhook.sh, or set AGENT_WEBHOOK_URL}"
  : "${AGENT_WEBHOOK_SECRET:?set AGENT_WEBHOOK_SECRET as well (it cannot be read back from the API)}"
fi

# The token Causely will send as "Authorization: Bearer <token>".
if [[ -z "${CAUSELY_NOTIF_TOKEN:-}" ]]; then
  existing="$(aws secretsmanager get-secret-value --secret-id "$SECRET_INBOUND" \
    --query SecretString --output text --region "$REGION" 2>/dev/null || true)"
  if [[ -n "$existing" && "$existing" != "None" ]]; then
    CAUSELY_NOTIF_TOKEN="$existing"
    skip "reusing the existing inbound token from $SECRET_INBOUND"
  else
    CAUSELY_NOTIF_TOKEN="$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
    ok "generated a new inbound token"
  fi
fi

# ---------------------------------------------------------------------------
info "Secrets"
# ---------------------------------------------------------------------------

put_secret() {
  local secret_id="$1" value="$2" description="$3"
  if aws secretsmanager describe-secret --secret-id "$secret_id" --region "$REGION" >/dev/null 2>&1; then
    aws secretsmanager put-secret-value \
      --secret-id "$secret_id" --secret-string "$value" --region "$REGION" >/dev/null
    skip "updated $secret_id"
  else
    aws secretsmanager create-secret \
      --name "$secret_id" --description "$description" \
      --secret-string "$value" --region "$REGION" >/dev/null
    ok "created $secret_id"
  fi
  aws secretsmanager describe-secret --secret-id "$secret_id" --region "$REGION" \
    --query ARN --output text
}

if [[ -n "${AGENT_WEBHOOK_URL:-}" ]]; then
  # url and secret travel together so the Lambda needs a single lookup
  webhook_secret_json="$(python3 -c '
import json, sys
print(json.dumps({"url": sys.argv[1], "secret": sys.argv[2]}))
' "$AGENT_WEBHOOK_URL" "$AGENT_WEBHOOK_SECRET")"
  webhook_secret_arn="$(put_secret "$SECRET_WEBHOOK" "$webhook_secret_json" \
    "DevOps Agent webhook url + HMAC secret for the Causely relay")"
else
  webhook_secret_arn="$(aws secretsmanager describe-secret --secret-id "$SECRET_WEBHOOK" \
    --region "$REGION" --query ARN --output text)"
  skip "left $SECRET_WEBHOOK untouched"
fi
inbound_secret_arn="$(put_secret "$SECRET_INBOUND" "$CAUSELY_NOTIF_TOKEN" \
  "Bearer token Causely presents to the relay endpoint")"

# ---------------------------------------------------------------------------
echo
info "Execution role ($ROLE_RELAY)"
# ---------------------------------------------------------------------------

lambda_trust='{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "lambda.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }
  ]
}'

if aws iam get-role --role-name "$ROLE_RELAY" >/dev/null 2>&1; then
  skip "$ROLE_RELAY exists"
else
  aws iam create-role --role-name "$ROLE_RELAY" \
    --description "Execution role for the Causely -> DevOps Agent webhook relay" \
    --assume-role-policy-document "$lambda_trust" >/dev/null
  ok "created $ROLE_RELAY"
fi

aws iam attach-role-policy --role-name "$ROLE_RELAY" \
  --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole >/dev/null
ok "CloudWatch Logs permissions attached"

# Scoped to exactly the two secrets this function reads.
read_secrets_policy="$(python3 -c '
import json, sys
print(json.dumps({
    "Version": "2012-10-17",
    "Statement": [{
        "Sid": "ReadRelaySecrets",
        "Effect": "Allow",
        "Action": ["secretsmanager:GetSecretValue"],
        "Resource": sys.argv[1:],
    }],
}))
' "$webhook_secret_arn" "$inbound_secret_arn")"

aws iam put-role-policy --role-name "$ROLE_RELAY" \
  --policy-name ReadRelaySecrets --policy-document "$read_secrets_policy" >/dev/null
ok "secret read permissions scoped to the two relay secrets"

relay_role_arn="$(aws iam get-role --role-name "$ROLE_RELAY" --query Role.Arn --output text)"
state_put relay-role-arn "$relay_role_arn"

# ---------------------------------------------------------------------------
echo
info "Packaging"
# ---------------------------------------------------------------------------

rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"
cp "$REPO_ROOT/lambda/causely_relay/handler.py" "$BUILD_DIR/"
(cd "$BUILD_DIR" && zip -q relay.zip handler.py)
ok "built $BUILD_DIR/relay.zip (stdlib + boto3 only, nothing vendored)"

# ---------------------------------------------------------------------------
echo
info "Lambda function ($LAMBDA_NAME)"
# ---------------------------------------------------------------------------

lambda_env="Variables={WEBHOOK_SECRET_ID=${SECRET_WEBHOOK},INBOUND_SECRET_ID=${SECRET_INBOUND},DRY_RUN=${DRY_RUN:-false},LOG_LEVEL=INFO}"

if aws lambda get-function --function-name "$LAMBDA_NAME" --region "$REGION" >/dev/null 2>&1; then
  aws lambda update-function-code \
    --function-name "$LAMBDA_NAME" \
    --zip-file "fileb://$BUILD_DIR/relay.zip" \
    --region "$REGION" >/dev/null
  aws lambda wait function-updated --function-name "$LAMBDA_NAME" --region "$REGION"
  aws lambda update-function-configuration \
    --function-name "$LAMBDA_NAME" \
    --role "$relay_role_arn" \
    --timeout 30 --memory-size 256 \
    --environment "$lambda_env" \
    --region "$REGION" >/dev/null
  aws lambda wait function-updated --function-name "$LAMBDA_NAME" --region "$REGION"
  skip "updated $LAMBDA_NAME"
else
  # A freshly created role may not be assumable yet; retry briefly.
  for attempt in 1 2 3 4 5; do
    if aws lambda create-function \
        --function-name "$LAMBDA_NAME" \
        --runtime python3.12 \
        --handler handler.lambda_handler \
        --role "$relay_role_arn" \
        --zip-file "fileb://$BUILD_DIR/relay.zip" \
        --timeout 30 --memory-size 256 \
        --environment "$lambda_env" \
        --description "Translates Causely notifications into DevOps Agent incidents" \
        --region "$REGION" >/dev/null 2>&1; then
      ok "created $LAMBDA_NAME"
      break
    fi
    [[ $attempt -eq 5 ]] && die "could not create $LAMBDA_NAME (role propagation?)"
    warn "create failed (attempt $attempt/5) — waiting for IAM propagation"
    sleep 6
  done
  aws lambda wait function-active-v2 --function-name "$LAMBDA_NAME" --region "$REGION"
fi

# ---------------------------------------------------------------------------
echo
info "Ingress (API Gateway HTTP API)"
# ---------------------------------------------------------------------------

# A Lambda Function URL would be simpler, but anonymous invocation of Function URLs
# is denied in this account even with a correct `Principal: "*"` resource policy —
# every request comes back 403 from Lambda's own auth layer. An HTTP API is
# publicly reachable, forwards the Authorization header untouched, and the function
# still enforces the bearer token itself.
lambda_arn="$(aws lambda get-function --function-name "$LAMBDA_NAME" \
  --region "$REGION" --query Configuration.FunctionArn --output text)"

api_id="$(aws apigatewayv2 get-apis --region "$REGION" \
  --query "Items[?Name=='${RELAY_API_NAME}'].ApiId | [0]" --output text 2>/dev/null || true)"

if [[ -n "$api_id" && "$api_id" != "None" ]]; then
  skip "reusing HTTP API $api_id"
  api_endpoint="$(aws apigatewayv2 get-api --api-id "$api_id" --region "$REGION" \
    --query ApiEndpoint --output text)"
else
  # Quick-create: builds the proxy integration, a $default route, and a $default stage.
  created_api="$(aws apigatewayv2 create-api \
    --name "$RELAY_API_NAME" \
    --protocol-type HTTP \
    --target "$lambda_arn" \
    --region "$REGION")"
  api_id="$(json_get "$created_api" ApiId)"
  api_endpoint="$(json_get "$created_api" ApiEndpoint)"
  ok "created HTTP API $api_id"
fi

# Quick-create does *not* add the invoke permission, so without this every request
# returns a bare 500 from API Gateway with nothing in the Lambda logs.
aws lambda add-permission \
  --function-name "$LAMBDA_NAME" \
  --statement-id AllowApiGatewayInvoke \
  --action lambda:InvokeFunction \
  --principal apigateway.amazonaws.com \
  --source-arn "arn:aws:execute-api:${REGION}:${ACCOUNT_ID}:${api_id}/*/*" \
  --region "$REGION" >/dev/null 2>&1 && ok "granted API Gateway invoke permission" ||
  skip "invoke permission already present"

relay_endpoint="${api_endpoint%/}"
state_put relay-endpoint "$relay_endpoint"
state_put relay-api-id "$api_id"
state_put causely-notif-token "$CAUSELY_NOTIF_TOKEN"

# ---------------------------------------------------------------------------
echo
info "Relay deployed"
ok "Endpoint:     $relay_endpoint"
ok "Bearer token: $CAUSELY_NOTIF_TOKEN"
[[ "${DRY_RUN:-false}" == "true" ]] && warn "DRY_RUN=true — the relay will translate and log but not start investigations"
echo
info "Configure Causely staging to POST notifications to that endpoint with that bearer token,"
info "then verify with: tests/send-test-notification.sh"
