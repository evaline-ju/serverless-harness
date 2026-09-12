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

echo "== fix-round-2 item A: CLEANUP_JAIL is armed before jail_mount_dev runs"
# A failure between the mount and the arm upgrade (a failing mkfs.ext4, a
# cross-device ln, anything) used to leave only the original `rm -rf "$STAGE"`
# trap active, leaking the /dev/kvm and /dev/urandom bind mounts onto the host.
# Fix-round-7 item 2 replaced the four per-function dynamic `trap '...' EXIT`
# strings with one script-scope CLEANUP_JAIL variable read by a single
# cleanup_on_exit (see that function's own comment for why: an EXIT trap fires
# at PROCESS exit, not function return, and bash pops function locals before a
# mid-function failure's already-armed trap runs, so a trap that named a local
# by reference was never actually safe). The property this test guards --
# "the thing that lets cleanup unmount /dev is set up before the mount, not
# after" -- still has to hold; only the mechanism changed, so the assertion is
# rewritten around CLEANUP_JAIL="$jail" instead of a `trap` line.
#
# Fix-round-9: this arming sequence (CLEANUP_JAIL="$jail" then jail_mount_dev)
# no longer lives in each of the four functions -- it was retyped four times,
# which is exactly the shape of drift that produced fix-rounds 8 and 9's rig
# failures, so it was extracted into the single shared prepare_jail helper (see
# that function's own definition, next to jail_mount_dev). The property now
# needs to hold just once, of prepare_jail's own body, rather than once per
# VMM function; each function's OWN obligation is reduced to "call prepare_jail
# at all", asserted separately below (fix-round-9 section).
start=$(grep -n "^prepare_jail() {" "$SCRIPT" | head -n1 | cut -d: -f1)
ok=no
if [ -n "$start" ]; then
  end=$(awk -v s="$start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
  if [ -n "$end" ]; then
    arm_line=$(awk -v s="$start" -v e="$end" \
      'NR>=s && NR<=e && /CLEANUP_JAIL="\$jail"/{print NR; exit}' "$SCRIPT")
    mount_line=$(awk -v s="$start" -v e="$end" \
      'NR>=s && NR<=e && /jail_mount_dev "\$jail"/{print NR; exit}' "$SCRIPT")
    if [ -n "$arm_line" ] && [ -n "$mount_line" ] && [ "$arm_line" -lt "$mount_line" ]; then
      ok=yes
    fi
  fi
fi
check "prepare_jail sets CLEANUP_JAIL before jail_mount_dev (source order)" "$ok" "yes"

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

# Same discipline as fix-round-2 item A for the /dev bind mounts: cleanup must
# be armed BEFORE the directory is created, not after, so a failure between
# mkdir and go build (or inside the heredoc) still removes it. Fix-round-7 item
# 2 replaced this function's own one-off `trap '...' EXIT` (which embedded its
# local $tmp_pkg literally at arm time -- safe on its own, but one more trap
# idiom alongside the genuinely unsafe pid-referencing ones elsewhere in the
# file) with the same script-scope CLEANUP_EXTRA_DIR read by cleanup_on_exit.
# Source-order check within write_guest_client's own body, the same technique
# used for item A, rewritten around CLEANUP_EXTRA_DIR="$tmp_pkg" instead of a
# `trap` line.
start=$(grep -n "^write_guest_client() {" "$SCRIPT" | head -n1 | cut -d: -f1)
ok=no
if [ -n "$start" ]; then
  end=$(awk -v s="$start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
  if [ -n "$end" ]; then
    arm_line=$(awk -v s="$start" -v e="$end" \
      'NR>=s && NR<=e && /CLEANUP_EXTRA_DIR="\$tmp_pkg"/{print NR; exit}' "$SCRIPT")
    mkdir_line=$(awk -v s="$start" -v e="$end" \
      'NR>=s && NR<=e && /mkdir -p "\$tmp_pkg"/{print NR; exit}' "$SCRIPT")
    if [ -n "$arm_line" ] && [ -n "$mkdir_line" ] && [ "$arm_line" -lt "$mkdir_line" ]; then
      ok=yes
    fi
  fi
fi
check "write_guest_client sets CLEANUP_EXTRA_DIR before mkdir (source order)" "$ok" "yes"

echo "== fix-round-4 item 1: init exports a PATH before execing the agent"
# The kernel hands init an essentially empty environment (no PATH at all). init
# itself never needed one (every command it runs is absolute), but the agent it
# execs resolves the commands IT runs (bash, python3, curl, ...) by name via
# exec.LookPath, and an unset PATH makes every one of those fail even though the
# binary is present in the image -- this is exactly the bug that panicked the
# guest ("exec: \"bash\": executable file not found in \$PATH"). Scope every
# check to the init heredoc itself (between the `cat >.../sbin/init <<'INIT'`
# line and the closing bare `INIT` line), not the outer build script, so this
# can't accidentally pass by matching unrelated text elsewhere.
init_start=$(grep -nF 'cat >"$STAGE/rootfs-tree/sbin/init" <<'"'"'INIT'"'"'' "$SCRIPT" | head -n1 | cut -d: -f1)
init_end=""
if [ -n "$init_start" ]; then
  init_end=$(awk -v s="$init_start" 'NR>s && /^INIT$/{print NR; exit}' "$SCRIPT")
fi
ok=no
path_line=""
exec_line=""
if [ -n "$init_start" ] && [ -n "$init_end" ]; then
  path_line=$(awk -v s="$init_start" -v e="$init_end" \
    'NR>=s && NR<=e && /^export PATH=/{print NR; exit}' "$SCRIPT")
  exec_line=$(awk -v s="$init_start" -v e="$init_end" \
    'NR>=s && NR<=e && /^exec \/usr\/local\/bin\/agent/{print NR; exit}' "$SCRIPT")
  [ -n "$path_line" ] && ok=yes
fi
check "init exports a PATH" "$ok" "yes"

ok=no
if [ -n "$path_line" ]; then
  path_text=$(sed -n "${path_line}p" "$SCRIPT")
  case "$path_text" in
    *:/usr/bin:* | *:/usr/bin)
      case "$path_text" in
        *:/bin:* | *:/bin) ok=yes ;;
      esac
      ;;
  esac
