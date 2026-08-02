//go:build !no_extra_providers

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

var errFake = errors.New("fake error")

// synthResult builds a result document whose single workload metric takes the
// given per-trial values.
func synthResult(label string, targets map[string][]float64) *RunResult {
	r := &RunResult{
		SchemaVersion: schemaVersion,
		RunID:         "r" + label,
		Label:         label,
		StartedAt:     time.Unix(0, 0).UTC(),
		FinishedAt:    time.Unix(60, 0).UTC(),
		Harness:       HarnessInfo{Commit: "deadbeefdeadbeef"},
		Config:        RunConfig{Trials: 1, Order: orderABBA, Params: defaultParams()},
		Trials:        []TrialResult{},
		Errors:        []string{},
	}

	for name, values := range targets {
		r.Targets = append(r.Targets, TargetInfo{Name: name, StorageType: "filesystem"})

		for i, v := range values {
			r.Trials = append(r.Trials, TrialResult{
				Trial:  i,
				Target: name,
				Workloads: map[string]WorkloadMetrics{
					"small-write": {
						WallClockMS: v,
						Ops:         10,
						Samples:     10,
						Latency:     Percentiles{P10: v, P50: v, P90: v, P99: v},
					},
				},
				ErrorsByReason: map[string]int64{},
			})
		}
	}

	return r
}

// synthOrderedResult builds a two-target result document whose targets[] order
// is the given one, with one wall_clock_ms value per trial per target. The
// names are chosen by the caller so that a test can make configuration order
// and alphabetical order disagree.
func synthOrderedResult(names [2]string, values [2][]float64) *RunResult {
	const label = "cycle"

	r := &RunResult{
		SchemaVersion: schemaVersion,
		RunID:         "r" + label,
		Label:         label,
		StartedAt:     time.Unix(0, 0).UTC(),
		FinishedAt:    time.Unix(60, 0).UTC(),
		Harness:       HarnessInfo{Commit: "deadbeefdeadbeef"},
		Config:        RunConfig{Trials: len(values[0]), Order: orderABBA, Params: defaultParams()},
		Trials:        []TrialResult{},
		Errors:        []string{},
	}

	for _, n := range names {
		r.Targets = append(r.Targets, TargetInfo{Name: n, StorageType: "filesystem"})
	}

	// Trials are appended ABBA-style, so execution order differs from
	// configuration order too and cannot accidentally stand in for it.
	for i := range values[0] {
		order := []int{0, 1}
		if i%2 == 1 {
			order = []int{1, 0}
		}

		for _, ti := range order {
			r.Trials = append(r.Trials, TrialResult{
				Trial:  i,
				Target: names[ti],
				Workloads: map[string]WorkloadMetrics{
					"small-write": {
						WallClockMS: values[ti][i],
						Ops:         10,
						Samples:     10,
						Latency:     Percentiles{P10: values[ti][i], P50: values[ti][i], P90: values[ti][i], P99: values[ti][i]},
					},
				},
				ErrorsByReason: map[string]int64{},
			})
		}
	}

	return r
}

// TestCompareReferenceFollowsConfigurationOrder is the regression test for the
// sign-inversion hazard tuning cycle 1 hit: "control" sorts AFTER "candidate",
// so an alphabetical reference silently reads every delta backwards.
func TestCompareReferenceFollowsConfigurationOrder(t *testing.T) {
	// control is configured first and is the slower lane.
	res := synthOrderedResult([2]string{"control", "candidate"}, [2][]float64{
		{200, 210, 205},
		{100, 105, 102},
	})

	require.Equal(t, []string{"control", "candidate"}, res.targetNames(),
		"targetNames must follow targets[], which is targets-file order")

	series := buildSeries([]string{"x.json"}, []*RunResult{res})
	require.Equal(t, "control", series[0].Key)

	rep := compareSeries(series)
	require.Equal(t, "control", rep.Reference)

	row := findRow(t, rep, "small-write", "wall_clock_ms")
	require.Equal(t, "candidate", row.Target)
	require.Equal(t, sigReal, row.Significance)
	require.Negative(t, row.DeltaPct, "the faster candidate must read as a negative delta against the control reference")
	require.True(t, row.Improvement)
}

