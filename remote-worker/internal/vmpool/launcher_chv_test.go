package vmpool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func chvOpts(t *testing.T) CHVOptions {
	t.Helper()
	snapshotDir := envOr("SH_SNAPSHOT_IMAGE_DIR", t.TempDir())
	return CHVOptions{
		SnapshotDir:  snapshotDir,
		CHVBin:       envOr("SH_CHV_BIN", "/usr/bin/cloud-hypervisor"),
		ChRemoteBin:  envOr("SH_CH_REMOTE_BIN", "/usr/bin/ch-remote"),
		VirtiofsdBin: envOr("SH_VIRTIOFSD_BIN", "/usr/libexec/virtiofsd"),
		// RunDir must share a device with snapshotDir: Restore() hardlinks the golden
		// vmstate/memory-ranges files (os.Link) from SnapshotDir into RunDir/<id>/, and a
		// hardlink across devices is EXDEV, unconditionally -- the same failure mode
		// fcLauncher's ChrootBase had (see sameDeviceSiblingDir in
		// launcher_firecracker_test.go, which this reuses). When SH_SNAPSHOT_IMAGE_DIR is
		// unset, snapshotDir is itself a t.TempDir(), so the plain default below already
		// lands beside it on the same device; when it is set to a rig path like
		// /srv/snapshots/..., this is what keeps RunDir off of tmpfs.
		RunDir:       sameDeviceSiblingDir(t, snapshotDir),
		VirtiofsdUID: 65534, // nobody
		VirtiofsdGID: 65534,
		VsockPort:    1024,
	}
}

func TestCloudHypervisorDoesNotSerializeExecsPerRun(t *testing.T) {
	lc, err := NewCloudHypervisorLauncher(chvOpts(t))
	if err != nil {
		t.Fatalf("NewCloudHypervisorLauncher: %v", err)
	}
	if lc.Kind() != CloudHypervisor {
		t.Fatalf("Kind = %q", lc.Kind())
	}
	// Spec §4.3's decisive row: with virtio-fs the host filesystem arbitrates, so D>1
	// standbys and concurrent Execs per run are both fine. This is the whole reason the
	// VMM is a seam rather than a build choice.
	if lc.SerializesExecsPerRun() {
		t.Fatal("the Cloud Hypervisor arm must NOT serialize Execs per run")
	}
}

func TestVirtiofsdIsNeverRunAsRoot(t *testing.T) {
	opts := chvOpts(t)
	opts.VirtiofsdUID, opts.VirtiofsdGID = 0, 0
	// Spec §3.5: with virtio-fs, guest path resolution happens in virtiofsd on the HOST,
	// so it is the confinement boundary for the whole design — a root virtiofsd
	// compromise would be host root and would render the microVM boundary decorative.
	// Refusing at construction is the only place this can be enforced once and for all.
	if _, err := NewCloudHypervisorLauncher(opts); err == nil {
		t.Fatal("NewCloudHypervisorLauncher accepted VirtiofsdUID=0")
	}
}

func TestVirtiofsdArgvCarriesItsSandbox(t *testing.T) {
	// A pure argv assertion: --sandbox=namespace is virtiofsd's own confinement, and
	// spec §6's malicious-symlink row says it must be "configured and verified, never
	// assumed".
	argv := virtiofsdArgv(chvOpts(t), "/run/vfsd-1.sock", "/srv/workspaces/run-a")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--socket-path=/run/vfsd-1.sock", "--shared-dir=/srv/workspaces/run-a", "--sandbox=namespace", "--cache=never"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q is missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "--sandbox=none") {
		t.Error("--sandbox=none disables the boundary the whole design leans on")
	}
	// Corrections C1: --cache=auto disconnects the virtio-fs session immediately on the
	// installed virtiofsd build; --cache=never is the one that stays up. Guard against a
	// regression back to the brief's original (wrong) default.
	if strings.Contains(joined, "--cache=auto") {
		t.Error("--cache=auto is a known dead end (disconnects immediately) — must not appear in argv")
	}
	// Round 5: the inverse of the guard above — the brief DEMANDED
	// --inode-file-handles=mandatory, and this asserts it is ABSENT. That inversion is
	// deliberate, not a mistake: mandatory file handles require CAP_DAC_READ_SEARCH to
	// open a file handle for the shared directory's root node, which the unprivileged
	// VirtiofsdUID/GID this design requires (enforced by TestVirtiofsdIsNeverRunAsRoot
	// above, per spec §3.5) does not have. Confirmed on real hardware: at uid 65534
	// with --inode-file-handles=mandatory, virtiofsd logs "Failed to open file handle
	// for the root node: Operation not permitted (os error 1)" and exits before its
	// vhost-user socket ever appears; without the flag (or at root, which this design
	// forbids), it starts fine. If a future reader restores this flag "per the
	// brief", they will silently break the CH arm again in exactly this way — this
	// assertion exists so that regression fails loudly in CI instead of quietly on
	// the rig.
	if strings.Contains(joined, "--inode-file-handles=mandatory") {
		t.Error("--inode-file-handles=mandatory requires CAP_DAC_READ_SEARCH, which the unprivileged VirtiofsdUID this design requires (spec §3.5) does not have — confirmed on real hardware (\"Operation not permitted\" opening a file handle for the root node); must not reappear in argv")
	}
}

