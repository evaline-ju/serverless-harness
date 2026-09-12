package vmpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// This file is the Cloud Hypervisor arm of Launcher (spec §3.5, §4.3, §6), the
// second VMM behind the same seam launcher_firecracker.go implements. It is
// modelled on that file deliberately — same process-lifetime discipline (Restore
// uses exec.Command, not exec.CommandContext, and hands the *exec.Cmd to the
// returned VM so Destroy, not ctx cancellation, ends its life), same
// errors.Join-based cleanup-on-every-failure-path convention, same idempotent
// Destroy — because this is a second arm, not a second style.
//
// THE ONE STRUCTURAL DIFFERENCE FROM FIRECRACKER: virtio-fs. The workspace here is
// not a guest-owned ext4 block device but a HOST directory arbitrated by virtiofsd,
// so (a) there are TWO host processes per standby, not one (cloud-hypervisor and
// its own virtiofsd — spec §7.3's "Σ PSS across VMM + virtiofsd"), and (b) the host
// filesystem, not a device mount, is the concurrency authority: SerializesExecsPerRun
// is false, D>1 standbys are safe, and Run neither mounts nor syncs (see Run's
// comment). That asymmetry with Task 15 is the trade spec §4.3 prices, not an
// omission.
//
// CONFINEMENT GAP THIS FILE DOES NOT CLOSE (named per hardware-corrections C5):
// the cloud-hypervisor process itself runs UNCHROOTED on this arm. Firecracker gets
// jailer's chroot; Cloud Hypervisor has no jailer equivalent, and
// `systemd-run --scope` (the tutorial's own §8 proposal) reproduces jailer's cgroup
// behaviour but bundles no chroot, no --uid/--gid, nothing like jailer's
// hardlink-into-the-jail convention. virtiofsd itself DOES have a chroot/namespace
// mechanism (--sandbox=chroot|namespace, used below) so the narrower true gap is
// just the VMM process. Spec §5.3 expects an equivalent for it; Task 17 owns
// supplying the per-arm confinement/cgroup mechanism split, not this task. This is
// verified and known, not a guess — see task-16-hardware-corrections.md C5.
//
// TASK 17'S DISPOSITION OF C5 (D3/D4, hardware-corrections): D3 — the cgroup half —
// IS closed below: chvSystemdRunScopeArgv wraps the cloud-hypervisor exec in
// `systemd-run --scope --slice=<the same slice Firecracker's jailer --parent-cgroup
// targets> -p MemoryMax=<vmpool.PerVMBytes(cfg)>`, giving this arm the same per-VM
// cgroup and memory.max bound jailer gives Firecracker's, from the same
// single-source-of-truth function (config.go's PerVMBytes) so the two arms cannot
// drift apart (spec §5.3). `--scope` was chosen specifically because it execs the
// target IN PLACE of the systemd-run client process rather than forking a detached
// unit (confirmed against systemd-run(1): "the invoked process is run as part of
// the scope unit... rather than as a child process of systemd-run"), so
// vmmCmd.Process.Pid, fcKillProcessGroup(pid), and vmmCmd.Wait() below all keep
// working unmodified — the single most safety-critical property this whole file
// has (Destroy must always be able to kill and reap what Restore started) is
// preserved exactly, not merely assumed.
//
// D4 — the chroot/filesystem-isolation half — is DELIBERATELY NOT closed here,
// and this is a reasoned finding, not a silent gap. The one mechanism that could
// close it without hand-rolled Go-level mount-namespace code is switching from
// `systemd-run --scope` to a transient systemd *service* (drop --scope), because
// only a full service unit's execution context grants access to systemd's
// filesystem-sandboxing properties (RootDirectory=, ProtectSystem=strict,
// BindPaths=/BindReadOnlyPaths=, DeviceAllow=/dev/kvm rw with PrivateDevices=yes,
// NoNewPrivileges=yes) — a --scope unit only relocates an already-running process
// into a cgroup and applies none of those. But a transient service forks
// asynchronously and is reaped by the systemd manager, not by this process, so
// keeping our foreground exec.Cmd handle synchronized with it would require one of
// systemd-run's --pipe/--wait/--collect flags, whose exact interaction with
// signal delivery and process-group membership this task has no real Linux host
// with systemd + KVM to verify. Getting that interaction wrong would silently
// break exactly the property D3 above was careful to preserve: sending SIGKILL to
// vmmCmd's own pid would kill the systemd-run client but NOT the systemd-managed
// service process tree it detached from, so Destroy would report success having
// killed nothing — reintroducing, for this arm, the precise "worker crash leaks
// VMs" failure spec §6 names as the #1 practical failure this whole task exists to
// prevent, except now on ordinary Destroy rather than only on a crash. Shipping
// that unverified is a worse outcome than shipping the already-disclosed gap: a
// known, named absence versus a confinement mechanism that looks correct in argv
// construction and quietly defeats cleanup in production. Closing D4 properly is
// left as a follow-up that needs hardware verification, not a code change made
// blind.
const (
	// defaultCHVVsockPort mirrors launcher_firecracker.go's defaultFCVsockPort: the
	// guest agent's own default ("vsock:1024"), duplicated rather than imported for
	// the same binary-to-package-dependency reason given there.
	defaultCHVVsockPort uint32 = 1024

	// chvSnapshotConfigFile, chvSnapshotMemoryRanges, chvSnapshotStateFile are Cloud
	// Hypervisor's own three-file snapshot directory layout — config.json,
	// memory-ranges, state.json — structurally different from Firecracker's
	// vmstate+memfile pair (snapshot.go's fileVMState/fileMemory), and NOT added as
	// package-level constants there because they are CH-specific, not shared shape.
	chvSnapshotConfigFile   = "config.json"
	chvSnapshotMemoryRanges = "memory-ranges"
	chvSnapshotStateFile    = "state.json"
)

