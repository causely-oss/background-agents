package agent

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
	pollOnce(zap.NewNop(), cfg, client, watermark, newWeeklyBudget(0, ""), nil, nil, loadTriggerDedup(""))

	if gotParams["active_only"] != true {
		t.Errorf("active_only = %v, want true", gotParams["active_only"])
	}
	ns, ok := gotParams["namespace_names"].([]any)
	if !ok || len(ns) != 1 || ns[0] != "causely" {
		t.Errorf("namespace_names = %v, want [\"causely\"]", gotParams["namespace_names"])
	}
}

func TestPollOnce_OmitsNamespaceFilterButDefaultsSeverityWhenUnset(t *testing.T) {
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
	pollOnce(zap.NewNop(), Config{}, client, watermark, newWeeklyBudget(0, ""), nil, nil, loadTriggerDedup(""))

	if _, ok := gotParams["namespace_names"]; ok {
		t.Errorf("namespace_names = %v, want it omitted when ScopeNamespaces is unset", gotParams["namespace_names"])
	}
	// severities is NOT omitted: poll defaults to High/Critical when
	// AllowedSeverities is unset — see TestPollSeverities_DefaultsToHighCritical.
	sev, ok := gotParams["severities"].([]any)
	if !ok || len(sev) != 2 || sev[0] != "High" || sev[1] != "Critical" {
		t.Errorf("severities = %v, want the default [\"High\", \"Critical\"] when AllowedSeverities is unset", gotParams["severities"])
	}
}

// TestPollSeverities_DefaultsToHighCritical guards poll's safe-by-default
// severity filter: unlike the webhook/Slack paths (inScope in scope.go),
// which only filter by severity when explicitly configured, poll runs
// continuously and pays for every genuinely new occurrence it dispatches on
// — so an operator who never touches allowed_severities still gets a
// sensible default instead of investigating every Low-severity flicker.
func TestPollSeverities_DefaultsToHighCritical(t *testing.T) {
	got := pollSeverities(Config{})
	if len(got) != 2 || got[0] != "High" || got[1] != "Critical" {
		t.Errorf("pollSeverities(unset) = %v, want [\"High\", \"Critical\"]", got)
	}
}

// TestPollSeverities_OperatorOverrideWins guards the escape hatch: an
// operator who explicitly sets allowed_severities (even to something that
// includes Low/Medium) must get exactly that, not the default.
func TestPollSeverities_OperatorOverrideWins(t *testing.T) {
	cfg := Config{AllowedSeverities: []string{"Low", "Medium", "High", "Critical"}}
	got := pollSeverities(cfg)
	if len(got) != 4 {
		t.Errorf("pollSeverities(explicit) = %v, want the operator's own list unmodified", got)
	}
}

// TestPollOnce_SendsSeverityFilter guards the fix for duplicate paid
// investigations of the same chronic, flapping-severity issue: filtering
// get_issues server-side by severity means a low-severity flicker is never
// even fetched, not just rejected after the fact by inScope.
func TestPollOnce_SendsSeverityFilter(t *testing.T) {
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

	cfg := Config{AllowedSeverities: []string{"High", "Critical"}}
	client := newMCPClient(server.URL, "")
	watermark := loadPollWatermark("")
	pollOnce(zap.NewNop(), cfg, client, watermark, newWeeklyBudget(0, ""), nil, nil, loadTriggerDedup(""))

	sev, ok := gotParams["severities"].([]any)
	if !ok || len(sev) != 2 || sev[0] != "High" || sev[1] != "Critical" {
		t.Errorf("severities = %v, want [\"High\", \"Critical\"]", gotParams["severities"])
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

// TestPollOnce_InFlightOccurrenceIsNotLost guards the fix for poll.go
// committing the watermark before checking dedup: if an investigation for
// this issue is already in flight (e.g. a webhook trigger landed for the
// same issue moments earlier), this poll cycle must NOT advance the
// watermark for it — otherwise the next poll cycle would see the watermark
// as already up to date and silently never retry this occurrence.
func TestPollOnce_InFlightOccurrenceIsNotLost(t *testing.T) {
	issueJSON := `{"issues":[{"id":"issue-1","name":"Congested","severity":"High","updated_at":"2026-01-01T00:00:00Z"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":` + jsonQuote(issueJSON) + `}]}}`))
	}))
	defer server.Close()

	cfg := Config{}
	client := newMCPClient(server.URL, "")
	watermark := loadPollWatermark("")
	dedup := loadTriggerDedup("")

	// Simulate a concurrent trigger for the same issue already in flight.
	dedup.tryAcquire(triggerSourcePoll, "issue-1", "2026-01-01T00:00:00Z")

	pollOnce(zap.NewNop(), cfg, client, watermark, newWeeklyBudget(0, ""), nil, nil, dedup)

	if !watermark.hasChanged("issue-1", "2026-01-01T00:00:00Z") {
		t.Error("watermark should NOT have been committed for an in-flight occurrence — it would be silently lost on the next poll cycle otherwise")
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestPollWatermark_FirstSightingAndUnchangedValueDontRetrigger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watermark.json")
	w := loadPollWatermark(path)

	if !w.hasChanged("rc-1", "v1") {
		t.Error("first sighting of rc-1 should report changed")
	}
	w.commit("rc-1", "v1")
	if w.hasChanged("rc-1", "v1") {
		t.Error("same version again should not report changed")
	}
	if !w.hasChanged("rc-1", "v2") {
		t.Error("a new version for the same id should report changed")
	}
}

// TestPollWatermark_HasChangedDoesNotMutate guards the fix for poll.go
// committing the watermark before checking whether dedup would even allow
// dispatch: hasChanged must be a pure check, so a caller can decide NOT to
// commit (e.g. because the occurrence turned out to be in-flight) without
// having already lost track of the fact it was never actually acted on.
func TestPollWatermark_HasChangedDoesNotMutate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watermark.json")
	w := loadPollWatermark(path)

	if !w.hasChanged("rc-1", "v1") {
		t.Fatal("first hasChanged() should report changed")
	}
	// Deliberately not calling commit — simulates a rejected dispatch attempt.
	if !w.hasChanged("rc-1", "v1") {
		t.Error("hasChanged() without a commit in between should still report changed — it must not have side effects")
	}
}

func TestPollWatermark_PersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watermark.json")
	w := loadPollWatermark(path)
	w.commit("rc-1", "v1")
	if err := w.persist(); err != nil {
		t.Fatalf("persist() error = %v", err)
	}

	reloaded := loadPollWatermark(path)
	if reloaded.hasChanged("rc-1", "v1") {
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
