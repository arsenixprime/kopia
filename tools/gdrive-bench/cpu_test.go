//go:build !no_extra_providers

package main

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/clock"
)

// busyFor burns CPU in this process for at least d, so a test can assert that a
// CPU counter moved without depending on a clock the harness controls.
func busyFor(d time.Duration) {
	deadline := clock.Now().Add(d)

	var x uint64

	for clock.Now().Before(deadline) {
		for range 100000 {
			x = x*1664525 + 1013904223
		}
	}

	runtime.KeepAlive(x)
}

// TestSamplerRecordsSelfCPU is the busy-loop workload stub: a phase that spends
// its time computing must report CPU it actually spent, and a cpu_pct near one
// core.
func TestSamplerRecordsSelfCPU(t *testing.T) {
	if _, ok := readRusage(rusageSelf); !ok {
		t.Skip("this platform has no getrusage")
	}

	s := newSampler(nil)

	const burn = 300 * time.Millisecond

	busyFor(burn)
	s.observe(burn, nil)

	m := s.finish(nil)

	require.Positive(t, m.CPUUserMS, "a busy loop must move the RUSAGE_SELF user counter")
	require.GreaterOrEqual(t, m.CPUTotalMS, m.CPUUserMS+m.CPUSysMS-1e-9)
	require.GreaterOrEqual(t, m.CPUTotalMS, float64(burn.Milliseconds())*0.5,
		"a single-threaded busy loop should account for most of the phase")

	// One saturated core is 100%; allow slack for the runtime's own goroutines.
	require.Greater(t, m.CPUPct, 50.0)
	require.Contains(t, m.CPUSource, cpuSourceRusage)
}

// TestCPUMeterIdlePhaseIsCheap pins the other direction: a phase that sleeps
// must not be charged CPU it did not spend. Without this, "native uses less
// CPU" could just mean "the counter is broken".
func TestCPUMeterIdlePhaseIsCheap(t *testing.T) {
	if _, ok := readRusage(rusageSelf); !ok {
		t.Skip("this platform has no getrusage")
	}

	m := startCPUMeterOn(newCPULedger())

	time.Sleep(200 * time.Millisecond)

	s := m.stop()

	require.Less(t, s.self.total(), 100*time.Millisecond)
}

// TestCPULedgerReconciles pins the rule that stops the same CPU being charged
// twice: a process attributed live and then reaped hands back exactly what it
// had been credited with, and the reap lump is charged only for the remainder.
func TestCPULedgerReconciles(t *testing.T) {
	l := newCPULedger()
	k := procKey{pid: 4242, start: 99}

	l.note(k, cpuTimes{user: 300 * time.Millisecond, sys: 100 * time.Millisecond})
	l.note(k, cpuTimes{user: 200 * time.Millisecond, sys: 400 * time.Millisecond})

	// note never lowers a mark: the counters it tracks only grow.
	require.Equal(t, cpuTimes{user: 300 * time.Millisecond, sys: 400 * time.Millisecond}, l.forget(k))

	// Forgetting is idempotent, so a double stop cannot subtract twice.
	require.Equal(t, cpuTimes{}, l.forget(k))

	l.note(k, cpuTimes{user: time.Second})
	l.prune(map[procKey]cpuTimes{{pid: 1}: {}})
	require.Equal(t, cpuTimes{}, l.forget(k), "prune drops processes that are gone")
}

// TestSuspectCPUAccountingTripwire pins the report-time guard: a lane that did
// seconds of network work while reporting almost no CPU has leaked its
// accounting, and the number must never be published as a win.
func TestSuspectCPUAccountingTripwire(t *testing.T) {
	// 30 s of WebDAV-over-TLS traffic cannot cost 4 ms of CPU.
	require.True(t, suspectCPUAccounting(30000, 4))

	// A plausible figure for the same phase is not flagged.
	require.False(t, suspectCPUAccounting(30000, 2500))

	// A short phase is not judged at all: it legitimately shows almost nothing.
	require.False(t, suspectCPUAccounting(200, 0))

	// Right at the floor: 1 s of work with 20 ms of CPU is believable, 5 is not.
	require.False(t, suspectCPUAccounting(1000, 25))
	require.True(t, suspectCPUAccounting(1000, 5))
}

