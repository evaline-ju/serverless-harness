#!/usr/bin/env bash
# deploy/microvm/tests/e10-lifecycle.test.sh
#
# Cluster-free, KVM-free tests for e10-lifecycle.sh. The script itself needs
# /dev/kvm, a real relay, and real vmpoolctl/hypervisor binaries and cannot run on
# every PR — but its CONTRACT can rot silently, and this is the first task in the
# P4 microVM tier that produces a performance NUMBER rather than a correctness
# proof, so a rotted contract here produces a verdict nobody should trust:
#
#   - it must refuse without SH_SUBSTRATE, or every record it writes is unlabeled
#     (spec §6: "Record the substrate in every run record").
#   - its governor check must be the three-way logic hardware-corrections E10
#     requires, not the brief's naive two-way "cat the path or die" (which would
#     die with a misleading message if the path is simply absent).
#   - it must be STRUCTURALLY incapable of printing a STOP or MANDATORY verdict
#     when the substrate is not exactly "metal" (hardware-corrections E9: "This
#     rig is not metal ... The script must be structurally incapable of printing
#     one when the substrate is not metal — enforce that in the test, not in a
#     comment.") — checked here by actually calling verdict() with inputs that
#     WOULD trigger those strings on metal, under a nested substrate, and
#     confirming they do not appear; the same inputs under metal are asserted TO
#     produce them, so this is not a vacuously-true check.
#   - it must encode all four rows of spec §7.2's decision-rule table, including
#     the ratio tie-break in the middle band and the >=15ms hard stop.
#   - it must randomize rung/arm order and drop caches between arms (spec §7.5:
#     page-cache asymmetry), and never read guest-side timing (spec §2.4: guest
#     clocks jump on resume).
#   - it must run rung 3 both pinned and unpinned (the density mechanism's price).
#
# The whole script cannot be sourced for these behavioral checks: it ends in an
# unconditional `main "$@"` call that would immediately demand /dev/kvm, a running
# relay and real SH_* env vars. e10-lifecycle.sh works around this itself via
# E10_LIFECYCLE_SOURCE_ONLY=1 (skips main); for testing ONE function in isolation
# (check_governor, verdict) without even preflight's other three refusals firing,
# this file extracts that function's own source text (grep for its opening line,
# awk for the matching closing "}") and sources ONLY that snippet in a subshell —
# the same pattern deploy/microvm/tests/build-snapshot.test.sh uses for
# hardlink_or_copy_bin.
#
# Run: bash deploy/microvm/tests/e10-lifecycle.test.sh

set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$DIR/e10-lifecycle.sh"
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

echo "== the script exists, is executable, and is shellcheck-clean"
check "e10-lifecycle.sh present" "$([ -f "$SCRIPT" ] && echo yes || echo no)" "yes"
check "e10-lifecycle.sh executable" "$([ -x "$SCRIPT" ] && echo yes || echo no)" "yes"
if command -v shellcheck >/dev/null; then
  if shellcheck "$SCRIPT" >/tmp/e10-shellcheck.out 2>&1; then
    check "shellcheck" "clean" "clean"
  else
    check "shellcheck" "$(cat /tmp/e10-shellcheck.out)" "clean"
  fi
fi

# SC2154 (a variable referenced but never assigned) is an OPTIONAL shellcheck check, off by
# default at every severity -- so `shellcheck -S warning` passes a driver that references a
# variable nothing defines, and `bash -n` passes it too. Under these drivers' own
# `set -uo pipefail` that is a HARD RUNTIME FAILURE on the first line that reads it.
#
# This assertion exists because exactly that shipped: a fix to the grpcurl invocation
# introduced $PROTO_IMPORT_PATH and $PROTO_REL_PATH into BOTH drivers but defined them in
# only ONE, and nothing caught it -- not bash -n, not shellcheck at warning, not this suite,
# because the affected code path (rung 1 / the container arm) has never executed. It would
# have died on the metal box with "PROTO_IMPORT_PATH: unbound variable".
if command -v shellcheck >/dev/null; then
  if shellcheck -o check-unassigned-uppercase -S warning "$SCRIPT" >/tmp/e10-sc2154.out 2>&1; then
    check "no uppercase variable is referenced but never assigned (SC2154)" "clean" "clean"
  else
    check "no uppercase variable is referenced but never assigned (SC2154)" \
      "$(grep -c SC2154 /tmp/e10-sc2154.out) finding(s): $(grep SC2154 /tmp/e10-sc2154.out | head -3 | tr '\n' ' ')" "clean"
  fi
fi

echo "== it refuses to run without SH_SUBSTRATE"
out=$(env -u SH_SUBSTRATE SH_SNAPSHOT_DIR=/tmp SH_WORKSPACE_ROOT=/tmp bash "$SCRIPT" 2>&1)
rc=$?
case "$out" in *SH_SUBSTRATE*) has_msg=yes ;; *) has_msg=no ;; esac
check "refuses without SH_SUBSTRATE (nonzero exit)" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"
check "refusal message names SH_SUBSTRATE" "$has_msg" "yes"

echo "== it refuses to run without SH_SNAPSHOT_DIR / SH_WORKSPACE_ROOT"
out=$(env SH_SUBSTRATE=nested-test -u SH_SNAPSHOT_DIR -u SH_WORKSPACE_ROOT bash "$SCRIPT" 2>&1)
rc=$?
check "refuses without SH_SNAPSHOT_DIR/SH_WORKSPACE_ROOT" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"

