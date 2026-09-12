package vmpool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// requireKVM skips unless SH_KVM=1, following the repo's SH_LIVE_RELAY / M3_LIVE_SMOKE
// convention so `make test` stays green on a laptop (spec §8). No hypervisor is
// available to this task's own environment, so TestFirecrackerRestoresPausedAndRunsOneCommand
// and TestFirecrackerMountsAtAcquireNotAtRestore below are written against the real
// FirecrackerOptions/VM contract but can only be exercised on a rig with /dev/kvm and a
// built golden snapshot — see the task-15 report.
func requireKVM(t *testing.T) {
	t.Helper()
	if os.Getenv("SH_KVM") != "1" {
		t.Skip("needs /dev/kvm and a built golden snapshot; set SH_KVM=1 on the rig")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Fatalf("SH_KVM=1 but /dev/kvm is unusable: %v", err)
	}
}

func fcLauncher(t *testing.T) Launcher {
	t.Helper()
	snapshotDir := envOr("SH_SNAPSHOT_IMAGE_DIR", "/srv/snapshots/swebench-py311")
	lc, err := NewFirecrackerLauncher(FirecrackerOptions{
		SnapshotDir:         snapshotDir,
		JailerBin:           envOr("SH_JAILER_BIN", "/usr/bin/jailer"),
		FirecrackerBin:      envOr("SH_FIRECRACKER_BIN", "/usr/bin/firecracker"),
		ChrootBase:          sameDeviceSiblingDir(t, snapshotDir),
		UID:                 os.Getuid(),
		GID:                 os.Getgid(),
		WorkspaceImageBytes: 2 << 30,
		VsockPort:           1024,
	})
	if err != nil {
		t.Fatalf("NewFirecrackerLauncher: %v", err)
	}
	return lc
}

// sameDeviceSiblingDir returns a fresh directory guaranteed by construction to share a
// device with snapshotDir's parent, for use as a hardlink target next to the golden
// snapshot -- mirroring new_verify_dir() in deploy/microvm/build-snapshot.sh, which
// solves the identical problem for the shell harness's own restore-verification jail
// (see commit 4059338). Both VMM arms hardlink (never copy) snapshot components into a
// per-run directory -- Firecracker's jailer into ChrootBase/<id>/root/, Cloud
// Hypervisor's Restore into RunDir/<id>/ -- deliberately, so N standby VMs sharing one
// golden snapshot don't each duplicate a multi-hundred-MiB memfile. A hardlink across
// devices is EXDEV, unconditionally, so t.TempDir() alone cannot stand in here: it
// honours $TMPDIR, which has no reason to share a device with wherever the snapshot
// lives (on the rig, /tmp is tmpfs and the snapshot is on /srv's ext4).
//
// SH_CHROOT_BASE overrides the parent directory outright, for a rig with an unusual
// layout; absent that, the parent defaults to snapshotDir's own parent directory, which
// must already exist -- this deliberately does not os.MkdirAll it into existence, the
// same way new_verify_dir()'s mktemp -d fails loudly on a missing $(dirname "$OUT")
// rather than silently creating one.
func sameDeviceSiblingDir(t *testing.T, snapshotDir string) string {
	t.Helper()
	parent := envOr("SH_CHROOT_BASE", filepath.Dir(snapshotDir))
	dir, err := os.MkdirTemp(parent, ".gates-hardlink-jail-")
	if err != nil {
		t.Fatalf("same-device sibling dir: MkdirTemp under %s (a sibling of snapshot dir "+
			"%s, so hardlinks into it land on the same device -- see new_verify_dir in "+
			"deploy/microvm/build-snapshot.sh, or set SH_CHROOT_BASE): %v", parent, snapshotDir, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("cleanup same-device sibling dir %s: %v", dir, err)
		}
	})
	// Review finding (fix round 3): os.MkdirTemp creates dir at mode 0700,
	// root-owned when these gates run as root -- gates_kvm_test.go's poolFor
	// and this file's own fcLauncher require exactly that (see round 1's fix).
	// On the Firecracker arm a 0700 root-owned dir is harmless: jailer's whole
	// process tree runs as root too, so root needs no permission bit to enter
	// anywhere. On the Cloud Hypervisor arm it is NOT harmless: virtiofsd drops
	// privileges to VirtiofsdUID/GID (an unprivileged uid, e.g. 65534 "nobody" —
	// CHVOptions.validate() refuses 0) before it ever touches the workspace
	// directory this function's callers nest beneath dir (chvOpts's RunDir,
	// and every per-run WorkspaceDir under a WorkspaceRoot rooted here), and the
	// kernel checks execute permission on EVERY ancestor between "/" and that
	// workspace, not just the workspace's own (correctly chowned, by
	// chvPrepareOwnership) mode. A 0700 ancestor blocks that unprivileged
	// traversal exactly as effectively as a 0700 leaf would -- this is the bug
	// the coordinator diagnosed on the rig: virtiofsd's own EACCES on this
	// ancestor surfaced as the misleading "does not exist" on the leaf it could
	// never reach.
	//
	// chmod to 0711 -- execute-without-read -- rather than 0755: it lets an
	// unprivileged process traverse THROUGH to a path it already knows the name
	// of, without letting it list what else is in here (the standard "reachable
	// but not listable" posture), so this jail's other per-run contents stay
	// unlistable by anyone but its root owner. Do NOT tighten this back to
	// 0700: root needs no permission bit at all, so every Firecracker gate
	// would keep passing while every Cloud Hypervisor one silently broke again.
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatalf("same-device sibling dir: chmod %s to 0711 (needed so an unprivileged "+
			"virtiofsd can traverse into it -- see this function's doc comment): %v", dir, err)
	}
	return dir
}

