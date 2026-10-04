//go:build !no_extra_providers

package main

import (
	"context"
	"encoding/binary"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/internal/timetrack"
	"github.com/kopia/kopia/repo/blob"
)

// benchParams are the workload sizes. They are recorded verbatim in every
// result document, because a number is only comparable against another number
// produced with the same parameters.
type benchParams struct {
	SmallWriteN       int   `json:"small_write_n"`
	SmallBlobMinBytes int   `json:"small_blob_min_bytes"`
	SmallBlobMaxBytes int   `json:"small_blob_max_bytes"`
	PackWriteM        int   `json:"pack_write_m"`
	PackSizeBytes     int64 `json:"pack_size_bytes"`
	ReadMetadataN     int   `json:"read_metadata_n"`
	RangeReadR        int   `json:"range_read_r"`
	RangeReadPacks    int   `json:"range_read_packs"`
	RangeReadPackSize int64 `json:"range_read_pack_size_bytes"`
	RangeReadMinBytes int64 `json:"range_read_min_bytes"`
	RangeReadMaxBytes int64 `json:"range_read_max_bytes"`
	ListTraversals    int   `json:"list_traversals"`
	ListBlobsN        int   `json:"list_blobs_n"`
	DeleteSweepN      int   `json:"delete_sweep_n"`
	DeletePercent     int   `json:"delete_percent"`
	Parallelism       int   `json:"parallelism"`

	// SnapshotFiles, SnapshotDirs, SnapshotBytes and SnapshotMutatePercent size
	// the synthetic source tree of the out-of-process snapshot-e2e workload.
	SnapshotFiles         int   `json:"snapshot_files"`
	SnapshotDirs          int   `json:"snapshot_dirs"`
	SnapshotBytes         int64 `json:"snapshot_total_bytes"`
	SnapshotMutatePercent int   `json:"snapshot_mutate_percent"`

	// TrueCold makes the cold pass of read-metadata delete the on-disk file-ID
	// journal before it reopens the storage, instead of only dropping the
	// in-memory map. It changes what "read-metadata:cold" means, which is why it
	// lives in the params: two results are only comparable when it agrees.
	TrueCold bool `json:"true_cold"`
}

// Workload registry names. Each is also the result key, or the prefix of the
// result keys of its phases.
const (
	workloadSmallWrite   = "small-write"
	workloadPackWrite    = "pack-write"
	workloadReadMetadata = "read-metadata"
	workloadRangeRead    = "range-read"
	workloadListHeavy    = "list-heavy"
	workloadDeleteSweep  = "delete-sweep"
)

// Default workload sizes.
const (
	mib int64 = 1 << 20

	defaultSmallWriteN       = 100
	defaultSmallBlobMinBytes = 2 * 1024
	defaultSmallBlobMaxBytes = 8 * 1024
	defaultPackWriteM        = 5
	defaultPackSizeMB        = 20
	defaultReadMetadataN     = 50
	defaultRangeReadR        = 50
	defaultRangeReadPacks    = 2
	defaultRangeReadMinBytes = 4 * 1024
	defaultRangeReadMaxBytes = 256 * 1024
	defaultListTraversals    = 5
	defaultListBlobsN        = 40
	defaultDeleteSweepN      = 100
	defaultDeletePercent     = 50
	defaultParallelism       = 8

	defaultSnapshotFiles         = 800
	defaultSnapshotDirs          = 8
	defaultSnapshotMB            = 150
	defaultSnapshotMutatePercent = 5
)

// Smoke-mode workload sizes. pack-write deliberately keeps the full 20 MB
// object: it is the only workload whose point is the large-upload path, and a
// smoke run that never exercises a resumable upload would not be a smoke test
// of it. Everything else shrinks to single digits.
const (
	smokeSmallWriteN       = 10
	smokePackWriteM        = 1
	smokeReadMetadataN     = 5
	smokeRangeReadR        = 5
	smokeRangeReadPacks    = 1
	smokeRangeReadPackSize = 4 * mib
	smokeListTraversals    = 2
	smokeListBlobsN        = 5
	smokeDeleteSweepN      = 10
	smokeSnapshotFiles     = 40
	smokeSnapshotDirs      = 3
	smokeSnapshotMB        = 6
)

