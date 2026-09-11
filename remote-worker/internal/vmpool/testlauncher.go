package vmpool

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
)

// FakeLauncher runs commands with `bash -c` ON THE HOST instead of in a VM. It
// exists so vmpoolctl and the E10 driver can be exercised end to end — flag
// parsing, JSON contract, concurrency — on a laptop with no /dev/kvm.
//
// IT IS NOT A SANDBOX AND MUST NEVER BE REACHABLE FROM microvm-worker. Spec §3.5's
// privilege argument rests on "nothing agent-influenced ever executes outside a
// VM," so a fake that runs agent-authored commands in the worker's own namespace
// would be strictly worse than today's container. microvm-worker therefore has no
// flag that selects it, §8's "nothing executes outside a VM" gate pins that
// statically over this package, and it is selectable only by vmpoolctl's
// --vmm=fake — a separate binary that never runs agent-influenced input.
//
// This lives in a non-test file (not launcher_fake_test.go's internal fakeLauncher)
// because vmpoolctl, a real binary, must import it.
type FakeLauncher struct {
	mu   sync.Mutex
	live map[string]struct{}
}

// NewFakeLauncher returns a Launcher usable only by vmpoolctl (see the type doc).
func NewFakeLauncher() *FakeLauncher { return &FakeLauncher{live: map[string]struct{}{}} }

// Kind reports FakeVMM — the same sentinel Config.Normalize already accepts —
// which is NOT one of the two real arms, so a Config naming a real hypervisor can
// never be served by this launcher: New compares the two and refuses a mismatch.
func (l *FakeLauncher) Kind() VMMKind { return FakeVMM }

func (l *FakeLauncher) Restore(ctx context.Context, req RestoreRequest) (VM, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live[req.ID] = struct{}{}
	return &fakeHostVM{lc: l, id: req.ID, key: req.Key, dir: req.WorkspaceDir}, nil
}

// LiveCount is the number of VMs restored but not yet destroyed.
func (l *FakeLauncher) LiveCount() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.live) }

type fakeHostVM struct {
	lc           *FakeLauncher
	id, key, dir string
	mu           sync.Mutex
	resumed      bool
	ran          bool
}

func (v *fakeHostVM) Key() string { return v.key }

func (v *fakeHostVM) Resume(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.resumed = true
	return nil
}

func (v *fakeHostVM) Run(ctx context.Context, c Command, out Sink) (Result, error) {
	v.mu.Lock()
	if !v.resumed {
		v.mu.Unlock()
		return Result{}, fmt.Errorf("fake VM %s: Run before Resume", v.id)
	}
	if v.ran {
		v.mu.Unlock()
		return Result{}, fmt.Errorf("fake VM %s: a VM must serve exactly one Exec", v.id)
	}
	v.ran = true
	v.mu.Unlock()

	cmd := exec.CommandContext(ctx, "bash", "-c", c.Command)
	cmd.Dir = v.dir
	stdout, err := cmd.Output()
	if len(stdout) > 0 {
		out.Stdout(stdout)
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return Result{ExitCode: 0}, nil
	case asExitError(err, &ee):
		if len(ee.Stderr) > 0 {
			out.Stderr(ee.Stderr)
		}
		return Result{ExitCode: int32(ee.ExitCode())}, nil
	default:
		return Result{}, err
	}
}

func (v *fakeHostVM) Destroy() error {
	v.lc.mu.Lock()
	defer v.lc.mu.Unlock()
	delete(v.lc.live, v.id)
	return nil
}

func asExitError(err error, target **exec.ExitError) bool { return errors.As(err, target) }
