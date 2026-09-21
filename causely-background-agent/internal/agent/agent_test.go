package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolCallRepetition(t *testing.T) {
	calls := []ToolCallSummary{
		{Server: "causely", Tool: "get_issue", ArgumentFingerprint: "a", ResultFingerprint: "1"},
		{Server: "causely", Tool: "get_issue", ArgumentFingerprint: "a", ResultFingerprint: "2"},
		{Server: "causely", Tool: "get_issue", ArgumentFingerprint: "a", ResultFingerprint: "2"},
		{Server: "causely", Tool: "get_issue", ArgumentFingerprint: "b", ResultFingerprint: "2"},
	}

	if got := repeatedToolCalls(calls); got != 2 {
		t.Fatalf("repeatedToolCalls() = %d, want 2", got)
	}
	if got := redundantToolCalls(calls); got != 1 {
		t.Fatalf("redundantToolCalls() = %d, want 1", got)
	}
	if got := duplicateToolResults(calls); got != 2 {
		t.Fatalf("duplicateToolResults() = %d, want 2", got)
	}
}

func TestInvestigationCompletionAttributes(t *testing.T) {
	ir := InvestigationRecord{Verdict: verdictFailed, CostUSD: 1.25}
	attrs := investigationCompletionAttributes(ir, 5)
	byKey := make(map[string]float64)
	for _, attr := range attrs {
		if attr.Key == "causely.agent.estimated_cost_usd" || attr.Key == "causely.agent.cost_budget_usd" {
			byKey[string(attr.Key)] = attr.Value.AsFloat64()
		}
	}
	if byKey["causely.agent.estimated_cost_usd"] != 1.25 {
		t.Fatalf("estimated cost = %v, want 1.25", byKey["causely.agent.estimated_cost_usd"])
	}
	if byKey["causely.agent.cost_budget_usd"] != 5 {
		t.Fatalf("cost budget = %v, want 5", byKey["causely.agent.cost_budget_usd"])
	}
}

func TestInvestigationCompletionAttributesOmitsDisabledCostBudget(t *testing.T) {
	attrs := investigationCompletionAttributes(InvestigationRecord{CostUSD: 1.25}, 0)
	for _, attr := range attrs {
		if attr.Key == "causely.agent.cost_budget_usd" {
			t.Fatal("disabled cost budget must not be emitted")
		}
	}
}

func TestNoProgressDurationMS(t *testing.T) {
	calls := []ToolCallSummary{
		{Server: "causely", Tool: "get_issue", ResultFingerprint: "a", DurationMS: 10},
		{Server: "causely", Tool: "get_issue", ResultFingerprint: "a", DurationMS: 20},
		{Server: "causely", Tool: "get_issue", ResultFingerprint: "a", DurationMS: 30},
		{Server: "causely", Tool: "get_logs", ResultFingerprint: "b", DurationMS: 40},
		{Server: "causely", Tool: "get_logs", ResultFingerprint: "b", DurationMS: 50},
	}
	if got := noProgressDurationMS(calls); got != 50 {
		t.Fatalf("noProgressDurationMS() = %d, want 50", got)
	}
}

func TestFingerprint_IsStableAndDoesNotContainPayload(t *testing.T) {
	first := fingerprint(map[string]any{"b": 2, "a": "secret-value"})
	second := fingerprint(map[string]any{"a": "secret-value", "b": 2})
	if first != second {
		t.Fatalf("fingerprints differ for equivalent arguments: %q != %q", first, second)
	}
	if strings.Contains(first, "secret-value") {
		t.Fatal("fingerprint contains raw payload")
	}
}

func TestBuildTools_PrefixesEachMCPSourceByItsName(t *testing.T) {
	sources := []mcpSource{
		{
			Name: "causely",
			Tools: []mcpToolDef{
				{Name: "get_logs", Description: "logs"},
				{Name: "get_issue_details", Description: "rcd"},
			},
		},
		{
			Name: "grafana",
			Tools: []mcpToolDef{
				{Name: "query_range", Description: "metrics"},
			},
		},
	}

	tools := buildTools(sources, "org/repo", false, false)

	names := make(map[string]bool, len(tools))
	for _, tl := range tools {
		names[tl.Name] = true
	}

	for _, want := range []string{"causely__get_logs", "causely__get_issue_details", "grafana__query_range"} {
		if !names[want] {
			t.Errorf("buildTools() missing %q, got %v", want, names)
		}
	}
	for _, want := range []string{"read_file", "list_directory", "search_code", "propose_fix"} {
		if !names[want] {
			t.Errorf("buildTools() missing built-in tool %q", want)
		}
	}
}

func TestBuildTools_NoMCPSourcesStillHasBuiltins(t *testing.T) {
	tools := buildTools(nil, "org/repo", false, false)
	if len(tools) != 6 {
		t.Fatalf("buildTools(nil) = %d tools, want exactly the 6 built-ins", len(tools))
	}
}

