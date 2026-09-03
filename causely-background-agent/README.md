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
                                            ├─ GitHub read/write tools
                                            └─ recommend_remediation, or
                                               propose_fix → GitHub PR
```

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
`GITHUB_TOKEN`, `SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET` (optional),
`CAUSELY_MCP_TOKEN` (optional), `TRIGGER_SHARED_SECRET` (optional, but you
should set it — see below).

## Securing `/trigger`

This is a standalone, network-reachable service — unlike an in-repo call, it
can't lean on network topology alone. Set `TRIGGER_SHARED_SECRET` and require
callers to send `Authorization: Bearer <secret>`. If unset, `/trigger` logs a
startup warning and accepts unauthenticated requests.

## Build & run

```bash
go build -o causely-background-agent .
./causely-background-agent -config /path/to/config.yaml
```

Or build and push the container:

```bash
docker build -t docker.io/causely/causely-background-agent:latest .
docker push docker.io/causely/causely-background-agent:latest
```

## Deploy

See `deploy/deployment.yaml` and `deploy/configmap.example.yaml` — plain
Kubernetes manifests, no Helm/chart dependency. Create the Secret out-of-band
(never commit it); the deployment file's header comment has the exact
`kubectl create secret` command.

## Known limitations

- No RC-type allowlist — every root cause type triggers an investigation.
- `MCP_SERVERS_JSON`-style extra MCP server tokens configured via
  `mcp_servers` in config.yaml land in the ConfigMap, not a Secret — fine for
  an unauthenticated server, be aware for an authenticated one.
- Cost-state and poll-watermark persistence use whatever volume you mount at
  `/data` — an `emptyDir` survives container restarts but not pod
  recreation; use a PVC if you need the latter.
