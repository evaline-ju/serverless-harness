package vmpool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// launcherForArm returns the launcher for whichever arm the rig operator selected via
// SH_VMM, matching cmd/microvm-worker/main.go's own poolConfig convention so a gate run
// with SH_VMM=cloud-hypervisor exercises the same launcher production would build. Every
// caller of this helper already calls requireKVM(t) first — SH_VMM only chooses which
// real VMM a KVM-gated gate drives; it never itself causes a VM to launch.
func launcherForArm(t *testing.T) Launcher {
	t.Helper()
	switch v := envOr("SH_VMM", "firecracker"); v {
	case "firecracker":
		return fcLauncher(t)
	case "cloud-hypervisor":
		return chvLauncherForGate(t)
	default:
		t.Fatalf("SH_VMM=%q: unknown arm, want %q or %q", v, "firecracker", "cloud-hypervisor")
		return nil
	}
}

// chvLauncherForGate builds the Cloud Hypervisor launcher for the gates below, skipping
// (not failing) when the binary itself is not installed on this machine —
// launcher_chv_test.go's own real-VMM tests take the same posture, since a missing
// binary is a rig-provisioning fact, not a correctness failure this suite reports on.
// Named "ForGate" rather than plain chvLauncher: launcher_chv.go already defines an
// unexported TYPE named chvLauncher for the Launcher implementation itself.
func chvLauncherForGate(t *testing.T) Launcher {
	t.Helper()
	opts := chvOpts(t)
	if _, err := exec.LookPath(opts.CHVBin); err != nil {
		t.Skipf("cloud-hypervisor not installed: %v", err)
	}
	lc, err := NewCloudHypervisorLauncher(opts)
	if err != nil {
		t.Fatalf("NewCloudHypervisorLauncher: %v", err)
	}
	return lc
}

