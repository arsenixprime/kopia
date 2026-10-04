//go:build !no_extra_providers

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// cmdGraph implements "gdrive-bench graph".
//
// It turns a result document or a comparison report into a handful of
// self-contained SVGs meant to be attached to a pull-request body. The charts
// deliberately carry everything needed to read them without the report next to
// them: units, the number of trials behind every median, the significance
// verdict of every row, and a caption naming the run.
func cmdGraph(_ context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("graph", errOut)
	outDir := fs.String("out-dir", "", "directory the SVG files are written to (created if missing)")
	prefix := fs.String("prefix", "", "file name prefix for the generated charts (default: none)")

	if code, ok := parseFlags(fs, flagsFirst(fs, args)); !ok {
		return code
	}

	if fs.NArg() != 1 || *outDir == "" {
		outln(errOut, "usage: gdrive-bench graph <result.json|compare.json> --out-dir <dir> [--prefix=name-]")

		return exitRefused
	}

	d, err := loadGraphData(fs.Arg(0))
	if err != nil {
		outf(errOut, "gdrive-bench graph: %v\n", err)

		return exitRefused
	}

	written, err := writeCharts(d, *outDir, *prefix)
	if err != nil {
		outf(errOut, "gdrive-bench graph: %v\n", err)

		return exitFailure
	}

	for _, p := range written {
		outf(out, "chart: %v\n", p)
	}

	return exitOK
}

// writeCharts renders every chart the data supports.
func writeCharts(d *graphData, outDir, prefix string) ([]string, error) {
	if err := os.MkdirAll(outDir, publishDirPermissions); err != nil {
		return nil, errors.Wrapf(err, "unable to create %v", outDir)
	}

	type chartSpec struct {
		name  string
		chart *barChart
	}

	specs := []chartSpec{
		{"wall-clock-linear.svg", chartWallClock(d, false)},
		{"wall-clock-split.svg", chartWallClock(d, true)},
		{"snapshot-stages.svg", chartStages(d)},
		{"cpu-total.svg", chartCPU(d)},
		{"quota-units.svg", chartQuota(d)},
	}

	written := []string{}

	for _, s := range specs {
		if s.chart == nil {
			continue
		}

		path := filepath.Join(outDir, prefix+s.name)

		if err := os.WriteFile(path, []byte(renderBarChart(s.chart)), publishFilePermissions); err != nil {
			return written, errors.Wrapf(err, "unable to write %v", path)
		}

		written = append(written, path)
	}

	if len(written) == 0 {
		return nil, errors.New("the document holds nothing chartable: no workload has two comparable series")
	}

	return written, nil
}

// ---------------------------------------------------------------------------
// the data a chart is drawn from
// ---------------------------------------------------------------------------

// graphCell is one series' summary of one workload+metric, with the
// significance verdict of that series against the reference.
type graphCell struct {
	stat StatSummary
	sig  string
}

// graphData is a result document or a comparison report, reduced to what the
// charts need.
type graphData struct {
	sourceFile string
	label      string
	when       time.Time
	commit     string

	// series is in reference-first order: series[0] is the lane every delta and
	// every significance verdict is relative to.
	series []string
	trials map[string]int

	cells map[string]map[string]map[string]graphCell
}

func (d *graphData) put(workload, metric, series string, c graphCell) {
	byMetric, ok := d.cells[workload]
	if !ok {
		byMetric = map[string]map[string]graphCell{}
		d.cells[workload] = byMetric
	}

	bySeries, ok := byMetric[metric]
	if !ok {
		bySeries = map[string]graphCell{}
		byMetric[metric] = bySeries
	}

	bySeries[series] = c
}

func (d *graphData) get(workload, metric, series string) (graphCell, bool) {
	c, ok := d.cells[workload][metric][series]

	return c, ok && c.stat.N > 0
}

// keysWithMetric returns the workload keys that carry a metric, in the harness's
// own execution order rather than alphabetically - a chart whose rows read
// small-write, pack-write, read-metadata… is a chart a reader can follow.
func (d *graphData) keysWithMetric(metric string) []string {
	out := []string{}

	for wk, byMetric := range d.cells {
		if len(byMetric[metric]) > 0 {
			out = append(out, wk)
		}
	}

	sortWorkloadKeys(out)

	return out
}

