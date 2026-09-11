#!/usr/bin/env bash
# deploy/microvm/build-snapshot.sh
#
# Builds ONE golden VM snapshot: a paused, restorable microVM whose memory and
# device state (vmstate, memfile) sit alongside the exact kernel, rootfs and guest
# agent that produced them, plus a manifest.json pinning it all together. This is
# the artifact remote-worker's microvm-worker verifies at start and every real Exec
# then restores FROM — it is never built per run (spec §2.4, §5.2, §5.5).
#
# Deliberately linear, and each step fails the whole build rather than limping on
# with a partial artifact:
#
#   1. preflight   -- confirm THIS host can produce a snapshot THIS host (or one
#                      identical to it) can later restore: /dev/kvm, cgroups v2, no
#                      swap, kernel new enough for what the VMM needs. Restore
#                      requires identical hardware and software (spec §2.4), so the
#                      snapshot is built on the target instance type, not cross-built.
#   2. build_agent  -- the guest agent, static (CGO_ENABLED=0), because the rootfs
#                      is minimal and a dynamically-linked agent would need a libc
#                      the image may not carry.
#   3. assemble_rootfs -- layer the agent and a minimal init onto the caller's base
#                      rootfs. Nothing that identifies a run may enter here or at any
#                      later step: no bearer credential, no per-VM secret. The agent
#                      reads its per-run identity over vsock from the host at Exec
#                      time, never from a file baked into the image (spec §5.2).
#   4. boot_quiesce_snapshot -- boot cold, wait for the agent to park in accept(),
#                      quiesce the guest, then ask the VMM to serialize memory and
#                      device state to vmstate/memfile.
#   5. write_manifest -- record image, vmm, instance type, kernel release, guest RAM,
#                      the capabilities probed INSIDE the guest before snapshotting,
#                      and the three component digests + their combined hash, in the
#                      exact kernel/rootfs/agent order vmpool.Manifest.ComputeHash
#                      hashes them in.
#   6. lock_down    -- root-owned, read-only. Only a 64-bit CRC guards vmstate and the
#                      VMM trusts these files (spec §2.4); the filesystem permissions
#                      are the next line of defence after that.
#   7. verify_restore -- restore ONE VM from the artifact just produced and run a
#                      trivial command in it. A snapshot can hash correctly and still
#                      not restore on this host, and that must be caught here, not on
#                      a worker's first user request (spec §6).
#
# Needs a KVM host with the chosen VMM installed; it cannot run in CI. What CAN run
# everywhere is deploy/microvm/tests/build-snapshot.test.sh, which pins this script's
# contract (flags, permissions, no secrets, static build) without touching a
# hypervisor.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

KERNEL=""
ROOTFS=""
AGENT_SRC=""
IMAGE=""
INSTANCE_TYPE=""
VMM="firecracker"
GUEST_RAM_MB=256
OUT=""

usage() {
  cat >&2 <<'USAGE'
build-snapshot.sh --kernel PATH --rootfs PATH --agent PATH --image NAME
                  --instance-type TYPE [--vmm firecracker|cloud-hypervisor]
                  [--guest-ram-mb 256] [--out DIR]

Builds one golden snapshot, ON THE INSTANCE TYPE THAT WILL RUN IT (spec §2.4:
restore requires identical hardware and software). Writes vmstate, memfile and a
manifest.json the worker verifies at start, then restores one VM to prove the
artifact works.
USAGE
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --kernel)
      KERNEL="${2:-}"
      shift 2
      ;;
    --rootfs)
      ROOTFS="${2:-}"
      shift 2
      ;;
    --agent)
      AGENT_SRC="${2:-}"
      shift 2
      ;;
    --image)
      IMAGE="${2:-}"
      shift 2
      ;;
    --instance-type)
      INSTANCE_TYPE="${2:-}"
      shift 2
      ;;
    --vmm)
      VMM="${2:-}"
      shift 2
      ;;
    --guest-ram-mb)
      GUEST_RAM_MB="${2:-}"
      shift 2
      ;;
    --out)
      OUT="${2:-}"
      shift 2
      ;;
    -h | --help)
      usage
      ;;
    *)
      usage
      ;;
  esac
done