func TestBuildTools_IncludesNoActionNeeded(t *testing.T) {
	tools := buildTools(nil, "org/repo", false, false)
	found := false
	for _, tl := range tools {
		if tl.Name == "no_action_needed" {
			found = true
		}
	}
	if !found {
		t.Error("buildTools() missing no_action_needed — the agent has no way to conclude there's no genuine defect without it")
	}
}

func TestBuildTools_KubectlToolsOmittedWhenUnavailable(t *testing.T) {
	tools := buildTools(nil, "org/repo", false, true)
	for _, tl := range tools {
		if strings.HasPrefix(tl.Name, "kubectl_") {
			t.Errorf("buildTools(kubectlAvailable=false) included %q", tl.Name)
		}
	}
}

func TestBuildTools_ReadOnlyKubectlToolsOfferedInObserveMode(t *testing.T) {
	tools := buildTools(nil, "org/repo", true, false)
	names := make(map[string]bool, len(tools))
	for _, tl := range tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"kubectl_get", "kubectl_get_secret_keys", "kubectl_logs"} {
		if !names[want] {
			t.Errorf("buildTools(kubectlAvailable=true, acting=false) missing read-only tool %q", want)
		}
	}
	for _, mutating := range []string{"kubectl_rollout_restart", "kubectl_scale"} {
		if names[mutating] {
			t.Errorf("buildTools(acting=false) included mutating tool %q — should only be offered in act mode", mutating)
		}
	}
}

func TestBuildTools_MutatingKubectlToolsOnlyInActMode(t *testing.T) {
	tools := buildTools(nil, "org/repo", true, true)
	names := make(map[string]bool, len(tools))
	for _, tl := range tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"kubectl_get", "kubectl_rollout_restart", "kubectl_scale"} {
		if !names[want] {
			t.Errorf("buildTools(kubectlAvailable=true, acting=true) missing %q", want)
		}
	}
}

func TestFindMCPSource_RoutesByPrefix(t *testing.T) {
	causely := mcpSource{Name: "causely"}
	grafana := mcpSource{Name: "grafana"}
	sources := []mcpSource{causely, grafana}

	src, toolName, ok := findMCPSource(sources, "grafana__query_range")
	if !ok || src.Name != "grafana" || toolName != "query_range" {
		t.Errorf("findMCPSource(grafana__query_range) = (%+v, %q, %v), want (grafana, query_range, true)", src, toolName, ok)
	}

	src, toolName, ok = findMCPSource(sources, "causely__get_logs")
	if !ok || src.Name != "causely" || toolName != "get_logs" {
		t.Errorf("findMCPSource(causely__get_logs) = (%+v, %q, %v), want (causely, get_logs, true)", src, toolName, ok)
	}
}

func TestFindMCPSource_NonMCPToolNameDoesNotMatch(t *testing.T) {
	sources := []mcpSource{{Name: "causely"}, {Name: "grafana"}}
	for _, name := range []string{"read_file", "list_directory", "search_code", "propose_fix"} {
		if _, _, ok := findMCPSource(sources, name); ok {
			t.Errorf("findMCPSource(%q) matched an MCP source, want no match", name)
		}
	}
}

func TestDescribeMCPSources_ListsEachSourceWithItsToolCount(t *testing.T) {
	sources := []mcpSource{
		{Name: "causely", Description: "issue data", Tools: []mcpToolDef{{Name: "a"}, {Name: "b"}}},
		{Name: "grafana", Description: "", Tools: []mcpToolDef{{Name: "c"}}},
	}
	got := describeMCPSources(sources)
	if !strings.Contains(got, "causely__* (2 tools): issue data") {
		t.Errorf("describeMCPSources() = %q, missing the causely line", got)
	}
	if !strings.Contains(got, "grafana__* (1 tools): no description provided") {
		t.Errorf("describeMCPSources() = %q, missing the grafana fallback-description line", got)
	}
}

func TestDescribeMCPSources_EmptyIsExplicit(t *testing.T) {
	got := describeMCPSources(nil)
	if got != "(none configured)" {
		t.Errorf("describeMCPSources(nil) = %q, want an explicit none-configured message", got)
	}
}

func TestBuildSystemPrompt_MentionsConfiguredSources(t *testing.T) {
	payload := TriggerPayload{DiagnosisName: "CrashLoop", EntityName: "svc", Severity: "high"}
	sources := []mcpSource{{Name: "grafana", Description: "dashboards", Tools: []mcpToolDef{{Name: "q"}}}}
	prompt := buildSystemPrompt(payload, "org/repo", sources, false)
	if !strings.Contains(prompt, "grafana__* (1 tools): dashboards") {
		t.Errorf("buildSystemPrompt() does not mention the configured grafana source:\n%s", prompt)
	}
}

func TestFixHasRealChange_TrueWhenAnyChangeDiffers(t *testing.T) {
	fix := ProposedFix{Changes: []FileChange{
		{Path: "a.go", Search: "x", Replace: "x"},
		{Path: "b.go", Search: "y", Replace: "z"},
	}}
	if !fixHasRealChange(fix) {
		t.Error("fixHasRealChange() = false, want true when at least one change differs")
	}
}

