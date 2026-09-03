package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// claudeModel is the Anthropic model used for every call in the investigation loop.
const claudeModel = "claude-sonnet-4-6"

// modelPricing holds USD cost per 1M tokens, keyed by model name.
// Source: https://www.anthropic.com/pricing — hardcoded and needs periodic
// re-verification; it will silently drift out of date otherwise.
var modelPricing = map[string]struct {
	InputPerMTok  float64
	OutputPerMTok float64
}{
	"claude-sonnet-4-6": {InputPerMTok: 3.0, OutputPerMTok: 15.0},
}

// costUSD converts a single call's token usage into a dollar amount, falling
// back to the claudeModel's pricing if the model isn't in the table.
func costUSD(model string, inputTokens, outputTokens int) float64 {
	p, ok := modelPricing[model]
	if !ok {
		p = modelPricing[claudeModel]
	}
	return float64(inputTokens)/1_000_000*p.InputPerMTok + float64(outputTokens)/1_000_000*p.OutputPerMTok
}

// costTracker accumulates USD spend and token usage across a single
// investigation and enforces a per-investigation budget. A zero or negative
// budget disables the cap.
type costTracker struct {
	budgetUSD    float64
	spentUSD     float64
	inputTokens  int
	outputTokens int
}

func newCostTracker(budgetUSD float64) *costTracker {
	return &costTracker{budgetUSD: budgetUSD}
}

func (c *costTracker) add(model string, inputTokens, outputTokens int) {
	c.spentUSD += costUSD(model, inputTokens, outputTokens)
	c.inputTokens += inputTokens
	c.outputTokens += outputTokens
}

func (c *costTracker) exceeded() bool {
	return c.budgetUSD > 0 && c.spentUSD > c.budgetUSD
}

// weeklyBudgetWindow is a rolling window, not a calendar week — a flapping
// incident can spike spend starting on any day, and the point of this cap is
// to bound worst-case spend over any 7-day span, not to reset on Mondays.
const weeklyBudgetWindow = 7 * 24 * time.Hour

type costEntry struct {
	At  time.Time `json:"at"`
	USD float64   `json:"usd"`
}

// weeklyBudget enforces an aggregate cost ceiling across ALL investigations,
// independent of costTracker's per-investigation cap. Without this, a root
// cause that keeps recurring (each occurrence individually well under the
// per-incident cap) could still generate unbounded weekly spend.
//
// State is kept in memory and, if persistPath is set, mirrored to disk so a
// pod restart mid-week doesn't silently reset the counter to zero.
type weeklyBudget struct {
	mu          sync.Mutex
	limitUSD    float64 // <= 0 disables the cap
	entries     []costEntry
	persistPath string // optional; empty disables cross-restart persistence
}

func newWeeklyBudget(limitUSD float64, persistPath string) *weeklyBudget {
	w := &weeklyBudget{limitUSD: limitUSD, persistPath: persistPath}
	w.load()
	return w
}

func (w *weeklyBudget) load() {
	if w.persistPath == "" {
		return
	}
	raw, err := os.ReadFile(w.persistPath)
	if err != nil {
		return // fine on first run, or if no volume is mounted at this path
	}
	var entries []costEntry
	if err := json.Unmarshal(raw, &entries); err == nil {
		w.entries = entries
	}
}

// persist best-effort mirrors current entries to disk. Caller must hold w.mu.
func (w *weeklyBudget) persist() {
	if w.persistPath == "" {
		return
	}
	raw, err := json.Marshal(w.entries)
	if err != nil {
		return
	}
	_ = os.WriteFile(w.persistPath, raw, 0o644)
}

// pruneLocked drops entries outside the rolling window. Caller must hold w.mu.
func (w *weeklyBudget) pruneLocked(now time.Time) {
	cutoff := now.Add(-weeklyBudgetWindow)
	kept := w.entries[:0]
	for _, e := range w.entries {
		if e.At.After(cutoff) {
			kept = append(kept, e)
		}
	}
	w.entries = kept
}

// spent returns total USD spent within the rolling window as of now.
func (w *weeklyBudget) spent(now time.Time) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(now)
	var total float64
	for _, e := range w.entries {
		total += e.USD
	}
	return total
}

// exceeded reports whether the rolling-window spend has already hit the cap.
// Check this BEFORE starting an investigation, to skip it entirely rather
// than spending even the first Claude call once the weekly cap is blown.
func (w *weeklyBudget) exceeded() bool {
	return w.limitUSD > 0 && w.spent(time.Now()) >= w.limitUSD
}

// add records a completed (or aborted) investigation's actual cost.
func (w *weeklyBudget) add(usd float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	w.pruneLocked(now)
	w.entries = append(w.entries, costEntry{At: now, USD: usd})
	w.persist()
}