// CHVOptions configures the Cloud Hypervisor launcher.
type CHVOptions struct {
	SnapshotDir  string // golden snapshot dir: config.json, memory-ranges, state.json
	CHVBin       string
	ChRemoteBin  string
	VirtiofsdBin string
	RunDir       string // per-VM sockets/config live under RunDir/<id>/

	// VirtiofsdUID/VirtiofsdGID are the unprivileged user virtiofsd drops to via
	// SysProcAttr.Credential before exec. Spec §3.5: with virtio-fs, guest path
	// resolution happens in virtiofsd on the HOST, so it is the confinement boundary
	// for the whole design — a root virtiofsd compromise would be host root and
	// would render the microVM boundary decorative. Refusing UID/GID 0 at
	// construction (validate, below) is the one place this can be enforced once and
	// for all rather than left to a deployment note nobody reads.
	VirtiofsdUID, VirtiofsdGID int

	// ParentCgroup mirrors FirecrackerOptions.ParentCgroup's doc comment exactly:
	// must be configured consistently with Task 17's systemd slice, or left empty
	// to defer to a default this launcher does not guess. When set, it names a
	// cgroupfs path (e.g. "/sys/fs/cgroup/microvm-vms.slice") — chvCgroupSliceName
	// derives the bare slice name systemd-run --slice wants from it, so callers
	// configure this launcher and the Firecracker one with the identical value.
	ParentCgroup string

	// CgroupMemoryMaxBytes mirrors FirecrackerOptions.CgroupMemoryMaxBytes exactly
	// (see that field's doc comment for the full D1 argument): the per-VM cgroup
	// memory.max this arm's systemd-run --scope is told to set via
	// `-p MemoryMax=`, MUST equal vmpool.PerVMBytes(cfg) — the same figure
	// admission control charges per VM — set by the caller, never a fresh
	// constant. Only meaningful, and only applied, when ParentCgroup is also set.
	CgroupMemoryMaxBytes int64

	// SystemdRunBin is the systemd-run binary used to create this VM's per-VM
	// cgroup scope (D3, hardware-corrections). Defaults to "systemd-run" (PATH
	// lookup) when empty; overridable for tests the same way CHVBin/ChRemoteBin/
	// VirtiofsdBin are.
	SystemdRunBin string

	// VsockPort is the guest agent's listen port. Defaults to 1024 when zero.
	VsockPort uint32
}

func (o *CHVOptions) setDefaults() {
	if o.VsockPort == 0 {
		o.VsockPort = defaultCHVVsockPort
	}
	if o.SystemdRunBin == "" {
		o.SystemdRunBin = "systemd-run"
	}
}

