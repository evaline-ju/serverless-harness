// Command microvm-worker serves the SAME Attach wire contract as remote-worker, but
// runs every Exec in a microVM created for it and destroyed after it.
//
// It is a SIBLING of cmd/worker, not a replacement: remote-worker is untouched, which
// is what makes "the container arm cannot regress" a structural fact rather than a
// test result, and what makes E11's A/B an image swap (spec §10).
//
// PRIVILEGE. Unlike remote-worker this process needs /dev/kvm and the right to spawn
// VMMs. That is acceptable only because nothing agent-influenced ever executes outside
// a VM: this process parses a frame, writes bytes to a vsock, and spawns a VMM with
// fixed argv (spec §3.5). There is deliberately no host-execution fallback — see
// launcherFor.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/kagenti/serverless-harness/gen/go/sandbox/v1"
	"github.com/kagenti/serverless-harness/remote-worker/internal/session"
	"github.com/kagenti/serverless-harness/remote-worker/internal/vmpool"
)

const (
	backoffMin = 500 * time.Millisecond
	backoffMax = 30 * time.Second
)

func env(get func(string) string, k, def string) string {
	if v := get(k); v != "" {
		return v
	}
	return def
}

func envInt64(get func(string) string, k string, def int64) (int64, error) {
	v := get(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q must be a positive integer", k, v)
	}
	return n, nil
}

// poolConfig reads vmpool.Config from the environment and refuses rather than
// guessing. Every required value below is one whose wrong default is a security or
// capacity bug, so there is no "sensible default" to fall back on: spec §6's posture
// is "fail the unit at start", not "degrade".
func poolConfig(get func(string) string) (vmpool.Config, error) {
	var cfg vmpool.Config
	cfg.VMM = vmpool.VMMKind(env(get, "SH_VMM", string(vmpool.Firecracker)))
	cfg.SnapshotDir = get("SH_SNAPSHOT_DIR")
	if cfg.SnapshotDir == "" {
		return cfg, fmt.Errorf("SH_SNAPSHOT_DIR is required")
	}
	cfg.WorkspaceRoot = get("SH_WORKSPACE_ROOT")
	if cfg.WorkspaceRoot == "" {
		return cfg, fmt.Errorf("SH_WORKSPACE_ROOT is required")
	}
	if get("SH_MAX_COMMITTED_MB") == "" {
		return cfg, fmt.Errorf("SH_MAX_COMMITTED_MB is required: without the memory gate, " +
			"pressure goes straight to the OOM killer, whose size-ranked favourites include " +
			"this process and every run on the host (spec §6)")
	}
	var err error
	if v, e := envInt64(get, "SH_STANDBY_DEPTH", vmpool.DefaultStandbyDepth); e != nil {
		err = e
	} else {
		cfg.StandbyDepth = int(v)
	}
	if v, e := envInt64(get, "SH_GUEST_RAM_MB", vmpool.DefaultGuestRAMBytes>>20); e != nil && err == nil {
		err = e
	} else if e == nil {
		cfg.GuestRAMBytes = v << 20
	}
	if v, e := envInt64(get, "SH_MAX_RUNS", 64); e != nil && err == nil {
		err = e
	} else if e == nil {
		cfg.MaxRuns = int(v)
	}
	if v, e := envInt64(get, "SH_MAX_COMMITTED_MB", 0); e != nil && err == nil {
		err = e
	} else if e == nil {
		cfg.MaxCommittedBytes = v << 20
	}
	if get("SH_MEMORY_RESERVE_MB") != "" {
		if v, e := envInt64(get, "SH_MEMORY_RESERVE_MB", 0); e != nil && err == nil {
			err = e
		} else if e == nil {
			cfg.MemoryReserveBytes = v << 20
		}
	}
	return cfg, err
}

