package main

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
func handleSlackAction(logger *zap.Logger, cfg Config, weekly *weeklyBudget, rec *recorder, kc *kubeClient) http.HandlerFunc {
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

		var action slackActionPayload
		if err := json.Unmarshal([]byte(raw), &action); err != nil {
			http.Error(w, "invalid payload json", http.StatusBadRequest)
			return
		}

		// Find the causely_fix_it action.
		var value string
		for _, a := range action.Actions {
			if a.ActionID == "causely_fix_it" {
				value = a.Value
				break
			}
		}
		if value == "" {
			w.WriteHeader(http.StatusOK) // unknown action — ack and ignore
			return
		}

		var val struct {
			RootCauseID   string `json:"root_cause_id"`
			EntityID      string `json:"entity_id"`
			EntityName    string `json:"entity_name"`
			RootCauseName string `json:"root_cause_name"`
			Severity      string `json:"severity"`
			Description   string `json:"description"`
			Remediation   string `json:"remediation"`
		}
		if err := json.Unmarshal([]byte(value), &val); err != nil {
			http.Error(w, "invalid action value", http.StatusBadRequest)
			return
		}

		payload := TriggerPayload{
			RootCauseID:   val.RootCauseID,
			EntityID:      val.EntityID,
			EntityName:    val.EntityName,
			RootCauseName: val.RootCauseName,
			Severity:      val.Severity,
			Description:   val.Description,
			Remediation:   val.Remediation,
			SlackChannel:  action.Channel.ID,
			SlackThreadTS: action.Message.TS,
		}

		// Respond to Slack within 3 seconds or it retries.
		w.WriteHeader(http.StatusOK)

		go runAgent(logger, cfg, payload, weekly, rec, kc, triggerSourceSlackFixIt)
	}
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