echo "== no section 7.2 verdict is printed when the warm rung was not actually warm"
# Defect 18, found by the metal smoke pass: verdict() read rung 2's p50_total_us and called it
# the warm hot path without ever asking whether those acquires were warm. At ITERS=5 they were
# not -- one warm out of five, the rest cold because the pool cannot refill between Execs -- and
# the driver printed "STOP: warm hot path 69.87ms >= 15ms on metal", a recommendation to abandon
# the design, from a cold-path number. This is the highest-stakes instance of a class these
# drivers already guard ("a p95 of 0 at a rung where every Exec failed is not a fast rung").
check "the verdict path reads the acquire mix at all" \
  "$([ "$(grep -c 'warm_acquires' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "it refuses on a cold-dominated warm rung" \
  "$([ "$(grep -c 'is not a warm-hot-path measurement' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "the refusal names the fix (raise ITERS), not an override" \
  "$([ "$(grep -c 'Raise ITERS so replenishment' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
# The mix must reach the RECORD too, not just stderr: a reader judging a run that passed
# narrowly needs the numbers, and a verdict is only as good as the fraction behind it.
check "the summary records the mix" \
  "$(grep -c "rung2_warm_acquires" "$SCRIPT")" "1"
# And the guard must be a MAJORITY test, not "any cold at all": the very first Exec of a run is
# structurally a cold acquire (first-exec), so refusing on any cold would refuse every run.
check "the guard is a majority test, so a structural first-exec cold does not fail it" \
  "$([ "$(grep -c 'warm_acq \* 2' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== the missing-snapshot guard actually STOPS the run"
# It did not. die() was defined 53 lines below its first caller, so with a wrong
# SH_SNAPSHOT_DIR bash printed "die: command not found" and -- because this script sets
# -uo pipefail but NOT -e -- carried straight on into preflight. On a host with /dev/kvm
# that means the whole rung-1 container baseline runs before anything fails, which for a
# one-shot metal session is the baseline thrown away on a config typo. Asserted on
# BEHAVIOUR (exit status and message), not on the source text, because the source read
# perfectly fine while being unreachable.
out=$(env SH_SUBSTRATE=nested-test SH_SNAPSHOT_DIR=/nonexistent/parent \
  SH_SNAPSHOT_IMAGE=nope SH_WORKSPACE_ROOT=/tmp bash "$SCRIPT" 2>&1)
rc=$?
check "a missing manifest.json exits non-zero" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"
case "$out" in *"no manifest.json under"*) has_msg=yes ;; *) has_msg=no ;; esac
check "and says which path it looked in" "$has_msg" "yes"
case "$out" in *"command not found"*) unreachable=yes ;; *) unreachable=no ;; esac
check "die is defined before its first caller (no 'command not found')" "$unreachable" "no"

echo "== the PARTIAL RUN summary does not read variables that are still unassigned"
# warm_ms/repl_ms are declared unset at the top of main and assigned only BELOW the
# partial-run branch, so printing them there was an unbound-variable error under `set -u`:
# the summary died instead of printing, on exactly the path the runbook recommends for a
# host without grpcurl/docker/pnpm (SH_E10_RUNGS='2 3 4'). The branch must reference only
# values that exist where it runs.
partial_body="$(sed -n '/PARTIAL RUN - no section 7.2 verdict/,/exit 3/p' "$SCRIPT")"
check "the partial-run branch does not read \$warm_ms" \
  "$(printf '%s' "$partial_body" | grep -c '\${warm_ms}')" "0"
check "the partial-run branch does not read \$repl_ms" \
  "$(printf '%s' "$partial_body" | grep -c '\${repl_ms}')" "0"
check "it reports the warm term from a value that IS assigned there" \
  "$([ "$(printf '%s' "$partial_body" | grep -c 'warm_us')" -ge 1 ] && echo yes || echo no)" "yes"

echo "== check_governor: extracted and sourced in isolation (no real /dev/kvm needed)"
gov_body="$(extract_fn check_governor || true)"
check "check_governor helper exists" "$([ -n "$gov_body" ] && echo yes || echo no)" "yes"

if [ -n "$gov_body" ]; then
  gov_tmpdir="$(mktemp -d)"
  gov_snippet="$gov_tmpdir/gov.sh"
  {
    echo 'die() { echo "e10: $*" >&2; exit 1; }'
    printf '%s\n' "$gov_body"
  } >"$gov_snippet"

  # Case 1: path absent entirely -> proceed, print "not exposed" (E10: absence is a
  # fact, not a failure).
  gov_absent_path="$gov_tmpdir/does-not-exist"
  gov_absent_out=$(
    export GOVERNOR_PATH="$gov_absent_path"
    # shellcheck disable=SC1090
    . "$gov_snippet"
    check_governor
  )
  gov_absent_rc=$?
  check "governor path absent: exits 0" "$gov_absent_rc" "0"
  check "governor path absent: reports 'not exposed'" "$gov_absent_out" "not exposed"

  # Case 2: path present, value "performance" -> proceed, echo "performance".
  gov_perf_path="$gov_tmpdir/governor-performance"
  echo "performance" >"$gov_perf_path"
  gov_perf_out=$(
    export GOVERNOR_PATH="$gov_perf_path"
    # shellcheck disable=SC1090
    . "$gov_snippet"
    check_governor
  )
  gov_perf_rc=$?
  check "governor=performance: exits 0" "$gov_perf_rc" "0"
  check "governor=performance: reports 'performance'" "$gov_perf_out" "performance"

  # Case 3: path present, value something else -> REFUSE (it is fixable).
  gov_bad_path="$gov_tmpdir/governor-powersave"
  echo "powersave" >"$gov_bad_path"
  gov_bad_rc=0
  gov_bad_out=$({
    export GOVERNOR_PATH="$gov_bad_path"
    # shellcheck disable=SC1090
    . "$gov_snippet"
    check_governor
  } 2>&1) || gov_bad_rc=$?
  check "governor=powersave: refuses (nonzero)" "$([ "$gov_bad_rc" -ne 0 ] && echo yes || echo no)" "yes"
  case "$gov_bad_out" in *powersave*) named=yes ;; *) named=no ;; esac
  check "governor=powersave: refusal names the actual value" "$named" "yes"

  rm -rf "$gov_tmpdir"
fi

echo "== verdict(): the four §7.2 decision-rule rows, computed"
verdict_body="$(extract_fn verdict || true)"
check "verdict helper exists" "$([ -n "$verdict_body" ] && echo yes || echo no)" "yes"

