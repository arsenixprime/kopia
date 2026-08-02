//go:build !no_extra_providers

package gdrive

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/timetrack"
)

// Drive API method keys. Every HTTP request issued by the Drive service is
// attributed to exactly one of these from its URL path + verb, and all counters
// are bucketed by them.
//
// The quota-unit (QU) costs quoted below come from the Google Drive API usage
// limits page as captured in RECON.md §C.2 (the quota-unit model that replaced
// request counting on 2026-05-01). Budgets are 325,000 QU per minute per user
// per project and 1,000,000 QU per minute per project, so an operator can turn
// a stats dump into a quota estimate with
//
//	sum(PerMethod[key].Calls * cost(key))
//
// and compare it against those ceilings for the elapsed
// SnapshotTime-StartTime window.
const (
	// methodFilesList is files.list — "List items" class, 100 QU per page.
	// At 325,000 QU/min/user that is ~3,250 pages/min (~54/s).
	methodFilesList = "files.list"

	// methodFilesGet is a files.get metadata read — "Read items" class, 5 QU.
	// ~65,000/min/user (~1,083/s).
	methodFilesGet = "files.get"

	// methodFilesGetMedia is files.get with alt=media, i.e. a content download —
	// "Download items" class, 200 QU, ~1,625/min/user (~27/s). Downloads also
	// draw on the separate 1 TB/day/project egress cap.
	methodFilesGetMedia = "files.get.media"

	// methodFilesCreate is a metadata-only files.create — assumed "Edit items"
	// class, 50 QU (~6,500/min/user). UNVERIFIED: RECON.md §C.2 records that
	// Google's cost table names files.update but not files.create/files.delete;
	// 50 is the working assumption.
	methodFilesCreate = "files.create"

	// methodFilesUpdate is files.update (PATCH) — "Edit items" class, 50 QU.
	methodFilesUpdate = "files.update"

	// methodFilesDelete is files.delete — assumed "Edit items" class, 50 QU.
	// UNVERIFIED, same caveat as methodFilesCreate.
	methodFilesDelete = "files.delete"

	// methodUploadSimple is a media/multipart upload through /upload/drive/v3 —
	// edit-class, 50 QU assumed, one call per blob. UNVERIFIED.
	methodUploadSimple = "upload.simple"

	// methodUploadResumable covers both the session-initiating request and every
	// subsequent chunk PUT/status probe on a resumable session URI. The
	// per-chunk cost is UNVERIFIED (RECON.md §C.2 does not break upload
	// protocols out); assume edit-class 50 QU per request, which makes chunk
	// size a quota knob and not just an RTT knob.
	methodUploadResumable = "upload.resumable"

	// methodBatch is the /batch endpoint envelope. The envelope itself is free;
	// each sub-request inside it is charged at its own method's cost, and those
	// sub-requests are NOT visible to this transport (they are body parts, which
	// are never inspected). Multiply Calls by the batch size to estimate.
	methodBatch = "batch"

	// methodOther is everything else (about.get, changes, generateIds, ...) —
	// "Other actions" class, 5 QU.
	methodOther = "other"
)

// URL path prefixes used for method attribution. Matching is on the path only;
// query values are inspected but never recorded (they can carry access tokens
// and upload session IDs).
const (
	filesPath       = "/drive/v3/files"
	uploadFilesPath = "/upload/drive/v3/files"
	batchPath       = "/batch"
)

// Reasons recorded by the transport itself into ErrorsByReason. Transport
// reasons are always either "transport_error" or "http_<status>", which keeps
// them distinguishable from the free-form reason strings the pacer reports
// through RecordError.
const (
	reasonTransportError = "transport_error"
	reasonPrefixHTTP     = "http_"
	reasonUnknown        = "unknown"
)

// latencyBucketBoundsMS are the inclusive upper bounds, in milliseconds, of the
// fixed latency histogram. Bucket i counts observations with
// bounds[i-1] < ms <= bounds[i]; one extra overflow bucket at the end counts
// everything above the last bound. Counts are PER-BUCKET, not cumulative, so a
// benchmark harness diffs two snapshots by subtracting element-wise.
var latencyBucketBoundsMS = [15]int64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000}