fi
check "init's PATH covers both /usr/bin and /bin (merged-\$usr and split trees)" "$ok" "yes"

ok=no
if [ -n "$path_line" ] && [ -n "$exec_line" ] && [ "$path_line" -lt "$exec_line" ]; then
  ok=yes
fi
check "init's PATH export precedes the agent exec (source order)" "$ok" "yes"

ok=no
if [ -n "$init_start" ] && [ -n "$init_end" ]; then
  exec_check_line=$(awk -v s="$init_start" -v e="$init_end" \
    'NR>=s && NR<=e && /-x \/usr\/local\/bin\/agent/{print NR; exit}' "$SCRIPT")
  if [ -n "$exec_check_line" ] && [ -n "$exec_line" ] && [ "$exec_check_line" -lt "$exec_line" ]; then
    ok=yes
  fi
fi
check "init fails loudly if the agent binary is missing/non-executable, before exec" "$ok" "yes"

echo "== fix-round-4 item 2: a failed wait_for_agent preserves the guest console log"
# wait_for_agent's timeout path used to just print a generic timeout message and
# exit 1 -- and the EXIT trap then deletes \$STAGE, which is where the console
# log (the only artifact that explains a guest boot failure) lives. The failure
# most likely to occur on a new host was erasing its own diagnosis. A
# source-level assertion that the timeout path references and preserves
# \$console_log is legitimate and sufficient here (no live VMM needed).
wfa_start=$(grep -n "^wait_for_agent() {" "$SCRIPT" | head -n1 | cut -d: -f1)
ok=no
timeout_line=""
if [ -n "$wfa_start" ]; then
  wfa_end=$(awk -v s="$wfa_start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
  if [ -n "$wfa_end" ]; then
    timeout_line=$(awk -v s="$wfa_start" -v e="$wfa_end" \
      'NR>=s && NR<=e && /never became reachable/{print NR; exit}' "$SCRIPT")
    save_line=$(awk -v s="$wfa_start" -v e="$wfa_end" \
      'NR>=s && NR<=e && /cp "\$console_log"/{print NR; exit}' "$SCRIPT")
    excerpt_line=$(awk -v s="$wfa_start" -v e="$wfa_end" \
      'NR>=s && NR<=e && /tail -n [0-9]+ "\$console_log"/{print NR; exit}' "$SCRIPT")
    if [ -n "$save_line" ] && [ -n "$excerpt_line" ] && [ -n "$timeout_line" ] \
      && [ "$save_line" -lt "$timeout_line" ] && [ "$excerpt_line" -lt "$timeout_line" ]; then
      ok=yes
    fi
  fi
fi
check "wait_for_agent's timeout path saves and prints the console log before exiting" "$ok" "yes"

ok=no
if [ -n "$wfa_start" ]; then
  wfa_end=$(awk -v s="$wfa_start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
  if [ -n "$wfa_end" ]; then
    saved_var_line=$(awk -v s="$wfa_start" -v e="$wfa_end" \
      'NR>=s && NR<=e && /saved_console=/{print NR; exit}' "$SCRIPT")
    if [ -n "$saved_var_line" ]; then
      saved_var_text=$(sed -n "${saved_var_line}p" "$SCRIPT")
      case "$saved_var_text" in
        *'$STAGE'*) ok=no ;;
        *) ok=yes ;;
      esac
    fi
  fi
