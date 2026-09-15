//go:build windows

package main

// rlimitMemlock and rlimitNofile: POSIX rlimits have no Windows equivalent. Recorded
// as "unavailable" rather than omitted — spec §7.5's own framing: "an absent key reads
// as 'not checked', a present 'unavailable' reads as 'checked, not applicable'."
// Windows is not a deployment target for this project (see
// internal/vmpool/cgroup_windows.go for the identical posture taken there); this stub
// exists so that fact is a deliberate, documented choice rather than an accidental
// compile failure on an unlisted platform.
func rlimitMemlock() string { return "unavailable" }
func rlimitNofile() string  { return "unavailable" }
