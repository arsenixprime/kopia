//go:build !no_extra_providers

package gdrive

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/logging"
)

// persistentIDCache: on-disk format and concurrency story.
//
// FORMAT. One append-only journal file per (cache dir, Drive folder) pair:
//
//	<dir>/gdrive-fileids/<sha256(folderKey)[:16]>/fileids.journal
//	<dir>/gdrive-fileids/<sha256(folderKey)[:16]>/fileids.lock
//	<dir>/gdrive-fileids/<sha256(folderKey)[:16]>/namespace.json  (debugging aid)
//
// The journal starts with a fixed header line and continues with self-framing
// records:
//
//	magic(8) | payloadLen uint32le(4) | crc32c(4) | payload
//	where the payload holds blobIDLen uint16le(2), blobID, fileID
//
// An empty fileID is a tombstone. Replaying the journal in order yields the
// map; last record wins. The per-record CRC (Castagnoli, over the length field
// and the payload) plus the 8-byte sync magic make the reader self-resyncing:
// a torn tail, a flipped byte or injected garbage costs at most the records it
// physically damaged - the reader scans forward to the next valid magic+CRC and
// keeps going. CRC32 rather than internal/cacheprot's HMAC on purpose: this is
// a local latency cache whose contents are re-derivable from Drive and which is
// never authoritative (a cached ID that 404s must be re-resolved by name), so
// we need damage detection, not tamper detection - and cacheprot would also
// force a key and a per-entry blob layout on us. internal/cache.NewStorageOrNil
// (one sharded file per blob ID) was the alternative; it was rejected because a
// repository has millions of blobs, and one 40-byte file per blob costs an
// inode and 2 syscalls per lookup where the journal costs one sequential read
// at open and one batched append per write.
//
// CONCURRENCY. Three layers, and no lock is ever held across disk I/O on the
// Get/Put path (the bug this replaces held a per-blob mutex across the upload):
//
//  1. In-process, in-memory: mu (RWMutex) guards the map only. Get is a pure
//     RLock read. Put/Delete update the map, then hand an encoded record to a
//     pending buffer (pendingMu) that a background goroutine drains. Lock order
//     is always writeMu > mu > pendingMu; nothing nests mu inside pendingMu.
//  2. In-process, across gdriveStorage instances: instances are shared through
//     a refcounted package-level registry keyed by (dir, folderKey), so all
//     connections of one process - e.g. the five that
//     "kopia repository validate-provider" opens, issue #4272 - see each
//     other's writes immediately, with no disk round trip at all.
//  3. Across processes: every disk write (batch append, compaction, Flush)
//     runs under an exclusive advisory lock on the separate fileids.lock file
//     (github.com/gofrs/flock, already a kopia dependency, and portable to
//     Windows unlike syscall.Flock). Batches are written with a single write(2)
//     on an O_APPEND descriptor, so appenders never interleave or overwrite.
//     Compaction (triggered at open and after a flush when the journal holds
//     more than compactWasteFactor times the live entries) re-reads the journal
//     from disk under that lock - picking up records other processes appended
//     that this process never saw - and rewrites it as temp file + fsync +
//     atomic rename. Because the lock lives in its own never-renamed file, and
//     because every appender re-stats its descriptor (os.SameFile) after taking
//     the lock and reopens if the journal was replaced underneath it, a
//     compaction cannot lose an acknowledged entry belonging to another process.
//
// DURABILITY. Appends are batched and are not fsynced: a killed process loses
// at most the last unflushed batch, and an OS crash a little more. That is
// deliberate - the cache is an optimization and a missing entry only costs one
// files.list query - but a partially written record can never damage the
// records before it. Compaction does fsync before the rename, so the one
// operation that could destroy old entries is crash-atomic.
type persistentIDCache struct {
	dir       string // namespace directory, already scoped by folderKey
	folderKey string

	journalPath string
	lockPath    string

	logger logging.Logger

	// registry bookkeeping, guarded by idCacheRegistryMu.
	regKey string
	refs   int

	mu sync.RWMutex
	// +checklocks:mu
	entries map[blob.ID]string

	pendingMu sync.Mutex
	// +checklocks:pendingMu
	pending []byte
	// +checklocks:pendingMu
	pendingCount int

	writeMu sync.Mutex
	// +checklocks:writeMu
	f *os.File
	// +checklocks:writeMu
	lock *flock.Flock
	// +checklocks:writeMu
	diskRecords int
	// +checklocks:writeMu
	persistDisabled bool
	// scanOffset is the byte offset just past the last record this process has
	// replayed, and scanInfo identifies the file it was measured against. Together
	// they make RescanTail an incremental read instead of a full replay.
	// +checklocks:writeMu
	scanOffset int64
	// +checklocks:writeMu
	scanInfo os.FileInfo

	batchSize     int           // test hook
	flushInterval time.Duration // test hook
	maxEntries    int           // test hook

	capWarnOnce   sync.Once
	writeWarnOnce sync.Once

	flushSignal chan struct{}
	closeCh     chan struct{}
	closeOnce   sync.Once
	wg          sync.WaitGroup
}

