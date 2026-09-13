package vmpool

import (
	"fmt"
	"os"
	"path/filepath"
)

// statOwnerModeFunc is statOwnerMode through a seam, for the same reason
// deviceNumberFunc (devicecheck.go) is one: production code never touches this
// var. Unlike deviceNumberFunc, most of checkPathTraversableBy's own tests do
// NOT need it faked — the bug this file exists to catch (a root-owned 0700
// ancestor blocking an unprivileged uid, e.g. virtiofsd's 65534 "nobody") is
// fully reproducible with a REAL directory this test binary's own uid creates
// and chmods, because the target uid (65534) is essentially never the uid
// `go test` itself runs as. The seam remains for the one case that is not
// reproducible that way: asserting the "owner class" branch of canTraverse
// against an ancestor genuinely owned by the checked uid (see
// TestCheckPathTraversableByAllowsOwnerWhenUIDMatches, which uses the real
// statOwnerMode instead, reading back this test's own uid rather than faking
// one — but a future test needing a THIRD identity, e.g. group-class, without
// a matching real account, would fake it here).
var statOwnerModeFunc = statOwnerMode

// checkPathTraversableBy walks every ancestor directory of path — from its
// immediate parent up to the filesystem root — and confirms uid:gid can
// traverse each one, i.e. that stat(2)'s permission bits grant execute to
// uid:gid at every level between "/" and path.
//
// This is the traversal rule fix round 3's coordinator named explicitly: a
// directory's OWN mode/ownership says nothing about whether a process can
// ever reach it, because the kernel checks execute permission on EVERY path
// component from "/" down to the target, not just the target itself.
// chvPrepareOwnership (launcher_chv.go) correctly chowns req.WorkspaceDir
// itself before virtiofsd starts, but that call's own doc comment says
// plainly it does not reach WorkspaceDir's ANCESTORS — those belong to
// whoever created WorkspaceDir (the pool/orchestration layer, or, in the
// gates, sameDeviceSiblingDir), not to this launcher. This is the check that
// catches it when one of them is unreachable, with a precise error naming the
// path, the uid, and the blocking ancestor's own mode — instead of letting
// virtiofsd hit that ancestor's EACCES from underneath and mis-report it as
// the leaf itself "does not exist" (see launcher_chv.go's Restore, the
// pre-flight call site, for the full story).
func checkPathTraversableBy(path string, uid, gid uint32) error {
	dir := filepath.Dir(path)
	for {
		ownerUID, ownerGID, mode, err := statOwnerModeFunc(dir)
		if err != nil {
			return fmt.Errorf("vmpool: checking whether uid %d:gid %d can traverse %s "+
				"(an ancestor of %s): %w", uid, gid, dir, path, err)
		}
		if !mode.IsDir() {
			return fmt.Errorf("vmpool: %s (an ancestor of %s) is not a directory (mode %v); "+
				"cannot be traversed", dir, path, mode)
		}
		if !canTraverse(mode, ownerUID, ownerGID, uid, gid) {
			return fmt.Errorf("vmpool: %s (mode %04o, owned by uid %d:gid %d) blocks traversal "+
				"by uid %d:gid %d needed to reach %s -- grant execute permission on this ancestor "+
				"to that uid/gid, or to \"other\" if it has none of its own",
				dir, mode.Perm(), ownerUID, ownerGID, uid, gid, path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// canTraverse mirrors the kernel's own permission-check order for one
// directory component: the owner class governs if uid matches the directory's
// owning uid, else the group class governs if gid matches its owning gid
// (primary group only — this does not consult supplementary groups; a
// documented simplification, not an oversight: CHVOptions.VirtiofsdUID/GID's
// own doc comment already treats the deployment as a single dedicated uid:gid
// pair, never a group-membership scheme), else the "other" class governs. The
// "other" class is exactly where fix round 3's actual bug lives: a root-owned
// 0700 ancestor denies "other" execute to everyone but root, and virtiofsd is
// deliberately never root (CHVOptions.validate()).
func canTraverse(mode os.FileMode, ownerUID, ownerGID, uid, gid uint32) bool {
	perm := mode.Perm()
	switch {
	case uid == ownerUID:
		return perm&0o100 != 0
	case gid == ownerGID:
		return perm&0o010 != 0
	default:
		return perm&0o001 != 0
	}
}

// checkPathWritableBy is checkPathTraversableBy's sibling for a different
// question and a different single directory, not an extension of it: fix
// round 11's rig repro is "Error creating pid file '<socket>.pid': Permission
// denied" from virtiofsd itself, AFTER it has already reached its socket's
// directory (traversal was never the problem there — chvCheckWorkspaceReachable
// and the chown in chvPrepareVirtiofsdOwnership both already succeed). virtiofsd
// does not just bind(2) a socket inside that directory; before it does, it
// also creates a "<socket-path>.pid" sidecar FILE next to the socket, which
// requires WRITE permission on that directory, not merely execute (traversal).
// checkPathTraversableBy cannot catch this even in principle: it only ever
// inspects a path's ANCESTORS (starting at filepath.Dir(path)) and only ever
// checks the execute bit on each of them — it deliberately never inspects the
// LEAF directory itself, because the leaf's own mode was never fix round 3's
// concern (that was req.WorkspaceDir's ancestors, owned by the pool/
// orchestration layer, not this launcher). Here the leaf IS this launcher's
// own concern (runDir, which it created itself via os.MkdirAll — see
// launcher_chv.go's Restore), and the property that matters is write, not
// execute. Hence a new function rather than a parameter added to the old one.
func checkPathWritableBy(dir string, uid, gid uint32) error {
	ownerUID, ownerGID, mode, err := statOwnerModeFunc(dir)
	if err != nil {
		return fmt.Errorf("vmpool: checking whether uid %d:gid %d can write to %s: %w", uid, gid, dir, err)
	}
	if !mode.IsDir() {
		return fmt.Errorf("vmpool: %s is not a directory (mode %v); cannot be written into", dir, mode)
	}
	if !canWrite(mode, ownerUID, ownerGID, uid, gid) {
		return fmt.Errorf("vmpool: %s (mode %04o, owned by uid %d:gid %d) is not writable by uid %d:gid %d "+
			"-- virtiofsd must create its vhost-user socket (and the <socket>.pid file it writes beside it) "+
			"in this directory; chown/chmod it so that uid/gid owns it with write+execute permission",
			dir, mode.Perm(), ownerUID, ownerGID, uid, gid)
	}
	return nil
}

// canWrite mirrors canTraverse's permission-class ordering exactly (owner,
// then group, then other), but checks the write bit together with execute
// (0o3 per class) rather than execute alone: a directory needs execute to be
// entered/searched AND write to have entries created inside it (virtiofsd's
// socket bind and its ".pid" sidecar file both create a new directory entry).
// Checking write alone without execute would accept a directory the kernel
// would still refuse a mknod/creat(2) in for lack of search permission, so
// both bits are required together, matching what open(2)/bind(2) actually
// enforce.
func canWrite(mode os.FileMode, ownerUID, ownerGID, uid, gid uint32) bool {
	perm := mode.Perm()
	switch {
	case uid == ownerUID:
		return perm&0o300 == 0o300
	case gid == ownerGID:
		return perm&0o030 == 0o030
	default:
		return perm&0o003 == 0o003
	}
}