// latencyBucketCount is the number of histogram slots: one per bound plus the
// overflow bucket.
const latencyBucketCount = len(latencyBucketBoundsMS) + 1

// MethodStats is the snapshot of the counters for one method key.
type MethodStats struct {
	Calls     int64 `json:"calls"`
	Errors    int64 `json:"errors"`
	Retries   int64 `json:"retries"`
	Backoffs  int64 `json:"backoffs"`
	BytesUp   int64 `json:"bytesUp"`
	BytesDown int64 `json:"bytesDown"`

	// BackoffMS is the total time spent in pacer backoff for this method.
	BackoffMS int64 `json:"backoffMS"`

	// PacedWaits is the number of steady-state token waits this method paid,
	// and PacedWaitMS their total length. They count the pacer's ERROR-FREE
	// throttling: a call that had to wait for the token bucket to refill even
	// though nothing failed. Backoffs/BackoffMS count the error path instead.
	// Both being zero on a busy window is the measurement that says the pacer's
	// steady-state interval never bound.
	PacedWaits  int64 `json:"pacedWaits"`
	PacedWaitMS int64 `json:"pacedWaitMS"`

	// LatencySumMS is the total time-to-first-response-header across Calls.
	LatencySumMS int64 `json:"latencySumMS"`

	// LatencyMS holds per-bucket (not cumulative) counts of
	// time-to-first-response-header, aligned index-for-index with
	// TransportStatsSnapshot.LatencyBucketsMS. Its length is always
	// len(LatencyBucketsMS)+1; the final element is the overflow bucket.
	LatencyMS []int64 `json:"latencyMS"`
}

// TransportStatsSnapshot is a JSON-serializable, point-in-time copy of a
// TransportStats.
type TransportStatsSnapshot struct {
	// StartTime is when the stats were created or last Reset.
	StartTime time.Time `json:"startTime"`

	// SnapshotTime is when this snapshot was taken.
	SnapshotTime time.Time `json:"snapshotTime"`

	// LatencyBucketsMS are the histogram upper bounds shared by every
	// MethodStats.LatencyMS in this snapshot.
	LatencyBucketsMS []int64 `json:"latencyBucketsMS"`

	// PerMethod is keyed by method key (files.list, files.get.media, ...) for
	// transport-observed traffic, and by whatever op string the pacer passes to
	// the Record* methods for pacer-observed events.
	PerMethod map[string]MethodStats `json:"perMethod"`

	// ErrorsByReason aggregates error reasons across all methods.
	// Transport-detected failures appear as "http_<status>" or
	// "transport_error"; pacer-detected failures appear under the reason string
	// the pacer supplies (typically a gdriveerr classification).
	ErrorsByReason map[string]int64 `json:"errorsByReason"`

	// PacedWaits and PacedWaitMS are the sums of the per-method counters of the
	// same name. They are carried here as well so that a reader that only wants
	// "did the pacer throttle this window at all" does not have to walk
	// PerMethod; PerMethod remains the authority and the two always agree.
	PacedWaits  int64 `json:"pacedWaits"`
	PacedWaitMS int64 `json:"pacedWaitMS"`
}

// methodCounters holds the live counters for one method key. Every field is an
// atomic, so counters may be updated from any number of concurrent RoundTrips
// while a snapshot is being taken.
type methodCounters struct {
	calls          atomic.Int64
	errors         atomic.Int64
	retries        atomic.Int64
	backoffs       atomic.Int64
	backoffNanos   atomic.Int64
	pacedWaits     atomic.Int64
	pacedWaitNanos atomic.Int64
	bytesUp        atomic.Int64
	bytesDown      atomic.Int64
	latencyNanos   atomic.Int64
	latency        [latencyBucketCount]atomic.Int64
}

