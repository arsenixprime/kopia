//go:build !no_extra_providers

package main

import (
	"bytes"
	"context"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/timetrack"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/blob/gdrive"
	"github.com/kopia/kopia/repo/blob/rclone"
)

// snapshotE2EName is the registry name of the out-of-process workload.
const snapshotE2EName = "snapshot-e2e"

// Stage names. Each is reported as its own result key,
// "snapshot-e2e:<stage>", and each is timed around exactly one kopia
// invocation.
const (
	stageCreate      = "create"
	stageInit        = "init"
	stageIncremental = "incremental"
	stageVerify      = "verify"
	stageMaintenance = "maintenance"
)

// benchRepoPassword is the fixed repository password of every benchmark
// repository. The repositories are created, measured and deleted inside a
// single workload and never hold anything but synthetic random bytes, so the
// password is a constant rather than a secret.
const benchRepoPassword = "gdrive-bench"

// formatBlobID is the blob every kopia repository starts with. Its presence in
// the target folder is what the pre-flight check looks for.
const formatBlobID blob.ID = "kopia.repository"

// outputTailBytes is how much of a failed kopia invocation's output is kept in
// the result document.
const outputTailBytes = 4000

// Sizing constants of the synthetic source tree.
const (
	// minSyntheticFileBytes keeps every generated file large enough to be worth
	// a directory entry.
	minSyntheticFileBytes = 1024

	// maxSizeWeight is the ratio between the largest and the smallest file the
	// generator draws, before the sizes are scaled to the requested total. A
	// backup source is not uniform, and a workload made of 800 identical files
	// would measure something no real tree does.
	maxSizeWeight = 64

	// appendBytes is how much is appended to a file by the mutation step.
	appendBytes = 4096

	// appendPercent is the share of surviving files the mutation step appends
	// to, on top of the deletions and additions.
	appendPercent = 2

	// nestingStride puts every nth file one directory deeper, so the tree has
	// more than one level.
	nestingStride = 7
)

// estimateSnapshotE2E predicts the traffic of one execution.
//
// The uploader packs content into ~20 MB blobs, writes an index per session and
// re-reads everything during verify. The numbers are deliberately generous: the
// quota guard's job is to refuse a run that would be too big, and an estimate
// that is too small is the only failure mode that matters.
func estimateSnapshotE2E(p *benchParams) workloadEstimate {
	const (
		packBytes        = 20 * mib // the uploader's default pack blob size.
		metadataOverhead = 3        // index, manifest and directory blobs per pack, generously.
		maintenanceEdits = 20       // compaction rewrites a handful of index blobs.

		// mutationDirections counts the two ways the mutation step moves bytes:
		// the files it adds and the files it appends to.
		mutationDirections = 2
	)

	// Deletions and additions both move bytes, hence twice the mutation share.
	changed := p.SnapshotBytes * int64(mutationDirections*p.SnapshotMutatePercent) / percentDivisor
	total := p.SnapshotBytes + changed

	packs := total/packBytes + 1
	blobs := packs * metadataOverhead

	return workloadEstimate{
		UploadBytes: total,
		QuotaUnits: packs*quWrite(packBytes) +
			blobs*quWrite(int64(p.SmallBlobMinBytes)) +
			packs*quDownloadItem + // verify downloads every pack again.
			maintenanceEdits*quEditItem +
			quCleanup(int(packs+blobs)),
	}
}

