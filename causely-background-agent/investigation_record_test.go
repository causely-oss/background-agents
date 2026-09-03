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

func TestRecorder_NilPathIsNoOp(t *testing.T) {
	rec := newRecorder("", zap.NewNop())
	rec.record(InvestigationRecord{RootCauseID: "rc-1"}) // must not panic or create anything
}

func TestRecorder_NilRecorderIsNoOp(t *testing.T) {
	var rec *recorder
	rec.record(InvestigationRecord{RootCauseID: "rc-1"}) // must not panic
}
