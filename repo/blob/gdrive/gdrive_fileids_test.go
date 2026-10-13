//go:build !no_extra_providers

package gdrive

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/repo/blob"
)

// Environment used to re-execute the test binary as a genuinely separate
// process for the multi-process journal test.
const (
	childEnvDir    = "KOPIA_TEST_GDRIVE_FILEIDS_DIR"
	childEnvFolder = "KOPIA_TEST_GDRIVE_FILEIDS_FOLDER"
	childEnvPrefix = "KOPIA_TEST_GDRIVE_FILEIDS_PREFIX"
	childEnvCount  = "KOPIA_TEST_GDRIVE_FILEIDS_COUNT"
)

func TestMain(m *testing.M) {
	if os.Getenv(childEnvDir) != "" {
		os.Exit(runFileIDsChild())
	}

	os.Exit(m.Run())
}

// runFileIDsChild is the body of the child process: it appends its own
// mappings to a journal shared with the parent, flushing frequently so that
// its writes genuinely interleave with the parent's writes and compactions.
func runFileIDsChild() int {
	ctx := context.Background()

	n, err := strconv.Atoi(os.Getenv(childEnvCount))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad count:", err)
		return 1
	}

	c, err := newPersistentIDCache(ctx, os.Getenv(childEnvDir), os.Getenv(childEnvFolder))
	if err != nil {
		fmt.Fprintln(os.Stderr, "unable to open cache:", err)
		return 1
	}

	prefix := os.Getenv(childEnvPrefix)

	for i := range n {
		blobID := blob.ID(fmt.Sprintf("%v-%v", prefix, i))

		// write a superseded value first, so the journal accumulates waste and the
		// parent's compaction has something to do.
		c.Put(blobID, "stale")
		c.Put(blobID, fmt.Sprintf("file-%v-%v", prefix, i))

		if i%8 == 0 {
			c.flushNow()
		}

		// pace the child so that its lifetime spans many of the parent's
		// compactions rather than finishing before the first one.
		if i%25 == 0 {
			time.Sleep(time.Millisecond)
		}
	}

	if err := c.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "unable to close cache:", err)
		return 1
	}

	return 0
}

func newTestIDCache(t *testing.T, dir, folderKey string) *persistentIDCache {
	t.Helper()

	c, err := newPersistentIDCache(testlogging.Context(t), dir, folderKey)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, c.Close()) })

	return c
}

// abandonIDCache simulates a process that died: the background writer is
// stopped, pending records are dropped on the floor and the descriptors are
// released, all without the final flush that Close performs.
func abandonIDCache(t *testing.T, c *persistentIDCache) {
	t.Helper()

	idCacheRegistryMu.Lock()
	delete(idCacheRegistry, c.regKey)
	c.refs = 0

	idCacheRegistryMu.Unlock()

	c.closeOnce.Do(func() { close(c.closeCh) })
	c.wg.Wait()

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.f != nil {
		require.NoError(t, c.f.Close())
		c.f = nil
	}

	if c.lock != nil {
		require.NoError(t, c.lock.Close())
		c.lock = nil
	}
}

func forceCompact(t *testing.T, c *persistentIDCache) {
	t.Helper()

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	require.NoError(t, c.acquireFileLock())

	defer c.releaseFileLock()

	require.NoError(t, c.compactLocked())
}

func journalPathFor(dir, folderKey string) string {
	return filepath.Join(fileIDNamespaceDir(filepath.Clean(dir), folderKey), fileIDJournalName)
}

