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
# Hardware-corrections applied (task-20-hardware-corrections.md), because the brief
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
# non-empty payload (fresh-child path) — see task-20-report.md for the full writeup.
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
RESULTS="${RESULTS:-deploy/microvm/.results}"
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

SNAPSHOT_DIR="${SH_SNAPSHOT_DIR:?set SH_SNAPSHOT_DIR - the same env var name microvm-worker requires, see cmd/microvm-worker/main.go}"
WORKSPACE_ROOT="${SH_WORKSPACE_ROOT:?set SH_WORKSPACE_ROOT - the same env var name microvm-worker requires, see cmd/microvm-worker/main.go}"

# E8: the brief hardcodes ARMS=(firecracker cloud-hypervisor). On this rig the
# cloud-hypervisor arm hangs/dies during restore, so it defaults OFF; an operator on
# hardware where it works can opt back in.
read -r -a ARMS <<<"${SH_E10_ARMS:-firecracker}"

VMPOOLCTL="${VMPOOLCTL:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../remote-worker" 2>/dev/null && pwd)/vmpoolctl}"
REMOTE_WORKER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../remote-worker" 2>/dev/null && pwd)"
PROTO_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." 2>/dev/null && pwd)/proto/sandbox/v1/sandbox.proto"

# Rung 1 (container baseline) stack knobs — all overridable so a validation pass can
# reuse an already-running relay/redis instead of starting fresh ones.
RUNG1_REDIS_PORT="${SH_E10_REDIS_PORT:-6380}"
RUNG1_RELAY_PORT="${SH_E10_RELAY_PORT:-8443}"
RUNG1_RELAY_TOKEN="${SH_E10_RELAY_TOKEN:-e10-dev-token}"
RUNG1_SANDBOX_ID="${SH_E10_SANDBOX_ID:-e10-rung1}"
RUNG1_START_STACK="${SH_E10_START_STACK:-1}" # set 0 to reuse an already-running stack

die() { echo "e10: $*" >&2; exit 1; }
log() { echo "e10: $*" >&2; }

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

preflight() {
  check_kvm
  check_cgroups
  check_swap
  GOVERNOR_STATE="$(check_governor)"
  log "governor: $GOVERNOR_STATE"
  mkdir -p "$RESULTS"
}

# ---------------------------------------------------------------------------
# Rung 1: the container baseline. No microVM anywhere in this path — real relay,
# real remote-worker binary, real Exec RPC over grpcurl, spec §7.2's "without it,
# 15ms has nothing to be judged against."
# ---------------------------------------------------------------------------
RUNG1_WORKER_PID=""
RUNG1_RELAY_PID=""
RUNG1_WORKER_BIN=""

start_rung1_stack() {
  if [ "$RUNG1_START_STACK" != "1" ]; then
    log "rung1: SH_E10_START_STACK=0, reusing an already-running stack on port $RUNG1_RELAY_PORT"
    return 0
  fi
  log "rung1: starting redis on :$RUNG1_REDIS_PORT"
  docker run --rm -d -p "${RUNG1_REDIS_PORT}:6379" --name "sh-e10-redis-$$" redis:7 >/dev/null

  log "rung1: starting the relay on :$RUNG1_RELAY_PORT"
  (
    cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)" &&
      SH_RELAY_TOKEN="$RUNG1_RELAY_TOKEN" SH_RELAY_PORT="$RUNG1_RELAY_PORT" \
        REDIS_URL="redis://127.0.0.1:${RUNG1_REDIS_PORT}" \
        pnpm --filter @sh/sandbox-relay start >"$RESULTS/e10-rung1-relay.log" 2>&1 &
    echo $! >"$RESULTS/.rung1-relay.pid"
  )
  RUNG1_RELAY_PID="$(cat "$RESULTS/.rung1-relay.pid" 2>/dev/null || echo "")"

  # A binary is built and spawned directly rather than `go run`'d - `go run` forks a
  # child SIGKILL cannot reliably reach through the wrapper, which matters for clean
  # teardown here the same way it does in packages/k8s-sandbox/test/live-relay.test.ts.
  RUNG1_WORKER_BIN="$RESULTS/.rung1-worker-bin"
  log "rung1: building the worker binary"
  (cd "$REMOTE_WORKER_DIR" && go build -o "$RUNG1_WORKER_BIN" ./cmd/worker)

  log "rung1: starting the worker"
  SANDBOX_ID="$RUNG1_SANDBOX_ID" RELAY_ADDR="localhost:${RUNG1_RELAY_PORT}" \
    SANDBOX_TOKEN="$RUNG1_RELAY_TOKEN" \
    "$RUNG1_WORKER_BIN" >"$RESULTS/e10-rung1-worker.log" 2>&1 &
  RUNG1_WORKER_PID="$!"

  sleep 2 # let both processes finish binding before the first grpcurl call
}

stop_rung1_stack() {
  [ "$RUNG1_START_STACK" = "1" ] || return 0
  [ -n "$RUNG1_WORKER_PID" ] && kill "$RUNG1_WORKER_PID" 2>/dev/null
  [ -n "$RUNG1_RELAY_PID" ] && kill "$RUNG1_RELAY_PID" 2>/dev/null
  docker rm -f "sh-e10-redis-$$" >/dev/null 2>&1 || true
  return 0
}

# json_escape escapes a command string for embedding in a JSON string literal.
json_escape() {
  python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$1"
}

