//go:build !no_extra_providers

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo/blob"
)

// Trial orderings. abba is the default because it is the cheapest defense
// against a drifting network: with two targets the sequence is A B A B A,
// so any monotonic drift over the run affects both targets equally.
const (
	orderABBA       = "abba"
	orderRandom     = "random"
	orderSequential = "sequential"
)

// Run defaults.
const (
	defaultTrials               = 5
	smokeTrials                 = 1
	defaultCooldown             = 10 * time.Second
	defaultDailyUploadFraction  = 0.5
	runIDRandomBytes            = 3
	resultTimestampLayout       = "20060102T150405Z"
	filesystemStorageType       = "filesystem"
	defaultResultsDirNameSuffix = "kopia-gdrive-bench-results"
)

// runOptions is the parsed command line of "run".
type runOptions struct {
	targetsFile    string
	workloads      string
	trials         int
	order          string
	cooldown       time.Duration
	smoke          bool
	label          string
	keepGoing      bool
	seed           int64
	resultsDir     string
	uploadFraction float64
	packSizeMB     int64
	smallWriteN    int
	deleteSweepN   int
	parallelism    int
	harnessCommit  string
	trueCold       bool
	kopiaExe       string
	snapshotFiles  int
	snapshotMB     int64
	explicitlySet  map[string]bool
}

func cmdRun(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("run", errOut)
	o := runOptions{}

	fs.StringVar(&o.targetsFile, "targets", "", "path to the targets JSON file (an array of {name, connection}); omit with --smoke for a temp-dir filesystem target")
	fs.StringVar(&o.workloads, "workloads", "", "comma-separated workloads to run (default: "+strings.Join(defaultWorkloadNames(), ",")+"; also available, opt-in: "+snapshotE2EName+")")
	fs.IntVar(&o.trials, "trials", defaultTrials, "number of trials of the whole suite per target")
	fs.StringVar(&o.order, "order", orderABBA, "trial ordering: abba (default), random, or sequential (debugging only - it cannot separate a slow backend from a slow minute)")
	fs.DurationVar(&o.cooldown, "cooldown", defaultCooldown, "pause between consecutive target executions")
	fs.BoolVar(&o.smoke, "smoke", false, "tiny workload sizes and a single trial, for plumbing checks")
	fs.StringVar(&o.label, "label", "", "label for the result file name (default: the target names joined)")
	fs.BoolVar(&o.keepGoing, "keep-going", false, "continue after a workload fails (the run still exits nonzero)")
	fs.Int64Var(&o.seed, "seed", 0, "random seed; 0 picks one and records it in the result")
	fs.StringVar(&o.resultsDir, "results-dir", "", "where results are written (default: $KOPIA_GDRIVE_BENCH_RESULTS)")
	fs.Float64Var(&o.uploadFraction, "daily-upload-fraction", defaultDailyUploadFraction, "fraction of Google's 750 GB/day/user upload cap this harness may spend in one UTC day")
	fs.Int64Var(&o.packSizeMB, "pack-size-mb", 0, "size of a pack-write blob in MB (default: 20)")
	fs.IntVar(&o.smallWriteN, "small-write-n", 0, "number of small-write blobs (default: 100, smoke: 10)")
	fs.IntVar(&o.deleteSweepN, "delete-sweep-n", 0, "number of blobs delete-sweep writes, of which it deletes delete_percent (default: 100, smoke: 10)")
	fs.IntVar(&o.parallelism, "parallelism", 0, "bounded parallelism of cleanup, and of delete-sweep unless the target sets tuning.deleteParallelism (default: 8)")
	fs.StringVar(&o.harnessCommit, "harness-commit", "", "override the recorded harness commit (default: git rev-parse HEAD)")
	fs.BoolVar(&o.trueCold, "true-cold", false, "make read-metadata:cold genuinely cold by deleting the on-disk file ID journal between passes (adds a read-metadata:warm phase; requires every Drive target to set config.tuning.cacheDir)")
	fs.StringVar(&o.kopiaExe, "kopia-exe", "", "kopia binary the snapshot-e2e workload drives (default: build one from the current work tree)")
	fs.IntVar(&o.snapshotFiles, "snapshot-files", 0, "number of files in the snapshot-e2e source tree (default: 800, smoke: 40)")
	fs.Int64Var(&o.snapshotMB, "snapshot-mb", 0, "total size of the snapshot-e2e source tree in MB (default: 150, smoke: 6)")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	o.explicitlySet = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.explicitlySet[f.Name] = true })

	code, err := doRun(ctx, &o, out)
	if err != nil {
		outf(errOut, "gdrive-bench run: %v\n", err)
	}

	return code
}

