//go:build !no_extra_providers

package gdrive

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// newTransportTestServer starts an httptest server and returns a client whose
// transport is instrumented, together with the stats it writes into.
func newTransportTestServer(t *testing.T, h http.HandlerFunc) (*http.Client, *TransportStats, string) {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	stats := NewTransportStats()
	cli := &http.Client{Transport: newInstrumentedTransport(srv.Client().Transport, stats)}

	return cli, stats, srv.URL
}

func doTransportRequest(t *testing.T, cli *http.Client, verb, url string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), verb, url, body)
	require.NoError(t, err)

	resp, err := cli.Do(req)
	require.NoError(t, err)

	return resp
}

// doTransportRequestAndDrain issues a request, reads its body to completion and
// closes it.
func doTransportRequestAndDrain(t *testing.T, cli *http.Client, verb, url string) {
	t.Helper()

	resp := doTransportRequest(t, cli, verb, url, nil)

	_, err := io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

func drainTransportResponse(t *testing.T, resp *http.Response) {
	t.Helper()

	_, err := io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

func TestTransportMethodAttribution(t *testing.T) {
	cases := []struct {
		name    string
		verb    string
		path    string
		wantKey string
	}{
		{"list", http.MethodGet, "/drive/v3/files?q=trashed%3Dfalse&pageSize=1000", methodFilesList},
		{"create", http.MethodPost, "/drive/v3/files", methodFilesCreate},
		{"getMetadata", http.MethodGet, "/drive/v3/files/1AbCdEf", methodFilesGet},
		{"getMetadataWithFields", http.MethodGet, "/drive/v3/files/1AbCdEf?fields=name%2Cid", methodFilesGet},
		{"getMedia", http.MethodGet, "/drive/v3/files/1AbCdEf?alt=media", methodFilesGetMedia},
		{"headMedia", http.MethodHead, "/drive/v3/files/1AbCdEf?alt=media", methodFilesGetMedia},
		{"update", http.MethodPatch, "/drive/v3/files/1AbCdEf", methodFilesUpdate},
		{"updatePut", http.MethodPut, "/drive/v3/files/1AbCdEf", methodFilesUpdate},
		{"delete", http.MethodDelete, "/drive/v3/files/1AbCdEf", methodFilesDelete},
		{"uploadMedia", http.MethodPost, "/upload/drive/v3/files?uploadType=media", methodUploadSimple},
		{"uploadMultipart", http.MethodPost, "/upload/drive/v3/files?uploadType=multipart", methodUploadSimple},
		{"uploadNoType", http.MethodPost, "/upload/drive/v3/files", methodUploadSimple},
		{"uploadResumableInit", http.MethodPost, "/upload/drive/v3/files?uploadType=resumable", methodUploadResumable},
		{"uploadResumableChunk", http.MethodPut, "/upload/drive/v3/files?uploadType=resumable&upload_id=Zm9v", methodUploadResumable},
		{"uploadResumableSessionURI", http.MethodPut, "/upload/drive/v3/files?upload_id=Zm9v", methodUploadResumable},
		{"uploadResumableUpdate", http.MethodPatch, "/upload/drive/v3/files/1AbCdEf?uploadType=resumable", methodUploadResumable},
		{"batchGlobal", http.MethodPost, "/batch", methodBatch},
		{"batchDrive", http.MethodPost, "/batch/drive/v3", methodBatch},
		{"about", http.MethodGet, "/drive/v3/about?fields=storageQuota", methodOther},
		{"generateIds", http.MethodGet, "/drive/v3/files/generateIds?count=10", methodOther},
		{"subResource", http.MethodGet, "/drive/v3/files/1AbCdEf/permissions", methodOther},
		{"unknownPath", http.MethodGet, "/somewhere/else", methodOther},
		{"listTrailingSlash", http.MethodGet, "/drive/v3/files/", methodFilesList},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			doTransportRequestAndDrain(t, cli, tc.verb, base+tc.path)

			snap := stats.Snapshot()
			require.Len(t, snap.PerMethod, 1)
			require.Contains(t, snap.PerMethod, tc.wantKey)
			require.Equal(t, int64(1), snap.PerMethod[tc.wantKey].Calls)
			require.Zero(t, snap.PerMethod[tc.wantKey].Errors)
		})
	}
}

