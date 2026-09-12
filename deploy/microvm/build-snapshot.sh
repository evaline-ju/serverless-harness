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
                  [--instance-type TYPE] [--vmm firecracker|cloud-hypervisor]
                  [--guest-ram-mb 256] [--out DIR]

Builds one golden snapshot, ON THE INSTANCE TYPE THAT WILL RUN IT (spec §2.4:
restore requires identical hardware and software). --instance-type is an OVERRIDE:
left unset, the script auto-detects this host's identity (cloud metadata first,
then /sys/class/dmi/id/product_name -- see detect_host_instance_type) using the
SAME precedence remote-worker/cmd/microvm-worker/main.go's detectHostInstanceType
uses to verify the snapshot at start, so builder and verifier agree by
construction. Only pass --instance-type when you deliberately want the manifest to
name something other than what this host reports. Writes vmstate, memfile and a
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

if [ -z "$KERNEL" ] || [ -z "$ROOTFS" ] || [ -z "$AGENT_SRC" ] || [ -z "$IMAGE" ]; then
  # Fix-round-2 item C: --instance-type is deliberately NOT required here -- it is
  # an override over detect_host_instance_type's auto-detection, checked in
  # preflight() once all the detection helpers below are defined.
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
# instance-type auto-detection (fix-round-2 item C)
# ---------------------------------------------------------------------------
# --instance-type used to be a required, operator-typed argument written verbatim
# into the manifest. microvm-worker's detectHostInstanceType (remote-worker/cmd/
# microvm-worker/main.go) never reads that string back from the operator -- at
# verify time it PROBES the host itself (cloud metadata, then a DMI fallback) and
# compares its own answer to the manifest. On the EC2 dev box IMDS answers and a
# hand-typed --instance-type naturally agrees with it, so the two never disagree
# there; on the real bare-metal deployment target no cloud metadata answers, the
# worker falls all the way to /sys/class/dmi/id/product_name (something like
# "PowerEdge R760"), and a hand-typed manifest string will not match it --
# verifyInstanceType then refuses to start every worker on the fleet, and the only
# escape (SH_ALLOW_INSTANCE_TYPE_MISMATCH=true) disables the check entirely.
#
# detect_host_instance_type below MUST use the exact same precedence as
# detectHostInstanceType in remote-worker/cmd/microvm-worker/main.go: EC2 IMDSv2,
# then GCP metadata, then Azure IMDS, then DMI product_name. If you change the
# order (or add/remove a probe) on either side, change it on both -- this pairing
# IS the contract, not just a comment. --instance-type remains available as a
# deliberate OVERRIDE (e.g. a documented compatible substitute type), applied
# after detection and before anything reads $INSTANCE_TYPE.
metadata_get() {
  # $1: URL, remaining args: extra curl flags/headers. 300ms mirrors
  # metadataTimeout in main.go: these services answer in single-digit
  # milliseconds or not at all (wrong cloud, or none present).
  local url="$1"
  shift
  curl -fs -S --max-time 0.3 "$@" "$url" 2>/dev/null || true
}

ec2_instance_type() {
  # IMDSv2: a token must be minted (PUT /latest/api/token) before EC2's metadata
  # service answers any meta-data GET -- mirrors ec2InstanceType in main.go.
  local token
  token="$(curl -fs -S --max-time 0.3 -X PUT \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 60" \
    http://169.254.169.254/latest/api/token 2>/dev/null || true)"
  [ -n "$token" ] || return 0
  metadata_get "http://169.254.169.254/latest/meta-data/instance-type" \
    -H "X-aws-ec2-metadata-token: $token"
}

gcp_machine_type() {
  # GCE answers "projects/<num>/machineTypes/<type>"; take the trailing segment
  # so this is comparable to what EC2/Azure return -- mirrors gcpMachineType.
  local raw
  raw="$(metadata_get "http://metadata.google.internal/computeMetadata/v1/instance/machine-type" \
    -H "Metadata-Flavor: Google")"
  printf '%s\n' "${raw##*/}"
}

