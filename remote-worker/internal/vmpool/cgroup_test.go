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
//
// The map keys are raw DIRECTORY NAMES, not necessarily VM ids. That distinction is the
// whole of final-review H1: the shipped unit's Slice=microvm-vms.slice puts
// microvm-worker.service's OWN cgroup in this slice as a sibling of every VM cgroup, so
// tests must be able to build that name too.
func fakeSlice(t *testing.T, dirs map[string][]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "microvm-vms.slice")
	for id, pids := range dirs {
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

// startSleeper starts a real, long-lived child process and returns its pid plus a
// channel that closes the moment the kernel reaps it. A REAL process is the point (see
// TestSweepOrphansActuallyKillsALiveProcess): a fabricated pid makes "the kill was
// delivered" and "the kill was replaced with a no-op" indistinguishable, because
// syscall.Kill returns ESRCH either way.
//
// ownProcessGroup chooses which of two production shapes the child stands in for, and it
// is not a detail:
//
//   - true — a leaked VMM. Both launchers Setpgid their VMM into its own process group,
//     and a real orphan comes from a PREVIOUS worker incarnation, so it is never in the
//     sweeper's process group. Stand-ins for orphans must match that or SweepOrphans'
//     third guard refuses them and the test proves nothing about the sweep.
//   - false — the worker itself, or anything else sharing the sweeper's process group,
//     which that same guard must refuse.
func startSleeper(t *testing.T, ownProcessGroup bool) (pid int, reaped <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if ownProcessGroup {
		isolateProcessGroupForTest(cmd)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting real child process: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd.Process.Pid, done
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
	res, err := SweepOrphans(root)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if res.Swept != 3 {
		t.Fatalf("Swept = %d, want 3 (pids that no longer exist still count as swept)", res.Swept)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want none — every directory here is pool-named", res.Skipped)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("%d cgroup directories left behind: %v", len(entries), entries)
	}
}

// TestSweepOrphansSparesTheWorkersOwnCgroupAndStillKillsAVMOrphan is the regression
// test for final-review H1, which was production-fatal: the sweep walked EVERY
// immediate subdirectory of the slice and SIGKILLed every pid in each, while
// deploy/microvm/microvm-worker.service's `Slice=microvm-vms.slice` makes systemd nest
// the worker's own unit cgroup — microvm-vms.slice/microvm-worker.service — as one of
// those subdirectories. With Restart=on-failure/RestartSec=5s the worker therefore
// SIGKILLed itself every five seconds forever and the tier never reached Probe.
//
// The layout below is exactly what systemd creates (systemd.slice(5): a service
// assigned to a slice is placed beneath it in the cgroup tree — the same shape as
// system.slice/sshd.service), with real `sleep 300` children standing in for the worker
// and for a leaked VMM.
//
// BOTH halves are asserted in ONE test on purpose. "The worker's pid survives" is
// worthless alone: it also passes a sweep that does nothing at all, which would
// reintroduce spec §6's #1 practical failure (a crash-restart loop leaking VMs at the
// crash rate). So the same call must be shown to still kill the VM-shaped orphan. That
// pairing is the same rule the earlier fake-pid vacuity fix established here.
func TestSweepOrphansSparesTheWorkersOwnCgroupAndStillKillsAVMOrphan(t *testing.T) {
	workerPid, workerReaped := startSleeper(t, false) // shares this process's group, as the worker does
	vmPid, vmReaped := startSleeper(t, true)          // Setpgid'd, as both launchers do to their VMM

	root := fakeSlice(t, map[string][]string{
		// systemd's own child of the slice: the worker doing the sweeping.
		"microvm-worker.service": {strconv.Itoa(workerPid)},
		// The Firecracker arm's orphan: jailer --id vm-9 --parent-cgroup <slice>
		// creates <slice>/vm-9 (firecracker docs/jailer.md: with --cgroup supplied,
		// "the jailer will create a new cgroup named <id> for the microvm in the
		// <cgroup_base>/<parent_cgroup> subfolder").
		"vm-9": {strconv.Itoa(vmPid)},
	})

	res, err := SweepOrphans(root)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}

	// Half 1 (the presence — proves the sweep still fires at all): the VM-shaped
	// orphan's real process is dead and its directory is gone.
	select {
	case <-vmReaped:
	case <-time.After(5 * time.Second):
		t.Fatal("the VM-shaped orphan (vm-9) survived the sweep — the fail-closed filter has narrowed to nothing, which is the orphan leak spec §6 lists as its first practical failure mode")
	}
	if _, err := os.Stat(filepath.Join(root, "vm-9")); !os.IsNotExist(err) {
		t.Fatalf("vm-9's cgroup directory was not removed: stat err = %v", err)
	}

	// Half 2 (the absence): the worker's own cgroup was left strictly alone. Asserted
	// AFTER half 1 and BEFORE any bookkeeping check, so a regression reports the
	// self-kill itself rather than a count that merely implies it.
	select {
	case <-workerReaped:
		t.Fatalf("SweepOrphans SIGKILLed the stand-in worker process (pid %d) — it swept microvm-worker.service's own cgroup, which systemd nests under Slice=microvm-vms.slice (final review H1)", workerPid)
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(root, "microvm-worker.service")); err != nil {
		t.Fatalf("microvm-worker.service's cgroup directory was removed or damaged: %v", err)
	}

	// And the bookkeeping an operator reads: exactly one swept, exactly one declined BY
	// NAME. A refusal nobody can see is indistinguishable from a sweep that found nothing.
	if res.Swept != 1 {
		t.Errorf("Swept = %d, want 1 (vm-9 only)", res.Swept)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "microvm-worker.service" {
		t.Errorf("Skipped = %v, want exactly [microvm-worker.service]", res.Skipped)
	}
}

// TestSweepOrphansSkipsEveryDirectoryTheKernelPutsInTheSliceItselfDidNotName is the
// fail-closed half stated positively: the filter is an ALLOWLIST of names this pool
// creates, not a denylist of names it happens to know about today. A directory nobody
// here named is left alone AND reported, never signalled hopefully.
func TestSweepOrphansSkipsEveryDirectoryTheKernelPutsInTheSliceItselfDidNotName(t *testing.T) {
	// Isolated (true) so that if any of these names were wrongly accepted, guard 3 would
	// NOT quietly cover for it: only the name filter stands between this pid and SIGKILL.
	pid, reaped := startSleeper(t, true)
	root := fakeSlice(t, map[string][]string{
		"microvm-worker.service": {strconv.Itoa(pid)},
		"some-other.service":     {strconv.Itoa(pid)},
		"init.scope":             {strconv.Itoa(pid)},
		"nested.slice":           {strconv.Itoa(pid)},
		"vm-":                    {strconv.Itoa(pid)}, // prefix alone is not an id
		"vm-abc":                 {strconv.Itoa(pid)}, // ids are decimal (nextIDLocked)
		"vm-1.service":           {strconv.Itoa(pid)}, // a service is never a VM
	})

	res, err := SweepOrphans(root)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if res.Swept != 0 {
		t.Fatalf("Swept = %d, want 0 — none of these names is one this pool creates", res.Swept)
	}
	if len(res.Skipped) != 7 {
		t.Fatalf("Skipped = %v, want all 7 reported", res.Skipped)
	}
	select {
	case <-reaped:
		t.Fatal("a pid listed only in directories the pool never named was SIGKILLed")
	case <-time.After(500 * time.Millisecond):
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 7 {
		t.Fatalf("%d directories left, want all 7 untouched", len(entries))
	}
}

// TestSweepOrphansRecognisesBothArmsCgroupNames pins the sweep's allowlist against the
// names the pool's OWN code actually produces, rather than against literals retyped
// into the test. Spec §5.3's "two numbers that can drift is the bug", applied to a
// name: if nextIDLocked's id shape or the CH arm's scope-unit name changes and the
// filter is not updated with it, the filter silently narrows to nothing and the sweep
// becomes a no-op — a failure with no symptom until a crash leaks VMs.
func TestSweepOrphansRecognisesBothArmsCgroupNames(t *testing.T) {
	p := &pool{}
	id := p.nextIDLocked() // the real generator, not "vm-1" retyped

	// Firecracker: jailer --id <id> --parent-cgroup <slice> --cgroup memory.max=N.
	if !isPoolVMCgroupDirName(id) {
		t.Errorf("isPoolVMCgroupDirName(%q) = false — the Firecracker arm's own cgroup name is not recognised, so its orphans would never be swept", id)
	}
	// Cloud Hypervisor: systemd-run --scope --unit=<chvScopeUnitName(id)> --slice=…,
	// which systemd materialises as "<unit>.scope" under the slice.
	scopeDir := chvScopeUnitName(id) + chvScopeDirSuffix
	if !isPoolVMCgroupDirName(scopeDir) {
		t.Errorf("isPoolVMCgroupDirName(%q) = false — the Cloud Hypervisor arm's own scope cgroup name is not recognised", scopeDir)
	}
	// And the worker's own unit, whatever it is called, is not one of them.
	if isPoolVMCgroupDirName("microvm-worker.service") {
		t.Error("isPoolVMCgroupDirName accepted a .service directory")
	}
}

// TestParseSelfCgroupV2 drives guard 2's parse on darwin. The file it normally reads,
// /proc/self/cgroup, exists only on Linux — and a parse exercised only on the deployment
// platform is a parse nothing checks, which is exactly how final-review H2 shipped a
// Linux-only expression that was wrong on every Linux host. The inputs below are real
// /proc/self/cgroup shapes, including the systemd one this worker actually runs under.
func TestParseSelfCgroupV2(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"systemd-managed service (the shipped shape)", "0::/microvm-vms.slice/microvm-worker.service\n", "/microvm-vms.slice/microvm-worker.service"},
		{"a transient scope", "0::/microvm-vms.slice/vm-vm-9.scope\n", "/microvm-vms.slice/vm-vm-9.scope"},
		{"root cgroup is not a directory worth excluding", "0::/\n", ""},
		{"hybrid host: the v2 line is picked out of the v1 ones",
			"12:pids:/user.slice\n1:name=systemd:/user.slice/session-3.scope\n0::/user.slice/session-3.scope\n",
			"/user.slice/session-3.scope"},
		{"cgroup v1 only: no unified line at all", "12:pids:/user.slice\n1:name=systemd:/init.scope\n", ""},
		{"empty (what a non-Linux read yields before this is even called)", "", ""},
	} {
		if got := parseSelfCgroupV2(tc.in); got != tc.want {
			t.Errorf("%s: parseSelfCgroupV2(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestIsCallersOwnCgroupMatchesAcrossTheMountAnchorDifference covers guard 2's comparison.
// The two paths it compares are anchored differently on purpose — /proc/self/cgroup's path
// is relative to the cgroup2 mount, while the sweep walks filesystem paths — and what is
// asserted here is that the suffix match bridges that with no false NEGATIVE for the case
// that matters, and that its only failure direction is the safe one (refusing to sweep).
func TestIsCallersOwnCgroupMatchesAcrossTheMountAnchorDifference(t *testing.T) {
	self := "/microvm-vms.slice/microvm-worker.service"

	// The case H1 was: the worker's own cgroup, met as a filesystem path under the slice.
	if !isCallersOwnCgroup("/sys/fs/cgroup/microvm-vms.slice/microvm-worker.service", self) {
		t.Error("guard 2 did not recognise the caller's own cgroup as a cgroupfs path")
	}
	// A sibling VM cgroup in the same slice must NOT match, or the sweep would refuse
	// every orphan and silently stop working.
	for _, sibling := range []string{
		"/sys/fs/cgroup/microvm-vms.slice/vm-9",
		"/sys/fs/cgroup/microvm-vms.slice/vm-vm-9.scope",
	} {
		if isCallersOwnCgroup(sibling, self) {
			t.Errorf("guard 2 refused %q, a sibling VM cgroup — the sweep would never remove an orphan", sibling)
		}
	}
	// An ancestor of the caller's cgroup is refused too (a misconfigured SH_PARENT_CGROUP
	// pointing above the slice).
	if !isCallersOwnCgroup("/sys/fs/cgroup/microvm-vms.slice", self) {
		t.Error("guard 2 did not refuse an ancestor of the caller's own cgroup")
	}
	// And with no self path — every non-Linux platform, and a v1-only host — the guard is
	// inert rather than refusing everything. Guards 1 and 3 stand alone there.
	if isCallersOwnCgroup("/sys/fs/cgroup/microvm-vms.slice/vm-9", "") {
		t.Error("guard 2 refused a directory with no known self cgroup; it must be inert, not absolute")
	}
}

// TestSweepOrphansNeverSignalsTheCallerItself is the innermost of the three guards
// (name allowlist, own-cgroup exclusion, pid refusal). Even if a directory filter were
// wrong again, the pid layer must refuse the calling process, its process-group leader,
// and the kill(2) wildcards 0 (every process in the caller's own group) and -1 (every
// process the caller may signal) — any of which turns one sweep into a self-kill.
func TestSweepOrphansNeverSignalsTheCallerItself(t *testing.T) {
	for _, pid := range []int{os.Getpid(), 0, -1} {
		if reason, unsafe := unsafeToSignal(pid); !unsafe {
			t.Errorf("unsafeToSignal(%d) = false — SweepOrphans would signal it", pid)
		} else if reason == "" {
			t.Errorf("unsafeToSignal(%d) refused with no reason; an unexplained refusal is unreviewable", pid)
		}
	}
	// Non-vacuousness: the guard must NOT refuse a pid shaped like a real orphan (its own
	// process group, as both launchers arrange), or it would refuse every orphan too and
	// the sweep would silently stop working.
	pid, _ := startSleeper(t, true)
	if _, unsafe := unsafeToSignal(pid); unsafe {
		t.Fatalf("unsafeToSignal(%d) refused a real, Setpgid'd child pid — the guard has widened to refuse the very orphans it exists to let through", pid)
	}
	// And the complement, so the process-group rule is shown to be live rather than
	// dead code: a child that SHARES this process's group is refused.
	shared, _ := startSleeper(t, false)
	if _, unsafe := unsafeToSignal(shared); !unsafe {
		t.Fatalf("unsafeToSignal(%d) accepted a pid in the caller's own process group", shared)
	}

	// And the whole sweep must refuse a cgroup that holds the caller's own pid, whatever
	// the directory is called: a pool-named directory is not a licence to kill us.
	root := fakeSlice(t, map[string][]string{"vm-4": {strconv.Itoa(os.Getpid())}})
	res, err := SweepOrphans(root)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if res.Swept != 0 || len(res.Skipped) != 1 {
		t.Fatalf("Swept=%d Skipped=%v — a cgroup containing the caller's own pid must be skipped, not swept", res.Swept, res.Skipped)
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
	// The directory name must be one the pool itself creates ("vm-" + a decimal
	// sequence number, per nextIDLocked): since final-review H1, SweepOrphans is
	// fail-closed and skips anything else, so a made-up name like "vm-real" would make
	// this test assert nothing.
	pid, waitDone := startSleeper(t, true)

	root := fakeSlice(t, map[string][]string{
		"vm-11": {strconv.Itoa(pid)},
	})

	res, err := SweepOrphans(root)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if res.Swept != 1 {
		t.Fatalf("Swept = %d, want 1", res.Swept)
	}

	select {
	case <-waitDone:
		// Good: the kernel actually reaped the process, i.e. SweepOrphans really
		// signalled it — not merely removed a cgroup directory around it.
	case <-time.After(5 * time.Second):
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