func TestTransportNeverRecordsQueryValues(t *testing.T) {
	const secret = "SUPER-SECRET-ACCESS-TOKEN"

	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	doTransportRequestAndDrain(t, cli, http.MethodGet, base+"/drive/v3/files/1AbCdEf?access_token="+secret+"&alt=media")

	buf, err := json.Marshal(stats.Snapshot())
	require.NoError(t, err)
	require.NotContains(t, string(buf), secret)
	require.Contains(t, string(buf), methodFilesGetMedia)
}

func TestTransportBytesUpKnownLength(t *testing.T) {
	const payload = "hello-drive-payload"

	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		w.WriteHeader(http.StatusOK)
	})

	resp := doTransportRequest(t, cli, http.MethodPost, base+"/upload/drive/v3/files?uploadType=media", strings.NewReader(payload))

	_, err := io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	snap := stats.Snapshot()
	require.Equal(t, int64(len(payload)), snap.PerMethod[methodUploadSimple].BytesUp)
}

func TestTransportBytesUpUnknownLength(t *testing.T) {
	const payload = "streamed-chunk-body"

	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		w.WriteHeader(http.StatusOK)
	})

	// io.NopCloser hides the concrete reader type, so net/http cannot determine
	// ContentLength and sends the body chunked.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		base+"/upload/drive/v3/files?upload_id=Zm9v", io.NopCloser(strings.NewReader(payload)))
	require.NoError(t, err)
	require.LessOrEqual(t, req.ContentLength, int64(0))

	resp, err := cli.Do(req)
	require.NoError(t, err)
	drainTransportResponse(t, resp)

	snap := stats.Snapshot()
	require.Equal(t, int64(len(payload)), snap.PerMethod[methodUploadResumable].BytesUp)
}

func TestTransportBytesDownFullRead(t *testing.T) {
	payload := strings.Repeat("x", 4096)

	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	})

	doTransportRequestAndDrain(t, cli, http.MethodGet, base+"/drive/v3/files/1AbCdEf?alt=media")

	snap := stats.Snapshot()
	require.Equal(t, int64(len(payload)), snap.PerMethod[methodFilesGetMedia].BytesDown)
}

func TestTransportBytesDownPartialReadThenClose(t *testing.T) {
	const prefixLen = 10

	payload := strings.Repeat("y", 65536)

	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	})

	resp := doTransportRequest(t, cli, http.MethodGet, base+"/drive/v3/files/1AbCdEf?alt=media", nil)

	buf := make([]byte, prefixLen)
	_, err := io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	snap := stats.Snapshot()
	require.Equal(t, int64(prefixLen), snap.PerMethod[methodFilesGetMedia].BytesDown)
}

func TestTransportBytesDownCloseWithoutRead(t *testing.T) {
	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("z", 1024))
	})

	resp := doTransportRequest(t, cli, http.MethodGet, base+"/drive/v3/files/1AbCdEf?alt=media", nil)
	require.NoError(t, resp.Body.Close())

	snap := stats.Snapshot()
	require.Zero(t, snap.PerMethod[methodFilesGetMedia].BytesDown)
	require.Equal(t, int64(1), snap.PerMethod[methodFilesGetMedia].Calls)
}

func TestTransportLatencyBuckets(t *testing.T) {
	const slowRequestDelay = 25 * time.Millisecond

	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/drive/v3/files/") {
			time.Sleep(slowRequestDelay)
		}

		w.WriteHeader(http.StatusOK)
	})

	const fastCalls = 3

	for range fastCalls {
		doTransportRequestAndDrain(t, cli, http.MethodGet, base+"/drive/v3/files")
	}

	doTransportRequestAndDrain(t, cli, http.MethodGet, base+"/drive/v3/files/1AbCdEf")

	snap := stats.Snapshot()
	require.Len(t, snap.LatencyBucketsMS, len(latencyBucketBoundsMS))
	require.Equal(t, latencyBucketBoundsMS[:], snap.LatencyBucketsMS)

	fast := snap.PerMethod[methodFilesList]
	require.Len(t, fast.LatencyMS, len(snap.LatencyBucketsMS)+1)
	require.Equal(t, int64(fastCalls), sumInt64(fast.LatencyMS))

	slow := snap.PerMethod[methodFilesGet]
	require.Equal(t, int64(1), sumInt64(slow.LatencyMS))

	// the handler slept 25ms, so the observation must land in a bucket whose
	// upper bound is at least 50ms (the first bound > 25).
	idx := firstNonZero(slow.LatencyMS)
	require.GreaterOrEqual(t, idx, 0)
	require.Less(t, idx, len(snap.LatencyBucketsMS))
	require.GreaterOrEqual(t, snap.LatencyBucketsMS[idx], int64(50))
	require.GreaterOrEqual(t, slow.LatencySumMS, int64(20))
}