func (o CHVOptions) validate() error {
	switch {
	case o.SnapshotDir == "":
		return errors.New("cloud-hypervisor: SnapshotDir is required")
	case o.CHVBin == "":
		return errors.New("cloud-hypervisor: CHVBin is required")
	case o.ChRemoteBin == "":
		return errors.New("cloud-hypervisor: ChRemoteBin is required")
	case o.VirtiofsdBin == "":
		return errors.New("cloud-hypervisor: VirtiofsdBin is required")
	case o.RunDir == "":
		return errors.New("cloud-hypervisor: RunDir is required")
	case o.VirtiofsdUID == 0 || o.VirtiofsdGID == 0:
		// Spec §3.5's argument, verbatim in the error so a caller sees WHY, not just
		// THAT: virtiofsd resolves guest paths on the host, so it is the confinement
		// boundary for the whole design; a root virtiofsd compromise is host root and
		// makes the microVM boundary decorative.
		return errors.New("cloud-hypervisor: VirtiofsdUID/VirtiofsdGID must not be 0: " +
			"virtiofsd resolves guest paths on the host and is spec §3.5's confinement " +
			"boundary — running it as root would make that boundary decorative")
	case o.ParentCgroup != "" && o.CgroupMemoryMaxBytes <= 0:
		// D1's mirror for this arm: a ParentCgroup with no memory bound would leave
		// systemd-run --scope with nothing to set via -p MemoryMax=, which is this
		// arm's version of jailer moving a process into the slice without ever
		// creating a bounded per-VM cgroup. Fail loudly at construction, matching
		// launcher_firecracker.go's identical check.
		return errors.New("cloud-hypervisor: ParentCgroup is set but CgroupMemoryMaxBytes is <= 0 " +
			"— systemd-run --scope would create a per-VM cgroup with no memory.max " +
			"(spec §6 mitigation #3 would be unimplemented); set it from vmpool.PerVMBytes(cfg)")
	}
	return nil
}

// chvLauncher is the Cloud Hypervisor arm of Launcher.
type chvLauncher struct {
	opts CHVOptions
}

// NewCloudHypervisorLauncher validates opts, applies defaults, and returns a
// Launcher. Like NewFirecrackerLauncher, it touches nothing on disk and spawns
// nothing — that is all deferred to Restore, off the hot path (spec §4.3).
func NewCloudHypervisorLauncher(opts CHVOptions) (Launcher, error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &chvLauncher{opts: opts}, nil
}

func (l *chvLauncher) Kind() VMMKind { return CloudHypervisor }

// SerializesExecsPerRun is false: virtio-fs means the HOST filesystem, not a
// guest-owned block device, arbitrates concurrent access to the workspace. This is
// spec §4.3's decisive row and the whole reason the VMM is a seam rather than a
// build choice — see TestCloudHypervisorSupportsTwoStandbysForOneRun, "the row
// that decides the arm".
func (l *chvLauncher) SerializesExecsPerRun() bool { return false }

// virtiofsdArgv builds virtiofsd's argv as a pure function so it is testable
// without spawning anything (TestVirtiofsdArgvCarriesItsSandbox).
//
//   - --sandbox=namespace, NEVER --sandbox=none: spec §6's malicious-symlink row
//     says virtiofsd's sandboxing "must be configured and verified, never assumed".
//     hardware-corrections C2: every virtiofsd in the reference tutorial ran under
//     sudo, and the binary's own --help flags unprivileged --sandbox=namespace as
//     documented but untested — kept anyway, unweakened, because the alternative
//     (--sandbox=none) is the one thing this design cannot tolerate.
//   - --cache=never, NOT --cache=auto. hardware-corrections C1: --cache=auto
//     disconnected the virtio-fs session immediately on the reference host;
//     --cache=never "stayed up through every subsequent test", and CH's own
//     quickstart already uses it. The tutorial explicitly did not root-cause the
//     --cache=auto disconnect ("plausibly a feature-negotiation mismatch") — this
//     comment does not invent one either; a confident wrong explanation would be
//     worse than none.
//   - --inode-file-handles=mandatory where available, per the brief.
func virtiofsdArgv(opts CHVOptions, sock, dir string) []string {
	return []string{
		"--socket-path=" + sock,
		"--shared-dir=" + dir,
		"--sandbox=namespace",
		"--cache=never",
		"--inode-file-handles=mandatory",
	}
}

// chvCgroupSliceName derives the bare slice unit name systemd-run --slice wants
// (e.g. "microvm-vms.slice") from ParentCgroup's cgroupfs path (e.g.
// "/sys/fs/cgroup/microvm-vms.slice") — the same value FirecrackerOptions.ParentCgroup
// takes, so a caller configures both arms identically and this is the one place that
// translates it into what systemd-run itself expects on its command line.
func chvCgroupSliceName(parentCgroup string) string {
	return filepath.Base(parentCgroup)
}

