//go:build !no_extra_providers

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrialOrderABBA(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test.

	require.Equal(t, []int{0, 1}, trialOrder(orderABBA, 2, 0, rnd))
	require.Equal(t, []int{1, 0}, trialOrder(orderABBA, 2, 1, rnd))
	require.Equal(t, []int{0, 1}, trialOrder(orderABBA, 2, 2, rnd))
	require.Equal(t, []int{1, 0}, trialOrder(orderABBA, 2, 3, rnd))

	// The execution sequence of a four-trial, two-target run: A,B / B,A / A,B / B,A.
	var seq []int

	for trial := range 4 {
		seq = append(seq, trialOrder(orderABBA, 2, trial, rnd)...)
	}

	require.Equal(t, []int{0, 1, 1, 0, 0, 1, 1, 0}, seq)

	// Three targets reverse as a whole.
	require.Equal(t, []int{0, 1, 2}, trialOrder(orderABBA, 3, 0, rnd))
	require.Equal(t, []int{2, 1, 0}, trialOrder(orderABBA, 3, 1, rnd))
}

func TestTrialOrderSequentialNeverVaries(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test.

	for trial := range 5 {
		require.Equal(t, []int{0, 1, 2}, trialOrder(orderSequential, 3, trial, rnd))
	}
}

func TestTrialOrderRandomIsAPermutationAndVaries(t *testing.T) {
	rnd := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic test.

	varied := false

	for trial := range 20 {
		got := trialOrder(orderRandom, 3, trial, rnd)

		sorted := slices.Clone(got)
		slices.Sort(sorted)
		require.Equal(t, []int{0, 1, 2}, sorted)

		if !slices.Equal(got, []int{0, 1, 2}) {
			varied = true
		}
	}

	require.True(t, varied, "random ordering never differed from configuration order")
}

func TestSelectWorkloads(t *testing.T) {
	all, err := selectWorkloads("")
	require.NoError(t, err)
	require.Len(t, all, len(defaultWorkloads()))
	require.NotContains(t, defaultWorkloadNames(), snapshotE2EName, "the opt-in workload must stay out of the default suite")

	optIn, err := selectWorkloads(snapshotE2EName)
	require.NoError(t, err)
	require.Len(t, optIn, 1)

	some, err := selectWorkloads("small-write, delete-sweep")
	require.NoError(t, err)
	require.Len(t, some, 2)
	require.Equal(t, "small-write", some[0].Name)

	_, err = selectWorkloads("nosuchworkload")
	require.Error(t, err)

	_, err = selectWorkloads(",,")
	require.Error(t, err)
}

// gdriveTarget builds a native Drive target with the given tuning object. It
// never connects; only the decoded config is exercised.
func gdriveTarget(t *testing.T, tuning string) target {
	t.Helper()

	got, err := parseTargets([]byte(`[{"name":"native","connection":{"type":"gdrive","config":{
	   "folderID":"folder-1","tuning":`+tuning+`}}}]`), lookupMap(nil))
	require.NoError(t, err)

	return got[0]
}

func TestEffectiveDeleteParallelismPrefersTheTargetKnob(t *testing.T) {
	const harnessParallelism = 8

	// The target sets the backend knob: it wins, so a sweep of
	// deleteParallelism actually changes how many deletes are in flight.
	n, isSet := gdriveTarget(t, `{"deleteParallelism":24}`).deleteParallelism()
	require.True(t, isSet)
	require.Equal(t, 24, n)
	require.Equal(t, 24, effectiveDeleteParallelism(gdriveTarget(t, `{"deleteParallelism":24}`), harnessParallelism))

	// Unset, zero, negative and misspelled all read as "not configured" and
	// fall back to --parallelism.
	for _, tuning := range []string{`{}`, `{"deleteParallelism":0}`, `{"deleteParallelism":-4}`, `{"delete_parallelism":24}`} {
		tgt := gdriveTarget(t, tuning)

		_, isSet := tgt.deleteParallelism()
		require.False(t, isSet, "tuning %v", tuning)
		require.Equal(t, harnessParallelism, effectiveDeleteParallelism(tgt, harnessParallelism), "tuning %v", tuning)
	}

	// A non-Drive target has no such knob at all.
	fsTargets, err := parseTargets([]byte(`[{"name":"fs","connection":{"type":"filesystem","config":{"path":"/tmp/x"}}}]`), lookupMap(nil))
	require.NoError(t, err)
	require.Equal(t, harnessParallelism, effectiveDeleteParallelism(fsTargets[0], harnessParallelism))
}

