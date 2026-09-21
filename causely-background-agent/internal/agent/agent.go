package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// errBudgetExceeded is returned by runClaudeLoop when the investigation's cost
// budget (Config.MaxCostUSD) is exceeded before propose_fix is called.
var errBudgetExceeded = errors.New("investigation aborted: cost budget exceeded")

// errNoOpFix is surfaced back to Claude as a tool_result when propose_fix is
// called with every change's search == replace — see fixHasRealChange.
var errNoOpFix = errors.New("every change has identical search and replace text — a no-op that would open an empty PR. If the repository already matches the desired end state, this isn't a code bug: call recommend_remediation instead, don't call propose_fix again with this same diff")

func investigationCompletionAttributes(ir InvestigationRecord, costBudgetUSD float64) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("causely.agent.verdict", ir.Verdict),
		attribute.Float64("causely.agent.estimated_cost_usd", ir.CostUSD),
		attribute.Int("causely.agent.active_runs", 0),
		attribute.Int("causely.agent.operation.completed", 1),
		attribute.Int64("causely.agent.no_progress_duration_ms", noProgressDurationMS(ir.ToolCalls)),
	}
	if costBudgetUSD > 0 {
		attrs = append(attrs, attribute.Float64("causely.agent.cost_budget_usd", costBudgetUSD))
	}
	return attrs
}

// fixHasRealChange reports whether fix contains at least one change that would
// actually modify a file. A ProposedFix with no changes, or where every
// change's Search equals its Replace, produces a byte-identical PR — most
// commonly because the repo already matches the intended state and the real
// issue is a live/deployed config drift rather than a code bug.
func fixHasRealChange(fix ProposedFix) bool {
	for _, c := range fix.Changes {
		if c.Search != c.Replace {
			return true
		}
	}
	return false
}