const (
	fileIDCacheSubdir      = "gdrive-fileids"
	fileIDJournalName      = "fileids.journal"
	fileIDLockName         = "fileids.lock"
	fileIDNamespaceName    = "namespace.json"
	fileIDQuarantineSuffix = ".quarantined-"

	// journalHeader identifies the file and its layout version. A file that does
	// not start with it is not ours (or is damaged beyond the record level) and
	// gets quarantined rather than parsed.
	journalHeader = "kopia-gdrive-fileids-v1\n"

	recordMagicLen   = 8
	recordLenSize    = 4
	recordCRCSize    = 4
	recordHeaderLen  = recordMagicLen + recordLenSize + recordCRCSize
	blobIDLenSize    = 2
	maxRecordPayload = 64 << 10

	// defaultFileIDBatchSize records accumulate before the writer is woken up.
	defaultFileIDBatchSize = 256
	// defaultFileIDFlushInterval bounds how long a record can sit unwritten.
	defaultFileIDFlushInterval = 1 * time.Second

	// defaultMaxFileIDEntries caps how many mappings are persisted. At roughly
	// 100 bytes of Go heap per entry (map bucket + two string headers + ~40
	// bytes of key/value text) that is ~100 MB of process memory and a journal
	// of a similar size, for a repository of a million blobs. Beyond the cap the
	// in-memory map keeps working but nothing new is written to disk; there is
	// deliberately NO LRU eviction, because for a backup repository the working
	// set is the whole repository and an LRU would simply thrash.
	defaultMaxFileIDEntries = 1_000_000

	// compaction runs when the journal holds more than compactWasteFactor times
	// the number of live entries, and at least compactMinRecords records.
	compactWasteFactor = 2
	compactMinRecords  = 1024

	journalReadChunk = 64 << 10

	cacheDirMode  = 0o700
	cacheFileMode = 0o600
)

var (
	recordMagic = [recordMagicLen]byte{'k', 'p', 'f', 'i', 'd', 0x00, 0x1a, 0x5a}
	crcTable    = crc32.MakeTable(crc32.Castagnoli)

	errEmptyCacheDir   = errors.New("file ID cache directory must not be empty")
	errEmptyFolderKey  = errors.New("file ID cache folder key must not be empty")
	errBadJournalMagic = errors.New("not a kopia Google Drive file ID journal")
)

// idCacheRegistry shares one persistentIDCache between every gdriveStorage
// instance in this process that talks to the same folder, which is what gives
// separate connections read-your-writes without a disk round trip.
var (
	idCacheRegistryMu sync.Mutex
	idCacheRegistry   = map[string]*persistentIDCache{}
)

// newPersistentIDCache opens (or creates) the persistent blobID->fileID map for
// one Drive folder. dir is the final cache directory - the caller resolves the
// default against Options.Tuning.CacheDir - and folderKey, normally the Drive
// folder ID, scopes the on-disk namespace so that two repositories on one
// machine never see each other's entries.
//
// Repeated calls with the same (dir, folderKey) return the same instance with
// an incremented reference count; the last Close releases it. The context is
// used for logging only; this component performs no network I/O.
//
// A damaged, unreadable or unwritable cache never fails the call: it is
// reported, quarantined where possible, and the cache degrades to memory-only.
func newPersistentIDCache(ctx context.Context, dir, folderKey string) (*persistentIDCache, error) {
	if dir == "" {
		return nil, errEmptyCacheDir
	}

	if folderKey == "" {
		return nil, errEmptyFolderKey
	}

	dir = filepath.Clean(dir)
	regKey := dir + "\x00" + folderKey

	idCacheRegistryMu.Lock()
	defer idCacheRegistryMu.Unlock()

	if c := idCacheRegistry[regKey]; c != nil {
		c.refs++

		return c, nil
	}

	c := &persistentIDCache{
		dir:           fileIDNamespaceDir(dir, folderKey),
		folderKey:     folderKey,
		logger:        log(ctx),
		regKey:        regKey,
		refs:          1,
		entries:       map[blob.ID]string{},
		batchSize:     defaultFileIDBatchSize,
		flushInterval: defaultFileIDFlushInterval,
		maxEntries:    defaultMaxFileIDEntries,
		flushSignal:   make(chan struct{}, 1),
		closeCh:       make(chan struct{}),
	}

	c.journalPath = filepath.Join(c.dir, fileIDJournalName)
	c.lockPath = filepath.Join(c.dir, fileIDLockName)

	c.open()

	c.wg.Add(1)

	go c.flushLoop()

	idCacheRegistry[regKey] = c

	return c, nil
}

