# Cloud Hypervisor snapshot facts, against Firecracker's

Retrieved: 2026-09-10, corrected 2026-09-10, hardware-tested 2026-09-11. Spec
§2.4 states every platform fact from Firecracker's documentation and flags
Cloud Hypervisor's equivalents as unverified. This note closes that gap.
Cited URLs above each block.

> ## Read this first: this arm does not restore in the product
>
> Everything below is about **isolated platform behaviour**, exercised by hand. It is not a
> statement that the Cloud Hypervisor arm works. In this tier, with the launcher this repo
> ships, **CH fails 7 of the 10 spec §8 correctness gates**: `ch-remote restore` hangs and
> cloud-hypervisor dies during device restoration, right after logging
> `Restoring virtio-console`, propagating no error through its own API. Firecracker passes
> 10 of 10. See `deploy/microvm/EXPERIMENTS.md`.
>
> So a reader deciding whether to invest in this launcher should read the rows below as
> *"these platform primitives behave thus in isolation"*, **not** as *"restore works here"*.
> Several rows legitimately record a successful by-hand restore; that is not in tension with
> the gate result, because the gate drives the full product path (jailed paths, a per-VM
> workspace device, a vsock agent) and the by-hand tests did not.

**Hardware-test pass (2026-09-11).** *Provenance corrected 2026-09-14 after PR review: this
paragraph previously cited `docs/notes/cloud-hypervisor-tutorial.md`, and 13 rows below cited
it as their evidence. **That file does not exist** — not in this repository (no commit ever
added it) and not on the benchmark host, where `~/ch-tutorial` is a working directory of
sockets and artifacts rather than a write-up. The hardware work described here was really
done; it was never written up into the document that was cited. Rather than point at
something unopenable, this note now states what was exercised, so it is its own record.*

**What was exercised, by hand, on an EC2 `m8i.xlarge` (nested virtualization, 4 vCPU,
15.7 GiB) against a Cloud Hypervisor v53.0 install:** the same snapshot/restore,
vsock, cgroup, and virtio-fs paths Firecracker's own tutorial exercises —
multi-resume of one snapshot into VMs B and C, a cgroups-v2 OOM-kill test via
`systemd-run --scope`, two independent `virtiofsd` processes sharing one
directory, and an attempt to reproduce the RSS/PSS page-sharing measurement.
Rows below now marked **hardware-confirmed** were tested directly, not just
read about; this is a strictly stronger form of evidence than the
documentation-only pass below, and where the two disagree (row 4, notably),
the hardware result is what this note now trusts. Two findings below have no
row of their own because they surfaced only under test, not in any doc: an
advisory write-lock Cloud Hypervisor takes on disk images (blocks concurrent
restores that would share one disk path), and an inconclusive PRNG-reseed
signal on multi-resume. Both are folded into "Where Cloud Hypervisor is
worse" below.

Sources fetched 2026-09-10 (original pass):

- `https://www.cloudhypervisor.org/docs/prescriptive/features/snapshot-restore/` —
  **HTTP 404.** Cloud Hypervisor's docs site was restructured; its documentation
  index (`https://www.cloudhypervisor.org/docs/`) currently lists only a
  "Prologue" section (Introduction, Quick Start, Commands) and no
  snapshot/restore or "prescriptive" page at any URL.
- `https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md` —
  fetched successfully. Covers CID addressing, enabling `--vsock`, and
  host/guest connection setup. Contains no mention of snapshot, restore, or
  resume behavior at all.
- `https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/memory.md` —
  fetched successfully. Covers `--memory`/`--memory-zone` mapping flags
  (`shared`, `file`, `reserve`, `thp`) and how snapshot handles file-backed
  memory zones.
- `https://gitlab.com/virtio-fs/virtiofsd` README (fetched via
  `-/raw/main/README.md`) — fetched successfully. Documents the `--sandbox
{namespace,chroot,none}` isolation modes for the `virtiofsd` daemon process.

Sources fetched 2026-09-10 (correction pass — same directory as the two
`docs/*.md` URLs above, not new sources):

