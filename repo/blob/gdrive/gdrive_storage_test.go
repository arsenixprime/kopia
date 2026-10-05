//go:build !no_extra_providers

package gdrive

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	drive "google.golang.org/api/drive/v3"

	"github.com/kopia/kopia/internal/blobtesting"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/internal/providervalidation"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/repo/blob"
)

// Environment variables driving the live suite.
const (
	// testCredentialsEnv names a file holding either a Google service account
	// key or an authorized-user (OAuth refresh token) JSON document.
	testCredentialsEnv = "KOPIA_GDRIVE_TEST_CREDENTIALS"

	// testFolderEnv names the Drive folder the tests create their per-run
	// subfolder in. Under the default drive.file scope this folder must have
	// been created by the same OAuth client.
	testFolderEnv = "KOPIA_GDRIVE_TEST_FOLDER"
)

//
// ---------------------------------------------------------------- mock suite
//

// newMockStorage builds a storage backed by an in-memory fake Drive server. The
// whole production stack - auth seam aside - is exercised: instrumented
// transport, pacer, persistent ID cache, name resolution.
func newMockStorage(t *testing.T) (*fakeDrive, blob.Storage) {
	t.Helper()

	return newTunedMockStorage(t, func(*Options) {})
}

// newTunedMockStorage is newMockStorage with a chance to adjust the options
// before the storage is opened.
func newTunedMockStorage(t *testing.T, adjust func(*Options)) (*fakeDrive, blob.Storage) {
	t.Helper()

	ctx := testlogging.Context(t)
	d := newFakeDrive(t)
	opt := d.options(t, testutil.TempDirectory(t))

	adjust(opt)

	st, err := New(ctx, opt, false)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, st.Close(ctx)) })

	return d, st
}

func TestGdriveStorageMock(t *testing.T) {
	ctx := testlogging.Context(t)
	_, st := newMockStorage(t)

	blobtesting.VerifyStorage(ctx, t, st, blob.PutOptions{})
	blobtesting.AssertConnectionInfoRoundTrips(ctx, t, st)
}

// TestGdriveStorageMockValidateProvider runs the same validation
// "kopia repository validate-provider" runs. It is the regression test for
// issues #3788 (conditional creates) and #4272 (cross-connection
// read-your-writes): ValidateProvider opens five equivalent connections from
// one ConnectionInfo and picks a random one per call, which only works because
// the persistent ID cache is shared between them by its process-wide registry.
func TestGdriveStorageMockValidateProvider(t *testing.T) {
	ctx := testlogging.Context(t)
	_, st := newMockStorage(t)

	require.NoError(t, providervalidation.ValidateProvider(ctx, st, blobtesting.TestValidationOptions))
}

func TestGdriveStorageMockConcurrentAccess(t *testing.T) {
	_, st := newMockStorage(t)

	blobtesting.VerifyConcurrentAccess(t, st, blobtesting.ConcurrentAccessOptions{
		NumBlobs:                        8,
		Getters:                         4,
		Putters:                         4,
		Deleters:                        2,
		Listers:                         2,
		Iterations:                      6,
		RangeGetPercentage:              50,
		NonExistentListPrefixPercentage: 20,
	})
}

// TestGdriveStorageMockDoNotRecreate covers the conditional-create protocol on
// its own, including the branch that only fires when another writer wins the
// race.
func TestGdriveStorageMockDoNotRecreate(t *testing.T) {
	ctx := testlogging.Context(t)
	_, st := newMockStorage(t)

	const blobID = blob.ID("dnr-blob")

	require.NoError(t, st.PutBlob(ctx, blobID, gather.FromSlice([]byte("original")), blob.PutOptions{}))

	// Existing blob, resolvable from the cache: rejected without touching it.
	err := st.PutBlob(ctx, blobID, gather.FromSlice([]byte("replacement")), blob.PutOptions{DoNotRecreate: true})
	require.ErrorIs(t, err, blob.ErrBlobAlreadyExists)
	blobtesting.AssertGetBlob(ctx, t, st, blobID, []byte("original"))

	// Existing blob that this connection has never seen: still rejected, this
	// time via the name query.
	require.NoError(t, st.FlushCaches(ctx))

	err = st.PutBlob(ctx, blobID, gather.FromSlice([]byte("replacement")), blob.PutOptions{DoNotRecreate: true})
	require.ErrorIs(t, err, blob.ErrBlobAlreadyExists)
	blobtesting.AssertGetBlob(ctx, t, st, blobID, []byte("original"))
}

// TestGdriveStorageMockDoNotRecreateRace drives the losing branch of the
// duplicate-resolution protocol: another writer creates the same name after we
// have decided the blob is absent, but before our own create lands. The older
// file must survive and our file must be gone.
func TestGdriveStorageMockDoNotRecreateRace(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	const blobID = blob.ID("raced-blob")

	// The competing writer creates the file server-side after our existence
	// probe has run and while our own create is in flight, which is exactly the
	// window Drive gives no way to close.
	var otherID string

	d.onNext(methodUploadSimple, func() {
		otherID = d.addFile(string(blobID), []byte("other writer"), clock.Now())
	})

	err := st.PutBlob(ctx, blobID, gather.FromSlice([]byte("ours")), blob.PutOptions{DoNotRecreate: true})
	require.ErrorIs(t, err, blob.ErrBlobAlreadyExists)

	require.Equal(t, []string{otherID}, d.fileIDsNamed(string(blobID)),
		"the losing duplicate should have been deleted")

	blobtesting.AssertGetBlob(ctx, t, st, blobID, []byte("other writer"))
}

// TestGdriveStorageMockDoNotRecreateRaceWins is the mirror image: our file is
// the older one, so it survives and the write succeeds.
func TestGdriveStorageMockDoNotRecreateRaceWins(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	const blobID = blob.ID("raced-blob-win")

	otherID := d.addFile(string(blobID), []byte("other writer"), clock.Now().Add(time.Hour))

	d.injectFailure(methodFilesList, http.StatusNotFound, "notFound")

	require.NoError(t, st.PutBlob(ctx, blobID, gather.FromSlice([]byte("ours")), blob.PutOptions{DoNotRecreate: true}))

	ids := d.fileIDsNamed(string(blobID))
	require.Len(t, ids, 2, "the winner does not delete the other writer's file")
	require.Contains(t, ids, otherID)
}

// TestGdriveStorageMockStaleCachedID covers the rule that the ID cache is never
// authoritative: an entry that 404s is invalidated and the name is resolved
// again rather than reported as a missing blob.
func TestGdriveStorageMockStaleCachedID(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	const blobID = blob.ID("stale-blob")

	require.NoError(t, st.PutBlob(ctx, blobID, gather.FromSlice([]byte("v1")), blob.PutOptions{}))

	// Replace the file behind the blob without telling the cache, exactly as
	// another machine would.
	oldIDs := d.fileIDsNamed(string(blobID))
	require.Len(t, oldIDs, 1)

	d.deleteFileDirectly(oldIDs[0])
	newID := d.addFile(string(blobID), []byte("v2"), clock.Now())

	blobtesting.AssertGetBlob(ctx, t, st, blobID, []byte("v2"))

	bm, err := st.GetMetadata(ctx, blobID)
	require.NoError(t, err)
	require.Equal(t, int64(2), bm.Length)

	cached, ok := mustGdriveStorage(t, st).idCache.Get(blobID)
	require.True(t, ok)
	require.Equal(t, newID, cached, "the cache should have been repointed at the surviving file")
}