// fileIDNamespaceDir derives the per-folder directory. The folder key is hashed
// because Drive IDs are case-sensitive and may contain characters (notably '-'
// and '_') that survive on-disk fine but would make the mapping ambiguous on
// case-insensitive filesystems.
func fileIDNamespaceDir(dir, folderKey string) string {
	h := sha256.Sum256([]byte(folderKey))

	return filepath.Join(dir, fileIDCacheSubdir, hex.EncodeToString(h[:8]))
}

// Get returns the cached file ID for a blob. A tombstoned or unknown blob
// reports ok == false; the caller must then resolve the ID from Drive.
func (c *persistentIDCache) Get(blobID blob.ID) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	fileID, ok := c.entries[blobID]

	return fileID, ok
}

// Put records the file ID of a blob. It is write-through: the mapping is
// visible to every connection in this process immediately and reaches disk with
// the next batch. An empty fileID is ignored - use Delete to record a removal.
func (c *persistentIDCache) Put(blobID blob.ID, fileID string) {
	if blobID == "" || fileID == "" {
		return
	}

	c.mu.Lock()
	c.entries[blobID] = fileID
	n := len(c.entries)
	c.mu.Unlock()

	if n > c.maxEntries {
		c.capWarnOnce.Do(func() {
			c.logger.Warnf("Google Drive file ID cache for folder %v exceeded %v entries; new entries are kept in memory but no longer persisted", c.folderKey, c.maxEntries)
		})

		return
	}

	c.enqueue(blobID, fileID)
}

// Delete forgets the file ID of a blob and records a tombstone, so that
// replaying an older journal cannot resurrect the mapping. Tombstones are
// always persisted, including past the entry cap: they only ever shrink the
// state a reload reconstructs.
func (c *persistentIDCache) Delete(blobID blob.ID) {
	if blobID == "" {
		return
	}

	c.mu.Lock()
	delete(c.entries, blobID)
	c.mu.Unlock()

	c.enqueue(blobID, "")
}

// Flush drops all state, in memory and on disk, for every connection sharing
// this cache. It backs blob.Storage.FlushCaches, so it must leave nothing that
// a reopen could observe.
func (c *persistentIDCache) Flush() {
	c.mu.Lock()
	c.entries = map[blob.ID]string{}
	c.mu.Unlock()

	c.pendingMu.Lock()
	c.pending = nil
	c.pendingCount = 0
	c.pendingMu.Unlock()

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.persistDisabled {
		return
	}

	if err := c.resetLocked(); err != nil {
		c.logger.Warnf("unable to reset Google Drive file ID cache: %v", err)
	}
}

// +checklocks:c.writeMu
func (c *persistentIDCache) resetLocked() error {
	if err := c.acquireFileLock(); err != nil {
		return err
	}

	defer c.releaseFileLock()

	if err := c.ensureOpenLocked(); err != nil {
		return err
	}

	if err := c.f.Truncate(int64(len(journalHeader))); err != nil {
		return errors.Wrap(err, "unable to truncate file ID journal")
	}

	c.diskRecords = 0

	c.noteScannedLocked(int64(len(journalHeader)))

	return nil
}

// Close releases one reference. The last reference flushes pending records and
// closes the journal; calls beyond that are no-ops.
func (c *persistentIDCache) Close() error {
	idCacheRegistryMu.Lock()
	defer idCacheRegistryMu.Unlock()

	if c.refs <= 0 {
		return nil
	}

	c.refs--

	if c.refs > 0 {
		return nil
	}

	delete(idCacheRegistry, c.regKey)

	return c.shutdown()
}

