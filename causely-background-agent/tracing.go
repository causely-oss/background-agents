package main

import (
	"context"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	harnessruntime "github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/telemetry"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
)

// Tracing is kagent's. Its harness runtime (harness/runtime/a2a) opens a span
// per A2A turn and writes the GenAI execute_tool spans, token metrics and
// tool metrics from the events this adapter reports while translating the ACP
// stream: tool calls and results (client.go) and the turn's token usage
// (reportTurnUsage). Hermes' ACP stream has no per-model-call events, so the
// usage is reported once per turn, which kagent records in its token metrics
// and this adapter also puts on the turn span.
//
// For a BYO Harness the controller passes the exporter settings as standard
// OTEL_* variables, which telemetry.Init reads.

// initTelemetry installs kagent's OpenTelemetry providers. Metrics are
// deferred until the first request, as kagent's own harnesses do, because
// every Actor restored from one Substrate snapshot would otherwise share one
// metric identity.
func initTelemetry(ctx context.Context, serviceName string) (*telemetry.Providers, error) {
	return telemetry.Init(ctx, telemetry.Options{
		Defaults:     tracing.RuntimeTelemetry{}.ResourceDefaults(serviceName),
		DeferMetrics: true,
	})
}

// runtimeTelemetry is the telemetry contract for kagent's runtime: the
// compiled model, for attributing usage, and the standard GenAI content
// capture switch (OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT).
// kagent's runtime identity only names its own runtimes, so it is left empty.
func runtimeTelemetry(model hermesModel, environ []string) tracing.RuntimeTelemetry {
	return tracing.RuntimeTelemetry{Provider: model.Provider, Model: model.Default}.WithEnvironment(environ)
}

// reportTurnUsage records one turn's token usage: as a usage-only model call
// for kagent's token metrics, and as GenAI usage attributes on the turn span.
func reportTurnUsage(ctx context.Context, sink harnessruntime.ModelCallSink, model hermesModel, usage *turnUsage) {
	if usage == nil {
		return
	}
	input := usage.Input
	cacheRead := 0
	if usage.CacheRead != nil {
		cacheRead = *usage.CacheRead
		input += cacheRead
	}
	sink.ModelCall(harnessruntime.ModelCall{
		Provider: model.Provider, RequestModel: model.Default,
		InputTokens: int64(input), CacheReadTokens: int64(cacheRead), OutputTokens: int64(usage.Output),
		UsageOnly: true,
	})
	attributes := []attribute.KeyValue{
		attribute.Int("gen_ai.usage.input_tokens", input),
		attribute.Int("gen_ai.usage.output_tokens", usage.Output),
	}
	if usage.CacheRead != nil {
		attributes = append(attributes, attribute.Int("gen_ai.usage.cache_read.input_tokens", cacheRead))
	}
	tracing.InvocationFromContext(ctx).SetAttributes(attributes...)
}

// toolName recovers the Hermes tool name from an ACP tool call title, which
// Hermes builds as "<tool_name>: <preview>" (acp_adapter/tools.py
// build_tool_title), or the bare tool name.
func toolName(title string) string {
	name, _, _ := strings.Cut(title, ": ")
	name = strings.TrimSpace(name)
	if name == "" {
		return "tool"
	}
	return name
}

// toolPreview is the redacted argument summary Hermes puts in the title.
func toolPreview(title string) string {
	_, preview, _ := strings.Cut(title, ": ")
	return strings.TrimSpace(preview)
}

const mcpToolPrefix = "mcp__"

// mcpTool splits an MCP tool name (Hermes names them mcp__<server>__<tool>,
// tools/mcp_tool_schema.py) into server and tool, or returns "" for both.
func mcpTool(name string) (server, tool string) {
	rest, ok := strings.CutPrefix(name, mcpToolPrefix)
	if !ok {
		return "", ""
	}
	server, tool, ok = strings.Cut(rest, "__")
	if !ok || server == "" || tool == "" {
		return "", ""
	}
	return server, tool
}

// turnUsage is the token usage of one turn as Hermes counts it: Input is
// uncached input tokens, CacheRead the cache hits (Hermes' ACP usage has no
// cache-write count).
type turnUsage struct {
	Input, Output int
	CacheRead     *int
}

// usageMeter turns the session-cumulative usage Hermes returns with each ACP
// prompt response (its agent.session_* counters) into per-turn usage.
// Hermes starts those counters at zero for every process, including after
// session/load, so the baseline resets whenever the runner reconnects. A
// turn that fails without a prompt response (for example the time limit)
// reports nothing, and its tokens are counted in the next turn.
type usageMeter struct {
	last *acp.Usage
}

func (m *usageMeter) reset() { m.last = nil }

func (m *usageMeter) turn(total *acp.Usage) *turnUsage {
	if total == nil {
		return nil
	}
	previous := m.last
	current := *total
	m.last = &current
	if previous == nil || current.InputTokens < previous.InputTokens || current.OutputTokens < previous.OutputTokens {
		previous = &acp.Usage{}
	}
	u := &turnUsage{
		Input:  current.InputTokens - previous.InputTokens,
		Output: current.OutputTokens - previous.OutputTokens,
	}
	if current.CachedReadTokens != nil {
		read := *current.CachedReadTokens
		if previous.CachedReadTokens != nil && *previous.CachedReadTokens <= read {
			read -= *previous.CachedReadTokens
		}
		u.CacheRead = &read
	}
	return u
}