func TestGdriveStorageMockRangeReads(t *testing.T) {
	ctx := testlogging.Context(t)
	_, st := newMockStorage(t)

	const blobID = blob.ID("range-blob")

	content := []byte("0123456789")
	require.NoError(t, st.PutBlob(ctx, blobID, gather.FromSlice(content), blob.PutOptions{}))

	var out gather.WriteBuffer
	defer out.Close()

	cases := []struct {
		offset, length int64
		want           string
		wantErr        error
	}{
		{0, -1, "0123456789", nil},
		{3, -1, "3456789", nil},
		{0, 4, "0123", nil},
		{6, 4, "6789", nil},
		{5, 0, "", nil},
		{-1, 1, "", blob.ErrInvalidRange},
		{10, 3, "", blob.ErrInvalidRange},
		{9, 3, "", blob.ErrInvalidRange},
		{11, 3, "", blob.ErrInvalidRange},
	}

	for _, tc := range cases {
		err := st.GetBlob(ctx, blobID, tc.offset, tc.length, &out)

		if tc.wantErr != nil {
			require.ErrorIsf(t, err, tc.wantErr, "GetBlob(%v,%v)", tc.offset, tc.length)

			continue
		}

		require.NoErrorf(t, err, "GetBlob(%v,%v)", tc.offset, tc.length)
		require.Equalf(t, tc.want, string(out.ToByteSlice()), "GetBlob(%v,%v)", tc.offset, tc.length)
	}

	// A read of a missing blob must leave the output buffer empty.
	require.NoError(t, st.GetBlob(ctx, blobID, 0, -1, &out))
	blobtesting.AssertGetBlobNotFound(ctx, t, st, "no-such-blob")

	// A zero-length read is answered from metadata, never from a one-byte
	// download: it must cost no media call at all.
	before := mustGdriveStorage(t, st).stats.Snapshot().PerMethod[methodFilesGetMedia].Calls
	require.NoError(t, st.GetBlob(ctx, blobID, 5, 0, &out))
	require.Equal(t, before, mustGdriveStorage(t, st).stats.Snapshot().PerMethod[methodFilesGetMedia].Calls)
}

func mustGdriveStorage(t *testing.T, st blob.Storage) *gdriveStorage {
	t.Helper()

	s, ok := st.(*gdriveStorage)
	require.True(t, ok)

	return s
}

// TestGdriveStorageMockPacerRetry checks that the pacer, not repo/blob/retrying,
// is what makes a rate-limited call succeed, and that the retry is visible in
// the stats the benchmark harness reads.
func TestGdriveStorageMockPacerRetry(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	const blobID = blob.ID("paced-blob")

	require.NoError(t, st.PutBlob(ctx, blobID, gather.FromSlice([]byte("hello")), blob.PutOptions{}))

	d.injectFailure(methodFilesGetMedia, http.StatusForbidden, "rateLimitExceeded")

	var out gather.WriteBuffer
	defer out.Close()

	require.NoError(t, st.GetBlob(ctx, blobID, 0, -1, &out))
	require.Equal(t, "hello", string(out.ToByteSlice()))

	snap, ok := StatsFromStorage(st)
	require.True(t, ok)
	require.Positive(t, snap.PerMethod[methodFilesGetMedia].Retries, "the pacer should have recorded a retry")
	require.Positive(t, snap.ErrorsByReason["rateLimitExceeded"], "the reason should have been classified")
	require.Positive(t, snap.PerMethod[methodFilesGetMedia].Backoffs, "the retry should have been paced")
}

// TestGdriveStorageMockPermanentErrorNotRetried is the other half of #2656: a
// quota error is permanent and must fail on the first attempt, not after ten.
func TestGdriveStorageMockPermanentErrorNotRetried(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	for range 5 {
		d.injectFailure(methodUploadSimple, http.StatusForbidden, "storageQuotaExceeded")
	}

	err := st.PutBlob(ctx, "quota-blob", gather.FromSlice([]byte("x")), blob.PutOptions{})
	require.Error(t, err)
	require.Equal(t, 1, d.requestCount(methodUploadSimple), "a permanent quota error must not be retried")
}

// TestGdriveStorageMockResumableUpload exercises the chunked upload path, which
// a small blob would never reach.
func TestGdriveStorageMockResumableUpload(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)
	opt := d.options(t, testutil.TempDirectory(t))

	// 1 MB cutoff with 256 KiB chunks, so a 1 MB blob takes several chunks.
	opt.Tuning.SimpleUploadCutoffMB = 1
	opt.Tuning.UploadChunkSizeMB = 1

	st, err := New(ctx, opt, false)
	require.NoError(t, err)

	defer st.Close(ctx) //nolint:errcheck

	content := make([]byte, 3<<20)
	for i := range content {
		content[i] = byte(i)
	}

	require.NoError(t, st.PutBlob(ctx, "big-blob", gather.FromSlice(content), blob.PutOptions{}))
	require.Positive(t, d.requestCount(methodUploadResumable), "the resumable protocol should have been used")

	blobtesting.AssertGetBlob(ctx, t, st, "big-blob", content)

	// ... and an overwrite goes down the resumable update path too.
	require.NoError(t, st.PutBlob(ctx, "big-blob", gather.FromSlice(content[:2<<20]), blob.PutOptions{}))
	blobtesting.AssertGetBlob(ctx, t, st, "big-blob", content[:2<<20])
}

// TestGdriveStorageMockDuplicateNames pins the behavior when a folder already
// contains two files with the same name: the newest wins everywhere, the blob
// is reported exactly once, and deleting it removes both files.
func TestGdriveStorageMockDuplicateNames(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	const blobID = blob.ID("dup-blob")

	// Even-length contents: blobtesting.AssertGetBlob's second-half assertion
	// only lines up for an even length.
	d.addFile(string(blobID), []byte("older!"), clock.Now().Add(-time.Hour))
	d.addFile(string(blobID), []byte("newer!"), clock.Now())

	blobtesting.AssertListResultsIDs(ctx, t, st, blobID, blobID)
	blobtesting.AssertGetBlob(ctx, t, st, blobID, []byte("newer!"))

	// Deleting through a name query removes every file carrying the name, so the
	// blob cannot come back from the dead in a later listing. (With a cached file
	// ID the delete takes the single-call fast path; see DeleteBlob.)
	require.NoError(t, st.FlushCaches(ctx))
	require.NoError(t, st.DeleteBlob(ctx, blobID))
	require.Empty(t, d.fileIDsNamed(string(blobID)))

	blobtesting.AssertListResultsIDs(ctx, t, st, blobID)
}

func TestGdriveStorageMockCapacity(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	c, err := st.GetCapacity(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(fakeDefaultQuotaLimit), c.SizeB)
	require.Equal(t, uint64(fakeDefaultQuotaLimit-fakeDefaultQuotaUsage), c.FreeB)
	require.LessOrEqual(t, c.FreeB, c.SizeB)

	d.setQuota(0, fakeDefaultQuotaUsage)

	_, err = st.GetCapacity(ctx)
	require.ErrorIs(t, err, blob.ErrNotAVolume)
}

