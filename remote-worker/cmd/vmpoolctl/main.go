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
	"path/filepath"
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
	VMM             string            `json:"vmm"`
	Host            string            `json:"host"`
	Key             string            `json:"key"`
	Command         string            `json:"command"`
	Mode            string            `json:"mode"`
	Iterations      int               `json:"iterations"`
	Concurrency     int               `json:"concurrency"`
	WarmupDiscarded int               `json:"warmup_discarded"`
	Failures        int               `json:"failures"`
	StandbyDepth    int               `json:"standby_depth"`
	GuestRAMMB      int64             `json:"guest_ram_mb"`
	Substrate       string            `json:"substrate"`
	MemfilePinned   bool              `json:"memfile_pinned"`
	Limits          map[string]string `json:"limits"`
	WarmAcquires    uint64            `json:"warm_acquires"`
	ColdAcquires    map[string]uint64 `json:"cold_acquires"`
	Refusals        map[string]uint64 `json:"refusals"`
	P50AcquireUs    int64             `json:"p50_acquire_us"`
	P95AcquireUs    int64             `json:"p95_acquire_us"`
	P50ResumeUs     int64             `json:"p50_resume_us"`
	P95ResumeUs     int64             `json:"p95_resume_us"`
	P50RunUs        int64             `json:"p50_run_us"`
	P95RunUs        int64             `json:"p95_run_us"`
	P50DestroyUs    int64             `json:"p50_destroy_us"`
	P95DestroyUs    int64             `json:"p95_destroy_us"`
	P50TotalUs      int64             `json:"p50_total_us"`
	P95TotalUs      int64             `json:"p95_total_us"`
	WallMs          int64             `json:"wall_ms"`
	CPUChildUs      int64             `json:"cpu_child_us"`
}

// sample holds one measured iteration's phase decomposition. Every mode fills in
// only the fields its own primitive touches — e.g. "replenish" never sets run, and
// leaving the rest at their zero value is exactly what keeps a replenishment rung
// from being misread as a hot-path rung (see TestModeReplenishMeasuresRestoreOnly).
type sample struct{ acquire, resume, run, destroy, total time.Duration }

func main() {
	if err := realMain(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "vmpoolctl: %v\n", err)
		os.Exit(1)
	}
}

