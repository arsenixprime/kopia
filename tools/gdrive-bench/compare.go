//go:build !no_extra_providers

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pkg/errors"
)

// Significance verdicts. The rule is crude on purpose: an automated tuning
// agent must only ever act on REAL, and a crude rule that a human can verify by
// eye is safer than a statistic the agent can talk itself into.
//
//	REAL  - the [p10, p90] intervals of the two series do not overlap.
//	NOISE - the intervals overlap and the medians differ by less than 10%.
//	WEAK  - the intervals overlap and the medians differ by 10% or more.
//	NODATA - one of the series has no samples.
const (
	sigReal   = "REAL"
	sigWeak   = "WEAK"
	sigNoise  = "NOISE"
	sigNoData = "NODATA"

	// noiseThresholdPct is the median difference below which an overlapping
	// pair is called noise.
	noiseThresholdPct = 10.0
)

// compareSchemaVersion versions the comparison report document.
const compareSchemaVersion = 1

// CompareReport is the output of "compare" and of "baseline check".
type CompareReport struct {
	SchemaVersion     int             `json:"schema_version"`
	GeneratedAt       time.Time       `json:"generated_at"`
	NoiseThresholdPct float64         `json:"noise_threshold_pct"`
	Reference         string          `json:"reference"`
	Sources           []CompareSource `json:"sources"`
	Rows              []CompareRow    `json:"rows"`
}

// CompareSource identifies one compared series.
type CompareSource struct {
	Key string `json:"key"`

	// FileIndex is the position of the source document on the command line.
	FileIndex int `json:"file_index"`

	File   string `json:"file"`
	Label  string `json:"label"`
	Target string `json:"target"`
	Commit string `json:"commit"`
	Trials int    `json:"trials"`
}

// CompareRow is one workload+metric comparison of one series against the
// reference series.
type CompareRow struct {
	Workload       string      `json:"workload"`
	Metric         string      `json:"metric"`
	Reference      string      `json:"reference"`
	Target         string      `json:"target"`
	ReferenceStats StatSummary `json:"reference_stats"`
	TargetStats    StatSummary `json:"target_stats"`
	DeltaPct       float64     `json:"delta_pct"`
	DeltaUndefined bool        `json:"delta_undefined,omitempty"`
	Significance   string      `json:"significance"`
	Improvement    bool        `json:"improvement"`
}

// seriesData is every value one series contributed, indexed by workload key and
// metric name.
type seriesData struct {
	CompareSource

	values map[string]map[string][]float64
}

// add records one value.
func (s *seriesData) add(workload, metric string, v float64) {
	m, ok := s.values[workload]
	if !ok {
		m = map[string][]float64{}
		s.values[workload] = m
	}

	m[metric] = append(m[metric], v)
}

// summary summarizes one workload+metric series.
func (s *seriesData) summary(workload, metric string) StatSummary {
	return summarize(s.values[workload][metric])
}

// buildSeries turns result documents into comparable series, one per
// (file, target) pair.
//
// Series keys are target names when those are unique across all inputs, and
// "<target>@<file index>" when they are not - which is what makes comparing a
// result document against itself, or two runs of the same target, work.
func buildSeries(files []string, results []*RunResult) []*seriesData {
	counts := map[string]int{}

	for _, r := range results {
		for _, n := range r.targetNames() {
			counts[n]++
		}
	}

	out := []*seriesData{}

	for i, r := range results {
		for _, name := range r.targetNames() {
			key := name
			if counts[name] > 1 {
				key = name + "@" + strconv.Itoa(i)
			}

			s := &seriesData{
				CompareSource: CompareSource{
					Key:       key,
					FileIndex: i,
					File:      filepath.Base(files[i]), //nolint:gosec // i indexes results, which buildSeries is always called with alongside files.
					Label:     r.Label,
					Target:    name,
					Commit:    r.Harness.Commit,
				},
				values: map[string]map[string][]float64{},
			}

			for _, tr := range r.Trials {
				if tr.Target != name {
					continue
				}

				s.Trials++

				for wk := range tr.Workloads {
					m := tr.Workloads[wk]

					for _, metric := range metricNames {
						if v, ok := metricValue(m, metric); ok {
							s.add(wk, metric, v)
						}
					}
				}
			}

			out = append(out, s)
		}
	}

	return out
}

