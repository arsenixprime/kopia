//go:build !no_extra_providers

// Package gdrive implements Storage based on Google Drive.
package gdrive

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/iocopy"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/gdrive/gdriveerr"
	"github.com/kopia/kopia/repo/blob/gdrive/gdrivepacer"
	"github.com/kopia/kopia/repo/logging"
)

const (
	gdriveStorageType = "gdrive"

	// blobMimeType is the Drive MIME type every Kopia blob carries. It is part of
	// the on-the-wire contract with repositories written by the previous
	// implementation: every query filters on it, so changing it would make
	// existing repositories look empty.
	blobMimeType = "application/x-kopia"

	// uploadContentType is the media type of the uploaded byte stream, as
	// distinct from blobMimeType which is the type recorded on the Drive file.
	uploadContentType = "application/octet-stream"

	// folderMimeType identifies a Drive folder, used when creating the
	// per-repository folder from tests and tooling.
	folderMimeType = "application/vnd.google-apps.folder"

	// driveRootFolderAlias is the ID Drive accepts anywhere a folder ID is
	// expected to mean "the root of the signed-in user's My Drive". It is where
	// Options.FolderName resolution looks, and where a folder created from
	// Options.FolderName is placed.
	driveRootFolderAlias = "root"

	// folderNameProbePageSize bounds how many same-named folders the name
	// resolution looks at. More than one is already an error; the page size only
	// decides how many IDs the error message can name.
	folderNameProbePageSize = 100

	// folderWriteFields is the files.create response mask when the repository
	// folder itself is created: the ID is the answer, the name confirms what was
	// made.
	folderWriteFields = "id,name"

	// folderLookupFields is the files.list mask of the folder-name query. The
	// name is requested because Drive's `=` is case-insensitive and the match has
	// to be re-checked exactly.
	folderLookupFields = "files(" + folderWriteFields + ")"
)

// Tuning defaults. Every knob of Options.Tuning that is left at its zero value
// resolves to the constant below; they are gathered here so that the defaults
// are greppable in one place (PHASE2_DESIGN.md Contract 5).
const (
	// defaultUploadChunkSizeMB is the resumable-upload chunk size. Drive requires
	// a multiple of 256 KiB; whole megabytes always are.
	defaultUploadChunkSizeMB = 64

	// defaultSimpleUploadCutoffMB is the size at or above which a blob is
	// uploaded with the resumable protocol instead of a single multipart request.
	defaultSimpleUploadCutoffMB = 8

	// defaultListPageSize is the files.list page size; 1000 is the API maximum.
	defaultListPageSize = 1000

	// defaultDeleteParallelism bounds how many delete requests this connection
	// keeps in flight. Kopia's own fan-out (blob.DeleteMultiple, called by epoch
	// maintenance and the list cache) chooses its own parallelism - four for
	// epoch cleanup, one per prefix for the list cache - and nothing stops a
	// future caller from choosing hundreds. This is the backend's own ceiling:
	// the pacer already limits the RATE, but not the number of simultaneously
	// open connections, and Drive is happier with a handful of reused ones than
	// with a burst of new TLS handshakes.
	defaultDeleteParallelism = 8

	// defaultHTTP2ReadIdleTimeoutSec and defaultHTTP2PingTimeoutSec are the
	// HTTP/2 health check google.golang.org/api/transport/http configures for
	// its own clients (31s idle, 15s ping), which this backend bypasses by
	// supplying its own HTTP client. A connection whose peer vanished - the
	// classic resume-from-suspend case - is detected within ~46s instead of
	// waiting out the kernel's TCP retransmission timeout (tens of minutes).
	defaultHTTP2ReadIdleTimeoutSec = 31
	defaultHTTP2PingTimeoutSec     = 15

	// defaultResponseHeaderTimeoutSec bounds the wait for response headers. The
	// clock starts only once the request body is fully written, so it does not
	// limit upload size or speed; it covers the server finalizing a resumable
	// chunk or a file, which takes seconds. Two minutes is generous enough for a
	// slow finalize and catches a peer that keeps the connection alive (answers
	// HTTP/2 pings) but never answers the request.
	defaultResponseHeaderTimeoutSec = 120

	// defaultIOIdleTimeoutSec closes a connection on which nothing was read or
	// written for this long. It matches rclone's --timeout default and is the
	// backstop for HTTP/1.1 connections, which have no ping.
	defaultIOIdleTimeoutSec = 300

	// bytesPerMB converts the megabyte-denominated tuning knobs.
	bytesPerMB = 1 << 20
)

// Batched deletes (Options.Tuning.UseBatchDelete).
const (
	// maxBatchDeleteSize is Drive's documented hard limit: "You're limited to 100
	// calls in a single batch request." (RECON.md C.1). More than that "might
	// cause an error".
	maxBatchDeleteSize = 100

	// deleteBatchWindow is how long the goroutine assembling a batch waits for
	// company before sending what it has. It is a latency/coalescing trade: a
	// delete issued on its own pays the whole window before anything happens,
	// which is why batching is opt-in. Kopia only ever deletes blobs in bulk
	// (maintenance, epoch cleanup, GC), so in the workloads that matter the
	// window is filled long before it expires.
	deleteBatchWindow = 20 * time.Millisecond

	// batchAPIPath is the Drive-specific homogeneous batch endpoint, relative to
	// the API root. The global /batch endpoint was retired in 2019; the
	// per-API one is current and carries no deprecation notice (RECON.md C.1).
	batchAPIPath = "batch/drive/v3"

	// drivePathSuffix is the trailing path of drive.Service.BasePath, which the
	// batch endpoint replaces.
	drivePathSuffix = "drive/v3/"

	// batchPartContentType is the media type every part of a batch body carries.
	batchPartContentType = "application/http"

	// batchContentIDPrefix namespaces the Content-ID headers that tie a response
	// part back to the request part it answers. Drive echoes "<response-ID>".
	batchContentIDPrefix = "kopia-"
	batchResponsePrefix  = "response-"
)

// Name-resolution miss protocol.
//
// A blob is addressed by Drive file NAME, and Drive's name index is eventually
// consistent: a files.list query can miss a file that files.get would happily
// return. The persistent blobID->fileID cache (gdrive_fileids.go) removes this
// problem for anything this PROCESS wrote, and its process-wide registry
// removes it for every other connection in the process - which is what fixes
// the five-connection scenario of issue #4272, where "kopia repository
// validate-provider" reads through one connection what another just wrote.
//
// What is left is a blob written by another PROCESS or another MACHINE. The two
// are not equally expensive to cover:
//
//   - Another process on this machine shares the cache journal, so its writes
//     are recoverable by re-reading the journal tail: a local file read, no
//     round trip, no rate-limit budget. That is the first thing a miss does.
//   - Another machine can only be caught by re-querying the name index, which
//     costs a 100-quota-unit call and real latency, and is worth doing only
//     when there is reason to believe a write happened recently.
//
// So a miss escalates to the network only when the journal tail shows that
// somebody has been writing to this repository since we last looked. Otherwise
// - the overwhelmingly common case, an honestly absent blob, which Kopia treats
// as an ordinary expected outcome and asks about constantly - the answer comes
// back after one confirming query instead of after six queries spread over five
// seconds. Live evidence from the WP-2c validation run is that the escalation
// never fired at all, while the budget was charged to every absent-blob lookup.
//
// PutBlob deliberately does not use this protocol: resolveFileIDForWrite treats
// a miss as "absent" immediately and lets the duplicate-resolution protocol
// clean up after a wrong guess, because making every overwrite wait would be
// far worse than an occasional extra file. The create-retry path inside
// uploadNewFile likewise re-queries directly (listNewestByName), because there
// it is looking for a file it has just written itself.
//
// These are package constants rather than Options.Tuning knobs: Contract 5 does
// not include them.
const (
	// nameQueryRetryInitialDelay is the first escalation step, and also the pause
	// before the single confirming query.
	nameQueryRetryInitialDelay = 100 * time.Millisecond

	// nameQueryRetryMaxDelay caps one escalation step.
	nameQueryRetryMaxDelay = 1 * time.Second

	// nameQueryRetryBudget bounds the whole escalation, and only applies when
	// the journal showed concurrent write activity.
	nameQueryRetryBudget = 2 * time.Second
)

// Field masks. Drive charges for, and Kopia pays for, every field it does not
// ask for, so these are exactly what each call needs and no more.
const (
	// fileMetadataFields is what blob.Metadata needs from a files.get.
	fileMetadataFields = "id,name,size,modifiedTime"

	// fileWriteFields is the response mask for files.create/files.update: the ID
	// goes into the cache and modifiedTime answers PutOptions.GetModTime.
	fileWriteFields = "id,name,modifiedTime"

	// nameLookupFields resolves a name to a file. createdTime is requested so a
	// duplicate name can be resolved newest-first.
	nameLookupFields = "files(id,name,createdTime)"

	// nameLookupMetadataFields is nameLookupFields plus everything
	// blob.Metadata needs, so that GetMetadata never needs a second call.
	nameLookupMetadataFields = "files(id,name,size,modifiedTime,createdTime)"

	// listFields is the ListBlobs page mask. createdTime is deliberately absent:
	// the ordering that lets duplicates be collapsed is applied server-side by
	// orderBy, so the value itself is never read, and rclone's rule of never
	// requesting a field speculatively (RECON.md B.5.2) applies to a call that
	// runs once per thousand blobs in the repository.
	listFields = "nextPageToken,files(id,name,size,modifiedTime)"
)

