//go:build !no_extra_providers

package main

import (
	"encoding/json"
	"os"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo/blob/gdrive"
)

// schemaVersion is the version of the result document written by "run". It is
// frozen together with SCHEMA.md: any change that is not purely additive must
// bump it.
const schemaVersion = 1

// RunResult is the whole output of one "gdrive-bench run". SCHEMA.md documents
// every field; the JSON tags here are the contract.
type RunResult struct {
	SchemaVersion int         `json:"schema_version"`
	RunID         string      `json:"run_id"`
	Label         string      `json:"label"`
	StartedAt     time.Time   `json:"started_at"`
	FinishedAt    time.Time   `json:"finished_at"`
	Harness       HarnessInfo `json:"harness"`
	Config        RunConfig   `json:"config"`

	// Targets describes what was measured, in the order the targets file listed
	// them (not the order they ran in - that is Trials[].position).
	Targets []TargetInfo `json:"targets"`

	// Trials holds one entry per (trial, target) execution, in execution order.
	Trials []TrialResult `json:"trials"`

	Quota QuotaReport `json:"quota"`

	// Errors collects non-fatal problems: cleanup failures, ledger write
	// failures, and workload errors when --keep-going was given.
	Errors []string `json:"errors"`
}

// HarnessInfo identifies the code that produced a result.
type HarnessInfo struct {
	Commit    string `json:"commit"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Hostname  string `json:"hostname"`
}

// RunConfig records every harness-level knob a result depends on.
type RunConfig struct {
	Trials      int         `json:"trials"`
	Order       string      `json:"order"`
	CooldownMS  int64       `json:"cooldown_ms"`
	Smoke       bool        `json:"smoke"`
	Seed        int64       `json:"seed"`
	KeepGoing   bool        `json:"keep_going"`
	Workloads   []string    `json:"workloads"`
	TargetsFile string      `json:"targets_file"`
	Params      benchParams `json:"params"`

	// KopiaExe is the binary the out-of-process snapshot-e2e workload drove,
	// empty when that workload did not run. When the harness built it on
	// demand, it is the code of harness.commit.
	KopiaExe string `json:"kopia_exe,omitempty"`
}

// TargetInfo describes one measured backend.
type TargetInfo struct {
	Name string `json:"name"`

	// StorageType is the blob provider type ("gdrive", "rclone", "filesystem", ...).
	StorageType string `json:"storage_type"`

	// Knobs is the connection's "tuning" object as configured, or null when the
	// backend's built-in defaults are in force.
	Knobs map[string]any `json:"knobs"`

	// Config is the whole connection config with credentials redacted.
	Config map[string]any `json:"config"`
}

// TrialResult is one target's pass through the suite within one trial.
type TrialResult struct {
	Trial     int       `json:"trial"`
	Sequence  int       `json:"sequence"`
	Position  int       `json:"position"`
	Target    string    `json:"target"`
	StartedAt time.Time `json:"started_at"`

	// Workloads maps a result key to its metrics. The key is a registered
	// workload name, optionally suffixed with ":<phase>" when one workload
	// reports several phases (read-metadata:hot, read-metadata:cold).
	Workloads map[string]WorkloadMetrics `json:"workloads"`

	// ErrorsByReason is the trial's Drive error tally, summed from the transport
	// deltas of its workloads. Empty for backends without Drive instrumentation.
	ErrorsByReason map[string]int64 `json:"errors_by_reason"`

	// CPUTotalMS is the CPU the whole trial cost the machine - this process plus
	// every descendant - and CPUResidualMS is the part of it that no measured
	// phase claimed: workload setup writes, cleanup deletes, connection setup and
	// teardown, and the idle gaps between phases.
	//
	// It is published because it is the audit of the per-phase attribution: if
	// the phases stopped seeing the rclone subprocess, the CPU does not vanish,
	// it moves here. A residual comparable to the sum of the phases is a leak,
	// not a quiet measurement.
	CPUTotalMS    float64 `json:"cpu_total_ms"`
	CPUResidualMS float64 `json:"cpu_residual_ms"`
}

// WorkloadMetrics is what every workload reports.
type WorkloadMetrics struct {
	WallClockMS float64     `json:"wall_clock_ms"`
	Ops         int         `json:"ops"`
	Errors      int         `json:"errors"`
	BytesUp     int64       `json:"bytes_up"`
	BytesDown   int64       `json:"bytes_down"`
	Samples     int         `json:"samples"`
	Latency     Percentiles `json:"latency_ms"`

	// QuotaUnits is the workload's Drive quota-unit consumption: counted from
	// the observed API calls where the backend is instrumented, estimated from
	// the planned operations otherwise. QuotaUnitsSource says which.
	QuotaUnits       int64  `json:"quota_units"`
	QuotaUnitsSource string `json:"quota_units_source"`

	// CPU accounting for the phase, in milliseconds. CPUUserMS and CPUSysMS are
	// the harness process's own RUSAGE_SELF delta; CPUChildrenUserMS and
	// CPUChildrenSysMS are every descendant process's CPU attributed to the
	// phase. CPUTotalMS is their sum and is the headline number: it is the only
	// figure that compares a backend running INSIDE the harness against one whose
	// work happens in an rclone subprocess. CPUPct is CPUTotalMS/WallClockMS in
	// percent, so 100 is one core saturated. CPUSource names the accounting that
	// produced the children columns - see cpu.go.
	CPUUserMS         float64 `json:"cpu_user_ms"`
	CPUSysMS          float64 `json:"cpu_sys_ms"`
	CPUChildrenUserMS float64 `json:"cpu_children_user_ms"`
	CPUChildrenSysMS  float64 `json:"cpu_children_sys_ms"`
	CPUTotalMS        float64 `json:"cpu_total_ms"`
	CPUPct            float64 `json:"cpu_pct"`
	CPUSource         string  `json:"cpu_source"`

	// PacedWaitMS is the total time this phase spent waiting for the pacer's
	// steady-state token bucket - the pacer's error-free throttling, summed
	// from Transport. It is 0 for every backend that is not the native Drive
	// one, and 0 on a Drive workload that never outran the bucket, which is a
	// measurement and not an absence: it is what falsifies a hypothesis about
	// pacerMinSleepMS or pacerBurst.
	PacedWaitMS float64 `json:"paced_wait_ms"`

	// Params records the effective value of any workload parameter the harness
	// resolved at run time instead of taking straight from config.params - a
	// value config.params therefore cannot be trusted to report. Currently one
	// key: "parallelism" on delete-sweep, which prefers the target's
	// tuning.deleteParallelism over the harness's --parallelism.
	Params map[string]int64 `json:"params,omitempty"`

	// Transport is the native backend's Drive API counter delta for this
	// workload, or null for any backend that is not the native Drive one.
	Transport *gdrive.TransportStatsSnapshot `json:"transport_delta"`

	// Error is the first operation error of the workload, if any.
	Error string `json:"error,omitempty"`
}

// Percentiles is a latency distribution summary, in milliseconds.
type Percentiles struct {
	P10 float64 `json:"p10"`
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
}

// QuotaReport records what the quota guard decided and what the run actually
// spent.
type QuotaReport struct {
	Decision quotaDecision `json:"decision"`

	// ActualUploadBytes and ActualQuotaUnits are what was charged to the ledger:
	// they only count targets whose traffic leaves the machine (everything
	// except the filesystem backend).
	ActualUploadBytes int64 `json:"actual_upload_bytes"`
	ActualQuotaUnits  int64 `json:"actual_quota_units"`

	// ObservedQUPerMinute is the run's average quota-unit rate, for comparison
	// against the 325,000 QU/min/user ceiling. Averages hide bursts, so it is
	// informational only.
	ObservedQUPerMinute float64 `json:"observed_qu_per_minute"`

	LedgerPath string `json:"ledger_path"`
}

// StatSummary is one series' central value and spread, used by compare and
// baseline.
type StatSummary struct {
	N      int     `json:"n"`
	Median float64 `json:"median"`
	P10    float64 `json:"p10"`
	P90    float64 `json:"p90"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

// Metric names that the charts and checks refer to individually.
const (
	metricWallClockMS = "wall_clock_ms"
	metricQuotaUnits  = "quota_units"
	metricCPUTotalMS  = "cpu_total_ms"
	metricOpsPerSec   = "ops_per_sec"
	metricP50MS       = "p50_ms"
)

// metricNames are the per-workload metrics the comparison protocol understands,
// in report order.
var metricNames = []string{metricWallClockMS, metricP50MS, "p90_ms", "p99_ms", metricOpsPerSec, "errors", "bytes_up", "bytes_down", metricQuotaUnits, "paced_wait_ms", metricCPUTotalMS}

// keyMetricNames are the metrics a baseline check regards as pass/fail signals.
//
// cpu_total_ms is deliberately NOT one of them. It is the headline number of a
// lane comparison, where both lanes are measured minutes apart on the same
// machine; it is a poor regression gate, because the CPU a desktop has spare
// varies with whatever else the operator is running, and a baseline promoted on
// a quiet machine would fail every check made on a busy one. Compare flags it,
// a human reads it, and nothing fails a build over it.
var keyMetricNames = []string{metricWallClockMS, metricP50MS}

// higherIsBetter reports whether a larger value of a metric is an improvement.
// Everything the harness measures is a cost except throughput.
func higherIsBetter(metric string) bool {
	return metric == metricOpsPerSec
}

// metricValue extracts a named metric from a workload's metrics. The metrics
// are passed by pointer only because the struct is large; nothing is mutated.
func metricValue(m *WorkloadMetrics, metric string) (float64, bool) {
	switch metric {
	case metricWallClockMS:
		return m.WallClockMS, true
	case "p10_ms":
		return m.Latency.P10, true
	case metricP50MS:
		return m.Latency.P50, true
	case "p90_ms":
		return m.Latency.P90, true
	case "p99_ms":
		return m.Latency.P99, true
	case metricOpsPerSec:
		if m.WallClockMS <= 0 {
			return 0, false
		}

		return float64(m.Ops) * float64(time.Second/time.Millisecond) / m.WallClockMS, true
	case "errors":
		return float64(m.Errors), true
	case "bytes_up":
		return float64(m.BytesUp), true
	case "bytes_down":
		return float64(m.BytesDown), true
	case metricQuotaUnits:
		return float64(m.QuotaUnits), true
	case "paced_wait_ms":
		return m.PacedWaitMS, true
	default:
		return cpuMetricValue(m, metric)
	}
}

// cpuMetricValue extracts one of the CPU metrics (§8.8 of SCHEMA.md). It is
// split out of metricValue so that neither switch has to grow past the point a
// reader can check it at a glance.
func cpuMetricValue(m *WorkloadMetrics, metric string) (float64, bool) {
	switch metric {
	case "cpu_user_ms":
		return m.CPUUserMS, true
	case "cpu_sys_ms":
		return m.CPUSysMS, true
	case "cpu_children_user_ms":
		return m.CPUChildrenUserMS, true
	case "cpu_children_sys_ms":
		return m.CPUChildrenSysMS, true
	case metricCPUTotalMS:
		return m.CPUTotalMS, true
	case "cpu_pct":
		return m.CPUPct, true
	default:
		return 0, false
	}
}

// writeResult serializes a result document to path.
func writeResult(path string, r *RunResult) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errors.Wrap(err, "unable to encode the result document")
	}

	return writeFileAtomic(path, append(data, '\n'))
}

