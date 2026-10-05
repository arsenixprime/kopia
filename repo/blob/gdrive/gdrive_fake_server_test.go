//go:build !no_extra_providers

package gdrive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
)

// fakeDrive is an in-memory implementation of just enough of the Google Drive
// v3 API to run the whole blob.Storage compatibility suite without a network:
// files.list, files.get (metadata and alt=media with Range/416 semantics),
// files.create and files.update (multipart and resumable), files.delete and
// about.get.
//
// It is deliberately STRICT. Every query string, orderBy value and field mask
// the backend sends is parsed, and anything unrecognized is answered with an
// error rather than being ignored. A change to the backend that starts asking
// for something new therefore fails loudly here instead of silently drifting
// away from what was reviewed against the real API.
type fakeDrive struct {
	server *httptest.Server

	// rootFolderID is the folder a storage is normally pointed at.
	rootFolderID string

	mu sync.Mutex

	files    map[string]*fakeFile
	sessions map[string]*fakeSession
	seq      int

	quotaLimit int64
	quotaUsage int64

	// failures holds one-shot injected responses keyed by method key
	// (files.list, upload.resumable, ...).
	failures map[string][]fakeFailure

	// hooks holds one-shot callbacks invoked before a method is handled, used to
	// simulate another writer acting in the middle of an operation.
	hooks map[string][]func()

	// requests counts handled requests per method key.
	requests map[string]int

	// inFlight and peakInFlight track how many requests of a method are being
	// handled at once, which is how the backend's concurrency ceiling is
	// observed.
	inFlight     map[string]int
	peakInFlight map[string]int

	// lastListQuery is the `q` parameter of the most recent files.list.
	lastListQuery string

	// userAgent is the User-Agent of the most recent request.
	userAgent string

	// stalls holds one-shot stall injections; see stallNext.
	stalls []fakeStall

	// stallRelease is closed at test cleanup to unblock stalled handlers whose
	// client never closed the connection.
	stallRelease chan struct{}

	// resumableSessions counts resumable sessions started; resumableChunkBytes
	// sums the body bytes of every resumable chunk request the fake accepted.
	resumableSessions   int
	resumableChunkBytes int64

	// productionTransport and transportTimeouts are applied by registerTransport;
	// see useProductionTransport.
	productionTransport bool
	transportTimeouts   *transportTimeouts
}

// fakeStall makes a matching request hang: the handler reads bodyBytes of the
// request body and then neither reads further nor responds until the client
// gives up on the request.
type fakeStall struct {
	match     func(r *http.Request) bool
	bodyBytes int64
}

type fakeFile struct {
	id           string
	name         string
	mimeType     string
	parents      []string
	trashed      bool
	content      []byte
	createdTime  time.Time
	modifiedTime time.Time
}

type fakeSession struct {
	fileID   string // non-empty for an update session
	metadata fakeFileMetadata
	fields   string
	buf      []byte
}

// fakeFileMetadata is the subset of the File resource the backend ever writes.
type fakeFileMetadata struct {
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	Parents      []string `json:"parents"`
	ModifiedTime string   `json:"modifiedTime"`
}

type fakeFailure struct {
	status int
	reason string
}

const (
	fakeDefaultQuotaLimit = 1 << 40
	fakeDefaultQuotaUsage = 1 << 30

	// fakeTimeFormat mimics Drive, which reports RFC 3339 UTC timestamps with
	// millisecond resolution. Sub-millisecond creation ties are therefore
	// possible, which is what makes the duplicate tie-break rule reachable.
	fakeTimeFormat = "2006-01-02T15:04:05.000Z07:00"
)

func newFakeDrive(t *testing.T) *fakeDrive {
	t.Helper()

	d := &fakeDrive{
		rootFolderID: "fake-root-folder",
		files:        map[string]*fakeFile{},
		sessions:     map[string]*fakeSession{},
		quotaLimit:   fakeDefaultQuotaLimit,
		quotaUsage:   fakeDefaultQuotaUsage,
		failures:     map[string][]fakeFailure{},
		hooks:        map[string][]func(){},
		requests:     map[string]int{},
		inFlight:     map[string]int{},
		peakInFlight: map[string]int{},
		stallRelease: make(chan struct{}),
	}

	// My Drive itself, under the alias Drive accepts wherever a folder ID is
	// expected. Folder-by-name resolution both searches and creates here.
	d.files[driveRootFolderAlias] = &fakeFile{
		id:           driveRootFolderAlias,
		name:         "My Drive",
		mimeType:     folderMimeType,
		createdTime:  clock.Now(),
		modifiedTime: clock.Now(),
	}

	d.files[d.rootFolderID] = &fakeFile{
		id:           d.rootFolderID,
		name:         "kopia-fake-root",
		mimeType:     folderMimeType,
		parents:      []string{driveRootFolderAlias},
		createdTime:  clock.Now(),
		modifiedTime: clock.Now(),
	}

	d.server = httptest.NewServer(d)
	t.Cleanup(d.server.Close)

	// Registered after the server, so it runs before server.Close, which would
	// otherwise wait forever for a stalled handler.
	t.Cleanup(func() { close(d.stallRelease) })

	return d
}