// launcherFor maps a VMMKind to a Launcher.
//
// NO HOST-EXECUTION FALLBACK, deliberately and permanently. vmpool.FakeLauncher runs
// commands with host bash and is reachable only from vmpoolctl; wiring it here would
// put agent-authored code inside the privileged process, which is strictly worse than
// today's container (spec §3.3, §3.5). Spec §6's "KVM unavailable at startup" row
// says fail the unit at start — and the strongest form of that is having no code path
// that could do otherwise. §8's "nothing executes outside a VM" gate pins it.
//
// get and snapDir are threaded through (rather than read from the environment
// inline) so this function stays a pure mapping from already-resolved config to a
// Launcher — the same shape poolConfig above already uses.
func launcherFor(kind vmpool.VMMKind, get func(string) string, snapDir string) (vmpool.Launcher, error) {
	switch kind {
	case vmpool.Firecracker:
		wsImageMB, err := envInt64(get, "SH_WORKSPACE_IMAGE_MB", 2048)
		if err != nil {
			return nil, err
		}
		return vmpool.NewFirecrackerLauncher(vmpool.FirecrackerOptions{
			SnapshotDir:         snapDir,
			JailerBin:           env(get, "SH_JAILER_BIN", "/usr/bin/jailer"),
			FirecrackerBin:      env(get, "SH_FIRECRACKER_BIN", "/usr/bin/firecracker"),
			ChrootBase:          env(get, "SH_CHROOT_BASE", "/srv/jail"),
			UID:                 os.Getuid(),
			GID:                 os.Getgid(),
			ParentCgroup:        env(get, "SH_PARENT_CGROUP", "microvm-vms.slice"),
			WorkspaceImageBytes: wsImageMB << 20,
			VsockPort:           1024,
		})
	case vmpool.CloudHypervisor:
		// Task 16 returns the real launcher here.
		return nil, fmt.Errorf("VMM %q is not implemented yet (Phase D)", kind)
	default:
		return nil, fmt.Errorf("SH_VMM=%q must be %q or %q; there is no host-execution fallback (spec §3.5)",
			kind, vmpool.Firecracker, vmpool.CloudHypervisor)
	}
}

// --- Item 5 (fix round): verify the manifest's recorded InstanceType against the
// running host. This is ADDITIVE to the existing verify+pin+probe block in main():
// man.Verify above only checks the snapshot's own internal hashes, never the host
// it is about to be restored onto, and spec §2.4 requires identical hardware for a
// restore to be safe at all (device/BAR/CPU-feature assumptions baked into the
// paused VM state). None of vmpool's existing exported API is touched.

// instanceTypeCheckOverrideEnv opts a host OUT of this check for deliberate
// cross-host use (e.g. local dev, or a documented compatible substitute type). Off
// by default: silence here would mean every worker on the fleet restoring onto the
// wrong hardware and finding out only when a guest first misbehaves.
const instanceTypeCheckOverrideEnv = "SH_ALLOW_INSTANCE_TYPE_MISMATCH"

// metadataTimeout bounds each individual cloud metadata probe. These services only
// answer on the host's own link-local address and either respond in single-digit
// milliseconds or not at all (wrong cloud, or no metadata service present) --
// generous enough to tolerate a slow VM, short enough that probing three clouds in
// turn on a bare-metal box costs a human-imperceptible fraction of a second.
const metadataTimeout = 300 * time.Millisecond

// detectHostInstanceType identifies the machine microvm-worker is running on, in
// the same vocabulary build-snapshot.sh's manifest records for instance_type.
//
// Fix-round-2 item C: build-snapshot.sh's detect_host_instance_type (deploy/
// microvm/build-snapshot.sh) auto-detects the build host using THIS EXACT
// precedence -- EC2 IMDSv2, then GCP metadata, then Azure IMDS, then DMI
// product_name -- and only lets --instance-type override that when explicitly
// passed. That pairing is deliberate: if you change the order (or add/remove a
// probe) here, change it there too, or a manifest and the worker verifying it
// will silently drift onto different hardware identities again.
//
// It tries each cloud provider's metadata service in turn -- generalized beyond EC2
// deliberately, since nothing here should assume the fleet is EC2-only -- and falls
// back to a stable, non-cloud host identity when none answer, so bare-metal and
// other-hypervisor hosts still get a real check instead of none.
func detectHostInstanceType(ctx context.Context) string {
	if t := ec2InstanceType(ctx); t != "" {
		return t
	}
	if t := gcpMachineType(ctx); t != "" {
		return t
	}
	if t := azureVMSize(ctx); t != "" {
		return t
	}
	return stableHostIdentity()
}