// TestChvRestoreConfigArgIsPositionalNotAFlag is a pure argv-shape assertion for
// the round-6 fix: ch-remote's restore subcommand takes exactly one required
// POSITIONAL argument (a comma-separated key=value string), not a "--source-url"
// flag. Confirmed against the real ch-remote CLI source
// (cloud-hypervisor/src/bin/ch-remote.rs) and `ch-remote restore --help` on the
// installed v53.0 — see chvRestoreConfigArg's doc comment. This regressed once
// already (exit status 2: "unexpected argument '--source-url' found"); this test
// exists so that regression fails loudly here instead of quietly on the rig.
func TestChvRestoreConfigArgIsPositionalNotAFlag(t *testing.T) {
	arg := chvRestoreConfigArg("/run/vm-a")
	if strings.Contains(arg, "--source-url") {
		t.Errorf("restore_config %q must not contain a --source-url flag — restore_config is a single positional comma-separated key=value string, not a set of flags", arg)
	}
	if !strings.Contains(arg, "source_url=") {
		t.Errorf("restore_config %q is missing source_url=", arg)
	}
	if !strings.Contains(arg, "file:///run/vm-a") {
		t.Errorf("restore_config %q does not carry the run dir as a file:// URL", arg)
	}
	// Standbys are restored PAUSED (spec §3.2: a paused VM costs zero CPU, which is
	// what lets many standbys exist without burning cores on timer ticks). resume
	// must be passed explicitly rather than left to ch-remote's default, so a future
	// upstream default change can't silently turn every restored standby running.
	if !strings.Contains(arg, "resume=false") {
		t.Errorf("restore_config %q must explicitly carry resume=false — standbys are restored paused, and this must not rely on ch-remote's default (spec §3.2)", arg)
	}
	// The whole config must be ONE comma-separated positional string, not multiple
	// space-separated tokens masquerading as one (which would silently turn back into
	// flag-like argv splitting at the exec.Command call site).
	if strings.Contains(arg, " ") {
		t.Errorf("restore_config %q contains a space — must be one comma-separated token, not multiple argv entries", arg)
	}

	// Also assert the actual exec.CommandContext argv this produces, at the call
	// site's own construction shape: "restore" is followed by exactly ONE arg, and
	// that arg is the positional config string above — never split across
	// "--source-url", "<url>" as two separate argv entries the way the pre-round-6
	// code did.
	argv := []string{"--api-socket", "/run/api.sock", "restore", chvRestoreConfigArg("/run/vm-a")}
	for i, a := range argv {
		if a == "restore" {
			if i != len(argv)-2 {
				t.Fatalf("argv %q: \"restore\" must be followed by exactly one argument", argv)
			}
			if strings.HasPrefix(argv[i+1], "--") {
				t.Errorf("argv %q: the argument after \"restore\" must be the positional restore_config string, not a flag", argv)
			}
		}
	}
}

// TestCloudHypervisorParentCgroupRequiresMemoryMax is validate()'s mirror of
// launcher_firecracker.go's identical check: a ParentCgroup with no memory bound
// would leave systemd-run --scope creating a per-VM cgroup with no memory.max at
// all, which is this arm's version of D1's "half-wired state".
func TestCloudHypervisorParentCgroupRequiresMemoryMax(t *testing.T) {
	opts := chvOpts(t)
	opts.ParentCgroup = "/sys/fs/cgroup/microvm-vms.slice"
	opts.CgroupMemoryMaxBytes = 0
	if _, err := NewCloudHypervisorLauncher(opts); err == nil {
		t.Fatal("NewCloudHypervisorLauncher accepted ParentCgroup with CgroupMemoryMaxBytes <= 0")
	}
}

