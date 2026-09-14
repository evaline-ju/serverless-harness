#!/usr/bin/env bash
# deploy/microvm/tests/e11-density.test.sh
#
# Cluster-free, KVM-free tests for e11-density.sh. Like e10-lifecycle.sh, the real
# script needs /dev/kvm, a real relay, real worker binaries and (per task-21
# hardware-corrections F1/F2) a rig this task is explicitly forbidden from driving
# a real sweep against -- so what can rot silently here is the CONTRACT: the exact
# properties task-21-brief.md's Step 4 checklist requires of e11-density.sh,
# verbatim: "asserts: smaps_rollup is used and RSS is not; the ladder includes
# c=1; converge is timed separately; section 4.5's shape is recorded; the four
# idle settings are recorded and not swept; the driver is open-loop or declares
# the bias; and both arms are driven by the same code path."
#
# Each of those seven items gets its own section below, in the brief's order.
#
# Non-vacuousness pattern (matching e10-lifecycle.test.sh's own STOP/MANDATORY
# proof): a test for an absence must first prove the presence is reachable. The
# smaps_rollup section proves pss_bytes_for_pids CAN and DOES fail on an unreadable
# rollup for a still-alive pid (the presence), before asserting it never falls
# back to reading VmRSS to route around that failure (the absence).
#
# Functions are extracted from the real e11-density.sh source text (grep for the
# opening line, awk for the matching closing bare "}") and sourced in isolation --
# the same technique e10-lifecycle.test.sh and build-snapshot.test.sh use -- so
# these tests drive the REAL artifact, never a rewritten substitute.
#
# Run: bash deploy/microvm/tests/e11-density.test.sh

set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$DIR/e11-density.sh"
fails=0
check() { if [ "$2" = "$3" ]; then echo "  ok: $1"; else
  echo "  FAIL: $1 (want '$3', got '$2')"
  fails=$((fails + 1))
fi; }

# extract_fn prints the source text of a top-level "name() { ... }" function
# (opening line "name() {" and closing bare "}") from $SCRIPT.
extract_fn() {
  local name="$1" start end
  start=$(grep -n "^${name}() {" "$SCRIPT" | head -n1 | cut -d: -f1)
  [ -n "$start" ] || return 1
  end=$(awk -v s="$start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
  [ -n "$end" ] || return 1
  sed -n "${start},${end}p" "$SCRIPT"
}

# extract_fns prints several functions concatenated, in the order given, so a
# helper that calls another helper can be sourced together in one subshell.
extract_fns() {
  local name
  for name in "$@"; do
    extract_fn "$name" || return 1
    echo
  done
}

echo "== the script exists, is executable, and is shellcheck-clean"
check "e11-density.sh present" "$([ -f "$SCRIPT" ] && echo yes || echo no)" "yes"
check "e11-density.sh executable" "$([ -x "$SCRIPT" ] && echo yes || echo no)" "yes"
if command -v shellcheck >/dev/null; then
  if shellcheck -S warning "$SCRIPT" >/tmp/e11-shellcheck.out 2>&1; then
    check "shellcheck -S warning" "clean" "clean"
  else
    check "shellcheck -S warning" "$(cat /tmp/e11-shellcheck.out)" "clean"
  fi
fi

echo "== it refuses to run without SH_SUBSTRATE / SH_SNAPSHOT_DIR / SH_WORKSPACE_ROOT / SH_MAX_COMMITTED_MB"
out=$(env -u SH_SUBSTRATE SH_SNAPSHOT_DIR=/tmp SH_WORKSPACE_ROOT=/tmp SH_MAX_COMMITTED_MB=1024 bash "$SCRIPT" 2>&1)
rc=$?
case "$out" in *SH_SUBSTRATE*) has_msg=yes ;; *) has_msg=no ;; esac
check "refuses without SH_SUBSTRATE (nonzero exit)" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"
check "refusal message names SH_SUBSTRATE" "$has_msg" "yes"

out=$(env -u SH_SNAPSHOT_DIR SH_SUBSTRATE=nested-m8i SH_WORKSPACE_ROOT=/tmp SH_MAX_COMMITTED_MB=1024 bash "$SCRIPT" 2>&1)
rc=$?
check "refuses without SH_SNAPSHOT_DIR" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"