// Server-side prefix narrowing.
//
// ListBlobs is given a prefix, and Drive can filter on it with `name contains`.
// Two facts bound how useful that is:
//
//   - `contains` is documented as a substring match, not a prefix match, so it
//     can only ever be a NARROWING hint; the client-side prefix filter stays as
//     the correctness backstop. (Google's docs state that `contains` on `name`
//     matches from the start of the value, but the guarantee is not worded
//     strongly enough to make correctness depend on it.)
//   - Most of Kopia's prefixes are one or two characters (`p`, `q`, `s`, `n`,
//     `m`, `l`, `x`, `xe`, `xn`, `xr`, `xs`, `xw`) and select a large fraction
//     of the repository, so narrowing on them buys nothing and costs a term in
//     every query.
//
// But a real minority of call sites do use long prefixes, and for those the
// difference is between reading one page and paging through the entire
// repository:
//
//	xn<epoch>_                internal/epoch/epoch_manager.go:647,1055,1099
//	kopia.repository          repo/maintenance/blob_retain.go:93
//	kopia.blobcfg             repo/maintenance/blob_retain.go:93
//	kopia.repository.backup.  repo/format/upgrade_lock.go:142
//	_log_                     repo/maintenance/cleanup_logs.go:58, cli/command_logs_session.go:73
//	<full blob ID>            repo/format/format_blob.go:91
//	z<uuid>                   internal/providervalidation/providervalidation.go:483,491
//
// so narrowing is applied unconditionally above the length threshold.
const minPrefixNarrowingLength = 4

const (
	// nameProbePageSize is the page size of a name lookup: one result is the
	// answer, two is enough to detect that duplicates exist.
	nameProbePageSize = 2

	// duplicateResolutionPageSize bounds how many same-named files the
	// duplicate-resolution protocol will look at. Duplicates are pathological;
	// more than a handful means something is badly wrong, and the protocol still
	// converges because it re-runs on the next write.
	duplicateResolutionPageSize = 100

	// listOrderBy makes files.list return same-named files adjacently, newest
	// first. That is what lets ListBlobs collapse duplicate names in constant
	// memory: it only has to remember the previous name, not every name emitted
	// so far (a repository has millions of blobs).
	listOrderBy = "name,createdTime desc"
)

var log = logging.Module("gdrive")

// testDriveTransports is the seam through which tests point specific folder IDs
// at a fake Drive server: it replaces the OAuth-authenticated transport and the
// API endpoint, while everything else - instrumentation, pacer, ID cache,
// name resolution - is built exactly as in production, so the mock suite
// exercises the real code path. It is always empty in production, and it is
// keyed by folder ID (rather than being a single function pointer) so that
// tests can register concurrently without racing.
//
//nolint:gochecknoglobals
var testDriveTransports sync.Map // map[string]testDriveTransport

type testDriveTransport struct {
	// base replaces the OAuth-authenticated transport. nil selects the
	// production transport (newHTTPTransport) without authentication, which is
	// how tests exercise the stall timeouts against the fake server.
	base     http.RoundTripper
	endpoint string

	// timeouts, when set, replaces the resolved transport timeouts, which the
	// seconds-denominated tuning knobs cannot make small enough for a test.
	timeouts *transportTimeouts
}

// testTransportKey is the key a test registers its fake Drive under: the folder
// ID normally, and the folder NAME prefixed with "name:" when the folder is
// still to be created and therefore has no ID for either side to agree on yet.
func testTransportKey(opt *Options) string {
	if opt.FolderID != "" {
		return opt.FolderID
	}

	return "name:" + opt.FolderName
}

// resolvedTuning is Options.Tuning with all defaults applied and all unit
// conversions done, so that no hot path ever has to think about zero values.
type resolvedTuning struct {
	pacer              gdrivepacer.Config
	uploadChunkSize    int
	simpleUploadCutoff int64
	listPageSize       int64
	deleteParallelism  int
	useBatchDelete     bool
	cacheDir           string
	timeouts           transportTimeouts
}

type gdriveStorage struct {
	Options
	blob.DefaultProviderImplementation

	service *drive.Service
	files   *drive.FilesService
	about   *drive.AboutService

	httpClient *http.Client

	folderID string
	tuning   resolvedTuning

	pacer   *gdrivepacer.Pacer
	stats   *TransportStats
	idCache *persistentIDCache

	// deleteSem bounds the number of delete requests in flight at once; see
	// defaultDeleteParallelism.
	deleteSem chan struct{}

	// batchURL is the absolute URL of the homogeneous batch endpoint, derived
	// from the service's base path so that the test seam's endpoint override is
	// honored. Empty unless batching is enabled.
	batchURL string

	// deleteBatch coalesces concurrent deletes into batch requests. nil unless
	// Options.Tuning.UseBatchDelete is set.
	deleteBatch *deleteCollector

	closeOnce sync.Once
}

var _ blob.Storage = (*gdriveStorage)(nil)

//
// ---------------------------------------------------------------- construction
//

// New creates new Google Drive-backed storage with specified options:
//
// - the 'folderID' field is required and all other parameters are optional,
// except that a repository being CREATED may name a folder to make instead:
// with 'folderID' empty and 'folderName' set, New reuses or creates a folder of
// that name in the root of My Drive and writes its ID into opt.FolderID, so
// that everything downstream - this storage, its ConnectionInfo, the repository
// config file and any repository token minted from it - names a concrete
// folder. That is deliberately the ONLY mutation New makes to opt: the ID has
// to be visible to the caller, because it is what another machine connects
// with.
//
// Credentials are resolved by gdrive_auth.go: explicit service-account or
// authorized-user JSON, an interactive OAuth sign-in against a client secret
// file, or Google application default credentials.
//
// The returned storage is deliberately NOT wrapped in repo/blob/retrying: the
// pacer is this backend's retry layer. retrying cannot see Drive's
// Retry-After headers and would happily burn ten attempts on a permanent quota
// error (issue #2656), whereas the pacer classifies every failure through
// gdriveerr and backs the whole account off when Drive asks it to.
func New(ctx context.Context, opt *Options, isCreate bool) (blob.Storage, error) {
	if err := validateFolderOptions(opt, isCreate); err != nil {
		return nil, err
	}

	tuning := resolveTuning(opt)
	stats := NewTransportStats()

	service, httpClient, err := newDriveService(ctx, opt, tuning.timeouts, stats)
	if err != nil {
		return nil, err
	}

	pacer := gdrivepacer.New(tuning.pacer, stats)

	if opt.FolderID == "" {
		folderID, err := resolveFolderByName(ctx, service.Files, pacer, opt.FolderName)
		if err != nil {
			httpClient.CloseIdleConnections()

			return nil, err
		}

		// Written back into the caller's options on purpose; see the doc comment.
		// Everything below this line, and every consumer of ConnectionInfo, sees a
		// storage that is indistinguishable from one opened with --folder-id.
		opt.FolderID = folderID
	}

	idCache, err := newPersistentIDCache(ctx, tuning.cacheDir, opt.FolderID)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open the Google Drive file ID cache")
	}

	s := &gdriveStorage{
		Options:    *opt,
		service:    service,
		files:      service.Files,
		about:      service.About,
		httpClient: httpClient,
		folderID:   opt.FolderID,
		tuning:     tuning,
		stats:      stats,
		pacer:      pacer,
		idCache:    idCache,
		deleteSem:  make(chan struct{}, tuning.deleteParallelism),
	}

	if tuning.useBatchDelete {
		batchURL, err := batchEndpoint(service.BasePath)
		if err != nil {
			s.Close(ctx) //nolint:errcheck

			return nil, err
		}

		s.batchURL = batchURL
		s.deleteBatch = newDeleteCollector(s)
	}

	if err := s.verifyConnection(ctx); err != nil {
		s.Close(ctx) //nolint:errcheck

		return nil, err
	}

	return s, nil
}

// verifyConnection proves at connect time that the credentials work and the
// configured folder is usable, the way gcs proves its bucket exists.
//
// It takes two steps because, LIVE-VERIFIED, only the first of them actually
// detects a bad folder: Drive answers a files.list whose query names a
// non-existent parent with an empty result set, not an error, so the
// gcs-style "list a prefix that cannot match" probe alone would happily connect
// to a folder that is not there. files.get on the folder is authoritative, is
// twenty times cheaper in quota units, and produces the diagnosis a drive.file
// user needs: a folder this application did not create is invisible to it and
// 404s even though the account can see it in the web UI.
func (s *gdriveStorage) verifyConnection(ctx context.Context) error {
	var folder *drive.File

	err := s.pacer.Call(ctx, methodFilesGet, "", func() error {
		var err error

		folder, err = s.files.Get(s.folderID).SupportsAllDrives(true).Fields("id,mimeType").Context(ctx).Do()

		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return err
	})
	if err != nil {
		return errors.Wrapf(err, "unable to open the Google Drive folder %v", s.folderID)
	}

	if folder.MimeType != folderMimeType {
		return errors.Errorf("Google Drive item %v is not a folder (it is %v)", s.folderID, folder.MimeType)
	}

	// Then the listing probe, which also exercises the query and field masks
	// every other operation depends on. An empty result set is the expected
	// outcome; only an API-level failure counts.
	probe := blob.ID(fmt.Sprintf("kopia-gdrive-storage-initializing-%v", clock.Now().UnixNano()))

	if _, err := s.pacedListByName(ctx, methodFilesList, probe, nameLookupFields, nameProbePageSize); err != nil {
		return errors.Wrap(err, "unable to list from the folder")
	}

	return nil
}

//
// ------------------------------------------------------- folder by name
//

// validateFolderOptions decides which of the two ways of naming the repository
// folder is in play, and rejects the combinations that cannot mean anything.
//
// FolderID always wins when it is set: after a create-by-name the configuration
// carries BOTH fields - the ID because it is what every later connection uses,
// the name only as a record of provenance - so the two being present together
// is the normal steady state, not a conflict. The conflict that IS worth
// refusing (a user passing both on the command line) is caught at the CLI
// layer, where the two flags are still distinguishable from a config file.
func validateFolderOptions(opt *Options, isCreate bool) error {
	if opt.FolderID != "" {
		return nil
	}

	switch {
	case opt.FolderName == "":
		return errors.New("folder-id must be specified")

	case strings.TrimSpace(opt.FolderName) == "":
		return errors.New("the Google Drive folder name must not be blank")

	case opt.ReadOnly:
		return errors.New("a read-only connection cannot create the Google Drive folder: specify folder-id instead")

	case !isCreate:
		return errors.Errorf("the Google Drive folder %q can only be created while creating a repository; connect to an existing repository with folder-id", opt.FolderName)
	}

	return nil
}

