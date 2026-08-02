//go:build !no_extra_providers

package main

import (
	"bytes"
	"context"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
)

// kopiaExeOnce builds the kopia binary the integration test drives, once per
// test binary. The build is a no-op when the tree is already compiled, which is
// the normal case, so it costs about a second.
var (
	kopiaExeOnce sync.Once
	kopiaExePath string
	errKopiaExe  error
)

// testKopiaExe returns a kopia binary built from this work tree.
func testKopiaExe(t *testing.T) string {
	t.Helper()

	if v := os.Getenv("KOPIA_GDRIVE_BENCH_TEST_KOPIA_EXE"); v != "" {
		return v
	}

	if testing.Short() {
		t.Skip("-short: not building the kopia binary")
	}

	kopiaExeOnce.Do(func() {
		root, ok := repoRoot(context.Background())
		if !ok {
			errKopiaExe = os.ErrNotExist

			return
		}

		dir, err := os.MkdirTemp("", "gdrive-bench-test-kopia-")
		if err != nil {
			errKopiaExe = err

			return
		}

		exe := filepath.Join(dir, "kopia")

		cmd := exec.CommandContext(context.Background(), "go", "build", "-o", exe, ".")
		cmd.Dir = root

		if out, err := cmd.CombinedOutput(); err != nil {
			errKopiaExe = err

			t.Logf("go build output: %v", string(out))

			return
		}

		kopiaExePath = exe
	})

	if errKopiaExe != nil {
		t.Skipf("unable to build the kopia binary: %v", errKopiaExe)
	}

	return kopiaExePath
}

// TestSnapshotE2EAgainstAFilesystemTarget is the credential-free integration
// test of the out-of-process workload: a real kopia binary creates a real
// repository in a temp directory, backs up a tiny synthetic tree, backs it up
// again after a mutation, verifies every file and runs full maintenance - and
// the folder is empty again afterwards.
func TestSnapshotE2EAgainstAFilesystemTarget(t *testing.T) {
	exe := testKopiaExe(t)

	dir := t.TempDir()
	results := t.TempDir()

	targets := filepath.Join(dir, "targets.json")
	storageDir := filepath.Join(dir, "storage")

	require.NoError(t, writeFileAtomic(targets,
		[]byte(`[{"name":"fs","connection":{"type":"filesystem","config":{"path":"`+storageDir+`"}}}]`)))

	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{
		"run",
		"--targets", targets,
		"--results-dir", results,
		"--workloads", snapshotE2EName,
		"--kopia-exe", exe,
		"--smoke",
		"--snapshot-files", "12",
		"--snapshot-mb", "2",
		"--label", "e2eselftest",
	}, &out, &errOut)

	require.Equal(t, exitOK, code, "stdout:\n%v\nstderr:\n%v", out.String(), errOut.String())

	res, err := readResult(findResultFile(t, results, "e2eselftest"))
	require.NoError(t, err)
	require.Len(t, res.Trials, 1)
	require.Equal(t, exe, res.Config.KopiaExe)

	wl := res.Trials[0].Workloads
	require.Len(t, wl, len(snapshotStageKeys()))

	for _, key := range snapshotStageKeys() {
		m, ok := wl[key]
		require.True(t, ok, "missing stage %v; got %v", key, wl)
		require.Equal(t, 1, m.Ops, key)
		require.Zero(t, m.Errors, key)
		require.Positive(t, m.WallClockMS, key)
		require.Equal(t, m.WallClockMS, m.Latency.P50, key)
		require.Nil(t, m.Transport, key)
		// A filesystem target moves no Drive traffic.
		require.Equal(t, quotaSourceNone, m.QuotaUnitsSource, key)
	}

	require.Positive(t, wl[phaseKey(snapshotE2EName, stageInit)].BytesUp)
	require.Positive(t, wl[phaseKey(snapshotE2EName, stageIncremental)].BytesUp)
	require.Positive(t, wl[phaseKey(snapshotE2EName, stageVerify)].BytesDown)

	// Cleanup deleted every blob the repository consisted of.
	requireStorageEmpty(t, storageDir)
}

// snapshotStageKeys is the full set of result keys one snapshot-e2e execution
// produces.
func snapshotStageKeys() []string {
	out := []string{}
	for _, s := range []string{stageCreate, stageInit, stageIncremental, stageVerify, stageMaintenance} {
		out = append(out, phaseKey(snapshotE2EName, s))
	}

	return out
}

// requireStorageEmpty asserts that a filesystem-backed storage holds no blobs.
func requireStorageEmpty(t *testing.T, path string) {
	t.Helper()

	ctx := context.Background()

	st, err := filesystem.New(ctx, &filesystem.Options{Path: path}, false)
	require.NoError(t, err)

	defer st.Close(ctx) //nolint:errcheck // test cleanup.

	var left []blob.ID

	require.NoError(t, st.ListBlobs(ctx, "", func(bm blob.Metadata) error {
		left = append(left, bm.BlobID)

		return nil
	}))

	require.Empty(t, left, "snapshot-e2e cleanup left blobs behind")
}

