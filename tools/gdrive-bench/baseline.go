//go:build !no_extra_providers

package main

import (
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/pkg/errors"
)

// baselineFileName is the promoted baseline inside the results directory.
const baselineFileName = "baseline.json"

// defaultRegressionThresholdPct is how much worse a key metric may get before a
// REAL difference counts as a regression.
const defaultRegressionThresholdPct = 15.0

// cmdBaseline implements "gdrive-bench baseline set|check".
func cmdBaseline(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		outln(errOut, "usage: gdrive-bench baseline set <result.json> | baseline check <result.json> [--threshold-pct=15]")

		return exitRefused
	}

	switch args[0] {
	case "set":
		return baselineSet(ctx, args[1:], out, errOut)
	case "check":
		return baselineCheck(ctx, args[1:], out, errOut)
	default:
		outf(errOut, "unknown baseline subcommand %q, want \"set\" or \"check\"\n", args[0])

		return exitRefused
	}
}

// baselineSet promotes a result document to the baseline.
func baselineSet(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("baseline set", errOut)
	resultsDir := fs.String("results-dir", "", "where the baseline is stored (default: $"+resultsDirEnvVar+")")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	if fs.NArg() != 1 {
		outln(errOut, "usage: gdrive-bench baseline set <result.json>")

		return exitRefused
	}

	src := fs.Arg(0)

	// Read it through readResult first: a baseline that cannot be parsed would
	// fail every later check for the wrong reason.
	if _, err := readResult(src); err != nil {
		outf(errOut, "gdrive-bench baseline set: %v\n", err)

		return exitRefused
	}

	dir, err := resolveResultsDir(ctx, *resultsDir)
	if err != nil {
		outf(errOut, "gdrive-bench baseline set: %v\n", err)

		return exitRefused
	}

	dst := filepath.Join(dir, baselineFileName)

	if err := copyFile(src, dst); err != nil {
		outf(errOut, "gdrive-bench baseline set: %v\n", err)

		return exitFailure
	}

	outf(out, "baseline: %v\n", dst)

	return exitOK
}

// baselineCheck compares a candidate result against the promoted baseline and
// fails on any REAL regression of a key metric beyond the threshold. This is
// the cron/CI hook.
func baselineCheck(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("baseline check", errOut)
	resultsDir := fs.String("results-dir", "", "where the baseline is stored (default: $"+resultsDirEnvVar+")")
	threshold := fs.Float64("threshold-pct", defaultRegressionThresholdPct, "how much worse a key metric may get before a REAL difference is a regression")
	jsonOut := fs.String("json-out", "", "also write the comparison report as JSON to this path")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	if fs.NArg() != 1 {
		outln(errOut, "usage: gdrive-bench baseline check <result.json> [--threshold-pct=15]")

		return exitRefused
	}

	dir, err := resolveResultsDir(ctx, *resultsDir)
	if err != nil {
		outf(errOut, "gdrive-bench baseline check: %v\n", err)

		return exitRefused
	}

	basePath := filepath.Join(dir, baselineFileName)

	base, err := readResult(basePath)
	if err != nil {
		outf(errOut, "gdrive-bench baseline check: %v (promote one with \"baseline set\")\n", err)

		return exitRefused
	}

	cand, err := readResult(fs.Arg(0))
	if err != nil {
		outf(errOut, "gdrive-bench baseline check: %v\n", err)

		return exitRefused
	}

	rep, regressions := checkAgainstBaseline(basePath, fs.Arg(0), base, cand, *threshold)

	printBaselineReport(out, rep, regressions, *threshold)

	if *jsonOut != "" {
		if err := writeJSON(*jsonOut, rep); err != nil {
			outf(errOut, "gdrive-bench baseline check: %v\n", err)

			return exitFailure
		}
	}

	if len(regressions) > 0 {
		return exitFailure
	}

	return exitOK
}

// checkAgainstBaseline compares same-named targets of two result documents on
// the key metrics and returns the report plus the regressing rows.
func checkAgainstBaseline(basePath, candPath string, base, cand *RunResult, thresholdPct float64) (*CompareReport, []CompareRow) {
	series := buildSeries([]string{basePath, candPath}, []*RunResult{base, cand})

	baseByTarget := map[string]*seriesData{}
	candByTarget := map[string]*seriesData{}

	for _, s := range series {
		if s.FileIndex == 0 {
			baseByTarget[s.Target] = s
		} else {
			candByTarget[s.Target] = s
		}
	}

	rep := &CompareReport{
		SchemaVersion:     compareSchemaVersion,
		GeneratedAt:       nowUTC(),
		NoiseThresholdPct: noiseThresholdPct,
		Reference:         "baseline",
		Rows:              []CompareRow{},
	}

	for _, s := range series {
		rep.Sources = append(rep.Sources, s.CompareSource)
	}

	names := make([]string, 0, len(candByTarget))
	for n := range candByTarget {
		names = append(names, n)
	}

	sort.Strings(names)

	var regressions []CompareRow

	for _, n := range names {
		b, ok := baseByTarget[n]
		if !ok {
			continue
		}

		c := candByTarget[n]

		for _, wk := range workloadKeysOf([]*seriesData{b, c}) {
			for _, metric := range keyMetricNames {
				row := compareOne(wk, metric, "baseline:"+n, "candidate:"+n, b.summary(wk, metric), c.summary(wk, metric))
				rep.Rows = append(rep.Rows, row)

				if isRegression(&row, thresholdPct) {
					regressions = append(regressions, row)
				}
			}
		}
	}

	return rep, regressions
}

// isRegression reports whether a row is a real, material worsening.
func isRegression(r *CompareRow, thresholdPct float64) bool {
	if r.Significance != sigReal || r.DeltaUndefined || r.Improvement {
		return false
	}

	return math.Abs(r.DeltaPct) > thresholdPct
}

// printBaselineReport renders the check for humans.
func printBaselineReport(out io.Writer, rep *CompareReport, regressions []CompareRow, thresholdPct float64) {
	tw := tabwriter.NewWriter(out, 0, 0, tabwriterPadding, ' ', 0)

	outln(tw, "WORKLOAD\tMETRIC\tTARGET\tBASELINE median [p10..p90]\tCANDIDATE median [p10..p90]\tDELTA\tFLAG")

	for i := range rep.Rows {
		r := &rep.Rows[i]

		outf(tw, "%v\t%v\t%v\t%v\t%v\t%v\t%v\n",
			r.Workload, r.Metric, r.Target,
			formatStat(r.ReferenceStats), formatStat(r.TargetStats),
			formatDelta(r), r.Significance)
	}

	tw.Flush() //nolint:errcheck // best-effort table output.

	outln(out)

	if len(regressions) == 0 {
		outf(out, "OK: no REAL regression beyond %.0f%% on %v.\n", thresholdPct, keyMetricNames)

		return
	}

	outf(out, "REGRESSION: %v key metric(s) beyond %.0f%%:\n", len(regressions), thresholdPct)

	for i := range regressions {
		r := &regressions[i]

		outf(out, "  %v %v on %v: %.1f -> %.1f (%+.1f%%)\n", r.Workload, r.Metric, r.Target, r.ReferenceStats.Median, r.TargetStats.Median, r.DeltaPct)
	}
}

// copyFile copies src to dst with the harness's owner-only permissions.
func copyFile(src, dst string) error {
	//nolint:gosec // the path is named on the command line by the operator.
	data, err := os.ReadFile(src)
	if err != nil {
		return errors.Wrapf(err, "unable to read %v", src)
	}

	return writeFileAtomic(dst, data)
}
