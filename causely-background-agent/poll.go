package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// pollWatermark tracks, per issue ID, an opaque "version" string (whatever
// changed-when signal the issue payload offers — updated_at, or a fallback
// derived from severity+symptom count). A poll cycle only triggers an
// investigation when an issue's current version differs from what's stored
// here, so a still-open, unchanged issue isn't re-investigated every cycle,
// but a genuinely new occurrence or a material change (severity shift,
// symptom count growth) does trigger again.
type pollWatermark struct {
	path string
	seen map[string]string
}

func loadPollWatermark(path string) *pollWatermark {
	w := &pollWatermark{path: path, seen: map[string]string{}}
	if path == "" {
		return w
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return w // missing/unreadable state file just starts fresh
	}
	_ = json.Unmarshal(data, &w.seen)
	return w
}

// changed reports whether version is new for id, and records it either way
// so the next poll cycle sees it as already-seen.
func (w *pollWatermark) changed(id, version string) bool {
	if prev, ok := w.seen[id]; ok && prev == version {
		return false
	}
	w.seen[id] = version
	return true
}

func (w *pollWatermark) persist() error {
	if w.path == "" {
		return nil
	}
	data, err := json.Marshal(w.seen)
	if err != nil {
		return err
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.path)
}

// runPollLoop is a trigger source alongside the push webhook: on cfg.Poll.Interval,
// it asks the Causely MCP server for open issues directly instead of waiting for
// mediator to fire a one-time webhook. This exists because a webhook only reflects
// an issue's state at the moment it fired — issues evolve (more/fewer symptoms,
// severity shifts, auto-clear), and a poll loop is how the agent can notice that.
func runPollLoop(logger *zap.Logger, cfg Config, weekly *weeklyBudget, rec *recorder, kc *kubeClient) {
	interval, err := time.ParseDuration(cfg.Poll.Interval)
	if err != nil {
		logger.Fatal("invalid poll.interval", zap.Error(err))
	}
	watermark := loadPollWatermark(cfg.Poll.StateFile)

	// The built-in causely MCP server is always cfg.MCPServers[0] — see resolveMCPServers.
	client := cfg.MCPServers[0].newClient()

	logger.Info("poll loop starting", zap.Duration("interval", interval))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		pollOnce(logger, cfg, client, watermark, weekly, rec, kc)
	}
}

func pollOnce(logger *zap.Logger, cfg Config, client *mcpClient, watermark *pollWatermark, weekly *weeklyBudget, rec *recorder, kc *kubeClient) {
	// active_only is get_issues's real parameter name (its default is already true,
	// so this was previously a no-op typo — "only_active" — that happened to work by
	// coincidence). namespace_names filters server-side: get_issues's lightweight
	// per-issue response doesn't reliably include entity/label data (unlike
	// get_issue_details), so scope_namespaces can't be enforced client-side here the
	// way inScope() does for the webhook/Slack trigger sources — asking the API to
	// only return matching issues in the first place is the only sound way to keep
	// polling scoped to specific namespaces.
	args := map[string]any{"active_only": true}
	if len(cfg.ScopeNamespaces) > 0 {
		args["namespace_names"] = cfg.ScopeNamespaces
	}
	raw, err := client.CallTool("get_issues", args)
	if err != nil {
		logger.Warn("poll: get_issues failed", zap.Error(err))
		return
	}

	issues, err := parsePolledIssues(raw)
	if err != nil {
		logger.Warn("poll: could not parse get_issues response", zap.Error(err), zap.String("raw_prefix", truncate(raw, 500)))
		return
	}

	changedCount := 0
	for _, issue := range issues {
		if issue.IssueID == "" {
			continue
		}
		if !watermark.changed(issue.IssueID, issue.version()) {
			continue
		}
		changedCount++
		payload := issue.toTriggerPayload(cfg)
		go runAgent(logger, cfg, payload, weekly, rec, kc, triggerSourcePoll)
	}

	if changedCount > 0 {
		logger.Info("poll: dispatched investigations", zap.Int("changed", changedCount), zap.Int("total_open", len(issues)))
	}
	if err := watermark.persist(); err != nil {
		logger.Warn("poll: failed to persist watermark", zap.Error(err))
	}
}

