package vmpool

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The shipped systemd units, read at the path they actually live at. Never copied into a
// fixture: five probes on this branch produced confident wrong answers by moving an
// artifact out of reach of the thing that configures it, and H1 itself shipped because
// deploy/microvm/tests/systemd-units.test.sh could only grep the unit files' text —
// it had no way to evaluate them against the Go code they configure.
const (
	shippedUnitDir     = "../../../deploy/microvm"
	shippedServiceUnit = "microvm-worker.service"
)

// TestTheShippedUnitPlacesTheWorkerInTheSliceItSweepsAndTheSweepRefusesItAnyway is the
// cross-artifact half of final-review H1: the defect was not in either artifact alone but
// in their interaction — `Slice=microvm-vms.slice` in the unit file versus "every
// immediate subdirectory of the slice is a VM" in cgroup.go — and nothing could see both.
// This test can.
//
// It asserts, from the real files:
//
//  1. The unit really does place the worker inside the slice it hands the worker to sweep
//     (Slice= and SH_PARENT_CGROUP name the same slice). This is the PRESENCE half: if it
//     ever stopped being true, item 2 below would be asserting nothing, and this test
//     would be quietly vacuous rather than red.
//  2. The cgroup directory name systemd derives for that unit is one SweepOrphans
//     refuses. That is the property whose absence was production-fatal.
func TestTheShippedUnitPlacesTheWorkerInTheSliceItSweepsAndTheSweepRefusesItAnyway(t *testing.T) {
	path := filepath.Join(shippedUnitDir, shippedServiceUnit)
	b, err := os.ReadFile(path)
	if err != nil {
		// Not a skip: a skip here would silently disable the only pin on the
		// unit-file/code interaction, which is how H1 stayed invisible.
		t.Fatalf("reading the shipped unit %s: %v — if the unit moved, update shippedUnitDir; do not delete this test", path, err)
	}
	unit := string(b)

	slice := firstDirectiveValue(t, unit, "Slice")
	if slice == "" {
		t.Fatal("microvm-worker.service sets no Slice=; this test's premise (the worker is a sibling of the VM cgroups) no longer holds and the whole test must be reconsidered, not deleted")
	}
	parentCgroup := firstDirectiveValue(t, unit, "Environment=SH_PARENT_CGROUP")
	if parentCgroup == "" {
		t.Fatal("microvm-worker.service sets no Environment=SH_PARENT_CGROUP; main.go would fall back to its own default and this pin would stop reflecting the shipped configuration")
	}

	// 1. The presence: the swept path IS the slice this unit is placed in, so the worker's
	//    own cgroup is one of the directories SweepOrphans enumerates.
	if filepath.Base(parentCgroup) != slice {
		t.Fatalf("Slice=%q but SH_PARENT_CGROUP=%q — the unit and the sweep disagree about which slice this is (hardware-corrections D1/D3: %q)",
			slice, parentCgroup, "configured consistently ... or the two mechanisms fight and the leak we are preventing returns")
	}

	// 2. The absence: systemd names a service unit's cgroup after the unit itself
	//    (systemd.slice(5)), so the directory SweepOrphans will meet is exactly
	//    shippedServiceUnit — and it must be refused.
	if isPoolVMCgroupDirName(shippedServiceUnit) {
		t.Fatalf("SweepOrphans would treat %q as a VM cgroup. That directory holds the worker's OWN pid, so the sweep would SIGKILL the worker at every start — with Restart=on-failure/RestartSec=5s, a permanent crash loop in which the tier never reaches Probe (final review H1)", shippedServiceUnit)
	}

	// And the same for the OTHER unit types systemd routinely places in a slice, so this
	// is a property of the filter rather than a special case for one filename.
	for _, sibling := range []string{"microvm-worker.service", "init.scope", "user.slice", "dev-kvm.device"} {
		if isPoolVMCgroupDirName(sibling) {
			t.Errorf("isPoolVMCgroupDirName(%q) = true — systemd's own cgroups must never be swept", sibling)
		}
	}

	// Non-vacuousness for the filter itself, in this file too: the refusals above are
	// only meaningful if the filter still accepts what the pool creates.
	p := &pool{}
	id := p.nextIDLocked()
	if !isPoolVMCgroupDirName(id) || !isPoolVMCgroupDirName(chvScopeUnitName(id)+chvScopeDirSuffix) {
		t.Fatalf("the filter refuses this pool's own VM cgroup names (%q / %q) — it has narrowed to nothing and the sweep is a no-op", id, chvScopeUnitName(id)+chvScopeDirSuffix)
	}
}

// firstDirectiveValue returns the value of the first "<key>=<value>" line in a systemd
// unit, ignoring comments. key may itself contain an "=" (as "Environment=SH_PARENT_CGROUP"
// does), which is why this is a prefix match on the line rather than a split on the first
// "=".
func firstDirectiveValue(t *testing.T, unit, key string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=(.*)$`)
	m := re.FindStringSubmatch(unit)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}