// doRun performs the whole run and returns the process exit code.
func doRun(ctx context.Context, o *runOptions, out io.Writer) (int, error) {
	cfg, err := resolveRunConfig(o)
	if err != nil {
		return exitRefused, err
	}

	targets, cleanupTargets, err := resolveTargets(o)
	if err != nil {
		return exitRefused, err
	}

	defer cleanupTargets()

	wls, err := selectWorkloads(o.workloads)
	if err != nil {
		return exitRefused, err
	}

	if cfg.Params.TrueCold {
		if err := validateTrueCold(targets); err != nil {
			return exitRefused, err
		}
	}

	cleanupExe, err := resolveKopiaExe(ctx, o, wls, &cfg, out)
	if err != nil {
		return exitRefused, err
	}

	defer cleanupExe()

	resultsDir, err := resolveResultsDir(ctx, o.resultsDir)
	if err != nil {
		return exitRefused, err
	}

	res := newRunResult(ctx, o, &cfg, targets, wls)

	ledger, err := loadQuotaLedger(resultsDir)
	if err != nil {
		return exitRefused, err
	}

	est := estimateRun(wls, &cfg.Params, targets, cfg.Trials)
	decision := checkQuota(ledger, nowUTC(), est.UploadBytes, est.QuotaUnits, o.uploadFraction)
	res.Quota.Decision = decision
	res.Quota.LedgerPath = filepath.Join(resultsDir, ledgerFileName)

	printQuotaPlan(out, decision)

	if !decision.Allowed {
		return exitRefused, errors.Errorf("refusing to start: %v (already %v today, this run would add %v, budget %v)",
			decision.Reason, formatBytes(decision.AlreadyBytes), formatBytes(decision.EstimatedBytes), formatBytes(decision.BudgetBytes))
	}

	failed := executeTrials(withRunID(ctx, res.RunID), o, &cfg, targets, wls, res, out)

	finishRun(res, resultsDir, ledger, out)

	path := resultPath(resultsDir, res)
	if err := writeResult(path, res); err != nil {
		return exitFailure, err
	}

	outf(out, "result: %v\n", path)

	if failed {
		return exitFailure, errors.New("one or more workloads failed; see the result document")
	}

	return exitOK, nil
}

// resolveRunConfig turns the command line into the recorded run configuration.
func resolveRunConfig(o *runOptions) (RunConfig, error) {
	switch o.order {
	case orderABBA, orderRandom, orderSequential:
	default:
		return RunConfig{}, errors.Errorf("unknown --order %q, want one of %v, %v, %v", o.order, orderABBA, orderRandom, orderSequential)
	}

	if o.uploadFraction <= 0 || o.uploadFraction > 1 {
		return RunConfig{}, errors.Errorf("--daily-upload-fraction must be in (0, 1], got %v", o.uploadFraction)
	}

	params := defaultParams()
	trials := o.trials

	if o.smoke {
		params = smokeParams()

		if !o.explicitlySet["trials"] {
			trials = smokeTrials
		}

		if !o.explicitlySet["cooldown"] {
			o.cooldown = 0
		}
	}

	if o.packSizeMB > 0 {
		params.PackSizeBytes = o.packSizeMB * mib
		params.RangeReadPackSize = o.packSizeMB * mib
	}

	if o.smallWriteN > 0 {
		params.SmallWriteN = o.smallWriteN
	}

	if o.deleteSweepN > 0 {
		params.DeleteSweepN = o.deleteSweepN
	}

	if o.parallelism > 0 {
		params.Parallelism = o.parallelism
	}

	if o.snapshotFiles > 0 {
		params.SnapshotFiles = o.snapshotFiles
	}

	if o.snapshotMB > 0 {
		params.SnapshotBytes = o.snapshotMB * mib
	}

	params.TrueCold = o.trueCold

	if trials < 1 {
		return RunConfig{}, errors.Errorf("--trials must be at least 1, got %v", trials)
	}

	if o.seed == 0 {
		o.seed = nowUTC().UnixNano()
	}

	return RunConfig{
		Trials:      trials,
		Order:       o.order,
		CooldownMS:  o.cooldown.Milliseconds(),
		Smoke:       o.smoke,
		Seed:        o.seed,
		KeepGoing:   o.keepGoing,
		TargetsFile: o.targetsFile,
		Params:      params,
	}, nil
}