// useProductionTransport makes storages opened against this fake use the
// production HTTP transport (stall timeouts and all, minus authentication)
// instead of the httptest client, with the given timeouts. It must be called
// before options().
func (d *fakeDrive) useProductionTransport(timeouts transportTimeouts) {
	d.productionTransport = true
	d.transportTimeouts = &timeouts
}

// stallNext makes the next request for which match returns true hang without
// a response; see fakeStall. match is called under the fake's lock.
func (d *fakeDrive) stallNext(match func(r *http.Request) bool, bodyBytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.stalls = append(d.stalls, fakeStall{match: match, bodyBytes: bodyBytes})
}

// stall implements one injected stall. It aborts the connection when the
// client has gone, so nothing is ever sent back.
func (d *fakeDrive) stall(r *http.Request, st fakeStall) {
	if st.bodyBytes > 0 {
		io.CopyN(io.Discard, r.Body, st.bodyBytes) //nolint:errcheck
	}

	select {
	case <-r.Context().Done():
	case <-d.stallRelease:
	}

	panic(http.ErrAbortHandler)
}

func (d *fakeDrive) resumableStats() (sessions int, chunkBytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.resumableSessions, d.resumableChunkBytes
}

// options returns Options pointing at this fake Drive and registers the
// transport override for its folder ID.
func (d *fakeDrive) options(t *testing.T, cacheDir string) *Options {
	t.Helper()

	d.registerTransport(t, d.rootFolderID)

	return &Options{
		FolderID: d.rootFolderID,
		Tuning:   TuningOptions{CacheDir: cacheDir},
	}
}

// optionsForFolderName returns Options that name a folder to be created rather
// than an existing folder ID, and registers the transport override under the
// key New uses while there is no ID yet.
func (d *fakeDrive) optionsForFolderName(t *testing.T, cacheDir, folderName string) *Options {
	t.Helper()

	opt := &Options{
		FolderName: folderName,
		Tuning:     TuningOptions{CacheDir: cacheDir},
	}

	d.registerTransport(t, testTransportKey(opt))

	return opt
}

// registerTransport points New at this fake server for one seam key (a folder
// ID, or "name:<folder name>" while the folder is still to be created).
func (d *fakeDrive) registerTransport(t *testing.T, key string) {
	t.Helper()

	tr := testDriveTransport{
		base:     d.server.Client().Transport,
		endpoint: d.server.URL + "/drive/v3/",
	}

	if d.productionTransport {
		tr.base = nil
		tr.timeouts = d.transportTimeouts
	}

	testDriveTransports.Store(key, tr)

	t.Cleanup(func() { testDriveTransports.Delete(key) })
}

// addRootFolder injects a folder in the root of My Drive directly, simulating
// one left behind by an earlier run (or made by hand under --scope=drive).
func (d *fakeDrive) addRootFolder(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.createFileLocked(fakeFileMetadata{
		Name:     name,
		MimeType: folderMimeType,
		Parents:  []string{driveRootFolderAlias},
	}, nil, clock.Now()).id
}

//
// ------------------------------------------------------------- test controls
//

// injectFailure makes the next request attributed to method fail with the given
// HTTP status and Drive reason.
func (d *fakeDrive) injectFailure(method string, status int, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.failures[method] = append(d.failures[method], fakeFailure{status: status, reason: reason})
}

// onNext registers a callback invoked (outside the fake's lock) just before the
// next request attributed to method is handled, so a test can make another
// writer act in the middle of an operation.
func (d *fakeDrive) onNext(method string, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.hooks[method] = append(d.hooks[method], fn)
}

func (d *fakeDrive) requestCount(method string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.requests[method]
}

// peakConcurrency reports the largest number of requests of one method the fake
// ever handled simultaneously.
func (d *fakeDrive) peakConcurrency(method string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.peakInFlight[method]
}

func (d *fakeDrive) listQuery() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.lastListQuery
}

// addFile injects a file directly, simulating another writer.
func (d *fakeDrive) addFile(name string, content []byte, createdTime time.Time) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.createFileLocked(fakeFileMetadata{
		Name:     name,
		MimeType: blobMimeType,
		Parents:  []string{d.rootFolderID},
	}, content, createdTime).id
}

// deleteFileDirectly removes a file behind the backend's back.
func (d *fakeDrive) deleteFileDirectly(fileID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.files, fileID)
}

func (d *fakeDrive) setQuota(limit, usage int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.quotaLimit, d.quotaUsage = limit, usage
}

