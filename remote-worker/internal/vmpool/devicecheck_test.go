package vmpool

import (
	"errors"
	"strings"
	"testing"
)

// withFakeDevices overrides deviceNumberFunc for the duration of the calling test,
// restoring the real deviceNumber on cleanup. by maps a path to a fake device
// number; a path not present in the map is a test bug (Fatal), not a "some other
// device" default, so a typo'd path shows up immediately rather than silently
// passing.
func withFakeDevices(t *testing.T, by map[string]uint64) {
	t.Helper()
	real := deviceNumberFunc
	t.Cleanup(func() { deviceNumberFunc = real })
	deviceNumberFunc = func(path string) (uint64, error) {
		d, ok := by[path]
		if !ok {
			t.Fatalf("withFakeDevices: unexpected path %q (not in the fake device map)", path)
		}
		return d, nil
	}
}

// TestCheckPathsShareDeviceAllowsMatch is checkPathsShareDevice's pass case: every
// path on the same fake device, no error. Exists as the counterpart to
// TestCheckPathsShareDeviceDetectsMismatch so a mutation that always returns an
// error (rather than one that only returns nil) would also be caught.
func TestCheckPathsShareDeviceAllowsMatch(t *testing.T) {
	withFakeDevices(t, map[string]uint64{"/a": 7, "/b": 7, "/c": 7})
	err := checkPathsShareDevice("why",
		namedPath{"A", "/a"}, namedPath{"B", "/b"}, namedPath{"C", "/c"})
	if err != nil {
		t.Fatalf("checkPathsShareDevice with matching devices: %v", err)
	}
}

// TestCheckPathsShareDeviceDetectsMismatch is Item 2's core mutation test, run
// through a fake deviceNumberFunc because this darwin dev machine has exactly one
// filesystem device (see device_unix.go's deviceNumber and
// TestSameDeviceSiblingDirSharesDeviceWithTarget) and so cannot produce a real
// cross-device pair of directories by path choice alone. The seam exercises the
// exact comparison and error-formatting logic checkPathsShareDevice runs in
// production; only the source of the device numbers is fake. Asserts the failure
// names every path AND its device number AND the why sentence, per the
// coordinator's ask.
func TestCheckPathsShareDeviceDetectsMismatch(t *testing.T) {
	withFakeDevices(t, map[string]uint64{"/snap": 1, "/work": 1, "/jail": 2})
	err := checkPathsShareDevice(
		"hardlinks: the snapshot components and the per-run workspace image",
		namedPath{"FirecrackerOptions.SnapshotDir", "/snap"},
		namedPath{"Config.WorkspaceRoot", "/work"},
		namedPath{"FirecrackerOptions.ChrootBase", "/jail"},
	)
	if err == nil {
		t.Fatal("checkPathsShareDevice with a mismatched device: want error, got nil")
	}
	for _, want := range []string{
		"FirecrackerOptions.SnapshotDir", "/snap", "device 1",
		"Config.WorkspaceRoot", "/work",
		"FirecrackerOptions.ChrootBase", "/jail", "device 2",
		"hardlinks: the snapshot components and the per-run workspace image",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("checkPathsShareDevice error %q: missing %q", err.Error(), want)
		}
	}
}

// TestFirecrackerCheckDeviceSharingNamesAllThreePaths exercises the real production
// entry point (firecrackerLauncher.checkDeviceSharing, called from pool.New) rather
// than the shared helper directly, with SnapshotDir, WorkspaceRoot and ChrootBase on
// three distinct fake devices — the coordinator's literal ask ("construct a config
// whose paths differ by device and assert the failure message names them"), done
// through the seam for the same single-device reason as
// TestCheckPathsShareDeviceDetectsMismatch above.
func TestFirecrackerCheckDeviceSharingNamesAllThreePaths(t *testing.T) {
	withFakeDevices(t, map[string]uint64{"/snap": 1, "/work": 2, "/jail": 3})
	lc := &firecrackerLauncher{opts: FirecrackerOptions{SnapshotDir: "/snap", ChrootBase: "/jail"}}
	err := lc.checkDeviceSharing(Config{WorkspaceRoot: "/work"})
	if err == nil {
		t.Fatal("checkDeviceSharing with three distinct devices: want error, got nil")
	}
	for _, want := range []string{
		"FirecrackerOptions.SnapshotDir", "/snap",
		"Config.WorkspaceRoot", "/work",
		"FirecrackerOptions.ChrootBase", "/jail",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("checkDeviceSharing error %q: missing %q", err.Error(), want)
		}
	}
}

// TestFirecrackerCheckDeviceSharingAllowsOneSharedDevice is the pass case for the
// same production entry point: all three on one fake device, no error — this is the
// state Item 1's gates_kvm_test.go fix (poolFor, TestGateLeakFreeTeardown) exists to
// reach on a real rig.
func TestFirecrackerCheckDeviceSharingAllowsOneSharedDevice(t *testing.T) {
	withFakeDevices(t, map[string]uint64{"/snap": 9, "/work": 9, "/jail": 9})
	lc := &firecrackerLauncher{opts: FirecrackerOptions{SnapshotDir: "/snap", ChrootBase: "/jail"}}
	if err := lc.checkDeviceSharing(Config{WorkspaceRoot: "/work"}); err != nil {
		t.Fatalf("checkDeviceSharing with a shared device: %v", err)
	}
}