// defaultParams returns the full-size workload parameters.
func defaultParams() benchParams {
	return benchParams{
		SmallWriteN:       defaultSmallWriteN,
		SmallBlobMinBytes: defaultSmallBlobMinBytes,
		SmallBlobMaxBytes: defaultSmallBlobMaxBytes,
		PackWriteM:        defaultPackWriteM,
		PackSizeBytes:     defaultPackSizeMB * mib,
		ReadMetadataN:     defaultReadMetadataN,
		RangeReadR:        defaultRangeReadR,
		RangeReadPacks:    defaultRangeReadPacks,
		RangeReadPackSize: defaultPackSizeMB * mib,
		RangeReadMinBytes: defaultRangeReadMinBytes,
		RangeReadMaxBytes: defaultRangeReadMaxBytes,
		ListTraversals:    defaultListTraversals,
		ListBlobsN:        defaultListBlobsN,
		DeleteSweepN:      defaultDeleteSweepN,
		DeletePercent:     defaultDeletePercent,
		Parallelism:       defaultParallelism,

		SnapshotFiles:         defaultSnapshotFiles,
		SnapshotDirs:          defaultSnapshotDirs,
		SnapshotBytes:         defaultSnapshotMB * mib,
		SnapshotMutatePercent: defaultSnapshotMutatePercent,
	}
}

// smokeParams shrinks every workload to the smallest size that still exercises
// its code path.
func smokeParams() benchParams {
	p := defaultParams()
	p.SmallWriteN = smokeSmallWriteN
	p.PackWriteM = smokePackWriteM
	p.ReadMetadataN = smokeReadMetadataN
	p.RangeReadR = smokeRangeReadR
	p.RangeReadPacks = smokeRangeReadPacks
	p.RangeReadPackSize = smokeRangeReadPackSize
	p.ListTraversals = smokeListTraversals
	p.ListBlobsN = smokeListBlobsN
	p.DeleteSweepN = smokeDeleteSweepN
	p.SnapshotFiles = smokeSnapshotFiles
	p.SnapshotDirs = smokeSnapshotDirs
	p.SnapshotBytes = smokeSnapshotMB * mib

	return p
}

// workloadEstimate is a workload's predicted cost, used by the quota guard
// before anything is written.
type workloadEstimate struct {
	UploadBytes int64
	QuotaUnits  int64
}

func (e workloadEstimate) plus(o workloadEstimate) workloadEstimate {
	return workloadEstimate{UploadBytes: e.UploadBytes + o.UploadBytes, QuotaUnits: e.QuotaUnits + o.QuotaUnits}
}

// phaseMetrics is one result key produced by a workload. Most workloads produce
// exactly one; the ones with a hot/cold or full/prefixed split produce two.
type phaseMetrics struct {
	Key     string
	Metrics WorkloadMetrics
}

// workloadDef is one entry of the registry. Adding a workload - including the
// out-of-process snapshot workload of WP-3b - means appending to the registry;
// the result schema does not change, because workload names are strings and the
// metrics are a per-workload map.
type workloadDef struct {
	// Name is the registry name, selectable through --workloads.
	Name string

	// Doc is the one-line description printed by "run --help" and "knobs".
	Doc string

	// OptIn keeps a workload out of the default suite: it still runs when
	// --workloads names it, but "everything" does not include it. snapshot-e2e
	// is opt-in because it drives a separate kopia binary, which a plumbing
	// smoke run has no business building.
	OptIn bool

	// Estimate predicts the cost of one execution, for the quota guard.
	Estimate func(p *benchParams) workloadEstimate

	// Run performs setup, measurement and (through the runner's deferred
	// cleanup) teardown of one execution.
	Run func(ctx context.Context, e *workloadEnv) ([]phaseMetrics, error)
}