func TestPersistentIDCacheBasics(t *testing.T) {
	c := newTestIDCache(t, t.TempDir(), "folder-1")

	_, ok := c.Get("missing")
	require.False(t, ok)

	c.Put("blob-1", "file-1")
	c.Put("blob-2", "file-2")

	fileID, ok := c.Get("blob-1")
	require.True(t, ok)
	require.Equal(t, "file-1", fileID)

	// last write wins.
	c.Put("blob-1", "file-1b")
	fileID, ok = c.Get("blob-1")
	require.True(t, ok)
	require.Equal(t, "file-1b", fileID)

	// tombstone.
	c.Delete("blob-1")
	fileID, ok = c.Get("blob-1")
	require.False(t, ok)
	require.Empty(t, fileID)

	// deleting an unknown blob is harmless.
	c.Delete("never-existed")

	// an empty file ID is not a mapping.
	c.Put("blob-3", "")
	_, ok = c.Get("blob-3")
	require.False(t, ok)

	// empty blob IDs are ignored.
	c.Put("", "file-x")
	c.Delete("")

	fileID, ok = c.Get("blob-2")
	require.True(t, ok)
	require.Equal(t, "file-2", fileID)
}

func TestPersistentIDCacheEmptyArguments(t *testing.T) {
	ctx := testlogging.Context(t)

	_, err := newPersistentIDCache(ctx, "", "folder")
	require.ErrorIs(t, err, errEmptyCacheDir)

	_, err = newPersistentIDCache(ctx, t.TempDir(), "")
	require.ErrorIs(t, err, errEmptyFolderKey)
}

func TestPersistentIDCacheDurability(t *testing.T) {
	dir := t.TempDir()

	const n = 500

	c := newTestIDCache(t, dir, "folder-1")

	for i := range n {
		c.Put(blob.ID(fmt.Sprintf("blob-%v", i)), fmt.Sprintf("file-%v", i))
	}

	c.Delete("blob-7")
	require.NoError(t, c.Close())

	c2 := newTestIDCache(t, dir, "folder-1")

	for i := range n {
		fileID, ok := c2.Get(blob.ID(fmt.Sprintf("blob-%v", i)))

		if i == 7 {
			require.False(t, ok, "tombstone was resurrected")
			continue
		}

		require.True(t, ok, "missing entry %v", i)
		require.Equal(t, fmt.Sprintf("file-%v", i), fileID)
	}
}

func TestPersistentIDCacheCrashLosesOnlyUnflushed(t *testing.T) {
	dir := t.TempDir()

	c := newTestIDCache(t, dir, "folder-1")
	c.batchSize = 1 << 30 // never auto-flush; the test decides

	for i := range 5 {
		c.Put(blob.ID(fmt.Sprintf("flushed-%v", i)), fmt.Sprintf("file-%v", i))
	}

	c.flushNow()

	for i := range 5 {
		c.Put(blob.ID(fmt.Sprintf("unflushed-%v", i)), fmt.Sprintf("file-%v", i))
	}

	abandonIDCache(t, c)

	c2 := newTestIDCache(t, dir, "folder-1")

	// flushed batches survive; unflushed ones are acceptable losses.
	for i := range 5 {
		fileID, ok := c2.Get(blob.ID(fmt.Sprintf("flushed-%v", i)))
		require.True(t, ok, "lost an acknowledged entry")
		require.Equal(t, fmt.Sprintf("file-%v", i), fileID)
	}
}

func TestPersistentIDCacheFlushDropsEverything(t *testing.T) {
	dir := t.TempDir()

	c := newTestIDCache(t, dir, "folder-1")

	for i := range 20 {
		c.Put(blob.ID(fmt.Sprintf("blob-%v", i)), fmt.Sprintf("file-%v", i))
	}

	c.flushNow()
	c.Flush()

	_, ok := c.Get("blob-1")
	require.False(t, ok)

	st, err := os.Stat(journalPathFor(dir, "folder-1"))
	require.NoError(t, err)
	require.Equal(t, int64(len(journalHeader)), st.Size())

	// the cache remains usable after a flush.
	c.Put("after-flush", "file-after")
	require.NoError(t, c.Close())

	c2 := newTestIDCache(t, dir, "folder-1")

	_, ok = c2.Get("blob-1")
	require.False(t, ok, "flushed state came back after reopen")

	fileID, ok := c2.Get("after-flush")
	require.True(t, ok)
	require.Equal(t, "file-after", fileID)
}