// selectReference moves the series named by ref to the front, so that
// compareSeries uses it as the reference. An empty ref keeps the order
// buildSeries produced, which is configuration order (see
// RunResult.targetNames).
//
// ref is matched against a series key first and against a target name second,
// because the two differ only when the same target appears in several input
// files - and in that case naming the bare target is ambiguous and is refused
// rather than guessed.
func selectReference(series []*seriesData, ref string) ([]*seriesData, error) {
	if ref == "" {
		return series, nil
	}

	idx := -1

	for i, s := range series {
		if s.Key == ref {
			idx = i
			break
		}
	}

	if idx < 0 {
		matches := []int{}

		for i, s := range series {
			if s.Target == ref {
				matches = append(matches, i)
			}
		}

		switch len(matches) {
		case 0:
			return nil, errors.Errorf("no series named %q; available: %v", ref, strings.Join(seriesKeys(series), ", "))
		case 1:
			idx = matches[0]
		default:
			return nil, errors.Errorf("%q names %v series; use one of the unambiguous keys: %v", ref, len(matches), strings.Join(seriesKeys(series), ", "))
		}
	}

	out := make([]*seriesData, 0, len(series))
	out = append(out, series[idx])
	out = append(out, series[:idx]...)
	out = append(out, series[idx+1:]...)

	return out, nil
}

// seriesKeys lists the series keys, for error messages.
func seriesKeys(series []*seriesData) []string {
	out := make([]string, 0, len(series))
	for _, s := range series {
		out = append(out, s.Key)
	}

	return out
}

// compareSeries builds the report: every series after the first is compared
// against the first, which is the reference.
func compareSeries(series []*seriesData) *CompareReport {
	rep := &CompareReport{
		SchemaVersion:     compareSchemaVersion,
		GeneratedAt:       nowUTC(),
		NoiseThresholdPct: noiseThresholdPct,
		Rows:              []CompareRow{},
	}

	for _, s := range series {
		rep.Sources = append(rep.Sources, s.CompareSource)
	}

	if len(series) < minComparableSeries {
		return rep
	}

	ref := series[0]
	rep.Reference = ref.Key

	for _, wk := range workloadKeysOf(series) {
		for _, metric := range metricNames {
			refStat := ref.summary(wk, metric)

			for _, s := range series[1:] {
				rep.Rows = append(rep.Rows, compareOne(wk, metric, ref.Key, s.Key, refStat, s.summary(wk, metric)))
			}
		}
	}

	return rep
}

// minComparableSeries is how many series a comparison needs.
const minComparableSeries = 2

// compareOne compares one metric of one workload between two series.
func compareOne(workload, metric, refKey, targetKey string, ref, cand StatSummary) CompareRow {
	row := CompareRow{
		Workload:       workload,
		Metric:         metric,
		Reference:      refKey,
		Target:         targetKey,
		ReferenceStats: ref,
		TargetStats:    cand,
	}

	row.Significance, row.DeltaPct, row.DeltaUndefined = classify(ref, cand)
	row.Improvement = isImprovement(metric, row.DeltaPct)

	return row
}

// classify applies the significance rule and computes the median delta in
// percent. The delta is reported as undefined (and zeroed, because JSON cannot
// carry an infinity) when the reference median is zero and the candidate's is
// not.
func classify(ref, cand StatSummary) (verdict string, deltaPct float64, undefined bool) {
	if ref.N == 0 || cand.N == 0 {
		return sigNoData, 0, true
	}

	switch {
	case ref.Median == 0 && cand.Median == 0:
		deltaPct = 0
	case ref.Median == 0:
		undefined = true
	default:
		deltaPct = (cand.Median - ref.Median) / math.Abs(ref.Median) * percentFull
	}

	if !intervalsOverlap(ref.P10, ref.P90, cand.P10, cand.P90) {
		return sigReal, deltaPct, undefined
	}

	if !undefined && math.Abs(deltaPct) < noiseThresholdPct {
		return sigNoise, deltaPct, undefined
	}

	return sigWeak, deltaPct, undefined
}

// intervalsOverlap reports whether two closed intervals share any point.
func intervalsOverlap(aLo, aHi, bLo, bHi float64) bool {
	return aLo <= bHi && bLo <= aHi
}

// isImprovement reports whether a delta moved the metric in the good direction.
func isImprovement(metric string, deltaPct float64) bool {
	if higherIsBetter(metric) {
		return deltaPct > 0
	}

	return deltaPct < 0
}