out=$(env -u SH_WORKSPACE_ROOT SH_SUBSTRATE=nested-m8i SH_SNAPSHOT_DIR=/tmp SH_MAX_COMMITTED_MB=1024 bash "$SCRIPT" 2>&1)
rc=$?
check "refuses without SH_WORKSPACE_ROOT" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"

out=$(env -u SH_MAX_COMMITTED_MB SH_SUBSTRATE=nested-m8i SH_SNAPSHOT_DIR=/tmp SH_WORKSPACE_ROOT=/tmp bash "$SCRIPT" 2>&1)
rc=$?
case "$out" in *SH_MAX_COMMITTED_MB*) has_msg2=yes ;; *) has_msg2=no ;; esac
check "refuses without SH_MAX_COMMITTED_MB (nonzero exit)" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"
check "refusal message names SH_MAX_COMMITTED_MB" "$has_msg2" "yes"

# ---------------------------------------------------------------------------
# 1. "smaps_rollup is used and RSS is not"
# ---------------------------------------------------------------------------
echo "== smaps_rollup is used and RSS is not (with non-vacuousness proof)"

pss_body="$(extract_fn pss_bytes_for_pids || true)"
check "pss_bytes_for_pids helper exists" "$([ -n "$pss_body" ] && echo yes || echo no)" "yes"

if [ -n "$pss_body" ]; then
  pss_tmpdir="$(mktemp -d)"
  pss_snippet="$pss_tmpdir/pss.sh"
  {
    echo 'die() { echo "e11: $*" >&2; exit 1; }'
    printf '%s\n' "$pss_body"
  } >"$pss_snippet"

  # A real, still-alive process (this shell's own subshell sleeper) whose fake
  # PROC_ROOT/<pid>/smaps_rollup we control directly.
  sh -c 'sleep 5' &
  live_pid=$!

  fake_proc="$pss_tmpdir/proc"
  mkdir -p "$fake_proc/$live_pid"

  # Case A (non-vacuousness proof): smaps_rollup MISSING for a live pid -> the
  # function DOES fail. This proves the failure path is reachable at all, before
  # we test that RSS is never used to route around it.
  rc_missing=0
  out_missing=$(
    PROC_ROOT="$fake_proc"
    export PROC_ROOT
    # shellcheck disable=SC1090
    . "$pss_snippet"
    pss_bytes_for_pids "$live_pid" 2>&1
  ) || rc_missing=$?
  case "$out_missing" in *smaps_rollup*) named_missing=yes ;; *) named_missing=no ;; esac
  check "non-vacuousness: unreadable smaps_rollup for a LIVE pid DOES fail (nonzero)" \
    "$([ "$rc_missing" -ne 0 ] && echo yes || echo no)" "yes"
  check "the failure names smaps_rollup, not a generic error" "$named_missing" "yes"

  # Case B: smaps_rollup present -> sums the Pss: lines, ignoring any Rss: lines
  # placed in the same file (proves it is reading Pss specifically, not just
  # whatever numeric field appears first).
  {
    echo "Rss:              999999 kB"
    echo "Pss:                 512 kB"
    echo "Pss_Anon:             256 kB"
  } >"$fake_proc/$live_pid/smaps_rollup"
  out_present=$(
    PROC_ROOT="$fake_proc"
    export PROC_ROOT
    # shellcheck disable=SC1090
    . "$pss_snippet"
    pss_bytes_for_pids "$live_pid"
  )
  check "reads Pss (512 kB -> 524288 bytes), ignoring the Rss line in the same file" \
    "$out_present" "524288"

  # Case C: pid already exited between discovery and sampling (no smaps_rollup,
  # kill -0 fails) -> contributes 0, is NOT treated as an unreadable-file failure.
  dead_pid=99999
  while kill -0 "$dead_pid" 2>/dev/null; do dead_pid=$((dead_pid + 1)); done
  rc_dead=0
  out_dead=$(
    PROC_ROOT="$fake_proc"
    export PROC_ROOT
    # shellcheck disable=SC1090
    . "$pss_snippet"
    pss_bytes_for_pids "$dead_pid"
  ) || rc_dead=$?
  check "an already-exited pid contributes 0, is not an unreadable-file failure" "$rc_dead" "0"
  check "an already-exited pid's contribution is exactly 0 bytes" "$out_dead" "0"

  kill "$live_pid" 2>/dev/null
  wait "$live_pid" 2>/dev/null
  rm -rf "$pss_tmpdir"