// runAgent is the top-level entry point called per trigger, regardless of
// trigger source (webhook, poll, Slack button). weekly enforces the aggregate
// rolling-window cost cap shared across every investigation this process
// runs — independent of the per-investigation cap inside runClaudeLoop. rec
// (may be nil) receives one InvestigationRecord no matter how this call ends
// — skipped, failed, or completed — so behavior and RCA quality can be
// measured later instead of only observed anecdotally in logs/Slack.
func runAgent(logger *zap.Logger, cfg Config, payload TriggerPayload, weekly *weeklyBudget, rec *recorder, kc *kubeClient, triggerSource string) {
	log := logger.With(
		zap.String("diagnosis", payload.DiagnosisName),
		zap.String("entity", payload.EntityName),
		zap.String("severity", payload.Severity),
		zap.String("trigger_source", triggerSource),
	)
	log.Info("agent triggered")

	startedAt := time.Now()
	var investigationSpan trace.Span
	finish := func(ir InvestigationRecord) {
		ir.IssueID = payload.IssueID
		ir.EntityID = payload.EntityID
		ir.EntityName = payload.EntityName
		ir.DiagnosisName = payload.DiagnosisName
		ir.Severity = payload.Severity
		ir.TriggerSource = triggerSource
		ir.ActionMode = cfg.ActionMode
		ir.CauselyRemediationHint = payload.Remediation
		ir.StartedAt = startedAt
		ir.FinishedAt = time.Now()
		ir.DurationMS = ir.FinishedAt.Sub(startedAt).Milliseconds()
		rec.record(ir)
		if investigationSpan != nil {
			investigationSpan.SetAttributes(investigationCompletionAttributes(ir, cfg.MaxCostUSD)...)
			if ir.Error != "" {
				investigationSpan.SetStatus(codes.Error, ir.Error)
			}
			investigationSpan.End()
		}
	}
	acting := cfg.ActionMode == actionModeAct

	if ok, reason := inScope(cfg, payload); !ok {
		log.Info("Issue out of scope for this agent instance, skipping", zap.String("reason", reason))
		finish(InvestigationRecord{Verdict: verdictSkippedScope, SkipReason: reason})
		return
	}

	// Bounds how many investigations run concurrently, so a burst of
	// near-simultaneous triggers can't all pass the exceeded() check below
	// before any of them records spend — see withConcurrencyLimit in cost.go.
	weekly.acquire()
	defer weekly.release()

	if weekly.exceeded() {
		reason := fmt.Sprintf("weekly cost cap ($%.2f) already reached", weekly.limitUSD)
		log.Warn("weekly cost cap already reached, skipping investigation",
			zap.Float64("weekly_limit_usd", weekly.limitUSD), zap.Float64("weekly_spent_usd", weekly.spent(time.Now())))
		if acting {
			slack := newSlackClient(cfg.SlackBotToken)
			_ = slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS,
				fmt.Sprintf("⚠️ causely-background-agent skipped investigating %s: %s", payload.DiagnosisName, reason))
		}
		finish(InvestigationRecord{Verdict: verdictSkippedBudget, SkipReason: reason})
		return
	}

	// The built-in causely MCP server is always cfg.MCPServers[0] — see resolveMCPServers.
	if skip, reason := checkIssueStillActive(cfg.MCPServers[0].newClient(), payload.IssueID, log); skip {
		log.Info("Issue already resolved, skipping investigation", zap.String("reason", reason))
		finish(InvestigationRecord{Verdict: verdictSkippedStale, SkipReason: reason})
		return
	}

	ctx, span := otel.Tracer(telemetryInstrumentation).Start(
		context.Background(),
		"invoke_agent "+agentName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("causely.operation.type", "AgentOperation"),
			attribute.String("causely.agent.operation.name", agentWorkflowName),
			attribute.Int("causely.agent.active_runs", 1),
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.agent.name", agentName),
			attribute.String("gen_ai.system", "anthropic"),
			attribute.String("gen_ai.request.model", claudeModel),
			attribute.String("causely.agent.trigger.type", triggerSource),
		),
	)
	investigationSpan = span

	gh := newGitHubClient(cfg.GitHubToken, cfg.GitHubRepo)
	slack := newSlackClient(cfg.SlackBotToken)

	// 1. Load tools from every configured MCP server (Causely, and whatever
	// else is listed under mcp_servers in config.yaml — see mcp_config.go).
	sources := loadMCPSources(ctx, cfg, log)

	// 2. Run the agent loop: Claude investigates via MCP + GitHub, then either
	// recommends an immediate remediation or proposes a long-term code fix.
	outcome, tracker, toolCalls, err := runClaudeLoop(ctx, cfg, payload, sources, gh, kc, log)
	investigationSpan.SetAttributes(
		attribute.Int("causely.agent.tool_calls", len(toolCalls)),
		attribute.Int("causely.agent.repeated_tool_calls", repeatedToolCalls(toolCalls)),
		attribute.Int("causely.agent.redundant_tool_calls", redundantToolCalls(toolCalls)),
		attribute.Int("causely.agent.duplicate_tool_results", duplicateToolResults(toolCalls)),
		attribute.Int("gen_ai.usage.input_tokens", tracker.inputTokens),
		attribute.Int("gen_ai.usage.output_tokens", tracker.outputTokens),
	)
	if err != nil {
		investigationSpan.RecordError(err)
	}
	weekly.add(tracker.spentUSD)
	log = log.With(zap.Float64("cost_usd", tracker.spentUSD))

	withUsage := func(ir InvestigationRecord) InvestigationRecord {
		ir.InputTokens = tracker.inputTokens
		ir.OutputTokens = tracker.outputTokens
		ir.CostUSD = tracker.spentUSD
		ir.ToolCalls = toolCalls
		ir.ToolCallCount = len(toolCalls)
		return ir
	}

	if err != nil {
		log.Warn("agent loop failed", zap.Error(err))
		if acting {
			_ = slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS,
				fmt.Sprintf("⚠️ causely-background-agent failed to investigate %s: %s\n\n💰 Cost: $%.4f%s",
					payload.DiagnosisName, err, tracker.spentUSD, partialFindingsSuffix(outcome.Summary, err)))
		}
		finish(withUsage(InvestigationRecord{Verdict: verdictFailed, Error: err.Error(), Summary: outcome.Summary}))
		return
	}

	// 3a. Verification found no genuine defect — the flagged exception/log is
	// already handled by design, or the issue already fully self-resolved with
	// the live state matching what any fix would produce. Record it and stop;
	// there's nothing to remediate or fix.
	if outcome.Fix == nil && outcome.Remediation == "" {
		if acting {
			msg := fmt.Sprintf("✅ *Diagnosis*: %s\n\n%s\n\nNo action needed: %s\n\n💰 Cost: $%.4f",
				payload.DiagnosisName, outcome.Summary, outcome.NoActionReasoning, tracker.spentUSD)
			if err := slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS, msg); err != nil {
				log.Warn("failed to post to slack", zap.Error(err))
			}
		} else {
			log.Info("observe mode: no action needed", zap.String("reasoning_preview", truncate(oneLine(outcome.NoActionReasoning), 200)))
		}
		summary := outcome.Summary
		if outcome.NoActionReasoning != "" {
			summary = strings.TrimSpace(summary + "\n\nNo action needed: " + outcome.NoActionReasoning)
		}
		finish(withUsage(InvestigationRecord{Verdict: verdictNoActionNeeded, Summary: summary}))
		return
	}

	// 3b. No code fix needed — an immediate remediation is sufficient. Post the
	// recommendation (if acting) and stop; there's no PR to open.
	if outcome.Fix == nil {
		if acting {
			msg := fmt.Sprintf("🔍 *Diagnosis*: %s\n\n%s\n\n⚡ *Recommended immediate remediation*: %s\n\n💰 Cost: $%.4f",
				payload.DiagnosisName, outcome.Summary, outcome.Remediation, tracker.spentUSD)
			if err := slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS, msg); err != nil {
				log.Warn("failed to post to slack", zap.Error(err))
			}
		} else {
			log.Info("observe mode: would have recommended remediation", zap.String("remediation_preview", truncate(oneLine(outcome.Remediation), 200)))
		}
		finish(withUsage(InvestigationRecord{Verdict: verdictRemediationOnly, Summary: outcome.Summary, Remediation: outcome.Remediation}))
		return
	}

	// 3c. A long-term code fix is warranted. In observe mode, stop here without
	// touching GitHub/Slack — the record still captures what would have happened.
	if !acting {
		log.Info("observe mode: would have opened a PR", zap.String("pr_title", outcome.Fix.PRTitle))
		finish(withUsage(InvestigationRecord{Verdict: verdictFixProposed, Summary: outcome.Summary, ProposedFix: outcome.Fix}))
		return
	}

	prURL, err := gh.CreatePR(*outcome.Fix, payload.IssueID)
	if err != nil {
		log.Warn("failed to create PR", zap.Error(err))
		_ = slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS,
			fmt.Sprintf("🔍 *Diagnosis*: %s\n\n⚠️ Could not open PR automatically: %s\n\n💰 Cost: $%.4f", outcome.Summary, err, tracker.spentUSD))
		finish(withUsage(InvestigationRecord{Verdict: verdictFailed, Summary: outcome.Summary, Error: err.Error(), ProposedFix: outcome.Fix}))
		return
	}
	log.Info("PR created", zap.String("url", prURL))

	// 4. Reply in the Slack thread.
	msg := fmt.Sprintf("🔍 *Diagnosis*: %s\n\n%s\n\n🔧 Fix PR: %s\n\n💰 Cost: $%.4f", payload.DiagnosisName, outcome.Summary, prURL, tracker.spentUSD)
	if err := slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS, msg); err != nil {
		log.Warn("failed to post to slack", zap.Error(err))
	}
	finish(withUsage(InvestigationRecord{Verdict: verdictFixProposed, Summary: outcome.Summary, PRUrl: prURL, ProposedFix: outcome.Fix}))
}