func (c *persistentIDCache) shutdown() error {
	c.closeOnce.Do(func() {
		close(c.closeCh)
	})

	c.wg.Wait()
	c.flushNow()

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	var err error

	if c.f != nil {
		err = errors.Wrap(c.f.Close(), "unable to close file ID journal")
		c.f = nil
	}

	if c.lock != nil {
		c.lock.Close() //nolint:errcheck
		c.lock = nil
	}

	return err
}

// enqueue appends an encoded record to the pending batch and wakes the writer
// once the batch is large enough. It never touches the disk.
func (c *persistentIDCache) enqueue(blobID blob.ID, fileID string) {
	rec, ok := encodeRecord(blobID, fileID)
	if !ok {
		c.logger.Warnf("not caching oversized Google Drive file ID mapping for blob %v", blobID)
		return
	}

	c.pendingMu.Lock()
	c.pending = append(c.pending, rec...)
	c.pendingCount++
	full := c.pendingCount >= c.batchSize
	c.pendingMu.Unlock()

	if full {
		select {
		case c.flushSignal <- struct{}{}:
		default:
		}
	}
}

func (c *persistentIDCache) flushLoop() {
	defer c.wg.Done()

	t := time.NewTicker(c.flushInterval)
	defer t.Stop()

	for {
		select {
		case <-c.closeCh:
			return
		case <-c.flushSignal:
		case <-t.C:
		}

		c.flushNow()
	}
}

// flushNow writes every record enqueued before the call. It is also the test
// hook that makes durability deterministic.
func (c *persistentIDCache) flushNow() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.pendingMu.Lock()
	buf, n := c.pending, c.pendingCount
	c.pending, c.pendingCount = nil, 0
	c.pendingMu.Unlock()

	if len(buf) == 0 || c.persistDisabled {
		return
	}

	if err := c.appendLocked(buf, n); err != nil {
		c.writeWarnOnce.Do(func() {
			c.logger.Warnf("Google Drive file ID cache is now memory-only: %v", err)
		})

		c.persistDisabled = true
	}
}

// appendLocked writes one batch of records to the end of the journal.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) appendLocked(buf []byte, n int) error {
	if err := c.acquireFileLock(); err != nil {
		return err
	}

	defer c.releaseFileLock()

	if err := c.ensureOpenLocked(); err != nil {
		return err
	}

	// single write(2) on an O_APPEND descriptor: atomic with respect to other
	// appenders even if the advisory lock were unavailable.
	if _, err := c.f.Write(buf); err != nil {
		return errors.Wrap(err, "unable to append to file ID journal")
	}

	c.diskRecords += n

	c.maybeCompactLocked()

	return nil
}

// maybeCompactLocked rewrites the journal when it holds substantially more
// records than live mappings. Callers hold writeMu and the file lock.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) maybeCompactLocked() {
	if c.diskRecords < compactMinRecords {
		return
	}

	c.mu.RLock()
	live := len(c.entries)
	c.mu.RUnlock()

	if c.diskRecords <= compactWasteFactor*live {
		return
	}

	if err := c.compactLocked(); err != nil {
		c.logger.Warnf("unable to compact Google Drive file ID journal: %v", err)
	}
}

// open loads the journal into memory. Every failure mode degrades instead of
// propagating: a missing directory or file simply means an empty cache, and an
// unusable one is quarantined so that the next run starts clean.
func (c *persistentIDCache) open() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := os.MkdirAll(c.dir, cacheDirMode); err != nil {
		c.logger.Warnf("Google Drive file ID cache is memory-only, cannot create %v: %v", c.dir, err)

		c.persistDisabled = true

		return
	}

	c.writeNamespaceMarker()

	c.lock = flock.New(c.lockPath)

	if err := c.loadLocked(); err != nil {
		c.logger.Warnf("unable to load Google Drive file ID cache: %v", err)
		c.quarantineLocked(err)
	}
}

// loadLocked replays the journal into the in-memory map.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) loadLocked() error {
	if err := c.acquireFileLock(); err != nil {
		return err
	}

	defer c.releaseFileLock()

	if err := c.ensureOpenLocked(); err != nil {
		return err
	}

	scan, err := c.readJournal()
	if err != nil {
		return err
	}

	if scan.damaged > 0 {
		c.logger.Warnf("skipped %v damaged bytes in Google Drive file ID journal %v", scan.damaged, c.journalPath)
	}

	c.mu.Lock()
	c.entries = scan.entries
	c.mu.Unlock()

	c.diskRecords = scan.records
	c.noteScannedLocked(scan.end)

	c.maybeCompactLocked()

	return nil
}

