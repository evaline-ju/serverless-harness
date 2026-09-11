package vmpool

import (
	"context"
	"os"
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
	lc, err := NewFirecrackerLauncher(FirecrackerOptions{
		SnapshotDir:         envOr("SH_SNAPSHOT_IMAGE_DIR", "/srv/snapshots/swebench-py311"),
		JailerBin:           envOr("SH_JAILER_BIN", "/usr/bin/jailer"),
		FirecrackerBin:      envOr("SH_FIRECRACKER_BIN", "/usr/bin/firecracker"),
		ChrootBase:          t.TempDir(),
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
