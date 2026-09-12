# microVM sandbox: experiments and correctness gates

## Correctness gates (spec §8)

Spec §8: "none of these produces a number, and all are blocking." This section is
recorded first, before any performance rung, so a number is never read without its
caveat.

Ten gates plus the static privilege pin. `gates_kvm_test.go` implements all ten;
`gates_privilege_test.go` implements the privilege pin (needs no KVM, already existed).
Four of the ten gates exercise the Pool's own bookkeeping against the **fake**
launcher/clock (`internal/vmpool`'s existing test double) and need no KVM, and were
run and verified directly by this task (mutation evidence below). The remaining six
(plus the `real_launcher` half of `TestGateNoVMReuse`) require `/dev/kvm` and a built
golden snapshot; this task's own environment is a darwin workstation with neither, so
they were run on the rig by the fix-round-1 reviewer, on top of two fixes that landed
after this task's initial commit (the CH snapshot-naming translation, `00f7c11`, and
the guest-agent clock-latch removal below) plus the chroot/run-dir device fix in this
round. That run is reported, not independently re-executed by this agent — see
"Fix round 1" below for exactly what changed and why the rig run needed it.

| Gate | Arm | Substrate | Result |
| --- | --- | --- | --- |
| `TestGateEmptyKeyIsRefused` | n/a (pre-Acquire refusal) | fake launcher + fake clock | PASS (this task, mutation-verified) |
| `TestGateNoVMReuse` (`fake_launcher_concurrency` subtest) | n/a | fake launcher + fake clock | PASS (this task, mutation-verified) |
| `TestGateParkedThenResumed` | n/a | fake launcher + fake clock | PASS (this task, mutation-verified) |
| `TestGateReclaimThenRedispatch` | n/a | fake launcher + fake clock | PASS (this task, mutation-verified) |
| `TestGateNoVMReuse` (`real_launcher` subtest) | firecracker | real KVM, rig | PASS — reported, fix round 1 |
| `TestGateWriteDurability` | firecracker | real KVM, rig | PASS — reported, fix round 1 |
| `TestGateNoCrossRunBleed` | firecracker | real KVM, rig | PASS — reported, fix round 1 |
| `TestGateSnapshotHoldsNoSecrets` | firecracker | real KVM, rig | PASS — reported, fix round 1 |
| `TestGateLeakFreeTeardown` | firecracker | real KVM, rig | PASS — reported, fix round 1 |
| `TestGateClock` | firecracker | real KVM, rig | PASS — reported, fix round 1; **failed first**, correctly, see below |
| `TestGateOutputCapAtSource` | firecracker | real KVM, rig | PASS — reported, fix round 1 |
| `TestNothingInTheWorkerPathSpawnsAShell` (privilege pin, static) | n/a | AST scan, no KVM | PASS (this task) |
| `TestTheHostFakeCannotServeARealVMMConfig` | n/a | fake launcher | PASS (this task) |

The reported rig run exercised the **firecracker** arm only. The cloud-hypervisor arm's
snapshot-naming mismatch that used to block it here is fixed (`00f7c11`, see below), and
this round additionally fixed `chvOpts`'s `RunDir` to share a device with the snapshot
the same way `fcLauncher`'s `ChrootBase` now does — but nobody has run the ten gates
against the cloud-hypervisor arm on real KVM yet. Treat that arm as fixed-on-paper, not
verified, until someone runs the rig command below with `SH_VMM=cloud-hypervisor`.

### Rig command for the six KVM-only gates (both arms)

Two things the first rig run got wrong that are easy to repeat, so recorded here for
whoever runs this next:

1. **The suite must run as root.** `/proc/sys/fs/protected_hardlinks` is `1` and the
   golden snapshot's files are root-owned `0444`, so an unprivileged process cannot
   hardlink them into a jail; the jailer itself also needs root to chroot and to
   bind-mount `/dev/kvm`. Without `sudo`, every Firecracker gate fails EPERM. (The
   command below previously omitted `sudo` — that was a bug in this document, not in
   the gates.)
2. **`sudo` resets the environment.** `PATH` must explicitly include `/sbin` and
   `/usr/sbin` — `mkfs.ext4` lives there, and the launcher shells out to it to build
   each VM's workspace image — and every `SH_*` variable must be passed on the `sudo`
   command line itself rather than relying on `sudo -E`, which does not reliably
   survive a hardened `sudoers` policy.

