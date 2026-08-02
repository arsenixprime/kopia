//go:build !no_extra_providers

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo/blob/gdrive"
)

// Quota-unit costs, from RECON.md §C.2 (Google's quota-unit model, effective
// 2026-05-01). Reads cost 5, lists 100, downloads 200, edits 50, everything
// else 5.
//
// UNVERIFIED, and recorded as such in RECON.md: Google's cost table names
// files.update but neither files.create nor files.delete, and says nothing
// about the two upload protocols. The harness assumes every mutation and every
// upload request is edit-class (50) - which makes the resumable chunk size a
// quota knob, not just an RTT knob. If that assumption is ever falsified, only
// these constants change.
const (
	quReadItem     = 5
	quListItems    = 100
	quDownloadItem = 200
	quEditItem     = 50
	quOtherAction  = 5
)

// quotaUnitCost returns the quota-unit cost of one call of a Drive method,
// keyed by the method keys the backend's instrumented transport records.
func quotaUnitCost(method string) int64 {
	switch method {
	case "files.list":
		return quListItems
	case "files.get":
		return quReadItem
	case "files.get.media":
		return quDownloadItem
	case "files.create", "files.update", "files.delete", "upload.simple", "upload.resumable":
		return quEditItem
	case "batch":
		// The batch envelope itself is free; every sub-request inside it is
		// charged at its own method's cost and is invisible to the transport
		// (sub-requests are body parts). Batched runs therefore UNDER-report
		// quota units, which is why the default delete path is parallel singles.
		return 0
	default:
		return quOtherAction
	}
}

// Where a WorkloadMetrics.quota_units figure came from.
const (
	quotaSourceObserved = "observed" // counted from the Drive API calls the transport saw.
	quotaSourceNone     = "none"     // the backend is not Drive-instrumented, so it spends no Drive quota.

	// quotaSourceEstimated is what the out-of-process workload reports: the
	// Drive traffic happens in a child process, where this harness's transport
	// counters cannot see it, so the figure is derived from the bytes moved.
	quotaSourceEstimated = "estimated"
)

// quotaUnitsOfSnapshot totals the quota units of a transport delta.
func quotaUnitsOfSnapshot(s gdrive.TransportStatsSnapshot) int64 {
	var total int64

	for k, m := range s.PerMethod {
		total += m.Calls * quotaUnitCost(k)
	}

	return total
}

// Google's published ceilings, from RECON.md §C.2 and §C.3.
const (
	// dailyUploadBytesLimit is the documented 750 GB/day/user upload cap. The
	// decimal interpretation is used because it is the smaller, and therefore
	// the safer, reading of "750 GB".
	dailyUploadBytesLimit int64 = 750 * 1000 * 1000 * 1000

	// perUserQUPerMinute is the 325,000 quota units per minute per user per
	// project ceiling. It is a RATE, not a daily budget, so the harness only
	// reports against it - see the note in SCHEMA.md.
	perUserQUPerMinute int64 = 325_000

	// ledgerRetentionDays is how much history the quota ledger keeps.
	ledgerRetentionDays = 7

	// ledgerVersion is the ledger file's own schema version.
	ledgerVersion = 1

	// ledgerFileName is the ledger's name inside the results directory.
	ledgerFileName = "quota-ledger.json"
)

// quotaLedger is the persistent record of what the harness has already spent,
// per UTC day. It exists so that a series of individually harmless runs cannot
// add up to a day's upload allowance.
type quotaLedger struct {
	Version int        `json:"version"`
	Days    []quotaDay `json:"days"`
}

// quotaDay is one UTC day of the ledger.
type quotaDay struct {
	Date        string `json:"date"`
	UploadBytes int64  `json:"upload_bytes"`
	QuotaUnits  int64  `json:"quota_units"`
	Runs        int    `json:"runs"`
}

// utcDay formats a time as the ledger's day key.
func utcDay(t time.Time) string {
	return t.UTC().Format(time.DateOnly)
}

// loadQuotaLedger reads the ledger from a results directory. A missing file is
// an empty ledger, not an error; a corrupt one IS an error, because silently
// resetting the accounting is exactly the failure the ledger exists to prevent.
func loadQuotaLedger(dir string) (*quotaLedger, error) {
	path := filepath.Join(dir, ledgerFileName)

	//nolint:gosec // the path is derived from the operator's own results directory.
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &quotaLedger{Version: ledgerVersion}, nil
	}

	if err != nil {
		return nil, errors.Wrapf(err, "unable to read the quota ledger %v", path)
	}

	l := &quotaLedger{}
	if err := json.Unmarshal(data, l); err != nil {
		return nil, errors.Wrapf(err, "the quota ledger %v is corrupt; inspect it and fix or remove it by hand", path)
	}

	if l.Version == 0 {
		l.Version = ledgerVersion
	}

	return l, nil
}

