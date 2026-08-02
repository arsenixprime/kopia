//go:build !linux && !no_extra_providers

package main

// walkDescendants has no portable implementation: enumerating a process tree
// and its per-process CPU is procfs-specific. Everywhere but Linux the harness
// falls back to the RUSAGE_CHILDREN delta, which sees direct children once they
// are reaped and never sees a grandchild - so cpu_source will say "rusage"
// rather than "rusage+proc", and a phase whose child outlives it reports no
// child CPU rather than a wrong one.
func walkDescendants() (map[procKey]cpuTimes, bool) {
	return nil, false
}