azure_vm_size() {
  # Mirrors azureVMSize: Azure IMDS answers any request carrying its required
  # header without further auth.
  metadata_get "http://169.254.169.254/metadata/instance/compute/vmSize?api-version=2021-02-01" \
    -H "Metadata: true"
}

stable_host_identity() {
  # Mirrors stableHostIdentity: /sys/class/dmi/id/product_name is set by
  # firmware/the hypervisor and stable across reboots on real hardware and most
  # non-cloud hypervisors alike. uname is this script's equivalent of main.go's
  # runtime.GOOS/GOARCH last resort, so this always returns SOMETHING.
  local pn
  if [ -r /sys/class/dmi/id/product_name ]; then
    pn="$(cat /sys/class/dmi/id/product_name 2>/dev/null || true)"
    if [ -n "$pn" ]; then
      printf '%s\n' "$pn"
      return
    fi
  fi
  printf '%s/%s\n' "$(uname -s)" "$(uname -m)"
}

detect_host_instance_type() {
  local t
  t="$(ec2_instance_type)"
  if [ -n "$t" ]; then
    printf '%s\n' "$t"
    return
  fi
  t="$(gcp_machine_type)"
  if [ -n "$t" ]; then
    printf '%s\n' "$t"
    return
  fi
  t="$(azure_vm_size)"
  if [ -n "$t" ]; then
    printf '%s\n' "$t"
    return
  fi
  stable_host_identity
}

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

  if [ -z "$INSTANCE_TYPE" ]; then
    # Fix-round-2 item C: detect using the same precedence the worker verifies
    # with, instead of demanding the operator type (and keep in sync by hand)
    # something the worker will independently re-derive at start.
    INSTANCE_TYPE="$(detect_host_instance_type)"
    log "preflight: --instance-type not given, detected '$INSTANCE_TYPE'"
  fi

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
#
# Fix-round-3: the generated file is written under $AGENT_SRC (inside the module),
# not $STAGE. Go's internal-package rule keys off the IMPORTING FILE's own
# directory, not the process's working directory, so building from $STAGE via
# `cd "$AGENT_SRC" && go build .../$STAGE/guest_client.go` stayed illegal
# regardless of the cd -- $STAGE sits outside the module tree, so importing
# internal/guestagent from a file there is never allowed. The temp package
# directory is dot-prefixed so `go build ./...`, `go vet ./...`, and `gofmt -l .`
# (which this repo's own checks run) skip it even if cleanup below is somehow
# skipped, and its cleanup is armed (folded into the same EXIT trap $STAGE
# already uses) BEFORE the directory is created -- same discipline as
# fix-round-2 item A for the /dev bind mounts, because this directory lives
# inside the user's SOURCE TREE, not /tmp, so leaking it is worse than an
# ordinary $STAGE leak, and the script runs as root, so anything left behind
# would be root-owned in what is normally a non-root user's checkout.
# ---------------------------------------------------------------------------
write_guest_client() {
  local tmp_pkg="$AGENT_SRC/.build-snapshot-tmp-$$"
  trap 'rm -rf "'"$tmp_pkg"'" "$STAGE"' EXIT
  if ! mkdir -p "$tmp_pkg"; then
    echo "build-snapshot.sh: cannot create $tmp_pkg -- is --agent $AGENT_SRC" \
      "writable? (guest_client.go must be generated inside the module tree so its" \
      "internal/guestagent import is legal; see the comment above write_guest_client)" >&2
    exit 1
  fi
  cat >"$tmp_pkg/guest_client.go" <<'GOEOF'
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
  (cd "$AGENT_SRC" && go build -o "$STAGE/guest_client" "$tmp_pkg/guest_client.go")
  rm -rf "$tmp_pkg"
  trap 'rm -rf "$STAGE"' EXIT
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

# ---------------------------------------------------------------------------
# Jail helpers (fix-round items 1, 2, 9): both VMM arms boot and restore
# chrooted into a directory whose ONLY structure is the fixed, jail-relative
# basenames baked into their configs -- /kernel, /rootfs, /workspace.img,
# /vsock.sock, /run/<api-sock> -- because that is the one thing that makes a
# recorded path still resolve after the process that recorded it, and the
# directory it was chrooted into, are both gone. This is not invented here: it
# is remote-worker/internal/vmpool/launcher_firecracker.go's own jailer
# convention (apiSockRelPath, vsockRelPath, the fileVMState/fileMemory/
# fileKernel/fileRootfs/fileAgent hardlink set, workspace.img), copied
# verbatim rather than re-derived, per this round's own instruction that the
# launcher wins on any disagreement. No disagreement was found.
# ---------------------------------------------------------------------------

# DefaultWorkspaceImageBytes in launcher_firecracker.go: 2 GiB, "matches the
# brief's own test fixtures". Kept identical here so a golden snapshot built
# by this script presents the guest the same /dev/vdb capacity the production
# launcher's lazily-created workspace.img would.
WORKSPACE_IMAGE_BYTES=$((2 * 1024 * 1024 * 1024))

require_root() {
  # Item 4: the API socket either VMM creates is root-owned the moment the VMM
  # creates it, and both boot/verify paths now chroot and bind-mount /dev/kvm
  # into a jail -- both need root. Failing loudly here beats failing confusingly
  # at the first `curl --unix-socket`/`mount --bind` permission error.
  if [ "$(id -u)" -ne 0 ]; then
    echo "build-snapshot.sh: must run as root (needed for chroot, mount --bind" >&2
    echo "  /dev/kvm, and the VMM's own root-owned API socket)" >&2
    exit 1
  fi
}

# api_put issues a PUT with JSON body $3 to path $2 on the VMM's Unix-socket API
# $1, and treats a transport failure OR a non-2xx response as fatal (item 4). The
# previous form (`curl -s ... >/dev/null`) swallowed both kinds of failure and let
# the build limp on to a snapshot silently missing whatever the call configured.
api_put() {
  local sock="$1" path="$2" body="$3" resp status
  resp="$(curl -s -S --unix-socket "$sock" -w '\n%{http_code}' -X PUT "http://localhost${path}" -d "$body")" || {
    echo "build-snapshot.sh: PUT $path: curl could not reach $sock" >&2
    exit 1
  }
  status="${resp##*$'\n'}"
  case "$status" in
    2??) ;;
    *)
      echo "build-snapshot.sh: PUT $path returned HTTP $status: ${resp%$'\n'*}" >&2
      exit 1
      ;;
  esac
}

