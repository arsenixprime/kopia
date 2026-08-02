//go:build !no_extra_providers

package main

import (
	"os"
	"strings"
	"sync"
	"time"
)

// CPU accounting for a measured phase.
//
// A backup tool's cost to a machine is not only wall clock: a backend that wins
// on latency while burning three cores is not obviously a win. Measuring it
// honestly across the two lanes of this comparison is awkward, because the two
// lanes spend their CPU in different processes:
//
//   - the native Drive backend runs INSIDE the harness, so all of its CPU is the
//     harness process's own (RUSAGE_SELF);
//   - the rclone lane's real work happens in an `rclone serve webdav` child that
//     kopia's rclone backend spawns, so almost none of its CPU is RUSAGE_SELF;
//   - `snapshot-e2e` runs a whole kopia binary as a child, and on the rclone lane
//     THAT child spawns its own rclone, a grandchild of the harness.
//
// The only comparable number is therefore "CPU this phase cost the machine" =
// harness CPU + every descendant's CPU, which is what cpu_total_ms is.
//
// # Why reap-time accounting alone is not enough
//
// RUSAGE_CHILDREN only moves when a direct child is REAPED. Kopia's rclone
// backend reaps its `rclone serve webdav` at storage Close, which happens at a
// connection recycle or in the runner's deferred cleanup - OUTSIDE every
// measured window, and once per workload rather than once per phase. Attributing
// per workload from reaps alone would therefore report ~0 CPU for every rclone
// phase and dump a whole connection's CPU into whichever window happened to
// contain a Close. So the primary attribution is live and cumulative:
//
//	/proc/<pid>/stat utime+stime are cumulative counters. Read them for every
//	live descendant at the phase boundaries and the difference IS that phase's
//	descendant CPU, with no sampling error at all.
//
// Background polling (descendantPollInterval) exists only as a catch-net for a
// descendant that EXITS inside a phase: its last increment would otherwise
// vanish from /proc before the closing read.
//
// # Reconciliation
//
// A process attributed live and later reaped ALSO lands in RUSAGE_CHILDREN, so
// the two sources must never simply be added. cpuLedger tracks, per process,
// how much of its cumulative CPU has already been accounted for; when a
// descendant disappears, exactly that much is subtracted from the reap lump
// before the remainder (the tail burned after the last observation) is charged.
// The result is exact rather than approximate, and is what makes a
// `snapshot-e2e` stage on the rclone lane account for kopia AND the rclone
// grandchild kopia spawned.
//
// A third source cross-checks the out-of-process stages: on Linux the kernel
// fills wait4's rusage with RUSAGE_BOTH of the dead child, so a stage's
// ProcessState already includes the CPU of the child's own reaped children. It
// is folded in with max(), never with +, so it can only ever raise a floor.
//
// # Residual
//
// Whatever CPU the trial spent outside a measured phase - setup writes, cleanup
// deletes, the cooldown, connection setup - is published per trial as
// cpu_residual_ms, so an attribution leak is visible in the result document
// itself instead of having to be inferred.

// descendantPollInterval is how often the catch-net poller re-reads the process
// tree. It only has to be fast enough that a descendant which exits mid-phase
// is observed close to its exit; the phase boundaries themselves are read
// exactly, not sampled.
const descendantPollInterval = 200 * time.Millisecond

// cpuTimes is a user/system CPU pair.
type cpuTimes struct {
	user time.Duration
	sys  time.Duration
}

// total is the pair's sum, which is what cpu_total_ms is built from.
func (c cpuTimes) total() time.Duration {
	return c.user + c.sys
}

func (c cpuTimes) plus(o cpuTimes) cpuTimes {
	return cpuTimes{user: c.user + o.user, sys: c.sys + o.sys}
}

// minus subtracts, clamping each component at zero: CPU counters only ever
// grow, so a negative result can only come from a racy or mismatched reading.
func (c cpuTimes) minus(o cpuTimes) cpuTimes {
	return cpuTimes{user: max(c.user-o.user, 0), sys: max(c.sys-o.sys, 0)}
}

// maxCPUTimes combines two estimates of the same quantity without ever adding
// them together.
func maxCPUTimes(a, b cpuTimes) cpuTimes {
	return cpuTimes{user: max(a.user, b.user), sys: max(a.sys, b.sys)}
}