func (d *fakeDrive) lastUserAgent() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.userAgent
}

func (d *fakeDrive) fileIDsNamed(name string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	var res []string

	for _, f := range d.files {
		if f.name == name && !f.trashed {
			res = append(res, f.id)
		}
	}

	sort.Strings(res)

	return res
}

// parentsOf reports the parent folder IDs recorded for a file.
func (d *fakeDrive) parentsOf(fileID string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	f, ok := d.files[fileID]
	if !ok {
		return nil
	}

	return append([]string(nil), f.parents...)
}

//
// ------------------------------------------------------------------ routing
//

func (d *fakeDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := d.attribute(r)

	d.mu.Lock()
	d.requests[method]++
	d.userAgent = r.UserAgent()

	d.inFlight[method]++
	d.peakInFlight[method] = max(d.peakInFlight[method], d.inFlight[method])

	defer func() {
		d.mu.Lock()
		d.inFlight[method]--
		d.mu.Unlock()
	}()

	for i := range d.stalls {
		if !d.stalls[i].match(r) {
			continue
		}

		st := d.stalls[i]
		d.stalls = slices.Delete(d.stalls, i, i+1)
		d.mu.Unlock()

		d.stall(r, st) // aborts the handler; never returns

		return
	}

	var hook func()

	if h := d.hooks[method]; len(h) > 0 {
		hook, d.hooks[method] = h[0], h[1:]
	}

	var failure *fakeFailure

	if f := d.failures[method]; len(f) > 0 {
		failure, d.failures[method] = &f[0], f[1:]
	}

	d.mu.Unlock()

	if hook != nil {
		hook()
	}

	if failure != nil {
		writeDriveError(w, failure.status, failure.reason, "injected failure")
		return
	}

	switch {
	case r.URL.Path == "/batch/drive/v3":
		d.handleBatch(w, r)
	case r.URL.Path == "/drive/v3/about":
		d.handleAbout(w, r)
	case r.URL.Path == "/upload/drive/v3/files":
		d.handleUploadCollection(w, r)
	case strings.HasPrefix(r.URL.Path, "/upload/drive/v3/files/"):
		d.handleUploadItem(w, r, strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files/"))
	case r.URL.Path == "/drive/v3/files":
		d.handleFilesCollection(w, r)
	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/"):
		d.handleFilesItem(w, r, strings.TrimPrefix(r.URL.Path, "/drive/v3/files/"))
	default:
		writeDriveError(w, http.StatusNotFound, "notFound", "no such endpoint: "+r.URL.Path)
	}
}

// attribute mirrors the production transport's method attribution so that
// failures can be injected per API method.
func (d *fakeDrive) attribute(r *http.Request) string {
	return attributeMethod(r)
}

func (d *fakeDrive) handleAbout(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	limit, usage := d.quotaLimit, d.quotaUsage
	d.mu.Unlock()

	quota := map[string]any{}
	if limit > 0 {
		quota["limit"] = strconv.FormatInt(limit, 10)
	}

	quota["usage"] = strconv.FormatInt(usage, 10)

	writeJSON(w, applyMask(map[string]any{"storageQuota": quota}, r.URL.Query().Get("fields")))
}

func (d *fakeDrive) handleFilesCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.handleList(w, r)
	case http.MethodPost:
		d.handleCreateMetadataOnly(w, r)
	default:
		writeDriveError(w, http.StatusMethodNotAllowed, "badRequest", "unsupported method "+r.Method)
	}
}

func (d *fakeDrive) handleFilesItem(w http.ResponseWriter, r *http.Request, fileID string) {
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("alt") == "media" {
			d.handleDownload(w, r, fileID)
			return
		}

		d.handleGetMetadata(w, r, fileID)
	case http.MethodPatch:
		d.handleUpdateMetadataOnly(w, r, fileID)
	case http.MethodDelete:
		d.handleDelete(w, fileID)
	default:
		writeDriveError(w, http.StatusMethodNotAllowed, "badRequest", "unsupported method "+r.Method)
	}
}

//
// ------------------------------------------------------------------- batch
//