// resolveTargets loads the targets file, or synthesizes the zero-configuration
// smoke target. The returned function removes anything the harness created.
func resolveTargets(o *runOptions) ([]target, func(), error) {
	noop := func() {}

	if o.targetsFile != "" {
		t, err := loadTargets(o.targetsFile, envLookup)

		return t, noop, err
	}

	if !o.smoke {
		return nil, noop, errors.New("--targets is required (only --smoke may run without one, against a temporary filesystem target)")
	}

	dir, err := os.MkdirTemp("", "gdrive-bench-smoke-")
	if err != nil {
		return nil, noop, errors.Wrap(err, "unable to create the temporary smoke target directory")
	}

	t, err := parseTargets([]byte(`[{"name":"filesystem-tmp","connection":{"type":"filesystem","config":{"path":"`+dir+`"}}}]`), envLookup)
	if err != nil {
		os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup on a path that cannot normally fail.

		return nil, noop, err
	}

	return t, func() { os.RemoveAll(dir) }, nil //nolint:errcheck // best-effort cleanup of our own temp dir.
}

// resolveKopiaExe makes a kopia binary available when the out-of-process
// workload was selected, building one from the current work tree when the
// operator did not name one. The returned function removes anything it built.
func resolveKopiaExe(ctx context.Context, o *runOptions, wls []workloadDef, cfg *RunConfig, out io.Writer) (func(), error) {
	noop := func() {}

	if !needsKopiaExe(wls) {
		return noop, nil
	}

	if o.kopiaExe != "" {
		abs, err := filepath.Abs(o.kopiaExe)
		if err != nil {
			return noop, errors.Wrapf(err, "unable to resolve --kopia-exe %v", o.kopiaExe)
		}

		if _, err := os.Stat(abs); err != nil {
			return noop, errors.Wrapf(err, "unable to use --kopia-exe %v", abs)
		}

		cfg.KopiaExe = abs

		return noop, nil
	}

	dir, err := os.MkdirTemp("", "gdrive-bench-kopia-")
	if err != nil {
		return noop, errors.Wrap(err, "unable to create the build directory for kopia")
	}

	cleanup := func() { os.RemoveAll(dir) } //nolint:errcheck // best-effort cleanup of our own temp dir.

	outf(out, "building kopia for the %v workload (pass --kopia-exe to skip)\n", snapshotE2EName)

	bctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()

	exe, err := buildKopia(bctx, dir)
	if err != nil {
		cleanup()

		return noop, err
	}

	cfg.KopiaExe = exe

	return cleanup, nil
}

// needsKopiaExe reports whether the selected workloads include one that drives
// the kopia binary.
func needsKopiaExe(wls []workloadDef) bool {
	for _, w := range wls {
		if w.Name == snapshotE2EName {
			return true
		}
	}

	return false
}

// selectWorkloads resolves the --workloads list.
func selectWorkloads(csv string) ([]workloadDef, error) {
	if csv == "" {
		return defaultWorkloads(), nil
	}

	out := []workloadDef{}

	for n := range strings.SplitSeq(csv, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}

		w, ok := workloadByName(n)
		if !ok {
			return nil, errors.Errorf("unknown workload %q, known workloads are %v", n, strings.Join(workloadNames(), ", "))
		}

		out = append(out, w)
	}

	if len(out) == 0 {
		return nil, errors.New("--workloads selected nothing")
	}

	return out, nil
}