// poolFor builds a real Pool over lc for the gates that exercise Pool-level behaviour
// (parking, reclaim, teardown, cross-run isolation) rather than a bare Launcher. Mirrors
// testPool's shape but takes the real launcher and a real clock, since these gates are
// exactly the ones spec §8 requires against real KVM, not the fake.
func poolFor(t *testing.T, lc Launcher) Pool {
	t.Helper()
	// WorkspaceRoot must share a device with the Firecracker arm's SnapshotDir and
	// ChrootBase (see firecrackerLauncher.checkDeviceSharing): Restore() hardlinks
	// workspace.img from here into the jail root, and hardlink(2) is EXDEV across
	// devices. Routed through sameDeviceSiblingDir keyed to the SAME snapshotDir
	// value fcLauncher uses, exactly like ChrootBase and chvOpts's RunDir are,
	// rather than a bare t.TempDir(). Harmless for the Cloud Hypervisor arm, whose
	// checkDeviceSharing does not constrain WorkspaceRoot at all -- virtiofsd shares
	// it with the guest live and it is never hardlinked.
	snapshotDir := envOr("SH_SNAPSHOT_IMAGE_DIR", "/srv/snapshots/swebench-py311")
	cfg := Config{
		VMM:               lc.Kind(),
		SnapshotDir:       t.TempDir(),
		WorkspaceRoot:     sameDeviceSiblingDir(t, snapshotDir),
		MaxRuns:           4,
		MaxCommittedBytes: 32 << 30,
	}
	p, err := New(cfg, lc, RealClock())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// Spec §8: "A file written in Exec N is intact in Exec N+1. Both VMM arms — this is the
// gate that catches a missing sync." Two Execs against the SAME workspace_key; the
// second reads what the first wrote. On the Firecracker arm this is the gate that would
// catch a dropped `sync` in wrapCommand before VM teardown (the guest's ext4 write cache
// would otherwise still be dirty when the VM is destroyed); on the Cloud Hypervisor arm
// the write is already durable via virtio-fs's host-filesystem authority, so this gate is
// expected to pass there structurally rather than by catching anything (see
// TestCloudHypervisorRestoresPausedAndRunsOneCommand's comment).
func TestGateWriteDurability(t *testing.T) {
	requireKVM(t)
	lc := launcherForArm(t)
	p := poolFor(t, lc)
	ctx := context.Background()
	const key = "gate-durability"

	if _, err := p.Exec(ctx, key, Exec{Command: "echo n1 >> log.txt", TimeoutS: 30}, &capturingSink{}); err != nil {
		t.Fatalf("Exec N: %v", err)
	}
	var out capturingSink
	if _, err := p.Exec(ctx, key, Exec{Command: "cat log.txt", TimeoutS: 30}, &out); err != nil {
		t.Fatalf("Exec N+1: %v", err)
	}
	if out.out() != "n1\n" {
		t.Fatalf("Exec N+1 read %q, want %q — a write from Exec N was lost", out.out(), "n1\n")
	}
}

// Spec §8: "Two interleaved workspace_keys; neither sees the other's files. The property
// this slice exists for." Interleaved, not merely alternated: write A, write B, read A,
// read B — so a read landing in the wrong workspace would have a DIFFERENT run's write
// available to leak, not just an empty file that happens to look clean.
func TestGateNoCrossRunBleed(t *testing.T) {
	requireKVM(t)
	lc := launcherForArm(t)
	p := poolFor(t, lc)
	ctx := context.Background()

	if _, err := p.Exec(ctx, "run-a", Exec{Command: "echo from-a > secret.txt", TimeoutS: 30}, &capturingSink{}); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if _, err := p.Exec(ctx, "run-b", Exec{Command: "echo from-b > secret.txt", TimeoutS: 30}, &capturingSink{}); err != nil {
		t.Fatalf("write B: %v", err)
	}
	var outA capturingSink
	if _, err := p.Exec(ctx, "run-a", Exec{Command: "cat secret.txt", TimeoutS: 30}, &outA); err != nil {
		t.Fatalf("read A: %v", err)
	}
	if outA.out() != "from-a\n" {
		t.Fatalf("run-a read %q, want %q — it saw run-b's file", outA.out(), "from-a\n")
	}
	var outB capturingSink
	if _, err := p.Exec(ctx, "run-b", Exec{Command: "cat secret.txt", TimeoutS: 30}, &outB); err != nil {
		t.Fatalf("read B: %v", err)
	}
	if outB.out() != "from-b\n" {
		t.Fatalf("run-b read %q, want %q — it saw run-a's file", outB.out(), "from-b\n")
	}
}

// Spec §8: "An Exec with workspace_key: \"\" gets a counted ExecError and runs nothing" —
// assert the launcher created zero VMs. No KVM needed: the refusal happens before Acquire
// ever calls the launcher, which is exactly what this asserts against the fake.
func TestGateEmptyKeyIsRefused(t *testing.T) {
	p, lc, _ := testPool(t)
	_, err := p.Exec(context.Background(), "", Exec{Command: "true"}, &capturingSink{})
	if err == nil {
		t.Fatal("Exec with an empty workspace_key: want an error, got nil")
	}
	if got := ReasonOf(err); got != RefuseEmptyKey {
		t.Fatalf("ReasonOf(err) = %v, want %v", got, RefuseEmptyKey)
	}
	if n := lc.createdCount(); n != 0 {
		t.Fatalf("launcher created %d VMs for a refused Exec, want 0 — the gate is that nothing runs", n)
	}
	if got := p.Stats().Refusals[RefuseEmptyKey]; got != 1 {
		t.Fatalf("Stats().Refusals[RefuseEmptyKey] = %d, want 1 — the refusal must be counted", got)
	}
}

// Spec §8: "The workspace_key assertion holds under concurrency — N goroutines, M keys,
// every VM's Run called exactly once (the real launchers must fail a second Run, as the
// fake does)." Split in two: a fake-launcher half that runs everywhere and proves the
// concurrency property itself (many goroutines racing a handful of keys, no coordination
// beyond what the Pool provides — fakeVM.Run's own "Run called %d times" guard would
// surface as an Exec error if any VM were ever handed a second Run), and a real-launcher
// half gated on KVM. The real launchers do not re-implement that counter themselves —
// the guarantee there is pool.go's own single-Run-per-VM discipline (Acquire, Resume,
// Run, one Destroy defer for every path; no code path reuses a VM handle) — so the real
// half proves that discipline holds under concurrency against actual hardware, not just
// the test double.
func TestGateNoVMReuse(t *testing.T) {
	t.Run("fake_launcher_concurrency", func(t *testing.T) {
		p, lc, _ := testPool(t)
		keys := []string{"run-a", "run-b", "run-c"}
		const perKey = 4
		var wg sync.WaitGroup
		errs := make(chan error, len(keys)*perKey)
		for _, key := range keys {
			for i := 0; i < perKey; i++ {
				wg.Add(1)
				go func(key string) {
					defer wg.Done()
					_, err := p.Exec(context.Background(), key, Exec{Command: "true", TimeoutS: 5}, &capturingSink{})
					errs <- err
				}(key)
			}
		}
		wg.Wait()
		close(errs)
		total := 0
		for err := range errs {
			total++
			if err != nil {
				t.Fatalf("concurrent Exec: %v", err)
			}
		}
		if got := lc.createdCount(); got != total {
			t.Fatalf("launcher created %d VMs for %d concurrent Execs across %d keys, want exactly one VM per Exec (no reuse)", got, total, len(keys))
		}
	})

	t.Run("real_launcher", func(t *testing.T) {
		requireKVM(t)
		lc := launcherForArm(t)
		p := poolFor(t, lc)
		keys := []string{"gate-reuse-a", "gate-reuse-b"}
		const perKey = 2
		var wg sync.WaitGroup
		errs := make(chan error, len(keys)*perKey)
		for _, key := range keys {
			for i := 0; i < perKey; i++ {
				wg.Add(1)
				go func(key string) {
					defer wg.Done()
					_, err := p.Exec(context.Background(), key, Exec{Command: "true", TimeoutS: 30}, &capturingSink{})
					errs <- err
				}(key)
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent Exec against real launcher: %v", err)
			}
		}
	})
}

// Spec §5.2's invariant, pinned with a test rather than trusted: "nothing secret or
// unique may exist in the golden snapshot." Firecracker documents resuming one
// snapshot more than once as INSECURE — IDs, RNG seeds, entropy pools and tokens are
// duplicated, and while VMGenID reseeds the kernel PRNG on Linux >= 5.18, non-kernel
// state "will still be replicated". We use that pattern knowingly, so this is the
// mitigation, and a test is what keeps it true after someone adds a convenience env var
// to the guest image.
//
// Duplicated guest ASLR is NOT what this checks and is not a boundary we rely on: the
// attacker already executes arbitrary code inside the guest, and our boundary is KVM.
func TestGateSnapshotHoldsNoSecrets(t *testing.T) {
	requireKVM(t)

	// The memfile grep is filesystem-level and arm-independent: build-snapshot.sh's
	// lock_down step writes the golden $OUT under the name "memfile" for BOTH arms (it
	// renames Cloud Hypervisor's native memory-ranges to memfile at snapshot build
	// time — see boot_quiesce_snapshot_cloud_hypervisor and lock_down), so this half
	// needs no launcher at all.
	memfile := filepath.Join(envOr("SH_SNAPSHOT_IMAGE_DIR", "/srv/snapshots/swebench-py311"), "memfile")
	b, err := os.ReadFile(memfile)
	if err != nil {
		t.Fatalf("reading golden memfile %s: %v", memfile, err)
	}
	for _, pattern := range []string{"AKIA", "ghp_", "-----BEGIN"} {
		if bytes.Contains(b, []byte(pattern)) {
			t.Errorf("golden memfile contains credential pattern %q", pattern)
		}
	}
	if live := os.Getenv("SANDBOX_TOKEN"); live != "" && bytes.Contains(b, []byte(live)) {
		t.Error("golden memfile contains the live SANDBOX_TOKEN value")
	}

	lc := launcherForArm(t)
	p := poolFor(t, lc)
	var out capturingSink
	if _, err := p.Exec(context.Background(), "gate-secrets", Exec{Command: "env", TimeoutS: 30}, &out); err != nil {
		t.Fatalf("Exec env: %v", err)
	}
	for _, name := range []string{"SANDBOX_TOKEN", "AWS_", "ANTHROPIC_"} {
		if strings.Contains(out.out(), name) {
			t.Errorf("guest env contains %q:\n%s", name, out.out())
		}
	}
}

// countCgroupPids sums cgroup.procs across every VM cgroup directly under parentSlice,
// reusing cgroup.go's own file format rather than re-deriving it. Never counts by
// process name — cgroup.go's D8 doc comment: cloud-hypervisor's 16-char name is
// truncated to 15 by TASK_COMM_LEN, so pgrep/pkill -x cloud-hypervisor never matches,
// and a name-based count would silently pass on that arm while leaking.
func countCgroupPids(parentSlice string) (int, error) {
	entries, err := os.ReadDir(parentSlice)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	total := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pids, err := readCgroupProcs(filepath.Join(parentSlice, e.Name(), "cgroup.procs"))
		if err != nil {
			return 0, err
		}
		total += len(pids)
	}
	return total, nil
}