// chvSystemdRunScopeArgv returns the systemd-run argv PREFIX (everything before the
// real cloud-hypervisor binary and its own args) that creates and bounds this VM's
// per-VM cgroup (D3, hardware-corrections): --scope so the target execs in place of
// the systemd-run client rather than forking a detached unit (this file's
// package-level comment explains why that property is load-bearing for Destroy's
// kill-and-reap contract), --unit so the transient scope has a stable,
// human-diagnosable name, --slice so it lands under the SAME parent slice
// Firecracker's jailer --parent-cgroup targets (spec §5.3), and
// -p MemoryMax=<bytes> — the value that must equal vmpool.PerVMBytes(cfg), never a
// second constant (D1's argument, mirrored here). Split out of Restore's argv
// construction, like firecrackerCgroupArgs, so a test can assert the memory bound
// agrees with PerVMBytes(cfg) without spawning systemd-run.
//
// UNVERIFIED END TO END (see this file's package comment, D4 disposition): this task
// has no host with systemd + KVM to run this argv for real. What IS verified is the
// documented behaviour of --scope (systemd-run(1)) that motivated choosing it over a
// transient service.
func chvSystemdRunScopeArgv(opts CHVOptions, unitName string) []string {
	if opts.ParentCgroup == "" {
		return nil
	}
	args := []string{"--scope", "--unit=" + unitName, "--slice=" + chvCgroupSliceName(opts.ParentCgroup)}
	if opts.CgroupMemoryMaxBytes > 0 {
		args = append(args, "-p", fmt.Sprintf("MemoryMax=%d", opts.CgroupMemoryMaxBytes))
	}
	return args
}

// chvChown is os.Chown, indirected so tests can verify Restore's ownership
// preparation without needing the real syscall's privilege (CAP_CHOWN, or
// already owning the target) that a non-root test runner has neither — see
// TestRestorePreparesVirtiofsdOwnership.
var chvChown = os.Chown

// chvPrepareVirtiofsdOwnership chowns runDir and workspaceDir to uid:gid before
// virtiofsd is spawned.
//
// Review finding (fix round 1, item 1): Restore creates runDir via
// os.MkdirAll — owned by whatever this launcher process runs as — and then
// drops virtiofsd to VirtiofsdUID/VirtiofsdGID via SysProcAttr.Credential
// before exec (chvIsolateAndDropPrivileges). Without this chown, virtiofsd's
// own bind() of its socket inside runDir fails with EACCES the instant it
// starts, unless the launcher happens to already run as that uid — the
// unprivileged posture CHVOptions.VirtiofsdUID's doc comment and validate()
// exist to require was, before this fix, unable to actually start.
// workspaceDir needs the same treatment for the same reason one level up:
// virtiofsd must traverse into and serve it, so a directory it cannot enter
// fails identically to a socket it cannot create.
//
// This does NOT reach workspaceDir's ANCESTORS. Firecracker's UID/GID needs no
// analogous fix there because it only ever touches paths *inside* its own
// jail (a directory tree it created and chowns as it goes, exactly like
// runDir here); virtio-fs is different because virtiofsd serves req.WorkspaceDir
// directly rather than a copy hardlinked under a launcher-owned root, so its
// own ancestor chain is out of this launcher's control — it belongs to
// whatever created WorkspaceDir (the pool/orchestration layer), the same class
// of assumption this file already makes about RunDir's and SnapshotDir's own
// parents being reachable.
func chvPrepareVirtiofsdOwnership(runDir, workspaceDir string, uid, gid int) error {
	if err := chvChown(runDir, uid, gid); err != nil {
		return fmt.Errorf("chown run dir %s to %d:%d: %w", runDir, uid, gid, err)
	}
	if err := chvChown(workspaceDir, uid, gid); err != nil {
		return fmt.Errorf("chown workspace dir %s to %d:%d: %w", workspaceDir, uid, gid, err)
	}
	return nil
}

// chvPrepareOwnership is chvPrepareVirtiofsdOwnership, indirected so a test can
// observe the CALL SITE inside Restore — not just the helper in isolation.
//
// Review finding (fix round 2): the round-1 tests
// (TestRestorePreparesVirtiofsdOwnership and its failure-propagation
// sibling) called chvPrepareVirtiofsdOwnership directly and never exercised
// Restore at all. That leaves the integration point — whether Restore
// actually calls the helper, and with which uid/gid — uncovered: a mutation
// that changed Restore's call site to pass 0:0 (chown to root, undoing the
// whole fix) or removed the call entirely still passed every test, because
// nothing was watching that call site. See TestRestoreCallsPrepareOwnership.
var chvPrepareOwnership = chvPrepareVirtiofsdOwnership

