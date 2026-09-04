# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
"""
Everything pre-supplied: the agent's system prompt, tool declarations, and the
chat UI. agent.py imports from here; you generally don't need to edit this file.
"""
import json
import os
import time
from datetime import datetime, timezone

import streamlit as st

# Scope the agent to a single namespace. Set K8S_NAMESPACE="" to lift the
# restriction and let the agent investigate any namespace it finds.
K8S_NAMESPACE = os.environ.get("K8S_NAMESPACE", "scenario-01")

SYSTEM_PROMPT = f"""You are an SRE agent investigating a live Kubernetes cluster. You have READ-ONLY access to the cluster and its observability stack through tools:
- Kubernetes tools: list and inspect namespaces, pods, deployments, services, events, and pod logs. Resource queries take a namespace argument.
- Grafana tools (if available): query Prometheus metrics, Loki logs, and Tempo traces.

Investigate the ACTUAL cluster with these tools. You have no pre-loaded data — do not rely on prior assumptions.

Your scope is fixed to the `{K8S_NAMESPACE}` namespace. Never list, inspect, or query resources, events, metrics, or logs from any other namespace, even if a tool would let you and even if a dependency appears to live elsewhere — restrict every investigation to what `{K8S_NAMESPACE}` contains.

Approach:
1. Identify the affected workload within `{K8S_NAMESPACE}`.
2. Check workload state via Kubernetes tools: restarts, CrashLoopBackOff, pending, OOMKills, readiness.
3. Check recent Kubernetes events for `{K8S_NAMESPACE}`.
4. Pull logs for the failing workload AND its in-namespace dependencies, filtering for errors.
5. Correlate across workloads within the namespace: a symptom in one service is often caused by a dependency. Distinguish the failing service from the underlying cause.

Rules:
- Every factual claim (a metric value, an error, a restart count) MUST come from a tool call. If data is not available through your tools, say so explicitly. Never invent numbers or guess.
- Every query targets the `{K8S_NAMESPACE}` namespace — never omit it, never substitute another.
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


# Causely notifications arrive as an ordinary user.message (see webhook.py's
# _investigation_message) — there's no separate event type for them at the
# Managed Agents API level. This prefix is how the UI tells "Causely pushed
# this" apart from "a person typed this", so it can render the two
# differently (see _render_message). webhook.py imports this constant rather
# than hardcoding the string so the two stay in sync.
NOTIFICATION_PREFIX = "Causely fired a notification:"

# Placeholder glyph for a message whose timestamp is missing (e.g. an old
# persisted session from before timestamps existed) — deliberately not
# now(), which would misrepresent when the message actually happened.
_EM_SPACE = " "

_KIND_LABEL = {"agent": "AGENT", "notification": "CAUSELY NOTIFICATION"}  # "user" gets no label


def _iso(dt) -> str | None:
    """UTC timestamp -> ISO-8601 string, or None if the event has none."""
    return dt.isoformat() if dt else None


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _load_history(session_id: str):
    """Replay a session's conversation from the server-side event log.

    Each entry carries the message's ORIGINAL creation time as reported by
    the API (`processed_at` on the event) rather than anything computed at
    render time, so reloading a session shows when things actually happened.
    """
    import agent
    hist: list[dict] = []
    for ev in agent.client.beta.sessions.events.list(session_id, order="asc", limit=500).data:
        if ev.type == "user.message":
            text = _text(ev.content)
            kind = "notification" if text.startswith(NOTIFICATION_PREFIX) else "user"
            hist.append({"role": "user", "kind": kind, "text": text, "ts": _iso(ev.processed_at)})
        elif ev.type == "agent.message":
            txt = _text(ev.content)
            if hist and hist[-1]["kind"] == "agent":
                hist[-1]["text"] += txt
            else:
                hist.append({"role": "assistant", "kind": "agent", "text": txt, "ts": _iso(ev.processed_at)})
        elif ev.type == "agent.tool_use":
            line = f"\n\n`{ev.name}`"
            if hist and hist[-1]["kind"] == "agent":
                hist[-1]["text"] += line
            else:
                hist.append({
                    "role": "assistant", "kind": "agent", "text": line,
                    "ts": _iso(getattr(ev, "processed_at", None)),
                })
    return hist


def _text(content) -> str:
    if not content:
        return ""
    return "".join(getattr(b, "text", "") for b in content if getattr(b, "type", None) == "text")


def _msg_header_html(kind: str, ts: str | None) -> str:
    """Small header row for one message: label (left) + timestamp (right).

    The timestamp is rendered server-side only as a <time data-utc=...>
    placeholder — _inject_dynamic_behavior() fills in the actual text in the
    viewer's local timezone, since that's a browser-side fact the server
    can't know.
    """
    label = _KIND_LABEL.get(kind, "")
    label_html = f'<span class="msg-label">{label}</span>' if label else ""
    return (
        '<div class="msg-head">'
        f'{label_html}'
        f'<time class="msg-ts" data-utc="{ts or ""}">{_EM_SPACE}</time>'
        '</div>'
    )


def _render_message(idx, msg: dict):
    """Render one transcript entry: header row (label + timestamp) grouped
    with the actual chat bubble inside a keyed container, so CSS can target
    `__kind-{notification,agent,user}` to give each its distinct treatment
    (see assets/style.css)."""
    with st.container(key=f"m{idx}__kind-{msg['kind']}"):
        st.markdown(_msg_header_html(msg["kind"], msg["ts"]), unsafe_allow_html=True)
        with st.chat_message(msg["role"]):
            st.markdown(msg["text"])


# TODO: no per-tool logo assets exist in this repo yet (e.g.
# assets/causely-logo.svg, assets/grafana-logo.svg, ...) — one shared
# placeholder mark is used for every active chip below so the four read as
# equally available rather than singling one out. Swap in real per-tool
# logos here if/when they're added.
_TOOL_ICON_SVG = (
    '<svg viewBox="0 0 24 24" width="13" height="13" aria-hidden="true">'
    '<circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="2"/>'
    '<path d="M12 7v5l3.5 2" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round"/>'
    '</svg>'
)


def _tool_chip(label: str, *, active: bool) -> str:
    """`active` chips (this tool is actually wired up) all get the identical
    border + icon treatment — no chip should look more "available" than
    another when all of them are. A chip that isn't active (currently only
    possible for Causely, via ENABLE_CAUSELY) renders plain: no border, no
    icon, rather than being omitted."""
    if not active:
        return f'<span class="tool-chip">{label}</span>'
    icon = f'<span class="chip-icon">{_TOOL_ICON_SVG}</span>'
    return f'<span class="tool-chip tool-chip--active">{icon}{label}</span>'


def _available_tools_strip():
    """Descriptive-only strip of what's configured for this session — no
    live health checks or polling, just what the env plumbing says is wired
    up (same env vars agent.py gates the actual MCP servers/tools on).
    Same "##### " heading level as the AGENT header it sits beside, so the
    two match in size/weight — see the shared h5 rule in style.css."""
    st.markdown("##### AVAILABLE TOOLS")
    st.caption("configured for this session")

    causely_on = os.environ.get("ENABLE_CAUSELY") == "1"
    chips = [
        _tool_chip("Causely MCP", active=causely_on),
        _tool_chip("Grafana MCP: traces, metrics, logs", active=True),
        _tool_chip("K8s MCP", active=True),
        _tool_chip("GitHub repo access", active=True),
    ]
    st.markdown(f'<div class="tools-strip">{"".join(chips)}</div>', unsafe_allow_html=True)


def _inject_dynamic_behavior():
    """Browser-only behavior no server-side render can do:
      1. format each message's stored UTC timestamp in the viewer's own
         timezone (Intl.DateTimeFormat needs to run where the viewer is).
      2. autoscroll the transcript to a new message only when the viewer was
         already near the bottom; otherwise show a "jump to latest" button
         instead of yanking their scroll position.
    Runs via components.v1.html, which (unlike st.markdown(unsafe_allow_html))
    actually executes <script> tags, in an iframe that's same-origin with the
    app, so it can reach into window.parent.document. The leading comment
    embeds a fresh value every call so Streamlit treats this as changed
    content and re-runs it on every script run — a byte-identical string
    would only execute once.
    """
    html = f"""
