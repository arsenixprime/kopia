//go:build !no_extra_providers

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQuotaLedgerAccounting(t *testing.T) {
	dir := t.TempDir()

	l, err := loadQuotaLedger(dir)
	require.NoError(t, err)
	require.Empty(t, l.Days)

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	l.add(now, 100, 10)
	l.add(now, 50, 5)
	l.add(now.AddDate(0, 0, -1), 7, 1)

	today := l.today(now)
	require.Equal(t, "2026-08-01", today.Date)
	require.Equal(t, int64(150), today.UploadBytes)
	require.Equal(t, int64(15), today.QuotaUnits)
	require.Equal(t, 2, today.Runs)

	require.NoError(t, l.save(dir))

	reloaded, err := loadQuotaLedger(dir)
	require.NoError(t, err)
	require.Equal(t, l.Days, reloaded.Days)
	require.Equal(t, ledgerVersion, reloaded.Version)

	// The ledger is owner-only: it records where and how much a machine backs up.
	st, err := os.Stat(filepath.Join(dir, ledgerFileName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(filePermissions), st.Mode().Perm())
}

func TestQuotaLedgerPrunesOldDays(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	l := &quotaLedger{Version: ledgerVersion}

	for i := range 20 {
		l.Days = append(l.Days, quotaDay{Date: utcDay(now.AddDate(0, 0, -i)), UploadBytes: 1})
	}

	l.prune(now)

	require.Len(t, l.Days, ledgerRetentionDays+1) // the retention window plus today.
	require.Equal(t, "2026-08-03", l.Days[0].Date)
	require.Equal(t, "2026-08-10", l.Days[len(l.Days)-1].Date)
}

func TestQuotaLedgerRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeFileAtomic(filepath.Join(dir, ledgerFileName), []byte("{not json")))

	_, err := loadQuotaLedger(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "corrupt")
}

func TestCheckQuotaRefusesOverBudget(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	l := &quotaLedger{Version: ledgerVersion}

	half := dailyUploadBytesLimit / 2

	// Nothing spent yet: a run just under half the cap is allowed at fraction 0.5.
	d := checkQuota(l, now, half-1, 1000, 0.5)
	require.True(t, d.Allowed)
	require.Equal(t, half, d.BudgetBytes)
	require.Equal(t, int64(1000), d.EstimatedQU)
	require.Equal(t, perUserQUPerMinute, d.QUCeiling)

	// The same run again, after the first one was charged, must be refused.
	l.add(now, half-1, 1000)

	d = checkQuota(l, now, half-1, 1000, 0.5)
	require.False(t, d.Allowed)
	require.NotEmpty(t, d.Reason)

	// A smaller fraction refuses sooner.
	require.False(t, checkQuota(&quotaLedger{}, now, half, 0, 0.1).Allowed)

	// Yesterday's spending does not count against today.
	l2 := &quotaLedger{Version: ledgerVersion}
	l2.add(now.AddDate(0, 0, -1), dailyUploadBytesLimit, 0)
	require.True(t, checkQuota(l2, now, 1000, 0, 0.5).Allowed)
}

func TestQuotaUnitCosts(t *testing.T) {
	require.Equal(t, int64(quListItems), quotaUnitCost("files.list"))
	require.Equal(t, int64(quReadItem), quotaUnitCost("files.get"))
	require.Equal(t, int64(quDownloadItem), quotaUnitCost("files.get.media"))
	require.Equal(t, int64(quEditItem), quotaUnitCost("files.create"))
	require.Equal(t, int64(quEditItem), quotaUnitCost("upload.resumable"))
	require.Equal(t, int64(0), quotaUnitCost("batch"))
	require.Equal(t, int64(quOtherAction), quotaUnitCost("something.else"))
}

func TestQuotaWriteEstimateFollowsUploadProtocol(t *testing.T) {
	// Below the simple-upload cutoff: one name lookup plus one edit.
	require.Equal(t, int64(quListItems+quEditItem), quWrite(1024))

	// Above it: one lookup plus a session and one chunk.
	require.Equal(t, int64(quListItems+2*quEditItem), quWrite(20*mib))

	// Three chunks plus the session for a 150 MB blob at the 64 MB default.
	require.Equal(t, int64(quListItems+4*quEditItem), quWrite(150*mib))
}

func TestRunRefusesWhenLedgerIsExhausted(t *testing.T) {
	dir := t.TempDir()
	targetsPath := filepath.Join(dir, "targets.json")

	// A gdrive target is never connected to: the quota guard runs first and
	// refuses, which is the whole point of the guard.
	require.NoError(t, writeFileAtomic(targetsPath, []byte(
		`[{"name":"native","connection":{"type":"gdrive","config":{"folderID":"none","tuning":{}}}}]`,
	)))

	results := filepath.Join(dir, "results")
	require.NoError(t, os.MkdirAll(results, dirPermissions))

	l := &quotaLedger{Version: ledgerVersion}
	l.add(nowUTC(), dailyUploadBytesLimit, 0)
	require.NoError(t, l.save(results))

	var out, errOut bytes.Buffer

	code := dispatch(context.Background(), []string{
		"run", "--targets", targetsPath, "--results-dir", results, "--trials", "1",
	}, &out, &errOut)

	require.Equal(t, exitRefused, code)
	require.Contains(t, errOut.String(), "refusing to start")

	// The refusal must not have charged anything.
	reloaded, err := loadQuotaLedger(results)
	require.NoError(t, err)
	require.Equal(t, dailyUploadBytesLimit, reloaded.today(nowUTC()).UploadBytes)
}

func TestEstimateRunCountsOnlyRemoteTargets(t *testing.T) {
	fsTarget, err := parseTargets([]byte(`[{"name":"fs","connection":{"type":"filesystem","config":{"path":"/tmp/x"}}}]`), lookupMap(nil))
	require.NoError(t, err)

	driveTarget, err := parseTargets([]byte(`[{"name":"n","connection":{"type":"gdrive","config":{"folderID":"f"}}}]`), lookupMap(nil))
	require.NoError(t, err)

	p := smokeParams()
	wls := allWorkloads()

	require.Equal(t, workloadEstimate{}, estimateRun(wls, &p, fsTarget, 3))

	remote := estimateRun(wls, &p, driveTarget, 3)
	require.Positive(t, remote.UploadBytes)
	require.Positive(t, remote.QuotaUnits)

	// The estimate scales with the number of trials.
	one := estimateRun(wls, &p, driveTarget, 1)
	require.Equal(t, one.UploadBytes*3, remote.UploadBytes)
}

func TestWriteFileAtomicLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "file.json")

	require.NoError(t, writeFileAtomic(path, []byte(`{"a":1}`)))
	require.NoError(t, writeFileAtomic(path, []byte(`{"a":2}`)))

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	m := map[string]int{}
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, 2, m["a"])
}