fi
check "the saved console-log path is outside \$STAGE (survives the EXIT trap)" "$ok" "yes"

echo "== fix-round-5 item 1: /vm is paused with PATCH, not PUT (Firecracker has no PUT /vm)"
# Confirmed against the real firecracker v1.17.0 binary: PUT /vm returns HTTP 400
# "Invalid request method and/or path: PUT vm.", while PATCH /vm (even before
# the VM is started, so it still fails, but for an unrelated reason) returns "The
# requested operation is not supported before starting the microVM." -- a state
# complaint, not a method/path complaint, proving PATCH is the method the route
# actually accepts. Firecracker's own swagger spec confirms /vm defines no PUT
# method at all; every other api_put call site in the script (/boot-source,
# /drives/{id} pre-boot, /vsock, /machine-config pre-boot, /actions,
# /snapshot/create, /snapshot/load, and cloud-hypervisor's uniformly-PUT
# /api/v1/vm.restore) was individually checked against the spec/docs and is
# already correct, so only the /vm pause call needed to change.
check "api_patch helper exists" \
  "$([ "$(grep -c '^api_patch()' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "the pause call uses api_patch, not api_put" \
  "$([ "$(grep -cE 'api_patch "\$api_sock" /vm ' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "no PUT call against /vm remains anywhere in the script" \
  "$(grep -cE 'api_put "\$api_sock" /vm ' "$SCRIPT")" "0"

echo "== fix-round-5 item 1 (cont'd): api_put and api_patch share one copy of the status-checking logic"
# Generalising api_put to take a method is fine; two independent copies of the
# non-2xx-is-fatal check would let them drift, so both wrappers must funnel
# through a single api_request.
check "api_request is the single shared implementation" \
  "$([ "$(grep -c '^api_request()' "$SCRIPT")" -eq 1 ] && echo yes || echo no)" "yes"
check "the non-2xx-is-fatal status check exists exactly once (not duplicated per verb)" \
  "$([ "$(grep -cF 'returned HTTP $status' "$SCRIPT")" -eq 1 ] && echo yes || echo no)" "yes"
check "api_put is a thin wrapper around api_request" \
  "$([ "$(grep -cF 'api_request PUT "$1" "$2" "$3"' "$SCRIPT")" -eq 1 ] && echo yes || echo no)" "yes"
check "api_patch is a thin wrapper around api_request" \
  "$([ "$(grep -cF 'api_request PATCH "$1" "$2" "$3"' "$SCRIPT")" -eq 1 ] && echo yes || echo no)" "yes"

echo "== fix-round-5 item 2: the EXIT-trap cleanup kills, waits, unmounts, then removes -- in that order"
# A killed process does not release its held file descriptors (including the
# /dev/kvm bind mount) synchronously -- `kill` only requests exit, it does not
# wait for it -- so `wait` must run before jail_unmount_dev can succeed, which
# must in turn run before the jail directory is removed. Fix-round-5 item 2
# made this true of each function's own one-off abnormal-exit trap string;
# fix-round-7 item 2 collapsed all four of those (each unsafe -- see
# cleanup_on_exit's own comment) into ONE cleanup_on_exit function, so the
# property now needs to hold just once, of that one function's body, rather
# than once per VMM function. The round-2/round-3 source-order idiom (comparing
# line numbers within a function's own span) still applies; cleanup_on_exit's
# actions are one per line now rather than packed into a single trap string, so
# this compares LINE order instead of the old same-line COLUMN order.
check "rm_rf_jail helper exists (guarded rm -rf that refuses over a live mount)" \
  "$([ "$(grep -c '^rm_rf_jail()' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "cleanup_on_exit is defined exactly once" \
  "$([ "$(grep -c '^cleanup_on_exit()' "$SCRIPT")" -eq 1 ] && echo yes || echo no)" "yes"
coe_start=$(grep -n "^cleanup_on_exit() {" "$SCRIPT" | head -n1 | cut -d: -f1)
ok=no
if [ -n "$coe_start" ]; then
  coe_end=$(awk -v s="$coe_start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
  if [ -n "$coe_end" ]; then
    kill_line=$(awk -v s="$coe_start" -v e="$coe_end" \
      'NR>=s && NR<=e && /^  kill /{print NR; exit}' "$SCRIPT")
    wait_line=$(awk -v s="$coe_start" -v e="$coe_end" \
      'NR>=s && NR<=e && /^  wait /{print NR; exit}' "$SCRIPT")
    unmount_line=$(awk -v s="$coe_start" -v e="$coe_end" \
      'NR>=s && NR<=e && /jail_unmount_dev/{print NR; exit}' "$SCRIPT")
    rm_line=$(awk -v s="$coe_start" -v e="$coe_end" \
      'NR>=s && NR<=e && /^  rm_rf_jail/{print NR; exit}' "$SCRIPT")
    if [ -n "$kill_line" ] && [ -n "$wait_line" ] && [ -n "$unmount_line" ] && [ -n "$rm_line" ] \
      && [ "$kill_line" -lt "$wait_line" ] && [ "$wait_line" -lt "$unmount_line" ] \
      && [ "$unmount_line" -lt "$rm_line" ]; then
      ok=yes
    fi
  fi
fi
check "cleanup_on_exit kills, waits, unmounts, then removes (in that order)" "$ok" "yes"

echo "== fix-round-5 item 2 (cont'd) / fix-round-7 item 1: rm -rf on \$STAGE (and any extra dir) is routed through the mount-aware guard"
# rm -rf and rm_rf_jail together, over a live mountpoint, are a hazardous pair
# (see the rm_rf_jail comment): today the only bind mounts are device nodes, so
# a failed unmount just makes rm -rf fail loudly, but the shape is one small
# change away (a future directory bind mount) from rm -rf silently recursing
# through the mount and deleting whatever is on the other side of it. Every
# \$STAGE removal in the script should go through the guard. cleanup_on_exit
# builds an array (\$STAGE, plus CLEANUP_EXTRA_DIR -- the verify_root sibling
# of \$OUT introduced by fix-round-7 item 1 -- when one is live) and passes it
# to rm_rf_jail in one call, so the old literal 'rm_rf_jail "\$STAGE"' text no
# longer appears; assert the array-based form instead.
check "no bare 'rm -rf \"\$STAGE\"' remains anywhere in the script" \
  "$(grep -cF 'rm -rf "$STAGE"' "$SCRIPT")" "0"
check "cleanup_on_exit builds its rm targets from \$STAGE" \
  "$([ "$(grep -cF 'rm_targets=("$STAGE")' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "cleanup_on_exit's removal is routed through rm_rf_jail with that array" \
  "$([ "$(grep -cF 'rm_rf_jail "${rm_targets[@]}"' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"

echo "== fix-round-6 item 1: /snapshot/create sends no resume_vm field"
# resume_vm belongs to /snapshot/load's SnapshotLoadParams, not /snapshot/create's
# SnapshotCreateParams -- confirmed against v1.17.0's firecracker.yaml, which lists
# only snapshot_path, mem_file_path, snapshot_type and sync_snapshot_files as valid
# fields on create, and against the real binary's own rejection of this exact
# request on the rig ("unknown field `resume_vm`, expected one of `snapshot_type`,
# `snapshot_path`, `mem_file_path`, `sync_snapshot_files`"). Pull out just the JSON
# body line following the /snapshot/create call so this assertion cannot be
# satisfied by resume_vm merely being absent from some unrelated line/comment.
snapshot_create_body=$(awk '/\/snapshot\/create \\$/{getline; print; exit}' "$SCRIPT")
check "a /snapshot/create call body was found" \
  "$([ -n "$snapshot_create_body" ] && echo yes || echo no)" "yes"
check "the /snapshot/create body contains no resume_vm field" \
  "$(printf '%s' "$snapshot_create_body" | grep -c 'resume_vm')" "0"
check "the /snapshot/create body still sets snapshot_type Full" \
  "$(printf '%s' "$snapshot_create_body" | grep -cF '\"snapshot_type\":\"Full\"')" "1"

echo "== fix-round-6 item 2: /snapshot/load's vsock_override is an object, not a bare string"
# vsock_override is the VsockOverride schema (a JSON object with one required
# property, uds_path), not a plain string -- confirmed against v1.17.0's
# firecracker.yaml and against Firecracker's own docs/vsock.md "Unix Domain Socket
# Renaming" section, whose worked example is
# `"vsock_override": {"uds_path": "./v.sock.2"}`. This call had never executed
# against the real binary before fix-round-6, so unlike item 1 this was caught by
# audit rather than by a rig failure. Pull out just the JSON body line following
# the /snapshot/load call for the same false-positive-avoidance reason as item 1.
snapshot_load_body=$(awk '/\/snapshot\/load \\$/{getline; print; exit}' "$SCRIPT")
check "a /snapshot/load call body was found" \
  "$([ -n "$snapshot_load_body" ] && echo yes || echo no)" "yes"
check "the /snapshot/load body's vsock_override is an object keyed by uds_path" \
  "$(printf '%s' "$snapshot_load_body" | grep -cF '\"vsock_override\":{\"uds_path\":')" "1"
check "the /snapshot/load body's vsock_override is not a bare string" \
  "$(printf '%s' "$snapshot_load_body" | grep -cF '\"vsock_override\":\"')" "0"
check "the /snapshot/load body still sets resume_vm true (valid here, unlike on create)" \
  "$(printf '%s' "$snapshot_load_body" | grep -cF '\"resume_vm\":true')" "1"

echo "== fix-round-7 item 2: an EXIT trap can never reference a function-local (structural fix)"
# Real rig failure: "line 1: fc_pid: unbound variable" -- an EXIT trap fires at
# WHOLE-PROCESS exit, not function return, and bash pops a function's locals
# off as soon as `set -e` unwinds out of its call frame, which for a
# mid-function failure happens BEFORE the trap body (already pointing at that
# now-gone local, by name) gets to run. This bit all four functions that armed
# their own `trap '...' EXIT` referencing a local pid/jail variable (both
# boot_quiesce_snapshot_* arms, both verify_restore_* arms) -- the fix is
# structural: a single trap, once, calling a function that only ever touches
# script-scope globals (never a popped local). These assertions try to catch
# the whole class, not just the one site that failed on the rig: no function
# anywhere in the file may still declare a `local` pid variable of this shape,
# and the three globals cleanup_on_exit depends on must never be shadowed
# `local` by anything (a shadow would silently revive the exact bug: a
# function-local of the same name, invisible to the trap's own copy of the
# global once that function returns -- no, worse, invisible to the *rest of
# the script* the moment such a shadow's frame is popped, same failure mode).
# Excludes comment-only lines throughout this block: the explanatory comments
# above (and the ones cleanup_on_exit itself carries) deliberately quote both
# the old buggy shape and the new fixed shape as prose, e.g. "the single
# `trap cleanup_on_exit EXIT` armed once at the top", which would otherwise
# double-count the real, live statement below.
check "exactly one 'trap ... EXIT' statement exists in the whole script" \
  "$(grep -cE '^[[:space:]]*trap ' "$SCRIPT")" "1"
check "the one remaining trap is 'trap cleanup_on_exit EXIT'" \
  "$([ "$(grep -v '^[[:space:]]*#' "$SCRIPT" | grep -cF 'trap cleanup_on_exit EXIT')" -eq 1 ] && echo yes || echo no)" "yes"
check "cleanup_on_exit function is defined" \
  "$([ "$(grep -c '^cleanup_on_exit()' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "no LIVE 'local fc_pid' declaration remains anywhere in the script" \
  "$(grep -v '^[[:space:]]*#' "$SCRIPT" | grep -cE '\blocal fc_pid\b')" "0"
check "no LIVE 'local ch_pid' declaration remains anywhere in the script" \
  "$(grep -v '^[[:space:]]*#' "$SCRIPT" | grep -cE '\blocal ch_pid\b')" "0"
# Excludes comment-only lines: the explanatory comments above deliberately
# quote the old buggy code shape (e.g. "`trap 'kill "$fc_pid" ...' EXIT`") to
# document what this fixes, so a plain substring grep over the whole file
# would false-positive on the documentation, not the code.
check "no LIVE CODE line references \"\$fc_pid\" (only explanatory comments may)" \
  "$(grep -v '^[[:space:]]*#' "$SCRIPT" | grep -cF '$fc_pid')" "0"
check "no LIVE CODE line references \"\$ch_pid\" (only explanatory comments may)" \
  "$(grep -v '^[[:space:]]*#' "$SCRIPT" | grep -cF '$ch_pid')" "0"
for g in CLEANUP_PID CLEANUP_JAIL CLEANUP_EXTRA_DIR; do
  check "$g is never declared 'local' anywhere (stays script-scope)" \
    "$(grep -cE "local ${g}\b" "$SCRIPT")" "0"
done
check "CLEANUP_PID is assigned by every VMM-launching function" \
  "$([ "$(grep -cF 'CLEANUP_PID=$!' "$SCRIPT")" -eq 4 ] && echo yes || echo no)" "yes"

echo "== fix-round-7 item 1: verify_restore hard-links \$OUT's files via a same-device sibling, not \$STAGE"
# Real rig failure: 'ln: failed to create hard link ... Invalid cross-device
# link' -- \$STAGE lives on tmpfs (a genuine, deliberate speed win for
# assembling the rootfs tree and mkfs'ing the ext4 image, per the coordinator's
# explicit "do not move \$STAGE wholesale" instruction), but \$OUT is normally
# on persistent disk, and ln(1) across two filesystems is always EXDEV, not a
# permissions problem. new_verify_dir/link_snapshot_file move ONLY the
# verify-time jail onto a sibling directory of \$OUT (sharing \$OUT's device),
# leaving \$STAGE itself untouched and still tmpfs for everything else.
check "new_verify_dir helper exists" \
  "$([ "$(grep -c '^new_verify_dir()' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "new_verify_dir allocates its directory under dirname \"\$OUT\" (a sibling of \$OUT)" \
  "$([ "$(grep -cF 'mktemp -d "$base/.build-snapshot-verify.XXXXXX"' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "link_snapshot_file helper exists" \
  "$([ "$(grep -c '^link_snapshot_file()' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "link_snapshot_file attempts a hard link first" \
  "$([ "$(grep -cF 'if ln "$src" "$dst" 2>/dev/null; then' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "link_snapshot_file's fallback is a LOUD warning, not a silent copy" \
  "$([ "$(grep -c 'WARNING: could not hard-link' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "link_snapshot_file's warning names memfile/guest-RAM as the risk, not just \"a file\"" \
  "$([ "$(grep -cF 'ENTIRE guest RAM image' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "link_snapshot_file's fallback copy is cp -p (preserves mode/mtime), after the warning" \
  "$([ "$(grep -cF 'cp -p "$src" "$dst"' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "no bare 'ln \"\$OUT/...' call site remains anywhere in the script" \
  "$(grep -cE 'ln "\$OUT/' "$SCRIPT")" "0"
check "both verify functions route \$OUT's vmstate/memfile/rootfs/ch-config.json through link_snapshot_file" \
  "$([ "$(grep -cF 'link_snapshot_file "$OUT/' "$SCRIPT")" -eq 7 ] && echo yes || echo no)" "yes"
check "both verify jails are rooted under new_verify_dir, not \$STAGE" \
  "$([ "$(grep -cF 'jail="$verify_root/verify-jail"' "$SCRIPT")" -eq 2 ] && echo yes || echo no)" "yes"
check "no verify jail is still rooted at \$STAGE/verify-jail" \
  "$(grep -cF 'jail="$STAGE/verify-jail"' "$SCRIPT")" "0"
check "both verify functions register their verify_root with CLEANUP_EXTRA_DIR" \
  "$([ "$(grep -cF 'CLEANUP_EXTRA_DIR="$verify_root"' "$SCRIPT")" -eq 2 ] && echo yes || echo no)" "yes"
check "both verify functions remove verify_root on their own normal-path teardown too" \
  "$([ "$(grep -cF 'rm_rf_jail "$verify_root"' "$SCRIPT")" -eq 2 ] && echo yes || echo no)" "yes"

echo "== fix-round-8: verify_restore_firecracker makes no boot-resource configuration calls before /snapshot/load"
# Real rig failure: PUT /snapshot/load returned HTTP 400 "Loading a microVM
# snapshot not allowed after configuring boot-specific resources." A restoring
# instance gets exactly ONE configuration call -- /snapshot/load itself, with
# vsock_override standing in for the one restore-time path override
# Firecracker offers. Any PUT /boot-source, /drives/..., /machine-config or
# /vsock issued between starting the VMM and calling /snapshot/load
# reproduces that rejection, no matter that the values being configured
# already match what the snapshot carries.
#
# Comment-collision trap (bit fix-round-7's tests): this file's own comments,
# and build-snapshot.sh's, deliberately quote "/vsock", "vsock_override" and
# "/vsock.sock" while explaining why a bare device PUT is forbidden -- a grep
# across the whole function body would match those explanations, not just
# live code. Strip comment lines first, every time.
restore_fc_body="$(awk '/^verify_restore_firecracker\(\)/{flag=1} flag{print} flag && /^}/{exit}' "$SCRIPT" | grep -v '^[[:space:]]*#')"
check "verify_restore_firecracker starts exactly one VMM (one CLEANUP_PID=\$!)" \
  "$([ "$(printf '%s\n' "$restore_fc_body" | grep -cF 'CLEANUP_PID=$!')" -eq 1 ] && echo yes || echo no)" "yes"
check "verify_restore_firecracker calls /snapshot/load exactly once" \
  "$([ "$(printf '%s\n' "$restore_fc_body" | grep -cF '/snapshot/load')" -eq 1 ] && echo yes || echo no)" "yes"
# The segment strictly between starting the VMM and the /snapshot/load call
# (the sed '$d' drops the /snapshot/load line itself, which is the range's
# terminator, not something "between" the two events).
restore_segment="$(printf '%s\n' "$restore_fc_body" | sed -n '/CLEANUP_PID=\$!/,/\/snapshot\/load/p' | sed '$d')"
check "no PUT /boot-source between starting the VMM and /snapshot/load" \
  "$(printf '%s\n' "$restore_segment" | grep -cF '/boot-source')" "0"
check "no PUT /drives/... between starting the VMM and /snapshot/load" \
  "$(printf '%s\n' "$restore_segment" | grep -cF '/drives/')" "0"
check "no PUT /machine-config between starting the VMM and /snapshot/load" \
  "$(printf '%s\n' "$restore_segment" | grep -cF '/machine-config')" "0"
# Word-boundary, not a bare substring match: the /snapshot/load body itself
# legitimately carries "/vsock.sock" (inside vsock_override's uds_path) and
# that must NOT trip this check -- only a standalone /vsock path argument
# (Firecracker's device-configuration endpoint) should.
check "no PUT /vsock (device config) between starting the VMM and /snapshot/load" \
  "$(printf '%s\n' "$restore_segment" | grep -cE '/vsock([[:space:]]|"|$)')" "0"

echo "== fix-round-9: build-vs-verify jail setup/teardown is shared, not retyped (BUG: CH build never created \$jail/ch-snapshot)"
# Real rig failure: Cloud Hypervisor's own ch-remote refused to snapshot with
# "Destination is not a directory: \"/ch-snapshot\"" -- boot_quiesce_snapshot_
# cloud_hypervisor never created \$jail/ch-snapshot before calling `ch-remote
# ... snapshot "file:///ch-snapshot"`, while verify_restore_cloud_hypervisor
# (which needs the same directory to stage files into before restore) did, via
# its own `mkdir -p "$jail/run" "$jail/ch-snapshot"`. This is the THIRD round
# of build-vs-verify drift the coordinator caught by execution (round 7: EXIT
# traps; round 8: a stray boot-resource PUT copied into verify) -- so instead
# of a fourth point-fix, the jail-prep/teardown sequence common to all four
# functions was extracted into two shared helpers, prepare_jail and
# teardown_jail, so the two cloud-hypervisor functions share ONE call pattern
# for this directory rather than each retyping mkdir independently.
check "prepare_jail helper exists" \
  "$([ "$(grep -c '^prepare_jail()' "$SCRIPT")" -eq 1 ] && echo yes || echo no)" "yes"
check "teardown_jail helper exists" \
  "$([ "$(grep -c '^teardown_jail()' "$SCRIPT")" -eq 1 ] && echo yes || echo no)" "yes"

# prepare_jail's own body: mkdir (the fixed step) must run before the binary is
# staged, which must run before the device mount -- same source-order idiom
# used throughout this file.
pj_start=$(grep -n "^prepare_jail() {" "$SCRIPT" | head -n1 | cut -d: -f1)
ok=no
if [ -n "$pj_start" ]; then
  pj_end=$(awk -v s="$pj_start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
  if [ -n "$pj_end" ]; then
    mkdir_line=$(awk -v s="$pj_start" -v e="$pj_end" \
      'NR>=s && NR<=e && /mkdir -p "\$jail\/run" "\$@"/{print NR; exit}' "$SCRIPT")
    bin_line=$(awk -v s="$pj_start" -v e="$pj_end" \
      'NR>=s && NR<=e && /hardlink_or_copy_bin "\$bin"/{print NR; exit}' "$SCRIPT")
    mount_line=$(awk -v s="$pj_start" -v e="$pj_end" \
      'NR>=s && NR<=e && /jail_mount_dev "\$jail"/{print NR; exit}' "$SCRIPT")
    if [ -n "$mkdir_line" ] && [ -n "$bin_line" ] && [ -n "$mount_line" ] \
      && [ "$mkdir_line" -lt "$bin_line" ] && [ "$bin_line" -lt "$mount_line" ]; then
      ok=yes
    fi
  fi
fi
check "prepare_jail creates directories (incl. any extra dirs) before staging the binary, before mounting /dev" "$ok" "yes"

# The bug fix itself, as a direct regression test: both cloud-hypervisor
# functions' prepare_jail calls must pass "$jail/ch-snapshot" as an extra
# directory. Comment-collision trap: strip comment lines first -- this file's
# and build-snapshot.sh's own explanatory comments now discuss "ch-snapshot"
# and "prepare_jail" extensively in prose.
live_lines="$(grep -v '^[[:space:]]*#' "$SCRIPT")"
check "both cloud-hypervisor functions' prepare_jail calls create \$jail/ch-snapshot" \
  "$([ "$(printf '%s\n' "$live_lines" | grep -cF 'prepare_jail cloud-hypervisor "$jail" "$jail/ch-snapshot"')" -eq 2 ] && echo yes || echo no)" "yes"
check "both firecracker functions' prepare_jail calls take no extra directory" \
  "$([ "$(printf '%s\n' "$live_lines" | grep -cE 'prepare_jail firecracker "\$jail"[[:space:]]*$')" -eq 2 ] && echo yes || echo no)" "yes"
check "prepare_jail is called by exactly the four build/verify functions" \
  "$([ "$(printf '%s\n' "$live_lines" | grep -cE '^[[:space:]]*prepare_jail (firecracker|cloud-hypervisor) ')" -eq 4 ] && echo yes || echo no)" "yes"
check "teardown_jail is called by exactly the four build/verify functions" \
  "$([ "$(printf '%s\n' "$live_lines" | grep -cF 'teardown_jail "$jail"')" -eq 4 ] && echo yes || echo no)" "yes"

# No function should still retype the inline kill/wait/unmount teardown
# sequence -- if one did, it and teardown_jail could drift apart exactly like
# the two build/verify mkdir calls did. Exclude teardown_jail's own definition
# (the one legitimate site) by cutting it out of the search text first.
non_teardown_body="$(awk '/^teardown_jail\(\) \{/{skip=1} skip{if(/^}$/){skip=0}; next} {print}' "$SCRIPT")"
check "no function outside teardown_jail still kills/waits on \$CLEANUP_PID inline" \
  "$(printf '%s\n' "$non_teardown_body" | grep -v '^[[:space:]]*#' | grep -cF 'kill "$CLEANUP_PID"')" "0"
check "no function outside teardown_jail still calls jail_unmount_dev directly" \
  "$(printf '%s\n' "$non_teardown_body" | grep -v '^[[:space:]]*#' | grep -cF 'jail_unmount_dev "$jail"')" "0"

# Source-order regression guard for the fixed bug: within
# boot_quiesce_snapshot_cloud_hypervisor, the prepare_jail call (which now
# creates $jail/ch-snapshot) must precede the `ch-remote ... snapshot` call
# that requires the directory to already exist.
ch_start=$(grep -n "^boot_quiesce_snapshot_cloud_hypervisor() {" "$SCRIPT" | head -n1 | cut -d: -f1)
ok=no
if [ -n "$ch_start" ]; then
  ch_end=$(awk -v s="$ch_start" 'NR>s && /^}$/{print NR; exit}' "$SCRIPT")
  if [ -n "$ch_end" ]; then
    prep_line=$(awk -v s="$ch_start" -v e="$ch_end" \
      'NR>=s && NR<=e && /^  prepare_jail cloud-hypervisor/{print NR; exit}' "$SCRIPT")
    snap_line=$(awk -v s="$ch_start" -v e="$ch_end" \
      'NR>=s && NR<=e && /ch-remote --api-socket "\$api_sock" snapshot/{print NR; exit}' "$SCRIPT")
    if [ -n "$prep_line" ] && [ -n "$snap_line" ] && [ "$prep_line" -lt "$snap_line" ]; then
      ok=yes
    fi
  fi
fi
check "boot_quiesce_snapshot_cloud_hypervisor creates \$jail/ch-snapshot before ch-remote snapshot needs it" "$ok" "yes"

# Deliberate-difference comments the audit called for must actually be present
# at their sites, not just in the fix-round report.
check "a comment explains cloud-hypervisor's lack of ensure_workspace_image (virtio-fs)" \
  "$([ "$(grep -cF 'virtio-fs' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "a comment explains cloud-hypervisor's virtiofsd-served workspace by name" \
  "$([ "$(grep -cF 'virtiofsd' "$SCRIPT")" -ge 1 ] && echo yes || echo no)" "yes"
check "each build function's fresh-boot-only block and workspace-image asymmetry both carry a deliberate-difference comment (2 sites x 2 VMM arms)" \
  "$([ "$(grep -cF 'Deliberate build-vs-verify asymmetry' "$SCRIPT")" -eq 4 ] && echo yes || echo no)" "yes"

if [ "$fails" -eq 0 ]; then echo "PASS"; else echo "FAIL ($fails)"; fi
exit "$fails"
