//go:build !no_extra_providers

package gdrive

import (
	"context"
	"net/http"
	"os"
	"strings"
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

	ctx := testlogging.Context(t)
	d := newFakeDrive(t)
	opt := d.options(t, testutil.TempDirectory(t))

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
