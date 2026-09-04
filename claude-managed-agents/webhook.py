# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
"""
Turns Causely notifications into agent investigations.

Causely POSTs its own notification payload verbatim to /webhook/causely (no
templating on its side — see docs/causely-webhook.md). This authenticates the
call, points the agent at the Causely object id (get_issue_details /
get_diagnosis_details) so it starts from a known cause instead of re-deriving
one, starts a session, and lets the investigation run in the background —
nobody is watching a webhook-triggered investigation live. Review results the
same way as any other session, in `streamlit run app.py`'s session picker:
setup_agent()/setup_environment() in agent.py resolve to the same cloud
agent/environment regardless of which process (this or app.py) calls them.

Run locally with scripts/run-webhook.sh, which also opens a cloudflared
tunnel so Causely (a public SaaS you don't control the network of) can reach
this process.
"""
import hmac
import logging
import os

from dotenv import load_dotenv

load_dotenv()

from fastapi import BackgroundTasks, FastAPI, Header, HTTPException, Request

import agent
from provided import NOTIFICATION_PREFIX

logging.basicConfig(level=logging.INFO)
LOG = logging.getLogger("webhook")

WEBHOOK_SECRET = os.environ["CAUSELY_WEBHOOK_SECRET"]

app = FastAPI()

# Causely's object_type decides which Causely MCP tool resolves objectId into
# a causal chain: an Issue is the incident-level view (a group of diagnoses
# with one designated primary), a defect is a single diagnosis beneath it.
# Configure Causely's notification to send Issues, not defects — see
# docs/causely-webhook.md. An absent or unrecognised object_type is treated
# as an Issue.
OBJECT_TYPE_TOOL = {
    "issue": ("get_issue_details", "issue_id"),
    "defect": ("get_diagnosis_details", "diagnosis_id"),
}
DEFAULT_OBJECT_TYPE = "issue"


def _authorized(authorization: str | None) -> bool:
    # Causely sends its configured token verbatim as the Authorization
    # header, so "Bearer <token>" is the expected form. A bare "<token>" is
    # accepted too — forgetting the scheme is an easy misconfiguration whose
    # only symptom is a silent 401 and a missing investigation.
    presented = (authorization or "").strip()
    if not presented:
        return False
    scheme, _, remainder = presented.partition(" ")
    if remainder.strip():
        if scheme.lower() != "bearer":
            return False
        candidate = remainder.strip()
    else:
        candidate = presented
    return hmac.compare_digest(candidate, WEBHOOK_SECRET)


def _describe(causely: dict) -> str:
    """Flatten Causely's structured description into plain text, if present."""
    description = causely.get("description")
    if isinstance(description, str):
        return description
    if not isinstance(description, dict):
        return ""
    sections = [description.get("summary"), description.get("details")]
    return "\n\n".join(s for s in sections if s)


def _investigation_message(causely: dict) -> str:
    """Translate a Causely notification into the agent's first user message."""
    entity = causely.get("entity") or {}
    name = causely.get("name") or "Causely notification"
    entity_name = entity.get("name")
    object_id = causely.get("objectId")
    object_type = str(causely.get("object_type") or "").lower()
    severity = causely.get("severity") or "unknown"

    title = f"{name} on {entity_name}" if entity_name else name
    lines = [f"{NOTIFICATION_PREFIX} {title}", f"severity: {severity}"]

    description = _describe(causely)
    if description:
        lines.append(f"description: {description}")

    if object_id:
        tool, param = OBJECT_TYPE_TOOL.get(object_type, OBJECT_TYPE_TOOL[DEFAULT_OBJECT_TYPE])
        lines.append(
            f"Causely has already identified the root cause. Call the Causely "
            f"MCP tool {tool} with {param} '{object_id}' FIRST to retrieve the "
            f"full causal chain, affected entities, and blast radius before "
            f"looking at anything else."
        )
    else:
        lines.append("No Causely object id was provided on this notification.")

    lines.append("Investigate and report the root cause and a mitigation plan.")
    return "\n".join(lines)


def _run_investigation(message: str) -> None:
    agent_id = agent.setup_agent()
    env_id = agent.setup_environment()
    session_id = agent.start_session(agent_id, env_id)
    LOG.info("started session %s for Causely notification", session_id)
    try:
        for ev in agent.stream_reply(session_id, message):
            if ev.type == "session.status_idle":
                LOG.info("session %s finished: %s", session_id, ev.stop_reason.type)
                return
    except Exception:
        LOG.exception("investigation failed for session %s", session_id)


@app.get("/healthz")
def healthz():
    return {"ok": True}


@app.post("/webhook/causely")
async def causely_webhook(
    request: Request,
    background_tasks: BackgroundTasks,
    authorization: str | None = Header(default=None),
):
    if not _authorized(authorization):
        LOG.warning("rejected request with missing or invalid bearer token")
        raise HTTPException(status_code=401, detail="unauthorized")

    try:
        causely = await request.json()
    except Exception as exc:
        raise HTTPException(status_code=400, detail=f"invalid JSON body: {exc}") from exc
    if not isinstance(causely, dict):
        raise HTTPException(status_code=400, detail="expected a JSON object")

    LOG.info(
        "received causely notification type=%s severity=%s objectId=%s",
        causely.get("type"), causely.get("severity"), causely.get("objectId"),
    )

    message = _investigation_message(causely)
    background_tasks.add_task(_run_investigation, message)
    return {"received": True}
