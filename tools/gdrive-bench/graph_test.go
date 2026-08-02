//go:build !no_extra_providers

package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// parseSVG asserts that a document is well-formed XML and returns the name of
// every element it contains, with the text content concatenated.
//
// The charts are checked structurally rather than byte for byte: a golden file
// of 20 KB of path data would fail on every cosmetic change and would tell a
// reader nothing about what broke.
func parseSVG(t *testing.T, doc string) (elements []string, text string) {
	t.Helper()

	var sb strings.Builder

	dec := xml.NewDecoder(strings.NewReader(doc))

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}

		switch v := tok.(type) {
		case xml.StartElement:
			elements = append(elements, v.Name.Local)
		case xml.CharData:
			sb.Write(v)
			sb.WriteString("\n")
		}
	}

	// A truncated or unbalanced document ends with an error that is not io.EOF;
	// decoding to the end is the well-formedness check.
	require.NoError(t, xml.Unmarshal([]byte(strings.TrimPrefix(doc, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")), new(struct {
		XMLName xml.Name `xml:"svg"`
	})))

	return elements, sb.String()
}

func countElement(elements []string, name string) int {
	n := 0

	for _, e := range elements {
		if e == name {
			n++
		}
	}

	return n
}

// fakeGraphData builds a two-lane data set covering every feature the charts
// have to render: a REAL row, a NOISE row, a magnitude spread wide enough to
// force a split, and a lane whose CPU accounting leaked.
func fakeGraphData() *graphData {
	d := &graphData{
		sourceFile: "run.json",
		label:      "native-vs-rclone-cpu",
		when:       time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
		commit:     "abcdef1234567890",
		series:     []string{"native", "rclone"},
		trials:     map[string]int{"native": 5, "rclone": 5},
		cells:      map[string]map[string]map[string]graphCell{},
	}

	put := func(wk, metric string, refMed, candMed float64, sig string) {
		d.put(wk, metric, "native", graphCell{stat: StatSummary{N: 5, Median: refMed, P10: refMed * 0.95, P90: refMed * 1.05}})
		d.put(wk, metric, "rclone", graphCell{stat: StatSummary{N: 5, Median: candMed, P10: candMed * 0.95, P90: candMed * 1.05}, sig: sig})
	}

	put("small-write", "wall_clock_ms", 120900, 199000, sigReal)
	put("small-write", "cpu_total_ms", 4200, 9100, sigReal)
	put("small-write", "quota_units", 15000, 0, sigReal)

	put("delete-sweep", "wall_clock_ms", 2380, 2450, sigNoise)
	put("delete-sweep", "cpu_total_ms", 190, 3, sigReal)
	put("delete-sweep", "quota_units", 2500, 0, sigReal)

	put(snapshotE2EName+":verify", "wall_clock_ms", 77300, 251200, sigReal)
	put(snapshotE2EName+":create", "wall_clock_ms", 12900, 28000, sigReal)

	return d
}

func TestGraphChartsAreWellFormedSVG(t *testing.T) {
	d := fakeGraphData()

	for name, c := range map[string]*barChart{
		"wall-clock-linear": chartWallClock(d, false),
		"wall-clock-split":  chartWallClock(d, true),
		"snapshot-stages":   chartStages(d),
		"cpu-total":         chartCPU(d),
		"quota-units":       chartQuota(d),
	} {
		require.NotNil(t, c, name)

		doc := renderBarChart(c)
		elements, text := parseSVG(t, doc)

		require.Equal(t, "svg", elements[0], name)
		require.Contains(t, elements, "title", name)
		require.Contains(t, elements, "desc", name)
		require.Contains(t, text, c.title, name)

		// Every chart carries its provenance and the number of trials behind it.
		require.Contains(t, text, "native-vs-rclone-cpu", name)
		require.Contains(t, text, "2026-08-02", name)
		require.Contains(t, text, "n=5 trials per lane", name)

		// A baked light surface is what makes the chart readable in a dark GitHub
		// theme; without it the dark ink lands on a dark page.
		require.Contains(t, doc, "fill=\""+colorSurface+"\"", name)
		require.NotContains(t, doc, "<script", name)
	}
}