// entries written by this test have a constant on-disk record size, which lets
// the corruption cases target an exact record.
const (
	corruptEntries    = 10
	corruptRecordSize = recordHeaderLen + blobIDLenSize + len("blob-0") + len("file-0")
)

func writeCorruptionFixture(t *testing.T, dir string) string {
	t.Helper()

	c := newTestIDCache(t, dir, "folder-1")

	for i := range corruptEntries {
		c.Put(blob.ID(fmt.Sprintf("blob-%v", i)), fmt.Sprintf("file-%v", i))
	}

	require.NoError(t, c.Close())

	path := journalPathFor(dir, "folder-1")

	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(len(journalHeader)+corruptEntries*corruptRecordSize), st.Size(),
		"fixture assumes fixed-size records")

	return path
}

func TestPersistentIDCacheCorruptionTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	path := writeCorruptionFixture(t, dir)

	// cut the file in the middle of record 6.
	require.NoError(t, os.Truncate(path, int64(len(journalHeader)+6*corruptRecordSize+7)))

	c := newTestIDCache(t, dir, "folder-1")

	for i := range corruptEntries {
		_, ok := c.Get(blob.ID(fmt.Sprintf("blob-%v", i)))
		require.Equal(t, i < 6, ok, "unexpected presence of entry %v", i)
	}

	// still writable afterwards.
	c.Put("post-damage", "file-post")
	c.flushNow()
	require.NoError(t, c.Close())

	c2 := newTestIDCache(t, dir, "folder-1")
	_, ok := c2.Get("post-damage")
	require.True(t, ok)
}

func TestPersistentIDCacheCorruptionTrailingGarbage(t *testing.T) {
	dir := t.TempDir()
	path := writeCorruptionFixture(t, dir)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, cacheFileMode)
	require.NoError(t, err)

	garbage := make([]byte, 1024)
	for i := range garbage {
		garbage[i] = byte(i%251) | 1
	}

	_, err = f.Write(garbage)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	c := newTestIDCache(t, dir, "folder-1")

	for i := range corruptEntries {
		fileID, ok := c.Get(blob.ID(fmt.Sprintf("blob-%v", i)))
		require.True(t, ok, "lost entry %v to trailing garbage", i)
		require.Equal(t, fmt.Sprintf("file-%v", i), fileID)
	}
}

func TestPersistentIDCacheCorruptionFlippedByteInMiddleRecord(t *testing.T) {
	dir := t.TempDir()
	path := writeCorruptionFixture(t, dir)

	const damaged = 4

	f, err := os.OpenFile(path, os.O_RDWR, cacheFileMode)
	require.NoError(t, err)

	// flip a bit inside the payload of record 4.
	off := int64(len(journalHeader) + damaged*corruptRecordSize + recordHeaderLen + 3)

	var b [1]byte

	_, err = f.ReadAt(b[:], off)
	require.NoError(t, err)

	b[0] ^= 0x40

	_, err = f.WriteAt(b[:], off)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	c := newTestIDCache(t, dir, "folder-1")

	for i := range corruptEntries {
		fileID, ok := c.Get(blob.ID(fmt.Sprintf("blob-%v", i)))

		if i == damaged {
			require.False(t, ok, "damaged record was accepted")
			continue
		}

		require.True(t, ok, "intact record %v was dropped", i)
		require.Equal(t, fmt.Sprintf("file-%v", i), fileID)
	}
}

func TestPersistentIDCacheCorruptionInjectedBytes(t *testing.T) {
	dir := t.TempDir()
	path := writeCorruptionFixture(t, dir)

	orig, err := os.ReadFile(path)
	require.NoError(t, err)

	cut := len(journalHeader) + 5*corruptRecordSize

	junk := make([]byte, 37)
	for i := range junk {
		junk[i] = 0xA5
	}

	var damagedFile []byte

	damagedFile = append(damagedFile, orig[:cut]...)
	damagedFile = append(damagedFile, junk...)
	damagedFile = append(damagedFile, orig[cut:]...)

	require.NoError(t, os.WriteFile(path, damagedFile, cacheFileMode))

	c := newTestIDCache(t, dir, "folder-1")

	// records on both sides of the injected junk must survive.
	for i := range corruptEntries {
		fileID, ok := c.Get(blob.ID(fmt.Sprintf("blob-%v", i)))
		require.True(t, ok, "lost entry %v to injected bytes", i)
		require.Equal(t, fmt.Sprintf("file-%v", i), fileID)
	}
}

