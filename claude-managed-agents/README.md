<!-- Copyright 2026 Anthropic PBC -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Managed Agent · Remote MCP servers

> Based on Anthropic's [ship-your-first-managed-agent](https://github.com/anthropic-experimental/cwc-workshops)
> workshop.

A [Claude Managed Agent](https://platform.claude.com/docs/en/managed-agents/overview)
running in Anthropic's cloud, wired up to remote MCP servers so it can
investigate a real Kubernetes cluster: list namespaces, inspect workloads,
read events and logs, and (optionally) query Grafana or a causal-analysis
layer — all through tools it discovers from the servers themselves, not code
you write.

It's runnable end to end against a local [kind](https://kind.sigs.k8s.io/)
cluster.

## Why a tunnel for a "local" cluster

The agent doesn't run on your laptop — it runs in a managed container in
Anthropic's cloud. That container **cannot reach `localhost` or a private
kind API server**; it can only reach public URLs. So the MCP server that
talks to your kind cluster runs locally (pointed at kind via your
kubeconfig), and a [cloudflared](https://github.com/cloudflare/cloudflared)
quick tunnel gives it a public HTTPS URL the cloud agent can call. Nothing
in this setup ever points the agent at `localhost`.

The cloudflared quick tunnel is the works-today default here, good for
local development and proof of concept. For a production or enterprise deployment —
where you don't want to expose your cluster publicly at all — Anthropic
Managed Agents' native **MCP tunnels** feature (research preview, request
access) is the right path: an outbound-only gateway you deploy, with no
inbound firewall rules or public endpoint. See
[docs/mcp-tunnels.md](docs/mcp-tunnels.md) for how it maps onto this repo.

## Quickstart

Prerequisites: Docker, [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation),
`kubectl`, [cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/),
Node (for `npx`) or Go, Python 3.10+, and an
[Anthropic API key](https://console.anthropic.com/settings/keys).

```bash
git clone https://github.com/causely-oss/background-agents.git
cd background-agents/claude-managed-agents 

python -m venv .venv
source .venv/bin/activate          # Windows: .venv\Scripts\activate
pip install -r requirements.txt

cp .env.example .env               # fill in ANTHROPIC_API_KEY

./kind/setup.sh                    # creates the kind cluster + demo workload
./scripts/run-k8s-mcp.sh           # runs the k8s MCP server + cloudflared tunnel
```

Copy the printed `K8S_MCP_URL` into `.env`, then:

```bash
streamlit run app.py
```

The dashboard opens at `localhost:8501` with a chat panel talking to the
agent. 

Start a new session by clicking the '+' sign. 

Try: *"list the namespaces and pods you can see, then point out
anything that looks unhealthy."*

You can review logs from the session including how long the response took and how many tokens were used under [Managed Agents -> Sessions](https://platform.claude.com/)

When you're done: `./kind/teardown.sh` deletes the kind cluster, and
Ctrl-C stops `run-k8s-mcp.sh`.

## The three servers, three auth patterns

| Server | Runnable in this repo? | Auth pattern |
|---|---|---|
| **k8s** | Yes, against local kind | None at the MCP layer — reached over an unauthenticated cloudflared tunnel¹ |
| **grafana** | Optional, needs kube-prometheus-stack ([docs/grafana.md](docs/grafana.md)) | Static bearer token → vault `static_bearer` |
| **causely** | Optional, needs a Causely tenant | OAuth client-credentials → Frontegg JWT → vault `static_bearer` |

¹ In production, swap the cloudflared tunnel for a native MCP tunnel
(research preview, request access) — see [docs/mcp-tunnels.md](docs/mcp-tunnels.md).

Full explanation of the vault mechanism and the Bearer-only connector
constraint: [docs/auth.md](docs/auth.md).

## Trying it with Grafana and Causely

Both are optional add-ons on top of the k8s MCP server above, and both follow
the same pattern: stand up the server, tunnel it, add its URL (and any
credential) to `.env`, then **fully restart** `streamlit run app.py` — not
just refresh the browser. `setup_agent()` is cached for the life of the
process, so a still-running process keeps reusing an agent that was created
without the new server wired in.

### Add Grafana

Follow [docs/grafana.md](docs/grafana.md) to install kube-prometheus-stack,
create a read-only Grafana service-account token, and tunnel `mcp-grafana`.
Then set in `.env`:

```bash
GRAFANA_MCP_URL=https://<grafana-tunnel-host>.trycloudflare.com/mcp
GRAFANA_TOKEN=<the service account token>
```

Restart Streamlit and try:

*"Check the `demo` namespace for anything unhealthy, then pull CPU and
memory usage for the affected pod over the last 15 minutes from Grafana."*

That exercises both servers in one turn — k8s tools to find the workload,
Grafana tools to pull the metrics behind it.

### Add Causely

Causely is a causal-analysis layer that already models this cluster's
topology and root causes, so the agent is instructed (`_CAUSELY_FIRST` in
`provided.py`) to treat its diagnosis as authoritative rather than
re-deriving one from raw k8s/Grafana state. Set in `.env`:

```bash
ENABLE_CAUSELY=1
CAUSELY_MCP_URL=https://api.causely.app/mcp
CAUSELY_CLIENT_ID=<your Causely client id>
CAUSELY_CLIENT_SECRET=<your Causely client secret>
CAUSELY_TOKEN_URL=https://auth.causely.app/frontegg/identity/resources/auth/v2/api-token
```

Restart Streamlit and try:

*"What's broken in this cluster right now, and what's the root cause?"*

With Causely wired in, the agent calls `get_diagnoses` first and returns its
diagnosis directly.

## Want a more realistic scenario?

The `kind/` demo here is intentionally minimal — three pods, one deliberately
crash-looping, just enough to exercise the agent end-to-end. For a fuller
application topology with realistic services and chaos scenarios to
investigate, see [causely-oss/otel-demo](https://github.com/causely-oss/otel-demo)
and point `run-k8s-mcp.sh` (and, if you're using Grafana,
[docs/grafana.md](docs/grafana.md)'s kube-prometheus-stack) at that cluster
instead of the one from `kind/setup.sh`.

## The Managed Agents resource model

Four resources, created in this order:

**Agent → Environment → Session → Events**

- **Agent** — the model, system prompt, and tools (including which MCP
  servers it can call). Created once, reused forever.
- **Environment** — where the agent's container runs.
- **Session** — one conversation, bound to an agent + environment (+ a vault
  of MCP credentials). Sessions are real cloud resources, listed with
  `sessions.list()` and replayed with `events.list()` — no local database.
- **Events** — the message/tool-call stream for a session, opened with
  `sessions.events.stream()` and appended to with `sessions.events.send()`.

`agent.py` implements this as six functions:

| # | Function | API call |
|---|---|---|
| 1 | `setup_agent()` | `client.beta.agents.create` |
| 2 | `setup_vault()` | `client.beta.vaults.create` + `vaults.credentials.create` |
| 3 | `setup_environment()` | `client.beta.environments.create` |
| 4 | `start_session()` | `client.beta.sessions.create` |
| 5 | `stream_reply()` | `client.beta.sessions.events.stream` + `.send` |
| 6 | `delete_session()` | `client.beta.sessions.delete` |

Everything else — the system prompt, tool declarations, session picker, and
chat UI — is in `provided.py`.

## Tools and auth, in brief

- Every MCP server the agent can call is declared by URL in `agent.py`'s
  `mcp_servers` list, and enabled as a tool in `provided.py`'s `TOOLS` list
  via `mcp_toolset`. The two lists are gated on the same env vars — a server
  declared without a matching toolset (or vice versa) makes agent creation
  fail with a 400.
- Credentials for those servers don't live on the server entry — they live
  in a **vault**, created per session and referenced by `vault_ids=[...]` at
  session creation, matched to a server by exact URL string.
- The MCP connector only ever sends `Authorization: Bearer <token>` — it
  can't send Basic auth or a custom header. That's why Causely's native
  client-credentials flow is exchanged for a JWT first, then handed to the
  vault as a bearer token, rather than passed through directly.

Details: [docs/auth.md](docs/auth.md).

## Coming next

A reproducible with/without-Causely benchmark — same investigation, same
cluster, measured — is a planned follow-up repo.

## Repo layout

```
agent.py            ← the agent: 6 functions, one Managed Agents call each
provided.py          ← system prompt, tool declarations, chat UI
e2e.py               ← headless smoke test of the k8s MCP path

app.py               ← Streamlit entry point
ui.py, assets/       ← styling

kind/                ← kind cluster config + demo workload + setup/teardown
scripts/             ← run-k8s-mcp.sh: local MCP server + cloudflared tunnel
docs/                ← auth.md, grafana.md, mcp-tunnels.md
```

## License

Apache-2.0. See [LICENSE](LICENSE).