// phaseOrder is the reading order of the phases of a multi-phase workload.
// Alphabetical would put "cold" before "hot" and "create" after "verify", which
// is the opposite of the order they happen in.
var phaseOrder = map[string][]string{
	workloadReadMetadata: {phaseHot, phaseWarm, phaseCold},
	workloadListHeavy:    {"full", "prefixed"},
	snapshotE2EName:      {stageCreate, stageInit, stageIncremental, stageVerify, stageMaintenance},
}

// sortWorkloadKeys orders result keys by the registry order of their workload
// and then by the workload's own phase order.
func sortWorkloadKeys(keys []string) {
	registry := workloadNames()

	rank := func(k string) (int, int) {
		w := workloadOfKey(k)

		wi := slices.Index(registry, w)
		if wi < 0 {
			wi = len(registry)
		}

		pi := 0

		if phases, ok := phaseOrder[w]; ok {
			phase := strings.TrimPrefix(k, w+":")

			pi = slices.Index(phases, phase)
			if pi < 0 {
				pi = len(phases)
			}
		}

		return wi, pi
	}

	sort.SliceStable(keys, func(i, j int) bool {
		wi, pi := rank(keys[i])
		wj, pj := rank(keys[j])

		if wi != wj {
			return wi < wj
		}

		if pi != pj {
			return pi < pj
		}

		return keys[i] < keys[j]
	})
}

// documentProbe is how loadGraphData tells the two accepted input documents
// apart without guessing from the file name.
type documentProbe struct {
	SchemaVersion int             `json:"schema_version"`
	Trials        json.RawMessage `json:"trials"`
	Rows          json.RawMessage `json:"rows"`
}

// loadGraphData accepts either a run result or a comparison report.
func loadGraphData(path string) (*graphData, error) {
	//nolint:gosec // the path is named on the command line by the operator.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read %v", path)
	}

	var probe documentProbe

	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, errors.Wrapf(err, "unable to decode %v", path)
	}

	switch {
	case len(probe.Trials) > 0:
		r, err := readResult(path)
		if err != nil {
			return nil, err
		}

		return graphDataFromResult(path, r), nil

	case len(probe.Rows) > 0:
		rep := &CompareReport{}
		if err := json.Unmarshal(data, rep); err != nil {
			return nil, errors.Wrapf(err, "unable to decode the comparison report %v", path)
		}

		return graphDataFromCompare(path, rep)

	default:
		return nil, errors.Errorf("%v is neither a run result (no \"trials\") nor a comparison report (no \"rows\")", path)
	}
}

// graphDataFromResult summarizes a run document exactly the way compare does,
// so a chart and a comparison table can never disagree.
func graphDataFromResult(path string, r *RunResult) *graphData {
	series := buildSeries([]string{path}, []*RunResult{r})

	d := &graphData{
		sourceFile: filepath.Base(path),
		label:      r.Label,
		when:       r.StartedAt,
		commit:     r.Harness.Commit,
		trials:     map[string]int{},
		cells:      map[string]map[string]map[string]graphCell{},
	}

	for _, s := range series {
		d.series = append(d.series, s.Key)
		d.trials[s.Key] = s.Trials
	}

	if len(series) == 0 {
		return d
	}

	ref := series[0]

	for _, wk := range workloadKeysOf(series) {
		for _, metric := range chartMetrics {
			refStat := ref.summary(wk, metric)
			d.put(wk, metric, ref.Key, graphCell{stat: refStat})

			for _, s := range series[1:] {
				stat := s.summary(wk, metric)
				sig, _, _ := classify(refStat, stat)

				d.put(wk, metric, s.Key, graphCell{stat: stat, sig: sig})
			}
		}
	}

	return d
}