if [ -z "$KERNEL" ] || [ -z "$ROOTFS" ] || [ -z "$AGENT_SRC" ] || [ -z "$IMAGE" ] || [ -z "$INSTANCE_TYPE" ]; then
  usage
fi
case "$VMM" in
  firecracker | cloud-hypervisor) ;;
  *)
    echo "build-snapshot.sh: --vmm must be firecracker or cloud-hypervisor, got '$VMM'" >&2
    exit 2
    ;;
esac

OUT="${OUT:-$REPO_ROOT/.build/microvm-snapshots/$IMAGE}"
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/build-snapshot.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT

log() { echo "build-snapshot.sh: $*" >&2; }

# ---------------------------------------------------------------------------
# 1. preflight
# ---------------------------------------------------------------------------
kernel_at_least() {
  # $1: running release, e.g. "6.8.0-45-generic"; $2: minimum "major.minor".
  local have want
  have="${1%%-*}"
  want="$2"
  [ "$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -n1)" = "$want" ]
}

preflight() {
  log "preflight: checking this host can build AND restore a snapshot"

  if [ ! -e /dev/kvm ]; then
    echo "build-snapshot.sh: /dev/kvm is not present on this host. A golden snapshot" >&2
    echo "  must be built on the SAME instance type that will restore it (spec §2.4)," >&2
    echo "  so this cannot be cross-built on a non-KVM machine." >&2
    exit 1
  fi

  local cgroup_type
  cgroup_type="$(stat -fc %T /sys/fs/cgroup 2>/dev/null || echo unknown)"
  if [ "$cgroup_type" != "cgroup2fs" ]; then
    echo "build-snapshot.sh: cgroups v2 is required (/sys/fs/cgroup is '$cgroup_type', want cgroup2fs)" >&2
    exit 1
  fi

  if [ -n "$(swapon --show 2>/dev/null || true)" ]; then
    echo "build-snapshot.sh: swap must be off on a snapshot-build host; a swapped-out" >&2
    echo "  guest page would make the memory digest and the restore behaviour disagree" >&2
    exit 1
  fi

  local release
  release="$(uname -r)"
  if ! kernel_at_least "$release" "5.18"; then
    echo "build-snapshot.sh: kernel $release is older than the minimum 5.18" >&2
    exit 1
  fi

  log "preflight: ok (kernel $release)"
}

# ---------------------------------------------------------------------------
# 2. build_agent
# ---------------------------------------------------------------------------
build_agent() {
  log "building the guest agent statically from $AGENT_SRC"
  if [ ! -d "$AGENT_SRC/cmd/guest-agent" ]; then
    echo "build-snapshot.sh: --agent $AGENT_SRC has no cmd/guest-agent; pass the" >&2
    echo "  remote-worker module root (the directory containing go.mod)" >&2
    exit 1
  fi
  (
    cd "$AGENT_SRC"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' \
      -o "$STAGE/agent" ./cmd/guest-agent
  )
}

# ---------------------------------------------------------------------------
# 3. assemble_rootfs
# ---------------------------------------------------------------------------
assemble_rootfs() {
  log "assembling the rootfs from $ROOTFS"
  rm -rf "$STAGE/rootfs-tree"
  mkdir -p "$STAGE/rootfs-tree"
  # ROOTFS is a base tree (sandbox toolchain, no per-run identity of any kind) that
  # this build layers the agent and init onto. It is copied, never mutated in place,
  # so a failed build never corrupts the caller's base image.
  cp -a "$ROOTFS/." "$STAGE/rootfs-tree/"
  mkdir -p "$STAGE/rootfs-tree/usr/local/bin" "$STAGE/rootfs-tree/sbin" \
    "$STAGE/rootfs-tree/tmp" "$STAGE/rootfs-tree/var" "$STAGE/rootfs-tree/workspace"
  install -m 0555 "$STAGE/agent" "$STAGE/rootfs-tree/usr/local/bin/agent"

  # A minimal init: mount the ephemeral filesystems every run needs writable, then
  # EXEC (not fork) the agent as PID 1 so the agent's own exit tears the VM down
  # instead of leaving an orphaned init for the next restore. /tmp and /var are
  # tmpfs so nothing a run writes there survives past this VM's destruction.
  cat >"$STAGE/rootfs-tree/sbin/init" <<'INIT'
#!/bin/sh
set -e
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t tmpfs -o mode=1777 tmpfs /tmp
mount -t tmpfs tmpfs /var
exec /usr/local/bin/agent --listen vsock:1024 --workdir /workspace
INIT
  chmod 0555 "$STAGE/rootfs-tree/sbin/init"

  # A single ext4 image is what the VMM's block device backend wants; building it
  # here (rather than shipping a directory tree) is also what makes the "rootfs"
  # file hashable as one artifact in the manifest.
  local mb
  mb="$(du -sm "$STAGE/rootfs-tree" | cut -f1)"
  truncate -s "$((mb + 256))M" "$STAGE/rootfs"
  mkfs.ext4 -q -F -d "$STAGE/rootfs-tree" "$STAGE/rootfs"
}