// procKey identifies one observed process. The start time is part of the key so
// that a recycled PID cannot be mistaken for the process that held it before.
type procKey struct {
	pid   int
	start uint64
}

// cpuLedger records, per descendant process, how much of that process's
// cumulative CPU the harness has already accounted for - either because a phase
// was charged for it, or because it had already been burned when a phase opened
// and was therefore deliberately excluded from it.
//
// It is what keeps live attribution and reap-time RUSAGE_CHILDREN lumps from
// counting the same CPU twice. It is process-wide because a process tree is,
// and measured phases never overlap; the mutex is there because the catch-net
// poller and the phase boundaries touch it from different goroutines.
type cpuLedger struct {
	mu        sync.Mutex
	accounted map[procKey]cpuTimes
}

func newCPULedger() *cpuLedger {
	return &cpuLedger{accounted: map[procKey]cpuTimes{}}
}

// note raises the accounted mark of one process.
func (l *cpuLedger) note(k procKey, v cpuTimes) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.accounted[k] = maxCPUTimes(l.accounted[k], v)
}

// forget drops a process that is gone and returns what had been accounted for
// it, which is exactly the part of its reap lump that must not be charged
// again.
func (l *cpuLedger) forget(k procKey) cpuTimes {
	l.mu.Lock()
	defer l.mu.Unlock()

	v := l.accounted[k]
	delete(l.accounted, k)

	return v
}

// prune drops every process that is no longer alive, so a long run does not
// accumulate entries for processes nobody observed disappearing.
func (l *cpuLedger) prune(live map[procKey]cpuTimes) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for k := range l.accounted {
		if _, ok := live[k]; !ok {
			delete(l.accounted, k)
		}
	}
}

// processLedger is the harness-wide reconciliation ledger.
var processLedger = newCPULedger()

// cpuMeter is an open CPU measurement window over one phase.
type cpuMeter struct {
	ledger *cpuLedger

	rusageOK  bool
	self0     cpuTimes
	children0 cpuTimes

	desc *descendantMeter
}

// startCPUMeter opens a window. It never fails: on a platform without the
// accounting it simply reports nothing, and cpu_source says so.
func startCPUMeter() *cpuMeter {
	return startCPUMeterOn(processLedger)
}

// startCPUMeterOn is startCPUMeter against an explicit ledger, which is what
// the tests use so they cannot disturb each other.
func startCPUMeterOn(ledger *cpuLedger) *cpuMeter {
	m := &cpuMeter{ledger: ledger}

	self, selfOK := readRusage(rusageSelf)
	children, childrenOK := readRusage(rusageChildren)

	m.rusageOK = selfOK && childrenOK
	m.self0 = self
	m.children0 = children
	m.desc = startDescendantMeter(ledger)

	return m
}

// stop closes the window and returns what the phase cost.
func (m *cpuMeter) stop() cpuSample {
	if m == nil {
		return cpuSample{}
	}

	var s cpuSample

	live, reaped, procOK := m.desc.stop()

	if m.rusageOK {
		self, selfOK := readRusage(rusageSelf)
		children, childrenOK := readRusage(rusageChildren)

		if selfOK && childrenOK {
			s.self = self.minus(m.self0)
			s.fromRusage = true

			// The reap lump minus what was already attributed live is the tail every
			// descendant burned between its last observation and its exit. Without
			// the subtraction this would re-charge the whole life of every process
			// the phase already paid for.
			s.children = children.minus(m.children0).minus(reaped)
		}
	}

	if procOK {
		s.children = s.children.plus(live)
		s.fromProc = true
	}

	return s
}

// cpuSample is one phase's CPU cost, split between the harness process and
// everything below it.
type cpuSample struct {
	self     cpuTimes
	children cpuTimes

	fromRusage bool
	fromProc   bool
	fromWait4  bool
}

// total is the sample's whole machine cost.
func (s cpuSample) total() cpuTimes {
	return s.self.plus(s.children)
}

// Names of the CPU accounting sources, as they appear in cpu_source.
const (
	cpuSourceNone   = "none"
	cpuSourceRusage = "rusage"
	cpuSourceProc   = "proc"
	cpuSourceWait4  = "wait4"
)

