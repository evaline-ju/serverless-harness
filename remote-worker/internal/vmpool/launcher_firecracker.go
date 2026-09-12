package vmpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// This file is the Firecracker arm of Launcher (spec §4.3, §5.3). Two things about
// it are load-bearing enough to say once, here, rather than repeat at every call
// site:
//
// PROCESS LIFETIME. The jailer/firecracker process Restore starts must outlive the
// context Restore itself was called with — that ctx is a per-warm or per-Probe
// context that is cancelled the moment THAT call returns, long before the VM it
// created is done being useful. So Restore uses exec.Command, never
// exec.CommandContext, for that one process, and hands the *exec.Cmd to the
// returned VM so Destroy (deferred unconditionally by every caller, per
// pool.go's ExecPhased) is what ends its life, not ctx cancellation.
//
// TWO CONFIRMED GAPS THIS TASK DOES NOT CLOSE. deploy/microvm/build-snapshot.sh
// (a) runs firecracker directly, unjailed, and records the rootfs drive's
// path_on_host as an absolute path inside a `mktemp -d` staging directory that is
// deleted (`trap 'rm -rf "$STAGE"' EXIT`) once the build finishes, and (b) never
// configures a second (workspace) drive at all — there is no PUT /drives/workspace
// call anywhere in it. Firecracker's snapshot/load API has no restore-time
// override for a drive's path_on_host (unlike vsock, which has vsock_override —
// see fcapi.go) or for an absent device, so this launcher cannot make either gap
// disappear from inside Restore. What it does instead: hardlink every snapshot
// component AND workspace.img into the jail at the FIXED, jail-relative paths a
// FIXED build script would need to have recorded in vmstate for LoadSnapshot to
// find them (see Restore's comments below for exactly which paths). Until
// build-snapshot.sh is changed to build under that same convention, restoring a
// snapshot the CURRENT script produced will fail at LoadSnapshot with a
// fault_message naming the missing/wrong path — loudly, not silently. See the
// task-15 report for the full writeup; this is a blocker for whichever task owns
// the build pipeline next, not something Task 15's interface work can paper over.
const (
	// DefaultWorkspaceImageBytes sizes workspace.img when FirecrackerOptions leaves
	// WorkspaceImageBytes unset. 2 GiB matches the brief's own test fixtures.
	DefaultWorkspaceImageBytes int64 = 2 << 30
	// defaultFCVsockPort is cmd/guest-agent/main.go's own default ("vsock:1024") —
	// duplicated here as a fallback rather than imported, since importing
	// cmd/guest-agent from internal/vmpool would be a binary-to-package dependency
	// pointing the wrong way.
	defaultFCVsockPort uint32 = 1024

	// apiSockRelPath and vsockRelPath are jail-relative (i.e. as Firecracker itself,
	// running chrooted, sees them): "/" here means jailRoot on the host side. Fixed
	// rather than derived from anything per-VM, because nothing about them needs to
	// vary — every jail is a fresh, disposable root.
	apiSockRelPath = "/run/firecracker.socket"
	vsockRelPath   = "/vsock.sock"
)

// FirecrackerOptions configures the Firecracker launcher.
type FirecrackerOptions struct {
	SnapshotDir    string // golden snapshot dir: vmstate, memfile, kernel, rootfs, agent, manifest.json (snapshot.go)
	JailerBin      string
	FirecrackerBin string
	ChrootBase     string // jailer's --chroot-base-dir

	UID, GID int // jailer's --uid/--gid: the unprivileged user the jailed firecracker process runs as

	// ParentCgroup is jailer's --parent-cgroup. It MUST be configured consistently
	// with the systemd slice Task 17 defines (spec §5.3: "they must be configured
	// consistently ... or the two mechanisms fight and the leak we are preventing
	// returns"). Left empty, jailer's own default cgroup placement is used and
	// --cgroup-version/--parent-cgroup are omitted entirely — Restore does not
	// guess a value Task 17 has not defined yet.
	ParentCgroup string

	// CgroupMemoryMaxBytes is the per-VM cgroup memory.max jailer is told to write via
	// --cgroup (Task 17, hardware-corrections D1). Without --cgroup, jailer only
	// MOVES the jailed process into --parent-cgroup if that path already exists — it
	// does NOT create a new cgroup, so on its own ParentCgroup produces no per-VM
	// memory.max at all and spec §6's mitigation #3 ("a ballooning command is killed
	// inside its own cgroup") is unimplemented; this was confirmed directly against
	// real jailer output (task-17-hardware-corrections.md D1). This value MUST equal
	// vmpool.PerVMBytes(cfg) — the same figure admission control charges per VM — set
	// by the caller (cmd/microvm-worker/main.go's launcherFor), never a fresh
	// constant, or the two numbers drift apart (spec §5.3). Only meaningful, and only
	// applied, when ParentCgroup is also set.
	CgroupMemoryMaxBytes int64

	// WorkspaceImageBytes sizes the lazily-created workspace.img ext4 filesystem
	// (the second drive, guest /dev/vdb). Created once per run (RestoreRequest.
	// WorkspaceDir) on first Restore into that workspace, then hardlinked into every
	// VM subsequently restored for the same run. Defaults to DefaultWorkspaceImageBytes
	// when <= 0.
	WorkspaceImageBytes int64

	// VsockPort is the guest agent's listen port. Defaults to 1024 (the guest
	// agent's own default) when zero.
	VsockPort uint32
}

