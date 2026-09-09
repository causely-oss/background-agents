package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func mcpToolResultServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	encodedText, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("marshal text: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":` + string(encodedText) + `}]}}`))
	}))
}

func TestCheckIssueStillActive_ResolvedIssueIsSkipped(t *testing.T) {
	server := mcpToolResultServer(t, `{"issues":[{"id":"rc-1","ended_at":"2026-08-30T12:03:13Z"}]}`)
	defer server.Close()

	client := newMCPClient(server.URL, "")
	skip, reason := checkIssueStillActive(client, "rc-1", zap.NewNop())
	if !skip {
		t.Fatal("checkIssueStillActive() skip = false, want true for a resolved issue")
	}
	if reason == "" {
		t.Error("expected a non-empty skip reason")
	}
}

func TestCheckIssueStillActive_ActiveIssueProceeds(t *testing.T) {
	server := mcpToolResultServer(t, `{"issues":[{"id":"rc-1","ended_at":""}]}`)
	defer server.Close()

	client := newMCPClient(server.URL, "")
	skip, _ := checkIssueStillActive(client, "rc-1", zap.NewNop())
	if skip {
		t.Error("checkIssueStillActive() skip = true, want false for a still-active issue")
	}
}

func TestCheckIssueStillActive_FailsOpenOnUnparseableResponse(t *testing.T) {
	server := mcpToolResultServer(t, `not json at all`)
	defer server.Close()

	client := newMCPClient(server.URL, "")
	skip, _ := checkIssueStillActive(client, "rc-1", zap.NewNop())
	if skip {
		t.Error("checkIssueStillActive() skip = true, want false (fail open) when the response can't be parsed")
	}
}

func TestCheckIssueStillActive_FailsOpenOnToolError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"message":"boom"}}`))
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	skip, _ := checkIssueStillActive(client, "rc-1", zap.NewNop())
	if skip {
		t.Error("checkIssueStillActive() skip = true, want false (fail open) when the tool call errors")
	}
}
