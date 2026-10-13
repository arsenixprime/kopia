//go:build !no_extra_providers

package gdrive

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"

	"github.com/kopia/kopia/internal/blobtesting"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/internal/timetrack"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/gdrive/gdriveerr"
)

// stallMechanisms are the two transport timeouts a stall on a plain HTTP/1.1
// connection can trip; each case leaves the other one out of reach, so a
// passing case proves that mechanism on its own.
func stallMechanisms() map[string]transportTimeouts {
	header := relaxedTimeouts()
	header.responseHeader = stallTestTimeout

	idle := relaxedTimeouts()
	idle.ioIdle = stallTestTimeout

	return map[string]transportTimeouts{
		"response-header-timeout": header,
		"io-idle-timeout":         idle,
	}
}

// newStallTestStorage opens a storage against a fake Drive through the
// production HTTP transport with the given timeouts.
func newStallTestStorage(t *testing.T, timeouts transportTimeouts, adjust func(*Options)) (*fakeDrive, *gdriveStorage) {
	t.Helper()

	ctx := testlogging.Context(t)
	d := newFakeDrive(t)
	d.useProductionTransport(timeouts)

	// Every request on a fresh connection: net/http transparently replays an
	// idempotent request that fails reading the response on a REUSED
	// connection, which would hide some stalls from the layers under test
	// (and is welcome in production).
	d.server.Config.SetKeepAlivesEnabled(false)

	opt := d.options(t, testutil.TempDirectory(t))
	adjust(opt)

	st, err := New(ctx, opt, false)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, st.Close(ctx)) })

	return d, mustGdriveStorage(t, st)
}

func isMethod(method string) func(*http.Request) bool {
	return func(r *http.Request) bool { return attributeMethod(r) == method }
}

// TestGdriveStorageMockStalledRequestIsRetried: a metadata request and a
// listing whose server never answers fail within the stall bound and succeed
// on the pacer's next attempt.
func TestGdriveStorageMockStalledRequestIsRetried(t *testing.T) {
	for name, timeouts := range stallMechanisms() {
		t.Run(name, func(t *testing.T) {
			ctx := testlogging.Context(t)
			d, s := newStallTestStorage(t, timeouts, func(*Options) {})

			const blobID = blob.ID("stalled-blob")

			require.NoError(t, s.PutBlob(ctx, blobID, gather.FromSlice([]byte("hello")), blob.PutOptions{}))

			// files.get: the ID is cached, so GetMetadata goes straight to it.
			d.stallNext(isMethod(methodFilesGet), 0)

			timer := timetrack.StartTimer()

			bm, err := s.GetMetadata(ctx, blobID)
			require.NoError(t, err)
			require.Equal(t, int64(5), bm.Length)
			require.GreaterOrEqual(t, timer.Elapsed(), stallTestTimeout, "the stall must have been waited out")
			require.Less(t, timer.Elapsed(), stallTestDeadline)

			// files.list
			d.stallNext(isMethod(methodFilesList), 0)

			timer = timetrack.StartTimer()

			blobtesting.AssertListResultsIDs(ctx, t, s, "stalled", blobID)
			require.GreaterOrEqual(t, timer.Elapsed(), stallTestTimeout)
			require.Less(t, timer.Elapsed(), stallTestDeadline)

			snap := s.StatsSnapshot()
			require.Positive(t, snap.PerMethod[methodFilesGet].Retries, "the stalled files.get must have been retried by the pacer")
			require.Positive(t, snap.PerMethod[methodFilesList].Retries, "the stalled files.list must have been retried by the pacer")
		})
	}
}

// TestGdriveStorageMockStallClassification pins the classification of the real
// errors the production transport produces for a stall: retryable while the
// caller's context is live, the caller's own error once it is not.
func TestGdriveStorageMockStallClassification(t *testing.T) {
	for name, timeouts := range stallMechanisms() {
		t.Run(name, func(t *testing.T) {
			ctx := testlogging.Context(t)
			d, s := newStallTestStorage(t, timeouts, func(*Options) {})

			require.NoError(t, s.PutBlob(ctx, "b", gather.FromSlice([]byte("x")), blob.PutOptions{}))

			fileID, ok := s.idCache.Get("b")
			require.True(t, ok)

			d.stallNext(isMethod(methodFilesGet), 0)

			_, rawErr := s.files.Get(fileID).Fields("id").Context(ctx).Do()
			require.Error(t, rawErr)

			classified := gdriveerr.ClassifyContext(ctx, rawErr, methodFilesGet, "b")

			de, ok := gdriveerr.AsDriveError(classified)
			require.True(t, ok, "a stall with a live caller context must be classified, got %v", classified)
			require.Equal(t, gdriveerr.RetryableResume, de.Disposition())
			require.NotErrorIs(t, classified, context.DeadlineExceeded, "a stall must not read as the caller's deadline")
			require.NotErrorIs(t, classified, context.Canceled)

			if errors.Is(rawErr, context.DeadlineExceeded) {
				// net/http's ResponseHeaderTimeout claims to be a context deadline;
				// the same error under a context that is done belongs to the caller.
				canceled, cancel := context.WithCancel(ctx)
				cancel()

				require.Equal(t, rawErr, gdriveerr.ClassifyContext(canceled, rawErr, methodFilesGet, "b"))
			}
		})
	}
}

