"""Relay Causely notifications into an AWS DevOps Agent webhook.

Causely forwards its own notification payload verbatim and offers no templating,
while the DevOps Agent webhook accepts only its own incident schema. This function
bridges the two: it authenticates the inbound call, translates the payload, and
re-signs the result with the HMAC scheme the agent webhook requires.

Causely's diagnosis id (``objectId``) and portal link are deliberately carried
through into ``data``. That is the hinge of the integration — it lets the agent call
Causely's ``get_diagnosis_details`` and inherit the causal chain instead of
re-deriving root cause from raw telemetry.

Only the standard library plus boto3 (present in the Lambda runtime) is used, so
there is nothing to vendor into the deployment package.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import logging
import os
import urllib.error
import urllib.request
from datetime import datetime, timezone
from typing import Any

import boto3

LOG = logging.getLogger()
LOG.setLevel(os.environ.get("LOG_LEVEL", "INFO"))

WEBHOOK_SECRET_ID = os.environ["WEBHOOK_SECRET_ID"]
INBOUND_SECRET_ID = os.environ["INBOUND_SECRET_ID"]
HTTP_TIMEOUT = float(os.environ.get("HTTP_TIMEOUT", "10"))

# When set, translate and log but do not actually call the agent webhook. Useful
# for exercising the relay without starting a billable investigation.
DRY_RUN = os.environ.get("DRY_RUN", "").lower() in {"1", "true", "yes"}

_secrets = boto3.client("secretsmanager")
_secret_cache: dict[str, str] = {}

SEVERITY_TO_PRIORITY = {
    "critical": "CRITICAL",
    "high": "HIGH",
    "medium": "MEDIUM",
    "low": "LOW",
}

# Causely emits ProblemDetected / ProblemCleared; the agent wants a lifecycle verb.
TYPE_TO_ACTION = {
    "problemdetected": "created",
    "problemcleared": "resolved",
}


def _now_iso() -> str:
    """UTC timestamp in the millisecond-precision ISO-8601 form the agent expects."""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def _secret(secret_id: str) -> str:
    if secret_id not in _secret_cache:
        _secret_cache[secret_id] = _secrets.get_secret_value(SecretId=secret_id)["SecretString"]
    return _secret_cache[secret_id]


def _webhook_config() -> tuple[str, str]:
    """The agent webhook url and HMAC secret, stored together as one JSON secret."""
    config = json.loads(_secret(WEBHOOK_SECRET_ID))
    return config["url"], config["secret"]


# ---------------------------------------------------------------------------
# Inbound request handling
# ---------------------------------------------------------------------------


def _header(event: dict, name: str) -> str | None:
    """Case-insensitive header lookup; Function URLs lowercase, but be tolerant."""
    target = name.lower()
    for key, value in (event.get("headers") or {}).items():
        if key.lower() == target:
            return value
    return None


def _authorized(event: dict) -> bool:
    """Validate the inbound credential.

    Causely sends its configured token verbatim as the Authorization header, so the
    expected form is "Bearer <token>". A bare "<token>" is accepted too: forgetting
    the scheme is an easy misconfiguration whose only symptom is a silent 401 and a
    missing investigation. Accepting both costs nothing — either way the value is
    compared against the same secret in constant time.
    """
    presented = (_header(event, "authorization") or "").strip()
    if not presented:
        return False

    scheme, _, remainder = presented.partition(" ")
    if remainder.strip():
        if scheme.lower() != "bearer":
            return False
        candidate = remainder.strip()
    else:
        candidate = presented

    return hmac.compare_digest(candidate, _secret(INBOUND_SECRET_ID).strip())


def _parse_body(event: dict) -> dict:
    raw = event.get("body") or ""
    if event.get("isBase64Encoded"):
        raw = base64.b64decode(raw).decode("utf-8")
    parsed = json.loads(raw)
    if not isinstance(parsed, dict):
        raise ValueError("expected a JSON object")
    return parsed


def _response(status: int, payload: dict) -> dict:
    return {
        "statusCode": status,
        "headers": {"Content-Type": "application/json"},
        "body": json.dumps(payload),
    }


# ---------------------------------------------------------------------------
# Translation: Causely notification -> DevOps Agent incident
# ---------------------------------------------------------------------------


def _describe(causely: dict) -> str:
    """Flatten Causely's structured description, remediation options included."""
    description: Any = causely.get("description")
    if isinstance(description, str):
        return description
    if not isinstance(description, dict):
        return ""

    sections = [description.get("summary"), description.get("details")]

    options = description.get("remediationOptions") or []
    if options:
        rendered = "\n".join(
            f"- {opt.get('title') or 'Option'}: {opt.get('description') or ''}".rstrip(": ")
            for opt in options
            if isinstance(opt, dict)
        )
        if rendered:
            sections.append("Remediation options suggested by Causely:\n" + rendered)

    return "\n\n".join(section for section in sections if section)