// resolveFolderByName turns Options.FolderName into a folder ID, creating the
// folder in the root of My Drive if it is not there yet.
//
// This is what makes a Drive repository deployable with nothing but the kopia
// binary. Under the default drive.file scope Kopia can only ever see files it
// created itself, so a folder made by hand in the web UI is invisible to it;
// before this existed the only way to obtain an app-owned folder was to make
// one with some other tool that held the same OAuth client.
//
// It is idempotent on purpose: re-running the same create against the same
// account reuses the folder rather than accumulating same-named siblings (Drive
// permits any number of them). Two or more matches is the one case that cannot
// be resolved safely, because picking one would silently split a repository
// across folders, so it is an error that names the candidates.
func resolveFolderByName(ctx context.Context, files *drive.FilesService, pacer *gdrivepacer.Pacer, folderName string) (string, error) {
	name := strings.TrimSpace(folderName)

	existing, err := findFoldersNamed(ctx, files, pacer, name)
	if err != nil {
		return "", err
	}

	if len(existing) > 1 {
		ids := make([]string, 0, len(existing))
		for _, f := range existing {
			ids = append(ids, f.Id)
		}

		slices.Sort(ids)

		return "", errors.Errorf("found %v Google Drive folders named %q in the root of My Drive (IDs %v); rename or remove all but one, or select the one you want with folder-id",
			len(existing), name, strings.Join(ids, ", "))
	}

	if len(existing) == 1 {
		log(ctx).Infof("Reusing the existing Google Drive folder %q, ID %v.", name, existing[0].Id)

		return existing[0].Id, nil
	}

	var created *drive.File

	if err := pacer.Call(ctx, methodFilesCreate, "", func() error {
		var cerr error

		created, cerr = files.Create(&drive.File{
			Name:     name,
			MimeType: folderMimeType,
			Parents:  []string{driveRootFolderAlias},
		}).SupportsAllDrives(true).Fields(folderWriteFields).Context(ctx).Do()

		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return cerr
	}); err != nil {
		return "", errors.Wrapf(err, "unable to create the Google Drive folder %q", name)
	}

	// Read the folder back before anything is written into it. A create that
	// reports success and a folder that is not subsequently readable would
	// otherwise surface much later, as a confusing failure of the connection
	// probe against an ID the user has already been told to reuse.
	if err := verifyFolderVisible(ctx, files, pacer, created.Id); err != nil {
		return "", errors.Wrapf(err, "created the Google Drive folder %q (ID %v), but it is not readable", name, created.Id)
	}

	log(ctx).Infof("Created the Google Drive folder %q, ID %v.", name, created.Id)

	return created.Id, nil
}

// findFoldersNamed returns the folders of exactly that name in the root of My
// Drive that this application is allowed to see. Under drive.file that is only
// the ones it created, which is precisely the set a previous run of this code
// could have left behind.
func findFoldersNamed(ctx context.Context, files *drive.FilesService, pacer *gdrivepacer.Pacer, name string) ([]*drive.File, error) {
	q := fmt.Sprintf("'%s' in parents and name = '%s' and mimeType = '%s' and trashed = false",
		driveRootFolderAlias, escapeQueryLiteral(name), folderMimeType)

	var res *drive.FileList

	if err := pacer.Call(ctx, methodFilesList, "", func() error {
		var lerr error

		res, lerr = files.List().
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true).
			Q(q).
			PageSize(folderNameProbePageSize).
			Fields(googleapi.Field(folderLookupFields)).
			Context(ctx).
			Do()

		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return lerr
	}); err != nil {
		return nil, errors.Wrapf(err, "unable to look for a Google Drive folder named %q", name)
	}

	// Drive's `=` is CASE-INSENSITIVE (RECON.md B.5.3), so this query can return
	// a "Kopia" when asked for "kopia". Reusing one of those would put the
	// repository somewhere the user did not name.
	return slices.DeleteFunc(res.Files, func(f *drive.File) bool {
		return f.Name != name
	}), nil
}

// verifyFolderVisible is the files.get half of what verifyConnection does,
// applied to a folder that has just been created.
func verifyFolderVisible(ctx context.Context, files *drive.FilesService, pacer *gdrivepacer.Pacer, folderID string) error {
	var f *drive.File

	if err := pacer.Call(ctx, methodFilesGet, "", func() error {
		var gerr error

		f, gerr = files.Get(folderID).SupportsAllDrives(true).Fields("id,mimeType").Context(ctx).Do()

		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return gerr
	}); err != nil {
		//nolint:wrapcheck // the pacer returns a *gdriveerr.DriveError that already names the operation and the Drive reason.
		return err
	}

	if f.MimeType != folderMimeType {
		return errors.Errorf("Google Drive item %v is not a folder (it is %v)", folderID, f.MimeType)
	}

	return nil
}

// resolveTuning applies Contract 5 defaults to Options.Tuning.
func resolveTuning(opt *Options) resolvedTuning {
	t := opt.Tuning

	res := resolvedTuning{
		pacer: gdrivepacer.Config{
			MinSleep: time.Duration(t.PacerMinSleepMS) * time.Millisecond,
			Burst:    t.PacerBurst,
			MaxSleep: time.Duration(t.PacerMaxSleepMS) * time.Millisecond,
			MaxTries: t.MaxTries,
		},
		uploadChunkSize:    valueOrDefault(t.UploadChunkSizeMB, defaultUploadChunkSizeMB) * bytesPerMB,
		simpleUploadCutoff: int64(valueOrDefault(t.SimpleUploadCutoffMB, defaultSimpleUploadCutoffMB)) * bytesPerMB,
		listPageSize:       int64(valueOrDefault(t.ListPageSize, defaultListPageSize)),
		deleteParallelism:  valueOrDefault(t.DeleteParallelism, defaultDeleteParallelism),

		// nil means "let the backend decide", and the backend's decision - until
		// Phase 3's A/B measurement says otherwise - is the simpler path.
		useBatchDelete: t.UseBatchDelete != nil && *t.UseBatchDelete,

		cacheDir: t.CacheDir,

		timeouts: transportTimeouts{
			http2ReadIdle:  secondsOrDefault(t.HTTP2ReadIdleTimeoutSec, defaultHTTP2ReadIdleTimeoutSec),
			http2Ping:      secondsOrDefault(t.HTTP2PingTimeoutSec, defaultHTTP2PingTimeoutSec),
			responseHeader: secondsOrDefault(t.ResponseHeaderTimeoutSec, defaultResponseHeaderTimeoutSec),
			ioIdle:         secondsOrDefault(t.IOIdleTimeoutSec, defaultIOIdleTimeoutSec),
		},
	}

	if res.cacheDir == "" {
		res.cacheDir = defaultCacheDir()
	}

	if v, ok := testDriveTransports.Load(testTransportKey(opt)); ok {
		if t, _ := v.(testDriveTransport); t.timeouts != nil {
			res.timeouts = *t.timeouts
		}
	}

	return res
}

func secondsOrDefault(v, def int) time.Duration {
	return time.Duration(valueOrDefault(v, def)) * time.Second
}

func valueOrDefault(v, def int) int {
	if v <= 0 {
		return def
	}

	return v
}

// defaultCacheDir returns the root under which the blobID->fileID cache lives
// when Options.Tuning.CacheDir is unset.
//
// internal/ospath offers ConfigDir and LogsDir but no cache directory, so this
// follows what repo.setupCachingOptionsWithDefaults does for the content cache:
// os.UserCacheDir() + "/kopia". A caller that wants the cache next to the rest
// of a repository's caches sets Options.Tuning.CacheDir explicitly.
func defaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		// os.UserCacheDir only fails when the environment names no home at all.
		// A temp directory still gives cross-connection read-your-writes for the
		// lifetime of the machine's temp space, which is the property that
		// matters most, so degrade rather than refuse to connect.
		return filepath.Join(os.TempDir(), "kopia")
	}

	return filepath.Join(d, "kopia")
}

// newDriveService builds the Drive client stack:
//
//	newHTTPTransport                 (stall timeouts, HTTP/2 health checks)
//	  -> oauth2.Transport            (adds Authorization; token source from gdrive_auth.go)
//	    -> instrumentedTransport     (counts calls, bytes, latency, statuses)
//	      -> drive.Service
//
// The instrumentation sits OUTSIDE the OAuth transport on purpose, so that a
// token-refresh round trip is attributed like any other call and the byte
// counters see exactly what went on the wire.
func newDriveService(ctx context.Context, opt *Options, timeouts transportTimeouts, stats *TransportStats) (*drive.Service, *http.Client, error) {
	base, endpoint, err := newBaseTransport(ctx, opt, timeouts)
	if err != nil {
		return nil, nil, err
	}

	httpClient := &http.Client{Transport: newInstrumentedTransport(base, stats)}

	clientOpts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if endpoint != "" {
		clientOpts = append(clientOpts, option.WithEndpoint(endpoint))
	}

	service, err := drive.NewService(ctx, clientOpts...)
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to create Drive client")
	}

	// Identify Kopia to the provider. The previous implementation did not do
	// this, so Drive-side traffic from Kopia was indistinguishable from any
	// other Go client. drive.NewService never populates this field itself, even
	// when option.WithUserAgent is supplied together with a custom HTTP client.
	service.UserAgent = blob.ApplicationID

	return service, httpClient, nil
}

func newBaseTransport(ctx context.Context, opt *Options, timeouts transportTimeouts) (http.RoundTripper, string, error) {
	if v, ok := testDriveTransports.Load(testTransportKey(opt)); ok {
		t, _ := v.(testDriveTransport)
		if t.base == nil {
			return newHTTPTransport(timeouts), t.endpoint, nil
		}

		return t.base, t.endpoint, nil
	}

	scope, err := driveScope(opt)
	if err != nil {
		return nil, "", err
	}

	ts, authMode, err := newTokenSource(ctx, opt, scope)
	if err != nil {
		return nil, "", errors.Wrap(err, "unable to initialize token source")
	}

	log(ctx).Debugf("Google Drive authentication mode: %v, scope %v", authMode, scope)

	// Same shape as oauth2.NewClient, but built explicitly so the transport can
	// be wrapped afterwards.
	return &oauth2.Transport{
		Base:   newHTTPTransport(timeouts),
		Source: oauth2.ReuseTokenSource(nil, ts),
	}, "", nil
}

