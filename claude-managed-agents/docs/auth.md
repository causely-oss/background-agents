<!-- Copyright 2026 Anthropic PBC -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Auth: how Managed Agents talk to MCP servers

A Managed Agent's `mcp_servers` list only ever declares a `type`, `name`, and
`url` — there's no field on the server entry for credentials. Auth is bound
separately, at session creation, through a **vault**.

## The vault

```python
vault = client.beta.vaults.create(display_name="mcp-creds")
client.beta.vaults.credentials.create(
    vault_id=vault.id,
    auth={
        "type": "static_bearer",
        "mcp_server_url": "<exact server url>",
        "token": "<bearer token>",
    },
)
```

A credential is matched to a server **by exact URL string**, not by name. If
`mcp_server_url` doesn't match the declared server's `url` character-for-
character, the credential silently doesn't apply and the connector calls the
server unauthenticated instead of raising an error — worth knowing when
debugging a 401 that appears to come from nowhere.

The vault is passed into session creation, not agent creation:

```python
session = client.beta.sessions.create(
    agent=agent_id,
    environment_id=env_id,
    vault_ids=[vault.id],
)
```

## The connector only speaks Bearer

The MCP connector sends exactly one shape of credential to the server:
`Authorization: Bearer <token>`. It cannot send `Basic` auth or a custom
header. If your MCP server's native auth is Basic, an API key header, or
anything else, you need to exchange it for a bearer token yourself (see the
Causely example below) — there's no way to pass a different scheme through
the vault.

## Auth patterns used in this repo

| Server  | Pattern |
|---|---|
| k8s     | None — reached over an unauthenticated cloudflared tunnel. Fine for a local demo; don't do this for anything that isn't throwaway. |
| grafana | Static bearer: a Grafana service-account token, put straight into the vault as `static_bearer`. See [docs/grafana.md](grafana.md). |
| causely | OAuth client-credentials exchanged for a JWT, then that JWT goes into the vault as `static_bearer`. |

### Causely: client-credentials → JWT → static_bearer

Causely's MCP server validates a Bearer JWT natively — no reverse proxy or
custom header needed. The integration is:

1. POST your `CAUSELY_CLIENT_ID` / `CAUSELY_CLIENT_SECRET` to your tenant's
   OAuth token endpoint (`CAUSELY_TOKEN_URL` — ask your Causely contact for
   the exact URL and request shape; it's tenant-specific, not a fixed value).
2. Put the JWT it returns into the vault as a `static_bearer` credential for
   the Causely server's URL.

The JSON key holding the token in that response also varies by tenant and
endpoint version — some return `access_token`, others `accessToken` or
`token`. `_fetch_causely_access_token()` checks all three in that order and
raises with the full response body if none of them are present, so a shape
mismatch is a readable error instead of a `KeyError`.

See `_fetch_causely_access_token()` and `setup_vault()` in `agent.py`.

**These JWTs are short-lived.** `setup_vault()` mints one fresh per session
rather than baking it into a long-lived cached resource — cache it the way
`setup_agent()` caches the agent, and the integration works for about an
hour, then silently starts 401ing on every tool call.