// rewriteSnapshotConfig returns a copy of the golden snapshot's config.json with
// its embedded vsock and (if present) virtio-fs socket paths replaced by
// per-VM-unique ones.
//
// WHY THIS EXISTS (a design decision this task made, not one the brief or
// hardware-corrections prescribe a mechanism for): the reference tutorial
// documents that config.json embeds the vsock socket path VERBATIM, and that
// restoring the SAME snapshot without removing the stale socket collides —
// "Cannot create virtio-vsock backend", "Error binding to the host-side Unix
// socket" (errno 98, address in use) — because "whichever process restores it
// recreates that path". The tutorial's own fix (rm -f the stale socket) only
// covers SEQUENTIAL restores where the first VM is already dead; it does not
// cover TWO LIVE standbys restored from the identical config.json at once, which
// is exactly what TestCloudHypervisorSupportsTwoStandbysForOneRun exercises.
// hardware-corrections C8 anticipates needing exactly this: "If that test
// nonetheless fails, the disk lock is not your cause; look at ... the vsock
// socket path, or the snapshot's own config.json." This function, plus Restore's
// per-VM directory below, is this task's answer to that hint. The `disks` array
// is deliberately left untouched: C4/C8 already confirmed readonly=on (baked into
// the golden snapshot by the build pipeline) makes the rootfs's advisory lock
// shareable across standbys, so nothing about the disk path needs to vary per VM.
//
// UNVERIFIED END TO END: this task has no KVM access, so this rewrite has been
// exercised only as a pure function against synthetic JSON (see
// launcher_chv_test.go), never against a real config.json or a real restore.
func rewriteSnapshotConfig(src []byte, vsockPath, fsSocketPath string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("parse config.json: %w", err)
	}
	if vsock, ok := doc["vsock"].(map[string]any); ok {
		vsock["socket"] = vsockPath
	}
	if fsSocketPath != "" {
		if fsList, ok := doc["fs"].([]any); ok {
			for _, entry := range fsList {
				if fs, ok := entry.(map[string]any); ok {
					fs["socket"] = fsSocketPath
				}
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal config.json: %w", err)
	}
	return out, nil
}

// Restore brings up one standby from the golden snapshot and returns it PAUSED
// (CH's own restore does not resume — Resume, below, does). Every failure path
// cleans up whatever it already created and returns (nil, err), mirroring
// launcher_firecracker.go's Restore exactly: never a non-nil VM alongside a
// non-nil error.
func (l *chvLauncher) Restore(ctx context.Context, req RestoreRequest) (VM, error) {
	// Reused directly from the Firecracker arm: always nil on unix, and Cloud
	// Hypervisor/virtiofsd require Linux just as much as Firecracker/jailer do, so
	// this is the same clean-refusal-before-spawning-anything check, not a
	// Firecracker-specific one that happens to also work here.
	if err := fcPlatformSupported(); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: restore %s: %w", req.ID, err)
	}

	runDir := filepath.Join(l.opts.RunDir, req.ID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: restore %s: create run dir: %w", req.ID, err)
	}

	var (
		vmmCmd    *exec.Cmd
		fsCmd     *exec.Cmd
		console   *os.File
		fsConsole *os.File
	)
	// cleanup mirrors launcher_firecracker.go's Restore cleanup closure: kill
	// whatever was already spawned (VMM first, then virtiofsd, per the brief and
	// per Destroy below), then remove the per-VM run directory. Every failure return
	// in this function goes through it, so a partially-started Restore never leaks a
	// process or socket the caller has no handle to destroy.
	cleanup := func() error {
		var errs []error
		if vmmCmd != nil && vmmCmd.Process != nil {
			pid := vmmCmd.Process.Pid
			if err := fcKillProcessGroup(pid); err != nil && !fcProcessNotFound(err) {
				errs = append(errs, fmt.Errorf("kill cloud-hypervisor -%d: %w", pid, err))
			}
			_ = vmmCmd.Wait()
		}
		if fsCmd != nil && fsCmd.Process != nil {
			pid := fsCmd.Process.Pid
			if err := fcKillProcessGroup(pid); err != nil && !fcProcessNotFound(err) {
				errs = append(errs, fmt.Errorf("kill virtiofsd -%d: %w", pid, err))
			}
			_ = fsCmd.Wait()
		}
		if console != nil {
			_ = console.Close()
		}
		if fsConsole != nil {
			_ = fsConsole.Close()
		}
		if err := os.RemoveAll(runDir); err != nil {
			errs = append(errs, fmt.Errorf("remove run dir %s: %w", runDir, err))
		}
		return errors.Join(errs...)
	}

	// Review finding (fix round 1, item 1): chown BOTH paths virtiofsd needs —
	// the run dir it will bind its own socket inside, and the workspace it must
	// traverse into and serve — to the uid/gid it is about to drop to. This must
	// happen after runDir exists (MkdirAll above) and before fsCmd.Start() below;
	// doing it any later leaves a window where virtiofsd's own bind()/traversal
	// hits EACCES instead of finding a directory it can already enter. See
	// chvPrepareVirtiofsdOwnership's doc comment for what this does and does not
	// cover (workspaceDir's ancestors are out of scope here).
	if err := chvPrepareOwnership(runDir, req.WorkspaceDir, l.opts.VirtiofsdUID, l.opts.VirtiofsdGID); err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: %w", req.ID, err), cleanup())
	}

	// --- virtiofsd, privileges dropped before exec (never run as root: see
	// CHVOptions.VirtiofsdUID's doc comment and validate() above). Its own
	// stdout/stderr are captured to a file, not discarded (review finding, fix
	// round 1, item 2): with output discarded, the exact EACCES failure item 1
	// fixes would have surfaced as nothing but a bare socket timeout below —
	// which is precisely the "secure configuration looks broken for no visible
	// reason" trap hardware-corrections C2 warns against, the one that tempts a
	// reader into "fixing" it by running virtiofsd as root instead. ---
	fsSock := filepath.Join(runDir, "vfsd.sock")
	fsArgv := virtiofsdArgv(l.opts, fsSock, req.WorkspaceDir)
	fsCmd = exec.Command(l.opts.VirtiofsdBin, fsArgv...)
	var err error
	fsConsole, err = os.Create(filepath.Join(runDir, "virtiofsd.log"))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: create virtiofsd console log: %w", req.ID, err), cleanup())
	}
	fsCmd.Stdout = fsConsole
	fsCmd.Stderr = fsConsole
	chvIsolateAndDropPrivileges(fsCmd, l.opts.VirtiofsdUID, l.opts.VirtiofsdGID)
	if err := fsCmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: start virtiofsd: %w", req.ID, err), cleanup())
	}
	if err := waitForUnixSocket(ctx, fsSock, 5*time.Second); err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: virtiofsd socket never appeared: %s: %w", req.ID, chvReadConsole(fsConsole.Name()), err), cleanup())
	}

	// --- per-VM snapshot config: hardlink the two large golden files unchanged,
	// copy+rewrite config.json's embedded vsock/fs socket paths so two standbys
	// restored from the SAME golden snapshot never collide on either socket path —
	// see rewriteSnapshotConfig's doc comment for the full justification and its
	// "unverified end to end" caveat. ---
	vsockSock := filepath.Join(runDir, "vsock.sock")
	for _, name := range []string{chvSnapshotMemoryRanges, chvSnapshotStateFile} {
		src := filepath.Join(l.opts.SnapshotDir, name)
		dst := filepath.Join(runDir, name)
		_ = os.Remove(dst) // best-effort: a stale link from an aborted prior attempt at this ID
		if err := os.Link(src, dst); err != nil {
			return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: hardlink %s: %w", req.ID, name, err), cleanup())
		}
	}
	golden, err := os.ReadFile(filepath.Join(l.opts.SnapshotDir, chvSnapshotConfigFile))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: read golden config.json: %w", req.ID, err), cleanup())
	}
	rewritten, err := rewriteSnapshotConfig(golden, vsockSock, fsSock)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: rewrite config.json: %w", req.ID, err), cleanup())
	}
	perVMConfigPath := filepath.Join(runDir, chvSnapshotConfigFile)
	if err := os.WriteFile(perVMConfigPath, rewritten, 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: write per-VM config.json: %w", req.ID, err), cleanup())
	}

	// --- cloud-hypervisor itself. UNCHROOTED on this arm — see this file's
	// package-level comment (hardware-corrections C5, and Task 17's D3/D4
	// disposition of it just below that): the per-VM CGROUP half of that gap IS
	// closed here, via chvSystemdRunScopeArgv, when l.opts.ParentCgroup is set; the
	// filesystem-confinement half is a documented, reasoned non-closure, not a
	// silent one. Console output is captured to a file (not discarded, unlike
	// Firecracker's jailed stdout/stderr) specifically so a guest panic — e.g.
	// hardware-corrections C3's missing-`root=`-on-cmdline panic — surfaces in the
	// returned error instead of degrading into an opaque waitForUnixSocket/
	// ch-remote timeout indistinguishable from a wedged VMM. ---
	apiSock := filepath.Join(runDir, "api.sock")
	console, err = os.Create(filepath.Join(runDir, "console.log"))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: create console log: %w", req.ID, err), cleanup())
	}
	vmmArgv := []string{"--api-socket", apiSock}
	vmmBin := l.opts.CHVBin
	if l.opts.ParentCgroup != "" {
		// D3: wrap the real binary+args behind systemd-run --scope so this VM gets
		// its own cgroup under the same slice Firecracker's jailer targets, bounded
		// to the same PerVMBytes(cfg) figure. --scope execs cloud-hypervisor IN
		// PLACE of the systemd-run client (see chvSystemdRunScopeArgv's doc
		// comment), so vmmCmd.Process.Pid below is cloud-hypervisor's own pid, not
		// a detached systemd-managed process this handle can no longer kill —
		// Destroy's kill-and-reap contract is unchanged by this wrapping.
		vmmBin = l.opts.SystemdRunBin
		vmmArgv = append(chvSystemdRunScopeArgv(l.opts, "vm-"+req.ID), append([]string{l.opts.CHVBin}, vmmArgv...)...)
	}
	vmmCmd = exec.Command(vmmBin, vmmArgv...)
	vmmCmd.Stdout = console
	vmmCmd.Stderr = console
	chvIsolateAndDropPrivileges(vmmCmd, 0, 0) // Setpgid only: uid==0 is a no-op sentinel, see the helper's doc comment
	if err := vmmCmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: start cloud-hypervisor: %w", req.ID, err), cleanup())
	}
	if err := waitForUnixSocket(ctx, apiSock, 5*time.Second); err != nil {
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: API socket never appeared: %s: %w", req.ID, chvReadConsole(console.Name()), err), cleanup())
	}

	// ch-remote restore --source-url file://<per-VM run dir>, per the brief. CH's
	// vm.restore API (per the reference tutorial) exposes only source_url, resume,
	// and memory_restore_mode — no field-level override for vsock/fs socket paths
	// the way Firecracker's LoadSnapshot has vsock_override (fcapi.go) — which is
	// exactly why the per-VM directory above exists: there is no other restore-time
	// knob to redirect those paths.
	restoreOut, err := exec.CommandContext(ctx, l.opts.ChRemoteBin,
		"--api-socket", apiSock, "restore", "--source-url", "file://"+runDir,
	).CombinedOutput()
	if err != nil {
		combined := string(restoreOut)
		// hardware-corrections C4/C8: a disk-lock failure ("Error locking disk
		// images", "Failed to get Write lock") is the signature of a writable golden
		// rootfs under concurrent standbys, not a generic restore failure — name it
		// so a future reader who has not read the tutorial still recognises it.
		// (This launcher never sets readonly=off; if this fires, the golden
		// snapshot's own config.json's disks[].readonly is the place to check —
		// that is a build-pipeline concern, not this launcher's argv.)
		if chvLooksLikeDiskLockError(combined) {
			return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: disk-lock error (golden rootfs is not read-only? see hardware-corrections C4/C8): %s", req.ID, combined), cleanup())
		}
		// hardware-corrections C3: a guest that panics for lack of `root=` on the
		// cmdline produces exactly this symptom from ch-remote's point of view — a
		// failed restore/resume with no further detail — so console output is
		// included here rather than only in the API-socket-timeout path above.
		return nil, errors.Join(fmt.Errorf("cloud-hypervisor: restore %s: ch-remote restore: %w: %s (console: %s)", req.ID, err, combined, chvReadConsole(console.Name())), cleanup())
	}

	return &chvVM{
		id:        req.ID,
		key:       req.Key,
		vmmCmd:    vmmCmd,
		fsCmd:     fsCmd,
		runDir:    runDir,
		apiSock:   apiSock,
		vsockSock: vsockSock,
		vsockPort: l.opts.VsockPort,
		chRemote:  l.opts.ChRemoteBin,
		console:   console,
		fsConsole: fsConsole,
	}, nil
}

