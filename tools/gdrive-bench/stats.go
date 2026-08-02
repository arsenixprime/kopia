//go:build !no_extra_providers

package main

import (
	"math"
	"slices"
	"time"

	"github.com/kopia/kopia/internal/timetrack"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/gdrive"
)

// percentile returns the p-th percentile (0..100) of values using linear
// interpolation between the two closest ranks - the definition used by Excel's
// PERCENTILE, NumPy's default and R's type 7. It is spelled out here because
// every number the tuning loop looks at comes out of it:
//
//	rank = p/100 * (n-1); result = v[floor(rank)] + frac * (v[ceil(rank)] - v[floor(rank)])
//
// so the median of an even-sized sample is the average of the middle two, and
// the percentiles of a single-sample series are all that one sample. values
// does not have to be sorted; it is copied, never modified in place.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := slices.Clone(values)
	slices.Sort(sorted)

	return percentileSorted(sorted, p)
}

// percentileSorted is percentile over an already-sorted slice.
func percentileSorted(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}

	if p <= 0 {
		return sorted[0]
	}

	if p >= percentFull {
		return sorted[n-1]
	}

	rank := p / percentFull * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))

	if lo == hi {
		return sorted[lo]
	}

	frac := rank - float64(lo)

	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// percentFull is 100%, named so the arithmetic above does not trip the
// magic-number linter.
const percentFull = 100.0

// summarize computes the median and the p10/p90 spread of a series, which is
// all the comparison protocol looks at.
func summarize(values []float64) StatSummary {
	if len(values) == 0 {
		return StatSummary{}
	}

	sorted := slices.Clone(values)
	slices.Sort(sorted)

	return StatSummary{
		N:      len(sorted),
		Median: percentileSorted(sorted, medianPercentile),
		P10:    percentileSorted(sorted, lowPercentile),
		P90:    percentileSorted(sorted, highPercentile),
		Min:    sorted[0],
		Max:    sorted[len(sorted)-1],
	}
}

// The percentiles the harness reports.
const (
	lowPercentile      = 10.0
	medianPercentile   = 50.0
	highPercentile     = 90.0
	tailPercentile     = 99.0
	millisPerNanoFloat = float64(time.Millisecond)
)

// percentilesOf computes the reported latency percentiles of a sample series.
func percentilesOf(values []float64) Percentiles {
	sorted := slices.Clone(values)
	slices.Sort(sorted)

	return Percentiles{
		P10: percentileSorted(sorted, lowPercentile),
		P50: percentileSorted(sorted, medianPercentile),
		P90: percentileSorted(sorted, highPercentile),
		P99: percentileSorted(sorted, tailPercentile),
	}
}

// sampler accumulates the measurements of one workload phase and turns them
// into a WorkloadMetrics.
//
// Latency samples are per operation and are recorded in milliseconds as
// floating point, so sub-millisecond filesystem operations (which is what the
// CI self-test measures) do not all collapse onto zero.
type sampler struct {
	timer   timetrack.Timer
	samples []float64
	ops     int
	errs    int
	bytesUp int64
	bytesDn int64
	before  *gdrive.TransportStatsSnapshot
	firstEr error
}

// newSampler starts a measurement window over st. When st is a native Drive
// storage, the Drive API counters are snapshotted so the window can be
// attributed; for every other backend the transport delta stays absent.
func newSampler(st blob.Storage) *sampler {
	s := &sampler{timer: timetrack.StartTimer()}

	if st != nil {
		if snap, ok := gdrive.StatsFromStorage(st); ok {
			s.before = &snap
		}
	}

	return s
}

// observe records one operation of the given duration and its outcome.
func (s *sampler) observe(d time.Duration, err error) {
	s.ops++
	s.samples = append(s.samples, float64(d.Nanoseconds())/millisPerNanoFloat)

	if err != nil {
		s.errs++

		if s.firstEr == nil {
			s.firstEr = err
		}
	}
}