# hardlink_or_copy_bin resolves $1 on PATH and hardlinks (falling back to a copy
# across filesystems) it into $2, so a chrooted VMM process can execve it from
# inside its own jail -- chroot resolves the command it execs AFTER changing
# root, so the binary must physically exist inside the jail, not just on $PATH.
hardlink_or_copy_bin() {
  local name="$1" dst="$2" src
  src="$(command -v "$name")" || {
    echo "build-snapshot.sh: $name not found on PATH" >&2
    exit 1
  }
  rm -f "$dst"
  ln "$src" "$dst" 2>/dev/null || cp -p "$src" "$dst"
  chmod 0555 "$dst"
}

# jail_mount_dev bind-mounts /dev/kvm (mandatory -- the VMM cannot start without
# it) and /dev/urandom (best-effort) into $1/dev, so a process chrooted into $1
# can still reach them by their normal absolute device paths.
jail_mount_dev() {
  local jail="$1"
  mkdir -p "$jail/dev"
  : >"$jail/dev/kvm"
  if ! mount --bind /dev/kvm "$jail/dev/kvm"; then
    echo "build-snapshot.sh: could not bind-mount /dev/kvm into the jail at $jail" >&2
    exit 1
  fi
  : >"$jail/dev/urandom" 2>/dev/null || true
  mount --bind /dev/urandom "$jail/dev/urandom" 2>/dev/null || true
}

# jail_unmount_dev is the inverse of jail_mount_dev, and MUST run before rm -rf on
# the jail: an active bind mount is a live mountpoint, and rm -rf through one
# fails (or worse, on some setups silently no-ops) rather than actually clearing
# the directory. Best-effort and safe to call even if nothing was mounted.
jail_unmount_dev() {
  local jail="$1"
  umount "$jail/dev/urandom" 2>/dev/null || true
  umount "$jail/dev/kvm" 2>/dev/null || true
}