```bash
for vmm in firecracker cloud-hypervisor; do
  echo "== $vmm"
  sudo env "PATH=/usr/sbin:/sbin:$PATH" \
    SH_KVM=1 SH_VMM=$vmm SH_SNAPSHOT_IMAGE_DIR=/srv/snapshots/swebench-py311 \
    go test ./internal/vmpool/ -run TestGate -v -timeout 30m
done
```

Expected: PASS for every gate on both arms. The firecracker arm is now reported green
end-to-end (fix round 1); the cloud-hypervisor arm has every known blocker cleared but
has not actually been run — see the note above.

### Fix round 1: chroot base / run dir must share a device with the snapshot

The first real rig run of the six KVM-only gates failed every Firecracker gate at
`Restore()`:

```
firecracker: restore vm-1: hardlink vmstate:
link /srv/snapshots/.../vmstate /tmp/TestGate.../root/vmstate: invalid cross-device link
```

Cause: `fcLauncher` (`launcher_firecracker_test.go`) set `ChrootBase: t.TempDir()`.
`t.TempDir()` honours `$TMPDIR`; on the rig `/tmp` is tmpfs while the golden snapshot
lives on `/srv` (ext4) — a different device. The jailer **hardlinks** (never copies)
every snapshot component into `ChrootBase/<id>/root/`, deliberately, so that N standby
VMs sharing one golden snapshot don't each duplicate a multi-hundred-MiB memfile. A
hardlink across devices is EXDEV, unconditionally, so this failed on every restore
regardless of permissions.

