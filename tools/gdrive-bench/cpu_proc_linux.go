//go:build linux && !no_extra_providers

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// clockTicksPerSecond is the USER_HZ that the CPU fields of /proc/<pid>/stat
// are expressed in. The kernel fixes it at 100 for the /proc ABI on every
// architecture, independently of the internal tick rate; reading it properly
// would mean sysconf(_SC_CLK_TCK), which needs cgo, and this tree builds
// without it.
const clockTicksPerSecond = 100

// procRoot is the procfs mount point, a variable only so the tests can point it
// at a fixture.
var procRoot = "/proc"

// maxProcAncestry bounds the parent-chain walk. A process tree cannot really be
// this deep; the bound exists because /proc is sampled without a lock, so a
// racy read could in principle produce a cycle.
const maxProcAncestry = 64

// Field positions in /proc/<pid>/stat, as proc(5) numbers them (1-based),
// counted AFTER the comm field - which is skipped by hand because it is the one
// field that can contain spaces and parentheses.
const (
	statFieldPPID      = 4
	statFieldUTime     = 14
	statFieldSTime     = 15
	statFieldStartTime = 22

	// statFieldsBeforeState is how many leading fields (pid, comm) are consumed
	// before the slice this code indexes into starts, so that proc(5)'s field
	// number N is at index N-1-statFieldsBeforeState.
	statFieldsBeforeState = 2
)

// procStat is the subset of /proc/<pid>/stat this harness reads.
type procStat struct {
	ppid int
	key  procKey
	cpu  cpuTimes
}

// walkDescendants returns the current CPU of every process below this one,
// keyed so that a reused PID cannot be confused with its predecessor.
//
// It counts each process's OWN utime+stime and deliberately not its cutime and
// cstime: every descendant is visited in its own right, so adding a parent's
// accumulated child time would count the same CPU twice.
func walkDescendants() (map[procKey]cpuTimes, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false
	}

	self := os.Getpid()
	all := make(map[int]procStat, len(entries))

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		st, ok := readProcStat(pid)
		if !ok {
			// The process exited between the directory listing and the read, which
			// is entirely normal and not an error.
			continue
		}

		all[pid] = st
	}

	out := map[procKey]cpuTimes{}

	for pid, st := range all {
		if pid == self {
			continue
		}

		if isDescendantOf(all, st, self) {
			out[st.key] = st.cpu
		}
	}

	return out, true
}

// isDescendantOf walks st's parent chain looking for ancestor.
func isDescendantOf(all map[int]procStat, st procStat, ancestor int) bool {
	for range maxProcAncestry {
		if st.ppid == ancestor {
			return true
		}

		next, ok := all[st.ppid]
		if !ok {
			return false
		}

		st = next
	}

	return false
}

// readProcStat parses one /proc/<pid>/stat.
func readProcStat(pid int) (procStat, bool) {
	// The path is built from a PID this function just read out of a /proc
	// directory listing, not from anything an operator can influence.
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, false
	}

	return parseProcStat(string(data))
}

// parseProcStat pulls the parent PID, the CPU times and the start time out of a
// /proc/<pid>/stat line.
//
// The comm field is an arbitrary executable name in parentheses and may itself
// contain spaces and parentheses, so parsing starts after its LAST ')' - the
// documented way to read this file.
func parseProcStat(line string) (procStat, bool) {
	commEnd := strings.LastIndexByte(line, ')')
	pidEnd := strings.IndexByte(line, ' ')

	if commEnd < 0 || pidEnd < 0 || commEnd+1 > len(line) {
		return procStat{}, false
	}

	fields := strings.Fields(line[commEnd+1:])

	utime, ok := statTicks(fields, statFieldUTime)
	if !ok {
		return procStat{}, false
	}

	stime, ok := statTicks(fields, statFieldSTime)
	if !ok {
		return procStat{}, false
	}

	ppid, ok := statField(fields, statFieldPPID)
	if !ok {
		return procStat{}, false
	}

	start, ok := statField(fields, statFieldStartTime)
	if !ok {
		return procStat{}, false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(line[:pidEnd]))
	if err != nil {
		return procStat{}, false
	}

	return procStat{
		ppid: int(ppid),
		key:  procKey{pid: pid, start: uint64(start)}, //nolint:gosec // starttime is a monotonic tick count and never negative.
		cpu:  cpuTimes{user: ticksToDuration(utime), sys: ticksToDuration(stime)},
	}, true
}

// statField reads proc(5) field number n out of the post-comm field slice.
func statField(fields []string, n int) (int64, bool) {
	i := n - 1 - statFieldsBeforeState
	if i < 0 || i >= len(fields) {
		return 0, false
	}

	v, err := strconv.ParseInt(fields[i], 10, 64)
	if err != nil {
		return 0, false
	}

	return v, true
}

// statTicks reads a CPU-time field, which must not be negative.
func statTicks(fields []string, n int) (int64, bool) {
	v, ok := statField(fields, n)
	if !ok || v < 0 {
		return 0, false
	}

	return v, true
}

// ticksToDuration converts USER_HZ ticks to a duration.
func ticksToDuration(ticks int64) time.Duration {
	return time.Duration(ticks) * time.Second / clockTicksPerSecond
}