# ensure_workspace_image creates $1 as a sparse ext4 filesystem of
# WORKSPACE_IMAGE_BYTES -- the exact recipe (truncate, then mkfs.ext4 -F) of
# launcher_firecracker.go's ensureWorkspaceImage, so the golden snapshot's second
# drive is byte-for-byte the kind of image the production launcher creates.
ensure_workspace_image() {
  local path="$1"
  truncate -s "$WORKSPACE_IMAGE_BYTES" "$path"
  mkfs.ext4 -q -F "$path" >/dev/null
}

boot_quiesce_snapshot_firecracker() {
  local jail="$STAGE"
  local api_sock="$jail/run/firecracker.socket" vsock_uds="$jail/vsock.sock" console_log="$STAGE/console.log"
  local fc_pid=""
  log "preparing the firecracker build jail at $jail (items 1, 2: jail-relative paths only)"
  mkdir -p "$jail/run"
  hardlink_or_copy_bin firecracker "$jail/firecracker"
  # Fix-round-2 item A: the trap is armed BEFORE jail_mount_dev runs, not after --
  # a failure anywhere between the mount and the old trap-arm point (ensure_workspace_image's
  # mkfs.ext4, the chroot itself) used to leave only the original `rm -rf "$STAGE"`
  # trap active, leaking the /dev/kvm and /dev/urandom bind mounts (a live mountpoint
  # under $STAGE that rm -rf then runs over, rather than clears). jail_unmount_dev is
  # unconditionally best-effort (umount ... || true), so arming it before the mount
  # exists is harmless -- it just no-ops if fired early. $fc_pid is looked up when
  # the trap FIRES, not when it is set, so declaring it empty here and assigning it
  # below is sufficient even under `set -u`.
  trap 'jail_unmount_dev "'"$jail"'"; kill "$fc_pid" 2>/dev/null || true; rm -rf "$STAGE"' EXIT
  jail_mount_dev "$jail"
  # Item 2: a second, non-root, read-write drive so a restored VM has something
  # for the per-run workspace to mount -- launcher_firecracker.go's Restore hard-
  # links a workspace.img into every jail at this exact path expecting the golden
  # snapshot to already have a matching drive configured; without one there is no
  # PUT /drives/workspace at restore time (Firecracker's snapshot/load has no such
  # call), so the drive has to already exist in the snapshotted config.
  ensure_workspace_image "$jail/workspace.img"

  log "starting firecracker chrooted into $jail ($api_sock)"
  # Item 3: stdin redirected -- a backgrounded VMM that inherits this script's
  # controlling terminal can be sent SIGTTIN and hang forever the moment it
  # touches stdin, indistinguishable from a slow boot from the outside.
  chroot "$jail" /firecracker --api-sock /run/firecracker.socket \
    </dev/null >"$console_log" 2>&1 &
  fc_pid=$!

  # Item 1: kernel_image_path and the rootfs drive's path_on_host are now
  # jail-relative ("/kernel", "/rootfs"), exactly like launcher_firecracker.go's
  # own hardlink set -- not "$STAGE/kernel"/"$STAGE/rootfs", which is this bug in
  # the first place: $STAGE is deleted by this script's own EXIT trap the moment
  # the build finishes, and every subsequent LoadSnapshot would fail.
  api_put "$api_sock" /boot-source \
    "{\"kernel_image_path\":\"/kernel\",\"boot_args\":\"console=ttyS0 reboot=k panic=1 pci=off\"}"
  api_put "$api_sock" /drives/rootfs \
    "{\"drive_id\":\"rootfs\",\"path_on_host\":\"/rootfs\",\"is_root_device\":true,\"is_read_only\":false}"
  api_put "$api_sock" /drives/workspace \
    "{\"drive_id\":\"workspace\",\"path_on_host\":\"/workspace.img\",\"is_root_device\":false,\"is_read_only\":false}"
  api_put "$api_sock" /vsock \
    "{\"vsock_id\":\"vsock0\",\"guest_cid\":3,\"uds_path\":\"/vsock.sock\"}"
  api_put "$api_sock" /machine-config \
    "{\"mem_size_mib\":$GUEST_RAM_MB,\"vcpu_count\":1}"
  api_put "$api_sock" /actions '{"action_type":"InstanceStart"}'

  wait_for_agent "$vsock_uds" "$console_log"
  MANIFEST_CAPABILITIES="$(probe_capabilities "$vsock_uds")"
  quiesce_guest "$vsock_uds"

  log "snapshotting (PUT /snapshot/create, resuming nothing afterwards)"
  api_put "$api_sock" /vm '{"state":"Paused"}'
  api_put "$api_sock" /snapshot/create \
    "{\"snapshot_path\":\"/vmstate\",\"mem_file_path\":\"/memfile\",\"snapshot_type\":\"Full\",\"resume_vm\":false}"

  kill "$fc_pid" 2>/dev/null || true
  wait "$fc_pid" 2>/dev/null || true
  jail_unmount_dev "$jail"
  trap 'rm -rf "$STAGE"' EXIT
}