func TestGdriveStorageMockMissingFolder(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)

	opt := d.options(t, testutil.TempDirectory(t))

	// Register the bogus folder ID with the same fake server, then connect to it:
	// the liveness probe in New must reject it.
	bogus := opt.FolderID + "-no-such-folder"
	testDriveTransports.Store(bogus, testDriveTransport{
		base:     d.server.Client().Transport,
		endpoint: d.server.URL + "/drive/v3/",
	})

	t.Cleanup(func() { testDriveTransports.Delete(bogus) })

	opt.FolderID = bogus

	_, err := New(ctx, opt, false)
	require.Error(t, err)
}

func TestGdriveStorageMockReadOnlyAndDisplayName(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)
	opt := d.options(t, testutil.TempDirectory(t))
	opt.ReadOnly = true

	st, err := New(ctx, opt, false)
	require.NoError(t, err)

	defer st.Close(ctx) //nolint:errcheck

	require.True(t, st.IsReadOnly())
	require.Equal(t, "Google Drive: "+d.rootFolderID, st.DisplayName())
	require.Equal(t, gdriveStorageType, st.ConnectionInfo().Type)
}

// TestGdriveStorageMockUserAgent pins the fix for the previous implementation
// not identifying Kopia to Drive at all.
func TestGdriveStorageMockUserAgent(t *testing.T) {
	d, _ := newMockStorage(t)

	require.Contains(t, d.lastUserAgent(), blob.ApplicationID)
}

func TestGdriveStorageMockListCallbackError(t *testing.T) {
	ctx := testlogging.Context(t)
	_, st := newMockStorage(t)

	for _, id := range []blob.ID{"aa1", "aa2", "bb1"} {
		require.NoError(t, st.PutBlob(ctx, id, gather.FromSlice([]byte(id)), blob.PutOptions{}))
	}

	sentinel := errors.New("sentinel")

	require.ErrorIs(t, st.ListBlobs(ctx, "", func(blob.Metadata) error { return sentinel }), sentinel)
	require.ErrorIs(t, st.ListBlobs(ctx, "aa", func(blob.Metadata) error { return sentinel }), sentinel)

	// A non-matching entry must not truncate the page: with a page size of one
	// the prefix filter runs on every page and all matching blobs still arrive.
	var got []blob.ID

	require.NoError(t, st.ListBlobs(ctx, "aa", func(m blob.Metadata) error {
		got = append(got, m.BlobID)

		return nil
	}))

	require.ElementsMatch(t, []blob.ID{"aa1", "aa2"}, got)
}

// TestGdriveStorageMockListPaging forces multiple pages so that the paging loop
// and the client-side prefix filter are exercised together. The previous
// implementation returned from the page consumer on the first non-matching
// entry, silently dropping the rest of the page.
func TestGdriveStorageMockListPaging(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)
	opt := d.options(t, testutil.TempDirectory(t))
	opt.Tuning.ListPageSize = 1

	st, err := New(ctx, opt, false)
	require.NoError(t, err)

	defer st.Close(ctx) //nolint:errcheck

	want := []blob.ID{}

	for _, id := range []blob.ID{"xa", "ya", "xb", "yb", "xc"} {
		require.NoError(t, st.PutBlob(ctx, id, gather.FromSlice([]byte(id)), blob.PutOptions{}))

		if strings.HasPrefix(string(id), "x") {
			want = append(want, id)
		}
	}

	blobtesting.AssertListResultsIDs(ctx, t, st, "x", want...)
}

//
// --------------------------------------------------------------- bulk deletes
//

// bulkDeleteBlobs is the shape of a maintenance-style delete: many blobs
// written by this connection, then removed through blob.DeleteMultiple, which
// is the only bulk-delete path Kopia has (repo/blob/storage.go:368).
const (
	bulkDeleteBlobCount   = 200
	bulkDeleteParallelism = 8

	// A 1 ms pacer interval with a burst of one makes the steady-state rate
	// observable without making the test slow: N calls cannot complete in less
	// than (N-1) ms, and that lower bound is what proves every delete went
	// through the pacer.
	bulkDeletePacerMinSleepMS = 1
)

func putBulkDeleteBlobs(ctx context.Context, t *testing.T, st blob.Storage) []blob.ID {
	t.Helper()

	ids := make([]blob.ID, 0, bulkDeleteBlobCount)

	for i := range bulkDeleteBlobCount {
		id := blob.ID(fmt.Sprintf("bulk-%04d", i))
		require.NoError(t, st.PutBlob(ctx, id, gather.FromSlice([]byte("x")), blob.PutOptions{}))

		ids = append(ids, id)
	}

	return ids
}

func tuneForBulkDeletes(useBatch bool) func(*Options) {
	return func(opt *Options) {
		opt.Tuning.PacerMinSleepMS = bulkDeletePacerMinSleepMS
		opt.Tuning.PacerBurst = 1
		opt.Tuning.DeleteParallelism = bulkDeleteParallelism
		opt.Tuning.UseBatchDelete = &useBatch
	}
}

// TestGdriveStorageMockParallelDeletes pins the cost of the default delete
// path: one files.delete per blob and nothing else. The blobs were written by
// this connection, so every file ID is cached, and a delete that spent a
// files.list to re-resolve a name it already knows would double the quota bill
// of every maintenance run.
func TestGdriveStorageMockParallelDeletes(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newTunedMockStorage(t, tuneForBulkDeletes(false))

	ids := putBulkDeleteBlobs(ctx, t, st)

	listsBefore := d.requestCount(methodFilesList)
	started := clock.Now()

	require.NoError(t, blob.DeleteMultiple(ctx, st, ids, bulkDeleteParallelism))

	elapsed := clock.Now().Sub(started)

	require.Equal(t, bulkDeleteBlobCount, d.requestCount(methodFilesDelete))
	require.Equal(t, listsBefore, d.requestCount(methodFilesList), "a cached file ID must not be re-resolved by name")
	require.Zero(t, d.requestCount(methodBatch))
	require.Empty(t, d.fileIDsNamed(string(ids[0])))

	require.LessOrEqual(t, d.peakConcurrency(methodFilesDelete), bulkDeleteParallelism,
		"deletes must respect the configured concurrency ceiling")

	// Every call went through the shared token bucket, so the wall clock cannot
	// be shorter than the steady-state rate allows.
	require.GreaterOrEqual(t, elapsed, (bulkDeleteBlobCount-1)*bulkDeletePacerMinSleepMS*time.Millisecond)

	t.Logf("%v parallel deletes: %v calls, %v", bulkDeleteBlobCount, d.requestCount(methodFilesDelete), elapsed)
}

// holdBatchWindowOpen makes the collector wait for a full batch rather than for
// the clock, which is what turns a timing-dependent coalescing test into a
// deterministic one: with a window this long the only thing that can release a
// batch is it reaching maxBatchDeleteSize.
func holdBatchWindowOpen(t *testing.T, st blob.Storage) {
	t.Helper()

	c := mustGdriveStorage(t, st).deleteBatch
	require.NotNil(t, c)

	c.window = time.Minute
}