// handleBatch implements the homogeneous batch endpoint: a multipart/mixed body
// whose parts are complete HTTP requests, answered by a multipart/mixed body
// whose parts are complete HTTP responses, each with its own status.
//
// Every inner request is routed back through ServeHTTP, so a part is handled by
// exactly the same code (and counted, and subject to the same injected
// failures) as the standalone call would have been. That is what lets a test
// build a batch of mixed outcomes.
func (d *fakeDrive) handleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeDriveError(w, http.StatusMethodNotAllowed, "badRequest", "unsupported method "+r.Method)
		return
	}

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		writeDriveError(w, http.StatusBadRequest, "badRequest",
			"a batch request must carry a multipart/mixed body, got "+r.Header.Get("Content-Type"))

		return
	}

	mr := multipart.NewReader(r.Body, params["boundary"])

	var out bytes.Buffer

	mw := multipart.NewWriter(&out)

	parts := 0

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			writeDriveError(w, http.StatusBadRequest, "badRequest", "unreadable batch part: "+err.Error())
			return
		}

		if got := part.Header.Get("Content-Type"); got != batchPartContentType {
			writeDriveError(w, http.StatusBadRequest, "badRequest", "a batch part must be "+batchPartContentType+", got "+got)
			return
		}

		if err := d.handleBatchPart(mw, part, part.Header.Get("Content-ID")); err != nil {
			writeDriveError(w, http.StatusBadRequest, "badRequest", err.Error())
			return
		}

		parts++
	}

	if parts == 0 || parts > maxBatchDeleteSize {
		writeDriveError(w, http.StatusBadRequest, "badRequest",
			fmt.Sprintf("a batch request must hold between 1 and %v calls, got %v", maxBatchDeleteSize, parts))

		return
	}

	mw.Close() //nolint:errcheck

	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	w.WriteHeader(http.StatusOK)
	w.Write(out.Bytes()) //nolint:errcheck
}

func (d *fakeDrive) handleBatchPart(mw *multipart.Writer, part *multipart.Part, contentID string) error {
	inner, err := http.ReadRequest(bufio.NewReader(part))
	if err != nil {
		return errors.Wrap(err, "unparseable batch part")
	}

	if strings.HasPrefix(inner.RequestURI, "http://") || strings.HasPrefix(inner.RequestURI, "https://") {
		return errors.Errorf("full URLs are not allowed in batch requests, got %q", inner.RequestURI)
	}

	rec := httptest.NewRecorder()

	d.ServeHTTP(rec, inner)

	h := textproto.MIMEHeader{}
	h.Set("Content-Type", batchPartContentType)
	h.Set("Content-ID", "<"+batchResponsePrefix+strings.Trim(contentID, "<>")+">")

	pw, err := mw.CreatePart(h)
	if err != nil {
		return errors.Wrap(err, "unable to write a batch response part")
	}

	_, err = pw.Write(rawHTTPResponse(rec))

	return errors.Wrap(err, "unable to write a batch response part")
}

// rawHTTPResponse renders a recorded response in HTTP/1.1 wire format, which is
// what a batch response part holds.
func rawHTTPResponse(rec *httptest.ResponseRecorder) []byte {
	var sb bytes.Buffer

	fmt.Fprintf(&sb, "HTTP/1.1 %d %s\r\n", rec.Code, http.StatusText(rec.Code))

	for _, k := range slices.Sorted(maps.Keys(rec.Header())) {
		for _, v := range rec.Header()[k] {
			fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
		}
	}

	if rec.Body.Len() > 0 {
		fmt.Fprintf(&sb, "Content-Length: %d\r\n", rec.Body.Len())
	}

	sb.WriteString("\r\n")
	sb.Write(rec.Body.Bytes())

	return sb.Bytes()
}

//
// -------------------------------------------------------------- files.list
//

func (d *fakeDrive) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	d.mu.Lock()
	d.lastListQuery = q.Get("q")
	d.mu.Unlock()

	parsed, err := parseFakeQuery(q.Get("q"))
	if err != nil {
		writeDriveError(w, http.StatusBadRequest, "invalidQuery", err.Error())
		return
	}

	order := q.Get("orderBy")
	if order != "" && order != listOrderBy {
		writeDriveError(w, http.StatusBadRequest, "invalidQuery", "unsupported orderBy: "+order)
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if parent, ok := d.files[parsed.parent]; !ok || parent.mimeType != folderMimeType {
		writeDriveError(w, http.StatusNotFound, "notFound", "File not found: "+parsed.parent+".")
		return
	}

	matches := d.matchingFilesLocked(parsed)
	sortFakeFiles(matches, order)

	pageSize := 100

	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeDriveError(w, http.StatusBadRequest, "invalidQuery", "bad pageSize")
			return
		}

		pageSize = n
	}

	start := 0

	if v := q.Get("pageToken"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > len(matches) {
			writeDriveError(w, http.StatusBadRequest, "invalidQuery", "bad pageToken")
			return
		}

		start = n
	}

	end := min(start+pageSize, len(matches))

	out := map[string]any{}

	files := make([]any, 0, end-start)
	for _, f := range matches[start:end] {
		files = append(files, fileResource(f))
	}

	out["files"] = files

	if end < len(matches) {
		out["nextPageToken"] = strconv.Itoa(end)
	}

	writeJSON(w, applyMask(out, q.Get("fields")))
}

