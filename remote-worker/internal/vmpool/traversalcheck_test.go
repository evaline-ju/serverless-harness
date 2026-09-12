package vmpool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckPathTraversableByAllowsWorldExecutableAncestors is the pass case:
// every ancestor between the target leaf and the filesystem root grants
// execute to "other" (the class governing everyone but the owning uid/gid),
// so an arbitrary uid:gid (65534:65534, virtiofsd's own configured default —
// see chvOpts) can reach it.
//
// t.TempDir() itself is NOT such an ancestor by default: like
// sameDeviceSiblingDir before fix round 3 (see that function's own doc
// comment in launcher_firecracker_test.go), it is created via os.MkdirTemp,
// mode 0700 -- exactly the bug shape this whole file exists to catch, one
// level up. Worse, testing.T.TempDir() actually creates TWO such levels for
// this test's first call: a per-test root (named after the test, e.g.
// ".../TestFoo1234/") and, beneath it, a per-call numbered subdirectory
// (".../TestFoo1234/001") -- both default to 0700, both owned by this test
// binary's own uid, and this test chmods both to 0711, the same way
// sameDeviceSiblingDir now does.
//
// That still is not enough on every machine this suite runs on: above those
// two levels, checkPathTraversableBy keeps walking into the host's OWN
// temp-directory hierarchy -- e.g. on darwin, $TMPDIR itself
// (/var/folders/<hash>/<hash>/T) is mode 0700, owned by the logged-in user,
// not by this test. Confirmed by running this test with only the two levels
// above chmodded: the error named $TMPDIR itself, not anything this test
// created, as the still-blocking ancestor. Chmodding a real, shared,
// system-owned directory just to make a unit test pass would be worse than
// the test itself (it would loosen a real security boundary on the host for
// as long as the test process happens to be running, races every other
// process on the machine, and is exactly the kind of self-healing this whole
// feature deliberately refuses to do to a caller's directories -- see
// launcher_chv.go's Restore, item 2's doc comment). So this test fakes
// statOwnerModeFunc for every ancestor ABOVE what it created and chmodded
// itself, reporting those as traversable without touching the real
// filesystem, while every ancestor it actually controls is still checked for
// real. That keeps this test about checkPathTraversableBy's walk-to-root
// logic, not a referendum on this machine's own temp-directory layout.
func TestCheckPathTraversableByAllowsWorldExecutableAncestors(t *testing.T) {
	root := t.TempDir()
	controlled := map[string]bool{root: true, filepath.Dir(root): true}
	for dir := range controlled {
		if err := os.Chmod(dir, 0o711); err != nil {
			t.Fatalf("chmod %s to 0711: %v", dir, err)
		}
	}

	origStat := statOwnerModeFunc
	defer func() { statOwnerModeFunc = origStat }()
	statOwnerModeFunc = func(path string) (uid, gid uint32, mode os.FileMode, err error) {
		if controlled[path] {
			return origStat(path)
		}
		// Above what this test controls: report a world-executable directory
		// without touching the real filesystem (see doc comment above).
		return 0, 0, os.ModeDir | 0o711, nil
	}

	leaf := filepath.Join(root, "workspace")
	if err := os.Mkdir(leaf, 0o755); err != nil {
		t.Fatalf("Mkdir %s: %v", leaf, err)
	}
	if err := checkPathTraversableBy(leaf, 65534, 65534); err != nil {
		t.Fatalf("checkPathTraversableBy(%s, 65534, 65534): %v", leaf, err)
	}
}

// TestCheckPathTraversableByDetectsBlockingAncestor is fix round 3's Item 2
// mutation test, done for real rather than through a fake seam: a genuine
// 0700 ancestor directory, owned by whatever uid runs `go test` (never 65534
// in any environment this suite runs in), blocks traversal by uid:gid
// 65534:65534 — exactly virtiofsd's own configured identity (chvOpts), and
// exactly the shape of the bug the coordinator diagnosed on the rig:
// os.MkdirTemp's default 0700 sitting ABOVE a correctly-chowned leaf. Asserts
// the error names the blocking ancestor's path, its mode, and the uid being
// checked — the coordinator's explicit ask ("say so precisely: which path,
// which uid, and which ancestor's mode is blocking").
func TestCheckPathTraversableByDetectsBlockingAncestor(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	leaf := filepath.Join(blocker, "workspace")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", leaf, err)
	}
	if err := os.Chmod(blocker, 0o700); err != nil {
		t.Fatalf("chmod %s to 0700: %v", blocker, err)
	}

	err := checkPathTraversableBy(leaf, 65534, 65534)
	if err == nil {
		t.Fatalf("checkPathTraversableBy(%s, 65534, 65534): want error (blocker is 0700, "+
			"owned by this test's own uid, not 65534), got nil", leaf)
	}
	for _, want := range []string{blocker, "65534", "0700"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("checkPathTraversableBy error %q: missing %q", err.Error(), want)
		}
	}
}

// TestCheckPathTraversableByAllowsOwnerWhenUIDMatches pins canTraverse's owner
// class branch: a 0700 ancestor owned by exactly the uid:gid being checked is
// NOT a blocker. This is what makes the Firecracker arm's own 0700 jail root
// fine as-is (fix round 3 does not touch it): it runs entirely as root, and
// root owns it, so the owner class — not "other" — governs.
func TestCheckPathTraversableByAllowsOwnerWhenUIDMatches(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "workspace")
	if err := os.Mkdir(leaf, 0o755); err != nil {
		t.Fatalf("Mkdir %s: %v", leaf, err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod %s to 0700: %v", root, err)
	}
	uid, gid, _, err := statOwnerMode(root)
	if err != nil {
		t.Skipf("statOwnerMode(%s): %v (this platform cannot verify ownership; see device_other.go)", root, err)
	}
	if err := checkPathTraversableBy(leaf, uid, gid); err != nil {
		t.Fatalf("checkPathTraversableBy(%s, %d, %d) with the ancestor's own owning uid:gid: %v", leaf, uid, gid, err)
	}
}
