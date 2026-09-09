package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// TestPollOnce_SendsActiveOnlyAndNamespaceFilter guards two bugs found live:
// get_issues's real parameter is "active_only", not "only_active" (a typo
// that silently no-op'd since active_only already defaults to true); and
// get_issues's lightweight response doesn't reliably carry per-issue
// namespace/label data the way inScope() needs, so scope_namespaces can only
// be enforced by asking the API to filter server-side via namespace_names.
func TestPollOnce_SendsActiveOnlyAndNamespaceFilter(t *testing.T) {
	var gotParams map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotParams = req.Params.Arguments
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"issues\":[]}"}]}}`))
	}))
	defer server.Close()

	cfg := Config{ScopeNamespaces: []string{"causely"}}
	client := newMCPClient(server.URL, "")
	watermark := loadPollWatermark("")
	pollOnce(zap.NewNop(), cfg, client, watermark, newWeeklyBudget(0, ""), nil, nil)

	if gotParams["active_only"] != true {
		t.Errorf("active_only = %v, want true", gotParams["active_only"])
	}
	ns, ok := gotParams["namespace_names"].([]any)
	if !ok || len(ns) != 1 || ns[0] != "causely" {
		t.Errorf("namespace_names = %v, want [\"causely\"]", gotParams["namespace_names"])
	}
}

func TestPollOnce_OmitsNamespaceFilterWhenScopeNamespacesUnset(t *testing.T) {
	var gotParams map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotParams = req.Params.Arguments
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"issues\":[]}"}]}}`))
	}))
	defer server.Close()

	client := newMCPClient(server.URL, "")
	watermark := loadPollWatermark("")
	pollOnce(zap.NewNop(), Config{}, client, watermark, newWeeklyBudget(0, ""), nil, nil)

	if _, ok := gotParams["namespace_names"]; ok {
		t.Errorf("namespace_names = %v, want it omitted when ScopeNamespaces is unset", gotParams["namespace_names"])
	}
}

func TestParsePolledIssues_PlainArray(t *testing.T) {
	raw := `[
		{"id": "rc-1", "name": "ImagePullErrors", "severity": "Critical",
		 "entity": {"id": "e-1", "name": "buggy-app"},
		 "description": {"summary": "bad tag", "remediation": "fix the tag"},
		 "updated_at": "2026-09-03T00:00:00Z"}
	]`
	issues, err := parsePolledIssues(raw)
	if err != nil {
		t.Fatalf("parsePolledIssues() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	got := issues[0]
	if got.IssueID != "rc-1" || got.EntityName != "buggy-app" || got.Severity != "Critical" ||
		got.Description != "bad tag" || got.Remediation != "fix the tag" || got.UpdatedAt != "2026-09-03T00:00:00Z" {
		t.Errorf("parsed issue = %+v", got)
	}
}

func TestParsePolledIssues_WrappedObject(t *testing.T) {
	raw := `{"issues": [{"id": "rc-1", "severity": "High"}]}`
	issues, err := parsePolledIssues(raw)
	if err != nil {
		t.Fatalf("parsePolledIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].IssueID != "rc-1" {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestParsePolledIssues_MissingUpdatedAtFallsBackToSeverityAndSymptomCount(t *testing.T) {
	raw := `[{"id": "rc-1", "severity": "High", "symptom_count": 3}]`
	issues, err := parsePolledIssues(raw)
	if err != nil {
		t.Fatalf("parsePolledIssues() error = %v", err)
	}
	if v := issues[0].version(); v != "High|3" {
		t.Errorf("version() = %q, want %q", v, "High|3")
	}
}

func TestPollWatermark_FirstSightingAndUnchangedValueDontRetrigger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watermark.json")
	w := loadPollWatermark(path)

	if !w.changed("rc-1", "v1") {
		t.Error("first sighting of rc-1 should report changed")
	}
	if w.changed("rc-1", "v1") {
		t.Error("same version again should not report changed")
	}
	if !w.changed("rc-1", "v2") {
		t.Error("a new version for the same id should report changed")
	}
}

func TestPollWatermark_PersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watermark.json")
	w := loadPollWatermark(path)
	w.changed("rc-1", "v1")
	if err := w.persist(); err != nil {
		t.Fatalf("persist() error = %v", err)
	}

	reloaded := loadPollWatermark(path)
	if reloaded.changed("rc-1", "v1") {
		t.Error("reloaded watermark should still know about rc-1@v1 and report unchanged")
	}
}

func TestOneLine_CollapsesNewlinesAndWhitespace(t *testing.T) {
	in := "line one\n\n1. step one\n2. step two\n   indented continuation"
	want := "line one 1. step one 2. step two indented continuation"
	if got := oneLine(in); got != want {
		t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
	}
}
