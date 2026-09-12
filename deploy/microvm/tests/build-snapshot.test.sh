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
for missing in --kernel --rootfs --agent --image; do
  out=$(bash "$SCRIPT" 2>&1)
  case "$out" in *"$missing"*) ok=yes ;; *) ok=no ;; esac
  check "usage names $missing" "$ok" "yes"
done

echo "== it records the instance type rather than leaving it blank"
# Matches the exact manifest field, not the bare substring "instance_type" --
# fix-round-2 item C added detect_host_instance_type/ec2_instance_type, which
# contain that substring too and would otherwise inflate a plain occurrence count.
check "instance-type reaches the manifest" \
  "$([ "$(grep -cF '"instance_type": "$INSTANCE_TYPE"' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== fix-round-2 item C: --instance-type is an auto-detected override, not a required input"
check "--instance-type is optional in usage (bracketed)" \
  "$([ "$(grep -c '\[--instance-type TYPE\]' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "detect_host_instance_type helper exists" \
  "$([ "$(grep -c 'detect_host_instance_type()' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "required-args check no longer ORs in INSTANCE_TYPE" \
  "$(grep -cF -- 'IMAGE" ] || [ -z "$INSTANCE_TYPE" ]' "$SCRIPT")" "0"
out=$(bash "$SCRIPT" --kernel /nonexistent --rootfs /nonexistent --agent /nonexistent --image test 2>&1)
case "$out" in
  *"restore requires identical hardware and software"*) ok=no ;;
  *) ok=yes ;;
esac
check "running with only kernel/rootfs/agent/image (no --instance-type) does not hit usage" "$ok" "yes"
# main() calls require_root before preflight, so non-root CI stops there instead
# of reaching the /dev/kvm check -- either message proves arg validation was
# passed (usage() exits 2 with the text above; both of these exit 1).
case "$out" in
  *"must run as root"* | *"/dev/kvm is not present"*) ok=yes ;;
  *) ok=no ;;
esac
check "it proceeds past arg validation (into require_root/preflight) instead" "$ok" "yes"

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

# Fix-round-2 item B: the old form of this check, `grep -c -- '--cmdline.*pci=off'
# "$SCRIPT"` == "0", was non-discriminating -- pre-fix there was no --cmdline flag
# at ALL, so the grep matched zero times before the fix too, and the check could
# never fail regardless of whether pci=off was actually kept out of the cmdline.
# Rewritten as one coupled assertion about the ACTUAL flag value: the cmdline must
# be present, must contain root=/dev/vda, and must NOT contain pci=off, all three
# together -- this genuinely fails against the pre-fix text (no --cmdline at all).
ch_cmdline="$(grep -oE -- '--cmdline "[^"]*"' "$SCRIPT" | head -n1)"
ok=no
if [ -n "$ch_cmdline" ]; then
  case "$ch_cmdline" in
    *'root=/dev/vda'*)
      case "$ch_cmdline" in
        *'pci=off'*) ok=no ;;
        *) ok=yes ;;
      esac
      ;;
  esac
fi
check "cloud-hypervisor cmdline is present, has root=/dev/vda, and omits pci=off" "$ok" "yes"

echo "== fix-round: cloud-hypervisor restore uses the vm.restore API, not a --restore flag (11th finding)"
check "no --restore CLI flag" \
  "$(grep -cF -- '--restore "' "$SCRIPT")" "0"