def to_incident(causely: dict) -> dict:
    """Map a Causely notification onto the DevOps Agent incident schema."""
    entity = causely.get("entity") or {}
    labels = causely.get("labels") or {}

    name = causely.get("name") or "Causely diagnosis"
    entity_name = entity.get("name")
    diagnosis_id = causely.get("objectId")

    cluster = labels.get("k8s.cluster.name") or labels.get("causely.ai/cluster")
    namespace = labels.get("k8s.namespace.name") or labels.get("causely.ai/namespace")

    context = {
        "source": "causely",
        # These two fields are what let the agent go back to Causely for the
        # causal chain rather than starting the investigation cold.
        "causelyDiagnosisId": diagnosis_id,
        "causelyDiagnosisName": name,
        "causelyLink": causely.get("link"),
        "causelyEventType": causely.get("type"),
        "causelyObjectType": causely.get("object_type"),
        "entityId": entity.get("id"),
        "entityName": entity_name,
        "entityType": entity.get("type"),
        "entityLink": entity.get("link"),
        "cluster": cluster,
        "namespace": namespace,
        "controllerKind": labels.get("k8s.controller.kind"),
    }
    if diagnosis_id:
        context["investigationHint"] = (
            "Causely has already identified the root cause. Call the Causely MCP tool "
            f"get_diagnosis_details with objectId '{diagnosis_id}' to retrieve the full "
            "causal chain, affected entities, and blast radius before proposing a fix."
        )

    incident = {
        "eventType": "incident",
        "incidentId": diagnosis_id or f"causely-{name}",
        "action": TYPE_TO_ACTION.get(str(causely.get("type") or "").lower(), "updated"),
        "priority": SEVERITY_TO_PRIORITY.get(str(causely.get("severity") or "").lower(), "MEDIUM"),
        "title": f"{name} on {entity_name}" if entity_name else name,
        "description": _describe(causely),
        "timestamp": causely.get("timestamp") or _now_iso(),
        "service": entity_name or namespace or "unknown",
        "data": {key: value for key, value in context.items() if value not in (None, "")},
    }
    return {key: value for key, value in incident.items() if value not in (None, "")}


# ---------------------------------------------------------------------------
# Outbound call to the agent webhook
# ---------------------------------------------------------------------------


def _post_incident(url: str, secret: str, incident: dict) -> int:
    # Serialise exactly once and sign those same bytes. Re-serialising between
    # signing and sending is the classic cause of signature mismatches here.
    body = json.dumps(incident, separators=(",", ":")).encode("utf-8")
    timestamp = _now_iso()

    signed = timestamp.encode("utf-8") + b":" + body
    signature = base64.b64encode(
        hmac.new(secret.encode("utf-8"), signed, hashlib.sha256).digest()
    ).decode("ascii")

    request = urllib.request.Request(
        url,
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "x-amzn-event-timestamp": timestamp,
            "x-amzn-event-signature": signature,
        },
    )

    try:
        with urllib.request.urlopen(request, timeout=HTTP_TIMEOUT) as response:
            LOG.info(
                "devops agent accepted incident: status=%s body=%s",
                response.status,
                response.read()[:500].decode("utf-8", "replace"),
            )
            return response.status
    except urllib.error.HTTPError as exc:
        LOG.error(
            "devops agent rejected incident: status=%s body=%s",
            exc.code,
            exc.read()[:500].decode("utf-8", "replace"),
        )
        raise


def lambda_handler(event: dict, _context: object = None) -> dict:
    if not _authorized(event):
        LOG.warning("rejected request with missing or invalid bearer token")
        return _response(401, {"error": "unauthorized"})

    try:
        causely = _parse_body(event)
    except (ValueError, TypeError, binascii.Error) as exc:
        LOG.warning("rejected malformed body: %s", exc)
        return _response(400, {"error": f"invalid JSON body: {exc}"})

    incident = to_incident(causely)
    LOG.info(
        "relaying causely notification type=%s severity=%s -> incident=%s",
        causely.get("type"),
        causely.get("severity"),
        json.dumps(incident),
    )

    if DRY_RUN:
        LOG.info("DRY_RUN set — not calling the agent webhook")
        return _response(200, {"relayed": False, "dryRun": True, "incident": incident})

    url, secret = _webhook_config()
    try:
        status = _post_incident(url, secret, incident)
    except urllib.error.HTTPError as exc:
        return _response(502, {"error": "agent webhook rejected the incident", "agentStatus": exc.code})
    except urllib.error.URLError as exc:
        LOG.error("could not reach the agent webhook: %s", exc)
        return _response(504, {"error": "could not reach the agent webhook"})

    return _response(
        200,
        {"relayed": True, "incidentId": incident["incidentId"], "agentStatus": status},
    )