func TestCompareReferenceOverride(t *testing.T) {
	res := synthOrderedResult([2]string{"control", "candidate"}, [2][]float64{
		{200, 210, 205},
		{100, 105, 102},
	})

	series := buildSeries([]string{"x.json"}, []*RunResult{res})

	flipped, err := selectReference(series, "candidate")
	require.NoError(t, err)
	require.Equal(t, []string{"candidate", "control"}, seriesKeys(flipped))

	rep := compareSeries(flipped)
	require.Equal(t, "candidate", rep.Reference)

	row := findRow(t, rep, "small-write", "wall_clock_ms")
	require.Equal(t, "control", row.Target)
	require.Positive(t, row.DeltaPct, "with the candidate as reference the slower control reads positive")

	// An empty override changes nothing.
	same, err := selectReference(series, "")
	require.NoError(t, err)
	require.Equal(t, series, same)

	// An unknown name is refused, and the message names what is available.
	_, err = selectReference(series, "nosuchlane")
	require.Error(t, err)
	require.Contains(t, err.Error(), "control")
	require.Contains(t, err.Error(), "candidate")
}

func TestCompareReferenceOverrideOnAmbiguousTargetName(t *testing.T) {
	res := synthOrderedResult([2]string{"control", "candidate"}, [2][]float64{{1, 2}, {3, 4}})
	series := buildSeries([]string{"a.json", "b.json"}, []*RunResult{res, res})

	// The same target in two files gets suffixed keys, so the bare target name
	// is ambiguous and must be refused rather than guessed.
	_, err := selectReference(series, "control")
	require.Error(t, err)
	require.Contains(t, err.Error(), "control@0")

	picked, err := selectReference(series, "control@1")
	require.NoError(t, err)
	require.Equal(t, "control@1", picked[0].Key)
}

func TestCompareCommandReferenceFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cycle.json")
	jsonOut := filepath.Join(dir, "report.json")

	require.NoError(t, writeResult(path, synthOrderedResult([2]string{"control", "candidate"}, [2][]float64{
		{200, 210, 205},
		{100, 105, 102},
	})))

	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{"compare", "--reference", "candidate", "--json-out", jsonOut, path}, &out, &errOut)
	require.Equal(t, exitOK, code, errOut.String())
	require.Contains(t, out.String(), "reference: candidate")

	data, err := os.ReadFile(jsonOut)
	require.NoError(t, err)

	rep := &CompareReport{}
	require.NoError(t, json.Unmarshal(data, rep))
	require.Equal(t, "candidate", rep.Reference)

	// Without the flag the reference is the first configured target.
	out.Reset()
	errOut.Reset()

	code = dispatch(context.Background(), []string{"compare", "--json-out", jsonOut, path}, &out, &errOut)
	require.Equal(t, exitOK, code, errOut.String())
	require.Contains(t, out.String(), "reference: control")

	// A name no series carries is refused before anything is written.
	errOut.Reset()

	code = dispatch(context.Background(), []string{"compare", "--reference", "nope", path}, &out, &errOut)
	require.Equal(t, exitRefused, code)
	require.Contains(t, errOut.String(), "no series named")
}

func findRow(t *testing.T, rep *CompareReport, workload, metric string) CompareRow {
	t.Helper()

	for i := range rep.Rows {
		if rep.Rows[i].Workload == workload && rep.Rows[i].Metric == metric {
			return rep.Rows[i]
		}
	}

	t.Fatalf("no %v/%v row in the report", workload, metric)

	return CompareRow{}
}

func TestClassifyDisjointIntervalsAreReal(t *testing.T) {
	ref := summarize([]float64{100, 101, 102, 103, 104})
	cand := summarize([]float64{200, 201, 202, 203, 204})

	verdict, delta, undefined := classify(ref, cand)

	require.Equal(t, sigReal, verdict)
	require.False(t, undefined)
	require.InDelta(t, 98.0, delta, 0.5)
}

func TestClassifyOverlappingSmallDeltaIsNoise(t *testing.T) {
	ref := summarize([]float64{100, 105, 110, 95, 90})
	cand := summarize([]float64{101, 106, 111, 96, 91})

	verdict, delta, _ := classify(ref, cand)

	require.Equal(t, sigNoise, verdict)
	require.Less(t, delta, noiseThresholdPct)
}

func TestClassifyOverlappingLargeDeltaIsWeak(t *testing.T) {
	// Wide, overlapping distributions whose medians are 50% apart: the harness
	// must NOT call this real.
	ref := summarize([]float64{10, 50, 100, 150, 200})
	cand := summarize([]float64{15, 75, 150, 225, 300})

	verdict, delta, _ := classify(ref, cand)

	require.Equal(t, sigWeak, verdict)
	require.InDelta(t, 50.0, delta, 1e-9)
}

func TestClassifyMissingSeriesIsNoData(t *testing.T) {
	verdict, _, undefined := classify(StatSummary{}, summarize([]float64{1}))
	require.Equal(t, sigNoData, verdict)
	require.True(t, undefined)
}