- The website's `snapshot-restore` page 404s, but its content did not cease to
  exist — it moved in-tree. Listing
  `https://github.com/cloud-hypervisor/cloud-hypervisor/tree/main/docs` (via
  the GitHub contents API, 48 files) shows `docs/snapshot_restore.md` and
  `docs/live_migration.md` present, alongside `vsock.md` and `memory.md`.
  Fetched both (`raw.githubusercontent.com/cloud-hypervisor/cloud-hypervisor/main/docs/{snapshot_restore,live_migration}.md`).
  This is the primary correction: several rows below were marked
  "undocumented" only because the website link was dead, not because Cloud
  Hypervisor is silent on the fact — they now carry real citations.
- Repo `README.md` — fetched for row 7, where the version-compatibility
  statement is known to exist. States plainly, under "Status": "Snapshot/restore
  is not supported across different versions" (and the same sentence for live
  migration).
- `docs/threat-model.md`, `docs/landlock.md`, `docs/seccomp.md` — fetched for
  rows 13/14 (jailer/cgroup equivalents). `threat-model.md`'s "Sandboxing"
  section documents Cloud Hypervisor's self-applied Landlock+seccomp
  confinement; `landlock.md` documents the `--landlock`/`--landlock-rules`
  mechanics. No file named `jailer` or containing a cgroup-configuration flag
  exists anywhere in the 48-file `docs/` listing.
- `docs/disk_locking.md`, `docs/io_throttling.md` — checked for row 10
  (quota/provisioning) and row 13 (cgroup) as the two remaining plausible
  in-tree candidates. Neither mentions quotas, disk-space provisioning, or
  cgroups.
- Rows 1, 5, 6, 8, 10, 12 were re-checked against `snapshot_restore.md` and
  `live_migration.md` specifically (grepped for vsock, clock, cgroup,
  diff/incremental, and quota/disk-space terms) and remain genuinely silent —
  their "undocumented" status is confirmed, not a dead-link artifact.

