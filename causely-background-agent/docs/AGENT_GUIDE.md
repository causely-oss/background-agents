# causely-background-agent — Guide

Audience: any human or agent that needs to understand, operate, or deploy
`causely-background-agent` without reading the Go source first. For design
rationale and known limitations, `README.md` in the repo root is the other
primary reference — this doc goes deeper on the three questions that come up
most: what it is, how it's wired end-to-end, and how to deploy it.

---

## 1. What is causely-background-agent

A standalone Go service that turns a Causely-detected Kubernetes root cause
into either an immediate remediation recommendation or a GitHub pull request
with a code-level fix. It is a **reference implementation** on top of
Causely's MCP server — not a required part of using Causely, and not
Causely's own product surface (that's the MCP server itself). Anyone could
build a different agent against the same MCP tools; this one exists as a
working example and as Causely's own dogfood instance.

At a high level, one investigation run does:

1. Receive a trigger (see §2) identifying a root cause + entity.
2. Load tool lists from every configured MCP server (Causely's, plus any
   extras like Grafana).
3. Run a Claude tool-use loop (`agent.go: runClaudeLoop`) where Claude calls
   MCP tools (root cause detail, topology, logs, metrics) and GitHub
   read-only tools (`read_file`, `list_directory`, `search_code`) to build a
   picture of the incident.
4. Claude terminates the loop by calling exactly one of two tools:
   - `recommend_remediation` — an immediate action (restart, rollback, scale,
     revert a config). No code change, no PR.
   - `propose_fix` — a structured PR (title, body, exact search/replace file
     changes) for a genuine code-level bug that an immediate action can't fix.
5. Depending on **action mode** (§ below), the agent either just records what
   it would do, or actually posts to Slack / opens the PR.
6. Regardless of outcome, one `InvestigationRecord` (§ below) is appended to
   a JSON-lines file — the durable dataset behind "how well is this actually
   working."

### Action modes

| Mode | PR opened? | Slack posted? | Investigation runs? | When to use |
|---|---|---|---|---|
| `observe` (default) | No | No | Yes, in full | Evaluating a new config, or Causely's own root-cause quality, without side effects |
| `act` | Yes | Yes | Yes, in full | Once you trust the configuration and want real remediation |

Set via `action_mode` in `config.yaml`. There is no per-severity or
per-namespace override today — it's one setting for the whole instance. In
both modes the investigation itself (MCP calls, GitHub reads, Claude cost) is
identical; only what happens with the result differs. This means `observe`
mode still costs real Anthropic API spend — it is a dry run of the *actions*,
not a free simulation of the whole pipeline.

### Trigger sources — how they differ in configuration and output

All three produce the same internal `TriggerPayload` (`main.go`) and feed the
same investigation path and the same `InvestigationRecord`. They differ in
where the data comes from and what metadata is available downstream:

| Trigger | Enabled by | Root-cause data source | Slack thread info? | Notes |
|---|---|---|---|---|
| **Webhook** (`POST /trigger`) | Always on; secured by `TRIGGER_SHARED_SECRET` | Causely mediator's existing `NotificationPayload` (`notification_payload.go`) — the same shape Slack/Teams destinations already get | No — mediator's generic-webhook delivery is separate from its Slack-specific delivery, so `SlackChannel`/`SlackThreadTS` are empty | Wiring this up is a **mediator config change, not a code change** — see §2 |
| **Poll** | `poll.enabled: true` in `config.yaml` | Calls Causely MCP's `get_issues` tool directly on an interval | No (no mediator involved) | Tracks a per-root-cause watermark (`poll.go`) so an unchanged, still-open issue isn't re-investigated every cycle; a severity shift, symptom-count change, or new occurrence does re-trigger |
| **Slack `/slack/actions`** | Always on; verified by `SLACK_SIGNING_SECRET` if set | The Slack "Fix it" button's `value` payload (round-tripped from whatever mediator put there) | Yes — this is the one path with real channel + thread, so `act`-mode replies thread correctly | Requires a Slack app configured to POST interactive payloads here |

Practical implication: if you want `act`-mode Slack replies threaded to the
original alert, use the Slack button path, not the webhook path.

### Investigation records

Every run — skipped (out of scope, budget exceeded), failed, or completed —
appends one JSON line to `investigation_record_path` (`investigation_record.go`).
Fields worth knowing:

