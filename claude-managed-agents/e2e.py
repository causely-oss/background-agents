# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
"""
Headless smoke test of the k8s MCP path — no Streamlit, no vault (k8s needs
none). Requires K8S_MCP_URL in .env, pointed at a running
scripts/run-k8s-mcp.sh tunnel over a kind cluster with kind/workload.yaml
applied.
"""
import os
import sys
import time
import uuid

import anthropic
from dotenv import load_dotenv

load_dotenv()
client = anthropic.Anthropic()

K8S_MCP_URL = os.environ["K8S_MCP_URL"]

SYSTEM = (
    "You are an SRE agent with READ-ONLY access to a Kubernetes cluster through "
    "MCP tools. Every factual claim must come from a tool call."
)
TOOLS = [
    {"type": "agent_toolset_20260401", "default_config": {"enabled": True}},
    {"type": "mcp_toolset", "mcp_server_name": "k8s",
     "default_config": {"enabled": True, "permission_policy": {"type": "always_allow"}}},
]

agent = client.beta.agents.create(
    name="e2e-smoke-test", model="claude-opus-4-7", system=SYSTEM, tools=TOOLS,
    mcp_servers=[{"type": "url", "name": "k8s", "url": K8S_MCP_URL}],
)
env = client.beta.environments.create(
    name=f"e2e-{uuid.uuid4().hex[:6]}",
    config={"type": "cloud", "networking": {"type": "unrestricted"}},
)
session = client.beta.sessions.create(agent=agent.id, environment_id=env.id)
print(f"agent={agent.id} env={env.id} session={session.id}")

transcript = []
tool_calls = 0
deadline = time.monotonic() + 300
with client.beta.sessions.events.stream(session.id) as stream:
    client.beta.sessions.events.send(
        session.id,
        events=[{"type": "user.message", "content": [
            {"type": "text", "text": "List the namespaces and pods you can see, "
                                      "then point out anything that looks unhealthy."},
        ]}],
    )
    for ev in stream:
        if time.monotonic() > deadline:
            print("\n!! timeout"); break
        if ev.type == "agent.message":
            for b in ev.content:
                transcript.append(b.text); print(b.text, end="", flush=True)
        elif ev.type == "agent.tool_use":
            tool_calls += 1
            print(f"\n[tool · {ev.name}]")
        elif ev.type == "session.status_idle" and ev.stop_reason.type == "end_turn":
            print("\n-- end_turn --"); break

client.beta.sessions.delete(session.id)

ok = tool_calls > 0 and bool("".join(transcript).strip())
print("\nverdict:", "PASS" if ok else "FAIL", f"({tool_calls} tool call(s))")
sys.exit(0 if ok else 1)