func sumInt64(v []int64) int64 {
	var total int64
	for _, x := range v {
		total += x
	}

	return total
}

func firstNonZero(v []int64) int {
	for i, x := range v {
		if x > 0 {
			return i
		}
	}

	return -1
}

func TestTransportHTTPErrorCounting(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantReason string
		wantErrors int64
	}{
		{"ok", http.StatusOK, "", 0},
		{"partialContent", http.StatusPartialContent, "", 0},
		{"resumeIncomplete", http.StatusPermanentRedirect, "", 0},
		{"badRequest", http.StatusBadRequest, "http_400", 1},
		{"notFound", http.StatusNotFound, "http_404", 1},
		{"rangeNotSatisfiable", http.StatusRequestedRangeNotSatisfiable, "http_416", 1},
		{"tooManyRequests", http.StatusTooManyRequests, "http_429", 1},
		{"serverError", http.StatusInternalServerError, "http_500", 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})

			// no redirect following, so a 308 is observed as-is.
			cli.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			}

			resp := doTransportRequest(t, cli, http.MethodGet, base+"/drive/v3/files/1AbCdEf", nil)
			require.Equal(t, tc.status, resp.StatusCode)
			drainTransportResponse(t, resp)

			snap := stats.Snapshot()
			require.Equal(t, tc.wantErrors, snap.PerMethod[methodFilesGet].Errors)

			if tc.wantReason == "" {
				require.Empty(t, snap.ErrorsByReason)
			} else {
				require.Equal(t, map[string]int64{tc.wantReason: 1}, snap.ErrorsByReason)
			}
		})
	}
}

func TestTransportResumableStatusProbeWithoutBody(t *testing.T) {
	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Range", "bytes=0-524287")
		w.WriteHeader(http.StatusPermanentRedirect)
	})

	cli.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp := doTransportRequest(t, cli, http.MethodPut, base+"/upload/drive/v3/files?upload_id=Zm9v", nil)
	require.Equal(t, http.StatusPermanentRedirect, resp.StatusCode)
	require.Equal(t, "bytes=0-524287", resp.Header.Get("Range"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Empty(t, body)
	require.NoError(t, resp.Body.Close())

	snap := stats.Snapshot()
	require.Zero(t, snap.PerMethod[methodUploadResumable].Errors)
	require.Zero(t, snap.PerMethod[methodUploadResumable].BytesDown)
	require.Empty(t, snap.ErrorsByReason)
}

var errTransportBoom = errors.New("boom")

func TestTransportRoundTripErrorCounting(t *testing.T) {
	stats := NewTransportStats()
	rt := newInstrumentedTransport(roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errTransportBoom
	}), stats)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, "https://www.googleapis.com/drive/v3/files/1AbCdEf", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req) //nolint:bodyclose
	require.Nil(t, resp)
	require.ErrorIs(t, err, errTransportBoom)

	snap := stats.Snapshot()
	require.Equal(t, int64(1), snap.PerMethod[methodFilesDelete].Calls)
	require.Equal(t, int64(1), snap.PerMethod[methodFilesDelete].Errors)
	require.Equal(t, map[string]int64{reasonTransportError: 1}, snap.ErrorsByReason)
	require.Equal(t, int64(1), sumInt64(snap.PerMethod[methodFilesDelete].LatencyMS))
}

