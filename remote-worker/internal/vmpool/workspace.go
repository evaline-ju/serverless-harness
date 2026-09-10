package vmpool

import (
	"os"
	"path/filepath"
	"regexp"
)

// validKey bounds what may become a directory name under WorkspaceRoot. The key
// arrives over the wire, so an unvalidated one is a path-traversal primitive into
// the host — spec §2.3's cross-tenant leak reached through the new door. It is
// deliberately narrower than "run ids we happen to emit": lease run ids are
// session-derived slugs, and anything else is refused rather than escaped.
var validKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func checkKey(key string) error {
	if key == "" {
		return refusal(RefuseEmptyKey,
			"the microVM path requires a non-empty workspace_key: there is no correct "+
				"workspace to choose and nothing to fall back to (spec §3.4)")
	}
	if !validKey.MatchString(key) {
		return refusal(RefuseInvalidKey,
			"workspace_key %q must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}", key)
	}
	return nil
}

// workspaceDir resolves WorkspaceRoot/<key>. checkKey has already rejected
// anything that could escape; the containment check here makes that belt-and-braces
// rather than one regex standing between a wire string and the host filesystem.
func (p *pool) workspaceDir(key string) (string, error) {
	root := filepath.Clean(p.cfg.WorkspaceRoot)
	dir := filepath.Clean(filepath.Join(root, key))
	if filepath.Dir(dir) != root || dir == root {
		return "", refusal(RefuseInvalidKey, "workspace_key %q does not resolve directly inside %s", key, root)
	}
	return dir, nil
}

// ensureWorkspace creates the run's directory. 0o700 because the tree is
// bind-mounted into a guest but must not be readable by other host users; spec
// §2.3's cross-tenant path is this file-mode question one layer down.
func ensureWorkspace(dir string) error { return os.MkdirAll(dir, 0o700) }

func removeWorkspace(dir string) error { return os.RemoveAll(dir) }
