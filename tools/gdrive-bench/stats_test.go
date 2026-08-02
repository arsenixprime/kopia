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

func TestMergeErrorsByReason(t *testing.T) {
	dst := map[string]int64{"a": 1}
	mergeErrorsByReason(dst, map[string]int64{"a": 2, "b": 3})
	require.Equal(t, map[string]int64{"a": 3, "b": 3}, dst)
}
