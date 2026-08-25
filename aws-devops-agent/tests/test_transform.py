#!/usr/bin/env python3
"""Unit tests for the Causely -> DevOps Agent payload translation.

Runs with no AWS access and no dependencies:

    python3 tests/test_transform.py

The translation is the part of the relay most likely to break silently — a wrong
severity mapping or a dropped object id degrades the result without erroring, so
it is worth pinning down here rather than discovering it live.
"""

from __future__ import annotations

import json
import os
import pathlib
import sys
import types
import unittest
from unittest import mock

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO_ROOT / "lambda" / "causely_relay"))

# handler.py reads its secret ids at import time and builds a boto3 client. boto3
# ships in the Lambda runtime but is not needed to test the pure logic, so stub it
# rather than requiring a local install.
os.environ.setdefault("WEBHOOK_SECRET_ID", "test/webhook")
os.environ.setdefault("INBOUND_SECRET_ID", "test/inbound")

if "boto3" not in sys.modules:
    _stub = types.ModuleType("boto3")
    _stub.client = lambda *_args, **_kwargs: mock.MagicMock()  # type: ignore[attr-defined]
    sys.modules["boto3"] = _stub

import handler  # noqa: E402


FIXTURE = json.loads((REPO_ROOT / "tests" / "fixtures" / "causely-problem-detected.json").read_text())


class TestToIncident(unittest.TestCase):
    def setUp(self) -> None:
        self.incident = handler.to_incident(FIXTURE)

    def test_shape_matches_agent_schema(self) -> None:
        self.assertEqual(self.incident["eventType"], "incident")
        for field in ("incidentId", "action", "priority", "title", "timestamp", "service"):
            self.assertIn(field, self.incident)

    def test_object_id_becomes_incident_id(self) -> None:
        # Reusing Causely's objectId keeps the two systems talking about one thing.
        self.assertEqual(self.incident["incidentId"], FIXTURE["objectId"])

    def test_severity_maps_to_priority(self) -> None:
        self.assertEqual(self.incident["priority"], "CRITICAL")

    def test_detected_maps_to_created(self) -> None:
        self.assertEqual(self.incident["action"], "created")

    def test_cleared_maps_to_resolved(self) -> None:
        cleared = dict(FIXTURE, type="ProblemCleared")
        self.assertEqual(handler.to_incident(cleared)["action"], "resolved")

    def test_unknown_type_and_severity_fall_back(self) -> None:
        odd = dict(FIXTURE, type="SomethingNew", severity="Weird")
        result = handler.to_incident(odd)
        self.assertEqual(result["action"], "updated")
        self.assertEqual(result["priority"], "MEDIUM")

    def test_title_names_the_problem_and_entity(self) -> None:
        self.assertEqual(self.incident["title"], "MemoryLeak on checkout")

    def test_description_flattens_summary_details_and_remediation(self) -> None:
        description = self.incident["description"]
        self.assertIn("leaking memory", description)
        self.assertIn("Container RSS", description)
        self.assertIn("Roll back to the previous revision", description)

    def test_carries_causely_handles_for_mcp_followup(self) -> None:
        # This is the hinge of the integration: without these the agent cannot pull the
        # causal chain back out of Causely.
        data = self.incident["data"]
        self.assertEqual(data["causelyObjectId"], FIXTURE["objectId"])
        self.assertEqual(data["causelyObjectType"], "issue")
        self.assertEqual(data["causelyLink"], FIXTURE["link"])
        self.assertEqual(data["cluster"], "demo-cluster")
        self.assertEqual(data["namespace"], "checkout")
        self.assertIn("get_issue_details", data["investigationHint"])
        self.assertIn("issue_id", data["investigationHint"])

    def test_defect_notification_points_at_the_diagnosis_tool(self) -> None:
        # The relay handles defect-level notifications too; the object_type is what
        # decides which tool can actually resolve the id.
        data = handler.to_incident(dict(FIXTURE, object_type="defect"))["data"]
        self.assertEqual(data["causelyObjectType"], "defect")
        self.assertIn("get_diagnosis_details", data["investigationHint"])
        self.assertIn("diagnosis_id", data["investigationHint"])
        self.assertNotIn("get_issue_details", data["investigationHint"])

    def test_unknown_object_type_assumes_issue_and_says_so(self) -> None:
        # Sending Issues is the documented setup, so an absent object_type is
        # treated as one — but the hint names the fallback rather than guessing
        # silently and leaving the agent with an id no tool will resolve.
        payload = dict(FIXTURE)
        payload.pop("object_type")
        hint = handler.to_incident(payload)["data"]["investigationHint"]
        self.assertIn("get_issue_details", hint)
        self.assertIn("get_diagnosis_details", hint)

    def test_no_empty_values_survive(self) -> None:
        for key, value in self.incident.items():
            self.assertNotIn(value, (None, ""), f"{key} should have been dropped")
        for key, value in self.incident["data"].items():
            self.assertNotIn(value, (None, ""), f"data.{key} should have been dropped")

    def test_minimal_payload_does_not_crash(self) -> None:
        result = handler.to_incident({"name": "Bare", "type": "ProblemDetected"})
        self.assertEqual(result["incidentId"], "causely-Bare")
        self.assertEqual(result["title"], "Bare")
        self.assertEqual(result["service"], "unknown")
        self.assertTrue(result["timestamp"])  # synthesised when absent

    def test_empty_payload_does_not_crash(self) -> None:
        result = handler.to_incident({})
        self.assertEqual(result["title"], "Causely issue")

    def test_string_description_passes_through(self) -> None:
        result = handler.to_incident(dict(FIXTURE, description="plain text"))
        self.assertEqual(result["description"], "plain text")