// runSnapshotE2E drives a real kopia binary through a complete backup cycle
// against the target, timing each stage separately.
//
// Unlike every other workload this one measures kopia, not blob.Storage: it
// exercises the read-your-writes, listing and concurrency behavior that only
// shows up when the repository layer is on top. The price is that no transport
// delta can be collected - the Drive counters live in the child process - so
// the API-call profile has to come from the in-process workloads.
func runSnapshotE2E(ctx context.Context, e *workloadEnv) (phases []phaseMetrics, err error) {
	if e.kopiaExe == "" {
		return nil, errors.New("snapshot-e2e needs a kopia binary: pass --kopia-exe (or let the harness build one)")
	}

	st := e.storage()

	if err := requireNoRepository(ctx, st); err != nil {
		return nil, err
	}

	before, err := blobIDSet(ctx, st)
	if err != nil {
		return nil, err
	}

	// Everything this workload writes goes into the target's own folder, so
	// cleanup is "delete what appeared". It is deferred and uncancellable for
	// the same reason the runner's prefix cleanup is: an interrupted run must
	// not leave a repository behind that fails the next run's pre-flight check.
	defer func() {
		if _, cerr := cleanupNewBlobs(context.WithoutCancel(ctx), st, before, e.params.Parallelism); cerr != nil && err == nil {
			err = cerr
		}
	}()

	root, err := os.MkdirTemp("", "gdrive-bench-e2e-")
	if err != nil {
		return nil, errors.Wrap(err, "unable to create the snapshot-e2e working directory")
	}

	defer os.RemoveAll(root) //nolint:errcheck // best-effort cleanup of our own temp tree.

	source := filepath.Join(root, "source")

	tree, err := generateTree(source, &e.params, e.rnd)
	if err != nil {
		return nil, err
	}

	k := newKopiaRunner(e, root)

	createArgs, err := repositoryCreateArgs(e.tgt, root)
	if err != nil {
		return nil, err
	}

	stages, err := k.runStages(ctx,
		stageSpec{stageCreate, 0, 0, createArgs},
		stageSpec{stageInit, tree.bytes, 0, snapshotCreateArgs(source)},
	)

	phases = append(phases, stages...)

	if err != nil {
		return phases, err
	}

	changed, err := mutateTree(source, tree, &e.params, e.rnd)
	if err != nil {
		return phases, err
	}

	stages, err = k.runStages(ctx,
		stageSpec{stageIncremental, changed, 0, snapshotCreateArgs(source)},
		stageSpec{stageVerify, 0, tree.bytes, []string{"snapshot", "verify", "--verify-files-percent=100"}},
		// safety=none is what makes this a maintenance measurement rather than a
		// no-op: the default full safety refuses to touch blobs younger than its
		// retention margins, and every blob in a repository created seconds ago
		// is younger than that.
		stageSpec{stageMaintenance, 0, 0, []string{"maintenance", "run", "--full", "--safety=none"}},
	)

	phases = append(phases, stages...)

	if err != nil {
		return phases, err
	}

	return phases, nil
}

// requireNoRepository fails fast when the target folder already holds a kopia
// repository. Cleanup deletes every blob the workload created, and a repository
// that was there first would be indistinguishable from one of them.
func requireNoRepository(ctx context.Context, st blob.Storage) error {
	_, err := st.GetMetadata(ctx, formatBlobID)

	switch {
	case err == nil:
		return errors.Errorf("the target already holds a kopia repository (%v exists): snapshot-e2e creates and then deletes a repository "+
			"in the target's own folder, so it refuses to run where one is already present", formatBlobID)
	case errors.Is(err, blob.ErrBlobNotFound):
		return nil
	default:
		return errors.Wrapf(err, "unable to check the target for an existing %v blob", formatBlobID)
	}
}

// blobIDSet lists every blob the target currently holds.
func blobIDSet(ctx context.Context, st blob.Storage) (map[blob.ID]bool, error) {
	out := map[blob.ID]bool{}

	if err := st.ListBlobs(ctx, "", func(bm blob.Metadata) error {
		out[bm.BlobID] = true

		return nil
	}); err != nil {
		return nil, errors.Wrap(err, "unable to list the target's blobs")
	}

	return out, nil
}

// cleanupNewBlobs deletes every blob that was not there before. Deleting the
// difference rather than a prefix is what lets the workload create a real
// repository - kopia's blob names are its own, and nothing the harness can
// namespace.
func cleanupNewBlobs(ctx context.Context, st blob.Storage, before map[blob.ID]bool, parallelism int) (int, error) {
	after, err := blobIDSet(ctx, st)
	if err != nil {
		return 0, err
	}

	var victims []blob.ID

	for id := range after {
		if !before[id] {
			victims = append(victims, id)
		}
	}

	slices.Sort(victims)

	var firstErr error

	for _, r := range deleteParallel(ctx, st, victims, parallelism) {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}

	if firstErr != nil {
		return len(victims), errors.Wrapf(firstErr, "unable to delete some of the %v blobs the snapshot cycle created", len(victims))
	}

	return len(victims), nil
}

// ---------------------------------------------------------------------------
// driving the kopia binary
// ---------------------------------------------------------------------------

// kopiaRunner invokes the kopia binary with a private configuration file, cache
// directory and log directory, so that a bench run cannot see - or disturb -
// the operator's own repositories.
type kopiaRunner struct {
	exe        string
	configPath string
	cacheDir   string
	logDir     string
	env        []string

	// chargesQuota records whether this target's traffic is Drive traffic, which
	// decides whether the stage reports estimated quota units or none at all.
	chargesQuota bool
}

