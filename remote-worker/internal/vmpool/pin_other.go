//go:build !linux

package vmpool

import "errors"

// PinMemoryFile is Linux-only. Spec §9 puts macOS out of scope for measurement — there
// is no snapshot-restore equivalent on Hypervisor.framework — so a Mac cannot pin a
// memory file it also cannot produce.
func PinMemoryFile(string) (func() error, error) {
	return nil, errors.New("vmpool: pinning the snapshot memory file requires Linux")
}