// mcpSource is one configured MCP server together with the tools it exposed
// as of the start of this investigation. Name is the tool-name prefix
// (e.g. "causely", "grafana") Claude sees on every tool from this server.
type mcpSource struct {
	Name        string
	Description string
	Client      *mcpClient
	Tools       []mcpToolDef
}

// loadMCPSources fetches the tool list from every MCP server in cfg.MCPServers
// once per investigation. A server that fails to respond is logged and
// skipped (contributes zero tools) rather than aborting the whole run — one
// down MCP source shouldn't block investigating via the others.
func loadMCPSources(ctx context.Context, cfg Config, log *zap.Logger) []mcpSource {
	sources := make([]mcpSource, 0, len(cfg.MCPServers))
	for _, s := range cfg.MCPServers {
		client := s.newClient()
		tools, err := client.ListToolsContext(ctx)
		if err != nil {
			log.Warn("failed to list MCP tools", zap.String("mcp_server", s.Name), zap.Error(err))
			tools = nil
		}
		log.Info("loaded MCP tools", zap.String("mcp_server", s.Name), zap.Int("count", len(tools)))
		sources = append(sources, mcpSource{Name: s.Name, Description: s.Description, Client: client, Tools: tools})
	}
	return sources
}

// findMCPSource returns the source whose prefix ("<name>__") matches toolName,
// and the tool name with that prefix stripped. ok is false if toolName isn't
// prefixed by any configured source.
func findMCPSource(sources []mcpSource, toolName string) (source mcpSource, mcpToolName string, ok bool) {
	for _, s := range sources {
		prefix := s.Name + "__"
		if strings.HasPrefix(toolName, prefix) {
			return s, strings.TrimPrefix(toolName, prefix), true
		}
	}
	return mcpSource{}, "", false
}

// partialFindingsSuffix appends whatever Claude had written so far when the
// loop was aborted for budget reasons, so the budget cap doesn't throw away
// investigation work that's already useful to a human reader.
func partialFindingsSuffix(summary string, err error) string {
	if !errors.Is(err, errBudgetExceeded) || summary == "" {
		return ""
	}
	return "\n\n🔍 *Partial findings*: " + summary
}

// --- Anthropic API types ---

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []contentBlock
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools"`
}

type anthropicResponse struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ProposedFix is the structured output Claude returns via the propose_fix tool.
type ProposedFix struct {
	PRTitle string       `json:"pr_title"`
	PRBody  string       `json:"pr_body"`
	Changes []FileChange `json:"changes"`
}

type FileChange struct {
	Path    string `json:"path"`
	Search  string `json:"search"`
	Replace string `json:"replace"`
}

// investigationOutcome is what runClaudeLoop produces on success. Exactly one
// of Fix, Remediation, or NoActionReasoning is meaningful: Fix != nil means
// Claude called propose_fix (a long-term code change, PR-worthy); Remediation
// != "" means it called recommend_remediation (an immediate action that
// doesn't need a code change); NoActionReasoning != "" means it called
// no_action_needed — verification found no genuine defect (already
// self-resolved with no live drift, or the flagged exception/log is already
// gracefully handled by design), so neither of the other two applies. Without
// this third option the tool schema would force an action-shaped answer even
// when the correct answer is "nothing is actually wrong" — found dogfooding:
// a diagnosis named "Unhandled...Exception" whose own source code caught and
// gracefully handled that exact exception by design.
type investigationOutcome struct {
	Fix               *ProposedFix
	Remediation       string
	NoActionReasoning string
	Summary           string
}

var proposefixSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pr_title": {"type": "string"},
    "pr_body":  {"type": "string"},
    "changes": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "path":    {"type": "string", "description": "file path in the repo"},
          "search":  {"type": "string", "description": "exact text to find"},
          "replace": {"type": "string", "description": "replacement text"}
        },
        "required": ["path", "search", "replace"]
      }
    }
  },
  "required": ["pr_title", "pr_body", "changes"]
}`)

var recommendRemediationSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "what you found investigating the issue"},
    "recommended_action": {"type": "string", "description": "the immediate remediation to take right now (e.g. restart, rollback, scale up, revert a config) — not a code change"}
  },
  "required": ["recommended_action"]
}`)

var noActionNeededSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "what you found investigating the issue"},
    "reasoning": {"type": "string", "description": "why no remediation or fix is warranted — e.g. the exception is already caught and handled by design (cite the file/lines), or the issue already fully self-resolved with the live state already matching what any fix would produce"}
  },
  "required": ["reasoning"]
}`)

func buildTools(sources []mcpSource, repo string, kubectlAvailable, acting bool) []anthropicTool {
	total := 0
	for _, s := range sources {
		total += len(s.Tools)
	}
	tools := make([]anthropicTool, 0, total+9)

	// MCP tools from every configured server, prefixed by source name so
	// Claude's tool calls can be routed back to the right server.
	for _, s := range sources {
		for _, t := range s.Tools {
			tools = append(tools, anthropicTool{
				Name:        s.Name + "__" + t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
	}

	// GitHub file reading tools.
	tools = append(tools,
		anthropicTool{
			Name:        "read_file",
			Description: "Read a file from the GitHub repository " + repo,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
		anthropicTool{
			Name:        "list_directory",
			Description: "List files in a directory of the GitHub repository " + repo,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","default":"."}},"required":[]}`),
		},
		anthropicTool{
			Name:        "search_code",
			Description: "Search for code across the whole GitHub repository " + repo + " by keyword, symbol name, or file path fragment. Use this instead of blindly walking directories in a large repo — e.g. search for a function name, error string, or config key to jump straight to the relevant file.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"GitHub code search query, e.g. a symbol name or literal string"}},"required":["query"]}`),
		},
		// propose_fix and recommend_remediation both terminate the loop —
		// exactly one of them should be called, whichever fits the finding.
		anthropicTool{
			Name:        "propose_fix",
			Description: "Submit a proposed long-term code fix, opening a GitHub PR. Call this only when the underlying issue is a genuine code-level bug that an immediate remediation can't address.",
			InputSchema: proposefixSchema,
		},
		anthropicTool{
			Name:        "recommend_remediation",
			Description: "Recommend an immediate remediation (restart, rollback, scale, revert a config) with no code change and no PR. Prefer this whenever an immediate action resolves the issue.",
			InputSchema: recommendRemediationSchema,
		},
		anthropicTool{
			Name:        "no_action_needed",
			Description: "Call this when investigation shows there is no genuine defect to remediate or fix — the flagged exception/log is already caught and handled gracefully by the code (by design, not a bug), or the issue has already fully self-resolved and the live state already matches what any fix would produce. Do not force recommend_remediation or propose_fix when neither actually applies.",
			InputSchema: noActionNeededSchema,
		},
	)

	if kubectlAvailable {
		tools = append(tools,
			anthropicTool{
				Name:        "kubectl_get",
				Description: "Get a live Kubernetes resource as JSON — the most authoritative source of current cluster state, more so than any cached/indexed view. Supported kinds: deployment, statefulset, daemonset, replicaset, pod, service, configmap. Does NOT return Secret content — use kubectl_get_secret_keys for that.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string"},"namespace":{"type":"string"},"name":{"type":"string"}},"required":["kind","namespace","name"]}`),
			},
			anthropicTool{
				Name:        "kubectl_get_secret_keys",
				Description: "List the key names (not values) present in a Kubernetes Secret's data — lets you confirm a Secret's shape (e.g. does it have a given config key) without exposing its contents.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"},"name":{"type":"string"}},"required":["namespace","name"]}`),
			},
			anthropicTool{
				Name:        "kubectl_logs",
				Description: "Fetch recent log lines from a pod's container.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"},"pod":{"type":"string"},"container":{"type":"string","description":"omit if the pod has only one container"},"tail_lines":{"type":"integer","default":100}},"required":["namespace","pod"]}`),
			},
		)
		if acting {
			tools = append(tools,
				anthropicTool{
					Name:        "kubectl_rollout_restart",
					Description: "Trigger a rolling restart of a Deployment, StatefulSet, or DaemonSet — equivalent to `kubectl rollout restart`. This executes immediately; only call it once you're confident it's the right remediation.",
					InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["deployment","statefulset","daemonset"]},"namespace":{"type":"string"},"name":{"type":"string"}},"required":["kind","namespace","name"]}`),
				},
				anthropicTool{
					Name:        "kubectl_scale",
					Description: "Set the replica count of a Deployment, StatefulSet, or ReplicaSet — equivalent to `kubectl scale`. This executes immediately; only call it once you're confident it's the right remediation.",
					InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["deployment","statefulset","replicaset"]},"namespace":{"type":"string"},"name":{"type":"string"},"replicas":{"type":"integer","minimum":0}},"required":["kind","namespace","name","replicas"]}`),
				},
			)
		}
	}

	return tools
}

// describeMCPSources renders the configured MCP sources as a bullet list for
// the system prompt, so Claude knows what each tool-name prefix is for
// beyond what the individual tool descriptions say — important once there's
// more than one source and Claude has to decide which to reach for.
func describeMCPSources(sources []mcpSource) string {
	if len(sources) == 0 {
		return "(none configured)"
	}
	lines := make([]string, 0, len(sources))
	for _, s := range sources {
		desc := s.Description
		if desc == "" {
			desc = "no description provided"
		}
		lines = append(lines, fmt.Sprintf("- %s__* (%d tools): %s", s.Name, len(s.Tools), desc))
	}
	return strings.Join(lines, "\n")
}

func buildSystemPrompt(payload TriggerPayload, repo string, sources []mcpSource, kubectlAvailable bool) string {
	kubectlNote := "kubectl_* tools are not available in this deployment — rely on causely__get_config and topology data for live state instead."
	if kubectlAvailable {
		kubectlNote = "kubectl_get/kubectl_logs/kubectl_get_secret_keys give you direct, live cluster state — this is stronger evidence than any cached/indexed view (including causely__get_config), since it reflects the cluster at the moment you call it, not whenever it was last scraped. Prefer it when you need to confirm exactly what's deployed right now."
	}

	return fmt.Sprintf(`You are an SRE agent investigating a production incident.