func (d *fakeDrive) matchingFilesLocked(q fakeQuery) []*fakeFile {
	var res []*fakeFile

	for _, f := range d.files {
		switch {
		case !slices.Contains(f.parents, q.parent):

		// Drive's `=` is case-insensitive (RECON.md B.5.3). Modeling that here is
		// what proves the backend's client-side exact-name filter is load-bearing.
		case q.hasName && !strings.EqualFold(f.name, q.name):

		// `contains` is modeled as a plain substring match, i.e. the WEAKEST
		// semantics Drive's documentation can be read as promising, so that a
		// backend relying on it for correctness rather than for narrowing fails
		// here.
		case q.hasNameContains && !strings.Contains(f.name, q.nameContains):

		case q.hasMimeType && f.mimeType != q.mimeType:
		case q.trashedFalse && f.trashed:
		default:
			res = append(res, f)
		}
	}

	return res
}

func sortFakeFiles(files []*fakeFile, order string) {
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i], files[j]

		if order == listOrderBy {
			if a.name != b.name {
				return a.name < b.name
			}

			at, bt := a.createdTime.Format(fakeTimeFormat), b.createdTime.Format(fakeTimeFormat)
			if at != bt {
				return at > bt
			}
		}

		return a.id < b.id
	})
}

//
// --------------------------------------------------------------- files.get
//

func (d *fakeDrive) handleGetMetadata(w http.ResponseWriter, r *http.Request, fileID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	f, ok := d.files[fileID]
	if !ok {
		writeDriveError(w, http.StatusNotFound, "notFound", "File not found: "+fileID+".")
		return
	}

	writeJSON(w, applyMask(fileResource(f), r.URL.Query().Get("fields")))
}

func (d *fakeDrive) handleDownload(w http.ResponseWriter, r *http.Request, fileID string) {
	d.mu.Lock()

	f, ok := d.files[fileID]
	if !ok {
		d.mu.Unlock()
		writeDriveError(w, http.StatusNotFound, "notFound", "File not found: "+fileID+".")

		return
	}

	content := append([]byte(nil), f.content...)

	d.mu.Unlock()

	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		w.Header().Set("Content-Type", uploadContentType)
		w.WriteHeader(http.StatusOK)
		w.Write(content) //nolint:errcheck

		return
	}

	start, end, err := parseByteRange(rangeHeader, int64(len(content)))
	if err != nil {
		writeDriveError(w, http.StatusBadRequest, "badRequest", err.Error())
		return
	}

	if start >= int64(len(content)) && (start != 0 || len(content) != 0) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(content)))
		writeDriveError(w, http.StatusRequestedRangeNotSatisfiable, "requestedRangeNotSatisfiable",
			"the requested range is not satisfiable")

		return
	}

	w.Header().Set("Content-Type", uploadContentType)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(content[start : end+1]) //nolint:errcheck
}

// parseByteRange understands the two forms the backend emits: "bytes=a-b" and
// "bytes=a-". end is inclusive and clamped to the content length.
func parseByteRange(v string, size int64) (start, end int64, err error) {
	spec, ok := strings.CutPrefix(v, "bytes=")
	if !ok {
		return 0, 0, errors.Errorf("unsupported Range header %q", v)
	}

	lo, hi, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, errors.Errorf("unsupported Range header %q", v)
	}

	start, err = strconv.ParseInt(lo, 10, 64)
	if err != nil {
		return 0, 0, errors.Errorf("unsupported Range header %q", v)
	}

	if hi == "" {
		return start, size - 1, nil
	}

	end, err = strconv.ParseInt(hi, 10, 64)
	if err != nil {
		return 0, 0, errors.Errorf("unsupported Range header %q", v)
	}

	if end > size-1 {
		end = size - 1
	}

	return start, end, nil
}

//
// ------------------------------------------------------ files.create/update
//

