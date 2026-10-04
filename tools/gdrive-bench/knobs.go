//go:build !no_extra_providers

package main

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/kopia/kopia/repo/blob/gdrive"
	"github.com/kopia/kopia/repo/blob/gdrive/gdrivepacer"
)

// Knob groups.
const (
	groupTuning  = "gdrive.tuning"
	groupPacer   = "gdrive.pacer"
	groupHarness = "harness"
)

// KnobInfo describes one tunable, as emitted by "gdrive-bench knobs".
type KnobInfo struct {
	// Name is the knob's fully qualified name: "<group>.<field>". For the two
	// gdrive groups the field is the JSON key of the connection config, so a
	// name is directly actionable in a targets file.
	Name  string `json:"name"`
	Group string `json:"group"`
	Field string `json:"field"`
	Type  string `json:"type"`

	// Default is the value in force when the knob is left unset.
	Default string `json:"default"`

	// RangeHint bounds what is worth sweeping. It is advice, not validation.
	RangeHint string `json:"range_hint"`

	// Affects lists the workloads whose numbers plausibly move when the knob
	// moves. It is hand-maintained (see knobTable) and is the map a tuning
	// agent uses to decide what to measure after a change.
	Affects []string `json:"affects"`

	Doc string `json:"doc"`

	// Undocumented is set when a field exists in the source struct but not in
	// the hand-maintained table - which means this harness is out of date with
	// the backend.
	Undocumented bool `json:"undocumented,omitempty"`
}

// knobMeta is the hand-maintained half of a knob description: everything that
// cannot be recovered by reflection.
type knobMeta struct {
	def     string
	hint    string
	affects []string
	doc     string
}

// Workload name groups used by the affects table.
var (
	writeWorkloads = []string{workloadSmallWrite, workloadPackWrite}
	allNames       = workloadNames()
)

// knobTable is the affects-mapping, maintained by hand. Every field of
// gdrive.TuningOptions and gdrivepacer.Config must appear here;
// TestKnobTableCoversEveryField enforces it, so adding a knob to the backend
// without describing it here breaks the build's tests rather than silently
// producing an incomplete dump.
var knobTable = map[string]knobMeta{
	groupTuning + ".pacerMinSleepMS": {
		def: "100", hint: "25..500 ms",
		affects: allNames,
		doc:     "Steady-state interval between Drive API calls. The single biggest throughput knob, and the one Google's rate limits push back on.",
	},
	groupTuning + ".pacerBurst": {
		def: "100", hint: "10..500 calls",
		affects: allNames,
		doc:     "Calls allowed back-to-back before the steady-state interval applies. Governs how fast a burst of small operations can start.",
	},
	groupTuning + ".pacerMaxSleepMS": {
		def: "16000", hint: "1000..60000 ms",
		affects: allNames,
		doc:     "Cap on the exponential backoff ladder. Only visible on a run that is actually being rate limited.",
	},
	groupTuning + ".maxTries": {
		def: "10", hint: "3..20 attempts",
		affects: allNames,
		doc:     "Attempts a retriable operation gets. Affects tail latency and how a bad minute is absorbed rather than reported.",
	},
	groupTuning + ".uploadChunkSizeMB": {
		def: "64", hint: "8..256 MB, multiples of 8",
		affects: []string{workloadPackWrite},
		doc:     "Resumable upload chunk size. Also a quota knob: every chunk is a separate edit-class request.",
	},
	groupTuning + ".simpleUploadCutoffMB": {
		def: "8", hint: "1..64 MB",
		affects: writeWorkloads,
		doc:     "Size at or above which the resumable protocol replaces a single multipart request.",
	},
	groupTuning + ".listPageSize": {
		def: "1000", hint: "100..1000 items (1000 is the API maximum)",
		affects: []string{workloadListHeavy, workloadReadMetadata},
		doc:     "files.list page size. Each page is a 100-quota-unit call, so smaller pages cost proportionally more quota.",
	},
	groupTuning + ".deleteParallelism": {
		def: "8", hint: "1..32 concurrent deletes",
		affects: []string{workloadDeleteSweep},
		doc: "Ceiling on concurrent delete calls held open by one connection. Setting it also raises the delete-sweep workload's own parallelism to " +
			"the same value (the harness would otherwise clamp it at --parallelism and the knob would measure nothing); the value actually used is " +
			"recorded in the workload's params.parallelism.",
	},
	groupTuning + ".useBatchDelete": {
		def: "false", hint: "true|false",
		affects: []string{workloadDeleteSweep},
		doc:     "Selects the homogeneous batch delete endpoint. Measured at 2.5x SLOWER than parallel singles in Phase 2 (Drive runs batch parts serially); kept for re-measurement, not as a recommendation.",
	},
	groupTuning + ".cacheDir": {
		def: "platform cache directory", hint: "an absolute path",
		affects: []string{workloadReadMetadata, workloadDeleteSweep},
		doc:     "Directory holding the persistent blob-ID to file-ID journal. Moving it onto slower storage shows up in the cold metadata pass.",
	},

	groupPacer + ".minSleep": {
		def: gdrivepacer.DefaultMinSleep.String(), hint: "25ms..500ms",
		affects: allNames,
		doc:     "Resolved form of tuning.pacerMinSleepMS. Set through the tuning knob, not directly.",
	},
	groupPacer + ".burst": {
		def: "100", hint: "10..500 calls",
		affects: allNames,
		doc:     "Resolved form of tuning.pacerBurst.",
	},
	groupPacer + ".maxSleep": {
		def: gdrivepacer.DefaultMaxSleep.String(), hint: "1s..60s",
		affects: allNames,
		doc:     "Resolved form of tuning.pacerMaxSleepMS. A Retry-After from Drive is honored uncapped regardless.",
	},
	groupPacer + ".maxTries": {
		def: "10", hint: "3..20 attempts",
		affects: allNames,
		doc:     "Resolved form of tuning.maxTries.",
	},
}

