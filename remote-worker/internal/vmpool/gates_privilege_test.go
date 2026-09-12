package vmpool

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Spec §8's last gate: "Nothing executes outside a VM — §3.5's privilege property: no
// `bash` child of microvm-worker on any code path."
//
// A runtime test cannot prove a NEGATIVE over all code paths, so this is a static one:
// no file reachable from microvm-worker may spawn a process, with the two deliberate
// exceptions named below. That is a stronger claim than any single execution, and it is
// the claim §3.5's whole privilege argument rests on — the privileged process only
// parses a frame, writes bytes to a vsock, and spawns a VMM with fixed argv.
func TestNothingInTheWorkerPathSpawnsAShell(t *testing.T) {
	// testlauncher.go runs host bash on purpose and is reachable ONLY from vmpoolctl
	// (cmd/microvm-worker's launcherFor refuses it — see its own test). The launchers
	// spawn VMMs with fixed argv, which is exactly what §3.5 permits.
	allowed := map[string]string{
		"testlauncher.go":         "vmpoolctl-only host fake; microvm-worker cannot select it",
		"launcher_firecracker.go": "spawns the jailer with fixed argv (§3.5)",
		"launcher_chv.go":         "spawns cloud-hypervisor and virtiofsd with fixed argv (§3.5)",
		"cgroup.go":               "signals pids, spawns nothing",
	}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := allowed[name]; ok {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return true
			}
			switch sel.Sel.Name {
			case "Command", "CommandContext":
				t.Errorf("%s spawns a process (exec.%s) — §3.5's privilege property is that "+
					"nothing agent-influenced executes outside a VM. If this is a fixed-argv VMM "+
					"spawn, add the file to the allowed map WITH its reason.",
					filepath.Base(fset.Position(sel.Pos()).Filename), sel.Sel.Name)
			}
			return true
		})
	}
}

// The complement: the host-bash fake must never be reachable from the worker. Its own
// test asserts launcherFor refuses it; this asserts the sentinel Kind cannot satisfy a
// Config built for a real arm, so even a wiring mistake fails at New rather than at
// runtime.
func TestTheHostFakeCannotServeARealVMMConfig(t *testing.T) {
	lc := NewFakeLauncher()
	for _, kind := range []VMMKind{Firecracker, CloudHypervisor} {
		_, err := New(Config{
			VMM: kind, SnapshotDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
			MaxRuns: 4, MaxCommittedBytes: 1 << 30,
		}, lc, newFakeClock())
		if err == nil {
			t.Fatalf("New accepted the host fake for VMM=%q", kind)
		}
	}
}