func newKopiaRunner(e *workloadEnv, root string) *kopiaRunner {
	return &kopiaRunner{
		exe:        e.kopiaExe,
		configPath: filepath.Join(root, "repository.config"),
		cacheDir:   filepath.Join(root, "kopia-cache"),
		logDir:     filepath.Join(root, "logs"),
		env: append(os.Environ(),
			"KOPIA_PASSWORD="+benchRepoPassword,
			"KOPIA_CHECK_FOR_UPDATES=false",
			// The child's own Drive file-ID journal and any other platform cache
			// land inside the run's temp tree rather than in the operator's
			// ~/.cache, so consecutive trials start from the same state.
			"XDG_CACHE_HOME="+filepath.Join(root, "xdg-cache"),
		),
		chargesQuota: chargesQuota(e.tgt.Conn.Type),
	}
}

// stageSpec is one kopia invocation and the bytes it is expected to move.
type stageSpec struct {
	name string
	up   int64
	down int64
	args []string
}

// runStages runs a sequence of stages, stopping at the first failure and
// returning the metrics of everything it did run - a half-finished cycle is
// still worth recording.
func (k *kopiaRunner) runStages(ctx context.Context, specs ...stageSpec) ([]phaseMetrics, error) {
	out := make([]phaseMetrics, 0, len(specs))

	for _, s := range specs {
		m, err := k.stage(ctx, s.up, s.down, s.args...)
		out = append(out, phaseMetrics{Key: phaseKey(snapshotE2EName, s.name), Metrics: m})

		if err != nil {
			return out, err
		}
	}

	return out, nil
}

// stage runs one kopia invocation and turns it into a result key's metrics. The
// stage is a single operation, so its wall clock and its one latency sample are
// the same number; ops_per_sec is therefore meaningless here and the comparison
// should be read on wall_clock_ms.
func (k *kopiaRunner) stage(ctx context.Context, up, down int64, args ...string) (WorkloadMetrics, error) {
	t := timetrack.StartTimer()
	cpu := startCPUMeter()
	out, state, err := k.run(ctx, args...)
	elapsed := t.Elapsed()
	sample := cpu.stop().withProcessState(state)

	ms := float64(elapsed.Nanoseconds()) / millisPerNanoFloat

	m := WorkloadMetrics{
		WallClockMS: ms,
		Ops:         1,
		Samples:     1,
		Latency:     Percentiles{P10: ms, P50: ms, P90: ms, P99: ms},
		// Bytes are the source tree's size, not a byte counter: the transfer
		// happens in another process. Deduplication and compression make the
		// real figure smaller, which is the safe direction for the quota ledger.
		BytesUp:   up,
		BytesDown: down,
	}

	sample.applyTo(&m)

	if k.chargesQuota {
		m.QuotaUnits = estimateStageQuotaUnits(up, down)
		m.QuotaUnitsSource = quotaSourceEstimated
	} else {
		m.QuotaUnitsSource = quotaSourceNone
	}

	if err != nil {
		m.Errors = 1
		m.Error = err.Error()

		return m, errors.Wrapf(err, "kopia %v failed; last output:\n%v", strings.Join(args, " "), tailOf(out, outputTailBytes))
	}

	return m, nil
}

// run invokes kopia with the harness's global flags prepended. It returns the
// child's ProcessState alongside its output, because that is where the stage's
// exact child CPU comes from: on Linux the kernel fills wait4's rusage with the
// dead child's SELF **and** CHILDREN times, so a kopia that spawned and reaped
// an rclone of its own reports both.
func (k *kopiaRunner) run(ctx context.Context, args ...string) (string, *os.ProcessState, error) {
	full := append([]string{
		"--config-file=" + k.configPath,
		"--log-dir=" + k.logDir,
	}, args...)

	cmd := exec.CommandContext(ctx, k.exe, full...) //nolint:gosec // the executable is the operator's own --kopia-exe.
	cmd.Env = k.env

	var buf bytes.Buffer

	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()

	return buf.String(), cmd.ProcessState, err
}

// estimateStageQuotaUnits converts a stage's byte movement into Drive quota
// units, on the same assumptions the pre-run estimate uses: uploads are
// edit-class requests of one pack blob each, downloads are download-class.
func estimateStageQuotaUnits(up, down int64) int64 {
	const packBytes = 20 * mib

	var qu int64

	if up > 0 {
		qu += (up/packBytes + 1) * quWrite(packBytes)
	}

	if down > 0 {
		qu += (down/packBytes + 1) * quDownloadItem
	}

	return qu
}

// tailOf returns the last n bytes of s, prefixed with an ellipsis when it had
// to cut.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return "...\n" + s[len(s)-n:]
}