// noteScannedLocked records how far into the journal this process has replayed,
// which is the starting point of the next incremental tail read.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) noteScannedLocked(end int64) {
	st, err := os.Stat(c.journalPath) //nolint:gosec // G703: journalPath is derived from the cache directory and a config hash, not request input.
	if err != nil {
		c.scanInfo, c.scanOffset = nil, 0

		return
	}

	c.scanInfo, c.scanOffset = st, end
}

// writeNamespaceMarker records which folder a hashed directory belongs to. It
// is never read back; it exists so that a human looking at the cache directory
// can tell the namespaces apart.
func (c *persistentIDCache) writeNamespaceMarker() {
	b, err := json.Marshal(map[string]string{"folderKey": c.folderKey})
	if err != nil {
		return
	}

	if err := os.WriteFile(filepath.Join(c.dir, fileIDNamespaceName), b, cacheFileMode); err != nil {
		c.logger.Debugf("unable to write Google Drive file ID cache namespace marker: %v", err)
	}
}

// acquireFileLock takes the cross-process advisory lock, which every writer
// holds while it appends, compacts or resets the journal. Callers hold writeMu,
// which keeps this process from contending with itself.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) acquireFileLock() error {
	if c.lock == nil {
		return nil
	}

	return errors.Wrap(c.lock.Lock(), "unable to lock file ID cache")
}

// acquireSharedFileLock takes the cross-process advisory lock in shared mode,
// which readers use: several processes may tail-read the journal at once, and
// none of them blocks the others.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) acquireSharedFileLock() error {
	if c.lock == nil {
		return nil
	}

	return errors.Wrap(c.lock.RLock(), "unable to lock file ID cache for reading")
}

// +checklocks:c.writeMu
func (c *persistentIDCache) releaseFileLock() {
	if c.lock == nil {
		return
	}

	c.lock.Unlock() //nolint:errcheck
}

// reopenLocked reopens the journal under the cross-process lock.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) reopenLocked() error {
	if err := c.acquireFileLock(); err != nil {
		return err
	}

	defer c.releaseFileLock()

	return c.ensureOpenLocked()
}

// ensureOpenLocked opens the journal, or reopens it if another process replaced
// the file by compaction since the last write. This is what makes rename-based
// compaction safe for concurrent appenders.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) ensureOpenLocked() error {
	if c.f != nil {
		fi1, err1 := c.f.Stat()
		fi2, err2 := os.Stat(c.journalPath) //nolint:gosec // G703: journalPath is derived from the cache directory and a config hash, not request input.

		if err1 == nil && err2 == nil && os.SameFile(fi1, fi2) {
			return nil
		}

		c.f.Close() //nolint:errcheck
		c.f = nil
	}

	f, err := os.OpenFile(c.journalPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, cacheFileMode) //nolint:gosec // G703: journalPath is derived from the cache directory and a config hash, not request input.
	if err != nil {
		return errors.Wrap(err, "unable to open file ID journal")
	}

	c.f = f

	if err := c.ensureHeaderLocked(); err != nil {
		c.f.Close() //nolint:errcheck
		c.f = nil

		return err
	}

	return nil
}

// +checklocks:c.writeMu
func (c *persistentIDCache) ensureHeaderLocked() error {
	st, err := c.f.Stat()
	if err != nil {
		return errors.Wrap(err, "unable to stat file ID journal")
	}

	if st.Size() == 0 {
		if _, err := c.f.WriteString(journalHeader); err != nil {
			return errors.Wrap(err, "unable to write file ID journal header")
		}

		return nil
	}

	hdr := make([]byte, len(journalHeader))

	if _, err := c.f.ReadAt(hdr, 0); err != nil {
		return errors.Wrap(err, "unable to read file ID journal header")
	}

	if !bytes.Equal(hdr, []byte(journalHeader)) {
		return errBadJournalMagic
	}

	return nil
}

// quarantineLocked renames an unusable journal out of the way and starts over,
// so that cache damage costs one slow run rather than every future run.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) quarantineLocked(cause error) {
	if c.f != nil {
		c.f.Close() //nolint:errcheck
		c.f = nil
	}

	target := c.journalPath + fileIDQuarantineSuffix + clock.Now().UTC().Format("20060102-150405.000000000")

	if err := os.Rename(c.journalPath, target); err != nil && !os.IsNotExist(err) {
		c.logger.Warnf("unable to quarantine damaged Google Drive file ID journal %v: %v", c.journalPath, err)

		c.persistDisabled = true

		return
	}

	c.logger.Warnf("quarantined damaged Google Drive file ID journal as %v (%v)", target, cause)

	c.diskRecords = 0

	if err := c.reopenLocked(); err != nil {
		c.logger.Warnf("Google Drive file ID cache is memory-only: %v", err)

		c.persistDisabled = true
	}
}