fi

# ---------------------------------------------------------------------------
# 1b. The PSS helper's ONLY integration point: the JSON assembly that consumes it.
#
# Final-review H2. mem_available_bytes' awk was
#   /^MemAvailable:/{print $2*1024; exit} END{if (!found) print 0}
# with `found` never assigned; awk's `exit` in a main rule RUNS the END block, so on any
# Linux host this printed TWO lines. Interpolated into host_signals_snapshot's printf it
# put a newline inside a JSON numeric value, all four json.load calls in the rung-record
# writer failed, the writer died on a SyntaxError from the resulting empty interpolations,
# and because this driver runs `set -uo pipefail` WITHOUT `set -e` nothing aborted: E11
# completed its whole sweep having written zero rung records, and exited 0.
#
# It never showed up here because darwin has no /proc/meminfo, so the `|| echo 0` fallback
# yielded a clean single "0". Every case below therefore drives a LINUX-SHAPED fixture
# through the SH_E11_PROC_ROOT seam that already existed for the PSS helper -- it only ever
# needed a meminfo in it.
# ---------------------------------------------------------------------------
echo "== host signal assembly against a Linux-shaped /proc (final review H2)"

signals_body="$(extract_fns die require_numeric mem_available_bytes discover_pids pss_bytes_for_pids host_cpu_fraction host_signals_snapshot || true)"
check "die/require_numeric/mem_available_bytes/host_signals_snapshot all extractable" \
  "$([ -n "$signals_body" ] && echo yes || echo no)" "yes"