// chvReadConsole best-effort reads back the console log for inclusion in an error
// message. Never itself a source of a new failure: on any error it returns a
// placeholder string rather than propagating.
func chvReadConsole(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(console unavailable: " + err.Error() + ")"
	}
	if len(b) == 0 {
		return "(console empty)"
	}
	return string(b)
}

// chvLooksLikeDiskLockError recognises the exact error text hardware-corrections
// C4/C8 captured on real hardware for a write-locked disk image.
func chvLooksLikeDiskLockError(s string) bool {
	return containsAny(s, "Error locking disk images", "Failed to get Write lock", "AlreadyLocked")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// chvVM is one restored Cloud Hypervisor standby, holding both host processes
// (cloud-hypervisor and its own virtiofsd) and the per-VM socket paths
// rewriteSnapshotConfig baked into its own config.json copy.
type chvVM struct {
	id, key string

	vmmCmd    *exec.Cmd // cloud-hypervisor
	fsCmd     *exec.Cmd // virtiofsd
	runDir    string
	apiSock   string
	vsockSock string
	vsockPort uint32
	chRemote  string
	console   *os.File // cloud-hypervisor's stdout/stderr
	fsConsole *os.File // virtiofsd's stdout/stderr

	mu        sync.Mutex
	destroyed bool
}

func (v *chvVM) Key() string { return v.key }

// Resume unpauses the VM via ch-remote. Unlike the Firecracker arm's Resume, this
// does NOT mount anything: virtio-fs's workspace is already visible to the guest
// the moment the device is attached (baked into the golden snapshot's boot-time
// config, per this file's package comment), and there is nothing analogous to
// ext4's "re-read the device's metadata on every acquire" concern a shared host
// filesystem does not have.
func (v *chvVM) Resume(ctx context.Context) error {
	if err := v.checkNotDestroyed(); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, v.chRemote, "--api-socket", v.apiSock, "resume").CombinedOutput()
	if err != nil {
		return fmt.Errorf("cloud-hypervisor: resume %s: %w: %s (console: %s)", v.id, err, out, chvReadConsole(v.console.Name()))
	}
	return nil
}