func TestTransportStatsRecorderMethods(t *testing.T) {
	stats := NewTransportStats()

	stats.RecordRetry("files.list")
	stats.RecordRetry("files.list")
	stats.RecordError("files.list", "rate_limit")
	stats.RecordError("files.get", "rate_limit")
	stats.RecordError("files.get", "not_found")
	stats.RecordBackoff("files.list", 250*time.Millisecond)
	stats.RecordBackoff("files.list", 750*time.Millisecond)
	stats.RecordBackoff("files.get", 0)
	stats.RecordError("files.get", "")

	snap := stats.Snapshot()

	list := snap.PerMethod["files.list"]
	require.Equal(t, int64(2), list.Retries)
	require.Equal(t, int64(1), list.Errors)
	require.Equal(t, int64(2), list.Backoffs)
	require.Equal(t, int64(1000), list.BackoffMS)
	require.Zero(t, list.Calls)

	get := snap.PerMethod["files.get"]
	require.Equal(t, int64(3), get.Errors)
	require.Equal(t, int64(1), get.Backoffs)
	require.Zero(t, get.BackoffMS)

	require.Equal(t, map[string]int64{
		"rate_limit":  2,
		"not_found":   1,
		reasonUnknown: 1,
	}, snap.ErrorsByReason)
}

func TestTransportStatsRecordPacedWait(t *testing.T) {
	stats := NewTransportStats()

	stats.RecordPacedWait(methodFilesList, 120*time.Millisecond)
	stats.RecordPacedWait(methodFilesList, 80*time.Millisecond)
	stats.RecordPacedWait(methodFilesDelete, 250*time.Millisecond)

	// A call that did not have to wait is not evidence of pacing: neither the
	// count nor the duration may move.
	stats.RecordPacedWait(methodFilesGet, 0)
	stats.RecordPacedWait(methodFilesGet, -time.Second)

	snap := stats.Snapshot()

	require.Equal(t, int64(2), snap.PerMethod[methodFilesList].PacedWaits)
	require.Equal(t, int64(200), snap.PerMethod[methodFilesList].PacedWaitMS)
	require.Equal(t, int64(1), snap.PerMethod[methodFilesDelete].PacedWaits)
	require.Equal(t, int64(250), snap.PerMethod[methodFilesDelete].PacedWaitMS)
	require.NotContains(t, snap.PerMethod, methodFilesGet)

	// The snapshot totals are the sums of the per-method counters.
	require.Equal(t, int64(3), snap.PacedWaits)
	require.Equal(t, int64(450), snap.PacedWaitMS)

	// Paced waits are the error-free half of the pacer and must not leak into
	// the backoff counters.
	require.Zero(t, snap.PerMethod[methodFilesList].Backoffs)
	require.Zero(t, snap.PerMethod[methodFilesList].BackoffMS)
	require.Empty(t, snap.ErrorsByReason)

	stats.Reset()

	after := stats.Snapshot()
	require.Zero(t, after.PacedWaits)
	require.Zero(t, after.PacedWaitMS)
}

func TestTransportStatsSeparatesPacerAndHTTPReasons(t *testing.T) {
	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	doTransportRequestAndDrain(t, cli, http.MethodGet, base+"/drive/v3/files")

	stats.RecordError(methodFilesList, "retryable_backoff")

	snap := stats.Snapshot()
	require.Equal(t, map[string]int64{
		"http_429":          1,
		"retryable_backoff": 1,
	}, snap.ErrorsByReason)
	require.Equal(t, int64(2), snap.PerMethod[methodFilesList].Errors)
}

func TestTransportStatsReset(t *testing.T) {
	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	doTransportRequestAndDrain(t, cli, http.MethodGet, base+"/drive/v3/files")
	stats.RecordRetry(methodFilesList)

	before := stats.Snapshot()
	require.NotEmpty(t, before.PerMethod)
	require.NotEmpty(t, before.ErrorsByReason)

	stats.Reset()

	after := stats.Snapshot()
	require.Empty(t, after.PerMethod)
	require.Empty(t, after.ErrorsByReason)
	require.GreaterOrEqual(t, after.StartTime.UnixNano(), before.StartTime.UnixNano())

	// counters keep working after a reset.
	doTransportRequestAndDrain(t, cli, http.MethodGet, base+"/drive/v3/files")

	again := stats.Snapshot()
	require.Equal(t, int64(1), again.PerMethod[methodFilesList].Calls)
	require.Equal(t, map[string]int64{"http_404": 1}, again.ErrorsByReason)
}