// CreateDriveService creates a new Google Drive service, which encapsulates
// multiple clients used to access different Google Drive functionality.
// Exported for tests and tooling that need to manage the repository folder
// itself (which is not a blob and therefore not reachable through blob.Storage).
func CreateDriveService(ctx context.Context, opt *Options) (*drive.Service, error) {
	service, _, err := newDriveService(ctx, opt, resolveTuning(opt).timeouts, NewTransportStats())

	return service, err
}

//
// ------------------------------------------------------------- blob.Storage
//

func (s *gdriveStorage) GetCapacity(ctx context.Context) (blob.Capacity, error) {
	var res *drive.About

	err := s.pacer.Call(ctx, methodOther, "", func() error {
		var err error

		res, err = s.about.Get().Fields("storageQuota").Context(ctx).Do()

		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return err
	})
	if err != nil {
		return blob.Capacity{}, errors.Wrap(err, "get about in GetCapacity()")
	}

	q := res.StorageQuota
	if q == nil || q.Limit == 0 {
		// An unset limit means the Drive has no size limit (Workspace pooled or
		// unlimited storage), which is not a volume Kopia can report on.
		return blob.Capacity{}, blob.ErrNotAVolume
	}

	free := max(q.Limit-q.Usage, 0)

	return blob.Capacity{
		SizeB: uint64(q.Limit), //nolint:gosec // Limit > 0 was checked above.
		FreeB: uint64(free),
	}, nil
}

func (s *gdriveStorage) GetBlob(ctx context.Context, blobID blob.ID, offset, length int64, output blob.OutputBuffer) error {
	if offset < 0 {
		output.Reset()

		return blob.ErrInvalidRange
	}

	if length == 0 {
		// A zero-length read still has to prove the blob exists, but there is no
		// point spending a download on it: the previous implementation asked for
		// one byte and threw it away. Metadata answers the question for the price
		// of a 5-quota-unit call instead of a 200-unit one.
		output.Reset()

		_, err := s.GetMetadata(ctx, blobID)

		return err
	}

	return s.withFileID(ctx, methodFilesGetMedia, blobID, func(fileID string) error {
		return s.download(ctx, blobID, fileID, offset, length, output)
	})
}

// download fetches one byte range of one file. It is safe to call repeatedly:
// the output buffer is reset at the start of every attempt, so a retry never
// appends to a partial result and a failure never leaves bytes behind (the
// suite asserts an empty buffer after a not-found read).
func (s *gdriveStorage) download(ctx context.Context, blobID blob.ID, fileID string, offset, length int64, output blob.OutputBuffer) error {
	err := s.pacer.Call(ctx, methodFilesGetMedia, string(blobID), func() error {
		output.Reset()

		req := s.files.Get(fileID).SupportsAllDrives(true)

		if r := toRange(offset, length); r != "" {
			req.Header().Set("Range", r)
		}

		res, err := req.Context(ctx).Download()
		if err != nil {
			//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
			return err
		}
		defer res.Body.Close() //nolint:errcheck

		return iocopy.JustCopy(output, res.Body)
	})
	if err != nil {
		//nolint:wrapcheck // the pacer returns a *gdriveerr.DriveError that already names the operation, the blob and the Drive reason; wrapping it again would only duplicate that text.
		return err
	}

	//nolint:wrapcheck // EnsureLengthExactly returns blob.ErrInvalidRange, which callers match with errors.Is.
	return blob.EnsureLengthExactly(output.Length(), length)
}

func (s *gdriveStorage) GetMetadata(ctx context.Context, blobID blob.ID) (blob.Metadata, error) {
	if fileID, ok := s.idCache.Get(blobID); ok {
		bm, err := s.getMetadataByFileID(ctx, blobID, fileID)
		if err == nil {
			return bm, nil
		}

		if !errors.Is(err, blob.ErrBlobNotFound) {
			return blob.Metadata{}, err
		}

		// The cache is an optimization, never an authority: a cached ID that 404s
		// is invalidated and the name is resolved again from Drive.
		s.idCache.Delete(blobID)
	}

	ref, err := s.resolveBlobFile(ctx, methodFilesList, blobID, nameLookupMetadataFields)
	if err != nil {
		return blob.Metadata{}, err
	}

	if ref.file == nil {
		// Resolved from the local journal, which knows the ID and nothing else.
		return s.getMetadataByFileID(ctx, blobID, ref.fileID)
	}

	s.idCache.Put(blobID, ref.file.Id)

	return parseFileMetadata(ref.file, blobID)
}

func (s *gdriveStorage) getMetadataByFileID(ctx context.Context, blobID blob.ID, fileID string) (blob.Metadata, error) {
	var f *drive.File

	err := s.pacer.Call(ctx, methodFilesGet, string(blobID), func() error {
		var err error

		f, err = s.files.Get(fileID).SupportsAllDrives(true).Fields(fileMetadataFields).Context(ctx).Do()

		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return err
	})
	if err != nil {
		//nolint:wrapcheck // the pacer returns a *gdriveerr.DriveError that already names the operation, the blob and the Drive reason; wrapping it again would only duplicate that text.
		return blob.Metadata{}, err
	}

	return parseFileMetadata(f, blobID)
}

func (s *gdriveStorage) PutBlob(ctx context.Context, blobID blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	if opts.HasRetentionOptions() {
		return errors.Wrap(blob.ErrUnsupportedPutBlobOption, "blob-retention")
	}

	// Writers of the same blob ID in this process are serialized. Drive cannot
	// create a file conditionally, so two concurrent creates of one name produce
	// two files with that name - a state this backend must never enter, because
	// deleting "the" blob would then leave the other copy behind. Readers never
	// take this lock, and different blob IDs never contend, so the practical cost
	// is zero: Kopia's blob IDs are content-derived and it does not write the same
	// one twice concurrently. Cross-process races are handled by the
	// duplicate-resolution protocol in resolveCreateRace.
	defer lockBlob(s.folderID, blobID)()

	fileID, err := s.resolveFileIDForWrite(ctx, blobID)

	switch {
	case err == nil:
		if opts.DoNotRecreate {
			return blob.ErrBlobAlreadyExists
		}

		return s.updateBlob(ctx, blobID, fileID, data, opts)

	case errors.Is(err, blob.ErrBlobNotFound):
		return s.createBlob(ctx, blobID, data, opts)

	default:
		return err
	}
}

// resolveFileIDForWrite finds the file backing a blob without the escalating
// re-query the read path uses: a write that guesses "absent" creates the file,
// and creating a file that already exists is recoverable (the duplicate is
// resolved), whereas making every overwrite wait five seconds is not.
func (s *gdriveStorage) resolveFileIDForWrite(ctx context.Context, blobID blob.ID) (string, error) {
	if fileID, ok := s.idCache.Get(blobID); ok {
		return fileID, nil
	}

	f, err := s.lookupFileByName(ctx, methodFilesList, blobID, nameLookupFields)
	if err != nil {
		return "", err
	}

	s.idCache.Put(blobID, f.Id)

	return f.Id, nil
}

func (s *gdriveStorage) updateBlob(ctx context.Context, blobID blob.ID, fileID string, data blob.Bytes, opts blob.PutOptions) error {
	var f *drive.File

	op := s.uploadOp(data.Length())

	err := s.pacer.Call(ctx, op, string(blobID), func() error {
		reader := data.Reader()
		defer reader.Close() //nolint:errcheck

		var err error

		f, err = s.files.Update(fileID, &drive.File{ModifiedTime: modifiedTimeOf(opts)}).
			SupportsAllDrives(true).
			Fields(fileWriteFields).
			Media(reader, s.mediaOptions(data.Length())...).
			Context(ctx).
			Do()

		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return err
	})

	if errors.Is(err, blob.ErrBlobNotFound) {
		// The file was deleted between resolution and update. Forget it and write
		// a new one, which is what "PutBlob creates or replaces" means.
		s.idCache.Delete(blobID)

		return s.createBlob(ctx, blobID, data, opts)
	}

	if err != nil {
		//nolint:wrapcheck // the pacer returns a *gdriveerr.DriveError that already names the operation, the blob and the Drive reason; wrapping it again would only duplicate that text.
		return err
	}

	s.idCache.Put(blobID, f.Id)

	return applyGetModTime(opts, f)
}

func (s *gdriveStorage) createBlob(ctx context.Context, blobID blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	f, err := s.uploadNewFile(ctx, blobID, data, opts)
	if err != nil {
		return err
	}

	// Write-through, before anything else can fail: this is what gives every
	// other connection in this process read-your-writes without a name query,
	// and it is the core of the fix for issue #4272.
	s.idCache.Put(blobID, f.Id)

	if opts.DoNotRecreate {
		if err := s.resolveCreateRace(ctx, blobID, f); err != nil {
			return err
		}
	}

	return applyGetModTime(opts, f)
}

// uploadNewFile creates the Drive file holding a blob.
//
// The pacer may retry, and a create that failed after Drive had already
// committed it would then be created twice. So from the second attempt onwards
// the name is looked up first: if the previous attempt did land, its file is
// adopted instead of a second one being created. This closes the common
// duplicate-producing window (a network error on the response of a successful
// create) without costing anything on the happy path.
func (s *gdriveStorage) uploadNewFile(ctx context.Context, blobID blob.ID, data blob.Bytes, opts blob.PutOptions) (*drive.File, error) {
	var (
		result  *drive.File
		attempt int
	)

	op := s.uploadOp(data.Length())

	err := s.pacer.Call(ctx, op, string(blobID), func() error {
		if attempt > 0 {
			if f, lerr := s.listNewestByName(ctx, blobID, fileWriteFields); lerr == nil && f != nil {
				result = f

				return nil
			}
		}

		attempt++

		reader := data.Reader()
		defer reader.Close() //nolint:errcheck

		f, err := s.files.Create(&drive.File{
			Name:         toFileName(blobID),
			Parents:      []string{s.folderID},
			MimeType:     blobMimeType,
			ModifiedTime: modifiedTimeOf(opts),
		}).
			SupportsAllDrives(true).
			Fields(fileWriteFields).
			Media(reader, s.mediaOptions(data.Length())...).
			Context(ctx).
			Do()
		if err != nil {
			//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
			return err
		}

		result = f

		return nil
	})
	if err != nil {
		//nolint:wrapcheck // the pacer returns a *gdriveerr.DriveError that already names the operation, the blob and the Drive reason; wrapping it again would only duplicate that text.
		return nil, err
	}

	return result, nil
}

