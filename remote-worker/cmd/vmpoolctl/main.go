// Command vmpoolctl drives vmpool directly — no relay, no harness, no protocol
// confound (spec §3.1). It is E10's driver: its rungs are terms, not concurrency, so
// it reports the hot path decomposed into acquire / run / destroy rather than one
// round-trip number.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kagenti/serverless-harness/remote-worker/internal/vmpool"
)

// runResult is the record E10's shell driver parses. Every field exists because a
// rung is unreportable without it; spec §6 additionally requires the substrate
// recorded in every run record, which is what VMM and Host are for.
type runResult struct {
	VMM          string            `json:"vmm"`
	Host         string            `json:"host"`
	Key          string            `json:"key"`
	Command      string            `json:"command"`
	Iterations   int               `json:"iterations"`
	Concurrency  int               `json:"concurrency"`
	Failures     int               `json:"failures"`
	StandbyDepth int               `json:"standby_depth"`
	GuestRAMMB   int64             `json:"guest_ram_mb"`
	WarmAcquires uint64            `json:"warm_acquires"`
	ColdAcquires map[string]uint64 `json:"cold_acquires"`
	Refusals     map[string]uint64 `json:"refusals"`
	P50AcquireUs int64             `json:"p50_acquire_us"`
	P95AcquireUs int64             `json:"p95_acquire_us"`
	P50ResumeUs  int64             `json:"p50_resume_us"`
	P95ResumeUs  int64             `json:"p95_resume_us"`
	P50RunUs     int64             `json:"p50_run_us"`
	P95RunUs     int64             `json:"p95_run_us"`
	P50DestroyUs int64             `json:"p50_destroy_us"`
	P95DestroyUs int64             `json:"p95_destroy_us"`
	P50TotalUs   int64             `json:"p50_total_us"`
	P95TotalUs   int64             `json:"p95_total_us"`
	WallMs       int64             `json:"wall_ms"`
}

func main() {
	if err := realMain(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "vmpoolctl: %v\n", err)
		os.Exit(1)
	}
}