func metadataGET(ctx context.Context, url string, headers map[string]string) string {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// ec2InstanceType speaks IMDSv2: a token must be minted (PUT /latest/api/token)
// before EC2's metadata service will answer any meta-data GET.
func ec2InstanceType(ctx context.Context) string {
	tctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	treq, err := http.NewRequestWithContext(tctx, http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	if err != nil {
		return ""
	}
	treq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tresp, err := http.DefaultClient.Do(treq)
	if err != nil {
		return ""
	}
	defer func() { _ = tresp.Body.Close() }()
	if tresp.StatusCode != http.StatusOK {
		return ""
	}
	tokBytes, err := io.ReadAll(io.LimitReader(tresp.Body, 256))
	if err != nil {
		return ""
	}
	token := strings.TrimSpace(string(tokBytes))
	return metadataGET(ctx, "http://169.254.169.254/latest/meta-data/instance-type",
		map[string]string{"X-aws-ec2-metadata-token": token})
}

// gcpMachineType asks GCE's metadata server, which answers any request carrying the
// Metadata-Flavor header without further auth.
func gcpMachineType(ctx context.Context) string {
	raw := metadataGET(ctx, "http://metadata.google.internal/computeMetadata/v1/instance/machine-type",
		map[string]string{"Metadata-Flavor": "Google"})
	// GCE returns "projects/<num>/machineTypes/<type>"; take the trailing segment so
	// this is comparable to what build-snapshot.sh recorded.
	if idx := strings.LastIndex(raw, "/"); idx >= 0 {
		return raw[idx+1:]
	}
	return raw
}

// azureVMSize asks Azure IMDS, which (like GCE) answers any request carrying its
// required header without further auth.
func azureVMSize(ctx context.Context) string {
	return metadataGET(ctx,
		"http://169.254.169.254/metadata/instance/compute/vmSize?api-version=2021-02-01",
		map[string]string{"Metadata": "true"})
}

// stableHostIdentity is the non-cloud fallback, used when no metadata service
// answered. /sys/class/dmi/id/product_name is set by firmware/the hypervisor and
// stable across reboots on real hardware and most non-cloud hypervisors alike;
// runtime.GOOS/GOARCH is the last resort so this always returns SOMETHING rather
// than an empty string a caller might mistake for "no host identity exists".
func stableHostIdentity() string {
	if b, err := os.ReadFile("/sys/class/dmi/id/product_name"); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return runtime.GOOS + "/" + runtime.GOARCH
}

// verifyInstanceType refuses loudly, naming both values, when the manifest's
// recorded InstanceType does not match what detect reports for the running host --
// unless the operator has explicitly opted out via instanceTypeCheckOverrideEnv.
// detect is injected (rather than called directly) so tests exercise this without
// touching the network. An empty manifestType (an old manifest predating this
// field) or an undetectable host both fail OPEN, not closed: this check is meant to
// catch a real, recorded mismatch, not to block startup when there is nothing to
// compare.
func verifyInstanceType(get func(string) string, manifestType string, detect func(context.Context) string) error {
	if manifestType == "" {
		return nil
	}
	if skip, err := strconv.ParseBool(get(instanceTypeCheckOverrideEnv)); err == nil && skip {
		log.Printf("microvm-worker: %s=true, skipping instance-type verification (snapshot recorded %q)",
			instanceTypeCheckOverrideEnv, manifestType)
		return nil
	}
	host := detect(context.Background())
	if host == "" || host == manifestType {
		return nil
	}
	return fmt.Errorf("snapshot was built on instance type %q but this host reports %q "+
		"(spec §2.4: restore requires identical hardware; set %s=true to override deliberately)",
		manifestType, host, instanceTypeCheckOverrideEnv)
}

func nextBackoff(d time.Duration) time.Duration { return min(d*2, backoffMax) }

func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int64N(int64(d/2)+1))
}

