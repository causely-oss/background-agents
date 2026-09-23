package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestParseSlackFixItAction_ParsesScopeHints guards the fix for a real bug:
// without entity_namespace/github_repo_label parsed here, inScope (scope.go)
// silently misbehaves on every Slack-triggered click — the repo-label check
// becomes a silent no-op (it only applies "if set"), and worse, if
// scope_namespaces is configured, EntityNamespace always being "" makes
// inScope reject every single click outright, regardless of the real
// namespace.
func TestParseSlackFixItAction_ParsesScopeHints(t *testing.T) {
	raw := []byte(`{
		"channel": {"id": "C123"},
		"message": {"ts": "1234.5678"},
		"actions": [{
			"action_id": "causely_fix_it",
			"value": "{\"issue_id\":\"i1\",\"entity_namespace\":\"causely\",\"github_repo_label\":\"org/repo\"}"
		}]
	}`)

	payload, found, err := parseSlackFixItAction(raw)
	if err != nil {
		t.Fatalf("parseSlackFixItAction() error = %v", err)
	}
	if !found {
		t.Fatal("parseSlackFixItAction() found = false, want true")
	}
	if payload.EntityNamespace != "causely" {
		t.Errorf("payload.EntityNamespace = %q, want %q", payload.EntityNamespace, "causely")
	}
	if payload.GitHubRepoLabel != "org/repo" {
		t.Errorf("payload.GitHubRepoLabel = %q, want %q", payload.GitHubRepoLabel, "org/repo")
	}
	if payload.SlackChannel != "C123" || payload.SlackThreadTS != "1234.5678" {
		t.Errorf("payload Slack fields = %q/%q, want C123/1234.5678", payload.SlackChannel, payload.SlackThreadTS)
	}
}

// TestParseSlackFixItAction_ScopeNamespacesWouldHaveRejectedBeforeTheFix is a
// regression test proving the actual failure mode: with scope_namespaces
// configured and a payload missing entity_namespace (the pre-fix shape),
// inScope rejects every click outright. After the fix, a correctly-parsed
// namespace passes.
func TestParseSlackFixItAction_ScopeNamespacesWouldHaveRejectedBeforeTheFix(t *testing.T) {
	raw := []byte(`{
		"channel": {"id": "C123"},
		"message": {"ts": "1234.5678"},
		"actions": [{
			"action_id": "causely_fix_it",
			"value": "{\"issue_id\":\"i1\",\"entity_namespace\":\"causely\"}"
		}]
	}`)
	payload, found, err := parseSlackFixItAction(raw)
	if err != nil || !found {
		t.Fatalf("parseSlackFixItAction() error = %v, found = %v", err, found)
	}

	cfg := Config{ScopeNamespaces: []string{"causely"}}
	ok, reason := inScope(cfg, payload)
	if !ok {
		t.Errorf("inScope() = false (%s), want true — entity_namespace was parsed and matches scope_namespaces", reason)
	}
}

func TestParseSlackFixItAction_UnknownActionIsNotFoundNotError(t *testing.T) {
	raw := []byte(`{"channel":{"id":"C1"},"message":{"ts":"1"},"actions":[{"action_id":"some_other_button","value":"x"}]}`)
	_, found, err := parseSlackFixItAction(raw)
	if err != nil {
		t.Fatalf("parseSlackFixItAction() error = %v, want nil for an unrelated action", err)
	}
	if found {
		t.Error("parseSlackFixItAction() found = true, want false for an unrelated action_id")
	}
}

func TestParseSlackFixItAction_InvalidEnvelopeJSONIsAnError(t *testing.T) {
	_, _, err := parseSlackFixItAction([]byte("not json"))
	if err == nil {
		t.Fatal("parseSlackFixItAction() should error on invalid envelope JSON")
	}
}

func TestParseSlackFixItAction_InvalidValueJSONIsAnError(t *testing.T) {
	raw := []byte(`{"channel":{"id":"C1"},"message":{"ts":"1"},"actions":[{"action_id":"causely_fix_it","value":"not json"}]}`)
	_, _, err := parseSlackFixItAction(raw)
	if err == nil {
		t.Fatal("parseSlackFixItAction() should error on invalid action value JSON")
	}
}

// --- verifySlackSignature: previously zero test coverage on the one
// signature-verification path guarding a network-exposed endpoint. ---

func signSlackRequest(secret string, ts string, body []byte) string {
	base := fmt.Sprintf("v0:%s:%s", ts, body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(base))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySlackSignature_ValidSignatureIsAccepted(t *testing.T) {
	secret := "test-signing-secret"
	body := []byte(`{"hello":"world"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	headers := http.Header{}
	headers.Set("X-Slack-Request-Timestamp", ts)
	headers.Set("X-Slack-Signature", signSlackRequest(secret, ts, body))

	if err := verifySlackSignature(headers, body, secret); err != nil {
		t.Errorf("verifySlackSignature() error = %v, want nil for a validly-signed request", err)
	}
}

func TestVerifySlackSignature_WrongSecretIsRejected(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	headers := http.Header{}
	headers.Set("X-Slack-Request-Timestamp", ts)
	headers.Set("X-Slack-Signature", signSlackRequest("signed-with-this-secret", ts, body))

	if err := verifySlackSignature(headers, body, "verified-against-a-different-secret"); err == nil {
		t.Error("verifySlackSignature() should reject a signature made with the wrong secret")
	}
}

func TestVerifySlackSignature_TamperedBodyIsRejected(t *testing.T) {
	secret := "test-signing-secret"
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	headers := http.Header{}
	headers.Set("X-Slack-Request-Timestamp", ts)
	headers.Set("X-Slack-Signature", signSlackRequest(secret, ts, []byte(`{"original":true}`)))

	if err := verifySlackSignature(headers, []byte(`{"tampered":true}`), secret); err == nil {
		t.Error("verifySlackSignature() should reject when the body doesn't match what was signed")
	}
}

func TestVerifySlackSignature_StaleTimestampIsRejected(t *testing.T) {
	secret := "test-signing-secret"
	body := []byte(`{"hello":"world"}`)
	staleTS := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)

	headers := http.Header{}
	headers.Set("X-Slack-Request-Timestamp", staleTS)
	headers.Set("X-Slack-Signature", signSlackRequest(secret, staleTS, body))

	if err := verifySlackSignature(headers, body, secret); err == nil {
		t.Error("verifySlackSignature() should reject a request older than 5 minutes — a replayed request must not be accepted just because it was validly signed once")
	}
}

func TestVerifySlackSignature_MissingHeadersAreRejected(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	if err := verifySlackSignature(http.Header{}, body, "any-secret"); err == nil {
		t.Error("verifySlackSignature() should reject a request with no signature headers at all")
	}
}