func (d *fakeDrive) handleCreateMetadataOnly(w http.ResponseWriter, r *http.Request) {
	var md fakeFileMetadata

	if err := json.NewDecoder(r.Body).Decode(&md); err != nil {
		writeDriveError(w, http.StatusBadRequest, "badRequest", "unparseable metadata")
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	f := d.createFileLocked(md, nil, clock.Now())

	writeJSON(w, applyMask(fileResource(f), r.URL.Query().Get("fields")))
}

func (d *fakeDrive) handleUpdateMetadataOnly(w http.ResponseWriter, r *http.Request, fileID string) {
	var md fakeFileMetadata

	if err := json.NewDecoder(r.Body).Decode(&md); err != nil {
		writeDriveError(w, http.StatusBadRequest, "badRequest", "unparseable metadata")
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	f, ok := d.files[fileID]
	if !ok {
		writeDriveError(w, http.StatusNotFound, "notFound", "File not found: "+fileID+".")
		return
	}

	applyModifiedTime(f, md.ModifiedTime)

	writeJSON(w, applyMask(fileResource(f), r.URL.Query().Get("fields")))
}

func (d *fakeDrive) handleDelete(w http.ResponseWriter, fileID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.files[fileID]; !ok {
		writeDriveError(w, http.StatusNotFound, "notFound", "File not found: "+fileID+".")
		return
	}

	// Permanent, never a move to the trash - that is the behavior the backend
	// depends on, so the fake must not paper over a regression to trashing.
	delete(d.files, fileID)

	w.WriteHeader(http.StatusNoContent)
}

//
// ------------------------------------------------------------------ uploads
//

func (d *fakeDrive) handleUploadCollection(w http.ResponseWriter, r *http.Request) {
	if uploadID := r.URL.Query().Get("upload_id"); uploadID != "" {
		d.handleResumableChunk(w, r, uploadID)
		return
	}

	if r.Method != http.MethodPost {
		writeDriveError(w, http.StatusMethodNotAllowed, "badRequest", "unsupported method "+r.Method)
		return
	}

	d.handleUpload(w, r, "")
}

func (d *fakeDrive) handleUploadItem(w http.ResponseWriter, r *http.Request, fileID string) {
	if r.Method != http.MethodPatch {
		writeDriveError(w, http.StatusMethodNotAllowed, "badRequest", "unsupported method "+r.Method)
		return
	}

	d.mu.Lock()
	_, ok := d.files[fileID]
	d.mu.Unlock()

	if !ok {
		writeDriveError(w, http.StatusNotFound, "notFound", "File not found: "+fileID+".")
		return
	}

	d.handleUpload(w, r, fileID)
}

func (d *fakeDrive) handleUpload(w http.ResponseWriter, r *http.Request, fileID string) {
	switch r.URL.Query().Get("uploadType") {
	case "multipart":
		d.handleMultipartUpload(w, r, fileID)
	case "resumable":
		d.handleResumableStart(w, r, fileID)
	default:
		writeDriveError(w, http.StatusBadRequest, "badRequest", "unsupported uploadType")
	}
}

func (d *fakeDrive) handleMultipartUpload(w http.ResponseWriter, r *http.Request, fileID string) {
	md, content, err := readMultipartUpload(r)
	if err != nil {
		writeDriveError(w, http.StatusBadRequest, "badRequest", err.Error())
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	f, err := d.storeUploadLocked(fileID, md, content)
	if err != nil {
		writeDriveError(w, http.StatusNotFound, "notFound", err.Error())
		return
	}

	writeJSON(w, applyMask(fileResource(f), r.URL.Query().Get("fields")))
}

func readMultipartUpload(r *http.Request) (fakeFileMetadata, []byte, error) {
	var md fakeFileMetadata

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return md, nil, errors.Errorf("expected a multipart body, got %q", r.Header.Get("Content-Type"))
	}

	mr := multipart.NewReader(r.Body, params["boundary"])

	metaPart, err := mr.NextPart()
	if err != nil {
		return md, nil, errors.Wrap(err, "missing metadata part")
	}

	if err := json.NewDecoder(metaPart).Decode(&md); err != nil {
		return md, nil, errors.Wrap(err, "unparseable metadata part")
	}

	mediaPart, err := mr.NextPart()
	if err != nil {
		return md, nil, errors.Wrap(err, "missing media part")
	}

	content, err := io.ReadAll(mediaPart)
	if err != nil {
		return md, nil, errors.Wrap(err, "unreadable media part")
	}

	return md, content, nil
}

func (d *fakeDrive) handleResumableStart(w http.ResponseWriter, r *http.Request, fileID string) {
	var md fakeFileMetadata

	if err := json.NewDecoder(r.Body).Decode(&md); err != nil {
		writeDriveError(w, http.StatusBadRequest, "badRequest", "unparseable metadata")
		return
	}

	d.mu.Lock()

	d.seq++
	d.resumableSessions++
	sessionID := fmt.Sprintf("session-%04d", d.seq)
	d.sessions[sessionID] = &fakeSession{
		fileID:   fileID,
		metadata: md,
		fields:   r.URL.Query().Get("fields"),
	}

	d.mu.Unlock()

	loc := *r.URL
	loc.Scheme = "http"
	loc.Host = r.Host
	loc.Path = "/upload/drive/v3/files"
	loc.RawQuery = url.Values{
		"uploadType": {"resumable"},
		"upload_id":  {sessionID},
	}.Encode()

	w.Header().Set("Location", loc.String())
	w.WriteHeader(http.StatusOK)
}

func (d *fakeDrive) handleResumableChunk(w http.ResponseWriter, r *http.Request, uploadID string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeDriveError(w, http.StatusBadRequest, "badRequest", "unreadable chunk")
		return
	}

	start, final, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeDriveError(w, http.StatusBadRequest, "badRequest", err.Error())
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	s, ok := d.sessions[uploadID]
	if !ok {
		writeDriveError(w, http.StatusNotFound, "notFound", "no such upload session")
		return
	}

	// A chunk is placed at the offset the client names, the way Drive does, so
	// a re-sent chunk replaces bytes instead of duplicating them. Only an
	// offset beyond what the session holds is a client bug.
	if start < 0 {
		start = int64(len(s.buf))
	}

	if start > int64(len(s.buf)) {
		writeDriveError(w, http.StatusBadRequest, "badRequest", fmt.Sprintf("chunk starts at %v but the session holds %v bytes", start, len(s.buf)))
		return
	}

	d.resumableChunkBytes += int64(len(body))
	s.buf = append(s.buf[:start], body...)

	if !final {
		// Drive replies 200 + X-Http-Status-Code-Override: 308 when the client
		// sends X-GUploader-No-308, which the Go client always does.
		w.Header().Set("X-Http-Status-Code-Override", "308")
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(s.buf)-1))
		w.WriteHeader(http.StatusOK)

		return
	}

	delete(d.sessions, uploadID)

	f, err := d.storeUploadLocked(s.fileID, s.metadata, s.buf)
	if err != nil {
		writeDriveError(w, http.StatusNotFound, "notFound", err.Error())
		return
	}

	writeJSON(w, applyMask(fileResource(f), s.fields))
}

