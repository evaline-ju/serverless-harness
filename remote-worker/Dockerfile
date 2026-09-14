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
# `caps=[bash base64 file]`, so it advertised a set it could not honour.
#
# It also matters for measurement. Spec §2.3's duty bases were derived from workloads whose git
# operations cost ~470ms; without git in the sandbox, an E8 tool call can only be a ~0ms no-op, so
# the hands tier is exercised structurally but carries no load and the measured duty cycle describes
# a cheaper workload than the basis it is compared against.
#
# `rg` is deliberately still absent: ripgrep is not in the UBI 9 repositories, so honouring it means
# either EPEL or vendoring a binary into a demo image. Left as a known gap rather than a silent one —
# whoever needs capability-matched scheduling should either add it or shorten `probed`.
# hadolint ignore=DL3041
RUN microdnf install -y --nodocs bash coreutils-single findutils file git python3 \
    && microdnf clean all
COPY --from=build /out/remote-worker /usr/local/bin/remote-worker
# The harness execs every tool call from its sandbox working directory, which defaults to
# /workspace (KAGENTI_SANDBOX_CWD, select-sandbox.ts). Without this directory the worker's
# `bash -c` fails before running anything the caller asked for:
#   bash: line 1: cd: /workspace: No such file or directory
#   Command exited with code 1
# and the harness reports a tool error for a sandbox that is otherwise healthy and attached --
# observed on a real VM run, where the exec reached the leaf correctly and died on the cd. Owned by
# 1001:0 because that is the uid this image runs as (gid 0, matching the nonroot-v2 SCC below), so
# the agent can actually write in its own workspace.
RUN mkdir -p /workspace && chown 1001:0 /workspace && chmod 775 /workspace
# Matches the nonroot-v2 SCC deploy-incluster.sh applies.
USER 1001
ENTRYPOINT ["/usr/local/bin/remote-worker"]
