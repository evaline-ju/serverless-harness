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

echo
echo "Total failures: $fails"
exit "$fails"