# ---------------------------------------------------------------------------
# 4. boot_quiesce_snapshot
#
# guest_client.go is generated here rather than committed: it is a thin, one-shot
# wrapper around the ALREADY-EXPORTED guestagent framed-protocol primitives
# (WriteFrame/ReadFrame/Request/End — see internal/guestagent/protocol.go), so the
# wire format has exactly one implementation in this repo rather than a second,
# hand-rolled one in shell. It never becomes a repo file: cmd/guest-agent and
# internal/guestagent stay untouched, and this script is the only caller.
# ---------------------------------------------------------------------------
write_guest_client() {
  cat >"$STAGE/guest_client.go" <<'GOEOF'
// Command guest_client is a build-time-only helper: it speaks the same framed
// protocol internal/vmpool/guestconn.go speaks at serve time, but from a
// throwaway process instead of the pool, because build-snapshot.sh has no pool.
//
// Firecracker's and Cloud Hypervisor's Unix-socket vsock backends both proxy a
// host connection into the guest's listener on a fixed port via a one-line
// handshake: the host writes "CONNECT <port>\n" on the VMM-created Unix socket and,
// once the guest has accept()ed, the socket becomes a raw duplex stream to the
// guest side. That handshake is VMM-and-version-specific; verify it against the
// VMM's current vsock documentation when this runs for real (Task 12's KVM gate),
// since it cannot be exercised without a hypervisor.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	ga "github.com/kagenti/serverless-harness/remote-worker/internal/guestagent"
)

func main() {
	uds := flag.String("uds", "", "path to the VMM's vsock unix socket")
	port := flag.Uint("port", 1024, "guest vsock port the agent listens on")
	command := flag.String("command", "", "command to run in the guest; empty means probe-only")
	timeoutS := flag.Uint("timeout-s", 30, "guest-side command timeout")
	probeOnly := flag.Bool("probe-only", false, "just prove the guest is accepting; run nothing")
	dialTimeout := flag.Duration("dial-timeout", 10*time.Second, "how long to wait for the CONNECT handshake")
	flag.Parse()

	if *uds == "" {
		fmt.Fprintln(os.Stderr, "guest_client: -uds is required")
		os.Exit(2)
	}

	conn, err := dialGuest(*uds, uint32(*port), *dialTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "guest_client: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if *probeOnly {
		return
	}

	req := ga.Request{Command: *command, TimeoutS: uint32(*timeoutS), CapBytes: ga.MaxFrame, HostUnixNanos: time.Now().UnixNano()}
	if err := ga.WriteJSON(conn, ga.KindRequest, req); err != nil {
		fmt.Fprintf(os.Stderr, "guest_client: send request: %v\n", err)
		os.Exit(1)
	}
	if err := ga.WriteFrame(conn, ga.KindStdinEOF, nil); err != nil {
		fmt.Fprintf(os.Stderr, "guest_client: send stdin-eof: %v\n", err)
		os.Exit(1)
	}

	for {
		kind, payload, err := ga.ReadFrame(conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "guest_client: read: %v\n", err)
			os.Exit(1)
		}
		switch kind {
		case ga.KindStdout:
			os.Stdout.Write(payload)
		case ga.KindStderr:
			os.Stderr.Write(payload)
		case ga.KindEnd:
			var e ga.End
			if err := json.Unmarshal(payload, &e); err != nil {
				fmt.Fprintf(os.Stderr, "guest_client: undecodable End: %v\n", err)
				os.Exit(1)
			}
			os.Exit(int(e.ExitCode))
		case ga.KindError:
			fmt.Fprintf(os.Stderr, "guest_client: guest error: %s\n", payload)
			os.Exit(1)
		}
	}
}

