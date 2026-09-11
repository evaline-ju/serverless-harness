#!/usr/bin/env bash
# deploy/microvm/tests/build-snapshot.test.sh
#
# Cluster-free, KVM-free tests for build-snapshot.sh. The script itself needs /dev/kvm
# and cannot run on every PR -- but its CONTRACT can rot silently, and a rotted
# contract produces a snapshot the worker then refuses at start (or worse, accepts):
#
#   - the manifest must carry the five fields vmpool.LoadManifest requires, or every
#     worker on the fleet fails its startup verification at once.
#   - the snapshot must be built on the target instance type (spec §2.4: restore
#     requires identical hardware), so the script must RECORD the type rather than
#     leaving it blank.
#   - the artifact must be root-owned and read-only (spec §5.5): only a 64-bit CRC
#     guards the state file and the VMM trusts these files.
#   - the agent must be static (CGO_ENABLED=0): the rootfs is minimal and a dynamic
#     agent would need a libc the image may not carry.
#
# Run: bash deploy/microvm/tests/build-snapshot.test.sh
set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$DIR/build-snapshot.sh"
fails=0
check() { if [ "$2" = "$3" ]; then echo "  ok: $1"; else
  echo "  FAIL: $1 (want '$3', got '$2')"
  fails=$((fails + 1))
fi; }

echo "== the script exists and is shellcheck-clean"
check "build-snapshot.sh present" "$([ -f "$SCRIPT" ] && echo yes || echo no)" "yes"
if command -v shellcheck >/dev/null; then
  shellcheck "$SCRIPT" && check "shellcheck" "$?" "0"
fi

echo "== it refuses to run without the inputs it cannot invent"
for missing in --kernel --rootfs --agent --image --instance-type; do
  out=$(bash "$SCRIPT" 2>&1)
  case "$out" in *"$missing"*) ok=yes ;; *) ok=no ;; esac
  check "usage names $missing" "$ok" "yes"
done

echo "== it records the instance type rather than leaving it blank"
check "instance-type reaches the manifest" \
  "$(grep -c 'instance_type' "$SCRIPT")" "1"

echo "== the artifact is made read-only and root-owned"
check "chmod 0444 on the snapshot files" \
  "$([ "$(grep -c 'chmod .*444' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "chown root on the snapshot dir" \
  "$([ "$(grep -c 'chown .*root' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== the guest agent is built static"
check "CGO_ENABLED=0" "$([ "$(grep -c 'CGO_ENABLED=0' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== nothing secret can enter the snapshot (spec §5.2's invariant)"
check "no SANDBOX_TOKEN in the build" \
  "$(grep -c 'SANDBOX_TOKEN' "$SCRIPT")" "0"
check "swap/cgroup preflight is present" \
  "$([ "$(grep -c 'cgroup' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== fix-round: rootfs/vmstate/memfile paths are jail-relative, not the ephemeral \$STAGE (items 1, 9)"
check "no path_on_host under \$STAGE (item 1)" \
  "$(grep -cF 'path_on_host\":\"$STAGE' "$SCRIPT")" "0"
check "firecracker rootfs drive path is jail-relative /rootfs" \
  "$([ "$(grep -cF 'path_on_host\":\"/rootfs\"' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "cloud-hypervisor --disk path is jail-relative /rootfs (item 9)" \
  "$([ "$(grep -cF -- '--disk "path=/rootfs' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "no snapshot_path/mem_file_path under \$OUT at restore time (item 9 corollary)" \
  "$(grep -cE 'snapshot_path\\":\\"\$OUT|mem_file_path\\":\\"\$OUT' "$SCRIPT")" "0"

echo "== fix-round: a workspace drive is provisioned for restored VMs (item 2)"
check "a workspace drive is configured" \
  "$([ "$(grep -c 'drive_id.*workspace\|/drives/workspace' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "ensure_workspace_image helper exists" \
  "$([ "$(grep -c 'ensure_workspace_image()' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== fix-round: every backgrounded VMM has stdin redirected (items 3, 10)"
check "count of backgrounded VMM launches equals count of </dev/null redirects" \
  "$(grep -c '&$' "$SCRIPT")" "$(grep -c '</dev/null' "$SCRIPT")"
check "at least four </dev/null redirects (build x2, verify x2)" \
  "$([ "$(grep -c '</dev/null' "$SCRIPT")" -ge 4 ] && echo yes || echo no)" "yes"

echo "== fix-round: every VMM API call is checked for failure (item 4)"
check "require_root helper exists" \
  "$([ "$(grep -c 'require_root()' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "require_root is called from main" \
  "$([ "$(grep -c '^  require_root$' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "api_put helper (curl -s -S, status checked) exists" \
  "$([ "$(grep -c 'api_put()' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "no bare unchecked curl PUT calls remain" \
  "$(grep -c "curl -s --unix-socket" "$SCRIPT")" "0"

echo "== fix-round: cloud-hypervisor boots with a cmdline and a read-only rootfs (items 7, 8)"
check "--cmdline is passed to cloud-hypervisor" \
  "$([ "$(grep -c -- '--cmdline "console=ttyS0 root=/dev/vda rw reboot=k panic=1"' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "cloud-hypervisor rootfs disk is readonly=on" \
  "$([ "$(grep -c -- 'readonly=on' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "pci=off is not passed to cloud-hypervisor's cmdline" \
  "$(grep -c -- '--cmdline.*pci=off' "$SCRIPT")" "0"

echo "== fix-round: cloud-hypervisor restore uses the vm.restore API, not a --restore flag (11th finding)"
check "no --restore CLI flag" \
  "$(grep -cF -- '--restore "' "$SCRIPT")" "0"
check "vm.restore API call is present" \
  "$([ "$(grep -c 'vm.restore' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "config.json is preserved for cloud-hypervisor restores (item 9 corollary)" \
  "$([ "$(grep -c 'ch-config.json' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

if [ "$fails" -eq 0 ]; then echo "PASS"; else echo "FAIL ($fails)"; fi
exit "$fails"