// journalScan is the result of replaying part or all of the journal.
type journalScan struct {
	// entries holds the live mappings the replayed region produced: a record
	// superseded by a later tombstone within the same region leaves no trace.
	entries map[blob.ID]string
	// records is the number of valid records the region held.
	records int
	// damaged is the number of bytes that could not be framed.
	damaged int64
	// end is the byte offset just past the last VALID record, which is where the
	// next incremental read must resume. A torn record at the tail is therefore
	// re-examined next time, by which point the writer may have completed it.
	end int64
}

// readJournal replays the whole journal, which is what open() needs.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) readJournal() (journalScan, error) {
	return c.readJournalFrom(0)
}

// readJournalFrom replays the journal starting at byte offset start, which must
// be either 0 (the header is then consumed first) or an offset previously
// reported as journalScan.end. Damaged regions are skipped, never fatal; only a
// failure to open or read the file is an error.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) readJournalFrom(start int64) (journalScan, error) {
	res := journalScan{
		entries: map[blob.ID]string{},
		end:     start,
	}

	f, err := os.Open(c.journalPath) //nolint:gosec // G703: journalPath is derived from the cache directory and a config hash, not request input.
	if err != nil {
		if os.IsNotExist(err) {
			return journalScan{entries: res.entries}, nil
		}

		return journalScan{}, errors.Wrap(err, "unable to open file ID journal for reading")
	}

	defer f.Close() //nolint:errcheck

	if start == 0 {
		if err := consumeJournalHeader(f); err != nil {
			return journalScan{}, err
		}

		res.end = int64(len(journalHeader))
	} else if _, err := f.Seek(start, io.SeekStart); err != nil {
		return journalScan{}, errors.Wrap(err, "unable to seek in file ID journal")
	}

	s := newJournalScanner(f)

	for {
		blobID, fileID, ok := s.next()
		if !ok {
			break
		}

		res.records++

		if fileID == "" {
			delete(res.entries, blobID)
		} else {
			res.entries[blobID] = fileID
		}
	}

	if s.err != nil {
		return journalScan{}, s.err
	}

	res.damaged = s.damaged
	res.end += s.lastRecordEnd

	return res, nil
}

// consumeJournalHeader reads and validates the fixed header a journal starts
// with, leaving the reader positioned at the first record.
func consumeJournalHeader(r io.Reader) error {
	hdr := make([]byte, len(journalHeader))

	if _, err := io.ReadFull(r, hdr); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errBadJournalMagic
		}

		return errors.Wrap(err, "unable to read file ID journal header")
	}

	if !bytes.Equal(hdr, []byte(journalHeader)) {
		return errBadJournalMagic
	}

	return nil
}

// RescanTail re-reads the records appended to the journal since this process
// last replayed it, so that a blob written by ANOTHER process on this machine
// can be resolved without a Drive round trip.
//
// It reports the file ID of blobID when the tail named it, and whether the tail
// held any records at all - which the read path uses as evidence that somebody
// is actively writing to this repository, and therefore that a name-index miss
// is worth re-querying (see the commentary on nameQueryRetryBudget).
//
// The scan takes the cross-process lock in SHARED mode, so it never blocks
// another process's reads and only ever waits behind one write(2)-sized
// critical section.
//
// Records are applied ONLY to blob IDs this process does not already know. Our
// own in-memory map is newer than anything on disk (appends are batched), so
// re-reading our own records must not be allowed to revert it; and a blob we
// already have an ID for does not need this path in the first place, because a
// stale ID is corrected by the 404-invalidates rule.
func (c *persistentIDCache) RescanTail(blobID blob.ID) (fileID string, found, appended bool) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.persistDisabled {
		return "", false, false
	}

	if err := c.acquireSharedFileLock(); err != nil {
		c.logger.Debugf("unable to lock Google Drive file ID cache for a tail scan: %v", err)

		return "", false, false
	}

	defer c.releaseFileLock()

	st, err := os.Stat(c.journalPath)
	if err != nil {
		return "", false, false
	}

	// A journal that was replaced (another process compacted it) or that shrank
	// cannot be read incrementally. Resynchronize on its current end rather than
	// replaying it: a full replay would apply records this process has already
	// superseded in memory. The compaction itself is write activity, so it is
	// reported as such.
	if c.scanInfo == nil || !os.SameFile(c.scanInfo, st) || st.Size() < c.scanOffset {
		c.scanInfo, c.scanOffset = st, st.Size()

		return "", false, true
	}

	if st.Size() == c.scanOffset {
		return "", false, false
	}

	scan, err := c.readJournalFrom(c.scanOffset)
	if err != nil {
		c.logger.Debugf("unable to tail-read Google Drive file ID cache: %v", err)

		return "", false, false
	}

	c.scanInfo, c.scanOffset = st, scan.end

	if len(scan.entries) > 0 {
		c.applyTailEntries(scan.entries)
	}

	c.mu.RLock()
	fileID, found = c.entries[blobID]
	c.mu.RUnlock()

	return fileID, found, scan.records > 0
}

