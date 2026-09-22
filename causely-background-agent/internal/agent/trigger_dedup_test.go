package agent

import (
	"path/filepath"
	"testing"
)

func TestTriggerDedup_FirstAcquireSucceeds(t *testing.T) {
	d := loadTriggerDedup("")
	if !d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-a") {
		t.Error("tryAcquire() on a fresh dedup should succeed")
	}
}

func TestTriggerDedup_SameContentHashIsRejectedAsRetry(t *testing.T) {
	d := loadTriggerDedup("")
	if !d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-a") {
		t.Fatal("first tryAcquire() should succeed")
	}
	d.release("issue-1")
	if d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-a") {
		t.Error("tryAcquire() with the same content hash after release should be rejected as a retry")
	}
}

func TestTriggerDedup_DifferentContentHashIsANewOccurrence(t *testing.T) {
	d := loadTriggerDedup("")
	if !d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-a") {
		t.Fatal("first tryAcquire() should succeed")
	}
	d.release("issue-1")
	if !d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-b") {
		t.Error("tryAcquire() with a different content hash should succeed — it's a genuinely new occurrence, not a retry")
	}
}

func TestTriggerDedup_InFlightIsRejectedEvenWithDifferentContentHash(t *testing.T) {
	d := loadTriggerDedup("")
	if !d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-a") {
		t.Fatal("first tryAcquire() should succeed")
	}
	// Not released yet — still in flight.
	if d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-b") {
		t.Error("tryAcquire() should reject a second trigger for the same issue while the first is still in flight, regardless of content hash")
	}
}

func TestTriggerDedup_ReleaseAllowsReacquire(t *testing.T) {
	d := loadTriggerDedup("")
	if !d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-a") {
		t.Fatal("first tryAcquire() should succeed")
	}
	d.release("issue-1")
	if !d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-b") {
		t.Error("tryAcquire() should succeed again after release, for a new occurrence")
	}
}

func TestTriggerDedup_EmptyIssueIDAlwaysAcquires(t *testing.T) {
	// Empty IssueID means "nothing to key dedup on" — reject it upstream
	// (see server.go's objectId-required check), don't let dedup silently
	// swallow every empty-ID trigger under one shared bucket.
	d := loadTriggerDedup("")
	if !d.tryAcquire(triggerSourceWebhook, "", "hash-a") {
		t.Error("tryAcquire() with an empty issue ID should always succeed")
	}
	if !d.tryAcquire(triggerSourceWebhook, "", "hash-b") {
		t.Error("tryAcquire() with an empty issue ID should always succeed, repeatedly")
	}
}

func TestTriggerDedup_PersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dedup.json")
	d := loadTriggerDedup(path)
	if !d.tryAcquire(triggerSourceWebhook, "issue-1", "hash-a") {
		t.Fatal("tryAcquire() should succeed")
	}
	d.release("issue-1")

	reloaded := loadTriggerDedup(path)
	if reloaded.tryAcquire(triggerSourceWebhook, "issue-1", "hash-a") {
		t.Error("a reloaded dedup should still recognize the same content hash as a retry — the watermark should have persisted across the reload")
	}
	if !reloaded.tryAcquire(triggerSourceWebhook, "issue-1", "hash-b") {
		t.Error("a reloaded dedup should still accept a genuinely new content hash")
	}
}

// TestTriggerDedup_DifferentSourcesDoNotClobberEachOthersWatermark guards the
// fix for keying "seen" by issueID alone: a poll write (issue.version()
// identity) must not erase a webhook's ability to recognize its OWN retry
// (content-hash identity) for the same issue, and vice versa.
func TestTriggerDedup_DifferentSourcesDoNotClobberEachOthersWatermark(t *testing.T) {
	d := loadTriggerDedup("")

	if !d.tryAcquire(triggerSourceWebhook, "issue-1", "webhook-hash-a") {
		t.Fatal("webhook tryAcquire() should succeed")
	}
	d.release("issue-1")

	// A poll cycle for the same issue, with its own (different-shaped)
	// identity — a genuinely different source, should succeed.
	if !d.tryAcquire(triggerSourcePoll, "issue-1", "2026-01-01T00:00:00Z") {
		t.Fatal("poll tryAcquire() for the same issue should succeed — it's a different source with its own identity")
	}
	d.release("issue-1")

	// The ORIGINAL webhook notification retries with the exact same content
	// hash as before. This must still be recognized as a retry and rejected
	// — even though poll's write happened in between.
	if d.tryAcquire(triggerSourceWebhook, "issue-1", "webhook-hash-a") {
		t.Error("webhook retry with the same content hash should be rejected, even after an intervening poll acquire for the same issue — poll's write must not have clobbered webhook's retry memory")
	}
}

// TestTriggerDedup_InFlightOnly_BlocksDoubleClickButNotDeliberateRetry guards
// #2: Slack's "Fix it" button must reject a double-click (in-flight), but
// must NOT permanently suppress a deliberate second click on an unchanged
// issue once the first investigation has finished.
func TestTriggerDedup_InFlightOnly_BlocksDoubleClickButNotDeliberateRetry(t *testing.T) {
	d := loadTriggerDedup("")

	if !d.tryAcquireInFlightOnly("issue-1") {
		t.Fatal("first tryAcquireInFlightOnly() should succeed")
	}
	// Double-click while still in flight — must be rejected.
	if d.tryAcquireInFlightOnly("issue-1") {
		t.Error("tryAcquireInFlightOnly() should reject a second click while the first is still in flight")
	}
	d.release("issue-1")

	// Deliberate retry after the first investigation finished, same issue,
	// unchanged content — must succeed, unlike the persisted-hash tryAcquire.
	if !d.tryAcquireInFlightOnly("issue-1") {
		t.Error("tryAcquireInFlightOnly() should succeed again after release, even with no content change — a human's deliberate retry must not be permanently suppressed")
	}
}

func TestTriggerContentHash_SameFieldsSameHash(t *testing.T) {
	a := TriggerPayload{IssueID: "i1", Severity: "High", DiagnosisName: "Congested", Description: "d", Remediation: "r"}
	b := a
	if triggerContentHash(a) != triggerContentHash(b) {
		t.Error("triggerContentHash() should be identical for identical payloads")
	}
}

func TestTriggerContentHash_DifferentSeverityDifferentHash(t *testing.T) {
	a := TriggerPayload{IssueID: "i1", Severity: "High", DiagnosisName: "Congested"}
	b := TriggerPayload{IssueID: "i1", Severity: "Critical", DiagnosisName: "Congested"}
	if triggerContentHash(a) == triggerContentHash(b) {
		t.Error("triggerContentHash() should differ when severity changes — that's a genuinely new occurrence, not a retry")
	}
}