// deviceOf returns the device number of the filesystem holding path, for asserting two
// directories share a device (the condition a cross-device hardlink needs). Delegates to
// the production deviceNumber (device_unix.go / device_other.go) rather than keeping a
// second, test-only *syscall.Stat_t lookup: this package already has to carry that
// build-tag split for pool.New's own startup check (checkPathsShareDevice), and a
// duplicate here broke `GOOS=windows go vet ./...` -- Stat_t does not exist on that
// platform, and vet, unlike build, compiles _test.go files too. On a platform where
// deviceNumber cannot answer (device_other.go's stub), this skips rather than fails: that
// stub is a documented "unsupported here", not a bug this test should report.
func deviceOf(t *testing.T, path string) uint64 {
	t.Helper()
	dev, err := deviceNumber(path)
	if err != nil {
		t.Skipf("deviceNumber(%s): %v (this platform cannot verify device-sharing; see device_other.go)", path, err)
	}
	return dev
}

// TestSameDeviceSiblingDirSharesDeviceWithTarget is the assertion that would have caught
// fcLauncher's original bug -- ChrootBase: t.TempDir() -- without needing a hypervisor:
// on the rig, /tmp (tmpfs) and /srv (ext4, where the golden snapshot lives) are different
// devices, so the jailer's hardlink of every snapshot component into ChrootBase always
// failed EXDEV. sameDeviceSiblingDir exists specifically so that never happens again;
// this test holds it to that.
//
// It stands in a t.TempDir() for the snapshot directory rather than requiring the real
// one (SH_SNAPSHOT_IMAGE_DIR) to exist, per spec §8: the gates that need no KVM must
// never require rig-only state, and this check is about which *device* a directory
// lands on relative to another, not about the snapshot's contents.
func TestSameDeviceSiblingDirSharesDeviceWithTarget(t *testing.T) {
	snapshotDir := t.TempDir()

	got := sameDeviceSiblingDir(t, snapshotDir)

	wantDev := deviceOf(t, filepath.Dir(snapshotDir))
	gotDev := deviceOf(t, got)
	if gotDev != wantDev {
		t.Fatalf("sameDeviceSiblingDir(%s) = %s, on device %d; want device %d (same as %s) -- "+
			"a hardlink from the snapshot dir into this directory would be cross-device and "+
			"therefore always fail EXDEV", snapshotDir, got, gotDev, wantDev, filepath.Dir(snapshotDir))
	}
}

// TestSameDeviceSiblingDirIsTraversableButNotListable is fix round 3's Item 1
// mutation test: pins the exact mode sameDeviceSiblingDir chmods its directory
// to, and that it is exactly 0711 -- not os.MkdirTemp's own default 0700 (the
// bug the coordinator diagnosed on the rig: a root-owned 0700 ancestor blocks
// an unprivileged virtiofsd's traversal into a correctly-chowned workspace
// beneath it), and not a looser 0755 (the coordinator's explicit "reachable
// but not listable" choice). Reverting the chmod call's argument to 0700, or
// deleting the chmod entirely, makes this fail immediately -- observed locally
// (see this task's report): with the chmod removed, this test fails with
// "mode = 0700, want 0711"; restoring the chmod makes it pass again.
func TestSameDeviceSiblingDirIsTraversableButNotListable(t *testing.T) {
	snapshotDir := t.TempDir()
	dir := sameDeviceSiblingDir(t, snapshotDir)

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if got := info.Mode().Perm(); got != 0o711 {
		t.Fatalf("sameDeviceSiblingDir(%s) mode = %04o, want 0711 (execute-without-read: "+
			"traversable by an unprivileged virtiofsd, not listable by it)", dir, got)
	}
}