func realMain(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("vmpoolctl", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are returned, not printed twice
	var (
		vmm         = fs.String("vmm", "fake", "cloud-hypervisor | firecracker | fake (host bash, NOT a sandbox)")
		snapshotDir = fs.String("snapshot-dir", "", "directory holding the golden snapshot")
		wsRoot      = fs.String("workspace-root", "", "directory holding per-run workspaces")
		key         = fs.String("key", "", "workspace_key to run under")
		depth       = fs.Int("standby-depth", vmpool.DefaultStandbyDepth, "D")
		guestMB     = fs.Int64("guest-ram-mb", vmpool.DefaultGuestRAMBytes>>20, "guest RAM per VM, MiB")
		maxRuns     = fs.Int("max-runs", 64, "MaxRuns backstop")
		committedMB = fs.Int64("max-committed-mb", 32<<10, "MaxCommittedBytes, MiB")
		iterations  = fs.Int("iterations", 1, "how many Execs to run")
		concurrency = fs.Int("concurrency", 1, "how many Execs in flight at once")
		timeoutS    = fs.Uint("timeout-s", 30, "per-Exec timeout")
		asJSON      = fs.Bool("json", false, "emit one runResult JSON record")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Join rather than take command[0]: "-- echo hello world" means what it looks
	// like, and an already-quoted single argument still passes through unchanged.
	// Silently running only the first word would let a run measure a different,
	// possibly no-op command while reporting a clean, plausible result.
	command := strings.Join(fs.Args(), " ")
	if command == "" {
		return fmt.Errorf("a command is required after --")
	}
	if *iterations < 1 || *concurrency < 1 {
		return fmt.Errorf("--iterations and --concurrency must be >= 1")
	}

	lc, err := launcher(*vmm)
	if err != nil {
		return err
	}
	cfg := vmpool.Config{
		VMM:               lc.Kind(),
		SnapshotDir:       *snapshotDir,
		WorkspaceRoot:     *wsRoot,
		StandbyDepth:      *depth,
		GuestRAMBytes:     *guestMB << 20,
		MaxRuns:           *maxRuns,
		MaxCommittedBytes: *committedMB << 20,
	}
	pool, err := vmpool.New(cfg, lc, vmpool.RealClock())
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	host, _ := os.Hostname()
	res := runResult{
		VMM: *vmm, Host: host, Key: *key, Command: command,
		Iterations: *iterations, Concurrency: *concurrency,
		StandbyDepth: cfg.StandbyDepth, GuestRAMMB: *guestMB,
	}

	type sample struct{ acquire, resume, run, destroy, total time.Duration }
	samples := make([]sample, *iterations)
	var firstErr error
	var mu sync.Mutex
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < *iterations; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			var s sample
			t0 := time.Now()
			// The pool owns acquire/run/destroy internally, so the CLI times the whole
			// Exec and reports the decomposition the pool exposes through its phase
			// callbacks — never a guess. See vmpool.Phases.
			ph := &vmpool.Phases{}
			_, err := pool.ExecPhased(context.Background(), *key, vmpool.Exec{
				ReqID: uint64(i + 1), Command: command, TimeoutS: uint32(*timeoutS), Streaming: true,
			}, discardSink{}, ph)
			s.total = time.Since(t0)
			s.acquire, s.resume, s.run, s.destroy = ph.Acquire, ph.Resume, ph.Run, ph.Destroy
			mu.Lock()
			samples[i] = s
			if err != nil {
				res.Failures++
				if firstErr == nil {
					firstErr = err
				}
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	res.WallMs = time.Since(start).Milliseconds()

	res.P50AcquireUs, res.P95AcquireUs = pct(samples, func(s sample) time.Duration { return s.acquire })
	res.P50ResumeUs, res.P95ResumeUs = pct(samples, func(s sample) time.Duration { return s.resume })
	res.P50RunUs, res.P95RunUs = pct(samples, func(s sample) time.Duration { return s.run })
	res.P50DestroyUs, res.P95DestroyUs = pct(samples, func(s sample) time.Duration { return s.destroy })
	res.P50TotalUs, res.P95TotalUs = pct(samples, func(s sample) time.Duration { return s.total })

	st := pool.Stats()
	res.WarmAcquires = st.WarmAcquires
	res.ColdAcquires = map[string]uint64{}
	for k, v := range st.ColdAcquires {
		res.ColdAcquires[string(k)] = v
	}
	res.Refusals = map[string]uint64{}
	for k, v := range st.Refusals {
		res.Refusals[string(k)] = v
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(stdout, "vmm=%s key=%s iters=%d conc=%d failures=%d\n", res.VMM, res.Key, res.Iterations, res.Concurrency, res.Failures)
		fmt.Fprintf(stdout, "acquire p50=%dus p95=%dus  resume p50=%dus p95=%dus  run p50=%dus p95=%dus  destroy p50=%dus p95=%dus  total p50=%dus p95=%dus\n",
			res.P50AcquireUs, res.P95AcquireUs, res.P50ResumeUs, res.P95ResumeUs, res.P50RunUs, res.P95RunUs, res.P50DestroyUs, res.P95DestroyUs, res.P50TotalUs, res.P95TotalUs)
		fmt.Fprintf(stdout, "warm=%d cold=%v refusals=%v\n", res.WarmAcquires, res.ColdAcquires, res.Refusals)
	}
	// A failed Exec is reported in the record AND as a non-zero exit, so a driver
	// that ignores the JSON still notices.
	return firstErr
}

func launcher(kind string) (vmpool.Launcher, error) {
	switch kind {
	case "fake":
		return vmpool.NewFakeLauncher(), nil
	case string(vmpool.Firecracker), string(vmpool.CloudHypervisor):
		// Task 15/16 replace this with the real constructors; until then the CLI is
		// honest about what it cannot do rather than silently running host bash.
		return nil, fmt.Errorf("--vmm=%s is not wired yet (Phase D); use --vmm=fake off a KVM host", kind)
	default:
		return nil, fmt.Errorf("--vmm=%q is not one of cloud-hypervisor, firecracker, fake", kind)
	}
}

type discardSink struct{}

func (discardSink) Stdout([]byte) {}
func (discardSink) Stderr([]byte) {}

// pct returns p50 and p95 in microseconds. Nearest-rank on a sorted copy: with the
// small sample counts E10's rungs use, interpolation would invent precision.
func pct[T any](xs []T, get func(T) time.Duration) (p50, p95 int64) {
	if len(xs) == 0 {
		return 0, 0
	}
	ds := make([]time.Duration, len(xs))
	for i, x := range xs {
		ds[i] = get(x)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	at := func(q float64) int64 {
		i := int(q*float64(len(ds)-1) + 0.5)
		return ds[i].Microseconds()
	}
	return at(0.50), at(0.95)
}