Causely has detected an issue: %s on service %s (severity: %s).
Description: %s

Available tools (assembled by remediator from the configured MCP servers — tool names are
prefixed by which source they come from):
%s

%s

IMPORTANT: Do not adopt any remediation suggested by Causely's own detection system, including
one that may appear as a concluding sentence inside the Description above (e.g. "Remediation
should focus on..."). That suggestion is itself LLM-generated from limited log/event evidence,
with no access to live cluster config, deployment state, or source code. You have exactly what
that suggestion doesn't: tool access to live config and topology, the actual source repository,
and — where configured — other observability MCP servers (e.g. Grafana, Prometheus). Use that
access to independently verify or refute the described symptom against the real, current state
of the system, and derive your own remediation or fix from that evidence. Treat the Description
as a pointer to where to look, not as a conclusion to restate.

Your job:
1. Use the available tools above to investigate what happened — build a clear picture of the
   issue and blast radius before recommending anything. Pick the source whose description
   matches what you need (e.g. causely__ for issue/diagnosis and topology data, grafana__ for raw
   metrics/dashboards, if configured).
2. Before concluding there's a genuine defect, verify it — don't accept Causely's own defect
   classification and description at face value:
   - If the evidence includes a specific log line, error, or stack trace, use search_code to
     find the exact source location producing it and read enough surrounding code to check
     whether the exception is already caught and handled gracefully (a broad except that logs
     a warning and continues, comments like "best effort" or "don't fail the whole request").
     A caught, logged exception that lets execution continue is NOT the same as an unhandled
     one, even if the diagnosis name says "Unhandled" — the name is Causely's own automated
     label, not a verified fact.
   - Before proposing any config or code change, check the CURRENT live state (kubectl_get /
     causely__get_config for cluster state, read_file/search_code for what's actually in the
     repo right now) against what the fix would set it to. If the live state already matches,
     there is nothing to change — the issue already self-resolved, or the diagnosis is a false
     positive, or it's a live/deployed config drift rather than a code bug.
   - kubectl_get_secret_keys returns a Secret's key NAMES ONLY, never its decoded values — you
     have no way to see Secret content. Never assert a claim about what a Secret currently
     contains, or that one "has been updated," unless you got that from a source that actually
     shows content (e.g. the repo's own manifest/values file, or a ConfigMap). If a symptom
     cleared and you can't otherwise explain why, say the mechanism is unconfirmed rather than
     inventing a plausible-sounding one.
   - Before citing source code as evidence that a specific caller does (or doesn't do)
     something — e.g. "this BRPOP call has no timeout" — confirm you found the actual call site
     (the real client method invocation, e.g. grep for ".BRPop(" not just the string "brpop"),
     and read its actual arguments. Code that merely recognizes or pattern-matches a command
     name (an observability span-classifier, a log-text symptom matcher) is NOT the same as the
     application code that issues that command — don't cite the former as if it were the
     latter. If you can't find the real call site, say so explicitly rather than presenting an
     inference as a confirmed finding.
3. If, after this verification, you find no genuine defect — the flagged exception is
   deliberately caught and handled by design, or the issue has already fully self-resolved
   with the live state already matching what any fix would produce — call no_action_needed.
   Don't force a remediation or fix just because one feels expected — concluding that
   nothing is actually wrong is a legitimate, first-class outcome when the evidence supports it.
4. Otherwise, immediate remediation is preferred. If restarting, rolling back, scaling, or
   reverting a config resolves the issue, call recommend_remediation with that action — do not
   touch code for something an immediate action already fixes.
5. Only pursue a long-term code fix if the underlying issue is a genuine code-level bug that an
   immediate remediation can't address. In that case: use search_code to jump straight to the
   relevant file(s) in repository %s — prefer it over list_directory when you have a symbol
   name, error string, or config key to search for; this is a large monorepo and blind
   directory walking wastes time and money. Use read_file to confirm the exact code, then call
   propose_fix with the exact search/replace change, a PR title, and a PR body.

Call exactly one of recommend_remediation, propose_fix, or no_action_needed to finish. Be
specific — propose_fix.changes must contain exact strings that appear in the code.`,
		payload.DiagnosisName, payload.EntityName, payload.Severity,
		payload.Description, describeMCPSources(sources), kubectlNote, repo)
}

// runClaudeLoop runs the Claude tool-use loop until Claude calls propose_fix
// or recommend_remediation. It returns the result alongside the cost tracker
// (USD spend + token totals) and a per-tool-call summary — the latter two are
// what let an InvestigationRecord answer "how much digging did this need,"
// independent of whether the loop finished normally or was aborted.
func runClaudeLoop(ctx context.Context, cfg Config, payload TriggerPayload, sources []mcpSource, gh *githubClient, kc *kubeClient, log *zap.Logger) (investigationOutcome, *costTracker, []ToolCallSummary, error) {
	acting := cfg.ActionMode == actionModeAct
	tools := buildTools(sources, cfg.GitHubRepo, kc != nil, acting)

	messages := []anthropicMessage{{
		Role:    "user",
		Content: fmt.Sprintf("Investigate issue '%s' (id: %s) on entity '%s'. Use available tools (e.g. causely__get_issue_details) to pull full evidence for this issue, then call recommend_remediation, propose_fix, or no_action_needed.", payload.DiagnosisName, payload.IssueID, payload.EntityName),
	}}

	system := buildSystemPrompt(payload, cfg.GitHubRepo, sources, kc != nil)
	tracker := newCostTracker(cfg.MaxCostUSD)
	var allText []string
	var toolCallSummaries []ToolCallSummary
	seenArguments := make(map[string]struct{})
	seenResults := make(map[string]struct{})

	for iteration := range 20 { // max iterations
		if tracker.exceeded() {
			log.Warn("cost budget exceeded, aborting investigation",
				zap.Float64("spent_usd", tracker.spentUSD), zap.Float64("budget_usd", tracker.budgetUSD))
			return investigationOutcome{Summary: strings.Join(allText, "\n")}, tracker, toolCallSummaries, errBudgetExceeded
		}

		resp, err := callClaude(ctx, cfg.AnthropicKey, system, messages, tools)
		if err != nil {
			return investigationOutcome{Summary: strings.Join(allText, "\n")}, tracker, toolCallSummaries, err
		}
		tracker.add(claudeModel, resp.Usage.InputTokens, resp.Usage.OutputTokens)

		// Collect tool calls and assistant message.
		var toolCalls []contentBlock
		var textParts []string
		for _, block := range resp.Content {
			switch block.Type {
			case "tool_use":
				toolCalls = append(toolCalls, block)
			case "text":
				if block.Text != "" {
					textParts = append(textParts, block.Text)
				}
			}
		}
		allText = append(allText, textParts...)

		// Append assistant turn.
		messages = append(messages, anthropicMessage{Role: "assistant", Content: resp.Content})

		if resp.StopReason == "end_turn" || len(toolCalls) == 0 {
			return investigationOutcome{Summary: strings.Join(allText, "\n")}, tracker, toolCallSummaries, fmt.Errorf("agent ended without calling recommend_remediation, propose_fix, or no_action_needed")
		}

		// Execute tool calls.
		var results []contentBlock
		for _, tc := range toolCalls {
			callStartedAt := time.Now()
			var args map[string]any
			_ = json.Unmarshal(tc.Input, &args)
			argumentFingerprint := fingerprint(args)

			log.Info("tool call", zap.String("tool", tc.Name))

			var result string
			var callErr error

			mcpSrc, mcpToolName, isMCPTool := findMCPSource(sources, tc.Name)
			server := "unknown"
			switch {
			case isMCPTool:
				server = mcpSrc.Name
			case tc.Name == "read_file" || tc.Name == "list_directory" || tc.Name == "search_code":
				server = "github"
			case strings.HasPrefix(tc.Name, "kubectl_"):
				server = "kubectl"
			case tc.Name == "propose_fix" || tc.Name == "recommend_remediation" || tc.Name == "no_action_needed":
				server = "agent"
			}

			var toolSpan trace.Span
			toolCallCtx := ctx
			isTerminalTool := server == "agent"
			if !isTerminalTool {
				key := server + "\x00" + tc.Name + "\x00" + argumentFingerprint
				_, toolCallRepeated := seenArguments[key]
				seenArguments[key] = struct{}{}
				attrs := []attribute.KeyValue{
					attribute.String("causely.agent.operation.name", agentWorkflowName),
					attribute.String("gen_ai.tool.name", tc.Name),
					attribute.Int("causely.agent.tool_calls", 1),
					attribute.Int("causely.agent.repeated_tool_calls", boolInt(toolCallRepeated)),
				}
				spanOptions := []trace.SpanStartOption{
					trace.WithSpanKind(trace.SpanKindInternal),
					trace.WithAttributes(attrs...),
				}
				spanName := "execute_tool " + tc.Name
				if isMCPTool {
					u, _ := url.Parse(mcpSrc.Client.url)
					port := 443
					if u.Scheme == "http" {
						port = 80
					}
					if parsedPort := u.Port(); parsedPort != "" {
						_, _ = fmt.Sscan(parsedPort, &port)
					}
					spanName = "tools/call " + mcpToolName
					spanOptions = []trace.SpanStartOption{
						trace.WithSpanKind(trace.SpanKindClient),
						trace.WithAttributes(append(attrs,
							attribute.String("mcp.method.name", "tools/call "+mcpToolName),
							attribute.String("server.address", u.Hostname()),
							attribute.Int("server.port", port),
						)...),
					}
				}
				toolCallCtx, toolSpan = otel.Tracer(telemetryInstrumentation).Start(ctx, spanName, spanOptions...)
			}

			switch {
			case tc.Name == "propose_fix":
				var fix ProposedFix
				if err := json.Unmarshal(tc.Input, &fix); err != nil {
					return investigationOutcome{}, tracker, toolCallSummaries, fmt.Errorf("parse propose_fix: %w", err)
				}
				if !fixHasRealChange(fix) {
					// Every change is a no-op (search == replace, or no changes at all) —
					// most commonly seen when the repo already matches the desired end
					// state and the actual issue is a live/deployed config drift, not
					// a code bug. Reject and let Claude reconsider rather than silently
					// terminating the loop with a PR-worthy verdict that would open an
					// empty, misleading PR in act mode.
					server = "agent"
					callErr = errNoOpFix
					break
				}
				return investigationOutcome{Fix: &fix, Summary: strings.Join(allText, "\n")}, tracker, toolCallSummaries, nil

			case tc.Name == "recommend_remediation":
				var rec struct {
					Summary           string `json:"summary"`
					RecommendedAction string `json:"recommended_action"`
				}
				if err := json.Unmarshal(tc.Input, &rec); err != nil {
					return investigationOutcome{}, tracker, toolCallSummaries, fmt.Errorf("parse recommend_remediation: %w", err)
				}
				summary := rec.Summary
				if summary == "" {
					summary = strings.Join(allText, "\n")
				}
				return investigationOutcome{Remediation: rec.RecommendedAction, Summary: summary}, tracker, toolCallSummaries, nil

			case tc.Name == "no_action_needed":
				var na struct {
					Summary   string `json:"summary"`
					Reasoning string `json:"reasoning"`
				}
				if err := json.Unmarshal(tc.Input, &na); err != nil {
					return investigationOutcome{}, tracker, toolCallSummaries, fmt.Errorf("parse no_action_needed: %w", err)
				}
				summary := na.Summary
				if summary == "" {
					summary = strings.Join(allText, "\n")
				}
				return investigationOutcome{NoActionReasoning: na.Reasoning, Summary: summary}, tracker, toolCallSummaries, nil

			case isMCPTool:
				result, callErr = mcpSrc.Client.CallToolContext(toolCallCtx, mcpToolName, args)

			case tc.Name == "read_file":
				path, _ := args["path"].(string)
				result, callErr = gh.ReadFile(path)

			case tc.Name == "list_directory":
				path, _ := args["path"].(string)
				result, callErr = gh.ListDirectory(path)

			case tc.Name == "search_code":
				query, _ := args["query"].(string)
				result, callErr = gh.SearchCode(query)

			case tc.Name == "kubectl_get" && kc != nil:
				kind, _ := args["kind"].(string)
				ns, _ := args["namespace"].(string)
				name, _ := args["name"].(string)
				result, callErr = kc.GetResource(context.Background(), kind, ns, name)

			case tc.Name == "kubectl_get_secret_keys" && kc != nil:
				server = "kubectl"
				ns, _ := args["namespace"].(string)
				name, _ := args["name"].(string)
				result, callErr = kc.GetSecretKeys(context.Background(), ns, name)

			case tc.Name == "kubectl_logs" && kc != nil:
				server = "kubectl"
				ns, _ := args["namespace"].(string)
				pod, _ := args["pod"].(string)
				container, _ := args["container"].(string)
				tailLines := int64(100)
				if v, ok := args["tail_lines"].(float64); ok {
					tailLines = int64(v)
				}
				result, callErr = kc.GetPodLogs(context.Background(), ns, pod, container, tailLines)

			case tc.Name == "kubectl_rollout_restart" && kc != nil && acting:
				server = "kubectl"
				kind, _ := args["kind"].(string)
				ns, _ := args["namespace"].(string)
				name, _ := args["name"].(string)
				result, callErr = kc.RolloutRestart(context.Background(), kind, ns, name)

			case tc.Name == "kubectl_scale" && kc != nil && acting:
				server = "kubectl"
				kind, _ := args["kind"].(string)
				ns, _ := args["namespace"].(string)
				name, _ := args["name"].(string)
				var replicas int32
				if v, ok := args["replicas"].(float64); ok {
					replicas = int32(v)
				}
				result, callErr = kc.ScaleResource(context.Background(), kind, ns, name, replicas)

			default:
				server = "unknown"
				result = "unknown tool"
			}

			if callErr != nil {
				log.Warn("tool error", zap.String("tool", tc.Name), zap.Error(callErr))
				result = "error: " + callErr.Error()
			}
			resultFingerprint := fingerprint(result)
			if toolSpan != nil {
				resultKey := server + "\x00" + tc.Name + "\x00" + resultFingerprint
				_, duplicateResult := seenResults[resultKey]
				seenResults[resultKey] = struct{}{}
				toolSpan.SetAttributes(attribute.Int("causely.agent.duplicate_tool_results", boolInt(duplicateResult)))
				if callErr != nil {
					toolSpan.RecordError(callErr)
					toolSpan.SetStatus(codes.Error, callErr.Error())
				}
				toolSpan.End()
			}
			toolCallSummaries = append(toolCallSummaries, ToolCallSummary{
				Server:              server,
				Tool:                tc.Name,
				Error:               callErr != nil,
				Iteration:           iteration + 1,
				Sequence:            len(toolCallSummaries) + 1,
				DurationMS:          time.Since(callStartedAt).Milliseconds(),
				ArgumentFingerprint: argumentFingerprint,
				ResultFingerprint:   resultFingerprint,
			})

			results = append(results, contentBlock{
				Type:      "tool_result",
				ToolUseID: tc.ID,
				Content:   result,
			})
		}

		messages = append(messages, anthropicMessage{Role: "user", Content: results})
	}

	return investigationOutcome{Summary: strings.Join(allText, "\n")}, tracker, toolCallSummaries, fmt.Errorf("reached max iterations without recommend_remediation, propose_fix, or no_action_needed")
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// noProgressDurationMS approximates time spent after the last tool result that
// added new evidence. It deliberately uses only fingerprints and durations,
// never request or response payloads.
func noProgressDurationMS(calls []ToolCallSummary) int64 {
	seen := make(map[string]struct{}, len(calls))
	var duration int64
	for _, call := range calls {
		key := call.Server + "\x00" + call.Tool + "\x00" + call.ResultFingerprint
		if _, duplicate := seen[key]; duplicate {
			duration += call.DurationMS
		} else {
			duration = 0
		}
		seen[key] = struct{}{}
	}
	return duration
}

func fingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum)
}

func repeatedToolCalls(calls []ToolCallSummary) int {
	seen := make(map[string]struct{}, len(calls))
	repeated := 0
	for _, call := range calls {
		key := call.Server + "\x00" + call.Tool + "\x00" + call.ArgumentFingerprint
		if _, ok := seen[key]; ok {
			repeated++
		}
		seen[key] = struct{}{}
	}
	return repeated
}

func redundantToolCalls(calls []ToolCallSummary) int {
	seen := make(map[string]struct{}, len(calls))
	redundant := 0
	for _, call := range calls {
		key := call.Server + "\x00" + call.Tool + "\x00" + call.ArgumentFingerprint + "\x00" + call.ResultFingerprint
		if _, ok := seen[key]; ok {
			redundant++
		}
		seen[key] = struct{}{}
	}
	return redundant
}

// duplicateToolResults counts later calls to the same tool that produce an
// already-seen result despite using different arguments. It is a stronger
// no-new-evidence signal than call volume alone, while remaining agnostic to
// whether the agent's retry or query refinement was justified.
func duplicateToolResults(calls []ToolCallSummary) int {
	seen := make(map[string]struct{}, len(calls))
	duplicates := 0
	for _, call := range calls {
		key := call.Server + "\x00" + call.Tool + "\x00" + call.ResultFingerprint
		if _, ok := seen[key]; ok {
			duplicates++
		}
		seen[key] = struct{}{}
	}
	return duplicates
}

func callClaude(ctx context.Context, apiKey, system string, messages []anthropicMessage, tools []anthropicTool) (*anthropicResponse, error) {
	req := anthropicRequest{
		Model:     claudeModel,
		MaxTokens: 4096,
		System:    system,
		Messages:  messages,
		Tools:     tools,
	}
	body, _ := json.Marshal(req)

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(httpReq.Header))

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic %s: %s", resp.Status, string(raw))
	}

	var result anthropicResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("anthropic decode: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("anthropic error: %s", result.Error.Message)
	}
	return &result, nil
}
