# causely-background-agent

A standalone reference agent that investigates a Kubernetes root cause using
[Causely](https://causely.ai)'s MCP tools plus GitHub read/write access, and
either opens a PR with a code-level fix or recommends an immediate
remediation (restart, rollback, scale, revert a config).

This is a **reference implementation**, not a required part of using Causely
— Causely's product surface is its MCP server; this repo shows one way to
build an agent on top of it. Bring your own agent if you'd rather build
something different against the same MCP tools.

## What it does

```
Causely root cause  →  causely-background-agent  →  Claude tool-use loop
  (webhook push,          /trigger         │
   or polled directly)  /slack/actions     ├─ Causely MCP tools (root cause,
                                            │  logs, topology, evidence)
                                            ├─ any additional MCP servers you
                                            │  configure (Prometheus, Grafana,
                                            │  your own internal tools, ...)
                                            ├─ GitHub read/write tools
                                            ├─ kubectl (if RBAC is granted)
                                            └─ recommend_remediation,
                                               propose_fix → GitHub PR, or
                                               no_action_needed
```

Causely and GitHub are the only two sources built in; every other tool here
is optional and additive. See
[Extending with other observability sources](#extending-with-other-observability-sources-prometheus-grafana-etc)
below for how to add more MCP servers, and [kubectl access](#kubectl-access)
for the live-cluster tools.

## Quick start

Any customer with a Kubernetes cluster and Causely reachable from it can
deploy this — nothing here is specific to any one tenant or environment.

**Prerequisites:**
- A Kubernetes cluster (this agent's own infrastructure — doesn't need to be
  where Causely itself runs)
- Causely reachable from that cluster, either Causely's hosted MCP endpoint
  (`https://api.causely.app/mcp`) or an in-cluster address
- An [Anthropic API key](https://console.anthropic.com/)
- A GitHub token (read + PR-write) for the one repo this instance should
  investigate and fix
- A Slack bot token and signing secret (both required to start even in
  `observe` mode; the token is never actually used to post unless you switch
  to `act` mode)
- `kubectl` access to the cluster you're deploying into

```bash
# 1. Use the published image, or build your own from this repo
docker pull docker.io/causelyai/causely-background-agent:latest

# 2. Create the Secret out-of-band — never commit these values
kubectl create secret generic causely-background-agent \
  --from-literal=anthropic-api-key="$ANTHROPIC_API_KEY" \
  --from-literal=github-token="$GITHUB_TOKEN" \
  --from-literal=slack-bot-token="$SLACK_BOT_TOKEN" \
  --from-literal=slack-signing-secret="$SLACK_SIGNING_SECRET" \
  --from-literal=trigger-shared-secret="$(openssl rand -hex 32)"
# trigger-shared-secret and slack-signing-secret are required — the agent
# refuses to start without them (see Securing /trigger below).

# 3. Copy deploy/configmap.example.yaml, set causely_mcp_url and github_repo
#    at minimum, then apply it
cp deploy/configmap.example.yaml deploy/configmap.yaml
kubectl apply -f deploy/configmap.yaml

# 4. Apply the PVC — required; deployment.yaml won't schedule without it
kubectl apply -f deploy/pvc.yaml

# 5. Apply RBAC — optional, only needed for the kubectl_* tools. Edit the
#    namespace placeholders in the file first (see its comments)
kubectl apply -f deploy/rbac.yaml

# 6. Apply the Deployment + Service
kubectl apply -f deploy/deployment.yaml

# 7. Verify
kubectl rollout status deployment/causely-background-agent
kubectl port-forward svc/causely-background-agent 8090:8090
curl -f http://localhost:8090/healthz
```

It starts in `observe` mode by default: it investigates and records what it
finds, but never opens a real PR or posts to Slack, until you deliberately
set `action_mode: "act"` in the ConfigMap. See `docs/AGENT_GUIDE.md` for the
full walkthrough — mediator webhook wiring, cost controls, multi-instance
scoping, and everything else below in more depth.

## Trigger sources

- **Push webhook** (`POST /trigger`) — Causely's mediator POSTs its real,
  already-existing notification payload (see `notification_payload.go`) after
  detecting a root cause; this is the same payload Slack/Teams destinations
  already receive, not a bespoke schema. Wiring this up is a **mediator
  notification-config change, not a code change**: point a `causelybot`-type
  destination at this agent's `/trigger` URL with a Bearer token matching
  `TRIGGER_SHARED_SECRET` — e.g. via env vars on the mediator deployment:
  `NOTIFICATION_CAUSELY_AGENT_TYPE=causelybot`,
  `NOTIFICATION_CAUSELY_AGENT_URL=http://<this-service>:8090/trigger`,
  `NOTIFICATION_CAUSELY_AGENT_TOKEN=<same as TRIGGER_SHARED_SECRET>`.
  Reflects the root cause's state at the moment it fired. Note: this delivery
  path doesn't carry Slack channel/thread info (that only exists after
  mediator's separate Slack-specific delivery), so a reply in `act` mode can't
  be threaded to the original alert via this trigger source.
- **Poll** (`poll.enabled: true` in config) — on an interval, asks Causely's
  MCP server for open issues directly (`get_issues`), instead of waiting for
  a one-time webhook. Tracks a watermark per issue so an unchanged, still-open
  issue isn't re-investigated every cycle, but a real change (severity shift,
  symptom count growth) or new occurrence triggers again. Useful because a
  webhook can't reflect how a root cause evolves after it fires.
- **Slack `/slack/actions`** — a human clicking "Fix it" on a Causely Slack
  alert.

All three feed the same investigation path and the same
`InvestigationRecord`.

## Action modes

- **`observe`** (default) — runs the full investigation and records what it
  found and what it would have done, but never opens a real PR or posts to
  Slack. Use this to evaluate a configuration or Causely's own root-cause
  quality without side effects.
- **`act`** — does it for real.

## Cost

**Every investigation costs real money, in every mode — including
`observe`.** `observe` only gates the *side effects* (opening a PR, posting
to Slack); the Claude tool-use loop itself always runs for real and always
spends real Anthropic API tokens. There is no free "dry run" of the
investigation itself.

Two independent caps exist (`config.yaml`, see `cost.go`):
- `max_cost_usd` (default `5`) — aborts a single investigation if it exceeds
  this. Partial findings up to that point are still recorded.
- `max_weekly_cost_usd` (default `50`) — a rolling 7-day cap across *all*
  investigations this instance runs; once hit, new investigations are
  skipped entirely (recorded with verdict `skipped_budget`) until spend ages
  out of the window. Persisted to `cost_state_file` if set, so a restart
  doesn't reset the counter. This check happens *before* an investigation
  starts, not as an atomic reservation, so `max_concurrent_investigations`
  (default `5`) bounds how many investigations can be in flight at once —
  without it, a burst of near-simultaneous triggers could all pass the check
  before any of them records spend, overshooting the weekly cap by more than
  one investigation's worth.

In our own testing, a single investigation typically cost in the
**$0.4–$3.5** range, varying with how much digging Claude needed to do
(a quick config check vs. tracing a bug through source across several
files) — treat this as a rough sense of scale, not a guarantee; it depends
on your repo's size, how deep an investigation goes, and Anthropic's current
pricing (`cost.go`'s pricing table is a hardcoded snapshot that needs
periodic manual updating against
[Anthropic's published pricing](https://www.anthropic.com/pricing) — it will
silently drift stale otherwise).

The biggest cost lever if you enable `poll` mode is `allowed_severities` —
poll runs continuously and pays for every genuinely new occurrence it acts
on, so it **defaults to `["High", "Critical"]`** even if you never set it
(see [Configuration](#configuration) below). Scoping `scope_namespaces` and
keeping `poll.interval` reasonable (default `5m`) are the other two main
levers. For the webhook trigger, the equivalent lever is on Causely's
mediator side: configure its notification destination to only push
High/Critical-severity root causes to this agent in the first place, rather
than relying on this agent to filter after the fact.

## Model

The Claude model is **currently hardcoded** (`claudeModel` in `cost.go`) —
there is no config field or environment variable to choose a different
model today. Model selection (and the accompanying per-model pricing table
`cost.go` uses for cost tracking) is planned to become configurable in a
future version. If you need a different model right now, that requires a
source change.

## kubectl access

If `deploy/rbac.yaml` is applied, the agent gets `kubectl_get`,
`kubectl_get_secret_keys`, and `kubectl_logs` — direct, live cluster state
that's more authoritative than any cached/indexed view (including Causely's
own `get_config` MCP tool), since it reflects the cluster at the moment of the
call. These read tools are granted **cluster-wide**, deliberately: a root
cause's actual dependency is often outside the namespace of the service it's
degrading (e.g. an app in `causely` failing because of something in
`monitoring`), and diagnosis shouldn't be namespace-blind. In `act` mode, two
mutating tools are also offered: `kubectl_rollout_restart` and
`kubectl_scale`, letting the agent actually apply a remediation instead of
only describing one in Slack — these are scoped per-namespace (matching
`scope_namespaces`), not cluster-wide, since a bad restart/scale call has a
much bigger blast radius than a bad read. `kubectl_get` never returns Secret
content — `kubectl_get_secret_keys` returns only a Secret's key names, never
decoded values, since Secret content flowing into Claude's context risks it
being echoed into a PR body, Slack message, or the investigation record. If
RBAC isn't applied, the agent starts fine and simply omits these tools for
that run — see `newKubeClient` in `kube.go`.

Everything these tools *do* return — ConfigMap data, pod logs, resource
specs — is not similarly filtered: it's sent to Anthropic's API as part of
the investigation (see [Cost](#cost)) and may be echoed into a PR body, Slack
message, or the investigation record, the same as source code Claude reads
via GitHub. If your ConfigMaps or logs can contain sensitive data (tokens
embedded in a log line, an internal hostname you don't want to leave the
cluster), account for that before granting broad read access, the same way
you would for any tool that lets an LLM read cluster state.

## Extending with other observability sources (Prometheus, Grafana, etc.)

`mcp_servers` in config.yaml already supports arbitrary extra MCP servers
beyond the built-in Causely one — **no code change required**. Each entry
becomes a set of tools Claude can call, prefixed by that server's `name` (so
a `prometheus` entry's tools show up as `prometheus__query_range`, etc. —
see `buildTools` in `agent.go`). You can add as many as you like:

```yaml
mcp_servers:
  - name: prometheus
    url: https://prometheus.example.com/mcp
    description: "raw metrics, PromQL queries"
  - name: grafana
    url: https://grafana.example.com/mcp
    description: "dashboards and panel data"
    token: "..."  # optional bearer token — see the Known limitations note below
```

The `description` field matters: it's what tells Claude *when* to reach for
each source (surfaced in the system prompt alongside every other configured
source), so a specific, distinguishing description is worth writing rather
than leaving it blank.

## A third terminal outcome: no_action_needed

Beyond `recommend_remediation` and `propose_fix`, the agent can call
`no_action_needed` — for when investigation shows there's no genuine defect:
the flagged exception is already caught and handled gracefully by design
(e.g. a broad `except` that logs a warning and deliberately continues), or
the issue has already fully self-resolved and the live state already matches
what any fix would produce. Without this, the tool schema would force an
action-shaped answer even when the correct answer is "nothing is actually
wrong" — found by manually investigating a cleared issue where Causely's own
diagnosis was named `PythonUnhandledException`, but the exact source line
producing it was inside a `try/except Exception` block explicitly commented
"best effort, don't fail the whole request." The system prompt now instructs
Claude to verify a diagnosis this way (check live state against any proposed
change, find and read the actual code around a cited log line) before
concluding a fix or remediation is warranted at all — see `buildSystemPrompt`
in `agent.go`.

## Not adopting Causely's own suggested remediation

Causely's root-cause detection includes its own LLM-generated remediation
suggestion (`description.remediationOptions`), sometimes also embedded as a
concluding sentence inside the root cause's description text (e.g.
"Remediation should focus on..."). This agent's system prompt (see
`buildSystemPrompt` in `agent.go`) deliberately never shows Claude that
suggestion, and explicitly instructs it not to adopt any such sentence found
inside the description either — this agent's entire value over the generic
suggestion is its tool access (live cluster state, source code, and whatever
else is configured) that the suggestion's own generation had none of; showing
it the suggestion risks anchoring it into restating that guess instead of
verifying it against real evidence. The suggestion is still captured in each
`InvestigationRecord` as `causely_remediation_hint`, purely for later human
comparison against what the agent independently concluded.

## Investigation records

Every run — regardless of trigger source, and regardless of whether it was
skipped (out of scope, budget exceeded), failed, or completed — writes one
`InvestigationRecord` as a line of JSON to `investigation_record_path` (if
set). It captures: identity, trigger source, action mode, timing, token/cost
usage, the list of tool calls made (which MCP server, which tool), and the
verdict. This is the dataset for answering "how well is this actually
working," not just per-incident Slack messages. See
`investigation_record.go`.

## Configuration

Non-secret settings come from a YAML config file (`-config`, default
`/config/config.yaml`); see `deploy/configmap.example.yaml` for every field.
Credentials come from environment variables only — `ANTHROPIC_API_KEY`,
`GITHUB_TOKEN`, `SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET`,
`CAUSELY_MCP_TOKEN` (optional, bearer-token auth to the Causely MCP server),
`CAUSELY_MCP_CLIENT_ID` + `CAUSELY_MCP_CLIENT_SECRET` (optional, HTTP Basic
auth instead — for a Causely tenant with Frontegg auth enabled, which
exchanges and caches a Frontegg access token server-side so this agent never
has to fetch/refresh one itself; set both or neither, and they take
precedence over `CAUSELY_MCP_TOKEN` if both are set), `TRIGGER_SHARED_SECRET`.

**`TRIGGER_SHARED_SECRET` and `SLACK_SIGNING_SECRET` are required — the agent
refuses to start without them.** An unauthenticated `/trigger` or an
unverified `/slack/actions` on a network-reachable service means anyone who
can reach it can spend this instance's Anthropic budget and, in `act` mode,
open PRs or mutate the cluster. For local/demo use against a throwaway
cluster only, set `allow_unauthenticated_trigger: true` /
`allow_unauthenticated_slack_actions: true` in `config.yaml` to explicitly
accept that risk instead.

`allowed_severities` (config.yaml, e.g. `["High", "Critical"]`) restricts
every trigger source to root causes at those severities — see `scope.go`'s
`inScope` and `poll.go`.

**If `poll.enabled: true` and this is left unset, poll defaults to
`["High", "Critical"]` on its own** (`pollSeverities` in `poll.go`) — poll
runs continuously and pays for every genuinely new occurrence it dispatches
on, so an operator who never touches this setting still gets a sane default
instead of paying to investigate every low-severity blip. To make poll
consider every severity, set this explicitly to
`["Low", "Medium", "High", "Critical"]`. The webhook and Slack paths do
**not** get this default — an unset `allowed_severities` there truly means
"no severity filter," since those are driven by a human or by mediator's own
notification config, not by this agent polling on a timer; if you want the
same High/Critical-only behavior for webhook-triggered investigations,
configure that filter on mediator's notification destination itself (which
severities it forwards to this agent), not here.

This setting also closes a real noise source: a chronic, recurring issue's
severity can flicker between a baseline and an elevated value as it merely
toggles active/inactive, and poll's watermark (falling back to severity when
Causely's `get_issues` doesn't provide an `updated_at`) can read that
flicker as "changed," paying for a second investigation of an event that
hadn't actually changed. Filtering by severity (passed straight through to
`get_issues`, so low-severity issues aren't even fetched) closes that off
regardless of what the watermark does.

## Securing `/trigger`

This is a standalone, network-reachable service — unlike an in-repo call, it
can't lean on network topology alone. `TRIGGER_SHARED_SECRET` is required;
the agent refuses to start without it (see [Configuration](#configuration)).
Set it and require callers to send `Authorization: Bearer <secret>`.

## Build & run

```bash
go build -o causely-background-agent .
./causely-background-agent -config /path/to/config.yaml
```

Or build and push the container:

```bash
docker build -t docker.io/causelyai/causely-background-agent:latest .
docker push docker.io/causelyai/causely-background-agent:latest
```

## Deploy

See [Quick start](#quick-start) above for the full sequence. All of it is
plain Kubernetes manifests under `deploy/` — no Helm/chart dependency.

## Known limitations

- No RC-type allowlist — every root cause type triggers an investigation.
- Extra MCP server tokens (and client_id/client_secret pairs) configured via
  `mcp_servers` in config.yaml land in the ConfigMap, not a Secret — fine for
  an unauthenticated server, be aware for an authenticated
  one.
- `/data` (investigation records, weekly cost state, poll watermark) is
  backed by the PVC in `deploy/pvc.yaml`, which survives pod recreation —
  apply it before `deployment.yaml`. If you deploy `deployment.yaml` without
  it, Kubernetes will refuse to schedule the pod (no matching volume), rather
  than silently falling back to ephemeral storage.
- RBAC can't restrict a Role to only a Secret's key names, not its decoded
  values — `deploy/rbac.yaml` grants `get`/`list` on the full Secret object;
  the safety boundary that `kubectl_get_secret_keys` never returns decoded
  values is enforced in this agent's own code (`kube.go`), not by RBAC.