func TestClassifyZeroReferenceMedianIsUndefinedNotInfinite(t *testing.T) {
	ref := summarize([]float64{0, 0, 0})
	cand := summarize([]float64{5, 5, 5})

	verdict, delta, undefined := classify(ref, cand)

	require.Equal(t, sigReal, verdict)
	require.True(t, undefined)
	require.InDelta(t, 0.0, delta, 1e-9)

	// A report containing this row must still be encodable.
	row := compareOne("small-write", "wall_clock_ms", "a", "b", ref, cand)
	_, err := json.Marshal(row)
	require.NoError(t, err)
}

func TestClassifyBothZeroIsNoise(t *testing.T) {
	zero := summarize([]float64{0, 0})

	verdict, delta, undefined := classify(zero, zero)

	require.Equal(t, sigNoise, verdict)
	require.False(t, undefined)
	require.InDelta(t, 0.0, delta, 1e-9)
}

func TestIntervalsOverlap(t *testing.T) {
	require.True(t, intervalsOverlap(1, 5, 5, 9))
	require.True(t, intervalsOverlap(1, 5, 2, 3))
	require.False(t, intervalsOverlap(1, 5, 5.1, 9))
	require.False(t, intervalsOverlap(5.1, 9, 1, 5))
}

func TestCompareSelfIsAllNoise(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self.json")

	require.NoError(t, writeResult(path, synthResult("self", map[string][]float64{"native": {100, 110, 120}})))

	r, err := readResult(path)
	require.NoError(t, err)

	series := buildSeries([]string{path, path}, []*RunResult{r, r})
	require.Len(t, series, 2)
	require.Equal(t, "native@0", series[0].Key)
	require.Equal(t, "native@1", series[1].Key)

	rep := compareSeries(series)
	require.NotEmpty(t, rep.Rows)

	for _, row := range rep.Rows {
		require.Equal(t, sigNoise, row.Significance, "%v/%v", row.Workload, row.Metric)
		require.InDelta(t, 0.0, row.DeltaPct, 1e-9)
	}
}

func TestCompareDistinctTargetsKeepPlainKeys(t *testing.T) {
	r := synthResult("two", map[string][]float64{"a": {1, 2}, "b": {3, 4}})
	series := buildSeries([]string{"x.json"}, []*RunResult{r})

	require.Len(t, series, 2)

	keys := []string{series[0].Key, series[1].Key}
	require.ElementsMatch(t, []string{"a", "b"}, keys)
}

func TestCompareCommandOnSingleMultiTargetFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "two.json")
	jsonOut := filepath.Join(dir, "report.json")

	require.NoError(t, writeResult(path, synthResult("two", map[string][]float64{
		"slow": {200, 210, 205},
		"fast": {100, 105, 102},
	})))

	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{"compare", "--json-out", jsonOut, path}, &out, &errOut)
	require.Equal(t, exitOK, code, errOut.String())
	require.Contains(t, out.String(), "wall_clock_ms")

	data, err := os.ReadFile(jsonOut)
	require.NoError(t, err)

	rep := &CompareReport{}
	require.NoError(t, json.Unmarshal(data, rep))
	require.Equal(t, compareSchemaVersion, rep.SchemaVersion)
	require.Len(t, rep.Sources, 2)
	require.NotEmpty(t, rep.Rows)

	var found bool

	for _, row := range rep.Rows {
		if row.Workload == "small-write" && row.Metric == "wall_clock_ms" {
			found = true

			require.Equal(t, sigReal, row.Significance)
		}
	}

	require.True(t, found, "expected a wall_clock_ms row")
}

func TestCompareRejectsSingleSeries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one.json")

	require.NoError(t, writeResult(path, synthResult("one", map[string][]float64{"only": {1, 2}})))

	var out, errOut bytes.Buffer

	require.Equal(t, exitRefused, dispatch(context.Background(), []string{"compare", path}, &out, &errOut))
	require.Contains(t, errOut.String(), "at least 2 series")
}

func TestIsRegressionOnlyOnRealMaterialWorsening(t *testing.T) {
	worse := CompareRow{Significance: sigReal, DeltaPct: 40}
	require.True(t, isRegression(&worse, defaultRegressionThresholdPct))

	small := CompareRow{Significance: sigReal, DeltaPct: 5}
	require.False(t, isRegression(&small, defaultRegressionThresholdPct))

	weak := CompareRow{Significance: sigWeak, DeltaPct: 40}
	require.False(t, isRegression(&weak, defaultRegressionThresholdPct))

	better := CompareRow{Significance: sigReal, DeltaPct: -40, Improvement: true}
	require.False(t, isRegression(&better, defaultRegressionThresholdPct))
}
