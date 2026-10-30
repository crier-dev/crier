// Package testsupport provides load-scaled hang guards for tests that wait on the host.
//
// A flat timeout turns a test's hang guard into a latency assertion. On a shared
// host, scheduler contention can delay a local HTTP round trip or SQLite write
// without changing the behavior under test. HangGuard derives a bounded budget
// from the host's runnable queue and logs that derivation.
package testsupport

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// HangGuardFloor is the idle-host budget. Healthy local completions are
	// normally much faster; this is headroom for scheduler contention.
	HangGuardFloor = 15 * time.Second
	// HangGuardCeiling keeps a wedged wait from consuming the package timeout.
	HangGuardCeiling = 30 * time.Second
)

// LoadFactor returns the one-minute load average, the scaling factor applied to
// the guard, and the number of logical CPUs. An unreadable load average leaves
// the factor at one and is reported as -1.
func LoadFactor() (load, factor float64, cpus int) {
	load, factor = -1, 1
	cpus = runtime.NumCPU()
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(raw)); len(fields) > 0 {
			if parsed, parseErr := strconv.ParseFloat(fields[0], 64); parseErr == nil {
				load = parsed
			}
		}
	}
	if load >= 0 && cpus > 0 && load > float64(cpus) {
		factor = load / float64(cpus)
	}
	return load, factor, cpus
}

// HangGuard returns a bounded timeout for a wait whose completion is not a
// latency contract. The budget is never below the floor or above the ceiling.
func HangGuard(t testing.TB) time.Duration {
	t.Helper()
	load, factor, cpus := LoadFactor()
	budget := time.Duration(float64(HangGuardFloor) * factor)
	if budget < HangGuardFloor {
		budget = HangGuardFloor
	}
	if budget > HangGuardCeiling {
		budget = HangGuardCeiling
	}
	if load >= 0 {
		t.Logf("hang guard: %s = floor %s x load factor %.2f (loadavg=%.2f over %d CPUs, ceiling %s; not a latency assertion)",
			budget, HangGuardFloor, factor, load, cpus, HangGuardCeiling)
	} else {
		t.Logf("hang guard: %s = floor %s (loadavg unavailable, factor 1.00, ceiling %s; not a latency assertion)",
			budget, HangGuardFloor, HangGuardCeiling)
	}
	return budget
}
