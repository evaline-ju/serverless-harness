# Optional: build the worker as an in-cluster pod image.
# Build from the REPO ROOT so the proto stubs (../gen/go, referenced via a
# `replace` in go.mod) are in the build context:
#
#   docker build -f remote-worker/Dockerfile -t quay.io/aslomnet/remote-worker:dev .
#
# Then set that image in ../deploy/knative/worker-example.yaml with
# RELAY_ADDR=sandbox-relay.default.svc:8443 (no port-forward needed in-cluster).
FROM golang:1.25-alpine AS build
WORKDIR /src
# go.work first: it is what lets the two modules be bumped independently (see #212).
# Without it the build here resolves each go.mod on its own and fails on a stale one.
COPY go.work ./
COPY gen/go/ ./gen/go/
COPY remote-worker/ ./remote-worker/
WORKDIR /src/remote-worker
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/remote-worker ./cmd/worker

# ripgrep is vendored because it is the IMPLEMENTATION of two registered tools, not a scheduling
# label. The Find/Glob tool runs `rg --files --hidden` (k8s-sandbox operations.ts:170) and the Grep
# tool runs `rg` directly (grep-tool.ts), and both land in THIS container whenever a leased gRPC
# presence record supplies a relay transport: extension.ts's `opts?.transport ??` overrides the fast
# AND the stream transport, so every pod operation runs over the relay. Without `rg` on PATH the model's Glob
# returns "glob failed in pod (rg exited 127)" and its Grep "rg failed in pod (exit 127)" -- the
# same attached-and-healthy-yet-every-tool-fails signature as the /workspace bug below. Both other
# sandbox images in the repo install it (sandbox.Dockerfile, k8s-sandbox/deploy/sandbox.yaml).
#
# It is genuinely not in the UBI 9 repositories (verified: `microdnf install ripgrep` -> "No package
# matches"), and EPEL would add a third-party repository to an image that executes model-authored
# commands. So: the upstream static musl build, pinned by version and checked against the sha256 the
# release publishes. Static, so it needs no libc from this base. Fetched in its own stage, so the
# tar/gzip needed to unpack it never reach the runtime image.
#
# To bump: change RG_VERSION and BOTH digests together. The digests are per-arch and `sha256sum -c`
# fails the build closed if either is wrong. Arch comes from `uname -m` rather than TARGETARCH
# because buildah / `oc new-build --strategy=docker` do not populate BuildKit's automatic args.
# hadolint ignore=DL3007
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest AS rg
ARG RG_VERSION=15.2.0
ARG RG_SHA256_X86_64=33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c
ARG RG_SHA256_AARCH64=800b1e7206afe799dfb5a6901f23147cfaabe0e52210538100f61e86e1740915
# hadolint ignore=DL3041
RUN microdnf install -y --nodocs tar gzip \
    && microdnf clean all
RUN set -eux; \
    case "$(uname -m)" in \
      x86_64)  target=x86_64-unknown-linux-musl;  sha="$RG_SHA256_X86_64" ;; \
      aarch64) target=aarch64-unknown-linux-musl; sha="$RG_SHA256_AARCH64" ;; \
      *) echo "no pinned ripgrep build for $(uname -m)" >&2; exit 1 ;; \
    esac; \
    tarball="ripgrep-${RG_VERSION}-${target}.tar.gz"; \
    curl -fsSL -o "/tmp/${tarball}" \
      "https://github.com/BurntSushi/ripgrep/releases/download/${RG_VERSION}/${tarball}"; \
    printf '%s  %s\n' "${sha}" "/tmp/${tarball}" > /tmp/rg.sha256; \
    sha256sum -c /tmp/rg.sha256; \
    tar -xzf "/tmp/${tarball}" -C /tmp; \
    install -m 755 "/tmp/ripgrep-${RG_VERSION}-${target}/rg" /usr/local/bin/rg; \
    rm -rf "/tmp/${tarball}" /tmp/rg.sha256 "/tmp/ripgrep-${RG_VERSION}-${target}"; \
    /usr/local/bin/rg --version

# The worker execs `bash -c <command>`, so the runtime image needs a shell and the
# coreutils the harness's ops call: base64 (writeFile), file (image mime sniffing).
# distroless/static ships none of them — every exec would fail with ENOENT.
# In production the binary drops into a real sandbox image instead; this stage
# exists so the standalone demo image works on its own.
#
# These are standalone demo images that track the current UBI 9 minor; a pinned
# digest would go stale silently, and microdnf package pinning breaks on minor
# bumps for the same reason .hadolint.yaml already ignores DL3018 for Alpine.
# hadolint ignore=DL3007
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
# git and python3 are here because cmd/worker/main.go's `probed` list advertises
#   bash, rg, base64, file, python3, git
# as this worker's Hello.capabilities, and its comment says "the pool will eventually match on
# these, so they must be true". Three of the six were not: on a real VM the leaf reported
# `caps=[bash base64 file]`, so it advertised a set it could not honour. All six are now installed
# (`rg` from the stage above), and dockerfile_parity_test.go fails if that stops being true.
#
# git also matters for measurement. Spec §2.3's duty bases were derived from workloads whose git
# operations cost ~470ms; without git in the sandbox, an E8 tool call can only be a ~0ms no-op, so
# the hands tier is exercised structurally but carries no load and the measured duty cycle describes
# a cheaper workload than the basis it is compared against.
# hadolint ignore=DL3041
RUN microdnf install -y --nodocs bash coreutils-single findutils file git python3 \
    && microdnf clean all
COPY --from=build /out/remote-worker /usr/local/bin/remote-worker
COPY --from=rg /usr/local/bin/rg /usr/local/bin/rg
# The harness execs every tool call from its sandbox working directory, which defaults to
# /workspace (KAGENTI_SANDBOX_CWD, select-sandbox.ts). Without this directory the worker's
# `bash -c` fails before running anything the caller asked for:
#   bash: line 1: cd: /workspace: No such file or directory
#   Command exited with code 1
# and the harness reports a tool error for a sandbox that is otherwise healthy and attached --
# observed on a real VM run, where the exec reached the leaf correctly and died on the cd. Owned by
# 1001:0 because that is the uid this image runs as (gid 0, matching the nonroot-v2 SCC below), so
# the agent can actually write in its own workspace. One `install -d` states owner, group and mode in
# one place, as deploy/knative/build-swebench-sandbox.sh:268 already does.
#
# WORKDIR because runner.go:159's "every command is self-contained as `cd 'cwd' && ...`" is today the
# only thing making the process cwd irrelevant. Any exec path that ever sends a bare command -- or an
# operator running `podman exec` to debug a sandbox -- would otherwise land in /, which is a second
# round of the bug documented directly above. Both sandbox images that came before this one set it.
RUN install -d -o 1001 -g 0 -m 775 /workspace
WORKDIR /workspace
# Matches the nonroot-v2 SCC deploy-incluster.sh applies.
USER 1001
ENTRYPOINT ["/usr/local/bin/remote-worker"]