func TestPersistentIDCacheQuarantinesUnusableJournal(t *testing.T) {
	dir := t.TempDir()
	path := writeCorruptionFixture(t, dir)

	// destroy the file-level header, which makes the whole file unusable.
	f, err := os.OpenFile(path, os.O_RDWR, cacheFileMode)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("NOT-A-KOPIA-JOURNAL!!!!!"), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	c := newTestIDCache(t, dir, "folder-1")

	_, ok := c.Get("blob-1")
	require.False(t, ok, "entries loaded from an unusable journal")

	quarantined, err := filepath.Glob(path + fileIDQuarantineSuffix + "*")
	require.NoError(t, err)
	require.Len(t, quarantined, 1, "damaged journal was not quarantined")

	// and the cache works from scratch.
	c.Put("fresh", "file-fresh")
	require.NoError(t, c.Close())

	c2 := newTestIDCache(t, dir, "folder-1")
	fileID, ok := c2.Get("fresh")
	require.True(t, ok)
	require.Equal(t, "file-fresh", fileID)
}

func TestPersistentIDCacheSharedInstanceRegistry(t *testing.T) {
	dir := t.TempDir()
	ctx := testlogging.Context(t)

	c1, err := newPersistentIDCache(ctx, dir, "folder-1")
	require.NoError(t, err)

	c2, err := newPersistentIDCache(ctx, dir, "folder-1")
	require.NoError(t, err)

	require.Same(t, c1, c2, "connections to the same folder must share one cache")

	// same directory expressed differently still resolves to one instance.
	c3, err := newPersistentIDCache(ctx, dir+string(filepath.Separator)+".", "folder-1")
	require.NoError(t, err)
	require.Same(t, c1, c3)

	other, err := newPersistentIDCache(ctx, dir, "folder-2")
	require.NoError(t, err)
	require.NotSame(t, c1, other)

	// read-your-writes across "connections".
	c1.Put("blob-1", "file-1")

	fileID, ok := c2.Get("blob-1")
	require.True(t, ok)
	require.Equal(t, "file-1", fileID)

	// refcounting: the instance stays alive and registered until the last Close.
	require.NoError(t, c3.Close())
	require.NoError(t, c2.Close())

	idCacheRegistryMu.Lock()

	_, stillRegistered := idCacheRegistry[c1.regKey]

	idCacheRegistryMu.Unlock()
	require.True(t, stillRegistered)

	require.NoError(t, c1.Close())

	idCacheRegistryMu.Lock()

	_, stillRegistered = idCacheRegistry[c1.regKey]

	idCacheRegistryMu.Unlock()
	require.False(t, stillRegistered)

	// extra closes are harmless.
	require.NoError(t, c1.Close())

	c4, err := newPersistentIDCache(ctx, dir, "folder-1")
	require.NoError(t, err)
	require.NotSame(t, c1, c4)

	fileID, ok = c4.Get("blob-1")
	require.True(t, ok, "entry did not survive the last Close")
	require.Equal(t, "file-1", fileID)

	require.NoError(t, c4.Close())
	require.NoError(t, other.Close())
}

