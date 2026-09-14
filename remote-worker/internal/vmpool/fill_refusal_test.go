package vmpool

import (
	"context"
	"testing"
)

// TestFillStandbysCountsARefusal pins the diagnostic gap E10's teardown-bulk record
// exposed: the mode reported "failures: 143, refusals: {}" -- the count was taken, the
// REASON was not, and the one field that would have explained 143 failures was empty.
//
// Scope worth being exact about, because the first reading of this was wrong: acquire's
// production path already wrapped this same error in countRefusal (see pool.go), so
// Stats().Refusals was never under-reporting for real Execs. FillStandbys, reachable only
// from vmpoolctl's BenchmarkHooks, was the single site that dropped it.
func TestFillStandbysCountsARefusal(t *testing.T) {
	// MaxRuns=1 so the second key's fill is refused at the run ceiling, which is the
	// cheapest of the two ceilings to provoke deterministically.
	lc := newFakeLauncher()
	clk := newFakeClock()
	p, err := New(Config{
		VMM:               lc.Kind(),
		SnapshotDir:       t.TempDir(),
		WorkspaceRoot:     t.TempDir(),
		MaxRuns:           1,
		MaxCommittedBytes: 32 << 30,
	}, lc, clk)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	hooks, ok := p.(BenchmarkHooks)
	if !ok {
		t.Fatal("pool does not implement BenchmarkHooks")
	}

	if err := hooks.FillStandbys(context.Background(), "run-a", 1); err != nil {
		t.Fatalf("first key's fill: %v", err)
	}
	// A second RUN is one past MaxRuns=1, so this fill must be refused.
	err = hooks.FillStandbys(context.Background(), "run-b", 1)
	if err == nil {
		t.Fatal("a second run's fill was admitted with MaxRuns=1")
	}
	if got := ReasonOf(err); got != RefuseMaxRuns {
		t.Fatalf("reason = %q, want %q", got, RefuseMaxRuns)
	}
	if got := p.Stats().Refusals[RefuseMaxRuns]; got != 1 {
		t.Fatalf("Stats().Refusals[%s] = %d, want 1 — a counted failure with no counted "+
			"reason is a record that cannot explain itself", RefuseMaxRuns, got)
	}
}

// TestFillStandbysCountsNothingWhenItSucceeds is the converse: a hook that incremented the
// refusal counter unconditionally would satisfy the test above while making every clean run
// look like it had been throttled — and E11's prediction 1 is ABOUT reading that counter as
// back-pressure, so a false positive there is worse than a missing count.
func TestFillStandbysCountsNothingWhenItSucceeds(t *testing.T) {
	p, _, _ := testPool(t)
	hooks, ok := p.(BenchmarkHooks)
	if !ok {
		t.Fatal("pool does not implement BenchmarkHooks")
	}
	if err := hooks.FillStandbys(context.Background(), "run-a", 2); err != nil {
		t.Fatalf("FillStandbys: %v", err)
	}
	if got := p.Stats().Refusals; len(got) != 0 {
		t.Fatalf("Refusals = %v after a clean fill, want empty", got)
	}
}