if [ -n "$verdict_body" ]; then
  verdict_snippet="$(mktemp)"
  printf '%s\n' "$verdict_body" >"$verdict_snippet"
  run_verdict() {
    (
      # shellcheck disable=SC1090
      . "$verdict_snippet"
      verdict "$1" "$2" "$3" "$4"
    )
  }

  # Row 1: proceed as designed. metal, warm=3ms (<5ms), container=2ms -> ratio 1.5x.
  out="$(run_verdict metal 3 2 10)"
  case "$out" in *"PROCEED AS DESIGNED"*) row1=yes ;; *) row1=no ;; esac
  check "row1 (metal, warm<5ms): PROCEED AS DESIGNED" "$row1" "yes"

  # Row 1, nested variant: <8ms nested still proceeds as designed.
  out="$(run_verdict nested-m8i 7 5 10)"
  case "$out" in *"PROCEED AS DESIGNED"*) row1n=yes ;; *) row1n=no ;; esac
  check "row1 (nested, warm<8ms): PROCEED AS DESIGNED" "$row1n" "yes"

  # Row 2: middle band, ratio within 2x -> re-priced, no split pre-authorised language.
  out="$(run_verdict metal 8 5 10)"
  case "$out" in *"RE-PRICED"*) row2=yes ;; *) row2=no ;; esac
  check "row2 (metal, warm in [5,15)): RE-PRICED against container baseline" "$row2" "yes"

  # Row 2, ratio tie-break: ratio > 2x pre-authorises the split. "In the middle band
  # the ratio governs."
  out="$(run_verdict metal 10 2 10)"
  case "$out" in *"pre-authorised"*) row2ratio=yes ;; *) row2ratio=no ;; esac
  check "row2 tie-break (ratio > 2x): split pre-authorised" "$row2ratio" "yes"

  # Row 3: warm >= 15ms on metal is a hard stop REGARDLESS of ratio (even a tiny
  # ratio must still stop).
  out="$(run_verdict metal 15 14 10)"
  case "$out" in *STOP*) row3=yes ;; *) row3=no ;; esac
  check "row3 (metal, warm>=15ms, ratio~1x): STOP regardless of ratio" "$row3" "yes"

  # Replenishment row: >50ms metal is MANDATORY.
  out="$(run_verdict metal 3 2 51)"
  case "$out" in *MANDATORY*) row4=yes ;; *) row4=no ;; esac
  check "replenishment (metal, >50ms): MANDATORY" "$row4" "yes"

  # Replenishment middle band [25,50].
  out="$(run_verdict metal 3 2 30)"
  case "$out" in *"RE-PRICED"*) row4mid=yes ;; *) row4mid=no ;; esac
  check "replenishment (metal, [25,50]ms): RE-PRICED" "$row4mid" "yes"

  # Replenishment proceed. Matched on the row-1 PHRASE, not on the substring
  # "PROCEED", which "PROCEED, RE-PRICED" also contains -- so the old form of this
  # check could not tell row 1 from row 2 at all.
  out="$(run_verdict metal 3 2 10)"
  repl_line="$(echo "$out" | sed -n 's/^repl_verdict=//p')"
  case "$repl_line" in "PROCEED - "*) row4ok=yes ;; *) row4ok=no ;; esac
  check "replenishment (metal, <25ms): row 1 PROCEED, not row 2 RE-PRICED" "$row4ok" "yes"

  # -------------------------------------------------------------------------
  # The NESTED replenishment half (final review M2). Spec §7.2's row 1 sets TWO dual
  # thresholds -- warm hot path < 5ms metal / < 8ms nested AND replenishment CPU
  # < 25ms metal / < 40ms nested -- and only the first was honoured. The warm path
  # already had a nested case above; these three cover the half that did not, which
  # is why M2 shipped: all three replenishment cases passed `metal`, so the untested
  # half was exactly the wrong half.
  #
  # RULING 20-B makes nested the only substrate this project has, so [25, 40) ms is
  # the band an actual run is most likely to land in.
  # -------------------------------------------------------------------------
  repl_verdict_of() { echo "$1" | sed -n 's/^repl_verdict=//p'; }

  # 30ms on nested: spec row 1 (< 40ms nested) -> PROCEED, not RE-PRICED.
  out="$(run_verdict nested-m8i 3 2 30)"
  repl_line="$(repl_verdict_of "$out")"
  case "$repl_line" in "PROCEED - "*) n30=yes ;; *) n30=no ;; esac
  check "replenishment (nested, 30ms): row 1 PROCEED - the spec's nested threshold is 40ms" "$n30" "yes"
  case "$repl_line" in *RE-PRICED*) n30r=yes ;; *) n30r=no ;; esac
  check "  ...and NOT row 2's RE-PRICED (M2's misclassification)" "$n30r" "no"
  # The labelling half of M2: a nested measurement must never be given a metal band.
  case "$repl_line" in *metal*) n30m=yes ;; *) n30m=no ;; esac
  check "  ...and the text never labels a nested number with a 'metal' band (RULING 20-B)" "$n30m" "no"
  case "$repl_line" in *nested-m8i*) n30s=yes ;; *) n30s=no ;; esac
  check "  ...and DOES name the actual substrate" "$n30s" "yes"

  # 45ms on nested: above 40ms, at or below 50ms -> row 2, and the band it names must
  # be this substrate's [40,50], not metal's [25,50].
  out="$(run_verdict nested-m8i 3 2 45)"
  repl_line="$(repl_verdict_of "$out")"
  case "$repl_line" in *"RE-PRICED"*) n45=yes ;; *) n45=no ;; esac
  check "replenishment (nested, 45ms): row 2 RE-PRICED (>= the 40ms nested threshold)" "$n45" "yes"
  case "$repl_line" in *"[40,50]"*) n45b=yes ;; *) n45b=no ;; esac
  check "  ...and names the [40,50]ms band for this substrate, not metal's [25,50]" "$n45b" "yes"
  case "$repl_line" in *metal\ band*) n45m=yes ;; *) n45m=no ;; esac
  check "  ...and never calls it a 'metal band'" "$n45m" "no"

  # Boundary, both directions, so the cutoff is pinned at 40 rather than merely
  # somewhere between 30 and 45: 39.9ms is row 1, 40ms is row 2.
  out="$(run_verdict nested-m8i 3 2 39.9)"
  case "$(repl_verdict_of "$out")" in "PROCEED - "*) nb1=yes ;; *) nb1=no ;; esac
  check "replenishment (nested, 39.9ms): still row 1 (strictly below 40)" "$nb1" "yes"
  out="$(run_verdict nested-m8i 3 2 40)"
  case "$(repl_verdict_of "$out")" in *RE-PRICED*) nb2=yes ;; *) nb2=no ;; esac
  check "replenishment (nested, 40ms exactly): row 2 (the band is [40,50] inclusive)" "$nb2" "yes"

  # And the metal boundary is unchanged by all of this -- 30ms on METAL is still row 2.
  # Without this, moving the nested cutoff could have moved metal's with it unnoticed.
  out="$(run_verdict metal 3 2 30)"
  repl_line="$(repl_verdict_of "$out")"
  case "$repl_line" in *"[25,50]ms metal band"*) mb=yes ;; *) mb=no ;; esac
  check "replenishment (metal, 30ms): STILL row 2's [25,50]ms metal band (unchanged)" "$mb" "yes"

  echo "== E9's structural-incapacity property: never STOP/MANDATORY off metal"
  # Extreme inputs that DO trigger STOP and MANDATORY on metal (non-vacuousness
  # check performed first, so the absence of those strings under nested cannot be
  # explained by the inputs themselves being too mild).
  metal_extreme="$(run_verdict metal 20 1 100)"
  case "$metal_extreme" in *STOP*) metal_has_stop=yes ;; *) metal_has_stop=no ;; esac
  case "$metal_extreme" in *MANDATORY*) metal_has_mand=yes ;; *) metal_has_mand=no ;; esac
  check "non-vacuousness: metal + extreme inputs DOES say STOP" "$metal_has_stop" "yes"
  check "non-vacuousness: metal + extreme inputs DOES say MANDATORY" "$metal_has_mand" "yes"

  nested_extreme="$(run_verdict nested-c8i 20 1 100)"
  case "$nested_extreme" in *STOP*) nested_has_stop=yes ;; *) nested_has_stop=no ;; esac
  case "$nested_extreme" in *MANDATORY*) nested_has_mand=yes ;; *) nested_has_mand=no ;; esac
  check "E9: nested-c8i + the SAME extreme inputs never says STOP" "$nested_has_stop" "no"
  check "E9: nested-c8i + the SAME extreme inputs never says MANDATORY" "$nested_has_mand" "no"

  # Belt-and-suspenders: sweep a grid of substrate x warm x repl combinations,
  # asserting the invariant holds everywhere it is tested, not just at one point.
  sweep_ok=yes
  for s in nested-c8i nested-m8i nested-anything; do
    for w in 1 6 9 14 15 16 25 50 100; do
      for r in 1 10 24 25 30 50 51 75 200; do
        o="$(run_verdict "$s" "$w" 1 "$r")"
        case "$o" in *STOP*) sweep_ok=no ;; esac
        case "$o" in *MANDATORY*) sweep_ok=no ;; esac
      done
    done
  done
  check "E9 sweep: no (substrate!=metal, warm, repl) triple ever emits STOP/MANDATORY" "$sweep_ok" "yes"

  rm -f "$verdict_snippet"