func realMain(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("vmpoolctl", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are returned, not printed twice
	defaultSubstrate := os.Getenv("SH_SUBSTRATE")
	if defaultSubstrate == "" {
		defaultSubstrate = "unknown"
	}
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
		mode        = fs.String("mode", "exec",
			"exec | replenish | teardown-inflight | teardown-standby | teardown-bulk (spec §7.2)")
		warmup = fs.Int("warmup", -1,
			"iterations to discard before measuring; -1 means min(iterations/10, 5) (spec §7.5)")
		substrate = fs.String("substrate", defaultSubstrate,
			"substrate label recorded in every record (spec §6); defaults to $SH_SUBSTRATE, else \"unknown\"")
		pinMemfile = fs.Bool("pin-memfile", true,
			"mlock the snapshot memfile before measuring — the density mechanism (spec §7.5, hardware-corrections E5)")
		stdin = fs.String("stdin", "",
			"bytes to feed the command's stdin (mode=exec only). Non-empty forces "+
				"vmpool.Exec.HasStdin, which on a real launcher selects the guest agent's "+
				"freshly-forked-child path rather than the parked bash (spec §5.4) — this is "+
				"how E10 rung 2 prices parked vs fresh, since the guest agent has no "+
				"standalone mode toggle of its own; the choice is per-request, driven by "+
				"whether the request carries stdin")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *mode {
	case "exec", "replenish", "teardown-inflight", "teardown-standby", "teardown-bulk":
	default:
		return fmt.Errorf("--mode=%q is not one of exec, replenish, teardown-inflight, teardown-standby, teardown-bulk", *mode)
	}
	// Join rather than take command[0]: "-- echo hello world" means what it looks
	// like, and an already-quoted single argument still passes through unchanged.
	// Silently running only the first word would let a run measure a different,
	// possibly no-op command while reporting a clean, plausible result. Required in
	// every mode, even the ones that never run it, so every rung's invocation has
	// the same shape and a record always carries what was asked for.
	command := strings.Join(fs.Args(), " ")
	if command == "" {
		return fmt.Errorf("a command is required after --")
	}
	if *iterations < 1 || *concurrency < 1 {
		return fmt.Errorf("--iterations and --concurrency must be >= 1")
	}
	// Spec §7.5: "The first restore differs from the hundredth (page cache, THP,
	// fragmentation). Discard warmup, report steady state." -1 is the "unset"
	// sentinel — 0 is a legitimate, explicit request for no warmup at all.
	warmupN := *warmup
	if warmupN < 0 {
		warmupN = *iterations / 10
		if warmupN > 5 {
			warmupN = 5
		}
	}
	if warmupN >= *iterations {
		return fmt.Errorf("--warmup=%d must be less than --iterations=%d", warmupN, *iterations)
	}

	// perVMBytes is vmpool.PerVMBytes(cfg) (hardware-corrections D1) computed before
	// cfg itself exists below: cfg.VMM is derived from lc.Kind() once lc is built, so
	// building the whole Config first would be circular. Only GuestRAMBytes is needed
	// for the figure — VMOverheadBytes is left at its zero value here exactly as
	// cmd/microvm-worker/main.go's own launcherFor call does (poolConfig there never
	// sets it either), so both binaries compute the identical figure from the
	// identical inputs.
	perVMBytes := vmpool.PerVMBytes(vmpool.Config{GuestRAMBytes: *guestMB << 20})
	lc, err := launcher(*vmm, *snapshotDir, perVMBytes)
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

	// The replenish/teardown-* modes exercise the individual lifecycle primitives
	// through BenchmarkHooks, which is deliberately not part of Pool (nothing in the
	// request path may acquire a VM without Exec's destroy defer). vmpool.New always
	// returns the one concrete *pool type, which implements BenchmarkHooks, so this
	// assertion only fails if that invariant is ever broken — worth a clear error
	// rather than a nil-pointer panic three lines into a mode branch.
	var hooks vmpool.BenchmarkHooks
	if *mode != "exec" {
		h, ok := pool.(vmpool.BenchmarkHooks)
		if !ok {
			return fmt.Errorf("--mode=%s requires vmpool.BenchmarkHooks, but this Pool does not implement it", *mode)
		}
		hooks = h
	}

	host, _ := os.Hostname()
	res := runResult{
		VMM: *vmm, Host: host, Key: *key, Command: command, Mode: *mode,
		Iterations: *iterations, Concurrency: *concurrency,
		StandbyDepth: cfg.StandbyDepth, GuestRAMMB: *guestMB,
		Substrate: *substrate,
		Limits:    gatherLimits(),
	}

	// --pin-memfile: mlock the snapshot's memory file so restores across VMs share
	// pages (spec §7.5, hardware-corrections E5 — "say what was actually done").
	// Non-fatal: MemfilePinned staying false is itself the record of what happened,
	// exactly like a bound RLIMIT_MEMLOCK — a silently-failed pin must never look
	// like a successful one.
	if *pinMemfile {
		if unpin, pinErr := vmpool.PinMemoryFile(filepath.Join(*snapshotDir, "memfile")); pinErr == nil {
			res.MemfilePinned = true
			defer func() { _ = unpin() }()
		}
	}

	// CPUChildUs: RUSAGE_CHILDREN's delta across the measured window. The VMMs this
	// binary launches (Firecracker's jailer, cloud-hypervisor) are children of this
	// process, so their CPU IS replenishment's CPU cost (spec §7.2, hardware-
	// corrections brief step 2). Non-fatal on error (e.g. windows): CPUChildUs stays
	// 0, which is diagnostic-only and never load-bearing for the rest of the record.
	cpuBefore, cpuBeforeErr := childCPUUsage()

	var firstErr error
	var samples []sample
	switch *mode {
	case "exec":
		samples, firstErr = runExecMode(pool, *key, command, []byte(*stdin), *timeoutS, *iterations, warmupN, *concurrency, &res)
	case "replenish":
		samples, firstErr = runReplenishMode(hooks, *key, *iterations, warmupN, &res)
	case "teardown-inflight", "teardown-standby":
		samples, firstErr = runTeardownPerVMMode(hooks, *mode, *key, *wsRoot, *iterations, warmupN, &res)
	case "teardown-bulk":
		samples, firstErr = runTeardownBulkMode(hooks, *key, *iterations, *depth, &res)
	default:
		// Guards DIVERGENCE between two lists that must agree: the flag validator's
		// accepted set (see the switch near the top of realMain) and this dispatcher's
		// cases. A mode added to the validator but not here would pass validation and
		// then fall straight through, leaving `samples` nil so every percentile computed
		// as 0 — while the process still exited 0. That record is indistinguishable from
		// a real rung whose timings fell below clock resolution, which is the same shape
		// as E11's mem_available_bytes defect (a sweep that wrote zero records and
		// exited 0). Unreachable while the two lists agree; TestEveryValidModeIsDispatched
		// is what keeps them agreeing, and this is what makes the disagreement loud
		// instead of silent.
		return fmt.Errorf("vmpoolctl: --mode %q passed validation but is not dispatched "+
			"(the validator's mode list and realMain's dispatch switch have diverged)", *mode)
	}
	res.WarmupDiscarded = warmupN

	if cpuAfter, cpuAfterErr := childCPUUsage(); cpuBeforeErr == nil && cpuAfterErr == nil {
		res.CPUChildUs = cpuAfter - cpuBefore
	}

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
		fmt.Fprintf(stdout, "vmm=%s mode=%s key=%s iters=%d warmup=%d conc=%d failures=%d\n",
			res.VMM, res.Mode, res.Key, res.Iterations, res.WarmupDiscarded, res.Concurrency, res.Failures)
		fmt.Fprintf(stdout, "acquire p50=%dus p95=%dus  resume p50=%dus p95=%dus  run p50=%dus p95=%dus  destroy p50=%dus p95=%dus  total p50=%dus p95=%dus\n",
			res.P50AcquireUs, res.P95AcquireUs, res.P50ResumeUs, res.P95ResumeUs, res.P50RunUs, res.P95RunUs, res.P50DestroyUs, res.P95DestroyUs, res.P50TotalUs, res.P95TotalUs)
		fmt.Fprintf(stdout, "cpu_child=%dus memfile_pinned=%v substrate=%s\n", res.CPUChildUs, res.MemfilePinned, res.Substrate)
		fmt.Fprintf(stdout, "warm=%d cold=%v refusals=%v limits=%v\n", res.WarmAcquires, res.ColdAcquires, res.Refusals, res.Limits)
	}
	// A failed Exec is reported in the record AND as a non-zero exit, so a driver
	// that ignores the JSON still notices.
	return firstErr
}