check "vm.restore API call is present" \
  "$([ "$(grep -c 'vm.restore' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "config.json is preserved for cloud-hypervisor restores (item 9 corollary)" \
  "$([ "$(grep -c 'ch-config.json' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== fix-round-2 item A: the /dev bind-mount trap is armed before jail_mount_dev runs"
# A failure between the mount and the trap upgrade (a failing mkfs.ext4, a
# cross-device ln, anything) used to leave only the original `rm -rf "$STAGE"`
# trap active, leaking the /dev/kvm and /dev/urandom bind mounts onto the host.
# This can't exercise a live mount without KVM, but asserting the trap-arm line
# precedes the jail_mount_dev call line, by source order, in all four jail-setup
# functions is a legitimate and sufficient check that the leak window is closed.
for fn in boot_quiesce_snapshot_firecracker boot_quiesce_snapshot_cloud_hypervisor \
  verify_restore_firecracker verify_restore_cloud_hypervisor; do
  start=$(grep -n "^${fn}() {" "$SCRIPT" | head -n1 | cut -d: -f1)
  ok=no
  if [ -n "$start" ]; then
    end=$(awk -v s="$start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
    if [ -n "$end" ]; then
      trap_line=$(awk -v s="$start" -v e="$end" \
        'NR>=s && NR<=e && /trap .jail_unmount_dev/{print NR; exit}' "$SCRIPT")
      mount_line=$(awk -v s="$start" -v e="$end" \
        'NR>=s && NR<=e && /jail_mount_dev "\$jail"/{print NR; exit}' "$SCRIPT")
      if [ -n "$trap_line" ] && [ -n "$mount_line" ] && [ "$trap_line" -lt "$mount_line" ]; then
        ok=yes
      fi
    fi
  fi
  check "$fn arms the unmount trap before jail_mount_dev (source order)" "$ok" "yes"
done

echo "== fix-round-3: guest_client.go is generated inside \$AGENT_SRC's module, not \$STAGE"
# Go's internal-package rule keys off the IMPORTING FILE's own directory, not the
# working directory: `cd "$AGENT_SRC" && go build .../$STAGE/guest_client.go` was
# illegal regardless of the cd, because $STAGE sits outside the module tree and
# guest_client.go imports remote-worker/internal/guestagent. A source-level
# assertion is legitimate and sufficient here (no live `go build` needed): the
# generated file's path must be under $AGENT_SRC, and never under $STAGE.
check "guest_client.go is written to a tmp_pkg variable, not directly under \$STAGE" \
  "$([ "$(grep -cF 'cat >"$tmp_pkg/guest_client.go"' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "tmp_pkg is rooted under \$AGENT_SRC (module root), not under \$STAGE" \
  "$([ "$(grep -cE 'tmp_pkg="\$AGENT_SRC/' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "guest_client.go is no longer written under \$STAGE" \
  "$(grep -cF 'cat >"$STAGE/guest_client.go"' "$SCRIPT")" "0"
check "go build no longer reads guest_client.go out of \$STAGE" \
  "$(grep -cF '"$STAGE/guest_client.go"' "$SCRIPT")" "0"

# The temp package directory must be dot-prefixed, or `go build ./...`, `go vet
# ./...` and `gofmt -l .` (which this repo's own checks run against remote-worker)
# would pick up a lingering one and fail in a way that looks unrelated to this
# script.
check "the temp package dir under \$AGENT_SRC is dot-prefixed" \
  "$([ "$(grep -cE 'AGENT_SRC/\.[A-Za-z]' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

# Same discipline as fix-round-2 item A for the /dev bind mounts: the cleanup trap
# must be armed BEFORE the directory is created, not after, so a failure between
# mkdir and go build (or inside the heredoc) still removes it. Source-order check
# within write_guest_client's own body, the same technique used for item A.
start=$(grep -n "^write_guest_client() {" "$SCRIPT" | head -n1 | cut -d: -f1)
ok=no
if [ -n "$start" ]; then
  end=$(awk -v s="$start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
  if [ -n "$end" ]; then
    trap_line=$(awk -v s="$start" -v e="$end" \
      'NR>=s && NR<=e && /trap .rm -rf.*tmp_pkg/{print NR; exit}' "$SCRIPT")
    mkdir_line=$(awk -v s="$start" -v e="$end" \
      'NR>=s && NR<=e && /mkdir -p "\$tmp_pkg"/{print NR; exit}' "$SCRIPT")
    if [ -n "$trap_line" ] && [ -n "$mkdir_line" ] && [ "$trap_line" -lt "$mkdir_line" ]; then
      ok=yes
    fi
  fi
fi
check "write_guest_client arms the temp-dir cleanup trap before mkdir (source order)" "$ok" "yes"

if [ "$fails" -eq 0 ]; then echo "PASS"; else echo "FAIL ($fails)"; fi
exit "$fails"