fi

echo "== structural: source-level checks that cannot be faked by only passing the above"
check "SUBSTRATE binding uses SH_SUBSTRATE:? (required, not defaulted)" \
  "$(grep -c 'SH_SUBSTRATE:?' "$SCRIPT")" "1"
[ "$(grep -c 'drop_caches' "$SCRIPT")" -ge 1 ] && drop_present=yes || drop_present=no
check "drop_caches is actually present (>=1 occurrence)" "$drop_present" "yes"
[ "$(grep -Ec 'shuffle_arms|rand\(\)' "$SCRIPT")" -ge 1 ] && shuffle_present=yes || shuffle_present=no
check "a randomization/shuffle step actually exists (>=1 occurrence)" "$shuffle_present" "yes"

check "rung 3 runs with --pin-memfile=false" \
  "$(grep -c -- '--pin-memfile=false' "$SCRIPT")" "1"
check "rung 3 runs with --pin-memfile=true" \
  "$(grep -c -- '--pin-memfile=true' "$SCRIPT")" "1"

check "rung 2 runs once with no --stdin (parked-bash path)" \
  "$(grep -c -- 'mode=exec -- true' "$SCRIPT")" "1"
check "rung 2 runs once with --stdin set (fresh-child path)" \
  "$(grep -c -- '\-\-stdin=' "$SCRIPT")" "1"

echo "== ARMS defaults to firecracker only (hardware-corrections E8)"
check "ARMS defaults to firecracker, not the brief's two-arm list" \
  "$(grep -c 'SH_E10_ARMS:-firecracker' "$SCRIPT")" "1"
check "the brief's hardcoded two-arm default is not present" \
  "$(grep -c '^ARMS=(firecracker cloud-hypervisor)' "$SCRIPT")" "0"

echo "== SH_E10_RUNGS: a partial run measures rungs but DECLINES the verdict"
# Rung 1 is the container baseline and needs grpcurl/docker/pnpm. A hypervisor host without
# them can still measure rungs 2-4, where the substrate-independent ratios live. What must
# never happen is a favourable verdict computed without a baseline.
wr_snippet="$(mktemp "$DIR/tests/zz-wants-rung-XXXXXX.sh")"
# Extracted into the tests directory, not /tmp: the suite computes DIR from its own location,
# and a snippet elsewhere would resolve paths differently.
sed -n '/^wants_rung() {/,/^}/p' "$SCRIPT" > "$wr_snippet"
wr_ok=yes
# RUNGS is read by the sourced wants_rung, not by this file, hence SC2034; and the snippet
# path is built at run time, hence SC1090.
# shellcheck disable=SC1090,SC2034
( RUNGS="2 3 4"; . "$wr_snippet"; wants_rung 1 ) && wr_ok=no    # must be FALSE
# shellcheck disable=SC1090,SC2034
( RUNGS="2 3 4"; . "$wr_snippet"; wants_rung 3 ) || wr_ok=no    # must be TRUE
# shellcheck disable=SC1090,SC2034
( RUNGS="1 2 3 4"; . "$wr_snippet"; wants_rung 1 ) || wr_ok=no  # non-vacuousness: TRUE by default
# shellcheck disable=SC1090,SC2034
( RUNGS="2 3 4"; . "$wr_snippet"; wants_rung 34 ) && wr_ok=no   # must not substring-match
rm -f "$wr_snippet"
check "wants_rung selects exactly the listed rungs (and does not substring-match)" "$wr_ok" "yes"