// polledIssue is a defensively-parsed subset of one entry from get_issues — the
// exact response schema isn't pinned down here, so every field is read with a
// fallback across the plausible key names rather than a rigid struct, and a
// missing root cause ID just skips the issue (logged) rather than panicking.
type polledIssue struct {
	IssueID         string
	EntityID        string
	EntityName      string
	EntityNamespace string
	RootCauseName   string
	Severity        string
	Description     string
	Remediation     string
	UpdatedAt       string
	SymptomCount    float64
}

// version returns an opaque string that changes only when there's genuinely
// new information about this issue, so pollOnce's watermark doesn't
// re-investigate something already seen.
//
// NOTE: the severity|symptom_count fallback below is known to be noisy —
// top-level severity can flicker (baseline vs. elevated) as the same
// diagnosis merely toggles active/inactive, which can make a single
// flare-and-clear look like two separate "new occurrences" a poll cycle
// apart. A tighter fix (keying on primary_diagnosis.id + started_at instead)
// was tried and reverted: that's really a signal-quality issue on Causely's
// own get_issues response, not something this one reference agent should
// silently work around — any other agent built against the same MCP tools
// would hit the identical noise. Belongs fixed upstream (e.g. a real
// updated_at on the issue), not patched here.
func (p polledIssue) version() string {
	if p.UpdatedAt != "" {
		return p.UpdatedAt
	}
	// No updated_at-style field on this payload shape — fall back to a coarse
	// "did anything visibly change" signal so a still-open, unchanged issue
	// doesn't get treated as new every single poll cycle.
	return fmt.Sprintf("%s|%.0f", p.Severity, p.SymptomCount)
}

func (p polledIssue) toTriggerPayload(cfg Config) TriggerPayload {
	namespace := p.EntityNamespace
	if namespace == "" {
		namespace = cfg.Poll.EntityNamespace
	}
	return TriggerPayload{
		IssueID:         p.IssueID,
		EntityID:        p.EntityID,
		EntityName:      p.EntityName,
		RootCauseName:   p.RootCauseName,
		Severity:        p.Severity,
		Description:     p.Description,
		Remediation:     p.Remediation,
		EntityNamespace: namespace,
		GitHubRepoLabel: cfg.Poll.GitHubRepoLabel,
	}
}

func parsePolledIssues(raw string) ([]polledIssue, error) {
	var generic []map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		// Some MCP tools wrap the array in a top-level object, e.g. {"issues": [...]}.
		var wrapped struct {
			Issues []map[string]any `json:"issues"`
		}
		if err2 := json.Unmarshal([]byte(raw), &wrapped); err2 != nil {
			return nil, err
		}
		generic = wrapped.Issues
	}

	issues := make([]polledIssue, 0, len(generic))
	for _, m := range generic {
		entity, _ := m["entity"].(map[string]any)
		desc, _ := m["description"].(map[string]any)

		issues = append(issues, polledIssue{
			IssueID:         firstString(m, "id", "issue_id", "objectId"),
			EntityID:        firstString(entity, "id"),
			EntityName:      firstString(entity, "name"),
			EntityNamespace: firstString(m, "entity_namespace", "namespace"),
			RootCauseName:   firstString(m, "name", "root_cause_name", "type"),
			Severity:        firstString(m, "severity"),
			Description:     firstString(desc, "summary"),
			Remediation:     firstString(desc, "remediation"),
			UpdatedAt:       firstString(m, "updated_at", "last_seen", "timestamp"),
			SymptomCount:    firstFloat(m, "symptom_count", "symptomCount"),
		})
	}
	return issues, nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok {
			return v
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// oneLine collapses newlines (and the whitespace runs they tend to leave
// behind) into single spaces, so a preview of a long multi-line field — logged
// as one JSON structured-log value — reads as a single line in a terminal
// instead of a wall of escaped "\n"s. The full, unflattened text still goes to
// the persisted InvestigationRecord; this is only for the terse stdout log.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