// readResult loads and validates a result document.
func readResult(path string) (*RunResult, error) {
	//nolint:gosec // the path is named on the command line by the operator.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read the result document %v", path)
	}

	r := &RunResult{}
	if err := json.Unmarshal(data, r); err != nil {
		return nil, errors.Wrapf(err, "unable to decode the result document %v", path)
	}

	if r.SchemaVersion != schemaVersion {
		return nil, errors.Errorf("%v has schema_version %v, this harness understands %v", path, r.SchemaVersion, schemaVersion)
	}

	return r, nil
}

// targetNames returns every target name that actually ran, **in configuration
// order** - the order the targets file listed them, which `targets[]` preserves
// (§2 of SCHEMA.md).
//
// This ordering is load-bearing, not cosmetic: compare treats the first series
// as the reference, so the first target of the targets file is the control lane
// and every delta reads "candidate relative to control". Sorting here (which is
// what this did before WP-4b) silently inverted the sign of every delta for any
// pair of lane names whose alphabetical order differs from their configured
// one - `control` / `candidate` being exactly such a pair.
//
// A target that ran without appearing in `targets[]` can only come from a
// hand-edited document, and is appended in execution order rather than dropped:
// losing a series would be a worse failure than ordering it oddly.
func (r *RunResult) targetNames() []string {
	ran := map[string]bool{}
	executionOrder := []string{}

	for _, t := range r.Trials {
		if !ran[t.Target] {
			ran[t.Target] = true

			executionOrder = append(executionOrder, t.Target)
		}
	}

	out := []string{}
	emitted := map[string]bool{}

	for _, t := range r.Targets {
		if ran[t.Name] && !emitted[t.Name] {
			emitted[t.Name] = true

			out = append(out, t.Name)
		}
	}

	for _, n := range executionOrder {
		if !emitted[n] {
			emitted[n] = true

			out = append(out, n)
		}
	}

	return out
}
