# Cloud Hypervisor snapshot facts, against Firecracker's

Retrieved: 2026-09-10, corrected 2026-09-10. Spec §2.4 states every platform
fact from Firecracker's documentation and flags Cloud Hypervisor's equivalents
as unverified. This note closes that gap. Cited URLs above each block.

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

| #   | Fact (Firecracker, spec §2.4)                                                     | Cloud Hypervisor                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  | Same?                    | Design consequence                                |
| --- | --------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------ | ------------------------------------------------- |
| 1   | Listening vsock sockets survive restore; established connections closed on resume | Undocumented. `docs/vsock.md` describes CID addressing and connection setup but says nothing about restore/resume behavior for listening or established sockets. Re-checked `docs/snapshot_restore.md` and `docs/live_migration.md` (the in-tree location the 404'd website page moved to) — neither mentions vsock at all. Genuinely silent, not a dead-link artifact.                                                                                                                                                                                                                                                                                                                                                           | undocumented             | §5.1's parked-in-`accept()` agent                 |
| 2   | Resuming one snapshot more than once is documented as insecure                    | Documented as an intended feature, with no security caveat. `docs/snapshot_restore.md`: a snapshot "can be used as the base for creating new identical virtual machines, without the need to boot them from scratch," and its copy-on-write restore mode is described so "many VMs restored from the same snapshot share it." No warning against reuse appears anywhere in `snapshot_restore.md` or `live_migration.md`.                                                                                                                                                                                                                                                                                                          | no                       | §5.2's no-secrets invariant                       |
| 3   | Host resources must be reachable at the same relative paths                       | Undocumented for the block-device case even after the correction pass (neither `snapshot_restore.md` nor `live_migration.md` addresses host-path layout for disks). For virtio-fs, `virtiofsd`'s README documents a different mechanism entirely: path resolution happens **on the host**, inside the daemon's own sandboxed root (`--sandbox namespace` or `chroot`), not via fixed guest-relative paths.                                                                                                                                                                                                                                                                                                                        | no — different mechanism | §5.3's per-VM jail                                |
| 4   | Memory file mapped `MAP_PRIVATE`, immutable, retained for the VM's life           | Confirmed, but only under a non-default, opt-in mode. `docs/memory.md`: guest RAM defaults to `MAP_PRIVATE`. `docs/snapshot_restore.md`'s copy-on-write restore section (`memory_restore_mode=copyonwrite`): "The snapshot memory file must remain on disk **and unchanged** for the entire lifetime of the VM... the copy-on-write region stays file-backed forever, so truncating it delivers a synchronous `SIGBUS` and any in-place edit corrupts the guest" — immutable and retained for the VM's life, matching Firecracker's fact exactly. But `memory_restore_mode` defaults to eager `copy` (memory fully copied up front; the source file need not persist), so this guarantee is opt-in, not the default restore path. | partial                  | §7.3's density mechanism                          |
| 5   | Guest wall clock resumes from the snapshot moment                                 | Undocumented. Re-checked `docs/snapshot_restore.md` and `docs/live_migration.md` — no mention of guest wall-clock behavior on restore or resume.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  | undocumented             | Deviation 5's agent-side clock set                |
| 6   | cgroups v1 causes high restore latency                                            | Undocumented. Re-checked `docs/snapshot_restore.md`, `docs/live_migration.md`, `docs/threat-model.md`, `docs/landlock.md`, `docs/seccomp.md`, `docs/disk_locking.md`, and `docs/io_throttling.md` — none mentions cgroups in connection with restore latency (or at all, except `threat-model.md`'s generic recommendation to use cgroups for guest resource limits, which is unrelated to restore speed).                                                                                                                                                                                                                                                                                                                        | undocumented             | Rig requirement                                   |
| 7   | Restore requires identical hardware/software                                      | Partially documented. Repo `README.md`, under "Status": "Snapshot/restore is not supported across different versions." This confirms the software-version half of Firecracker's fact. Neither the README nor `snapshot_restore.md`/`live_migration.md` says anything about matching CPU model or hardware, the other half.                                                                                                                                                                                                                                                                                                                                                                                                        | partial                  | Build the snapshot on the target instance type    |
| 8   | Diff snapshots are developer preview, generally not resumable                     | Undocumented. Re-checked `docs/snapshot_restore.md` — no diff/incremental snapshot concept is mentioned at all; every documented snapshot is a full snapshot.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | undocumented             | Full snapshots only                               |
| 9   | Only a 64-bit CRC on the state file; files are trusted                            | Documented as worse: no integrity mechanism at all, and files are explicitly editable. `docs/snapshot_restore.md`: `config.json` "is stored in a human readable format so that it could be modified between the snapshot and restore phases to achieve some very special use cases." No checksum, CRC, or signature is mentioned for `config.json`, `state.json`, or `memory-ranges` in `snapshot_restore.md` or `live_migration.md`. `docs/threat-model.md` states plainly: "Snapshot files are trusted input for restore operations" — trust is total, with no documented integrity check, not even Firecracker's CRC.                                                                                                          | no                       | §5.5's root-owned read-only artifact              |
| 10  | Integrators must provision disk and enforce quotas                                | Undocumented. Re-checked `docs/snapshot_restore.md`, `docs/disk_locking.md`, and `docs/io_throttling.md` — none mentions disk-space provisioning or quotas.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       | undocumented             | §6's per-workspace quota                          |
| 11  | No virtio-fs (Firecracker)                                                        | virtio-fs present                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | no                       | Why CH is in scope at all (§4.3)                  |
| 12  | Some vsock packet loss should be anticipated for resumed guests                   | Undocumented. `docs/vsock.md` never discusses resume semantics or packet loss for the vsock device; re-checked `docs/snapshot_restore.md` and `docs/live_migration.md` — neither mentions vsock either.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | undocumented             | §5.4's framed request/response                    |
| 13  | An equivalent of `--cgroup` on the jailer                                         | Undocumented. Re-checked the full 48-file `docs/` listing, plus `docs/threat-model.md`, `docs/landlock.md`, `docs/seccomp.md`, `docs/disk_locking.md`, and `docs/io_throttling.md` directly — no file named `jailer` and no cgroup-configuration flag for the `cloud-hypervisor` binary itself is documented anywhere. `virtiofsd`'s `--sandbox` flag isolates the file-server process, not the VMM, and has no cgroup semantics.                                                                                                                                                                                                                                                                                                 | undocumented             | §5.3/§6's cgroup agreement                        |
| 14  | A jailer equivalent at all (chroot per VM)                                        | A real mechanism exists, but it is a different kind of confinement, not an equivalent. `docs/threat-model.md`'s "Sandboxing" section: "Cloud Hypervisor can sandbox itself via Landlock and seccomp. Once sandboxed, Cloud Hypervisor is not able to access any resources blocked by the sandbox. This is true even if an attacker can execute arbitrary code in the context of the Cloud Hypervisor process." `docs/landlock.md` documents this as a ruleset the process applies to **itself** at `vm_create` (`--landlock`), not an external per-VM chroot wrapper launched before the VMM starts. No chroot-per-VM jailer process is documented anywhere in the `docs/` directory.                                             | no                       | §5.3 — if absent, `vmpool` owns the chroot itself |

## Where Cloud Hypervisor is worse

- **Row 2** (multi-resume insecurity): now documented, and worse than
  undocumented — Cloud Hypervisor's own docs frame restoring the same snapshot
  into multiple VMs as an intended feature (including a copy-on-write mode
  built explicitly so several VMs can share one snapshot), with no security
  caveat anywhere. §5.2's no-secrets invariant should be treated as **required**
  on this arm, not merely unconfirmed.
- **Row 3** (same relative paths): not just undocumented but a **different
  mechanism** — virtio-fs resolves paths on the host inside `virtiofsd`, so
  §5.3's per-VM jail would need a different design on the Cloud Hypervisor arm,
  not a port of Firecracker's.
- **Row 9** (integrity/trust): now documented, and worse than a weak CRC —
  Cloud Hypervisor's snapshot format has **no** documented integrity mechanism,
  and `config.json` is explicitly designed to be human-edited between snapshot
  and restore. §5.5's root-owned read-only artifact design is not merely
  unconfirmed here; it is doing strictly more work than Cloud Hypervisor's own
  documentation claims for itself.
- **Row 14** (jailer/chroot-per-VM equivalent): a real mechanism exists
  (Landlock + seccomp self-sandboxing), but it confines the VMM process from the
  inside, applied by the process to itself, rather than an external per-VM
  chroot imposed before the VMM starts. `vmpool` would still have to implement
  and own the per-VM chroot itself on this arm to match Firecracker's jailer
  guarantee, though it could additionally require `--landlock` as
  defense-in-depth.
- **Row 7** (identical hardware/software on restore, partial): confirmed for
  the software half only — the repo README's version-compatibility statement
  matches Firecracker's fact. The CPU-model half is completely unaddressed
  anywhere in the `docs/` set. Because spec §5.2 and §2.4 treat "build the
  golden snapshot on the instance type that will run it" as a hard
  requirement, this silence matters in practice: there is no Cloud
  Hypervisor-side confirmation of what happens on a CPU-model mismatch (silent
  tolerance, degraded functionality, or a hard failure), so the strict
  same-instance-type rule should keep being enforced on this arm exactly as it
  is for Firecracker, not relaxed on the strength of the confirmed software
  half alone.
- **Row 1** (vsock survive-restore semantics): still undocumented after
  checking the fuller `docs/` set — the assumption that a listening socket
  survives restore has no citation anywhere in-tree.
- **Row 5** (wall clock on resume): still undocumented — Deviation 5's
  agent-side clock-set fix has no confirmed trigger condition on this arm.
- **Row 6** (cgroups v1 restore latency): still undocumented — the rig
  requirement that motivated this fact for Firecracker has no Cloud
  Hypervisor confirmation either way, even after checking the sandboxing docs.
- **Row 8** (diff snapshots not resumable): still undocumented — Cloud
  Hypervisor's docs describe only full snapshots, so this fact simply has no
  counterpart to confirm or deny.
- **Row 10** (disk provisioning/quota is the integrator's job): still
  undocumented after checking `disk_locking.md` and `io_throttling.md` — §6's
  per-workspace quota assumption is unconfirmed for this arm.
- **Row 12** (vsock packet loss on resume): still undocumented — §5.4's framed
  request/response protocol was justified by Firecracker's documented
  packet-loss warning; Cloud Hypervisor gives no such warning to justify (or
  rule out) the same design.
- **Row 13** (`--cgroup`-equivalent flag): still undocumented after checking
  the full 48-file `docs/` listing — §5.3/§6's requirement that the VMM's
  cgroup mechanism agree with the systemd slice has no Cloud Hypervisor-side
  mechanism identified to configure.

## Where Cloud Hypervisor is better

- **Row 11**: virtio-fs is present in Cloud Hypervisor, confirmed by its own
  `docs/memory.md`/`docs/vsock.md` ecosystem and general architecture — this is
  the entire reason Cloud Hypervisor is in scope at all (§4.3).
- **Row 4** (partial): where its non-default `copyonwrite` restore mode is
  used, Cloud Hypervisor's documented guarantee is at least as strong as
  Firecracker's — immutable, file-backed for the VM's life — though this is
  opt-in rather than the default, so it can't be assumed without configuring
  for it.
- virtio-fs makes **D > 1 standbys** and **concurrent `Exec`s per run** possible
  at all (§4.3) — both are correctness-impossible on the Firecracker arm as
  specified, because two guest kernels mounting one ext4 rw block device
  corrupt it.
- The host filesystem is the write-durability authority under virtio-fs, so
  **no `sync` is needed on the hot path** and writes survive `SIGKILL` (§4.3,
  §6) — Firecracker's arm requires a mandatory `sync` before kill on the hot
  path to get the same guarantee.

**Verdict:** `firecracker`. After the correction pass, seven of the fourteen
rows remain genuinely undocumented against Cloud Hypervisor's own sources,
including this task's cited `docs/*` directory in full (rows 1, 5, 6, 8, 10,
12, 13); two rows are partial — confirmed only in scope or only in a
non-default mode (rows 4, 7); and five rows read `no` (rows 2, 3, 9, 11, 14) —
of those, only row 11 (virtio-fs presence) favors Cloud Hypervisor, row 3 is a
different-but-not-worse mechanism, and rows 2, 9, and 14 are now **confirmed
worse**, not merely silent: multi-resume is a documented feature with no
security caveat (row 2), snapshot files carry no integrity mechanism at all and
are meant to be edited (row 9), and the only self-sandboxing mechanism found
(Landlock + seccomp) confines the VMM from the inside rather than providing an
external per-VM chroot (row 14). Spec §9's risk is **still live, but
narrower than before**: the correction pass converted what looked like a
uniform wall of silence into a mix of confirmed-worse findings (rows 2, 9, 14)
and a genuinely silent remainder (seven rows, half the table) — neither
direction favors switching the preferred arm, and per §9's own wording ("if
its caveats are worse **or less documented**"), both a confirmed-worse finding
and an undocumented one keep the risk open. Phase D should implement the
Firecracker launcher first, where every one of these 14 facts is confirmed by
upstream documentation, and treat Cloud Hypervisor's virtio-fs advantage
(§4.3, D>1 standbys) as a second launcher to de-risk once rows 1, 5, 6, 8, 10,
12, and 13 above have real citations, and once rows 2, 9, and 14's confirmed
gaps are either closed by `vmpool`-side mitigations or accepted as an explicit
trade-off — not assumptions carried over from Firecracker.

_Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>_
