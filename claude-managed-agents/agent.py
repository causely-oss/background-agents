# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
"""
The SRE Agent. Six functions, each built around a single Managed Agents API
call, wiring a Claude Managed Agent up to remote MCP servers. See provided.py
for the system prompt, tool declarations, and chat UI.
"""
import os

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

# Optional — mount a GitHub repo checkout into the agent's sandbox. Leave
# GITHUB_REPO_URL empty to skip this entirely; GITHUB_BRANCH is optional too
# (omitted, the session checks out the repo's default branch).
GITHUB_REPO_URL = os.environ.get("GITHUB_REPO_URL", "")
GITHUB_TOKEN = os.environ.get("GITHUB_TOKEN", "")
GITHUB_BRANCH = os.environ.get("GITHUB_BRANCH", "")
GITHUB_MOUNT_PATH = os.environ.get("GITHUB_MOUNT_PATH", "/workspace/repo")


AGENT_NAME = "SRE Agent"
ENVIRONMENT_NAME = "sre-agent"


# ── 1. Agent ──────────────────────────────────────────────────────────────
# What the agent IS: model, system prompt, tools, and the remote MCP servers
# it's allowed to call. Create once, reuse forever — found by name rather
# than just cached client-side, so a second process (e.g. webhook.py running
# alongside app.py) converges on the SAME cloud agent instead of minting a
# duplicate. If one already exists, its config is synced to the current
# SYSTEM_PROMPT/TOOLS/mcp_servers on every fetch, so a still-running process
# never serves a stale tool set after you add a server and restart.
@st.cache_resource
def setup_agent() -> str:
    existing = _find_by_name(client.beta.agents.list(limit=100).data, AGENT_NAME)
    if existing is not None:
        updated = client.beta.agents.update(
            existing.id, version=existing.version,
            system=SYSTEM_PROMPT, tools=TOOLS, mcp_servers=_mcp_servers(),
        )
        return updated.id
    created = client.beta.agents.create(
        name=AGENT_NAME, model="claude-opus-4-7", system=SYSTEM_PROMPT,
        tools=TOOLS, mcp_servers=_mcp_servers(),
    )
    return created.id


def _find_by_name(items, name: str):
    for item in items:
        if item.name == name:
            return item
    return None


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
# Where the agent's container runs. Create once, reuse forever — same
# find-by-name-before-create pattern as setup_agent(), for the same reason:
# a fixed name lets every process land on one shared environment instead of
# each restart (or each separate process) minting a new container.
@st.cache_resource
def setup_environment() -> str:
    existing = _find_by_name(client.beta.environments.list(limit=100).data, ENVIRONMENT_NAME)
    if existing is not None:
        return existing.id
    created = client.beta.environments.create(
        name=ENVIRONMENT_NAME,
        config={"type": "cloud", "networking": {"type": "unrestricted"}},
    )
    return created.id


# ── 4. Session ────────────────────────────────────────────────────────────
# Bind agent + environment + a freshly-built vault. The vault is rebuilt per
# session (not cached) because the Causely token inside it is a short-lived
# Frontegg JWT — baking it into a long-lived cached resource would leave the
# session working for an hour and then silently 401ing.
#
# `resources` is separate from `mcp_servers`/vaults above: instead of giving
# the agent tools to call a remote service, it checks a GitHub repo out
# straight into the sandbox filesystem at `mount_path`, on the branch named
# by `checkout` (or the repo's default branch if GITHUB_BRANCH is unset).
def start_session(agent_id: str, env_id: str) -> str:
    kwargs = {}
    if GITHUB_REPO_URL:
        kwargs["resources"] = [_github_repository_resource()]
    session = client.beta.sessions.create(
        agent=agent_id,
        environment_id=env_id,
        vault_ids=[setup_vault()],
        **kwargs,
    )
    return session.id


def _github_repository_resource() -> dict:
    # The API wants https://github.com/{owner}/{repo} exactly — no .git suffix
    # — but that's the form git clone URLs and GitHub's own "Copy" button use,
    # so strip it here rather than trip up everyone who pastes one into .env.
    url = GITHUB_REPO_URL.removesuffix(".git")
    resource = {
        "type": "github_repository",
        "url": url,
        "mount_path": GITHUB_MOUNT_PATH,
    }
    if GITHUB_TOKEN:
        resource["authorization_token"] = GITHUB_TOKEN
    if GITHUB_BRANCH:
        resource["checkout"] = {"type": "branch", "name": GITHUB_BRANCH}
    return resource


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
