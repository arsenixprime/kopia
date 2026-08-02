//go:build !no_extra_providers

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/blob/gdrive"
)

func TestPercentileKnownSeries(t *testing.T) {
	v := []float64{10, 1, 2, 3, 4, 5, 6, 7, 8, 9}

	cases := []struct {
		p    float64
		want float64
	}{
		{0, 1},
		{10, 1.9},
		{50, 5.5},
		{90, 9.1},
		{99, 9.91},
		{100, 10},
	}

	for _, tc := range cases {
		require.InDelta(t, tc.want, percentile(v, tc.p), 1e-9, "p%v", tc.p)
	}
}

func TestPercentileEdgeCases(t *testing.T) {
	require.InDelta(t, 0.0, percentile(nil, 50), 1e-9)
	require.InDelta(t, 7.0, percentile([]float64{7}, 10), 1e-9)
	require.InDelta(t, 7.0, percentile([]float64{7}, 99), 1e-9)

	// Percentiles beyond the range clamp rather than extrapolate.
	require.InDelta(t, 1.0, percentile([]float64{1, 2}, -5), 1e-9)
	require.InDelta(t, 2.0, percentile([]float64{1, 2}, 500), 1e-9)
}

func TestPercentileDoesNotMutateInput(t *testing.T) {
	in := []float64{3, 1, 2}
	percentile(in, 50)
	require.Equal(t, []float64{3, 1, 2}, in)
}

func TestSummarize(t *testing.T) {
	s := summarize([]float64{1, 2, 3, 4, 5})
	require.Equal(t, 5, s.N)
	require.InDelta(t, 3.0, s.Median, 1e-9)
	require.InDelta(t, 1.4, s.P10, 1e-9)
	require.InDelta(t, 4.6, s.P90, 1e-9)
	require.InDelta(t, 1.0, s.Min, 1e-9)
	require.InDelta(t, 5.0, s.Max, 1e-9)

	require.Equal(t, StatSummary{}, summarize(nil))
}

func TestSamplerCollectsMetrics(t *testing.T) {
	s := newSampler(nil)
	s.observe(2*time.Millisecond, nil)
	s.observe(4*time.Millisecond, nil)
	s.observe(6*time.Millisecond, errFake)
	s.up(100)
	s.down(50)

	m := s.finish(nil)

	require.Equal(t, 3, m.Ops)
	require.Equal(t, 1, m.Errors)
	require.Equal(t, 3, m.Samples)
	require.Equal(t, int64(100), m.BytesUp)
	require.Equal(t, int64(50), m.BytesDown)
	require.InDelta(t, 4.0, m.Latency.P50, 1e-9)
	require.Nil(t, m.Transport)
	require.Equal(t, quotaSourceNone, m.QuotaUnitsSource)
	require.NotEmpty(t, m.Error)
	require.Error(t, s.failure("test"))
}

func TestDiffTransportStats(t *testing.T) {
	before := gdrive.TransportStatsSnapshot{
		SnapshotTime:     time.Unix(100, 0).UTC(),
		LatencyBucketsMS: []int64{1, 2},
		PerMethod: map[string]gdrive.MethodStats{
			"files.get": {Calls: 5, BytesDown: 500, LatencyMS: []int64{1, 2, 3}},
		},
		ErrorsByReason: map[string]int64{"http_403": 2},
	}

	after := gdrive.TransportStatsSnapshot{
		SnapshotTime:     time.Unix(160, 0).UTC(),
		LatencyBucketsMS: []int64{1, 2},
		PerMethod: map[string]gdrive.MethodStats{
			"files.get":  {Calls: 9, BytesDown: 900, LatencyMS: []int64{4, 2, 8}},
			"files.list": {Calls: 2},
		},
		ErrorsByReason: map[string]int64{"http_403": 5, "http_429": 1},
	}

	d := diffTransportStats(before, after)

	require.Equal(t, before.SnapshotTime, d.StartTime)
	require.Equal(t, after.SnapshotTime, d.SnapshotTime)
	require.Equal(t, int64(4), d.PerMethod["files.get"].Calls)
	require.Equal(t, int64(400), d.PerMethod["files.get"].BytesDown)
	require.Equal(t, []int64{3, 0, 5}, d.PerMethod["files.get"].LatencyMS)
	require.Equal(t, int64(2), d.PerMethod["files.list"].Calls)
	require.Equal(t, map[string]int64{"http_403": 3, "http_429": 1}, d.ErrorsByReason)

	// 4 metadata reads (5 QU each) + 2 listings (100 QU each).
	require.Equal(t, int64(4*quReadItem+2*quListItems), quotaUnitsOfSnapshot(d))
}

func TestDiffTransportStatsCarriesPacedWaits(t *testing.T) {
	before := gdrive.TransportStatsSnapshot{
		SnapshotTime: time.Unix(100, 0).UTC(),
		PerMethod: map[string]gdrive.MethodStats{
			"files.delete": {Calls: 5, PacedWaits: 2, PacedWaitMS: 200},
		},
		PacedWaits:  2,
		PacedWaitMS: 200,
	}

	after := gdrive.TransportStatsSnapshot{
		SnapshotTime: time.Unix(160, 0).UTC(),
		PerMethod: map[string]gdrive.MethodStats{
			"files.delete":  {Calls: 20, PacedWaits: 14, PacedWaitMS: 1400},
			"upload.simple": {Calls: 3, PacedWaits: 1, PacedWaitMS: 50},
		},
		PacedWaits:  15,
		PacedWaitMS: 1450,
	}

	d := diffTransportStats(before, after)

	require.Equal(t, int64(12), d.PerMethod["files.delete"].PacedWaits)
	require.Equal(t, int64(1200), d.PerMethod["files.delete"].PacedWaitMS)
	require.Equal(t, int64(1), d.PerMethod["upload.simple"].PacedWaits)

	// The window totals are re-derived from the diffed methods, never carried
	// over from the absolute snapshots.
	require.Equal(t, int64(13), d.PacedWaits)
	require.Equal(t, int64(1250), d.PacedWaitMS)
}

// TestPacedWaitReachesTheWorkloadMetric pins the whole path a pacer hypothesis
// is read through: transport counters -> phase delta -> paced_wait_ms ->
// compare.
func TestPacedWaitReachesTheWorkloadMetric(t *testing.T) {
	stats := gdrive.NewTransportStats()
	stats.RecordPacedWait("files.delete", 300*time.Millisecond)
	stats.RecordPacedWait("files.delete", 700*time.Millisecond)

	delta := diffTransportStats(gdrive.TransportStatsSnapshot{}, stats.Snapshot())

	m := WorkloadMetrics{Transport: &delta, PacedWaitMS: float64(delta.PacedWaitMS)}

	v, ok := metricValue(m, "paced_wait_ms")
	require.True(t, ok)
	require.InDelta(t, 1000.0, v, 1e-9)
	require.Contains(t, metricNames, "paced_wait_ms")

	// A backend without Drive instrumentation reports 0 rather than nothing, so
	// the metric is always comparable.
	zero, ok := metricValue(WorkloadMetrics{}, "paced_wait_ms")
	require.True(t, ok)
	require.InDelta(t, 0.0, zero, 1e-9)
}

func TestMergeErrorsByReason(t *testing.T) {
	dst := map[string]int64{"a": 1}
	mergeErrorsByReason(dst, map[string]int64{"a": 2, "b": 3})
	require.Equal(t, map[string]int64{"a": 3, "b": 3}, dst)
}
