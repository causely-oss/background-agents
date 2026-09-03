package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
