package vmpool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Files a snapshot directory must contain. vmstate and memfile are what the VMM
// loads; kernel, rootfs and agent are kept alongside so the hash covers what actually
// determines guest behaviour rather than only the serialized state.
const (
	fileVMState  = "vmstate"
	fileMemory   = "memfile"
	fileKernel   = "kernel"
	fileRootfs   = "rootfs"
	fileAgent    = "agent"
	fileManifest = "manifest.json"
)

// Manifest pins one golden snapshot.
//
// InstanceType is not decoration: restore requires IDENTICAL hardware and software
// (spec §2.4), so the snapshot is instance-generation-specific and must be built on
// the type that will run it. Recording it is also spec §6's "record the substrate in
// every run record" at the artifact level — a nested-vs-metal divergence is
// undiagnosable without it.
type Manifest struct {
	Image        string    `json:"image"`
	VMM          string    `json:"vmm"`
	InstanceType string    `json:"instance_type"`
	Kernel       string    `json:"kernel_release"`
	GuestRAMMB   int64     `json:"guest_ram_mb"`
	Capabilities []string  `json:"capabilities"`
	BuiltAt      time.Time `json:"built_at"`

	KernelSHA256 string `json:"kernel_sha256"`
	RootfsSHA256 string `json:"rootfs_sha256"`
	AgentSHA256  string `json:"agent_sha256"`
	// Hash is over kernel + rootfs + agent, in that order (spec §5.5). It does NOT
	// cover vmstate or memfile: those are derived from the three, are enormous, and
	// change on every rebuild even when nothing behavioural did.
	Hash string `json:"hash"`
}

// Fill computes the component digests and Hash from the files in dir.
func (m *Manifest) Fill(dir string) error {
	var err error
	if m.KernelSHA256, err = fileSHA256(filepath.Join(dir, fileKernel)); err != nil {
		return err
	}
	if m.RootfsSHA256, err = fileSHA256(filepath.Join(dir, fileRootfs)); err != nil {
		return err
	}
	if m.AgentSHA256, err = fileSHA256(filepath.Join(dir, fileAgent)); err != nil {
		return err
	}
	if m.BuiltAt.IsZero() {
		m.BuiltAt = time.Now().UTC()
	}
	sort.Strings(m.Capabilities)
	m.Hash = m.ComputeHash()
	return nil
}

func (m Manifest) ComputeHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "kernel:%s\nrootfs:%s\nagent:%s\n", m.KernelSHA256, m.RootfsSHA256, m.AgentSHA256)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func LoadManifest(dir string) (Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, fileManifest))
	if err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", dir, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: manifest: %w", dir, err)
	}
	return m, nil
}

// Verify re-hashes the components on disk and compares against the manifest.
//
// LOUDLY, and naming the component that drifted. Spec §5.5's failure mode is a stale
// snapshot silently serving an old toolchain — which surfaces as a workload failing
// for reasons nothing in the run record explains. Because only a 64-bit CRC guards the
// state file and the VMM trusts these files (spec §2.4), this check is the trust
// boundary and it deliberately runs OUTSIDE the VMM.
func (m Manifest) Verify(dir string) error {
	for _, f := range []string{fileVMState, fileMemory, fileKernel, fileRootfs, fileAgent} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return fmt.Errorf("snapshot %s: %s: %w", dir, f, err)
		}
	}
	for _, c := range []struct {
		name string
		want string
	}{
		{fileKernel, m.KernelSHA256},
		{fileRootfs, m.RootfsSHA256},
		{fileAgent, m.AgentSHA256},
	} {
		got, err := fileSHA256(filepath.Join(dir, c.name))
		if err != nil {
			return err
		}
		if got != c.want {
			return fmt.Errorf("snapshot %s: %s drifted: manifest says %s, on disk %s — "+
				"rebuild the snapshot rather than running an old toolchain (spec §5.5)", dir, c.name, c.want, got)
		}
	}
	if got := m.ComputeHash(); got != m.Hash {
		return fmt.Errorf("snapshot %s: manifest hash %s does not match its own components (%s)", dir, m.Hash, got)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// probeKey is reserved and can never collide with a wire-supplied workspace_key:
// checkKey requires the first character to be alphanumeric.
const probeKey = "_probe"

// Probe restores one VM, runs a trivial command in it and destroys it.
//
// Spec §6's last row: the pinned hash is not sufficient, because a snapshot can be
// intact and still unrestorable on this host — wrong instance generation, cgroups v1,
// no /dev/kvm, a jailer misconfiguration. So the unit fails at START rather than on a
// user's first request. It bypasses checkKey deliberately, using a reserved key that
// no wire value can spell.
func (p *pool) Probe(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	dir := filepath.Join(p.cfg.WorkspaceRoot, probeKey)
	id := p.nextIDLocked()
	p.mu.Unlock()

	vm, err := p.warm(ctx, probeKey, dir, id)
	if err != nil {
		return fmt.Errorf("startup probe: could not restore a VM: %w", err)
	}
	defer func() {
		if err := vm.Destroy(); err != nil {
			p.counters.destroyFailed()
		}
		_ = removeWorkspace(dir)
	}()
	if err := vm.Resume(ctx); err != nil {
		return fmt.Errorf("startup probe: could not resume: %w", err)
	}
	res, err := vm.Run(ctx, Command{Command: "true", TimeoutS: 30, CapBytes: OutputCapBytes}, discardingSink{})
	if err != nil {
		return fmt.Errorf("startup probe: guest did not answer: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("startup probe: `true` exited %d in the guest", res.ExitCode)
	}
	return nil
}

type discardingSink struct{}

func (discardingSink) Stdout([]byte) {}
func (discardingSink) Stderr([]byte) {}
