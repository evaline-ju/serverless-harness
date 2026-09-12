#!/usr/bin/env bash
# deploy/microvm/tests/systemd-units.test.sh
#
# Cluster-free, root-free, systemd-free tests for microvm-worker.service and
# microvm-vms.slice (Task 17). Neither unit can be exercised for real here -- that
# needs systemctl, a real host, and root -- but the unit FILES' contract can rot
# silently the same way build-snapshot.test.sh's manifest contract can, and a rotted
# contract reintroduces spec §6's #1 practical failure (a worker crash leaks VMs)
# without any test ever going red:
#
#   - KillMode=control-group is the half of the crash-leak mitigation systemd itself
#     provides: on stop/crash it kills every process left in the unit's cgroup, VM
#     processes included, not just the worker's own pid.
#   - Slice=microvm-vms.slice is what makes that cgroup the SAME one SweepOrphans
#     walks on the next start, and the same one both launcher arms' per-VM cgroups
#     (jailer --parent-cgroup / systemd-run --scope --slice=) nest under
#     (hardware-corrections D1/D3: "configured consistently ... or the two
#     mechanisms fight and the leak we are preventing returns").
#   - AssertPathExists=/dev/kvm fails the unit before ExecStart even runs -- spec §6:
#     "fail the unit at start ... never fall back to running commands on the host."
#   - LimitMEMLOCK=infinity keeps the unit's own ceiling from being the thing that
#     silently caps VM density (spec §7.5: a kernel limit mistaken for a density
#     ceiling fails at 500 VMs after working at 20).
#   - SH_MAX_COMMITTED_MB must be set and non-zero: main.go's poolConfig refuses to
#     start without it (the memory gate is mandatory, not a default).
#   - MemorySwapMax=0 on the slice keeps a ballooning guest's OOM kill fast and
#     attributable (in-cgroup) instead of a slow host-wide swap thrash.
#   - No SANDBOX_TOKEN= literal in either file: spec §5.2's invariant that nothing
#     secret enters a checked-in artifact, mirroring build-snapshot.test.sh's
#     identical check on the snapshot build script.
#
# Run: bash deploy/microvm/tests/systemd-units.test.sh
set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVICE="$DIR/microvm-worker.service"
SLICE="$DIR/microvm-vms.slice"
fails=0
check() { if [ "$2" = "$3" ]; then echo "  ok: $1"; else
  echo "  FAIL: $1 (want '$3', got '$2')"
  fails=$((fails + 1))
fi; }

echo "== both unit files exist"
check "microvm-worker.service present" "$([ -f "$SERVICE" ] && echo yes || echo no)" "yes"
check "microvm-vms.slice present" "$([ -f "$SLICE" ] && echo yes || echo no)" "yes"

if command -v systemd-analyze >/dev/null && systemd-analyze verify "$SERVICE" >/dev/null 2>&1; then
  check "systemd-analyze verify microvm-worker.service" "0" "0"
else
  echo "  (skip: systemd-analyze unavailable or non-Linux -- not run, not claimed verified)"
fi

echo "== the crash-leak mitigation itself (spec §6's #1 practical failure)"
check "KillMode=control-group is set" \
  "$([ "$(grep -cF 'KillMode=control-group' "$SERVICE")" -ge 1 ] && echo yes || echo no)" "yes"
check "Slice=microvm-vms.slice is set" \
  "$([ "$(grep -cF 'Slice=microvm-vms.slice' "$SERVICE")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== fail the unit at start, never fall back to the host (spec §6)"
check "AssertPathExists=/dev/kvm is set" \
  "$([ "$(grep -cF 'AssertPathExists=/dev/kvm' "$SERVICE")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== kernel limits raised deliberately, not discovered at 500 VMs (spec §7.5)"
check "LimitMEMLOCK=infinity is set" \
  "$([ "$(grep -cF 'LimitMEMLOCK=infinity' "$SERVICE")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== the memory gate is present and non-zero (poolConfig refuses to start without it)"
sh_max_committed="$(grep -oE 'SH_MAX_COMMITTED_MB=[0-9]+' "$SERVICE" | head -n1 | cut -d= -f2)"
check "SH_MAX_COMMITTED_MB is set" "$([ -n "$sh_max_committed" ] && echo yes || echo no)" "yes"
check "SH_MAX_COMMITTED_MB is non-zero" "$([ -n "$sh_max_committed" ] && [ "$sh_max_committed" -gt 0 ] && echo yes || echo no)" "yes"

echo "== the slice's swap posture (spec §6 mitigation #3: fast, attributable in-cgroup OOM kill)"
check "MemorySwapMax=0 is set on the slice" \
  "$([ "$(grep -cF 'MemorySwapMax=0' "$SLICE")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== nothing secret can enter a checked-in unit file (spec §5.2's invariant)"
check "no SANDBOX_TOKEN= in microvm-worker.service" \
  "$(grep -cF 'SANDBOX_TOKEN=' "$SERVICE")" "0"
check "no SANDBOX_TOKEN= in microvm-vms.slice" \
  "$(grep -cF 'SANDBOX_TOKEN=' "$SLICE")" "0"

echo "== D8: no name-based pkill/pgrep -x cloud-hypervisor trap (hardware-corrections D8)"
# The 15-char comm-truncation trap makes 'pkill -x cloud-hypervisor' a silent no-op.
# These unit files should not contain any such invocation at all -- the kill path here
# is systemd's own KillMode=control-group, not a name match.
check "no pkill/pgrep -x cloud-hypervisor in the service unit" \
  "$(grep -cE 'p(kill|grep) .*-x cloud-hypervisor' "$SERVICE")" "0"
check "no pkill/pgrep -x cloud-hypervisor in the slice unit" \
  "$(grep -cE 'p(kill|grep) .*-x cloud-hypervisor' "$SLICE")" "0"

echo "== the slice and unit agree on the SAME slice name (hardware-corrections D1/D3)"
check "SH_PARENT_CGROUP path ends in the slice this service is placed in" \
  "$([ "$(grep -oE 'SH_PARENT_CGROUP=.*' "$SERVICE" | grep -cF 'microvm-vms.slice')" -ge 1 ] && echo yes || echo no)" "yes"

if [ "$fails" -eq 0 ]; then echo "PASS"; else echo "FAIL ($fails)"; fi
exit "$fails"
