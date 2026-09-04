package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func TestRecorder_AppendsOneJSONLinePerRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	rec := newRecorder(path, zap.NewNop())

	rec.record(InvestigationRecord{RootCauseID: "rc-1", Verdict: verdictFixProposed})
	rec.record(InvestigationRecord{RootCauseID: "rc-2", Verdict: verdictSkippedScope})

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open records file: %v", err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}

	var first InvestigationRecord
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first line: %v", err)
	}
	if first.RootCauseID != "rc-1" || first.Verdict != verdictFixProposed {
		t.Errorf("first record = %+v", first)
	}
}

// TestRecorder_PersistsProposedFix guards against the ProposedFix field being
// dropped by JSON round-tripping — this is the only place an observe-mode
// fix_proposed verdict's actual diff survives; a transient log line is not enough.
func TestRecorder_PersistsProposedFix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	rec := newRecorder(path, zap.NewNop())

	fix := &ProposedFix{
		PRTitle: "fix: correct the thing",
		PRBody:  "body",
		Changes: []FileChange{{Path: "main.go", Search: "old", Replace: "new"}},
	}
	rec.record(InvestigationRecord{RootCauseID: "rc-1", Verdict: verdictFixProposed, ProposedFix: fix})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read records file: %v", err)
	}
	var got InvestigationRecord
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	if got.ProposedFix == nil {
		t.Fatal("ProposedFix = nil, want it persisted")
	}
	if got.ProposedFix.PRTitle != fix.PRTitle || len(got.ProposedFix.Changes) != 1 || got.ProposedFix.Changes[0].Search != "old" {
		t.Errorf("ProposedFix = %+v, want it to round-trip fully", got.ProposedFix)
	}
}

func TestRecorder_NilPathIsNoOp(t *testing.T) {
	rec := newRecorder("", zap.NewNop())
	rec.record(InvestigationRecord{RootCauseID: "rc-1"}) // must not panic or create anything
}

func TestRecorder_NilRecorderIsNoOp(t *testing.T) {
	var rec *recorder
	rec.record(InvestigationRecord{RootCauseID: "rc-1"}) // must not panic
}