func TestPersistentIDCacheNamespaceIsolation(t *testing.T) {
	dir := t.TempDir()

	a := newTestIDCache(t, dir, "folder-a")
	b := newTestIDCache(t, dir, "folder-b")

	require.NotEqual(t, a.dir, b.dir)
	require.NotEqual(t, journalPathFor(dir, "folder-a"), journalPathFor(dir, "folder-b"))

	a.Put("shared-name", "file-in-a")
	b.Put("shared-name", "file-in-b")
	a.Put("only-in-a", "file-a")

	require.NoError(t, a.Close())
	require.NoError(t, b.Close())

	a2 := newTestIDCache(t, dir, "folder-a")
	b2 := newTestIDCache(t, dir, "folder-b")

	fileID, ok := a2.Get("shared-name")
	require.True(t, ok)
	require.Equal(t, "file-in-a", fileID)

	fileID, ok = b2.Get("shared-name")
	require.True(t, ok)
	require.Equal(t, "file-in-b", fileID)

	_, ok = b2.Get("only-in-a")
	require.False(t, ok, "namespaces cross-pollinated")
}

func TestPersistentIDCacheConcurrentAccess(t *testing.T) {
	dir := t.TempDir()

	c := newTestIDCache(t, dir, "folder-1")
	c.batchSize = 16

	const (
		workers = 8
		perGoro = 300
	)

	var wg sync.WaitGroup

	for w := range workers {
		wg.Go(func() {
			for i := range perGoro {
				blobID := blob.ID(fmt.Sprintf("w%v-blob-%v", w, i))
				c.Put(blobID, fmt.Sprintf("w%v-file-%v", w, i))

				if fileID, ok := c.Get(blobID); ok && fileID != fmt.Sprintf("w%v-file-%v", w, i) {
					t.Errorf("Get(%v) returned %q", blobID, fileID)
				}

				if i%3 == 0 {
					c.Delete(blob.ID(fmt.Sprintf("w%v-blob-%v", w, i)))
				}

				if i%97 == 0 {
					c.flushNow()
				}
			}
		})
	}

	// concurrent readers and a concurrent whole-cache flush must not corrupt
	// anything either.
	wg.Go(func() {
		for i := range 200 {
			c.Get(blob.ID(fmt.Sprintf("w0-blob-%v", i)))
		}
	})

	wg.Wait()
	c.flushNow()

	for w := range workers {
		for i := range perGoro {
			fileID, ok := c.Get(blob.ID(fmt.Sprintf("w%v-blob-%v", w, i)))

			if i%3 == 0 {
				require.False(t, ok)
				continue
			}

			require.True(t, ok)
			require.Equal(t, fmt.Sprintf("w%v-file-%v", w, i), fileID)
		}
	}
}

func TestPersistentIDCacheCompaction(t *testing.T) {
	dir := t.TempDir()

	c := newTestIDCache(t, dir, "folder-1")
	c.batchSize = 1 << 30

	// deliberately below compactMinRecords, so that only the explicit
	// compaction below can shrink the file.
	const (
		live     = 50
		rewrites = 10
	)

	for r := range rewrites {
		for i := range live {
			c.Put(blob.ID(fmt.Sprintf("blob-%v", i)), fmt.Sprintf("file-%v-%v", i, r))
		}
	}

	c.Put("doomed", "file-doomed")
	c.Delete("doomed")
	c.flushNow()

	before, err := os.Stat(journalPathFor(dir, "folder-1"))
	require.NoError(t, err)

	forceCompact(t, c)

	after, err := os.Stat(journalPathFor(dir, "folder-1"))
	require.NoError(t, err)
	require.Less(t, after.Size(), before.Size(), "compaction did not shrink the journal")

	require.NoError(t, c.Close())

	c2 := newTestIDCache(t, dir, "folder-1")

	for i := range live {
		fileID, ok := c2.Get(blob.ID(fmt.Sprintf("blob-%v", i)))
		require.True(t, ok)
		require.Equal(t, fmt.Sprintf("file-%v-%v", i, rewrites-1), fileID)
	}

	_, ok := c2.Get("doomed")
	require.False(t, ok, "compaction resurrected a tombstoned entry")
}

