package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
)

// InvestigationRecord captures everything about one investigation run,
// regardless of trigger source or verdict, so causely-background-agent's behavior *and*
// Causely's own root-cause analysis quality can be measured and compared over
// time — instead of only observed anecdotally per-incident in logs and Slack.
type InvestigationRecord struct {
	// Identity
	RootCauseID   string `json:"root_cause_id"`
	EntityID      string `json:"entity_id,omitempty"`
	EntityName    string `json:"entity_name,omitempty"`
	RootCauseName string `json:"root_cause_name,omitempty"`
	Severity      string `json:"severity,omitempty"`

	// Provenance
	TriggerSource string `json:"trigger_source"` // "webhook", "poll", "slack_action"
	ActionMode    string `json:"action_mode"`    // "observe" or "act"

	// Timing
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMS int64     `json:"duration_ms"`

	// Cost — the primary signal for "how expensive was this to figure out."
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`

	// Investigation shape — the primary signal for "how much digging did it need."
	ToolCalls     []ToolCallSummary `json:"tool_calls,omitempty"`
	ToolCallCount int               `json:"tool_call_count"`

	// Outcome
	Verdict     string `json:"verdict"` // see the verdict* constants below
	SkipReason  string `json:"skip_reason,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Remediation string `json:"remediation,omitempty"`
	PRUrl       string `json:"pr_url,omitempty"`
	Error       string `json:"error,omitempty"`

	// Filled in later by human review or synthetic-bug ground truth — the
	// agent never sets these itself.
	CorrectnessLabel string `json:"correctness_label,omitempty"` // "correct", "incorrect", "insufficient_evidence"
	CorrectnessNotes string `json:"correctness_notes,omitempty"`
}

const (
	verdictSkippedScope     = "skipped_scope"
	verdictSkippedBudget    = "skipped_budget"
	verdictFixProposed      = "fix_proposed"
	verdictRemediationOnly  = "remediation_recommended"
	verdictFailed           = "failed"
	triggerSourceWebhook    = "webhook"
	triggerSourcePoll       = "poll"
	triggerSourceSlackFixIt = "slack_action"
	actionModeObserve       = "observe"
	actionModeAct           = "act"
)

// ToolCallSummary is one tool call made during an investigation. Server is the
// MCP source name ("causely", "grafana") or "github" for the repo-reading tools;
// it's what lets a later analysis separate "time spent gathering Causely
// evidence" from "time spent locating the fix in source."
type ToolCallSummary struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
	Error  bool   `json:"error"`
}

// recorder appends InvestigationRecords as JSON Lines to a file, one record
// per line, so the dataset can be read incrementally and queried with any
// line-oriented tool (jq, grep, a notebook) without a database dependency.
// A nil path (recording disabled) makes record() a no-op.
type recorder struct {
	mu     sync.Mutex
	path   string
	logger *zap.Logger
}

func newRecorder(path string, logger *zap.Logger) *recorder {
	return &recorder{path: path, logger: logger}
}

func (r *recorder) record(rec InvestigationRecord) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	line, err := json.Marshal(rec)
	if err != nil {
		r.logger.Warn("investigation record: marshal failed", zap.Error(err))
		return
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		r.logger.Warn("investigation record: open failed", zap.String("path", r.path), zap.Error(err))
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		r.logger.Warn("investigation record: write failed", zap.String("path", r.path), zap.Error(err))
	}
}
