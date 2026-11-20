# Causely investigator on Hermes + kagent: operator guide

This guide walks through what an investigation does, how to deploy it, and how
to move from observe to act safely. The [README](../README.md) is the
reference (architecture, safety model, config mapping, what changed from the
Go service); this is the walkthrough.

## 1. The pieces

| Resource | Owner decides | File |
|---|---|---|
| **Harness** `causely-hermes` | the image, and the deployment policy: observe/act, cluster writes, repository, namespace scope, time limit | `deploy/kagent/harness.yaml` |
| **AgentTemplate** `causely-investigator` | how to investigate (system prompt) and which Causely tools the model sees | `deploy/kagent/agenttemplate.yaml` |
| **Agent** `causely-investigator` | which template runs on which Harness, and which EgressPolicies it gets | `deploy/kagent/agenttemplate.yaml` |
| **EgressPolicy** `causely-github` | which hosts beyond the model and MCP servers the Actor may reach, and the GitHub token the egress gateway adds | `deploy/kagent/egress.yaml` |
| **ModelConfig** `causely-anthropic` | model and API key | `deploy/kagent/model-and-tools.yaml` |
| **RemoteMCPServer** `causely` | Causely MCP URL and auth header | `deploy/kagent/model-and-tools.yaml` |
| **ServiceAccount + RBAC** `causely-investigator` | what kubectl can see and change | `deploy/kagent/rbac.yaml`, `rbac-mutate.example.yaml` |
| **Skill** `causely-investigation` | ledger, sweep and command recipes, report format (baked into the image) | `skills/causely-investigation/SKILL.md` |
| **ScheduledRun** `causely-sweep` | when sweeps run and what they look at (gRPC, not a CRD) | `deploy/kagent/scheduled-sweep.json` |
| **Mediator A2A destination** | which Causely notifications are pushed (configured in Causely) | `deploy/kagent/triggering.md` |
| **Ledger** (GitHub issues labeled `causely-investigation`) | shared "already investigated" state across runs | in `CAUSELY_GITHUB_REPO` |

Splitting the Harness (policy) from the AgentTemplate (behavior) is
deliberate: the people tuning the prompt should not be able to grant the agent
write access by editing it.

## 2. What one investigation looks like

1. **Trigger.** A new kagent Session of the Agent gets one message:
   - the mediator's push: `Causely detected a problem...` with a
     `Causely ID:` line and the notification JSON;
   - a ScheduledRun execution: `[causely-sweep] ...`;
   - or a person: `Investigate Causely issue <id>.`
2. **Actor start.** Substrate restores an Actor from the Agent's golden
   snapshot. The adapter fetches its configuration (model, prompt, MCP
   servers) from the kagent control plane, reads the Harness env, writes
   `/data/hermes/config.yaml` and `SOUL.md`, and starts `hermes acp`.
   Credentials are not in the Actor; Substrate's egress gateway adds them to
   requests to the model, the MCP servers and GitHub.
3. **Pre-checks** (per issue; a sweep first lists issues with `get_issues`
   and runs one ledger lookup for all of them): scope from the notification's
   namespace label, then the ledger (`already_handled` if claimed or updated
   within `CAUSELY_DEDUP_WINDOW`, or an open `causely/<id>-*` PR exists), then
   `get_issue_details` (`no_action_needed` if no longer active). Then the agent
   **claims** the issue with a ledger comment.