// graphDataFromCompare reads a comparison report, whose rows already carry both
// series' statistics and the verdict.
func graphDataFromCompare(path string, rep *CompareReport) (*graphData, error) {
	if rep.Reference == "" || len(rep.Rows) == 0 {
		return nil, errors.Errorf("%v has no comparison rows", path)
	}

	d := &graphData{
		sourceFile: filepath.Base(path),
		when:       rep.GeneratedAt,
		trials:     map[string]int{},
		cells:      map[string]map[string]map[string]graphCell{},
		series:     []string{rep.Reference},
	}

	for _, s := range rep.Sources {
		d.trials[s.Key] = s.Trials

		if s.Key == rep.Reference {
			d.label = s.Label
			d.commit = s.Commit
		}
	}

	for i := range rep.Rows {
		row := &rep.Rows[i]

		if !slices.Contains(d.series, row.Target) {
			d.series = append(d.series, row.Target)
		}

		d.put(row.Workload, row.Metric, row.Reference, graphCell{stat: row.ReferenceStats})
		d.put(row.Workload, row.Metric, row.Target, graphCell{stat: row.TargetStats, sig: row.Significance})
	}

	return d, nil
}

// chartMetrics are the metrics the charts read out of a run document.
var chartMetrics = []string{metricWallClockMS, metricCPUTotalMS, metricQuotaUnits}

// ---------------------------------------------------------------------------
// the charts
// ---------------------------------------------------------------------------

// isStageKey reports whether a result key belongs to the out-of-process
// snapshot workload.
func isStageKey(k string) bool {
	return workloadOfKey(k) == snapshotE2EName
}

// chartWallClock is the headline chart: median wall clock per workload, one bar
// per lane, with the [p10..p90] interval drawn over each bar.
//
// The split variant exists because the measured phases span more than three
// orders of magnitude - a whole-storage traversal takes a second, a verify takes
// four minutes - and on one linear axis the fast rows collapse into the axis and
// stop being readable. Splitting into magnitude bands keeps every panel linear,
// which keeps bar LENGTH meaning what it looks like it means; a log axis would
// not (a bar twice as long would not be twice the time). The panels are labeled
// with their own range and the chart says they do not share a scale.
func chartWallClock(d *graphData, split bool) *barChart {
	keys := d.keysWithMetric(metricWallClockMS)
	if len(keys) == 0 {
		return nil
	}

	rows := d.rowsFor(keys, metricWallClockMS, formatMS, nil)
	if len(rows) == 0 {
		return nil
	}

	c := &barChart{
		title:    "Measured wall clock per workload",
		subtitle: "median over " + d.trialSummary() + "; the line over each bar is the [p10..p90] interval. Shorter is better.",
		unit:     formatMS,
		series:   d.series,
		caption:  d.caption(),
		footnote: significanceFootnote,
	}

	if !split {
		c.subtitle += " One linear axis: the long rows compress the short ones - see the split-panel chart for those."
		c.panels = []barPanel{{rows: rows}}

		return c
	}

	c.title = "Measured wall clock per workload (split by magnitude)"
	c.subtitle += " Each panel is LINEAR and has its OWN axis - compare bar lengths within a panel, never across panels."
	c.panels = splitPanels(rows, formatMS)

	return c
}

// chartStages is the end-to-end backup cycle, stage by stage.
func chartStages(d *graphData) *barChart {
	keys := []string{}

	for _, k := range d.keysWithMetric(metricWallClockMS) {
		if isStageKey(k) {
			keys = append(keys, k)
		}
	}

	if len(keys) == 0 {
		return nil
	}

	rows := d.rowsFor(keys, metricWallClockMS, formatMS, nil)
	if len(rows) == 0 {
		return nil
	}

	// The title already says which workload this is; repeating it on every row
	// costs the label column for nothing.
	for i := range rows {
		rows[i].label = strings.TrimPrefix(rows[i].label, snapshotE2EName+":")
	}

	return &barChart{
		title:    "snapshot-e2e: the backup cycle, stage by stage",
		subtitle: "one real kopia invocation per stage, median over " + d.trialSummary() + ". Shorter is better.",
		unit:     formatMS,
		series:   d.series,
		caption:  d.caption(),
		footnote: significanceFootnote,
		panels:   []barPanel{{rows: rows}},
	}
}