// allWorkloads is the registry, in execution order.
func allWorkloads() []workloadDef {
	return []workloadDef{
		{
			Name:     workloadSmallWrite,
			Doc:      "N small index-like blobs, one at a time - per-operation write overhead",
			Estimate: estimateSmallWrite,
			Run:      runSmallWrite,
		},
		{
			Name:     workloadPackWrite,
			Doc:      "M large pack blobs - upload throughput and the resumable-upload path",
			Estimate: estimatePackWrite,
			Run:      runPackWrite,
		},
		{
			Name:     workloadReadMetadata,
			Doc:      "GetMetadata over a written set, with a hot and a cold file-ID cache",
			Estimate: estimateReadMetadata,
			Run:      runReadMetadata,
		},
		{
			Name:     workloadRangeRead,
			Doc:      "R seeded random range reads from pack blobs",
			Estimate: estimateRangeRead,
			Run:      runRangeRead,
		},
		{
			Name:     workloadListHeavy,
			Doc:      "repeated whole-storage and prefixed ListBlobs traversals",
			Estimate: estimateListHeavy,
			Run:      runListHeavy,
		},
		{
			Name:     workloadDeleteSweep,
			Doc:      "delete a percentage of a written set with bounded parallelism",
			Estimate: estimateDeleteSweep,
			Run:      runDeleteSweep,
		},
		{
			Name:     snapshotE2EName,
			Doc:      "the real kopia binary: repository create, initial snapshot, incremental, verify, full maintenance (opt-in, needs --kopia-exe)",
			OptIn:    true,
			Estimate: estimateSnapshotE2E,
			Run:      runSnapshotE2E,
		},
	}
}

// defaultWorkloads is what "--workloads" selects when it is not given: every
// registered workload that is not opt-in.
func defaultWorkloads() []workloadDef {
	out := []workloadDef{}

	for _, w := range allWorkloads() {
		if !w.OptIn {
			out = append(out, w)
		}
	}

	return out
}

// workloadByName looks a workload up in the registry.
func workloadByName(name string) (workloadDef, bool) {
	for _, w := range allWorkloads() {
		if w.Name == name {
			return w, true
		}
	}

	return workloadDef{}, false
}

// workloadNames returns every registered workload name, in registry order.
func workloadNames() []string {
	out := []string{}

	for _, w := range allWorkloads() {
		out = append(out, w.Name)
	}

	return out
}

// defaultWorkloadNames returns the names of the default suite.
func defaultWorkloadNames() []string {
	out := []string{}

	for _, w := range defaultWorkloads() {
		out = append(out, w.Name)
	}

	return out
}

// workloadEnv is what a workload gets to work with.
type workloadEnv struct {
	params benchParams

	// prefix namespaces every blob this execution creates, so cleanup can find
	// them all and two concurrent runs cannot collide.
	prefix blob.ID

	// rnd is seeded per (run, trial, target, workload), so a workload makes the
	// same random choices for every target of a run.
	rnd *rand.Rand

	open func(ctx context.Context) (blob.Storage, error)
	cur  blob.Storage

	// tgt is the target under measurement. Only the out-of-process workload
	// needs it: it builds a kopia command line out of the connection.
	tgt target

	// cacheDir is the target's harness-owned local cache directory, or "" when
	// the target has no purgeable on-disk cache. recycleCold empties the file-ID
	// journal underneath it.
	cacheDir string

	// kopiaExe is the kopia binary the out-of-process workload drives.
	kopiaExe string
}

// storage returns the connection the workload is currently using.
func (e *workloadEnv) storage() blob.Storage {
	return e.cur
}

// recycle closes the current connection and opens a fresh one, WITHOUT asking
// the backend to drop anything. It is how the WARM pass drops the backend's
// in-memory state and only that.
//
// This is honest only up to a point, and only for the native backend: the
// process-wide file-ID cache is refcounted, so dropping the last reference does
// discard the in-memory map, but the on-disk journal survives - as it does in
// production, where a process restart still finds the journal. recycleFlushed
// and recycleCold are the cold variants.
func (e *workloadEnv) recycle(ctx context.Context) error {
	if err := e.closeCurrent(ctx, false); err != nil {
		return err
	}

	return e.openNew(ctx)
}

// recycleFlushed is recycle plus a FlushCaches, which the blob.Storage contract
// defines as a full wipe of the backend's local caches - for the native Drive
// backend, the persisted file-ID journal as well as the in-memory map. It is how
// the cold pass of a run WITHOUT --true-cold is made cold: there is no
// harness-owned cache directory to delete in that mode, so the backend has to be
// asked to forget by itself.
func (e *workloadEnv) recycleFlushed(ctx context.Context) error {
	if err := e.closeCurrent(ctx, true); err != nil {
		return err
	}

	return e.openNew(ctx)
}

