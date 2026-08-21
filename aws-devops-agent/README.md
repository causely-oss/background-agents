# AWS DevOps Agent + Causely MCP

[AWS DevOps Agent](https://aws.amazon.com/devops-agent/) is Amazon's managed agentic SRE service:
you create an *Agent Space*, associate an AWS account with it, and the service discovers your
topology and runs autonomous investigations — in its own cloud, on its own model, billed by
agent-second. It accepts remote MCP servers as capability providers. Point it at Causely and it
stops re-deriving root cause from CloudWatch metrics and starts reading a causal chain that has
already been computed.

This example wires the two together in both directions:

- **Proactive** — Causely detects a root cause and fires an Issue notification. A small relay
  translates it into the agent's incident schema, carrying the Issue id across, and signs it. The
  agent opens an investigation, calls back into Causely MCP with that id, walks the causal chain,
  and produces a root cause and a staged mitigation plan. No human in the loop.
- **Reactive** — an engineer asks the agent a plain-language question in its operator web app, and
  the agent reaches into the same Causely MCP tools to answer.

The point of both is that the agent never has to rediscover causality. Causely supplies it; the
agent acts on it.

**There is no agent code here.** No Agent SDK, no Bedrock call, no model id, no system prompt —
the reasoning is AWS's managed service. What this directory contains is the plumbing (a
dependency-free relay Lambda), the guardrail policy (a read-only MCP tool allowlist), and idempotent
setup scripts. That is the interesting part of the claim: remote MCP over Streamable HTTP plus a
signed webhook is standard plumbing, and the same wiring works with any MCP-capable agent.

**New to this?** [`DEPLOYMENT.md`](DEPLOYMENT.md) is the full from-scratch walkthrough. This page is
the overview; [`WALKTHROUGH.md`](WALKTHROUGH.md) shows what the two flows actually look like once
they run.

## What's here

| File | Purpose |
|------|---------|
| [`scripts/`](scripts) | Idempotent setup, `00`–`07` in order. Re-running updates in place; resolved ids cache in `.state/` so each script stands alone |
| [`scripts/lib.sh`](scripts/lib.sh) | Shared config — every default is env-overridable, and the DevOps Agent CLI namespace is auto-detected |
| [`scripts/03-causely-mcp.sh`](scripts/03-causely-mcp.sh) | Registers Causely MCP and attaches it with the read-only tool allowlist (`DESIRED_TOOLS` at the top) |
| [`lambda/causely_relay/handler.py`](lambda/causely_relay/handler.py) | The relay: authenticate, translate, HMAC-sign, forward. Stdlib and boto3 only, nothing vendored |
| [`tests/test_transform.py`](tests/test_transform.py) | 29 unit tests over the translation and signing. No AWS needed |
| [`tests/fixtures/`](tests/fixtures) | Causely notification payloads for exercising the relay |
| [`examples/causely-notification-secret.yaml`](examples/causely-notification-secret.yaml) | The Causely side of the trigger, as an applyable Mediator Secret |
| [`scripts/show-tool-calls.sh`](scripts/show-tool-calls.sh) | Which tools an investigation actually used — the honest check that Causely was consulted |

## How it fits together

```
PROACTIVE
  Causely
    │  notification, Authorization: Bearer <notif token>
    ▼
  API Gateway HTTP API ─── validate token
                      ├─── translate payload to the agent's incident schema
                      ├─── carry objectId + portal link into data
                      └─── sign HMAC-SHA256 over "<timestamp>:<body>"
    │  x-amzn-event-signature
    ▼
  DevOps Agent webhook ──> investigation
                            └─> Causely MCP get_issue_details(issue_id)
                            └─> Root Cause tab + mitigation plan ──> Slack

REACTIVE
  Engineer ──> Operator Web App chat ──> Causely MCP tools ──> answer
```

The relay exists because Causely forwards its own notification payload verbatim with no templating,
while the agent webhook accepts only its own `eventType: incident` schema. Neither side can produce
the other's shape, so a small translation step is unavoidable. Beyond reformatting, it carries
Causely's `objectId` and `object_type` through into the incident's `data`, along with an
`investigationHint` naming the exact tool that resolves them — `get_issue_details` for an Issue,
`get_diagnosis_details` for a single defect. That one string is what turns a generic alert into an
investigation that starts from a known cause.

## Quickstart

Prerequisites: AWS CLI **v2 2.36+** (older builds have no `devops-agent` commands), `python3`,
`curl`, `zip`, `openssl`, credentials for an AWS account you can create IAM roles in, and OAuth
client credentials for a Causely tenant. Full detail in [`DEPLOYMENT.md`](DEPLOYMENT.md).

```bash
export CAUSELY_CLIENT_ID=...  CAUSELY_CLIENT_SECRET=...

scripts/00-preflight.sh --fix   # check everything; --fix promotes the Resource Explorer index
scripts/01-iam-roles.sh         # two service roles, with confused-deputy conditions
scripts/02-agent-space.sh       # agent space, account association, operator web app
scripts/03-causely-mcp.sh       # register Causely MCP + allowlist its read-only tools
scripts/04-webhook.sh           # inbound webhook; stores its HMAC secret in Secrets Manager
scripts/05-deploy-relay.sh      # exec role, Lambda, API Gateway ingress
scripts/06-verify.sh            # read-only status report
```