// source describes which accounting the sample actually used, "+"-joined in a
// fixed order.
func (s cpuSample) source() string {
	parts := []string{}

	if s.fromRusage {
		parts = append(parts, cpuSourceRusage)
	}

	if s.fromProc {
		parts = append(parts, cpuSourceProc)
	}

	if s.fromWait4 {
		parts = append(parts, cpuSourceWait4)
	}

	if len(parts) == 0 {
		return cpuSourceNone
	}

	return strings.Join(parts, "+")
}

// withProcessState folds in the exact rusage of a child the harness waited on.
// On Linux that rusage covers the child's own reaped descendants too, so it is
// an independent measurement of the same quantity the ledger reconstructs -
// hence max() and not addition: it can raise a floor (if /proc and
// RUSAGE_CHILDREN were both unavailable) and never inflate a working one.
func (s cpuSample) withProcessState(st *os.ProcessState) cpuSample {
	if st == nil {
		return s
	}

	s.children = maxCPUTimes(s.children, cpuTimes{user: st.UserTime(), sys: st.SystemTime()})
	s.fromWait4 = true

	return s
}

// applyTo writes the sample into a phase's metrics. It must be called after
// WallClockMS is set, because cpu_pct is relative to it.
func (s cpuSample) applyTo(m *WorkloadMetrics) {
	m.CPUUserMS = durationMS(s.self.user)
	m.CPUSysMS = durationMS(s.self.sys)
	m.CPUChildrenUserMS = durationMS(s.children.user)
	m.CPUChildrenSysMS = durationMS(s.children.sys)
	m.CPUTotalMS = durationMS(s.total().total())
	m.CPUSource = s.source()

	if m.WallClockMS > 0 {
		m.CPUPct = m.CPUTotalMS / m.WallClockMS * percentFull
	}
}

// durationMS renders a duration the way every other harness metric is rendered.
func durationMS(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / millisPerNanoFloat
}

// ---------------------------------------------------------------------------
// the descendant tree
// ---------------------------------------------------------------------------

// descendantMeter attributes the CPU of every process below this one to the
// phase it was burned in.
//
// At the opening boundary it reads every live descendant's cumulative counter
// and records it as already-accounted, so a phase is never charged for CPU that
// was burned before it started. At the closing boundary it reads them again;
// the difference is the phase's descendant CPU, exactly. The poller in between
// only exists so that a descendant which exits mid-phase still contributes
// everything it burned up to its last observation.
type descendantMeter struct {
	ledger *cpuLedger

	mu   sync.Mutex
	base map[procKey]cpuTimes
	seen map[procKey]cpuTimes

	stopCh chan struct{}
	doneCh chan struct{}
}

// startDescendantMeter begins a window, or returns nil where /proc is not
// available.
func startDescendantMeter(ledger *cpuLedger) *descendantMeter {
	base, ok := walkDescendants()
	if !ok {
		return nil
	}

	m := &descendantMeter{
		ledger: ledger,
		base:   base,
		seen:   map[procKey]cpuTimes{},
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	for k, v := range base {
		// Everything already burned belongs to an earlier window (or to the run's
		// unattributed residual), never to this phase.
		ledger.note(k, v)
	}

	go m.poll()

	return m
}

func (m *descendantMeter) poll() {
	defer close(m.doneCh)

	t := time.NewTicker(descendantPollInterval)
	defer t.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-t.C:
			m.sample()
		}
	}
}