// resolveCreateRace implements DoNotRecreate for a store that has no
// conditional create.
//
// Drive v3 offers no create-if-absent precondition, so the semantics are
// emulated: create the file, then look at every file with that name and let
// exactly one survive. The survivor is the OLDEST by createdTime - the writer
// who got there first - with the lexicographically smaller file ID breaking a
// tie, because Drive's createdTime has millisecond resolution and two creates
// can share a timestamp. A loser deletes the file it just created and reports
// blob.ErrBlobAlreadyExists, which is the sentinel the contract requires; the
// winner keeps its file and returns success.
//
// The protocol converges without coordination because every participant applies
// the same total order to the same set of files. What it does NOT do is make
// the operation atomic: between our create and our dedupe query, a reader can
// observe two files with this name, and a writer that used a plain PutBlob (no
// DoNotRecreate) never runs this check at all. Both are tolerable here only
// because Kopia's blob names are content-derived: the duplicates hold identical
// bytes, so whichever one survives, readers see the same blob. This is the
// residual race behind issue #3788, reduced to a window that cannot corrupt
// data.
func (s *gdriveStorage) resolveCreateRace(ctx context.Context, blobID blob.ID, created *drive.File) error {
	files, err := s.listByName(ctx, methodFilesList, blobID, nameLookupFields, duplicateResolutionPageSize)
	if err != nil {
		if errors.Is(err, blob.ErrBlobNotFound) {
			// Our own file is not in the index yet; nothing to resolve against.
			return nil
		}

		return err
	}

	winner := oldestFile(files)
	if winner == nil || winner.Id == created.Id {
		return nil
	}

	log(ctx).Warnf("Concurrent creation of blob %v detected on Google Drive, keeping file %v and deleting %v",
		blobID, winner.Id, created.Id)

	if derr := s.deleteFileByID(ctx, blobID, created.Id); derr != nil && !errors.Is(derr, blob.ErrBlobNotFound) {
		return errors.Wrapf(derr, "unable to delete the losing duplicate of %v", blobID)
	}

	s.idCache.Put(blobID, winner.Id)

	return blob.ErrBlobAlreadyExists
}

// DeleteBlob removes one blob.
//
// Kopia has no bulk-delete entry point in blob.Storage: maintenance deletes
// large sets through blob.DeleteMultiple (repo/blob/storage.go:368), which is
// nothing more than this method called from a bounded pool of goroutines. So
// the cost of deleting N blobs is decided here, one blob at a time, and the
// whole of it is the single files.delete this reaches on a cache hit - no
// verification read before it and no name query after it. Concurrency is
// bounded by deleteSem and paced by the pacer, both of which are shared by
// every caller of this method, so an aggressive fan-out above cannot turn into
// an aggressive fan-out on the wire.
func (s *gdriveStorage) DeleteBlob(ctx context.Context, blobID blob.ID) error {
	defer lockBlob(s.folderID, blobID)()

	// Fast path: the file this process knows about is deleted without a name
	// query. In the pathological case where the folder holds several files with
	// this name, that leaves the others behind until something resolves the name
	// again (see the loop below, which deletes every match). Paying for a
	// files.list on every delete to cover a state the write path is designed to
	// prevent - and which content-derived blob names make almost unreachable -
	// would cost 100 quota units per deleted blob for nothing.
	if fileID, ok := s.idCache.Get(blobID); ok {
		err := s.deleteFileByID(ctx, blobID, fileID)

		switch {
		case err == nil:
			s.idCache.Delete(blobID)

			return nil

		case errors.Is(err, blob.ErrBlobNotFound):
			// Stale cache entry; forget it and check the name index in case
			// another writer's file carries this name.
			s.idCache.Delete(blobID)

		default:
			return err
		}
	}

	files, err := s.listByName(ctx, methodFilesList, blobID, nameLookupFields, duplicateResolutionPageSize)
	if err != nil {
		if errors.Is(err, blob.ErrBlobNotFound) {
			// Deleting a blob that is not there is not an error: the suite
			// requires two successive deletes of the same blob to both succeed.
			s.idCache.Delete(blobID)

			return nil
		}

		return err
	}

	// Every file carrying this name is the blob, so all of them go: leaving a
	// duplicate behind would make a deleted blob reappear in ListBlobs.
	for _, f := range files {
		if err := s.deleteFileByID(ctx, blobID, f.Id); err != nil && !errors.Is(err, blob.ErrBlobNotFound) {
			return err
		}
	}

	s.idCache.Delete(blobID)

	return nil
}

// deleteFileByID permanently deletes a file. It never trashes: trashed items
// still count against Drive's 500,000-items-per-folder and per-Shared-Drive
// limits, so a repository that trashed its blobs would eventually refuse writes
// while appearing to have deleted them (RECON.md C.4).
//
// With batching enabled this hands the file to the collector, which merges it
// with whatever other deletes are in flight; otherwise it is one request.
// Either way the concurrency ceiling and the pacer apply.
func (s *gdriveStorage) deleteFileByID(ctx context.Context, blobID blob.ID, fileID string) error {
	if s.deleteBatch != nil {
		return s.deleteBatch.delete(ctx, blobID, fileID)
	}

	return s.deleteFileByIDNow(ctx, blobID, fileID)
}

// deleteFileByIDNow issues one files.delete, bypassing the collector.
func (s *gdriveStorage) deleteFileByIDNow(ctx context.Context, blobID blob.ID, fileID string) error {
	release, err := s.acquireDeleteSlot(ctx)
	if err != nil {
		return err
	}

	defer release()

	//nolint:wrapcheck // the pacer returns a *gdriveerr.DriveError that already names the operation, the blob and the Drive reason.
	return s.pacer.Call(ctx, methodFilesDelete, string(blobID), func() error {
		// Fields("") asks Drive for an empty response body. A delete answers 204
		// with no content anyway, but saying so costs nothing and is what rclone
		// does on the same call (RECON.md B.3.1).
		return s.files.Delete(fileID).SupportsAllDrives(true).Fields("").Context(ctx).Do()
	})
}

// acquireDeleteSlot takes one of the connection's delete slots, returning the
// function that gives it back. It respects context cancellation while waiting.
func (s *gdriveStorage) acquireDeleteSlot(ctx context.Context) (func(), error) {
	select {
	case s.deleteSem <- struct{}{}:
		return func() { <-s.deleteSem }, nil

	case <-ctx.Done():
		//nolint:wrapcheck // context errors are propagated verbatim.
		return nil, ctx.Err()
	}
}

//
// -------------------------------------------------------------- batch deletes
//
// Drive supports homogeneous batching: up to 100 sub-requests posted as one
// multipart/mixed body to batch/drive/v3, each answered by its own part with
// its own status (RECON.md C.1). It saves ROUND TRIPS ONLY - "a set of n
// requests batched together counts toward your usage limit as n requests" - so
// it is worth having exactly to the extent that a delete workload is
// latency-bound rather than quota-bound, which is what Phase 3 measures. Until
// then it is off by default and the default path is bounded-parallel singles.
//
// The Go client has no batch support, so the request is built by hand. There is
// no aggregation point in blob.Storage to build a batch from - DeleteBlob is
// synchronous and single-blob - so the batch is assembled from calls that are
// ALREADY concurrent: a caller that finds no batch forming starts one, waits a
// short window for company, and then sends and distributes the results. No
// background goroutine, no flush timer, nothing to shut down: the work is
// always done by a goroutine that was going to block on a delete anyway.
//
// FIRST LIVE MEASUREMENT (2026-08-01, 200 tiny blobs deleted through
// blob.DeleteMultiple at DeleteParallelism=8, real Drive):
//
//	parallel singles, default pacer   10.3 s  (19.5 blobs/s), 200 requests
//	batch,            default pacer   23.1 s  ( 8.6 blobs/s),  25 requests
//	parallel singles, relaxed pacer    8.7 s  (23.0 blobs/s), 200 requests
//	batch,            relaxed pacer   26.4 s  ( 7.6 blobs/s),  25 requests
//
// Batching LOST, by a factor of about 2.5, and the reason is structural rather
// than incidental: Drive executes the sub-requests of a batch one after another,
// so a batch of eight turns eight overlapping round trips into one round trip
// containing eight serial server-side deletes (~0.93 s per batch of eight
// against ~0.41 s for one delete with eight in flight). Coalescing only pays
// when the caller's fan-out is much wider than the connection count, which for
// blob.DeleteMultiple it never is. Phase 3 owns the final call; on this evidence
// the knob to turn is DeleteParallelism, not UseBatchDelete.

// deleteRequest is one blob's delete, waiting for its batch.
type deleteRequest struct {
	blobID blob.ID
	fileID string

	// ctx is the caller's own context, kept so that a request can be told apart
	// from the batch it happened to travel in: the batch is issued under the
	// context of whichever caller assembled it, and that caller giving up must
	// not be reported to everybody else as their own cancellation.
	ctx context.Context //nolint:containedctx

	// done carries the final outcome to the waiting caller. Buffered, so the
	// sender never blocks on a caller that has given up.
	done chan error

	// resolved and err are written only by the goroutine currently flushing the
	// batch this request belongs to, and read only after that goroutine has
	// handed the result over through done.
	resolved bool
	err      error
}

// deleteCollector coalesces concurrent deletes into batch requests.
type deleteCollector struct {
	s *gdriveStorage

	window  time.Duration // test hook
	maxSize int           // test hook

	// full is signaled when pending reaches maxSize, so a batch that fills up
	// early does not wait out the window.
	full chan struct{}

	mu sync.Mutex
	// +checklocks:mu
	pending []*deleteRequest
	// +checklocks:mu
	leading bool
}

func newDeleteCollector(s *gdriveStorage) *deleteCollector {
	return &deleteCollector{
		s:       s,
		window:  deleteBatchWindow,
		maxSize: maxBatchDeleteSize,
		full:    make(chan struct{}, 1),
	}
}