// TestCloudHypervisorSystemdRunScopeAgreesWithPerVMBytes is
// TestFirecrackerJailerCgroupMemoryMaxAgreesWithPerVMBytes's mirror for this arm
// (D1/D3): the -p MemoryMax= value systemd-run --scope is told to set must equal
// vmpool.PerVMBytes(cfg), not a second, independently maintained constant.
func TestCloudHypervisorSystemdRunScopeAgreesWithPerVMBytes(t *testing.T) {
	cfg := Config{
		GuestRAMBytes:   256 << 20,
		VMOverheadBytes: DefaultVMOverheadBytes,
	}
	want := PerVMBytes(cfg)

	opts := chvOpts(t)
	opts.ParentCgroup = "/sys/fs/cgroup/microvm-vms.slice"
	opts.CgroupMemoryMaxBytes = want
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	args := chvSystemdRunScopeArgv(opts, "vm-7")
	joined := strings.Join(args, " ")
	wantFlag := "MemoryMax=" + strconv.FormatInt(want, 10)
	if !strings.Contains(joined, wantFlag) {
		t.Fatalf("systemd-run args %q do not contain %q — the cgroup bound has drifted from PerVMBytes(cfg) = %d", joined, wantFlag, want)
	}
	if !strings.Contains(joined, "--slice=microvm-vms.slice") {
		t.Fatalf("systemd-run args %q do not target the microvm-vms.slice parent (spec §5.3: both arms must agree on the same slice)", joined)
	}
}

// TestChvCgroupSliceNameStripsTheCgroupfsPrefix guards the ParentCgroup ->
// --slice translation: systemd-run --slice wants a bare unit name
// ("microvm-vms.slice"), not the full cgroupfs path
// ("/sys/fs/cgroup/microvm-vms.slice") FirecrackerOptions.ParentCgroup and this
// arm's own ParentCgroup share.
func TestChvCgroupSliceNameStripsTheCgroupfsPrefix(t *testing.T) {
	got := chvCgroupSliceName("/sys/fs/cgroup/microvm-vms.slice")
	if got != "microvm-vms.slice" {
		t.Fatalf("chvCgroupSliceName = %q, want %q", got, "microvm-vms.slice")
	}
}