// TestGdriveStorageMockBatchDeletes is the same workload through the batch
// endpoint: 100 concurrent deletes become ONE HTTP round trip instead of 100.
// Quota is unaffected - Drive charges a batch of n as n requests - so this is
// purely the RTT experiment Phase 3 has to settle.
func TestGdriveStorageMockBatchDeletes(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newTunedMockStorage(t, tuneForBulkDeletes(true))

	holdBatchWindowOpen(t, st)

	ids := putBulkDeleteBlobs(ctx, t, st)[:maxBatchDeleteSize]

	listsBefore := d.requestCount(methodFilesList)
	started := clock.Now()

	require.NoError(t, blob.DeleteMultiple(ctx, st, ids, len(ids)))

	elapsed := clock.Now().Sub(started)

	require.Equal(t, 1, d.requestCount(methodBatch))
	require.Equal(t, len(ids), d.requestCount(methodFilesDelete),
		"a batch of n still executes n deletes; only the round trips are saved")
	require.Equal(t, listsBefore, d.requestCount(methodFilesList))

	for _, id := range ids {
		require.Empty(t, d.fileIDsNamed(string(id)))
	}

	t.Logf("%v batched deletes: %v batch calls, %v", len(ids), d.requestCount(methodBatch), elapsed)
}

// TestGdriveStorageMockBatchDeleteCoalescing runs the full 200-blob workload
// with the production window. Exactly how the arrivals fall into batches is a
// scheduling detail, but the order of magnitude is not: 200 deletes must cost a
// handful of round trips, not 200.
func TestGdriveStorageMockBatchDeleteCoalescing(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newTunedMockStorage(t, tuneForBulkDeletes(true))

	ids := putBulkDeleteBlobs(ctx, t, st)

	require.NoError(t, blob.DeleteMultiple(ctx, st, ids, bulkDeleteBlobCount))

	batches := d.requestCount(methodBatch)

	require.Equal(t, bulkDeleteBlobCount, d.requestCount(methodFilesDelete))
	require.GreaterOrEqual(t, batches, bulkDeleteBlobCount/maxBatchDeleteSize,
		"a batch cannot hold more than %v calls", maxBatchDeleteSize)
	require.LessOrEqual(t, batches, bulkDeleteBlobCount/10, "the deletes were not coalesced")

	for _, id := range ids {
		require.Empty(t, d.fileIDsNamed(string(id)))
	}

	t.Logf("%v deletes coalesced into %v batches", bulkDeleteBlobCount, batches)
}

// TestGdriveStorageMockBatchDeleteMixedParts drives the part-level error
// semantics: one file has already been deleted behind the backend's back (404)
// and one part is rate-limited (403). Every caller must still get the right
// answer, the rate-limited part must be retried - alone - and no successful
// part may be executed twice.
func TestGdriveStorageMockBatchDeleteMixedParts(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newTunedMockStorage(t, tuneForBulkDeletes(true))

	holdBatchWindowOpen(t, st)

	const n = maxBatchDeleteSize

	ids := make([]blob.ID, 0, n)

	for i := range n {
		id := blob.ID(fmt.Sprintf("mixed-%03d", i))
		require.NoError(t, st.PutBlob(ctx, id, gather.FromSlice([]byte("x")), blob.PutOptions{}))

		ids = append(ids, id)
	}

	// One blob disappears from under us: its part comes back 404, which for a
	// delete means the blob is already gone.
	vanished := d.fileIDsNamed(string(ids[n-1]))
	require.Len(t, vanished, 1)
	d.deleteFileDirectly(vanished[0])

	// ... and one part is throttled. The pacer must back the whole backend off
	// and re-send only that part.
	d.injectFailure(methodFilesDelete, http.StatusForbidden, "rateLimitExceeded")

	deletesBefore := d.requestCount(methodFilesDelete)

	require.NoError(t, blob.DeleteMultiple(ctx, st, ids, n))

	for _, id := range ids {
		require.Emptyf(t, d.fileIDsNamed(string(id)), "blob %v survived", id)
	}

	require.Equal(t, 2, d.requestCount(methodBatch), "the throttled part should have been retried in a second batch")
	require.Equal(t, n+1, d.requestCount(methodFilesDelete)-deletesBefore,
		"only the throttled part may be re-executed")

	snap, ok := StatsFromStorage(st)
	require.True(t, ok)
	require.Positive(t, snap.PerMethod[methodBatch].Retries)
	require.Positive(t, snap.ErrorsByReason["rateLimitExceeded"])
}

// TestGdriveStorageMockBatchDeletePerCallerResults pins the property that makes
// batching safe at all: a part that fails permanently fails ITS OWN caller and
// nobody else's. A batch is not a transaction.
func TestGdriveStorageMockBatchDeletePerCallerResults(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newTunedMockStorage(t, tuneForBulkDeletes(true))

	const n = 20

	ids := make([]blob.ID, 0, n)

	for i := range n {
		id := blob.ID(fmt.Sprintf("percaller-%02d", i))
		require.NoError(t, st.PutBlob(ctx, id, gather.FromSlice([]byte("x")), blob.PutOptions{}))

		ids = append(ids, id)
	}

	// One part - whichever Drive handles first - is refused outright.
	d.injectFailure(methodFilesDelete, http.StatusForbidden, "insufficientFilePermissions")

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []blob.ID
	)

	for _, id := range ids {
		wg.Go(func() {
			err := st.DeleteBlob(ctx, id)
			if err == nil {
				return
			}

			mu.Lock()
			defer mu.Unlock()

			failed = append(failed, id)
		})
	}

	wg.Wait()

	require.Len(t, failed, 1, "exactly one caller should have seen the refusal")

	for _, id := range ids {
		if id == failed[0] {
			require.NotEmpty(t, d.fileIDsNamed(string(id)), "the refused blob must still be there")
			continue
		}

		require.Emptyf(t, d.fileIDsNamed(string(id)), "blob %v shared a batch with a failing part and survived", id)
	}
}

// TestGdriveStorageMockBatchEnvelopeFailure covers the other failure axis: the
// batch request itself is rejected, so no part ran and the whole batch is
// retried through the pacer.
func TestGdriveStorageMockBatchEnvelopeFailure(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newTunedMockStorage(t, tuneForBulkDeletes(true))

	holdBatchWindowOpen(t, st)

	ids := putBulkDeleteBlobs(ctx, t, st)[:maxBatchDeleteSize]

	d.injectFailure(methodBatch, http.StatusServiceUnavailable, "backendError")

	require.NoError(t, blob.DeleteMultiple(ctx, st, ids, len(ids)))

	require.Equal(t, 2, d.requestCount(methodBatch), "the rejected envelope should have been re-sent whole")
	require.Equal(t, len(ids), d.requestCount(methodFilesDelete), "a rejected envelope executes no parts")

	for _, id := range ids {
		require.Empty(t, d.fileIDsNamed(string(id)))
	}
}

// TestGdriveStorageMockBatchDeleteSingle covers the degenerate batch: one
// delete with no company is sent as a plain files.delete, because wrapping a
// single call in a batch envelope is strictly more work than making it.
func TestGdriveStorageMockBatchDeleteSingle(t *testing.T) {
	ctx := testlogging.Context(t)
	useBatch := true
	d, st := newTunedMockStorage(t, func(opt *Options) { opt.Tuning.UseBatchDelete = &useBatch })

	require.NoError(t, st.PutBlob(ctx, "lonely", gather.FromSlice([]byte("x")), blob.PutOptions{}))
	require.NoError(t, st.DeleteBlob(ctx, "lonely"))

	require.Zero(t, d.requestCount(methodBatch))
	require.Equal(t, 1, d.requestCount(methodFilesDelete))
	require.Empty(t, d.fileIDsNamed("lonely"))
}