// workloadKeysOf returns every workload key any series carries, sorted.
func workloadKeysOf(series []*seriesData) []string {
	seen := map[string]bool{}
	out := []string{}

	for _, s := range series {
		for k := range s.values {
			if !seen[k] {
				seen[k] = true

				out = append(out, k)
			}
		}
	}

	sort.Strings(out)

	return out
}

// cmdCompare implements "gdrive-bench compare".
func cmdCompare(_ context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("compare", errOut)
	jsonOut := fs.String("json-out", "", "also write the comparison report as JSON to this path")
	quiet := fs.Bool("quiet", false, "suppress the human-readable table")
	realOnly := fs.Bool("real-only", false, "print only rows flagged REAL")
	reference := fs.String("reference", "", "series (or target) name every other series is compared against (default: the first target of the first result file, in targets-file order)")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	files := fs.Args()
	if len(files) == 0 {
		outln(errOut, "compare needs at least one result file (a single file works when it holds two or more targets)")

		return exitRefused
	}

	results := make([]*RunResult, 0, len(files))

	for _, f := range files {
		r, err := readResult(f)
		if err != nil {
			outf(errOut, "gdrive-bench compare: %v\n", err)

			return exitRefused
		}

		results = append(results, r)
	}

	series := buildSeries(files, results)
	if len(series) < minComparableSeries {
		outf(errOut, "compare needs at least %v series; the given file(s) hold %v\n", minComparableSeries, len(series))

		return exitRefused
	}

	series, err := selectReference(series, *reference)
	if err != nil {
		outf(errOut, "gdrive-bench compare: %v\n", err)

		return exitRefused
	}

	rep := compareSeries(series)

	if !*quiet {
		printCompareTable(out, rep, *realOnly)
	}

	if *jsonOut != "" {
		if err := writeJSON(*jsonOut, rep); err != nil {
			outf(errOut, "gdrive-bench compare: %v\n", err)

			return exitFailure
		}

		outf(out, "report: %v\n", *jsonOut)
	}

	return exitOK
}

// printCompareTable renders the comparison for humans.
func printCompareTable(out io.Writer, rep *CompareReport, realOnly bool) {
	outf(out, "reference: %v  (every DELTA reads TARGET relative to this series; --reference overrides)\n", rep.Reference)

	for _, s := range rep.Sources {
		outf(out, "  %-24v %v trials  %v  commit %v\n", s.Key, s.Trials, s.File, shortCommit(s.Commit))
	}

	outln(out)

	tw := tabwriter.NewWriter(out, 0, 0, tabwriterPadding, ' ', 0)

	outln(tw, "WORKLOAD\tMETRIC\tTARGET\tREF median [p10..p90]\tTARGET median [p10..p90]\tDELTA\tFLAG")

	for i := range rep.Rows {
		r := &rep.Rows[i]

		if realOnly && r.Significance != sigReal {
			continue
		}

		outf(tw, "%v\t%v\t%v\t%v\t%v\t%v\t%v\n",
			r.Workload, r.Metric, r.Target,
			formatStat(r.ReferenceStats), formatStat(r.TargetStats),
			formatDelta(r), r.Significance)
	}

	tw.Flush() //nolint:errcheck // best-effort table output.

	outln(out)
	outf(out, "REAL = [p10,p90] intervals disjoint; NOISE = overlapping and medians within %.0f%%; WEAK = overlapping but medians further apart.\n", rep.NoiseThresholdPct)
	outln(out, "Only REAL differences are safe to act on.")
}

// formatStat renders a summary compactly.
func formatStat(s StatSummary) string {
	if s.N == 0 {
		return "-"
	}

	return fmt.Sprintf("%.1f [%.1f..%.1f]", s.Median, s.P10, s.P90)
}

// formatDelta renders a signed percentage, marking improvements.
func formatDelta(r *CompareRow) string {
	if r.DeltaUndefined {
		return "n/a"
	}

	sign := "+"
	if r.DeltaPct < 0 {
		sign = ""
	}

	dir := "worse"
	if r.Improvement {
		dir = "better"
	}

	if r.DeltaPct == 0 {
		return "0.0%"
	}

	return fmt.Sprintf("%v%.1f%% %v", sign, r.DeltaPct, dir)
}

// shortCommit abbreviates a commit hash for display.
func shortCommit(c string) string {
	const short = 8

	if len(c) > short {
		return c[:short]
	}

	if c == "" {
		return "unknown"
	}

	return c
}

// writeJSON serializes any document the harness produces.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errors.Wrap(err, "unable to encode the document")
	}

	return writeFileAtomic(path, append(data, '\n'))
}