func (c *methodCounters) observeLatency(d time.Duration) {
	if d < 0 {
		d = 0
	}

	c.latencyNanos.Add(int64(d))

	ms := d.Milliseconds()

	for i, bound := range latencyBucketBoundsMS {
		if ms <= bound {
			c.latency[i].Add(1)
			return
		}
	}

	c.latency[latencyBucketCount-1].Add(1)
}

func (c *methodCounters) snapshot() MethodStats {
	res := MethodStats{
		Calls:        c.calls.Load(),
		Errors:       c.errors.Load(),
		Retries:      c.retries.Load(),
		Backoffs:     c.backoffs.Load(),
		BytesUp:      c.bytesUp.Load(),
		BytesDown:    c.bytesDown.Load(),
		BackoffMS:    c.backoffNanos.Load() / int64(time.Millisecond),
		PacedWaits:   c.pacedWaits.Load(),
		PacedWaitMS:  c.pacedWaitNanos.Load() / int64(time.Millisecond),
		LatencySumMS: c.latencyNanos.Load() / int64(time.Millisecond),
		LatencyMS:    make([]int64, latencyBucketCount),
	}

	for i := range c.latency {
		res.LatencyMS[i] = c.latency[i].Load()
	}

	return res
}

// TransportStats accumulates per-method Drive API counters. The counters
// themselves are atomics; the small string-keyed registries that own them are
// guarded by a RWMutex which is only ever write-locked when a previously unseen
// method key or error reason shows up (and by Reset). It is safe for concurrent
// use by any number of RoundTrips plus a concurrent Snapshot.
//
// It records method keys, byte counts and timings only. URLs (which carry query
// values such as access tokens and upload session IDs), headers and message
// bodies are never stored or logged.
type TransportStats struct {
	mu             sync.RWMutex
	perMethod      map[string]*methodCounters
	errorsByReason map[string]*atomic.Int64

	startNanos atomic.Int64
}

// NewTransportStats returns an empty TransportStats whose observation window
// starts now.
func NewTransportStats() *TransportStats {
	s := &TransportStats{
		perMethod:      map[string]*methodCounters{},
		errorsByReason: map[string]*atomic.Int64{},
	}

	s.startNanos.Store(clock.Now().UnixNano())

	return s
}

// counters returns the counters for op, creating them on first use.
func (s *TransportStats) counters(op string) *methodCounters {
	if op == "" {
		op = methodOther
	}

	s.mu.RLock()
	c := s.perMethod[op]
	s.mu.RUnlock()

	if c != nil {
		return c
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if c = s.perMethod[op]; c == nil {
		c = &methodCounters{}
		s.perMethod[op] = c
	}

	return c
}

// addReason increments the global by-reason error counter.
func (s *TransportStats) addReason(reason string) {
	if reason == "" {
		reason = reasonUnknown
	}

	s.mu.RLock()
	c := s.errorsByReason[reason]
	s.mu.RUnlock()

	if c == nil {
		s.mu.Lock()

		if c = s.errorsByReason[reason]; c == nil {
			c = &atomic.Int64{}
			s.errorsByReason[reason] = c
		}

		s.mu.Unlock()
	}

	c.Add(1)
}

// RecordRetry counts one retry attempt for op.
//
// RecordRetry, RecordError, RecordBackoff and RecordPacedWait exist so that the
// pacer can report into the same stats object as the transport. They
// structurally satisfy the pacer's Recorder interface; that interface is
// deliberately NOT imported, to keep the transport free of any dependency on
// the pacer. Adding a method to it therefore means adding one here too, or the
// backend stops compiling at gdrivepacer.New.
func (s *TransportStats) RecordRetry(op string) {
	s.counters(op).retries.Add(1)
}

// RecordError counts one error for op and attributes it to reason, which is
// aggregated into ErrorsByReason. Callers must pass a low-cardinality
// classification (e.g. a gdriveerr reason), never an error message: raw error
// strings can embed request URLs and therefore credentials.
func (s *TransportStats) RecordError(op, reason string) {
	s.counters(op).errors.Add(1)
	s.addReason(reason)
}

// RecordBackoff counts one backoff of duration d for op.
func (s *TransportStats) RecordBackoff(op string, d time.Duration) {
	c := s.counters(op)
	c.backoffs.Add(1)

	if d > 0 {
		c.backoffNanos.Add(int64(d))
	}
}

// RecordPacedWait counts one steady-state pacer wait of duration d for op. A
// non-positive d is ignored entirely - not even the count is incremented -
// because a call that did not have to wait is not evidence of pacing.
func (s *TransportStats) RecordPacedWait(op string, d time.Duration) {
	if d <= 0 {
		return
	}

	c := s.counters(op)
	c.pacedWaits.Add(1)
	c.pacedWaitNanos.Add(int64(d))
}

// Snapshot returns a JSON-serializable copy of the current counters. It
// allocates only the returned maps and slices; the hot path (RoundTrip) never
// allocates on account of stats.
func (s *TransportStats) Snapshot() TransportStatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	res := TransportStatsSnapshot{
		StartTime:        time.Unix(0, s.startNanos.Load()).UTC(),
		SnapshotTime:     clock.Now().UTC(),
		LatencyBucketsMS: slices.Clone(latencyBucketBoundsMS[:]),
		PerMethod:        make(map[string]MethodStats, len(s.perMethod)),
		ErrorsByReason:   make(map[string]int64, len(s.errorsByReason)),
	}

	for k, c := range s.perMethod {
		ms := c.snapshot()
		res.PerMethod[k] = ms
		res.PacedWaits += ms.PacedWaits
		res.PacedWaitMS += ms.PacedWaitMS
	}

	for k, c := range s.errorsByReason {
		res.ErrorsByReason[k] = c.Load()
	}

	return res
}