// Run sends exactly one command over a fresh vsock connection. The command is
// UNWRAPPED — no mount, no sync — which is the one deliberate asymmetry with the
// Firecracker arm's Run/wrapCommand: virtio-fs means every write already lands on
// the HOST filesystem the moment the guest issues it (there is no guest page cache
// standing between the write and durability the way there is for the ext4
// workspace image), so a write in Exec N surviving into Exec N+1 needs no help
// from this launcher. This is spec §4.3's trade, not an omission — a reader
// diffing this against wrapCommand should not conclude sync was forgotten.
func (v *chvVM) Run(ctx context.Context, c Command, out Sink) (Result, error) {
	if err := v.checkNotDestroyed(); err != nil {
		return Result{}, err
	}
	conn, err := dialVsock(v.vsockSock, v.vsockPort)
	if err != nil {
		return Result{}, fmt.Errorf("cloud-hypervisor: run %s: dial vsock: %w", v.id, err)
	}
	return runOverConn(ctx, conn, c, out, time.Now())
}

// Destroy SIGKILLs the VMM first, then virtiofsd (per the brief), reaps both, and
// removes the per-VM run directory (which holds both sockets and the per-VM
// config.json copy). Idempotent, mirroring launcher_firecracker.go's Destroy.
//
// hardware-corrections C10: cloud-hypervisor's Linux `comm` field is truncated to
// "cloud-hyperviso" (15 chars), so pgrep/pkill -x cloud-hypervisor NEVER matches —
// a silent no-op that would report success having killed nothing. This is exactly
// why both processes are killed by the *os.Process handle this struct already
// holds, never by searching for a name.
func (v *chvVM) Destroy() error {
	v.mu.Lock()
	if v.destroyed {
		v.mu.Unlock()
		return nil
	}
	v.destroyed = true
	vmmCmd, fsCmd, console, fsConsole := v.vmmCmd, v.fsCmd, v.console, v.fsConsole
	v.mu.Unlock()

	var errs []error
	for _, cmd := range []*exec.Cmd{vmmCmd, fsCmd} { // VMM first, then virtiofsd
		if cmd == nil || cmd.Process == nil {
			continue
		}
		pid := cmd.Process.Pid
		if err := fcKillProcessGroup(pid); err != nil && !fcProcessNotFound(err) {
			errs = append(errs, fmt.Errorf("kill -%d: %w", pid, err))
		}
		_ = cmd.Wait() // reap; "signal: killed" is the expected outcome, not a failure
	}
	if console != nil {
		_ = console.Close()
	}
	if fsConsole != nil {
		_ = fsConsole.Close()
	}
	if err := os.RemoveAll(v.runDir); err != nil {
		errs = append(errs, fmt.Errorf("remove run dir %s: %w", v.runDir, err))
	}
	return errors.Join(errs...)
}

func (v *chvVM) checkNotDestroyed() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.destroyed {
		return fmt.Errorf("cloud-hypervisor: VM %s: used after Destroy", v.id)
	}
	return nil
}

// chvIsolateAndDropPrivileges puts cmd in its own process group (same reasoning as
// fcIsolateProcessGroup: fcKillProcessGroup's -pid kill must reach every process
// this one spawns, without also reaching this launcher's own group) and, when uid
// and gid are both nonzero, drops the child's privileges to that uid/gid before
// exec via SysProcAttr.Credential. uid==0 (used for the cloud-hypervisor process
// itself, which this arm does not run under a dedicated unprivileged account) is a
// no-op sentinel for "isolate only, do not touch credentials" — validate() already
// refuses uid/gid 0 for virtiofsd specifically, so this parameter combination is
// never used to smuggle a root virtiofsd past that check.
func chvIsolateAndDropPrivileges(cmd *exec.Cmd, uid, gid int) {
	chvIsolateAndDropPrivilegesPlatform(cmd, uid, gid)
}
