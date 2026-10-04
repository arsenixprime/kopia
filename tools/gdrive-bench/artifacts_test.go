//go:build !no_extra_providers

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/blob/gdrive"
	"github.com/kopia/kopia/repo/blob/gdrive/gdrivepacer"
)

func TestResultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")

	in := synthResult("round-trip", map[string][]float64{"native": {10, 20}})
	in.Targets[0].StorageType = "gdrive"
	in.Targets[0].Knobs = map[string]any{"pacerMinSleepMS": 50.0}
	in.Targets[0].Config = map[string]any{"folderID": "f", "credentialsFile": redactedMarker}
	in.Quota = QuotaReport{
		Decision:            quotaDecision{Allowed: true, Fraction: 0.5, BudgetBytes: 1, EstimatedQU: 2, QUCeiling: perUserQUPerMinute},
		ActualUploadBytes:   1234,
		ActualQuotaUnits:    56,
		ObservedQUPerMinute: 7.5,
		LedgerPath:          "/tmp/ledger.json",
	}
	in.Errors = []string{"cleanup of x: boom"}
	in.Trials[0].ErrorsByReason = map[string]int64{"http_403": 2}
	in.Trials[0].Workloads["small-write"] = WorkloadMetrics{
		WallClockMS:      12.5,
		Ops:              10,
		BytesUp:          99,
		Samples:          10,
		Latency:          Percentiles{P10: 1, P50: 2, P90: 3, P99: 4},
		QuotaUnits:       500,
		QuotaUnitsSource: quotaSourceObserved,
		Transport: &gdrive.TransportStatsSnapshot{
			StartTime:        time.Unix(0, 0).UTC(),
			SnapshotTime:     time.Unix(10, 0).UTC(),
			LatencyBucketsMS: []int64{1, 2, 5},
			PerMethod:        map[string]gdrive.MethodStats{"files.create": {Calls: 10, BytesUp: 99, LatencyMS: []int64{1, 2, 3, 4}}},
			ErrorsByReason:   map[string]int64{"http_429": 1},
		},
	}

	require.NoError(t, writeResult(path, in))

	out, err := readResult(path)
	require.NoError(t, err)
	require.Equal(t, in, out)

	// Results are owner-only.
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(filePermissions), st.Mode().Perm())
}

func TestReadResultRejectsForeignSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.json")

	require.NoError(t, writeFileAtomic(path, []byte(`{"schema_version": 99}`)))

	_, err := readResult(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "schema_version")
}

func TestMetricValue(t *testing.T) {
	m := WorkloadMetrics{
		WallClockMS: 500,
		Ops:         10,
		Errors:      2,
		BytesUp:     100,
		BytesDown:   200,
		Latency:     Percentiles{P10: 1, P50: 2, P90: 3, P99: 4},
		QuotaUnits:  50,
	}

	cases := map[string]float64{
		"wall_clock_ms": 500,
		"p10_ms":        1,
		"p50_ms":        2,
		"p90_ms":        3,
		"p99_ms":        4,
		"ops_per_sec":   20,
		"errors":        2,
		"bytes_up":      100,
		"bytes_down":    200,
		"quota_units":   50,
	}

	for metric, want := range cases {
		got, ok := metricValue(&m, metric)
		require.True(t, ok, metric)
		require.InDelta(t, want, got, 1e-9, metric)
	}

	_, ok := metricValue(&m, "nosuchmetric")
	require.False(t, ok)

	// Throughput is undefined for a workload that took no measurable time.
	_, ok = metricValue(&WorkloadMetrics{Ops: 1}, "ops_per_sec")
	require.False(t, ok)

	require.True(t, higherIsBetter("ops_per_sec"))
	require.False(t, higherIsBetter("wall_clock_ms"))
}

func TestBaselineSetAndCheck(t *testing.T) {
	dir := t.TempDir()
	results := filepath.Join(dir, "results")
	require.NoError(t, os.MkdirAll(results, dirPermissions))

	basePath := filepath.Join(dir, "base.json")
	require.NoError(t, writeResult(basePath, synthResult("base", map[string][]float64{"native": {100, 101, 102}})))

	var out, errOut bytes.Buffer

	require.Equal(t, exitOK, dispatch(context.Background(), []string{"baseline", "set", "--results-dir", results, basePath}, &out, &errOut), errOut.String())
	require.FileExists(t, filepath.Join(results, baselineFileName))

	// A candidate that matches the baseline passes.
	samePath := filepath.Join(dir, "same.json")
	require.NoError(t, writeResult(samePath, synthResult("same", map[string][]float64{"native": {100, 101, 102}})))

	out.Reset()
	errOut.Reset()

	require.Equal(t, exitOK, dispatch(context.Background(), []string{"baseline", "check", "--results-dir", results, samePath}, &out, &errOut), errOut.String())
	require.Contains(t, out.String(), "OK: no REAL regression")

	// A candidate that is twice as slow fails.
	slowPath := filepath.Join(dir, "slow.json")
	require.NoError(t, writeResult(slowPath, synthResult("slow", map[string][]float64{"native": {200, 201, 202}})))

	out.Reset()
	errOut.Reset()

	require.Equal(t, exitFailure, dispatch(context.Background(), []string{"baseline", "check", "--results-dir", results, slowPath}, &out, &errOut))
	require.Contains(t, out.String(), "REGRESSION")

	// The same candidate passes with a threshold above the change.
	out.Reset()

	require.Equal(t, exitOK, dispatch(context.Background(),
		[]string{"baseline", "check", "--results-dir", results, "--threshold-pct", "200", slowPath}, &out, &errOut))

	// A candidate that is twice as FAST is not a regression.
	fastPath := filepath.Join(dir, "fast.json")
	require.NoError(t, writeResult(fastPath, synthResult("fast", map[string][]float64{"native": {50, 51, 52}})))

	out.Reset()

	require.Equal(t, exitOK, dispatch(context.Background(), []string{"baseline", "check", "--results-dir", results, fastPath}, &out, &errOut))
}