// Spec §8: "After N Execs across R completed runs, VM process count returns to baseline
// within StandbyIdle + ReclaimScanInterval and workspace count within WorkspaceIdle +
// ReclaimScanInterval — bounds, because at test timescales 'eventually' is
// indistinguishable from a leak." Uses a short StandbyIdle/WorkspaceIdle in the test
// Config and a REAL clock (spec §8 does not let this gate use the fake clock's instant
// advance — the bound is a wall-clock claim), and states its numeric bound in every
// failure message rather than looping forever.
func TestGateLeakFreeTeardown(t *testing.T) {
	requireKVM(t)
	lc := launcherForArm(t)

	const standbyIdle = 2 * time.Second
	const workspaceIdle = 4 * time.Second
	const reclaimScan = 500 * time.Millisecond
	// See poolFor's identical comment: WorkspaceRoot must share a device with the
	// Firecracker arm's SnapshotDir/ChrootBase, so it is routed through
	// sameDeviceSiblingDir rather than a bare t.TempDir(), keyed to the same
	// snapshotDir value fcLauncher used to build lc above.
	snapshotDir := envOr("SH_SNAPSHOT_IMAGE_DIR", "/srv/snapshots/swebench-py311")
	workspaceRoot := sameDeviceSiblingDir(t, snapshotDir)
	cfg := Config{
		VMM:                 lc.Kind(),
		SnapshotDir:         t.TempDir(),
		WorkspaceRoot:       workspaceRoot,
		MaxRuns:             4,
		MaxCommittedBytes:   32 << 30,
		StandbyIdle:         standbyIdle,
		WorkspaceIdle:       workspaceIdle,
		ReclaimScanInterval: reclaimScan,
	}
	p, err := New(cfg, lc, RealClock())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()

	parentSlice := envOr("SH_VM_CGROUP_SLICE", "/sys/fs/cgroup/microvm-vms.slice")
	baseline, err := countCgroupPids(parentSlice)
	if err != nil {
		t.Fatalf("baseline cgroup count: %v", err)
	}

	const runs = 3
	keys := make([]string, runs)
	for i := 0; i < runs; i++ {
		keys[i] = fmt.Sprintf("gate-teardown-%d", i)
		if _, err := p.Exec(context.Background(), keys[i], Exec{Command: "true", TimeoutS: 30}, &capturingSink{}); err != nil {
			t.Fatalf("Exec %s: %v", keys[i], err)
		}
	}

	// Slop is scheduling/poll-interval margin on top of the stated bound, not part of
	// the claim itself — the failure message below states the actual bound.
	const slop = 3 * time.Second

	standbyBound := standbyIdle + reclaimScan
	deadline := time.Now().Add(standbyBound + slop)
	for {
		n, err := countCgroupPids(parentSlice)
		if err != nil {
			t.Fatalf("cgroup count: %v", err)
		}
		if n <= baseline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("VM cgroup pid count = %d (baseline %d) after waiting past StandbyIdle+ReclaimScanInterval = %s (+%s slop) — VMs leaked", n, baseline, standbyBound, slop)
		}
		time.Sleep(50 * time.Millisecond)
	}

	workspaceBound := workspaceIdle + reclaimScan
	deadline = time.Now().Add(workspaceBound + slop)
	for {
		remaining := 0
		for _, key := range keys {
			if _, err := os.Stat(filepath.Join(workspaceRoot, key)); err == nil {
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d/%d workspaces still present after waiting past WorkspaceIdle+ReclaimScanInterval = %s (+%s slop) — workspaces leaked", remaining, runs, workspaceBound, slop)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Spec §8: "Drop a run's standbys at StandbyIdle, wait past it, Exec the same key: cold
// acquire, same workspace, no error." No KVM needed: this is one of the three gates this
// task requires to actually run (against the fake launcher and clock), since the
// property under test is the Pool's own parking/resume bookkeeping, not anything only
// real hardware could get wrong.
func TestGateParkedThenResumed(t *testing.T) {
	p, lc, clk := testPool(t)
	var dir string
	lc.setBeforeRestore(func(r RestoreRequest) { dir = r.WorkspaceDir })
	primed(t, p, clk, "gate-park")

	marker := dir + "/marker"
	if err := os.WriteFile(marker, []byte("same tree"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	clk.Advance(DefaultStandbyIdle + DefaultStandbyIdle/4 + time.Second)
	waitFor(t, func() bool { return p.Stats().StandbysResident == 0 })
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("workspace %s gone at StandbyIdle: %v — parking drops standbys, not the workspace", dir, err)
	}

	before := p.Stats().ColdAcquires[ColdParked]
	if _, err := p.Exec(context.Background(), "gate-park", Exec{Command: "true"}, &capturingSink{}); err != nil {
		t.Fatalf("Exec after parking: %v", err)
	}
	if got := p.Stats().ColdAcquires[ColdParked] - before; got != 1 {
		t.Fatalf("ColdAcquires[ColdParked] rose by %d, want 1 — a parked-then-resumed run must be a cold acquire", got)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "same tree" {
		t.Fatalf("marker after resume = %q, err=%v — resuming after parking must see the SAME workspace, not a fresh one", b, err)
	}
}

// Spec §8: "Reclaim a key outright, then Exec it again: the tree is re-derived and the
// leaf proceeds." No KVM needed: this is one of the three gates this task requires to
// actually run (against the fake launcher and clock) — Reclaim's contract (drop
// everything for the key, including the workspace) and re-derivation on the next Exec
// are Pool-level behaviour.
func TestGateReclaimThenRedispatch(t *testing.T) {
	p, lc, clk := testPool(t)
	var dir string
	lc.setBeforeRestore(func(r RestoreRequest) { dir = r.WorkspaceDir })
	primed(t, p, clk, "gate-reclaim")

	if err := p.Reclaim(context.Background(), "gate-reclaim"); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if s := p.Stats(); s.StandbysResident != 0 || s.ActiveRuns != 0 {
		t.Fatalf("Stats = %+v after Reclaim, want nothing left for the key", s)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("workspace %s survived Reclaim: err=%v", dir, err)
	}
	waitFor(t, func() bool { return lc.liveCount() == 0 })

	var out capturingSink
	if _, err := p.Exec(context.Background(), "gate-reclaim", Exec{Command: "redispatched"}, &out); err != nil {
		t.Fatalf("Exec after Reclaim: %v — the leaf must proceed on the re-derived tree", err)
	}
	if out.out() != "redispatched" {
		t.Fatalf("post-Reclaim Exec output = %q, want %q (fake launcher echoes Command verbatim)", out.out(), "redispatched")
	}
}

// Spec §8: "A VM's date +%s is within a second of the host's — catches §5.3's wall-clock
// trap, and is the end-to-end check on Deviation 5's agent-side clock_settime." A
// paused-then-resumed golden snapshot's guest clock is frozen at snapshot-build time
// until something resets it; this is the one gate that actually dials the guest and
// reads its own idea of the time, rather than trusting the mitigation exists.
func TestGateClock(t *testing.T) {
	requireKVM(t)
	lc := launcherForArm(t)
	p := poolFor(t, lc)

	before := time.Now().Unix()
	var out capturingSink
	if _, err := p.Exec(context.Background(), "gate-clock", Exec{Command: "date +%s", TimeoutS: 30}, &out); err != nil {
		t.Fatalf("Exec date: %v", err)
	}
	after := time.Now().Unix()

	guestSecs, err := strconv.ParseInt(strings.TrimSpace(out.out()), 10, 64)
	if err != nil {
		t.Fatalf("parsing guest date output %q: %v", out.out(), err)
	}
	// One second of slop on each side of the host window the Exec spanned — anything
	// outside that is the wall-clock trap, not scheduling jitter.
	if guestSecs < before-1 || guestSecs > after+1 {
		t.Fatalf("guest `date +%%s` = %d, host window = [%d, %d] — guest clock is stale or wrong", guestSecs, before-1, after+1)
	}
}

// Spec §8: "16 MiB of stdout against the 8 MiB cap returns DroppedStdout > 0, the
// command's REAL exit code, and — the point of capping in the guest — a wall time far
// below what moving 16 MiB across vsock would cost." The cap must be applied by the
// guest agent writing the stream, not by the host truncating after receiving the full
// 16 MiB, so this asserts the wall-clock signature of the former, not just the byte
// count. The 15s bound below is a smoke bound, not a measured throughput figure — it is
// not tuned to the rig's actual vsock throughput (that number belongs in the E10/E11
// performance rungs, not this gate); it only needs to be comfortably less than what
// producing and shipping the full, uncapped 16 MiB would take.
func TestGateOutputCapAtSource(t *testing.T) {
	requireKVM(t)
	lc := launcherForArm(t)
	p := poolFor(t, lc)

	const cmd = "dd if=/dev/zero bs=1M count=16 2>/dev/null; exit 7"
	start := time.Now()
	var out capturingSink
	res, err := p.Exec(context.Background(), "gate-outputcap", Exec{Command: cmd, TimeoutS: 60}, &out)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7 — the real exit code, cap or not", res.ExitCode)
	}
	if res.DroppedStdout <= 0 {
		t.Fatalf("DroppedStdout = %d, want > 0 for 16 MiB against an %d-byte cap", res.DroppedStdout, OutputCapBytes)
	}
	if int64(len(out.out())) > OutputCapBytes {
		t.Fatalf("captured stdout = %d bytes, want <= the %d-byte cap", len(out.out()), OutputCapBytes)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("Exec took %s to return 16 MiB of (capped) stdout — the cap does not look like it applied at the guest source", elapsed)
	}
}