// recycleCold is recycle plus the deletion of the on-disk file-ID journal, so
// the reopened connection has to resolve every blob name against Drive again.
// It is what a first-ever run on a new machine experiences.
//
// The journal is only deleted underneath the target's own configured cache
// directory, which the harness requires to be one it owns (see
// validateTrueCold): a bench run must never be able to delete the cache of a
// real repository.
func (e *workloadEnv) recycleCold(ctx context.Context) error {
	if err := e.closeCurrent(ctx, true); err != nil {
		return err
	}

	if err := purgeFileIDJournal(e.cacheDir); err != nil {
		return err
	}

	return e.openNew(ctx)
}

// gdriveFileIDCacheSubdir is the directory the native backend keeps its
// blobID-to-fileID journal in, relative to Options.Tuning.CacheDir. It is an
// unexported constant over in repo/blob/gdrive; the name is duplicated here
// deliberately, so that purging a cache never depends on unexported state - and
// so that a rename over there leaves this harness purging nothing rather than
// deleting something else. TestPurgeFileIDJournal pins the layout.
const gdriveFileIDCacheSubdir = "gdrive-fileids"

// purgeFileIDJournal removes the file-ID journal tree under a cache directory.
// An empty cacheDir means the target has no purgeable on-disk cache (every
// backend that is not the native Drive one), where a cold pass can only ever be
// a fresh HTTP connection.
func purgeFileIDJournal(cacheDir string) error {
	if cacheDir == "" {
		return nil
	}

	dir := filepath.Join(cacheDir, gdriveFileIDCacheSubdir)
	if err := os.RemoveAll(dir); err != nil {
		return errors.Wrapf(err, "unable to purge the file ID journal at %v", dir)
	}

	return nil
}

// closeCurrent drops the current connection. dropCaches first asks the backend
// to discard its local caches, which blob.Storage.FlushCaches defines as a full
// wipe: the native Drive backend deletes its persisted file-ID journal there, so
// a connection opened afterwards starts from nothing. Only a pass that wants to
// be cold may ask for it - the warm pass exists precisely to measure the reopen
// that still finds the journal.
func (e *workloadEnv) closeCurrent(ctx context.Context, dropCaches bool) error {
	if e.cur == nil {
		return nil
	}

	if dropCaches {
		if err := e.cur.FlushCaches(ctx); err != nil {
			return errors.Wrap(err, "unable to flush the storage caches")
		}
	}

	if err := e.cur.Close(ctx); err != nil {
		return errors.Wrap(err, "unable to close the storage")
	}

	e.cur = nil

	return nil
}

// openNew opens a fresh connection to the target.
func (e *workloadEnv) openNew(ctx context.Context) error {
	st, err := e.open(ctx)
	if err != nil {
		return err
	}

	e.cur = st

	return nil
}

// blobID builds a namespaced blob ID.
func (e *workloadEnv) blobID(kind string, i int) blob.ID {
	return e.prefix + blob.ID(kind) + blob.ID("-") + blob.ID(pad(i))
}

// idWidth is how many digits a blob ID index is padded to, so IDs sort
// lexicographically the way they were created.
const idWidth = 6

func pad(i int) string {
	s := strconv.Itoa(i)
	for len(s) < idWidth {
		s = "0" + s
	}

	return s
}

// randomBytes returns n deterministic pseudo-random bytes drawn from the
// workload's seeded generator.
func (e *workloadEnv) randomBytes(n int) []byte {
	buf := make([]byte, n)

	var scratch [8]byte

	for i := 0; i < n; i += len(scratch) {
		binary.LittleEndian.PutUint64(scratch[:], e.rnd.Uint64())
		copy(buf[i:], scratch[:])
	}

	return buf
}

