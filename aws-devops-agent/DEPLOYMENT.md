# Deploying AWS DevOps Agent with Causely as its causal layer

A from-scratch walkthrough: an Agent Space, Causely registered as an MCP capability provider, and a
signed-webhook relay so a Causely diagnosis can start an investigation with nobody watching.

If you already have all of this and just want to see what the flows look like, skip to
[`WALKTHROUGH.md`](WALKTHROUGH.md).

## What you end up with

```
  Causely notification (Critical/High only)
    │  Authorization: Bearer <notif token>
    ▼
  API Gateway HTTP API ──> relay Lambda ──> DevOps Agent webhook (HMAC-signed)
                                              │
                                              ▼
                                        investigation
                                          ├─ Causely MCP: get_diagnosis_details(objectId)
                                          ├─ use_kubectl / use_aws (read-only)
                                          └─ Root Cause + mitigation plan ──> Slack
```

Runtime footprint in your account: one Lambda, one HTTP API, two Secrets Manager secrets, three IAM
roles, and one EKS access entry. Everything else is AWS-managed Agent Space state.

## Prerequisites

- **AWS CLI v2, 2.36 or newer.** Older builds have no DevOps Agent commands at all.
- `python3`, `curl`, `zip`, `openssl`.
- AWS credentials for an account you can create IAM roles in, and a region where DevOps Agent is
  available (`us-east-1` is the default here and gets features first).
- **OAuth client credentials for a Causely tenant** — a client id and secret with the
  `client_credentials` grant.
- Optional, for later steps: an EKS cluster, a Grafana MCP endpoint, a Slack workspace where you can
  approve an app install.

Work top to bottom. Each step is self-contained, every script is idempotent, and resolved ids are
cached in `.state/` so you can stop and resume anywhere.

## ☐ 1. Upgrade the AWS CLI

- [ ] Check what you have:

  ```bash
  aws --version
  aws devops-agent 2>&1 | head -1   # "Invalid choice" means it is too old
  ```

- [ ] If it is too old, install the latest v2 without touching the system copy:

  ```bash
  cd /tmp
  curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o awscliv2.zip
  unzip -q awscliv2.zip
  ./aws/install --install-dir "$HOME/.local/aws-cli" --bin-dir "$HOME/.local/bin" --update
  hash -r && aws --version
  ```

  Make sure `$HOME/.local/bin` precedes `/usr/local/bin` on `PATH`, or update the system install
  with `sudo ./aws/install --update` instead.

The AWS user guide documents `aws devops-agent`; an AWS blog post uses `aws devopsagent`; the
service endpoint is `aidevops`. `lib.sh` probes for whichever your CLI actually exposes and caches
the answer, so this resolves itself — override with `DEVOPS_AGENT_CLI` if detection picks wrong.

## ☐ 2. Causely MCP credentials

The MCP server authenticates with OAuth. DevOps Agent needs the `client_credentials` grant, since
it runs unattended and cannot complete a browser flow.

- [ ] Mint a client id and secret for your tenant.
- [ ] Export them:

  ```bash
  export CAUSELY_CLIENT_ID=...
  export CAUSELY_CLIENT_SECRET=...
  ```

- [ ] If your tenant is not on `https://api.causely.app/mcp`, set `CAUSELY_MCP_ENDPOINT` too. The
      OAuth token endpoint is derived from it.

> Note: any older `X-Causely-Client-Basic` credential is unrelated and will not work here — that
> header is rejected with `401 invalid_client`.

## ☐ 3. Preflight

- [ ] Run it:

  ```bash
  scripts/00-preflight.sh --fix
  ```

Preflight verifies every prerequisite, then exchanges your Causely credentials for a token and calls
`tools/list` against the MCP endpoint. **Both must pass before step 6** — registration validates the
connection on AWS's side, so a failure here predicts a failure there.

`--fix` promotes the Resource Explorer index in your region to `AGGREGATOR`. This matters more than
it looks: cross-region topology discovery goes through Resource Explorer, and without an aggregator
index the agent sees nothing outside its own region. If your clusters live elsewhere they are simply
invisible, silently and with no error. Aggregation takes a few minutes to populate.

## ☐ 4. IAM roles

- [ ] Run it:

  ```bash
  scripts/01-iam-roles.sh
  ```

Creates `DevOpsAgentRole-AgentSpace` (managed policy `AIDevOpsAgentAccessPolicy`, plus permission to
create the Resource Explorer service-linked role) and `DevOpsAgentRole-WebappAdmin`
(`AIDevOpsOperatorAppAccessPolicy`).