func TestBatchEndpoint(t *testing.T) {
	cases := []struct {
		basePath string
		want     string
		wantErr  bool
	}{
		{basePath: "https://www.googleapis.com/drive/v3/", want: "https://www.googleapis.com/batch/drive/v3"},
		{basePath: "http://127.0.0.1:8080/drive/v3/", want: "http://127.0.0.1:8080/batch/drive/v3"},
		{basePath: "https://example.com/api/drive/v3/", want: "https://example.com/api/batch/drive/v3"},
		{basePath: "https://example.com/v2/", wantErr: true},
	}

	for _, tc := range cases {
		got, err := batchEndpoint(tc.basePath)

		if tc.wantErr {
			require.Errorf(t, err, "batchEndpoint(%q)", tc.basePath)

			continue
		}

		require.NoErrorf(t, err, "batchEndpoint(%q)", tc.basePath)
		require.Equal(t, tc.want, got)
	}
}

//
// ------------------------------------------------------- absent-blob latency
//

// TestGdriveStorageMockAbsentBlobIsFast pins the fix for the escalation budget:
// a blob nobody ever wrote is reported missing after one confirming query, not
// after a ladder of six spread over five seconds. Kopia asks about absent blobs
// constantly, and the old budget charged every one of those questions.
func TestGdriveStorageMockAbsentBlobIsFast(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	// Quiesce the ID cache journal: with no write activity, a miss has no reason
	// to suspect a lagging index.
	require.NoError(t, st.FlushCaches(ctx))

	before := d.requestCount(methodFilesList)
	started := clock.Now()

	_, err := st.GetMetadata(ctx, "definitely-not-there")
	elapsed := clock.Now().Sub(started)

	require.ErrorIs(t, err, blob.ErrBlobNotFound)
	require.Less(t, elapsed, time.Second, "an absent blob must not cost a retry ladder")
	require.Equal(t, 2, d.requestCount(methodFilesList)-before, "one query plus one confirming query")

	t.Logf("absent-blob lookup took %v", elapsed)
}

// TestGdriveStorageMockJournalTailRescan proves the other half of the same
// change: a blob written by ANOTHER PROCESS on this machine is found by
// re-reading the shared cache journal, with no additional Drive query at all.
//
// The other process is simulated by appending a record to the journal file the
// way that process would - a single write(2) of an encoded record onto the
// O_APPEND journal - while the file it names is one this connection's name
// queries cannot see, so a hit can only have come from the journal.
func TestGdriveStorageMockJournalTailRescan(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	const blobID = blob.ID("written-elsewhere")

	// The other process's file. It is deliberately NOT named after the blob, so
	// that no name query can resolve it.
	fileID := d.addFile("some-other-name", []byte("hello"), clock.Now())

	s := mustGdriveStorage(t, st)

	appendJournalRecord(t, s.idCache.journalPath, blobID, fileID)

	listsBefore := d.requestCount(methodFilesList)
	started := clock.Now()

	bm, err := st.GetMetadata(ctx, blobID)
	elapsed := clock.Now().Sub(started)

	require.NoError(t, err)
	require.Equal(t, int64(5), bm.Length)
	require.Equal(t, 1, d.requestCount(methodFilesList)-listsBefore,
		"the journal must answer the miss without a second name query")
	require.Less(t, elapsed, time.Second)

	// ... and the mapping is now in memory, so the next read costs nothing at all.
	cached, ok := s.idCache.Get(blobID)
	require.True(t, ok)
	require.Equal(t, fileID, cached)
}

// TestGdriveStorageMockReopenKeepsTheFileIDCacheWarm pins the property the
// benchmark harness's WARM pass exists to measure, and which a real process
// restart depends on: closing the last connection to a folder and opening a new
// one against the same cache directory must resolve blobs out of the on-disk
// journal - one files.get each - instead of re-querying every name.
func TestGdriveStorageMockReopenKeepsTheFileIDCacheWarm(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)
	cacheDir := testutil.TempDirectory(t)

	first, err := New(ctx, d.options(t, cacheDir), false)
	require.NoError(t, err)

	ids := []blob.ID{"warm-a", "warm-b", "warm-c"}
	for _, id := range ids {
		require.NoError(t, first.PutBlob(ctx, id, gather.FromSlice([]byte(id)), blob.PutOptions{}))
	}

	// Dropping the last reference discards the in-memory map and flushes the
	// pending batch; the journal survives, as it does across a process restart.
	require.NoError(t, first.Close(ctx))

	second, err := New(ctx, d.options(t, cacheDir), false)
	require.NoError(t, err)

	defer second.Close(ctx) //nolint:errcheck

	require.NotSame(t, mustGdriveStorage(t, first).idCache, mustGdriveStorage(t, second).idCache,
		"the reopened connection must not be sharing the closed connection's cache instance")

	listsBefore := d.requestCount(methodFilesList)
	getsBefore := d.requestCount(methodFilesGet)

	for _, id := range ids {
		bm, err := second.GetMetadata(ctx, id)
		require.NoError(t, err)
		require.Equal(t, int64(len(id)), bm.Length)
	}

	require.Equal(t, 0, d.requestCount(methodFilesList)-listsBefore,
		"a reopen with the journal intact must not cost a name query")
	require.Equal(t, len(ids), d.requestCount(methodFilesGet)-getsBefore,
		"every blob should have been read by cached file ID")
}

// TestGdriveStorageMockFlushCachesMakesAReopenCold is the other half of the
// contract, and the mechanism behind the harness defect this test pair was
// written for: FlushCaches is a full wipe, in memory AND on disk, so a
// connection opened after one starts from nothing and pays a name query per
// blob. Anything that wants a warm reopen must therefore not call it.
func TestGdriveStorageMockFlushCachesMakesAReopenCold(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)
	cacheDir := testutil.TempDirectory(t)

	first, err := New(ctx, d.options(t, cacheDir), false)
	require.NoError(t, err)

	ids := []blob.ID{"cold-a", "cold-b", "cold-c"}
	for _, id := range ids {
		require.NoError(t, first.PutBlob(ctx, id, gather.FromSlice([]byte(id)), blob.PutOptions{}))
	}

	require.NoError(t, first.FlushCaches(ctx))
	require.NoError(t, first.Close(ctx))

	second, err := New(ctx, d.options(t, cacheDir), false)
	require.NoError(t, err)

	defer second.Close(ctx) //nolint:errcheck

	listsBefore := d.requestCount(methodFilesList)

	for _, id := range ids {
		_, err := second.GetMetadata(ctx, id)
		require.NoError(t, err)
	}

	require.Equal(t, len(ids), d.requestCount(methodFilesList)-listsBefore,
		"a wiped journal must cost one name query per blob")
}

// appendJournalRecord writes one blobID->fileID record onto the end of a file ID
// journal exactly as a second Kopia process would.
func appendJournalRecord(t *testing.T, journalPath string, blobID blob.ID, fileID string) {
	t.Helper()

	rec, ok := encodeRecord(blobID, fileID)
	require.True(t, ok)

	f, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, cacheFileMode)
	require.NoError(t, err)

	defer f.Close() //nolint:errcheck

	_, err = f.Write(rec)
	require.NoError(t, err)
}