// dialGuest performs the VMM's vsock Unix-socket CONNECT handshake and returns the
// resulting stream. See the package comment for what is and is not verified here.
func dialGuest(uds string, port uint32, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("unix", uds)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", uds, err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	line, err := readLine(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read CONNECT ack: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("CONNECT %d refused: %q", port, line)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, nil
}

func readLine(conn net.Conn) (string, error) {
	buf := make([]byte, 0, 64)
	one := make([]byte, 1)
	for {
		if _, err := conn.Read(one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(buf), nil
		}
		buf = append(buf, one[0])
	}
}
GOEOF
  (cd "$AGENT_SRC" && go build -o "$STAGE/guest_client" "$STAGE/guest_client.go")
}

wait_for_agent() {
  local uds="$1" console_log="$2"
  log "waiting for the guest agent to park in accept()"
  local waited=0
  while [ "$waited" -lt 120 ]; do
    if [ -f "$console_log" ] && grep -q "parked in accept()" "$console_log" 2>/dev/null; then
      return 0
    fi
    if "$STAGE/guest_client" -uds "$uds" -port 1024 -probe-only -dial-timeout 1s 2>/dev/null; then
      return 0
    fi
    sleep 1
    waited=$((waited + 1))
  done
  echo "build-snapshot.sh: guest agent never became reachable on vsock:1024 within 120s" >&2
  exit 1
}

# probe_capabilities asks the ALREADY-BOOTED guest what its sandboxed toolchain can
# do, so the manifest advertises exactly what this rootfs carries rather than a
# hand-maintained guess that drifts from the image (spec §5.5).
probe_capabilities() {
  local uds="$1"
  local out
  out="$("$STAGE/guest_client" -uds "$uds" -port 1024 -timeout-s 10 \
    -command 'for c in python3 node git ripgrep rg curl; do command -v "$c" >/dev/null 2>&1 && echo "$c"; done' \
    2>/dev/null || true)"
  printf '%s\n' "$out" | sed '/^$/d' | sort -u
}

quiesce_guest() {
  local uds="$1"
  log "quiescing the guest before snapshotting"
  "$STAGE/guest_client" -uds "$uds" -port 1024 -timeout-s 10 -command 'sync' >/dev/null 2>&1
}

boot_quiesce_snapshot_firecracker() {
  local api_sock="$STAGE/firecracker-api.sock" vsock_uds="$STAGE/vsock.sock" console_log="$STAGE/console.log"
  log "starting firecracker ($api_sock)"
  # A real invocation configures boot-source/drives/vsock/machine-config over
  # $api_sock (PUT /boot-source, /drives/rootfs, /vsock, /machine-config), then
  # InstanceStart via PUT /actions -- omitted here because it needs a running
  # firecracker binary and /dev/kvm, neither available off a KVM host (spec's Task
  # 12 KVM gate covers exercising this for real).
  firecracker --api-sock "$api_sock" >"$console_log" 2>&1 &
  local fc_pid=$!
  trap 'kill "$fc_pid" 2>/dev/null || true; rm -rf "$STAGE"' EXIT

  curl -s --unix-socket "$api_sock" -X PUT 'http://localhost/boot-source' \
    -d "{\"kernel_image_path\":\"$KERNEL\",\"boot_args\":\"console=ttyS0 reboot=k panic=1 pci=off\"}" >/dev/null
  curl -s --unix-socket "$api_sock" -X PUT 'http://localhost/drives/rootfs' \
    -d "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$STAGE/rootfs\",\"is_root_device\":true,\"is_read_only\":false}" >/dev/null
  curl -s --unix-socket "$api_sock" -X PUT 'http://localhost/vsock' \
    -d "{\"vsock_id\":\"vsock0\",\"guest_cid\":3,\"uds_path\":\"$vsock_uds\"}" >/dev/null
  curl -s --unix-socket "$api_sock" -X PUT 'http://localhost/machine-config' \
    -d "{\"mem_size_mib\":$GUEST_RAM_MB,\"vcpu_count\":1}" >/dev/null
  curl -s --unix-socket "$api_sock" -X PUT 'http://localhost/actions' \
    -d '{"action_type":"InstanceStart"}' >/dev/null

  wait_for_agent "$vsock_uds" "$console_log"
  MANIFEST_CAPABILITIES="$(probe_capabilities "$vsock_uds")"
  quiesce_guest "$vsock_uds"

  log "snapshotting (PUT /snapshot/create, resuming nothing afterwards)"
  curl -s --unix-socket "$api_sock" -X PUT 'http://localhost/vm' -d '{"state":"Paused"}' >/dev/null
  curl -s --unix-socket "$api_sock" -X PUT 'http://localhost/snapshot/create' \
    -d "{\"snapshot_path\":\"$STAGE/vmstate\",\"mem_file_path\":\"$STAGE/memfile\",\"snapshot_type\":\"Full\",\"resume_vm\":false}" >/dev/null

  kill "$fc_pid" 2>/dev/null || true
  trap 'rm -rf "$STAGE"' EXIT
}