// chartCPU is the CPU cost of each workload to the whole machine.
func chartCPU(d *graphData) *barChart {
	keys := d.keysWithMetric(metricCPUTotalMS)
	if len(keys) == 0 {
		return nil
	}

	// The tripwire: a lane that did seconds of network work while reporting
	// almost no CPU has leaked its accounting, and the flattering number must be
	// published as a hole rather than as a measurement.
	broken := func(wk, series string) bool {
		wall, wok := d.get(wk, metricWallClockMS, series)
		cpu, cok := d.get(wk, metricCPUTotalMS, series)

		return wok && cok && suspectCPUAccounting(wall.stat.Median, cpu.stat.Median)
	}

	rows := d.rowsFor(keys, metricCPUTotalMS, formatMS, broken)
	if len(rows) == 0 || allZero(rows) {
		// A document written before CPU accounting existed carries the field with
		// its zero value everywhere. Drawing a chart of zeros would claim a
		// measurement that was never made.
		return nil
	}

	return &barChart{
		title:    "CPU cost to the machine per workload",
		subtitle: "harness process + every descendant (cpu_total_ms), median over " + d.trialSummary() + ". Lower is better.",
		unit:     formatMS,
		series:   d.series,
		caption:  d.caption(),
		footnote: significanceFootnote + "  " + fairnessFootnote,
		panels:   splitPanels(rows, formatMS),
	}
}

// chartQuota is Drive quota units, and it is deliberately a ONE-series chart.
//
// The harness counts quota units from its own instrumented HTTP transport,
// which only the native backend has. An rclone lane's traffic leaves through a
// subprocess the harness cannot see, so its quota units are not zero - they are
// UNOBSERVED, and drawing a zero-height bar next to a real one would state
// something false. The other lane is named in the subtitle instead.
func chartQuota(d *graphData) *barChart {
	if len(d.series) == 0 {
		return nil
	}

	ref := d.series[0]
	keys := []string{}

	for _, k := range d.keysWithMetric(metricQuotaUnits) {
		// snapshot-e2e's quota is ESTIMATED from bytes moved (§5), not counted:
		// its rows would be identical on both lanes by construction and would
		// silently mix a measurement with an assumption. Only the in-process
		// workloads, whose quota comes off the instrumented transport, are drawn.
		if isStageKey(k) {
			continue
		}

		if c, ok := d.get(k, metricQuotaUnits, ref); ok && c.stat.Median > 0 {
			keys = append(keys, k)
		}
	}

	if len(keys) == 0 {
		return nil
	}

	single := &graphData{series: []string{ref}, trials: d.trials, cells: d.cells}
	rows := single.rowsFor(keys, metricQuotaUnits, formatCount, nil)

	unobserved := "the other lane"
	if len(d.series) > 1 {
		unobserved = strings.Join(d.series[1:], ", ")
	}

	return &barChart{
		title: "Drive quota units per workload - " + ref + " lane only",
		subtitle: "counted from the native backend's own instrumented transport, median over " + d.trialSummary() + ". " +
			unobserved + " spends quota too: it is UNOBSERVED here, not zero, because that traffic leaves through a " +
			"subprocess the harness cannot instrument. snapshot-e2e is omitted: its quota is estimated from bytes " +
			"moved rather than counted.",
		unit:    formatCount,
		series:  single.series,
		caption: d.caption(),
		panels:  splitPanels(rows, formatCount),
	}
}

// significanceFootnote explains the dimmed rows.
const significanceFootnote = "Rows whose two intervals overlap (not REAL) are dimmed and flagged: they are not safe to act on."

// fairnessFootnote is the one sentence without which the CPU chart can be
// misread in either direction.
const fairnessFootnote = "Fairness: the native lane runs the backend IN the harness, so its CPU is the harness's own; the rclone lane's is the harness plus its rclone subprocess. Both bars are the sum - that is the comparable number."

