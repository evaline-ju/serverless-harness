package vmpool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func chvOpts(t *testing.T) CHVOptions {
	t.Helper()
	return CHVOptions{
		SnapshotDir:  envOr("SH_SNAPSHOT_IMAGE_DIR", t.TempDir()),
		CHVBin:       envOr("SH_CHV_BIN", "/usr/bin/cloud-hypervisor"),
		ChRemoteBin:  envOr("SH_CH_REMOTE_BIN", "/usr/bin/ch-remote"),
		VirtiofsdBin: envOr("SH_VIRTIOFSD_BIN", "/usr/libexec/virtiofsd"),
		RunDir:       t.TempDir(),
		VirtiofsdUID: 65534, // nobody
		VirtiofsdGID: 65534,
		VsockPort:    1024,
	}
}

func TestCloudHypervisorDoesNotSerializeExecsPerRun(t *testing.T) {
	lc, err := NewCloudHypervisorLauncher(chvOpts(t))
	if err != nil {
		t.Fatalf("NewCloudHypervisorLauncher: %v", err)
	}
	if lc.Kind() != CloudHypervisor {
		t.Fatalf("Kind = %q", lc.Kind())
	}
	// Spec §4.3's decisive row: with virtio-fs the host filesystem arbitrates, so D>1
	// standbys and concurrent Execs per run are both fine. This is the whole reason the
	// VMM is a seam rather than a build choice.
	if lc.SerializesExecsPerRun() {
		t.Fatal("the Cloud Hypervisor arm must NOT serialize Execs per run")
	}
}

func TestVirtiofsdIsNeverRunAsRoot(t *testing.T) {
	opts := chvOpts(t)
	opts.VirtiofsdUID, opts.VirtiofsdGID = 0, 0
	// Spec §3.5: with virtio-fs, guest path resolution happens in virtiofsd on the HOST,
	// so it is the confinement boundary for the whole design — a root virtiofsd
	// compromise would be host root and would render the microVM boundary decorative.
	// Refusing at construction is the only place this can be enforced once and for all.
	if _, err := NewCloudHypervisorLauncher(opts); err == nil {
		t.Fatal("NewCloudHypervisorLauncher accepted VirtiofsdUID=0")
	}
}

func TestVirtiofsdArgvCarriesItsSandbox(t *testing.T) {
	// A pure argv assertion: --sandbox=namespace is virtiofsd's own confinement, and
	// spec §6's malicious-symlink row says it must be "configured and verified, never
	// assumed".
	argv := virtiofsdArgv(chvOpts(t), "/run/vfsd-1.sock", "/srv/workspaces/run-a")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--socket-path=/run/vfsd-1.sock", "--shared-dir=/srv/workspaces/run-a", "--sandbox=namespace", "--cache=never"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q is missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "--sandbox=none") {
		t.Error("--sandbox=none disables the boundary the whole design leans on")
	}
	// Corrections C1: --cache=auto disconnects the virtio-fs session immediately on the
	// installed virtiofsd build; --cache=never is the one that stays up. Guard against a
	// regression back to the brief's original (wrong) default.
	if strings.Contains(joined, "--cache=auto") {
		t.Error("--cache=auto is a known dead end (disconnects immediately) — must not appear in argv")
	}
}