func TestPersistentIDCacheAutomaticCompactionOnWrite(t *testing.T) {
	dir := t.TempDir()

	c := newTestIDCache(t, dir, "folder-1")
	c.batchSize = 1 << 30

	const (
		live     = 100
		rewrites = 30
	)

	for r := range rewrites {
		for i := range live {
			c.Put(blob.ID(fmt.Sprintf("blob-%v", i)), fmt.Sprintf("file-%v-%v", i, r))
		}
	}

	c.flushNow()

	st, err := os.Stat(journalPathFor(dir, "folder-1"))
	require.NoError(t, err)

	c.writeMu.Lock()
	records := c.diskRecords
	c.writeMu.Unlock()

	require.Equal(t, live, records, "journal was not compacted after a large batch")
	require.Less(t, st.Size(), int64(live*rewrites*recordHeaderLen/2), "journal did not shrink")

	require.NoError(t, c.Close())

	c2 := newTestIDCache(t, dir, "folder-1")

	for i := range live {
		fileID, ok := c2.Get(blob.ID(fmt.Sprintf("blob-%v", i)))
		require.True(t, ok)
		require.Equal(t, fmt.Sprintf("file-%v-%v", i, rewrites-1), fileID)
	}
}

func TestPersistentIDCacheCompactionAtOpen(t *testing.T) {
	dir := t.TempDir()

	// build a wasteful journal directly, as a process that died before
	// compaction would have left it.
	require.NoError(t, newTestIDCache(t, dir, "folder-1").Close())

	path := journalPathFor(dir, "folder-1")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, cacheFileMode)
	require.NoError(t, err)

	const (
		live     = 100
		rewrites = 40
	)

	for r := range rewrites {
		for i := range live {
			rec, ok := encodeRecord(blob.ID(fmt.Sprintf("blob-%v", i)), fmt.Sprintf("file-%v-%v", i, r))
			require.True(t, ok)

			_, err = f.Write(rec)
			require.NoError(t, err)
		}
	}

	require.NoError(t, f.Close())

	big, err := os.Stat(path)
	require.NoError(t, err)

	c := newTestIDCache(t, dir, "folder-1")

	small, err := os.Stat(path)
	require.NoError(t, err)
	require.Less(t, small.Size(), big.Size(), "journal was not compacted at open")

	for i := range live {
		fileID, ok := c.Get(blob.ID(fmt.Sprintf("blob-%v", i)))
		require.True(t, ok)
		require.Equal(t, fmt.Sprintf("file-%v-%v", i, rewrites-1), fileID)
	}
}

func TestPersistentIDCacheEntryCap(t *testing.T) {
	dir := t.TempDir()

	c := newTestIDCache(t, dir, "folder-1")
	c.maxEntries = 10

	for i := range 25 {
		c.Put(blob.ID(fmt.Sprintf("blob-%02d", i)), fmt.Sprintf("file-%v", i))
	}

	// the in-memory map keeps working past the cap.
	fileID, ok := c.Get("blob-20")
	require.True(t, ok)
	require.Equal(t, "file-20", fileID)

	require.NoError(t, c.Close())

	c2 := newTestIDCache(t, dir, "folder-1")

	persisted := 0

	for i := range 25 {
		if _, ok := c2.Get(blob.ID(fmt.Sprintf("blob-%02d", i))); ok {
			persisted++
		}
	}

	require.Equal(t, 10, persisted, "entries past the cap must not be persisted")
}

func TestPersistentIDCacheRecordCodec(t *testing.T) {
	rec, ok := encodeRecord("blob", "file")
	require.True(t, ok)

	blobID, fileID, ok := decodeRecord(rec[recordHeaderLen:])
	require.True(t, ok)
	require.Equal(t, blob.ID("blob"), blobID)
	require.Equal(t, "file", fileID)

	// tombstone.
	rec, ok = encodeRecord("blob", "")
	require.True(t, ok)
	blobID, fileID, ok = decodeRecord(rec[recordHeaderLen:])
	require.True(t, ok)
	require.Equal(t, blob.ID("blob"), blobID)
	require.Empty(t, fileID)

	// oversized mappings are rejected rather than truncated.
	_, ok = encodeRecord(blob.ID(make([]byte, 70000)), "file")
	require.False(t, ok)

	_, ok = encodeRecord("blob", string(make([]byte, maxRecordPayload+1)))
	require.False(t, ok)

	// malformed payloads.
	_, _, ok = decodeRecord(nil)
	require.False(t, ok)
	_, _, ok = decodeRecord([]byte{0xFF, 0xFF, 'x'})
	require.False(t, ok)
	_, _, ok = decodeRecord([]byte{0, 0})
	require.False(t, ok, "zero-length blob IDs are not valid records")
}

