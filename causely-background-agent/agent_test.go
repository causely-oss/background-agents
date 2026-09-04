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

	tools := buildTools(sources, "org/repo")

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
	tools := buildTools(nil, "org/repo")
	if len(tools) != 5 {
		t.Fatalf("buildTools(nil) = %d tools, want exactly the 5 built-ins", len(tools))
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
	prompt := buildSystemPrompt(payload, "org/repo", sources)
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