class TestAuthorization(unittest.TestCase):
    def setUp(self) -> None:
        patcher = mock.patch.object(handler, "_secret", return_value="s3cret")
        self.addCleanup(patcher.stop)
        patcher.start()

    def test_accepts_the_right_token(self) -> None:
        self.assertTrue(handler._authorized({"headers": {"authorization": "Bearer s3cret"}}))

    def test_header_lookup_is_case_insensitive(self) -> None:
        self.assertTrue(handler._authorized({"headers": {"Authorization": "bearer s3cret"}}))

    def test_rejects_wrong_token(self) -> None:
        self.assertFalse(handler._authorized({"headers": {"authorization": "Bearer nope"}}))

    def test_rejects_missing_header(self) -> None:
        self.assertFalse(handler._authorized({"headers": {}}))
        self.assertFalse(handler._authorized({}))

    def test_rejects_wrong_scheme(self) -> None:
        self.assertFalse(handler._authorized({"headers": {"authorization": "Basic s3cret"}}))

    def test_accepts_bare_token_without_scheme(self) -> None:
        # Causely sends its token verbatim; omitting "Bearer " is an easy mistake
        # whose only symptom would be a silent 401.
        self.assertTrue(handler._authorized({"headers": {"authorization": "s3cret"}}))

    def test_bare_wrong_token_still_rejected(self) -> None:
        self.assertFalse(handler._authorized({"headers": {"authorization": "nope"}}))

    def test_tolerates_surrounding_whitespace(self) -> None:
        self.assertTrue(handler._authorized({"headers": {"authorization": "  Bearer s3cret  "}}))


class TestHandler(unittest.TestCase):
    def test_unauthorized_request_is_401_and_never_forwards(self) -> None:
        with mock.patch.object(handler, "_secret", return_value="s3cret"), \
             mock.patch.object(handler, "_post_incident") as post:
            response = handler.lambda_handler({"headers": {}, "body": "{}"})
        self.assertEqual(response["statusCode"], 401)
        post.assert_not_called()

    def test_malformed_body_is_400(self) -> None:
        with mock.patch.object(handler, "_secret", return_value="s3cret"):
            response = handler.lambda_handler(
                {"headers": {"authorization": "Bearer s3cret"}, "body": "not json"}
            )
        self.assertEqual(response["statusCode"], 400)

    def test_json_array_body_is_400(self) -> None:
        with mock.patch.object(handler, "_secret", return_value="s3cret"):
            response = handler.lambda_handler(
                {"headers": {"authorization": "Bearer s3cret"}, "body": "[1,2]"}
            )
        self.assertEqual(response["statusCode"], 400)

    def test_happy_path_forwards_and_reports(self) -> None:
        with mock.patch.object(handler, "_secret", return_value="s3cret"), \
             mock.patch.object(handler, "_webhook_config", return_value=("https://example.test", "hmac")), \
             mock.patch.object(handler, "_post_incident", return_value=200) as post:
            response = handler.lambda_handler(
                {"headers": {"authorization": "Bearer s3cret"}, "body": json.dumps(FIXTURE)}
            )
        self.assertEqual(response["statusCode"], 200)
        body = json.loads(response["body"])
        self.assertTrue(body["relayed"])
        self.assertEqual(body["incidentId"], FIXTURE["objectId"])
        post.assert_called_once()

    def test_base64_body_is_decoded(self) -> None:
        import base64 as b64

        encoded = b64.b64encode(json.dumps(FIXTURE).encode()).decode()
        with mock.patch.object(handler, "_secret", return_value="s3cret"), \
             mock.patch.object(handler, "_webhook_config", return_value=("https://example.test", "hmac")), \
             mock.patch.object(handler, "_post_incident", return_value=200):
            response = handler.lambda_handler(
                {
                    "headers": {"authorization": "Bearer s3cret"},
                    "body": encoded,
                    "isBase64Encoded": True,
                }
            )
        self.assertEqual(response["statusCode"], 200)


class TestSigning(unittest.TestCase):
    def test_signs_exactly_the_bytes_it_sends(self) -> None:
        """Guards the classic failure: signing a different serialisation than is sent."""
        import base64 as b64
        import hashlib
        import hmac as hmac_mod

        captured: dict[str, object] = {}

        class FakeResponse:
            status = 200

            def read(self) -> bytes:
                return b"{}"

            def __enter__(self):
                return self

            def __exit__(self, *_: object) -> None:
                return None

        def fake_urlopen(request, timeout=None):  # noqa: ANN001, ARG001
            captured["body"] = request.data
            captured["headers"] = {k.lower(): v for k, v in request.headers.items()}
            return FakeResponse()

        with mock.patch("urllib.request.urlopen", fake_urlopen):
            handler._post_incident("https://example.test", "hmac-secret", {"a": 1, "b": "x"})

        body = captured["body"]
        headers = captured["headers"]
        expected = b64.b64encode(
            hmac_mod.new(
                b"hmac-secret",
                headers["x-amzn-event-timestamp"].encode() + b":" + body,  # type: ignore[union-attr]
                hashlib.sha256,
            ).digest()
        ).decode()

        self.assertEqual(headers["x-amzn-event-signature"], expected)
        # Compact separators, so the signed bytes are byte-identical to the sent bytes.
        self.assertEqual(body, b'{"a":1,"b":"x"}')


if __name__ == "__main__":
    unittest.main(verbosity=2)
