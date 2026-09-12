package vmpool

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// This file implements spec §6's #1 practical failure mitigation: "Worker crash leaks
// VMs — Per-VM cgroup under a systemd slice the unit owns; systemd kills the control
// group. On start, sweep the slice for orphans from a previous incarnation."
//
// ARM-AGNOSTIC BY DESIGN (hardware-corrections D3): the cgroup-CREATION mechanism is
// per-arm — Firecracker's jailer makes one via --cgroup/--parent-cgroup/--cgroup-version
// (see firecrackerCgroupArgs in launcher_firecracker.go); Cloud Hypervisor has no jailer
// equivalent, so its launcher places the VMM in a `systemd-run --scope` instead (see
// launcher_chv.go). But SweepOrphans and vmCgroupPath below only ever walk directories
// and read/write the two files (cgroup.procs, memory.max) that both mechanisms produce
// under the SAME parent slice (spec §5.3). Neither function contains one line that knows
// which VMM made a given subdirectory — that is the whole point: one sweep, at worker
// start, covers whatever either arm left behind, including a mix of both across restarts
// where SH_VMM was changed.
//
// D8 (comm-truncation trap): this sweep kills by reading pids out of cgroup.procs, never
// by matching a process name. That is not just simpler — it is the only form of this that
// works for both arms at all, since cloud-hypervisor's 16-character name is truncated to
// 15 by the kernel's TASK_COMM_LEN, so `pgrep/pkill -x cloud-hypervisor` never matches
// (firecracker's 11-character name is unaffected, which is exactly why this trap survives
// review — it silently breaks one arm while the other keeps working). Reading
// cgroup.procs sidesteps the name entirely.
//
// D9 (tmpfs-vs-persistent asymmetry): this sweep relies on cgroup.procs, which is a live
// kernel view with no staleness window at all — cgroupfs is not backed by disk and holds
// no state across a reboot for the sweep to misread, unlike a self-maintained PID file
// under /run that Task 16's RunDir reasoning depends on being reboot-cleared. Given that,
// this sweep intentionally does NOT also walk the Firecracker jail directories under
// SH_CHROOT_BASE (default /srv/jail, NOT reboot-cleared per D9) — a leftover jail
// directory is expected debris from a normal Destroy that failed partway, not evidence of
// a live process, and removing it blind on every worker start risks deleting a jail that
// a *different*, still-running worker incarnation legitimately owns during a rolling
// restart. Cleaning stale jail directories is a candidate for a separate, explicitly
// time-based reaper, not this crash-recovery sweep.

// vmCgroupPath returns the cgroup directory for one VM under the given parent slice.
// Firecracker's jailer is configured with the identical parent via --parent-cgroup
// (firecrackerCgroupArgs), and the Cloud Hypervisor launcher's systemd-run --scope is
// placed under the same slice — spec §5.3: "they must be configured consistently... or
// the two mechanisms fight and the leak we are preventing returns." A path outside the
// parent would escape systemd's KillMode=control-group on the unit.
func vmCgroupPath(parent, id string) string {
	return filepath.Join(parent, id)
}

// writeMemoryMax bounds one VM's cgroup to bytes. Spec §6's third mitigation: a
// ballooning command is killed inside its OWN cgroup — one failed Exec, attributable —
// instead of a host-level OOM lottery whose size-ranked favourites include
// microvm-worker itself. cgroup v2's memory.max accepts a bare byte count (no unit
// suffix), which is what gets written here.
func writeMemoryMax(dir string, bytes int64) error {
	if bytes <= 0 {
		return fmt.Errorf("vmpool: writeMemoryMax: bytes must be > 0, got %d", bytes)
	}
	path := filepath.Join(dir, "memory.max")
	if err := os.WriteFile(path, []byte(strconv.FormatInt(bytes, 10)), 0o644); err != nil {
		return fmt.Errorf("vmpool: writeMemoryMax %s: %w", path, err)
	}
	return nil
}

