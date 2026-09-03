package main

import (
	"path/filepath"
	"testing"
)

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
	if got.RootCauseID != "rc-1" || got.EntityName != "buggy-app" || got.Severity != "Critical" ||
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
	if len(issues) != 1 || issues[0].RootCauseID != "rc-1" {
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
