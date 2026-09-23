package agent

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCostUSD(t *testing.T) {
	got := costUSD(claudeModel, 1_000_000, 1_000_000)
	want := 3.0 + 15.0
	if got != want {
		t.Fatalf("costUSD() = %v, want %v", got, want)
	}
}

func TestCostUSD_UnknownModelFallsBackToClaudeModelPricing(t *testing.T) {
	got := costUSD("some-future-model", 1_000_000, 1_000_000)
	want := costUSD(claudeModel, 1_000_000, 1_000_000)
	if got != want {
		t.Fatalf("costUSD() with unknown model = %v, want fallback %v", got, want)
	}
}

// TestWeeklyBudget_ConcurrencyLimitBoundsInFlightAcquires guards the fix for
// the check-then-act race: without a concurrency limit, an unbounded burst of
// goroutines could all pass exceeded() before any of them recorded spend.
// withConcurrencyLimit(n) must let at most n callers hold an acquired slot at
// once; acquire() blocks until one frees up.
func TestWeeklyBudget_ConcurrencyLimitBoundsInFlightAcquires(t *testing.T) {
	w := newWeeklyBudget(0, "").withConcurrencyLimit(2)

	w.acquire()
	w.acquire()

	acquired := make(chan struct{})
	go func() {
		w.acquire()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("a third acquire() succeeded while 2 slots (the limit) were already held")
	case <-time.After(50 * time.Millisecond):
		// expected: blocked
	}

	w.release()
	select {
	case <-acquired:
		// expected: releasing a slot let the blocked acquire() through
	case <-time.After(time.Second):
		t.Fatal("acquire() did not unblock after release()")
	}
	w.release()
}

// TestWeeklyBudget_NoConcurrencyLimitDoesNotBlock guards that the default
// (withConcurrencyLimit not called, or called with <= 0) leaves acquire/release
// as no-ops, matching every other caller/test that constructs a weeklyBudget
// directly via newWeeklyBudget without opting into a limit.
func TestWeeklyBudget_NoConcurrencyLimitDoesNotBlock(t *testing.T) {
	w := newWeeklyBudget(0, "")
	for i := 0; i < 100; i++ {
		w.acquire() // must never block when no limit was configured
	}
}

func TestCostTracker_NotExceededUnderBudget(t *testing.T) {
	tr := newCostTracker(5.0)
	tr.add(claudeModel, 100_000, 50_000) // well under $5
	if tr.exceeded() {
		t.Fatalf("expected not exceeded, spent=%v budget=%v", tr.spentUSD, tr.budgetUSD)
	}
}

func TestCostTracker_ExceededOverBudget(t *testing.T) {
	tr := newCostTracker(1.0)
	tr.add(claudeModel, 1_000_000, 1_000_000) // $18, way over $1
	if !tr.exceeded() {
		t.Fatalf("expected exceeded, spent=%v budget=%v", tr.spentUSD, tr.budgetUSD)
	}
}

func TestCostTracker_ZeroBudgetDisablesCap(t *testing.T) {
	tr := newCostTracker(0)
	tr.add(claudeModel, 100_000_000, 100_000_000)
	if tr.exceeded() {
		t.Fatalf("expected zero budget to disable the cap")
	}
}

func TestWeeklyBudget_NotExceededUnderLimit(t *testing.T) {
	w := newWeeklyBudget(50.0, "")
	w.add(4.0)
	w.add(4.0)
	if w.exceeded() {
		t.Fatalf("expected not exceeded, spent=%v limit=%v", w.spent(time.Now()), w.limitUSD)
	}
}

func TestWeeklyBudget_ExceededOverLimit(t *testing.T) {
	w := newWeeklyBudget(10.0, "")
	w.add(4.0)
	w.add(4.0)
	w.add(4.0) // $12 > $10
	if !w.exceeded() {
		t.Fatalf("expected exceeded, spent=%v limit=%v", w.spent(time.Now()), w.limitUSD)
	}
}

func TestWeeklyBudget_ZeroLimitDisablesCap(t *testing.T) {
	w := newWeeklyBudget(0, "")
	w.add(1000.0)
	if w.exceeded() {
		t.Fatalf("expected zero limit to disable the cap")
	}
}

func TestWeeklyBudget_EntriesOutsideRollingWindowAreExcluded(t *testing.T) {
	w := newWeeklyBudget(10.0, "")
	now := time.Now()
	// Manually seed an entry from 8 days ago — outside the 7-day window — plus
	// a recent one, then check spent() as of "now" excludes the stale entry.
	w.entries = []costEntry{
		{At: now.Add(-8 * 24 * time.Hour), USD: 100.0},
		{At: now.Add(-1 * time.Hour), USD: 3.0},
	}
	got := w.spent(now)
	if got != 3.0 {
		t.Fatalf("spent() = %v, want 3.0 (the 8-day-old entry should be pruned out of the rolling window)", got)
	}
	if w.exceeded() {
		t.Fatalf("expected not exceeded once the stale entry is pruned")
	}
}

func TestWeeklyBudget_PersistsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost_state.json")

	first := newWeeklyBudget(50.0, path)
	first.add(4.0)
	first.add(4.0)

	second := newWeeklyBudget(50.0, path)
	got := second.spent(time.Now())
	if got != 8.0 {
		t.Fatalf("spent() after reload = %v, want 8.0 (prior spend should survive a restart)", got)
	}
}

func TestWeeklyBudget_NoStateFileOnFirstRunIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist-yet.json")
	w := newWeeklyBudget(50.0, path)
	if w.exceeded() {
		t.Fatalf("expected a fresh budget with no prior state to not be exceeded")
	}
}

func TestWeeklyBudget_ConcurrentAddsAreSafe(t *testing.T) {
	w := newWeeklyBudget(1000.0, "")
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.add(1.0)
		}()
	}
	wg.Wait()
	got := w.spent(time.Now())
	if got != 50.0 {
		t.Fatalf("spent() = %v, want 50.0 after 50 concurrent $1 adds", got)
	}
}