// writeSet writes count blobs of the given kind and size, without measuring
// anything. It is the setup phase of the read/list/delete workloads.
func (e *workloadEnv) writeSet(ctx context.Context, kind string, count int, size func(i int) int64) ([]blob.ID, error) {
	st := e.storage()
	ids := make([]blob.ID, 0, count)

	for i := range count {
		id := e.blobID(kind, i)
		data := e.randomBytes(int(size(i)))

		if err := st.PutBlob(ctx, id, gather.FromSlice(data), blob.PutOptions{}); err != nil {
			return ids, errors.Wrapf(err, "unable to write the setup blob %v", id)
		}

		ids = append(ids, id)
	}

	return ids, nil
}

// failure turns a sampler's error tally into the workload's return error.
func (s *sampler) failure(what string) error {
	if s.errs == 0 {
		return nil
	}

	return errors.Wrapf(s.firstEr, "%v of %v %v operations failed, first error", s.errs, s.ops, what)
}

// one wraps a single set of metrics under the workload's own name. The metrics
// are passed by pointer only because the struct is large; the value is copied
// into the returned phase and never aliased.
func one(name string, m *WorkloadMetrics) []phaseMetrics {
	return []phaseMetrics{{Key: name, Metrics: *m}}
}

// phaseKey builds the result key of a multi-phase workload.
func phaseKey(workload, phase string) string {
	return workload + ":" + phase
}

// workloadOfKey returns the registry name a result key belongs to.
func workloadOfKey(key string) string {
	for i := range len(key) {
		if key[i] == ':' {
			return key[:i]
		}
	}

	return key
}

// ---------------------------------------------------------------------------
// 1. small-write
// ---------------------------------------------------------------------------

func estimateSmallWrite(p *benchParams) workloadEstimate {
	avg := int64(p.SmallBlobMinBytes+p.SmallBlobMaxBytes) / 2 //nolint:mnd // arithmetic mean of the two bounds.

	return workloadEstimate{
		UploadBytes: int64(p.SmallWriteN) * avg,
		QuotaUnits:  int64(p.SmallWriteN)*quWrite(avg) + quCleanup(p.SmallWriteN),
	}
}

func runSmallWrite(ctx context.Context, e *workloadEnv) ([]phaseMetrics, error) {
	st := e.storage()
	span := e.params.SmallBlobMaxBytes - e.params.SmallBlobMinBytes
	s := newSampler(st)

	for i := range e.params.SmallWriteN {
		size := e.params.SmallBlobMinBytes
		if span > 0 {
			size += e.rnd.IntN(span + 1)
		}

		data := e.randomBytes(size)
		t := timetrack.StartTimer()
		err := st.PutBlob(ctx, e.blobID("sw", i), gather.FromSlice(data), blob.PutOptions{})

		s.observe(t.Elapsed(), err)

		if err == nil {
			s.up(int64(size))
		}
	}

	m := s.finish(st)

	return one(workloadSmallWrite, &m), s.failure(workloadSmallWrite)
}

// ---------------------------------------------------------------------------
// 2. pack-write
// ---------------------------------------------------------------------------

func estimatePackWrite(p *benchParams) workloadEstimate {
	return workloadEstimate{
		UploadBytes: int64(p.PackWriteM) * p.PackSizeBytes,
		QuotaUnits:  int64(p.PackWriteM)*quWrite(p.PackSizeBytes) + quCleanup(p.PackWriteM),
	}
}

func runPackWrite(ctx context.Context, e *workloadEnv) ([]phaseMetrics, error) {
	st := e.storage()
	s := newSampler(st)

	for i := range e.params.PackWriteM {
		data := e.randomBytes(int(e.params.PackSizeBytes))
		t := timetrack.StartTimer()
		err := st.PutBlob(ctx, e.blobID("pw", i), gather.FromSlice(data), blob.PutOptions{})

		s.observe(t.Elapsed(), err)

		if err == nil {
			s.up(e.params.PackSizeBytes)
		}
	}

	m := s.finish(st)

	return one(workloadPackWrite, &m), s.failure(workloadPackWrite)
}

// ---------------------------------------------------------------------------
// 3. read-metadata
// ---------------------------------------------------------------------------