// snapshotCreateArgs builds the "snapshot create" command line for source.
func snapshotCreateArgs(source string) []string {
	return []string{"snapshot", "create", source}
}

// repositoryCreateArgs builds the "repository create" command line for a
// target, from the connection the targets file configured. Every provider the
// harness can benchmark needs an entry here, because the kopia CLI takes flags
// where the harness takes a JSON config.
func repositoryCreateArgs(t target, root string) ([]string, error) {
	head := []string{
		"repository", "create",
		"--cache-directory=" + filepath.Join(root, "kopia-cache"),
		"--no-check-for-updates",
	}

	switch cfg := t.Conn.Config.(type) {
	case *gdrive.Options:
		args := []string{"gdrive", "--folder-id=" + cfg.FolderID}

		if cfg.ServiceAccountCredentialsFile != "" {
			args = append(args, "--credentials-file="+cfg.ServiceAccountCredentialsFile)
		}

		if cfg.Scope != "" {
			args = append(args, "--scope="+cfg.Scope)
		}

		return append(head, args...), nil

	case *rclone.Options:
		args := []string{"rclone", "--remote-path=" + cfg.RemotePath}

		if cfg.RCloneExe != "" {
			args = append(args, "--rclone-exe="+cfg.RCloneExe)
		}

		for _, a := range cfg.RCloneArgs {
			args = append(args, "--rclone-args="+a)
		}

		for _, a := range cfg.RCloneEnv {
			args = append(args, "--rclone-env="+a)
		}

		return append(head, args...), nil

	case *filesystem.Options:
		return append(head, "filesystem", "--path="+cfg.Path), nil

	default:
		return nil, errors.Errorf("snapshot-e2e cannot build a kopia command line for storage type %q; add it to repositoryCreateArgs", t.Conn.Type)
	}
}

// ---------------------------------------------------------------------------
// the synthetic source tree
// ---------------------------------------------------------------------------

// sourceTree is the generated tree the snapshot stages back up.
type sourceTree struct {
	// files are paths relative to the tree root, in creation order.
	files []string

	// bytes is the total size of the tree.
	bytes int64
}

// generateTree writes a deterministic synthetic backup source below root.
//
// Sizes are drawn from the workload's seeded generator and scaled to hit the
// requested total, so every target of a trial backs up byte-identical content;
// the content itself is pseudo-random, which keeps deduplication and
// compression from turning a 150 MB tree into a 2 MB upload and making the
// measurement about the splitter rather than about the storage.
func generateTree(root string, p *benchParams, rnd *rand.Rand) (*sourceTree, error) {
	count := p.SnapshotFiles
	if count < 1 {
		return nil, errors.Errorf("snapshot-e2e needs at least one file, got %v", count)
	}

	dirs := max(p.SnapshotDirs, 1)

	sizes := drawSizes(count, p.SnapshotBytes, rnd)

	t := &sourceTree{}

	for i, size := range sizes {
		rel := treePath(i, dirs)

		if err := writeSyntheticFile(filepath.Join(root, rel), size, rnd); err != nil {
			return nil, err
		}

		t.files = append(t.files, rel)
		t.bytes += size
	}

	return t, nil
}

// treePath places file i in the tree: round-robin over the top-level
// directories, with every nth file one level deeper.
func treePath(i, dirs int) string {
	dir := "d" + strconv.Itoa(i%dirs)

	if i%nestingStride == 0 {
		return filepath.Join(dir, "nested", "f"+pad(i)+".bin")
	}

	return filepath.Join(dir, "f"+pad(i)+".bin")
}

// drawSizes returns count file sizes that sum to approximately total, spread
// over a heavy-tailed distribution rather than a uniform one.
func drawSizes(count int, total int64, rnd *rand.Rand) []int64 {
	weights := make([]float64, count)

	var sum float64

	for i := range weights {
		// A power of a uniform draw: many small files, a few big ones.
		w := 1 + rnd.Float64()*rnd.Float64()*maxSizeWeight
		weights[i] = w
		sum += w
	}

	out := make([]int64, count)

	for i, w := range weights {
		out[i] = max(int64(float64(total)*w/sum), minSyntheticFileBytes)
	}

	return out
}

// writeSyntheticFile creates one file of pseudo-random bytes.
func writeSyntheticFile(path string, size int64, rnd *rand.Rand) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPermissions); err != nil {
		return errors.Wrapf(err, "unable to create the source directory for %v", path)
	}

	data := randomChunk(size, rnd)

	if err := os.WriteFile(path, data, sourceFilePermissions); err != nil {
		return errors.Wrapf(err, "unable to write the source file %v", path)
	}

	return nil
}

