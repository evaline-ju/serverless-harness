# Running E10 and E11 on bare metal

For whoever runs the authoritative measurements, in a session that has no history of this
work. Everything here is a precondition, an exact command, or a refusal you will hit and
what it means.

> ## Read this before anything else
>
> **What has and has not been executed.** E10's rungs 2, 3 and 4 have now run against real
> VMs on a 4-vCPU **nested** rig, rung 4 at the full `ITERS=200`; every figure from that is
> a validation result and none of it is quotable. **E10 rung 1 and the whole of E11 have
> still never run** — rung 1 needs docker/grpcurl, absent on that rig. Both drivers are
> unit-tested (271 assertions), shellcheck-clean, with every failure path exercised against
> fabricated inputs. So still do the smoke pass in §4 before the real run in §5: for E11 it
> is the first execution of anything, and for E10 it is the first on this hardware. If the
> instrument is broken, you want to find out in two minutes, not ninety minutes into a
> ladder.
>
> The first-ever execution of E10 rung 4 found three defects, and the first run at
> `ITERS=200` found a fourth that `ITERS=5` could not (see §4). Expect the same of E11.
>
> **The golden snapshot cannot be copied to this machine.** Spec §2.4: a Firecracker
> snapshot only restores on identical hardware. A snapshot built on any other instance
> type will fail to restore here, usually as a hang rather than a clean error. **You must
> build it on this box** — §2.
>
> **Never pass `SH_SUBSTRATE=metal` unless this host really is bare metal.** That label is
> what makes a result authoritative and lets the decision-rule table print a stop verdict.
> On a virtual instance the correct label is `nested-<instance-type>` (e.g.
> `nested-m8i`), and the script then deliberately refuses to print a stop verdict at all,
> because a nested result "fires no stop rule" (spec §7.2).

---

## 1. Preflight the host

The drivers refuse rather than warn on each of these, because a rung measured on the wrong
substrate is not a degraded measurement — it is a wrong one that looks fine.

| Requirement        | Check                                                                       | If it fails                                                                                                                                             |
| ------------------ | --------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/dev/kvm` present | `ls -l /dev/kvm`                                                            | wrong instance type                                                                                                                                     |
| cgroups v2         | `stat -fc %T /sys/fs/cgroup` → `cgroup2fs`                                  | boot with unified cgroups; v1 is recorded as a cause of high restore latency                                                                            |
| swap off           | `swapon --show` → empty                                                     | `sudo swapoff -a`. Swapping guest RAM destroys the latency this design exists for                                                                       |
| CPU governor       | `cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor` → `performance` | `sudo cpupower frequency-set -g performance`. If the path does not exist at all, that is fine — the script records `governor: not exposed` and proceeds |
| root               | —                                                                           | both drivers need it (jailer chroot, cgroups, `mkfs.ext4`, `drop_caches`)                                                                               |

`sudo` resets the environment, so pass every `SH_*` variable explicitly on the command
line. `sudo -E` is not sufficient and `PATH` must include `/sbin` and `/usr/sbin` for
`mkfs.ext4`.

## 2. Build the golden snapshot on THIS machine

```bash
sudo PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/usr/bin:/bin \
  bash deploy/microvm/build-snapshot.sh \
    --kernel  /path/to/vmlinux \
    --rootfs  /path/to/rootfs.ext4 \
    --agent   /path/to/guest-agent \
    --image   swebench-py311 \
    --vmm     firecracker \
    --guest-ram-mb 256 \
    --out     /srv/snapshots/default