func estimateReadMetadata(p *benchParams) workloadEstimate {
	passes := int64(metadataPasses)
	if p.TrueCold {
		passes = trueColdMetadataPasses
	}

	avg := int64(p.SmallBlobMinBytes+p.SmallBlobMaxBytes) / 2 //nolint:mnd // arithmetic mean of the two bounds.

	return workloadEstimate{
		UploadBytes: int64(p.ReadMetadataN) * avg,
		QuotaUnits: int64(p.ReadMetadataN)*quWrite(avg) +
			passes*int64(p.ReadMetadataN)*quListItems +
			quCleanup(p.ReadMetadataN),
	}
}

// Phase names of read-metadata, and how many passes each mode makes.
const (
	phaseHot  = "hot"
	phaseWarm = "warm"
	phaseCold = "cold"

	metadataPasses         = 2
	trueColdMetadataPasses = 3
)

// runReadMetadata reads the metadata of a written set two or three times.
//
//	hot  - the connection that wrote the blobs, every file ID in memory.
//	warm - a fresh connection, in-memory map gone, on-disk journal intact: what
//	       a restarted process finds. Only produced with --true-cold.
//	cold - a fresh connection that knows nothing, so every blob name has to be
//	       resolved against Drive again: with --true-cold the journal is deleted
//	       from under it, without it the backend is asked to wipe its own caches.
func runReadMetadata(ctx context.Context, e *workloadEnv) ([]phaseMetrics, error) {
	ids, err := e.writeSet(ctx, "rm", e.params.ReadMetadataN, func(int) int64 {
		return int64(e.params.SmallBlobMinBytes)
	})
	if err != nil {
		return nil, err
	}

	out := []phaseMetrics{}

	hot, hotErr := metadataPass(ctx, e.storage(), ids)
	out = append(out, phaseMetrics{Key: phaseKey(workloadReadMetadata, phaseHot), Metrics: hot})

	if hotErr != nil {
		return out, hotErr
	}

	if e.params.TrueCold {
		if err := e.recycle(ctx); err != nil {
			return out, err
		}

		warm, warmErr := metadataPass(ctx, e.storage(), ids)
		out = append(out, phaseMetrics{Key: phaseKey(workloadReadMetadata, phaseWarm), Metrics: warm})

		if warmErr != nil {
			return out, warmErr
		}

		if err := e.recycleCold(ctx); err != nil {
			return out, err
		}
	} else if err := e.recycleFlushed(ctx); err != nil {
		return out, err
	}

	cold, coldErr := metadataPass(ctx, e.storage(), ids)
	out = append(out, phaseMetrics{Key: phaseKey(workloadReadMetadata, phaseCold), Metrics: cold})

	return out, coldErr
}

// metadataPass reads the metadata of every blob in ids once.
func metadataPass(ctx context.Context, st blob.Storage, ids []blob.ID) (WorkloadMetrics, error) {
	s := newSampler(st)

	for _, id := range ids {
		t := timetrack.StartTimer()
		_, err := st.GetMetadata(ctx, id)

		s.observe(t.Elapsed(), err)
	}

	return s.finish(st), s.failure(workloadReadMetadata)
}

// ---------------------------------------------------------------------------
// 4. range-read
// ---------------------------------------------------------------------------

func estimateRangeRead(p *benchParams) workloadEstimate {
	return workloadEstimate{
		UploadBytes: int64(p.RangeReadPacks) * p.RangeReadPackSize,
		QuotaUnits: int64(p.RangeReadPacks)*quWrite(p.RangeReadPackSize) +
			int64(p.RangeReadR)*quDownloadItem +
			quCleanup(p.RangeReadPacks),
	}
}

func runRangeRead(ctx context.Context, e *workloadEnv) ([]phaseMetrics, error) {
	ids, err := e.writeSet(ctx, "rr", e.params.RangeReadPacks, func(int) int64 {
		return e.params.RangeReadPackSize
	})
	if err != nil {
		return nil, err
	}

	st := e.storage()
	s := newSampler(st)

	buf := gather.NewWriteBuffer()
	defer buf.Close()

	span := e.params.RangeReadMaxBytes - e.params.RangeReadMinBytes

	for range e.params.RangeReadR {
		length := e.params.RangeReadMinBytes
		if span > 0 {
			length += e.rnd.Int64N(span + 1)
		}

		if length > e.params.RangeReadPackSize {
			length = e.params.RangeReadPackSize
		}

		id := ids[e.rnd.IntN(len(ids))]
		offset := e.rnd.Int64N(e.params.RangeReadPackSize - length + 1)

		buf.Reset()

		t := timetrack.StartTimer()
		err := st.GetBlob(ctx, id, offset, length, buf)

		s.observe(t.Elapsed(), err)

		if err == nil {
			s.down(int64(buf.Length()))
		}
	}

	m := s.finish(st)

	return one(workloadRangeRead, &m), s.failure(workloadRangeRead)
}

