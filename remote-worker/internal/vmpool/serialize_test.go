package vmpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// testSerializingPool is testPool's helper, but with SerializesExecsPerRun set to
// true BEFORE New is called. Pool.serialize is read once at construction (see
// pool.go's New), matching the real launchers — a real Firecracker launcher's
// answer never changes at runtime — so a test cannot flip it on an
// already-constructed pool the way testPool(t) builds one.
func testSerializingPool(t *testing.T) (Pool, *fakeLauncher, *fakeClock) {
	t.Helper()
	lc := newFakeLauncher()
	lc.setSerialize(true)
	clk := newFakeClock()
	cfg := Config{
		VMM:               lc.Kind(),
		SnapshotDir:       t.TempDir(),
		WorkspaceRoot:     t.TempDir(),
		MaxRuns:           16,
		MaxCommittedBytes: 32 << 30,
	}
	p, err := New(cfg, lc, clk)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, lc, clk
}

// TestExecSerializesRunsPerKeyWhenLauncherRequiresIt is Step 8's required test:
// with a launcher reporting SerializesExecsPerRun() == true, two concurrent Execs
// for the SAME key must never have Run in flight at the same time — the ext4
// workspace image is not a shared-disk filesystem, so two guests mounting it at
// once would corrupt it (spec §4.3). See runPool.execGate's doc comment.
func TestExecSerializesRunsPerKeyWhenLauncherRequiresIt(t *testing.T) {
	p, lc, _ := testSerializingPool(t)

	var inFlight atomic.Int32
	entered := make(chan int32, 4)
	release := make(chan struct{})
	lc.setRunFn(func(v *fakeVM, c Command, out Sink) (Result, error) {
		n := inFlight.Add(1)
		entered <- n
		<-release
		inFlight.Add(-1)
		return Result{ExitCode: 0}, nil
	})

	done := make(chan struct{}, 2)
	go func() {
		_, _ = p.Exec(context.Background(), "run-a", Exec{ReqID: 1, Command: "one", TimeoutS: 5}, discardingSink{})
		done <- struct{}{}
	}()

	// Wait for the first Exec to actually enter Run before starting the second —
	// otherwise a race in launch order could make this test pass for the wrong
	// reason.
	select {
	case n := <-entered:
		if n != 1 {
			t.Fatalf("first entrant saw inFlight=%d, want 1", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first Exec never entered Run")
	}

	go func() {
		_, _ = p.Exec(context.Background(), "run-a", Exec{ReqID: 2, Command: "two", TimeoutS: 5}, discardingSink{})
		done <- struct{}{}
	}()

	// The second Exec targets the SAME key, so it must block on execGate rather
	// than entering Run while the first still holds it. A bounded wait, not a
	// bare sleep, matches the convention in guestconn_test.go for this kind of
	// negative assertion.
	select {
	case n := <-entered:
		t.Fatalf("second Exec for the same key entered Run (inFlight=%d) while the first was still running", n)
	case <-time.After(200 * time.Millisecond):
		// Expected: the gate is held.
	}

	// Release the first; the second must now proceed through Run alone (never
	// overlapping the first, which has already fully exited by the time it is
	// released here).
	release <- struct{}{}
	<-done
	select {
	case n := <-entered:
		if n != 1 {
			t.Fatalf("second entrant saw inFlight=%d, want 1 — it overlapped another Run for the same key", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Exec for the same key never entered Run after the first was released")
	}
	release <- struct{}{}
	<-done
}

// TestExecDoesNotSerializeAcrossDifferentKeys proves the gate above is scoped to
// one run (runPool.execGate), not the whole pool: two different keys must be able
// to run concurrently even though the launcher requires per-run serialization.
func TestExecDoesNotSerializeAcrossDifferentKeys(t *testing.T) {
	p, lc, _ := testSerializingPool(t)

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	lc.setRunFn(func(v *fakeVM, c Command, out Sink) (Result, error) {
		entered <- struct{}{}
		<-release
		return Result{ExitCode: 0}, nil
	})

	done := make(chan struct{}, 2)
	go func() {
		_, _ = p.Exec(context.Background(), "run-a", Exec{ReqID: 1, Command: "one", TimeoutS: 5}, discardingSink{})
		done <- struct{}{}
	}()
	go func() {
		_, _ = p.Exec(context.Background(), "run-b", Exec{ReqID: 2, Command: "two", TimeoutS: 5}, discardingSink{})
		done <- struct{}{}
	}()

	// Both must enter Run without either being released first — if the gate were
	// global rather than per-run, the second would block here until the first's
	// release, and this loop would time out.
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 different-key Execs entered Run concurrently", i)
		}
	}

	close(release)
	<-done
	<-done
}