// applyTailEntries merges records read from the journal tail into the in-memory
// map, leaving mappings this process already holds untouched.
func (c *persistentIDCache) applyTailEntries(entries map[blob.ID]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for id, fileID := range entries {
		if _, ok := c.entries[id]; !ok {
			c.entries[id] = fileID
		}
	}
}

// compactLocked rewrites the journal with one record per live mapping. It
// re-reads the file first so that entries appended by other processes - which
// this process may never have seen - survive, then swaps the result in with a
// temp file plus atomic rename.
//
// +checklocks:c.writeMu
func (c *persistentIDCache) compactLocked() error {
	scan, err := c.readJournal()
	if err != nil {
		return err
	}

	entries := scan.entries

	if scan.records <= len(entries) {
		c.diskRecords = scan.records
		c.noteScannedLocked(scan.end)

		return nil
	}

	tmp, err := os.CreateTemp(c.dir, fileIDJournalName+".tmp-*")
	if err != nil {
		return errors.Wrap(err, "unable to create compacted file ID journal")
	}

	tmpName := tmp.Name()

	defer func() {
		tmp.Close()        //nolint:errcheck
		os.Remove(tmpName) //nolint:errcheck,gosec // G703: tmpName comes from os.CreateTemp in the cache directory.
	}()

	if err := tmp.Chmod(cacheFileMode); err != nil {
		return errors.Wrap(err, "unable to set compacted file ID journal permissions")
	}

	w := bufio.NewWriter(tmp)

	if _, err := w.WriteString(journalHeader); err != nil {
		return errors.Wrap(err, "unable to write compacted file ID journal")
	}

	written := 0

	for blobID, fileID := range entries {
		rec, ok := encodeRecord(blobID, fileID)
		if !ok {
			continue
		}

		if _, err := w.Write(rec); err != nil {
			return errors.Wrap(err, "unable to write compacted file ID journal")
		}

		written++
	}

	if err := w.Flush(); err != nil {
		return errors.Wrap(err, "unable to flush compacted file ID journal")
	}

	if err := tmp.Sync(); err != nil {
		return errors.Wrap(err, "unable to sync compacted file ID journal")
	}

	if err := tmp.Close(); err != nil {
		return errors.Wrap(err, "unable to close compacted file ID journal")
	}

	if err := os.Rename(tmpName, c.journalPath); err != nil { //nolint:gosec // G703: journalPath is derived from the cache directory and a config hash, not request input.
		return errors.Wrap(err, "unable to replace file ID journal")
	}

	// the descriptor now points at the replaced inode.
	if c.f != nil {
		c.f.Close() //nolint:errcheck
		c.f = nil
	}

	c.diskRecords = written

	// The compacted file holds exactly what this process has in memory, so the
	// next tail read must start at its end.
	if st, serr := os.Stat(c.journalPath); serr == nil { //nolint:gosec // G703: journalPath is derived from the cache directory and a config hash, not request input.
		c.scanInfo, c.scanOffset = st, st.Size()
	} else {
		c.scanInfo, c.scanOffset = nil, 0
	}

	return nil
}