check "SH_E10_RUNGS is the selector, defaulting to all four" \
  "$(grep -c 'RUNGS="\${SH_E10_RUNGS:-1 2 3 4}"' "$SCRIPT")" "1"

# grpcurl/docker/pnpm must only be demanded when rung 1 will actually run, or a rungs-2-4 run
# refuses on tooling it never uses.
pre_body="$(awk '/^preflight\(\) \{/,/^\}/' "$SCRIPT")"
gated=yes
printf '%s\n' "$pre_body" | grep -q 'if wants_rung 1; then' || gated=no
check "preflight gates rung 1's tooling behind wants_rung 1" "$gated" "yes"

# The decline path: it must print PARTIAL RUN and must NOT contain any verdict token, because
# section 7.2's middle band is a ratio against the baseline it does not have.
partial="$(awk '/if ! wants_rung 1; then/,/^  fi$/' "$SCRIPT")"
check "a partial run announces itself" \
  "$(printf '%s\n' "$partial" | grep -c 'PARTIAL RUN')" "1"
pv=no
printf '%s\n' "$partial" | grep -qE 'PROCEED|STOP|MANDATORY|RE-PRICED' || pv=yes
check "the partial-run branch emits NO verdict token" "$pv" "yes"
check "...and exits non-zero so a caller cannot mistake it for a completed experiment" \
  "$(printf '%s\n' "$partial" | grep -c 'exit 3')" "1"