//
// ------------------------------------------------------------ list narrowing
//

// TestGdriveStorageMockListPrefixNarrowing covers the server-side `name
// contains` narrowing and, more importantly, the client-side filter that backs
// it up: `contains` is a substring match, so the server can and does return
// names that merely embed the prefix.
func TestGdriveStorageMockListPrefixNarrowing(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	for _, id := range []blob.ID{"_log_aaa", "_log_bbb", "x_log_ccc", "other"} {
		require.NoError(t, st.PutBlob(ctx, id, gather.FromSlice([]byte(id)), blob.PutOptions{}))
	}

	blobtesting.AssertListResultsIDs(ctx, t, st, "_log_", "_log_aaa", "_log_bbb")
	require.Contains(t, d.listQuery(), "name contains '_log_'", "a long prefix should be pushed to the server")

	// Short prefixes are not narrowed: they select most of the repository, so the
	// term would cost a query condition and buy nothing.
	blobtesting.AssertListResultsIDs(ctx, t, st, "o", "other")
	require.NotContains(t, d.listQuery(), "name contains")
}

// TestGdriveStorageMockCaseInsensitiveNameQuery pins the other Drive quirk the
// listing path has to defend against: `name = 'x'` matches "X" as well, so a
// repository holding two blobs whose IDs differ only in case would otherwise
// serve one blob's bytes for the other.
func TestGdriveStorageMockCaseInsensitiveNameQuery(t *testing.T) {
	ctx := testlogging.Context(t)
	d, st := newMockStorage(t)

	d.addFile("MixedCase", []byte("upper!"), clock.Now())
	d.addFile("mixedcase", []byte("lower-case-content"), clock.Now().Add(time.Hour))

	require.NoError(t, st.FlushCaches(ctx))
	blobtesting.AssertGetBlob(ctx, t, st, "MixedCase", []byte("upper!"))

	require.NoError(t, st.FlushCaches(ctx))
	blobtesting.AssertGetBlob(ctx, t, st, "mixedcase", []byte("lower-case-content"))
}

//
// -------------------------------------------------- folder creation by name
//

// TestGdriveStorageMockCreateFolderByName is the happy path of --create-folder-name:
// nothing of that name exists, so the folder is made in the root of My Drive,
// its ID replaces the empty one in the options before anything is serialized,
// and the storage that comes back is an ordinary folder-ID storage.
func TestGdriveStorageMockCreateFolderByName(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)

	const folderName = "kopia-created-folder"

	opt := d.optionsForFolderName(t, testutil.TempDirectory(t), folderName)

	st, err := New(ctx, opt, true)
	require.NoError(t, err)

	defer st.Close(ctx) //nolint:errcheck

	// exactly one folder of that name now exists, in the root of My Drive.
	ids := d.fileIDsNamed(folderName)
	require.Len(t, ids, 1)
	require.Equal(t, []string{driveRootFolderAlias}, d.parentsOf(ids[0]))

	// the caller's options carry the resolved ID, which is what the repository
	// config file and any repository token are written from.
	require.Equal(t, ids[0], opt.FolderID)
	require.Equal(t, folderName, opt.FolderName, "the name is kept as provenance")

	ci := st.ConnectionInfo()
	require.Equal(t, gdriveStorageType, ci.Type)

	cfg, ok := ci.Config.(*Options)
	require.True(t, ok)
	require.Equal(t, ids[0], cfg.FolderID)
	require.Equal(t, folderName, cfg.FolderName)

	// re-opening from that serialized connection info is what a second machine
	// does, so it must reach the folder by ID alone.
	d.registerTransport(t, opt.FolderID)
	blobtesting.AssertConnectionInfoRoundTrips(ctx, t, st)

	// and it behaves exactly like a folder-ID storage: blobs land in the folder
	// that was created.
	require.NoError(t, st.PutBlob(ctx, "someblob", gather.FromSlice([]byte("hello!")), blob.PutOptions{}))
	blobtesting.AssertGetBlob(ctx, t, st, "someblob", []byte("hello!"))

	blobIDs := d.fileIDsNamed("someblob")
	require.Len(t, blobIDs, 1)
	require.Equal(t, []string{ids[0]}, d.parentsOf(blobIDs[0]))
}

// TestGdriveStorageMockCreateFolderByNameReusesExisting proves the operation is
// idempotent: a second create with the same name adopts the folder the first
// one made instead of piling up same-named siblings, which Drive would happily
// allow.
func TestGdriveStorageMockCreateFolderByNameReusesExisting(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)

	const folderName = "kopia-existing-folder"

	existing := d.addRootFolder(folderName)

	opt := d.optionsForFolderName(t, testutil.TempDirectory(t), folderName)

	st, err := New(ctx, opt, true)
	require.NoError(t, err)

	defer st.Close(ctx) //nolint:errcheck

	require.Equal(t, existing, opt.FolderID)
	require.Equal(t, []string{existing}, d.fileIDsNamed(folderName), "no second folder was created")
	require.Zero(t, d.requestCount(methodFilesCreate), "reuse must not create anything")
}

// TestGdriveStorageMockCreateFolderByNameAmbiguous: two folders of the same name
// cannot be told apart, and guessing would silently split a repository in two.
func TestGdriveStorageMockCreateFolderByNameAmbiguous(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)

	const folderName = "kopia-ambiguous-folder"

	first := d.addRootFolder(folderName)
	second := d.addRootFolder(folderName)

	opt := d.optionsForFolderName(t, testutil.TempDirectory(t), folderName)

	_, err := New(ctx, opt, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), folderName)
	require.Contains(t, err.Error(), first)
	require.Contains(t, err.Error(), second)
	require.Empty(t, opt.FolderID, "an ambiguous name must not resolve to anything")
}

// TestGdriveStorageMockCreateFolderByNameIsCaseSensitive: Drive's `name =` is
// case-insensitive, so the query for "kopia-Case" also returns "kopia-case".
// Reusing that one would put the repository in a folder the user did not name.
func TestGdriveStorageMockCreateFolderByNameIsCaseSensitive(t *testing.T) {
	ctx := testlogging.Context(t)
	d := newFakeDrive(t)

	const (
		wanted = "kopia-Case-Folder"
		decoy  = "kopia-case-folder"
	)

	decoyID := d.addRootFolder(decoy)

	opt := d.optionsForFolderName(t, testutil.TempDirectory(t), wanted)

	st, err := New(ctx, opt, true)
	require.NoError(t, err)

	defer st.Close(ctx) //nolint:errcheck

	require.NotEqual(t, decoyID, opt.FolderID)
	require.Equal(t, []string{opt.FolderID}, d.fileIDsNamed(wanted))
	require.Equal(t, []string{decoyID}, d.fileIDsNamed(decoy), "the differently-cased folder is untouched")
}