func TestFirecrackerRestoresPausedAndRunsOneCommand(t *testing.T) {
	requireKVM(t)
	lc := fcLauncher(t)
	dir := t.TempDir()
	vm, err := lc.Restore(context.Background(), RestoreRequest{
		ID: "vm-fc-1", Key: "run-a", WorkspaceDir: dir, GuestRAMBytes: 256 << 20,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	defer func() { _ = vm.Destroy() }()
	// Resume both unpauses the VM AND mounts /workspace (mount-at-acquire, spec
	// §4.3; see launcher.go's VM.Resume doc comment) — unlike the brief's own Step 7
	// draft, which put the mount in Run's wrapCommand. Resume failing here would
	// mean either the unpause or the mount failed; either is a Resume-time error,
	// not something Run needs to detect.
	if err := vm.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	var out capturingSink
	res, err := vm.Run(context.Background(), Command{Command: "echo hi", TimeoutS: 30, CapBytes: OutputCapBytes}, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || out.out() != "hi\n" {
		t.Fatalf("res=%+v stdout=%q", res, out.out())
	}
}

func TestFirecrackerMountsAtAcquireNotAtRestore(t *testing.T) {
	requireKVM(t)
	lc := fcLauncher(t)
	dir := t.TempDir()
	// TWO standbys for one run, both restored before either is resumed. Spec §4.3: two
	// guest kernels mounting one ext4 rw corrupt it, and pre-mounted standbys do exactly
	// that with ZERO concurrent Execs — so this must be safe, which is only true if the
	// mount happens at acquire (i.e. inside Resume, not Restore).
	var vms []VM
	for _, id := range []string{"vm-fc-a", "vm-fc-b"} {
		vm, err := lc.Restore(context.Background(), RestoreRequest{ID: id, Key: "run-a", WorkspaceDir: dir, GuestRAMBytes: 256 << 20})
		if err != nil {
			t.Fatalf("Restore %s: %v", id, err)
		}
		defer func() { _ = vm.Destroy() }()
		vms = append(vms, vm)
	}
	// Serially: the arm cannot hold the rw mount twice, which is why
	// SerializesExecsPerRun() is true for it.
	for i, vm := range vms {
		if err := vm.Resume(context.Background()); err != nil {
			t.Fatalf("Resume %d: %v", i, err)
		}
		var out capturingSink
		cmd := "echo " + string(rune('a'+i)) + " >> log.txt; cat log.txt"
		if _, err := vm.Run(context.Background(), Command{Command: cmd, TimeoutS: 30, CapBytes: OutputCapBytes}, &out); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if i == 1 && out.out() != "a\nb\n" {
			// The write from the first VM survived the second's fresh mount, which is
			// the durability gate the mandatory `sync` (in Run's wrapCommand) exists
			// for (spec §4.3, §6).
			t.Fatalf("second VM saw %q, want %q", out.out(), "a\nb\n")
		}
		if err := vm.Destroy(); err != nil {
			t.Fatalf("Destroy %d: %v", i, err)
		}
	}
}

func TestFirecrackerSerializesExecsPerRun(t *testing.T) {
	// A pure contract assertion, no KVM needed: the pool reads this to decide whether to
	// hold a per-run Exec mutex, and getting it wrong corrupts an ext4 (spec §4.3). Never
	// calls Restore, so it constructs no unix socket path and needs no shortUnixSocketDir
	// style workaround for macOS's sun_path limit.
	lc, err := NewFirecrackerLauncher(FirecrackerOptions{SnapshotDir: t.TempDir(), JailerBin: "/bin/true", FirecrackerBin: "/bin/true", ChrootBase: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFirecrackerLauncher: %v", err)
	}
	if !lc.SerializesExecsPerRun() {
		t.Fatal("the Firecracker arm MUST serialize Execs per run: only one VM may hold the rw ext4 mount")
	}
	if lc.Kind() != Firecracker {
		t.Fatalf("Kind = %q", lc.Kind())
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