boot_quiesce_snapshot_cloud_hypervisor() {
  local jail="$STAGE"
  local api_sock="$jail/run/ch-api.sock" vsock_uds="$jail/vsock.sock" console_log="$STAGE/console.log"
  local ch_pid=""
  log "preparing the cloud-hypervisor build jail at $jail (item 9: same jail-relative convention as the firecracker arm)"
  mkdir -p "$jail/run"
  hardlink_or_copy_bin cloud-hypervisor "$jail/cloud-hypervisor"
  # Fix-round-2 item A: trap armed before the mount, not after -- see the matching
  # comment in boot_quiesce_snapshot_firecracker for why.
  trap 'jail_unmount_dev "'"$jail"'"; kill "$ch_pid" 2>/dev/null || true; rm -rf "$STAGE"' EXIT
  jail_mount_dev "$jail"

  log "starting cloud-hypervisor chrooted into $jail ($api_sock)"
  # Item 7: Cloud Hypervisor has no is_root_device-style flag the way Firecracker
  # does -- without an explicit root= the guest kernel panics looking for its root
  # device (docs/notes/cloud-hypervisor-tutorial.md, hands-on reproduced there).
  # pci=off is Firecracker's own arg (its virtio devices are MMIO-only) and is
  # deliberately DROPPED here: Cloud Hypervisor's virtio devices default to the
  # PCI transport and need PCI enumerated to be found at all -- confirmed by the
  # tutorial's own hands-on-tested, working boot cmdline, which never passes
  # pci=off either.
  #
  # Item 8: readonly=on verified against the installed cloud-hypervisor v53.0's
  # own `--disk` help text (full grammar includes
  # "path=...,readonly=on|off,...,lock_granularity=byte-range|full"). Cloud
  # Hypervisor holds an advisory per-disk write lock that Firecracker does not, so
  # a writable disk here would block every concurrent restore against the same
  # rootfs file. lock_granularity is the documented alternative for a future case
  # that needs the disk writable under concurrency; not used here.
  #
  # Item 9: path=/rootfs is jail-relative, exactly like the firecracker arm's
  # path_on_host, chrooted into $jail -- "$STAGE/rootfs" is the same
  # gone-once-the-script-exits bug items 1/2 fix for firecracker.
  #
  # Item 3/10: stdin redirected -- same SIGTTIN hazard as the firecracker launch.
  chroot "$jail" /cloud-hypervisor \
    --api-socket /run/ch-api.sock \
    --kernel /kernel \
    --cmdline "console=ttyS0 root=/dev/vda rw reboot=k panic=1" \
    --disk "path=/rootfs,readonly=on" \
    --vsock "cid=3,socket=/vsock.sock" \
    --memory "size=${GUEST_RAM_MB}M" \
    --cpus boot=1 \
    --console "file=/console.log" \
    --serial off \
    </dev/null >/dev/null 2>&1 &
  ch_pid=$!

  wait_for_agent "$vsock_uds" "$console_log"
  MANIFEST_CAPABILITIES="$(probe_capabilities "$vsock_uds")"
  quiesce_guest "$vsock_uds"

  log "snapshotting (ch-remote pause + snapshot, no resume afterwards)"
  ch-remote --api-socket "$api_sock" pause
  ch-remote --api-socket "$api_sock" snapshot "file:///ch-snapshot"
  # Cloud Hypervisor writes one snapshot directory (config.json, state.json,
  # memory-ranges) rather than separate vmstate/memfile files. config.json is kept
  # (as ch-config.json) rather than discarded: vm.restore replays the WHOLE
  # directory, config.json included, and the tutorial documents it as the vehicle
  # for editing a restored snapshot's paths between snapshot and restore (§9a) --
  # dropping it, as the previous form of this script did, left restore with no
  # config to replay at all. state.json/memory-ranges are split out under the
  # vmstate/memfile names so both VMMs feed write_manifest identically.
  cp "$jail/ch-snapshot/config.json" "$STAGE/ch-config.json"
  mv "$jail/ch-snapshot/state.json" "$STAGE/vmstate"
  mv "$jail/ch-snapshot/memory-ranges" "$STAGE/memfile"

  kill "$ch_pid" 2>/dev/null || true
  wait "$ch_pid" 2>/dev/null || true
  jail_unmount_dev "$jail"
  trap 'rm -rf "$STAGE"' EXIT
}