// delete enqueues one delete and blocks until it has been answered.
//
// A caller whose context is canceled while it waits stops waiting, but its
// request stays in the batch: the delete may still happen, exactly as an
// in-flight HTTP request may still be served after its caller walks away.
func (c *deleteCollector) delete(ctx context.Context, blobID blob.ID, fileID string) error {
	req := &deleteRequest{blobID: blobID, fileID: fileID, ctx: ctx, done: make(chan error, 1)}

	c.mu.Lock()

	c.pending = append(c.pending, req)
	lead := !c.leading

	if lead {
		c.leading = true
	}

	if len(c.pending) >= c.maxSize {
		postSignal(c.full)
	}

	c.mu.Unlock()

	if lead {
		c.lead(ctx)
	}

	select {
	case err := <-req.done:
		return err

	case <-ctx.Done():
		//nolint:wrapcheck // context errors are propagated verbatim.
		return ctx.Err()
	}
}

// lead assembles and sends batches until nothing is waiting, then resigns.
//
// Leadership is handed over under the same mutex that enqueues a request, so a
// request can never be left in the queue with nobody to send it: either the
// enqueue happens first and this loop picks it up, or the resignation happens
// first and the enqueuing caller becomes the next leader.
func (c *deleteCollector) lead(ctx context.Context) {
	for {
		c.mu.Lock()

		if len(c.pending) == 0 {
			c.leading = false

			c.mu.Unlock()

			return
		}

		c.mu.Unlock()

		c.waitForBatch(ctx)

		c.mu.Lock()

		batch := c.pending
		c.pending = nil

		drainSignal(c.full)

		c.mu.Unlock()

		c.send(ctx, batch)
	}
}

// waitForBatch blocks until the window expires, the batch fills up, or the
// leader's own context is canceled.
func (c *deleteCollector) waitForBatch(ctx context.Context) {
	t := time.NewTimer(c.window)
	defer t.Stop()

	select {
	case <-t.C:
	case <-c.full:
	case <-ctx.Done():
	}
}

// send splits the collected requests into batch-sized chunks, executes them and
// answers every waiter.
func (c *deleteCollector) send(ctx context.Context, batch []*deleteRequest) {
	for chunk := range slices.Chunk(batch, c.maxSize) {
		if len(chunk) == 1 {
			// A batch envelope around a single call is strictly more work than
			// the call.
			r := chunk[0]
			r.err = c.s.deleteFileByIDNow(ctx, r.blobID, r.fileID)

			continue
		}

		c.s.runDeleteBatch(ctx, chunk)
	}

	for _, r := range batch {
		// The batch traveled under the assembling caller's context. If that
		// caller walked away but this one did not, the delete still has to
		// happen, so it is re-issued on its own.
		if isContextError(r.err) && r.ctx.Err() == nil {
			//nolint:contextcheck // r.ctx IS the caller's context; the point of this branch is that it is not the one the batch used.
			r.err = c.s.deleteFileByIDNow(r.ctx, r.blobID, r.fileID)
		}

		r.done <- r.err
	}
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// runDeleteBatch executes one batch, retrying through the pacer for as long as
// any part remains unresolved.
//
// Per-part error semantics, which are the whole subtlety of batching:
//
//   - The envelope and the parts fail independently. A failed envelope means
//     nothing was executed, so the entire batch is retried.
//   - Every part carries its own status and is classified on its own through
//     gdriveerr, so a 404 on one file and a success on the next are reported to
//     their own callers and to nobody else.
//   - A part whose classification is retryable - notably a 403 rateLimitExceeded
//     or userRateLimitExceeded, which Drive can return for individual parts of an
//     otherwise successful batch - is NOT resolved. One such error is returned to
//     the pacer, which backs the WHOLE BACKEND off (that is what a rate limit
//     means) and retries; the retry re-sends only the parts that are still
//     unresolved, so a partially-successful batch is never re-executed.
//   - When the pacer finally gives up, whatever is still unresolved fails with
//     the classified error it last produced.
func (s *gdriveStorage) runDeleteBatch(ctx context.Context, reqs []*deleteRequest) {
	release, err := s.acquireDeleteSlot(ctx)
	if err != nil {
		setUnresolved(reqs, err)

		return
	}

	defer release()

	if err := s.pacer.Call(ctx, methodBatch, "", func() error {
		return s.executeDeleteBatch(ctx, reqs)
	}); err != nil {
		setUnresolved(reqs, err)
	}
}

// executeDeleteBatch posts one batch request holding every unresolved delete and
// records the outcome of each part. It returns nil once every part has an
// outcome, and otherwise an error suitable for the pacer to act on.
func (s *gdriveStorage) executeDeleteBatch(ctx context.Context, reqs []*deleteRequest) error {
	todo := unresolvedRequests(reqs)
	if len(todo) == 0 {
		return nil
	}

	body, contentType, err := buildDeleteBatchBody(s.service.BasePath, todo)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.batchURL, bytes.NewReader(body))
	if err != nil {
		return errors.Wrap(err, "unable to build the Google Drive batch request")
	}

	req.Header.Set("Content-Type", contentType)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return err
	}

	defer resp.Body.Close() //nolint:errcheck

	// A non-2xx on the envelope itself means no part ran.
	if err := googleapi.CheckResponse(resp); err != nil {
		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return err
	}

	return s.applyDeleteBatchResponse(ctx, resp, todo)
}

// applyDeleteBatchResponse decodes the multipart response and records each
// part's outcome against its request.
func (s *gdriveStorage) applyDeleteBatchResponse(ctx context.Context, resp *http.Response, todo []*deleteRequest) error {
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return errors.Errorf("Google Drive answered a batch request with %q instead of a multipart body", resp.Header.Get("Content-Type"))
	}

	mr := multipart.NewReader(resp.Body, params["boundary"])

	for i := 0; ; i++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return errors.Wrap(err, "unable to read the Google Drive batch response")
		}

		target := matchBatchPart(todo, part.Header.Get("Content-ID"), i)

		perr := readBatchPartResult(part, target)

		part.Close() //nolint:errcheck

		if target == nil {
			continue
		}

		if perr == nil {
			target.resolved, target.err = true, nil

			continue
		}

		if d, _ := gdriveerr.DispositionOf(perr); d.Retryable() {
			// Left unresolved on purpose: the pacer decides whether to send it
			// again, and only the parts still in this state are re-sent.
			target.err = perr

			continue
		}

		target.resolved, target.err = true, perr
	}

	// Everything still unresolved goes back to the pacer as one representative
	// error. Drive answers every part it was given, so a request with no part at
	// all means a truncated body, which must not be mistaken for success.
	var retryable error

	for _, r := range todo {
		if r.resolved {
			continue
		}

		if r.err == nil {
			r.err = errors.Errorf("Google Drive returned no batch response for blob %v", r.blobID)
		}

		retryable = r.err
	}

	if retryable != nil {
		log(ctx).Debugf("retrying %v of %v Google Drive batch deletes: %v", countUnresolved(todo), len(todo), retryable)
	}

	return retryable
}

// readBatchPartResult parses one application/http part as an HTTP response and
// returns its error, if any, classified through gdriveerr.
func readBatchPartResult(part *multipart.Part, target *deleteRequest) error {
	pr, err := http.ReadResponse(bufio.NewReader(part), nil)
	if err != nil {
		return errors.Wrap(err, "unable to parse a Google Drive batch response part")
	}

	defer pr.Body.Close() //nolint:errcheck

	var blobID blob.ID
	if target != nil {
		blobID = target.blobID
	}

	// CheckResponse builds the same *googleapi.Error the generated client would
	// have produced for this status and body, so a part is classified by exactly
	// the same table as a standalone call.
	//nolint:wrapcheck // Classify returns a *gdriveerr.DriveError that already names the operation, the blob and the Drive reason.
	return gdriveerr.Classify(googleapi.CheckResponse(pr), methodFilesDelete, string(blobID))
}

// matchBatchPart finds the request a response part answers. Drive echoes the
// request's Content-ID prefixed with "response-", and documents that parts come
// back in request order; the header is authoritative and the position is the
// fallback.
func matchBatchPart(todo []*deleteRequest, contentID string, position int) *deleteRequest {
	id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(contentID), "<"), ">")
	id = strings.TrimPrefix(id, batchResponsePrefix)

	if n, err := strconv.Atoi(strings.TrimPrefix(id, batchContentIDPrefix)); err == nil && n >= 0 && n < len(todo) {
		return todo[n]
	}

	if position < len(todo) {
		return todo[position]
	}

	return nil
}

// buildDeleteBatchBody renders the multipart/mixed batch body. Inner requests
// must carry path-only URLs ("full URLs are not allowed in batch requests"),
// which is why the service's base path is needed here.
func buildDeleteBatchBody(basePath string, reqs []*deleteRequest) (body []byte, contentType string, err error) {
	base, err := url.Parse(basePath)
	if err != nil {
		return nil, "", errors.Wrapf(err, "unable to parse the Google Drive base path %q", basePath)
	}

	var buf bytes.Buffer

	w := multipart.NewWriter(&buf)

	for i, r := range reqs {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", batchPartContentType)
		h.Set("Content-ID", fmt.Sprintf("<%s%d>", batchContentIDPrefix, i))

		// binary keeps net/http's multipart writer from being tempted to
		// re-encode the embedded request.
		h.Set("Content-Transfer-Encoding", "binary")

		p, err := w.CreatePart(h)
		if err != nil {
			return nil, "", errors.Wrap(err, "unable to build the Google Drive batch body")
		}

		target := *base
		target.Path = base.Path + "files/" + r.fileID
		target.RawQuery = url.Values{"supportsAllDrives": {"true"}, "fields": {""}}.Encode()

		// A bare request line plus a blank line: no body, and no headers of its
		// own - the envelope's Authorization applies to every part.
		if _, err := fmt.Fprintf(p, "DELETE %s HTTP/1.1\r\n\r\n", target.RequestURI()); err != nil {
			return nil, "", errors.Wrap(err, "unable to build the Google Drive batch body")
		}
	}

	if err := w.Close(); err != nil {
		return nil, "", errors.Wrap(err, "unable to build the Google Drive batch body")
	}

	return buf.Bytes(), "multipart/mixed; boundary=" + w.Boundary(), nil
}

