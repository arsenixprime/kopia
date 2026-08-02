//go:build windows && !no_extra_providers

package main

// Windows has no getrusage. The harness still builds and runs there; it simply
// reports cpu_source "none" (or "wait4" for the out-of-process stages, whose
// ProcessState times are portable), so a reader can tell the difference between
// "no CPU was spent" and "CPU was not measured on this platform".
const (
	rusageSelf     = 0
	rusageChildren = 1
)

func readRusage(int) (cpuTimes, bool) {
	return cpuTimes{}, false
}
