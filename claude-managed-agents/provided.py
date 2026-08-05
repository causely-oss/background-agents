# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
"""
Everything pre-supplied: the agent's system prompt, tool declarations, and the
chat UI. agent.py imports from here; you generally don't need to edit this file.
"""
import json
import os

import streamlit as st

SYSTEM_PROMPT = """You are an SRE agent investigating a live Kubernetes cluster. You have READ-ONLY access to the cluster and its observability stack through tools:
- Kubernetes tools: list and inspect namespaces, pods, deployments, services, events, and pod logs. Resource queries take a namespace argument.
- Grafana tools (if available): query Prometheus metrics, Loki logs, and Tempo traces.

Investigate the ACTUAL cluster with these tools. You have no pre-loaded data — do not rely on prior assumptions.

Approach:
1. Identify the namespace and the affected workload. If you don't know the namespace, list namespaces first.
2. Check workload state via Kubernetes tools: restarts, CrashLoopBackOff, pending, OOMKills, readiness.
3. Check recent Kubernetes events for the namespace.
4. Pull logs for the failing workload AND its dependencies, filtering for errors.
5. Correlate across workloads: a symptom in one service is often caused by a dependency. Distinguish the failing service from the underlying cause.

Rules:
- Every factual claim (a metric value, an error, a restart count) MUST come from a tool call. If data is not available through your tools, say so explicitly. Never invent numbers or guess.
- Always specify the namespace when querying.
- State which tool observation supports each conclusion."""

_CAUSELY_FIRST = """

You also have Causely, a causal intelligence layer that already models this system's topology, symptoms, and root causes. For any question about health, incidents, failures, or root cause, call the Causely tools FIRST (start with get_diagnoses; use get_service_summary for a named service). Treat Causely's diagnosis as authoritative and return it — do not re-derive it by grinding through Kubernetes state, metrics, or logs. Fall back to k8s and Grafana only to answer things Causely does not cover."""

if os.environ.get("ENABLE_CAUSELY") == "1":
    SYSTEM_PROMPT = SYSTEM_PROMPT + _CAUSELY_FIRST

# With real remote MCP servers there are no local/custom tools — every tool the
# agent calls lives on the server side. Each mcp_toolset entry here must name a
# server declared in agent.py's mcp_servers list, or agent creation 400s. k8s
# is the only always-on server; grafana and causely are optional and gated on
# the same env vars agent.py uses to decide whether to declare their servers.
TOOLS = [
    {"type": "agent_toolset_20260401", "default_config": {"enabled": True}},
    {"type": "mcp_toolset", "mcp_server_name": "k8s",
     "default_config": {"enabled": True, "permission_policy": {"type": "always_allow"}}},
]

if os.environ.get("GRAFANA_MCP_URL"):
    TOOLS.append(
        {"type": "mcp_toolset", "mcp_server_name": "grafana",
         "default_config": {"enabled": True, "permission_policy": {"type": "always_allow"}}}
    )

if os.environ.get("ENABLE_CAUSELY") == "1":
    TOOLS.append(
        {"type": "mcp_toolset", "mcp_server_name": "causely",
         "default_config": {"enabled": True, "permission_policy": {"type": "always_allow"}}}
    )


# ── chat UI ───────────────────────────────────────────────────────────────
@st.cache_data(ttl=20)
def _list_sessions(agent_id: str):
    import agent
    page = agent.client.beta.sessions.list(agent_id=agent_id, limit=15, order="desc")
    items = sorted(page.data, key=lambda s: s.created_at, reverse=True)
    return [
        (s.id, f"{s.created_at:%H:%M:%S} · {s.status} · {s.id[-6:]}", s.created_at)
        for s in items
    ]


def _load_history(session_id: str):
    """Replay a session's conversation from the server-side event log."""
    import agent
    hist: list[tuple[str, str]] = []
    for ev in agent.client.beta.sessions.events.list(session_id, order="asc", limit=500).data:
        if ev.type == "user.message":
            hist.append(("user", _text(ev.content)))
        elif ev.type == "agent.message":
            txt = _text(ev.content)
            if hist and hist[-1][0] == "assistant":
                hist[-1] = ("assistant", hist[-1][1] + txt)
            else:
                hist.append(("assistant", txt))
        elif ev.type == "agent.tool_use":
            line = f"\n\n`{ev.name}`"
            if hist and hist[-1][0] == "assistant":
                hist[-1] = ("assistant", hist[-1][1] + line)
            else:
                hist.append(("assistant", line))
    return hist


