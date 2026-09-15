//go:build windows

package main

import "errors"

// childCPUUsage: RUSAGE_CHILDREN has no Windows equivalent in Go's standard syscall
// package. Windows is not a deployment target for this project (see
// internal/vmpool/cgroup_windows.go for the identical posture); this stub exists so
// that fact is a deliberate, documented choice rather than an accidental compile
// failure on an unlisted platform. The caller treats this error as non-fatal, exactly
// like a PinMemoryFile failure — CPUChildUs is diagnostic, not load-bearing for the
// rest of the record.
func childCPUUsage() (int64, error) {
	return 0, errors.New("vmpoolctl: RUSAGE_CHILDREN is not available on windows")
}