// appendRawRecord writes one record onto the end of a journal the way another
// process would: a single write(2) on an O_APPEND descriptor.
func appendRawRecord(t *testing.T, journalPath string, blobID blob.ID, fileID string) {
	t.Helper()

	rec, ok := encodeRecord(blobID, fileID)
	require.True(t, ok)

	f, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, cacheFileMode)
	require.NoError(t, err)

	defer f.Close() //nolint:errcheck

	_, err = f.Write(rec)
	require.NoError(t, err)
}

// TestPersistentIDCacheRescanTail covers the incremental tail read that the
// read path uses to recover a blob another process on this machine wrote.
func TestPersistentIDCacheRescanTail(t *testing.T) {
	dir := t.TempDir()
	c := newTestIDCache(t, dir, "folder-tail")

	// A quiet journal reports no activity and finds nothing.
	fileID, found, appended := c.RescanTail("elsewhere")
	require.Empty(t, fileID)
	require.False(t, found)
	require.False(t, appended, "an unchanged journal is not evidence of a concurrent writer")

	appendRawRecord(t, c.journalPath, "elsewhere", "file-from-another-process")

	fileID, found, appended = c.RescanTail("elsewhere")
	require.True(t, found)
	require.Equal(t, "file-from-another-process", fileID)
	require.True(t, appended)

	// The mapping is now in memory, so an ordinary Get sees it.
	got, ok := c.Get("elsewhere")
	require.True(t, ok)
	require.Equal(t, "file-from-another-process", got)

	// ... and the region just read is not read again.
	_, _, appended = c.RescanTail("elsewhere")
	require.False(t, appended)

	// A record about some other blob still counts as activity, which is what
	// licenses the read path to re-query a lagging name index.
	appendRawRecord(t, c.journalPath, "someone-else", "another-file")

	_, found, appended = c.RescanTail("elsewhere-still-missing")
	require.False(t, found)
	require.True(t, appended)
}

// TestPersistentIDCacheRescanTailKeepsOurOwnWrites pins the merge rule: records
// read from the tail never overwrite a mapping this process already holds,
// because our in-memory map is newer than anything on disk (appends are
// batched) and re-reading our own records must not revert it.
func TestPersistentIDCacheRescanTailKeepsOurOwnWrites(t *testing.T) {
	dir := t.TempDir()
	c := newTestIDCache(t, dir, "folder-tail-merge")

	c.Put("blob-a", "old-file")
	c.flushNow()

	c.Put("blob-a", "new-file")

	_, _, appended := c.RescanTail("blob-a")
	require.True(t, appended)

	got, ok := c.Get("blob-a")
	require.True(t, ok)
	require.Equal(t, "new-file", got, "the tail scan must not revert a newer in-memory mapping")

	// A tombstone from another process is likewise ignored for a blob we know:
	// a stale ID is corrected by the 404-invalidates rule, which is cheaper than
	// discarding a mapping we are still using.
	appendRawRecord(t, c.journalPath, "blob-a", "")

	_, _, appended = c.RescanTail("blob-a")
	require.True(t, appended)

	got, ok = c.Get("blob-a")
	require.True(t, ok)
	require.Equal(t, "new-file", got)
}

