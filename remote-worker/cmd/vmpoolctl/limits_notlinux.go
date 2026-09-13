//go:build !linux

package main

// procLimits: /proc/sys sysctls and cgroup v2 pids.max have no equivalent off Linux.
// Recorded as "unavailable" rather than omitted, matching rlimitMemlock/
// rlimitNofile's own convention on windows (limits_windows.go) — darwin, where this
// package's tests run day to day, hits this file too.
func procLimits() map[string]string {
	return map[string]string{
		"vm.max_map_count": "unavailable",
		"pid_max":          "unavailable",
		"TasksMax":         "unavailable",
	}
}
