package vmpool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A cgroup v2 tree is just a directory hierarchy with cgroup.procs and memory.max
// files, so the sweep's logic is testable against a fake tree with no root and no KVM.
func fakeSlice(t *testing.T, vms map[string][]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "microvm-vms.slice")
	for id, pids := range vms {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strings.Join(pids, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestSweepOrphansFindsEveryLeftoverVMCgroup(t *testing.T) {
	// A previous incarnation of the worker died mid-flight, leaving three VM cgroups.
	// Spec §6: on start, sweep the slice for orphans — otherwise a crash-restart loop
	// leaks VMs at the crash rate and the density number becomes a fiction.
	root := fakeSlice(t, map[string][]string{
		"vm-1": {"4242"},
		"vm-2": {"4243", "4244"},
		"vm-3": {}, // already exited; the directory just needs removing
	})
	swept, err := SweepOrphans(root)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if swept != 3 {
		t.Fatalf("swept = %d, want 3 (pids that no longer exist still count as swept)", swept)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("%d cgroup directories left behind: %v", len(entries), entries)
	}
}

// TestSweepOrphansActuallyKillsALiveProcess covers what
// TestSweepOrphansFindsEveryLeftoverVMCgroup above cannot. Fix round 1 (coordinator
// review of 36dbcb9), item 2: the coordinator mutation-tested that earlier test by
// replacing the kill call in sweepOneCgroup with a no-op, and the test still passed —
// because its pids (4242 etc.) never existed in the first place, so syscall.Kill
// returning ESRCH is indistinguishable from the kill never having been attempted at
// all. A cgroup v2 tree is just directories and files, so this uses a REAL child
// process instead of a fake pid: if the kill stops happening, this process keeps
// running past the test's timeout instead of nothing observably changing.
func TestSweepOrphansActuallyKillsALiveProcess(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting real child process: %v", err)
	}
	pid := cmd.Process.Pid

	// Reap the child as soon as the kernel finishes tearing it down, so waitDone closes
	// the instant SIGKILL actually lands rather than only on this goroutine's own polling
	// cadence, and so the process does not sit around as a zombie either way.
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()

	root := fakeSlice(t, map[string][]string{
		"vm-real": {strconv.Itoa(pid)},
	})

	swept, err := SweepOrphans(root)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}

	select {
	case <-waitDone:
		// Good: the kernel actually reaped the process, i.e. SweepOrphans really
		// signalled it — not merely removed a cgroup directory around it.
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-waitDone
		t.Fatal("real child process was still running well after SweepOrphans returned — the kill was not actually delivered")
	}
}

func TestSweepOrphansIsAbsentSliceTolerant(t *testing.T) {
	// First boot on a fresh host: no slice yet. This must not stop the unit — the
	// posture in spec §6 is "fail at start" for things that make the tier unusable, and
	// an empty slice is not one of them.
	if _, err := SweepOrphans(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatalf("SweepOrphans on an absent slice: %v", err)
	}
}

func TestWriteMemoryMaxBoundsOneVM(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMemoryMax(dir, 256<<20); err != nil {
		t.Fatalf("writeMemoryMax: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "memory.max"))
	// Spec §6's third mitigation: a ballooning command is killed inside its OWN cgroup —
	// one failed Exec, attributable — instead of a host-level OOM lottery whose
	// size-ranked favourites include microvm-worker itself.
	if strings.TrimSpace(string(b)) != "268435456" {
		t.Fatalf("memory.max = %q", b)
	}
}

func TestVMCgroupPathIsUnderTheParentSlice(t *testing.T) {
	got := vmCgroupPath("/sys/fs/cgroup/microvm-vms.slice", "vm-7")
	want := "/sys/fs/cgroup/microvm-vms.slice/vm-7"
	if got != want {
		t.Fatalf("vmCgroupPath = %q, want %q", got, want)
	}
	// The jailer is told the SAME parent (spec §5.3: "Firecracker's jailer has its own
	// --cgroup arguments. They must be configured consistently with the systemd slice §6
	// relies on for cleanup, or the two mechanisms fight and the leak we are preventing
	// returns"), so a path that did not sit under the parent would split the tree in two.
	if !strings.HasPrefix(got, "/sys/fs/cgroup/microvm-vms.slice/") {
		t.Fatal("a VM cgroup outside the parent slice would escape systemd's KillMode")
	}
}

// D1 (hardware-corrections): "two numbers that can drift is the bug." vmCgroupPath's
// own test above only checks the PATH is inside the slice; this checks the VALUE the
// Firecracker launcher configures into --cgroup memory.max= agrees with the same figure
// admission control charges per VM (PerVMBytes), not a second, independently-maintained
// constant.
func TestFirecrackerJailerCgroupMemoryMaxAgreesWithPerVMBytes(t *testing.T) {
	cfg := Config{
		GuestRAMBytes:   256 << 20,
		VMOverheadBytes: DefaultVMOverheadBytes,
	}
	want := PerVMBytes(cfg)

	opts := FirecrackerOptions{
		SnapshotDir:          t.TempDir(),
		JailerBin:            "/usr/bin/jailer",
		FirecrackerBin:       "/usr/bin/firecracker",
		ChrootBase:           t.TempDir(),
		UID:                  1000,
		GID:                  1000,
		ParentCgroup:         "/sys/fs/cgroup/microvm-vms.slice",
		CgroupMemoryMaxBytes: want,
	}
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	args := firecrackerCgroupArgs(opts)
	joined := strings.Join(args, " ")
	wantFlag := "memory.max=" + strconv.FormatInt(want, 10)
	if !strings.Contains(joined, wantFlag) {
		t.Fatalf("jailer args %q do not contain %q — the cgroup bound has drifted from PerVMBytes(cfg) = %d", joined, wantFlag, want)
	}
}
