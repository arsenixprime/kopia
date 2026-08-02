//go:build !no_extra_providers

// Package gdrive implements Storage based on Google Drive.
package gdrive

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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

	// bytesPerMB converts the megabyte-denominated tuning knobs.
	bytesPerMB = 1 << 20
)

// Name-resolution retry budget.
//
// A blob is addressed by Drive file NAME, and Drive's name index is eventually
// consistent: a files.list query can miss a file that files.get would happily
// return. The persistent blobID->fileID cache (gdrive_fileids.go) removes this
// problem for anything this machine wrote, because the cached ID is resolved
// with files.get. What remains is a blob written by ANOTHER process or machine,
// where only a name query can find it. For those, a read escalates its query
// instead of declaring the blob missing after a single miss - the previous
// implementation gave up after 3 attempts, 300 ms in total, which is issue
// #4272.
//
// These are deliberately package constants rather than Options.Tuning knobs:
// Contract 5 does not include them, and they trade latency on a genuinely
// missing blob (which Kopia treats as an ordinary, expected outcome) against
// robustness on a lagging index. Six queries spread over five seconds is the
// documented compromise.
const (
	nameQueryRetryInitialDelay = 100 * time.Millisecond
	nameQueryRetryMaxDelay     = 2 * time.Second
	nameQueryRetryBudget       = 5 * time.Second
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

	// listFields is the ListBlobs page mask.
	listFields = "nextPageToken,files(id,name,size,modifiedTime,createdTime)"
)

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
	base     http.RoundTripper
	endpoint string
}

// resolvedTuning is Options.Tuning with all defaults applied and all unit
// conversions done, so that no hot path ever has to think about zero values.
type resolvedTuning struct {
	pacer              gdrivepacer.Config
	uploadChunkSize    int
	simpleUploadCutoff int64
	listPageSize       int64
	cacheDir           string
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

	closeOnce sync.Once
}

var _ blob.Storage = (*gdriveStorage)(nil)

//
// ---------------------------------------------------------------- construction
//

// New creates new Google Drive-backed storage with specified options:
//
// - the 'folderID' field is required and all other parameters are optional.
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
	_ = isCreate

	if opt.FolderID == "" {
		return nil, errors.New("folder-id must be specified")
	}

	tuning := resolveTuning(opt)
	stats := NewTransportStats()

	service, httpClient, err := newDriveService(ctx, opt, stats)
	if err != nil {
		return nil, err
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
		pacer:      gdrivepacer.New(tuning.pacer, stats),
		idCache:    idCache,
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
		cacheDir:           t.CacheDir,
	}

	if res.cacheDir == "" {
		res.cacheDir = defaultCacheDir()
	}

	return res
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
//	token source (gdrive_auth.go)
//	  -> oauth2.Transport            (adds Authorization)
//	    -> instrumentedTransport     (counts calls, bytes, latency, statuses)
//	      -> drive.Service
//
// The instrumentation sits OUTSIDE the OAuth transport on purpose, so that a
// token-refresh round trip is attributed like any other call and the byte
// counters see exactly what went on the wire.
func newDriveService(ctx context.Context, opt *Options, stats *TransportStats) (*drive.Service, *http.Client, error) {
	base, endpoint, err := newBaseTransport(ctx, opt)
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

func newBaseTransport(ctx context.Context, opt *Options) (http.RoundTripper, string, error) {
	if v, ok := testDriveTransports.Load(opt.FolderID); ok {
		t, _ := v.(testDriveTransport)

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
		Base:   http.DefaultTransport,
		Source: oauth2.ReuseTokenSource(nil, ts),
	}, "", nil
}

// CreateDriveService creates a new Google Drive service, which encapsulates
// multiple clients used to access different Google Drive functionality.
// Exported for tests and tooling that need to manage the repository folder
// itself (which is not a blob and therefore not reachable through blob.Storage).
func CreateDriveService(ctx context.Context, opt *Options) (*drive.Service, error) {
	service, _, err := newDriveService(ctx, opt, NewTransportStats())

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
		FreeB: uint64(free),    //nolint:gosec // clamped to >= 0 above.
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

	f, err := s.lookupFileByNameWithRetry(ctx, methodFilesList, blobID, nameLookupMetadataFields)
	if err != nil {
		return blob.Metadata{}, err
	}

	s.idCache.Put(blobID, f.Id)

	return parseFileMetadata(f, blobID)
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
func (s *gdriveStorage) deleteFileByID(ctx context.Context, blobID blob.ID, fileID string) error {
	//nolint:wrapcheck // the pacer returns a *gdriveerr.DriveError that already names the operation, the blob and the Drive reason.
	return s.pacer.Call(ctx, methodFilesDelete, string(blobID), func() error {
		return s.files.Delete(fileID).SupportsAllDrives(true).Context(ctx).Do()
	})
}

func (s *gdriveStorage) ListBlobs(ctx context.Context, prefix blob.ID, callback func(blob.Metadata) error) error {
	q := fmt.Sprintf("%s and mimeType = '%s' and trashed = false", s.parentTerm(), blobMimeType)

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

	f, err := s.lookupFileByNameWithRetry(ctx, op, blobID, nameLookupFields)
	if err != nil {
		return err
	}

	s.idCache.Put(blobID, f.Id)

	return fn(f.Id)
}

// lookupFileByNameWithRetry resolves a blob name, re-querying with an escalating
// delay before concluding that the blob does not exist. See the commentary on
// nameQueryRetryBudget.
func (s *gdriveStorage) lookupFileByNameWithRetry(ctx context.Context, op string, blobID blob.ID, fields string) (*drive.File, error) {
	var (
		delay    = nameQueryRetryInitialDelay
		deadline = clock.Now().Add(nameQueryRetryBudget)
	)

	for {
		f, err := s.lookupFileByName(ctx, op, blobID, fields)
		if err == nil || !errors.Is(err, blob.ErrBlobNotFound) {
			return f, err
		}

		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 {
			return nil, err
		}

		if delay > remaining {
			delay = remaining
		}

		if !clock.SleepInterruptibly(ctx, delay) {
			//nolint:wrapcheck // context errors are propagated verbatim.
			return nil, ctx.Err()
		}

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
	q := fmt.Sprintf("%s and name = '%s' and mimeType = '%s' and trashed = false",
		s.parentTerm(), escapeQueryLiteral(toFileName(blobID)), blobMimeType)

	//nolint:wrapcheck // the pacer classifies this error through gdriveerr.
	return s.files.List().
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		Q(q).
		PageSize(pageSize).
		Fields(googleapi.Field(fields)).
		Context(ctx).
		Do()
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
func (s *gdriveStorage) mediaOptions(length int) []googleapi.MediaOption {
	chunkSize := 0
	if int64(length) >= s.tuning.simpleUploadCutoff {
		chunkSize = s.tuning.uploadChunkSize
	}

	return []googleapi.MediaOption{
		googleapi.ChunkSize(chunkSize),
		googleapi.ContentType(uploadContentType),
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
