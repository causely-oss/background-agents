# Triggering investigations

One kagent Agent (`kagent/causely-investigator`, which binds the
AgentTemplate of that name to the `causely-hermes` Harness) serves two
triggers. Both run the same
per-issue procedure and report format; the agent tells them apart from the
message itself.

| Trigger | Who sends it | Message shape | Turn time limit |
|---|---|---|---|
| **Mediator push** | Causely mediator, A2A notification destination | one issue: `Causely ID: <id>` line + notification JSON | `CAUSELY_MAX_TURN_DURATION` (20m) |
| **Scheduled sweep** | kagent ScheduledRun | starts with `[causely-sweep]` | `CAUSELY_MAX_SWEEP_DURATION` (60m) |
| Manual | a person, `kagent agent invoke` / any A2A client | `Investigate Causely issue <id>.` | `CAUSELY_MAX_TURN_DURATION` |

Every run gets its own kagent Session (and Actor), so runs share nothing on
disk. The **ledger** (GitHub issues labeled `causely-investigation` in
`CAUSELY_GITHUB_REPO`, see the skill) is what stops two runs investigating the
same Causely issue: each run checks it first, claims the issue before the deep
investigation, and records its outcome there.

## 1. Mediator push (A2A notification destination)

Causely's mediator can deliver notifications straight to kagent over A2A, with
no bridge service. This needs a mediator version that has the `A2A`
notification destination type.

> **kagent 1.0.** The destination below was written for pre-1.0 kagent, where
> it created an AgentInstance from a Harness and an AgentTemplate. kagent 1.0
> replaces that with Sessions of an Agent
> (`kagent.api.v1.SessionService/CreateSession` with the Agent
> `kagent/causely-investigator`, then an A2A message to the session). Until
> the mediator supports that, use the scheduled sweep below; it picks up the
> issues a push would have delivered.

Configure a destination of type `A2A` with:

- **URL**
  ```
  grpc://kagent-controller.kagent.svc.cluster.local:8083?kagentHarness=kagent/causely-hermes&kagentAgentTemplate=kagent/causely-investigator
  ```
  Add `&userId=<name>` to change the kagent user the mediator acts as
  (default `causely-mediator`, sent as `x-user-id`).
- **Token**: a credential kagent's RPC authorization accepts for that user,
  sent as `authorization: Bearer <token>`. It needs permission to create
  AgentInstances and send messages for this Harness/AgentTemplate, nothing
  more.
- **Types**: the default, `ProblemDetected` only, is what you want; the agent
  is not built to act on resolution notifications.

Filter by severity on the mediator side, as before: every delivered
notification is an investigation that costs tokens unless the ledger or scope
check stops it early.

What the mediator does per notification:

1. `CreateAgentInstance` with
   `request_id = "causely-" + sha256(objectId|type|scopeChange|timestamp)[:32]`.
   A retried delivery of the same notification reuses the instance; a later
   notification about the same issue (re-detection, identity change) creates
   a new one. Issue-level dedup is therefore the ledger's job, not kagent's.
2. Sends one text part, returning immediately (it never follows the task or
   deletes the instance):

   ````
   Causely detected a problem. Investigate it.

   Problem: <title> (<diagnosis name>)
   Entity: <type> <name>
   Severity: <severity>
   Causely ID: <issue objectId>
   Link: <url>

   The full Causely notification:

   ```json
   {"objectId": "...", "type": "ProblemDetected", "name": "...", "severity": "...",
    "entity": {...}, "labels": {"causely.ai/namespace": "...", ...},
    "description": "...", "impactedServices": [...], "link": "..."}
   ```
   ````

The agent reads the scope from `labels["causely.ai/namespace"]` before calling
any tool, then the ledger, then confirms the issue is still active with
`get_issue_details`. Duplicates and out-of-scope issues end there, for the
cost of a few short model turns.

Results are in kagent: list the AgentInstances created by the mediator's user
and read each task; the final message starts with an `OUTCOME:` block. The
ledger issue in GitHub carries the same block as a comment.

## 2. Scheduled sweep (kagent ScheduledRun)

kagent runs schedules natively. In kagent 1.0.1 a ScheduledRun is created
through the gRPC `kagent.api.v1.ScheduledRunService` (there is no CRD and no
`kagent` CLI command for it) and targets an Agent. Each execution creates its
own Session and sends the configured prompt as one text part.

[`scheduled-sweep.json`](scheduled-sweep.json) is a ready
`CreateScheduledRunRequest`: every 30 minutes, sweep active High/Critical
issues, investigate up to `CAUSELY_MAX_INVESTIGATIONS_PER_SWEEP` of them,
with an execution timeout of 65 minutes (above the adapter's 60-minute sweep
limit, so Hermes is cancelled cleanly first). `go test` validates it against
kagent's own proto and cron rules.

The kagent controller serves gRPC reflection, so `grpcurl` works without
proto files:

```bash
kubectl -n kagent port-forward svc/kagent-controller 8083:8083 &

# -plaintext only if your controller listens without TLS; otherwise drop it
grpcurl -plaintext \
  -H "authorization: Bearer $KAGENT_TOKEN" -H "x-user-id: causely-sweeper" \
  -d @ localhost:8083 kagent.api.v1.ScheduledRunService/CreateScheduledRun \
  < deploy/kagent/scheduled-sweep.json

# SCHEDULED_RUN_ID is .scheduledRun.id in the create response.
# Run one now instead of waiting for the schedule:
grpcurl -plaintext -H "authorization: Bearer $KAGENT_TOKEN" -H "x-user-id: causely-sweeper" \
  -d "{\"scheduledRunId\": \"$SCHEDULED_RUN_ID\", \"requestId\": \"manual-$(date +%s)\"}" \
  localhost:8083 kagent.api.v1.ScheduledRunService/TriggerScheduledRun
```

`requestId` makes creation idempotent. To change the schedule or prompt use
`UpdateScheduledRun`; to stop it, set `paused: true` or call
`DeleteScheduledRun`. `ListScheduledRunExecutions` shows each run and its
Session.

What a sweep does (prompt + skill): `get_issues` (active, scoped by
`CAUSELY_SCOPE_NAMESPACES`, High/Critical per the prompt), one ledger lookup
for all of them, drop the handled and out-of-scope ones, then investigate the
most severe remaining issues one at a time up to the limit, claiming and
recording each in the ledger. It ends with one OUTCOME block per issue and a
`SWEEP_SUMMARY` / `NOT_REACHED` block; issues not reached are picked up by the
next sweep.

The sweep and the mediator can overlap: a pushed issue being investigated is
claimed in the ledger, and the sweep skips it (and vice versa).

## 3. Manually

```bash
SESSION=$(kagent agent session create --agent causely-investigator -o json | jq -r .session.id)
kagent agent invoke --session "$SESSION" -S --task "Investigate Causely issue $ISSUE_ID."
```

(`kagent` talks to `http://localhost:8083`; port-forward
`svc/kagent-controller` or pass `--api-url`/`--gateway-url`.)

Add `force` to the task to investigate an issue the ledger says was handled
recently. Follow-up questions go to the same Session and resume the same
Hermes session.

To pull just the outcome:

```bash
kagent agent invoke --session "$SESSION" --task "Investigate Causely issue $ISSUE_ID." \
  | grep '^OUTCOME:'
```

## Slack

Hermes ships a Slack gateway, but it is a separate long-running Hermes
process, not the ACP path this harness uses, so it does not run inside the
kagent Actor. The old "Fix it" button on Causely's Slack alert has no direct
replacement; a Slack workflow can call the manual commands above.
