package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestExtractSSEData(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "single SSE event",
			raw:  "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n",
			want: `{"jsonrpc":"2.0","id":1,"result":{}}`,
		},
		{
			name: "SSE event with CRLF line endings",
			raw:  "event: message\r\ndata: {\"ok\":true}\r\n\r\n",
			want: `{"ok":true}`,
		},
		{
			name: "multiple data lines: last one wins",
			raw:  "data: {\"partial\":1}\ndata: {\"final\":2}\n\n",
			want: `{"final":2}`,
		},
		{
			name: "plain JSON body, not SSE-framed: returned unchanged",
			raw:  `{"jsonrpc":"2.0","id":1,"result":{}}`,
			want: `{"jsonrpc":"2.0","id":1,"result":{}}`,
		},
		{
			name: "empty body: returned unchanged",
			raw:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSSEData([]byte(tt.raw))
			if string(got) != tt.want {
				t.Errorf("extractSSEData(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestMCPClientPost_SSEResponse verifies the full request/response round trip
// against a fake MCP server that responds in the Streamable-HTTP SSE format the
// real Causely MCP server uses.
func TestMCPClientPost_SSEResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"get_logs\",\"description\":\"d\",\"inputSchema\":{}}]}}\n\n"))
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "get_logs" {
		t.Errorf("ListTools() = %+v, want one tool named get_logs", tools)
	}
}

func TestMCPClientPost_PlainJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"tools": []any{}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("ListTools() = %+v, want empty", tools)
	}
}

// TestMCPClient_PerformsInitializeHandshakeBeforeFirstToolCall guards the MCP
// connection lifecycle: a compliant, session-based server expects
// initialize, then the notifications/initialized notification, before any
// tools/list or tools/call — skipping this happens to work against Causely's
// own permissive endpoint but fails against a real stateful server.
func TestMCPClient_PerformsInitializeHandshakeBeforeFirstToolCall(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     *int   `json:"id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		methods = append(methods, req.Method)

		if req.Method == "notifications/initialized" {
			// A notification: no id, no response body per spec.
			if req.ID != nil {
				t.Errorf("notifications/initialized should not carry an id, got %v", *req.ID)
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"tools": []any{}},
		})
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	if _, err := client.ListTools(); err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}

	want := []string{"initialize", "notifications/initialized", "tools/list"}
	if len(methods) != len(want) {
		t.Fatalf("methods called = %v, want %v", methods, want)
	}
	for i, m := range want {
		if methods[i] != m {
			t.Errorf("call %d method = %q, want %q (full sequence: %v)", i, methods[i], m, methods)
		}
	}
}

// TestMCPClient_InitializeHandshakeOnlyHappensOnce guards against redoing the
// handshake on every tool call — ensureInitialized's sync.Once should make
// initialize+notifications/initialized run exactly once per client, however
// many tools/list or tools/call calls follow.
func TestMCPClient_InitializeHandshakeOnlyHappensOnce(t *testing.T) {
	initCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     *int   `json:"id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			initCount++
		}
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"tools": []any{}},
		})
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	if _, err := client.ListTools(); err != nil {
		t.Fatalf("first ListTools() error = %v", err)
	}
	if _, err := client.ListTools(); err != nil {
		t.Fatalf("second ListTools() error = %v", err)
	}
	if _, err := client.CallTool("some_tool", nil); err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}

	if initCount != 1 {
		t.Errorf("initialize was called %d times, want exactly 1", initCount)
	}
}