def _text(content) -> str:
    if not content:
        return ""
    return "".join(getattr(b, "text", "") for b in content if getattr(b, "type", None) == "text")


def chat_panel():
    import agent  # lazy import to avoid circular dependency

    st.markdown("##### AGENT")

    agent_id = agent.setup_agent()
    st.caption(f"agent · `{agent_id}`")

    env_id = agent.setup_environment()
    st.caption(f"env · `{env_id}`")

    # ── session picker: sessions are stateful + persisted server-side.
    # Never auto-create — resume the newest existing one, or show an empty state.
    if "sid" not in st.session_state:
        listed = _list_sessions(agent_id)
        if listed:
            st.session_state.sid = listed[0][0]
            st.session_state.hist = _load_history(st.session_state.sid)
    sid = st.session_state.get("sid")

    listed = _list_sessions(agent_id)
    labels = {s: l for s, l, _ in listed}
    ids = [s for s, _, _ in listed]
    if sid and sid not in labels:
        ids.insert(0, sid)
        labels[sid] = "just now · current"

    def _on_pick():
        chosen = st.session_state.session_picker
        st.session_state.sid = chosen
        st.session_state.hist = _load_history(chosen)

    if sid:
        st.session_state.session_picker = sid

    pick_col, new_col, del_col = st.columns([6, 1, 1])
    pick_col.selectbox(
        "session", ids, format_func=lambda v: labels.get(v, v), disabled=not ids,
        label_visibility="collapsed", key="session_picker", on_change=_on_pick,
    )
    if new_col.button("", icon=":material/add:", help="new session", use_container_width=True):
        st.session_state.sid = agent.start_session(agent_id, env_id)
        st.session_state.hist = []
        _list_sessions.clear()
        st.rerun()
    if del_col.button("", icon=":material/delete:", help="delete session",
                      use_container_width=True, disabled=not sid):
        agent.delete_session(st.session_state.sid)
        _list_sessions.clear()
        del st.session_state["sid"]
        st.rerun()

    if not sid:
        st.caption("no sessions — click **+** to start one")
        st.chat_input("ask…", disabled=True, key="off_nosession")
        return

    st.caption(f"`{sid}` — persisted in the cloud, not this browser")

    chat = st.container(height=400, border=False)
    with chat:
        for role, text in st.session_state.hist:
            with st.chat_message(role):
                st.markdown(text)

    if q := st.chat_input("ask the agent…"):
        st.session_state.hist.append(("user", q))
        with chat:
            with st.chat_message("user"):
                st.markdown(q)
            with st.chat_message("assistant"):
                text_ph = st.empty()
                buf = ""
                tool_boxes: dict[str, object] = {}
                for ev in agent.stream_reply(st.session_state.sid, q):
                    if ev.type == "agent.message":
                        buf += "".join(b.text for b in ev.content)
                        text_ph.markdown(buf)
                    elif ev.type == "agent.tool_use":
                        box = st.status(ev.name, state="running")
                        args = json.dumps(ev.input)
                        box.caption("args")
                        box.code(args if args != "{}" else "(none)", language="json")
                        tool_boxes[ev.id] = box
                    elif ev.type == "agent.tool_result":
                        box = tool_boxes.pop(ev.tool_use_id, None)
                        if box:
                            box.caption("result")
                            box.code(_text(ev.content)[:1500] or "(empty)", language="text")
                            box.update(state="complete")
                    elif ev.type == "span.model_request_start":
                        text_ph = st.empty(); buf = ""
                    elif ev.type == "session.status_idle" and ev.stop_reason.type == "end_turn":
                        for b in tool_boxes.values():
                            b.update(state="complete")
                        break
        st.session_state.hist.append(("assistant", buf))