// sample takes one catch-net reading, keeping the largest value seen per
// process so an exit cannot lose the increment before it.
func (m *descendantMeter) sample() {
	cur, ok := walkDescendants()
	if !ok {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.record(cur)
}

// record folds one reading into the window. The caller holds the lock.
func (m *descendantMeter) record(cur map[procKey]cpuTimes) {
	for k, v := range cur {
		if old, ok := m.seen[k]; !ok || v.total() > old.total() {
			m.seen[k] = v
		}
	}
}

// stop ends the window and returns
//
//	live    the CPU every descendant burned inside it,
//	reaped  how much of that has already been accounted for processes that are
//	        now gone - the amount to subtract from the RUSAGE_CHILDREN lump so
//	        the same CPU is not charged twice.
func (m *descendantMeter) stop() (live, reaped cpuTimes, ok bool) {
	if m == nil {
		return cpuTimes{}, cpuTimes{}, false
	}

	close(m.stopCh)
	<-m.doneCh

	// The poller is gone, so this closing read races with nothing. It is the
	// exact boundary for every descendant that is still alive.
	cur, curOK := walkDescendants()

	m.mu.Lock()
	defer m.mu.Unlock()

	if curOK {
		m.record(cur)
	}

	for k, v := range m.seen {
		live = live.plus(v.minus(m.base[k]))

		// The mark is raised BEFORE the process is dropped, so that what this
		// phase just attributed live is part of what gets handed back. Raising it
		// only for survivors would leave a process that was born and died inside
		// one phase with an accounted mark of zero, its whole life would be
		// charged again out of the RUSAGE_CHILDREN lump, and the phase would be
		// billed twice for it.
		m.ledger.note(k, v)

		if _, stillAlive := cur[k]; curOK && !stillAlive {
			// Gone: its whole life arrives in RUSAGE_CHILDREN when its parent is
			// reaped, so hand back everything already accounted for it.
			reaped = reaped.plus(m.ledger.forget(k))
		}
	}

	return live, reaped, true
}

// ---------------------------------------------------------------------------
// trial-level totals and the residual
// ---------------------------------------------------------------------------

// trialCPUMeter measures the CPU the whole trial cost the machine, which is
// what makes the per-phase attribution auditable: everything the trial spent
// that no phase claimed is published as cpu_residual_ms.
type trialCPUMeter struct {
	ledger *cpuLedger

	ok        bool
	self0     cpuTimes
	children0 cpuTimes
	live0     map[procKey]cpuTimes
}

// startTrialCPUMeter opens the trial-level window.
func startTrialCPUMeter() *trialCPUMeter {
	m := &trialCPUMeter{ledger: processLedger}

	self, selfOK := readRusage(rusageSelf)
	children, childrenOK := readRusage(rusageChildren)

	m.ok = selfOK && childrenOK
	m.self0 = self
	m.children0 = children

	if live, ok := walkDescendants(); ok {
		m.live0 = live
	}

	return m
}

// stop returns the trial's whole machine CPU:
//
//	RUSAGE_SELF delta + RUSAGE_CHILDREN delta + the increment of every
//	descendant that is still alive (and therefore not in RUSAGE_CHILDREN yet).
//
// The three terms are disjoint by construction, provided no descendant that was
// alive when the trial opened is reaped inside it - which the harness arranges
// by opening and closing every connection inside a workload.
func (m *trialCPUMeter) stop() (cpuTimes, bool) {
	if m == nil || !m.ok {
		return cpuTimes{}, false
	}

	self, selfOK := readRusage(rusageSelf)
	children, childrenOK := readRusage(rusageChildren)

	if !selfOK || !childrenOK {
		return cpuTimes{}, false
	}

	out := self.minus(m.self0).plus(children.minus(m.children0))

	if live, ok := walkDescendants(); ok {
		for k, v := range live {
			out = out.plus(v.minus(m.live0[k]))
		}

		m.ledger.prune(live)
	}

	return out, true
}

// ---------------------------------------------------------------------------
// the accounting tripwire
// ---------------------------------------------------------------------------

// Thresholds of the CPU accounting tripwire.
const (
	// suspectCPUMinWallMS is how much wall clock a phase needs before its CPU is
	// worth judging at all. A 50 ms phase legitimately shows almost no CPU.
	suspectCPUMinWallMS = 1000.0

	// suspectCPUFractionOfWall is the share of wall clock below which a phase's
	// measured CPU stops being believable for work that involves TLS, HTTP and a
	// WebDAV hop.
	suspectCPUFractionOfWall = 0.005

	// suspectCPUFloorMS is an absolute floor, so a very long phase is not held to
	// an unreasonably large absolute number.
	suspectCPUFloorMS = 20.0
)

// suspectCPUAccounting reports whether a phase's CPU figure is implausibly
// small for the work it did, which means the accounting leaked rather than that
// the backend is free.
//
// It exists because the failure mode is asymmetric and flattering: an rclone
// phase whose subprocess CPU was not attributed reports a number that looks
// like a win. Any cell this flags must be published as BROKEN-ACCOUNTING rather
// than as a measurement.
func suspectCPUAccounting(wallMS, cpuTotalMS float64) bool {
	if wallMS < suspectCPUMinWallMS {
		return false
	}

	return cpuTotalMS < max(suspectCPUFloorMS, wallMS*suspectCPUFractionOfWall)
}
