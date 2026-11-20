package main

import (
	"context"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	harnessruntime "github.com/kagent-dev/kagent/go/harness/runtime"
)

func toolCall(id, title string, rawInput any) acp.SessionNotification {
	return acp.SessionNotification{Update: acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
		ToolCallId: acp.ToolCallId(id), Title: title, Kind: acp.ToolKindExecute, RawInput: rawInput,
		SessionUpdate: "tool_call",
	}}}
}

func toolDone(id string, status acp.ToolCallStatus, rawOutput any) acp.SessionNotification {
	return acp.SessionNotification{Update: acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: acp.ToolCallId(id), Status: &status, RawOutput: rawOutput, SessionUpdate: "tool_call_update",
	}}}
}

// The ACP client reports bare tool names (kagent names execute_tool spans
// after them), the MCP server and tool, and the title preview as an argument.
func TestToolEventsForKagentRuntime(t *testing.T) {
	sink := &recordingSink{}
	client := newHermesACPClient()
	client.setSink(sink)
	ctx := context.Background()

	if err := client.SessionUpdate(ctx, toolCall("c1", "mcp__causely__get_issue_details", map[string]any{"issue_id": "abc"})); err != nil {
		t.Fatal(err)
	}
	if c := sink.call; c.Name != "mcp__causely__get_issue_details" || c.MCPServer != "causely" || c.MCPTool != "get_issue_details" || c.Arguments["issue_id"] != "abc" {
		t.Fatalf("mcp call = %+v", c)
	}

	if err := client.SessionUpdate(ctx, toolCall("c2", "terminal: kubectl get secret x -o yaml", nil)); err != nil {
		t.Fatal(err)
	}
	if c := sink.call; c.Name != "terminal" || c.MCPServer != "" || c.Arguments["preview"] != "kubectl get secret x -o yaml" {
		t.Fatalf("terminal call = %+v", c)
	}
	if err := client.SessionUpdate(ctx, toolDone("c2", acp.ToolCallStatusFailed, "denied")); err != nil {
		t.Fatal(err)
	}
	if r := sink.result; r.ID != "c2" || r.Name != "terminal" || !r.IsError {
		t.Fatalf("result = %+v", r)
	}
}

type modelCalls []harnessruntime.ModelCall

func (m *modelCalls) ModelCall(call harnessruntime.ModelCall) { *m = append(*m, call) }

func TestReportTurnUsage(t *testing.T) {
	var calls modelCalls
	model := hermesModel{Default: "claude-opus-5-5", Provider: "anthropic"}
	read := 900
	reportTurnUsage(context.Background(), &calls, model, &turnUsage{Input: 100, Output: 20, CacheRead: &read})
	reportTurnUsage(context.Background(), &calls, model, nil)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	c := calls[0]
	// kagent's ModelCall counts cached tokens in InputTokens.
	if !c.UsageOnly || c.InputTokens != 1000 || c.CacheReadTokens != 900 || c.OutputTokens != 20 ||
		c.Provider != "anthropic" || c.RequestModel != "claude-opus-5-5" {
		t.Fatalf("call = %+v", c)
	}
}

func TestRuntimeTelemetry(t *testing.T) {
	model := hermesModel{Default: "gpt-5", Provider: "openai"}
	got := runtimeTelemetry(model, []string{"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true"})
	if !got.CaptureContent || got.Model != "gpt-5" || got.Provider != "openai" || got.Runtime != "" {
		t.Fatalf("telemetry = %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if runtimeTelemetry(model, nil).CaptureContent {
		t.Fatal("capture must be off by default")
	}
}

func TestToolName(t *testing.T) {
	for title, want := range map[string]string{
		"terminal: kubectl get pods":        "terminal",
		"mcp__causely__get_issues":          "mcp__causely__get_issues",
		"read_file: /a: b":                  "read_file",
		"":                                  "tool",
		"mcp__causely__get_issues: {\"a\"}": "mcp__causely__get_issues",
	} {
		if got := toolName(title); got != want {
			t.Errorf("toolName(%q) = %q, want %q", title, got, want)
		}
	}
	if s, tool := mcpTool("mcp__causely__get_issues"); s != "causely" || tool != "get_issues" {
		t.Errorf("mcpTool = %q %q", s, tool)
	}
	for _, name := range []string{"terminal", "mcp__x", "mcp____y"} {
		if s, tool := mcpTool(name); s != "" || tool != "" {
			t.Errorf("mcpTool(%q) = %q %q", name, s, tool)
		}
	}
}

func intp(v int) *int { return &v }

func TestUsageMeterTurnsSessionTotalsIntoTurnUsage(t *testing.T) {
	var m usageMeter
	if m.turn(nil) != nil {
		t.Fatal("no report should give no usage")
	}
	u := m.turn(&acp.Usage{InputTokens: 100, OutputTokens: 10, CachedReadTokens: intp(50)})
	if u.Input != 100 || u.Output != 10 || *u.CacheRead != 50 {
		t.Fatalf("first turn = %+v", u)
	}
	u = m.turn(&acp.Usage{InputTokens: 250, OutputTokens: 30, CachedReadTokens: intp(170)})
	if u.Input != 150 || u.Output != 20 || *u.CacheRead != 120 {
		t.Fatalf("second turn = %+v", u)
	}
	// A restarted Hermes starts its counters again.
	m.reset()
	u = m.turn(&acp.Usage{InputTokens: 40, OutputTokens: 4})
	if u.Input != 40 || u.Output != 4 || u.CacheRead != nil {
		t.Fatalf("after reset = %+v", u)
	}
	// Counters that went backwards without a reset are taken as a new baseline.
	u = m.turn(&acp.Usage{InputTokens: 5, OutputTokens: 1})
	if u.Input != 5 || u.Output != 1 {
		t.Fatalf("after drop = %+v", u)
	}
}