// Reset drops all counters and restarts the observation window. The benchmark
// harness calls it between workload stages.
//
// Requests that are already in flight keep writing into the counter objects
// they captured on entry, so bytes still being streamed when Reset is called
// are dropped rather than misattributed to the new window.
func (s *TransportStats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.perMethod = map[string]*methodCounters{}
	s.errorsByReason = map[string]*atomic.Int64{}
	s.startNanos.Store(clock.Now().UnixNano())
}

// instrumentedTransport is an http.RoundTripper that attributes every request
// to a Drive method key and records call, error, byte and latency counters for
// it. It is transparent: requests and responses are passed through unmodified
// apart from the response body being wrapped in a counting io.ReadCloser.
type instrumentedTransport struct {
	base  http.RoundTripper
	stats *TransportStats
}

// newInstrumentedTransport wraps base (typically the oauth2 transport) so that
// all Drive API traffic is counted into stats. A nil base means
// http.DefaultTransport; nil stats means a fresh, unobservable TransportStats.
func newInstrumentedTransport(base http.RoundTripper, stats *TransportStats) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}

	if stats == nil {
		stats = NewTransportStats()
	}

	return &instrumentedTransport{base: base, stats: stats}
}

// RoundTrip implements http.RoundTripper.
func (t *instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c := t.stats.counters(attributeMethod(req))
	c.calls.Add(1)

	// Bytes up: a known ContentLength is counted up front and the request is
	// passed through untouched (RoundTrippers must not modify the request). A
	// body of unknown length is counted as it is consumed, which requires
	// substituting the body on a shallow clone. Per net/http, a client request
	// with a non-nil Body and ContentLength <= 0 has unknown length. Bodies
	// replayed by a lower layer via GetBody are not counted, and a request that
	// fails part-way through sending a known-length body is over-counted; both
	// are accepted approximations.
	outreq := req

	switch {
	case req.ContentLength > 0:
		c.bytesUp.Add(req.ContentLength)

	case req.Body != nil && req.Body != http.NoBody:
		outreq = req.Clone(req.Context())
		outreq.Body = &countingReadCloser{inner: req.Body, counter: &c.bytesUp}
	}

	// base.RoundTrip returns once the response headers have been received, so
	// this measures latency to first response header, not to last body byte.
	timer := timetrack.StartTimer()
	resp, err := t.base.RoundTrip(outreq)

	c.observeLatency(timer.Elapsed())

	if err != nil {
		c.errors.Add(1)
		// The error text can contain the request URL (and hence query values),
		// so only a fixed reason is recorded.
		t.stats.addReason(reasonTransportError)

		//nolint:wrapcheck // must be returned verbatim: http.Client relies on the exact error from the base RoundTripper.
		return resp, err
	}

	// Anything below 400 is a success as far as the transport is concerned;
	// notably 308 (resumable upload incomplete) is a normal status probe result.
	if resp.StatusCode >= http.StatusBadRequest {
		c.errors.Add(1)
		t.stats.addReason(reasonPrefixHTTP + strconv.Itoa(resp.StatusCode))
	}

	if resp.Body != nil {
		resp.Body = &countingReadCloser{inner: resp.Body, counter: &c.bytesDown}
	}

	return resp, nil
}