// TestMCPClient_RetainsAndSendsSessionID guards the other half of the
// lifecycle bug: a server assigning Mcp-Session-Id on the initialize response
// must have that same session id sent back on every subsequent request — a
// session-based server rejects requests missing it.
func TestMCPClient_RetainsAndSendsSessionID(t *testing.T) {
	const wantSessionID = "test-session-abc123"
	var sessionIDsSeen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionIDsSeen = append(sessionIDsSeen, r.Header.Get("Mcp-Session-Id"))

		var req struct {
			Method string `json:"method"`
			ID     *int   `json:"id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", wantSessionID)
		}
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"tools": []any{}},
		})
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	if _, err := client.ListTools(); err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}

	// [0]=initialize (no session id sent yet), [1]=notifications/initialized,
	// [2]=tools/list — both of the latter two must carry the assigned session id.
	if len(sessionIDsSeen) != 3 {
		t.Fatalf("requests seen = %d, want 3 (initialize, notifications/initialized, tools/list): %v", len(sessionIDsSeen), sessionIDsSeen)
	}
	if sessionIDsSeen[1] != wantSessionID {
		t.Errorf("notifications/initialized Mcp-Session-Id = %q, want %q", sessionIDsSeen[1], wantSessionID)
	}
	if sessionIDsSeen[2] != wantSessionID {
		t.Errorf("tools/list Mcp-Session-Id = %q, want %q", sessionIDsSeen[2], wantSessionID)
	}
}

// TestMCPClient_TransientInitFailureIsRetriedNotPermanent guards against a
// sync.Once-style permanent poison: a client whose first initialize attempt
// fails (server temporarily down) must succeed on a later call once the
// server recovers, not stay broken until process restart.
func TestMCPClient_TransientInitFailureIsRetriedNotPermanent(t *testing.T) {
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Method string `json:"method"`
			ID     *int   `json:"id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"tools": []any{}},
		})
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	if _, err := client.ListTools(); err == nil {
		t.Fatal("first ListTools() should fail while the server is returning 503")
	}

	fail = false
	if _, err := client.ListTools(); err != nil {
		t.Fatalf("ListTools() after the server recovered should succeed, got error: %v", err)
	}
}

// TestMCPClient_NonSuccessStatusIsAnError guards against a non-2xx response
// (auth gateway 401/403, proxy 5xx) with an empty or generically-shaped body
// being silently unmarshaled into a zero-value "success."
func TestMCPClient_NonSuccessStatusIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // empty body
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	if _, err := client.ListTools(); err == nil {
		t.Fatal("ListTools() against a 403 response should return an error, not silently succeed")
	}
}

// TestMCPClient_FailedInitializeDoesNotRetainSessionID guards against a
// session id captured from a malformed/partial response (real status
// non-2xx, but a session id header still present) poisoning the client — and
// against a session id from a successful initialize surviving a subsequent
// notifications/initialized failure, since the handshake as a whole didn't
// complete either way.
func TestMCPClient_FailedInitializeDoesNotRetainSessionID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			// Malformed: a session id header on a failing status.
			w.Header().Set("Mcp-Session-Id", "poisoned-session")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	if _, err := client.ListTools(); err == nil {
		t.Fatal("ListTools() should fail when initialize returns a 500")
	}
	if got := client.getSessionID(); got != "" {
		t.Errorf("sessionID = %q after a failed initialize, want empty", got)
	}
}

// TestMCPClient_CallToolIsRaceFree guards the seq-increment race: multiple
// goroutines calling tools concurrently (ensureInitialized explicitly
// supports this) must not race on request-id allocation. Run with -race.
func TestMCPClient_CallToolIsRaceFree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     *int   `json:"id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"content": []any{}},
		})
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.CallTool("some_tool", nil); err != nil {
				t.Errorf("CallTool() error = %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestMCPClientPost_SendsBearerTokenWhenConfigured(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "secret-token")
	if _, err := client.ListTools(); err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-token")
	}
}

func TestMCPClientPost_SendsBasicAuthWhenClientCredentialsConfigured(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
	defer server.Close()

	client := newMCPClientBasicAuth(server.URL, "my-client", "my-secret")
	if _, err := client.ListTools(); err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("my-client:my-secret"))
	if gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
}

func TestMCPClientPost_NoAuthHeaderWhenTokenEmpty(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	if _, err := client.ListTools(); err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty when no token is configured", gotAuth)
	}
}

func TestMCPClientPost_ErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"message\":\"boom\"}}\n\n"))
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	_, err := client.ListTools()
	if err == nil {
		t.Fatal("ListTools() expected an error, got nil")
	}
}

func TestMCPClientPost_InjectsTraceContext(t *testing.T) {
	var gotTraceparent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
	defer server.Close()

	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previousPropagator) })

	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1},
		SpanID:  trace.SpanID{2},
		Remote:  false,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	client := newMCPClient(server.URL, "")
	if _, err := client.ListToolsContext(ctx); err != nil {
		t.Fatalf("ListToolsContext() error = %v", err)
	}
	if gotTraceparent == "" {
		t.Fatal("traceparent header was not injected")
	}
}