// TestGdriveStorageMockCreateFolderByNameRejected covers the option combinations
// that cannot mean anything, all of which must fail before any Drive call.
func TestGdriveStorageMockCreateFolderByNameRejected(t *testing.T) {
	cases := []struct {
		name     string
		opt      Options
		isCreate bool
		wantErr  string
	}{
		{"neither", Options{}, true, "folder-id must be specified"},
		{"blank name", Options{FolderName: "   "}, true, "must not be blank"},
		{"read-only", Options{FolderName: "kopia-ro"}, true, "read-only"},
		{"connect", Options{FolderName: "kopia-connect"}, false, "creating a repository"},
	}

	cases[2].opt.ReadOnly = true

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testlogging.Context(t)

			opt := tc.opt
			opt.Tuning.CacheDir = testutil.TempDirectory(t)

			_, err := New(ctx, &opt, tc.isCreate)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestGdriveStorageMockFolderNameIgnoredWhenIDIsSet is the steady state after a
// create-by-name: the configuration carries both fields, and every later
// connection must use the ID and never go looking for the name again.
func TestGdriveStorageMockFolderNameIgnoredWhenIDIsSet(t *testing.T) {
	ctx := testlogging.Context(t)

	d, st := newTunedMockStorage(t, func(o *Options) {
		o.FolderName = "some-other-folder-entirely"
	})

	require.Zero(t, d.requestCount(methodFilesCreate))
	require.Empty(t, d.fileIDsNamed("some-other-folder-entirely"))
	require.Equal(t, "Google Drive: "+d.rootFolderID, st.DisplayName())

	blobtesting.AssertConnectionInfoRoundTrips(ctx, t, st)
}

//
// ---------------------------------------------------------------- live suite
//

// TestGdriveStorageLive runs the full compatibility suite and the provider
// validation against a real Google Drive folder. It is skipped unless
// KOPIA_PROVIDER_TEST is set together with credentials.
func TestGdriveStorageLive(t *testing.T) {
	t.Parallel()
	testutil.ProviderTest(t)

	ctx := testlogging.Context(t)

	// Use a context that is canceled right after opening the storage, to prove
	// New() does not retain it.
	newCtx, cancel := context.WithCancel(ctx)

	opt := mustGetLiveOptionsOrSkip(t)
	testOpt := createLiveTestFolder(newCtx, t, opt)

	st, err := New(newCtx, testOpt, false)

	cancel()
	require.NoError(t, err)

	defer func() {
		require.NoError(t, st.Close(ctx))
		deleteLiveTestFolder(ctx, t, testOpt)
	}()

	requireHTTP2ToDrive(ctx, t, mustGdriveStorage(t, st))

	blobtesting.VerifyStorage(ctx, t, st, blob.PutOptions{})
	blobtesting.AssertConnectionInfoRoundTrips(ctx, t, st)

	started := clock.Now()

	require.NoError(t, providervalidation.ValidateProvider(ctx, st, blobtesting.TestValidationOptions))
	t.Logf("ValidateProvider took %v", clock.Now().Sub(started))

	if snap, ok := StatsFromStorage(st); ok {
		for method, ms := range snap.PerMethod {
			t.Logf("stats %-18s calls=%-6d errors=%-4d retries=%-4d up=%-10d down=%-10d", method, ms.Calls, ms.Errors, ms.Retries, ms.BytesUp, ms.BytesDown)
		}
	}
}

// TestGdriveLivePrefixNarrowing verifies against real Drive that pushing a long
// listing prefix into the query with `name contains` neither loses a blob that
// matches nor is trusted to exclude one that does not. The decoy blob embeds
// the prefix without starting with it, which `contains` matches and the
// client-side filter must reject.
//
// It is deliberately NOT named ...StorageLive..., so that it is not swept up by
// the routine live run.
func TestGdriveLivePrefixNarrowing(t *testing.T) {
	t.Parallel()
	testutil.ProviderTest(t)

	ctx := testlogging.Context(t)
	st, cleanup := newLiveTestStorage(ctx, t)

	defer cleanup()

	prefix := blob.ID("narrow" + strings.ReplaceAll(uuid.NewString(), "-", "") + "-")

	want := []blob.ID{prefix + "one", prefix + "two"}
	decoy := "x" + prefix + "three"

	for _, id := range append(append([]blob.ID{}, want...), decoy) {
		require.NoError(t, st.PutBlob(ctx, id, gather.FromSlice([]byte("x")), blob.PutOptions{}))
	}

	blobtesting.AssertListResultsIDs(ctx, t, st, prefix, want...)

	// ... and a prefix below the narrowing threshold still works, through the
	// client-side filter alone.
	blobtesting.AssertListResultsIDs(ctx, t, st, "x"+prefix[:1], decoy)
}

// TestGdriveLiveDeleteBenchmark measures the two delete paths against real
// Drive: bounded-parallel singles versus the batch endpoint, each with the
// production pacer and with the pacer relaxed so that the round trips rather
// than the rate limiter dominate.
//
// It reports rather than asserts (beyond correctness): the number this produces
// is the first real data point for the Phase 3 A/B, and the default stays where
// PHASE2_DESIGN.md put it until that measurement is made properly.
func TestGdriveLiveDeleteBenchmark(t *testing.T) {
	t.Parallel()
	testutil.ProviderTest(t)

	ctx := testlogging.Context(t)

	const blobCount = 200

	cases := []struct {
		name     string
		batch    bool
		minSleep int
	}{
		{name: "parallel-singles/default-pacer", batch: false},
		{name: "batch/default-pacer", batch: true},
		{name: "parallel-singles/relaxed-pacer", batch: false, minSleep: 1},
		{name: "batch/relaxed-pacer", batch: true, minSleep: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, cleanup := newLiveTestStorage(ctx, t, func(opt *Options) {
				opt.Tuning.UseBatchDelete = &tc.batch
				opt.Tuning.PacerMinSleepMS = tc.minSleep
			})

			defer cleanup()

			ids := make([]blob.ID, 0, blobCount)

			for i := range blobCount {
				id := blob.ID(fmt.Sprintf("bench-%04d", i))
				require.NoError(t, st.PutBlob(ctx, id, gather.FromSlice([]byte("x")), blob.PutOptions{}))

				ids = append(ids, id)
			}

			before, ok := StatsFromStorage(st)
			require.True(t, ok)

			started := clock.Now()

			require.NoError(t, blob.DeleteMultiple(ctx, st, ids, defaultDeleteParallelism))

			elapsed := clock.Now().Sub(started)

			after, _ := StatsFromStorage(st)

			t.Logf("%v: %v blobs in %v (%.1f blobs/s)", tc.name, blobCount, elapsed, float64(blobCount)/elapsed.Seconds())

			for _, m := range []string{methodFilesDelete, methodBatch, methodFilesList} {
				d, a := before.PerMethod[m], after.PerMethod[m]
				if a.Calls-d.Calls == 0 {
					continue
				}

				t.Logf("    %-14s calls=%-5d errors=%-3d retries=%-3d backoffs=%-3d",
					m, a.Calls-d.Calls, a.Errors-d.Errors, a.Retries-d.Retries, a.Backoffs-d.Backoffs)
			}

			// Correctness is not negotiable regardless of which path is faster.
			blobtesting.AssertListResultsIDs(ctx, t, st, "bench-")
		})
	}
}

// TestGdriveLiveAbsentBlobLatency measures what a lookup of a blob nobody wrote
// costs against real Drive, which is the number the escalation budget trades
// against.
func TestGdriveLiveAbsentBlobLatency(t *testing.T) {
	t.Parallel()
	testutil.ProviderTest(t)

	ctx := testlogging.Context(t)
	st, cleanup := newLiveTestStorage(ctx, t)

	defer cleanup()

	const samples = 5

	var total time.Duration

	for i := range samples {
		started := clock.Now()

		_, err := st.GetMetadata(ctx, blob.ID(fmt.Sprintf("absent-%v-%v", uuid.NewString(), i)))
		require.ErrorIs(t, err, blob.ErrBlobNotFound)

		total += clock.Now().Sub(started)
	}

	t.Logf("absent-blob GetMetadata: %v average over %v samples", total/samples, samples)
}

