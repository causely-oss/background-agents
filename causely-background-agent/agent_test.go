package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildTools_PrefixesEachMCPSourceByItsName(t *testing.T) {
	sources := []mcpSource{
		{
			Name: "causely",
			Tools: []mcpToolDef{
				{Name: "get_logs", Description: "logs"},
				{Name: "get_root_cause_details", Description: "rcd"},
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

	for _, want := range []string{"causely__get_logs", "causely__get_root_cause_details", "grafana__query_range"} {
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
		{Name: "causely", Description: "root cause data", Tools: []mcpToolDef{{Name: "a"}, {Name: "b"}}},
		{Name: "grafana", Description: "", Tools: []mcpToolDef{{Name: "c"}}},
	}
	got := describeMCPSources(sources)
	if !strings.Contains(got, "causely__* (2 tools): root cause data") {
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
	payload := TriggerPayload{RootCauseName: "CrashLoop", EntityName: "svc", Severity: "high"}
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
		RootCauseName: "Congested",
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
