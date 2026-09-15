//go:build linux

package main

import (
	"os"
	"strings"
)

// procLimits reads the three kernel ceilings that have no meaning off Linux: two flat
// sysctls under /proc/sys, and TasksMax from this process's own cgroup v2 pids.max.
// Spec §7.5 lists all three among the limits that "fail at 500 VMs after working at
// 20, indistinguishably from a real ceiling" unless raised and recorded per run.
func procLimits() map[string]string {
	return map[string]string{
		"vm.max_map_count": readSysctlFile("/proc/sys/vm/max_map_count"),
		"pid_max":          readSysctlFile("/proc/sys/kernel/pid_max"),
		"TasksMax":         readOwnPidsMax(),
	}
}

func readSysctlFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(b))
}

// readOwnPidsMax finds this process's own cgroup v2 leaf — the unified hierarchy's
// single "0::<path>" line in /proc/self/cgroup, the only shape a cgroup v2 host
// produces (pool.New itself already refuses cgroups v1 hosts, spec §2.4, so a v1-style
// numbered line here would mean this binary is not even running where its own numbers
// would count) — and reads its pids.max, which IS systemd's TasksMax=: there is no
// separate file to read, systemd's setting and cgroup v2's pids.max are the same
// number under two names.
func readOwnPidsMax() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "unavailable"
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		rel := strings.TrimPrefix(line, "0::")
		return readSysctlFile("/sys/fs/cgroup" + rel + "/pids.max")
	}
	return "unavailable"
}