func TestFixHasRealChange_FalseWhenAllChangesAreNoOps(t *testing.T) {
	fix := ProposedFix{Changes: []FileChange{
		{Path: "a.go", Search: "x", Replace: "x"},
		{Path: "b.go", Search: "y", Replace: "y"},
	}}
	if fixHasRealChange(fix) {
		t.Error("fixHasRealChange() = true, want false when every change is a no-op")
	}
}

func TestFixHasRealChange_FalseWhenNoChanges(t *testing.T) {
	if fixHasRealChange(ProposedFix{}) {
		t.Error("fixHasRealChange() = true, want false for an empty change set")
	}
}

// TestBuildSystemPrompt_NeverLeaksCauselyRemediationHint guards against the
// exact anchoring bug found dogfooding on staging: the agent's own conclusion
// tracked Causely's canned remediation almost word-for-word instead of
// deriving one from its own tool access. payload.Remediation must never reach
// the prompt Claude sees.
func TestBuildSystemPrompt_NeverLeaksCauselyRemediationHint(t *testing.T) {
	payload := TriggerPayload{
		DiagnosisName: "Congested",
		EntityName:    "causely/gateway",
		Severity:      "Medium",
		Description:   "some description",
		Remediation:   "SENTINEL_DO_NOT_LEAK_redirect metrics to alloy.monitoring:4318",
	}
	prompt := buildSystemPrompt(payload, "org/repo", nil, false)
	if strings.Contains(prompt, "SENTINEL_DO_NOT_LEAK") {
		t.Error("buildSystemPrompt() leaked payload.Remediation into the prompt Claude sees")
	}
	if strings.Contains(prompt, "Remediation hint") {
		t.Error("buildSystemPrompt() still contains a 'Remediation hint' line")
	}
}

// TestBuildSystemPrompt_InstructsIndependentVerification guards the mitigation
// for the same finding: since Causely's own root-cause Description text often
// embeds a suggested fix inline (e.g. "Remediation should focus on..."), the
// prompt must explicitly tell Claude not to adopt any such embedded suggestion.
func TestBuildSystemPrompt_InstructsIndependentVerification(t *testing.T) {
	prompt := buildSystemPrompt(TriggerPayload{}, "org/repo", nil, false)
	for _, want := range []string{"Do not adopt any remediation suggested by Causely", "independently verify"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("buildSystemPrompt() missing anti-anchoring instruction %q", want)
		}
	}
}

// TestBuildSystemPrompt_InstructsVerificationBeforeConcludingDefect guards the
// methodology added after manually judging a cleared issue: the flagged
// exception turned out to be already caught and gracefully handled by design
// (a try/except explicitly commented "best effort, don't fail the whole
// request"), and the "fix" Causely suggested was already the live config.
// Both required checking source code and live state before trusting the
// diagnosis — the prompt must tell Claude to do the same.
func TestBuildSystemPrompt_InstructsVerificationBeforeConcludingDefect(t *testing.T) {
	prompt := buildSystemPrompt(TriggerPayload{}, "org/repo", nil, false)
	for _, want := range []string{
		"already caught and handled gracefully",
		"check the CURRENT live state",
		"no_action_needed",
		"nothing is actually wrong",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("buildSystemPrompt() missing verification-methodology text %q", want)
		}
	}
}

// TestBuildSystemPrompt_InstructsAgainstUnverifiedSecretClaims guards a
// hallucination found dogfooding: the agent asserted "the Secret has been
// updated" as fact — something it structurally cannot see, since
// kubectl_get_secret_keys only ever returns key names, never values.
func TestBuildSystemPrompt_InstructsAgainstUnverifiedSecretClaims(t *testing.T) {
	prompt := buildSystemPrompt(TriggerPayload{}, "org/repo", nil, false)
	for _, want := range []string{
		"key NAMES ONLY, never its decoded values",
		`Never assert a claim about what a Secret currently`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("buildSystemPrompt() missing Secret-claim guardrail text %q", want)
		}
	}
}

// TestBuildSystemPrompt_InstructsAgainstConflatingClassifierWithCaller guards
// a second hallucination found dogfooding: the agent cited an observability
// span-classifier (code that merely recognizes the string "brpop") as if it
// were the actual application code issuing that Redis command without a
// timeout — a real call site that didn't exist anywhere in the repo.
func TestBuildSystemPrompt_InstructsAgainstConflatingClassifierWithCaller(t *testing.T) {
	prompt := buildSystemPrompt(TriggerPayload{}, "org/repo", nil, false)
	for _, want := range []string{
		"confirm you found the actual call site",
		"is NOT the same as the",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("buildSystemPrompt() missing classifier-vs-caller guardrail text %q", want)
		}
	}
}

func TestMCPToolDefUnmarshalsInputSchema(t *testing.T) {
	var def mcpToolDef
	raw := `{"name":"x","description":"d","inputSchema":{"type":"object"}}`
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		t.Fatalf("unmarshal mcpToolDef: %v", err)
	}
	if def.Name != "x" {
		t.Errorf("Name = %q, want x", def.Name)
	}
}