- **Identity**: `root_cause_id`, `entity_id`, `entity_name`, `root_cause_name`, `severity`
- **Provenance**: `trigger_source` (`webhook` | `poll` | `slack_action`), `action_mode`
- **Cost**: `input_tokens`, `output_tokens`, `cost_usd`
- **Shape**: `tool_calls` (server + tool name + error flag, per call), `tool_call_count`
- **Outcome**: `verdict` (`skipped_scope` | `skipped_budget` | `fix_proposed` | `remediation_recommended` | `failed`), `skip_reason`, `summary`, `remediation`, `pr_url`, `error`
- **Human-filled later**: `correctness_label`, `correctness_notes` — the agent
  never sets these; they exist so a human (or synthetic-bug ground truth) can
  label a run after the fact and turn the JSONL file into an evaluation set.

This is a plain append-only file, not a database — designed to be queried
with `jq`/grep/a notebook, not a service.

### Cost controls

Two independent caps, both in `agent.go`/`cost.go`:

- **Per-investigation** (`max_cost_usd`, default $5): checked every loop
  iteration inside `runClaudeLoop`; exceeding it aborts the investigation
  (partial findings are preserved in the record's `summary`).
- **Weekly aggregate** (`max_weekly_cost_usd`, default $50): a rolling
  7-day window (not calendar-week) across *all* investigations this process
  runs, checked *before* starting a new investigation so a recurring root
  cause can't blow the budget one under-cap incident at a time. Persisted to
  `cost_state_file` if set, so a pod restart doesn't reset the counter.

---

## 2. End-to-end workflow — mediator, Claude, GitHub, Slack

```
Causely mediator                 causely-background-agent                  external services
─────────────────                ─────────────────────────                  ─────────────────
root cause detected
  │
  ├─ Slack/Teams notify  (existing, unrelated to this agent)
  │
  └─ "causelybot" webhook ──POST /trigger──▶ validTriggerAuth()
       (Bearer TRIGGER_SHARED_SECRET)              │
                                                    ▼
                                          notification.toTriggerPayload()
                                                    │
                                                    ▼
                                              go runAgent(...)
                                                    │
                              ┌─────────────────────┼─────────────────────────┐
                              │ inScope()?           weekly.exceeded()?        │
                              └─────────────────────┬─────────────────────────┘
                                                     ▼
                                          loadMCPSources(cfg)  ──tools/list──▶  Causely MCP server (+ extras, e.g. Grafana)
                                                     │
                                                     ▼
                                          runClaudeLoop(...)
                                            │
                                            ├──POST /v1/messages──▶ Anthropic API (claude-sonnet-4-6)
                                            │◀──tool_use blocks────┘
                                            │
                                            ├──tools/call──▶ Causely/extra MCP servers  (evidence gathering)
                                            ├──GET contents/search──▶ GitHub API        (read_file, list_directory, search_code)
                                            │
                                            └──terminates on recommend_remediation OR propose_fix
                                                     │
                              ┌──────────────────────┴──────────────────────┐
                              ▼ (observe mode: stop here, just record)       ▼ (act mode)
                                                                   ┌─────────┴─────────┐
                                                                   ▼                   ▼
                                                        gh.CreatePR(...)      slack.PostToThread(...)
                                                        ──POST /pulls──▶       ──POST chat.postMessage──▶
                                                          GitHub (branch,        Slack
                                                          commit, PR)
                                                     │
                                                     ▼
                                          rec.record(InvestigationRecord)  ──append──▶  investigation_record_path (JSONL)
```

### Mediator → agent wiring

This is purely a **mediator-side notification-config change** — no code
change to mediator, and no code change to this agent. Point a
`causelybot`-type notification destination at this service's `/trigger` URL.
On mediator's deployment, that typically means environment variables such as:

```
NOTIFICATION_CAUSELY_AGENT_TYPE=causelybot
NOTIFICATION_CAUSELY_AGENT_URL=http://causely-background-agent:8090/trigger
NOTIFICATION_CAUSELY_AGENT_TOKEN=<same value as this agent's TRIGGER_SHARED_SECRET>
```

(Exact env var names live in mediator's own notification-destination config,
not in this repo — confirm current names against mediator's deployed config
before wiring, since this agent's repo doesn't own that contract.)

### Claude interaction

- One Claude conversation per investigation, capped at 20 tool-use
  iterations (`agent.go: runClaudeLoop`), model `claude-sonnet-4-6`
  (`cost.go: claudeModel`).
- Tools presented to Claude = every MCP server's tools (prefixed
  `<server-name>__`, e.g. `causely__get_root_cause`) + 5 built-ins:
  `read_file`, `list_directory`, `search_code`, `propose_fix`,
  `recommend_remediation`.