func (o *FirecrackerOptions) setDefaults() {
	if o.VsockPort == 0 {
		o.VsockPort = defaultFCVsockPort
	}
	if o.WorkspaceImageBytes <= 0 {
		o.WorkspaceImageBytes = DefaultWorkspaceImageBytes
	}
}

func (o FirecrackerOptions) validate() error {
	switch {
	case o.SnapshotDir == "":
		return errors.New("firecracker: SnapshotDir is required")
	case o.JailerBin == "":
		return errors.New("firecracker: JailerBin is required")
	case o.FirecrackerBin == "":
		return errors.New("firecracker: FirecrackerBin is required")
	case o.ChrootBase == "":
		return errors.New("firecracker: ChrootBase is required")
	case o.ParentCgroup != "" && o.CgroupMemoryMaxBytes <= 0:
		// D1: a ParentCgroup with no memory bound is exactly the half-wired state the
		// hardware corrections found in committed code — jailer moves the process into
		// the slice but creates no per-VM cgroup and sets no memory.max. Fail loudly at
		// construction rather than silently omitting --cgroup at Restore time.
		return errors.New("firecracker: ParentCgroup is set but CgroupMemoryMaxBytes is <= 0 " +
			"— jailer would move the VM into the slice without creating a per-VM cgroup or " +
			"memory.max (spec §6 mitigation #3 would be unimplemented); set it from vmpool.PerVMBytes(cfg)")
	}
	return nil
}

// firecrackerCgroupArgs returns the jailer flags that create and bound this VM's own
// cgroup: --cgroup-version 2 (D2: jailer's own default is version "1", which this
// cgroup2-only host does not have — always pass 2 explicitly), --parent-cgroup (so the
// VM's cgroup lands under Task 17's systemd slice, spec §5.3), and --cgroup
// memory.max=<bytes> (D1: the flag that actually creates the per-VM cgroup at all;
// --parent-cgroup alone only relocates the process into the shared parent). Split out
// of Restore's argv construction so a test can assert the memory bound agrees with
// PerVMBytes(cfg) without spawning jailer.
func firecrackerCgroupArgs(opts FirecrackerOptions) []string {
	if opts.ParentCgroup == "" {
		return nil
	}
	args := []string{"--cgroup-version", "2", "--parent-cgroup", opts.ParentCgroup}
	if opts.CgroupMemoryMaxBytes > 0 {
		args = append(args, "--cgroup", fmt.Sprintf("memory.max=%d", opts.CgroupMemoryMaxBytes))
	}
	return args
}

// firecrackerLauncher is the Firecracker arm of Launcher.
type firecrackerLauncher struct {
	opts FirecrackerOptions
}

// NewFirecrackerLauncher validates opts, applies defaults, and returns a Launcher.
// It does not touch the filesystem or spawn anything — that is all deferred to
// Restore, off the hot path (spec §4.3).
func NewFirecrackerLauncher(opts FirecrackerOptions) (Launcher, error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &firecrackerLauncher{opts: opts}, nil
}

func (l *firecrackerLauncher) Kind() VMMKind { return Firecracker }

