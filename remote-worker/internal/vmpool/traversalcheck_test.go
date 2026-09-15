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

// TestCheckPathWritableByDetectsUnwritableDir is fix round 11's direct
// (non-Restore) mutation test for checkPathWritableBy, done for real rather
// than through a fake seam — the same shape as
// TestCheckPathTraversableByDetectsBlockingAncestor, but checking the LEAF
// directory itself (write+execute) instead of an ancestor (execute only): a
// genuine 0700 directory, owned by whatever uid runs `go test` (never 65534 in
// any environment this suite runs in), is not writable by uid:gid 65534:65534
// — exactly virtiofsd's own configured identity (chvOpts), and exactly the
// shape of the rig's "Error creating pid file ...: Permission denied": runDir
// itself, not one of its ancestors, is the directory virtiofsd could not
// write its socket and .pid sidecar file into.
func TestCheckPathWritableByDetectsUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod %s to 0700: %v", dir, err)
	}

	err := checkPathWritableBy(dir, 65534, 65534)
	if err == nil {
		t.Fatalf("checkPathWritableBy(%s, 65534, 65534): want error (dir is 0700, "+
			"owned by this test's own uid, not 65534), got nil", dir)
	}
	for _, want := range []string{dir, "65534", "0700"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("checkPathWritableBy error %q: missing %q", err.Error(), want)
		}
	}
}

// TestCheckPathWritableByAllowsWorldWritableDir is the pass case: a directory
// granting write+execute to "other" (0777) is writable by an arbitrary
// uid:gid that owns neither it nor its group — pinning canWrite's "other"
// class branch, the one virtiofsd's unprivileged uid actually depends on in
// production (this launcher's chvChmod sets runDir to 0700 owner-only, not
// world-writable, but canWrite's other-class arithmetic is exercised here
// independently of what this launcher happens to choose to chmod to).
func TestCheckPathWritableByAllowsWorldWritableDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod %s to 0777: %v", dir, err)
	}
	if err := checkPathWritableBy(dir, 65534, 65534); err != nil {
		t.Fatalf("checkPathWritableBy(%s, 65534, 65534) with a world-writable dir: %v", dir, err)
	}
}

// TestCheckPathWritableByRejectsExecuteOnlyDir pins the one detail that makes
// checkPathWritableBy a genuinely different check from checkPathTraversableBy
// rather than a copy of it with a different error message: canWrite requires
// write AND execute together per permission class (0o3), not execute alone
// (0o1, canTraverse's own bit) — a directory an uid can enter but not create
// entries in (mode 0701: "other" gets --x, no w) is exactly what virtiofsd's
// bind()+".pid"-file-create needs and traversal alone does not provide. A
// canWrite that checked only the execute bit (silently degrading into
// canTraverse) would pass every other test in this file — none of them uses a
// mode with execute set but write cleared for the class under test — so this
// one exists specifically to catch that reduction.
func TestCheckPathWritableByRejectsExecuteOnlyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o701); err != nil {
		t.Fatalf("chmod %s to 0701: %v", dir, err)
	}

	// canTraverse (the OTHER function's bit test) must still consider this mode
	// traversable by 65534:65534 -- confirming this mode really does isolate
	// "execute granted, write not" rather than accidentally testing a
	// fully-blocked directory a weakened canWrite could pass for the wrong
	// reason. This deliberately calls canTraverse directly rather than
	// checkPathTraversableBy(dir's own ancestors, which include this test's
	// t.TempDir() parent levels (0700, owned by this test's own uid) and are
	// not the point of this test -- see
	// TestCheckPathTraversableByAllowsWorldExecutableAncestors's doc comment
	// for why walking those for real would fail for unrelated reasons.
	ownerUID, ownerGID, mode, err := statOwnerMode(dir)
	if err != nil {
		t.Skipf("statOwnerMode(%s): %v (this platform cannot verify ownership; see device_other.go)", dir, err)
	}
	if !canTraverse(mode, ownerUID, ownerGID, 65534, 65534) {
		t.Fatalf("canTraverse(mode 0701, ..., 65534, 65534) = false, want true -- this test needs " +
			"a mode where \"other\" can traverse but not write to isolate canWrite's extra bit")
	}

	werr := checkPathWritableBy(dir, 65534, 65534)
	if werr == nil {
		t.Fatalf("checkPathWritableBy(%s, 65534, 65534): want error (dir is 0701 -- other can "+
			"traverse but not write), got nil", dir)
	}
	if !strings.Contains(werr.Error(), "0701") {
		t.Fatalf("checkPathWritableBy error %q: missing %q", werr.Error(), "0701")
	}
}

// TestCheckPathWritableByAllowsOwnerWhenUIDMatches mirrors
// TestCheckPathTraversableByAllowsOwnerWhenUIDMatches for canWrite's owner
// class branch: a 0700 directory owned by exactly the uid:gid being checked is
// writable by it — this is what makes runDir's own chvChmod(runDir, 0o700)
// correct in production, where VirtiofsdUID/GID is also the uid chvChown just
// gave runDir, so the owner class (not "other") governs.
func TestCheckPathWritableByAllowsOwnerWhenUIDMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod %s to 0700: %v", dir, err)
	}
	uid, gid, _, err := statOwnerMode(dir)
	if err != nil {
		t.Skipf("statOwnerMode(%s): %v (this platform cannot verify ownership; see device_other.go)", dir, err)
	}
	if err := checkPathWritableBy(dir, uid, gid); err != nil {
		t.Fatalf("checkPathWritableBy(%s, %d, %d) with the dir's own owning uid:gid: %v", dir, uid, gid, err)
	}
}