func TestDeleteSweepNFlagPlumbing(t *testing.T) {
	// Default and smoke default are unchanged by the new flag.
	require.Equal(t, defaultDeleteSweepN, defaultParams().DeleteSweepN)
	require.Equal(t, smokeDeleteSweepN, smokeParams().DeleteSweepN)

	cfg, err := resolveRunConfig(&runOptions{trials: 1, order: orderABBA, uploadFraction: 0.5})
	require.NoError(t, err)
	require.Equal(t, defaultDeleteSweepN, cfg.Params.DeleteSweepN)

	cfg, err = resolveRunConfig(&runOptions{trials: 1, order: orderABBA, uploadFraction: 0.5, deleteSweepN: 500})
	require.NoError(t, err)
	require.Equal(t, 500, cfg.Params.DeleteSweepN)

	// It survives smoke mode, so a pacer cycle can ask for a phase long enough
	// to reach the token bucket without giving up the small smoke sizes.
	cfg, err = resolveRunConfig(&runOptions{trials: 1, order: orderABBA, uploadFraction: 0.5, smoke: true, deleteSweepN: 40, explicitlySet: map[string]bool{}})
	require.NoError(t, err)
	require.Equal(t, 40, cfg.Params.DeleteSweepN)

	// And the estimator scales with it, so the quota guard sees the bigger run.
	small := estimateDeleteSweep(&benchParams{DeleteSweepN: 100, SmallBlobMinBytes: 2048})
	big := estimateDeleteSweep(&benchParams{DeleteSweepN: 1000, SmallBlobMinBytes: 2048})
	require.Greater(t, big.QuotaUnits, small.QuotaUnits)
}

func TestDeleteSweepRecordsItsEffectiveParallelism(t *testing.T) {
	results := t.TempDir()

	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{
		"run",
		"--results-dir", results,
		"--smoke",
		"--workloads", "delete-sweep",
		"--delete-sweep-n", "6",
		"--parallelism", "3",
		"--label", "dsweep",
	}, &out, &errOut)
	require.Equal(t, exitOK, code, "stdout:\n%v\nstderr:\n%v", out.String(), errOut.String())

	res, err := readResult(findResultFile(t, results, "dsweep"))
	require.NoError(t, err)
	require.Equal(t, 6, res.Config.Params.DeleteSweepN)

	m := res.Trials[0].Workloads["delete-sweep"]

	// 6 blobs written, delete_percent (50) of them deleted.
	require.Equal(t, 3, m.Ops)

	// The filesystem smoke target sets no deleteParallelism, so --parallelism
	// is what was in force - and the result says so rather than leaving it to
	// be inferred.
	require.Equal(t, map[string]int64{deleteSweepParallelismParam: 3}, m.Params)
}

func TestWorkloadOfKey(t *testing.T) {
	require.Equal(t, "read-metadata", workloadOfKey("read-metadata:cold"))
	require.Equal(t, "small-write", workloadOfKey("small-write"))
}

func TestSanitizeLabel(t *testing.T) {
	require.Equal(t, "a-b_c.d", sanitizeLabel("a-b_c.d"))
	require.Equal(t, "a_b", sanitizeLabel("a/b"))
	require.Equal(t, "run", sanitizeLabel(""))
}