// parseContentRange returns the chunk's starting offset (-1 for an empty final
// chunk, which has none) and whether the chunk is the last one. The Go client
// emits "bytes a-b/*" while more is coming and "bytes a-b/total" (or
// "bytes */total") for the final chunk.
func parseContentRange(v string) (start int64, final bool, err error) {
	spec, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, false, errors.Errorf("unsupported Content-Range %q", v)
	}

	rng, total, ok := strings.Cut(spec, "/")
	if !ok {
		return 0, false, errors.Errorf("unsupported Content-Range %q", v)
	}

	if rng == "*" {
		return -1, total != "*", nil
	}

	first, _, ok := strings.Cut(rng, "-")
	if !ok {
		return 0, false, errors.Errorf("unsupported Content-Range %q", v)
	}

	start, err = strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, false, errors.Wrapf(err, "unsupported Content-Range %q", v)
	}

	return start, total != "*", nil
}

// storeUploadLocked creates or replaces the file behind an upload.
func (d *fakeDrive) storeUploadLocked(fileID string, md fakeFileMetadata, content []byte) (*fakeFile, error) {
	if fileID == "" {
		return d.createFileLocked(md, content, clock.Now()), nil
	}

	f, ok := d.files[fileID]
	if !ok {
		return nil, errors.Errorf("File not found: %v.", fileID)
	}

	f.content = content
	f.modifiedTime = clock.Now()

	applyModifiedTime(f, md.ModifiedTime)

	return f, nil
}

func (d *fakeDrive) createFileLocked(md fakeFileMetadata, content []byte, createdTime time.Time) *fakeFile {
	d.seq++

	f := &fakeFile{
		id:           fmt.Sprintf("file-%04d", d.seq),
		name:         md.Name,
		mimeType:     md.MimeType,
		parents:      append([]string(nil), md.Parents...),
		content:      content,
		createdTime:  createdTime,
		modifiedTime: createdTime,
	}

	applyModifiedTime(f, md.ModifiedTime)

	d.files[f.id] = f

	return f
}

func applyModifiedTime(f *fakeFile, v string) {
	if v == "" {
		return
	}

	if t, err := time.Parse(time.RFC3339, v); err == nil {
		f.modifiedTime = t
	}
}

//
// ------------------------------------------------------------- serialization
//

func fileResource(f *fakeFile) map[string]any {
	res := map[string]any{
		"id":           f.id,
		"name":         f.name,
		"mimeType":     f.mimeType,
		"createdTime":  f.createdTime.UTC().Format(fakeTimeFormat),
		"modifiedTime": f.modifiedTime.UTC().Format(fakeTimeFormat),
		"trashed":      f.trashed,
	}

	if len(f.parents) > 0 {
		parents := make([]any, 0, len(f.parents))
		for _, p := range f.parents {
			parents = append(parents, p)
		}

		res["parents"] = parents
	}

	// Drive reports size as a decimal string, and omits it for a folder.
	if f.mimeType != folderMimeType {
		res["size"] = strconv.Itoa(len(f.content))
	}

	return res
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(v) //nolint:errcheck,errchkjson
}

func writeDriveError(w http.ResponseWriter, status int, reason, message string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)

	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,errchkjson
		"error": map[string]any{
			"code":    status,
			"message": message,
			"errors": []any{
				map[string]any{
					"domain":  "global",
					"reason":  reason,
					"message": message,
				},
			},
		},
	})
}

//
// -------------------------------------------------------------- field masks
//