// trialSummary renders the per-lane trial counts.
func (d *graphData) trialSummary() string {
	counts := map[int]bool{}
	for _, s := range d.series {
		counts[d.trials[s]] = true
	}

	if len(counts) == 1 {
		for n := range counts {
			return fmt.Sprintf("n=%v trials per lane", n)
		}
	}

	parts := []string{}
	for _, s := range d.series {
		parts = append(parts, fmt.Sprintf("%v n=%v", s, d.trials[s]))
	}

	return strings.Join(parts, ", ")
}

// caption is the provenance line every chart carries.
func (d *graphData) caption() string {
	parts := []string{}

	if d.label != "" {
		parts = append(parts, "run "+d.label)
	}

	if !d.when.IsZero() {
		parts = append(parts, d.when.UTC().Format("2006-01-02"))
	}

	parts = append(parts, d.sourceFile)

	if d.commit != "" {
		parts = append(parts, "harness "+shortCommit(d.commit))
	}

	return "gdrive-bench · " + strings.Join(parts, " · ")
}

// rowsFor turns a set of workload keys into chart rows. broken, when given,
// decides whether a cell's measurement must be published as untrustworthy.
func (d *graphData) rowsFor(keys []string, metric string, unit func(float64) string, broken func(wk, series string) bool) []barRow {
	rows := []barRow{}

	for _, wk := range keys {
		row := barRow{label: wk, values: make([]barValue, len(d.series))}

		hasValue := false

		for i, s := range d.series {
			c, ok := d.get(wk, metric, s)
			if !ok {
				continue
			}

			hasValue = true
			row.values[i] = barValue{
				present: true,
				median:  c.stat.Median,
				p10:     c.stat.P10,
				p90:     c.stat.P90,
				text:    unit(c.stat.Median),
			}

			if broken != nil && broken(wk, s) {
				row.values[i].broken = true
				row.note = "BROKEN-ACCOUNTING"
			}

			if i > 0 && c.sig != "" && c.sig != sigReal {
				row.dim = true

				if row.note == "" {
					row.note = c.sig
				}
			}
		}

		if hasValue {
			rows = append(rows, row)
		}
	}

	return rows
}

// splitBands are the magnitude bands of the faceted charts, in milliseconds,
// largest first. They step by roughly half a decade, so no panel's own range
// exceeds about 3x and no bar inside one collapses into the axis.
var splitBands = []float64{100000, 30000, 10000, 3000, 1000, 300, 0}

// splitPanels facets a row set into magnitude bands so that one four-minute bar
// cannot flatten ten sub-second ones. Each panel gets its own linear axis, which
// is why every chart that uses this says so in its subtitle: bar lengths are
// comparable inside a panel and meaningless across panels.
func splitPanels(rows []barRow, unit func(float64) string) []barPanel {
	byBand := map[int][]barRow{}

	for _, r := range rows {
		v := r.maxMedian()

		for i, lo := range splitBands {
			if v >= lo {
				byBand[i] = append(byBand[i], r)

				break
			}
		}
	}

	out := []barPanel{}

	for i, lo := range splitBands {
		if len(byBand[i]) == 0 {
			continue
		}

		hi := math.Inf(1)
		if i > 0 {
			hi = splitBands[i-1]
		}

		out = append(out, barPanel{heading: bandHeading(lo, hi, unit), rows: byBand[i]})
	}

	if len(out) == 1 {
		// A single band means the data did not actually need splitting, and a
		// heading that describes the whole chart is noise.
		out[0].heading = ""
	}

	return out
}

// allZero reports whether every row of a set is zero, which means the metric
// was not measured rather than that it was measured as nothing.
func allZero(rows []barRow) bool {
	for _, r := range rows {
		if r.maxExtent() > 0 {
			return false
		}
	}

	return true
}

// bandHeading names one magnitude band.
func bandHeading(lo, hi float64, unit func(float64) string) string {
	if math.IsInf(hi, 1) {
		return unit(lo) + " and above"
	}

	if lo == 0 {
		return "under " + unit(hi)
	}

	return unit(lo) + " to " + unit(hi)
}