func TestGraphWallClockHasOneBarPerSeriesPerRow(t *testing.T) {
	d := fakeGraphData()

	c := chartWallClock(d, false)
	require.Len(t, c.panels, 1)
	require.Len(t, c.panels[0].rows, 4)

	elements, text := parseSVG(t, renderBarChart(c))

	// One path per bar: four rows, two lanes.
	require.Equal(t, 8, countElement(elements, "path"))

	for _, want := range []string{"native", "rclone", "small-write", "delete-sweep", "120.9 s", "199.0 s"} {
		require.Contains(t, text, want)
	}

	// The overlapping row is flagged rather than presented as a difference.
	require.Contains(t, text, sigNoise)
}

func TestGraphSplitPanelsSeparateMagnitudes(t *testing.T) {
	c := chartWallClock(fakeGraphData(), true)

	require.Greater(t, len(c.panels), 1, "a 100x spread must not share one axis")

	for _, p := range c.panels {
		require.NotEmpty(t, p.heading, "every panel of a split chart names its band")
	}

	_, text := parseSVG(t, renderBarChart(c))
	require.Contains(t, text, "OWN axis")
}

// TestGraphCPUChartFlagsBrokenAccounting pins the tripwire in the place a
// reader actually meets it: a lane that did 2.4 s of network work while
// reporting 3 ms of CPU is drawn as a hatch labeled BROKEN-ACCOUNTING, not as a
// very short bar that looks like a win.
func TestGraphCPUChartFlagsBrokenAccounting(t *testing.T) {
	c := chartCPU(fakeGraphData())
	require.NotNil(t, c)

	doc := renderBarChart(c)
	_, text := parseSVG(t, doc)

	require.Contains(t, text, "BROKEN-ACCOUNTING")
	require.Contains(t, doc, "url(#broken)")
	require.Contains(t, text, "Fairness:", "the CPU chart must carry the fairness note")
}

// TestGraphQuotaChartIsSingleSeries pins the honesty rule: rclone's quota units
// are unobservable, and a zero-height bar next to a real one would state
// something false.
func TestGraphQuotaChartIsSingleSeries(t *testing.T) {
	c := chartQuota(fakeGraphData())
	require.NotNil(t, c)
	require.Equal(t, []string{"native"}, c.series)

	_, text := parseSVG(t, renderBarChart(c))

	require.Contains(t, text, "UNOBSERVED")
	require.Contains(t, text, "rclone")
	require.Contains(t, text, "15,000")
}

func TestGraphSuppressesUnmeasuredCPU(t *testing.T) {
	d := fakeGraphData()

	for _, s := range d.series {
		for wk := range d.cells {
			if _, ok := d.cells[wk]["cpu_total_ms"]; ok {
				d.put(wk, "cpu_total_ms", s, graphCell{stat: StatSummary{N: 5}})
			}
		}
	}

	require.Nil(t, chartCPU(d), "a document written before CPU accounting must not get a chart of zeros")
}

func TestSortWorkloadKeysUsesExecutionOrder(t *testing.T) {
	keys := []string{
		snapshotE2EName + ":verify", "list-heavy:prefixed", "read-metadata:cold",
		"small-write", snapshotE2EName + ":create", "read-metadata:hot", "list-heavy:full", "delete-sweep",
	}

	sortWorkloadKeys(keys)

	require.Equal(t, []string{
		"small-write",
		"read-metadata:hot",
		"read-metadata:cold",
		"list-heavy:full",
		"list-heavy:prefixed",
		"delete-sweep",
		snapshotE2EName + ":create",
		snapshotE2EName + ":verify",
	}, keys)
}

func TestNiceTicksCoverTheData(t *testing.T) {
	for _, maxV := range []float64{1, 7, 108.6, 999, 1234, 251200} {
		ticks := niceTicks(maxV, tickTarget)

		require.GreaterOrEqual(t, ticks[len(ticks)-1], maxV, "the axis must reach past the data, max %v", maxV)
		require.InDelta(t, 0.0, ticks[0], 1e-9)
		require.GreaterOrEqual(t, len(ticks), 2)
	}
}

