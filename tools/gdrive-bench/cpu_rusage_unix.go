//go:build !windows && !no_extra_providers

package main

import (
	"syscall"
	"time"
)

// The getrusage "who" arguments, named portably so the rest of the harness does
// not have to import syscall.
const (
	rusageSelf     = syscall.RUSAGE_SELF
	rusageChildren = syscall.RUSAGE_CHILDREN
)

// readRusage reads one getrusage bucket. RUSAGE_SELF is this process (all of
// its threads); RUSAGE_CHILDREN is the accumulated CPU of every child that has
// been waited for, which is why it only moves at a reap.
func readRusage(who int) (cpuTimes, bool) {
	var ru syscall.Rusage

	if err := syscall.Getrusage(who, &ru); err != nil {
		return cpuTimes{}, false
	}

	return cpuTimes{user: timevalDuration(ru.Utime), sys: timevalDuration(ru.Stime)}, true
}

// timevalDuration converts a kernel timeval. The field widths differ per
// platform, hence the conversions.
func timevalDuration(tv syscall.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}