// checkDeviceSharing implements deviceRequirer. Restore hardlinks (os.Link, never
// a copy) every golden-snapshot component from l.opts.SnapshotDir, AND the
// per-run workspace image from cfg.WorkspaceRoot, into the jail under
// l.opts.ChrootBase — so all three must share one filesystem device or every
// Restore fails with EXDEV. The workspace image is hardlinked rather than copied
// deliberately: the run's workspace image must be the SAME INODE across that
// run's Execs, because that is how a file written in Exec N is still there in
// Exec N+1 (TestGateWriteDurability pins exactly this); a copy would silently
// break that durability guarantee instead of failing loudly, which is worse.
func (l *firecrackerLauncher) checkDeviceSharing(cfg Config) error {
	return checkPathsShareDevice(
		"Restore hardlinks the golden snapshot's components and the per-run "+
			"workspace image into the jail, and hardlink(2) cannot cross devices",
		namedPath{"FirecrackerOptions.SnapshotDir", l.opts.SnapshotDir},
		namedPath{"Config.WorkspaceRoot", cfg.WorkspaceRoot},
		namedPath{"FirecrackerOptions.ChrootBase", l.opts.ChrootBase},
	)
}

// SerializesExecsPerRun is true: the workspace is an ext4 image, not a shared-disk
// filesystem, and only one guest may hold its rw mount at a time (spec §4.3) — two
// concurrent Execs for the same run would mount it twice and corrupt it.
func (l *firecrackerLauncher) SerializesExecsPerRun() bool { return true }