// ---------------------------------------------------------------------------
// 5. list-heavy
// ---------------------------------------------------------------------------

func estimateListHeavy(p *benchParams) workloadEstimate {
	const phases = 2

	return workloadEstimate{
		UploadBytes: int64(p.ListBlobsN) * int64(p.SmallBlobMinBytes),
		QuotaUnits: int64(p.ListBlobsN)*quWrite(int64(p.SmallBlobMinBytes)) +
			phases*int64(p.ListTraversals)*quListItems +
			quCleanup(p.ListBlobsN),
	}
}

func runListHeavy(ctx context.Context, e *workloadEnv) ([]phaseMetrics, error) {
	if _, err := e.writeSet(ctx, "lh", e.params.ListBlobsN, func(int) int64 {
		return int64(e.params.SmallBlobMinBytes)
	}); err != nil {
		return nil, err
	}

	st := e.storage()

	// A whole-storage traversal is what blob GC and repository maintenance do,
	// and it is the listing cost that actually hurts on Drive, so the "full"
	// phase deliberately uses the empty prefix and pays for everything else in
	// the folder too.
	full, fullErr := listPass(ctx, st, "", e.params.ListTraversals)
	if fullErr != nil {
		return one(phaseKey(workloadListHeavy, "full"), &full), fullErr
	}

	prefixed, prefixedErr := listPass(ctx, st, e.prefix+"lh-", e.params.ListTraversals)

	return []phaseMetrics{
		{Key: phaseKey(workloadListHeavy, "full"), Metrics: full},
		{Key: phaseKey(workloadListHeavy, "prefixed"), Metrics: prefixed},
	}, prefixedErr
}

// listPass performs count traversals, timing each one.
func listPass(ctx context.Context, st blob.Storage, prefix blob.ID, count int) (WorkloadMetrics, error) {
	s := newSampler(st)

	for range count {
		t := timetrack.StartTimer()
		err := st.ListBlobs(ctx, prefix, func(blob.Metadata) error { return nil })

		s.observe(t.Elapsed(), err)
	}

	return s.finish(st), s.failure(workloadListHeavy)
}

// ---------------------------------------------------------------------------
// 6. delete-sweep
// ---------------------------------------------------------------------------

func estimateDeleteSweep(p *benchParams) workloadEstimate {
	return workloadEstimate{
		UploadBytes: int64(p.DeleteSweepN) * int64(p.SmallBlobMinBytes),
		QuotaUnits: int64(p.DeleteSweepN)*quWrite(int64(p.SmallBlobMinBytes)) +
			int64(p.DeleteSweepN)*quEditItem +
			quCleanup(p.DeleteSweepN),
	}
}

func runDeleteSweep(ctx context.Context, e *workloadEnv) ([]phaseMetrics, error) {
	ids, err := e.writeSet(ctx, "ds", e.params.DeleteSweepN, func(int) int64 {
		return int64(e.params.SmallBlobMinBytes)
	})
	if err != nil {
		return nil, err
	}

	victims := ids[:len(ids)*e.params.DeletePercent/percentDivisor]
	st := e.storage()
	s := newSampler(st)

	parallelism := effectiveDeleteParallelism(e.tgt, e.params.Parallelism)

	for _, r := range deleteParallel(ctx, st, victims, parallelism) {
		s.observe(r.d, r.err)
	}

	m := s.finish(st)
	m.Params = map[string]int64{deleteSweepParallelismParam: int64(parallelism)}

	return one(workloadDeleteSweep, &m), s.failure(workloadDeleteSweep)
}