// newLiveTestStorage opens a storage on a fresh per-test Drive folder and
// returns it with the function that removes the folder again.
func newLiveTestStorage(ctx context.Context, t *testing.T, adjust ...func(*Options)) (blob.Storage, func()) {
	t.Helper()

	opt := createLiveTestFolder(ctx, t, mustGetLiveOptionsOrSkip(t))

	for _, a := range adjust {
		a(opt)
	}

	st, err := New(ctx, opt, false)
	require.NoError(t, err)

	return st, func() {
		require.NoError(t, st.Close(ctx))
		deleteLiveTestFolder(ctx, t, opt)
	}
}

// TestGdriveStorageLiveCreateFolderByName exercises --create-folder-name against
// real Google Drive: a uuid-named folder is made in the root of My Drive, is
// visible to files.get afterwards (which is the whole point - a folder made by
// hand there would NOT be, under the default drive.file scope), resolves to the
// same ID when the same name is created again, and is deleted on the way out so
// the account is left exactly as it was found.
//
// It is deliberately NOT named ...StorageLive..., so that it is not swept up by
// the routine live run: it writes into the root of My Drive rather than into
// KOPIA_GDRIVE_TEST_FOLDER.
func TestGdriveLiveCreateFolderByName(t *testing.T) {
	t.Parallel()
	testutil.ProviderTest(t)

	ctx := testlogging.Context(t)
	credentialsFile := getEnvVarOrSkip(t, testCredentialsEnv)

	folderName := "kopia-wp7-" + uuid.NewString()

	newOptions := func() *Options {
		return &Options{
			FolderName:                    folderName,
			ServiceAccountCredentialsFile: credentialsFile,
			Tuning:                        TuningOptions{CacheDir: testutil.TempDirectory(t)},
		}
	}

	opt := newOptions()

	st, err := New(ctx, opt, true)
	require.NoError(t, err)

	require.NotEmpty(t, opt.FolderID, "the created folder ID must be written back into the options")
	t.Logf("created Google Drive folder %q with ID %v", folderName, opt.FolderID)

	defer func() {
		require.NoError(t, st.Close(ctx))
		deleteLiveTestFolder(ctx, t, opt)
		t.Logf("deleted Google Drive folder %v", opt.FolderID)
	}()

	// files.get sees it, it is a folder, and it is in the root of My Drive.
	service, err := CreateDriveService(ctx, opt)
	require.NoError(t, err)

	f, err := service.Files.Get(opt.FolderID).SupportsAllDrives(true).Fields("id,name,mimeType,parents,trashed").Context(ctx).Do()
	require.NoError(t, err, "the created folder must be visible to files.get")
	require.Equal(t, folderName, f.Name)
	require.Equal(t, folderMimeType, f.MimeType)
	require.False(t, f.Trashed)

	// It has exactly one parent, the root of My Drive. The root's own ID cannot be
	// asserted against here: LIVE-VERIFIED, files.get on "root" is itself a 404
	// under the drive.file scope - the alias is accepted as a place to CREATE in,
	// but the root folder is not a file this application made and so cannot be
	// read. That asymmetry is exactly why this feature has to exist.
	require.Len(t, f.Parents, 1)
	t.Logf("parent (My Drive root) is %v", f.Parents[0])

	// and it is usable as a repository folder.
	require.NoError(t, st.PutBlob(ctx, "wp7-blob", gather.FromSlice([]byte("wp7!")), blob.PutOptions{}))
	blobtesting.AssertGetBlob(ctx, t, st, "wp7-blob", []byte("wp7!"))

	// creating the same name again adopts the same folder instead of making a
	// second one.
	again := newOptions()

	st2, err := New(ctx, again, true)
	require.NoError(t, err)

	defer st2.Close(ctx) //nolint:errcheck

	require.Equal(t, opt.FolderID, again.FolderID, "a second create of the same name must reuse the folder")
	blobtesting.AssertGetBlob(ctx, t, st2, "wp7-blob", []byte("wp7!"))
}

func TestGdriveStorageLiveCleanupOldData(t *testing.T) {
	t.Parallel()
	testutil.ProviderTest(t)

	ctx := testlogging.Context(t)

	st, err := New(ctx, mustGetLiveOptionsOrSkip(t), false)
	require.NoError(t, err)

	defer st.Close(ctx) //nolint:errcheck

	blobtesting.CleanupOldData(ctx, t, st, blobtesting.MinCleanupAge)
}

func TestGdriveStorageLiveInvalidFolder(t *testing.T) {
	t.Parallel()
	testutil.ProviderTest(t)

	ctx := testlogging.Context(t)

	opt := mustGetLiveOptionsOrSkip(t)
	opt.FolderID += "-no-such-folder"

	_, err := New(ctx, opt, false)
	require.Error(t, err, "unexpected success connecting to a non-existent Drive folder")
}

// requireHTTP2ToDrive checks that the production transport still negotiates
// HTTP/2 with Google - the health-check pings only exist on HTTP/2.
func requireHTTP2ToDrive(ctx context.Context, t *testing.T, s *gdriveStorage) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.service.BasePath+"about?fields=user(kind)", http.NoBody)
	require.NoError(t, err)

	resp, err := s.httpClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, resp.ProtoMajor, "Drive must be reached over HTTP/2, got %v", resp.Proto)
}

func mustGetLiveOptionsOrSkip(t *testing.T) *Options {
	t.Helper()

	folderID := getEnvVarOrSkip(t, testFolderEnv)
	credentialsFile := getEnvVarOrSkip(t, testCredentialsEnv)

	return &Options{
		FolderID:                      folderID,
		ServiceAccountCredentialsFile: credentialsFile,
		Tuning:                        TuningOptions{CacheDir: testutil.TempDirectory(t)},
	}
}

func getEnvVarOrSkip(t *testing.T, name string) string {
	t.Helper()

	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%q is not set", name)
	}

	return v
}

// createLiveTestFolder creates a uuid-named subfolder of the configured folder
// and returns Options pointing at it, so concurrent runs never share state.
// Under the drive.file scope the subfolder must be created by this application,
// which it is.
func createLiveTestFolder(ctx context.Context, t *testing.T, opt *Options) *Options {
	t.Helper()

	service, err := CreateDriveService(ctx, opt)
	require.NoError(t, err)

	folder, err := service.Files.Create(&drive.File{
		Name:     uuid.NewString(),
		Parents:  []string{opt.FolderID},
		MimeType: folderMimeType,
	}).SupportsAllDrives(true).Fields("id").Context(ctx).Do()
	require.NoError(t, err, "unable to create the per-test Drive folder")

	newOpt := *opt
	newOpt.FolderID = folder.Id

	return &newOpt
}

func deleteLiveTestFolder(ctx context.Context, t *testing.T, opt *Options) {
	t.Helper()

	service, err := CreateDriveService(ctx, opt)
	require.NoError(t, err)

	require.NoError(t, service.Files.Delete(opt.FolderID).SupportsAllDrives(true).Context(ctx).Do())
}