// Restore brings up one VM from the golden snapshot into its own jail and returns
// it PAUSED. Every failure path below cleans up whatever it already created (kills
// any spawned process, removes the jail directory) and returns (nil, err) — never a
// non-nil VM alongside a non-nil error (spec §6: an early return here must not leak
// a jail or a process the caller has no handle to destroy).
func (l *firecrackerLauncher) Restore(ctx context.Context, req RestoreRequest) (VM, error) {
	// Refuse before spawning anything on a platform with no equivalent of the
	// process-group isolation (Setpgid/Kill) Destroy depends on — see
	// launcher_firecracker_unix.go / launcher_firecracker_other.go. Always nil on
	// unix; Firecracker/jailer are Linux-only regardless, but this is what turns a
	// non-unix build's failure into one clear message instead of an opaque exec
	// error deep inside cmd.Start.
	if err := fcPlatformSupported(); err != nil {
		return nil, fmt.Errorf("firecracker: restore %s: %w", req.ID, err)
	}
	// Jailer's own chroot convention: <chroot-base-dir>/<exec-file basename>/<id>/root.
	// UNVERIFIED against real hardware (none is available to this task — see the
	// task-15 report); this matches Firecracker's jailer documentation, but Task 16
	// or the first rig run should confirm it before trusting it further.
	jailRoot := filepath.Join(l.opts.ChrootBase, filepath.Base(l.opts.FirecrackerBin), req.ID, "root")

	var cmd *exec.Cmd
	// cleanup mirrors Destroy's error handling below: a failure here (permission,
	// EBUSY) must be surfaced, not swallowed, or the caller sees only the original
	// failure with no signal that a stale jail or process was left behind — the
	// same class of silent leak the never-return-a-VM rule exists to prevent, on
	// the error path instead of the happy path. It is exactly how a prior aborted
	// restore's leftovers cause the next restore's socket-already-in-use failure.
	cleanup := func() error {
		var errs []error
		if cmd != nil && cmd.Process != nil {
			pid := cmd.Process.Pid
			if err := fcKillProcessGroup(pid); err != nil && !fcProcessNotFound(err) {
				errs = append(errs, fmt.Errorf("kill -%d: %w", pid, err))
			}
			_ = cmd.Wait()
		}
		if err := os.RemoveAll(jailRoot); err != nil {
			errs = append(errs, fmt.Errorf("remove jail %s: %w", jailRoot, err))
		}
		return errors.Join(errs...)
	}

	if err := os.MkdirAll(filepath.Join(jailRoot, "run"), 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("firecracker: restore %s: create jail: %w", req.ID, err), cleanup())
	}

	// Hardlink the golden snapshot's components into the jail at fixed, jail-relative
	// basenames. A hardlink (not a copy) keeps memfile a single page-cache object
	// shared across every VM restored from this snapshot, so Task 14's
	// PinMemoryFile mlock actually shares physical pages across VMs (spec §7.3)
	// rather than each VM privately mlocking its own copy.
	//
	// See this file's package-level comment for the rootfs path_on_host gap this
	// depends on build-snapshot.sh closing: LoadSnapshot below will fail against a
	// snapshot the CURRENT script produced, loudly, with Firecracker's own
	// fault_message identifying the path it could not find.
	for _, name := range []string{fileVMState, fileMemory, fileKernel, fileRootfs, fileAgent} {
		src := filepath.Join(l.opts.SnapshotDir, name)
		dst := filepath.Join(jailRoot, name)
		_ = os.Remove(dst) // best-effort: a stale link from an aborted prior attempt at this same ID
		if err := os.Link(src, dst); err != nil {
			return nil, errors.Join(fmt.Errorf("firecracker: restore %s: hardlink %s: %w", req.ID, name, err), cleanup())
		}
		if err := os.Chown(dst, l.opts.UID, l.opts.GID); err != nil {
			// Coordinator finding #3: the API socket (and, by the same mechanism, every
			// file this launcher pre-populates into the jail) is created by THIS
			// process, not by jailer's own resource-copying, so it is owned by
			// whatever this launcher runs as rather than by the uid/gid jailer drops
			// privileges to. Firecracker cannot open a hardlink it cannot read once
			// jailer setuid/setgid's into opts.UID/opts.GID, so this must be fatal
			// rather than logged-and-ignored.
			return nil, errors.Join(fmt.Errorf("firecracker: restore %s: chown %s to %d:%d: %w", req.ID, name, l.opts.UID, l.opts.GID, err), cleanup())
		}
	}

	// The workspace drive. Lazily created once per run (WorkspaceDir), then
	// hardlinked into every VM's jail restored for that run — multiple standbys for
	// one run share the same underlying image, which is safe precisely BECAUSE only
	// one of them is ever mounted rw at a time (SerializesExecsPerRun, spec §4.3).
	//
	// See this file's package-level comment for the absent-workspace-drive gap this
	// depends on build-snapshot.sh closing: nothing makes the guest see this file as
	// /dev/vdb until the golden image is built with that drive already attached.
	imgPath := filepath.Join(req.WorkspaceDir, "workspace.img")
	if err := ensureWorkspaceImage(ctx, imgPath, l.opts.WorkspaceImageBytes); err != nil {
		return nil, errors.Join(fmt.Errorf("firecracker: restore %s: workspace image: %w", req.ID, err), cleanup())
	}
	workspaceDst := filepath.Join(jailRoot, "workspace.img")
	_ = os.Remove(workspaceDst)
	if err := os.Link(imgPath, workspaceDst); err != nil {
		return nil, errors.Join(fmt.Errorf("firecracker: restore %s: hardlink workspace.img: %w", req.ID, err), cleanup())
	}
	if err := os.Chown(workspaceDst, l.opts.UID, l.opts.GID); err != nil {
		return nil, errors.Join(fmt.Errorf("firecracker: restore %s: chown workspace.img to %d:%d: %w", req.ID, l.opts.UID, l.opts.GID, err), cleanup())
	}

	// Jailer execve's into FirecrackerBin after chroot/cgroup/uid/gid setup rather
	// than forking — so cmd.Process.Pid names the eventual firecracker process too,
	// and Destroy's process-group kill below reaches it under the same pid. Setpgid
	// puts it in its own group so the kill reaches any helper thread/process
	// Firecracker's own vcpu/seccomp machinery spawns, without also reaching this
	// launcher's own process group (coordinator finding #2's neighbor: an unrelated
	// SIGKILL storm is exactly the kind of accident a shared pgid invites).
	//
	// UNVERIFIED against real hardware, same as jailRoot above (none is available
	// to this task — see the task-15 report): the flag shape itself (this exact
	// set of jailer flags, "--" as the separator before the jailed binary's own
	// argv, and putting --api-sock after it), AND the assumption that jailer
	// resolves --api-sock (and, symmetrically, vsock's uds_path/vsock_override)
	// relative to the jail root rather than to some other directory. If either
	// assumption is wrong, waitForUnixSocket below times out rather than failing
	// with a clear cause — that timeout is the first place to look.
	args := []string{
		"--id", req.ID,
		"--exec-file", l.opts.FirecrackerBin,
		"--uid", strconv.Itoa(l.opts.UID),
		"--gid", strconv.Itoa(l.opts.GID),
		"--chroot-base-dir", l.opts.ChrootBase,
	}
	// firecrackerCgroupArgs appends --cgroup-version/--parent-cgroup/--cgroup: the last
	// of the three is load-bearing, not decorative — without it jailer relocates this
	// process into the shared parent cgroup but creates no cgroup of its own, so no
	// per-VM memory.max is ever set (Task 17, hardware-corrections D1).
	args = append(args, firecrackerCgroupArgs(l.opts)...)
	// Coordinator finding #5: this launcher never issues PUT /network-interfaces —
	// standbys are headless by construction, not by omission. Nothing below adds one.
	args = append(args, "--", "--api-sock", apiSockRelPath)

	cmd = exec.Command(l.opts.JailerBin, args...)
	// NOT exec.CommandContext — see this file's package-level comment on process
	// lifetime. Stdin is deliberately left nil (not e.g. os.Stdin): a jailed process
	// that inherits a controlling terminal's stdin can be sent SIGTTIN and stop the
	// moment it tries to read from it, which from this launcher's side is
	// indistinguishable from a hang (coordinator finding #2).
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	fcIsolateProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("firecracker: restore %s: start jailer: %w", req.ID, err), cleanup())
	}

	apiSockHost := filepath.Join(jailRoot, apiSockRelPath)
	if err := waitForUnixSocket(ctx, apiSockHost, 5*time.Second); err != nil {
		return nil, errors.Join(fmt.Errorf("firecracker: restore %s: API socket never appeared: %w", req.ID, err), cleanup())
	}

	fc := newFCClient(apiSockHost)
	// vsock_override redirects the vsock device's host-side socket to a path of OUR
	// choosing, rather than whatever uds_path build-snapshot.sh's guest_client.go
	// baked into the snapshot (an ephemeral staging path with the same "gone by
	// restore time" problem as rootfs's path_on_host — see the package comment).
	// This is the one restore-time path override Firecracker's snapshot/load API
	// actually offers (fcapi.go's vsockOverride doc comment), which is why Restore
	// uses it here instead of trying to guess or fix up the recorded path.
	fc.setVsockOverride(vsockRelPath)
	if err := fc.LoadSnapshot(ctx, filepath.Join("/", fileVMState), filepath.Join("/", fileMemory)); err != nil {
		return nil, errors.Join(fmt.Errorf("firecracker: restore %s: load snapshot: %w", req.ID, err), cleanup())
	}

	return &firecrackerVM{
		id:            req.ID,
		key:           req.Key,
		cmd:           cmd,
		jailRoot:      jailRoot,
		apiSockHost:   apiSockHost,
		vsockHostPath: filepath.Join(jailRoot, vsockRelPath),
		vsockPort:     l.opts.VsockPort,
	}, nil
}