// deleteSweepParallelismParam is the key delete-sweep records its effective
// parallelism under in WorkloadMetrics.Params.
const deleteSweepParallelismParam = "parallelism"

// effectiveDeleteParallelism decides how many deletes delete-sweep issues at
// once: the TARGET's own tuning.deleteParallelism when it sets one, and the
// harness's --parallelism otherwise.
//
// The target wins because otherwise a cycle sweeping deleteParallelism measures
// nothing: the harness's own semaphore (default 8) is the tighter of the two
// and clamps the backend knob out of the experiment, which is exactly what
// happened to tuning cycle 1. The chosen value is recorded in the phase's
// Params so the result document says which of the two was in force rather than
// leaving a reader to infer it from config.params.
//
// The backend's own deleteSem still applies underneath; this only stops the
// harness from being the binding constraint.
func effectiveDeleteParallelism(t target, harnessParallelism int) int {
	if n, ok := t.deleteParallelism(); ok {
		return n
	}

	return harnessParallelism
}

// percentDivisor turns DeletePercent into a fraction.
const percentDivisor = 100

// opResult is one timed operation of a parallel phase.
type opResult struct {
	d   time.Duration
	err error
}

// deleteParallel deletes ids with bounded parallelism, the way
// blob.DeleteMultiple does, but timing each operation individually. Results
// come back in the order of ids so that a run is reproducible.
func deleteParallel(ctx context.Context, st blob.Storage, ids []blob.ID, parallelism int) []opResult {
	if parallelism < 1 {
		parallelism = 1
	}

	res := make([]opResult, len(ids))
	sem := make(chan struct{}, parallelism)

	var wg sync.WaitGroup

	for i, id := range ids {
		sem <- struct{}{}

		wg.Add(1)

		go func() {
			defer func() {
				<-sem

				wg.Done()
			}()

			t := timetrack.StartTimer()
			err := st.DeleteBlob(ctx, id)
			res[i] = opResult{d: t.Elapsed(), err: err}
		}()
	}

	wg.Wait()

	return res
}

// cleanupPrefix deletes everything under a prefix. It is deferred by the runner
// so that it runs even when a workload fails half-way through, and its failures
// are reported but never fatal: leftover benchmark blobs cost a few bytes,
// while aborting a run on a cleanup error loses the measurement.
func cleanupPrefix(ctx context.Context, st blob.Storage, prefix blob.ID, parallelism int) (deleted int, err error) {
	var ids []blob.ID

	if err := st.ListBlobs(ctx, prefix, func(bm blob.Metadata) error {
		ids = append(ids, bm.BlobID)

		return nil
	}); err != nil {
		return 0, errors.Wrapf(err, "unable to list the blobs of %v for cleanup", prefix)
	}

	slices.Sort(ids)

	var firstErr error

	for _, r := range deleteParallel(ctx, st, ids, parallelism) {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}

	if firstErr != nil {
		return len(ids), errors.Wrapf(firstErr, "unable to delete some of the %v blobs under %v", len(ids), prefix)
	}

	return len(ids), nil
}

// quWrite estimates the quota units one blob write costs: a name resolution
// (files.list, 100) plus the upload itself. Uploads below the backend's simple
// upload cutoff are a single edit-class request; larger ones are a resumable
// session plus one request per chunk. The cutoff and chunk size below are the
// backend's defaults - a target that tunes them makes this an over-estimate,
// which is the safe direction for a quota guard.
func quWrite(size int64) int64 {
	if size < defaultSimpleUploadCutoffBytes {
		return quListItems + quEditItem
	}

	chunks := (size + defaultUploadChunkBytes - 1) / defaultUploadChunkBytes

	return quListItems + (chunks+1)*quEditItem
}

// quCleanup estimates the cost of a workload's teardown: one listing plus one
// delete per blob.
func quCleanup(blobs int) int64 {
	return quListItems + int64(blobs)*quEditItem
}

// The native backend's upload defaults, mirrored here for cost estimation only.
// They are package constants over in repo/blob/gdrive; duplicating the numbers
// is deliberate, because the estimate must not depend on unexported state.
const (
	defaultSimpleUploadCutoffBytes = 8 * mib
	defaultUploadChunkBytes        = 64 * mib
)