- The system prompt (`buildSystemPrompt`) explicitly biases Claude toward
  `recommend_remediation` first, and only toward `propose_fix` (which costs a
  PR, code changes, review overhead) when an immediate action can't resolve
  the root cause.
- Model pricing is hardcoded in `cost.go: modelPricing` — needs periodic
  manual re-verification against https://www.anthropic.com/pricing since
  there's no dynamic pricing lookup.

### GitHub interaction

- Read: `GET /repos/{owner}/{repo}/contents/{path}` (files, directories),
  `GET /search/code` scoped to the configured repo.
- Write (`act` mode + `propose_fix` only): creates a branch named
  `causely-fix/<sanitized-root-cause-id>-<unix-ts>`, applies each
  `FileChange` as a single-string search/replace via the Contents API, opens
  a PR against the repo's default branch.
- Dedup: before creating a new PR, `findExistingFixPR` looks for an
  already-open PR whose branch starts with `causely-fix/<root-cause-id>-` —
  if found, new changes are pushed onto that branch instead of opening a
  duplicate PR (a recurring root cause fires this webhook multiple times
  before it's actually resolved).
- One GitHub token (`GITHUB_TOKEN`), one repo (`github_repo` in config) per
  agent instance — this agent cannot fan out writes across multiple repos.
  Multi-repo coverage means multiple deployed instances (see `inScope`
  below).

### Slack interaction

- `act` mode only: `slackClient.PostToThread` (`slack.go`) posts via
  `chat.postMessage`, as user "Causely Background Agent" with a wrench
  emoji, threaded to `SlackThreadTS` when the trigger source provided one
  (only the Slack-button trigger does today — see the table in §1).
- `/slack/actions` verifies Slack's HMAC request signature
  (`slack_actions.go: verifySlackSignature`) if `SLACK_SIGNING_SECRET` is
  set, rejecting requests older than 5 minutes as a replay guard.

### Scope enforcement (multi-instance safety)

`scope.go: inScope` decides whether *this* deployed instance is allowed to
act on a given root cause, since mediator-side routing to the right instance
is best-effort, not a guarantee:

1. If the root cause's entity carries a `causely.ai/github-repo` label, it
   must case-insensitively match this instance's configured `github_repo`.
2. If `scope_namespaces` is configured (non-empty), the entity's
   `causely.ai/namespace` label must be in that list.
3. If neither signal is present/configured, the trigger is accepted by
   default.

This is how you run several `causely-background-agent` deployments — one per
repo/team — safely pointed at the same Causely tenant.

---

## 3. Deploying to a Causely-installed cluster

There is currently **no automated installer** — deployment is plain
Kubernetes manifests (`deploy/deployment.yaml`, `deploy/configmap.example.yaml`),
no Helm chart, no operator. An agent (human or AI) doing this deploy needs
exactly the following inputs; everything else below is mechanical.

### Required inputs (the "xxx, yyy, zz" checklist)

| # | Value | Where it's used | Secret or config? |
|---|---|---|---|
| 1 | `ANTHROPIC_API_KEY` | Claude API calls | **Secret**, required |
| 2 | `GITHUB_TOKEN` | GitHub read + PR creation (needs `repo` scope, and `read:org`/code-search access, on the target repo) | **Secret**, required |
| 3 | `SLACK_BOT_TOKEN` | Posting replies (`act` mode); a valid token is still required to construct the client even in `observe` mode | **Secret**, required |
| 4 | `github_repo` (`org/repo`) | The one repo this instance reads/writes | Config, required |
| 5 | `causely_mcp_url` | This tenant's Causely MCP endpoint (hosted: `https://api.causely.app/mcp`; self-run: in-cluster address) | Config, defaults to a placeholder localhost URL — **not enforced**, override it for anything real |
| 6a | `CAUSELY_MCP_TOKEN` | Bearer-token auth to the Causely MCP server, on a tenant with auth disabled or expecting a static bearer token | Secret, optional |
| 6b | `CAUSELY_MCP_CLIENT_ID` + `CAUSELY_MCP_CLIENT_SECRET` | HTTP Basic auth to the Causely MCP server instead of 6a — for a tenant with Frontegg auth enabled (e.g. staging). The server exchanges/caches a Frontegg access token itself, so this pair is static and never needs refreshing. Set both or neither; takes precedence over `CAUSELY_MCP_TOKEN` if both are set. | Secret, optional (required if the tenant's MCP endpoint has `authentication.disabled: false` — check the tenant's `api` ConfigMap, `services.MCP.config.authentication`) |
| 7 | `TRIGGER_SHARED_SECRET` | Bearer-auth for `POST /trigger` | Secret, **strongly recommended** — without it `/trigger` accepts unauthenticated requests from anyone who can reach the service |
| 8 | `SLACK_SIGNING_SECRET` | Verifies `/slack/actions` payloads are really from Slack | Secret, optional but recommended if using the Slack button trigger |
| 9 | `action_mode` | `observe` or `act` | Config, defaults to `observe` — deploy in `observe` first |
| 10 | `scope_namespaces` / `causely.ai/github-repo` label | Which entities this instance is allowed to act on, if running more than one instance | Config, optional |

### Step-by-step

```bash
# 0. Build & push the image (skip if using a pre-built one)
docker build -t docker.io/causelyai/causely-background-agent:latest .
docker push docker.io/causelyai/causely-background-agent:latest

# 1. Create the Secret out-of-band — never commit these values.
kubectl create secret generic causely-background-agent \
  --from-literal=anthropic-api-key="$ANTHROPIC_API_KEY" \
  --from-literal=github-token="$GITHUB_TOKEN" \
  --from-literal=slack-bot-token="$SLACK_BOT_TOKEN" \
  --from-literal=slack-signing-secret="$SLACK_SIGNING_SECRET" \
  --from-literal=causely-mcp-token="$CAUSELY_MCP_TOKEN" \
  --from-literal=causely-mcp-client-id="$CAUSELY_MCP_CLIENT_ID" \
  --from-literal=causely-mcp-client-secret="$CAUSELY_MCP_CLIENT_SECRET" \
  --from-literal=trigger-shared-secret="$(openssl rand -hex 32)"
#   Use causely-mcp-token OR the causely-mcp-client-id/-secret pair, whichever
#   the tenant's Causely MCP endpoint expects (see row 6a/6b above) — leave
#   the other one's --from-literal value empty rather than omitting the flag.

# 2. Copy deploy/configmap.example.yaml, fill in causely_mcp_url and
#    github_repo (and any other fields you want to change from defaults),
#    then apply it. It is NOT applied automatically by deployment.yaml.
cp deploy/configmap.example.yaml deploy/configmap.yaml
#   ... edit deploy/configmap.yaml ...
kubectl apply -f deploy/configmap.yaml

# 3. Apply the Deployment + Service.
kubectl apply -f deploy/deployment.yaml

# 4. Verify it's healthy.
kubectl rollout status deployment/causely-background-agent
kubectl port-forward svc/causely-background-agent 8090:8090
curl -f http://localhost:8090/healthz

# 5. Wire mediator to this instance's /trigger (see §2's "Mediator → agent
#    wiring") using the SAME value for the token as trigger-shared-secret
#    above. This is a mediator notification-config change, done separately
#    from this repo/deploy.

# 6. Confirm end-to-end in observe mode: trigger a real or synthetic root
#    cause, then check the pod logs and investigation_record_path (exec into
#    the pod, or mount /data via a debug pod) for a completed
#    InvestigationRecord before ever flipping action_mode to "act".

# 7. Once trusted: edit deploy/configmap.yaml, set action_mode: "act",
#    kubectl apply -f deploy/configmap.yaml, then roll the deployment
#    (kubectl rollout restart deployment/causely-background-agent) since
#    ConfigMap changes aren't hot-reloaded.
```

### Multi-repo / multi-team deployment

Each `causely-background-agent` instance handles exactly one `github_repo`.
To cover multiple repos against the same Causely tenant, deploy multiple
instances (distinct `metadata.name`/Service, distinct ConfigMap/Secret,
distinct `github_repo` + `scope_namespaces`), each pointed at the same
`causely_mcp_url`. `inScope()` (§2) is what keeps them from stepping on each
other if mediator's routing is imperfect.

### Persistence caveats (read before relying on `act` mode long-term)

- `/data` is an `emptyDir` in the stock `deployment.yaml` — survives
  container restarts but **not** pod recreation/rescheduling. Swap it for a
  PVC if you need `cost_state_file` (weekly budget) or
  `poll.state_file` (poll watermark) to survive that.
- `deployment.yaml` uses `strategy: Recreate` and `replicas: 1` deliberately
  — this agent isn't safe to run as multiple replicas racing on the same
  `/data` volume.
- Extra MCP server tokens configured via `mcp_servers` in `config.yaml` land
  in the **ConfigMap**, not a Secret — acceptable for an unauthenticated
  extra server (e.g. an open Grafana MCP endpoint), not for one that needs a
  real credential. Don't put a sensitive extra-MCP token in `config.yaml`
  today; that gap isn't closed yet.