func TestResolveResultsDirRefusesTheRepositoryTree(t *testing.T) {
	root, ok := repoRoot(context.Background())
	if !ok {
		t.Skip("not running inside a git work tree")
	}

	_, err := resolveResultsDir(context.Background(), filepath.Join(root, "tools", "gdrive-bench", "results"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "inside the repository")
}

func TestResolveResultsDirFallsBackWithoutEnvironment(t *testing.T) {
	t.Setenv(resultsDirEnvVar, "")

	dir, err := resolveResultsDir(context.Background(), "")
	require.NoError(t, err)
	require.Contains(t, dir, defaultResultsDirNameSuffix)
	require.DirExists(t, dir)
}

// writeTwoFilesystemTargets creates a targets file with two temp-dir filesystem
// targets, one of which is reached through a ${VAR}.
func writeTwoFilesystemTargets(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")

	t.Setenv("GDRIVE_BENCH_TEST_PATH_B", b)

	path := filepath.Join(dir, "targets.json")
	doc := `[
	  {"name":"fs-a","connection":{"type":"filesystem","config":{"path":"` + a + `"}}},
	  {"name":"fs-b","connection":{"type":"filesystem","config":{"path":"${GDRIVE_BENCH_TEST_PATH_B}"}}}
	]`

	require.NoError(t, writeFileAtomic(path, []byte(doc)))

	return path
}

// TestSmokeRunAgainstTwoFilesystemTargets is the CI-safe self-test: it runs the
// whole harness end to end, with no network and no credentials, and asserts
// that a well-formed comparison lands.
func TestSmokeRunAgainstTwoFilesystemTargets(t *testing.T) {
	targetsPath := writeTwoFilesystemTargets(t)
	results := t.TempDir()

	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{
		"run",
		"--targets", targetsPath,
		"--results-dir", results,
		"--smoke",
		"--trials", "3",
		"--pack-size-mb", "1",
		"--cooldown", "0",
		"--seed", "424242",
		"--label", "selftest",
	}, &out, &errOut)

	require.Equal(t, exitOK, code, "stdout:\n%v\nstderr:\n%v", out.String(), errOut.String())

	resultFile := findResultFile(t, results, "selftest")

	res, err := readResult(resultFile)
	require.NoError(t, err)

	require.Equal(t, schemaVersion, res.SchemaVersion)
	require.Equal(t, "selftest", res.Label)
	require.Len(t, res.Targets, 2)
	require.Len(t, res.Trials, 3*2, "three trials times two targets")
	require.Empty(t, res.Errors)
	require.NotEmpty(t, res.Harness.Commit)
	require.Equal(t, int64(424242), res.Config.Seed)

	// ABBA interleaving, trial by trial: (a, b) then (b, a) then (a, b).
	var order []string
	for _, tr := range res.Trials {
		order = append(order, tr.Target)
	}

	require.Equal(t, []string{"fs-a", "fs-b", "fs-b", "fs-a", "fs-a", "fs-b"}, order)

	// Every workload reported, no errors, and no Drive stats for a filesystem
	// backend.
	for _, tr := range res.Trials {
		require.Len(t, tr.Workloads, len(defaultWorkloads())+expectedExtraPhases)

		for key, m := range tr.Workloads {
			require.Zero(t, m.Errors, key)
			require.Positive(t, m.Ops, key)
			require.Equal(t, m.Ops, m.Samples, key)
			require.Nil(t, m.Transport, key)
			require.Equal(t, quotaSourceNone, m.QuotaUnitsSource, key)
		}
	}

	require.Positive(t, res.Trials[0].Workloads["small-write"].BytesUp)
	require.Positive(t, res.Trials[0].Workloads["range-read"].BytesDown)

	// A filesystem target spends no Drive quota, so nothing is charged.
	require.Zero(t, res.Quota.ActualUploadBytes)
	require.NoFileExists(t, filepath.Join(results, ledgerFileName))

	// Cleanup: every workload removed what it wrote.
	for _, td := range res.Targets {
		path, ok := td.Config["path"].(string)
		require.True(t, ok)
		requireNoBenchBlobs(t, path)
	}

	// ... and the comparison of the two targets is well formed.
	var cmpOut, cmpErr bytes.Buffer

	reportPath := filepath.Join(results, "report.json")

	code = dispatch(context.Background(), []string{"compare", "--json-out", reportPath, resultFile}, &cmpOut, &cmpErr)
	require.Equal(t, exitOK, code, cmpErr.String())

	data, err := os.ReadFile(reportPath)
	require.NoError(t, err)

	rep := &CompareReport{}
	require.NoError(t, json.Unmarshal(data, rep))

	require.Len(t, rep.Sources, 2)
	require.Equal(t, 3, rep.Sources[0].Trials)
	require.NotEmpty(t, rep.Rows)

	for _, row := range rep.Rows {
		require.Contains(t, []string{sigReal, sigWeak, sigNoise, sigNoData}, row.Significance)
		require.False(t, isNaNOrInf(row.DeltaPct), "%v/%v produced a non-finite delta", row.Workload, row.Metric)
	}

	require.Contains(t, cmpOut.String(), "small-write")
	require.Contains(t, cmpOut.String(), "Only REAL differences are safe to act on")
}

