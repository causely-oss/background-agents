package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// slackActionPayload is the subset of the Slack block_actions payload we need.
type slackActionPayload struct {
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS string `json:"ts"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// handleSlackAction handles POST /slack/actions from Slack interactive components.
// It verifies the request signature, extracts the causely_fix_it action, and
// launches a remediation investigation as a goroutine.
func handleSlackAction(logger *zap.Logger, cfg Config, weekly *weeklyBudget, rec *recorder, kc *kubeClient, dedup *triggerDedup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}

		if cfg.SlackSigningSecret != "" {
			if err := verifySlackSignature(r.Header, body, cfg.SlackSigningSecret); err != nil {
				logger.Warn("slack actions: signature verification failed", zap.Error(err))
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return
			}
		}

		// Slack sends action payloads as URL-encoded form with a "payload" key.
		form, err := url.ParseQuery(string(body))
		if err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		raw := form.Get("payload")
		if raw == "" {
			http.Error(w, "missing payload", http.StatusBadRequest)
			return
		}

		payload, found, err := parseSlackFixItAction([]byte(raw))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !found {
			w.WriteHeader(http.StatusOK) // unknown action — ack and ignore
			return
		}

		// Only the in-flight guard applies here, not persisted content-hash
		// replay suppression — see tryAcquireInFlightOnly's doc comment for
		// why: a double-click must still be rejected, but a deliberate
		// second click by a human (including retrying after a failed
		// investigation) must not be silently swallowed forever just because
		// the issue's content hasn't changed.
		if !dedup.tryAcquireInFlightOnly(payload.IssueID) {
			logger.Info("investigation already in flight for this issue, skipping slack action", zap.String("issue_id", payload.IssueID))
			w.WriteHeader(http.StatusOK)
			return
		}

		// Respond to Slack within 3 seconds or it retries.
		w.WriteHeader(http.StatusOK)

		go func() {
			defer dedup.release(payload.IssueID)
			runAgent(logger, cfg, payload, weekly, rec, kc, triggerSourceSlackFixIt)
		}()
	}
}

// parseSlackFixItAction extracts a TriggerPayload from a raw Slack
// block_actions payload. found is false (with a nil error) when the payload
// doesn't contain a "causely_fix_it" action — a normal, expected case for
// any other interactive component Slack might deliver to this same endpoint.
func parseSlackFixItAction(raw []byte) (payload TriggerPayload, found bool, err error) {
	var action slackActionPayload
	if err := json.Unmarshal(raw, &action); err != nil {
		return TriggerPayload{}, false, fmt.Errorf("invalid payload json: %w", err)
	}

	var value string
	for _, a := range action.Actions {
		if a.ActionID == "causely_fix_it" {
			value = a.Value
			break
		}
	}
	if value == "" {
		return TriggerPayload{}, false, nil
	}

	var val struct {
		IssueID       string `json:"issue_id"`
		EntityID      string `json:"entity_id"`
		EntityName    string `json:"entity_name"`
		DiagnosisName string `json:"diagnosis_name"`
		Severity      string `json:"severity"`
		Description   string `json:"description"`
		Remediation   string `json:"remediation"`
		// Without these two, inScope (scope.go) silently misbehaves on every
		// Slack-triggered click: GitHubRepoLabel's check only applies "if
		// set," so it's a silent no-op here (fails open); worse, if
		// scope_namespaces IS configured, EntityNamespace always being ""
		// makes inScope reject every single click outright, regardless of
		// the real namespace (fails closed on everything). Found in review,
		// not dogfooding — this path was previously untested against a
		// scope_namespaces-configured deployment.
		EntityNamespace string `json:"entity_namespace"`
		GitHubRepoLabel string `json:"github_repo_label"`
	}
	if err := json.Unmarshal([]byte(value), &val); err != nil {
		return TriggerPayload{}, false, fmt.Errorf("invalid action value: %w", err)
	}

	return TriggerPayload{
		IssueID:         val.IssueID,
		EntityID:        val.EntityID,
		EntityName:      val.EntityName,
		DiagnosisName:   val.DiagnosisName,
		Severity:        val.Severity,
		Description:     val.Description,
		Remediation:     val.Remediation,
		SlackChannel:    action.Channel.ID,
		SlackThreadTS:   action.Message.TS,
		EntityNamespace: val.EntityNamespace,
		GitHubRepoLabel: val.GitHubRepoLabel,
	}, true, nil
}

// verifySlackSignature checks the X-Slack-Signature header against the signing secret.
func verifySlackSignature(headers http.Header, body []byte, signingSecret string) error {
	ts := headers.Get("X-Slack-Request-Timestamp")
	sig := headers.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return fmt.Errorf("missing signature headers")
	}

	// Reject requests older than 5 minutes to prevent replay attacks.
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp")
	}
	if time.Since(time.Unix(tsInt, 0)) > 5*time.Minute {
		return fmt.Errorf("timestamp too old")
	}

	base := fmt.Sprintf("v0:%s:%s", ts, body)
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(base))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}