Then point Causely at the relay endpoint that `05` prints — Settings → Notifications, type
`Generic` — or apply [`examples/causely-notification-secret.yaml`](examples/causely-notification-secret.yaml).

Almost everything is scriptable through the `devops-agent` API, including MCP registration and
webhook creation. Only two things genuinely require the console, because both are interactive OAuth
installs: Slack and GitHub.

**The webhook secret is returned exactly once**, at creation, and no API reads it back. `04` writes
it straight to Secrets Manager. If it is lost the only recovery is to disassociate and mint a new
webhook.

**Send Issues, not defects.** An Issue groups the related diagnoses for an affected entity into one
incident with a designated primary diagnosis — the right granularity for a single investigation. A
defect is one finding beneath it. Set the object type on the notification accordingly; the relay
handles either, and `object_type` on the payload decides which Causely tool it points the agent at.

**Use a real Issue, not the synthetic fixture.** `tests/fixtures/causely-problem-detected.json`
describes a problem your tenant does not have, so the agent queries Causely, finds nothing, and
concludes at length that the alert cannot be corroborated. That is the agent behaving correctly and
telling you nothing. `scripts/make-fixture.sh` builds a fixture from a live Issue instead.

## What the agent is allowed to do

The safety model here is **capability-based, not approval-based**. There is no human gate on the
proactive path — a notification starts an investigation with nobody watching. That is safe because
the agent cannot write anywhere: it produces a mitigation plan a human executes, never the change
itself.

| Surface | Grant | Excluded |
|---|---|---|
| Causely MCP | 19 read-only tools, explicitly allowlisted | `submit_feedback`, `generate_ticket` (both write back), bulk analytics (burn the per-space tool quota) |
| Kubernetes | `AmazonEKSViewPolicy` at cluster scope — the built-in `view` ClusterRole | Secrets, and every write verb |
| AWS | `AIDevOpsAgentAccessPolicy` | — |
| Grafana (optional) | 17 read-only tools | all 8 write tools, named individually in `07-grafana-mcp.sh` |

Two details worth knowing. First, the allowlist is *intersected* with what the server actually
advertises rather than asserted blindly — `03-causely-mcp.sh` calls `tools/list` first, so a renamed
or retired Causely tool shows up as a warning instead of a silent gap. Second, AWS is explicit that
MCP output is a prompt-injection surface; an allowlist alone is half a mitigation, and keeping the
Causely credentials themselves read-only is the other half.

Tools reach the agent **prefixed with the MCP server name** — `get_issue_details` arrives as
`Causely_get_issue_details`. The 64-character tool-name limit applies to the prefixed form, so a
long `MCP_NAME` eats into that budget.

## Verify

The falsifiable check is whether the agent actually consulted Causely, and you should not take the
investigation's prose for it:

```bash
scripts/show-tool-calls.sh
```

It reads the execution journal and prints per-tool call counts, tagging the Causely ones — and says
plainly when the agent used only its built-in tools. If Causely was not consulted, check the MCP
association before anything else, then consider adding a custom skill (a Markdown runbook authored
in the operator console) telling the agent to consult Causely first for Kubernetes and
application-layer causality. Tool selection left to chance is the most common reason this looks
wired but behaves like it isn't.

Before involving Causely at all, three rungs in this order — a failure then tells you *where* the
problem is:

```bash
python3 tests/test_transform.py        # translation and signing logic, no AWS
tests/send-direct-webhook.sh           # webhook url + secret + signing only
tests/send-test-notification.sh        # the full relay, including a deliberate 401 check
```

The last two start real investigations, which are billed. `send-test-notification.sh` sends a
knowingly wrong token first and aborts unless it gets a 401 — a public endpoint that ignores its
token would be a real problem.

## Costs

Investigations bill at **$0.0083 per agent-second**, so an eight-minute investigation is roughly
$4. New accounts get a two-month trial covering 10 agent spaces and 20 hours of investigations per
month. Do not leave a webhook firing in a loop unattended.

The severity filter in the notification config is the practical cost guardrail — without it a noisy
afternoon of Low-severity issues becomes a noisy AWS bill:

```yaml
notif_config_filters_enabled: "true"
notif_config_filters: |
  [{"field": "severity", "operator": "in", "value": ["Critical", "High"]}]
```

`DRY_RUN=true` on `05-deploy-relay.sh` deploys in translate-and-log mode, which exercises the
mapping without starting anything billable.

## Tearing down

See [`DEPLOYMENT.md`](DEPLOYMENT.md#tearing-down). Deleting the Agent Space also removes its
associations and investigation history, so the account-level services that outlive it have to be
deregistered afterwards.

## License

Apache-2.0. See [LICENSE](../LICENSE).