// TestCHVCheckDeviceSharingIgnoresWorkspaceRoot pins the CHV-vs-Firecracker scope
// distinction documented on chvLauncher.checkDeviceSharing: WorkspaceRoot on a THIRD
// fake device must not fail this arm's check, because Cloud Hypervisor's Restore
// never hardlinks the workspace (virtiofsd shares it live) — only SnapshotDir and
// RunDir need to match. Deliberately the mirror image of
// TestFirecrackerCheckDeviceSharingNamesAllThreePaths: same three-device fixture,
// opposite expected outcome, because it exercises a different launcher.
func TestCHVCheckDeviceSharingIgnoresWorkspaceRoot(t *testing.T) {
	withFakeDevices(t, map[string]uint64{"/snap": 1, "/run": 1, "/work": 2})
	lc := &chvLauncher{opts: CHVOptions{SnapshotDir: "/snap", RunDir: "/run"}}
	if err := lc.checkDeviceSharing(Config{WorkspaceRoot: "/work"}); err != nil {
		t.Fatalf("CHV checkDeviceSharing must not consider WorkspaceRoot: %v", err)
	}
}

// TestCHVCheckDeviceSharingDetectsMismatch is the CHV analogue of
// TestFirecrackerCheckDeviceSharingNamesAllThreePaths: SnapshotDir and RunDir on
// distinct fake devices must fail, naming both.
func TestCHVCheckDeviceSharingDetectsMismatch(t *testing.T) {
	withFakeDevices(t, map[string]uint64{"/snap": 1, "/run": 2})
	lc := &chvLauncher{opts: CHVOptions{SnapshotDir: "/snap", RunDir: "/run"}}
	err := lc.checkDeviceSharing(Config{WorkspaceRoot: "/anywhere"})
	if err == nil {
		t.Fatal("CHV checkDeviceSharing with mismatched SnapshotDir/RunDir: want error, got nil")
	}
	for _, want := range []string{"CHVOptions.SnapshotDir", "/snap", "CHVOptions.RunDir", "/run"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("checkDeviceSharing error %q: missing %q", err.Error(), want)
		}
	}
}

// deviceCheckLauncher wraps a real Launcher and adds a controllable
// checkDeviceSharing, so pool.New's wiring (the `if dr, ok := lc.(deviceRequirer)`
// block) can be tested in isolation from any real launcher's hardlink logic —
// FakeLauncher itself deliberately does NOT implement deviceRequirer (it hardlinks
// nothing), so this is the only way to drive that branch with a Config that also
// satisfies Normalize.
type deviceCheckLauncher struct {
	Launcher
	err error
}

func (f *deviceCheckLauncher) checkDeviceSharing(Config) error { return f.err }

// TestNewPropagatesDeviceSharingFailure is the mutation test for pool.New's wiring
// itself, distinct from checkDeviceSharing's own logic tested above: it fails if
// the `if dr, ok := lc.(deviceRequirer); ok { ... }` block in pool.New is ever
// deleted or short-circuited, because then this launcher's checkDeviceSharing
// (which always errors) would never run and New would wrongly succeed.
func TestNewPropagatesDeviceSharingFailure(t *testing.T) {
	wantErr := errors.New("boom: fake device mismatch")
	lc := &deviceCheckLauncher{Launcher: NewFakeLauncher(), err: wantErr}
	cfg := Config{VMM: FakeVMM, SnapshotDir: t.TempDir(), WorkspaceRoot: t.TempDir(), MaxRuns: 1, MaxCommittedBytes: 1 << 30}
	_, err := New(cfg, lc, RealClock())
	if !errors.Is(err, wantErr) {
		t.Fatalf("New with a failing deviceRequirer: got %v, want an error wrapping %v", err, wantErr)
	}
}

// TestNewSucceedsWhenDeviceSharingPasses is the pass-case counterpart: a launcher
// whose checkDeviceSharing returns nil must not block New, and a launcher that
// implements no such method at all (FakeLauncher, exercised by every other test in
// this package that calls New) must not either — that second case is already
// covered by the rest of the suite, so this only pins the explicit-nil case.
func TestNewSucceedsWhenDeviceSharingPasses(t *testing.T) {
	lc := &deviceCheckLauncher{Launcher: NewFakeLauncher(), err: nil}
	cfg := Config{VMM: FakeVMM, SnapshotDir: t.TempDir(), WorkspaceRoot: t.TempDir(), MaxRuns: 1, MaxCommittedBytes: 1 << 30}
	p, err := New(cfg, lc, RealClock())
	if err != nil {
		t.Fatalf("New with a passing deviceRequirer: %v", err)
	}
	_ = p.Close()
}