// harnessKnobs are the knobs of the harness itself, which change what is
// measured rather than how the backend behaves.
func harnessKnobs() []KnobInfo {
	mk := func(field, typ, def, hint, doc string, affects []string) KnobInfo {
		return KnobInfo{
			Name: groupHarness + "." + field, Group: groupHarness, Field: field,
			Type: typ, Default: def, RangeHint: hint, Affects: affects, Doc: doc,
		}
	}

	return []KnobInfo{
		mk("trials", "int", "5 (smoke: 1)", "3..15", "Number of trials of the whole suite per target. Fewer than 3 makes the p10/p90 interval meaningless and every comparison unreliable.", allNames),
		mk("order", "string", "abba", "abba|random|sequential", "Trial interleaving. sequential exists for debugging only: it cannot tell a slow backend from a slow minute.", allNames),
		mk("cooldown", "duration", "10s (smoke: 0)", "0s..60s", "Pause between consecutive target executions, so one target's backoff state does not spill into the next.", allNames),
		mk("seed", "int64", "0 (clock)", "any", "Random seed. Recorded in the result; reusing it reproduces the exact blob sizes, offsets and orderings.", []string{workloadSmallWrite, workloadRangeRead}),
		mk("pack-size-mb", "int64", "20", "1..1024 MB", "Size of a pack-write blob, and of the pack the range reads come from.", []string{workloadPackWrite, workloadRangeRead}),
		mk("small-write-n", "int", "100 (smoke: 10)", "10..1000", "Number of small blobs written by small-write.", []string{workloadSmallWrite}),
		mk("delete-sweep-n", "int", "100 (smoke: 10)", "10..1000",
			"Blobs delete-sweep writes; it then deletes delete_percent of them. Raise it to make the measured phase long enough for a pacer knob to bind - "+
				"at the default 100 the phase is ~2 s and draws fewer tokens than the burst pool holds.",
			[]string{workloadDeleteSweep}),
		mk("parallelism", "int", "8", "1..64",
			"Bounded parallelism of every workload's cleanup, and of delete-sweep unless the target sets tuning.deleteParallelism, which takes precedence "+
				"(see that knob). A run that changes it is not comparable to the standing baseline.",
			[]string{workloadDeleteSweep}),
		mk("daily-upload-fraction", "float64", "0.5", "0.01..1.0", "Fraction of Google's 750 GB/day/user upload cap the harness may spend in one UTC day before it refuses to start.", nil),
		mk("true-cold", "bool", "false", "true|false",
			"Delete the on-disk file ID journal between the warm and the cold metadata pass, so read-metadata:cold measures a machine that has "+
				"never seen the folder. Adds read-metadata:warm and requires every Drive target to set tuning.cacheDir.",
			[]string{workloadReadMetadata}),
		mk("kopia-exe", "string", "built from the work tree", "a path", "The kopia binary snapshot-e2e drives. Leave it unset to measure the code the result's harness.commit names.", []string{snapshotE2EName}),
		mk("snapshot-files", "int", "800 (smoke: 40)", "50..10000", "Files in the snapshot-e2e source tree. Together with snapshot-mb it decides whether the workload measures per-file overhead or upload throughput.", []string{snapshotE2EName}),
		mk("snapshot-mb", "int64", "150 (smoke: 6)", "10..2000 MB", "Total size of the snapshot-e2e source tree.", []string{snapshotE2EName}),
	}
}