# Non-vacuousness for the two checks above: verdict tokens DO exist elsewhere in the script,
# so their absence in that branch is a property of the branch, not of the file.
check "non-vacuousness: verdict tokens exist elsewhere in the script" \
  "$([ "$(grep -cE 'PROCEED AS DESIGNED' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== every rung passes a command after --, because vmpoolctl requires one in EVERY mode"
# Rungs 3 and 4 measure a lifecycle phase, not a command, and originally passed none -- so both
# died on "a command is required after --" the first time they were ever run. Rung 2 did pass
# one, which is why the ladder's first rung worked and this survived review.
vr_body="$(awk '/^vmpoolctl_run\(\) \{/,/^\}/' "$SCRIPT")"
check "vmpoolctl_run supplies a no-op command when the caller gave none" \
  "$(printf '%s\n' "$vr_body" | grep -c 'set -- "$@" -- true')" "1"
check "...detected by looking for an existing -- in the caller's args" \
  "$(printf '%s\n' "$vr_body" | grep -c 'has_cmd=1')" "1"
# Non-vacuousness: rung 2 DOES pass its own command, so the guard must not double it.
r2_body="$(awk '/^run_rung2\(\) \{/,/^\}/' "$SCRIPT")"
check "non-vacuousness: rung 2 still passes its own command" \
  "$([ "$(printf '%s\n' "$r2_body" | grep -c -- '-- true')" -ge 1 ] && echo yes || echo no)" "yes"

echo "== no guest-side timing: no 'date' inside any vmpoolctl -- command"
# vmpoolctl_run's own definition legitimately shells out to vmpoolctl; what must
# never happen is a 'date' call embedded in the COMMAND STRING handed to vmpoolctl
# (i.e. inside the guest), since guest clocks jump on resume (spec §2.4).
bad_guest_date=$(grep -E -- '--\s+.*\bdate\b' "$SCRIPT" || true)
check "no 'date' embedded in a guest command string" "$([ -z "$bad_guest_date" ] && echo yes || echo no)" "yes"
# The script's own host-side timing (grpc_exec_ms) legitimately calls date — that
# is fine, since it is measuring host wall time around the RPC, never a guest clock.
check "the script still times rung1 itself (date used somewhere, on the host)" \
  "$([ "$(grep -c 'date +%s%N' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== preflight refuses on the other three hard checks too (still two-way, not relaxed)"
kvm_body="$(extract_fn check_kvm || true)"
check "check_kvm helper exists" "$([ -n "$kvm_body" ] && echo yes || echo no)" "yes"
cgroups_body="$(extract_fn check_cgroups || true)"
check "check_cgroups helper exists" "$([ -n "$cgroups_body" ] && echo yes || echo no)" "yes"
swap_body="$(extract_fn check_swap || true)"
check "check_swap helper exists" "$([ -n "$swap_body" ] && echo yes || echo no)" "yes"

echo "== rung1 anchor: the tool-call mix goes beyond the trivial command + 1 MiB read"
mix_body="$(extract_fn rung1_tool_call_mix || true)"
check "rung1_tool_call_mix helper exists" "$([ -n "$mix_body" ] && echo yes || echo no)" "yes"
check "rung1_tool_call_mix includes the brief's trivial command" \
  "$(printf '%s' "$mix_body" | grep -c '"true"')" "1"
check "rung1_tool_call_mix includes the brief's 1 MiB read" \
  "$(printf '%s' "$mix_body" | grep -c '1048576')" "1"
[ "$(printf '%s\n' "$mix_body" | grep -c '^  echo "')" -ge 5 ] && mix_rich=yes || mix_rich=no
check "rung1_tool_call_mix has at least 5 distinct command shapes" "$mix_rich" "yes"

# ---------------------------------------------------------------------------
# vmpoolctl is built and checked, and no rung's p50 may default to zero.
#
# Review 4001908604: nothing built vmpoolctl and nothing checked it -- $VMPOOLCTL was just
# a path. A missing binary exits 127 per rung; with `set -e` absent the `>` redirections
# left empty JSON files, the readback in main() swallowed the json.load failure with
# `2>/dev/null || echo 0`, and the script printed "PROCEED AS DESIGNED - 0.00ms is below
# the 8ms threshold" -- the most favourable row of spec section 7.2's table, at exit 0, for
# a run in which rungs 2, 3 and 4 never executed.
# ---------------------------------------------------------------------------
echo "== vmpoolctl is built and existence-checked before any rung uses it"
check "ensure_vmpoolctl exists" "$(grep -c '^ensure_vmpoolctl() {' "$SCRIPT")" "1"
check "preflight calls it, so no rung can run against a missing binary" \
  "$(grep -c '^  ensure_vmpoolctl$' "$SCRIPT")" "1"
check "it builds ./cmd/vmpoolctl, not only ./cmd/worker" \
  "$(grep -c 'go build -o "\$VMPOOLCTL" ./cmd/vmpoolctl' "$SCRIPT")" "1"
check "it refuses if the binary is still not executable afterwards" \
  "$(grep -c '\[ -x "\$VMPOOLCTL" \]' "$SCRIPT")" "2"
check "grpcurl is checked by name too (rung 1 measures nothing without it)" \
  "$([ "$(grep -c 'require_tool grpcurl' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== every vmpoolctl rung is refused unless it wrote a parseable record"
check "vmpoolctl_run dies when vmpoolctl exits non-zero" \
  "$([ "$(grep -c 'vmpoolctl exited non-zero for' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "vmpoolctl_run dies when the record is empty (E11's [ -s ] guard, which E10 lacked)" \
  "$([ "$(grep -c '\[ -s "\$out_json" \]' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "vmpoolctl_run dies when the record is not valid JSON" \
  "$([ "$(grep -c 'is not valid JSON' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
# The shape that made the old form silent: the rungs redirected vmpoolctl's stdout at the
# CALL SITE, so an empty file was indistinguishable from a record. The redirect now lives
# inside vmpoolctl_run, which checks it.
check "no rung redirects vmpoolctl's stdout at the call site any more" \
  "$(grep -cE '^\s*>\"\$RESULTS/e10-rung' "$SCRIPT")" "0"
check "the summary write is guarded the same way" \
  "$([ "$(grep -c '\[ -s "\$RESULTS/e10-summary.json" \]' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "both p50 readbacks now die instead of defaulting to 0" \
  "$(grep -c 'refusing to print a section 7.2 verdict about' "$SCRIPT")" "2"
# The two `|| echo 0` left in code are `wc -l <file` LINE COUNTS -- a single command, not a
# pipeline, so `pipefail` cannot make them emit a value plus a second line. Asserted as a
# pair so a future `<pipeline> || echo 0` cannot slip in under the same count.
code_only_e10="$(grep -v '^[[:space:]]*#' "$SCRIPT")"
check "only two '|| echo 0' remain in code" \
  "$(printf '%s\n' "$code_only_e10" | grep -c '|| echo 0')" "2"
check "  ...and both are wc -l line counts, which cannot produce a two-line value" \
  "$(printf '%s\n' "$code_only_e10" | grep '|| echo 0' | grep -c 'wc -l <')" "2"

echo "== require_positive: a zero or absent p50 is refused, not defaulted"
pos_body="$(extract_fn require_positive || true)"
check "require_positive exists" "$([ -n "$pos_body" ] && echo yes || echo no)" "yes"

if [ -n "$pos_body" ]; then
  pos_tmpdir="$(mktemp -d)"
  pos_snippet="$pos_tmpdir/pos.sh"
  {
    echo 'die() { echo "e10: $*" >&2; exit 1; }'
    echo 'RESULTS=/tmp/e10-test-results'
    printf '%s\n' "$pos_body"
  } >"$pos_snippet"
  rp() {
    (
      # shellcheck disable=SC1090
      . "$pos_snippet"
      require_positive "$1" "$2"
    )
  }

  # Non-vacuousness first: real measurements pass through unchanged, so the refusals below
  # are about the VALUES and not about require_positive rejecting everything.
  check "a real integer p50 passes through unchanged" "$(rp warm_us 4312)" "4312"
  check "a real decimal p50 passes through unchanged" "$(rp warm_ms 3.75)" "3.75"

  for bad_case in "0:a zero (the value a rung that never ran produced)" \
    ":an empty value (what a swallowed python traceback leaves)" \
    "0.00:a zero with decimals"; do
    bad_value="${bad_case%%:*}"
    bad_why="${bad_case#*:}"
    bad_rc=0
    bad_out="$(rp warm_hot_path_p50_us "$bad_value" 2>&1)" || bad_rc=$?
    check "refuses $bad_why" "$([ "$bad_rc" -ne 0 ] && echo yes || echo no)" "yes"
    case "$bad_out" in *warm_hot_path_p50_us*) bad_named=yes ;; *) bad_named=no ;; esac
    check "  ...naming the field" "$bad_named" "yes"
  done

  # The two-line shape, which is the H2 defect E11 hit: every line numeric, two of them.
  two_rc=0
  two_out="$(rp warm_us "$(printf '4312\n0')" 2>&1)" || two_rc=$?
  check "refuses a two-line all-numeric value (the '<pipeline> || echo 0' shape)" \
    "$([ "$two_rc" -ne 0 ] && echo yes || echo no)" "yes"
  check "  ...naming the field" "$(case "$two_out" in *warm_us*) echo yes ;; *) echo no ;; esac)" "yes"

  rm -rf "$pos_tmpdir"
fi

# And the end-to-end consequence, driven through the REAL verdict(): the string that a
# zeroed warm p50 used to produce must be reachable (non-vacuousness), which is exactly why
# require_positive has to refuse the zero before verdict() ever sees it.
if [ -n "$verdict_body" ]; then
  verdict_snippet2="$(mktemp)"
  printf '%s\n' "$verdict_body" >"$verdict_snippet2"
  zeroed="$(
    # shellcheck disable=SC1090
    . "$verdict_snippet2"
    verdict nested-m8i 0.00 0 0.00
  )"
  case "$zeroed" in *"PROCEED AS DESIGNED"*) zeroed_favourable=yes ;; *) zeroed_favourable=no ;; esac
  check "non-vacuousness: zeroed p50s really do reach section 7.2's most favourable row" \
    "$zeroed_favourable" "yes"
  check "  ...which is why require_positive gates all three inputs to it" \
    "$(grep -cE '^  (warm_us|repl_us|container_ms)="\$\(require_positive ' "$SCRIPT")" "3"
  rm -f "$verdict_snippet2"
fi

# ---------------------------------------------------------------------------
# percentile: a missing/empty input is a refusal, not a zero (review 4001908597 covers
# e11-density.sh's copy; this file has the same helper, feeding the container baseline).
# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# The summary writer, ACTUALLY RUN. Its numeric fields are bare Python interpolations, so
# an empty one (what a rung that never recorded leaves behind) is a SyntaxError, not a 0 --
# which is why require_positive has to refuse before the writer runs, and why the write is
# guarded by [ -s ] afterwards.
# ---------------------------------------------------------------------------
echo "== the run-summary writer, executed with a representative record"

# extract_python_block prints the `python3 -c "..."` block containing $1, from its opening
# line to the closing quote line (which for the summary carries its own `>` redirect, so
# the extracted text writes to $RESULTS exactly as the real script does).
extract_python_block() {
  awk -v marker="$1" '
    !f && $0 == "  python3 -c \"" { f = 1; buf = $0 "\n"; next }
    f {
      buf = buf $0 "\n"
      if ($0 ~ /^"/) { if (index(buf, marker) > 0) { printf "%s", buf; exit } f = 0; buf = "" }
    }
  ' "$SCRIPT"
}

summary_body="$(extract_python_block 'summary = {')"
check "the summary writer is extractable from the real script" \
  "$([ -n "$summary_body" ] && echo yes || echo no)" "yes"

if [ -n "$summary_body" ]; then
  sm_tmpdir="$(mktemp -d)"
  # shellcheck disable=SC2034,SC2317 # all read by the extracted writer, through the eval
  run_summary() {
    (
      RESULTS="$sm_tmpdir"
      SUBSTRATE=nested-m8i
      arms_json='["firecracker"]'
      GOVERNOR_STATE="not exposed"
      ITERS=5 WARMUP=1
      container_ms="$1" warm_ms="$2" repl_ms="$3"
      # The acquire mix the writer also records. Every variable the extracted writer
      # interpolates has to exist in this fixture, or the isolation test fails on the
      # fixture rather than on the writer -- which is how adding these two fields first
      # showed up. The values are the metal smoke pass's real mix (1 warm of 5), which is
      # exactly the shape the verdict guard refuses.
      warm_acq=1 cold_acq=4
      eval "$summary_body"
    )
  }

  # --- NON-VACUOUSNESS: an EMPTY numeric field really does break the writer, against this
  # exact summary shape. That is what a rung whose record was never written leaves behind.
  rm -f "$sm_tmpdir/e10-summary.json"
  sm_bad_rc=0
  sm_bad_err="$(run_summary "" 3.5 12.0 2>&1)" || sm_bad_rc=$?
  check "non-vacuousness: an empty p50 DOES break the summary writer (nonzero)" \
    "$([ "$sm_bad_rc" -ne 0 ] && echo yes || echo no)" "yes"
  case "$sm_bad_err" in *SyntaxError*) sm_syn=yes ;; *) sm_syn=no ;; esac
  check "  ...with a SyntaxError from the bare interpolation" "$sm_syn" "yes"
  check "  ...leaving no summary, which the [ -s ] guard then catches" \
    "$([ -s "$sm_tmpdir/e10-summary.json" ] && echo wrote || echo nothing)" "nothing"

  # --- And with the values require_positive now guarantees, it writes a record json.load
  # accepts, with the governor's "not exposed" fact preserved rather than dropped.
  sm_rc=0
  sm_err="$(run_summary 2.5 3.75 12.0 2>&1)" || sm_rc=$?
  check "a representative summary writes successfully" "$sm_rc" "0"
  check "  ...with no error output" "$sm_err" ""
  check "  ...and json.load parses it" \
    "$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["container_p50_ms"], d["warm_p50_ms"], d["replenishment_cpu_mean_ms_per_restore"], d["governor"])' "$sm_tmpdir/e10-summary.json" 2>&1)" \
    "2.5 3.75 12.0 not exposed"
  rm -rf "$sm_tmpdir"
fi

echo "== percentile refuses an absent measurement instead of printing 0"
pct_body="$(extract_fn percentile || true)"
check "percentile is extractable" "$([ -n "$pct_body" ] && echo yes || echo no)" "yes"

if [ -n "$pct_body" ]; then
  pct_tmpdir="$(mktemp -d)"
  pct_snippet="$pct_tmpdir/pct.sh"
  printf '%s\n' "$pct_body" >"$pct_snippet"

  # Non-vacuousness: the pre-fix pipeline really did produce a two-line value for a missing
  # file under `pipefail` + `|| echo 0` (sort exits 2, awk still prints 0, pipefail
  # propagates sort's status, `|| echo 0` appends a second line).
  prefix_value="$(
    set -uo pipefail
    prefix_percentile() { sort -n "$1" | awk 'END { if (NR == 0) { print 0; exit } }'; }
    prefix_percentile "$pct_tmpdir/never-created" 2>/dev/null || echo 0
  )"
  check "non-vacuousness: the pre-fix form really does yield TWO lines on a missing file" \
    "$(printf '%s\n' "$prefix_value" | wc -l | tr -d ' ')" "2"

  pct_rc=0
  pct_out="$(
    # shellcheck disable=SC1090
    . "$pct_snippet"
    percentile 50 "$pct_tmpdir/never-created" 2>/dev/null
  )" || pct_rc=$?
  check "percentile on a missing file exits nonzero" \
    "$([ "$pct_rc" -ne 0 ] && echo yes || echo no)" "yes"
  check "  ...and prints nothing (never a 0 that becomes the container baseline)" "$pct_out" ""

  : >"$pct_tmpdir/empty"
  pct_e_rc=0
  pct_e_out="$(
    # shellcheck disable=SC1090
    . "$pct_snippet"
    percentile 50 "$pct_tmpdir/empty" 2>/dev/null
  )" || pct_e_rc=$?
  check "percentile on an EMPTY file also refuses" \
    "$([ "$pct_e_rc" -ne 0 ] && echo yes || echo no)" "yes"
  check "  ...printing nothing" "$pct_e_out" ""

  # ...and it still computes the right nearest-rank value, so the refusals are not just
  # "percentile stopped working".
  printf '1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n' >"$pct_tmpdir/ten"
  check "percentile 50 over 1..10 is still 5" \
    "$(
      # shellcheck disable=SC1090
      . "$pct_snippet"
      percentile 50 "$pct_tmpdir/ten"
    )" "5"
  check "percentile 95 over 1..10 is still the nearest-rank 9" \
    "$(
      # shellcheck disable=SC1090
      . "$pct_snippet"
      percentile 95 "$pct_tmpdir/ten"
    )" "9"
  rm -rf "$pct_tmpdir"