<script>
/* tick:{time.time()} */
(function() {{
  var doc = window.parent.document;

  // ---- 1. timestamps: stored UTC -> viewer's local time ----
  var now = new Date();
  doc.querySelectorAll('time.msg-ts[data-utc]').forEach(function(el) {{
    var raw = el.getAttribute('data-utc');
    if (!raw) return;  // no stored timestamp — keep the em-space placeholder
    var d = new Date(raw);
    if (isNaN(d.getTime())) return;
    var sameDay = d.getFullYear() === now.getFullYear() &&
                  d.getMonth() === now.getMonth() &&
                  d.getDate() === now.getDate();
    var time_ = new Intl.DateTimeFormat(undefined, {{
      hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false
    }}).format(d);
    if (sameDay) {{
      el.textContent = time_;
    }} else {{
      var day = new Intl.DateTimeFormat(undefined, {{month: 'short', day: 'numeric'}}).format(d);
      el.textContent = day + ', ' + time_;
    }}
  }});

  // ---- 2. autoscroll transcript, with a "jump to latest" escape hatch ----
  var pane = doc.querySelector('[class*="st-key-chat_pane"]');
  if (!pane) return;

  var state = window.parent.__causelyChat || (window.parent.__causelyChat = {{
    nearBottom: true, lastCount: -1
  }});
  var NEAR_PX = 64;
  function isNearBottom() {{
    return pane.scrollHeight - pane.scrollTop - pane.clientHeight < NEAR_PX;
  }}

  var btn = doc.getElementById('chat-jump-latest');
  if (!btn) {{
    btn = doc.createElement('button');
    btn.id = 'chat-jump-latest';
    btn.type = 'button';
    btn.textContent = '↓ jump to latest';
    doc.body.appendChild(btn);
  }}
  function positionButton() {{
    var r = pane.getBoundingClientRect();
    btn.style.position = 'fixed';
    btn.style.left = Math.max(r.left + 12, r.right - 168) + 'px';
    btn.style.top = (r.bottom - 44) + 'px';
    btn.style.zIndex = 999999;
  }}

  // Assign handlers as PROPERTIES (.onscroll/.onclick/.onresize), not
  // addEventListener, and do it on every tick rather than once-guarded.
  // This script runs inside a components.v1.html iframe that Streamlit
  // tears down and replaces on every rerun — a listener attached via
  // addEventListener from one tick's closure goes silently dead once its
  // originating iframe is gone (the button/pane themselves live on in the
  // parent document, but that closure doesn't). Property assignment always
  // overwrites with a closure from the CURRENT, still-alive tick.
  pane.onscroll = function() {{
    state.nearBottom = isNearBottom();
    if (state.nearBottom) btn.style.display = 'none';
  }};
  btn.onclick = function() {{
    var live = doc.querySelector('[class*="st-key-chat_pane"]');
    if (live) live.scrollTop = live.scrollHeight;
    state.nearBottom = true;
    btn.style.display = 'none';
  }};
  window.onresize = positionButton;

  var count = pane.querySelectorAll('[data-testid="stChatMessage"]').length;
  var grew = count > state.lastCount;
  state.lastCount = count;

  positionButton();
  if (grew) {{
    if (state.nearBottom) {{
      pane.scrollTop = pane.scrollHeight;
      btn.style.display = 'none';
    }} else {{
      btn.style.display = 'block';
    }}
  }}
}})();
</script>
"""
    st.components.v1.html(html, height=0)


def chat_panel():
    import agent  # lazy import to avoid circular dependency

    # AVAILABLE TOOLS sits top-right, vertically aligned with AGENT (same
    # row, same "##### " heading level) instead of buried below it.
    agent_col, tools_col = st.columns([3, 2])
    with agent_col:
        st.markdown("##### AGENT")
        agent_id = agent.setup_agent()
        st.caption(f"agent · `{agent_id}`")
        env_id = agent.setup_environment()
        st.caption(f"env · `{env_id}`")
    with tools_col:
        _available_tools_strip()

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

    # height="stretch" (not a fixed pixel height) fills whatever room the
    # flex layout in assets/style.css gives .st-key-chat_pane — no clipping,
    # no dead space. That CSS is also where the actual overflow-y:auto and
    # autoscroll-vs-jump-to-latest behavior (_inject_dynamic_behavior) attach.
    chat = st.container(key="chat_pane", height="stretch", border=False)
    with chat:
        for idx, msg in enumerate(st.session_state.hist):
            _render_message(idx, msg)

    if q := st.chat_input("ask the agent…"):
        user_msg = {"role": "user", "kind": "user", "text": q, "ts": _now_iso()}
        st.session_state.hist.append(user_msg)
        with chat:
            _render_message(len(st.session_state.hist) - 1, user_msg)

            agent_idx = len(st.session_state.hist)
            with st.container(key=f"m{agent_idx}__kind-agent"):
                head_ph = st.empty()
                agent_ts = None
                with st.chat_message("assistant"):
                    text_ph = st.empty()
                    buf = ""
                    tool_boxes: dict[str, object] = {}
                    for ev in agent.stream_reply(st.session_state.sid, q):
                        if agent_ts is None:
                            processed_at = getattr(ev, "processed_at", None)
                            if processed_at:
                                agent_ts = _iso(processed_at)
                                head_ph.markdown(_msg_header_html("agent", agent_ts), unsafe_allow_html=True)
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
        st.session_state.hist.append({"role": "assistant", "kind": "agent", "text": buf, "ts": agent_ts})

    _inject_dynamic_behavior()