// save writes the ledger back atomically, owner-readable only.
func (l *quotaLedger) save(dir string) error {
	l.Version = ledgerVersion

	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return errors.Wrap(err, "unable to encode the quota ledger")
	}

	return writeFileAtomic(filepath.Join(dir, ledgerFileName), append(data, '\n'))
}

// prune drops days older than ledgerRetentionDays and keeps the rest sorted.
func (l *quotaLedger) prune(now time.Time) {
	cutoff := utcDay(now.UTC().AddDate(0, 0, -ledgerRetentionDays))

	kept := l.Days[:0]

	for _, d := range l.Days {
		if d.Date >= cutoff {
			kept = append(kept, d)
		}
	}

	l.Days = kept

	sort.Slice(l.Days, func(i, j int) bool { return l.Days[i].Date < l.Days[j].Date })
}

// today returns the accumulated totals for the UTC day of now.
func (l *quotaLedger) today(now time.Time) quotaDay {
	day := utcDay(now)

	for _, d := range l.Days {
		if d.Date == day {
			return d
		}
	}

	return quotaDay{Date: day}
}

// add records a run's actual consumption against the UTC day of now.
func (l *quotaLedger) add(now time.Time, uploadBytes, quotaUnits int64) {
	day := utcDay(now)

	for i := range l.Days {
		if l.Days[i].Date == day {
			l.Days[i].UploadBytes += uploadBytes
			l.Days[i].QuotaUnits += quotaUnits
			l.Days[i].Runs++

			return
		}
	}

	l.Days = append(l.Days, quotaDay{Date: day, UploadBytes: uploadBytes, QuotaUnits: quotaUnits, Runs: 1})

	l.prune(now)
}

// quotaDecision is the outcome of the pre-run guard.
type quotaDecision struct {
	Allowed        bool    `json:"allowed"`
	Fraction       float64 `json:"daily_upload_fraction"`
	BudgetBytes    int64   `json:"budget_bytes"`
	AlreadyBytes   int64   `json:"already_uploaded_bytes_today"`
	EstimatedBytes int64   `json:"estimated_upload_bytes"`
	EstimatedQU    int64   `json:"estimated_quota_units"`
	QUCeiling      int64   `json:"quota_units_per_minute_ceiling"`
	Reason         string  `json:"reason,omitempty"`
}

// checkQuota applies the upload-byte guard and prepares the informational
// quota-unit line. The byte cap is enforced for real; the quota-unit ceiling is
// per MINUTE and cannot be enforced from a daily ledger, so it is only
// reported - see SCHEMA.md.
func checkQuota(l *quotaLedger, now time.Time, estBytes, estQU int64, fraction float64) quotaDecision {
	budget := int64(float64(dailyUploadBytesLimit) * fraction)
	already := l.today(now).UploadBytes

	d := quotaDecision{
		Allowed:        true,
		Fraction:       fraction,
		BudgetBytes:    budget,
		AlreadyBytes:   already,
		EstimatedBytes: estBytes,
		EstimatedQU:    estQU,
		QUCeiling:      perUserQUPerMinute,
	}

	if already+estBytes > budget {
		d.Allowed = false
		d.Reason = "the estimated upload would take today's total past the configured fraction of Google's 750 GB/day/user cap"
	}

	return d
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, so a crashed or concurrent run can never leave a half-written
// ledger or result behind.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, dirPermissions); err != nil {
		return errors.Wrapf(err, "unable to create %v", dir)
	}

	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return errors.Wrapf(err, "unable to create a temporary file in %v", dir)
	}

	tmp := f.Name()

	defer os.Remove(tmp) //nolint:errcheck // best-effort cleanup of an already-renamed or already-failed file.

	if err := f.Chmod(filePermissions); err != nil {
		f.Close() //nolint:errcheck // the write has already failed.

		return errors.Wrapf(err, "unable to set the permissions of %v", tmp)
	}

	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck // the write has already failed.

		return errors.Wrapf(err, "unable to write %v", tmp)
	}

	if err := f.Close(); err != nil {
		return errors.Wrapf(err, "unable to close %v", tmp)
	}

	if err := os.Rename(tmp, path); err != nil {
		return errors.Wrapf(err, "unable to rename %v to %v", tmp, path)
	}

	return nil
}

// File modes used for everything the harness writes. Results can name folder
// IDs and machine paths, so they are owner-only.
const (
	filePermissions = 0o600
	dirPermissions  = 0o700
)

// nowUTC is the harness's clock. The repository forbids time.Now outside
// internal/clock, and routing every timestamp through one function keeps the
// result documents consistently UTC.
func nowUTC() time.Time {
	return clock.Now().UTC()
}
