package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// errBudgetExceeded is returned by runClaudeLoop when the investigation's cost
// budget (Config.MaxCostUSD) is exceeded before propose_fix is called.
var errBudgetExceeded = errors.New("investigation aborted: cost budget exceeded")

// runAgent is the top-level entry point called per trigger, regardless of
// trigger source (webhook, poll, Slack button). weekly enforces the aggregate
// rolling-window cost cap shared across every investigation this process
// runs — independent of the per-investigation cap inside runClaudeLoop. rec
// (may be nil) receives one InvestigationRecord no matter how this call ends
// — skipped, failed, or completed — so behavior and RCA quality can be
// measured later instead of only observed anecdotally in logs/Slack.
func runAgent(logger *zap.Logger, cfg Config, payload TriggerPayload, weekly *weeklyBudget, rec *recorder, triggerSource string) {
	log := logger.With(
		zap.String("rc", payload.RootCauseName),
		zap.String("entity", payload.EntityName),
		zap.String("severity", payload.Severity),
		zap.String("trigger_source", triggerSource),
	)
	log.Info("agent triggered")

	startedAt := time.Now()
	finish := func(ir InvestigationRecord) {
		ir.RootCauseID = payload.RootCauseID
		ir.EntityID = payload.EntityID
		ir.EntityName = payload.EntityName
		ir.RootCauseName = payload.RootCauseName
		ir.Severity = payload.Severity
		ir.TriggerSource = triggerSource
		ir.ActionMode = cfg.ActionMode
		ir.StartedAt = startedAt
		ir.FinishedAt = time.Now()
		ir.DurationMS = ir.FinishedAt.Sub(startedAt).Milliseconds()
		rec.record(ir)
	}
	acting := cfg.ActionMode == actionModeAct

	if ok, reason := inScope(cfg, payload); !ok {
		log.Info("root cause out of scope for this agent instance, skipping", zap.String("reason", reason))
		finish(InvestigationRecord{Verdict: verdictSkippedScope, SkipReason: reason})
		return
	}

	if weekly.exceeded() {
		reason := fmt.Sprintf("weekly cost cap ($%.2f) already reached", weekly.limitUSD)
		log.Warn("weekly cost cap already reached, skipping investigation",
			zap.Float64("weekly_limit_usd", weekly.limitUSD), zap.Float64("weekly_spent_usd", weekly.spent(time.Now())))
		if acting {
			slack := newSlackClient(cfg.SlackBotToken)
			_ = slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS,
				fmt.Sprintf("⚠️ causely-background-agent skipped investigating %s: %s", payload.RootCauseName, reason))
		}
		finish(InvestigationRecord{Verdict: verdictSkippedBudget, SkipReason: reason})
		return
	}

	gh := newGitHubClient(cfg.GitHubToken, cfg.GitHubRepo)
	slack := newSlackClient(cfg.SlackBotToken)

	// 1. Load tools from every configured MCP server (Causely, and whatever
	// else is set via MCP_SERVERS_JSON — see mcp_config.go).
	sources := loadMCPSources(cfg, log)

	// 2. Run the agent loop: Claude investigates via MCP + GitHub, then either
	// recommends an immediate remediation or proposes a long-term code fix.
	outcome, tracker, toolCalls, err := runClaudeLoop(cfg, payload, sources, gh, log)
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
					payload.RootCauseName, err, tracker.spentUSD, partialFindingsSuffix(outcome.Summary, err)))
		}
		finish(withUsage(InvestigationRecord{Verdict: verdictFailed, Error: err.Error(), Summary: outcome.Summary}))
		return
	}

	// 3a. No code fix needed — an immediate remediation is sufficient. Post the
	// recommendation (if acting) and stop; there's no PR to open.
	if outcome.Fix == nil {
		if acting {
			msg := fmt.Sprintf("🔍 *Root cause*: %s\n\n%s\n\n⚡ *Recommended immediate remediation*: %s\n\n💰 Cost: $%.4f",
				payload.RootCauseName, outcome.Summary, outcome.Remediation, tracker.spentUSD)
			if err := slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS, msg); err != nil {
				log.Warn("failed to post to slack", zap.Error(err))
			}
		} else {
			log.Info("observe mode: would have recommended remediation", zap.String("remediation", outcome.Remediation))
		}
		finish(withUsage(InvestigationRecord{Verdict: verdictRemediationOnly, Summary: outcome.Summary, Remediation: outcome.Remediation}))
		return
	}

	// 3b. A long-term code fix is warranted. In observe mode, stop here without
	// touching GitHub/Slack — the record still captures what would have happened.
	if !acting {
		log.Info("observe mode: would have opened a PR", zap.String("pr_title", outcome.Fix.PRTitle))
		finish(withUsage(InvestigationRecord{Verdict: verdictFixProposed, Summary: outcome.Summary}))
		return
	}

	prURL, err := gh.CreatePR(*outcome.Fix, payload.RootCauseID)
	if err != nil {
		log.Warn("failed to create PR", zap.Error(err))
		_ = slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS,
			fmt.Sprintf("🔍 *Diagnosis*: %s\n\n⚠️ Could not open PR automatically: %s\n\n💰 Cost: $%.4f", outcome.Summary, err, tracker.spentUSD))
		finish(withUsage(InvestigationRecord{Verdict: verdictFailed, Summary: outcome.Summary, Error: err.Error()}))
		return
	}
	log.Info("PR created", zap.String("url", prURL))

	// 4. Reply in the Slack thread.
	msg := fmt.Sprintf("🔍 *Root cause*: %s\n\n%s\n\n🔧 Fix PR: %s\n\n💰 Cost: $%.4f", payload.RootCauseName, outcome.Summary, prURL, tracker.spentUSD)
	if err := slack.PostToThread(payload.SlackChannel, payload.SlackThreadTS, msg); err != nil {
		log.Warn("failed to post to slack", zap.Error(err))
	}
	finish(withUsage(InvestigationRecord{Verdict: verdictFixProposed, Summary: outcome.Summary, PRUrl: prURL}))
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
func loadMCPSources(cfg Config, log *zap.Logger) []mcpSource {
	sources := make([]mcpSource, 0, len(cfg.MCPServers))
	for _, s := range cfg.MCPServers {
		client := newMCPClient(s.URL, s.Token)
		tools, err := client.ListTools()
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
// of Fix or Remediation is meaningful: Fix != nil means Claude called
// propose_fix (a long-term code change, PR-worthy); Fix == nil means Claude
// called recommend_remediation instead (an immediate action — restart,
// rollback, scale — that doesn't need a code change at all).
type investigationOutcome struct {
	Fix         *ProposedFix
	Remediation string
	Summary     string
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
    "summary": {"type": "string", "description": "what you found investigating the root cause"},
    "recommended_action": {"type": "string", "description": "the immediate remediation to take right now (e.g. restart, rollback, scale up, revert a config) — not a code change"}
  },
  "required": ["recommended_action"]
}`)

func buildTools(sources []mcpSource, repo string) []anthropicTool {
	total := 0
	for _, s := range sources {
		total += len(s.Tools)
	}
	tools := make([]anthropicTool, 0, total+4)

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
			Description: "Submit a proposed long-term code fix, opening a GitHub PR. Call this only when the root cause is a genuine code-level bug that an immediate remediation can't address.",
			InputSchema: proposefixSchema,
		},
		anthropicTool{
			Name:        "recommend_remediation",
			Description: "Recommend an immediate remediation (restart, rollback, scale, revert a config) with no code change and no PR. Prefer this whenever an immediate action resolves the issue.",
			InputSchema: recommendRemediationSchema,
		},
	)
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

func buildSystemPrompt(payload TriggerPayload, repo string, sources []mcpSource) string {
	return fmt.Sprintf(`You are an SRE agent investigating a production incident.

Causely has detected a root cause: %s on service %s (severity: %s).
Description: %s
Remediation hint: %s

Available tools (assembled by remediator from the configured MCP servers — tool names are
prefixed by which source they come from):
%s

Your job:
1. Use the available tools above to investigate what happened — build a clear picture of the
   root cause and blast radius before recommending anything. Pick the source whose description
   matches what you need (e.g. causely__ for root-cause and topology data, grafana__ for raw
   metrics/dashboards, if configured).
2. Immediate remediation is preferred. If restarting, rolling back, scaling, or reverting a
   config resolves the issue, call recommend_remediation with that action — do not touch code
   for something an immediate action already fixes.
3. Only pursue a long-term code fix if the root cause is a genuine code-level bug that an
   immediate remediation can't address. In that case: use search_code to jump straight to the
   relevant file(s) in repository %s — prefer it over list_directory when you have a symbol
   name, error string, or config key to search for; this is a large monorepo and blind
   directory walking wastes time and money. Use read_file to confirm the exact code, then call
   propose_fix with the exact search/replace change, a PR title, and a PR body.

Call exactly one of recommend_remediation or propose_fix to finish. Be specific —
propose_fix.changes must contain exact strings that appear in the code.`,
		payload.RootCauseName, payload.EntityName, payload.Severity,
		payload.Description, payload.Remediation, describeMCPSources(sources), repo)
}

// runClaudeLoop runs the Claude tool-use loop until Claude calls propose_fix
// or recommend_remediation. It returns the result alongside the cost tracker
// (USD spend + token totals) and a per-tool-call summary — the latter two are
// what let an InvestigationRecord answer "how much digging did this need,"
// independent of whether the loop finished normally or was aborted.
func runClaudeLoop(cfg Config, payload TriggerPayload, sources []mcpSource, gh *githubClient, log *zap.Logger) (investigationOutcome, *costTracker, []ToolCallSummary, error) {
	tools := buildTools(sources, cfg.GitHubRepo)

	messages := []anthropicMessage{{
		Role:    "user",
		Content: fmt.Sprintf("Investigate root cause '%s' (id: %s) on entity '%s'. Use available tools, then call recommend_remediation or propose_fix.", payload.RootCauseName, payload.RootCauseID, payload.EntityName),
	}}

	system := buildSystemPrompt(payload, cfg.GitHubRepo, sources)
	tracker := newCostTracker(cfg.MaxCostUSD)
	var allText []string
	var toolCallSummaries []ToolCallSummary

	for range 20 { // max iterations
		if tracker.exceeded() {
			log.Warn("cost budget exceeded, aborting investigation",
				zap.Float64("spent_usd", tracker.spentUSD), zap.Float64("budget_usd", tracker.budgetUSD))
			return investigationOutcome{Summary: strings.Join(allText, "\n")}, tracker, toolCallSummaries, errBudgetExceeded
		}

		resp, err := callClaude(cfg.AnthropicKey, system, messages, tools)
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
			return investigationOutcome{Summary: strings.Join(allText, "\n")}, tracker, toolCallSummaries, fmt.Errorf("agent ended without calling recommend_remediation or propose_fix")
		}

		// Execute tool calls.
		var results []contentBlock
		for _, tc := range toolCalls {
			var args map[string]any
			_ = json.Unmarshal(tc.Input, &args)

			log.Info("tool call", zap.String("tool", tc.Name))

			var result string
			var callErr error
			var server string

			mcpSrc, mcpToolName, isMCPTool := findMCPSource(sources, tc.Name)

			switch {
			case tc.Name == "propose_fix":
				var fix ProposedFix
				if err := json.Unmarshal(tc.Input, &fix); err != nil {
					return investigationOutcome{}, tracker, toolCallSummaries, fmt.Errorf("parse propose_fix: %w", err)
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

			case isMCPTool:
				server = mcpSrc.Name
				result, callErr = mcpSrc.Client.CallTool(mcpToolName, args)

			case tc.Name == "read_file":
				server = "github"
				path, _ := args["path"].(string)
				result, callErr = gh.ReadFile(path)

			case tc.Name == "list_directory":
				server = "github"
				path, _ := args["path"].(string)
				result, callErr = gh.ListDirectory(path)

			case tc.Name == "search_code":
				server = "github"
				query, _ := args["query"].(string)
				result, callErr = gh.SearchCode(query)

			default:
				server = "unknown"
				result = "unknown tool"
			}

			if callErr != nil {
				log.Warn("tool error", zap.String("tool", tc.Name), zap.Error(callErr))
				result = "error: " + callErr.Error()
			}
			toolCallSummaries = append(toolCallSummaries, ToolCallSummary{Server: server, Tool: tc.Name, Error: callErr != nil})

			results = append(results, contentBlock{
				Type:      "tool_result",
				ToolUseID: tc.ID,
				Content:   result,
			})
		}

		messages = append(messages, anthropicMessage{Role: "user", Content: results})
	}

	return investigationOutcome{Summary: strings.Join(allText, "\n")}, tracker, toolCallSummaries, fmt.Errorf("reached max iterations without recommend_remediation or propose_fix")
}

func callClaude(apiKey, system string, messages []anthropicMessage, tools []anthropicTool) (*anthropicResponse, error) {
	req := anthropicRequest{
		Model:     claudeModel,
		MaxTokens: 4096,
		System:    system,
		Messages:  messages,
		Tools:     tools,
	}
	body, _ := json.Marshal(req)

	httpReq, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")

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