// applyMask filters a response body by a Drive `fields` parameter. Supported
// forms are exactly the ones the backend emits: a comma-separated list of
// top-level names, optionally with a parenthesized sub-list. An empty mask
// means "everything".
//
// Filtering (rather than ignoring the mask) is what makes the fake catch a
// backend that forgets to ask for a field it then reads.
func applyMask(v map[string]any, mask string) map[string]any {
	if strings.TrimSpace(mask) == "" {
		return v
	}

	return applyParsedMask(v, parseMask(mask))
}

type maskNode map[string]maskNode

func parseMask(mask string) maskNode {
	res := maskNode{}

	for _, tok := range splitTopLevel(mask) {
		name, sub, ok := strings.Cut(tok, "(")
		if !ok {
			res[strings.TrimSpace(tok)] = nil
			continue
		}

		res[strings.TrimSpace(name)] = parseMask(strings.TrimSuffix(sub, ")"))
	}

	return res
}

// splitTopLevel splits on commas that are not inside parentheses.
func splitTopLevel(s string) []string {
	var (
		res   []string
		depth int
		cur   strings.Builder
	)

	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				res = append(res, cur.String())
				cur.Reset()

				continue
			}
		}

		cur.WriteRune(r)
	}

	if cur.Len() > 0 {
		res = append(res, cur.String())
	}

	return res
}

func applyParsedMask(v map[string]any, m maskNode) map[string]any {
	res := map[string]any{}

	for name, sub := range m {
		val, ok := v[name]
		if !ok {
			continue
		}

		if sub == nil {
			res[name] = val
			continue
		}

		switch t := val.(type) {
		case map[string]any:
			res[name] = applyParsedMask(t, sub)
		case []any:
			out := make([]any, 0, len(t))

			for _, e := range t {
				if em, ok := e.(map[string]any); ok {
					out = append(out, applyParsedMask(em, sub))
				}
			}

			res[name] = out
		default:
			res[name] = val
		}
	}

	return res
}

//
// -------------------------------------------------------------- query parser
//

type fakeQuery struct {
	parent string

	name    string
	hasName bool

	nameContains    string
	hasNameContains bool

	mimeType    string
	hasMimeType bool

	trashedFalse bool
}

// parseFakeQuery understands exactly the three `q` shapes the backend emits:
//
//	'<parent>' in parents and mimeType = '<mime>' and trashed = false
//	'<parent>' in parents and mimeType = '<mime>' and trashed = false and name contains '<prefix>'
//	'<parent>' in parents and name = '<name>' and mimeType = '<mime>' and trashed = false
//
// Anything else is rejected so that a change in the backend's query building
// shows up as a test failure rather than as a silently different result set.
func parseFakeQuery(q string) (fakeQuery, error) {
	var res fakeQuery

	if strings.TrimSpace(q) == "" {
		return res, errors.New("empty query")
	}

	for term := range strings.SplitSeq(q, " and ") {
		term = strings.TrimSpace(term)

		switch {
		case strings.HasSuffix(term, " in parents"):
			v, err := unquoteDriveLiteral(strings.TrimSuffix(term, " in parents"))
			if err != nil {
				return res, err
			}

			res.parent = v

		case strings.HasPrefix(term, "name = "):
			v, err := unquoteDriveLiteral(strings.TrimPrefix(term, "name = "))
			if err != nil {
				return res, err
			}

			res.name, res.hasName = v, true

		case strings.HasPrefix(term, "name contains "):
			v, err := unquoteDriveLiteral(strings.TrimPrefix(term, "name contains "))
			if err != nil {
				return res, err
			}

			res.nameContains, res.hasNameContains = v, true

		case strings.HasPrefix(term, "mimeType = "):
			v, err := unquoteDriveLiteral(strings.TrimPrefix(term, "mimeType = "))
			if err != nil {
				return res, err
			}

			res.mimeType, res.hasMimeType = v, true

		case term == "trashed = false":
			res.trashedFalse = true

		default:
			return res, errors.Errorf("unrecognized query term %q", term)
		}
	}

	if res.parent == "" {
		return res, errors.New("query does not constrain the parent folder")
	}

	return res, nil
}

func unquoteDriveLiteral(v string) (string, error) {
	v = strings.TrimSpace(v)

	if len(v) < 2 || v[0] != '\'' || v[len(v)-1] != '\'' {
		return "", errors.Errorf("expected a single-quoted literal, got %q", v)
	}

	var (
		sb      strings.Builder
		escaped bool
	)

	for _, r := range v[1 : len(v)-1] {
		switch {
		case escaped:
			sb.WriteRune(r)

			escaped = false
		case r == '\\':
			escaped = true
		case r == '\'':
			return "", errors.Errorf("unescaped quote in literal %q", v)
		default:
			sb.WriteRune(r)
		}
	}

	return sb.String(), nil
}