// TestRestorePreparesVirtiofsdOwnership covers fix round 1, item 1: Restore
// must chown both paths virtiofsd needs (the run dir it binds its own socket
// inside, and the workspace it must traverse into and serve) to the uid/gid
// it drops to via SysProcAttr.Credential — otherwise the unprivileged posture
// CHVOptions.VirtiofsdUID/GID's doc comment requires cannot actually start
// (virtiofsd's own bind()/traversal hits EACCES the instant it runs).
//
// This cannot be tested against the real os.Chown without root (a non-root
// test runner has neither CAP_CHOWN nor, generally, ownership of a freshly
// created t.TempDir() under another uid) — stated explicitly, per this
// round's own instruction, rather than adding an assertion that cannot fail.
// What IS tested here, by substituting the indirected chvChown, is that
// Restore's ownership-preparation step calls chown for exactly the run dir
// and the workspace dir, with the configured (non-zero) uid/gid — not uid/gid
// 0, and not skipped.
func TestRestorePreparesVirtiofsdOwnership(t *testing.T) {
	orig := chvChown
	defer func() { chvChown = orig }()

	type call struct {
		path     string
		uid, gid int
	}
	var calls []call
	chvChown = func(path string, uid, gid int) error {
		calls = append(calls, call{path, uid, gid})
		return nil
	}

	opts := chvOpts(t)
	runDir := t.TempDir()
	workspaceDir := t.TempDir()
	if err := chvPrepareVirtiofsdOwnership(runDir, workspaceDir, opts.VirtiofsdUID, opts.VirtiofsdGID); err != nil {
		t.Fatalf("chvPrepareVirtiofsdOwnership: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("chvChown called %d times, want 2: %+v", len(calls), calls)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.path] = true
		if c.uid == 0 || c.gid == 0 {
			t.Errorf("chown %q to %d:%d — must never chown to root", c.path, c.uid, c.gid)
		}
		if c.uid != opts.VirtiofsdUID || c.gid != opts.VirtiofsdGID {
			t.Errorf("chown %q to %d:%d, want %d:%d", c.path, c.uid, c.gid, opts.VirtiofsdUID, opts.VirtiofsdGID)
		}
	}
	if !seen[runDir] {
		t.Errorf("runDir %q was never chowned; got calls %+v", runDir, calls)
	}
	if !seen[workspaceDir] {
		t.Errorf("workspaceDir %q was never chowned; got calls %+v", workspaceDir, calls)
	}
}

// TestRestorePreparesVirtiofsdOwnershipPropagatesFailure covers the error
// path: if the underlying chown fails (e.g. the real EACCES/EPERM a non-root
// launcher process would hit trying to chown a path it does not own), that
// failure must propagate rather than being swallowed — a silently-skipped
// chown would reintroduce exactly the bug this fix closes.
func TestRestorePreparesVirtiofsdOwnershipPropagatesFailure(t *testing.T) {
	orig := chvChown
	defer func() { chvChown = orig }()
	chvChown = func(path string, uid, gid int) error {
		return os.ErrPermission
	}
	opts := chvOpts(t)
	err := chvPrepareVirtiofsdOwnership(t.TempDir(), t.TempDir(), opts.VirtiofsdUID, opts.VirtiofsdGID)
	if err == nil {
		t.Fatal("chvPrepareVirtiofsdOwnership swallowed a chown failure")
	}
}

// TestRestoreCallsPrepareOwnership covers fix round 2: the two tests above
// call chvPrepareVirtiofsdOwnership directly and never exercise Restore, so
// they cannot tell whether Restore actually calls it, or with which uid/gid.
// A mutation that changed Restore's call site to chown to 0:0 (undoing the
// whole round-1 fix) or removed the call entirely still passed every
// existing test — nothing was watching the call site inside Restore itself.
//
// This test drives Restore and observes that call site via chvPrepareOwnership
// (the indirected var), substituting a recorder that returns an error. That
// error makes Restore abort right there — before any process spawn, so no
// binaries and no KVM are needed — and lets this test assert against the
// exact arguments Restore passed, and against Restore's own return-value
// invariant (non-nil error, nil VM) on that path.
func TestRestoreCallsPrepareOwnership(t *testing.T) {
	orig := chvPrepareOwnership
	defer func() { chvPrepareOwnership = orig }()

	type call struct {
		runDir, workspaceDir string
		uid, gid             int
	}
	var got []call
	sentinel := errors.New("sentinel: ownership prep refused")
	chvPrepareOwnership = func(runDir, workspaceDir string, uid, gid int) error {
		got = append(got, call{runDir, workspaceDir, uid, gid})
		return sentinel
	}

	opts := chvOpts(t)
	lc, err := NewCloudHypervisorLauncher(opts)
	if err != nil {
		t.Fatalf("NewCloudHypervisorLauncher: %v", err)
	}
	workspaceDir := t.TempDir()
	vm, err := lc.Restore(context.Background(), RestoreRequest{
		ID: "vm-chv-ownership", Key: "run-a", WorkspaceDir: workspaceDir, GuestRAMBytes: 256 << 20,
	})

	// Restore's own invariant: never a non-nil VM alongside a non-nil error.
	if err == nil {
		t.Fatal("Restore returned nil error despite chvPrepareOwnership failing")
	}
	if vm != nil {
		t.Fatalf("Restore returned a non-nil VM alongside an error: %v", vm)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Restore's error does not wrap the sentinel: %v", err)
	}

	// Kills "removed the call entirely" (mutation 3): if Restore never calls
	// chvPrepareOwnership, got stays empty and this fails.
	if len(got) != 1 {
		t.Fatalf("chvPrepareOwnership called %d times via Restore, want 1: %+v", len(got), got)
	}
	c := got[0]

	// Kills "chown to 0:0" (mutation 2): asserting against opts.VirtiofsdUID/GID
	// alone would be self-referential if Restore hardcoded some OTHER non-zero
	// value, so also assert directly against zero.
	if c.uid == 0 || c.gid == 0 {
		t.Fatalf("Restore called chvPrepareOwnership with uid:gid %d:%d — must never be root", c.uid, c.gid)
	}
	if c.uid != opts.VirtiofsdUID || c.gid != opts.VirtiofsdGID {
		t.Fatalf("Restore called chvPrepareOwnership with %d:%d, want configured %d:%d", c.uid, c.gid, opts.VirtiofsdUID, opts.VirtiofsdGID)
	}

	// The paths must be the run dir Restore itself created for this VM (a
	// per-VM child of opts.RunDir keyed by req.ID) and req.WorkspaceDir, not
	// something else entirely — e.g. opts.RunDir itself, or opts.SnapshotDir.
	wantRunDir := filepath.Join(opts.RunDir, "vm-chv-ownership")
	if c.runDir != wantRunDir {
		t.Fatalf("runDir passed to chvPrepareOwnership = %q, want %q", c.runDir, wantRunDir)
	}
	if c.workspaceDir != workspaceDir {
		t.Fatalf("workspaceDir passed to chvPrepareOwnership = %q, want %q", c.workspaceDir, workspaceDir)
	}
}

// TestRestoreChecksWorkspaceReachableBeforeVirtiofsd covers fix round 3, item
// 2's wiring: Restore must call the reachability check (via the indirected
// chvCheckWorkspaceReachable) with req.WorkspaceDir and the configured
// VirtiofsdUID/GID, and must abort — before any process spawn, so no binaries
// and no KVM are needed — if that check fails. Mirrors
// TestRestoreCallsPrepareOwnership's structure exactly, for the same reason:
// without this, a mutation that deleted the call site, or passed the wrong
// path/uid/gid, would pass every other test in this file.
func TestRestoreChecksWorkspaceReachableBeforeVirtiofsd(t *testing.T) {
	origCheck := chvCheckWorkspaceReachable
	defer func() { chvCheckWorkspaceReachable = origCheck }()

	// chvPrepareOwnership runs just before this check and, for real, calls
	// os.Chown(..., VirtiofsdUID, VirtiofsdGID) — as a non-root test runner
	// that chown itself fails with "operation not permitted" before Restore
	// ever reaches the code under test here (see
	// TestRestorePreparesVirtiofsdOwnershipPropagatesFailure's own doc comment
	// for the identical, already-documented limitation). Stub it out so this
	// test exercises only the reachability-check wiring, not real chown.
	origPrepare := chvPrepareOwnership
	defer func() { chvPrepareOwnership = origPrepare }()
	chvPrepareOwnership = func(runDir, workspaceDir string, uid, gid int) error { return nil }

	type call struct {
		path     string
		uid, gid uint32
	}
	var got []call
	sentinel := errors.New("sentinel: workspace unreachable")
	chvCheckWorkspaceReachable = func(path string, uid, gid uint32) error {
		got = append(got, call{path, uid, gid})
		return sentinel
	}

	opts := chvOpts(t)
	lc, err := NewCloudHypervisorLauncher(opts)
	if err != nil {
		t.Fatalf("NewCloudHypervisorLauncher: %v", err)
	}
	workspaceDir := t.TempDir()
	vm, err := lc.Restore(context.Background(), RestoreRequest{
		ID: "vm-chv-reachable", Key: "run-a", WorkspaceDir: workspaceDir, GuestRAMBytes: 256 << 20,
	})

	// Restore's own invariant: never a non-nil VM alongside a non-nil error.
	if err == nil {
		t.Fatal("Restore returned nil error despite chvCheckWorkspaceReachable failing")
	}
	if vm != nil {
		t.Fatalf("Restore returned a non-nil VM alongside an error: %v", vm)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Restore's error does not wrap the sentinel: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("chvCheckWorkspaceReachable called %d times via Restore, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.path != workspaceDir {
		t.Fatalf("chvCheckWorkspaceReachable called with path %q, want %q", c.path, workspaceDir)
	}
	if c.uid != uint32(opts.VirtiofsdUID) || c.gid != uint32(opts.VirtiofsdGID) {
		t.Fatalf("chvCheckWorkspaceReachable called with %d:%d, want configured %d:%d",
			c.uid, c.gid, opts.VirtiofsdUID, opts.VirtiofsdGID)
	}
}

// TestRestoreFailsWhenWorkspaceIsUnreachable is fix round 3, item 2's
// mutation test run for real, through the actual Restore entry point rather
// than the seam above: a genuine 0700 ancestor above workspaceDir, owned by
// this test's own uid (never chvOpts's VirtiofsdUID 65534), reproduces exactly
// the rig's failure shape without needing virtiofsd, cloud-hypervisor, or
// root — the check runs and fails before any binary is exec'd. Asserts the
// resulting error names the blocking ancestor, so a reader gets the
// coordinator's ask ("which path, which uid, and which ancestor's mode is
// blocking") instead of virtiofsd's own misleading "does not exist".
func TestRestoreFailsWhenWorkspaceIsUnreachable(t *testing.T) {
	// chvPrepareOwnership runs just before the reachability check under test
	// and, for real, calls os.Chown(..., VirtiofsdUID, VirtiofsdGID) — as a
	// non-root test runner that chown itself fails with "operation not
	// permitted" before Restore ever reaches the check this test targets (see
	// TestRestorePreparesVirtiofsdOwnershipPropagatesFailure's own doc comment
	// for the identical, already-documented limitation). Stub it out so the
	// real chvCheckWorkspaceReachable (not faked here — this test exercises it
	// for real) is what actually fails Restore.
	origPrepare := chvPrepareOwnership
	defer func() { chvPrepareOwnership = origPrepare }()
	chvPrepareOwnership = func(runDir, workspaceDir string, uid, gid int) error { return nil }

	opts := chvOpts(t)
	lc, err := NewCloudHypervisorLauncher(opts)
	if err != nil {
		t.Fatalf("NewCloudHypervisorLauncher: %v", err)
	}

	blocker := filepath.Join(t.TempDir(), "blocker")
	workspaceDir := filepath.Join(blocker, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", workspaceDir, err)
	}
	if err := os.Chmod(blocker, 0o700); err != nil {
		t.Fatalf("chmod %s to 0700: %v", blocker, err)
	}

	vm, err := lc.Restore(context.Background(), RestoreRequest{
		ID: "vm-chv-unreachable", Key: "run-a", WorkspaceDir: workspaceDir, GuestRAMBytes: 256 << 20,
	})
	if err == nil {
		t.Fatal("Restore with an unreachable workspace: want error, got nil")
	}
	if vm != nil {
		t.Fatalf("Restore returned a non-nil VM alongside an error: %v", vm)
	}
	for _, want := range []string{blocker, "0700", "65534"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Restore error %q: missing %q", err.Error(), want)
		}
	}
}