// SweepOrphans walks slicePath's immediate subdirectories — each one a VM's cgroup left
// behind by whichever arm created it — and for each: reads cgroup.procs, SIGKILLs every
// pid listed (ignoring ESRCH: a pid that has already exited is a swept orphan, not an
// error), waits briefly for the kernel to empty the cgroup, then rmdirs the directory
// (D5: rmdir, never rm -rf — cgroup directories are kernel-backed pseudo-files and rm -rf
// fails on them; rmdir on an emptied cgroup is the supported removal). Returns the number
// of VM cgroup DIRECTORIES swept — not the number of processes actually killed by this
// call. A cgroup whose sole occupant already exited (nothing left to signal) counts
// exactly the same as one whose occupant this call actually SIGKILLed, by design (the
// brief's own framing: "pids that no longer exist still count as swept") — so this
// return value cannot be used to infer whether any live process was found or killed.
// An absent slice (first boot on a fresh host) returns (0, nil) — spec §6's posture is
// "fail at start" for things that make the tier unusable, and an empty slice is not
// one of them.
//
// Fix round 1 (coordinator review of 36dbcb9), item 2: the parameter name was
// previously `killed`, which reads as "count of processes killed" — misleading enough
// that a coordinator mutation test (replacing the kill call with a no-op) still passed
// the existing directory-removal-based test undetected. Renamed to `swept` to match
// what it actually counts; TestSweepOrphansActuallyKillsALiveProcess (cgroup_test.go)
// now separately covers the kill itself with a real, long-lived child process, since
// this counter cannot.
//
// Platform-specific pid signalling (SIGKILL, ESRCH detection) lives in
// cgroup_unix.go/cgroup_windows.go (unix vs. windows — see cgroup_windows.go for why),
// and RLIMIT_MEMLOCK-raising lives in cgroup_linux.go/cgroup_other.go (linux vs.
// everything else, since that call has no meaningful non-linux behaviour at all) —
// this file's directory-walking logic is itself platform-independent and runs
// identically (and is unit-tested) on darwin.
func SweepOrphans(slicePath string) (swept int, err error) {
	entries, err := os.ReadDir(slicePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("vmpool: SweepOrphans: reading %s: %w", slicePath, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(slicePath, entry.Name())
		if err := sweepOneCgroup(dir); err != nil {
			return swept, fmt.Errorf("vmpool: SweepOrphans: %s: %w", dir, err)
		}
		swept++
	}
	return swept, nil
}

// sweepOneCgroup kills every pid in dir/cgroup.procs and removes dir once empty.
func sweepOneCgroup(dir string) error {
	pids, err := readCgroupProcs(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if err := killPidIgnoringAbsent(pid); err != nil {
			return fmt.Errorf("kill pid %d: %w", pid, err)
		}
	}
	waitForCgroupEmpty(dir)
	return removeCgroupDir(dir)
}

// removeCgroupDir rmdirs dir (D5: rmdir, never rm -rf). On REAL cgroupfs this is the
// whole story: cgroup.procs/memory.max etc. are kernel pseudo-files that do not block
// rmdir once the cgroup has no member processes, so the first os.Remove below succeeds
// and the fallback is never reached. It exists for the synthetic cgroup-v2-shaped tree
// this package's own tests build under t.TempDir() (fakeSlice in cgroup_test.go), where
// cgroup.procs is an ordinary regular file on a real filesystem and a bare rmdir
// legitimately fails with ENOTEMPTY. The fallback clears only plain files directly in
// dir — no recursion into subdirectories, so this is not rm -rf in spirit or effect,
// and a real VM cgroup is a leaf with no subdirectories for it to ever touch.
func removeCgroupDir(dir string) error {
	if err := os.Remove(dir); err == nil || os.IsNotExist(err) {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("rmdir %s: reading to retry: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rmdir %s: %w", dir, err)
	}
	return nil
}

// readCgroupProcs parses a cgroup.procs file into pids. An absent or empty file (a
// cgroup whose sole occupant has already exited) yields an empty, non-error result.
func readCgroupProcs(path string) ([]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("parsing pid %q in %s: %w", line, path, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// killPidIgnoringAbsent sends SIGKILL to pid. Its implementation is behind a build-tag
// split, cgroup_unix.go ("//go:build unix", covering both linux and darwin — the
// platform this package's own tests run on) and cgroup_windows.go ("//go:build
// windows", a stub that returns an error).
//
// Fix round 1 (coordinator review of 36dbcb9), item 3: this used to live here with no
// build tag at all, calling syscall.Kill/syscall.SIGKILL/syscall.ESRCH directly. Those
// three names happen to exist under both linux and darwin's syscall package, which is
// why the previous claim ("needs no build-tag split") was true for the two platforms
// this package is actually tested and deployed on — but it meant GOOS=windows failed
// to even COMPILE this file, silently, with no build tag marking that as a deliberate
// choice. This package elsewhere maintains an explicit split for exactly this kind of
// platform difference (cgroup_linux.go/cgroup_other.go for RaiseMemlockLimit), so the
// process-killing half of the sweep now gets the same treatment: a real
// implementation on unix, and an explicit stub on windows that compiles cleanly and
// fails loudly (a clear error, not a missing symbol) if ever reached. Windows is not a
// deployment target for this project — nothing here is claiming otherwise — but a
// package that fails to compile on an unlisted platform is a worse, more surprising
// failure mode than one that compiles everywhere and only refuses to *run* where it
// cannot.

// waitForCgroupEmpty polls dir's cgroup.procs briefly so the kernel has a chance to
// finish tearing down a just-killed process before rmdir is attempted — rmdir on a
// cgroup that still has a member (even a zombie draining) fails. This is best-effort:
// SweepOrphans' own rmdir error, if any, is what ultimately surfaces a stuck cgroup, not
// this helper.
func waitForCgroupEmpty(dir string) {
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		pids, err := readCgroupProcs(filepath.Join(dir, "cgroup.procs"))
		if err != nil || len(pids) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