Both trust policies carry `aws:SourceAccount` and `aws:SourceArn` conditions for confused-deputy
protection. The web app role additionally needs `sts:TagSession`, because the service scopes web app
permissions with an `AgentSpaceId` principal tag.

## ☐ 5. Agent Space

- [ ] Run it:

  ```bash
  scripts/02-agent-space.sh
  ```

Creates the Agent Space, associates the account as `monitor`, and enables the operator web app with
IAM auth. (IAM auth is right for getting started; production would use IAM Identity Center.)

**Topology discovery runs asynchronously.** Start it now and work through the remaining steps while
it populates.

If this fails with a trust or assume-role error, IAM has not propagated yet. Wait ~15 seconds and
re-run — the script is idempotent.

## ☐ 6. Register the Causely MCP server

- [ ] With the credentials from step 2 still exported:

  ```bash
  scripts/03-causely-mcp.sh
  ```

Registers the server at account level as the generic `mcpserver` type with OAuth client
credentials, then attaches it to the Agent Space with an explicit tool allowlist.

The allowlist is read-only and investigation-relevant — `DESIRED_TOOLS` at the top of the script:

```
get_diagnoses                     get_symptoms             get_slo
get_diagnosis_details             get_service_summary      get_logs
get_diagnosis_observable_signals  get_entity_health        get_metrics
get_issues                        get_environment_health   get_events
get_issue_details                 get_topology             name_lookup
get_incident_impact               get_entities             investigate_alert
postmortem
```

It deliberately does not allow everything. `submit_feedback` and `generate_ticket` write back into
Causely, and the bulk analytics tools burn the per-space tool quota without helping either flow. AWS
is explicit that only read-only tools should be allowlisted and that MCP output is a prompt-injection
surface; keeping the Causely credentials read-only is the other half of that mitigation.

The script asks the server what it advertises via `tools/list` first and intersects that with the
desired list, so a renamed or retired tool surfaces as a warning rather than a silent gap later.

- [ ] Note that tools reach the agent **prefixed with the server name**: `get_diagnosis_details`
      appears as `Causely_get_diagnosis_details`. The 64-character tool-name limit applies to the
      prefixed form, so keep `MCP_NAME` short.

Re-running with an edited `DESIRED_TOOLS` updates the allowlist in place. Rotating credentials does
not work that way — there is no in-place credential update, so you deregister and register again.

> The console equivalent is **Capability Providers → MCP Server → Register**, then Agent Space →
> **Capabilities** → **MCP Servers** → **Add**. Causely also exposes `/mcp/oauth/register`, so the
> console's **Dynamic Client Registration** option works if minting credentials by hand is
> inconvenient.

## ☐ 7. Create the inbound webhook

- [ ] Run it:

  ```bash
  scripts/04-webhook.sh
  ```

Registers an `eventChannel` service and associates it. The response carries both the webhook URL and
its HMAC secret, and the script writes the secret straight into Secrets Manager.

**The secret is returned exactly once and no API reads it back.** If it is lost, the only recovery
is to disassociate and mint a new webhook. The script says as much if it finds a webhook whose
secret was never stored.

## ☐ 8. Deploy the relay

- [ ] Run it — no arguments; it picks up what step 7 stored:

  ```bash
  scripts/05-deploy-relay.sh
  ```

  Pass `AGENT_WEBHOOK_URL` and `AGENT_WEBHOOK_SECRET` only to point the relay at a webhook created
  some other way. Add `DRY_RUN=true` to deploy in translate-and-log mode, which checks the mapping
  without starting billable investigations.

- [ ] Save the two values it prints — the relay endpoint and the bearer token Causely must present.
      Both are cached in `.state/`, and the token is also in Secrets Manager.

The function is Python 3.12, 256 MB, 30-second timeout, packaged with **no dependencies** — stdlib
and boto3 only. Secrets live in Secrets Manager rather than Lambda environment variables; the exec
role's inline policy is scoped to exactly the two secret ARNs it reads.

Two things that cost real debugging time, both handled by the script:

**Why API Gateway and not a Lambda Function URL.** A Function URL would be one fewer moving part,
but some accounts deny anonymous invocation outright: with `AuthType: NONE` and a correct
`Principal: "*"` resource policy in place, every request still returns `403 AccessDeniedException`
from Lambda's own auth layer, with no invocation recorded in the function's logs. An HTTP API is
reliably reachable, forwards the `Authorization` header untouched, and the function still enforces
the bearer token itself.

**`create-api --target` does not add the invoke permission.** Quick-create builds the integration,
route, and stage, but not `lambda:InvokeFunction` for API Gateway. Without it every request returns
a bare `500` and nothing appears in the Lambda logs. `05-deploy-relay.sh` adds it explicitly.