// ensureWorkspaceImage creates path as a sparse ext4 filesystem of sizeBytes if it
// does not already exist. Called at most once per run (WorkspaceDir is one run's
// directory, shared by every VM Restore creates for it), so the mkfs cost is paid
// once per run rather than once per VM.
func ensureWorkspaceImage(ctx context.Context, path string, sizeBytes int64) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(sizeBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	out, err := exec.CommandContext(ctx, "mkfs.ext4", "-F", path).CombinedOutput()
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", path, err, out)
	}
	return nil
}

// waitForUnixSocket polls until a listener answers at path, or ctx ends, or timeout
// elapses. File existence alone is not enough: Firecracker creates the socket file
// before it is actually accept()ing on it.
func waitForUnixSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if conn, err := net.DialTimeout("unix", path, 100*time.Millisecond); err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// firecrackerVM is one restored, jailed VM.
type firecrackerVM struct {
	id, key string

	cmd           *exec.Cmd // the jailer process; execve's into FirecrackerBin, same pid
	jailRoot      string
	apiSockHost   string
	vsockHostPath string
	vsockPort     uint32

	mu        sync.Mutex
	destroyed bool
}

func (v *firecrackerVM) Key() string { return v.key }

// Resume unpauses the VM and mounts the workspace over a fresh vsock connection —
// see launcher.go's VM.Resume doc comment, which is authoritative over this
// package's own design draft on where the mount happens: mount-at-acquire, not
// inside Run. Standbys restore UNMOUNTED because ext4 is not a shared-disk
// filesystem — two guest kernels mounting one rw image would corrupt it, and a
// standby that stayed mounted across restores would do exactly that with zero
// concurrent Execs. Mounting fresh on every acquire is also correct rather than
// merely necessary: it re-reads the device's metadata, retiring the stale-metadata
// hazard a pre-mounted standby would carry (spec §4.3).
func (v *firecrackerVM) Resume(ctx context.Context) error {
	if err := v.checkNotDestroyed(); err != nil {
		return err
	}

	fc := newFCClient(v.apiSockHost)
	if err := fc.Resume(ctx); err != nil {
		return fmt.Errorf("firecracker: resume %s: %w", v.id, err)
	}

	conn, err := dialVsock(v.vsockHostPath, v.vsockPort)
	if err != nil {
		return fmt.Errorf("firecracker: resume %s: dial vsock: %w", v.id, err)
	}
	// runOverConn closes conn itself (guestconn.go), and this is a FRESH connection
	// used for exactly this one internal command — never reused by Run, which dials
	// its own (see runOverConn's doc comment on why: established connections are
	// closed on resume, so a connection carried across a snapshot boundary would be a
	// connection to nowhere, and by the same logic one connection cannot straddle
	// "the mount" and "the user's command" either without risking exactly that if a
	// future resume ever intervened between them).
	//
	// time.Now() here (not a Clock): VM implementations have no injected Clock —
	// only pool.go, one layer up, threads one through — so this is the one place in
	// the call chain that must read the wall clock directly. This IS coordinator
	// finding #6's wall-clock correction: sendRequest (guestconn.go) puts it in the
	// Request frame as HostUnixNanos, which the guest agent uses to correct its own
	// clock after resume — Resume needs no separate step for that.
	res, err := runOverConn(ctx, conn, Command{
		Command:  "mountpoint -q /workspace || mount -o rw,noatime /dev/vdb /workspace",
		TimeoutS: 30,
		CapBytes: OutputCapBytes,
	}, discardingSink{}, time.Now())
	if err != nil {
		return fmt.Errorf("firecracker: resume %s: mount /workspace: %w", v.id, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("firecracker: resume %s: mount /workspace exited %d", v.id, res.ExitCode)
	}
	return nil
}

// wrapCommand is Run's one mandatory addition: sync before the VM dies. (The other
// candidate addition, mounting /workspace, already happened in Resume — see its
// doc comment — so this does NOT repeat a mount here; an earlier draft of this
// launcher did, and that was wrong for the reason Resume's comment explains.)
//
// SYNC BEFORE THE VM DIES. The guest page cache dies with the VM, so without this a
// write in Exec N is simply absent in Exec N+1 — spec §8's write-durability gate is
// what catches its omission. `(exit $__fc_rc)` preserves the command's own exit
// status: sync must not be able to change what the worker reports.
func wrapCommand(cmd string) string {
	return "cd /workspace\n{ " + cmd + "\n}\n__fc_rc=$?\nsync\n(exit $__fc_rc)\n"
}

// Run sends exactly one command over a fresh vsock connection. Dialed AFTER resume,
// never before: established connections are closed on resume (spec §2.4), and this
// is always called after Resume has already succeeded (pool.go's ExecPhased order).
func (v *firecrackerVM) Run(ctx context.Context, c Command, out Sink) (Result, error) {
	if err := v.checkNotDestroyed(); err != nil {
		return Result{}, err
	}

	conn, err := dialVsock(v.vsockHostPath, v.vsockPort)
	if err != nil {
		return Result{}, fmt.Errorf("firecracker: run %s: dial vsock: %w", v.id, err)
	}
	wrapped := c
	wrapped.Command = wrapCommand(c.Command)
	// runOverConn is THE protocol implementation (guestconn.go) — reused here rather
	// than re-implemented, per this task's constraint that there must be exactly one.
	return runOverConn(ctx, conn, wrapped, out, time.Now())
}

// Destroy SIGKILLs the jailer's (and, by the same pid, Firecracker's) process
// group, reaps it, and removes the jail directory. Idempotent: spec §6 requires
// abort-after-teardown not to error, and pool.go's destroy calls this exactly once
// per VM but a caller-side bug retrying it must not become a second failure mode.
// A failure here is logged and counted by pool.go's destroy, never returned up the
// Exec path — it does not change what the command already did.
func (v *firecrackerVM) Destroy() error {
	v.mu.Lock()
	if v.destroyed {
		v.mu.Unlock()
		return nil
	}
	v.destroyed = true
	cmd := v.cmd
	v.mu.Unlock()

	var errs []error
	if cmd != nil && cmd.Process != nil {
		pid := cmd.Process.Pid
		if err := fcKillProcessGroup(pid); err != nil && !fcProcessNotFound(err) {
			errs = append(errs, fmt.Errorf("kill -%d: %w", pid, err))
		}
		_ = cmd.Wait() // reap; "signal: killed" is the expected outcome, not a failure
	}
	if err := os.RemoveAll(v.jailRoot); err != nil {
		errs = append(errs, fmt.Errorf("remove jail %s: %w", v.jailRoot, err))
	}
	return errors.Join(errs...)
}

func (v *firecrackerVM) checkNotDestroyed() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.destroyed {
		return fmt.Errorf("firecracker: VM %s: used after Destroy", v.id)
	}
	return nil
}
