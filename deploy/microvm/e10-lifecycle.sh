#!/usr/bin/env bash
# deploy/microvm/e10-lifecycle.sh
#
# E10, the lifecycle primitive ladder (spec §7.1-§7.5). The first task in the P4
# microVM tier that produces a performance number rather than a correctness proof.
#
# Four rungs, each a TERM in the hot-path/replenishment equation, not a concurrency
# sweep (that is Task 21/E11):
#
#   1. Container baseline  — the number the microVM tier is priced AGAINST. Spec §7.2:
#      "Without it, 15ms has nothing to be judged against — a 4ms baseline would make
#      this a 3x regression bought for isolation." Driven through the real relay and
#      remote-worker over grpcurl, no microVM involved at all.
#   2. Warm hot path        — acquire (standby pop) -> resume -> run -> destroy, via
#      vmpoolctl --mode=exec, once with an empty stdin (parked-bash path) and once
#      with a non-empty stdin (fresh-child path, spec §5.4).
#   3. Replenishment        — spawn -> restore -> pause -> ready, via
#      vmpoolctl --mode=replenish, wall AND CPU (RUSAGE_CHILDREN), once with a cold
#      memfile and once pinned (spec §7.2: "The CPU number is what §7.3 divides into
#      host capacity. Wall time alone misleads.").
#   4. Teardown              — three variants (inflight / standby / bulk), via
#      vmpoolctl --mode=teardown-*, because "the per-VM number does not predict" the
#      cost a bulk reclaim actually pays.
#
# Every number this script prints is a FLOOR, not a verdict on the fleet: it is one
# VM in isolation, not N VMs contending for a host (spec §7.1).
#
# §7.5 traps, and how each is handled here (not hoped away):
#   - closed-loop queueing:      each rung issues one Exec/replenish/teardown at a
#                                 time and waits for completion before the next; the
#                                 bias this creates is declared, not hidden.
#   - page-cache asymmetry:      rung and arm order is randomized, and
#                                 `echo 3 > /proc/sys/vm/drop_caches` runs between
#                                 arms so whichever arm runs second does not inherit
#                                 the other's warmed cache.
#   - CPU frequency/thermal:     the governor is checked (see check_governor below)
#                                 and its state is recorded in every summary.
#   - warmup vs steady state:    --warmup discards the first N iterations of every
#                                 vmpoolctl invocation before percentiles are taken.
#   - guest-side timing:         never used. Every timestamp in this script and in
#                                 vmpoolctl's own timers is taken on the HOST; guest
#                                 clocks jump on resume (spec §2.4) and would silently
#                                 corrupt exactly the numbers this task exists to
#                                 produce.
#   - kernel limits:             vmpoolctl records RLIMIT_MEMLOCK, RLIMIT_NOFILE,
#                                 vm.max_map_count, TasksMax and pid_max per run
#                                 (main.go's Limits field); this script does not
#                                 duplicate that, only surfaces it in the summary.
#
# Hardware-corrections applied (pre-run hardware corrections; a build-time note, not committed), because the brief
# alone is not the whole spec here:
#   - E8:  ARMS defaults to firecracker ONLY. ch-remote restore hangs and
#          cloud-hypervisor dies silently during device restoration on this rig's
#          kernel/qemu combination; a timeout in every cell of rungs 2-4 reads as
#          "slow", which is worse than an absent row. Override with SH_E10_ARMS.
#   - E9:  This rig is not metal. The script is structurally incapable of printing a
#          STOP or MANDATORY verdict unless SUBSTRATE is exactly "metal" — see
#          verdict() below, and its test in e10-lifecycle.test.sh, which proves this
#          behaviorally rather than trusting this comment.
#   - E10: The governor check is three-way, not two-way: present+performance
#          proceeds; present+other REFUSES (it is fixable); PATH ABSENT proceeds and
#          records "not exposed" in the run record, because absence is a fact, not a
#          failure.
#
# A disclosed judgment call: the brief's skeleton calls for driving rung 2 twice via
# SH_GUEST_PARKED_SHELL=1|0. That env var does not exist anywhere in this codebase.
# The real mechanism (spec §5.4) is guestconn.go's HasStdin: len(c.Stdin) > 0, which
# vmpoolctl's --stdin flag drives end to end (see main_test.go's
# TestStdinFlagReachesTheCommand/TestStdinFlagDefaultsToEmpty). This script therefore
# runs rung 2 once with no --stdin (parked-bash path) and once with --stdin set to a
# non-empty payload (fresh-child path) — the full write-up is a build-time note, not committed.
#
# Usage:
#   SH_SUBSTRATE=nested-m8i \
#   SH_SNAPSHOT_DIR=/srv/snapshots SH_WORKSPACE_ROOT=/srv/workspaces \
#     bash deploy/microvm/e10-lifecycle.sh
#
# For a short local validation pass, ITERS/WARMUP are deliberately small-run-safe:
# nothing below hardcodes the full 200x20 shape, so `ITERS=5 WARMUP=1` produces a
# complete, if noisier, run of every rung. The project owner has confirmed the metal
# run happens last, after a PR review; before that this rig is for hypothesis
# validation only.
set -uo pipefail

