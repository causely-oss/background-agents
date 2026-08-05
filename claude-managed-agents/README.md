<!-- Copyright 2026 Anthropic PBC -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Managed Agent · Remote MCP servers

> Based on Anthropic's [ship-your-first-managed-agent](https://github.com/anthropic-experimental/cwc-workshops)
> workshop. Not maintained and not accepting contributions.

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
local development and demos. For a production or enterprise deployment —
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
git clone <this-repo>
cd claude-managed-agents

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
agent. Try: *"list the namespaces and pods you can see, then point out
anything that looks unhealthy."*

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