// newRunResult builds the skeleton of the result document.
func newRunResult(ctx context.Context, o *runOptions, cfg *RunConfig, targets []target, wls []workloadDef) *RunResult {
	names := []string{}
	for _, w := range wls {
		names = append(names, w.Name)
	}

	cfg.Workloads = names

	tinfos := make([]TargetInfo, 0, len(targets))
	tnames := make([]string, 0, len(targets))

	for _, t := range targets {
		tinfos = append(tinfos, TargetInfo{Name: t.Name, StorageType: t.Conn.Type, Knobs: t.Knobs, Config: t.RedactedConfig})
		tnames = append(tnames, t.Name)
	}

	label := o.label
	if label == "" {
		label = strings.Join(tnames, "-")
	}

	return &RunResult{
		SchemaVersion: schemaVersion,
		RunID:         newRunID(),
		Label:         sanitizeLabel(label),
		StartedAt:     nowUTC(),
		Harness:       harnessInfo(ctx, o.harnessCommit),
		Config:        *cfg,
		Targets:       tinfos,
		Trials:        []TrialResult{},
		Errors:        []string{},
	}
}

// executeTrials runs the interleaved suite and reports whether anything failed.
func executeTrials(ctx context.Context, o *runOptions, cfg *RunConfig, targets []target, wls []workloadDef, res *RunResult, out io.Writer) bool {
	rnd := mathrand.New(mathrand.NewPCG(uint64(cfg.Seed), orderStreamSalt)) //nolint:gosec // reproducible benchmark ordering, not security.
	seq := 0
	failed := false

	for trial := range cfg.Trials {
		for pos, ti := range trialOrder(cfg.Order, len(targets), trial, rnd) {
			if seq > 0 && o.cooldown > 0 {
				outf(out, "cooldown %v\n", o.cooldown)

				if !clock.SleepInterruptibly(ctx, o.cooldown) {
					res.Errors = append(res.Errors, "interrupted during cooldown")

					return true
				}
			}

			tr, softErrs, hardErr := runSuite(ctx, targets[ti], wls, cfg, trial, seq, pos, out)
			res.Trials = append(res.Trials, tr)
			res.Errors = append(res.Errors, softErrs...)
			seq++

			if hardErr != nil {
				failed = true

				res.Errors = append(res.Errors, hardErr.Error())

				if !o.keepGoing {
					outf(out, "aborting: %v (pass --keep-going to continue)\n", hardErr)

					return true
				}
			}

			if ctx.Err() != nil {
				res.Errors = append(res.Errors, "interrupted")

				return true
			}
		}
	}

	return failed
}

// orderStreamSalt keeps the ordering random stream independent of the workload
// streams, so changing the workload set does not change the trial order.
const orderStreamSalt = 0x6f_72_64_65_72

// trialOrder returns the target indices to execute, in order, for one trial.
func trialOrder(order string, n, trial int, rnd *mathrand.Rand) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}

	switch order {
	case orderRandom:
		rnd.Shuffle(n, func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })

	case orderABBA:
		if trial%2 == 1 {
			for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
				idx[i], idx[j] = idx[j], idx[i]
			}
		}

	case orderSequential:
		// Configuration order, every trial. Debugging only.
	}

	return idx
}

// runSuite runs every selected workload once against one target.
func runSuite(ctx context.Context, t target, wls []workloadDef, cfg *RunConfig, trial, seq, pos int, out io.Writer) (TrialResult, []string, error) {
	tr := TrialResult{
		Trial:          trial,
		Sequence:       seq,
		Position:       pos,
		Target:         t.Name,
		StartedAt:      nowUTC(),
		Workloads:      map[string]WorkloadMetrics{},
		ErrorsByReason: map[string]int64{},
	}

	var soft []string

	for _, w := range wls {
		outf(out, "[trial %v/%v] %v %v ... ", trial+1, cfg.Trials, t.Name, w.Name)

		phases, softErrs, err := runOneWorkload(ctx, t, w, cfg, trial)
		soft = append(soft, softErrs...)

		for i := range phases {
			tr.Workloads[phases[i].Key] = phases[i].Metrics

			if phases[i].Metrics.Transport != nil {
				mergeErrorsByReason(tr.ErrorsByReason, phases[i].Metrics.Transport.ErrorsByReason)
			}
		}

		outln(out, summarizePhases(phases, err))

		if err != nil {
			return tr, soft, errors.Wrapf(err, "workload %v on target %v (trial %v)", w.Name, t.Name, trial+1)
		}
	}

	return tr, soft, nil
}