if [ -n "$signals_body" ]; then
  sig_tmpdir="$(mktemp -d)"
  sig_snippet="$sig_tmpdir/signals.sh"
  printf '%s\n' "$signals_body" >"$sig_snippet"

  # A Linux-shaped /proc: meminfo with a real MemAvailable line, a /proc/stat cpu line,
  # and a smaps_rollup for one live pid.
  sig_proc="$sig_tmpdir/proc"
  mkdir -p "$sig_proc"
  printf 'MemTotal:       16384000 kB\nMemFree:            1000 kB\nMemAvailable:    8192000 kB\n' >"$sig_proc/meminfo"
  printf 'cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 100 0 100 800 0 0 0 0 0 0\n' >"$sig_proc/stat"

  # --- NON-VACUOUSNESS, first: prove the pathology is REAL and REACHABLE against this
  # exact fixture, before asserting it is absent. The pre-fix awk form is run here
  # verbatim; if it did not emit two lines against this meminfo, every assertion below
  # would be passing for the wrong reason.
  buggy_lines="$(awk '/^MemAvailable:/{print $2*1024; exit} END{if (!found) print 0}' "$sig_proc/meminfo" | wc -l | tr -d ' ')"
  check "non-vacuousness: the PRE-FIX awk form really does emit 2 lines on this fixture" \
    "$buggy_lines" "2"
  # ...and that a two-line value really does break the consumer, not merely look odd.
  buggy_json="$(printf '{"memAvailableBytes":%s}' "$(awk '/^MemAvailable:/{print $2*1024; exit} END{if (!found) print 0}' "$sig_proc/meminfo")")"
  buggy_rc=0
  printf '%s' "$buggy_json" | python3 -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null || buggy_rc=$?
  check "non-vacuousness: a two-line value really does make json.load fail" \
    "$([ "$buggy_rc" -ne 0 ] && echo yes || echo no)" "yes"

  # --- The fix: exactly one line, and the right number (8192000 kB * 1024).
  mem_out=$(
    PROC_ROOT="$sig_proc"
    export PROC_ROOT
    # shellcheck disable=SC1090
    . "$sig_snippet"
    mem_available_bytes
  )
  check "mem_available_bytes emits exactly ONE line on a Linux-shaped meminfo" \
    "$(printf '%s\n' "$mem_out" | wc -l | tr -d ' ')" "1"
  check "mem_available_bytes converts kB to bytes correctly" "$mem_out" "8388608000"

  # A meminfo with no MemAvailable line at all: the END fallback, still one line.
  printf 'MemTotal:       16384000 kB\n' >"$sig_proc/meminfo-noavail"
  mem_none=$(
    PROC_ROOT="$sig_proc"
    export PROC_ROOT
    # shellcheck disable=SC1090
    . "$sig_snippet"
    awk '/^MemAvailable:/{found=1; print $2*1024; exit} END{if (!found) print 0}' "$sig_proc/meminfo-noavail"
  )
  check "no MemAvailable line -> a single 0 (the END fallback still fires)" "$mem_none" "0"
  printf 'MemTotal:       16384000 kB\nMemFree:            1000 kB\nMemAvailable:    8192000 kB\n' >"$sig_proc/meminfo"

  # --- require_numeric: the guard that keeps this class from recurring. Refuses a
  # multi-line value even though EVERY line of it is numeric, which is precisely the
  # shape H2 had.
  rn_rc=0
  rn_out=$(
    # shellcheck disable=SC1090
    . "$sig_snippet"
    require_numeric memAvailableBytes "$(printf '8388608000\n0')" 2>&1
  ) || rn_rc=$?
  check "require_numeric REFUSES a two-line all-numeric value (nonzero)" \
    "$([ "$rn_rc" -ne 0 ] && echo yes || echo no)" "yes"
  case "$rn_out" in *memAvailableBytes*) rn_named=yes ;; *) rn_named=no ;; esac
  check "the refusal names the offending FIELD, not just 'bad input'" "$rn_named" "yes"
  rn_ok_rc=0
  rn_ok=$(
    # shellcheck disable=SC1090
    . "$sig_snippet"
    require_numeric memAvailableBytes "8388608000"
  ) || rn_ok_rc=$?
  check "require_numeric accepts a single integer (does not refuse everything)" "$rn_ok" "8388608000"
  check "  ...with exit 0" "$rn_ok_rc" "0"
  rn_frac=$(
    # shellcheck disable=SC1090
    . "$sig_snippet"
    require_numeric hostCpuFraction "0.5000"
  )
  check "require_numeric accepts a decimal (hostCpuFraction is printf %.4f)" "$rn_frac" "0.5000"

  # --- The whole assembly, end to end, parsed by its REAL consumer (json.load), with a
  # live pid whose smaps_rollup exists so no other field can fail for its own reasons.
  sh -c 'sleep 5' &
  sig_live=$!
  mkdir -p "$sig_proc/$sig_live"
  printf 'Pss:                 512 kB\n' >"$sig_proc/$sig_live/smaps_rollup"
  sig_json=$(
    PROC_ROOT="$sig_proc"
    export PROC_ROOT
    VMM_PROC_PATTERN="__e11_no_such_process__"
    VIRTIOFSD_PROC_PATTERN="__e11_no_such_process__"
    # shellcheck disable=SC1090
    . "$sig_snippet"
    host_signals_snapshot
  )
  sig_json_rc=0
  sig_mem="$(printf '%s' "$sig_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["memAvailableBytes"])' 2>&1)" || sig_json_rc=$?
  check "host_signals_snapshot's output parses as JSON (all four json.load calls' premise)" \
    "$sig_json_rc" "0"
  check "  ...and memAvailableBytes survives the round trip intact" "$sig_mem" "8388608000"

  # --- And the failure direction: a bad field makes the WHOLE snapshot fail loudly and
  # print nothing, rather than emit a malformed object for a consumer to choke on 350
  # lines later. Provoked with an unreadable smaps_rollup for a LIVE pid, the one
  # refusal spec section 7.3's boxed warning makes non-negotiable -- which previously
  # could not stop anything, because a `die` inside `x="$(helper)"` exits only the
  # command substitution's subshell and `set -e` is not in force.
  chmod 000 "$sig_proc/$sig_live/smaps_rollup" 2>/dev/null || true
  sig_fail_rc=0
  sig_fail_out=$(
    PROC_ROOT="$sig_proc"
    export PROC_ROOT
    # shellcheck disable=SC2034 # read by host_signals_snapshot, sourced below
    VMM_PROC_PATTERN="sleep 5"
    # shellcheck disable=SC2034
    VIRTIOFSD_PROC_PATTERN="__e11_no_such_process__"
    # shellcheck disable=SC1090
    . "$sig_snippet"
    host_signals_snapshot
  ) || sig_fail_rc=$?
  if [ -r "$sig_proc/$sig_live/smaps_rollup" ]; then
    echo "  (skip: running as root or on a filesystem ignoring chmod 000 -- the unreadable-rollup case was not exercised, not claimed verified)"
  else
    check "an unsampleable signal makes host_signals_snapshot exit NONZERO" \
      "$([ "$sig_fail_rc" -ne 0 ] && echo yes || echo no)" "yes"
    check "  ...and print no JSON at all, rather than a malformed object" "$sig_fail_out" ""
  fi
  chmod 644 "$sig_proc/$sig_live/smaps_rollup" 2>/dev/null || true

  kill "$sig_live" 2>/dev/null
  wait "$sig_live" 2>/dev/null
  rm -rf "$sig_tmpdir"
