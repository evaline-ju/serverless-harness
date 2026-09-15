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

### Tools, and which rungs die without them

Check this **before** §4, not during it. E10 rung 1 does not silently skip when a tool is
missing — it **dies**, taking the whole ladder with it, because a rung-1 timing measured
against an absent `grpcurl` is a timing of `grpcurl` failing to launch.

| Tool          | Needed by                                           | If absent                                                             |
| ------------- | --------------------------------------------------- | --------------------------------------------------------------------- |
| `jailer`      | E10 rungs 2–4, E11                                  | set `SH_JAILER_BIN`; nothing runs without it                          |
| `firecracker` | E10 rungs 2–4, E11                                  | set `SH_FIRECRACKER_BIN`                                              |
| `mkfs.ext4`   | every microVM rung (workspace images)               | install e2fsprogs; `PATH` must include `/sbin` and `/usr/sbin`        |
| `go`          | E10 rungs 2–4 (builds `vmpoolctl`), E11 (2 workers) | install it; both drivers build their own binaries                     |
| `grpcurl`     | **E10 rung 1**, **all of E11**                      | E10: `SH_E10_RUNGS='2 3 4'` to skip rung 1. E11 cannot run at all     |
| `docker`      | **E10 rung 1**, **all of E11** (Redis)              | as above, or `SH_E10_START_STACK=0` to reuse an already-running stack |
| `pnpm`        | **E10 rung 1**, **all of E11**                      | `SH_E10_RUNGS='2 3 4'`; E11 cannot run at all                         |

### The relay stack: a workspace install and a BUILT pi-fork

Any rung that goes through the relay — E10 rung 1 and **both** of E11's arms — starts it with
`pnpm --filter @sh/sandbox-relay start`. That pulls in `harness/src/run-turn.ts`, which
imports `@earendil-works/pi-coding-agent` and `@earendil-works/pi-ai`. Both are
`link:../pi-fork/...` dependencies on the **pi-fork submodule**, so the relay cannot start
until that submodule is present AND built:

```bash
git submodule update --init --recursive
cd pi-fork && npm ci && npm run build && cd ..
pnpm install
```

**pi-fork requires Node >= 22.19.0** (its own `engines` field) and its build refuses an older
one. Most distributions' default `nodejs` package is older: Amazon Linux 2023 ships 18,
Ubuntu 22.04 and 24.04 ship 18. So check `node --version` BEFORE building rather than after,
and install a 22.x from a versioned distro package, NodeSource or nvm if it is short.

Where a distribution manages `node` through an alternatives system, installing a 22.x package
is **not by itself enough** to change what `node` on `PATH` resolves to — the alternatives
link keeps pointing at the old one. Verify with `node --version`, never with the package
list. This cost a build on the validation rig.

This is the same setup CLAUDE.md prescribes for the repo generally; it is repeated here
because a missing pi-fork build presents only as the relay dying at startup, taking every
downstream Exec with it. The drivers now fail at that point, naming the relay's log and
printing its last 20 lines, rather than letting it surface ten seconds later as a converge
timeout — but you still have to build it.

Worth verifying before §4, since it costs nothing:

```bash
pnpm --filter @sh/sandbox-relay exec node -e 'console.log("relay deps resolve")'
```

If you skip rung 1 you lose the container baseline, and with it **sealed prediction 2**
(warm hot path within 2x of the container baseline) — that prediction is a ratio against
rung 1 and cannot be computed without it. Decide deliberately rather than by discovering a
missing tool at run time.

`sudo` resets the environment, so pass every `SH_*` variable explicitly on the command
line. `sudo -E` is not sufficient and `PATH` must include `/sbin` and `/usr/sbin` for
`mkfs.ext4`.

## 2. Build the golden snapshot on THIS machine

```bash
sudo PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/usr/bin:/bin \
  bash deploy/microvm/build-snapshot.sh \
    --kernel  /path/to/vmlinux \
    --rootfs  /path/to/extracted-rootfs-TREE \
    --agent   /path/to/remote-worker \
    --image   swebench-py311 \
    --vmm     firecracker \
    --guest-ram-mb 256 \
    --out     /srv/snapshots/default
```

Notes that cost time if missed:

- **`--rootfs` is a DIRECTORY TREE, not a filesystem image.** The script does
  `cp -a "$ROOTFS/." …` to layer the agent and its init on top, so an `.ext4` file fails with
  `cp: cannot stat '…/rootfs.ext4/.': Not a directory`. Extract the image (or its squashfs)
  first and pass the resulting tree.
- **`--agent` is the remote-worker MODULE ROOT, not a prebuilt binary.** The script builds
  the agent itself, statically, from `<that dir>/cmd/guest-agent`; passing a compiled binary
  fails with `has no cmd/guest-agent; pass the remote-worker module root`. Building it
  in-script is deliberate — the agent in the snapshot is then provably from this tree.

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

### Also clear a leaked relay, and do it between the smoke pass and the real run

A leaked **relay** is a quieter hazard than a leaked VMM, and it matters here specifically
because **this runbook runs the drivers twice** — the §4 smoke pass and then the §5 real run.
The drivers background the relay with `( … & echo $! > pidfile )`, which captures the
backgrounding subshell rather than the relay itself, so teardown can SIGTERM the wrong process
and leave the relay holding its port after the run.

If that happens, the next run does not fail cleanly. Its own relay cannot bind, so it dies with
`EADDRINUSE` into its own log; the driver's liveness check then sees _something_ listening on
the port and proceeds, because it cannot tell the right relay from a stale one; and the run
measures against a relay whose worker registration belongs to the previous invocation. That is
silently wrong data, which is worse than a crash.