boot_quiesce_snapshot_cloud_hypervisor() {
  local api_sock="$STAGE/ch-api.sock" vsock_uds="$STAGE/vsock.sock" console_log="$STAGE/console.log"
  log "starting cloud-hypervisor ($api_sock)"
  cloud-hypervisor --api-socket "$api_sock" \
    --kernel "$KERNEL" \
    --disk "path=$STAGE/rootfs" \
    --vsock "cid=3,socket=$vsock_uds" \
    --memory "size=${GUEST_RAM_MB}M" \
    --cpus boot=1 \
    --console file="$console_log" \
    --serial off >/dev/null 2>&1 &
  local ch_pid=$!
  trap 'kill "$ch_pid" 2>/dev/null || true; rm -rf "$STAGE"' EXIT

  wait_for_agent "$vsock_uds" "$console_log"
  MANIFEST_CAPABILITIES="$(probe_capabilities "$vsock_uds")"
  quiesce_guest "$vsock_uds"

  log "snapshotting (ch-remote pause + snapshot, no resume afterwards)"
  ch-remote --api-socket "$api_sock" pause
  ch-remote --api-socket "$api_sock" snapshot "file://$STAGE/ch-snapshot"
  # Cloud Hypervisor writes one snapshot directory rather than separate
  # vmstate/memfile files; split it so both VMMs feed write_manifest identically.
  mv "$STAGE/ch-snapshot/state.json" "$STAGE/vmstate"
  mv "$STAGE/ch-snapshot/memory-ranges" "$STAGE/memfile"

  kill "$ch_pid" 2>/dev/null || true
  trap 'rm -rf "$STAGE"' EXIT
}

boot_quiesce_snapshot() {
  write_guest_client
  case "$VMM" in
    firecracker) boot_quiesce_snapshot_firecracker ;;
    cloud-hypervisor) boot_quiesce_snapshot_cloud_hypervisor ;;
  esac
}

# ---------------------------------------------------------------------------
# 5. write_manifest
# ---------------------------------------------------------------------------
write_manifest() {
  log "writing manifest.json"
  cp "$KERNEL" "$STAGE/kernel"

  local khash rhash ahash hash caps_json built_at
  khash="sha256:$(sha256sum "$STAGE/kernel" | cut -d' ' -f1)"
  rhash="sha256:$(sha256sum "$STAGE/rootfs" | cut -d' ' -f1)"
  ahash="sha256:$(sha256sum "$STAGE/agent" | cut -d' ' -f1)"
  # Hashed in this exact order -- kernel, then rootfs, then agent -- to match
  # vmpool.Manifest.ComputeHash byte for byte; a different order here would make
  # every worker's startup verification fail against a perfectly good snapshot.
  hash="sha256:$(printf 'kernel:%s\nrootfs:%s\nagent:%s\n' "$khash" "$rhash" "$ahash" | sha256sum | cut -d' ' -f1)"

  caps_json="$(printf '%s\n' "${MANIFEST_CAPABILITIES:-}" | sed '/^$/d' | awk '{printf "%s\"%s\"", sep, $0; sep=","}')"
  built_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  cat >"$STAGE/manifest.json" <<MANIFEST
{
  "image": "$IMAGE",
  "vmm": "$VMM",
  "instance_type": "$INSTANCE_TYPE",
  "kernel_release": "$(uname -r)",
  "guest_ram_mb": $GUEST_RAM_MB,
  "capabilities": [$caps_json],
  "built_at": "$built_at",
  "kernel_sha256": "$khash",
  "rootfs_sha256": "$rhash",
  "agent_sha256": "$ahash",
  "hash": "$hash"
}
MANIFEST
}