func TestCloudHypervisorRestoresPausedAndRunsOneCommand(t *testing.T) {
	requireKVM(t)
	if _, err := exec.LookPath(chvOpts(t).CHVBin); err != nil {
		t.Skipf("cloud-hypervisor not installed: %v", err)
	}
	lc, err := NewCloudHypervisorLauncher(chvOpts(t))
	if err != nil {
		t.Fatalf("NewCloudHypervisorLauncher: %v", err)
	}
	dir := t.TempDir()
	vm, err := lc.Restore(context.Background(), RestoreRequest{
		ID: "vm-chv-1", Key: "run-a", WorkspaceDir: dir, GuestRAMBytes: 256 << 20,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	defer func() { _ = vm.Destroy() }()
	if err := vm.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	var out capturingSink
	if _, err := vm.Run(context.Background(), Command{Command: "echo hi > f; cat f", TimeoutS: 30, CapBytes: OutputCapBytes}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.out() != "hi\n" {
		t.Fatalf("stdout = %q", out.out())
	}
	// virtio-fs means the HOST filesystem is the authority, so the write is already
	// durable with no sync anywhere — spec §4.3's "nothing is lost" row, which is also
	// why the write-durability gate cannot fail on this arm for a missing sync.
	if b, err := os.ReadFile(dir + "/f"); err != nil || string(b) != "hi\n" {
		t.Fatalf("host-side file = %q err=%v", b, err)
	}
}

func TestCloudHypervisorSupportsTwoStandbysForOneRun(t *testing.T) {
	requireKVM(t)
	if _, err := exec.LookPath(chvOpts(t).CHVBin); err != nil {
		t.Skipf("cloud-hypervisor not installed: %v", err)
	}
	lc, _ := NewCloudHypervisorLauncher(chvOpts(t))
	dir := t.TempDir()
	// The row that decides the arm: D>1 pre-mounted standbys, which the Firecracker arm
	// cannot have at all (spec §4.3).
	var vms []VM
	for _, id := range []string{"vm-chv-a", "vm-chv-b"} {
		vm, err := lc.Restore(context.Background(), RestoreRequest{ID: id, Key: "run-a", WorkspaceDir: dir, GuestRAMBytes: 256 << 20})
		if err != nil {
			t.Fatalf("Restore %s: %v", id, err)
		}
		defer func() { _ = vm.Destroy() }()
		vms = append(vms, vm)
	}
	for i, vm := range vms {
		if err := vm.Resume(context.Background()); err != nil {
			t.Fatalf("Resume %d: %v", i, err)
		}
	}
	// Both resumed, both mounted, no corruption: run in each, concurrently.
	errs := make(chan error, len(vms))
	for i, vm := range vms {
		go func(i int, vm VM) {
			_, err := vm.Run(context.Background(), Command{
				Command: "echo " + string(rune('a'+i)) + " > f" + string(rune('a'+i)), TimeoutS: 30, CapBytes: OutputCapBytes,
			}, &capturingSink{})
			errs <- err
		}(i, vm)
	}
	for range vms {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Run: %v", err)
		}
	}
	for _, n := range []string{"fa", "fb"} {
		if _, err := os.Stat(dir + "/" + n); err != nil {
			t.Fatalf("%s missing: %v", n, err)
		}
	}
}

// TestRewriteSnapshotConfigGivesEachVMItsOwnSockets is not part of the brief's
// verbatim test list — it covers rewriteSnapshotConfig, a helper this task's own
// design added (see its doc comment in launcher_chv.go) to solve a problem neither
// the brief nor hardware-corrections spells out a mechanism for: config.json
// embeds its vsock (and virtio-fs) socket paths verbatim, so two standbys
// restored from the SAME golden config.json would otherwise collide binding the
// identical host-side Unix socket. This is the one piece of that design
// verifiable without KVM access — a pure JSON transform, run here against
// synthetic input shaped like the reference tutorial's documented config.json.
func TestRewriteSnapshotConfigGivesEachVMItsOwnSockets(t *testing.T) {
	golden := `{
		"vsock": {"cid": 3, "socket": "/golden/vsock.sock"},
		"fs": [{"tag": "workspace", "socket": "/golden/vfsd.sock", "num_queues": 1, "queue_size": 1024}],
		"disks": [{"path": "/golden/rootfs.ext4", "readonly": true}]
	}`
	out, err := rewriteSnapshotConfig([]byte(golden), "/run/vm-a/vsock.sock", "/run/vm-a/vfsd.sock")
	if err != nil {
		t.Fatalf("rewriteSnapshotConfig: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	vsock, _ := doc["vsock"].(map[string]any)
	if got := vsock["socket"]; got != "/run/vm-a/vsock.sock" {
		t.Fatalf("vsock.socket = %v, want /run/vm-a/vsock.sock", got)
	}
	fsList, _ := doc["fs"].([]any)
	if len(fsList) != 1 {
		t.Fatalf("fs list = %v", fsList)
	}
	fs, _ := fsList[0].(map[string]any)
	if got := fs["socket"]; got != "/run/vm-a/vfsd.sock" {
		t.Fatalf("fs[0].socket = %v, want /run/vm-a/vfsd.sock", got)
	}
	// The disk path/readonly flag must be untouched: C4/C8 already settled that
	// readonly=on (baked into the golden snapshot) is what makes the disk lock
	// shareable across standbys, so nothing about it should vary per VM.
	disks, _ := doc["disks"].([]any)
	disk, _ := disks[0].(map[string]any)
	if got := disk["path"]; got != "/golden/rootfs.ext4" {
		t.Fatalf("disks[0].path = %v, want unchanged /golden/rootfs.ext4", got)
	}
	if got, ok := disk["readonly"].(bool); !ok || !got {
		t.Fatalf("disks[0].readonly = %v, want unchanged true", disk["readonly"])
	}
}

// TestRewriteSnapshotConfigToleratesNoFsSection covers a config.json with no
// virtio-fs device at all (fsSocketPath == "") — rewriteSnapshotConfig must not
// fail or invent an "fs" key that was not there.
func TestRewriteSnapshotConfigToleratesNoFsSection(t *testing.T) {
	golden := `{"vsock": {"cid": 3, "socket": "/golden/vsock.sock"}}`
	out, err := rewriteSnapshotConfig([]byte(golden), "/run/vm-a/vsock.sock", "")
	if err != nil {
		t.Fatalf("rewriteSnapshotConfig: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if _, present := doc["fs"]; present {
		t.Fatalf("fs key should not have been invented: %v", doc["fs"])
	}
}