func TestRepositoryCreateArgs(t *testing.T) {
	root := "/tmp/bench-root"

	drive, err := parseTargets([]byte(`[{"name":"native","connection":{"type":"gdrive","config":{
		"folderID":"FOLDER","credentialsFile":"/creds.json","scope":"drive.file"}}}]`), lookupMap(nil))
	require.NoError(t, err)

	args, err := repositoryCreateArgs(drive[0], root)
	require.NoError(t, err)
	require.Equal(t, []string{
		"repository", "create",
		"--cache-directory=" + filepath.Join(root, "kopia-cache"),
		"--no-check-for-updates",
		"gdrive",
		"--folder-id=FOLDER",
		"--credentials-file=/creds.json",
		"--scope=drive.file",
	}, args)

	rc, err := parseTargets([]byte(`[{"name":"rclone","connection":{"type":"rclone","config":{
		"remotePath":"remote:path","rcloneExe":"/usr/bin/rclone","rcloneArgs":["--drive-root-folder-id","XYZ"]}}}]`), lookupMap(nil))
	require.NoError(t, err)

	args, err = repositoryCreateArgs(rc[0], root)
	require.NoError(t, err)
	require.Contains(t, args, "rclone")
	require.Contains(t, args, "--remote-path=remote:path")
	require.Contains(t, args, "--rclone-exe=/usr/bin/rclone")
	require.Contains(t, args, "--rclone-args=--drive-root-folder-id")
	require.Contains(t, args, "--rclone-args=XYZ")

	fs, err := parseTargets([]byte(`[{"name":"fs","connection":{"type":"filesystem","config":{"path":"/data"}}}]`), lookupMap(nil))
	require.NoError(t, err)

	args, err = repositoryCreateArgs(fs[0], root)
	require.NoError(t, err)
	require.Contains(t, args, "--path=/data")

	gcs, err := parseTargets([]byte(`[{"name":"gcs","connection":{"type":"gcs","config":{"bucket":"b"}}}]`), lookupMap(nil))
	require.NoError(t, err)

	_, err = repositoryCreateArgs(gcs[0], root)
	require.Error(t, err, "an unmapped provider must be reported, not silently skipped")
	require.Contains(t, err.Error(), "repositoryCreateArgs")
}

func TestGenerateTreeIsDeterministicAndApproximatelySized(t *testing.T) {
	p := smokeParams()
	p.SnapshotFiles = 30
	p.SnapshotDirs = 3
	p.SnapshotBytes = 2 * mib

	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")

	ta, err := generateTree(a, &p, newTestRand())
	require.NoError(t, err)

	tb, err := generateTree(b, &p, newTestRand())
	require.NoError(t, err)

	require.Equal(t, ta.files, tb.files)
	require.Equal(t, ta.bytes, tb.bytes)
	require.Len(t, ta.files, 30)

	// Within a factor of two of the requested total: the per-file minimum can
	// only push it up, and the rounding down of every file can only push it
	// slightly down.
	require.Greater(t, ta.bytes, p.SnapshotBytes/2)
	require.Less(t, ta.bytes, p.SnapshotBytes*2)

	// Same seed, same bytes, so the two trees are identical on disk.
	for _, rel := range ta.files {
		da, err := os.ReadFile(filepath.Join(a, rel))
		require.NoError(t, err)

		db, err := os.ReadFile(filepath.Join(b, rel))
		require.NoError(t, err)

		require.Equal(t, da, db, rel)
	}

	// The tree really is nested and spread over the configured directories.
	dirs := map[string]bool{}
	nested := 0

	for _, rel := range ta.files {
		parts := strings.Split(rel, string(filepath.Separator))
		dirs[parts[0]] = true

		if len(parts) > 2 {
			nested++
		}
	}

	require.Len(t, dirs, 3)
	require.Positive(t, nested)
}

func TestMutateTreeChangesTheTree(t *testing.T) {
	p := smokeParams()
	p.SnapshotFiles = 40
	p.SnapshotBytes = 1 * mib
	p.SnapshotMutatePercent = 10

	root := filepath.Join(t.TempDir(), "src")

	tree, err := generateTree(root, &p, newTestRand())
	require.NoError(t, err)

	before := countFiles(t, root)

	added, err := mutateTree(root, tree, &p, newTestRand())
	require.NoError(t, err)
	require.Positive(t, added)

	// Four deleted, four added: the file count is stable but the content is not.
	require.Equal(t, before, countFiles(t, root))
}

func countFiles(t *testing.T, root string) int {
	t.Helper()

	n := 0

	require.NoError(t, filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			n++
		}

		return nil
	}))

	return n
}

func newTestRand() *rand.Rand {
	return rand.New(rand.NewPCG(99, 100)) //nolint:gosec // deterministic test data.
}

func TestEstimateSnapshotE2EScalesWithTheTree(t *testing.T) {
	sp := smokeParams()
	dp := defaultParams()

	small := estimateSnapshotE2E(&sp)
	big := estimateSnapshotE2E(&dp)

	require.Positive(t, small.UploadBytes)
	require.Greater(t, big.UploadBytes, small.UploadBytes)
	require.Greater(t, big.QuotaUnits, small.QuotaUnits)
}

func TestTailOf(t *testing.T) {
	require.Equal(t, "abc", tailOf("abc", 10))
	require.Equal(t, "...\ncde", tailOf("abcde", 3))
}