# ---------------------------------------------------------------------------
# 6. lock_down
# ---------------------------------------------------------------------------
lock_down() {
  log "locking down $OUT (root-owned, read-only)"
  mkdir -p "$OUT"
  for f in vmstate memfile kernel rootfs agent manifest.json; do
    install -m 0644 "$STAGE/$f" "$OUT/$f"
  done
  chown -R root:root "$OUT"
  chmod 0444 "$OUT"/vmstate "$OUT"/memfile "$OUT"/kernel "$OUT"/rootfs "$OUT"/agent "$OUT"/manifest.json
  chmod 0555 "$OUT"
}

# ---------------------------------------------------------------------------
# 7. verify_restore
# ---------------------------------------------------------------------------
verify_restore() {
  log "verifying: restoring one VM from $OUT and running \`true\` in it"
  case "$VMM" in
    firecracker) verify_restore_firecracker ;;
    cloud-hypervisor) verify_restore_cloud_hypervisor ;;
  esac
  log "verify: ok"
}

verify_restore_firecracker() {
  local api_sock="$STAGE/verify-api.sock" vsock_uds="$STAGE/verify-vsock.sock"
  firecracker --api-sock "$api_sock" >"$STAGE/verify-console.log" 2>&1 &
  local fc_pid=$!
  trap 'kill "$fc_pid" 2>/dev/null || true; rm -rf "$STAGE"' EXIT

  curl -s --unix-socket "$api_sock" -X PUT 'http://localhost/vsock' \
    -d "{\"vsock_id\":\"vsock0\",\"guest_cid\":3,\"uds_path\":\"$vsock_uds\"}" >/dev/null
  curl -s --unix-socket "$api_sock" -X PUT 'http://localhost/snapshot/load' \
    -d "{\"snapshot_path\":\"$OUT/vmstate\",\"mem_file_path\":\"$OUT/memfile\",\"resume_vm\":true}" >/dev/null

  local exit_code=0
  "$STAGE/guest_client" -uds "$vsock_uds" -port 1024 -timeout-s 30 -command true || exit_code=$?
  kill "$fc_pid" 2>/dev/null || true
  trap 'rm -rf "$STAGE"' EXIT
  if [ "$exit_code" -ne 0 ]; then
    echo "build-snapshot.sh: the fresh snapshot restored but \`true\` exited $exit_code" >&2
    exit 1
  fi
}

verify_restore_cloud_hypervisor() {
  local api_sock="$STAGE/verify-ch-api.sock" vsock_uds="$STAGE/verify-vsock.sock"
  cloud-hypervisor --api-socket "$api_sock" \
    --restore "source_url=file://$OUT" \
    --vsock "cid=3,socket=$vsock_uds" >/dev/null 2>&1 &
  local ch_pid=$!
  trap 'kill "$ch_pid" 2>/dev/null || true; rm -rf "$STAGE"' EXIT

  local exit_code=0
  "$STAGE/guest_client" -uds "$vsock_uds" -port 1024 -timeout-s 30 -command true || exit_code=$?
  kill "$ch_pid" 2>/dev/null || true
  trap 'rm -rf "$STAGE"' EXIT
  if [ "$exit_code" -ne 0 ]; then
    echo "build-snapshot.sh: the fresh snapshot restored but \`true\` exited $exit_code" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
main() {
  preflight
  build_agent
  assemble_rootfs
  boot_quiesce_snapshot
  write_manifest
  lock_down
  verify_restore
  log "done: $OUT"
}

main