boot_quiesce_snapshot() {
  write_guest_client
  # The kernel is copied into $STAGE BEFORE booting (not in write_manifest, where
  # it used to happen): both VMM arms above now need it in place, at the
  # jail-relative name "/kernel", before they ever start the VMM.
  cp "$KERNEL" "$STAGE/kernel"
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
  # $STAGE/kernel is copied by boot_quiesce_snapshot, before either VMM arm
  # boots -- both now need it in place at the jail-relative name "/kernel"
  # pre-boot, not just afterwards for hashing.

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
  files=(vmstate memfile kernel rootfs agent manifest.json)
  # cloud-hypervisor's snapshot is a directory (config.json, state.json,
  # memory-ranges), not just the vmstate/memfile pair -- config.json has to ship
  # too, or vm.restore's source_url has nothing to replay at restore time.
  if [ "$VMM" = "cloud-hypervisor" ]; then
    files+=(ch-config.json)
  fi
  for f in "${files[@]}"; do
    install -m 0644 "$STAGE/$f" "$OUT/$f"
  done
  chown -R root:root "$OUT"
  for f in "${files[@]}"; do
    chmod 0444 "$OUT/$f"
  done
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
  # Items 1/2/9's whole point: prove the snapshot in $OUT is portable by
  # restoring it into a FRESH jail at a DIFFERENT absolute path than the one it
  # was built under -- if $OUT still baked in an absolute, build-time path, this
  # jail would never see it and LoadSnapshot would fail exactly like the
  # original bug this round fixes.
  local jail="$STAGE/verify-jail"
  local api_sock="$jail/run/verify-api.sock" vsock_uds="$jail/vsock.sock"
  local fc_pid=""
  mkdir -p "$jail/run"
  hardlink_or_copy_bin firecracker "$jail/firecracker"
  # Fix-round-2 item A: trap armed before the mount, not after -- a failing `ln`
  # (e.g. cross-device) or ensure_workspace_image's mkfs.ext4 below used to run
  # inside the leak window; see the matching comment in
  # boot_quiesce_snapshot_firecracker for the full rationale.
  trap 'jail_unmount_dev "'"$jail"'"; kill "$fc_pid" 2>/dev/null || true; rm -rf "$STAGE"' EXIT
  jail_mount_dev "$jail"
  ln "$OUT/vmstate" "$jail/vmstate"
  ln "$OUT/memfile" "$jail/memfile"
  ln "$OUT/rootfs" "$jail/rootfs"
  ensure_workspace_image "$jail/workspace.img"

  chroot "$jail" /firecracker --api-sock /run/verify-api.sock \
    </dev/null >"$STAGE/verify-console.log" 2>&1 &
  fc_pid=$!

  # Wire format confirmed against fcapi.go's loadSnapshotRequest struct:
  # snapshot_path is top-level, the memory file nests under mem_backend as
  # {backend_path, backend_type} -- NOT the flat mem_file_path field, which
  # belongs to the separate /snapshot/create request. vsock_override lets the
  # vsock UDS path move between snapshot and restore; here it does not move
  # (both are /vsock.sock, jail-relative) but is still supplied to match the
  # launcher's own restore call shape.
  api_put "$api_sock" /vsock \
    "{\"vsock_id\":\"vsock0\",\"guest_cid\":3,\"uds_path\":\"/vsock.sock\"}"
  api_put "$api_sock" /snapshot/load \
    "{\"snapshot_path\":\"/vmstate\",\"mem_backend\":{\"backend_path\":\"/memfile\",\"backend_type\":\"File\"},\"vsock_override\":\"/vsock.sock\",\"resume_vm\":true}"

  local exit_code=0
  "$STAGE/guest_client" -uds "$vsock_uds" -port 1024 -timeout-s 30 -command true || exit_code=$?
  kill "$fc_pid" 2>/dev/null || true
  wait "$fc_pid" 2>/dev/null || true
  jail_unmount_dev "$jail"
  trap 'rm -rf "$STAGE"' EXIT
  if [ "$exit_code" -ne 0 ]; then
    echo "build-snapshot.sh: the fresh snapshot restored but \`true\` exited $exit_code" >&2
    exit 1
  fi
}