// attributeMethod derives the method key for a request from its URL path and
// verb. Query parameters are read (alt, uploadType, upload_id) but never
// retained.
func attributeMethod(req *http.Request) string {
	if req == nil || req.URL == nil {
		return methodOther
	}

	p := req.URL.Path
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}

	switch {
	case p == batchPath || strings.HasPrefix(p, batchPath+"/"):
		return methodBatch

	case p == uploadFilesPath || strings.HasPrefix(p, uploadFilesPath+"/"):
		return uploadMethod(req.URL)

	case p == filesPath:
		return filesCollectionMethod(req.Method)

	case strings.HasPrefix(p, filesPath+"/"):
		return filesItemMethod(req.Method, req.URL, strings.TrimPrefix(p, filesPath+"/"))

	default:
		return methodOther
	}
}

// uploadMethod distinguishes the two upload protocols. A resumable session URI
// carries upload_id, so chunk uploads and status probes against it are
// attributed to the resumable bucket even though they no longer carry
// uploadType.
func uploadMethod(u *url.URL) string {
	q := u.Query()
	if q.Get("uploadType") == "resumable" || q.Get("upload_id") != "" {
		return methodUploadResumable
	}

	return methodUploadSimple
}

func filesCollectionMethod(verb string) string {
	switch verb {
	case http.MethodGet:
		return methodFilesList
	case http.MethodPost:
		return methodFilesCreate
	default:
		return methodOther
	}
}

// nonFileIDSegments are path segments under /drive/v3/files/ that name an
// operation rather than a file ID.
var nonFileIDSegments = map[string]struct{}{
	"generateIds": {},
	"trash":       {},
}

func filesItemMethod(verb string, u *url.URL, rest string) string {
	// Sub-resources (permissions, revisions, comments, ...) and named operations
	// are not on the blob data path.
	if strings.Contains(rest, "/") {
		return methodOther
	}

	if _, ok := nonFileIDSegments[rest]; ok {
		return methodOther
	}

	switch verb {
	case http.MethodGet, http.MethodHead:
		if u.Query().Get("alt") == "media" {
			return methodFilesGetMedia
		}

		return methodFilesGet

	case http.MethodPatch, http.MethodPut:
		return methodFilesUpdate

	case http.MethodDelete:
		return methodFilesDelete

	default:
		return methodOther
	}
}

// countingReadCloser adds the number of bytes actually transferred to counter.
// It is a strict pass-through: Read and Close return the inner values verbatim
// (including io.EOF and any error), so partial reads followed by Close count
// exactly what was consumed and a body that is closed without being read counts
// zero.
type countingReadCloser struct {
	inner   io.ReadCloser
	counter *atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	if n > 0 {
		c.counter.Add(int64(n))
	}

	//nolint:wrapcheck // io.Reader semantics require the error to be returned verbatim.
	return n, err
}

func (c *countingReadCloser) Close() error {
	//nolint:wrapcheck // io.Closer semantics require the error to be returned verbatim.
	return c.inner.Close()
}

var (
	_ http.RoundTripper = (*instrumentedTransport)(nil)
	_ io.ReadCloser     = (*countingReadCloser)(nil)
)