// expectedExtraPhases is the number of extra result keys the multi-phase
// workloads add: read-metadata (hot, cold) and list-heavy (full, prefixed).
const expectedExtraPhases = 2

func isNaNOrInf(v float64) bool {
	return v != v || v > 1e300 || v < -1e300 //nolint:gocritic // NaN check by self-comparison is intentional.
}

func findResultFile(t *testing.T, dir, label string) string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-"+label+".json") {
			return filepath.Join(dir, e.Name())
		}
	}

	t.Fatalf("no result file for label %v in %v", label, dir)

	return ""
}

func requireNoBenchBlobs(t *testing.T, root string) {
	t.Helper()

	var found []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.Contains(info.Name(), "bench-") {
			found = append(found, path)
		}

		return nil
	})
	require.NoError(t, err)
	require.Empty(t, found, "workload cleanup left blobs behind")
}

func TestRunWithoutTargetsRequiresSmoke(t *testing.T) {
	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{"run", "--results-dir", t.TempDir()}, &out, &errOut)
	require.Equal(t, exitRefused, code)
	require.Contains(t, errOut.String(), "--targets is required")
}

func TestRunRejectsBadOrderAndFraction(t *testing.T) {
	var out, errOut bytes.Buffer

	require.Equal(t, exitRefused, dispatch(context.Background(),
		[]string{"run", "--smoke", "--order", "sideways", "--results-dir", t.TempDir()}, &out, &errOut))
	require.Contains(t, errOut.String(), "unknown --order")

	errOut.Reset()

	require.Equal(t, exitRefused, dispatch(context.Background(),
		[]string{"run", "--smoke", "--daily-upload-fraction", "3", "--results-dir", t.TempDir()}, &out, &errOut))
	require.Contains(t, errOut.String(), "daily-upload-fraction")
}

func TestZeroEnvironmentSmokeRun(t *testing.T) {
	// No targets file, no credentials, no results directory: the harness must
	// still produce a result document. This is the CI-safe path.
	t.Setenv(resultsDirEnvVar, t.TempDir())

	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{
		"run", "--smoke", "--pack-size-mb", "1", "--workloads", "small-write,list-heavy", "--label", "zeroenv",
	}, &out, &errOut)

	require.Equal(t, exitOK, code, "stdout:\n%v\nstderr:\n%v", out.String(), errOut.String())

	res, err := readResult(findResultFile(t, os.Getenv(resultsDirEnvVar), "zeroenv"))
	require.NoError(t, err)
	require.Len(t, res.Targets, 1)
	require.Equal(t, "filesystem", res.Targets[0].StorageType)
	require.Equal(t, []string{"small-write", "list-heavy"}, res.Config.Workloads)
}

func TestDispatchUsage(t *testing.T) {
	var out, errOut bytes.Buffer

	require.Equal(t, exitRefused, dispatch(context.Background(), nil, &out, &errOut))
	require.Contains(t, errOut.String(), "usage")

	out.Reset()
	require.Equal(t, exitOK, dispatch(context.Background(), []string{"--help"}, &out, &errOut))
	require.Contains(t, out.String(), "compare")

	errOut.Reset()
	require.Equal(t, exitRefused, dispatch(context.Background(), []string{"nosuchcommand"}, &out, &errOut))
	require.Contains(t, errOut.String(), "unknown subcommand")
}

func TestFormatBytesIsDecimal(t *testing.T) {
	require.Equal(t, "999 B", formatBytes(999))
	require.Equal(t, "1.0 kB", formatBytes(1000))
	require.Equal(t, "750.0 GB", formatBytes(dailyUploadBytesLimit))
}