The ports are `8443` (E10 rung 1) and `8444` (E11) for the relays, `6380` and `6381` for their
scratch redises. Clear by **port and container name**, which cannot self-match the way
`pgrep -f` can:

```bash
# what is holding the driver ports, if anything
sudo ss -ltnp | grep -E ':8443|:8444|:6380|:6381'

# kill whatever is listening on them, by port rather than by name
for port in 8443 8444; do
  pid=$(sudo ss -ltnp "sport = :$port" | grep -oP 'pid=\K[0-9]+' | head -1)
  [ -n "$pid" ] && sudo kill "$pid"
done

# and the drivers' own scratch redis containers (named sh-e10-redis-* / sh-e11-redis-*)
sudo docker ps -aq --filter 'name=sh-e10-redis' --filter 'name=sh-e11-redis' |
  xargs -r sudo docker rm -f
```

Verify with `ss`, not with `pgrep -f sandbox-relay`: that pattern matches the command line of
the very shell you run it from, so it reports a relay that does not exist. It cost a
false "leaked relay" reading during validation.

## 4. Smoke pass first — two minutes, not ninety

The point is to prove the instrument runs end to end, not to get a number. Use the same
`SH_SUBSTRATE` you will use for real, so the record shape is identical.

> **The smoke pass produces NO usable verdict, and now says so.** At `ITERS=5` rung 2's standby
> pool cannot refill between Execs, so its acquires are mostly cold and its "warm hot path" p50
> is a cold-path number. On metal that combination printed
> `STOP: warm hot path 69.87ms >= 15ms` — a recommendation to abandon the design, computed from
> four Execs of which one was warm. E10 now refuses the verdict when the warm rung was not warm.
> Treat §4 as proof the instrument runs, and nothing else.
>
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

### Run E10 FIRST, then set E11's cold-latency threshold from its numbers

E11 reports `coldAcquireRate` — the fraction of acquires that had to cold-replenish instead of
popping a warm standby — and sealed **prediction 3** is a claim about the shape of that curve.
There is no pool introspection endpoint, so the figure is a **latency proxy**: an Exec counts as
cold at or above `SH_E11_COLD_LATENCY_MS`, default **50**.

That default cannot serve both arms. On the validation rig the container arm ran p95 40–41ms —
under the threshold, so nothing was ever classified cold — while the microVM arm ran p95
236–266ms, so **every** Exec was classified cold regardless of what the pool did
(`coldAcquireRate` 1.0 at the least-loaded rung). The metric was reporting which arm it was on,
and prediction 3 came back "falsified" on that basis alone.

The threshold only needs to serve the **microVM** arm: the container arm has no standbys at all
(`standbysResident: 0`), so "cold acquire" has no referent there. And E10 measures exactly what
E11 has to assume — so run E10 first and read two numbers out of it.

> **Derive it from E11's OWN latencies, not E10's.** E10 rung 2 drives `vmpool` DIRECTLY, with
> no relay in the path; E11 measures the RELAYED path. On metal those differ by more than 2x --
> E10's warm p50 was 55ms while E11's microVM c=1 p95 was 133ms -- so a threshold taken from E10
> sits _below_ E11's warm floor, classifies every Exec as cold, and the analyzer then (correctly)
> refuses to score prediction 3. Take the c=1 p95 from a prior E11 run on this host and add half
> the restore cost E10 rung 3 measured: 133 + 23/2 gave 145 here. The first authoritative run
> used 67, derived the wrong way, and P3 came back inconclusive for exactly that reason.
>
> **And read E10's numbers from the REAL run, never from the §4 smoke pass.** At a small `ITERS` the standby
> pool cannot refill between back-to-back Execs (`ReplenishDelay` is 200ms), so rung 2's acquires
> come out mostly **cold** and its p50 is a replenishment number, not a warm one. Measured on
> metal at `ITERS=5`: one warm acquire out of five, `p50_acquire_us` 23747 where a genuine warm
> acquire is ~1µs. A threshold derived from that is calibrated against the wrong quantity. E10
> now refuses to print a §7.2 verdict when that happens and prints the mix either way — check
> `rung2_warm_acquires` / `rung2_cold_acquires` in `e10-summary.json` before trusting either
> number.

```bash
# warm Exec latency on this host — rung 2, parked variant
python3 -c "import json;d=json.load(open('deploy/microvm/.results/e10-rung2-firecracker-$SH_SUBSTRATE-parked.json'));print(d['p50_total_us']/1000.0)"
# the restore a cold acquire additionally pays — rung 3, pinned variant
python3 -c "import json;d=json.load(open('deploy/microvm/.results/e10-rung3-firecracker-$SH_SUBSTRATE-pinned.json'));print(d['p50_acquire_us']/1000.0)"
```

A cold acquire costs about `warm + restore`, so put the threshold at the midpoint:

```
SH_E11_COLD_LATENCY_MS ≈ warm_ms + (restore_ms / 2)
```

Worked example from the nested rig: warm ≈ 236ms, restore ≈ 100ms → cold ≈ 336ms → threshold
≈ **285**. Recompute on metal; those numbers will be smaller and the midpoint will move.

```bash
sudo PATH=… <the same SH_* exports> SH_E11_COLD_LATENCY_MS=<computed> \
  bash deploy/microvm/e11-density.sh 2>&1 | tee /tmp/e11-metal.log
```

**If you skip this**, the analyzer detects that the classifier has no headroom — the lowest-c
rung is the warm case by construction, so a threshold at or below its p95 cannot discriminate —
and reports prediction 3 as `inconclusive` rather than emitting a verdict about the threshold.
That is the safe outcome, not a good one: it costs the prediction. Nothing else in E11 depends
on this value.

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