func main() {
	get := os.Getenv
	cfg, err := poolConfig(get)
	if err != nil {
		log.Fatalf("microvm-worker: %v", err)
	}
	// snapDir must be resolved before launcherFor: the Firecracker arm's
	// FirecrackerOptions.SnapshotDir IS this directory (the golden vmstate/memfile/
	// kernel/rootfs/agent/manifest.json set), not cfg.SnapshotDir itself, which is
	// only the parent directory a specific image lives under.
	snapDir := filepath.Join(cfg.SnapshotDir, env(get, "SH_SNAPSHOT_IMAGE", "default"))
	lc, err := launcherFor(cfg.VMM, get, snapDir)
	if err != nil {
		log.Fatalf("microvm-worker: %v", err)
	}
	pool, err := vmpool.New(cfg, lc, vmpool.RealClock())
	if err != nil {
		log.Fatalf("microvm-worker: %v", err)
	}
	defer func() { _ = pool.Close() }()

	// Verify, pin and probe BEFORE the Attach stream opens, because registration IS
	// the live stream (remote-worker/DESIGN.md:29-30): a worker that registers and then
	// discovers it cannot restore has already been given work. Spec §6: fail at start,
	// not on a user's first request.
	man, err := vmpool.LoadManifest(snapDir)
	if err != nil {
		log.Fatalf("microvm-worker: %v", err)
	}
	if err := man.Verify(snapDir); err != nil {
		log.Fatalf("microvm-worker: %v", err)
	}
	if err := verifyInstanceType(get, man.InstanceType, detectHostInstanceType); err != nil {
		log.Fatalf("microvm-worker: %v", err)
	}
	unpin, err := vmpool.PinMemoryFile(filepath.Join(snapDir, "memfile"))
	if err != nil {
		log.Fatalf("microvm-worker: %v", err)
	}
	defer func() { _ = unpin() }()
	if err := pool.Probe(context.Background()); err != nil {
		log.Fatalf("microvm-worker: %v", err)
	}
	log.Printf("microvm-worker: snapshot %s verified and pinned (%s, built %s on %s)",
		man.Image, man.Hash, man.BuiltAt.Format(time.RFC3339), man.InstanceType)

	relayAddr := env(get, "RELAY_ADDR", "localhost:8443")
	token := env(get, "SANDBOX_TOKEN", "dev-token")
	useTLS, err := strconv.ParseBool(env(get, "RELAY_TLS", "false"))
	if err != nil {
		// Same posture as cmd/worker: this gates whether the bearer token crosses the
		// wire in cleartext, so the worker refuses to guess.
		log.Fatalf("microvm-worker: RELAY_TLS=%q is not a boolean", get("RELAY_TLS"))
	}
	maxConcurrent := session.DefaultConcurrency
	if v, e := envInt64(get, "WORKER_MAX_CONCURRENT", int64(session.DefaultConcurrency)); e == nil {
		maxConcurrent = int(v)
	}

	var creds credentials.TransportCredentials
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	} else {
		creds = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(relayAddr, session.DialOptions(creds)...)
	if err != nil {
		log.Fatalf("microvm-worker: dial %s: %v", relayAddr, err)
	}
	defer conn.Close()
	client := pb.NewSandboxWorkerClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; log.Println("microvm-worker: signal received, shutting down"); cancel() }()

	// Capabilities are NOT probed from the worker's own PATH here: the commands run in
	// a guest, so the worker's PATH says nothing about what an Exec can use. They come
	// from the golden snapshot's manifest loaded above instead, which the build script
	// fills by probing INSIDE the guest before snapshotting; an empty manifest field
	// advertises nothing rather than lying.
	sess := session.New(session.Config{
		SandboxID:     env(get, "SANDBOX_ID", "sbx-microvm-1"),
		Image:         env(get, "SANDBOX_IMAGE", ""),
		Trust:         env(get, "SANDBOX_TRUST", "untrusted"),
		Capabilities:  man.Capabilities,
		MaxConcurrent: maxConcurrent,
	}, vmpool.Runner{Pool: pool})

	log.Printf("microvm-worker: relay=%s sandbox_id=%s tls=%v vmm=%s D=%d guest=%dMiB budget=%dMiB",
		relayAddr, env(get, "SANDBOX_ID", "sbx-microvm-1"), useTLS, cfg.VMM,
		cfg.StandbyDepth, cfg.GuestRAMBytes>>20, cfg.MaxCommittedBytes>>20)

	backoff := backoffMin
	for ctx.Err() == nil {
		attachCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		stream, err := client.Attach(attachCtx)
		if err == nil {
			log.Printf("microvm-worker: attached, serving execs")
			start := time.Now()
			err = sess.Serve(attachCtx, stream)
			if time.Since(start) > 30*time.Second {
				backoff = backoffMin
			}
		}
		if ctx.Err() != nil {
			break
		}
		wait := jitter(backoff)
		log.Printf("microvm-worker: stream ended (%v); reconnecting in %s", err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		backoff = nextBackoff(backoff)
	}
	log.Println("microvm-worker: stopped")
}