## ☐ 9. Point Causely at the relay

Either route works. The UI is simpler; the Secret is reproducible.

- [ ] **Via the Causely UI** — Settings → Notifications, create a notification of type **Generic**:

  | Field | Value |
  |---|---|
  | Type | `Generic` |
  | URL | the relay endpoint from step 8 (`.state/relay-endpoint`) |
  | Token | `Bearer <token from .state/causely-notif-token>` |

- [ ] **Or via the Mediator**, if you prefer declaring it in the cluster — edit and apply
      [`examples/causely-notification-secret.yaml`](examples/causely-notification-secret.yaml):

  ```bash
  kubectl apply -f examples/causely-notification-secret.yaml
  ```

- [ ] Set a severity filter. Every notification starts a billable investigation, so restrict to what
      is worth waking an agent for:

  ```yaml
  notif_config_filters_enabled: "true"
  notif_config_filters: |
    [{"field": "severity", "operator": "in", "value": ["Critical", "High"]}]
  ```

**Include the `Bearer ` prefix.** Causely sends the configured token verbatim as the `Authorization`
header, so the scheme has to come from you. The relay does accept a bare token as well — that
misconfiguration would otherwise show up only as a silent 401 and a missing investigation — but
`Bearer <token>` is the correct form.

## ☐ 10. Verify

Three rungs, in this order, so a failure tells you *where* the problem is:

- [ ] **Translation and signing, no AWS involved:**

  ```bash
  python3 tests/test_transform.py
  ```

- [ ] **The agent webhook, url + secret + signing only:**

  ```bash
  tests/send-direct-webhook.sh
  ```

- [ ] **The full relay, including a deliberate bad-token check:**

  ```bash
  tests/send-test-notification.sh
  ```

  This one sends a knowingly wrong token first and aborts unless it gets a 401.

- [ ] **Then with a real diagnosis**, which is the only version that proves anything:

  ```bash
  scripts/make-fixture.sh
  tests/send-test-notification.sh tests/fixtures/causely-live.json
  ```

  `make-fixture.sh` fetches the most severe currently-active diagnosis and writes a fixture with its
  real `objectId`, so `get_diagnosis_details` returns an actual causal chain. Re-run it shortly
  before you need it; diagnoses resolve.

- [ ] **Confirm Causely was actually consulted** — do not take the investigation's prose for it:

  ```bash
  scripts/show-tool-calls.sh
  ```