fi

echo "== a rung that writes no record fails loudly (final review H2)"
# The driver runs `set -uo pipefail` without `set -e`, deliberately (see the assertion's
# own comment in the script). That makes an explicit check for the record the only thing
# standing between a failed writer and a sweep that exits 0 having recorded nothing.
# The DRIVER's own shell options are the first `set` line in the file. Matched that way
# rather than by grepping the whole script for `set -e`, because build_converge_script's
# heredoc legitimately contains `set -eu` for the GUEST script it emits -- a whole-file
# grep would conflate the two and go red on correct code.
check "the driver's own shell options are exactly 'set -uo pipefail'" \
  "$(grep -m1 -E '^set ' "$SCRIPT")" "set -uo pipefail"
check "run_density_rung dies when out_json_path is empty or missing" \
  "$([ "$(grep -c 'wrote no record to' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "the record assertion tests the file, not the writer's exit status" \
  "$([ "$(grep -c '\[ -s "\$out_json_path" \]' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== RSS is never read as a fallback anywhere pss_bytes_for_pids or its callers run"
# Comment lines (the header's own disclosure that RSS/VmRSS is deliberately
# avoided) are stripped first, so this targets actual code, not prose that
# mentions the forbidden pattern by name while explaining its absence.
code_only="$(grep -v '^[[:space:]]*#' "$SCRIPT")"
check "no VmRSS field is ever read in code (comments excluded)" \
  "$(printf '%s\n' "$code_only" | grep -c 'VmRSS')" "0"
check "no /proc/<pid>/status is ever read in code for memory accounting (comments excluded)" \
  "$(printf '%s\n' "$code_only" | grep -c '/status')" "0"
check "the header explicitly documents that VmRSS/status is never used as a fallback" \
  "$([ "$(grep -c 'NEVER reads VmRSS' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "smaps_rollup is the only source cited for PSS" \
  "$([ "$(grep -c 'smaps_rollup' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

# ---------------------------------------------------------------------------
# 2. "the ladder includes c=1"
# ---------------------------------------------------------------------------
echo "== the active-runs ladder includes c=1"
check "ACTIVE_RUNS default includes 1" \
  "$(grep -c 'SH_E11_ACTIVE_RUNS:-1 ' "$SCRIPT")" "1"
check "the header explains why (detectKnee's c===1 baseline requirement)" \
  "$([ "$(grep -c 'c === 1\|c===1\|c=1' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

# ---------------------------------------------------------------------------
# 3. "converge is timed separately"
# ---------------------------------------------------------------------------
echo "== converge is timed as its own Exec, before the steady-state loop, in its own field"
check "build_converge_script exists (harness/src/converge.ts's script, reproduced)" \
  "$(grep -c '^build_converge_script() {' "$SCRIPT")" "1"
check "converge_slot times it host-side, separately" \
  "$(grep -c '^converge_slot() {' "$SCRIPT")" "1"
check "converge result lands in its own JSON field (convergeMsP50), not p95Ms" \
  "$(grep -c 'convergeMsP50' "$SCRIPT")" "2"
# Structural: converge_slot must be invoked BEFORE the Exec-mix while-loop within
# run_density_rung, not after -- extract the function and check line order.
rdr_body="$(extract_fn run_density_rung || true)"
check "run_density_rung exists" "$([ -n "$rdr_body" ] && echo yes || echo no)" "yes"
if [ -n "$rdr_body" ]; then
  converge_line=$(printf '%s\n' "$rdr_body" | grep -n 'converge_slot' | head -n1 | cut -d: -f1)
  mix_line=$(printf '%s\n' "$rdr_body" | grep -n 'e11_tool_call_mix\|while \[' | head -n1 | cut -d: -f1)
  check "converge_slot is called before the Exec-mix loop starts" \
    "$([ -n "$converge_line" ] && [ -n "$mix_line" ] && [ "$converge_line" -lt "$mix_line" ] && echo yes || echo no)" "yes"
fi

echo "== build_converge_script matches harness/src/converge.ts's buildConvergeScript shape"
conv_body="$(extract_fn build_converge_script || true)"
check "reproduces the flock-serialized fetch" "$(printf '%s' "$conv_body" | grep -c 'flock 9')" "1"
check "reproduces the /workspace/repo path" "$(printf '%s' "$conv_body" | grep -c '/workspace/repo')" "1"
check "reproduces the /workspace/leaves/<runId> leaf path" "$(printf '%s' "$conv_body" | grep -c '/workspace/leaves')" "2"
check "reproduces the retry-with-fresh-init-on-fetch-failure branch" \
  "$([ "$(printf '%s' "$conv_body" | grep -c 'git init -q')" -ge 2 ] && echo yes || echo no)" "yes"
check "reproduces worktree add --detach" "$(printf '%s' "$conv_body" | grep -c 'worktree add')" "1"

# ---------------------------------------------------------------------------
# 4. "section 4.5's shape is recorded"
# ---------------------------------------------------------------------------
echo "== section 4.5's repo-cache shape is recorded, and only the three named shapes validate"
check "repoCacheShape lands in the JSON record" "$(grep -c 'repoCacheShape' "$SCRIPT")" "1"
val_body="$(extract_fn validate_repo_cache_shape || true)"
check "validate_repo_cache_shape helper exists" "$([ -n "$val_body" ] && echo yes || echo no)" "yes"

if [ -n "$val_body" ]; then
  val_tmpdir="$(mktemp -d)"
  val_snippet="$val_tmpdir/val.sh"
  {
    echo 'die() { echo "e11: $*" >&2; exit 1; }'
    printf '%s\n' "$val_body"
  } >"$val_snippet"
  run_validate() {
    (
      REPO_CACHE_SHAPE="$1"
      export REPO_CACHE_SHAPE
      # shellcheck disable=SC1090
      . "$val_snippet"
      validate_repo_cache_shape
    )
  }
  for shape in two-mounts shared-clone accept-cold-fetch; do
    rc_shape=0
    run_validate "$shape" >/dev/null 2>&1 || rc_shape=$?
    check "shape '$shape' (one of section 4.5's three) validates" "$rc_shape" "0"
  done
  rc_bad=0
  bad_out=$(run_validate "made-up-shape" 2>&1) || rc_bad=$?
  check "an invented fourth shape is refused (nonzero)" "$([ "$rc_bad" -ne 0 ] && echo yes || echo no)" "yes"
  case "$bad_out" in *"made-up-shape"*) named_bad=yes ;; *) named_bad=no ;; esac
  check "the refusal names the bad value" "$named_bad" "yes"
  rm -rf "$val_tmpdir"
fi

check "this task DOES NOT decide among the three shapes (F7): no shape is hardcoded as the only option" \
  "$([ "$(grep -c 'REPO_CACHE_SHAPE=\"\${SH_E11_REPO_CACHE_SHAPE' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

# ---------------------------------------------------------------------------
# 5. "the four idle settings are recorded and not swept"
# ---------------------------------------------------------------------------
echo "== the four section 4.1 settings are recorded, and no env var sweeps them"
static_body="$(extract_fn static_settings_json || true)"
check "static_settings_json helper exists" "$([ -n "$static_body" ] && echo yes || echo no)" "yes"
if [ -n "$static_body" ]; then
  static_out=$(
    # shellcheck disable=SC1090
    . <(printf '%s\n' "$static_body")
    static_settings_json
  )
  check "records standbyIdleS=90 (DefaultStandbyIdle)" \
    "$(printf '%s' "$static_out" | grep -c '"standbyIdleS":90')" "1"
  check "records workspaceIdleS=1800 (DefaultWorkspaceIdle, 30m)" \
    "$(printf '%s' "$static_out" | grep -c '"workspaceIdleS":1800')" "1"
  check "records replenishDelayS=0.2 (DefaultReplenishDelay, 200ms)" \
    "$(printf '%s' "$static_out" | grep -c '"replenishDelayS":0.2')" "1"
  check "records reclaimScanIntervalS=22.5 (StandbyIdle/4)" \
    "$(printf '%s' "$static_out" | grep -c '"reclaimScanIntervalS":22.5')" "1"
fi
for var in SH_E11_STANDBY_IDLE SH_E11_WORKSPACE_IDLE SH_E11_REPLENISH_DELAY SH_E11_RECLAIM_SCAN_INTERVAL; do
  check "no sweep env var exists for $var (there is nothing to override)" \
    "$(grep -c "$var" "$SCRIPT")" "0"
done
check "staticSettings is embedded in every rung's JSON record" \
  "$(grep -c 'staticSettings' "$SCRIPT")" "1"

# ---------------------------------------------------------------------------
# 6. "the driver is open-loop or declares the bias"
# ---------------------------------------------------------------------------
echo "== the closed-loop bias is declared, not silently eliminated or hidden"
check "drivingModel is recorded in the JSON output" "$(grep -c 'drivingModel' "$SCRIPT")" "3"
check "the recorded value names the model" "$(grep -c 'closed-loop-per-slot' "$SCRIPT")" "2"
check "drivingModel's JSON-output occurrence is the exact declared value" \
  "$([ "$(grep -c "'drivingModel': 'closed-loop-per-slot'" "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "the header comment explains WHY this is a declared bias, not a fix" \
  "$([ "$(grep -c 'coordinated omission' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

# ---------------------------------------------------------------------------
# 7. "both arms are driven by the same code path"
# ---------------------------------------------------------------------------
echo "== the container and microvm arms are driven by ONE function, not two"
check "run_density_rung is defined exactly once" \
  "$(grep -c '^run_density_rung() {' "$SCRIPT")" "1"
check "main() calls run_density_rung for the container arm" \
  "$(grep -c 'run_density_rung container' "$SCRIPT")" "1"
check "main() calls run_density_rung for the microvm arm" \
  "$(grep -c 'run_density_rung microvm' "$SCRIPT")" "1"
check "no second, arm-specific Exec-driving function exists" \
  "$(grep -Ec '^run_density_rung_(container|microvm)\(\)' "$SCRIPT")" "0"
check "grpc_exec_record (the actual RPC call) is defined exactly once, used by both arms" \
  "$(grep -c '^grpc_exec_record() {' "$SCRIPT")" "1"

# ---------------------------------------------------------------------------
# Additional structural properties this task's hardware-corrections require,
# beyond the brief's own seven-item checklist.
# ---------------------------------------------------------------------------
echo "== F5: Cloud Hypervisor is absent as an arm, and the reason is stated"
check "no cloud-hypervisor arm is driven" "$(grep -c 'cloud-hypervisor' "$SCRIPT")" "0"
check "the header explains CH's absence is because it does not restore" \
  "$([ "$(grep -c 'does not restore' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "the exact CH hang signature is cited (Restoring virtio-console __console)" \
  "$(grep -c 'Restoring virtio-console __console' "$SCRIPT")" "1"

echo "== the microvm arm passes SH_VMM=firecracker explicitly (never a host-exec fallback)"
check "SH_VMM=firecracker is set when starting the microvm worker" \
  "$(grep -c 'SH_VMM=firecracker' "$SCRIPT")" "1"

echo "== virtiofsd's legitimate absence on the Firecracker-only arm is documented"
check "virtiofsd is still sampled for (summed, can legitimately be 0)" \
  "$(grep -c 'VIRTIOFSD_PROC_PATTERN' "$SCRIPT")" "2"
check "the header states 0 virtiofsd PSS here is expected, not a bug" \
  "$([ "$(grep -c 'EXPECTED result of an absent process' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== F3: SH_SUBSTRATE is a required, explicit label (never auto-derived, never metal-defaulted)"
check "SUBSTRATE binding uses SH_SUBSTRATE:? (required, not defaulted)" \
  "$(grep -c 'SH_SUBSTRATE:?' "$SCRIPT")" "1"
# The header legitimately WARNS against SH_SUBSTRATE=metal in a comment (F3
# disclosure); that mention must not be confused with an actual assignment.
# Strip comment lines first so this targets code, not the warning itself.
check "no hardcoded SH_SUBSTRATE=metal assignment appears in code (comments excluded)" \
  "$(grep -v '^[[:space:]]*#' "$SCRIPT" | grep -c 'SH_SUBSTRATE=metal')" "0"
check "the header explicitly warns against SH_SUBSTRATE=metal on this box" \
  "$([ "$(grep -c 'never pass SH_SUBSTRATE=metal' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== leaseSaturations is always 0, and this is disclosed, not silently assumed"
check "leaseSaturations is hardcoded to 0 in the JSON record" \
  "$(grep -c "'leaseSaturations': 0," "$SCRIPT")" "1"
check "the limitation is disclosed in the recorded proxyLimitations" \
  "$(grep -c 'bypasses the harness lease layer' "$SCRIPT")" "1"

echo "== page-cache asymmetry: caches are dropped between arms, arm order is randomized"
check "drop_caches is present" "$(grep -c '^drop_caches() {' "$SCRIPT")" "1"
check "shuffle_e11_arms randomizes arm order" "$(grep -c '^shuffle_e11_arms() {' "$SCRIPT")" "1"

echo "== no guest-side timing: no 'date' embedded inside a guest command string"
bad_guest_date=$(grep -E -- '(command|script)[^#]*\bdate\b' "$SCRIPT" | grep -v 'date +%s%N' || true)
check "no 'date' embedded in a guest command/script string" "$([ -z "$bad_guest_date" ] && echo yes || echo no)" "yes"
check "the script itself times things host-side (date +%s%N used)" \
  "$([ "$(grep -c 'date +%s%N' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== this script is never invoked by main(): it documents that it must be run by a human operator"
check "the header states it is not invoked by any automated test" \
  "$([ "$(grep -c 'NOT invoked by any automated test' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "source-only guard exists (E11_DENSITY_SOURCE_ONLY), matching e10's own pattern" \
  "$(grep -c 'E11_DENSITY_SOURCE_ONLY' "$SCRIPT")" "1"

echo "== security: the scratch redis is published on loopback, never on all interfaces"
# unbound_publishes prints every `docker run -p <host>:6379` publish in $1 whose host side
# is not explicitly bound to 127.0.0.1. `-p "6381:6379"` binds 0.0.0.0, which on the
# documented rig (an EC2 m8i.xlarge with a public interface, running microvm-worker as
# root) publishes an unauthenticated redis to the internet -- a standard host-takeover
# path via CONFIG SET dir + dbfilename.
unbound_publishes() {
  # Comment lines are stripped first (the same convention this file's VmRSS checks use):
  # the fix's own comments quote the pre-fix `-p "${PORT}:6379"` line by name, and a
  # whole-file grep would flag the explanation of the defect as the defect.
  grep -v '^[[:space:]]*#' "$1" | grep -nE -- '-p +"?[^ "]+:6379' | grep -v '127\.0\.0\.1' || true
}

# Non-vacuousness FIRST: the detector must flag the exact pre-fix line. Without this, an
# empty result below could mean "no publish is unbound" or "the regex matches nothing".
pub_fixture="$(mktemp)"
printf 'docker run --rm -d -p "${E11_REDIS_PORT}:6379" --name x redis:7 >/dev/null\n' >"$pub_fixture"
check "non-vacuousness: the detector DOES flag the pre-fix 0.0.0.0 publish" \
  "$([ -n "$(unbound_publishes "$pub_fixture")" ] && echo yes || echo no)" "yes"
rm -f "$pub_fixture"

check "no docker run publishes 6379 on all interfaces" \
  "$([ -z "$(unbound_publishes "$SCRIPT")" ] && echo yes || echo no)" "yes"
# The complement, so the check above cannot pass merely because the redis went away.
check "the redis publish is explicitly bound to 127.0.0.1" \
  "$([ "$(grep -c -- '-p "127.0.0.1:' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "both arms start redis through ONE helper (a second docker run cannot drift)" \
  "$(grep -v '^[[:space:]]*#' "$SCRIPT" | grep -c 'docker run')" "1"
check "start_redis_loopback is called by both arms" \
  "$(grep -c '^  start_redis_loopback ' "$SCRIPT")" "2"
check "the redis image is overridable so an operator can pin a digest" \
  "$(grep -c 'SH_E11_REDIS_IMAGE' "$SCRIPT")" "2"
check "redis is started with RDB snapshots disabled (--save '')" \
  "$([ "$(grep -c -- "--save ''" "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo
echo "Total failures: $fails"
exit "$fails"