4. **Investigation.** Hermes reads the issue through `mcp__causely__*`
   tools, checks live state with kubectl, clones the repository with `gh` and
   searches it with `rg`, and verifies the diagnosis before deciding anything
   (see the prompt's verification rules). A sweep does this for up to
   `CAUSELY_MAX_INVESTIGATIONS_PER_SWEEP` issues, most severe first.
5. **Outcome**, recorded as a comment on the ledger issue.
   - `no_action_needed`: verification shows the error is handled or the issue
     has self-resolved.
   - `already_handled`: the ledger or an open PR shows another run has it.
   - `remediation`: give the command; run it only in act mode with
     `CAUSELY_ALLOW_KUBE_MUTATIONS=true` and the per-namespace RBAC.
   - `fix_pr`: in act mode push `causely/<issue>-<slug>` and open a PR; in
     observe mode put the diff, title and body in the report.
   - `out_of_scope` / `blocked`: the entity is outside the namespaces this
     deployment owns, or Causely MCP (or another required tool) is missing.
6. **Result.** The final message starts with an `OUTCOME:` block (a sweep:
   one per issue plus `SWEEP_SUMMARY`). The full stream (text and every tool
   call and result) is the Session's task history in kagent; the ledger
   issue in GitHub carries the outcome too. If kagent's telemetry is on, the
   turn is also one OpenTelemetry trace: a turn span carrying the turn's
   token usage, with an `execute_tool` span per tool call (README,
   "Tracing").

Follow-ups ("why did you rule out the database?") go to the same Session
and resume the same Hermes session.

## 3. Deploying

### Inputs checklist

| # | Input | Where |
|---|---|---|
| 1 | Image pushed and pinned by digest | `harness.yaml` `workload.image` |
| 2 | WorkerPool name and snapshot bucket | `harness.yaml` `substrate` |
| 3 | Repository `owner/name` | `harness.yaml` `CAUSELY_GITHUB_REPO` |
| 4 | Anthropic API key | Secret `causely-hermes-anthropic` |
| 5 | Causely MCP URL and token | `model-and-tools.yaml`, Secret `causely-hermes-causely-mcp` |
| 6 | GitHub token scoped to that repository (Issues write for the ledger) | Secret `causely-hermes-github` (`api-token`, `git-basic`) |
| 7 | (optional) kubectl: not available for the in-cluster API server yet | secrets.md, "kubectl" |
| 8 | (optional) namespaces this deployment owns | `harness.yaml` `CAUSELY_SCOPE_NAMESPACES` |
| 9 | A kagent RPC token for the mediator user, and one for creating the ScheduledRun | mediator A2A destination; your `grpcurl` call |

### Steps

```bash
# 0. Image
make push IMAGE=<registry>/causely-hermes-harness TAG=0.1.0
#    (linux/amd64 + linux/arm64); then set harness.yaml workload.image to
#    <registry>/causely-hermes-harness@sha256:<digest of the image index>

# 1. Secrets: see deploy/kagent/secrets.md (exact commands and token scopes)

# 2. kagent resources (Harness, AgentTemplate + Agent, ModelConfig,
#    RemoteMCPServer, EgressPolicy)
FILES="-f deploy/kagent/model-and-tools.yaml -f deploy/kagent/agenttemplate.yaml -f deploy/kagent/harness.yaml -f deploy/kagent/egress.yaml"
kubectl apply --dry-run=server $FILES && kubectl apply $FILES

# 3. Wait for the Agent's golden snapshot (Substrate pulls the image and
#    starts the adapter once)
kubectl -n kagent wait agent/causely-investigator --for=condition=Ready --timeout=10m

# 4. Run one investigation in observe mode against a real issue
#    (the kagent CLI talks to localhost:8083: port-forward svc/kagent-controller)
SESSION=$(kagent agent session create --agent causely-investigator -o json | jq -r .session.id)
kagent agent invoke --session "$SESSION" -S --task "Investigate Causely issue $ISSUE_ID."
#    check the ledger issue it created in CAUSELY_GITHUB_REPO; run the same
#    command again and expect OUTCOME: already_handled

# 5. Turn on the triggers (deploy/kagent/triggering.md)
#    - mediator: add the A2A notification destination (ProblemDetected, High/Critical)
#    - sweep: grpcurl ... CreateScheduledRun < deploy/kagent/scheduled-sweep.json
```

If the Agent's `Ready` condition reports a failed golden build, its message
says why (for example the image could not be pulled: Substrate pulls images
itself, so the registry must be reachable from its `atelet` pods, not just
from the nodes). If the adapter fails, it logs one line saying why: an
unsupported model type, an invalid `CAUSELY_ACTION_MODE`, or a configuration
kagent's contract rejects. To reproduce outside the cluster, run the image
with `--check` (README, "Build and test").

### Wiring triggers

See [deploy/kagent/triggering.md](../deploy/kagent/triggering.md). Two
triggers feed the same Agent:

- **Mediator push** (real-time): the Causely mediator's `A2A` notification
  destination sends each notification to a new Session. Filter severity
  there; the old service's `allowed_severities` default of High/Critical is
  a good starting point. The mediator's destination still targets pre-1.0
  kagent; see triggering.md.
- **Scheduled sweep** (catch-up, replaces poll mode): a kagent ScheduledRun
  every 30 minutes looks for active High/Critical issues nobody has handled,
  including ones whose push was missed or filtered.

They are safe to run together: the ledger makes whichever run gets there
first own the issue for `CAUSELY_DEDUP_WINDOW`.

## 4. From observe to act

Run in observe mode until the reports are consistently right. Then, one step
at a time:

1. **PRs.** Give the GitHub token Contents write and Pull requests write,
   protect the default branch, and set `CAUSELY_ACTION_MODE=act`. Re-apply the
   Harness; new Sessions get the new policy once the Agent's new revision is
   Ready (existing Sessions keep the revision they started with).
2. **Restarts and scaling** (optional). Apply `rbac-mutate.example.yaml` once
   per namespace in `CAUSELY_SCOPE_NAMESPACES`, then set
   `CAUSELY_ALLOW_KUBE_MUTATIONS=true`. All three (act mode, the flag, the
   RoleBinding) must agree.

Rolling back is the reverse: set `CAUSELY_ACTION_MODE=observe` (or drop the
token's write scopes) and the agent can no longer push.

## 5. Cost

Every investigation spends real model tokens, in observe mode too. Bounds:

- `CAUSELY_MAX_TURN_DURATION` (default `20m`) cancels a single-issue turn
  that runs too long, `CAUSELY_MAX_SWEEP_DURATION` (default `60m`) a sweep;
  Hermes also stops after 90 model iterations per turn (which a sweep of
  several issues shares).
- The ledger, scope and staleness pre-checks end duplicates and resolved
  issues within a few model turns.
- Sweep cost is bounded by the schedule times
  `CAUSELY_MAX_INVESTIGATIONS_PER_SWEEP`; a quiet sweep (nothing new) costs
  one `get_issues` call and one ledger lookup.
- With telemetry on, `gen_ai.usage.*` on the turn spans (and kagent's GenAI
  token metrics) gives tokens per turn, and so per issue (a sweep turn covers several issues);
  that is the place to build spend dashboards.
- There is no dollar cap and no weekly cap any more. Put a spend limit on the
  Anthropic workspace the API key belongs to, and filter by severity on the
  mediator destination and in the sweep prompt.
- Sending only the issue ID keeps the prompt small; the agent fetches what it
  needs.

## 6. Several repositories or teams

One Harness carries one repository and one policy. For several repositories,
create one Harness per repository (distinct name, `CAUSELY_GITHUB_REPO`,
`CAUSELY_SCOPE_NAMESPACES`), one GitHub Secret and EgressPolicy per token,
and one Agent per Harness, all using the same AgentTemplate and Causely
RemoteMCPServer. Route with one ScheduledRun (and mediator destination) per
Agent; `CAUSELY_SCOPE_NAMESPACES` makes each deployment
drop other teams' issues as `out_of_scope` before any tool call, and keeps its
sweeps to its own namespaces. Each repository gets its own ledger.

## 7. State and persistence

- `/data` (Hermes home and sessions, the workspace `/data/work` with the
  cloned repository, the adapter's continuation store) is in the Actor and
  covered by the Substrate snapshot policy. Nothing secret is written there;
  the Actor holds no secrets at all.
- Nothing carries over between Sessions. The only cross-run state is the
  ledger in GitHub; the full investigation record is the Session history in
  kagent. There is no separate JSONL file to back up.
- To force a re-investigation inside the dedup window, send a manual message
  containing `force`, or wait out `CAUSELY_DEDUP_WINDOW`. Closing the ledger issue does not reset it (the
  window keys on its last update).

## 8. Changing behavior

- **Investigation method:** edit the ConfigMap in `agenttemplate.yaml`. Keep
  the verification rules; they are what stops the agent restating Causely's
  diagnosis.
- **Command recipes or report format:** edit
  `skills/causely-investigation/SKILL.md` and rebuild the image.
- **What the agent may do:** Harness env, token scopes and RBAC. Never the
  prompt.
- **More data sources:** add a RemoteMCPServer and a `tools` entry on the
  AgentTemplate.