The last two rungs start real investigations, billed by agent-second. The agent deduplicates on
`incidentId` (mapped from Causely's `objectId`), so re-sending the same fixture folds into the
existing investigation — pass `--fresh` to force a new one.

- [ ] Finally, use **Send Test Notification** in Causely and confirm an investigation appears. If
      nothing does, the relay logs say why:

  ```bash
  aws logs tail /aws/lambda/causely-devops-agent-relay --region us-east-1 --since 10m
  ```

  A `401` there means the token does not match; no log lines at all means the request never arrived.

## ☐ 11. Optional: read-only cluster access

Investigations will tell you when they hit this wall: without kubectl the agent cannot inspect
ConfigMaps, Helm release metadata, or workload specs, which is exactly where the "what changed" half
of a root cause usually lives.

- [ ] Grant a read-only access entry:

  ```bash
  EKS_CLUSTER=<your-cluster> EKS_CLUSTER_REGION=<region> scripts/eks-access.sh
  ```

`AmazonEKSViewPolicy` at cluster scope maps to Kubernetes' built-in `view` ClusterRole: read-only
across namespaces, and it **excludes Secrets**. A cluster in `API` or `API_AND_CONFIG_MAP` auth mode
needs no `aws-auth` ConfigMap edit; the script checks and tells you if yours is in `CONFIG_MAP` mode.
`--revoke` removes the entry.

The cluster name Causely displays is not necessarily the EKS cluster name — it comes from however
the cluster identified itself at install time. If you are unsure of the mapping, resolve one of the
cluster's ingress hostnames to its load balancer and read the balancer's
`kubernetes.io/cluster/<name>` tag.

## ☐ 12. Optional: Grafana MCP

Grafana complements Causely rather than duplicating it: Causely supplies the causal chain, Grafana
supplies the raw PromQL/LogQL underneath it.

- [ ] **First, make sure your endpoint requires authentication.** DevOps Agent probes it during
      registration and refuses anything that answers anonymously:

  > `ValidationException: MCP Server at '...' does not have any authorization configured.
  > Authorization is required for remote MCP Servers to ensure secure communication.`

  A default `mcp-grafana` deployment carries the service-account token in its own environment and so
  answers anyone who can reach it. Make it require a per-request credential instead — either remove
  that token so it reads `X-Grafana-API-Key` from the caller, or put a gateway auth policy in front
  of it. This is worth doing regardless of this integration.

- [ ] Then register it:

  ```bash
  GRAFANA_MCP_ENDPOINT=https://<host>/mcp GRAFANA_TOKEN=<service account token> \
    scripts/07-grafana-mcp.sh
  ```

  Viewer role is enough. Use `GRAFANA_AUTH_MODE=bearer` if your server expects
  `Authorization: Bearer` instead of `X-Grafana-API-Key`.

The script probes the endpoint before calling AWS and explains rather than failing opaquely. It
allowlists 17 read-only tools and names every write tool it excludes, so the omission is visibly
deliberate.

## ☐ 13. Optional: Slack

Registering the Slack app is an interactive OAuth install with no API equivalent, so that half is
console-only.

- [ ] Console → **Settings** → **Communications** → **Register**, and authorize the AWS DevOps Agent
      Slack app. This needs workspace admin approval.
- [ ] Bind the channel:

  ```bash
  SLACK_CHANNEL_ID=C0123456789 scripts/slack.sh
  ```

- [ ] In the channel itself: `/invite @AWS DevOps Agent`.

**Do not skip the invite.** Without it the agent cannot post, and the association alone will look
deceptively healthy.

## Troubleshooting quick reference

| Symptom | Likely cause / fix |
|---|---|
| `aws devops-agent` → "Invalid choice" | CLI too old — step 1 |
| `02-agent-space.sh` fails on trust / assume-role | IAM has not propagated; wait ~15s and re-run |
| Agent sees no resources outside its own region | No Resource Explorer `AGGREGATOR` index — `scripts/00-preflight.sh --fix` |
| MCP registration rejected as unauthorized | The endpoint answers anonymously — step 12 |
| No investigation appears | Notification never arrived — `aws logs tail /aws/lambda/causely-devops-agent-relay --since 10m` |
| Relay returns 401 | Token mismatch; compare with `.state/causely-notif-token`, and check the `Bearer ` prefix |
| Relay returns 502 | Webhook url/secret wrong — `tests/send-direct-webhook.sh` isolates the agent side |
| Relay returns 500, no Lambda logs | API Gateway cannot invoke the function — re-run `scripts/05-deploy-relay.sh` |
| Investigation runs but never calls Causely | Tools not allowlisted, or MCP auth expired — Agent Space → Capabilities → MCP Servers |
| MCP calls fail mid-investigation | Credentials rotated; there is no in-place update — deregister and re-register |
| Agent answers from CloudWatch only | No reason to prefer Causely — add a custom skill (see [`WALKTHROUGH.md`](WALKTHROUGH.md)) |
| Agent concludes the alert cannot be corroborated | Synthetic or stale fixture — `scripts/make-fixture.sh` |
| Slack association healthy but nothing posts | The agent was never `/invite`d to the channel |

## Tearing down

```bash
source scripts/lib.sh
AGENT_SPACE_ID="$(cat .state/agent-space-id)"

# Deleting the agent space also removes its associations and investigation history.
aws "$(cat .state/agent-cli)" delete-agent-space --agent-space-id "$AGENT_SPACE_ID" --region "$REGION"

aws apigatewayv2 delete-api --api-id "$(cat .state/relay-api-id)" --region "$REGION"
aws lambda delete-function --function-name "$LAMBDA_NAME" --region "$REGION"
aws secretsmanager delete-secret --secret-id "$SECRET_WEBHOOK" --force-delete-without-recovery --region "$REGION"
aws secretsmanager delete-secret --secret-id "$SECRET_INBOUND" --force-delete-without-recovery --region "$REGION"

for role in "$ROLE_RELAY" "$ROLE_AGENTSPACE" "$ROLE_WEBAPP"; do
  for policy in $(aws iam list-attached-role-policies --role-name "$role" --query 'AttachedPolicies[].PolicyArn' --output text); do
    aws iam detach-role-policy --role-name "$role" --policy-arn "$policy"
  done
  for policy in $(aws iam list-role-policies --role-name "$role" --query 'PolicyNames[]' --output text); do
    aws iam delete-role-policy --role-name "$role" --policy-name "$policy"
  done
  aws iam delete-role --role-name "$role"
done
```

Deleting the Agent Space removes its associations, so afterwards deregister the account-level
services that outlive it:

```bash
CLI="$(cat .state/agent-cli)"
for id in $(cat .state/causely-service-id .state/webhook-service-id 2>/dev/null); do
  aws "$CLI" deregister-service --service-id "$id" --region "$REGION"
done
```

Credential rotation follows the same path — deregister, then register again.