// encodeRecord builds one self-framing journal record. It reports false for a
// mapping too large to frame, which cannot happen with real Drive IDs.
func encodeRecord(blobID blob.ID, fileID string) ([]byte, bool) {
	if len(blobID) > math.MaxUint16 {
		return nil, false
	}

	payloadLen := blobIDLenSize + len(blobID) + len(fileID)
	if payloadLen > maxRecordPayload {
		return nil, false
	}

	rec := make([]byte, recordHeaderLen, recordHeaderLen+payloadLen)

	copy(rec, recordMagic[:])
	binary.LittleEndian.PutUint32(rec[recordMagicLen:], uint32(payloadLen))

	//nolint:gosec // bounds-checked immediately above.
	rec = binary.LittleEndian.AppendUint16(rec, uint16(len(blobID)))
	rec = append(rec, blobID...)
	rec = append(rec, fileID...)

	crc := crc32.Update(0, crcTable, rec[recordMagicLen:recordMagicLen+recordLenSize])
	crc = crc32.Update(crc, crcTable, rec[recordHeaderLen:])

	binary.LittleEndian.PutUint32(rec[recordMagicLen+recordLenSize:], crc)

	return rec, true
}

func decodeRecord(payload []byte) (blob.ID, string, bool) {
	if len(payload) < blobIDLenSize {
		return "", "", false
	}

	n := int(binary.LittleEndian.Uint16(payload))
	if n == 0 || blobIDLenSize+n > len(payload) {
		return "", "", false
	}

	return blob.ID(payload[blobIDLenSize : blobIDLenSize+n]), string(payload[blobIDLenSize+n:]), true
}

// journalScanner reads records from a stream, resynchronizing on the record
// magic whenever the framing or the checksum does not hold. Every byte it
// cannot turn into a valid record is counted in damaged and dropped.
type journalScanner struct {
	r       *bufio.Reader
	buf     []byte
	scratch []byte
	damaged int64
	eof     bool
	err     error

	// consumed counts every byte removed from buf, damaged or not, and
	// lastRecordEnd is its value just after the most recent valid record. An
	// incremental reader resumes at lastRecordEnd, so a record still being
	// written when the file was read is examined again next time instead of
	// being skipped forever.
	consumed      int64
	lastRecordEnd int64
}

func newJournalScanner(r io.Reader) *journalScanner {
	return &journalScanner{
		r:       bufio.NewReaderSize(r, journalReadChunk),
		scratch: make([]byte, journalReadChunk),
	}
}

// fill grows the buffer to at least n bytes, reporting false when the stream
// cannot supply that many.
func (s *journalScanner) fill(n int) bool {
	for len(s.buf) < n {
		if s.eof {
			return false
		}

		got, err := s.r.Read(s.scratch)
		s.buf = append(s.buf, s.scratch[:got]...)

		if err != nil {
			s.eof = true

			if !errors.Is(err, io.EOF) {
				s.err = errors.Wrap(err, "unable to read file ID journal")

				return false
			}
		}
	}

	return true
}

// skip drops n leading bytes as damaged.
func (s *journalScanner) skip(n int) {
	s.buf = s.buf[n:]
	s.damaged += int64(n)
	s.consumed += int64(n)
}

func (s *journalScanner) next() (blob.ID, string, bool) {
	for {
		if !s.fill(recordHeaderLen) {
			s.skip(len(s.buf))

			return "", "", false
		}

		if i := bytes.Index(s.buf, recordMagic[:]); i != 0 {
			if !s.resync(i) {
				return "", "", false
			}

			continue
		}

		payloadLen := int(binary.LittleEndian.Uint32(s.buf[recordMagicLen:]))
		total := recordHeaderLen + payloadLen

		if payloadLen > maxRecordPayload || !s.fill(total) {
			s.skip(1)

			continue
		}

		want := binary.LittleEndian.Uint32(s.buf[recordMagicLen+recordLenSize:])
		crc := crc32.Update(0, crcTable, s.buf[recordMagicLen:recordMagicLen+recordLenSize])
		crc = crc32.Update(crc, crcTable, s.buf[recordHeaderLen:total])

		if crc != want {
			s.skip(1)

			continue
		}

		blobID, fileID, ok := decodeRecord(s.buf[recordHeaderLen:total])
		if !ok {
			s.skip(1)

			continue
		}

		s.buf = s.buf[total:]
		s.consumed += int64(total)
		s.lastRecordEnd = s.consumed

		return blobID, fileID, true
	}
}

// resync discards everything before the next possible record start. i is the
// index of the magic in the buffer, or -1 when it is not there at all.
func (s *journalScanner) resync(i int) bool {
	if i > 0 {
		s.skip(i)

		return true
	}

	// no magic in the buffer: keep the bytes that could still be its prefix.
	if keep := recordMagicLen - 1; len(s.buf) > keep {
		s.skip(len(s.buf) - keep)
	}

	if !s.fill(len(s.buf) + 1) {
		s.skip(len(s.buf))

		return false
	}

	return true
}