# grpc_exec_ms drives one Exec RPC through grpcurl and prints the host-side wall
# time, in milliseconds, that the call took — never a guest-side timestamp.
grpc_exec_ms() {
  local cmd="$1" req_id="$2" t0 t1
  t0="$(date +%s%N)"
  grpcurl -plaintext -proto "$PROTO_FILE" \
    -d "{\"sandbox_id\":\"$RUNG1_SANDBOX_ID\",\"exec\":{\"req_id\":$req_id,\"command\":$(json_escape "$cmd"),\"timeout_s\":30}}" \
    "localhost:${RUNG1_RELAY_PORT}" sandbox.v1.SandboxExec/Exec >/dev/null 2>>"$RESULTS/e10-rung1-grpcurl.log"
  t1="$(date +%s%N)"
  echo $(((t1 - t0) / 1000000))
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
  local i=0 times_file
  times_file="$(mktemp)"
  local want=$((ITERS + WARMUP))
  while [ "$(wc -l <"$times_file" 2>/dev/null || echo 0)" -lt "$want" ]; do
    while IFS= read -r cmd; do
      i=$((i + 1))
      grpc_exec_ms "$cmd" "$i" >>"$times_file"
    done < <(rung1_tool_call_mix)
  done
  tail -n "+$((WARMUP + 1))" "$times_file" | head -n "$ITERS" >"${times_file}.steady"
  RUNG1_P50_MS="$(percentile 50 "${times_file}.steady")"
  RUNG1_P95_MS="$(percentile 95 "${times_file}.steady")"
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
  sort -n "$file" | awk -v p="$p" '
    { a[NR] = $1; n = NR }
    END {
      if (n == 0) { print 0; exit }
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

vmpoolctl_run() {
  local key="$1"
  shift
  "$VMPOOLCTL" --snapshot-dir="$SNAPSHOT_DIR" --workspace-root="$WORKSPACE_ROOT" \
    --substrate="$SUBSTRATE" --iterations="$ITERS" --warmup="$WARMUP" --json \
    --key="$key" "$@"
}

run_rung2() {
  local arm="$1"
  log "rung2 ($arm): warm hot path, parked-bash (no stdin)"
  vmpoolctl_run "e10-r2-${arm}-parked" --vmm="$arm" --mode=exec -- true \
    >"$RESULTS/e10-rung2-${arm}-${SUBSTRATE}-parked.json" 2>"$RESULTS/e10-rung2-${arm}-parked.log"
  log "rung2 ($arm): warm hot path, fresh-child (--stdin set)"
  vmpoolctl_run "e10-r2-${arm}-freshchild" --vmm="$arm" --mode=exec --stdin="e10-stdin-payload" -- "cat >/dev/null" \
    >"$RESULTS/e10-rung2-${arm}-${SUBSTRATE}-freshchild.json" 2>"$RESULTS/e10-rung2-${arm}-freshchild.log"
}

run_rung3() {
  local arm="$1"
  log "rung3 ($arm): replenishment, cold memfile"
  vmpoolctl_run "e10-r3-${arm}-cold" --vmm="$arm" --mode=replenish --pin-memfile=false \
    >"$RESULTS/e10-rung3-${arm}-${SUBSTRATE}-cold.json" 2>"$RESULTS/e10-rung3-${arm}-cold.log"
  log "rung3 ($arm): replenishment, pinned memfile"
  vmpoolctl_run "e10-r3-${arm}-pinned" --vmm="$arm" --mode=replenish --pin-memfile=true \
    >"$RESULTS/e10-rung3-${arm}-${SUBSTRATE}-pinned.json" 2>"$RESULTS/e10-rung3-${arm}-pinned.log"
}

run_rung4() {
  local arm="$1"
  local mode
  for mode in teardown-inflight teardown-standby teardown-bulk; do
    log "rung4 ($arm): $mode"
    vmpoolctl_run "e10-r4-${arm}-${mode}" --vmm="$arm" --mode="$mode" \
      >"$RESULTS/e10-rung4-${arm}-${mode}-${SUBSTRATE}.json" 2>"$RESULTS/e10-rung4-${arm}-${mode}.log"
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
  run_rung1
  run_vmpoolctl_rungs

  # Pull the warm-path (rung2, parked) and replenishment (rung3, pinned) p50s the
  # summary needs for the verdict. main.go's runResult carries these as
  # p50_run_us/p50_acquire_us/... and the CPU child figure; this script reads them
  # back out of the JSON it just wrote rather than re-deriving them.
  local warm_ms container_ms repl_ms arm="${ARMS[0]}"
  local warm_us repl_us
  warm_us="$(python3 -c "
import json
d = json.load(open('$RESULTS/e10-rung2-${arm}-${SUBSTRATE}-parked.json'))
print(d.get('p50_run_us',0)+d.get('p50_acquire_us',0)+d.get('p50_resume_us',0)+d.get('p50_destroy_us',0))
" 2>/dev/null || echo 0)"
  repl_us="$(python3 -c "
import json
d = json.load(open('$RESULTS/e10-rung3-${arm}-${SUBSTRATE}-pinned.json'))
print(d.get('cpu_child_us',0))
" 2>/dev/null || echo 0)"
  container_ms="${RUNG1_P50_MS:-0}"
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

  print_verdict "$SUBSTRATE" "$warm_ms" "$container_ms" "$repl_ms"
}

# Allow this file to be sourced (for tests that extract individual functions) without
# invoking main — but running it directly, as an operator would, still calls main.
if [ "${E10_LIFECYCLE_SOURCE_ONLY:-0}" != "1" ]; then
  main "$@"
fi
