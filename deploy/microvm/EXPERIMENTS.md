# microVM sandbox: experiments and correctness gates

## Correctness gates (spec §8)

Spec §8: "none of these produces a number, and all are blocking." This section is
recorded first, before any performance rung, so a number is never read without its
caveat.

Ten gates plus the static privilege pin. `gates_kvm_test.go` implements all ten;
`gates_privilege_test.go` implements the privilege pin (needs no KVM, already existed).
Four of the ten gates exercise the Pool's own bookkeeping against the **fake**
launcher/clock (`internal/vmpool`'s existing test double) and need no KVM; the
remaining six (plus the `real_launcher` half of `TestGateNoVMReuse`) require
`/dev/kvm` and a built golden snapshot and were **not run** on this machine (a
darwin workstation with no KVM). Every row below reflects only what actually ran —
no PASS is recorded for a gate that did not execute against real hardware.

| Gate | Arm | Substrate | Date | Result |
| --- | --- | --- | --- | --- |
| `TestGateEmptyKeyIsRefused` | n/a (pre-Acquire refusal) | fake launcher + fake clock | 2026-09-12 | PASS |
| `TestGateNoVMReuse` (`fake_launcher_concurrency` subtest) | n/a | fake launcher + fake clock | 2026-09-12 | PASS |
| `TestGateParkedThenResumed` | n/a | fake launcher + fake clock | 2026-09-12 | PASS |
| `TestGateReclaimThenRedispatch` | n/a | fake launcher + fake clock | 2026-09-12 | PASS |
| `TestGateNoVMReuse` (`real_launcher` subtest) | firecracker / cloud-hypervisor | real KVM | — | NOT RUN — needs `/dev/kvm`; see rig command below |
| `TestGateWriteDurability` | firecracker / cloud-hypervisor | real KVM | — | NOT RUN — needs `/dev/kvm` and a built golden snapshot |
| `TestGateNoCrossRunBleed` | firecracker / cloud-hypervisor | real KVM | — | NOT RUN — needs `/dev/kvm` and a built golden snapshot |
| `TestGateSnapshotHoldsNoSecrets` | firecracker / cloud-hypervisor | real KVM | — | NOT RUN — needs `/dev/kvm` and a built golden snapshot |
| `TestGateLeakFreeTeardown` | firecracker / cloud-hypervisor | real KVM | — | NOT RUN — needs `/dev/kvm` and a built golden snapshot |
| `TestGateClock` | firecracker / cloud-hypervisor | real KVM | — | NOT RUN — needs `/dev/kvm` and a built golden snapshot |
| `TestGateOutputCapAtSource` | firecracker / cloud-hypervisor | real KVM | — | NOT RUN — needs `/dev/kvm` and a built golden snapshot |
| `TestNothingInTheWorkerPathSpawnsAShell` (privilege pin, static) | n/a | AST scan, no KVM | 2026-09-12 | PASS |
| `TestTheHostFakeCannotServeARealVMMConfig` | n/a | fake launcher | 2026-09-12 | PASS |

### Rig command for the six not-yet-run gates (both arms)

```bash
for vmm in firecracker cloud-hypervisor; do
  echo "== $vmm"
  SH_KVM=1 SH_VMM=$vmm SH_SNAPSHOT_IMAGE_DIR=/srv/snapshots/swebench-py311 \
    go test ./internal/vmpool/ -run TestGate -v -timeout 30m
done
```

Expected: PASS for every gate on both arms. This is what Task 18's brief requires and
what remains outstanding before this slice can be called done — **see the blocker
below for the cloud-hypervisor arm specifically.**

### Mutation-test evidence for the four gates that ran

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

### Known blocker: cloud-hypervisor snapshot naming mismatch (pre-existing, out of scope)

`launcher_chv.go`'s `Restore()` reads the golden snapshot directly out of
`SnapshotDir` under cloud-hypervisor's own native names — `config.json`,
`memory-ranges`, `state.json` (`launcher_chv.go` lines ~95-97, 478-486). But
`build-snapshot.sh`'s `lock_down()` ships the golden snapshot under the **unified**
names Firecracker and Cloud Hypervisor are meant to share on disk — `vmstate`,
`memfile`, plus `ch-config.json` for the CHV arm (`files=(vmstate memfile kernel
rootfs agent manifest.json)` at line 1087, `files+=(ch-config.json)` at line 1092,
written out at lines 1095-1099). `build-snapshot.sh`'s own restore-side jail setup
(`link_snapshot_file` calls at lines 1258-1260) renames `$OUT/ch-config.json` ->
`config.json`, `$OUT/vmstate` -> `state.json`, `$OUT/memfile` -> `memory-ranges` when
staging its own verification jail — but `launcher_chv.go`'s `Restore()` does **not**
perform this rename; it expects those CHV-native names to already exist directly
under `SnapshotDir`. On a rig whose `SH_SNAPSHOT_IMAGE_DIR` holds what
`build-snapshot.sh` actually produces, every cloud-hypervisor-arm gate that resumes a
real guest (`TestGateWriteDurability`, `TestGateNoCrossRunBleed`, the guest-`env` half
of `TestGateSnapshotHoldsNoSecrets`, `TestGateLeakFreeTeardown`, `TestGateClock`,
`TestGateOutputCapAtSource`, and the CHV half of `TestGateNoVMReuse`'s
`real_launcher` subtest) is expected to fail at `Restore()` with a "no such file"
error reading `config.json`/`memory-ranges`/`state.json`, not because the gate's
property is false, but because the two shipped pieces of Tasks 14-17 disagree on a
filename convention.

The memfile-grep half of `TestGateSnapshotHoldsNoSecrets` is unaffected: it reads
`$OUT/memfile` directly by that name, which exists under that name for **both** arms,
so that half is arm-independent and correct as written.

This is a pre-existing defect in code shipped by Tasks 14-17, not introduced or
touched by this task, and fixing it is out of this task's scope. It blocks the
cloud-hypervisor arm's rig run above until whoever owns Tasks 14-17 either makes
`build-snapshot.sh` ship CHV's snapshot under its native names (or a symlinked
alias), or makes `launcher_chv.go`'s `Restore()` read the unified names instead.

## Performance rungs

Not yet run. E10/E11 depend on the rig and a built golden snapshot, same as the
correctness gates above; see the rig command sections in those tasks' own briefs.
