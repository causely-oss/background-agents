# causely-background-agent (Hermes on kagent)

A reference agent that investigates [Causely](https://causely.ai) issues with
Causely's MCP tools, the source repository and live cluster state, then ends
in one of three outcomes: a **fix PR**, an **immediate remediation** (restart,
rollback, scale, config revert), or **no action needed**.

This version has no service of its own. The agent is
[Hermes Agent](https://github.com/NousResearch/hermes-agent) running as a
[kagent](https://kagent.dev) **BYO Harness** on Agent Substrate. The
investigation method lives in an AgentTemplate system prompt and a Hermes
skill; deployment policy (observe or act, cluster writes, which repository)
lives on the Harness. The only code here is a small Go adapter that turns
kagent's configuration into Hermes configuration and bridges A2A to Hermes'
ACP interface. It follows the
[kagent-oss-hermes](https://github.com/AdminTurnedDevOps/agentic-demo-repo/tree/main/substrate/kagent-substrate/kagent-oss-hermes)
reference.

This is a reference implementation, not a required part of using Causely:
Causely's product surface is its MCP server.

> Replaces the earlier standalone Go service (webhook, poll and Slack
> triggers with a hand-written Claude tool loop). See
> [What changed from the Go service](#what-changed-from-the-go-service) for
> what that dropped and what replaces it.

## How it works

```
Causely mediator ─ A2A push ──┐  "Causely ID: <id>" + notification JSON
kagent ScheduledRun ──────────┤  "[causely-sweep] ..." every 30 min
kagent agent invoke / A2A ────┘  "Investigate Causely issue <id>"
        ▼
 kagent Agent causely-investigator ── Session ── Substrate Actor (this image)
                              │        (egress gateway adds model, MCP and
                              │         GitHub credentials to outgoing calls)
                              ├─ causely-hermes (Go): A2A server ⇄ ACP client
                              │     writes $HERMES_HOME/config.yaml + SOUL.md
                              │     from kagent's bootstrap config + Harness env
                              │
                              └─ hermes acp (Python)
                                    ├─ model: Anthropic (or OpenAI) ModelConfig
                                    ├─ MCP: Causely (+ any other RemoteMCPServer)
                                    ├─ terminal: git, gh, kubectl, rg
                                    ├─ skill: causely-investigation
                                    ├─ approvals.deny: policy enforcement
                                    └─ ledger: GitHub issues labeled
                                       causely-investigation (dedup across runs)
                                          │
                                          ▼
     per issue: OUTCOME: fix_pr | remediation | no_action_needed
                       | already_handled | out_of_scope | blocked
     (a sweep adds a SWEEP_SUMMARY block)
```

Every trigger gets its own kagent Session, Actor, Hermes session and
workspace, so runs share nothing on disk; the ledger is the shared state.
Follow-up messages to the same Session resume the same Hermes session.
See [triggering.md](deploy/kagent/triggering.md) for configuring each
trigger.

## Repository layout

```
causely-background-agent/
├── main.go              # startup: policy, credentials, config, A2A app
├── runner.go            # starts `hermes acp`, one session per Actor, per-turn time limit
├── client.go            # ACP client: streams Hermes text/tool events to kagent, rejects permission prompts
├── config.go            # kagent ADKConfig -> Hermes config.yaml (model, mcp_servers, approvals, skills)
├── soul.go              # SOUL.md = AgentTemplate prompt + generated deployment-policy section
├── policy.go            # Harness env -> policy, and policy -> Hermes approvals.deny globs
├── env.go               # keeps credentials and kagent's own variables out of Hermes' tool environment
├── tracing.go           # kagent telemetry init, per-turn token usage, tool naming for kagent's spans
├── *_test.go
├── gh-wrapper.sh        # /usr/local/bin/gh: supplies the GitHub token from a file
├── skills/causely-investigation/SKILL.md   # command recipes + report format
├── Dockerfile, Makefile  # image pins Hermes by version + sha256 (HERMES_VERSION, HERMES_SHA256)
├── deploy/kagent/
│   ├── harness.yaml             # Harness: image + policy env (literal values only)
│   ├── agenttemplate.yaml       # ConfigMap (system prompt) + AgentTemplate (Causely tool allowlist) + Agent
│   ├── model-and-tools.yaml     # Anthropic ModelConfig + Causely RemoteMCPServer
│   ├── egress.yaml              # EgressPolicy: GitHub access, token injected by the egress gateway
│   ├── rbac.yaml                # read-only ServiceAccount for kubectl (no Secrets)
│   ├── rbac-mutate.example.yaml # opt-in restart/scale, per namespace
│   ├── secrets.md               # which Secrets, how each is kept from the model
│   ├── scheduled-sweep.json     # kagent CreateScheduledRunRequest for the sweep (targets the Agent)
│   └── triggering.md            # mediator A2A push, scheduled sweep, manual, Slack
└── docs/AGENT_GUIDE.md  # operator walkthrough
```

## Quick start

Prerequisites: a cluster with kagent 1.0.1 and Agent Substrate 1.0 installed
(a `WorkerPool` named `kagent-default`, a snapshot bucket), the `kagent` CLI,
an Anthropic API key, a Causely MCP token, and a GitHub token for the one
repository this deployment investigates.

```bash
# 1. Build and push the image (linux/amd64 + linux/arm64), then pin the
#    image index digest in deploy/kagent/harness.yaml
make push IMAGE=<your-registry>/causely-hermes-harness TAG=0.1.0

# 2. Secrets (details and token scopes: deploy/kagent/secrets.md). None of
#    them is handed to the Actor: the egress gateway adds them to requests.
kubectl -n kagent create secret generic causely-hermes-anthropic --from-literal=api-key="$ANTHROPIC_API_KEY"
kubectl -n kagent create secret generic causely-hermes-causely-mcp --from-literal=authorization="Bearer $CAUSELY_MCP_TOKEN"
kubectl -n kagent create secret generic causely-hermes-github \
  --from-literal=api-token="$GITHUB_TOKEN" \
  --from-literal=git-basic="$(printf 'x-access-token:%s' "$GITHUB_TOKEN" | base64)"

# 3. Edit harness.yaml (image digest, CAUSELY_GITHUB_REPO, snapshot bucket), then apply
FILES="-f deploy/kagent/model-and-tools.yaml -f deploy/kagent/agenttemplate.yaml -f deploy/kagent/harness.yaml -f deploy/kagent/egress.yaml"
kubectl apply --dry-run=server $FILES && kubectl apply $FILES
kubectl -n kagent wait agent/causely-investigator --for=condition=Ready --timeout=10m

# 4. Investigate one issue by hand
SESSION=$(kagent agent session create --agent causely-investigator -o json | jq -r .session.id)
kagent agent invoke --session "$SESSION" -S --task "Investigate Causely issue $ISSUE_ID."

# 5. Wire the triggers (deploy/kagent/triggering.md):
#    - mediator: an A2A notification destination pointing at kagent
#    - sweep: grpcurl ... ScheduledRunService/CreateScheduledRun < deploy/kagent/scheduled-sweep.json
```

It starts in **observe** mode: it investigates fully and reports the fix or
remediation it would apply, but cannot push, open PRs or change the cluster.
Set `CAUSELY_ACTION_MODE=act` on the Harness once you trust it. See
[docs/AGENT_GUIDE.md](docs/AGENT_GUIDE.md) for the full walkthrough and
[deploy/kagent/triggering.md](deploy/kagent/triggering.md) for wiring
triggers.

## Investigation behavior

The system prompt ([agenttemplate.yaml](deploy/kagent/agenttemplate.yaml)) is
a port of the old service's `buildSystemPrompt`, unchanged in substance:

- **Don't adopt Causely's own remediation hint.** It is LLM-generated from
  limited evidence; the agent must verify the symptom against live config,
  the source and other observability data and derive its own fix.
- **Verify before concluding.** Find the exact source of a logged error and
  check whether it is already caught and handled; compare live state to what
  a fix would set before proposing one; never claim what a Secret contains;
  cite a real call site, not code that merely pattern-matches a name.
- **Outcomes, in order of preference:** `no_action_needed` when verification
  shows nothing is wrong (a first-class result, not a failure), then
  `remediation` when an immediate action fixes it, and only then `fix_pr` for
  a genuine code bug. Plus `already_handled` (ledger), `out_of_scope` (entity
  outside `CAUSELY_SCOPE_NAMESPACES`) and `blocked` (Causely MCP or another
  required tool unavailable).

New since the port, the same instructions serve two request shapes:

- **Single issue** (mediator push, or a person): recognised by a
  `Causely ID: <id>` line, the notification JSON block, or
  `Investigate Causely issue <id>`.
- **Sweep** (ScheduledRun): starts with `[causely-sweep]`. The agent lists
  active issues with `get_issues`, drops handled and out-of-scope ones, and
  investigates the most severe up to `CAUSELY_MAX_INVESTIGATIONS_PER_SWEEP`.

Either way each issue goes through the same procedure, cheapest checks first,
which is where the old service's pre-filters now live:

1. **Scope** from the notification's `causely.ai/namespace` label, before any
   tool call.
2. **Ledger**: skip as `already_handled` if the issue's GitHub ledger issue
   was claimed or updated within `CAUSELY_DEDUP_WINDOW` (24h), or an open
   `causely/<id>-*` PR exists.
3. **Staleness**: `get_issue_details`; if no longer active,
   `no_action_needed` without a deep investigation.
4. **Claim** in the ledger, investigate, finish, **record** the outcome as a
   ledger comment.

The [`causely-investigation` skill](skills/causely-investigation/SKILL.md)
holds the mechanics: ledger commands, sweep procedure, clone and search the
repo locally, kubectl recipes, the branch/commit/PR steps, the no-op-fix
check, and the report format. Every issue's report starts with a parseable
block; a sweep repeats it per issue and adds a `SWEEP_SUMMARY` /
`NOT_REACHED` block:

```
OUTCOME: fix_pr | remediation | no_action_needed | already_handled | out_of_scope | blocked
ISSUE: <Causely issue ID>
ENTITY: <entity name> (<namespace>)
ACTION_TAKEN: <PR URL | command run | ledger issue URL | none>
SUMMARY: <one or two sentences>
```

### The ledger

Actors are ephemeral and the mediator can notify several times about one
issue (detection, identity change, re-detection), so "already investigated"
has to live outside the Actor. It lives in GitHub, next to the PRs it
deduplicates: one issue per Causely issue in `CAUSELY_GITHUB_REPO`, labeled
`causely-investigation`, body containing `causely-issue: <id>`, one comment
per claim and per outcome. People can read it, close it, or re-trigger with
`force`. Lookups use the REST list endpoint rather than search, which lags new
issues. Set `CAUSELY_LEDGER=off` to disable it (no dedup; observe mode then
makes no GitHub writes at all).

## Safety model

Policy is set on the **Harness**, so editing the investigator's prompt cannot
turn on act mode. The adapter applies it in three layers, strongest first:

| Property | Credential / RBAC (holds regardless of the model) | Hermes `approvals.deny` (deterministic, terminal only) | Prompt |
|---|---|---|---|
| Observe mode: no push, no PR | GitHub token without Contents or Pull requests write (Issues write only, for the ledger) | `git push`, `gh pr create/edit/comment/close`, `gh api` with `-X`/`-f`/`--method`/`--input`, `gh issue edit/close`; `gh issue create/comment` too when the ledger is off | generated policy section |
| Ledger is append-only | (Issues write cannot be narrowed further) | `gh issue delete/transfer/lock`, `gh label delete` | skill |
| Never merge, force-push, or push to main/master | Branch protection | `gh pr merge`, `git push --force`, `push ... main` | skill |
| No cluster writes unless enabled | Read-only ClusterRole; mutate Role is opt-in per namespace | `kubectl apply/create/delete/patch/edit/...` always; `scale` and `rollout restart` unless `act` + `CAUSELY_ALLOW_KUBE_MUTATIONS=true` | generated policy section |
| Never read Secret values | ClusterRole has no access to Secrets | `kubectl ... secret ... -o/--output/--template`, `--raw`, `exec`, `cp`, `/var/run/secrets/` | system prompt |
| Never expose credentials | Tokens scoped to one repo / tenant / ServiceAccount | `gh auth token`, `git credential fill`, `*_TOKEN`/`*_API_KEY` names, the secrets directory, `/proc/*/environ` | generated policy section |
| Only read-only Causely tools | Causely token scope | n/a: write tools are not registered (AgentTemplate `tools` allowlist -> Hermes `tools.include`) | |

Other properties:

- `approvals.mode: manual`, and the adapter's ACP client **rejects every
  permission request**, so any command Hermes classifies as dangerous fails
  instead of waiting for a human who is not there.
- **Credentials never enter the Actor.** Under Agent Substrate 1.0 the
  model key, the Causely MCP header and the GitHub token stay in Secrets; the
  Actor sees placeholders, and Substrate's egress gateway (which terminates
  TLS, trusted through `SSL_CERT_FILE`/`GIT_SSL_CAINFO`) replaces the header
  on requests to the configured origins
  ([secrets.md](deploy/kagent/secrets.md), [egress.yaml](deploy/kagent/egress.yaml)).
  A model that prints its environment learns nothing usable.
- **Egress is closed by default.** An Actor reaches the model, its MCP
  servers and the kagent control plane; anything else (GitHub here) needs an
  `EgressPolicy` the Agent lists.
- The adapter still keeps kagent's own variables (`KAGENT_TOKEN`, the
  bootstrap descriptor) and any MCP header variable out of the environment
  Hermes' tools inherit, for standalone runs that pass real values.
- `CAUSELY_MAX_TURN_DURATION` (default `20m`, or `CAUSELY_MAX_SWEEP_DURATION`,
  default `60m`, for a prompt starting with `[causely-sweep]`) cancels a turn
  that runs too long; the ACP SDK sends `session/cancel` so Hermes stops calling the model.

**The deny rules are defense in depth, not a sandbox.** Hermes' `execute_code`
tool can start processes without going through the terminal tool, and every
command runs as the same Unix user as Hermes. Assume a determined model can
reach any credential in the Actor, and scope the credentials as described in
[secrets.md](deploy/kagent/secrets.md). That is the actual boundary, as it was
for the old service's GitHub token and ServiceAccount.

## How the adapter maps kagent config onto Hermes

Every Hermes key below was checked against the Hermes Agent v0.23.3 source
and exercised against that release (config load, MCP interpolation, deny-rule
matching, SOUL.md scanner, skill discovery), not guessed.

kagent 1.0 sends a BYO Actor its configuration as a bootstrap bundle the
adapter fetches from the control plane (`KAGENT_BOOTSTRAP_JSON`): an
`ADKConfig` protobuf with a `runtime` part (control-plane URL, limits) and
the `agent` part mapped below. Outside kagent (`--check`, local tests) the
adapter reads `KAGENT_CONFIG_JSON` and `KAGENT_AGENT_CARD_JSON` instead.

| kagent (`ADKConfig.agent`) | Hermes | Notes |
|---|---|---|
| `model` with `anthropic` | `model: {provider: anthropic, default, base_url, max_tokens}` | `ANTHROPIC_API_KEY` holds only a placeholder; the egress gateway adds the real key |
| `model` with `openai` | `model: {provider: openai, ...}` + `providers.openai` (`key_env: OPENAI_API_KEY`, `api_mode: chat_completions` or `codex_responses`) | as in the reference |
| other model types, `apiKeyPassthrough` | rejected at startup | |
| `instruction` | `$HERMES_HOME/SOUL.md` | Hermes' ACP adapter does not read `agent.system_prompt` (CLI only), so the reference's approach drops the prompt in ACP mode. SOUL.md is the identity slot every entry point loads. |
| `http_tools[]` / `sse_tools[]` | `mcp_servers.<name>: {url, transport: sse?, headers, timeout, ssl_verify, tools.include}` | name derived from the URL host (`api.causely.app` -> `causely`, tools appear as `mcp__causely__<tool>`); binding `tools` -> `tools.include` |
| header `__KAGENT_ENV[NAME]__` | `${AUXILIARY_NAME_API_KEY}` | Hermes interpolates `${VAR}` at connect time; the rename keeps the value out of subprocess environments. Under Substrate the value is a placeholder the egress gateway replaces |
| `require_approval`, `allowed_headers`, `stdio_tools`, `agent_plugins`, `sub_agents` | not mapped | Hermes has no per-MCP-server approval; skills ship in the image instead of AgentTemplate `skills`/`plugins` |
| Harness env `CAUSELY_*` | `approvals.deny` + SOUL.md policy section | see [Safety model](#safety-model) |
| (image) | `skills.external_dirs: [/opt/causely/skills]`, `terminal.cwd: /data/work`, `approvals.mode: manual` | |
| (always) | `_config_version: 54`, `tools.tool_search.enabled: off`, `agent.max_turns: 90` | the schema version stops Hermes treating the file as pre-versioning; tool search would otherwise hide some tools, MCP ones included, behind a `tool_search` call, while the prompt names the Causely tools directly; Hermes 0.23 has no iteration cap by default, so 90 keeps the previous per-turn bound |

The image also installs the `anthropic` and `mcp` extras of `hermes-agent`.
The reference lock (`hermes-agent[acp]`) has neither: Hermes then tries to
`pip install` the Anthropic SDK at runtime (which fails as the non-root user)
and silently skips every MCP server.

## GitHub: `gh` CLI, not the GitHub MCP server

The agent reads code by cloning the repository and searching it locally with
`rg` / Hermes' `search_files`, and opens PRs with `gh pr create`. We chose
this over the GitHub MCP server because:

- **Better investigation.** A shallow clone plus ripgrep searches a large
  monorepo exactly and quickly, including `git log` of the suspect file and
  reading surrounding code. The old service's REST `search_code` tool was its
  weakest link; MCP code search has the same index limits.
- **Enforceable.** `git push` and `gh pr ...` go through Hermes' terminal
  tool, so `approvals.deny` can block them in observe mode. GitHub MCP write
  tools (`create_or_update_file`, `push_files`, `create_pull_request`) are
  MCP calls that the deny rules cannot see, so observe mode would rest on the
  prompt alone.
- **Cheaper and simpler.** No 40-odd extra tool schemas in every request, no
  extra RemoteMCPServer, egress or credential.

The token is not reachable from the terminal: `gh` and git send the
`egress-injected` placeholder and the egress gateway substitutes the real
token for `api.github.com` (Bearer) and `github.com` (Basic, for git over
HTTPS). Token scope is still the boundary for what the agent can do with it.

## Extending with other MCP servers

Add a `RemoteMCPServer` (Grafana, Prometheus, an internal tool) and another
`tools` entry on the AgentTemplate. The adapter maps it into
`mcp_servers` automatically, and kagent opens egress to its URL; the prompt
already tells the agent to pick the source whose purpose matches the
question. Put any credential in `headersFrom` with `type: Secret` so it is
injected at egress like Causely's. Use the full in-cluster DNS name
(`<svc>.<ns>.svc.cluster.local`): Actors cannot resolve short names.

## What changed from the Go service

| Old service feature | Status | Replacement, if any |
|---|---|---|
| `POST /trigger` webhook (Causely `causelybot` notification) with shared-secret auth | **Replaced** | The mediator's `A2A` notification destination pushes each notification straight to kagent (bearer token to kagent's RPC API), no bridge service ([triggering.md](deploy/kagent/triggering.md)). Needs a mediator version with that destination. |
| Poll mode (watermarks, re-investigate on change) | **Replaced** | kagent ScheduledRun sweep ([scheduled-sweep.json](deploy/kagent/scheduled-sweep.json)): every 30 min, `get_issues` for active High/Critical issues, up to `CAUSELY_MAX_INVESTIGATIONS_PER_SWEEP`. No per-issue change watermark: an issue is revisited once its ledger entry is older than `CAUSELY_DEDUP_WINDOW`. |
| Dedup of repeated triggers for the same issue | **Replaced** | GitHub-issue ledger checked before any deep work, in both triggers. The mediator's own `request_id` only makes retries of one notification idempotent. |
| Staleness check (skip resolved issues) | **Kept, as a pre-check** | `get_issue_details` state check right after scope and ledger; inactive -> `no_action_needed` before any deep investigation. A few model turns instead of free. |
| Scope check (`scope_namespaces`, repo label) | **Kept, mostly** | `CAUSELY_SCOPE_NAMESPACES`, checked from the notification's `causely.ai/namespace` label before any tool call, and passed as `namespace_names` in sweeps. Prompt-level, plus RBAC limiting mutations to bound namespaces. The repo-label check is gone. |
| Per-investigation cost cap (`max_cost_usd`) | **Replaced by a time cap** | `CAUSELY_MAX_TURN_DURATION` (20m per issue) / `CAUSELY_MAX_SWEEP_DURATION` (60m per sweep), plus a 90-iteration limit per turn (`agent.max_turns`). No dollar accounting. |
| Weekly cost cap, concurrency cap | **Lost** | Use Anthropic workspace spend limits on the API key, mediator severity filters, the sweep size, and Substrate WorkerPool capacity. |
| JSONL investigation records (cost, tool calls, verdict) | **Replaced** | The kagent Session's A2A task history (every text and tool event streamed by the adapter), the parseable `OUTCOME:` block, and the outcome comment on the ledger issue. No cost or token totals. |
| OpenTelemetry spans per tool call | **Kept, extended** | kagent's harness runtime writes a span per turn (with its token usage), GenAI `execute_tool` spans and token metrics from the events the adapter reports. On when kagent's telemetry is on; see [Tracing](#tracing-opentelemetry). |
| Slack "Fix it" button and Slack replies in act mode | **Lost** | None in-process. Hermes' Slack gateway is a separate long-running process, not the ACP path. A Slack workflow can run the manual `kagent agent invoke`; results are visible in the ledger issue. |
| `observe` / `act` | **Kept** | `CAUSELY_ACTION_MODE` on the Harness, enforced by deny rules and token scope. |
| `allow_kubernetes_mutations` (restart/scale) | **Kept** | `CAUSELY_ALLOW_KUBE_MUTATIONS` (act mode only) + opt-in RBAC. |
| `kubectl_get_secret_keys` (key names only) | **Lost** | RBAC cannot grant key names without values, so Secrets are not readable at all. |
| No-op `propose_fix` rejection (search == replace) | **Moved to the model** | Skill requires a non-empty `git diff`, else re-classify. Not enforced in code. |
| Structured terminal tools (`propose_fix`, `recommend_remediation`, `no_action_needed`) | **Replaced** | The `OUTCOME:` report block. Free text, so a caller must parse it and handle a missing block. |
| GitHub REST tools (`read_file`, `search_code`, `list_directory`) | **Replaced** | Local clone + `rg` + `gh`. |
| Extra MCP servers from `config.yaml` (tokens in the ConfigMap) | **Improved** | RemoteMCPServer + Secret-backed `headersFrom`. |

## Tracing (OpenTelemetry)

Telemetry is kagent's. For a BYO Harness the controller passes its exporter
settings to the Actor as standard `OTEL_*` variables; the adapter starts
kagent's providers with them (`pkg/telemetry.Init`), and kagent's harness
runtime, which runs the A2A side of the adapter, writes the spans and
metrics from the events the adapter reports while translating Hermes' ACP
stream. Each A2A turn is one trace that continues the controller's:

```
a2a.request                                   one per turn (issue or sweep), with its token usage
├── execute_tool mcp__causely__get_issue_details
├── execute_tool terminal                     kubectl, gh, git, rg ... (failed or denied ones are errors)
└── execute_tool read_file
```

- **Turn span.** kagent names it `invoke_agent` only for its own Claude and
  Codex runtimes; a BYO runtime gets `a2a.request`. The adapter adds the
  turn's `gen_ai.usage.input_tokens` (cache reads included),
  `gen_ai.usage.output_tokens` and `gen_ai.usage.cache_read.input_tokens`.
  Hermes returns the session's cumulative counts with each ACP prompt
  response; the adapter subtracts the previous turn's (the counts restart
  at zero in each Hermes process).
- **`execute_tool` spans** come from Hermes' `tool_call` notification and
  its `completed`/`failed` update. The adapter reports the bare tool name
  (Hermes' ACP title is `<tool>: <preview>`), and the MCP server and tool
  for `mcp__<server>__<tool>` names.
- **Metrics.** The turn's usage is also reported as a usage-only model call,
  which kagent adds to its GenAI token metrics, along with its tool metrics.
- ACP has no per-model-call events, so there are no `chat` spans: model
  latency, retries and finish reasons are not visible, and token usage is
  per turn rather than per model call.

Content is **not** recorded by default: no prompts, model output, tool
arguments or tool results. Set
`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true` in the Harness env
to let kagent record the prompt and tool arguments (Hermes' raw input, or
its redacted title preview) and results. Turn it on only if your trace
backend may hold investigation data (logs, code snippets) for as long as it
keeps traces.

## Build and test

```bash
go build ./... && go test ./...   # adapter unit tests
make build                        # tests + image for this machine's platform, loaded locally
make build-all                    # tests + build linux/amd64 and linux/arm64 (no load/push)
make push                         # tests + push one multi-arch image (PLATFORMS=... to change)
```

Multi-platform builds need a buildx builder that can target both
architectures (Docker Desktop's default does; on Linux install QEMU binfmt,
for example `docker run --privileged --rm tonistiigi/binfmt --install all`).
The Go adapter is cross-compiled on the build host; the Hermes layer is
installed per platform, under emulation for the non-native one.

Validate an image without calling the model (config, Hermes ACP handshake and
session creation). `KAGENT_CONFIG_JSON` takes the ADKConfig kagent compiles:

```bash
docker run --rm -e ANTHROPIC_API_KEY=dummy \
  -e 'KAGENT_CONFIG_JSON={"runtime":{"control_plane_url":"http://kagent-controller.kagent:8083"},"agent":{"name":"causely_check","instruction":"Investigate.","model":{"model":"claude-opus-5-5","anthropic":{}}}}' \
  -e 'KAGENT_AGENT_CARD_JSON={"name":"causely_check","description":"check","version":"v1","supportedInterfaces":[{"url":"http://127.0.0.1:80","protocolBinding":"GRPC","protocolVersion":"1.0"}],"capabilities":{"streaming":true},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[]}' \
  <image> --check
```

## Known limitations and open risks

- **Tested on kind with kagent 1.0.1 and Agent Substrate 1.0** (gVisor
  WorkerPool, image from a local registry): the golden snapshot builds;
  sessions run Hermes with an OpenAI ModelConfig through the egress gateway;
  the in-cluster Causely MCP server's tools are discovered and called; tool
  events reach kagent; deny rules block `kubectl ... secret`; GitHub is
  unreachable without an EgressPolicy and reachable with one (`curl`, `gh`,
  `git` trust the gateway). Not yet exercised there: an Anthropic
  ModelConfig, token injection for GitHub (no token was available), a real
  investigation of an active issue (the test cluster had none), the
  mediator destination and the ScheduledRun, and an OTLP collector.
- **Deny rules are glob matching on command text.** They catch the obvious
  forms (including `bash -c`, `cd ... &&`), can be bypassed with enough
  indirection or via `execute_code`, and occasionally over-block (for
  example a namespace containing `-o` in a `kubectl ... secret` command).
  Credentials and RBAC are the boundary.
- **kubectl has no working credential path inside Substrate 1.0 for an
  in-cluster API server.** Harness env takes literal values only, so the
  old `KUBECONFIG_DATA` Secret cannot be used. The adapter supports
  `CAUSELY_KUBE_API_SERVER`: it writes a kubeconfig with a placeholder token
  and the gateway CA, for an EgressPolicy that injects a ServiceAccount
  token. On kind this reached the gateway, but the gateway rejected the API
  server's certificate (`UnknownIssuer`: it does not trust the cluster CA,
  and an EgressPolicy cannot add one). It can work for an API server with a
  publicly trusted certificate. Until Substrate supports this, investigate
  without kubectl (the shipped Harness does) or give the agent cluster reads
  through an MCP server instead.
- **Turn token usage** is Hermes' session counters, which leave out its
  auxiliary model calls (session titles, context compression); those still
  cost tokens.
- **The ledger is enforced by the model, not code.** The adapter cannot see
  which issue a message is about, so lookup, claim and record are
  instructions in the prompt and skill. A run that skips them duplicates work
  (cost), it cannot bypass the safety rules.
- **Ledger races.** Two runs that start within seconds of each other (the
  mediator's detect + identity-change notifications, or a push landing during
  a sweep) can both see no claim and both investigate. Each claims before
  the deep work, which narrows the window to the first few model turns.
  Duplicate ledger issues for one Causely issue are possible in that case;
  lookups match on the `causely-issue:` line, so both are found later.
- **Observe mode now writes to GitHub** (ledger issues and comments) when the
  ledger is on. Use a repository where that is acceptable, or
  `CAUSELY_LEDGER=off` to keep observe strictly read-only and give up dedup.
- **Mediator destination not exercised here.** The mediator's A2A
  destination was written for pre-1.0 kagent (it creates an AgentInstance
  from a Harness and AgentTemplate). kagent 1.0 creates Sessions for an
  Agent, so the mediator needs to target `kagent/causely-investigator` as an
  Agent; see triggering.md.
- **ScheduledRun has no manifest form** in kagent 1.0.1; it is a gRPC call
  targeting the Agent, so it is not reconciled from Git like the other
  resources.
- **Output is free text.** Callers should treat a missing or malformed
  `OUTCOME:` block as a failed investigation.
- **One session per Actor**, inherited from the reference runner: a second
  conversation needs a second kagent Session, which is the intended model
  anyway (one per issue).
- **Substrate's `/data` is a fresh tmpfs with a root-owned
  `/data/workspace`**, so Hermes works in `/data/work` instead.
- **Pinned upstreams move.** kagent is pinned to v1.0.1 (as a Go
  pseudo-version, since its Go module has no tag), Hermes to
  v0.23.3; the AUXILIARY_ env-name trick, the SOUL.md path, the ACP tool
  title format and the cumulative ACP usage depend on Hermes internals and
  should be re-checked on upgrade.
- **Hermes runs a gateway process.** Since 0.23, `hermes acp` is a thin
  client: sessions, MCP connections and tool execution live in a Hermes
  gateway it starts on demand, which inherits the adapter-built environment
  (so the credential handling above still applies), keeps
  `gateway.pid`/`gateway.sock` under `/data/hermes`, and also runs Hermes'
  cron and kanban schedulers (idle here). A container restart with `/data`
  kept, as after a snapshot restore, resumed the session in local testing.
- **Hermes is installed from source.** Hermes stopped publishing to PyPI
  after 0.19.0 and refuses wheel builds, so the image downloads the release
  tarball (sha256-pinned), syncs a venv from Hermes' own `uv.lock`
  (`--frozen`, hashes from upstream), and writes Hermes' install stamp so
  `hermes --version` reports the release. To upgrade, change
  `HERMES_VERSION`, `HERMES_SHA256` and `HERMES_COMMIT` in the Dockerfile.