```

Notes that cost time if missed:

- **`--out /srv/snapshots/default`.** The worker reads `SH_SNAPSHOT_DIR/SH_SNAPSHOT_IMAGE`
  and `SH_SNAPSHOT_IMAGE` defaults to `default`. Pointing `SH_SNAPSHOT_DIR` at the leaf
  snapshot instead is a mistake that presents as a device check failing on a path nobody
  configured.
- `SnapshotDir`, `WorkspaceRoot` and the jailer chroot base **must share one filesystem
  device** — the build hardlinks between them. `/tmp` is usually tmpfs; use paths under one
  real disk.
- The script verifies the snapshot _before_ publishing it, so a failed verification leaves
  nothing usable behind. If it fails, nothing was sealed and you can re-run.

## 3. Set the environment

Seven variables are required across the two drivers; each refuses if unset.

```bash
export SH_SUBSTRATE=metal                 # ONLY if this is really bare metal — see the banner
export SH_SNAPSHOT_DIR=/srv/snapshots     # the PARENT; image name is appended
export SH_WORKSPACE_ROOT=/srv/workspaces
export SH_MAX_COMMITTED_MB=<see below>    # E11 only
```

**Size `SH_MAX_COMMITTED_MB` to this host.** It is the admission budget and nothing in the
worker reads physical RAM, so a budget above installed memory means the gate cannot refuse
before the OOM killer arrives — and every density number becomes a measurement of the OOM
killer. Leave real headroom: total RAM minus what the host needs, minus
`SH_MEMORY_RESERVE_MB`. The shipped unit's 24576 is a placeholder for a large host, not a
recommendation.

If `jailer` and `firecracker` are not in `/usr/local/bin`, set `SH_JAILER_BIN` and
`SH_FIRECRACKER_BIN`. An absolute `SH_PARENT_CGROUP` is refused outright — jailer rejects
absolute `--parent-cgroup` — so it must be slice-relative, e.g.
`microvm.slice/microvm-vms.slice`.

## 3a. Clear orphaned VMMs before every run

`vmpoolctl` runs **no startup orphan sweep** — only `microvm-worker` does, and the E10/E11
drivers do not go through it. A live VMM left behind by an aborted run still holds its VM
id's API socket, and the next run that mints the same id refuses rather than restoring:

```
firecracker: restore vm-6: a live VMM already holds this VM id's API socket at
/srv/jail/firecracker/vm-6/root/run/firecracker.socket — refusing to load a snapshot
into another microVM
```

That refusal is deliberate. Before this guard existed, the restore instead loaded a
snapshot into the orphan's already-loaded microVM, and the operator saw Firecracker's
`PUT /snapshot/load: not supported after starting the microVM (400)` — a message naming
neither the collision nor the id.

Check and clear before each run, **by cgroup membership, never by process name**
(`pkill -f firecracker` has matched an operator's own ssh shell on a test rig):

```bash
sudo find /sys/fs/cgroup -maxdepth 5 -type d -name 'vm-*'
for d in $(sudo find /sys/fs/cgroup/microvm.slice -maxdepth 2 -type d -name 'vm-*'); do
  for pid in $(sudo cat "$d/cgroup.procs"); do sudo kill -9 "$pid"; done