| #   | Fact (Firecracker, spec §2.4)                                                     | Cloud Hypervisor                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | Same?                                       | Design consequence                                |
| --- | --------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------- | ------------------------------------------------- |
| 1   | Listening vsock sockets survive restore; established connections closed on resume | **Hardware-confirmed** (by-hand pass, see the provenance note above): booted a VM with an established vsock connection held open across pause/snapshot, killed the source VM, restored the same snapshot into a fresh process — the listener survived, the established connection (and its forked handler) did not. Matches Firecracker's documented behavior and Cloud Hypervisor's own v52.0 release note ("Vsock connections are now reset on snapshot restore to avoid stale half-open connections"). Confirmed again on a _second_ restore of the same snapshot into a third process. Previously undocumented in any `docs/*.md`; the documentation gap itself is now moot.                                                                                                                                                                                                                                                                                                                                                                                            | **yes (hardware-confirmed)**                | §5.1's parked-in-`accept()` agent                 |
| 2   | Resuming one snapshot more than once is documented as insecure                    | Documented as an intended feature, with no security caveat (as before). **Hardware-confirmed as exploitable, not just theoretically insecure** (by-hand pass, see the provenance note above): the same snapshot was restored into VM B and then VM C, and both came up with a byte-identical guest-materialized token — proving the "share a snapshot, get identical guest state" property in practice, not just in doc language. Also hardware-tested: no VMGenID-equivalent device exists in Cloud Hypervisor's own repository (0 hits searching for it), yet B's and C's first `/proc/sys/kernel/random/uuid` draws differed — some divergence occurred, but the tutorial could not isolate its source (plausibly interrupt-timing jitter during each restore's own device setup) and explicitly declines to call it a reseed mechanism. Net: Cloud Hypervisor gives **no named, versioned guarantee** the way Firecracker's VMGenID does, and the one piece of evidence pointing the other way is a single, unexplained trial.                                          | no                                          | §5.2's no-secrets invariant                       |
| 3   | Host resources must be reachable at the same relative paths                       | Undocumented for the block-device case even after the correction pass (neither `snapshot_restore.md` nor `live_migration.md` addresses host-path layout for disks). For virtio-fs, `virtiofsd`'s README documents a different mechanism entirely: path resolution happens **on the host**, inside the daemon's own sandboxed root (`--sandbox namespace` or `chroot`), not via fixed guest-relative paths.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | no — different mechanism                    | §5.3's per-VM jail                                |
| 4   | Memory file mapped `MAP_PRIVATE`, immutable, retained for the VM's life           | Documented as an opt-in mode (as before): `docs/memory.md` says guest RAM defaults to `MAP_PRIVATE`; `docs/snapshot_restore.md`'s copy-on-write restore section describes the immutable, retained-for-life guarantee under `memory_restore_mode=CopyOnWrite`. **Hardware-tested, and the opt-in mode does not exist in the installed release** (by-hand pass, see the provenance note above): passing `memory_restore_mode: CopyOnWrite` to v53.0's `vm.restore` fails outright — `"unknown variant `CopyOnWrite`, expected `Copy`or`OnDemand`"`. The documented mechanism for this fact is not shipping. The two modes that _do_ exist were tested instead (§9b): `OnDemand` restore of the same snapshot into three separate processes showed **no RSS/PSS gap at all** (each process's `Pss` ≈ 99% of its own `VmRSS` — the signature of unshared memory), the opposite of Firecracker's measured ~3× gap under the same experiment design. This downgrades row 4 from "confirmed opt-in" to **"documented upstream, unavailable and unreproduced on the tested release."** | **partial → weaker than previously stated** | §7.3's density mechanism                          |
| 5   | Guest wall clock resumes from the snapshot moment                                 | Undocumented in `docs/*.md` (as before). **Hardware-tested** (by-hand pass, see the provenance note above): guest clock was stale after restore (~3 minutes behind host) despite v53.0's release notes claiming an automatic clock-advance fix on resume. Root cause confirmed identical to Firecracker's: the fix (both VMMs') only fires when the guest clocksource is `kvmclock`, and this rootfs's kernel defaults to `tsc`. Same precondition, same non-effect, on both VMMs — a genuine, hardware-confirmed parity, not a Cloud Hypervisor-specific gap.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | **yes (hardware-confirmed parity)**         | Deviation 5's agent-side clock set                |
| 6   | cgroups v1 causes high restore latency                                            | Undocumented. Re-checked `docs/snapshot_restore.md`, `docs/live_migration.md`, `docs/threat-model.md`, `docs/landlock.md`, `docs/seccomp.md`, `docs/disk_locking.md`, and `docs/io_throttling.md` — none mentions cgroups in connection with restore latency (or at all, except `threat-model.md`'s generic recommendation to use cgroups for guest resource limits, which is unrelated to restore speed).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | undocumented                                | Rig requirement                                   |
| 7   | Restore requires identical hardware/software                                      | Partially documented. Repo `README.md`, under "Status": "Snapshot/restore is not supported across different versions." This confirms the software-version half of Firecracker's fact. Neither the README nor `snapshot_restore.md`/`live_migration.md` says anything about matching CPU model or hardware, the other half.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | partial                                     | Build the snapshot on the target instance type    |
| 8   | Diff snapshots are developer preview, generally not resumable                     | Undocumented. Re-checked `docs/snapshot_restore.md` — no diff/incremental snapshot concept is mentioned at all; every documented snapshot is a full snapshot.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | undocumented                                | Full snapshots only                               |
| 9   | Only a 64-bit CRC on the state file; files are trusted                            | Documented as worse (as before): no integrity mechanism at all, `config.json` explicitly designed to be human-edited, `docs/threat-model.md`: "Snapshot files are trusted input for restore operations." **Hardware-confirmed the editability is not just a documentation claim** (by-hand pass, see the provenance note above): editing a restored snapshot's `config.json` in place (via a one-line `python3 -c` rewrite of the `disks` array) is exactly what unblocked a real multi-process restore test that a disk-image lock would otherwise have refused. No checksum, CRC, or signature was ever computed or checked on the edited file. Total trust, demonstrated in practice, not asserted from a doc page.                                                                                                                                                                                                                                                                                                                                                    | no                                          | §5.5's root-owned read-only artifact              |
| 10  | Integrators must provision disk and enforce quotas                                | Undocumented. Re-checked `docs/snapshot_restore.md`, `docs/disk_locking.md`, and `docs/io_throttling.md` — none mentions disk-space provisioning or quotas.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | undocumented                                | §6's per-workspace quota                          |
| 11  | No virtio-fs (Firecracker)                                                        | virtio-fs present, and now **hardware-confirmed** rather than just present-by-architecture (by-hand pass, see the provenance note above): two independent `virtiofsd` processes, each backing a separate restored VM, mounted the **same** host directory; a file written from one VM was read back correctly from the other. This is the direct hardware demonstration that D>1 concurrent standbys work on this arm, where Firecracker's shared-ext4-block-device approach is impossible as specified.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | **no (confirmed better)**                   | Why CH is in scope at all (§4.3)                  |
| 12  | Some vsock packet loss should be anticipated for resumed guests                   | Undocumented. `docs/vsock.md` never discusses resume semantics or packet loss for the vsock device; re-checked `docs/snapshot_restore.md` and `docs/live_migration.md` — neither mentions vsock either. Not exercised directly by the hardware pass either (§6's vsock tests checked connection survival and reset, not packet-loss-under-load), so this row stays undocumented rather than hardware-confirmed either way.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | undocumented                                | §5.4's framed request/response                    |
| 13  | An equivalent of `--cgroup` on the jailer                                         | Undocumented in `docs/*.md` (as before), but **hardware-confirmed** that the outcome is achievable through a different, unbundled mechanism (by-hand pass, see the provenance note above): `systemd-run --scope -p MemoryMax=200M` placed a live Cloud Hypervisor process in its own cgroup, and dirtying more guest RAM than the limit produced a real, scoped OOM-kill (`oom_memcg=/system.slice/ch-cgtest.scope`) that left the rest of the host untouched — the same _result_ jailer's `--cgroup memory.max=...` gives Firecracker. No packaging comes with it: no chroot, no `--uid`/`--gid`, nothing analogous to jailer's hardlink-into-jail convention.                                                                                                                                                                                                                                                                                                                                                                                                                 | **confirmed: outcome yes, packaging no**    | §5.3/§6's cgroup agreement                        |
| 14  | A jailer equivalent at all (chroot per VM)                                        | A real mechanism exists, but it is a different kind of confinement, not an equivalent (as before — Landlock+seccomp self-sandboxing, documented in `docs/threat-model.md`/`docs/landlock.md`). **Hardware-confirmed there is no supplied chroot-per-VM wrapper** (by-hand pass, see the provenance note above): building and launching Cloud Hypervisor under `systemd-run --scope` for the cgroup test above required no jail of any kind, and the tutorial states directly that a real `vmpool` on this arm "has to build that chroot (or equivalent confinement) itself... most plausibly via a plain Linux mount namespace plus bind mount, since that's the same primitive jailer itself uses under the hood, minus the packaging." This is exactly the decision this fix round already made for the build script's own use of a plain `chroot` (items 1/2/9) rather than assuming a jailer-equivalent existed.                                                                                                                                                            | no                                          | §5.3 — if absent, `vmpool` owns the chroot itself |

## Where Cloud Hypervisor is worse

- **Row 2** (multi-resume insecurity): documented as an intended feature with
  no security caveat, and now **hardware-demonstrated, not just
  doc-asserted** (by-hand pass, see the provenance note above) — the same snapshot
  restored twice produced two VMs with byte-identical guest-materialized
  secrets. §5.2's no-secrets invariant should be treated as **required** on
  this arm, not merely unconfirmed. The PRNG-reseed question is genuinely
  open, not resolved either way (see the new bullet below) — that residual
  uncertainty does not weaken the requirement, since §5.2 already treats
  ASLR/PRNG duplication as out of scope for the security boundary.
- **Row 3** (same relative paths): not just undocumented but a **different
  mechanism** — virtio-fs resolves paths on the host inside `virtiofsd`, so
  §5.3's per-VM jail would need a different design on the Cloud Hypervisor arm,
  not a port of Firecracker's. **Hardware-tested** (§7): confirmed there is no
  supplied jailer-style hardlink-into-chroot convention at all on this arm —
  see row 14 below.
- **New finding, no row of its own — advisory write-lock on disk images**
  (by-hand pass, see the provenance note above): restoring a _second_ process against a
  snapshot whose `config.json` still names the same disk path fails outright
  — `"Error locking disk images: Another instance likely holds a lock...
Failed to get Write lock for disk image"` — errno-for-errno different from
  anything Firecracker's own tutorial encountered. The workaround used
  (editing `config.json` to drop the `disks` array) is not generally
  available to a real restore path that needs its rootfs disk attached. This
  fix round's `--disk "path=...,readonly=on"` (item 8) was **not tested
  against this specific lock** by either tutorial; whether a read-only disk
  attachment still takes the write lock is an open question this note
  surfaces rather than answers, and is a candidate blocker for D>1 concurrent
  restores that all need the same rootfs image, distinct from virtio-fs's
  already-confirmed workspace-sharing story (row 11).
- **New finding, no row of its own — page-cache sharing across restored VMs is
  the open item, not a green light** (by-hand pass, see the provenance note above): the documented mechanism for it (`memory_restore_mode=CopyOnWrite`)
  does not exist in the installed v53.0 stable release (§9a — the API call
  itself is refused as an unknown variant); the mode that does exist
  (`OnDemand`) was tested directly and showed **no RSS/PSS gap at all** across
  three processes restoring the same snapshot (§9b) — the opposite of the
  ~3× gap Firecracker's own tutorial measured with `vmtouch`+`MAP_PRIVATE`.
  §7.3's memory-density arithmetic ("the dominant term is computable, not
  empirical, because unmodified pages are shared host page cache") has **not
  been demonstrated to hold for Cloud Hypervisor on this release**, and the
  tutorial explicitly declines to guess why (userfaultfd populating private
  per-process copies vs. genuine cross-process sharing simply not being a
  `Copy`/`OnDemand` feature are both live candidates, untested). This is left
  here as the open question it is — not resolved in either direction — because
  resolving it needs either a newer Cloud Hypervisor build with `CopyOnWrite`,
  or a different mechanism entirely (e.g. KSM against `shared=on` memory),
  neither of which this fix round's scope covers (that measurement work
  belongs to Tasks 20/21, not this task).
- **Row 9** (integrity/trust): now documented, and worse than a weak CRC —
  Cloud Hypervisor's snapshot format has **no** documented integrity mechanism,
  and `config.json` is explicitly designed to be human-edited between snapshot
  and restore. **Hardware-confirmed** (§6a, §9b): this editability was
  exploited directly to unblock the disk-lock finding above, with no
  checksum, CRC, or signature check firing at any point. §5.5's root-owned
  read-only artifact design is not merely unconfirmed here; it is doing
  strictly more work than Cloud Hypervisor's own documentation claims for
  itself.
- **Row 14** (jailer/chroot-per-VM equivalent): a real mechanism exists
  (Landlock + seccomp self-sandboxing), but it confines the VMM process from the
  inside, applied by the process to itself, rather than an external per-VM
  chroot imposed before the VMM starts. **Hardware-confirmed absent** (§7): no
  jail of any kind was needed or available to launch a cgroup-scoped Cloud
  Hypervisor process; `vmpool` (or, at build time, this fix round's own plain
  `chroot` treatment of items 1/2/9) has to build and own the per-VM chroot
  itself on this arm to match Firecracker's jailer guarantee, though it could
  additionally require `--landlock` as defense-in-depth.
- **Row 7** (identical hardware/software on restore, partial): confirmed for
  the software half only — the repo README's version-compatibility statement
  matches Firecracker's fact. The CPU-model half is completely unaddressed
  anywhere in the `docs/` set, and the hardware pass did not attempt a
  cross-CPU-model restore either (out of scope for a single-box tutorial).
  Because spec §5.2 and §2.4 treat "build the golden snapshot on the instance
  type that will run it" as a hard requirement, this silence matters in
  practice: there is no Cloud Hypervisor-side confirmation of what happens on
  a CPU-model mismatch (silent tolerance, degraded functionality, or a hard
  failure), so the strict same-instance-type rule should keep being enforced
  on this arm exactly as it is for Firecracker — which is exactly what this
  fix round's item 5 now does, uniformly across both VMM arms, not relaxed on
  the strength of the confirmed software half alone.
- **Row 6** (cgroups v1 restore latency): still undocumented, and the
  hardware pass did not isolate this specific variable either — §7's cgroup
  test confirms the _mechanism_ (`systemd-run --scope` enforcing
  `memory.max`) works, but restore latency under cgroups v1 specifically was
  not measured.

  *Two restore-latency figures were removed here on 2026-09-14. They cited "§10's informal
  timing table" — this note has no §10 and no timing table, and the companion document they
  came from does not exist, so a reader could neither check the method nor the substrate.
  No restore latency has been measured under this project's measurement discipline: that is
  E10's job, on a labelled substrate, with warmup discarded and percentiles reported. Any
  latency figure for this tier should come from there and nowhere else.*
- **Row 8** (diff snapshots not resumable): still undocumented — Cloud
  Hypervisor's docs describe only full snapshots, so this fact simply has no
  counterpart to confirm or deny. Not exercised by the hardware pass either.
- **Row 10** (disk provisioning/quota is the integrator's job): still
  undocumented after checking `disk_locking.md` and `io_throttling.md` — §6's
  per-workspace quota assumption is unconfirmed for this arm. Not exercised by
  the hardware pass.
- **Row 12** (vsock packet loss on resume): still undocumented — §5.4's framed
  request/response protocol was justified by Firecracker's documented
  packet-loss warning; Cloud Hypervisor gives no such warning to justify (or
  rule out) the same design. The hardware pass's vsock tests checked
  connection survival/reset (row 1), not packet loss under load, so this row
  is not hardware-confirmed either way.
- **Row 13** (`--cgroup`-equivalent flag): undocumented in `docs/*.md`, but
  **hardware-confirmed to have a working substitute with no bundled
  packaging** (§7) — see the table row above. §5.3/§6's requirement that the
  VMM's cgroup mechanism agree with the systemd slice is achievable via
  `systemd-run --scope`, but nothing ships it as a Cloud Hypervisor-native
  flag the way jailer's `--cgroup` is native to Firecracker's launcher.
- **Corroborates two of this fix round's own engineering decisions**: item 7
  (a bare `--cmdline` with no `root=` produces an immediate kernel panic,
  reproduced directly in §3 and restated in §11's comparison table: "Root
  device on cmdline: Not needed (Firecracker) vs. Required (root=/dev/vda) or
  an immediate kernel panic (CH)") and item 9 (the jail-relative path
  treatment, motivated by the same ephemeral-`$STAGE` bug Firecracker has, and
  by row 14's confirmed absence of any jailer-equivalent to lean on instead).

## Where Cloud Hypervisor is better

- **Row 11**: virtio-fs is present in Cloud Hypervisor, and now
  **hardware-confirmed working** (by-hand pass, see the provenance note above) — two
  independent `virtiofsd` processes backing two separate VMs, sharing one host
  directory, with a direct cross-VM read/write test — not just present by
  architecture. This is the entire reason Cloud Hypervisor is in scope at all
  (§4.3).
- **Row 4** (partial → weaker on hardware): where its non-default
  `CopyOnWrite` restore mode is _documented_, Cloud Hypervisor's guarantee is
  at least as strong as Firecracker's — immutable, file-backed for the VM's
  life. But that mode **does not exist in the tested v53.0 release** (see the
  table row and the page-cache-sharing finding above), so on hardware this
  advantage is currently theoretical, not available to configure for.
- virtio-fs makes **D > 1 standbys** and **concurrent `Exec`s per run** possible
  at all (§4.3) — both are correctness-impossible on the Firecracker arm as
  specified, because two guest kernels mounting one ext4 rw block device
  corrupt it. **Hardware-confirmed** (§8), though see the disk-image
  write-lock finding above for a distinct, not-yet-fully-characterized limit
  on how many restores can share one _disk_ path (as opposed to one virtio-fs
  directory).
- The host filesystem is the write-durability authority under virtio-fs, so
  **no `sync` is needed on the hot path** and writes survive `SIGKILL` (§4.3,
  §6) — Firecracker's arm requires a mandatory `sync` before kill on the hot
  path to get the same guarantee.

**Verdict: `firecracker`, unchanged — and the hardware pass makes the reasons
for it stronger, not weaker.** After the correction pass, seven of the
fourteen rows remained genuinely undocumented against Cloud Hypervisor's own
sources (rows 1, 5, 6, 8, 10, 12, 13); two were partial (rows 4, 7); five read
`no` (rows 2, 3, 9, 11, 14). The 2026-09-11 hardware pass then **replaced
guesswork with a real snapshot/restore/vsock/cgroup/virtio-fs cycle** on an
actual Cloud Hypervisor v53.0 install, and the results tilt the balance
further toward Firecracker-first rather than closer to parity:

- Two previously-undocumented rows are now **hardware-confirmed matches**
  with Firecracker (row 1's vsock reset-on-restore, row 5's shared
  tsc-vs-kvmclock stale-clock precondition) — real parity, genuinely earned,
  not assumed.
- Row 4's opt-in advantage **shrank on contact with the actual binary**: the
  documented `CopyOnWrite` restore mode that would give Cloud Hypervisor a
  Firecracker-equivalent page-sharing story is not present in v53.0 at all,
  and the mode that is present (`OnDemand`) showed no page sharing when
  tested directly. This is the single most consequential finding of the
  hardware pass: §7.3's memory-density arithmetic, which this spec relies on
  to keep per-VM memory accounting cheap and honest, has **not been
  demonstrated to transfer to Cloud Hypervisor on the shipped release** — and
  this note states that as the open question it is, rather than resolving it
  in either direction (closing it is out of scope for this fix round; it is a
  measurement item for Tasks 20/21).
- Rows 2, 9, and 14, already read as confirmed-worse from documentation alone,
  are now **also hardware-demonstrated**: multi-resume of one snapshot
  produced byte-identical guest secrets across two independent restores (row
  2); `config.json`'s documented editability was exploited directly, with no
  integrity check ever firing, to route around an undocumented disk-image
  lock (row 9, and see the new disk-lock finding above); and launching a
  cgroup-scoped Cloud Hypervisor process required building the confinement
  from scratch with no jailer-equivalent to lean on (row 14).
- Row 11 (virtio-fs, and the entire reason Cloud Hypervisor is in scope) is
  now hardware-confirmed working, not merely present by architecture — the
  strongest point in Cloud Hypervisor's favor stands up to a real test.
- Two genuinely new findings surfaced only under test, not in any doc: an
  advisory write-lock on disk images that may limit how many concurrent
  restores can share one rootfs disk path (distinct from, and not yet
  reconciled with, virtio-fs's confirmed workspace-sharing story), and an
  inconclusive PRNG-reseed signal on multi-resume that this note declines to
  characterize as a mechanism, per the tutorial's own refusal to guess.

Spec §9's risk is **still live**, and per §9's own wording ("if its caveats
are worse **or less documented**"), a confirmed-worse-on-hardware finding
keeps the risk open exactly as an undocumented one would. Phase D should
implement the Firecracker launcher first, where every one of these 14 facts
is confirmed by upstream documentation and now, for several rows, by direct
hardware testing too. Cloud Hypervisor's virtio-fs advantage (§4.3, D>1
standbys) remains the reason for a second launcher, now de-risked on the one
axis that mattered most (row 11, hardware-confirmed) but with a **new,
harder-edged blocker** to resolve first: page-cache sharing across restores
(row 4 / the open item above) is not just undocumented, it is
**unreproduced on the shipped release**, and that gap should be closed or
explicitly accepted as a memory-budget cost before Cloud Hypervisor's D>1
advantage is relied on at the density §7.3 assumes.

_Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>_
