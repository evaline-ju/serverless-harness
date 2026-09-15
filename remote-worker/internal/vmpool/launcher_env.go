package vmpool

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// vmmBin resolves a VMM binary: an explicit env override wins, then PATH, then the
// distro-packaged location as a last resort.
//
// The hardcoded /usr/bin default was wrong on every host that installs Firecracker the way
// upstream ships it. Its release tarball puts firecracker and jailer under /usr/local/bin,
// which is also where this repo's own systemd unit puts microvm-worker -- so the default
// disagreed with the project's own install convention. It surfaced twice: the shipped unit
// crash-looped on "fork/exec /usr/bin/jailer: no such file or directory" until the unit named
// the paths explicitly, and then the E10 driver hit the identical error through vmpoolctl,
// because naming them in the unit fixed only the unit.
//
// Resolving through PATH fixes it once for every caller -- the unit, both benchmark drivers,
// vmpoolctl and the gates -- instead of requiring each one to know where the binary lives. An
// explicit SH_*_BIN still wins, so a host with two installs can pin the one it means, and the
// final fallback keeps behaviour unchanged where /usr/bin really is correct.
func vmmBin(get func(string) string, envVar, name, fallback string) string {
	if v := get(envVar); v != "" {
		return v
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return fallback
}

// env and envInt64 back LauncherFromEnv below. They mirror the two helpers of the
// same names cmd/microvm-worker/main.go has defined since before this file existed
// (poolConfig and verifyInstanceType there still use their own copies for unrelated
// env reads) — get is threaded through rather than calling os.Getenv directly so a
// test can inject envFrom(map[string]string), the same shape main.go's own tests use.
func env(get func(string) string, k, def string) string {
	if v := get(k); v != "" {
		return v
	}
	return def
}

func envInt64(get func(string) string, k string, def int64) (int64, error) {
	v := get(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q must be a positive integer", k, v)
	}
	return n, nil
}

// chvDefaultRunDir derives CloudHypervisor's default RunDir as a same-device sibling
// of snapshotDir when SH_CHV_RUN_DIR is unset — see LauncherFromEnv's CloudHypervisor
// case for why this replaced a hardcoded /run/<name> path (fix round 10, Task 16).
// name discriminates each caller's default (e.g. "microvm-worker", "vmpoolctl") so two
// callers pointed at the same snapshotDir never derive the identical RunDir.
//
// The derived directory is a SIBLING of snapshotDir (filepath.Dir(snapshotDir) is its
// parent), never a CHILD of it: SnapshotDir is root-owned 0555 and hash-pinned in the
// build manifest (deploy/microvm/build-snapshot.sh's lock_down), so nothing may create
// or write beneath it. The guard below checks this directly on the derived path (not
// just "trust the formula") because that is the exact regression
// TestChvDefaultRunDirIsNotInsideSnapshotDir mutates in: changing the Join below to
// nest under snapshotDir instead of beside it. This mirrors sameDeviceSiblingDir (this
// package's own test helper, launcher_firecracker_test.go) and new_verify_dir() in
// deploy/microvm/build-snapshot.sh, which solve the identical problem for their own
// hardlink targets.
//
// createdHere reports whether this call created the directory (false if it already
// existed — e.g. a second launcher construction against the same snapshotDir in the
// same process, or a directory left over from this host's last run). Callers use it to
// decide whether THEY are responsible for removing it again on a later failure — see
// LauncherFromEnv's cleanupRunDir. If chvDefaultRunDir itself creates the directory but
// the device-sharing self-check below then fails, it removes what it just created
// before returning the error, rather than leaving an empty, unusable directory behind.
//
// The self-check re-uses checkPathsShareDevice — the same production machinery
// checkDeviceSharing calls at pool.New — rather than trusting the sibling derivation
// blindly: "same parent directory" is only "same device" on a normal layout, and an
// unusual mount (e.g. a bind mount, or a parent that is itself a mount point) could
// make filepath.Dir(snapshotDir) share a name but not a device with snapshotDir. Going
// through checkPathsShareDevice means TestChvDefaultRunDirSelfCheckCatchesDeviceMismatch
// can force a real mismatch through the withFakeDevices seam (devicecheck_test.go) and
// confirm this function reacts correctly (error returned, directory removed if this
// call created it), without needing this dev machine to actually have a second real
// filesystem device — see deviceNumberFunc's doc comment on why it does not.
func chvDefaultRunDir(snapshotDir, name string) (dir string, createdHere bool, err error) {
	parent := filepath.Dir(snapshotDir)
	dir = filepath.Join(parent, ".chv-run-"+name)
	clean := filepath.Clean(snapshotDir)
	if dir == clean || strings.HasPrefix(dir, clean+string(filepath.Separator)) {
		return "", false, fmt.Errorf(
			"vmpool: derived CloudHypervisor RunDir %s is inside SnapshotDir %s, not beside "+
				"it — SnapshotDir is root-owned read-only and hash-pinned; RunDir must be a "+
				"sibling (see chvDefaultRunDir's doc comment)", dir, snapshotDir)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		createdHere = false
	} else if errors.Is(statErr, os.ErrNotExist) {
		// 0711 (execute-without-read), matching sameDeviceSiblingDir's own chmod: an
		// unprivileged virtiofsd must be able to traverse INTO this directory's per-VM
		// subdirectories without being able to list its other, unrelated siblings'
		// contents. See that function's doc comment for the round-3 incident this
		// mode fixed.
		if mkErr := os.MkdirAll(dir, 0o711); mkErr != nil {
			return "", false, fmt.Errorf("vmpool: creating default CloudHypervisor RunDir %s: %w", dir, mkErr)
		}
		createdHere = true
	} else {
		return "", false, fmt.Errorf("vmpool: stat default CloudHypervisor RunDir %s: %w", dir, statErr)
	}
	if chkErr := checkPathsShareDevice(
		"Restore hardlinks the golden snapshot's vmstate and memory-ranges files into "+
			"the run directory, and hardlink(2) cannot cross devices",
		namedPath{"derived CloudHypervisor RunDir", dir},
		namedPath{"CHVOptions.SnapshotDir", snapshotDir},
	); chkErr != nil {
		if createdHere {
			_ = os.RemoveAll(dir)
		}
		return "", false, chkErr
	}
	return dir, createdHere, nil
}

// LauncherFromEnv maps a real VMMKind (Firecracker or CloudHypervisor) to a Launcher,
// reading the environment variables both binaries that drive real hardware need to
// agree on.
//
// Fix round 9 (Task 16). Before this, cmd/microvm-worker/main.go's launcherFor and
// cmd/vmpoolctl/main.go's launcher each hand-rolled this same switch. Round 3 wired
// launcherFor's CloudHypervisor case to the real constructor; vmpoolctl's copy was
// never touched, so it kept returning "not implemented yet (Phase D)" for six more
// rounds — the exact defect class recurring because there were two switches to keep
// in sync and only one got the memo. Two call sites was already enough to drift, so
// they are collapsed into this one implementation: a THIRD caller now has no
// per-caller switch left to copy a stale placeholder into.
//
// get and perVMBytes are threaded through rather than read inline, matching the
// convention launcherFor already used: this stays a pure mapping from already-
// resolved config to a Launcher, so a caller's test can inject both without an
// environment or a real snapshot dir. perVMBytes is vmpool.PerVMBytes(cfg)
// (hardware-corrections D1) — the SAME figure admission control charges per VM,
// threaded into both arms' CgroupMemoryMaxBytes so jailer's --cgroup memory.max=
// and systemd-run --scope's -p MemoryMax= can never drift from a second,
// independently maintained constant. chvRunDirName lets each caller pick its own
// discriminator for the default run directory chvDefaultRunDir derives (see the
// CloudHypervisor case below) — a short name, not a full path, since round 10
// stopped hardcoding fixed /run/... paths; see chvDefaultRunDir's doc comment for
// why.
//
// Deliberately NOT included: a "fake" case. vmpool.FakeLauncher (host bash) must
// stay reachable ONLY from vmpoolctl's own launcher() — folding it in here would put
// microvm-worker's launcherFor one accidental case away from a host-execution
// fallback, which spec §3.3/§3.5 rule out structurally, not just by convention. See
// cmd/microvm-worker/main_test.go's TestThereIsNoHostFallbackLauncher.
func LauncherFromEnv(kind VMMKind, get func(string) string, snapshotDir string, perVMBytes int64, chvRunDirName string) (Launcher, error) {
	switch kind {
	case Firecracker:
		wsImageMB, err := envInt64(get, "SH_WORKSPACE_IMAGE_MB", 2048)
		if err != nil {
			return nil, err
		}
		return NewFirecrackerLauncher(FirecrackerOptions{
			SnapshotDir:          snapshotDir,
			JailerBin:            vmmBin(get, "SH_JAILER_BIN", "jailer", "/usr/bin/jailer"),
			FirecrackerBin:       vmmBin(get, "SH_FIRECRACKER_BIN", "firecracker", "/usr/bin/firecracker"),
			ChrootBase:           env(get, "SH_CHROOT_BASE", "/srv/jail"),
			UID:                  os.Getuid(),
			GID:                  os.Getgid(),
			ParentCgroup:         env(get, "SH_PARENT_CGROUP", DefaultParentCgroup),
			CgroupMemoryMaxBytes: perVMBytes,
			WorkspaceImageBytes:  wsImageMB << 20,
			VsockPort:            1024,
		})
	case CloudHypervisor:
		// VirtiofsdUID/VirtiofsdGID deliberately do NOT mirror the Firecracker case's
		// os.Getuid()/os.Getgid() above: a caller of this function may itself run
		// privileged (microvm-worker needs /dev/kvm and jailer), so os.Getuid() here
		// could be 0, and CHVOptions.validate correctly refuses that — virtiofsd
		// resolves guest paths on the host and is spec §3.5's confinement boundary, so
		// it must drop to an unprivileged user of its own, independent of the
		// caller's privilege. 65534 (nobody) matches launcher_chv_test.go's chvOpts
		// default.
		uid, err := envInt64(get, "SH_VIRTIOFSD_UID", 65534)
		if err != nil {
			return nil, err
		}
		gid, err := envInt64(get, "SH_VIRTIOFSD_GID", 65534)
		if err != nil {
			return nil, err
		}
		// RunDir's default used to be a hardcoded /run/<caller>/chv (tmpfs). Fix round 3
		// picked /run because it is cleared on every reboot, which let Task 17's orphan
		// sweep reconcile leftover per-VM directories against live PIDs within a single
		// boot with no risk of a stale recorded PID being reused by an unrelated
		// process after a reboot (a PID file that survived a reboot cannot be trusted;
		// a tmpfs RunDir simply isn't there anymore to be stale). That protection is
		// abandoned here — not because it stopped mattering, but because Task 18
		// established a harder constraint that outranks it: Restore hardlinks the
		// golden snapshot's vmstate and memory-ranges files into RunDir/<id>/ (see
		// checkDeviceSharing below), and hardlink(2) always fails EXDEV across a device
		// boundary, unconditionally — /run is tmpfs and SnapshotDir is persistent disk,
		// so nothing could ever restore at all under the old default. A hard "restore
		// cannot work" beats a mere "orphan reconciliation is a little more
		// convenient". What now gives the orphan sweep the same protection /run used to
		// provide: Task 17's own correction prefers matching a live process's
		// cgroup.procs membership over trusting any recorded PID file, because kernel
		// cgroup membership cannot go stale across a reboot the way a PID number can
		// (the process either still is or isn't a member; there is no
		// reuse-after-reboot ambiguity) — see Task 17's report. So RunDir now defaults
		// to a same-device SIBLING of SnapshotDir (chvDefaultRunDir above), mirroring
		// sameDeviceSiblingDir (this package's own test helper) and new_verify_dir() in
		// deploy/microvm/build-snapshot.sh, which solve the identical problem for their
		// own hardlink targets. SH_CHV_RUN_DIR still overrides this outright, so an
		// operator-supplied bad value is still caught by checkDeviceSharing at
		// pool.New rather than silently accepted; chvRunDirName discriminates each
		// caller's default (microvm-worker vs. vmpoolctl) so the production daemon and
		// a diagnostic CLI run against the same host never collide on the same per-VM
		// socket/config directory naming.
		//
		// Because the derived directory now lives on persistent storage (a sibling of
		// SnapshotDir, not tmpfs), nothing reclaims it on reboot the way /run used to —
		// whatever creates it must remove it again, including on any failure path.
		// That is why it is created up front here, tracked via cleanupRunDir, and torn
		// down below if NewCloudHypervisorLauncher goes on to fail validation for an
		// unrelated reason — rather than left to Restore's own os.MkdirAll, which only
		// ever created it as an incidental side effect of creating its per-VM child and
		// never removed the top-level directory itself.
		runDir := get("SH_CHV_RUN_DIR")
		var cleanupRunDir func()
		if runDir == "" {
			derivedDir, createdHere, derr := chvDefaultRunDir(snapshotDir, chvRunDirName)
			if derr != nil {
				return nil, derr
			}
			runDir = derivedDir
			if createdHere {
				cleanupRunDir = func() { _ = os.RemoveAll(derivedDir) }
			}
		}
		lc, err := NewCloudHypervisorLauncher(CHVOptions{
			SnapshotDir:          snapshotDir,
			CHVBin:               vmmBin(get, "SH_CHV_BIN", "cloud-hypervisor", "/usr/bin/cloud-hypervisor"),
			ChRemoteBin:          env(get, "SH_CH_REMOTE_BIN", "/usr/bin/ch-remote"),
			VirtiofsdBin:         vmmBin(get, "SH_VIRTIOFSD_BIN", "virtiofsd", "/usr/libexec/virtiofsd"),
			RunDir:               runDir,
			VirtiofsdUID:         int(uid),
			VirtiofsdGID:         int(gid),
			ParentCgroup:         env(get, "SH_PARENT_CGROUP", DefaultParentCgroup),
			CgroupMemoryMaxBytes: perVMBytes,
			VsockPort:            1024,
		})
		if err != nil {
			if cleanupRunDir != nil {
				cleanupRunDir()
			}
			return nil, err
		}
		return lc, nil
	default:
		return nil, fmt.Errorf("vmpool: LauncherFromEnv cannot construct %q — only %q and %q "+
			"are wired here; the fake launcher is deliberately excluded (see this function's "+
			"doc comment)", kind, Firecracker, CloudHypervisor)
	}
}