done
sudo rm -rf "$SH_WORKSPACE_ROOT"/* /srv/jail/firecracker
```

A jail directory left behind with **no** live process is a different, milder case: the
restore fails loudly after a 5s socket timeout, removes the stale jail, and an immediate
retry succeeds. Clearing first avoids paying that per orphaned id.

## 4. Smoke pass first — two minutes, not ninety

The point is to prove the instrument runs end to end, not to get a number. Use the same
`SH_SUBSTRATE` you will use for real, so the record shape is identical.

> **A smoke pass at `ITERS=5` cannot catch a failure that only exists at scale.** It has
> already missed one: `teardown-bulk` used to take its batch size from `--iterations`, so
> `ITERS=5` built 10 VMs and passed while `ITERS=200` asked for 400, exhausted the
> admission budget, exited non-zero, and aborted the ladder at rung 4. Fan-out now has its
> own knob (`--bulk-keys`, §5), so the shapes agree — but treat a green smoke pass as
> evidence the instrument runs, not as evidence the real run will finish.

```bash
sudo PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/usr/bin:/bin \
  SH_SUBSTRATE="$SH_SUBSTRATE" SH_SNAPSHOT_DIR="$SH_SNAPSHOT_DIR" \
  SH_WORKSPACE_ROOT="$SH_WORKSPACE_ROOT" \
  ITERS=5 WARMUP=1 \
  bash deploy/microvm/e10-lifecycle.sh 2>&1 | tee /tmp/e10-smoke.log
```

**A smoke pass is successful only if a rung record was written.** Check:

```bash
ls -la deploy/microvm/.results/
```

An empty `.results/` with exit 0 means the run measured nothing — that class of failure is
what most of this branch's driver review was about, and the drivers now refuse rather than
report a favourable verdict, but check anyway.

Then the same for E11 with a tiny ladder:

```bash
sudo PATH=… SH_SUBSTRATE="$SH_SUBSTRATE" SH_SNAPSHOT_DIR="$SH_SNAPSHOT_DIR" \
  SH_WORKSPACE_ROOT="$SH_WORKSPACE_ROOT" SH_MAX_COMMITTED_MB="$SH_MAX_COMMITTED_MB" \
  SH_E11_ACTIVE_RUNS="1 2" SH_E11_ITERS_PER_SLOT=5 \
  bash deploy/microvm/e11-density.sh 2>&1 | tee /tmp/e11-smoke.log
```

`SH_E11_ACTIVE_RUNS` **must include `1`**: the knee detector throws without a single-run
baseline, and it is better to learn that in a smoke pass than two hours into a sweep.

## 5. The authoritative run

Defaults are `ITERS=200 WARMUP=20` for E10 and `SH_E11_ACTIVE_RUNS="1 2 4 8"` for E11.
Drop the overrides from §4 and run both. Expect E11 to take a while; it drops caches
between arms and samples until idle standbys converge.

```bash
sudo PATH=… <the same SH_* exports> bash deploy/microvm/e10-lifecycle.sh 2>&1 | tee /tmp/e10-metal.log
sudo PATH=… <the same SH_* exports> bash deploy/microvm/e11-density.sh  2>&1 | tee /tmp/e11-metal.log
```

**Run nothing else on the host.** These are latency and density measurements; a competing
workload does not degrade them, it makes them wrong in a way that looks fine.

### What rung 4's bulk variant costs, and the one knob it has

`teardown-bulk` prices the sweep's bulk reclaim. Its batch is `--bulk-keys` run pools at
`--standby-depth` standbys each; `--bulk-keys` defaults to whatever makes the batch
`MaxReclaimsPerScan` VMs — 8 at the shipped defaults, which is the most a real sweep ever
reclaims in one scan (`sweep.go`'s own budget). `--iterations` is a repeat count here, as
it is for every other mode, so each iteration is one full fill-then-destroy cycle with
only the destroy timed.

The driver passes no `--bulk-keys`, so the derived default applies and nothing needs
setting. Raising it is how you would price a larger batch than the sweep performs — but
`--bulk-keys × --standby-depth × (guest RAM + 32 MiB overhead)` must stay inside
`--max-committed-mb` (default 32 GiB), or admission refuses mid-batch and the rung fails.

Budget the time: at `ITERS=200` this rung took **293s** on a 4-vCPU nested rig, against 62s
and 49s for the two per-VM variants. Expect metal to be faster, not slower, but plan for
rung 4 being the long pole of E10 outside rung 1.

## 6. What to capture and hand back

- `deploy/microvm/.results/` — every rung record, plus `e10-summary.json` and
  `e11-ladder.json`. This is the actual deliverable.
- `/tmp/e10-metal.log`, `/tmp/e11-metal.log` — including the printed decision-rule verdict.
- The host's identity: instance type or hardware, kernel, CPU model, total RAM, and whether
  the governor was `performance` or not exposed.
- Confirm `pnpm -C experiments exec vitest run microvm-predictions` still passes. The
  predictions were hash-pinned before any run so they cannot be adjusted to fit the
  results; that pin must survive.

**Do not edit `deploy/microvm/predictions.json`.** Falsified predictions get reported as
plainly as confirmed ones — that is the point of having pinned them.

## 7. Reading the verdict

The script computes and prints the decision-rule row rather than leaving it to judgment:

| Observation                                                                                             | Verdict                                                                                     |
| ------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| warm hot path `< 5 ms` metal (`< 8 ms` nested) and replenishment CPU `< 25 ms` metal (`< 40 ms` nested) | proceed as designed                                                                         |
| warm path in `[5, 15) ms`                                                                               | proceed, re-priced against the container baseline; in the middle band **the ratio governs** |
| warm path `≥ 15 ms`                                                                                     | hard stop, regardless of ratio                                                              |
| replenishment CPU `> 50 ms`                                                                             | the §7.4 split becomes mandatory                                                            |

On any non-metal substrate the script is structurally incapable of printing a stop verdict.
If the metal verdict is a stop or makes the split mandatory, **record it and stop** — E11's
write-up says what happens next, and the spec's §9 fallbacks are the path, not a retry.

---

## Appendix: a starting prompt for a fresh session

If an assistant is driving this, paste something like:

> I need to run the authoritative E10 and E11 measurements for the P4 microVM sandbox tier
> on a bare-metal host. Read `deploy/microvm/METAL-RUNBOOK.md` and follow it. Key things I
> want you to respect: the golden snapshot must be built on this machine because a snapshot
> only restores on identical hardware; do the smoke pass before the real run because these
> drivers have never been executed; size `SH_MAX_COMMITTED_MB` to this host's actual RAM;
> and do not edit `predictions.json` under any circumstances. Before the long run, tell me
> what the smoke pass produced and confirm a rung record was actually written — an empty
> `.results/` at exit 0 means the run measured nothing. Report the printed decision-rule
> verdict verbatim, including if it is a stop.

Two failure modes worth naming for whoever helps: **a local pass is evidence about that
machine**, so check the artifact the run produced rather than the exit status; and **a zero
or missing value is not data** — if a rung reports `0` for a memory or latency term, treat
it as a refusal to investigate, not a measurement.