// batchEndpoint derives the batch URL from the Drive service's base path, which
// is "<root>/drive/v3/" in production and the fake server's address in tests.
func batchEndpoint(basePath string) (string, error) {
	u, err := url.Parse(basePath)
	if err != nil {
		return "", errors.Wrapf(err, "unable to parse the Google Drive base path %q", basePath)
	}

	root, ok := strings.CutSuffix(u.Path, drivePathSuffix)
	if !ok {
		return "", errors.Errorf("unable to derive the Google Drive batch endpoint from %q", basePath)
	}

	u.Path = root + batchAPIPath
	u.RawQuery, u.Fragment = "", ""

	return u.String(), nil
}

func unresolvedRequests(reqs []*deleteRequest) []*deleteRequest {
	res := make([]*deleteRequest, 0, len(reqs))

	for _, r := range reqs {
		if !r.resolved {
			res = append(res, r)
		}
	}

	return res
}

func countUnresolved(reqs []*deleteRequest) int {
	return len(unresolvedRequests(reqs))
}

func setUnresolved(reqs []*deleteRequest, err error) {
	for _, r := range reqs {
		if !r.resolved {
			r.resolved, r.err = true, err
		}
	}
}

// postSignal posts to a capacity-one channel without blocking.
func postSignal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// drainSignal empties a capacity-one channel without blocking.
func drainSignal(ch chan struct{}) {
	select {
	case <-ch:
	default:
	}
}

func (s *gdriveStorage) ListBlobs(ctx context.Context, prefix blob.ID, callback func(blob.Metadata) error) error {
	q := fmt.Sprintf("%s and mimeType = '%s' and trashed = false", s.parentTerm(), blobMimeType)

	if len(prefix) >= minPrefixNarrowingLength {
		q += fmt.Sprintf(" and name contains '%s'", escapeQueryLiteral(string(prefix)))
	}

	var (
		pageToken string
		// previous is the name of the last file seen, in any page. Because the
		// query is ordered by name, a repeat of it is a duplicate and the copy
		// already emitted was the newest one.
		previous    blob.ID
		hasPrevious bool
	)

	for {
		var page *drive.FileList

		err := s.pacer.Call(ctx, methodFilesList, string(prefix), func() error {
			var err error

			page, err = s.files.List().
				SupportsAllDrives(true).
				IncludeItemsFromAllDrives(true).
				Q(q).
				OrderBy(listOrderBy).
				PageSize(s.tuning.listPageSize).
				Fields(listFields).
				PageToken(pageToken).
				Context(ctx).
				Do()

			//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
			return err
		})
		if err != nil {
			return errors.Wrapf(err, "List in ListBlobs(%s)", prefix)
		}

		for _, f := range page.Files {
			blobID := toBlobID(f.Name)

			if hasPrevious && blobID == previous {
				log(ctx).Warnf("Google Drive folder %v holds more than one file named %v; ignoring the older copy %v",
					s.folderID, blobID, f.Id)

				continue
			}

			previous, hasPrevious = blobID, true

			// A non-matching name skips this entry and CONTINUES. Returning here
			// would drop the rest of the page, which is how the previous
			// implementation silently lost blobs.
			if !strings.HasPrefix(string(blobID), string(prefix)) {
				continue
			}

			s.idCache.Put(blobID, f.Id)

			bm, err := parseFileMetadata(f, blobID)
			if err != nil {
				return err
			}

			// The callback's error is the caller's, and is returned verbatim so
			// that errors.Is finds it.
			if err := callback(bm); err != nil {
				return err
			}
		}

		pageToken = page.NextPageToken
		if pageToken == "" {
			return nil
		}
	}
}

func (s *gdriveStorage) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{
		Type:   gdriveStorageType,
		Config: &s.Options,
	}
}

func (s *gdriveStorage) DisplayName() string {
	return fmt.Sprintf("Google Drive: %v", s.folderID)
}

func (s *gdriveStorage) IsReadOnly() bool {
	return s.ReadOnly
}

func (s *gdriveStorage) FlushCaches(_ context.Context) error {
	s.idCache.Flush()

	return nil
}

// Close releases the file ID cache reference and the idle HTTP connections held
// by this storage. It is idempotent: providervalidation closes its extra
// connections and the caller closes the original, and the cache's reference
// count must not be decremented twice for one New.
func (s *gdriveStorage) Close(_ context.Context) error {
	var err error

	s.closeOnce.Do(func() {
		err = s.idCache.Close()

		if s.httpClient != nil {
			s.httpClient.CloseIdleConnections()
		}
	})

	return errors.Wrap(err, "unable to close the Google Drive file ID cache")
}

// StatsSnapshot returns a point-in-time copy of this connection's Drive API
// counters. The benchmark harness diffs two snapshots to attribute a workload.
func (s *gdriveStorage) StatsSnapshot() TransportStatsSnapshot {
	return s.stats.Snapshot()
}

// StatsFromStorage returns the Drive API counters of a storage created by New,
// including one hidden behind wrappers that expose their delegate through an
// Unwrap method. It reports false for any other storage.
func StatsFromStorage(st blob.Storage) (TransportStatsSnapshot, bool) {
	type snapshotter interface {
		StatsSnapshot() TransportStatsSnapshot
	}

	type unwrapper interface {
		Unwrap() blob.Storage
	}

	for st != nil {
		if s, ok := st.(snapshotter); ok {
			return s.StatsSnapshot(), true
		}

		u, ok := st.(unwrapper)
		if !ok {
			break
		}

		st = u.Unwrap()
	}

	return TransportStatsSnapshot{}, false
}

//
// ------------------------------------------------------------ name resolution
//

// withFileID runs fn against the file backing a blob, re-resolving the name if
// a cached file ID turns out to be stale. The cache is only ever a shortcut: a
// 404 on a cached ID invalidates the entry and falls back to a name query
// before the blob may be declared missing.
func (s *gdriveStorage) withFileID(ctx context.Context, op string, blobID blob.ID, fn func(fileID string) error) error {
	if fileID, ok := s.idCache.Get(blobID); ok {
		err := fn(fileID)
		if err == nil || !errors.Is(err, blob.ErrBlobNotFound) {
			return err
		}

		s.idCache.Delete(blobID)
	}

	ref, err := s.resolveBlobFile(ctx, op, blobID, nameLookupFields)
	if err != nil {
		return err
	}

	if ref.file != nil {
		s.idCache.Put(blobID, ref.file.Id)
	}

	return fn(ref.id())
}

// blobFileRef is what the miss protocol resolves a blob name to: either the
// file the name index returned, carrying the requested fields, or nothing but
// the file ID, recovered from the local cache journal.
type blobFileRef struct {
	file   *drive.File
	fileID string
}

func (r blobFileRef) id() string {
	if r.file != nil {
		return r.file.Id
	}

	return r.fileID
}

// resolveBlobFile resolves a blob name to its Drive file, applying the miss
// protocol documented at nameQueryRetryBudget: one query, then the local
// journal, then - only if the journal shows somebody has been writing - an
// escalating re-query, and otherwise a single confirming query.
func (s *gdriveStorage) resolveBlobFile(ctx context.Context, op string, blobID blob.ID, fields string) (blobFileRef, error) {
	f, err := s.lookupFileByName(ctx, op, blobID, fields)
	if err == nil {
		return blobFileRef{file: f}, nil
	}

	if !errors.Is(err, blob.ErrBlobNotFound) {
		return blobFileRef{}, err
	}

	fileID, found, appended := s.idCache.RescanTail(blobID)
	if found {
		// Another process on this machine wrote it and recorded the file ID.
		// files.get on that ID is authoritative and needs no name index at all.
		log(ctx).Debugf("resolved blob %v from the Google Drive file ID journal after a name-index miss", blobID)

		return blobFileRef{fileID: fileID}, nil
	}

	if !appended {
		// Nothing has been written to this repository since we last looked, so
		// there is no lagging index to wait for. One confirming query, then the
		// answer the caller almost certainly expects.
		return s.confirmMissing(ctx, op, blobID, fields, err)
	}

	return s.escalateMissing(ctx, op, blobID, fields, err)
}

// confirmMissing re-runs the name query once, after a short pause, and reports
// the original not-found error if it misses again.
func (s *gdriveStorage) confirmMissing(ctx context.Context, op string, blobID blob.ID, fields string, missErr error) (blobFileRef, error) {
	if !clock.SleepInterruptibly(ctx, nameQueryRetryInitialDelay) {
		//nolint:wrapcheck // context errors are propagated verbatim.
		return blobFileRef{}, ctx.Err()
	}

	f, err := s.lookupFileByName(ctx, op, blobID, fields)
	if err == nil {
		return blobFileRef{file: f}, nil
	}

	if !errors.Is(err, blob.ErrBlobNotFound) {
		return blobFileRef{}, err
	}

	return blobFileRef{}, missErr
}

// escalateMissing re-queries the name index with an escalating delay for up to
// nameQueryRetryBudget, re-reading the cache journal on every round because a
// concurrent local writer may record the file ID before Drive's index catches
// up.
func (s *gdriveStorage) escalateMissing(ctx context.Context, op string, blobID blob.ID, fields string, missErr error) (blobFileRef, error) {
	var (
		delay    = nameQueryRetryInitialDelay
		deadline = clock.Now().Add(nameQueryRetryBudget)
	)

	for {
		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 {
			return blobFileRef{}, missErr
		}

		if !clock.SleepInterruptibly(ctx, min(delay, remaining)) {
			//nolint:wrapcheck // context errors are propagated verbatim.
			return blobFileRef{}, ctx.Err()
		}

		if fileID, found, _ := s.idCache.RescanTail(blobID); found {
			return blobFileRef{fileID: fileID}, nil
		}

		f, err := s.lookupFileByName(ctx, op, blobID, fields)
		if err == nil {
			return blobFileRef{file: f}, nil
		}

		if !errors.Is(err, blob.ErrBlobNotFound) {
			return blobFileRef{}, err
		}

		missErr = err

		if delay *= 2; delay > nameQueryRetryMaxDelay {
			delay = nameQueryRetryMaxDelay
		}
	}
}