Fix: added `sameDeviceSiblingDir(t, snapshotDir)` to
`remote-worker/internal/vmpool/launcher_firecracker_test.go`, mirroring
`new_verify_dir()` in `build-snapshot.sh` (a `mktemp -d` sibling of the snapshot
directory's parent, on the same filesystem by construction, itself the fix for the
identical problem in the shell harness's own verify jail, commit `4059338`). It does
**not** `os.MkdirAll` the parent into existence — like `new_verify_dir()`'s `mktemp -d`,
a missing parent (e.g. `/srv/snapshots` itself absent) is a real misconfiguration and
should fail loudly rather than silently create a directory tree nobody asked for. An
`SH_CHROOT_BASE` env var overrides the parent outright for a rig with an unusual layout;
absent that, the default is now correct without the operator knowing anything. Cleanup
is via `t.Cleanup`, which — like `t.TempDir()`'s own guarantee — runs even on a failing
test, so a gate that fails partway through does not leak a jail (each holds a
hardlinked ~256 MiB memfile plus a workspace image; on a rig with a 31 GiB disk that
adds up fast across repeated runs).

`fcLauncher` is only ever called after `requireKVM(t)` at every call site in the
codebase, so this helper is never invoked — and never touches the filesystem, and never
requires `SnapshotDir` to exist — when `SH_KVM` is unset. The non-KVM path is therefore
unaffected by construction, not just by testing; the full non-KVM suite
(`go test ./internal/vmpool/... -race -count=1`) was re-run after this change and stays
green (4 gates PASS, the rest SKIP, 0 FAIL).

Cloud Hypervisor's `Restore()` (`launcher_chv.go`) has the structurally identical
exposure: it also hardlinks (`os.Link`) the golden `vmstate`/`memory-ranges` files from
`SnapshotDir` into `RunDir/<id>/`. `chvOpts(t)` (`launcher_chv_test.go`) set
`RunDir: t.TempDir()` — the same bug, just never exercised on the rig yet because the
reported run only covered the Firecracker arm. Fixed it the same way:
`RunDir: sameDeviceSiblingDir(t, snapshotDir)`, reusing the identical helper. This is
beyond the single item flagged in the fix-round request, done because the fix was
already written, generic, and the alternative was knowingly leaving an identical
landmine in the other arm.

**New test:** `TestSameDeviceSiblingDirSharesDeviceWithTarget`
(`launcher_firecracker_test.go`) is the assertion that would have caught the original
bug without any hypervisor — it stats `sameDeviceSiblingDir`'s output and the
snapshot directory's parent (`syscall.Stat_t.Dev`) and asserts they match. It stands in
a `t.TempDir()` for the snapshot directory rather than requiring the real
`SH_SNAPSHOT_IMAGE_DIR` to exist, so it runs everywhere the fake-substrate gates run.

**Mutation-test result — honest non-reproduction on this machine:** the intended
mutation is to point the assertion's "got" directory at a hardcoded `/tmp` instead of
`sameDeviceSiblingDir`'s real output, and confirm the assertion fails on a host where
`/tmp` is a separate filesystem from the snapshot directory. On this task's own darwin
development machine, `stat -f` shows `/tmp`, `$TMPDIR`, `/var/tmp`, and the repo's own
working directory all report the **same** device number (`16777234` — macOS mounts one
APFS volume for all of these under normal configuration). Applying the mutation
(`got := "/tmp"` in place of the `sameDeviceSiblingDir` call) left the test PASSing, not
failing, confirming this machine cannot exhibit the failure the assertion exists to
catch. The mutation was reverted immediately after (`git diff --stat` confirmed clean),
and the test re-run to confirm PASS on the real code path. This is exactly the situation
flagged as worth reporting honestly rather than claiming a green mutation result that
would not reproduce: on the rig, where `/tmp` is tmpfs and `/srv` is a separate ext4
device, this same mutation would be expected to fail the assertion — but that has not
been verified by this agent on real rig hardware, only reasoned from the `df`-visible
device split the coordinator's own bug report already demonstrated.

### `TestGateClock` caught a real defect on its merits

The rig's first run of `TestGateClock` failed — correctly. It caught a `clockOK`
one-shot latch in the guest agent that had been baked into the golden snapshot's own
memory image: every VM restored from that snapshot believed its clock had already been
corrected, because the snapshot was taken *after* the guest agent's real boot-time clock
fix had already run once and set the latch. Fixed upstream in `66fdf86` (remove the
latch entirely — restoring a paused VM's clock correction must not depend on in-memory
state captured before the snapshot, since that state is exactly what gets replayed
unconditionally on every restore). This is precisely the class of defect spec §8's gates
exist to find and a launcher-level test never would have: it is a property of the
golden snapshot's captured memory, not of the launcher or the pool.

### Mutation-test evidence for the four gates that ran directly under this task

Each of the four fake-substrate gates above was verified by breaking the exact
production code path it exists to catch, observing the gate FAIL with a specific
message, reverting the change (`git diff --stat` confirmed clean on the mutated file
each time), and observing the gate PASS again:

- **`TestGateNoVMReuse`** — mutated `pool.go`'s `destroy()` to append the
  about-to-be-destroyed VM back onto the run's standby pool and skip the real
  `vm.Destroy()` call (simulating a "recycle the handle instead of destroying it"
  regression). Observed failure: `concurrent Exec: spawn-failure: resume "run-a":
  fakeVM vm-3: Resume twice`. Reverted; gate passed again.
- **`TestGateEmptyKeyIsRefused`** — mutated `workspace.go`'s `checkKey` to skip the
  empty-key check. Observed failure: `ReasonOf(err) = invalid-workspace-key, want
  empty-workspace-key` (the empty string fell through to the regex check instead of
  being refused for being empty). Reverted; gate passed again.
- **`TestGateParkedThenResumed`** — mutated `sweep.go`'s `sweepOnce` so the
  `WorkspaceIdle` branch (drop the workspace) fires at `StandbyIdle` too instead of
  only parking. Observed failure: `ColdAcquires[ColdParked] rose by 0, want 1 — a
  parked-then-resumed run must be a cold acquire` (the run's map entry was deleted
  outright, so the next Exec was classified as a first-exec cold acquire, not a
  parked one). Reverted; gate passed again.
- **`TestGateReclaimThenRedispatch`** — mutated `pool.go`'s `Reclaim` to skip its
  final `removeWorkspace(dir)` call. Observed failure: `workspace .../gate-reclaim
  survived Reclaim: err=<nil>`. Reverted; gate passed again.

### Resolved: cloud-hypervisor snapshot naming mismatch

This section originally documented a blocker found while writing the gates:
`launcher_chv.go`'s `Restore()` expected the golden snapshot's cloud-hypervisor-native
filenames (`config.json`, `memory-ranges`, `state.json`) directly under `SnapshotDir`,
but `build-snapshot.sh` ships it under the unified names (`vmstate`, `memfile`,
`ch-config.json`) shared with the Firecracker arm, with no rename in between — every
cloud-hypervisor-arm restore was expected to fail with "no such file," not because any
gate's property was false, but because the two pieces disagreed on a filename
convention.

**Fixed in `00f7c11`** ("translate golden snapshot names to CH-native names in
Restore"), which landed after this task's initial commit and before this fix round.
This agent has not independently re-verified the fix against real cloud-hypervisor
hardware (the reported rig run covered the Firecracker arm only — see above); it is
recorded here as resolved based on the commit itself and the coordinator's account,
not a fresh gate run against CHV.

## Performance rungs

Not yet run. E10/E11 depend on the rig and a built golden snapshot, same as the
correctness gates above; see the rig command sections in those tasks' own briefs.
