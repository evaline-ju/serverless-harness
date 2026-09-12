package vmpool

import (
	"fmt"
	"strings"
)

// namedPath pairs a host path with the name it should be reported under in a
// device-mismatch error — the Config field or Options field a fix actually
// needs to change, not just a bare directory string.
type namedPath struct {
	name string
	path string
}

// checkPathsShareDevice validates that every one of paths lives on the same
// filesystem device, returning an error that names every path AND its device
// number plus why they must match when they do not. why is one sentence,
// supplied by the caller because the reason differs by launcher (see
// firecrackerLauncher's and chvLauncher's checkDeviceSharing below).
//
// This exists because hardlink(2) always fails with EXDEV ("invalid
// cross-device link") across a device boundary, regardless of permissions —
// and both launchers hardlink rather than copy, deliberately: see
// checkDeviceSharing's callers for the load-bearing reason a copy would
// silently break (spec §8's TestGateWriteDurability, for Firecracker's
// workspace image). Checked once here, at pool construction (spec §6's "fail
// the unit at start" — the same reasoning as the KVM-unavailable check,
// Probe), rather than surfacing as a confusing EXDEV on the first Exec of the
// first run.
// deviceNumberFunc is deviceNumber through a seam, for exactly the reason the pool
// takes a Clock instead of calling time.Now() directly: this repo's darwin dev
// machine has a single filesystem device (confirmed by TestSameDeviceSiblingDirSharesDeviceWithTarget's
// own doc comment history), so no real pair of paths on it can ever exercise the
// mismatch branch below. Tests override this var to fake two devices; production
// code never touches it.
var deviceNumberFunc = deviceNumber

func checkPathsShareDevice(why string, paths ...namedPath) error {
	if len(paths) < 2 {
		return nil
	}
	devs := make([]uint64, len(paths))
	for i, p := range paths {
		d, err := deviceNumberFunc(p.path)
		if err != nil {
			return fmt.Errorf("vmpool: checking device for %s (%s): %w", p.name, p.path, err)
		}
		devs[i] = d
	}
	mismatch := false
	for _, d := range devs[1:] {
		if d != devs[0] {
			mismatch = true
			break
		}
	}
	if !mismatch {
		return nil
	}
	parts := make([]string, len(paths))
	for i, p := range paths {
		parts[i] = fmt.Sprintf("%s=%s (device %d)", p.name, p.path, devs[i])
	}
	return fmt.Errorf("vmpool: %s must all be on the same filesystem device, but are not: %s. %s",
		strings.Join(namesOf(paths), ", "), strings.Join(parts, "; "), why)
}

func namesOf(paths []namedPath) []string {
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = p.name
	}
	return names
}

// deviceRequirer is implemented by any launcher whose Restore hardlinks files
// between configured host paths, so pool.New (the one place a Config and a
// Launcher are always both in scope — spec §6's "fail at start" needs exactly
// that overlap) can enforce their shared-device requirement once at
// construction. cfg is passed in, not just read from the launcher's own
// Options, because WorkspaceRoot lives on Config, not on either launcher's
// Options struct. A launcher that hardlinks nothing (FakeLauncher — never
// reachable from microvm-worker, see its type doc) simply does not implement
// this, and pool.New's type-assertion silently no-ops for it.
type deviceRequirer interface {
	checkDeviceSharing(cfg Config) error
}