// runOneWorkload opens a connection for one workload, runs it, and always
// cleans up after it.
func runOneWorkload(ctx context.Context, t target, w workloadDef, cfg *RunConfig, trial int) (phases []phaseMetrics, soft []string, err error) {
	cacheDir, _ := t.fileIDCacheDir()

	env := &workloadEnv{
		params:   cfg.Params,
		prefix:   blob.ID(fmt.Sprintf("bench-%v-t%v-%v-", runIDOf(ctx), trial, w.Name)),
		rnd:      mathrand.New(mathrand.NewPCG(uint64(cfg.Seed), workloadSalt(w.Name, trial))), //nolint:gosec // reproducible benchmark data, not security.
		open:     t.openStorage,
		tgt:      t,
		cacheDir: cacheDir,
		kopiaExe: cfg.KopiaExe,
	}

	if err := env.openNew(ctx); err != nil {
		return nil, nil, err
	}

	// Cleanup runs even when the workload fails, and even when the run was
	// interrupted, which is why it gets a context that cannot be canceled.
	defer func() {
		cctx := context.WithoutCancel(ctx)

		if _, cerr := cleanupPrefix(cctx, env.storage(), env.prefix, cfg.Params.Parallelism); cerr != nil {
			soft = append(soft, fmt.Sprintf("cleanup of %v on %v: %v", env.prefix, t.Name, cerr))
		}

		if cerr := env.storage().Close(cctx); cerr != nil {
			soft = append(soft, fmt.Sprintf("closing %v: %v", t.Name, cerr))
		}
	}()

	phases, err = w.Run(ctx, env)

	return phases, soft, err
}

// workloadSalt derives a per-workload, per-trial random stream that does not
// depend on the target: every target of a trial therefore writes the same bytes
// and makes the same random choices.
func workloadSalt(name string, trial int) uint64 {
	const (
		fnvOffset uint64 = 14695981039346656037
		fnvPrime  uint64 = 1099511628211
	)

	h := fnvOffset
	for i := range len(name) {
		h = (h ^ uint64(name[i])) * fnvPrime
	}

	return h ^ uint64(uint32(trial)) //nolint:gosec // a trial index is small and non-negative; the truncation is intentional mixing.
}

// summarizePhases renders the one-line progress report of a finished workload.
func summarizePhases(phases []phaseMetrics, err error) string {
	if err != nil {
		return "FAILED: " + err.Error()
	}

	parts := []string{}

	for i := range phases {
		p := &phases[i]

		parts = append(parts, fmt.Sprintf("%v %.1fms (%v ops, p50 %.2fms)", p.Key, p.Metrics.WallClockMS, p.Metrics.Ops, p.Metrics.Latency.P50))
	}

	return strings.Join(parts, "; ")
}

// estimateRun predicts what a whole run will upload and spend, counting only
// the targets whose traffic actually leaves the machine.
func estimateRun(wls []workloadDef, p *benchParams, targets []target, trials int) workloadEstimate {
	var per workloadEstimate

	for _, w := range wls {
		per = per.plus(w.Estimate(p))
	}

	var total workloadEstimate

	for _, t := range targets {
		if !chargesQuota(t.Conn.Type) {
			continue
		}

		for range trials {
			total = total.plus(per)
		}
	}

	return total
}

// chargesQuota reports whether a storage type's traffic should be charged to
// the quota ledger. Everything but the local filesystem is assumed to leave the
// machine; that over-counts a non-Drive remote, which is the safe direction.
func chargesQuota(storageType string) bool {
	return storageType != filesystemStorageType
}