// up records uploaded payload bytes.
func (s *sampler) up(n int64) { s.bytesUp += n }

// down records downloaded payload bytes.
func (s *sampler) down(n int64) { s.bytesDn += n }

// finish closes the measurement window and returns the metrics. st is the
// storage the after-snapshot is taken from; it may be a different connection
// than the one the window started on (the cold-cache pass reopens the storage),
// in which case the delta is that connection's own totals.
func (s *sampler) finish(st blob.Storage) WorkloadMetrics {
	m := WorkloadMetrics{
		WallClockMS: float64(s.timer.Elapsed().Nanoseconds()) / millisPerNanoFloat,
		Ops:         s.ops,
		Errors:      s.errs,
		BytesUp:     s.bytesUp,
		BytesDown:   s.bytesDn,
		Samples:     len(s.samples),
		Latency:     percentilesOf(s.samples),
	}

	if s.firstEr != nil {
		m.Error = s.firstEr.Error()
	}

	if st != nil {
		if after, ok := gdrive.StatsFromStorage(st); ok {
			d := after
			if s.before != nil {
				d = diffTransportStats(*s.before, after)
			}

			m.Transport = &d
			m.QuotaUnits = quotaUnitsOfSnapshot(d)
			m.QuotaUnitsSource = quotaSourceObserved

			return m
		}
	}

	m.QuotaUnitsSource = quotaSourceNone

	return m
}

// diffTransportStats subtracts before from after, element by element, producing
// the traffic that happened between the two snapshots. Counters only ever grow
// within a connection, so a negative result can only come from mismatched
// snapshots and is clamped to zero.
func diffTransportStats(before, after gdrive.TransportStatsSnapshot) gdrive.TransportStatsSnapshot {
	res := gdrive.TransportStatsSnapshot{
		StartTime:        before.SnapshotTime,
		SnapshotTime:     after.SnapshotTime,
		LatencyBucketsMS: slices.Clone(after.LatencyBucketsMS),
		PerMethod:        make(map[string]gdrive.MethodStats, len(after.PerMethod)),
		ErrorsByReason:   make(map[string]int64, len(after.ErrorsByReason)),
	}

	for k, a := range after.PerMethod {
		res.PerMethod[k] = diffMethodStats(before.PerMethod[k], a)
	}

	for k, a := range after.ErrorsByReason {
		if d := a - before.ErrorsByReason[k]; d > 0 {
			res.ErrorsByReason[k] = d
		}
	}

	return res
}

// diffMethodStats subtracts one method's counters.
func diffMethodStats(before, after gdrive.MethodStats) gdrive.MethodStats {
	res := gdrive.MethodStats{
		Calls:        nonNegative(after.Calls - before.Calls),
		Errors:       nonNegative(after.Errors - before.Errors),
		Retries:      nonNegative(after.Retries - before.Retries),
		Backoffs:     nonNegative(after.Backoffs - before.Backoffs),
		BytesUp:      nonNegative(after.BytesUp - before.BytesUp),
		BytesDown:    nonNegative(after.BytesDown - before.BytesDown),
		BackoffMS:    nonNegative(after.BackoffMS - before.BackoffMS),
		LatencySumMS: nonNegative(after.LatencySumMS - before.LatencySumMS),
	}

	if len(after.LatencyMS) > 0 {
		res.LatencyMS = make([]int64, len(after.LatencyMS))

		for i, v := range after.LatencyMS {
			var b int64

			if i < len(before.LatencyMS) {
				b = before.LatencyMS[i]
			}

			res.LatencyMS[i] = nonNegative(v - b)
		}
	}

	return res
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}

	return v
}

// mergeErrorsByReason accumulates the error reasons of a transport delta into
// the trial-level tally.
func mergeErrorsByReason(dst, src map[string]int64) {
	for k, v := range src {
		dst[k] += v
	}
}
