package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
)

// triggerDedup guards against distinct problems found in review:
//   - A webhook retry (the sender didn't get an ack in time, or a proxy
//     retried) delivers a byte-identical notification again. Without a check,
//     POST /trigger spends the Anthropic budget and races PR/workload actions
//     a second time for the exact same occurrence.
//   - The webhook, poll, and Slack trigger sources can race on the same
//     issue — e.g. a poll cycle fires while a webhook-triggered investigation
//     for that same issue is still running.
//
// It combines a persisted "last dispatched content identity per (source,
// issue ID)" (so a retry of the same content is recognized even across a
// restart) with an in-memory, issue-ID-only in-flight set (so two concurrent
// requests for the same issue, from ANY source, can't both pass the check
// before either finishes — a check-then-act gap a map alone wouldn't close).
//
// The "seen" watermark is deliberately keyed by (source, issueID), not just
// issueID: webhook/Slack use a content hash (see triggerContentHash) and poll
// uses issue.version() — two genuinely different identity schemes, since
// they see different data shapes. Keying by issueID alone meant whichever
// source wrote last silently clobbered the other's retry memory — a poll
// write could erase a webhook's ability to recognize its own retry, and vice
// versa, letting a real retry slip through as if it were new.
type triggerDedup struct {
	mu       sync.Mutex
	path     string
	seen     map[string]string   // "source\x00issueID" -> last dispatched identity for that source
	inFlight map[string]struct{} // issue IDs currently being investigated (in-memory only, any source)
}

func loadTriggerDedup(path string) *triggerDedup {
	d := &triggerDedup{path: path, seen: map[string]string{}, inFlight: map[string]struct{}{}}
	if path == "" {
		return d
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return d // missing/unreadable state file just starts fresh
	}
	_ = json.Unmarshal(data, &d.seen)
	return d
}

func seenKey(source, issueID string) string {
	return source + "\x00" + issueID
}

// tryAcquire reports whether an investigation should be dispatched for this
// source + issue ID + identity (a content hash for webhook/Slack,
// issue.version() for poll — see triggerContentHash and pollWatermark). It
// returns false when either: an investigation for this issue ID is already
// in flight from ANY source, or identity exactly matches the last one this
// SAME source actually dispatched for this issue ID (a retry). On true, it
// reserves the in-flight slot and persists the new watermark — callers MUST
// call release(issueID) once the investigation finishes, success or not.
func (d *triggerDedup) tryAcquire(source, issueID, identity string) bool {
	if issueID == "" {
		return true // nothing to key dedup on; let callers reject empty IDs upstream
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, busy := d.inFlight[issueID]; busy {
		return false
	}
	key := seenKey(source, issueID)
	if prev, ok := d.seen[key]; ok && prev == identity {
		return false
	}
	d.inFlight[issueID] = struct{}{}
	d.seen[key] = identity
	d.persistLocked()
	return true
}

// tryAcquireInFlightOnly is like tryAcquire, but skips the persisted
// replay-suppression check entirely — only the in-flight guard applies. Used
// for Slack's "Fix it" button: a double-click on the button must still be
// rejected (in-flight), but a deliberate second click by a human after the
// first investigation finished — including after it failed — must NOT be
// permanently suppressed just because the issue's content hasn't changed. A
// human clicking a button is not a delivery retry; treating it like one via
// persisted content-hash suppression silently ate every intentional retry
// forever, which is a materially worse failure mode than occasionally
// allowing a genuine double-click through (the in-flight guard alone already
// prevents that specific case).
func (d *triggerDedup) tryAcquireInFlightOnly(issueID string) bool {
	if issueID == "" {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, busy := d.inFlight[issueID]; busy {
		return false
	}
	d.inFlight[issueID] = struct{}{}
	return true
}

// release frees the in-flight slot for issueID. Safe to call even if
// tryAcquire/tryAcquireInFlightOnly was never called or returned false for
// this issueID.
func (d *triggerDedup) release(issueID string) {
	if issueID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inFlight, issueID)
}

func (d *triggerDedup) persistLocked() {
	if d.path == "" {
		return
	}
	data, err := json.Marshal(d.seen)
	if err != nil {
		return
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, d.path)
}

// triggerContentHash summarizes the fields of a TriggerPayload that define
// "the same occurrence" — if all of these are unchanged, a repeat delivery is
// a retry, not a new event worth re-investigating. Used by the webhook and
// Slack trigger sources; poll uses issue.version() instead (see poll.go),
// since it sees a different, richer data shape than a TriggerPayload.
func triggerContentHash(p TriggerPayload) string {
	h := sha256.New()
	for _, field := range []string{p.IssueID, p.Severity, p.DiagnosisName, p.Description, p.Remediation} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