verify_restore_cloud_hypervisor() {
  # Same portability check as the firecracker arm, restoring into a fresh jail
  # at a different absolute path than the one used to build $OUT.
  #
  # Self-discovered 11th finding: the previous form of this function passed
  # `--restore source_url=file://$OUT` as a CLI flag. cloud-hypervisor has no
  # such flag -- docs/notes/cloud-hypervisor-tutorial.md's own hands-on-tested
  # restore procedure (Sec 6a-6c) always starts a BARE cloud-hypervisor process
  # against only --api-socket, then issues `PUT /api/v1/vm.restore` with body
  # {"source_url":..., "resume":true}; a web search for a --restore CLI flag
  # found no confirmation either. Rebuilt on the API-call pattern below.
  local jail="$STAGE/verify-jail"
  local api_sock="$jail/run/verify-ch-api.sock" vsock_uds="$jail/vsock.sock"
  local ch_pid=""
  mkdir -p "$jail/run" "$jail/ch-snapshot"
  hardlink_or_copy_bin cloud-hypervisor "$jail/cloud-hypervisor"
  # Fix-round-2 item A: trap armed before the mount, not after -- see the matching
  # comment in boot_quiesce_snapshot_firecracker for the full rationale.
  trap 'jail_unmount_dev "'"$jail"'"; kill "$ch_pid" 2>/dev/null || true; rm -rf "$STAGE"' EXIT
  jail_mount_dev "$jail"
  ln "$OUT/rootfs" "$jail/rootfs"
  # vm.restore replays the whole snapshot directory, not just memory state, so
  # config.json (shipped as ch-config.json, see lock_down/item 9's corollary)
  # has to be put back next to the state files under their original names
  # before the restore call.
  ln "$OUT/ch-config.json" "$jail/ch-snapshot/config.json"
  ln "$OUT/vmstate" "$jail/ch-snapshot/state.json"
  ln "$OUT/memfile" "$jail/ch-snapshot/memory-ranges"

  chroot "$jail" /cloud-hypervisor --api-socket /run/verify-ch-api.sock \
    </dev/null >"$STAGE/verify-console.log" 2>&1 &
  ch_pid=$!

  api_put "$api_sock" /api/v1/vm.restore \
    '{"source_url":"file:///ch-snapshot","resume":true}'

  local exit_code=0
  "$STAGE/guest_client" -uds "$vsock_uds" -port 1024 -timeout-s 30 -command true || exit_code=$?
  kill "$ch_pid" 2>/dev/null || true
  wait "$ch_pid" 2>/dev/null || true
  jail_unmount_dev "$jail"
  trap 'rm -rf "$STAGE"' EXIT
  if [ "$exit_code" -ne 0 ]; then
    echo "build-snapshot.sh: the fresh snapshot restored but \`true\` exited $exit_code" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
main() {
  require_root
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
