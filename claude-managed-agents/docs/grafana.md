<!-- Copyright 2026 Anthropic PBC -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Optional: wiring up Grafana

This is optional. Skip it and the agent still works with just the k8s MCP
server — `GRAFANA_MCP_URL` and `GRAFANA_TOKEN` empty in `.env` means the
Grafana server and toolset are left out entirely (see the gating in
`agent.py` / `provided.py`).

## 1. Install kube-prometheus-stack on kind

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
  --namespace monitoring --create-namespace
```

This gives you Prometheus, Alertmanager, and a Grafana instance with
Kubernetes dashboards already provisioned.

## 2. Create a Grafana service-account token (Viewer role)

Port-forward Grafana locally and create a service account with the
**Viewer** role — read-only is all the MCP server needs:

```bash
kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
```

In the Grafana UI (`http://localhost:3000`, default admin credentials are in
the chart's `values.yaml` unless you overrode them): **Administration →
Users and access → Service accounts** → create one with the **Viewer** role
→ add a service account token. Copy it.

## 3. Run the Grafana MCP server + tunnel

Using [grafana/mcp-grafana](https://github.com/grafana/mcp-grafana):

```bash
export GRAFANA_URL="http://localhost:3000"
export GRAFANA_SERVICE_ACCOUNT_TOKEN="<the token from step 2>"
mcp-grafana -t streamable-http --address localhost:8081
```

Check the server's own startup log for the exact path it serves (typically
`/mcp` for `streamable-http` mode), then open a tunnel to it the same way
`scripts/run-k8s-mcp.sh` does for the k8s server:

```bash
cloudflared tunnel --url http://localhost:8081
```

## 4. Wire it into .env

```bash
GRAFANA_MCP_URL=https://<the-tunnel-host>.trycloudflare.com/mcp
GRAFANA_TOKEN=<the same service account token from step 2>
```

`GRAFANA_TOKEN` goes into the Managed Agents vault as a `static_bearer`
credential scoped to `GRAFANA_MCP_URL` — see `setup_vault()` in `agent.py`
and [docs/auth.md](auth.md) for how that mechanism works.