// TestPersistentIDCacheRescanTailAfterCompaction covers the case where the
// journal this process was tracking has been replaced: the offsets are
// meaningless, so the scan resynchronizes on the new file and reports the
// replacement as activity rather than replaying it.
func TestPersistentIDCacheRescanTailAfterCompaction(t *testing.T) {
	dir := t.TempDir()
	c := newTestIDCache(t, dir, "folder-tail-compaction")

	c.Put("blob-a", "file-a")
	c.flushNow()

	_, _, appended := c.RescanTail("blob-a")
	require.True(t, appended)

	// Another process compacts the journal out from under us: it builds a
	// replacement elsewhere and renames it over ours, which is what
	// compactLocked does.
	journal := journalPathFor(dir, "folder-tail-compaction")

	replacement := newTestIDCache(t, filepath.Join(dir, "other-process"), "folder-tail-compaction")
	replacement.Put("blob-c", "file-c")
	replacement.flushNow()

	rebuilt, err := os.ReadFile(replacement.journalPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(journal+".tmp", rebuilt, cacheFileMode))
	require.NoError(t, os.Rename(journal+".tmp", journal))

	_, found, appended := c.RescanTail("blob-c")
	require.False(t, found, "a replaced journal is resynchronized, not replayed")
	require.True(t, appended, "the replacement itself is write activity")

	// Records appended to the NEW file are picked up normally from there on.
	appendRawRecord(t, journal, "blob-d", "file-d")

	fileID, found, appended := c.RescanTail("blob-d")
	require.True(t, found)
	require.Equal(t, "file-d", fileID)
	require.True(t, appended)
}

func TestPersistentIDCacheMultiProcess(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)

	dir := t.TempDir()

	const (
		children      = 3
		perChild      = 600
		maxParentPuts = 200000
	)

	parent := newTestIDCache(t, dir, "folder-shared")

	type childResult struct {
		out []byte
		err error
	}

	results := make(chan childResult, children)

	var (
		wg   sync.WaitGroup
		done atomic.Int32
	)

	for ch := range children {
		wg.Go(func() {
			defer done.Add(1)

			cmd := exec.CommandContext(t.Context(), exe, "-test.run=TestPersistentIDCacheNeverMatches")

			cmd.Env = append(os.Environ(),
				childEnvDir+"="+dir,
				childEnvFolder+"=folder-shared",
				childEnvPrefix+"=child"+strconv.Itoa(ch),
				childEnvCount+"="+strconv.Itoa(perChild),
			)

			out, err := cmd.CombinedOutput()
			results <- childResult{out, err}
		})
	}

	// keep writing and compacting for as long as the children are alive, so that
	// this process really does rename the journal underneath their descriptors.
	parentPuts := 0

	for parentPuts < maxParentPuts && done.Load() < children {
		parent.Put(blob.ID(fmt.Sprintf("parent-%v", parentPuts)), fmt.Sprintf("file-parent-%v", parentPuts))
		parentPuts++

		if parentPuts%20 == 0 {
			parent.flushNow()
			forceCompact(t, parent)
			time.Sleep(time.Millisecond)
		}
	}

	t.Logf("parent wrote %v entries alongside %v child processes", parentPuts, children)
	require.Greater(t, parentPuts, 50)

	parent.flushNow()

	wg.Wait()
	close(results)

	for r := range results {
		require.NoError(t, r.err, "child failed: %s", r.out)
	}

	forceCompact(t, parent)
	require.NoError(t, parent.Close())

	// a fresh instance must see every acknowledged entry of every process.
	final := newTestIDCache(t, dir, "folder-shared")

	for i := range parentPuts {
		fileID, ok := final.Get(blob.ID(fmt.Sprintf("parent-%v", i)))
		require.True(t, ok, "lost parent entry %v", i)
		require.Equal(t, fmt.Sprintf("file-parent-%v", i), fileID)
	}

	for ch := range children {
		for i := range perChild {
			prefix := "child" + strconv.Itoa(ch)

			fileID, ok := final.Get(blob.ID(fmt.Sprintf("%v-%v", prefix, i)))
			require.True(t, ok, "lost entry %v of child %v", i, ch)
			require.Equal(t, fmt.Sprintf("file-%v-%v", prefix, i), fileID)
		}
	}
}