// lookupFileByName resolves a blob name to exactly one file, returning
// blob.ErrBlobNotFound when there is none.
func (s *gdriveStorage) lookupFileByName(ctx context.Context, op string, blobID blob.ID, fields string) (*drive.File, error) {
	files, err := s.listByName(ctx, op, blobID, fields, nameProbePageSize)
	if err != nil {
		return nil, err
	}

	if len(files) > 1 {
		log(ctx).Warnf("Google Drive folder %v holds more than one file named %v (%v and %v); using the most recently created one",
			s.folderID, blobID, files[0].Id, files[1].Id)
	}

	return newestFile(files), nil
}

// listNewestByName is lookupFileByName without the pacer, for use from inside a
// pacer callback (the pacer must not be re-entered from its own fn).
func (s *gdriveStorage) listNewestByName(ctx context.Context, blobID blob.ID, fields string) (*drive.File, error) {
	res, err := s.rawListByName(ctx, blobID, "files("+fields+",createdTime)", nameProbePageSize)
	if err != nil {
		return nil, err
	}

	f := newestFile(res.Files)
	if f == nil {
		return nil, blob.ErrBlobNotFound
	}

	return f, nil
}

// pacedListByName runs the name query through the pacer. An empty result set is
// not an error here; only listByName turns it into blob.ErrBlobNotFound, so
// that callers which care about the difference between "the folder is gone" and
// "the blob is not there" can tell them apart.
func (s *gdriveStorage) pacedListByName(ctx context.Context, op string, blobID blob.ID, fields string, pageSize int64) (*drive.FileList, error) {
	var res *drive.FileList

	err := s.pacer.Call(ctx, op, string(blobID), func() error {
		var err error

		res, err = s.rawListByName(ctx, blobID, fields, pageSize)

		return err
	})

	//nolint:wrapcheck // the pacer returns a *gdriveerr.DriveError that already names the operation, the blob and the Drive reason.
	return res, err
}

// listByName runs the name query through the pacer and fails with
// blob.ErrBlobNotFound when nothing matches.
func (s *gdriveStorage) listByName(ctx context.Context, op string, blobID blob.ID, fields string, pageSize int64) ([]*drive.File, error) {
	res, err := s.pacedListByName(ctx, op, blobID, fields, pageSize)
	if err != nil {
		return nil, err
	}

	if len(res.Files) == 0 {
		return nil, errors.WithMessagef(blob.ErrBlobNotFound, "no file named %q in the Google Drive folder", blobID)
	}

	return res.Files, nil
}

func (s *gdriveStorage) rawListByName(ctx context.Context, blobID blob.ID, fields string, pageSize int64) (*drive.FileList, error) {
	name := toFileName(blobID)

	q := fmt.Sprintf("%s and name = '%s' and mimeType = '%s' and trashed = false",
		s.parentTerm(), escapeQueryLiteral(name), blobMimeType)

	res, err := s.files.List().
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		Q(q).
		PageSize(pageSize).
		Fields(googleapi.Field(fields)).
		Context(ctx).
		Do()
	if err != nil {
		//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
		return nil, err
	}

	// Drive's `=` operator is CASE-INSENSITIVE (rclone re-checks the same way,
	// RECON.md B.5.3), so a query for "abc" can return a file called "ABC".
	// Kopia's blob IDs are case-sensitive, and a repository holding both would
	// otherwise see one blob's bytes served for the other.
	res.Files = slices.DeleteFunc(res.Files, func(f *drive.File) bool {
		return f.Name != name
	})

	return res, nil
}

func (s *gdriveStorage) parentTerm() string {
	return fmt.Sprintf("'%s' in parents", escapeQueryLiteral(s.folderID))
}

// escapeQueryLiteral escapes a value for inclusion in a single-quoted Drive
// query literal. Kopia's blob IDs never contain a quote or a backslash, but the
// folder ID comes from configuration and the escaping keeps a malformed one from
// turning into a different query.
func escapeQueryLiteral(v string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v)
}

//
// ------------------------------------------------------------------- uploads
//

// mediaOptions selects the upload protocol. Below the cutoff a blob goes up in
// one multipart request; at or above it, googleapi's resumable machinery splits
// it into chunks, so a failure part-way through costs one chunk instead of the
// whole blob. Chunking is disabled outright (ChunkSize(0)) for the simple case
// so that the payload is streamed rather than buffered in memory.
//
// Both paths are safe to retry because every attempt obtains a fresh
// blob.Bytes.Reader(); nothing here relies on the HTTP layer being able to
// replay a body. Canceling the context aborts the transfer at the next chunk
// boundary: googleapi's chunk loop selects on ctx.Done() and returns ctx.Err(),
// which gdriveerr passes through untouched, and the abandoned resumable session
// expires on Drive's side (one week) without holding any quota.
//
// A chunk that fails with a network error (including a stall timeout) is
// re-sent by googleapi on the SAME session at the same offset, so only that
// chunk is repeated - but only while the chunk's retry deadline has not passed,
// and googleapi measures that deadline from the chunk's first attempt. Its
// default (32s) is shorter than every stall timeout, which would turn each
// stall into a pacer-level retry that starts a brand-new session and re-sends
// the whole blob; see transportTimeouts.chunkRetryDeadline.
func (s *gdriveStorage) mediaOptions(length int) []googleapi.MediaOption {
	chunkSize := 0
	if int64(length) >= s.tuning.simpleUploadCutoff {
		chunkSize = s.tuning.uploadChunkSize
	}

	return []googleapi.MediaOption{
		googleapi.ChunkSize(chunkSize),
		googleapi.ContentType(uploadContentType),
		googleapi.ChunkRetryDeadline(s.tuning.timeouts.chunkRetryDeadline()),
	}
}

func (s *gdriveStorage) uploadOp(length int) string {
	if int64(length) >= s.tuning.simpleUploadCutoff {
		return methodUploadResumable
	}

	return methodUploadSimple
}

//
// -------------------------------------------------------------------- helpers
//

func toFileName(blobID blob.ID) string {
	return string(blobID)
}

func toBlobID(fileName string) blob.ID {
	return blob.ID(fileName)
}

// toRange renders the Range header for a read. An empty result means "no Range
// header", i.e. the whole file.
func toRange(offset, length int64) string {
	switch {
	case length < 0 && offset == 0:
		return ""
	case length < 0:
		return fmt.Sprintf("bytes=%d-", offset)
	default:
		return fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
}

func modifiedTimeOf(opts blob.PutOptions) string {
	if opts.SetModTime.IsZero() {
		return ""
	}

	// Set on the create/update call itself: a second request to stamp the time
	// would be a second chance to fail, and would leave the blob briefly carrying
	// the wrong timestamp.
	return opts.SetModTime.UTC().Format(time.RFC3339Nano)
}

// applyGetModTime reports the modification time Drive actually recorded. A
// timestamp that cannot be parsed is an error, not something to swallow: the
// caller asked for the value and the suite compares it against GetMetadata.
func applyGetModTime(opts blob.PutOptions, f *drive.File) error {
	if opts.GetModTime == nil {
		return nil
	}

	t, err := parseModifiedTime(f)
	if err != nil {
		return err
	}

	*opts.GetModTime = t

	return nil
}

func parseFileMetadata(f *drive.File, blobID blob.ID) (blob.Metadata, error) {
	mtime, err := parseModifiedTime(f)
	if err != nil {
		return blob.Metadata{}, err
	}

	return blob.Metadata{
		BlobID:    blobID,
		Length:    f.Size,
		Timestamp: mtime,
	}, nil
}

func parseModifiedTime(f *drive.File) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, f.ModifiedTime)
	if err != nil {
		return time.Time{}, errors.Wrapf(err, "unable to parse the modification time %q reported by Google Drive", f.ModifiedTime)
	}

	return t, nil
}

// newestFile picks the most recently created of a set of same-named files.
func newestFile(files []*drive.File) *drive.File {
	return pickFile(files, func(candidate, best *drive.File) bool {
		if candidate.CreatedTime != best.CreatedTime {
			return candidate.CreatedTime > best.CreatedTime
		}

		return candidate.Id > best.Id
	})
}

// oldestFile picks the first-created of a set of same-named files, breaking a
// tie with the lexicographically smaller file ID.
func oldestFile(files []*drive.File) *drive.File {
	return pickFile(files, func(candidate, best *drive.File) bool {
		if candidate.CreatedTime != best.CreatedTime {
			return candidate.CreatedTime < best.CreatedTime
		}

		return candidate.Id < best.Id
	})
}

// pickFile returns the element for which better() reports true against every
// other. RFC 3339 timestamps of the same offset sort correctly as strings,
// which is what Drive returns (always UTC, always "Z").
func pickFile(files []*drive.File, better func(candidate, best *drive.File) bool) *drive.File {
	var best *drive.File

	for _, f := range files {
		if best == nil || better(f, best) {
			best = f
		}
	}

	return best
}

//
// --------------------------------------------------------------- per-blob lock
//

// blobLockTable serializes mutations of one blob name across every connection
// in this process. It is keyed by folder and blob so that two repositories, or
// two blobs, never contend. Entries are reference-counted and removed when the
// last holder releases, so the table cannot grow without bound.
//
//nolint:gochecknoglobals
var blobLockTable = struct {
	mu      sync.Mutex
	entries map[string]*blobLock
}{entries: map[string]*blobLock{}}

type blobLock struct {
	mu   sync.Mutex
	refs int
}

// lockBlob acquires the lock for one blob and returns the release function.
func lockBlob(folderID string, blobID blob.ID) func() {
	key := folderID + "\x00" + string(blobID)

	blobLockTable.mu.Lock()

	e := blobLockTable.entries[key]
	if e == nil {
		e = &blobLock{}
		blobLockTable.entries[key] = e
	}

	e.refs++

	blobLockTable.mu.Unlock()

	e.mu.Lock()

	return func() {
		e.mu.Unlock()

		blobLockTable.mu.Lock()
		defer blobLockTable.mu.Unlock()

		e.refs--

		if e.refs == 0 {
			delete(blobLockTable.entries, key)
		}
	}
}

func init() {
	blob.AddSupportedStorage(gdriveStorageType, Options{}, New)
}
