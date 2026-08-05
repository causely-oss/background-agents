# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
"""
The SRE Agent. Six functions, each built around a single Managed Agents API
call, wiring a Claude Managed Agent up to remote MCP servers. See provided.py
for the system prompt, tool declarations, and chat UI.
"""
import os
import uuid

import anthropic
import httpx
import streamlit as st

from provided import SYSTEM_PROMPT, TOOLS

client = anthropic.Anthropic()

# Any remote MCP URL works here as-is — including an MCP tunnel hostname
# (see docs/mcp-tunnels.md), if you set one instead of running scripts/run-k8s-mcp.sh.
K8S_MCP_URL = os.environ.get("K8S_MCP_URL", "")
GRAFANA_MCP_URL = os.environ.get("GRAFANA_MCP_URL", "")
CAUSELY_MCP_URL = os.environ.get("CAUSELY_MCP_URL", "")


# ── 1. Agent ──────────────────────────────────────────────────────────────
# What the agent IS: model, system prompt, tools, and the remote MCP servers
# it's allowed to call. Create once, reuse forever.
@st.cache_resource
def setup_agent() -> str:
    agent = client.beta.agents.create(
        name="SRE Agent", model="claude-opus-4-7", system=SYSTEM_PROMPT,
        tools=TOOLS,
        mcp_servers=_mcp_servers(),
    )
    return agent.id


def _mcp_servers() -> list[dict]:
    # Every server listed here needs a matching mcp_toolset entry in
    # provided.TOOLS (same name, same gating condition) or agent creation
    # fails with a 400.
    servers = [{"type": "url", "name": "k8s", "url": K8S_MCP_URL}]
    if GRAFANA_MCP_URL:
        servers.append({"type": "url", "name": "grafana", "url": GRAFANA_MCP_URL})
    if os.environ.get("ENABLE_CAUSELY") == "1":
        servers.append({"type": "url", "name": "causely", "url": CAUSELY_MCP_URL})
    return servers


# ── 2. Vault ──────────────────────────────────────────────────────────────
# MCP credentials aren't attached to the server entry above — they live in a
# vault, referenced at session creation, matched to a server by exact URL.
# The MCP connector only ever sends `Authorization: Bearer <token>`; it can't
# send custom headers or Basic auth, so every credential here is static_bearer.
# k8s needs no credential (it's just reached over an unauthenticated tunnel).
def setup_vault() -> str:
    vault = client.beta.vaults.create(display_name="mcp-creds")

    if GRAFANA_MCP_URL and os.environ.get("GRAFANA_TOKEN"):
        client.beta.vaults.credentials.create(
            vault_id=vault.id,
            auth={
                "type": "static_bearer",
                "mcp_server_url": GRAFANA_MCP_URL,
                "token": os.environ["GRAFANA_TOKEN"],
            },
        )

    if os.environ.get("ENABLE_CAUSELY") == "1":
        client.beta.vaults.credentials.create(
            vault_id=vault.id,
            auth={
                "type": "static_bearer",
                "mcp_server_url": CAUSELY_MCP_URL,
                "token": _fetch_causely_access_token(),
            },
        )

    return vault.id


def _fetch_causely_access_token() -> str:
    # Causely's MCP server accepts a Bearer JWT from its own Frontegg tenant
    # directly, so we mint one ourselves with client-credentials and hand it
    # to the vault as a static_bearer — no proxy, no custom header needed.
    # CAUSELY_TOKEN_URL is your tenant's token endpoint; ask your Causely
    # contact for the exact value and request shape if this default doesn't
    # match it.
    resp = httpx.post(
        os.environ["CAUSELY_TOKEN_URL"],
        json={
            "clientId": os.environ["CAUSELY_CLIENT_ID"],
            "secret": os.environ["CAUSELY_CLIENT_SECRET"],
        },
        timeout=10,
    )
    resp.raise_for_status()
    body = resp.json()
    for key in ("access_token", "accessToken", "token"):
        if key in body:
            return body[key]
    raise KeyError(
        "No access token found in Causely token response "
        f"(expected one of access_token/accessToken/token): {body!r}"
    )


# ── 3. Environment ────────────────────────────────────────────────────────
# Where the agent's container runs. Create once, reuse forever.
@st.cache_resource
def setup_environment() -> str:
    env = client.beta.environments.create(
        name=f"sre-agent-{uuid.uuid4().hex[:6]}",
        config={"type": "cloud", "networking": {"type": "unrestricted"}},
    )
    return env.id


# ── 4. Session ────────────────────────────────────────────────────────────
# Bind agent + environment + a freshly-built vault. The vault is rebuilt per
# session (not cached) because the Causely token inside it is a short-lived
# Frontegg JWT — baking it into a long-lived cached resource would leave the
# session working for an hour and then silently 401ing.
def start_session(agent_id: str, env_id: str) -> str:
    session = client.beta.sessions.create(
        agent=agent_id,
        environment_id=env_id,
        vault_ids=[setup_vault()],
    )
    return session.id


# ── 5. Stream loop ────────────────────────────────────────────────────────
# Open the event stream, send the user's message, yield events. With real MCP
# servers there are no local tools to service — just render the stream.
def stream_reply(session_id: str, user_text: str):
    with client.beta.sessions.events.stream(session_id) as stream:
        client.beta.sessions.events.send(
            session_id,
            events=[{"type": "user.message", "content": [{"type": "text", "text": user_text}]}],
        )
        yield from stream


# ── 6. Delete session ─────────────────────────────────────────────────────
# Sessions are real cloud resources — clean them up.
def delete_session(session_id: str) -> None:
    client.beta.sessions.delete(session_id)