func TestGraphLoadsBothDocumentKinds(t *testing.T) {
	dir := t.TempDir()

	res := syntheticResult()
	resPath := filepath.Join(dir, "run.json")
	require.NoError(t, writeResult(resPath, res))

	fromResult, err := loadGraphData(resPath)
	require.NoError(t, err)
	require.Equal(t, []string{"native", "rclone"}, fromResult.series)

	series := buildSeries([]string{resPath}, []*RunResult{res})
	rep := compareSeries(series)
	cmpPath := filepath.Join(dir, "compare.json")
	require.NoError(t, writeJSON(cmpPath, rep))

	fromCompare, err := loadGraphData(cmpPath)
	require.NoError(t, err)
	require.Equal(t, []string{"native", "rclone"}, fromCompare.series)

	// The two paths must agree: a chart and a comparison table are the same
	// numbers.
	a, ok := fromResult.get("small-write", "wall_clock_ms", "rclone")
	require.True(t, ok)

	b, ok := fromCompare.get("small-write", "wall_clock_ms", "rclone")
	require.True(t, ok)
	require.InDelta(t, a.stat.Median, b.stat.Median, 1e-9)

	// And a document that is neither is refused rather than half-drawn.
	junk := filepath.Join(dir, "junk.json")
	require.NoError(t, os.WriteFile(junk, []byte(`{"hello":"world"}`), 0o600))

	_, err = loadGraphData(junk)
	require.Error(t, err)
}

func TestGraphCommandWritesFiles(t *testing.T) {
	dir := t.TempDir()

	resPath := filepath.Join(dir, "run.json")
	require.NoError(t, writeResult(resPath, syntheticResult()))

	outDir := filepath.Join(dir, "graphs")

	var out, errOut bytes.Buffer

	// The file name comes before the flag, which is how the subcommand is
	// documented and how an operator will type it.
	require.Equal(t, exitOK, cmdGraph(context.Background(), []string{resPath, "--out-dir", outDir, "--prefix", "x-"}, &out, &errOut), errOut.String())

	entries, err := os.ReadDir(outDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	for _, e := range entries {
		require.True(t, strings.HasPrefix(e.Name(), "x-"), e.Name())
		require.True(t, strings.HasSuffix(e.Name(), ".svg"), e.Name())

		data, err := os.ReadFile(filepath.Join(outDir, e.Name()))
		require.NoError(t, err)

		parseSVG(t, string(data))
	}

	require.Equal(t, exitRefused, cmdGraph(context.Background(), []string{resPath}, &out, &errOut), "an --out-dir is required")
}

// syntheticResult is a minimal two-lane run document with CPU metrics.
func syntheticResult() *RunResult {
	trial := func(target string, i int, wall, cpu float64) TrialResult {
		return TrialResult{
			Trial:  i,
			Target: target,
			Workloads: map[string]WorkloadMetrics{
				"small-write": {
					WallClockMS: wall, Ops: 100, Samples: 100,
					Latency:    Percentiles{P50: wall / 100},
					CPUTotalMS: cpu, QuotaUnits: 15000, CPUSource: "rusage+proc",
				},
			},
			ErrorsByReason: map[string]int64{},
			CPUTotalMS:     cpu * 2,
			CPUResidualMS:  cpu,
		}
	}

	r := &RunResult{
		SchemaVersion: schemaVersion,
		RunID:         "rtest",
		Label:         "synthetic",
		StartedAt:     time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
		Harness:       HarnessInfo{Commit: "0123456789abcdef"},
		Targets: []TargetInfo{
			{Name: "native", StorageType: "gdrive"},
			{Name: "rclone", StorageType: "rclone"},
		},
		Errors: []string{},
	}

	for i := range 5 {
		r.Trials = append(r.Trials,
			trial("native", i, 120000+float64(i)*300, 4200+float64(i)*10),
			trial("rclone", i, 199000+float64(i)*400, 9100+float64(i)*10),
		)
	}

	return r
}