// finishRun fills in the totals, charges the ledger and prints the summary.
func finishRun(res *RunResult, resultsDir string, ledger *quotaLedger, out io.Writer) {
	res.FinishedAt = nowUTC()

	charged := map[string]bool{}
	for _, t := range res.Targets {
		charged[t.Name] = chargesQuota(t.StorageType)
	}

	var up, qu int64

	for _, tr := range res.Trials {
		if !charged[tr.Target] {
			continue
		}

		for key := range tr.Workloads {
			up += tr.Workloads[key].BytesUp
			qu += tr.Workloads[key].QuotaUnits
		}
	}

	res.Quota.ActualUploadBytes = up
	res.Quota.ActualQuotaUnits = qu

	if mins := res.FinishedAt.Sub(res.StartedAt).Minutes(); mins > 0 {
		res.Quota.ObservedQUPerMinute = float64(qu) / mins
	}

	if up > 0 || qu > 0 {
		ledger.add(nowUTC(), up, qu)

		if err := ledger.save(resultsDir); err != nil {
			res.Errors = append(res.Errors, "unable to update the quota ledger: "+err.Error())
		}
	}

	outf(out, "uploaded %v, %v quota units, ~%.0f QU/min (%.2f%% of the %v QU/min/user ceiling)\n",
		formatBytes(up), qu, res.Quota.ObservedQUPerMinute,
		percentOf(res.Quota.ObservedQUPerMinute, float64(perUserQUPerMinute)), perUserQUPerMinute)

	for _, e := range res.Errors {
		outf(out, "note: %v\n", e)
	}
}

// printQuotaPlan prints the pre-run quota lines: the enforced byte budget and
// the informational quota-unit rate.
func printQuotaPlan(out io.Writer, d quotaDecision) {
	outf(out, "quota: today %v of %v (%.0f%% of Google's 750 GB/day/user cap), this run adds ~%v\n",
		formatBytes(d.AlreadyBytes), formatBytes(d.BudgetBytes), d.Fraction*percentFull, formatBytes(d.EstimatedBytes))
	outf(out, "quota: ~%v quota units estimated; if they were all spent inside one minute that would be %.1f%% of the %v QU/min/user ceiling (informational - the QU limit is per minute, not per day)\n",
		d.EstimatedQU, percentOf(float64(d.EstimatedQU), float64(perUserQUPerMinute)), perUserQUPerMinute)
}

func percentOf(v, total float64) float64 {
	if total == 0 {
		return 0
	}

	return v / total * percentFull
}

// resultPath builds the result file name: <UTC timestamp>-<label>.json.
func resultPath(dir string, r *RunResult) string {
	return filepath.Join(dir, r.StartedAt.Format(resultTimestampLayout)+"-"+r.Label+".json")
}

// sanitizeLabel keeps a label usable as a file name.
func sanitizeLabel(s string) string {
	var sb strings.Builder

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}

	if sb.Len() == 0 {
		return "run"
	}

	return sb.String()
}

// newRunID returns a short, collision-resistant identifier for one run; it also
// namespaces every blob the run writes.
func newRunID() string {
	var b [runIDRandomBytes]byte

	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot realistically fail; fall back to the clock rather
		// than aborting a benchmark over it.
		return "r" + fmt.Sprintf("%06x", nowUTC().UnixNano()&math.MaxUint32)
	}

	return "r" + hex.EncodeToString(b[:])
}

// runIDKey types the run ID carried in the context.
type runIDKeyType struct{}

// withRunID puts the run ID in the context so blob prefixes can name it.
func withRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runIDKeyType{}, id)
}

// runIDOf reads the run ID back out of the context.
func runIDOf(ctx context.Context) string {
	if v, ok := ctx.Value(runIDKeyType{}).(string); ok {
		return v
	}

	return "unknown"
}

// formatBytes renders a byte count for humans, in decimal units - the same
// units Google states its 750 GB/day cap in, so the guard's messages and the
// documented limit can be compared directly.
func formatBytes(n int64) string {
	const unit = 1000

	if n < unit {
		return fmt.Sprintf("%v B", n)
	}

	div, exp := int64(unit), 0

	for v := n / unit; v >= unit && exp < len("kMGTP")-1; v /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %vB", float64(n)/float64(div), string("kMGTP"[exp]))
}
