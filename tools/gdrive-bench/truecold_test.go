//go:build !no_extra_providers

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
)

// plantJournal creates a file-ID journal tree the way the native backend lays
// it out, and returns the directory the purge is expected to remove.
func plantJournal(t *testing.T, cacheDir string) string {
	t.Helper()

	// The namespace directory is named after a hash of the folder ID; the purge
	// removes the whole subtree, so the name does not matter here.
	ns := filepath.Join(cacheDir, gdriveFileIDCacheSubdir, "0123456789abcdef")
	require.NoError(t, os.MkdirAll(ns, dirPermissions))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "fileids.journal"), []byte("kpfid"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "fileids.lock"), nil, 0o600))

	return filepath.Join(cacheDir, gdriveFileIDCacheSubdir)
}

func TestPurgeFileIDJournal(t *testing.T) {
	cacheDir := t.TempDir()
	journalDir := plantJournal(t, cacheDir)

	// A sibling file in the same cache directory must survive: the purge is
	// surgical, not a wipe of whatever the operator pointed cacheDir at.
	sibling := filepath.Join(cacheDir, "unrelated.txt")
	require.NoError(t, os.WriteFile(sibling, []byte("keep me"), 0o600))

	require.NoError(t, purgeFileIDJournal(cacheDir))
	require.NoDirExists(t, journalDir)
	require.FileExists(t, sibling)

	// Purging twice, and purging a target with no on-disk cache at all, are both
	// no-ops rather than errors.
	require.NoError(t, purgeFileIDJournal(cacheDir))
	require.NoError(t, purgeFileIDJournal(""))
}

// TestRecycleColdPurgesTheJournalBetweenPasses drives the harness-level cold
// pass: the storage is reopened and the journal directory is gone afterwards.
func TestRecycleColdPurgesTheJournalBetweenPasses(t *testing.T) {
	ctx := context.Background()

	storageDir := t.TempDir()
	cacheDir := t.TempDir()
	journalDir := plantJournal(t, cacheDir)

	opens := 0

	e := &workloadEnv{
		params:   smokeParams(),
		cacheDir: cacheDir,
		open: func(ctx context.Context) (blob.Storage, error) {
			opens++

			return filesystem.New(ctx, &filesystem.Options{Path: storageDir}, true) //nolint:wrapcheck // test seam.
		},
	}

	require.NoError(t, e.openNew(ctx))
	require.Equal(t, 1, opens)

	first := e.storage()

	// The warm pass keeps the journal; only the cold one deletes it.
	require.NoError(t, e.recycle(ctx))
	require.DirExists(t, journalDir)

	require.NoError(t, e.recycleCold(ctx))
	require.NoDirExists(t, journalDir)
	require.Equal(t, 3, opens)
	require.NotSame(t, first, e.storage(), "the cold pass must be measured on a fresh connection")

	require.NoError(t, e.storage().Close(ctx))
}

func TestValidateTrueColdRequiresAHarnessOwnedCacheDir(t *testing.T) {
	bare, err := parseTargets([]byte(`[{"name":"native","connection":{"type":"gdrive","config":{"folderID":"F"}}}]`), lookupMap(nil))
	require.NoError(t, err)

	err = validateTrueCold(bare)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cacheDir")

	tuned, err := parseTargets([]byte(`[{"name":"native","connection":{"type":"gdrive","config":{
		"folderID":"F","tuning":{"cacheDir":"/tmp/bench-cache"}}}}]`), lookupMap(nil))
	require.NoError(t, err)
	require.NoError(t, validateTrueCold(tuned))

	dir, isDrive := tuned[0].fileIDCacheDir()
	require.True(t, isDrive)
	require.Equal(t, "/tmp/bench-cache", dir)

	// A non-Drive target has no journal to purge and is never refused.
	fs, err := parseTargets([]byte(`[{"name":"fs","connection":{"type":"filesystem","config":{"path":"/tmp/x"}}}]`), lookupMap(nil))
	require.NoError(t, err)
	require.NoError(t, validateTrueCold(fs))

	_, isDrive = fs[0].fileIDCacheDir()
	require.False(t, isDrive)
}

func TestRunRefusesTrueColdWithoutACacheDir(t *testing.T) {
	dir := t.TempDir()
	targets := filepath.Join(dir, "targets.json")

	require.NoError(t, writeFileAtomic(targets,
		[]byte(`[{"name":"native","connection":{"type":"gdrive","config":{"folderID":"F"}}}]`)))

	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{
		"run", "--targets", targets, "--results-dir", t.TempDir(), "--smoke", "--true-cold",
	}, &out, &errOut)

	require.Equal(t, exitRefused, code)
	require.Contains(t, errOut.String(), "cacheDir")
}

// TestTrueColdAddsTheWarmPhase runs the whole suite against filesystem targets
// with --true-cold and asserts the phase set changes as documented: hot, warm
// and cold instead of hot and cold.
func TestTrueColdAddsTheWarmPhase(t *testing.T) {
	targetsPath := writeTwoFilesystemTargets(t)
	results := t.TempDir()

	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{
		"run",
		"--targets", targetsPath,
		"--results-dir", results,
		"--smoke",
		"--true-cold",
		"--workloads", "read-metadata",
		"--label", "truecold",
	}, &out, &errOut)

	require.Equal(t, exitOK, code, "stdout:\n%v\nstderr:\n%v", out.String(), errOut.String())

	res, err := readResult(findResultFile(t, results, "truecold"))
	require.NoError(t, err)
	require.True(t, res.Config.Params.TrueCold)

	for _, tr := range res.Trials {
		require.Len(t, tr.Workloads, 3)

		for _, phase := range []string{phaseHot, phaseWarm, phaseCold} {
			_, ok := tr.Workloads[phaseKey("read-metadata", phase)]
			require.True(t, ok, "missing read-metadata:%v", phase)
		}
	}
}