func TestCloudHypervisorRestoresPausedAndRunsOneCommand(t *testing.T) {
	requireKVM(t)
	if _, err := exec.LookPath(chvOpts(t).CHVBin); err != nil {
		t.Skipf("cloud-hypervisor not installed: %v", err)
	}
	lc, err := NewCloudHypervisorLauncher(chvOpts(t))
	if err != nil {
		t.Fatalf("NewCloudHypervisorLauncher: %v", err)
	}
	dir := t.TempDir()
	vm, err := lc.Restore(context.Background(), RestoreRequest{
		ID: "vm-chv-1", Key: "run-a", WorkspaceDir: dir, GuestRAMBytes: 256 << 20,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	defer func() { _ = vm.Destroy() }()
	if err := vm.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	var out capturingSink
	if _, err := vm.Run(context.Background(), Command{Command: "echo hi > f; cat f", TimeoutS: 30, CapBytes: OutputCapBytes}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.out() != "hi\n" {
		t.Fatalf("stdout = %q", out.out())
	}
	// virtio-fs means the HOST filesystem is the authority, so the write is already
	// durable with no sync anywhere — spec §4.3's "nothing is lost" row, which is also
	// why the write-durability gate cannot fail on this arm for a missing sync.
	if b, err := os.ReadFile(dir + "/f"); err != nil || string(b) != "hi\n" {
		t.Fatalf("host-side file = %q err=%v", b, err)
	}
}

func TestCloudHypervisorSupportsTwoStandbysForOneRun(t *testing.T) {
	requireKVM(t)
	if _, err := exec.LookPath(chvOpts(t).CHVBin); err != nil {
		t.Skipf("cloud-hypervisor not installed: %v", err)
	}
	lc, _ := NewCloudHypervisorLauncher(chvOpts(t))
	dir := t.TempDir()
	// The row that decides the arm: D>1 pre-mounted standbys, which the Firecracker arm
	// cannot have at all (spec §4.3).
	var vms []VM
	for _, id := range []string{"vm-chv-a", "vm-chv-b"} {
		vm, err := lc.Restore(context.Background(), RestoreRequest{ID: id, Key: "run-a", WorkspaceDir: dir, GuestRAMBytes: 256 << 20})
		if err != nil {
			t.Fatalf("Restore %s: %v", id, err)
		}
		defer func() { _ = vm.Destroy() }()
		vms = append(vms, vm)
	}
	for i, vm := range vms {
		if err := vm.Resume(context.Background()); err != nil {
			t.Fatalf("Resume %d: %v", i, err)
		}
	}
	// Both resumed, both mounted, no corruption: run in each, concurrently.
	errs := make(chan error, len(vms))
	for i, vm := range vms {
		go func(i int, vm VM) {
			_, err := vm.Run(context.Background(), Command{
				Command: "echo " + string(rune('a'+i)) + " > f" + string(rune('a'+i)), TimeoutS: 30, CapBytes: OutputCapBytes,
			}, &capturingSink{})
			errs <- err
		}(i, vm)
	}
	for range vms {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Run: %v", err)
		}
	}
	for _, n := range []string{"fa", "fb"} {
		if _, err := os.Stat(dir + "/" + n); err != nil {
			t.Fatalf("%s missing: %v", n, err)
		}
	}
}

// TestRewriteSnapshotConfigGivesEachVMItsOwnSockets is not part of the brief's
// verbatim test list — it covers rewriteSnapshotConfig, a helper this task's own
// design added (see its doc comment in launcher_chv.go) to solve a problem neither
// the brief nor hardware-corrections spells out a mechanism for: config.json
// embeds its vsock (and virtio-fs) socket paths verbatim, so two standbys
// restored from the SAME golden config.json would otherwise collide binding the
// identical host-side Unix socket. This is the one piece of that design
// verifiable without KVM access — a pure JSON transform, run here against
// synthetic input shaped like the reference tutorial's documented config.json.
func TestRewriteSnapshotConfigGivesEachVMItsOwnSockets(t *testing.T) {
	golden := `{
		"vsock": {"cid": 3, "socket": "/golden/vsock.sock"},
		"fs": [{"tag": "workspace", "socket": "/golden/vfsd.sock", "num_queues": 1, "queue_size": 1024}],
		"disks": [{"path": "/golden/rootfs.ext4", "readonly": true}]
	}`
	out, err := rewriteSnapshotConfig([]byte(golden), "/run/vm-a/vsock.sock", "/run/vm-a/vfsd.sock")
	if err != nil {
		t.Fatalf("rewriteSnapshotConfig: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	vsock, _ := doc["vsock"].(map[string]any)
	if got := vsock["socket"]; got != "/run/vm-a/vsock.sock" {
		t.Fatalf("vsock.socket = %v, want /run/vm-a/vsock.sock", got)
	}
	fsList, _ := doc["fs"].([]any)
	if len(fsList) != 1 {
		t.Fatalf("fs list = %v", fsList)
	}
	fs, _ := fsList[0].(map[string]any)
	if got := fs["socket"]; got != "/run/vm-a/vfsd.sock" {
		t.Fatalf("fs[0].socket = %v, want /run/vm-a/vfsd.sock", got)
	}
	// The disk path/readonly flag must be untouched: C4/C8 already settled that
	// readonly=on (baked into the golden snapshot) is what makes the disk lock
	// shareable across standbys, so nothing about it should vary per VM.
	disks, _ := doc["disks"].([]any)
	disk, _ := disks[0].(map[string]any)
	if got := disk["path"]; got != "/golden/rootfs.ext4" {
		t.Fatalf("disks[0].path = %v, want unchanged /golden/rootfs.ext4", got)
	}
	if got, ok := disk["readonly"].(bool); !ok || !got {
		t.Fatalf("disks[0].readonly = %v, want unchanged true", disk["readonly"])
	}
}

// TestRewriteSnapshotConfigToleratesNoFsSection covers a config.json with no
// virtio-fs device at all (fsSocketPath == "") — rewriteSnapshotConfig must not
// fail or invent an "fs" key that was not there.
func TestRewriteSnapshotConfigToleratesNoFsSection(t *testing.T) {
	golden := `{"vsock": {"cid": 3, "socket": "/golden/vsock.sock"}}`
	out, err := rewriteSnapshotConfig([]byte(golden), "/run/vm-a/vsock.sock", "")
	if err != nil {
		t.Fatalf("rewriteSnapshotConfig: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if _, present := doc["fs"]; present {
		t.Fatalf("fs key should not have been invented: %v", doc["fs"])
	}
}

// TestRestoreStagesCHNativeNamesFromGoldenNames is the test the coordinator's
// round-4 ruling required: it exists because launcher_chv.go's Restore once read
// CH's own native names (config.json, memory-ranges, state.json) straight out of
// the golden SnapshotDir, but deploy/microvm/build-snapshot.sh's lock_down ships
// that same golden directory under the UNIFIED cross-VMM names (vmstate, memfile,
// ch-config.json) instead — so a real restore would have failed on a missing
// file, and nothing on this branch caught it because every prior test only
// asserted on the chvSnapshot*/chvGolden* constants, never on what actually landed
// on disk. This test populates a fake golden directory using the real golden
// names, runs the pure staging step, and inspects runDir directly: it must
// contain CH's native names (and only those — not the golden names) for
// vm.restore to have anything to replay.
func TestRestoreStagesCHNativeNamesFromGoldenNames(t *testing.T) {
	goldenDir := t.TempDir()
	runDir := t.TempDir()

	// Same synthetic shape as TestRewriteSnapshotConfigGivesEachVMItsOwnSockets,
	// reused here because chvStageSnapshotFiles' config path runs through the same
	// rewriteSnapshotConfig — the point of this test is the file-name translation
	// around it, not re-litigating the JSON rewrite itself.
	goldenConfig := `{
		"vsock": {"cid": 3, "socket": "/golden/vsock.sock"},
		"fs": [{"tag": "workspace", "socket": "/golden/vfsd.sock", "num_queues": 1, "queue_size": 1024}],
		"disks": [{"path": "/golden/rootfs.ext4", "readonly": true}]
	}`
	golden := map[string]string{
		chvGoldenVMState:    "fake vmstate bytes",
		chvGoldenMemFile:    "fake memfile bytes",
		chvGoldenConfigFile: goldenConfig,
	}
	for name, content := range golden {
		if err := os.WriteFile(filepath.Join(goldenDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("seed golden %s: %v", name, err)
		}
	}

	vsockSock := filepath.Join(runDir, "vsock.sock")
	fsSock := filepath.Join(runDir, "vfsd.sock")
	if err := chvStageSnapshotFiles(goldenDir, runDir, vsockSock, fsSock); err != nil {
		t.Fatalf("chvStageSnapshotFiles: %v", err)
	}

	// The decisive assertion: CH's native names must be PRESENT in runDir, by
	// filename, with the right content carried over from their golden counterpart —
	// not merely "the constants exist somewhere in the source".
	wantContent := map[string]string{
		chvSnapshotStateFile:    "fake vmstate bytes", // golden vmstate -> native state.json
		chvSnapshotMemoryRanges: "fake memfile bytes", // golden memfile -> native memory-ranges
	}
	for native, want := range wantContent {
		got, err := os.ReadFile(filepath.Join(runDir, native))
		if err != nil {
			t.Fatalf("runDir missing native file %q: %v", native, err)
		}
		if string(got) != want {
			t.Fatalf("runDir/%s content = %q, want %q", native, got, want)
		}
	}
	// config.json (native) must exist and be the REWRITTEN golden ch-config.json,
	// with this VM's own socket paths substituted in.
	cfg, err := os.ReadFile(filepath.Join(runDir, chvSnapshotConfigFile))
	if err != nil {
		t.Fatalf("runDir missing native %q: %v", chvSnapshotConfigFile, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(cfg, &doc); err != nil {
		t.Fatalf("runDir/%s is not valid JSON: %v", chvSnapshotConfigFile, err)
	}
	if vsock, _ := doc["vsock"].(map[string]any); vsock["socket"] != vsockSock {
		t.Fatalf("runDir/%s vsock.socket = %v, want %v", chvSnapshotConfigFile, vsock["socket"], vsockSock)
	}

	// And the golden names themselves must NOT be the names runDir exposes to CH —
	// this is what actually catches a reversed or mis-pointed mapping, since a
	// broken mapping that merely renamed golden->golden would otherwise slip past
	// the assertions above.
	for _, goldenName := range []string{chvGoldenVMState, chvGoldenMemFile, chvGoldenConfigFile} {
		if goldenName == chvSnapshotConfigFile || goldenName == chvSnapshotMemoryRanges || goldenName == chvSnapshotStateFile {
			continue // names happen to collide; nothing to check
		}
		if _, err := os.Stat(filepath.Join(runDir, goldenName)); err == nil {
			t.Fatalf("runDir unexpectedly has a file under the GOLDEN name %q; vm.restore needs CH's native names", goldenName)
		}
	}
}
