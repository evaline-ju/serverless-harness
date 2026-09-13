package vmpool

import (
	"fmt"
	"os"
	"strconv"
)

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
// independently maintained constant. chvRunDirDefault lets each caller pick its own
// default run directory (see the CloudHypervisor case below) while still sharing
// everything else.
//
// Deliberately NOT included: a "fake" case. vmpool.FakeLauncher (host bash) must
// stay reachable ONLY from vmpoolctl's own launcher() — folding it in here would put
// microvm-worker's launcherFor one accidental case away from a host-execution
// fallback, which spec §3.3/§3.5 rule out structurally, not just by convention. See
// cmd/microvm-worker/main_test.go's TestThereIsNoHostFallbackLauncher.
func LauncherFromEnv(kind VMMKind, get func(string) string, snapshotDir string, perVMBytes int64, chvRunDirDefault string) (Launcher, error) {
	switch kind {
	case Firecracker:
		wsImageMB, err := envInt64(get, "SH_WORKSPACE_IMAGE_MB", 2048)
		if err != nil {
			return nil, err
		}
		return NewFirecrackerLauncher(FirecrackerOptions{
			SnapshotDir:          snapshotDir,
			JailerBin:            env(get, "SH_JAILER_BIN", "/usr/bin/jailer"),
			FirecrackerBin:       env(get, "SH_FIRECRACKER_BIN", "/usr/bin/firecracker"),
			ChrootBase:           env(get, "SH_CHROOT_BASE", "/srv/jail"),
			UID:                  os.Getuid(),
			GID:                  os.Getgid(),
			ParentCgroup:         env(get, "SH_PARENT_CGROUP", "microvm-vms.slice"),
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
		// RunDir default is under /run, not /srv or /tmp: it holds only per-VM sockets
		// and rewritten config for the CURRENT boot's live cloud-hypervisor/virtiofsd
		// processes (spec §4.3, §7.3) — see launcherFor's own longer comment on this in
		// cmd/microvm-worker/main.go for the orphan-sweep argument (Task 17). Each
		// caller supplies its own chvRunDirDefault (microvm-worker and vmpoolctl use
		// different defaults, /run/microvm-worker/chv and /run/vmpoolctl/chv) so the
		// production daemon and a diagnostic CLI run against the same host never
		// collide on the same per-VM socket/config directory naming, while SH_CHV_RUN_DIR
		// still overrides either the same way.
		return NewCloudHypervisorLauncher(CHVOptions{
			SnapshotDir:          snapshotDir,
			CHVBin:               env(get, "SH_CHV_BIN", "/usr/bin/cloud-hypervisor"),
			ChRemoteBin:          env(get, "SH_CH_REMOTE_BIN", "/usr/bin/ch-remote"),
			VirtiofsdBin:         env(get, "SH_VIRTIOFSD_BIN", "/usr/libexec/virtiofsd"),
			RunDir:               env(get, "SH_CHV_RUN_DIR", chvRunDirDefault),
			VirtiofsdUID:         int(uid),
			VirtiofsdGID:         int(gid),
			ParentCgroup:         env(get, "SH_PARENT_CGROUP", "microvm-vms.slice"),
			CgroupMemoryMaxBytes: perVMBytes,
			VsockPort:            1024,
		})
	default:
		return nil, fmt.Errorf("vmpool: LauncherFromEnv cannot construct %q — only %q and %q "+
			"are wired here; the fake launcher is deliberately excluded (see this function's "+
			"doc comment)", kind, Firecracker, CloudHypervisor)
	}
}