fi

echo "== no value-producing helper is left with the '|| echo' two-line shape"
# The whole class, audited rather than the one instance: with `pipefail`, a `|| echo` on a
# PIPELINE that still printed something yields a two-line value. Comments are stripped so
# the explanations of the defect do not count as instances of it.
pipeline_or_echo=$(grep -v '^[[:space:]]*#' "$SCRIPT" | grep -nE '\|[^|]+\|\| *echo' || true)
check "no '<pipeline> || echo' remains anywhere in the driver" \
  "$([ -z "$pipeline_or_echo" ] && echo yes || echo no)" "yes"
check "rung 1's percentile calls die instead of defaulting" \
  "$(grep -c 'percentile .* || echo' "$SCRIPT")" "0"

echo "== rung 1 refuses a baseline assembled from failed Execs"
check "grpc_exec_ms returns the RPC's own status (it used to swallow it)" \
  "$([ "$(grep -c 'return "\$rc"' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "run_rung1 dies on a post-warmup Exec failure" \
  "$([ "$(grep -c 'failed against the container baseline stack' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "run_rung1 also refuses a short steady-state sample" \
  "$([ "$(grep -c 'steady-state samples, wanted' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

# ---------------------------------------------------------------------------
# The EXIT trap (review 4001908613), exercised rather than grepped.
# ---------------------------------------------------------------------------
echo "== an EXIT trap tears rung 1's stack down on every die/exit path"
check "a trap is installed at all (there were zero before)" \
  "$(grep -c '^trap cleanup_on_exit EXIT' "$SCRIPT")" "1"
trap_body="$(extract_fn cleanup_on_exit || true)"
check "cleanup_on_exit is extractable" "$([ -n "$trap_body" ] && echo yes || echo no)" "yes"

if [ -n "$trap_body" ]; then
  tr_tmpdir="$(mktemp -d)"
  tr_probe="$tr_tmpdir/probe.sh"
  tr_order="$tr_tmpdir/order"
  doomed="$tr_tmpdir/doomed"
  mkdir -p "$doomed"
  : >"$doomed/rung1.times"
  {
    echo 'set -uo pipefail'
    echo 'stop_rung1_stack() { echo stopped >>"$ORDER"; }'
    printf '%s\n' "$trap_body"
    echo 'trap cleanup_on_exit EXIT'
    echo 'exit 7'
  } >"$tr_probe"
  tr_rc=0
  ORDER="$tr_order" E10_TMPDIR="$doomed" bash "$tr_probe" || tr_rc=$?
  check "the trap does not swallow the script's exit status" "$tr_rc" "7"
  check "it stops rung 1's stack (kill before remove, as build-snapshot.sh does)" \
    "$(cat "$tr_order")" "stopped"
  check "and it removes the temp root, so no timings file survives a die" \
    "$([ -e "$doomed" ] && echo survived || echo gone)" "gone"

  # Non-vacuousness for the removal: the same exit without the trap leaves the dir behind.
  mkdir -p "$doomed"
  {
    echo 'set -uo pipefail'
    echo 'exit 7'
  } >"$tr_tmpdir/probe-no-trap.sh"
  bash "$tr_tmpdir/probe-no-trap.sh" || true
  check "non-vacuousness: without the trap the same dir survives the same exit" \
    "$([ -e "$doomed" ] && echo survived || echo gone)" "survived"
  rm -rf "$tr_tmpdir"
fi

check "the stack teardown tolerates an EXIT before the pids exist (no unbound variable)" \
  "$(grep -cE '\[ -n "\$\{RUNG1_(WORKER|RELAY)_PID:-\}" \]' "$SCRIPT")" "2"
check "no bare mktemp local is left for the trap to miss" \
  "$(grep -v '^[[:space:]]*#' "$SCRIPT" | grep -cE 'mktemp( -d)? *$|mktemp( -d)?\)')" "0"
check "the temp root itself is created once, at script scope" \
  "$(grep -c '^E10_TMPDIR="\$(mktemp -d' "$SCRIPT")" "1"

echo "== security: the scratch redis is published on loopback, never on all interfaces"
# unbound_publishes prints every `docker run -p <host>:6379` publish in $1 whose host side
# is not explicitly bound to 127.0.0.1. `-p "6380:6379"` binds 0.0.0.0, which on the
# documented rig (an EC2 m8i.xlarge with a public interface) publishes an unauthenticated
# redis to the internet -- a standard host-takeover path via CONFIG SET dir + dbfilename.
unbound_publishes() {
  # Comment lines are stripped first (the same convention this file's VmRSS checks use):
  # the fix's own comments quote the pre-fix `-p "${PORT}:6379"` line by name, and a
  # whole-file grep would flag the explanation of the defect as the defect.
  grep -v '^[[:space:]]*#' "$1" | grep -nE -- '-p +"?[^ "]+:6379' | grep -v '127\.0\.0\.1' || true
}

# Non-vacuousness FIRST: the detector must flag the exact pre-fix line. Without this, an
# empty result below could mean "no publish is unbound" or "the regex matches nothing".
pub_fixture="$(mktemp)"
printf 'docker run --rm -d -p "${RUNG1_REDIS_PORT}:6379" --name x redis:7 >/dev/null\n' >"$pub_fixture"
check "non-vacuousness: the detector DOES flag the pre-fix 0.0.0.0 publish" \
  "$([ -n "$(unbound_publishes "$pub_fixture")" ] && echo yes || echo no)" "yes"
rm -f "$pub_fixture"

check "no docker run publishes 6379 on all interfaces" \
  "$([ -z "$(unbound_publishes "$SCRIPT")" ] && echo yes || echo no)" "yes"
# The complement, so the check above cannot pass merely because the redis went away.
check "the redis publish is explicitly bound to 127.0.0.1" \
  "$([ "$(grep -c -- '-p "127.0.0.1:' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "the redis image is overridable so an operator can pin a digest" \
  "$(grep -c 'SH_E10_REDIS_IMAGE' "$SCRIPT")" "2"
check "redis is started with RDB snapshots disabled (--save '')" \
  "$([ "$(grep -c -- "--save ''" "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo
echo "Total failures: $fails"
exit "$fails"