// TestGdriveStorageMockCallerCancelIsNotRetried: the caller's own deadline
// expiring while a request hangs is returned as the caller's error, not
// retried.
func TestGdriveStorageMockCallerCancelIsNotRetried(t *testing.T) {
	d, s := newStallTestStorage(t, relaxedTimeouts(), func(*Options) {})

	ctx := testlogging.Context(t)
	require.NoError(t, s.PutBlob(ctx, "b", gather.FromSlice([]byte("x")), blob.PutOptions{}))

	before := d.requestCount(methodFilesGet)

	d.stallNext(isMethod(methodFilesGet), 0)

	short, cancel := context.WithTimeout(ctx, stallTestTimeout)
	defer cancel()

	_, err := s.GetMetadata(short, "b")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	_, classified := gdriveerr.AsDriveError(err)
	require.False(t, classified, "the caller's own deadline must pass through unclassified")
	require.Equal(t, 1, d.requestCount(methodFilesGet)-before, "exactly one stalled attempt, no retry")
}

// TestGdriveStorageMockStalledChunkResumesSession stalls one chunk of a
// resumable upload in the middle of its body and checks that the upload
// recovers by re-sending that chunk on the SAME session, not by starting the
// blob over.
func TestGdriveStorageMockStalledChunkResumesSession(t *testing.T) {
	for name, timeouts := range stallMechanisms() {
		t.Run(name, func(t *testing.T) {
			ctx := testlogging.Context(t)
			d, s := newStallTestStorage(t, timeouts, func(o *Options) {
				o.Tuning.SimpleUploadCutoffMB = 1
				o.Tuning.UploadChunkSizeMB = 1
			})

			const (
				chunk       = 1 << 20
				stalledPart = 2 // 1-based index of the chunk request that hangs
			)

			content := make([]byte, 3*chunk+chunk/2) // four chunks, the last one partial
			for i := range content {
				content[i] = byte(i * 7)
			}

			chunkRequests := 0

			d.stallNext(func(r *http.Request) bool {
				if r.URL.Query().Get("upload_id") == "" {
					return false
				}

				chunkRequests++

				return chunkRequests == stalledPart
			}, chunk/4)

			timer := timetrack.StartTimer()

			require.NoError(t, s.PutBlob(ctx, "resumed-blob", gather.FromSlice(content), blob.PutOptions{}))
			require.Less(t, timer.Elapsed(), stallTestDeadline)

			blobtesting.AssertGetBlob(ctx, t, s, "resumed-blob", content)

			sessions, chunkBytes := d.resumableStats()
			require.Equal(t, 1, sessions, "a resumed upload keeps its session; a restart would have opened a second one")
			require.GreaterOrEqual(t, chunkBytes, int64(len(content)))
			require.Less(t, chunkBytes, int64(2*len(content)), "only the stalled chunk may be re-sent, not the whole blob")

			// session start + 4 chunks + the stalled attempt of chunk 2
			require.GreaterOrEqual(t, d.requestCount(methodUploadResumable), 6)
		})
	}
}

// TestGdriveStorageMockStalledSimpleUploadIsRetried covers the non-resumable
// upload path, which has no session to resume and is retried whole by the
// pacer.
func TestGdriveStorageMockStalledSimpleUploadIsRetried(t *testing.T) {
	timeouts := relaxedTimeouts()
	timeouts.responseHeader = stallTestTimeout

	ctx := testlogging.Context(t)
	d, s := newStallTestStorage(t, timeouts, func(*Options) {})

	d.stallNext(isMethod(methodUploadSimple), 0)

	content := []byte("simple upload that stalls once")

	require.NoError(t, s.PutBlob(ctx, "simple-blob", gather.FromSlice(content), blob.PutOptions{}))
	blobtesting.AssertGetBlob(ctx, t, s, "simple-blob", content)
	require.Len(t, d.fileIDsNamed("simple-blob"), 1, "the retry must not leave a duplicate behind")
	require.Equal(t, 2, d.requestCount(methodUploadSimple))
}

// TestMediaOptionsChunkRetryDeadlineOutlastsStallDetection: googleapi only
// re-sends a failed chunk on the same session while the chunk's retry deadline
// (32s by default, counted from the chunk's first attempt) has not passed.
// Every production stall bound is longer than that, so without the override a
// stall would always restart the whole blob on a new session.
func TestMediaOptionsChunkRetryDeadlineOutlastsStallDetection(t *testing.T) {
	_, s := newMockStorage(t)
	gs := mustGdriveStorage(t, s)

	opts := googleapi.ProcessMediaOptions(gs.mediaOptions(int(gs.tuning.simpleUploadCutoff)))

	to := gs.tuning.timeouts
	slowestDetection := max(to.ioIdle, to.responseHeader, to.http2ReadIdle+to.http2Ping)

	require.Equal(t, 10*time.Minute, opts.ChunkRetryDeadline)
	require.Greater(t, opts.ChunkRetryDeadline, slowestDetection)
}