func TestCPUTimesArithmetic(t *testing.T) {
	a := cpuTimes{user: 5 * time.Second, sys: 2 * time.Second}
	b := cpuTimes{user: 3 * time.Second, sys: 4 * time.Second}

	require.Equal(t, cpuTimes{user: 8 * time.Second, sys: 6 * time.Second}, a.plus(b))
	require.Equal(t, cpuTimes{user: 2 * time.Second, sys: 0}, a.minus(b), "minus clamps at zero")
	require.Equal(t, cpuTimes{user: 5 * time.Second, sys: 4 * time.Second}, maxCPUTimes(a, b))
	require.Equal(t, 7*time.Second, a.total())
}

func TestCPUSampleSourceNaming(t *testing.T) {
	require.Equal(t, cpuSourceNone, cpuSample{}.source())
	require.Equal(t, "rusage", cpuSample{fromRusage: true}.source())
	require.Equal(t, "rusage+proc", cpuSample{fromRusage: true, fromProc: true}.source())
	require.Equal(t, "rusage+proc+wait4", cpuSample{fromRusage: true, fromProc: true, fromWait4: true}.source())
	require.Equal(t, "wait4", cpuSample{fromWait4: true}.source())
}

func TestCPUSampleAppliesToMetrics(t *testing.T) {
	s := cpuSample{
		self:       cpuTimes{user: 400 * time.Millisecond, sys: 100 * time.Millisecond},
		children:   cpuTimes{user: 200 * time.Millisecond, sys: 300 * time.Millisecond},
		fromRusage: true,
		fromProc:   true,
	}

	m := WorkloadMetrics{WallClockMS: 2000}
	s.applyTo(&m)

	require.InDelta(t, 400.0, m.CPUUserMS, 1e-9)
	require.InDelta(t, 100.0, m.CPUSysMS, 1e-9)
	require.InDelta(t, 200.0, m.CPUChildrenUserMS, 1e-9)
	require.InDelta(t, 300.0, m.CPUChildrenSysMS, 1e-9)
	require.InDelta(t, 1000.0, m.CPUTotalMS, 1e-9)
	require.InDelta(t, 50.0, m.CPUPct, 1e-9)
	require.Equal(t, "rusage+proc", m.CPUSource)

	// A zero wall clock must not produce a NaN or an infinity in the document.
	zero := WorkloadMetrics{}
	s.applyTo(&zero)
	require.InDelta(t, 0.0, zero.CPUPct, 1e-9)
}

// TestWithProcessStateNeverAdds pins the no-double-counting rule: the three
// accounting sources are alternative estimates of the same CPU, so they are
// combined with max, never with +.
func TestWithProcessStateNeverAdds(t *testing.T) {
	s := cpuSample{children: cpuTimes{user: 900 * time.Millisecond, sys: 100 * time.Millisecond}, fromProc: true}

	// A ProcessState the test can build without running anything: nil is the
	// documented "no child was waited for" case and must be a no-op.
	require.Equal(t, s, s.withProcessState(nil))
}

// TestCPUTotalIsAComparedButNotAKeyMetric pins the two decisions of §8.8: the
// comparison flags CPU, and no baseline check ever fails on it.
func TestCPUTotalIsAComparedButNotAKeyMetric(t *testing.T) {
	require.Contains(t, metricNames, "cpu_total_ms")
	require.NotContains(t, keyMetricNames, "cpu_total_ms")

	m := WorkloadMetrics{CPUTotalMS: 1234, CPUPct: 61, CPUChildrenUserMS: 7}

	for _, name := range []string{"cpu_user_ms", "cpu_sys_ms", "cpu_children_user_ms", "cpu_children_sys_ms", "cpu_total_ms", "cpu_pct"} {
		_, ok := metricValue(&m, name)
		require.True(t, ok, "metricValue must understand %v", name)
	}

	v, ok := metricValue(&m, "cpu_total_ms")
	require.True(t, ok)
	require.InDelta(t, 1234.0, v, 1e-9)

	require.False(t, higherIsBetter("cpu_total_ms"), "CPU is a cost")
}