// collectKnobs enumerates every tunable: the two backend structs by reflection,
// plus the harness's own.
func collectKnobs() []KnobInfo {
	out := []KnobInfo{}
	out = append(out, reflectKnobs(groupTuning, reflect.TypeFor[gdrive.TuningOptions]())...)
	out = append(out, reflectKnobs(groupPacer, reflect.TypeFor[gdrivepacer.Config]())...)
	out = append(out, harnessKnobs()...)

	return out
}

// reflectKnobs enumerates the exported fields of a struct, taking their names
// from the JSON tags so a knob's name is the key an operator writes into a
// targets file.
func reflectKnobs(group string, t reflect.Type) []KnobInfo {
	out := []KnobInfo{}

	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}

		name := jsonFieldName(f)
		if name == "" {
			continue
		}

		k := KnobInfo{
			Name:  group + "." + name,
			Group: group,
			Field: name,
			Type:  f.Type.String(),
		}

		if meta, ok := knobTable[k.Name]; ok {
			k.Default = meta.def
			k.RangeHint = meta.hint
			k.Affects = meta.affects
			k.Doc = meta.doc
		} else {
			k.Undocumented = true
			k.Doc = "UNDOCUMENTED: this field exists in the backend but not in the harness's knob table; update tools/gdrive-bench/knobs.go."
		}

		out = append(out, k)
	}

	return out
}

// jsonFieldName returns the JSON key of a struct field, or "" when the field is
// not serialized.
func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}

	if tag == "" {
		return f.Name
	}

	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}

	if tag == "" {
		return f.Name
	}

	return tag
}

// undocumentedKnobs returns the knobs whose metadata is missing.
func undocumentedKnobs(knobs []KnobInfo) []string {
	out := []string{}

	for i := range knobs {
		if knobs[i].Undocumented {
			out = append(out, knobs[i].Name)
		}
	}

	sort.Strings(out)

	return out
}

// staleKnobTableEntries returns table entries that no longer correspond to a
// field of the backend structs.
func staleKnobTableEntries(knobs []KnobInfo) []string {
	live := map[string]bool{}
	for i := range knobs {
		live[knobs[i].Name] = true
	}

	out := []string{}

	for name := range knobTable {
		if !live[name] {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out
}

// cmdKnobs implements "gdrive-bench knobs".
func cmdKnobs(_ context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("knobs", errOut)
	format := fs.String("format", "json", "output format: json or table")
	group := fs.String("group", "", "restrict to one group: "+strings.Join([]string{groupTuning, groupPacer, groupHarness}, ", "))

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	knobs := collectKnobs()

	if *group != "" {
		filtered := []KnobInfo{}

		for i := range knobs {
			if knobs[i].Group == *group {
				filtered = append(filtered, knobs[i])
			}
		}

		knobs = filtered
	}

	for _, n := range undocumentedKnobs(knobs) {
		outf(errOut, "warning: %v has no entry in the harness knob table\n", n)
	}

	if *format == "table" {
		printKnobTable(out, knobs)

		return exitOK
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")

	if err := enc.Encode(knobs); err != nil {
		outf(errOut, "gdrive-bench knobs: %v\n", err)

		return exitFailure
	}

	return exitOK
}

// printKnobTable renders the knobs for humans.
func printKnobTable(out io.Writer, knobs []KnobInfo) {
	tw := tabwriter.NewWriter(out, 0, 0, tabwriterPadding, ' ', 0)

	outln(tw, "NAME\tTYPE\tDEFAULT\tRANGE\tAFFECTS")

	for i := range knobs {
		k := &knobs[i]

		outf(tw, "%v\t%v\t%v\t%v\t%v\n", k.Name, k.Type, k.Default, k.RangeHint, strings.Join(k.Affects, ","))
	}

	tw.Flush() //nolint:errcheck // best-effort table output.
}