// runExecMode is E10 rungs 1/2's shape: the pool owns acquire/run/destroy
// internally, so the CLI times the whole Exec and reports the decomposition the
// pool exposes through its phase callbacks — never a guess. See vmpool.Phases. The
// first warmupN iterations run sequentially and are discarded before the measured,
// concurrent loop begins; ReqIDs are offset by warmupN so every ReqID stays unique
// across the whole invocation.
//
// stdin, when non-empty, is carried on every Exec so vmpool.Exec.HasStdin (derived
// as len(Stdin) > 0 in guestconn.go) is true for the whole run — on a real launcher
// this selects the guest agent's freshly-forked-child path rather than the parked
// bash it would otherwise reuse (spec §5.4). This is the flag E10's rung 2 uses
// twice: once with stdin empty (parked bash) and once with it set (fresh
// `bash -c`), pricing the one distinction §5.4 draws.
func runExecMode(pool vmpool.Pool, key, command string, stdin []byte, timeoutS uint, iterations, warmupN, concurrency int, res *runResult) ([]sample, error) {
	for i := 0; i < warmupN; i++ {
		ph := &vmpool.Phases{}
		_, _ = pool.ExecPhased(context.Background(), key, vmpool.Exec{
			ReqID: uint64(i + 1), Command: command, Stdin: stdin, TimeoutS: uint32(timeoutS), Streaming: true,
		}, discardSink{}, ph)
	}

	measured := iterations - warmupN
	samples := make([]sample, measured)
	var firstErr error
	var mu sync.Mutex
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < measured; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			var s sample
			t0 := time.Now()
			ph := &vmpool.Phases{}
			_, err := pool.ExecPhased(context.Background(), key, vmpool.Exec{
				ReqID: uint64(warmupN + i + 1), Command: command, Stdin: stdin, TimeoutS: uint32(timeoutS), Streaming: true,
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
	return samples, firstErr
}

// runReplenishMode is E10 rung 3: spawn -> restore -> pause -> ready, timed on its
// own via RestoreOne, with none of ExecPhased's acquire bookkeeping folded in. The
// restored VM is destroyed immediately after each measured sample, but OUTSIDE the
// timed window — replenishment cost is what is being priced, not teardown (rung 4's
// job). No command ever runs, so run/resume/destroy all stay at their zero value on
// every sample: reporting a run figure here would invite reading this rung as a hot
// path rung.
func runReplenishMode(hooks vmpool.BenchmarkHooks, key string, iterations, warmupN int, res *runResult) ([]sample, error) {
	for i := 0; i < warmupN; i++ {
		if vm, err := hooks.RestoreOne(context.Background(), key); err == nil {
			_ = vm.Destroy()
		}
	}

	measured := iterations - warmupN
	samples := make([]sample, measured)
	var firstErr error
	start := time.Now()
	for i := 0; i < measured; i++ {
		t0 := time.Now()
		vm, err := hooks.RestoreOne(context.Background(), key)
		d := time.Since(t0)
		samples[i].acquire = d
		samples[i].total = d
		if err != nil {
			res.Failures++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_ = vm.Destroy()
	}
	res.WallMs = time.Since(start).Milliseconds()
	return samples, firstErr
}

// runTeardownPerVMMode is two of E10 rung 4's three variants (hardware-corrections
// brief step 2's "in-flight" and "paused standby"): SIGKILL to reaped, timed on its
// own. "teardown-standby" destroys a VM RestoreOne left paused, exactly as it comes
// back from Restore — never resumed, matching the standby state a parked run's VMs
// actually sit in. "teardown-inflight" additionally Resumes the VM first (outside
// the timed window) so the destroy being measured is of an active VM, the state a
// VM serving a live Exec is torn down from — pricing the one term "the sub-15ms
// literature assumes is free" (spec §7.2) in both of the states it actually occurs.
//
// The timed window also removes the run's workspace directory. On a real launcher,
// VM.Destroy already does equivalent I/O internally (SIGKILL, reap, then remove the
// per-VM jail root — see launcher_firecracker.go / launcher_chv.go's own Destroy),
// so this adds nothing there. Under --vmm=fake there is no process to kill and no
// jail root to remove — measured directly, a bare fakeHostVM.Destroy is a mutex
// lock and a map delete, consistently under 1us (well below what time.Duration.
// Microseconds' truncation can represent) and would report a false, misleading zero
// for the very rung meant to price "the one term the sub-15ms literature assumes is
// free." Folding the per-key workspace's real removal into the same window keeps the
// fake arm honest about there being real reclaim work, without it ever running on
// the two real launchers' own already-realistic number. The directory is recreated
// by the next iteration's RestoreOne (via ensureWorkspace) exactly as production
// does after any Reclaim, so this is self-consistent across iterations.
func runTeardownPerVMMode(hooks vmpool.BenchmarkHooks, mode, key, wsRoot string, iterations, warmupN int, res *runResult) ([]sample, error) {
	resumeFirst := mode == "teardown-inflight"
	dir := filepath.Join(filepath.Clean(wsRoot), key)
	warm := func() {
		vm, err := hooks.RestoreOne(context.Background(), key)
		if err != nil {
			return
		}
		if resumeFirst {
			_ = vm.Resume(context.Background())
		}
		_ = vm.Destroy()
		_ = os.RemoveAll(dir)
	}
	for i := 0; i < warmupN; i++ {
		warm()
	}

	measured := iterations - warmupN
	samples := make([]sample, measured)
	var firstErr error
	start := time.Now()
	for i := 0; i < measured; i++ {
		vm, err := hooks.RestoreOne(context.Background(), key)
		if err != nil {
			res.Failures++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if resumeFirst {
			_ = vm.Resume(context.Background())
		}
		t0 := time.Now()
		if err := vm.Destroy(); err != nil {
			res.Failures++
			if firstErr == nil {
				firstErr = err
			}
		}
		_ = os.RemoveAll(dir)
		d := time.Since(t0)
		samples[i].destroy = d
		samples[i].total = d
	}
	res.WallMs = time.Since(start).Milliseconds()
	return samples, firstErr
}

// runTeardownBulkMode is E10 rung 4's third variant: a bulk reclaim of D x R
// standbys in one DestroyAllStandbys call, because "the per-VM number does not
// predict" the sweep's bulk reclaim (spec §7.2) and this rung exists to price that
// sweep directly rather than as R separate single-VM destroys. D is --standby-depth,
// the same knob production Config uses; R is --iterations, reused rather than given
// a dedicated flag so this mode's invocation looks like every other mode's. R
// synthetic keys are derived from --key (FillStandbys is per-key) so the batch
// spans multiple run pools exactly as a real sweep's bulk reclaim would, rather
// than D standbys under one key alone.
//
// This is a one-shot batch primitive, not a per-iteration one: DestroyAllStandbys
// reclaims everything pool-wide in a single call, so there is no warmup concept to
// discard and no meaningful way to repeat it within one invocation (a second call
// would find nothing left to destroy). res.WarmupDiscarded is left at 0 by the
// caller for this mode; that is a deliberate, documented choice, not an oversight.
func runTeardownBulkMode(hooks vmpool.BenchmarkHooks, key string, r, depth int, res *runResult) ([]sample, error) {
	var firstErr error
	for i := 0; i < r; i++ {
		subKey := fmt.Sprintf("%s-bulk%d", key, i)
		if err := hooks.FillStandbys(context.Background(), subKey, depth); err != nil {
			res.Failures++
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	start := time.Now()
	_, err := hooks.DestroyAllStandbys(context.Background())
	d := time.Since(start)
	res.WallMs = d.Milliseconds()
	if err != nil {
		res.Failures++
		if firstErr == nil {
			firstErr = err
		}
	}
	return []sample{{destroy: d, total: d}}, firstErr
}

// launcher maps --vmm to a Launcher. "fake" is handled here and ONLY here — it must
// never be reachable from cmd/microvm-worker/main.go's launcherFor (spec §3.3, §3.5:
// nothing agent-influenced may execute outside a VM, and microvm-worker runs
// privileged). Firecracker and CloudHypervisor both delegate to
// vmpool.LauncherFromEnv, reading the SAME env vars cmd/microvm-worker/main.go's
// launcherFor does, so E10's driver measures the production configuration rather
// than a CLI-only variant, and so a fix to one arm's wiring (e.g. round 3's
// CloudHypervisor fix) cannot land in one binary's copy of this switch and not the
// other's — see LauncherFromEnv's doc comment for why round 9 exists at all.
//
// Fix round 9 (Task 16): the CloudHypervisor case below used to be a hardcoded
// "--vmm=%s is not wired yet (Phase D)" error — the exact defect round 3 had already
// fixed in cmd/microvm-worker/main.go's launcherFor, recurring here because this
// file's copy of the switch was never updated when that fix landed. Grepping the repo
// for "Phase D" and for any other --vmm/SH_VMM switch turned up exactly one other
// production call site (main.go's launcherFor, already correct) and one test-only
// helper (internal/vmpool/gates_kvm_test.go's launcherForArm, which already called
// NewCloudHypervisorLauncher directly and never carried this placeholder) — no third
// site is left uninspected.
func launcher(kind string, snapshotDir string, perVMBytes int64) (vmpool.Launcher, error) {
	switch kind {
	case "fake":
		return vmpool.NewFakeLauncher(), nil
	case string(vmpool.Firecracker), string(vmpool.CloudHypervisor):
		// "vmpoolctl" differs from main.go's "microvm-worker" so the default RunDir
		// vmpool.LauncherFromEnv derives (chvDefaultRunDir, fix round 10 — a
		// same-device sibling of snapshotDir, not a hardcoded /run/... path any more)
		// never collides with microvm-worker's own default when this CLI is run for
		// diagnostics on the same host as a live microvm-worker daemon pointed at the
		// same snapshot; SH_CHV_RUN_DIR still overrides either the same way.
		return vmpool.LauncherFromEnv(vmpool.VMMKind(kind), os.Getenv, snapshotDir, perVMBytes, "vmpoolctl")
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
