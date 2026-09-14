#!/usr/bin/env bash
# deploy/microvm/e11-density.sh
#
# E11 — density and the replenishment ceiling (spec section 7.3). Unlike E10 (Task
# 20), this drives THROUGH THE RELAY against a real worker binary, sweeping
# concurrent active runs x D x GuestRAMBytes, so it is a concurrency sweep, not a
# ladder of isolated terms.
#
# SCOPE (pre-run hardware correction F1; a build-time note, not committed): this task builds the INSTRUMENT and
# runs it, once, on a shared nested rig to validate the mechanism -- not to produce
# the headline density number. This script is written to be run on that rig by a
# human operator; it is NOT invoked by any automated test in this repo, and nothing
# in deploy/microvm/tests/e11-density.test.sh calls main(). See the scope note in the header
# for the full disclosure of every proxy/limitation below.
#
# Two arms only (hardware-corrections F5): "container" (today's remote-worker, no
# microVM at all -- the baseline E11 is priced against) and "microvm"
# (microvm-worker, Firecracker ONLY). Cloud Hypervisor is not a third arm: on this
# project's rig it dies during device restoration after logging
# "Restoring virtio-console __console", with no error propagated through its API,
# and presents as a 30-second hang -- it does not restore, so there is nothing to
# sweep. Where a CH column would appear in a write-up, that absence is the reason,
# not "it was slower" (F5).
#
# Because the microvm arm is Firecracker-only, and spec section 4.3's Firecracker
# cross-cut is "+ block with mount-at-acquire" (not virtio-fs), a correctly-running
# sweep on this arm has ZERO virtiofsd processes -- virtiofsd is a Cloud Hypervisor
# thing. pss_bytes_for_pids summing 0 bytes for the virtiofsd pattern here is
# therefore the EXPECTED result of an absent process, not evidence of a bug in the
# sampler. See discover_pids / host_signals_snapshot below.
#
# Spec section 7.5 traps, and how each is handled here:
#   - closed-loop driver hides queueing:  NOT eliminated. run_density_rung drives
#     each of the c concurrent "slots" as a tight loop that waits for one Exec to
#     finish before issuing the next (closed-loop, per slot) rather than a genuine
#     open-loop / rate-based arrival process. Spec section 7.5 permits either
#     ("drive open-loop / rate-based, or declare the bias") -- this script takes the
#     declared-bias branch. drivingModel="closed-loop-per-slot" is written into
#     every rung's JSON record for exactly this reason: coordinated omission means
#     this design UNDERSTATES latency at saturation, which is the regime
#     prediction 3 (cold-acquire shape) lives in. A future revision that swaps this
#     for a real rate-based driver would only need to change how requests are
#     scheduled onto the same grpc_exec_record() plumbing.
#   - page-cache asymmetry between arms: drop_caches (dup of e10-lifecycle.sh's own
#     function) runs between the container and microvm arms, and shuffle_e11_arms
#     randomizes which arm goes first, exactly as E10 does for its own arms.
#   - guest-side timing is garbage: every timestamp in this script is taken on the
#     HOST around a grpcurl call (date +%s%N); no guest clock is ever read.
#   - CPU frequency / thermal drift: check_governor (dup of e10-lifecycle.sh's own
#     function) still refuses a non-"performance" governor before any rung runs.
#   - converge hides inside the rungs (section 4.5): converge_slot times ONE
#     Exec running the exact converge script harness/src/converge.ts:
#     buildConvergeScript() builds (reproduced here verbatim, see build_converge_
#     script below) BEFORE a slot's timed Exec-mix loop starts, and its wall time
#     is recorded in a SEPARATE field (convergeMsP50) from the Exec-mix p50/p95, so
#     a slow fetch cannot misread as a throughput ceiling.
#
# Section 4.5's owed decision (hardware-corrections F7: DO NOT decide it here). The
# spec's own three shapes, verbatim:
#   1. "Two mounts." A host-shared /workspace/repo (read-mostly) plus a per-run rw
#      dir for worktrees. Cheapest to reason about; needs the fetch lock to become
#      host-level rather than per-pod.
#   2. "Per-run clone with --shared / alternates" against a host-side object store.
#   3. "Accept the cold fetch" and pre-seed the golden snapshot's workspace image
#      with the repos in play -- viable for the experiment, not for a general
#      deployment.
# SH_E11_REPO_CACHE_SHAPE below RECORDS which one a run claims (spec section 7.5:
# "record which of section 4.5's three shapes the run used") -- it does NOT decide
# among them. Its default, "accept-cold-fetch" (shape 3), is chosen here only
# because it is the one this script can drive with no new mount/clone
# infrastructure on the Firecracker+block arm (there is no host<->guest shared
# filesystem to put a "two mounts" or "--shared" object store on without inventing
# one) -- a narrower, disclosed choice about how THIS INSTRUMENT feeds workload, not
# a recommendation about what production should adopt. An operator who wants to
# validate shape 1 or 2 must point SH_E11_CONVERGE_REPO_URL at infrastructure they
# built themselves; this script does not implement the other two shapes.
#
# The four second-order settings held at their spec section 4.1 defaults and
# RECORDED, NOT SWEPT (spec section 7.3: "adding four dimensions to
# runs x D x GuestRAMBytes would multiply the rung count for a term the memory
# arithmetic already bounds"). These match remote-worker/internal/vmpool/config.go's
# own DefaultStandbyIdle / DefaultWorkspaceIdle / DefaultReplenishDelay constants and
# that file's own anchor comment ("E11 holds StandbyIdle, WorkspaceIdle,
# ReplenishDelay and ReclaimScanInterval at these values and RECORDS them rather
# than sweeping them"). None of the four has an env override in microvm-worker's
# poolConfig() -- there is nothing to sweep even if this script wanted to:
#   - StandbyIdle        = 90s   (config.go DefaultStandbyIdle)
#   - WorkspaceIdle       = 1800s (config.go DefaultWorkspaceIdle, 30m)
#   - ReplenishDelay      = 0.2s  (config.go DefaultReplenishDelay, 200ms)
#   - ReclaimScanInterval = 22.5s (StandbyIdle/4, per spec section 7.3's own framing)
#
# Disclosed proxies and limitations (the full write-up is a build-time note, not committed;
# summarized here at the point each is produced, not hidden in a report nobody
# reads before running this):
#   - leaseSaturations is ALWAYS 0. This driver issues Execs directly against the
#     relay/worker over grpcurl and never goes through harness/src/sandbox-lease.ts
#     or KAGENTI_SANDBOX_CAP, so it structurally cannot exercise or observe the
#     harness-side lease cap spec section 7.3's last metric row asks for. A rung
#     that would have saturated a lease in the real harness path is invisible here.
#   - coldAcquireRate is a LATENCY-CLASSIFICATION PROXY, not the real replenishment
#     signal: microvm-worker exposes no stats/introspection endpoint (confirmed:
#     none exists in cmd/microvm-worker/main.go), and adding one is a Go change out
#     of this task's deliverables. An Exec is counted as "cold" if its host-side
#     latency is >= SH_E11_COLD_LATENCY_MS. execErrorsByCause is real (derived from
#     the Exec RPC's own error/ExecError signal), coldAcquireRate is not.
#   - standbysResident is a PROXY: max(processCount - c, 0), not a real pool-side
#     count (same missing-introspection reason as above).
#   - The model stub P6 section 5.4 specifies (deploy/knative/model-stub/) does not
#     exist in this worktree/branch (confirmed via `ls`: only present on the
#     unmerged feat/p6-experiments branch). SH_E11_MODEL_STUB_CMD lets an operator
#     point at it once it exists; absent that, this script drives the Exec mix
#     itself at a fixed, declared rate rather than the model stub's calibrated
#     tool-call rate -- another reason drivingModel is recorded, not assumed.
#
# Usage (never run by this task -- the instrument is built, not executed):
#   SH_SUBSTRATE=nested-m8i \
#   SH_SNAPSHOT_DIR=/srv/snapshots SH_WORKSPACE_ROOT=/srv/workspaces \
#   SH_MAX_COMMITTED_MB=8192 \
#     bash deploy/microvm/e11-density.sh
set -uo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
# ABSOLUTE, always, because two callers `cd` elsewhere before using it: both
# `go build -o "$RESULTS/..."` calls run inside `(cd "$REMOTE_WORKER_DIR" && ...)`. With a
# relative RESULTS they wrote the worker binaries to
# remote-worker/deploy/microvm/.results/, a directory that does not exist, the build
# failed, its exit status was unchecked, and the first symptom was a converge failure
# naming neither the build nor the path. Found on E11's first-ever execution. The test
# suite could not see it: it overrides RESULTS with an absolute /tmp path.
#
# A relative override is normalised rather than rejected, so `RESULTS=out ./e11-density.sh`
# keeps working and means what it looks like.
RESULTS="${RESULTS:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/deploy/microvm/.results}"
case "$RESULTS" in /*) ;; *) RESULTS="$PWD/$RESULTS" ;; esac

# Required, no default -- same reasoning e10-lifecycle.sh gives for SH_SUBSTRATE:
# an explicit, operator-set label cannot silently mislabel a nested run as metal
# (hardware-corrections F3: this rig is nested-m8i, an EC2 m8i.xlarge, NOT
# nested-c8i -- never pass SH_SUBSTRATE=metal on this box).
SUBSTRATE="${SH_SUBSTRATE:?set SH_SUBSTRATE (e.g. nested-m8i per hardware-corrections F3) - spec section 6 requires the substrate in every run record}"
SNAPSHOT_DIR="${SH_SNAPSHOT_DIR:?set SH_SNAPSHOT_DIR - the same env var name microvm-worker requires, see cmd/microvm-worker/main.go}"
WORKSPACE_ROOT="${SH_WORKSPACE_ROOT:?set SH_WORKSPACE_ROOT - the same env var name microvm-worker requires, see cmd/microvm-worker/main.go}"
# microvm-worker's own required var (poolConfig(): "SH_MAX_COMMITTED_MB is required:
# without the memory gate, ..."); this script requires it too and passes it straight
# through, rather than inventing a separate density-sweep memory budget.
MAX_COMMITTED_MB="${SH_MAX_COMMITTED_MB:?set SH_MAX_COMMITTED_MB - the same required env var microvm-worker refuses to start without}"

GOVERNOR_PATH="${GOVERNOR_PATH:-/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor}"
PROC_ROOT="${SH_E11_PROC_ROOT:-/proc}" # overridable so tests can fake smaps_rollup without a Linux /proc

# Sweep dimensions (spec section 7.3: "Sweep concurrent active runs x D x
# GuestRAMBytes"). Defaults are vmpool's own single-point defaults
# (DefaultStandbyDepth=2, DefaultGuestRAMBytes=256MiB); the active-runs ladder
# MUST include c=1 -- detectKnee (experiments/src/sharing.ts) throws without a
# c===1 baseline point, and analyzeLadder relies on that, so this is validated
# below rather than discovered two hours into a sweep.
read -r -a D_VALUES <<<"${SH_E11_D_VALUES:-2}"
read -r -a RAM_MB_VALUES <<<"${SH_E11_GUEST_RAM_MB_VALUES:-256}"
read -r -a ACTIVE_RUNS <<<"${SH_E11_ACTIVE_RUNS:-1 2 4 8}"

ITERS_PER_SLOT="${SH_E11_ITERS_PER_SLOT:-20}"
WARMUP_PER_SLOT="${SH_E11_WARMUP_PER_SLOT:-3}"

# Section 4.5's owed shape, RECORDED not decided (hardware-corrections F7). See the
# header comment above for the exact three verbatim shape names this must be one of.
REPO_CACHE_SHAPE="${SH_E11_REPO_CACHE_SHAPE:-accept-cold-fetch}"
CONVERGE_REPO_URL="${SH_E11_CONVERGE_REPO_URL:-file:///workspace/seed-repo}"
CONVERGE_REF="${SH_E11_CONVERGE_REF:-HEAD}"

# Optional: point at a real P6 section 5.4 model stub once one exists in this repo.
# Absent, this script drives the Exec mix itself (see header comment's disclosure).
MODEL_STUB_CMD="${SH_E11_MODEL_STUB_CMD:-}"

# Disclosed latency-classification proxy threshold for coldAcquireRate (see header).
COLD_LATENCY_MS="${SH_E11_COLD_LATENCY_MS:-50}"

# VMM / virtiofsd host process patterns for PSS sampling (spec section 7.3: "Sigma
# PSS across VMM + virtiofsd"). Overridable so a test can point these at a fake
# marker process rather than a real firecracker/virtiofsd binary.
VMM_PROC_PATTERN="${SH_E11_VMM_PROC_PATTERN:-firecracker}"
VIRTIOFSD_PROC_PATTERN="${SH_E11_VIRTIOFSD_PROC_PATTERN:-virtiofsd}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
REMOTE_WORKER_DIR="$REPO_ROOT/remote-worker"
EXPERIMENTS_DIR="$REPO_ROOT/experiments"
PROTO_FILE="$REPO_ROOT/proto/sandbox/v1/sandbox.proto"
# grpcurl REFUSES an absolute -proto path unless it is also given at least one
# -import-path ("must specify at least one import path if any absolute file paths are
# given"), and it fails at proto-parsing time — before it dials anything. So every RPC
# this driver makes failed, in both arms, on any host: E11 could not measure a single
# Exec. Found on its first-ever execution; it would have failed identically on metal.
# The pair below is the form verified against the real grpcurl on the rig: import path at
# the proto ROOT, file named relative to it. PROTO_FILE is kept for the existence check,
# which is about the file being there rather than about how grpcurl is invoked.
# Client-side deadlines for grpcurl, a margin above each request's own timeout_s. Without
# them a wedged Exec hangs the WHOLE ladder rather than failing one rung: an Exec that
# collided on req_id (see run_density_rung) left grpcurl waiting 33 MINUTES on the
# validation rig, and the request's own timeout_s never fired because nothing server-side
# was late -- the response simply went to the other caller. Verified against that exact
# wedge on the rig: -max-time returns non-zero at the deadline where the call otherwise
# hung indefinitely, so it covers an established-stream stall and not merely a dial
# failure.
EXEC_MAX_TIME_S="${SH_E11_EXEC_MAX_TIME_S:-45}"      # guards timeout_s:30
CONVERGE_MAX_TIME_S="${SH_E11_CONVERGE_MAX_TIME_S:-360}" # guards timeout_s:300
PROTO_IMPORT_PATH="$REPO_ROOT/proto"
PROTO_REL_PATH="sandbox/v1/sandbox.proto"

# Shared relay/redis stack knobs -- only ONE arm's stack is ever up at a time (each
# arm is fully torn down before the next starts), so both arms reuse the same ports.
E11_REDIS_PORT="${SH_E11_REDIS_PORT:-6381}"
E11_RELAY_PORT="${SH_E11_RELAY_PORT:-8444}"
E11_RELAY_TOKEN="${SH_E11_RELAY_TOKEN:-e11-dev-token}"
E11_START_STACK="${SH_E11_START_STACK:-1}"

# The redis image, overridable so an operator can PIN A DIGEST
# (SH_E11_REDIS_IMAGE=redis@sha256:...) for a reproducible run. `redis:7` is a floating
# tag; it stays the default because it is what the rest of this repo already pulls, and
# because a digest this script has never pulled would be a guess, not a pin.
E11_REDIS_IMAGE="${SH_E11_REDIS_IMAGE:-redis:7}"

die() { echo "e11: $*" >&2; exit 1; }
log() { echo "e11: $*" >&2; }

# ---------------------------------------------------------------------------
# Teardown, armed BEFORE anything is started (review 4001908613).
#
# This driver had no `trap` at all, so every `die` path -- host_signals_snapshot's two
# refusals, the record guard, a failed percentile -- left the whole arm's stack running:
# the worker, the relay, the redis container, and the per-rung temp dirs. Beyond the
# leak, the orphans actively corrupt the next attempt:
#   - the orphaned relay/worker keep 8444/6381 bound, so the operator's rerun fails at
#     relay start with a bind error that says nothing about the real cause;
#   - an orphaned `firecracker` is still matched by discover_pids' UNSCOPED
#     `pgrep -f firecracker`, so it silently inflates the NEXT run's pssBytes -- the one
#     number spec section 7.3 insists must not be wrong, corrupted in a way that looks
#     like a real density result.
#
# Ordering and variable discipline both follow build-snapshot.sh's cleanup_on_exit
# (which documents the bug at length): kill before remove, and nothing this trap touches
# is ever a function local. That is why the pid globals and the temp root are declared
# HERE, above the trap, rather than further down next to the functions that assign them:
# a failure anywhere after this point finds them defined, and `${VAR:-}` defaults keep a
# future global added without one from reintroducing the "unbound variable INSIDE the
# trap, which aborts the rest of the trap" failure.
#
# Every temp path this script creates now lives under $E11_TMPDIR (rather than bare
# `mktemp`/`mktemp -d` calls held in function locals), so one `rm -rf` here reclaims all
# of them however deep the sweep was when it stopped.
# ---------------------------------------------------------------------------
E11_WORKER_PID=""
E11_RELAY_PID=""
E11_WORKER_BIN=""
E11_TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/e11-density.XXXXXX")"

cleanup_on_exit() {
  stop_container_stack || true
  stop_microvm_stack || true
  [ -z "${E11_TMPDIR:-}" ] || rm -rf "$E11_TMPDIR"
}
trap cleanup_on_exit EXIT

# ---------------------------------------------------------------------------
# Preflight -- duplicated from e10-lifecycle.sh's own check_kvm/check_cgroups/
# check_swap/check_governor rather than sourcing that file: sourcing a sibling
# script that ends in its own unconditional main() call is a fragile cross-script
# dependency for four ~5-line functions. Kept byte-for-byte equivalent in behavior
# (not merely in intent) so e11-density.test.sh can extract and test them exactly
# as e10-lifecycle.test.sh does.
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
    die "swap is on: swapping guest RAM destroys the latency this design exists for (spec section 2.4)"
}

check_governor() {
  if [ ! -e "$GOVERNOR_PATH" ]; then
    echo "not exposed"
    return 0
  fi
  local g
  g="$(cat "$GOVERNOR_PATH" 2>/dev/null || echo "")"
  if [ "$g" != "performance" ]; then
    die "governor is '$g', not performance: replenishment is a CPU burst (spec section 7.5). This is fixable - set it and rerun."
  fi
  echo "performance"
}

# validate_repo_cache_shape refuses an SH_E11_REPO_CACHE_SHAPE value that is not one
# of spec section 4.5's three named shapes -- recording an invented fourth shape
# would be worse than recording none.
validate_repo_cache_shape() {
  case "$REPO_CACHE_SHAPE" in
  two-mounts | shared-clone | accept-cold-fetch) : ;;
  *)
    die "SH_E11_REPO_CACHE_SHAPE='$REPO_CACHE_SHAPE' is not one of the spec section 4.5 shapes: two-mounts (shape 1, 'Two mounts.'), shared-clone (shape 2, 'Per-run clone with --shared / alternates'), accept-cold-fetch (shape 3, 'Accept the cold fetch')"
    ;;
  esac
}

# require_tool refuses a MISSING external binary by name, in preflight, rather than
# letting a rung "run" against a command that is not there -- the same guard, and the same
# wording, e10-lifecycle.sh already carries. E11 had NO tool preflight at all, and that is
# why its first-ever execution reported "converge FAILED after 35ms" three times over
# instead of naming grpcurl, the worker build, or pnpm. A driver that cannot say which
# tool is missing costs an operator a debugging session per missing tool.
require_tool() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is not on PATH: $2"
}

preflight() {
  # Every one of these is a HARD requirement for at least one arm, and each was found the
  # expensive way on the first execution. pnpm in particular is NOT optional: the container
  # arm's relay is `pnpm --filter @sh/sandbox-relay start`, so without it that whole arm --
  # the baseline the microvm arm is priced against -- cannot start.
  require_tool grpcurl "both arms drive their Exec RPCs through grpcurl; without it every timing would measure a client-side error rather than a sandbox"
  require_tool docker "both arms start their own scratch redis in a container; without it the relay has nowhere to publish its presence record"
  require_tool go "both arms build their own worker binary from ./cmd/worker and ./cmd/microvm-worker"
  require_tool pnpm "the container arm starts the real sandbox-relay via pnpm --filter @sh/sandbox-relay; without it the baseline arm cannot start at all"
  # The proto must be PRESENT as a file, separately from how grpcurl is told to find it
  # (PROTO_IMPORT_PATH/PROTO_REL_PATH): a missing proto is otherwise indistinguishable from
  # a malformed grpcurl invocation, and both present as an unencodable Exec. e10 carries the
  # same check for the same reason.
  [ -f "$PROTO_FILE" ] ||
    die "the sandbox proto is missing at $PROTO_FILE - grpcurl cannot encode an Exec request without it, so every rung would time a client-side error"
  check_kvm
  check_cgroups
  check_swap
  GOVERNOR_STATE="$(check_governor)"
  log "governor: $GOVERNOR_STATE"
  validate_repo_cache_shape
  mkdir -p "$RESULTS"
}

# static_settings_json prints the four spec section 4.1 settings this task holds
# fixed and records rather than sweeps (see header comment). Pure function, no
# globals read, so it is directly testable in isolation.
static_settings_json() {
  printf '{"standbyIdleS":90,"workspaceIdleS":1800,"replenishDelayS":0.2,"reclaimScanIntervalS":22.5}'
}

# ---------------------------------------------------------------------------
# PSS sampling (spec section 7.3's boxed warning: "Use PSS, not RSS"). Split into
# discover_pids (a real pgrep call, testable against a real spawned process with no
# KVM needed) and pss_bytes_for_pids (a pure reader over $PROC_ROOT, testable
# against a fabricated proc tree so it runs on any platform, Linux smaps_rollup or
# not).
# ---------------------------------------------------------------------------
discover_pids() {
  local pattern="$1"
  pgrep -f -- "$pattern" 2>/dev/null || true
}

# pss_bytes_for_pids sums the "Pss:" line of $PROC_ROOT/<pid>/smaps_rollup across
# every pid given. It NEVER reads VmRSS / RSS from /proc/<pid>/status as a
# fallback: if a pid is still alive (kill -0 succeeds) but its smaps_rollup is
# missing or unreadable, this dies rather than silently substituting a wrong-by-an-
# order-of-magnitude number (spec section 7.3: "reporting ~50 GiB where the truth
# is ~2 GiB ... in the pessimistic direction, so it would cause us to abandon a
# design that works"). A pid that has already exited between discovery and
# sampling contributes 0 (that is a race, not an unreadable file).
pss_bytes_for_pids() {
  local total_kb=0 pid smaps
  for pid in "$@"; do
    [ -n "$pid" ] || continue
    smaps="$PROC_ROOT/$pid/smaps_rollup"
    if [ ! -r "$smaps" ]; then
      if kill -0 "$pid" 2>/dev/null; then
        die "smaps_rollup unreadable for pid $pid ($smaps) - refusing to fall back to RSS (spec section 7.3's boxed warning)"
      fi
      continue # pid exited between discovery and sampling; not an unreadable file
    fi
    local pid_kb
    pid_kb="$(awk '/^Pss:/{sum+=$2} END{print sum+0}' "$smaps")"
    total_kb=$((total_kb + pid_kb))
  done
  echo $((total_kb * 1024))
}

# mem_available_bytes prints MemAvailable in bytes, or a single 0 when /proc/meminfo is
# absent or has no MemAvailable line.
#
# `found=1` in the match action is load-bearing, not tidying (final review H2). In awk,
# `exit` in a main rule RUNS the END block, so without it the guard `if (!found)` was
# always true and this printed the value AND a second line "0" on every Linux host. That
# two-line value was interpolated into host_signals_snapshot's JSON, which made all four
# json.load calls that consume it fail, which left the rung-record writer with empty
# Python expressions and a SyntaxError -- and because this driver runs `set -uo pipefail`
# without `set -e`, nothing aborted: E11 completed its entire sweep having written ZERO
# rung records, on the only platform it can run on. Invisible on darwin, which has no
# /proc/meminfo at all and so takes the `|| echo 0` fallback.
#
# The whole class was audited, not just this line: `grep -rn 'END *{'` over every
# non-test script under deploy/ finds nine other awk END blocks, and the only other
# `exit` reachable from one is inside percentile()'s own END block (both here and in
# e10-lifecycle.sh), where `exit` merely terminates and cannot re-enter END. This was
# the single instance of the shape. require_numeric below is the guard that keeps it
# from being the last one.
mem_available_bytes() {
  awk '/^MemAvailable:/{found=1; print $2*1024; exit} END{if (!found) print 0}' "$PROC_ROOT/meminfo" 2>/dev/null || echo 0
}

# require_numeric echoes value unchanged when it is exactly ONE line holding one bare
# number, and dies naming the field otherwise.
#
# Both halves matter and the first is the one H2 needed: every line of that broken value
# was individually numeric -- there were simply two of them. A field that is not a single
# bare number cannot be interpolated into JSON, so failing here, naming the field, is
# strictly better than assembling a record that four json.load calls will reject 350 lines
# later with a message about a column number.
require_numeric() {
  local field="$1" value="$2"
  if [ "$(printf '%s\n' "$value" | wc -l | tr -d ' ')" != "1" ] ||
    [ "$(printf '%s\n' "$value" | grep -cxE '\-?[0-9]+(\.[0-9]+)?')" != "1" ]; then
    die "host signal $field is not a single bare number (got '$(printf '%s' "$value" | tr '\n' '|')') - refusing to assemble JSON that would silently cost this rung its record (final review H2)"
  fi
  printf '%s' "$value"
}

# dimension_literal prints the PYTHON LITERAL for one swept dimension: the number itself,
# or `None` (JSON null) for the "-" main() passes on the container arm, which has neither a
# standby depth nor a guest RAM size.
#
# Review 4001908573: `run_density_rung container - - "$c"` fed those dashes straight into
# the record writer's bare numeric interpolations (`'standbyDepth': $d,` -> `: -,`), so
# EVERY container rung raised a SyntaxError, wrote no record, and tripped the `[ -s ]`
# guard -- and since shuffle_e11_arms randomises arm order, roughly half of all runs died
# there before the microvm arm ran at all, with the container stack left running. The arm
# the whole experiment is priced against recorded nothing.
#
# `required=1` (the microvm arm) makes "not applicable" itself a refusal: null there would
# mean the sweep lost the dimension it is sweeping.
dimension_literal() {
  local field="$1" value="$2" required="${3:-0}"
  case "$value" in
  '-' | '')
    [ "$required" != "1" ] ||
      die "$field is '$value' (not applicable) on an arm that requires it - a rung cannot be recorded without the dimension it swept"
    printf 'None'
    return 0
    ;;
  esac
  require_numeric "$field" "$value"
}

# host_cpu_fraction samples /proc/stat twice, SAMPLE_WINDOW_S apart, and returns
# the busy fraction over that window -- never a single-sample /proc/stat snapshot,
# which is meaningless (it is a cumulative counter since boot).
host_cpu_fraction() {
  local window="${1:-1}" a b idle_a idle_b total_a total_b
  a="$(awk '/^cpu /{print; exit}' "$PROC_ROOT/stat" 2>/dev/null)"
  sleep "$window"
  b="$(awk '/^cpu /{print; exit}' "$PROC_ROOT/stat" 2>/dev/null)"
  if [ -z "$a" ] || [ -z "$b" ]; then
    echo 0
    return 0
  fi
  idle_a="$(awk '{print $5+$6}' <<<"$a")"
  idle_b="$(awk '{print $5+$6}' <<<"$b")"
  total_a="$(awk '{s=0; for(i=2;i<=NF;i++) s+=$i; print s}' <<<"$a")"
  total_b="$(awk '{s=0; for(i=2;i<=NF;i++) s+=$i; print s}' <<<"$b")"
  awk -v ia="$idle_a" -v ib="$idle_b" -v ta="$total_a" -v tb="$total_b" \
    'BEGIN{ dt=tb-ta; di=ib-ia; if (dt>0) printf "%.4f", 1-(di/dt); else print 0 }'
}

# host_signals_snapshot prints one JSON object: pssBytes (VMM + virtiofsd, PSS
# only), memAvailableBytes, hostCpuFraction, processCount.
#
# Every field is validated before it reaches the printf, and the function RETURNS
# NON-ZERO (printing nothing) rather than emitting a malformed object. This is the
# integration point final-review H2 exposed: pss_bytes_for_pids had its own extracted
# test and a real non-vacuousness proof, while the JSON assembly that consumes it -- the
# only place any of these values is used -- had no test at all, so a malformed SIBLING
# field took the whole rung record down with it.
#
# Each `|| return 1` is also what makes `die` inside these helpers effective at all. A
# `die` in `x="$(helper)"` exits only the command substitution's SUBSHELL; with no
# `set -e` the assignment simply lands empty and the script sails on. That silently
# defanged even pss_bytes_for_pids's "refusing to fall back to RSS" refusal, which spec
# section 7.3's boxed warning makes the single most important failure in this file.
# Checking the status here is what turns those refusals back into stops.
#
# $1 ("require_vmm", default 0) is what makes a ZERO PSS TOTAL a refusal rather than a
# value on the arm where it can only be wrong (review 4001908599). With no match for
# $VMM_PROC_PATTERN, all_pids is empty, pss_bytes_for_pids returns 0, and require_numeric
# happily accepts it -- so the rung records pssBytes: 0. That is reachable from a mis-set
# SH_E11_VMM_PROC_PATTERN, from the jailer renaming the process, or from the arm simply
# not being up, and it is wrong in the OPTIMISTIC direction: the single number E11 exists
# to produce goes missing while the record still looks complete, which is the same failure
# mode as the RSS fallback that pss_bytes_for_pids goes to real lengths to refuse.
#
# It is a parameter, not an unconditional check, because ZERO IS CORRECT on the container
# arm (no VMM at all) and for the virtiofsd pattern on the Firecracker-only microvm arm
# (see the header). Only "the microvm arm found no VMM process" is impossible-by-
# construction, and only main()'s microvm call sites pass 1.
host_signals_snapshot() {
  local require_vmm="${1:-0}"
  local vmm_pids virtiofsd_pids all_pids pss mem cpu count
  vmm_pids="$(discover_pids "$VMM_PROC_PATTERN")"
  if [ "$require_vmm" = "1" ] && [ -z "$vmm_pids" ]; then
    die "no host process matches the VMM pattern '$VMM_PROC_PATTERN' while the microvm arm is running: PSS would total 0 bytes, and a zero is a REFUSAL here, not a measurement (spec section 7.3's boxed warning). Check SH_E11_VMM_PROC_PATTERN against the process the jailer actually spawns, and that the worker is up."
  fi
  virtiofsd_pids="$(discover_pids "$VIRTIOFSD_PROC_PATTERN")"
  # shellcheck disable=SC2086 # word-splitting into pss_bytes_for_pids's "$@" is intended
  all_pids="$vmm_pids $virtiofsd_pids"
  # shellcheck disable=SC2086
  pss="$(pss_bytes_for_pids $all_pids)" || return 1
  pss="$(require_numeric pssBytes "$pss")" || return 1
  if [ "$require_vmm" = "1" ] && [ "$pss" = "0" ]; then
    die "the VMM pattern '$VMM_PROC_PATTERN' matched pids [$(printf '%s' "$vmm_pids" | tr '\n' ' ')] but their PSS summed to 0 bytes - refusing a zero total on the microvm arm (spec section 7.3): a real Firecracker guest is never 0, so \$SH_E11_PROC_ROOT or the smaps_rollup content is wrong, not the memory."
  fi
  mem="$(require_numeric memAvailableBytes "$(mem_available_bytes)")" || return 1
  cpu="$(require_numeric hostCpuFraction "$(host_cpu_fraction 1)")" || return 1
  count="$(require_numeric processCount "$(printf '%s\n%s\n' "$vmm_pids" "$virtiofsd_pids" | grep -c '[0-9]' || true)")" || return 1
  printf '{"pssBytes":%s,"memAvailableBytes":%s,"hostCpuFraction":%s,"processCount":%s}' \
    "$pss" "$mem" "$cpu" "$count"
}

# ---------------------------------------------------------------------------
# json_escape / percentile -- duplicated from e10-lifecycle.sh verbatim (see that
# file's own copies); small enough that duplication beats sourcing a sibling
# script for these two alone.
# ---------------------------------------------------------------------------
json_escape() {
  python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$1"
}

percentile() {
  local p="$1" file="$2"
  # A missing or EMPTY input file is not a zero percentile, it is the absence of any
  # measurement -- so this refuses (prints nothing, returns non-zero) and the caller names
  # the rung. Two defects lived in the old `print 0` form (review 4001908597):
  #
  #   1. the value. `p95Ms: 0` at a rung where every Exec failed is not a fast rung, and
  #      detectKnee reads it as the healthiest point in the ladder; at c=1 it makes the
  #      baseline bound (p95 * degradeX) zero and marks every later rung unhealthy.
  #   2. the SHAPE. `sort -n` on a missing file exits 2, awk still printed 0, and
  #      `pipefail` propagated sort's status -- so a caller's `|| echo 0` appended a
  #      SECOND line, which is exactly the two-line-value defect final review H2 fixed in
  #      mem_available_bytes. The `[ -s ]` guard means `sort` is never handed a missing
  #      file at all, and the awk END branch exits non-zero instead of printing.
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

# e11_tool_call_mix is IDENTICAL to e10-lifecycle.sh's rung1_tool_call_mix
# (duplicated rather than sourced, for the same reason as the preflight checks
# above): spec section 7.3 requires E11 to drive "the Exec mix E10 measured", so
# reusing a different mix here would violate the spec's own cross-reference.
e11_tool_call_mix() {
  echo "true"
  echo "head -c 1048576 /dev/zero | wc -c"
  echo "ls -la /tmp"
  echo "cat /etc/hostname"
  echo "echo e11-mix > /tmp/e11-mix-$$.tmp"
  echo "grep -c e11 /tmp/e11-mix-$$.tmp"
  echo "rm -f /tmp/e11-mix-$$.tmp"
}

# ---------------------------------------------------------------------------
# The Exec RPC itself, extended from e10-lifecycle.sh's grpc_exec_ms with a
# workspace_key (proto/sandbox/v1/sandbox.proto: Exec.workspace_key, field 6,
# nested inside Exec, NOT a top-level ExecRequest field) and error classification
# for execErrorsByCause.
# ---------------------------------------------------------------------------
grpc_exec_record() {
  local relay_port="$1" sandbox_id="$2" workspace_key="$3" cmd="$4" req_id="$5" out_file="$6"
  local t0 t1 ms err_log cause status
  err_log="$(mktemp "$E11_TMPDIR/errlog.XXXXXX")"
  t0="$(date +%s%N)"
  if grpcurl -plaintext -max-time "$EXEC_MAX_TIME_S" -import-path "$PROTO_IMPORT_PATH" -proto "$PROTO_REL_PATH" \
    -d "{\"sandbox_id\":\"$sandbox_id\",\"exec\":{\"req_id\":$req_id,\"command\":$(json_escape "$cmd"),\"timeout_s\":30,\"workspace_key\":$(json_escape "$workspace_key")}}" \
    "localhost:${relay_port}" sandbox.v1.SandboxExec/Exec >/dev/null 2>"$err_log"; then
    status="ok"
    cause="-"
  else
    status="err"
    if grep -qi "workspace_key" "$err_log"; then
      cause="empty-workspace-key"
    elif grep -qi "mem" "$err_log"; then
      cause="memory-gate"
    elif grep -qi "maxruns\|max-runs\|max_runs" "$err_log"; then
      cause="max-runs"
    elif grep -qi "spawn" "$err_log"; then
      cause="spawn-failure"
    elif grep -qi "vsock" "$err_log"; then
      cause="vsock-short-response"
    else
      cause="unknown"
    fi
  fi
  t1="$(date +%s%N)"
  ms=$(((t1 - t0) / 1000000))
  echo "$ms $status $cause" >>"$out_file"
  rm -f "$err_log"
}

# build_converge_script reproduces harness/src/converge.ts:buildConvergeScript()
# verbatim (see that file), so this driver's converge step is the SAME script the
# harness actually runs in production, not an invented substitute.
build_converge_script() {
  local repo_url="$1" ref="$2" run_id="$3" leaf
  leaf="/workspace/leaves/${run_id}"
  cat <<SCRIPT
set -eu
REPO=/workspace/repo; LOCK=/workspace/.sh-fetch.lock; LEAF='${leaf}'
mkdir -p /workspace/leaves
(
  flock 9
  [ -d "\$REPO/.git" ] || { rm -rf "\$REPO"; git init -q "\$REPO"; }
  git -C "\$REPO" fetch --quiet '${repo_url}' '${ref}' || { rm -rf "\$REPO"; git init -q "\$REPO"; git -C "\$REPO" fetch --quiet '${repo_url}' '${ref}'; }
) 9>"\$LOCK"
COMMIT=\$(git -C "\$REPO" rev-parse FETCH_HEAD)
[ -d "\$LEAF" ] || git -C "\$REPO" worktree add --quiet --detach "\$LEAF" "\$COMMIT"
printf '%s' "\$LEAF"
SCRIPT
}

# converge_slot times ONE Exec running build_converge_script's output, SEPARATELY
# from the slot's Exec-mix loop (spec section 7.5: "Time converge separately from
# Exec ... or the cost hides inside the rungs"). Prints elapsed ms.
# It also RETURNS THE RPC's OWN STATUS. A FAILED converge is not a fast converge: the
# workspace was never prepared, so every Exec in that slot afterwards measures something
# else, and timing the failure would put a small number in convergeMsP50 -- wrong in the
# "looks cheap" direction. The slot below turns a non-zero status here into a slot failure,
# and run_density_rung refuses the rung.
# req_id is a PARAMETER, not the constant 0 it used to be. Every slot's converge used
# req_id 0 against one shared sandbox_id, so at c>=2 two concurrent converges collided --
# see run_density_rung's own comment for what that collision does.
converge_slot() {
  local relay_port="$1" sandbox_id="$2" workspace_key="$3" run_id="$4" req_id="$5"
  local script t0 t1 rc=0
  script="$(build_converge_script "$CONVERGE_REPO_URL" "$CONVERGE_REF" "$run_id")"
  t0="$(date +%s%N)"
  grpcurl -plaintext -max-time "$CONVERGE_MAX_TIME_S" -import-path "$PROTO_IMPORT_PATH" -proto "$PROTO_REL_PATH" \
    -d "{\"sandbox_id\":\"$sandbox_id\",\"exec\":{\"req_id\":$req_id,\"command\":$(json_escape "$script"),\"timeout_s\":300,\"workspace_key\":$(json_escape "$workspace_key")}}" \
    "localhost:${relay_port}" sandbox.v1.SandboxExec/Exec >/dev/null 2>>"$RESULTS/e11-converge.log" || rc=$?
  t1="$(date +%s%N)"
  echo $(((t1 - t0) / 1000000))
  return "$rc"
}

# ---------------------------------------------------------------------------
# Arm stacks
# ---------------------------------------------------------------------------
drop_caches() {
  if [ -w /proc/sys/vm/drop_caches ]; then
    echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || log "drop_caches: not permitted, continuing (informational only)"
  else
    log "drop_caches: /proc/sys/vm/drop_caches not writable here, continuing"
  fi
}

# shuffle_e11_arms prints "container" and "microvm" in randomized order (spec
# section 7.5: page-cache asymmetry between arms), same technique as
# e10-lifecycle.sh's shuffle_arms.
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

shuffle_e11_arms() {
  printf 'container\nmicrovm\n' | awk -v seed="$(($$ + $(date +%s)))" 'BEGIN{srand(seed)} {print rand()"\t"$0}' | sort -n | cut -f2-
}

# start_redis_loopback publishes this driver's scratch redis on LOOPBACK ONLY, and is
# the single place either arm starts one (both arms had the same `docker run` line).
#
# It was `-p "${PORT}:6379"`, which binds 0.0.0.0. On the documented rig -- an EC2
# m8i.xlarge with a public interface, running microvm-worker as root -- that publishes
# an UNAUTHENTICATED redis to the internet, and an open redis is a standard
# host-takeover path: CONFIG SET dir + dbfilename, then write an authorized_keys or a
# cron file. Nothing outside this host needs to reach a benchmark's scratch redis: the
# relay and the worker both connect over 127.0.0.1 (see REDIS_URL below). `--save ''`
# additionally disables RDB snapshots, so the container writes no dump file at all.
start_redis_loopback() {
  local arm="$1"
  log "$arm: starting redis on 127.0.0.1:$E11_REDIS_PORT (loopback only)"
  docker run --rm -d -p "127.0.0.1:${E11_REDIS_PORT}:6379" --name "sh-e11-redis-$$" \
    "$E11_REDIS_IMAGE" --save '' >/dev/null ||
    die "could not start the scratch redis on 127.0.0.1:$E11_REDIS_PORT for the $arm arm - the relay has nowhere to publish its presence record, so every Exec in this arm would fail for a reason that has nothing to do with density"
}

start_container_stack() {
  [ "$E11_START_STACK" = "1" ] || {
    log "container stack: SH_E11_START_STACK=0, reusing an already-running stack"
    return 0
  }
  # LOOPBACK ONLY -- see start_redis_loopback's own comment for why 0.0.0.0 is a
  # host-takeover path on the documented rig.
  start_redis_loopback container

  log "container: starting the relay on :$E11_RELAY_PORT"
  (
    cd "$REPO_ROOT" &&
      SH_RELAY_TOKEN="$E11_RELAY_TOKEN" SH_RELAY_PORT="$E11_RELAY_PORT" \
        REDIS_URL="redis://127.0.0.1:${E11_REDIS_PORT}" \
        pnpm --filter @sh/sandbox-relay start >"$RESULTS/e11-container-relay.log" 2>&1 &
    echo $! >"$RESULTS/.e11-relay.pid"
  )
  E11_RELAY_PID="$(cat "$RESULTS/.e11-relay.pid" 2>/dev/null || echo "")"
  wait_for_relay_port "$E11_RELAY_PORT" "$RESULTS/e11-container-relay.log" "the container arm's sandbox-relay"

  E11_WORKER_BIN="$RESULTS/.e11-container-worker-bin"
  log "container: building the worker binary"
  (cd "$REMOTE_WORKER_DIR" && go build -o "$E11_WORKER_BIN" ./cmd/worker) ||
    die "go build ./cmd/worker failed - the container arm has nothing to drive, so every Exec would time a missing binary rather than a container baseline"

  log "container: starting the worker"
  SANDBOX_ID="e11-container" RELAY_ADDR="localhost:${E11_RELAY_PORT}" \
    SANDBOX_TOKEN="$E11_RELAY_TOKEN" \
    "$E11_WORKER_BIN" >"$RESULTS/e11-container-worker.log" 2>&1 &
  E11_WORKER_PID="$!"
  sleep 2
}

stop_container_stack() {
  [ "$E11_START_STACK" = "1" ] || return 0
  # ${VAR:-} because these run from the EXIT trap too, which can fire before either is
  # assigned (build-snapshot.sh's cleanup_on_exit documents that exact failure).
  [ -n "${E11_WORKER_PID:-}" ] && kill "${E11_WORKER_PID:-}" 2>/dev/null
  [ -n "${E11_RELAY_PID:-}" ] && kill "${E11_RELAY_PID:-}" 2>/dev/null
  docker rm -f "sh-e11-redis-$$" >/dev/null 2>&1 || true
  E11_WORKER_PID=""
  E11_RELAY_PID=""
  return 0
}

# start_microvm_stack starts one fresh microvm-worker process per (D, GuestRAMBytes)
# slice -- both are startup-fixed config (vmpool.Config), so a new slice needs a new
# process, not a running one reconfigured. SH_MAX_RUNS is sized to the largest
# active-runs rung so the sweep's own ladder never trips the MaxRuns backstop and
# gets misread as a VM-tier ceiling (spec section 7.3's lease-saturation metric row
# makes the analogous point one tier up).
start_microvm_stack() {
  local d="$1" ram_mb="$2" max_c="$3"
  [ "$E11_START_STACK" = "1" ] || {
    log "microvm stack: SH_E11_START_STACK=0, reusing an already-running stack"
    return 0
  }
  start_redis_loopback microvm

  log "microvm: starting the relay on :$E11_RELAY_PORT"
  (
    cd "$REPO_ROOT" &&
      SH_RELAY_TOKEN="$E11_RELAY_TOKEN" SH_RELAY_PORT="$E11_RELAY_PORT" \
        REDIS_URL="redis://127.0.0.1:${E11_REDIS_PORT}" \
        pnpm --filter @sh/sandbox-relay start >"$RESULTS/e11-microvm-relay-d${d}-ram${ram_mb}.log" 2>&1 &
    echo $! >"$RESULTS/.e11-relay.pid"
  )
  E11_RELAY_PID="$(cat "$RESULTS/.e11-relay.pid" 2>/dev/null || echo "")"
  wait_for_relay_port "$E11_RELAY_PORT" "$RESULTS/e11-microvm-relay-d${d}-ram${ram_mb}.log" "the microvm arm's sandbox-relay"

  E11_WORKER_BIN="$RESULTS/.e11-microvm-worker-bin"
  log "microvm: building the worker binary"
  (cd "$REMOTE_WORKER_DIR" && go build -o "$E11_WORKER_BIN" ./cmd/microvm-worker) ||
    die "go build ./cmd/microvm-worker failed - the microvm arm has nothing to drive, so every Exec would time a missing binary rather than a density ceiling"

  log "microvm: starting the worker (D=$d guest=${ram_mb}MiB)"
  SH_VMM=firecracker SH_STANDBY_DEPTH="$d" SH_GUEST_RAM_MB="$ram_mb" \
    SH_MAX_RUNS=$((max_c + d + 2)) SH_MAX_COMMITTED_MB="$MAX_COMMITTED_MB" \
    SH_SNAPSHOT_DIR="$SNAPSHOT_DIR" SH_SNAPSHOT_IMAGE="${SH_SNAPSHOT_IMAGE:-default}" \
    SH_WORKSPACE_ROOT="$WORKSPACE_ROOT" \
    SANDBOX_ID="e11-microvm-d${d}-ram${ram_mb}" RELAY_ADDR="localhost:${E11_RELAY_PORT}" \
    SANDBOX_TOKEN="$E11_RELAY_TOKEN" \
    "$E11_WORKER_BIN" >"$RESULTS/e11-microvm-worker-d${d}-ram${ram_mb}.log" 2>&1 &
  E11_WORKER_PID="$!"
  sleep 2
}

stop_microvm_stack() {
  [ "$E11_START_STACK" = "1" ] || return 0
  # ${VAR:-} because these run from the EXIT trap too, which can fire before either is
  # assigned (build-snapshot.sh's cleanup_on_exit documents that exact failure).
  [ -n "${E11_WORKER_PID:-}" ] && kill "${E11_WORKER_PID:-}" 2>/dev/null
  [ -n "${E11_RELAY_PID:-}" ] && kill "${E11_RELAY_PID:-}" 2>/dev/null
  docker rm -f "sh-e11-redis-$$" >/dev/null 2>&1 || true
  E11_WORKER_PID=""
  E11_RELAY_PID=""
  return 0
}

# ---------------------------------------------------------------------------
# run_density_rung: THE per-rung driver. Called identically for the container arm
# and the microvm arm (only sandbox_id, relay_port, and whether workspace_key is
# empty differ at the CALL SITE, in main() below) -- this is what makes "both arms
# driven by the same code path" true structurally rather than by claim.
#
# Writes one RungSample-shaped JSON object (matching
# experiments/src/microvm-density.ts's RungSample interface field-for-field) to
# out_json_path.
# ---------------------------------------------------------------------------
run_density_rung() {
  local arm="$1" d="$2" ram_mb="$3" c="$4" sandbox_id="$5" relay_port="$6" out_json_path="$7"
  log "rung: arm=$arm D=$d guest=${ram_mb}MiB c=$c"

  # Every temp path is under $E11_TMPDIR, named for the rung rather than mktemp-random, so
  # the EXIT trap reclaims all of them however this rung ends (review 4001908613).
  local slot_dir converge_file rung_tag
  rung_tag="${arm}-d${d}-ram${ram_mb}-c${c}"
  slot_dir="$E11_TMPDIR/slots-$rung_tag"
  mkdir -p "$slot_dir"
  converge_file="$E11_TMPDIR/converge-$rung_tag"
  : >"$converge_file"

  local wall_t0 wall_t1 pids=()
  wall_t0="$(date +%s%N)"
  local i
  for i in $(seq 1 "$c"); do
    (
      local wskey="" run_id="e11-${arm}-d${d}-ram${ram_mb}-c${c}-slot${i}"
      if [ "$arm" = "microvm" ]; then
        wskey="$run_id" # microvm arm REFUSES an empty workspace_key (proto doc comment)
      fi               # container arm may omit/empty it (today's single shared workspace)

      # A DISJOINT req_id space per slot. Every slot in this rung talks to ONE shared
      # sandbox_id, and the relay demultiplexes responses BY req_id (spec 3.1: req_id is
      # "only probabilistically unique across replicas", so uniqueness is the caller's
      # job). Two concurrent Execs sharing a req_id therefore collide: on the validation
      # rig one of the pair got the other's chunks -- with no reqId field on them -- and
      # the loser's stream was never terminated, hanging for 33 minutes until killed. That
      # is why the ladder could only ever complete its c=1 rung.
      #
      # Isolated with a three-arm probe before this fix was written: one Exec alone
      # succeeded (30ms); two concurrent with the SAME req_id wedged one of them; two
      # concurrent with DIFFERENT req_ids both succeeded (27ms, 28ms). So the collision is
      # the cause, and disjoint spaces are the fix.
      #
      # Base 1000000 per slot, converge at the base and the Exec mix above it: disjoint for
      # any ITERS_PER_SLOT below a million, which it always is.
      local req_base=$((i * 1000000))
      local cms cms_rc=0
      cms="$(converge_slot "$relay_port" "$sandbox_id" "$wskey" "$run_id" "$req_base")" || cms_rc=$?
      echo "$cms" >>"$converge_file"
      if [ "$cms_rc" -ne 0 ]; then
        echo "e11: slot $i: converge FAILED after ${cms}ms (see $RESULTS/e11-converge.log) - its workspace was never prepared, so its Exec timings would measure something else" >&2
        exit 1
      fi

      local times_file="$slot_dir/slot-$i.times" req="$req_base"
      # Create it empty first. The loop guard below reads it with `wc -l <"$times_file"`,
      # and `2>/dev/null` there binds to wc -- NOT to the shell's own redirection, so a
      # missing file printed "No such file or directory" to stderr on every slot's first
      # iteration. The fallback made it harmless, but an operator reading the log saw what
      # looked like a failure in the middle of a working rung.
      : >"$times_file"
      local want=$((ITERS_PER_SLOT + WARMUP_PER_SLOT))
      while [ "$(wc -l <"$times_file" 2>/dev/null || echo 0)" -lt "$want" ]; do
        while IFS= read -r cmd; do
          req=$((req + 1))
          grpc_exec_record "$relay_port" "$sandbox_id" "$wskey" "$cmd" "$req" "$times_file"
          [ "$(wc -l <"$times_file" 2>/dev/null || echo 0)" -ge "$want" ] && break
        done < <(e11_tool_call_mix)
      done
    ) &
    pids+=("$!")
  done
  # Each slot's exit status is CHECKED, not discarded: a slot exits non-zero only when its
  # converge failed, which means its Exec timings measured a workspace that was never
  # prepared. Recording that rung would put a fast-looking p95 and a small convergeMsP50
  # into the ladder.
  local pid slot_failures=0
  for pid in "${pids[@]}"; do
    wait "$pid" || slot_failures=$((slot_failures + 1))
  done
  [ "$slot_failures" -eq 0 ] ||
    die "rung arm=$arm d=$d ram=${ram_mb}MiB c=$c had $slot_failures of $c slot(s) fail before their timed loop (the reason is above, and in $RESULTS/e11-converge.log) - refusing to record a rung whose slots were not all measuring the same thing"
  wall_t1="$(date +%s%N)"
  local wall_s
  wall_s="$(require_numeric wallSeconds "$(awk -v ns=$((wall_t1 - wall_t0)) 'BEGIN{printf "%.4f", ns/1000000000.0}')")" ||
    die "rung arm=$arm c=$c could not measure its own wall time (see the refusal above) - throughput is derived from it, so there is nothing to record"

  # Aggregate every slot's steady-state (post-warmup) samples together.
  local all_times all_ok=0 all_total=0
  all_times="$E11_TMPDIR/all-times-$rung_tag"
  : >"$all_times"
  rm -f "${all_times}.ok"
  # `=()` is load-bearing, not style. Under `set -u`, `declare -A x` alone leaves x
  # DECLARED BUT UNSET, and `${#x[@]}` on it is an unbound-variable error -- verified on
  # this rig's bash 5.2.15. The only thing that ever assigned an element was the failure
  # branch below, so this rung's recorder crashed if and only if EVERY Exec succeeded:
  # the clean path was the broken one, and any run with a failure sailed past it. Found on
  # E11's first execution that got far enough to have a clean rung.
  declare -A cause_counts=()
  local f
  for f in "$slot_dir"/slot-*.times; do
    [ -e "$f" ] || continue
    tail -n "+$((WARMUP_PER_SLOT + 1))" "$f" | head -n "$ITERS_PER_SLOT" >>"$all_times"
  done
  while read -r ms status cause; do
    [ -n "$ms" ] || continue
    all_total=$((all_total + 1))
    if [ "$status" = "ok" ]; then
      all_ok=$((all_ok + 1))
      echo "$ms" >>"${all_times}.ok"
    else
      cause_counts["$cause"]=$(( ${cause_counts["$cause"]:-0} + 1 ))
    fi
  done <"$all_times"

  # execErrorsByCause is assembled HERE, ahead of the derived fields, rather than just
  # above the record writer where it used to be: the refusals below name these causes,
  # because "every Exec at this rung failed" is only actionable with the reason.
  local errors_json="{}"
  if [ "${#cause_counts[@]}" -gt 0 ]; then
    local parts=()
    local cause
    for cause in "${!cause_counts[@]}"; do
      parts+=("$(json_escape "$cause"):${cause_counts[$cause]}")
    done
    errors_json="{$(
      IFS=,
      echo "${parts[*]}"
    )}"
  fi

  # No `|| echo 0` on either percentile call (review 4001908597), and all five derived
  # fields go through require_numeric -- the guard that until now covered only the four
  # host signals, while p95, throughput, cold_rate, converge_p50 and wall_s reached the
  # record writer unvalidated.
  local p95 throughput cold_count=0
  p95="$(percentile 95 "${all_times}.ok")" ||
    die "rung arm=$arm d=$d ram=${ram_mb}MiB c=$c completed $all_ok successful Execs out of $all_total attempts, so it has no latency distribution to take a p95 of. Refusing to record p95Ms=0: that is not a fast rung, it is an absent measurement, and detectKnee would read it as the healthiest point in the ladder. Error causes: $errors_json - see the worker/relay logs in $RESULTS."
  p95="$(require_numeric p95Ms "$p95")" ||
    die "rung arm=$arm c=$c: p95 failed validation (see the refusal above)"
  throughput="$(require_numeric throughput "$(awk -v n="$all_ok" -v s="$wall_s" 'BEGIN{ if (s>0) printf "%.4f", n/s; else print 0 }')")" ||
    die "rung arm=$arm c=$c: throughput failed validation (see the refusal above)"
  # `.ok` is guaranteed non-empty here: the p95 refusal above is exactly the case where it
  # is not, so this needs no existence guard of its own.
  cold_count="$(awk -v t="$COLD_LATENCY_MS" '$1>=t{c++} END{print c+0}' "${all_times}.ok")"
  local cold_rate
  cold_rate="$(require_numeric coldAcquireRate "$(awk -v c="$cold_count" -v n="$all_total" 'BEGIN{ if (n>0) printf "%.4f", c/n; else print 0 }')")" ||
    die "rung arm=$arm c=$c: coldAcquireRate failed validation (see the refusal above)"

  local converge_p50
  converge_p50="$(percentile 50 "$converge_file")" ||
    die "rung arm=$arm d=$d ram=${ram_mb}MiB c=$c recorded no converge timings at all ($converge_file is empty), so section 4.5's converge cost -- which spec section 7.5 requires be timed SEPARATELY from the Exec mix -- has no value for this rung. Refusing to record 0, which would read as a free fetch."
  converge_p50="$(require_numeric convergeMsP50 "$converge_p50")" ||
    die "rung arm=$arm c=$c: convergeMsP50 failed validation (see the refusal above)"

  local signals pss_bytes mem_bytes cpu_frac proc_count
  # `|| die`, in run_density_rung's OWN shell (main calls it directly, not in a
  # subshell), so a bad snapshot stops the sweep here instead of producing a rung with no
  # record. Section 7.3's whole point is that a wrong density number is worse than none;
  # a sweep that completes having recorded nothing is worse still, because it looks
  # exactly like success (final review H2).
  #
  # require_vmm=1 only for the microvm arm's IN-RUNG snapshot, taken immediately after the
  # slots finish: with D >= 1 at least one standby VMM is necessarily still resident there
  # (StandbyIdle is 90s), so zero matching processes means the sampler is looking in the
  # wrong place, not that memory is free. The two exceptions are deliberate: the container
  # arm has no VMM at all, and a D=0 sweep legitimately keeps no standby resident.
  local require_vmm=0
  if [ "$arm" = "microvm" ] && [ "$d" != "0" ]; then
    require_vmm=1
  fi
  signals="$(host_signals_snapshot "$require_vmm")" ||
    die "host signal snapshot failed for rung arm=$arm c=$c (see the refusal above) - refusing to write a rung record from signals that could not be sampled"
  pss_bytes="$(python3 -c "import json,sys; print(json.load(sys.stdin)['pssBytes'])" <<<"$signals")"
  mem_bytes="$(python3 -c "import json,sys; print(json.load(sys.stdin)['memAvailableBytes'])" <<<"$signals")"
  cpu_frac="$(python3 -c "import json,sys; print(json.load(sys.stdin)['hostCpuFraction'])" <<<"$signals")"
  proc_count="$(python3 -c "import json,sys; print(json.load(sys.stdin)['processCount'])" <<<"$signals")"

  # standbysResident: disclosed proxy (see header). idleStandbyResidency + reclaim
  # convergence: poll the same process-count proxy after every slot has finished,
  # up to StandbyIdle + 2*ReclaimScanInterval (spec section 7.4 prediction 5),
  # sampling at ReclaimScanInterval. Skipped for the container arm, which has no
  # standby concept at all -- recorded as 0 rather than waited-for.
  local standbys_resident=0 idle_residency=0 reclaim_converge_s=0
  standbys_resident=$((proc_count > c ? proc_count - c : 0))
  if [ "$arm" = "microvm" ]; then
    local waited=0 budget=135 interval=23 last_count="$proc_count" idle_snapshot
    while [ "$waited" -lt "$budget" ]; do
      sleep "$interval"
      waited=$((waited + interval))
      # Captured to its own variable first: nesting host_signals_snapshot inside the
      # python command substitution would discard its exit status along with any
      # refusal it made, which is the same subshell-swallows-die shape as above.
      # NOT require_vmm=1: this poll exists to watch standbys BE RECLAIMED (spec section
      # 7.4 prediction 5), so reaching zero VMM processes here is the predicted outcome,
      # not a sampling failure.
      idle_snapshot="$(host_signals_snapshot 0)" ||
        die "host signal snapshot failed while polling idle standby residency for rung arm=$arm c=$c (see the refusal above)"
      last_count="$(python3 -c "import json,sys; print(json.load(sys.stdin)['processCount'])" <<<"$idle_snapshot")"
      # BREAK when the standbys are actually gone. Without this the loop always ran its full
      # budget, so reclaim_converge_s below was the CONSTANT 138 for every microvm rung no
      # matter when reclamation finished -- written into the record as reclaimConvergenceS as
      # though it were observed. Spec 7.4 prediction 5 is precisely a claim about how long
      # that takes, so a constant made it unmeasurable rather than merely imprecise. It also
      # burned the whole budget per rung (~9 minutes across the default metal ladder) to
      # learn nothing.
      #
      # processCount is the disclosed proxy for standbys (see the header): at this point every
      # slot has finished, so a count at or below c means nothing is parked beyond the
      # in-flight set, which is the convergence this is timing.
      if [ "$last_count" -le "$c" ]; then
        break
      fi
    done
    idle_residency="$last_count"
    reclaim_converge_s="$waited"
  fi

  # The two swept dimensions become PYTHON LITERALS in the writer below, so they go through
  # dimension_literal rather than straight into the interpolation -- see that function for
  # what a bare "-" did to every container rung (review 4001908573). Required on the
  # microvm arm, which cannot record a rung without the D and guest RAM it swept.
  local d_json ram_json required_dims=0
  [ "$arm" = "microvm" ] && required_dims=1
  d_json="$(dimension_literal standbyDepth "$d" "$required_dims")" ||
    die "rung arm=$arm c=$c cannot record standbyDepth (see the refusal above)"
  ram_json="$(dimension_literal guestRamMb "$ram_mb" "$required_dims")" ||
    die "rung arm=$arm c=$c cannot record guestRamMb (see the refusal above)"

  python3 -c "
import json
rec = {
  'c': $c,
  'throughput': $throughput,
  'p95Ms': $p95,
  'coldAcquireRate': $cold_rate,
  'coldLatencyThresholdMs': $COLD_LATENCY_MS,
  'pssBytes': $pss_bytes,
  'memAvailableBytes': $mem_bytes,
  'hostCpuFraction': $cpu_frac,
  'processCount': $proc_count,
  'standbysResident': $standbys_resident,
  'idleStandbyResidency': $idle_residency,
  'leaseSaturations': 0,
  'execErrorsByCause': json.loads('''$errors_json'''),
  # Recorded, not part of the RungSample contract consumed by analyzeLadder, but
  # written alongside it so no context is lost between the raw JSON and the report.
  'arm': '$arm',
  'standbyDepth': $d_json,
  'guestRamMb': $ram_json,
  'substrate': '$SUBSTRATE',
  'repoCacheShape': '$REPO_CACHE_SHAPE',
  'convergeMsP50': $converge_p50,
  'reclaimConvergenceS': $reclaim_converge_s,
  'drivingModel': 'closed-loop-per-slot',
  'staticSettings': json.loads('$(static_settings_json)'),
  'proxyLimitations': {
    'leaseSaturations': 'always 0 - driver bypasses the harness lease layer entirely',
    'coldAcquireRate': 'latency-classification proxy (>= ${COLD_LATENCY_MS}ms), not the real replenishment signal - no stats endpoint exists',
    'standbysResident': 'proxy: max(processCount - c, 0) - no pool introspection endpoint exists',
  },
}
open('$out_json_path', 'w').write(json.dumps(rec, indent=2))
"
  # A rung that wrote no record FAILS LOUDLY (final review H2). This driver runs
  # `set -uo pipefail` without `set -e`, so the writer above dying -- of a SyntaxError
  # from an empty interpolation, a KeyError, anything -- did not abort or even warn: the
  # sweep continued to the next rung and did it again, and E11 could complete an entire
  # sweep having recorded nothing while exiting 0.
  #
  # `set -e` was considered and deliberately NOT adopted for this script: it has never
  # been run end to end, and it contains many intentional non-zero statuses (`grep -c`
  # with no match, `|| true`, `|| log`), so turning every one of them into an abort
  # mid-sweep would trade a silent no-data outcome for a loud partial-data one with no
  # test able to tell which statuses were load-bearing. This assertion is the targeted
  # form: it fires exactly when the thing that matters -- the record -- is missing, and
  # names the rung so the operator does not have to diff a directory listing to find out
  # which one.
  [ -s "$out_json_path" ] ||
    die "rung arm=$arm d=$d ram=${ram_mb}MiB c=$c wrote no record to $out_json_path - the record writer failed (its Python traceback is above). A sweep that completes having recorded nothing is the worst outcome for a benchmark, because it looks like success."
  rm -rf "$slot_dir" "$all_times" "${all_times}.ok" "$converge_file"
}

# assemble_ladder collects every per-rung JSON file for one (arm, D, guest RAM)
# slice into a single JSON array, ascending by c, ready for analyzeLadder.
assemble_ladder() {
  local pattern="$1" out_path="$2"
  python3 -c "
import glob, json
files = sorted(glob.glob('$pattern'))
recs = [json.load(open(f)) for f in files]
recs.sort(key=lambda r: r['c'])
open('$out_path', 'w').write(json.dumps(recs, indent=2))
"
}

# analyze_slice invokes experiments/src/microvm-density.ts's analyzeLadder against
# one assembled ladder file and prints the report. Informational only -- this
# script never writes numbers into deploy/microvm/EXPERIMENTS.md itself (task-21
# scope: step 6 is structure-only, hardware-corrections F1).
# The import below says '.ts', NOT '.js'. Inside a compiled TypeScript file '.js' is the
# correct NodeNext specifier and tsx maps it to the .ts source -- but this is a "tsx -e" EVAL
# string, whose module lives at a synthetic <dir>/[eval] path, and that mapping does not
# apply there: resolution falls through to the CJS resolver and dies with "Cannot find
# module ./src/microvm-density.js". Reproduced on the rig (node 22) and on a dev machine
# (node 25), so it is not environment-specific.
#
# It went unnoticed because analyze_slice is deliberately called with "|| log": every ladder
# ran, the failure was one logged line, and no knee was ever computed -- and the knee is what
# sealed prediction 3 is ABOUT. Non-fatal was the right choice; silent was not.
#
# Keep prose out of the eval string itself: it is a bash double-quoted argument, so
# backticks in it are command substitution rather than markup.
analyze_slice() {
  local ladder_path="$1"
  (
    cd "$EXPERIMENTS_DIR" &&
      pnpm exec tsx -e "
        import { readFileSync } from 'node:fs';
        import { analyzeLadder } from './src/microvm-density.ts';
        const samples = JSON.parse(readFileSync(process.argv[1], 'utf8'));
        console.log(JSON.stringify(analyzeLadder(samples), null, 2));
      " "$ladder_path"
  )
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  preflight
  log "arms: container, microvm (Firecracker only - hardware-corrections F5)"
  log "D values: ${D_VALUES[*]}   guest RAM (MiB): ${RAM_MB_VALUES[*]}   active runs: ${ACTIVE_RUNS[*]}"
  [ -n "$MODEL_STUB_CMD" ] || log "no SH_E11_MODEL_STUB_CMD set - driving the Exec mix directly (disclosed limitation, see header)"

  local max_c=1 c
  for c in "${ACTIVE_RUNS[@]}"; do
    [ "$c" -gt "$max_c" ] && max_c="$c"
  done

  local order first=1 arm
  order="$(shuffle_e11_arms)"
  while IFS= read -r arm; do
    [ -n "$arm" ] || continue
    if [ "$first" -eq 0 ]; then
      drop_caches
    fi
    first=0

    if [ "$arm" = "container" ]; then
      start_container_stack
      for c in "${ACTIVE_RUNS[@]}"; do
        run_density_rung container - - "$c" "e11-container" "$E11_RELAY_PORT" \
          "$RESULTS/e11-rung-container-c${c}.json"
      done
      stop_container_stack
      assemble_ladder "$RESULTS/e11-rung-container-c*.json" "$RESULTS/e11-ladder-container.json"
      analyze_slice "$RESULTS/e11-ladder-container.json" || log "analyze_slice(container) failed - see output above"
    else
      local d ram_mb
      for d in "${D_VALUES[@]}"; do
        for ram_mb in "${RAM_MB_VALUES[@]}"; do
          start_microvm_stack "$d" "$ram_mb" "$max_c"
          for c in "${ACTIVE_RUNS[@]}"; do
            run_density_rung microvm "$d" "$ram_mb" "$c" "e11-microvm-d${d}-ram${ram_mb}" "$E11_RELAY_PORT" \
              "$RESULTS/e11-rung-microvm-d${d}-ram${ram_mb}-c${c}.json"
          done
          stop_microvm_stack
          assemble_ladder "$RESULTS/e11-rung-microvm-d${d}-ram${ram_mb}-c*.json" \
            "$RESULTS/e11-ladder-microvm-d${d}-ram${ram_mb}.json"
          analyze_slice "$RESULTS/e11-ladder-microvm-d${d}-ram${ram_mb}.json" ||
            log "analyze_slice(microvm d=$d ram=$ram_mb) failed - see output above"
        done
      done
    fi
  done <<<"$order"

  log "done. Per-slice ladders and analyses are in $RESULTS/e11-ladder-*.json"
}

# Allow this file to be sourced (for tests that extract individual functions)
# without invoking main.
if [ "${E11_DENSITY_SOURCE_ONLY:-0}" != "1" ]; then
  main "$@"
fi