// sourceFilePermissions is the mode of a generated source file.
const sourceFilePermissions = 0o600

// bytesPerDraw is how many bytes one draw of the generator yields, and
// bitsPerByte is what it takes to slice them out.
const (
	bytesPerDraw = 8
	bitsPerByte  = 8
)

// randomChunk returns n deterministic pseudo-random bytes.
func randomChunk(n int64, rnd *rand.Rand) []byte {
	buf := make([]byte, n)

	for i := int64(0); i < n; i += bytesPerDraw {
		v := rnd.Uint64()

		for j := range int64(bytesPerDraw) {
			if i+j < n {
				buf[i+j] = byte(v >> (bitsPerByte * j)) //nolint:gosec // truncation is intentional: this extracts byte j of v.
			}
		}
	}

	return buf
}

// mutateTree performs the change between the initial and the incremental
// snapshot: it deletes a percentage of the files, adds the same percentage of
// new ones, and appends to a smaller share of the survivors. It returns the
// number of bytes that are new, which is what the incremental stage reports as
// its upload.
func mutateTree(root string, t *sourceTree, p *benchParams, rnd *rand.Rand) (int64, error) {
	pct := max(p.SnapshotMutatePercent, 0)

	n := len(t.files)
	victims := atLeastOne(n*pct/percentDivisor, pct > 0)
	appends := atLeastOne(n*appendPercent/percentDivisor, true)

	if victims+appends > n {
		return 0, errors.Errorf("the snapshot-e2e tree of %v files is too small to mutate %v%% of it", n, pct)
	}

	order := rnd.Perm(n)

	// Deletions come off the front of the shuffled order, appends off the back,
	// so the two sets never overlap.
	for _, i := range order[:victims] {
		if err := os.Remove(filepath.Join(root, t.files[i])); err != nil {
			return 0, errors.Wrapf(err, "unable to delete %v during the mutation step", t.files[i])
		}
	}

	var added int64

	avg := t.bytes / int64(n)

	for i := range victims {
		rel := filepath.Join("d0", "added-"+pad(i)+".bin")

		if err := writeSyntheticFile(filepath.Join(root, rel), avg, rnd); err != nil {
			return 0, err
		}

		t.files = append(t.files, rel)
		added += avg
	}

	for _, i := range order[n-appends:] {
		if err := appendToFile(filepath.Join(root, t.files[i]), rnd); err != nil {
			return 0, err
		}

		added += appendBytes
	}

	return added, nil
}

// atLeastOne keeps a percentage of a small file count from rounding down to
// nothing: an incremental snapshot of an unchanged tree measures the scan, not
// the upload, and would silently stop being an incremental-write benchmark.
func atLeastOne(v int, wanted bool) int {
	if wanted && v < 1 {
		return 1
	}

	return v
}

// appendToFile grows one file by appendBytes.
func appendToFile(path string, rnd *rand.Rand) error {
	//nolint:gosec // the path is one this workload generated inside its own temp tree.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, sourceFilePermissions)
	if err != nil {
		return errors.Wrapf(err, "unable to open %v for appending", path)
	}

	defer f.Close() //nolint:errcheck // the write error below is the one that matters.

	if _, err := f.Write(randomChunk(appendBytes, rnd)); err != nil {
		return errors.Wrapf(err, "unable to append to %v", path)
	}

	return f.Close() //nolint:wrapcheck // an os.File close error needs no more context than its own message.
}

// buildKopia builds the kopia binary of the current work tree into dir. It is
// what --kopia-exe defaults to, so that the measured binary is always the code
// the result document names in harness.commit.
func buildKopia(ctx context.Context, dir string) (string, error) {
	root, ok := repoRoot(ctx)
	if !ok {
		return "", errors.New("snapshot-e2e needs a kopia binary and this is not a git work tree: pass --kopia-exe")
	}

	exe := filepath.Join(dir, "kopia-bench")

	cmd := exec.CommandContext(ctx, "go", "build", "-o", exe, ".") //nolint:gosec // fixed argv; exe is a path the harness just created.
	cmd.Dir = root

	if out, err := cmd.CombinedOutput(); err != nil {
		return "", errors.Wrapf(err, "unable to build kopia in %v: %v", root, tailOf(string(out), outputTailBytes))
	}

	return exe, nil
}

// buildTimeout bounds the on-demand kopia build.
const buildTimeout = 10 * time.Minute