# ---------------------------------------------------------------------------
# Configuration — every knob is an overridable env var, none of them hardcode the
# full-size run (the script needs to work well at small ITERS/WARMUP for a short
# validation pass, not only at the full 200x20).
# ---------------------------------------------------------------------------
# ABSOLUTE, always. rung 1's worker build runs inside `(cd "$REMOTE_WORKER_DIR" && go build
# -o "$RUNG1_WORKER_BIN" ...)`, and RUNG1_WORKER_BIN is under RESULTS — so a relative
# RESULTS wrote it to remote-worker/deploy/microvm/.results/, which does not exist. $VMPOOLCTL
# escaped this only because it is built absolute (see its own definition below), which is
# exactly why rungs 2-4 work and rung 1, never executed, does not. Found on E11's first run,
# where the same construct appears twice; fixed in both drivers rather than in the one that
# happened to be under the microscope. The test suite overrides RESULTS with an absolute
# /tmp path, so it could not have caught this.
RESULTS="${RESULTS:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/deploy/microvm/.results}"
case "$RESULTS" in /*) ;; *) RESULTS="$PWD/$RESULTS" ;; esac
ITERS="${ITERS:-200}"
WARMUP="${WARMUP:-20}"
GOVERNOR_PATH="${GOVERNOR_PATH:-/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor}"

# SH_SUBSTRATE stays an explicit, required, operator-set env var rather than being
# auto-derived (E3 suggests deriving it the way build-snapshot.sh derives instance
# type; E9 treats mislabeling it as "the most damaging single thing this task could
# do" — and E9's own phrasing, "never pass SH_SUBSTRATE=metal on this box", presumes
# it remains something an operator passes). Auto-detection risks silently mislabeling
# a substrate; an explicit, required flag cannot silently default to the wrong one.
SUBSTRATE="${SH_SUBSTRATE:?set SH_SUBSTRATE (e.g. nested-m8i, nested-c8i, or metal) - spec section 6 requires the substrate in every run record, and E9 requires it be labeled by rig, not bare nested}"

SNAPSHOT_PARENT="${SH_SNAPSHOT_DIR:?set SH_SNAPSHOT_DIR - the same env var name microvm-worker requires, see cmd/microvm-worker/main.go}"
# SH_SNAPSHOT_DIR is the PARENT, and SH_SNAPSHOT_IMAGE names the golden snapshot inside it --
# the same split cmd/microvm-worker/main.go makes (filepath.Join of the two, image defaulting
# to "default"). vmpoolctl's --snapshot-dir does NOT append an image name, so this script must
# join them itself; passing the parent straight through made vmpoolctl look for memfile one
# directory too high. One variable, one meaning, in both drivers and the unit.
SNAPSHOT_IMAGE="${SH_SNAPSHOT_IMAGE:-default}"
SNAPSHOT_DIR="$SNAPSHOT_PARENT/$SNAPSHOT_IMAGE"
# Defined HERE, above the first caller, not further down with the other helpers. The
# snapshot guard below is the first thing in this script that calls die, and it runs at load
# time -- so with the definition further down, bash printed "die: command not found" and,
# because this script sets -uo pipefail but NOT -e, CARRIED ON. Verified: a bogus
# SH_SNAPSHOT_DIR produced exactly that line and then continued into preflight. On a host
# with /dev/kvm it would have gone on to run the whole rung-1 container baseline (200x7 Execs
# through a real relay) before finally failing at rung 2 on a vmpoolctl snapshot error --
# the entire baseline thrown away on a config typo this one line exists to catch instantly.
die() { echo "e10: $*" >&2; exit 1; }
log() { echo "e10: $*" >&2; }

[ -f "$SNAPSHOT_DIR/manifest.json" ] ||
  die "no manifest.json under $SNAPSHOT_DIR - SH_SNAPSHOT_DIR is the PARENT of the golden snapshot and SH_SNAPSHOT_IMAGE names it (currently '$SNAPSHOT_IMAGE'). A golden snapshot built by build-snapshot.sh has manifest.json, vmstate, memfile, kernel and rootfs in it."
WORKSPACE_ROOT="${SH_WORKSPACE_ROOT:?set SH_WORKSPACE_ROOT - the same env var name microvm-worker requires, see cmd/microvm-worker/main.go}"

# E8: the brief hardcodes ARMS=(firecracker cloud-hypervisor). On this rig the
# cloud-hypervisor arm hangs/dies during restore, so it defaults OFF; an operator on
# hardware where it works can opt back in.
read -r -a ARMS <<<"${SH_E10_ARMS:-firecracker}"

VMPOOLCTL="${VMPOOLCTL:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../remote-worker" 2>/dev/null && pwd)/vmpoolctl}"
REMOTE_WORKER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../remote-worker" 2>/dev/null && pwd)"
PROTO_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." 2>/dev/null && pwd)/proto/sandbox/v1/sandbox.proto"
# grpcurl refuses an absolute -proto path unless also given at least one -import-path, and
# fails at proto-parse time before dialling. PROTO_FILE stays for the existence check --
# "is the file there" is a different question from "how is grpcurl invoked" -- and these two
# are what the invocation actually uses. Verified on the rig: import path at the proto ROOT
# with the file named relative to it parses and proceeds to dial.
PROTO_IMPORT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." 2>/dev/null && pwd)/proto"
PROTO_REL_PATH="sandbox/v1/sandbox.proto"

# Client-side deadline for grpcurl, a margin above the request's own timeout_s:30. Rung 1 is
# closed-loop and serial so it cannot hit E11's req_id collision, but any wedged Exec would
# otherwise hang the whole ladder with no deadline of its own -- which is what happened to
# E11 on the validation rig, for 33 minutes. Same guard, same reason. Verified there that
# -max-time returns non-zero on an established-stream stall, not just on a dial failure.
RUNG1_EXEC_MAX_TIME_S="${SH_E10_EXEC_MAX_TIME_S:-45}"

# Rung 1 (container baseline) stack knobs — all overridable so a validation pass can
# reuse an already-running relay/redis instead of starting fresh ones.
RUNG1_REDIS_PORT="${SH_E10_REDIS_PORT:-6380}"
RUNG1_RELAY_PORT="${SH_E10_RELAY_PORT:-8443}"
RUNG1_RELAY_TOKEN="${SH_E10_RELAY_TOKEN:-e10-dev-token}"
RUNG1_SANDBOX_ID="${SH_E10_SANDBOX_ID:-e10-rung1}"
RUNG1_START_STACK="${SH_E10_START_STACK:-1}" # set 0 to reuse an already-running stack

# Which rungs to run. Rung 1 is the CONTAINER BASELINE and needs grpcurl, docker and pnpm; a
# benchmark host with a hypervisor but none of those can still measure rungs 2-4, which are
# the microVM terms and where the substrate-independent RATIOS live (parked shell on/off,
# memfile pinned/unpinned). Selecting a subset is therefore legitimate.
#
# But a run without rung 1 has NO baseline, and section 7.2's middle band is decided by the
# ratio against it. So a partial run prints the rungs it measured and DECLINES the verdict,
# naming what is missing -- never a favourable verdict from an incomplete run, which is the
# defect most of this driver's review was about.
RUNGS="${SH_E10_RUNGS:-1 2 3 4}"
wants_rung() { case " $RUNGS " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

# The redis image, overridable so an operator can PIN A DIGEST
# (SH_E10_REDIS_IMAGE=redis@sha256:...) for a reproducible run. `redis:7` is a
# floating tag; it is kept as the default because it is what the rest of this repo
# already pulls (deploy/knative/README-worker.md), and because a digest this script
# has never actually pulled would be a guess, not a pin.
RUNG1_REDIS_IMAGE="${SH_E10_REDIS_IMAGE:-redis:7}"


# ---------------------------------------------------------------------------
# Teardown, armed BEFORE anything is started (review 4001908613).
#
# This driver had no `trap` at all, so every `die`/`exit` path left rung 1's whole stack
# running: the worker, the relay, and the redis container. Beyond the leak, the orphans
# keep 8443/6380 bound, so the operator's rerun fails at relay start with a bind error
# that says nothing about the real cause.
#
# Ordering and variable discipline follow build-snapshot.sh's cleanup_on_exit (which
# documents the bug class at length): kill before remove, and nothing this trap touches is
# ever a function local. That is why the pid globals and the temp root are declared HERE,
# above the trap, rather than next to the functions that assign them -- a failure anywhere
# after this point finds them defined, and the `${VAR:-}` defaults keep a future global
# added without one from reintroducing "unbound variable INSIDE the trap", which aborts
# the rest of the trap body.
# ---------------------------------------------------------------------------
RUNG1_WORKER_PID=""
RUNG1_RELAY_PID=""
RUNG1_WORKER_BIN=""
E10_TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/e10-lifecycle.XXXXXX")"

cleanup_on_exit() {
  stop_rung1_stack || true
  [ -z "${E10_TMPDIR:-}" ] || rm -rf "$E10_TMPDIR"
}
trap cleanup_on_exit EXIT

# require_tool refuses a MISSING external binary by name, in preflight, instead of letting
# a rung "run" against a command that is not there. Without this, a missing grpcurl still
# produced a full set of rung-1 timings -- of grpcurl failing to launch -- and a container
# baseline assembled from those is a fabricated number, not a measurement.
# wait_for_relay_port blocks until something is LISTENING on a loopback port, and dies
# naming the log if it never happens. Both drivers previously did `sleep 2` and hoped.
#
# Found on E11's first execution: the relay crashed at startup (a missing package -- the
# pi-fork submodule was not built), nothing was listening, the worker retried a refused
# connection five times, and the first thing the operator saw was a converge TIMEOUT ten
# seconds later. The cause was sitting in the relay log, which nothing pointed at. A stack
# that did not come up must fail where it failed, not as a latency measurement downstream.
wait_for_relay_port() {
  local port="$1" logfile="$2" what="$3" deadline=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  echo "----- last 20 lines of $logfile -----" >&2
  tail -n 20 "$logfile" >&2 2>/dev/null || echo "(no log at $logfile)" >&2
  echo "-------------------------------------" >&2
  die "$what never started listening on 127.0.0.1:$port within 30s - its log is above and in $logfile. Every Exec after this point would have measured a client-side dial failure, not a sandbox."
}

# wait_for_worker_attached blocks until a worker has ATTACHED to the relay, and dies naming
# its log if it never does. Both drivers previously did `sleep 2` and hoped.
#
# The window is not trivial startup. microvm-worker attaches only AFTER PinMemoryFile (an
# mlock whose cost scales with guest RAM), RaiseMemlockLimit, SweepOrphans and pool.Probe --
# and Probe is a FULL restore/resume/run/destroy of a real VM, ~300ms on the validation rig by
# E10's own decomposition. Two seconds usually clears it; the margin is thin and it grows on
# hardware with more guest RAM.
#
# The failure it prevents is one-shot fatal: until the attach lands the relay answers
# "no live worker" INSTANTLY, so E11's converge fails in milliseconds -- and a converge failure
# aborts the entire sweep, both arms, with no warmup tolerance.
#
# Waits on the log line both binaries print (cmd/worker and cmd/microvm-worker). That couples
# to a string, so the timeout dumps the log: if the wording ever changes this becomes a loud,
# diagnosable failure instead of the silent mis-blame `sleep 2` produced.
wait_for_worker_attached() {
  local logfile="$1" what="$2" deadline=$((SECONDS + 60))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if grep -q 'attached, serving execs' "$logfile" 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  echo "----- last 20 lines of $logfile -----" >&2
  tail -n 20 "$logfile" >&2 2>/dev/null || echo "(no log at $logfile)" >&2
  echo "-------------------------------------" >&2
  die "$what never attached to the relay within 60s - its log is above and in $logfile. Until a worker attaches the relay answers 'no live worker' immediately, so every Exec after this point would have measured that refusal rather than a sandbox."
}

# assert_relay_alive is a LIVENESS check, distinct from wait_for_relay_port's readiness check
# -- and the distinction is not academic. On the validation rig a relay bound :8444, a worker
# attached to it, and then the relay DIED on an unhandled Redis 'error' event
# (SocketClosedUnexpectedlyError). Nothing noticed: the port check had already passed, so the
# next Exec burned its full client deadline (360s) before the sweep aborted, and the cause sat
# unread in the relay's own log.
#
# Unlike the supervised deployments, these drivers start the relay themselves and nothing
# restarts it, so a Redis blip mid-run is a lost run rather than a blip. Checking between rungs
# turns six silent minutes into an immediate failure that names the log.
assert_relay_alive() {
  local port="$1" logfile="$2" what="$3"
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    return 0
  fi
  echo "----- last 20 lines of $logfile -----" >&2
  tail -n 20 "$logfile" >&2 2>/dev/null || echo "(no log at $logfile)" >&2
  echo "-------------------------------------" >&2
  die "$what is no longer listening on 127.0.0.1:$port - it started and then DIED mid-run; its log is above and in $logfile. Every Exec from here would time out against a dead relay rather than measure anything."
}

require_tool() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is not on PATH: $2"
}

# require_positive echoes value unchanged when it is exactly ONE line holding one bare
# POSITIVE number, and dies naming the field otherwise. Every field routed through it is a
# MEASURED LATENCY, so all three refusals matter (review 4001908604):
#
#   - not one line: a two-line "number" cannot be interpolated into JSON or awk, and it is
#     the shape a `<pipeline> || echo 0` produces under `pipefail`.
#   - not a number: an empty value is what a swallowed python traceback leaves behind.
#   - zero: a p50 of 0 across an acquire + resume + run + destroy, or across a container
#     Exec round trip through a real relay, is never "instant" -- it means the keys were
#     absent or the rung never ran. It is also the value that made this script print
#     "PROCEED AS DESIGNED - 0.00ms is below the 8ms threshold", the most favourable row in
#     spec section 7.2's table, for a run that measured nothing. A zero container baseline
#     additionally makes verdict()'s ratio "inf", applying the whole table to a
#     non-existent number.
require_positive() {
  local field="$1" value="$2"
  [ "$(printf '%s\n' "$value" | wc -l | tr -d ' ')" = "1" ] ||
    die "$field is not a single value (got '$(printf '%s' "$value" | tr '\n' '|')') - refusing to compute a section 7.2 verdict from it"
  printf '%s\n' "$value" | grep -qxE '[0-9]+(\.[0-9]+)?' ||
    die "$field is not a bare non-negative number (got '$value') - a rung that did not record leaves this empty, and a verdict computed from it would be a verdict about nothing"
  awk -v v="$value" 'BEGIN{exit !(v>0)}' ||
    die "$field is $value, which is not a measurement: refusing to print a section 7.2 verdict from a zero latency. Check the rung's own JSON record and log in $RESULTS."
  printf '%s' "$value"
}

# ---------------------------------------------------------------------------
# Preflight. Each check is its own function, unindented and closed by a bare "}" on
# its own line, so e10-lifecycle.test.sh can extract and source check_governor() in
# isolation (the whole script cannot be sourced: it ends in an unconditional main
# call that demands real flags, root and /dev/kvm).
# ---------------------------------------------------------------------------
check_kvm() {
  [ -e /dev/kvm ] || die "no /dev/kvm"
}

check_cgroups() {
  local t
  t="$(stat -fc %T /sys/fs/cgroup 2>/dev/null || echo unknown)"
  [ "$t" = cgroup2fs ] ||
    die "cgroups v1: spec section 2.4 records it as a cause of high restore latency, so a rung measured here is not comparable"
}

check_swap() {
  [ -z "$(swapon --show 2>/dev/null)" ] ||
    die "swap is on: swapping guest RAM destroys the latency this design exists for, and Firecracker's mincore dirty tracking needs it off (spec section 2.4)"
}

# check_governor implements hardware-corrections E10's three-way logic, replacing
# the brief's naive two-way "cat the path, compare, else die" (which would die with a
# misleading message if the path is simply absent rather than mis-set):
#   - path present, value "performance"   -> proceed, prints "performance"
#   - path present, value anything else   -> REFUSE (it is fixable)
#   - path absent entirely                -> proceed, prints "not exposed" (absence
#                                             is a fact that belongs in the record,
#                                             not a failure)
check_governor() {
  if [ ! -e "$GOVERNOR_PATH" ]; then
    echo "not exposed"
    return 0
  fi
  local g
  g="$(cat "$GOVERNOR_PATH" 2>/dev/null || echo "")"
  if [ "$g" != "performance" ]; then
    die "governor is '$g', not performance: replenishment is a CPU burst, so early rungs would run at higher clocks (spec section 7.5). This is fixable - set it and rerun."
  fi
  echo "performance"
}

# ensure_vmpoolctl guarantees $VMPOOLCTL is an executable BEFORE any rung tries to use it,
# building ./cmd/vmpoolctl if it is absent and refusing loudly if it still is not.
#
# Review 4001908604: this script `go build`s only ./cmd/worker, and $VMPOOLCTL was just a
# PATH -- nothing built it and nothing checked it. A missing binary exits 127 per rung;
# with no `set -e` the `>` redirections left empty JSON files, main()'s readback swallowed
# the resulting json.load failure with `2>/dev/null || echo 0`, and the run then printed
# "PROCEED AS DESIGNED - 0.00ms is below the 8ms threshold". That is the MOST FAVOURABLE
# verdict in spec section 7.2's table, at exit 0, for a run in which rungs 2-4 never
# executed. Building it here (and refusing the p50s below) is what makes that impossible.
ensure_vmpoolctl() {
  if [ -x "$VMPOOLCTL" ]; then
    log "vmpoolctl: $VMPOOLCTL"
    return 0
  fi
  [ -d "$REMOTE_WORKER_DIR" ] ||
    die "vmpoolctl is absent at '$VMPOOLCTL' and the remote-worker module directory was not found either - rungs 2-4 have nothing to drive"
  log "vmpoolctl: absent at $VMPOOLCTL - building ./cmd/vmpoolctl"
  (cd "$REMOTE_WORKER_DIR" && go build -o "$VMPOOLCTL" ./cmd/vmpoolctl) ||
    die "go build ./cmd/vmpoolctl failed - rungs 2, 3 and 4 (the warm hot path, replenishment and teardown) cannot run at all, and a verdict computed without them would be a verdict about nothing"
  [ -x "$VMPOOLCTL" ] ||
    die "go build reported success but '$VMPOOLCTL' is still not executable"
}

preflight() {
  check_kvm
  check_cgroups
  check_swap
  GOVERNOR_STATE="$(check_governor)"
  log "governor: $GOVERNOR_STATE"
  # Tooling, checked by name up front rather than discovered as a 127 mid-rung.
  require_tool python3 "every JSON record and readback in this script is written by python3"
  require_tool go "rung 1's worker and rungs 2-4's vmpoolctl are both built from source here"
  if wants_rung 1; then
    require_tool grpcurl "rung 1 drives its Exec RPCs through grpcurl; without it every timing would measure grpcurl failing to launch (set SH_E10_RUNGS='2 3 4' to skip the container baseline)"
    [ -f "$PROTO_FILE" ] ||
      die "the sandbox proto is missing at $PROTO_FILE - grpcurl cannot encode an Exec request without it, so rung 1 would time a client-side error"
    if [ "$RUNG1_START_STACK" = "1" ]; then
      require_tool docker "rung 1 starts its own redis in a container (set SH_E10_START_STACK=0 to reuse a running stack, or SH_E10_RUNGS='2 3 4' to skip the container baseline)"
      require_tool pnpm "rung 1 starts the real sandbox-relay via pnpm (set SH_E10_START_STACK=0 to reuse a running stack, or SH_E10_RUNGS='2 3 4' to skip the container baseline)"
    fi
  fi
  ensure_vmpoolctl
  mkdir -p "$RESULTS"
}

# ---------------------------------------------------------------------------
# Rung 1: the container baseline. No microVM anywhere in this path — real relay,
# real remote-worker binary, real Exec RPC over grpcurl, spec §7.2's "without it,
# 15ms has nothing to be judged against."
# ---------------------------------------------------------------------------
# RUNG1_WORKER_PID / RUNG1_RELAY_PID / RUNG1_WORKER_BIN are declared next to the EXIT trap
# above, not here: the trap must find them defined however early a failure lands.
start_rung1_stack() {
  if [ "$RUNG1_START_STACK" != "1" ]; then
    log "rung1: SH_E10_START_STACK=0, reusing an already-running stack on port $RUNG1_RELAY_PORT"
    return 0
  fi
  # LOOPBACK ONLY. `-p "${PORT}:6379"` binds 0.0.0.0, which on the documented rig (an
  # EC2 m8i.xlarge with a public interface) publishes an unauthenticated Redis to the
  # internet -- and an open Redis is a standard host-takeover path: CONFIG SET dir +
  # dbfilename, then write an authorized_keys or a cron file. Nothing outside this host
  # has any business reaching a benchmark's scratch redis; the relay and the worker both
  # connect over 127.0.0.1. `--save ''` additionally disables RDB snapshots, so the
  # container never writes a dump file at all.
  log "rung1: starting redis on 127.0.0.1:$RUNG1_REDIS_PORT (loopback only)"
  # || die, matching e11's own redis start. Without it a failed pull, a name collision or a
  # stopped daemon left redis absent, the relay started anyway, and the failure surfaced 20
  # tolerated warmup Execs later as "Exec #21 failed" -- pointing at the grpcurl log rather
  # than at docker.
  docker run --rm -d -p "127.0.0.1:${RUNG1_REDIS_PORT}:6379" --name "sh-e10-redis-$$" \
    "$RUNG1_REDIS_IMAGE" --save '' >/dev/null ||
    die "could not start rung 1's redis on 127.0.0.1:$RUNG1_REDIS_PORT - the relay has nowhere to publish its presence record, so every Exec in the container baseline would fail for a reason that has nothing to do with the baseline"

  log "rung1: starting the relay on :$RUNG1_RELAY_PORT"
  (
    cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)" &&
      SH_RELAY_TOKEN="$RUNG1_RELAY_TOKEN" SH_RELAY_PORT="$RUNG1_RELAY_PORT" \
        REDIS_URL="redis://127.0.0.1:${RUNG1_REDIS_PORT}" \
        pnpm --filter @sh/sandbox-relay start >"$RESULTS/e10-rung1-relay.log" 2>&1 &
    echo $! >"$RESULTS/.rung1-relay.pid"
  )
  RUNG1_RELAY_PID="$(cat "$RESULTS/.rung1-relay.pid" 2>/dev/null || echo "")"
  wait_for_relay_port "$RUNG1_RELAY_PORT" "$RESULTS/e10-rung1-relay.log" "rung 1's sandbox-relay"

  # A binary is built and spawned directly rather than `go run`'d - `go run` forks a
  # child SIGKILL cannot reliably reach through the wrapper, which matters for clean
  # teardown here the same way it does in packages/k8s-sandbox/test/live-relay.test.ts.
  RUNG1_WORKER_BIN="$RESULTS/.rung1-worker-bin"
  log "rung1: building the worker binary"
  (cd "$REMOTE_WORKER_DIR" && go build -o "$RUNG1_WORKER_BIN" ./cmd/worker) ||
    die "go build ./cmd/worker failed - rung 1 is the baseline every microVM number is priced against, and a rung 1 assembled from a missing binary would time a shell error"

  log "rung1: starting the worker"
  SANDBOX_ID="$RUNG1_SANDBOX_ID" RELAY_ADDR="localhost:${RUNG1_RELAY_PORT}" \
    SANDBOX_TOKEN="$RUNG1_RELAY_TOKEN" \
    "$RUNG1_WORKER_BIN" >"$RESULTS/e10-rung1-worker.log" 2>&1 &
  RUNG1_WORKER_PID="$!"

  wait_for_worker_attached "$RESULTS/e10-rung1-worker.log" "rung 1's worker"
}

stop_rung1_stack() {
  [ "$RUNG1_START_STACK" = "1" ] || return 0
  # ${VAR:-} because this also runs from the EXIT trap, which can fire before either pid is
  # assigned (build-snapshot.sh's cleanup_on_exit documents that exact failure). Kill
  # before remove, same ordering.
  [ -n "${RUNG1_WORKER_PID:-}" ] && kill "${RUNG1_WORKER_PID:-}" 2>/dev/null
  [ -n "${RUNG1_RELAY_PID:-}" ] && kill "${RUNG1_RELAY_PID:-}" 2>/dev/null
  docker rm -f "sh-e10-redis-$$" >/dev/null 2>&1 || true
  RUNG1_WORKER_PID=""
  RUNG1_RELAY_PID=""
  return 0
}

# json_escape escapes a command string for embedding in a JSON string literal.
json_escape() {
  python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$1"
}

# grpc_exec_ms drives one Exec RPC through grpcurl and prints the host-side wall
# time, in milliseconds, that the call took — never a guest-side timestamp.
#
# It also RETURNS THE RPC's OWN STATUS, so a caller can tell a measured Exec from a failed
# one. It used to swallow it: a failing (or missing) grpcurl still produced a timing line,
# so rung 1 would report a container baseline assembled from RPC failures -- fast,
# confident, and not a measurement of anything. run_rung1 refuses those below.
grpc_exec_ms() {
  local cmd="$1" req_id="$2" t0 t1 rc=0
  t0="$(date +%s%N)"
  grpcurl -plaintext -max-time "$RUNG1_EXEC_MAX_TIME_S" -import-path "$PROTO_IMPORT_PATH" -proto "$PROTO_REL_PATH" \
    -d "{\"sandbox_id\":\"$RUNG1_SANDBOX_ID\",\"exec\":{\"req_id\":$req_id,\"command\":$(json_escape "$cmd"),\"timeout_s\":30}}" \
    "localhost:${RUNG1_RELAY_PORT}" sandbox.v1.SandboxExec/Exec >/dev/null 2>>"$RESULTS/e10-rung1-grpcurl.log" || rc=$?
  t1="$(date +%s%N)"
  echo $(((t1 - t0) / 1000000))
  return "$rc"
}

# rung1_tool_call_mix is the "Exec-per-tool-call mix" the brief calls for as this
# rung's anchor, beyond the trivial command and the 1 MiB read: a short, defensible
# sample of the command SHAPES an agent's tool calls actually produce (list, read,
# write, search), each a single real Exec round trip rather than a shell pipeline
# assembled to look busy.
rung1_tool_call_mix() {
  echo "true"                                    # trivial command (brief, explicit)
  echo "head -c 1048576 /dev/zero | wc -c"        # 1 MiB read (brief, explicit)
  echo "ls -la /tmp"                              # list - a directory-listing tool call
  echo "cat /etc/hostname"                        # read - a small file read
  echo "echo e10-mix > /tmp/e10-mix-$$.tmp" # write - a small file write
  echo "grep -c e10 /tmp/e10-mix-$$.tmp"           # search - a grep over a small file
  echo "rm -f /tmp/e10-mix-$$.tmp"
}

run_rung1() {
  log "rung1: container baseline"
  start_rung1_stack
  # Under $E10_TMPDIR rather than a bare mktemp, so the EXIT trap reclaims it (see the trap).
  local i=0 times_file failures=0
  times_file="$E10_TMPDIR/rung1.times"
  : >"$times_file"
  local want=$((ITERS + WARMUP))
  while [ "$(wc -l <"$times_file" 2>/dev/null || echo 0)" -lt "$want" ]; do
    while IFS= read -r cmd; do
      i=$((i + 1))
      # A FAILED Exec is not a slow Exec. Failures inside the warmup window are tolerated
      # (that is what the warmup is for -- the relay and worker have just bound their
      # ports); one after it stops the rung, because the container baseline is the number
      # every microVM figure in spec section 7.2 is priced against, and a baseline built
      # partly out of RPC failures is not a baseline.
      if ! grpc_exec_ms "$cmd" "$i" >>"$times_file"; then
        failures=$((failures + 1))
        [ "$i" -le "$WARMUP" ] ||
          die "rung1: Exec #$i ($cmd) failed against the container baseline stack (grpcurl's own error is in $RESULTS/e10-rung1-grpcurl.log; the relay and worker logs are alongside it). Refusing to price the microVM tier against a baseline assembled from failed RPCs."
      fi
    done < <(rung1_tool_call_mix)
  done
  [ "$failures" -eq 0 ] || log "rung1: $failures Exec(s) failed inside the discarded warmup window"
  tail -n "+$((WARMUP + 1))" "$times_file" | head -n "$ITERS" >"${times_file}.steady"
  local steady
  steady="$(wc -l <"${times_file}.steady" 2>/dev/null || echo 0)"
  [ "$steady" -ge "$ITERS" ] ||
    die "rung1: collected $steady steady-state samples, wanted $ITERS - refusing to compute a baseline percentile from a short sample"
  # percentile refuses an absent measurement rather than printing 0 (see its own comment),
  # so these are `|| die`, never `|| echo 0`.
  RUNG1_P50_MS="$(percentile 50 "${times_file}.steady")" ||
    die "rung1: no steady-state samples to take a p50 of - the container baseline has no value, so nothing can be priced against it"
  RUNG1_P95_MS="$(percentile 95 "${times_file}.steady")" ||
    die "rung1: no steady-state samples to take a p95 of"
  write_json_record "rung1" "container" "$SUBSTRATE" \
    "$(printf '{"rung":"container-baseline","arm":"container","substrate":%s,"iterations":%d,"warmup_discarded":%d,"p50_ms":%s,"p95_ms":%s}' \
      "$(json_escape "$SUBSTRATE")" "$ITERS" "$WARMUP" "$RUNG1_P50_MS" "$RUNG1_P95_MS")"
  rm -f "$times_file" "${times_file}.steady"
  stop_rung1_stack
}

# ---------------------------------------------------------------------------
# percentile: nearest-rank percentile over a file of one number per line. Pure host
# arithmetic; no guest timing anywhere in this script.
# ---------------------------------------------------------------------------
percentile() {
  local p="$1" file="$2"
  # A missing or EMPTY input file is not a zero percentile, it is the absence of any
  # measurement -- so this refuses (prints nothing, returns non-zero) and the caller says
  # which rung had no samples. Kept identical to e11-density.sh's copy, for two reasons
  # that review 4001908597 raised against that one:
  #
  #   1. the value. A 0 here becomes the container baseline the whole of spec section 7.2
  #      divides by, and `ratio=inf` or "0.00ms is below the threshold" is the most
  #      favourable verdict in the table, printed for a rung that measured nothing.
  #   2. the SHAPE. `sort -n` on a missing file exits 2, awk still printed 0, and
  #      `pipefail` propagated sort's status -- so a caller's `|| echo 0` would append a
  #      SECOND line and produce a two-line "number". The `[ -s ]` guard means `sort` is
  #      never handed a missing file, and the awk END branch exits non-zero.
  [ -s "$file" ] || return 1
  sort -n "$file" | awk -v p="$p" '
    { a[NR] = $1; n = NR }
    END {
      if (n == 0) { exit 1 }
      rank = int((p / 100.0) * n)
      if (rank < 1) rank = 1
      if (rank > n) rank = n
      print a[rank]
    }'
}

write_json_record() {
  local rung="$1" arm="$2" substrate="$3" body="$4"
  echo "$body" >"$RESULTS/e10-${rung}-${arm}-${substrate}.json"
}

# ---------------------------------------------------------------------------
# Rungs 2-4, driven by vmpoolctl. Run once per arm, in randomized order, with
# drop_caches between arms (spec §7.5: page-cache asymmetry).
# ---------------------------------------------------------------------------
drop_caches() {
  if [ -w /proc/sys/vm/drop_caches ]; then
    echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || log "drop_caches: not permitted, continuing (informational only)"
  else
    log "drop_caches: /proc/sys/vm/drop_caches not writable here, continuing"
  fi
}

# shuffle_arms prints ARMS in randomized order using awk's rand(), seeded by the
# current PID and time so back-to-back runs do not always order arms the same way.
shuffle_arms() {
  printf '%s\n' "${ARMS[@]}" | awk -v seed="$(($$ + $(date +%s)))" 'BEGIN{srand(seed)} {print rand()"\t"$0}' | sort -n | cut -f2-
}

# vmpoolctl_run drives ONE vmpoolctl mode and REFUSES to continue unless it wrote a
# non-empty, parseable JSON record to $1.
#
# E11 grew a `[ -s ]` no-record guard for exactly this class and E10 had no equivalent
# (review 4001908604): with `set -e` absent, a vmpoolctl that exited 127 (missing binary),
# 1 (a failed restore) or anything else left an EMPTY file behind, the sweep carried on to
# the next rung, and the readback in main() then defaulted the missing p50s to 0. Every
# rung's record is now checked at the point it is written, naming the rung and its log.
vmpoolctl_run() {
  local out_json="$1" log_file="$2" key="$3"
  shift 3
  # vmpoolctl requires a command after `--` in every mode, including the replenishment and
  # teardown modes that never run it, so that every rung's invocation has the same shape.
  # Supply a no-op when the caller did not: rungs 3 and 4 measure a lifecycle phase rather
  # than a command, and both died on "a command is required after --" the first time they ran.
  local has_cmd=0 a
  for a in "$@"; do [ "$a" = "--" ] && has_cmd=1; done
  if [ "$has_cmd" -eq 0 ]; then set -- "$@" -- true; fi
  "$VMPOOLCTL" --snapshot-dir="$SNAPSHOT_DIR" --workspace-root="$WORKSPACE_ROOT" \
    --substrate="$SUBSTRATE" --iterations="$ITERS" --warmup="$WARMUP" --json \
    --key="$key" "$@" >"$out_json" 2>"$log_file" ||
    die "vmpoolctl exited non-zero for $key (its stderr is in $log_file) - refusing to continue a ladder with a rung that did not run"
  [ -s "$out_json" ] ||
    die "vmpoolctl wrote NO record for $key to $out_json (its stderr is in $log_file). A ladder missing this rung would still print a verdict, and it would be the most favourable one in the table."
  python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$out_json" >/dev/null 2>&1 ||
    die "the record vmpoolctl wrote for $key at $out_json is not valid JSON - refusing to read a p50 out of it"
}

run_rung2() {
  local arm="$1"
  log "rung2 ($arm): warm hot path, parked-bash (no stdin)"
  vmpoolctl_run "$RESULTS/e10-rung2-${arm}-${SUBSTRATE}-parked.json" \
    "$RESULTS/e10-rung2-${arm}-parked.log" "e10-r2-${arm}-parked" \
    --vmm="$arm" --mode=exec -- true
  log "rung2 ($arm): warm hot path, fresh-child (--stdin set)"
  vmpoolctl_run "$RESULTS/e10-rung2-${arm}-${SUBSTRATE}-freshchild.json" \
    "$RESULTS/e10-rung2-${arm}-freshchild.log" "e10-r2-${arm}-freshchild" \
    --vmm="$arm" --mode=exec --stdin="e10-stdin-payload" -- "cat >/dev/null"
}

run_rung3() {
  local arm="$1"
  log "rung3 ($arm): replenishment, cold memfile"
  vmpoolctl_run "$RESULTS/e10-rung3-${arm}-${SUBSTRATE}-cold.json" \
    "$RESULTS/e10-rung3-${arm}-cold.log" "e10-r3-${arm}-cold" \
    --vmm="$arm" --mode=replenish --pin-memfile=false
  log "rung3 ($arm): replenishment, pinned memfile"
  vmpoolctl_run "$RESULTS/e10-rung3-${arm}-${SUBSTRATE}-pinned.json" \
    "$RESULTS/e10-rung3-${arm}-pinned.log" "e10-r3-${arm}-pinned" \
    --vmm="$arm" --mode=replenish --pin-memfile=true
}

run_rung4() {
  local arm="$1"
  local mode
  for mode in teardown-inflight teardown-standby teardown-bulk; do
    log "rung4 ($arm): $mode"
    vmpoolctl_run "$RESULTS/e10-rung4-${arm}-${mode}-${SUBSTRATE}.json" \
      "$RESULTS/e10-rung4-${arm}-${mode}.log" "e10-r4-${arm}-${mode}" \
      --vmm="$arm" --mode="$mode"
  done
}

run_vmpoolctl_rungs() {
  local arm order
  order="$(shuffle_arms)"
  local first=1
  while IFS= read -r arm; do
    [ -n "$arm" ] || continue
    if [ "$first" -eq 0 ]; then
      drop_caches
    fi
    first=0
    log "arm: $arm"
    run_rung2 "$arm"
    run_rung3 "$arm"
    run_rung4 "$arm"
  done <<<"$order"
}

# ---------------------------------------------------------------------------
# The decision-rule verdict, computed rather than eyeballed (spec §7.2's four-row
# table, plus its tie-break rules). Pure function of its arguments — no globals read
# — so it can be extracted and sourced on its own by e10-lifecycle.test.sh the same
# way check_governor() is, and no code path inside it can print STOP or MANDATORY
# unless substrate is exactly "metal": that is the structural (not commentary)
# enforcement hardware-corrections E9 requires.
#
# Args: substrate warm_ms container_ms repl_cpu_ms
# Prints: three lines - ratio=, warm_verdict=, repl_verdict=.
verdict() {
  local substrate="$1" warm_ms="$2" container_ms="$3" repl_ms="$4"
  local ratio warm_cutoff repl_cutoff warm_verdict repl_verdict is_metal=0

  [ "$substrate" = "metal" ] && is_metal=1

  ratio="$(awk -v w="$warm_ms" -v c="$container_ms" 'BEGIN{ if (c>0) printf "%.2f", w/c; else print "inf" }')"

  # Spec §7.2's row 1 carries TWO dual thresholds on the SAME line, "because nested virt
  # taxes exactly the VM-exit-heavy work restore consists of":
  #
  #   Warm hot path < 5ms metal / < 8ms nested
  #   AND replenishment CPU < 25ms metal / < 40ms nested
  #
  # Final review M2: only the first was honoured here. The replenishment boundary was
  # hard-coded at 25/50 with no nested analogue, so on the nested rig -- per RULING 20-B
  # the only substrate this project has -- a replenishment CPU anywhere in [25, 40) ms was
  # put in row 2 ("proceed, re-priced, split pre-authorised if the ratio exceeds 2x") when
  # the spec's own nested threshold puts it in row 1 ("proceed as designed"), AND the
  # verdict text labelled that nested measurement with a "metal band". Both halves matter:
  # the row was wrong for the substrate, and labelling a nested number with a metal band is
  # the exact substrate conflation this script's discipline exists to prevent.
  #
  # Rows 2, 3 and 4 keep their metal-only 15ms / [25,50] / 50ms figures, because the spec
  # qualifies each of those with "metal" explicitly and states "nested fires no stop rule"
  # rather than inventing further nested thresholds. Only the row-1 boundaries are dual, so
  # only they are parameterised: off metal, row 2's lower edge moves to repl_cutoff and its
  # text says so, and no path can print STOP or MANDATORY (hardware-corrections E9).
  warm_cutoff=5
  [ "$is_metal" -eq 1 ] || warm_cutoff=8
  repl_cutoff=25
  [ "$is_metal" -eq 1 ] || repl_cutoff=40

  if awk -v w="$warm_ms" 'BEGIN{exit !(w>=15)}'; then
    # Row 3: warm hot path >= 15ms. "'>= 15ms' stays a hard stop regardless of ratio."
    # "Nested fires no stop rule ... it never, by itself, stops the design."
    if [ "$is_metal" -eq 1 ]; then
      warm_verdict="STOP: warm hot path ${warm_ms}ms >= 15ms on metal - the design fails on its own terms (spec section 9 rejected alternative: per-session resident VMs)"
    else
      warm_verdict="ABOVE 15ms on $substrate (${warm_ms}ms) - schedules a metal run; nested fires no stop rule by itself"
    fi
  elif awk -v w="$warm_ms" -v t="$warm_cutoff" 'BEGIN{exit !(w>=t)}'; then
    # Row 2: middle band. "In the middle band the ratio governs."
    if awk -v r="$ratio" 'BEGIN{exit !(r>2)}'; then
      if [ "$is_metal" -eq 1 ]; then
        warm_verdict="PROCEED, RE-PRICED against the container baseline - ratio ${ratio}x exceeds 2x, the section 3.3 split is pre-authorised"
      else
        warm_verdict="PROCEED, RE-PRICED against the container baseline on $substrate - ratio ${ratio}x exceeds 2x (pre-authorisation is contingent on confirming this on metal)"
      fi
    else
      warm_verdict="PROCEED, RE-PRICED against the container baseline - ratio ${ratio}x, within 2x"
    fi
  else
    # Row 1: proceed as designed.
    warm_verdict="PROCEED AS DESIGNED - ${warm_ms}ms is below the ${warm_cutoff}ms threshold; ratio ${ratio}x. VMM choice is then decided on the correctness/security axis of section 4.3"
  fi

  if awk -v r="$repl_ms" 'BEGIN{exit !(r>50)}'; then
    # "Replenishment CPU > 50ms metal: the split becomes MANDATORY, not a fallback."
    if [ "$is_metal" -eq 1 ]; then
      repl_verdict="MANDATORY: replenishment CPU ${repl_ms}ms > 50ms metal - the section 3.3 Exec.streaming split is no longer a fallback"
    else
      repl_verdict="ABOVE 50ms on $substrate (${repl_ms}ms) - schedules a metal run; never mandatory except on metal"
    fi
  elif awk -v r="$repl_ms" -v t="$repl_cutoff" 'BEGIN{exit !(r>=t)}'; then
    # Row 2's replenishment half. Its lower edge is the row-1 boundary for THIS substrate.
    if [ "$is_metal" -eq 1 ]; then
      repl_verdict="PROCEED, RE-PRICED - replenishment CPU ${repl_ms}ms is in the [25,50]ms metal band; section 3.3 split pre-authorised if the warm-path ratio exceeds 2x"
    else
      repl_verdict="PROCEED, RE-PRICED on $substrate - replenishment CPU ${repl_ms}ms is in the [${repl_cutoff},50]ms band for $substrate; section 3.3 split pre-authorisation is contingent on confirming this on metal"
    fi
  else
    # Row 1's replenishment half: below this substrate's own threshold.
    repl_verdict="PROCEED - replenishment CPU ${repl_ms}ms < ${repl_cutoff}ms $substrate"
  fi

  echo "ratio=${ratio}x"
  echo "warm_verdict=${warm_verdict}"
  echo "repl_verdict=${repl_verdict}"
}

print_verdict() {
  local substrate="$1" warm_ms="$2" container_ms="$3" repl_ms="$4"
  local out ratio warm_verdict repl_verdict
  out="$(verdict "$substrate" "$warm_ms" "$container_ms" "$repl_ms")"
  ratio="$(echo "$out" | sed -n 's/^ratio=//p')"
  warm_verdict="$(echo "$out" | sed -n 's/^warm_verdict=//p')"
  repl_verdict="$(echo "$out" | sed -n 's/^repl_verdict=//p')"
  echo
  echo "=== E10 decision-rule verdict (substrate=$substrate) ==="
  echo "warm hot path p50 = ${warm_ms}ms ($substrate)   container baseline p50 = ${container_ms}ms"
  echo "ratio = ${ratio}   ->  ${warm_verdict}"
  echo "replenishment CPU p50 = ${repl_ms}ms      ->  ${repl_verdict}"
  if [ "$substrate" != "metal" ]; then
    echo "(substrate is not metal: this is a schedule-a-metal-run recommendation, never a stop or mandatory verdict - hardware-corrections E9)"
  fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  preflight
  if wants_rung 1; then
    run_rung1
  else
    log "rung1: SKIPPED (SH_E10_RUNGS='$RUNGS') - there will be no container baseline, so no section 7.2 verdict"
  fi
  run_vmpoolctl_rungs

  # Pull the warm-path (rung2, parked) and replenishment (rung3, pinned) p50s the
  # summary needs for the verdict. main.go's runResult carries these as
  # p50_run_us/p50_acquire_us/... and the CPU child figure; this script reads them
  # back out of the JSON it just wrote rather than re-deriving them.
  local warm_ms container_ms repl_ms arm="${ARMS[0]}"
  local warm_us repl_us
  # NOT `2>/dev/null || echo 0` (review 4001908604). That swallowed the json.load failure a
  # rung that never ran produces, defaulted both p50s to 0, and sent verdict() down its
  # row-1 branch: "PROCEED AS DESIGNED - 0.00ms is below the 8ms threshold", at exit 0, for
  # a run with no rung 2 or rung 3 at all. The traceback is now visible, the failure is
  # fatal, and require_positive refuses the zero itself -- a p50 of 0 us across an
  # acquire, a resume, a run and a destroy is not a fast VM, it is an absent measurement.
  warm_us="$(python3 -c "
import json
d = json.load(open('$RESULTS/e10-rung2-${arm}-${SUBSTRATE}-parked.json'))
print(d.get('p50_run_us',0)+d.get('p50_acquire_us',0)+d.get('p50_resume_us',0)+d.get('p50_destroy_us',0))
")" || die "could not read the rung 2 record at $RESULTS/e10-rung2-${arm}-${SUBSTRATE}-parked.json (the traceback is above) - refusing to print a section 7.2 verdict about a warm hot path that was never measured"
  warm_us="$(require_positive warm_hot_path_p50_us "$warm_us")" || exit 1
  repl_us="$(python3 -c "
import json
d = json.load(open('$RESULTS/e10-rung3-${arm}-${SUBSTRATE}-pinned.json'))
print(d.get('cpu_child_us',0))
")" || die "could not read the rung 3 record at $RESULTS/e10-rung3-${arm}-${SUBSTRATE}-pinned.json (the traceback is above) - refusing to print a section 7.2 verdict about a replenishment CPU cost that was never measured"
  repl_us="$(require_positive replenishment_cpu_p50_us "$repl_us")" || exit 1
  if ! wants_rung 1; then
    # The rung 2/3/4 records are already on disk; what cannot be produced is the RATIO row,
    # because section 7.2's middle band is defined against the container baseline. Say that,
    # report the terms that were measured, and exit non-zero so no caller mistakes a partial
    # run for a completed experiment.
    echo
    echo "PARTIAL RUN - no section 7.2 verdict"
    echo "  rungs run:              $RUNGS"
    # warm_us/repl_us, NOT warm_ms/repl_ms: those two are declared unset at the top of
    # main and only assigned BELOW this branch, so reading them here was an unbound-variable
    # error under `set -u` -- this summary would have died instead of printing, on the exact
    # path the runbook recommends when a host lacks grpcurl/docker/pnpm
    # (SH_E10_RUNGS='2 3 4'). Converted inline rather than by hoisting the assignments,
    # which would have to move above the rung-1 readback they depend on.
    echo "  warm hot path p50:      $(awk -v u="$warm_us" 'BEGIN{printf "%.2f", u/1000.0}')ms   (rung 2, parked)"
    echo "  replenishment CPU p50:  $(awk -v u="$repl_us" 'BEGIN{printf "%.2f", u/1000.0}')ms   (rung 3, pinned)"
    echo "  container baseline:     NOT MEASURED - rung 1 was not selected"
    echo "  substrate:              $SUBSTRATE"
    echo
    echo "The decision-rule table is NOT evaluated: its middle band is a ratio against the"
    echo "container baseline, and its absolute rows are only meaningful beside it. Per-rung"
    echo "records are in $RESULTS. Re-run with SH_E10_RUNGS='1 2 3 4' on a host with grpcurl,"
    echo "docker and pnpm to get a verdict."
    exit 3
  fi
  container_ms="$(require_positive container_baseline_p50_ms "${RUNG1_P50_MS:-}")" || exit 1
  warm_ms="$(awk -v u="$warm_us" 'BEGIN{printf "%.2f", u/1000.0}')"
  repl_ms="$(awk -v u="$repl_us" 'BEGIN{printf "%.2f", u/1000.0}')"

  local arms_json
  arms_json="$(printf '%s\n' "${ARMS[@]}" | python3 -c 'import json,sys; print(json.dumps([l.strip() for l in sys.stdin if l.strip()]))')"
  python3 -c "
import json
summary = {
  'substrate': '$SUBSTRATE',
  'arms': json.loads('''$arms_json'''),
  'governor': '${GOVERNOR_STATE:-unknown}',
  'iterations': $ITERS,
  'warmup': $WARMUP,
  'container_p50_ms': $container_ms,
  'warm_p50_ms': $warm_ms,
  'replenishment_cpu_p50_ms': $repl_ms,
}
print(json.dumps(summary, indent=2))
" >"$RESULTS/e10-summary.json"
  # The same no-record guard E11's rung writer has: `set -e` is deliberately absent here,
  # so a writer that died of a bad interpolation would otherwise leave an empty summary and
  # let the verdict print anyway (review 4001908604).
  [ -s "$RESULTS/e10-summary.json" ] ||
    die "the run summary at $RESULTS/e10-summary.json was not written (the writer's traceback is above) - refusing to print a verdict that no record backs"

  print_verdict "$SUBSTRATE" "$warm_ms" "$container_ms" "$repl_ms"
}

# Allow this file to be sourced (for tests that extract individual functions) without
# invoking main — but running it directly, as an operator would, still calls main.
if [ "${E10_LIFECYCLE_SOURCE_ONLY:-0}" != "1" ]; then
  main "$@"
fi