func TestTransportStatsSnapshotJSONRoundTrip(t *testing.T) {
	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "0123456789")
	})

	doTransportRequestAndDrain(t, cli, http.MethodGet, base+"/drive/v3/files/1AbCdEf?alt=media")
	doTransportRequestAndDrain(t, cli, http.MethodGet, base+"/drive/v3/files")
	stats.RecordError(methodFilesList, "some_reason")
	stats.RecordBackoff(methodFilesList, 3*time.Second)
	stats.RecordPacedWait(methodFilesList, 150*time.Millisecond)

	snap := stats.Snapshot()

	buf, err := json.Marshal(snap)
	require.NoError(t, err)

	var decoded TransportStatsSnapshot

	require.NoError(t, json.Unmarshal(buf, &decoded))
	require.WithinDuration(t, snap.StartTime, decoded.StartTime, 0)
	require.WithinDuration(t, snap.SnapshotTime, decoded.SnapshotTime, 0)

	decoded.StartTime, decoded.SnapshotTime = snap.StartTime, snap.SnapshotTime
	require.Equal(t, snap, decoded)

	// spot check the shape a benchmark harness relies on.
	require.Equal(t, int64(10), decoded.PerMethod[methodFilesGetMedia].BytesDown)
	require.Equal(t, int64(3000), decoded.PerMethod[methodFilesList].BackoffMS)
	require.Equal(t, int64(1), decoded.PerMethod[methodFilesList].PacedWaits)
	require.Equal(t, int64(150), decoded.PerMethod[methodFilesList].PacedWaitMS)
	require.Equal(t, int64(1), decoded.PacedWaits)
	require.Equal(t, int64(150), decoded.PacedWaitMS)
	require.Len(t, decoded.PerMethod[methodFilesList].LatencyMS, len(decoded.LatencyBucketsMS)+1)
}

func TestTransportConcurrentRoundTripAndSnapshot(t *testing.T) {
	const (
		workers           = 8
		requestsPerWorker = 25
		payload           = "0123456789abcdef"
	)

	cli, stats, base := newTransportTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deadbeef") {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		_, _ = io.WriteString(w, payload)
	})

	urls := []string{
		base + "/drive/v3/files",
		base + "/drive/v3/files/1AbCdEf?alt=media",
		base + "/drive/v3/files/deadbeef",
		base + "/upload/drive/v3/files?uploadType=resumable",
		base + "/batch/drive/v3",
	}

	var (
		wg   sync.WaitGroup
		stop = make(chan struct{})
	)

	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, err := json.Marshal(stats.Snapshot())
				assertNoErr(t, err)
			}
		}
	})

	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				stats.RecordRetry(methodFilesList)
				stats.RecordError(methodFilesGet, "classified")
				stats.RecordBackoff(methodFilesGet, time.Millisecond)
				stats.RecordPacedWait(methodFilesList, time.Millisecond)
			}
		}
	})

	for worker := range workers {
		wg.Go(func() {
			for j := range requestsPerWorker {
				u := urls[(worker+j)%len(urls)]

				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, http.NoBody)
				if !assertNoErr(t, err) {
					return
				}

				resp, err := cli.Do(req)
				if !assertNoErr(t, err) {
					return
				}

				_, err = io.Copy(io.Discard, resp.Body)
				assertNoErr(t, err)
				assertNoErr(t, resp.Body.Close())
			}
		})
	}

	// wait for the request workers, then stop the background goroutines.
	done := make(chan struct{})

	go func() {
		defer close(done)

		wg.Wait()
	}()

	// stop the two infinite goroutines once the workers have had a chance to run.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	<-done

	snap := stats.Snapshot()

	var totalCalls int64
	for _, ms := range snap.PerMethod {
		totalCalls += ms.Calls
	}

	require.Equal(t, int64(workers*requestsPerWorker), totalCalls)
	require.Positive(t, snap.PerMethod[methodFilesGetMedia].BytesDown)
	require.Positive(t, snap.ErrorsByReason["http_404"])
	require.Positive(t, snap.ErrorsByReason["classified"])
}

func assertNoErr(t *testing.T, err error) bool {
	t.Helper()

	if err != nil {
		t.Error(err)
		return false
	}

	return true
}

func TestTransportNilBaseAndNilStats(t *testing.T) {
	require.NotNil(t, newInstrumentedTransport(nil, nil))
	require.NotNil(t, newInstrumentedTransport(nil, NewTransportStats()))
}
