<!-- Copyright 2026 Anthropic PBC -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# MCP tunnels: the production path for private MCP servers

This repo reaches the k8s MCP server through a locally-run cloudflared quick
tunnel (see [README.md](../README.md#why-a-tunnel-for-a-local-cluster)).
That's fine for a local demo, but it depends on a process running on your
laptop and a tunnel URL that rotates every time you restart it — not
something you'd want to run a production integration on.

Anthropic Managed Agents has a native feature for this: **MCP tunnels**,
currently in **research preview (request access)**. It's the first-party
equivalent of the cloudflared rig in this repo, meant for private MCP
servers behind a firewall — a Kubernetes cluster, an internal API, anything
that isn't already reachable at a public URL.

Reference: [MCP tunnels overview](https://platform.claude.com/docs/en/agents-and-tools/mcp-tunnels/overview).

## The shape of it

- You deploy a lightweight gateway near your private MCP server (as a single
  binary, or via a Helm chart into a Kubernetes cluster). It makes a single
  **outbound** connection to Anthropic's tunnel infrastructure.
- Because the connection is outbound-only, there's nothing to open on your
  firewall — no inbound rule, no public endpoint, no exposed port.
- Each private MCP server behind the gateway gets a hostname under your
  tunnel domain (e.g. `k8s-mcp.<your-tunnel-domain>`).
- You attach those hostnames to a Managed Agent session from the Claude
  Console, or pass them in the Messages API `mcp_servers` array — the same
  way this repo already passes `K8S_MCP_URL` for the cloudflared tunnel.
- Tunnels themselves are created and managed from workspace settings in the
  Claude Console, by an org admin.

This repo does not include gateway install commands, Helm values, or exact
config field names — those are specific to the feature and may change while
it's in preview. See the reference doc above for the current specifics.

## How this maps onto this repo

The k8s MCP server is the piece that would move to a tunnel in production:

- Today: `scripts/run-k8s-mcp.sh` runs the MCP server locally and opens a
  cloudflared quick tunnel to it; the printed URL goes into `K8S_MCP_URL`.
- With an MCP tunnel: the k8s MCP server sits behind the tunnel gateway
  instead, and `K8S_MCP_URL` is set directly to the tunnel hostname your org
  admin created. `scripts/run-k8s-mcp.sh` and cloudflared aren't needed.

No other code changes are required — `agent.py` already treats
`K8S_MCP_URL` as an opaque remote MCP URL (see the comment next to it), so a
tunnel hostname works exactly like the cloudflared URL does today.

Grafana and Causely in this repo already point at long-lived remote URLs, so
they don't need a tunnel — MCP tunnels matter specifically for servers that,
like the k8s one, only exist inside a private network.

## Should you use it?

- **Local/demo (this repo's default):** cloudflared quick tunnel. Zero
  setup beyond running a script, no request-access gate.
- **Production or enterprise:** MCP tunnels, once you have access — no
  public exposure of your cluster, no rotating demo URL, managed centrally
  by an org admin instead of by whoever has a terminal open.

MCP tunnels are not generally available at time of writing; access is
request-only. Don't build a production rollout plan around them without
confirming current availability and specifics in the reference doc.