func TestBaselineCheckWithoutBaselineIsRefused(t *testing.T) {
	dir := t.TempDir()
	results := filepath.Join(dir, "results")
	candidate := filepath.Join(dir, "c.json")

	require.NoError(t, writeResult(candidate, synthResult("c", map[string][]float64{"native": {1}})))

	var out, errOut bytes.Buffer

	require.Equal(t, exitRefused, dispatch(context.Background(), []string{"baseline", "check", "--results-dir", results, candidate}, &out, &errOut))
	require.Contains(t, errOut.String(), "baseline set")
}

func TestBaselineUsage(t *testing.T) {
	var out, errOut bytes.Buffer

	require.Equal(t, exitRefused, dispatch(context.Background(), []string{"baseline"}, &out, &errOut))
	require.Equal(t, exitRefused, dispatch(context.Background(), []string{"baseline", "frobnicate"}, &out, &errOut))
	require.Contains(t, errOut.String(), "unknown baseline subcommand")
}

func TestPublishCopiesIntoTheCuratedDirectory(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "curated")

	src := filepath.Join(dir, "result.json")
	require.NoError(t, writeResult(src, synthResult("p", map[string][]float64{"native": {1}})))

	var out, errOut bytes.Buffer

	require.Equal(t, exitOK, dispatch(context.Background(), []string{"publish", "--dest", dest, src}, &out, &errOut), errOut.String())

	published := filepath.Join(dest, "result.json")
	require.FileExists(t, published)

	st, err := os.Stat(published)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(publishFilePermissions), st.Mode().Perm())

	// Publishing again is refused without --force ...
	errOut.Reset()
	require.Equal(t, exitFailure, dispatch(context.Background(), []string{"publish", "--dest", dest, src}, &out, &errOut))
	require.Contains(t, errOut.String(), "--force")

	// ... and allowed with it, under a chosen name.
	require.Equal(t, exitOK, dispatch(context.Background(),
		[]string{"publish", "--dest", dest, "--force", "--as", "recommended-baseline.json", src}, &out, &errOut))
	require.FileExists(t, filepath.Join(dest, "recommended-baseline.json"))
}

func TestPublishRejectsUnwantedArtifacts(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "curated")

	binary := filepath.Join(dir, "a.bin")
	require.NoError(t, writeFileAtomic(binary, []byte("x")))

	_, err := publishOne(binary, dest, "", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "curated artifacts")

	_, err = publishOne(binary, dest, "../escape.json", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be a file name")

	_, err = publishOne(filepath.Join(dir, "missing.json"), dest, "", false)
	require.Error(t, err)
}

func TestKnobTableCoversEveryField(t *testing.T) {
	knobs := collectKnobs()

	require.Empty(t, undocumentedKnobs(knobs), "every backend knob needs an entry in knobTable")
	require.Empty(t, staleKnobTableEntries(knobs), "knobTable has entries that no longer exist in the backend")

	// The reflection actually saw both structs.
	require.Equal(t, reflect.TypeFor[gdrive.TuningOptions]().NumField(), countGroup(knobs, groupTuning))
	require.Equal(t, reflect.TypeFor[gdrivepacer.Config]().NumField(), countGroup(knobs, groupPacer))
	require.Positive(t, countGroup(knobs, groupHarness))

	for i := range knobs {
		k := &knobs[i]

		require.NotEmpty(t, k.Name, k.Field)
		require.NotEmpty(t, k.Default, k.Name)
		require.NotEmpty(t, k.RangeHint, k.Name)
		require.NotEmpty(t, k.Doc, k.Name)

		for _, w := range k.Affects {
			_, ok := workloadByName(w)
			require.True(t, ok, "knob %v names unknown workload %v", k.Name, w)
		}
	}
}

func countGroup(knobs []KnobInfo, group string) int {
	n := 0

	for i := range knobs {
		if knobs[i].Group == group {
			n++
		}
	}

	return n
}

func TestKnobsCommandEmitsJSON(t *testing.T) {
	var out, errOut bytes.Buffer

	require.Equal(t, exitOK, dispatch(context.Background(), []string{"knobs"}, &out, &errOut))
	require.Empty(t, errOut.String())

	var knobs []KnobInfo

	require.NoError(t, json.Unmarshal(out.Bytes(), &knobs))
	require.NotEmpty(t, knobs)

	out.Reset()
	require.Equal(t, exitOK, dispatch(context.Background(), []string{"knobs", "--format", "table", "--group", groupTuning}, &out, &errOut))
	require.Contains(t, out.String(), "pacerMinSleepMS")
	require.NotContains(t, out.String(), groupHarness+".trials")
}

func TestKnobNamesMatchTheConnectionConfigKeys(t *testing.T) {
	// A knob name must be directly usable in a targets file, so the tuning
	// knobs' field names have to be the JSON keys of gdrive.TuningOptions.
	targets, err := parseTargets([]byte(
		`[{"name":"n","connection":{"type":"gdrive","config":{"folderID":"f","tuning":{"pacerMinSleepMS":50,"useBatchDelete":true}}}}]`,
	), lookupMap(nil))
	require.NoError(t, err)

	for key := range targets[0].Knobs {
		_, ok := knobTable[groupTuning+"."+key]
		require.True(t, ok, "tuning key %v has no knob table entry", key)
	}
}
