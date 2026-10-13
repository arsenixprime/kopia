//go:build !no_extra_providers

package gdriveerr

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo/blob"
)

const (
	testOp     = "PutBlob"
	testBlobID = "p0123456789abcdef"
)

// gerr builds a *googleapi.Error the way the Drive client would.
func gerr(code int, reason, message string) *googleapi.Error {
	e := &googleapi.Error{
		Code:    code,
		Message: message,
	}

	if reason != "" {
		e.Errors = []googleapi.ErrorItem{{Reason: reason, Message: message}}
	}

	return e
}

type classifyCase struct {
	name        string
	code        int
	reason      string
	message     string
	disposition Disposition
	sentinel    error
}

//nolint:maintidx
func classifyCases() []classifyCase {
	return []classifyCase{
		// ---- 400 Bad Request ----
		{name: "400/badRequest", code: 400, reason: "badRequest", disposition: NonRetryableOther},
		{name: "400/illegalKeepForeverModification", code: 400, reason: "illegalKeepForeverModification", disposition: NonRetryableOther},
		{name: "400/invalidSharingRequest", code: 400, reason: "invalidSharingRequest", disposition: NonRetryableOther},
		{name: "400/unknown", code: 400, reason: "someBrandNewReason", disposition: NonRetryableOther},
		{name: "400/no-reason", code: 400, disposition: NonRetryableOther},

		// ---- 401 Unauthorized ----
		{name: "401/authError", code: 401, reason: "authError", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "401/invalid_grant", code: 401, reason: "invalid_grant", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "401/unauthorized", code: 401, reason: "unauthorized", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "401/no-reason", code: 401, disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "401/unknown-reason", code: 401, reason: "whatIsThis", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		// documented under 401 but the doc's own sample carries code 403.
		{name: "401/fileNotDownloadable", code: 401, reason: "fileNotDownloadable", disposition: NonRetryableOther},
		{name: "403/fileNotDownloadable", code: 403, reason: "fileNotDownloadable", disposition: NonRetryableOther},

		// ---- 403 Forbidden ----
		{name: "403/accessNotConfigured", code: 403, reason: "accessNotConfigured", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "403/activeItemCreationLimitExceeded", code: 403, reason: "activeItemCreationLimitExceeded", disposition: FatalQuota, sentinel: ErrStorageQuotaExhausted},
		{name: "403/appNotAuthorizedToFile", code: 403, reason: "appNotAuthorizedToFile", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "403/cannotDownloadAbusiveFile", code: 403, reason: "cannotDownloadAbusiveFile", disposition: NonRetryableOther},
		{name: "403/cannotModifyInheritedTeamDrivePermission", code: 403, reason: "cannotModifyInheritedTeamDrivePermission", disposition: NonRetryableOther},
		{name: "403/dailyLimitExceeded", code: 403, reason: "dailyLimitExceeded", disposition: FatalQuota, sentinel: ErrStorageQuotaExhausted},
		{name: "403/domainPolicy", code: 403, reason: "domainPolicy", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "403/downloadQuotaExceeded", code: 403, reason: "downloadQuotaExceeded", disposition: FatalQuota, sentinel: ErrStorageQuotaExhausted},
		{name: "403/download_restricted_for_revision", code: 403, reason: "download_restricted_for_revision", disposition: NonRetryableOther},
		{name: "403/downloadRestrictedForRevision-camel", code: 403, reason: "downloadRestrictedForRevision", disposition: NonRetryableOther},
		{name: "403/fileNotExportable", code: 403, reason: "fileNotExportable", disposition: NonRetryableOther},
		{name: "403/fileOwnerNotMemberOfTeamDrive", code: 403, reason: "fileOwnerNotMemberOfTeamDrive", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "403/fileWriterTeamDriveMoveInDisabled", code: 403, reason: "fileWriterTeamDriveMoveInDisabled", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "403/insufficientFilePermissions", code: 403, reason: "insufficientFilePermissions", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "403/insufficientParentPermissions", code: 403, reason: "insufficientParentPermissions", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "403/myDriveHierarchyDepthLimitExceeded", code: 403, reason: "myDriveHierarchyDepthLimitExceeded", disposition: NonRetryableOther},
		{name: "403/numChildrenInNonRootLimitExceeded", code: 403, reason: "numChildrenInNonRootLimitExceeded", disposition: FatalQuota, sentinel: ErrStorageQuotaExhausted},
		{name: "403/quotaExceeded", code: 403, reason: "quotaExceeded", disposition: FatalQuota, sentinel: ErrStorageQuotaExhausted},
		{name: "403/rateLimitExceeded", code: 403, reason: "rateLimitExceeded", disposition: RetryableBackoff},
		{name: "403/sharingRateLimitExceeded", code: 403, reason: "sharingRateLimitExceeded", disposition: RetryableBackoff},
		{name: "403/storageQuotaExceeded", code: 403, reason: "storageQuotaExceeded", disposition: FatalQuota, sentinel: ErrStorageQuotaExhausted},
		{name: "403/teamDriveFileLimitExceeded", code: 403, reason: "teamDriveFileLimitExceeded", disposition: FatalQuota, sentinel: ErrStorageQuotaExhausted},
		{name: "403/teamDriveHierarchyTooDeep", code: 403, reason: "teamDriveHierarchyTooDeep", disposition: NonRetryableOther},
		{name: "403/teamDriveMembershipRequired", code: 403, reason: "teamDriveMembershipRequired", disposition: FatalAuth, sentinel: blob.ErrInvalidCredentials},
		{name: "403/teamDrivesFolderMoveInNotSupported", code: 403, reason: "teamDrivesFolderMoveInNotSupported", disposition: NonRetryableOther},
		{name: "403/teamDrivesParentLimit", code: 403, reason: "teamDrivesParentLimit", disposition: NonRetryableOther},
		{name: "403/UrlLeaseLimitExceeded", code: 403, reason: "UrlLeaseLimitExceeded", disposition: RetryableBackoff},
		{name: "403/userRateLimitExceeded", code: 403, reason: "userRateLimitExceeded", disposition: RetryableBackoff},
		{name: "403/unknown", code: 403, reason: "brandNewForbiddenReason", disposition: NonRetryableOther},
		{name: "403/no-reason", code: 403, disposition: NonRetryableOther},

		// ---- 404 Not Found ----
		{name: "404/notFound", code: 404, reason: "notFound", disposition: FatalNotFound, sentinel: blob.ErrBlobNotFound},
		{name: "404/fileNotFound", code: 404, reason: "fileNotFound", disposition: FatalNotFound, sentinel: blob.ErrBlobNotFound},
		{name: "404/no-reason", code: 404, disposition: FatalNotFound, sentinel: blob.ErrBlobNotFound},
		{name: "404/unknown-reason", code: 404, reason: "somethingElse", disposition: FatalNotFound, sentinel: blob.ErrBlobNotFound},

		// ---- 408 / 412 / 416 ----
		{name: "408/timeout", code: 408, disposition: RetryableResume},
		{name: "412/precondition", code: 412, disposition: FatalPrecondition, sentinel: blob.ErrBlobAlreadyExists},
		{name: "412/conditionNotMet", code: 412, reason: "conditionNotMet", disposition: FatalPrecondition, sentinel: blob.ErrBlobAlreadyExists},
		{name: "416/invalid-range", code: 416, disposition: FatalInvalidRange, sentinel: blob.ErrInvalidRange},

		// ---- 429 Too Many Requests: every reason backs off ----
		{name: "429/rateLimitExceeded", code: 429, reason: "rateLimitExceeded", disposition: RetryableBackoff},
		{name: "429/userRateLimitExceeded", code: 429, reason: "userRateLimitExceeded", disposition: RetryableBackoff},
		{name: "429/no-reason", code: 429, disposition: RetryableBackoff},
		{name: "429/unknown-reason", code: 429, reason: "brandNewThrottleReason", disposition: RetryableBackoff},
		// status wins over a reason that would otherwise be fatal.
		{name: "429/storageQuotaExceeded", code: 429, reason: "storageQuotaExceeded", disposition: RetryableBackoff},

		// ---- 5xx ----
		{name: "500/backendError", code: 500, reason: "backendError", disposition: RetryableResume},
		{name: "500/internalError", code: 500, reason: "internalError", disposition: RetryableResume},
		{name: "500/no-reason", code: 500, disposition: RetryableResume},
		{name: "502/no-reason", code: 502, disposition: RetryableResume},
		{name: "503/backendError", code: 503, reason: "backendError", disposition: RetryableResume},
		{name: "504/no-reason", code: 504, disposition: RetryableResume},
	}
}

// allSentinels is every sentinel this package can unwrap to; used to assert
// that retryable errors match none of them.
//
//nolint:gochecknoglobals
var allSentinels = []error{
	blob.ErrBlobNotFound,
	blob.ErrBlobAlreadyExists,
	blob.ErrInvalidRange,
	blob.ErrInvalidCredentials,
	ErrStorageQuotaExhausted,
}

func TestClassify(t *testing.T) {
	for _, tc := range classifyCases() {
		t.Run(tc.name, func(t *testing.T) {
			original := gerr(tc.code, tc.reason, tc.message)

			wrappers := map[string]error{
				"bare":     original,
				"wrapped":  errors.Wrap(original, "drive call failed"),
				"urlError": &url.Error{Op: "Post", URL: "https://www.googleapis.com/drive/v3/files", Err: original},
				"wrapped-urlError": errors.Wrap(
					&url.Error{Op: "Post", URL: "https://www.googleapis.com/drive/v3/files", Err: original},
					"outer",
				),
			}

			for wname, in := range wrappers {
				t.Run(wname, func(t *testing.T) {
					out := Classify(in, testOp, testBlobID)
					require.Error(t, out)

					de, ok := AsDriveError(out)
					require.True(t, ok, "result is not a *DriveError: %v", out)

					require.Equal(t, tc.disposition, de.Disposition(),
						"unexpected disposition for %v/%v: %v", tc.code, tc.reason, out)
					require.Equal(t, tc.code, de.HTTPStatus())
					require.Equal(t, tc.reason, de.Reason())
					require.Equal(t, testOp, de.Op())
					require.Equal(t, testBlobID, de.BlobID())

					d, known := DispositionOf(out)
					require.True(t, known)
					require.Equal(t, tc.disposition, d)

					// errors.As must still recover the original API error.
					var recovered *googleapi.Error

					require.ErrorAs(t, out, &recovered)
					require.Same(t, original, recovered)

					// sentinel mapping, positive and negative.
					for _, s := range allSentinels {
						if tc.sentinel != nil && errors.Is(s, tc.sentinel) {
							require.ErrorIs(t, out, s)
						} else {
							require.NotErrorIs(t, out, s)
						}
					}

					// the reason string is never lost.
					if tc.reason != "" {
						require.Contains(t, out.Error(), tc.reason)
					}

					// op and blob ID are always present.
					require.Contains(t, out.Error(), testOp)
					require.Contains(t, out.Error(), testBlobID)
				})
			}
		})
	}
}

func TestClassifyNilReturnsNil(t *testing.T) {
	require.NoError(t, Classify(nil, testOp, testBlobID))
}

func TestClassifyIsIdempotent(t *testing.T) {
	first := Classify(gerr(403, "storageQuotaExceeded", "quota"), testOp, testBlobID)
	require.Equal(t, first, Classify(first, "OtherOp", "otherblob"))

	// an already-classified error nested deeper in the chain is also left alone.
	wrapped := errors.Wrap(first, "outer")
	require.Equal(t, wrapped, Classify(wrapped, "OtherOp", "otherblob"))
}

func TestContextErrorsPassThroughUnchanged(t *testing.T) {
	cases := map[string]struct {
		in       error
		sentinel error
	}{
		"canceled":                 {in: context.Canceled, sentinel: context.Canceled},
		"deadline":                 {in: context.DeadlineExceeded, sentinel: context.DeadlineExceeded},
		"wrapped-canceled":         {in: errors.Wrap(context.Canceled, "doing thing"), sentinel: context.Canceled},
		"wrapped-deadline":         {in: errors.Wrap(context.DeadlineExceeded, "doing thing"), sentinel: context.DeadlineExceeded},
		"urlerror-canceled":        {in: &url.Error{Op: "Get", URL: "https://example.com", Err: context.Canceled}, sentinel: context.Canceled},
		"urlerror-deadline":        {in: &url.Error{Op: "Get", URL: "https://example.com", Err: context.DeadlineExceeded}, sentinel: context.DeadlineExceeded},
		"wrapped-urlerror-cancel":  {in: errors.Wrap(&url.Error{Op: "Get", URL: "https://example.com", Err: context.Canceled}, "outer"), sentinel: context.Canceled},
		"googleapi-wraps-canceled": {in: wrapGoogleAPI(gerr(500, "backendError", "boom"), context.Canceled), sentinel: context.Canceled},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out := Classify(tc.in, testOp, testBlobID)

			// same error value, unchanged.
			require.Equal(t, tc.in, out)
			require.ErrorIs(t, out, tc.sentinel)
			require.ErrorIs(t, tc.in, tc.sentinel)

			// a canceled request must never look retryable.
			_, ok := AsDriveError(out)
			require.False(t, ok, "context error was classified: %v", out)
		})
	}
}

// wrapGoogleAPI returns a *googleapi.Error that wraps cause, which is how the
// generated clients report a transport error that also produced a response.
func wrapGoogleAPI(ae *googleapi.Error, cause error) *googleapi.Error {
	ae.Wrap(cause)

	return ae
}

func TestNetworkErrorsAreRetryableResume(t *testing.T) {
	cases := map[string]error{
		"unexpected-eof": io.ErrUnexpectedEOF,
		"eof":            io.EOF,
		"conn-reset":     syscall.ECONNRESET,
		"broken-pipe":    syscall.EPIPE,
		"op-error":       &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		"dns-error":      &net.DNSError{Err: "no such host", Name: "www.googleapis.com", IsNotFound: true},
		"url-error":      &url.Error{Op: "Post", URL: "https://www.googleapis.com/", Err: io.ErrUnexpectedEOF},
		"wrapped":        errors.Wrap(io.ErrUnexpectedEOF, "reading response"),
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := Classify(in, testOp, testBlobID)

			de, ok := AsDriveError(out)
			require.True(t, ok)
			require.Equal(t, RetryableResume, de.Disposition())
			require.Equal(t, 0, de.HTTPStatus())
			require.Contains(t, out.Error(), testOp)
			require.Contains(t, out.Error(), testBlobID)
			require.ErrorIs(t, out, in)
		})
	}
}

func TestNonGoogleErrorGetsSensibleDefault(t *testing.T) {
	in := errors.New("something entirely unexpected happened")

	out := Classify(in, "GetMetadata", "xdeadbeef")

	de, ok := AsDriveError(out)
	require.True(t, ok)
	require.Equal(t, RetryableResume, de.Disposition(), "unclassified errors keep Kopia's retry-everything default")
	require.Equal(t, 0, de.HTTPStatus())
	require.Empty(t, de.Reason())

	// message preserved, along with op and blob ID.
	require.Contains(t, out.Error(), "something entirely unexpected happened")
	require.Contains(t, out.Error(), "GetMetadata")
	require.Contains(t, out.Error(), "xdeadbeef")
	require.ErrorIs(t, out, in)

	for _, s := range allSentinels {
		require.NotErrorIs(t, out, s)
	}
}

func TestOAuthRetrieveErrorIsFatalAuth(t *testing.T) {
	cases := map[string]*oauth2.RetrieveError{
		"invalid_grant": {
			Response:  &http.Response{StatusCode: http.StatusBadRequest},
			Body:      []byte(`{"error":"invalid_grant"}`),
			ErrorCode: "invalid_grant",
		},
		"unauthorized_client": {
			Response:  &http.Response{StatusCode: http.StatusUnauthorized},
			Body:      []byte(`{"error":"unauthorized_client"}`),
			ErrorCode: "unauthorized_client",
		},
		"no-response": {
			Body:      []byte(`oauth2: cannot fetch token`),
			ErrorCode: "",
		},
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := Classify(in, testOp, testBlobID)

			de, ok := AsDriveError(out)
			require.True(t, ok)
			require.Equal(t, FatalAuth, de.Disposition())
			require.ErrorIs(t, out, blob.ErrInvalidCredentials)
			require.Contains(t, out.Error(), testOp)
			require.Contains(t, out.Error(), testBlobID)

			var recovered *oauth2.RetrieveError

			require.ErrorAs(t, out, &recovered)
			require.Same(t, in, recovered)
		})
	}
}

func TestOAuthRetrieveErrorRateLimitStaysRetryable(t *testing.T) {
	in := &oauth2.RetrieveError{
		Response:  &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"7"}}},
		Body:      []byte(`{"error":"rate_limit_exceeded"}`),
		ErrorCode: "rateLimitExceeded",
	}

	de, ok := AsDriveError(Classify(in, testOp, testBlobID))
	require.True(t, ok)
	require.Equal(t, RetryableBackoff, de.Disposition())
	require.Equal(t, 7*time.Second, de.RetryAfter())
}

func TestRetryAfterParsing(t *testing.T) {
	const httpDateDelay = 90 * time.Second

	future := clock.Now().Add(httpDateDelay).UTC().Format(http.TimeFormat)

	cases := []struct {
		name    string
		code    int
		reason  string
		header  string
		wantMin time.Duration
		wantMax time.Duration
	}{
		{name: "429/delta-seconds", code: 429, reason: "rateLimitExceeded", header: "120", wantMin: 120 * time.Second, wantMax: 120 * time.Second},
		{name: "503/delta-seconds", code: 503, reason: "backendError", header: "120", wantMin: 120 * time.Second, wantMax: 120 * time.Second},
		{name: "429/http-date", code: 429, reason: "rateLimitExceeded", header: future, wantMin: httpDateDelay - 5*time.Second, wantMax: httpDateDelay},
		{name: "503/http-date", code: 503, reason: "backendError", header: future, wantMin: httpDateDelay - 5*time.Second, wantMax: httpDateDelay},
		{name: "past-http-date", code: 429, reason: "rateLimitExceeded", header: "Mon, 02 Jan 2006 15:04:05 GMT"},
		{name: "unparseable", code: 429, reason: "rateLimitExceeded", header: "soonish"},
		{name: "zero", code: 429, reason: "rateLimitExceeded", header: "0"},
		{name: "negative", code: 429, reason: "rateLimitExceeded", header: "-5"},
		{name: "absent", code: 429, reason: "rateLimitExceeded"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ae := gerr(tc.code, tc.reason, "throttled")
			if tc.header != "" {
				ae.Header = http.Header{"Retry-After": []string{tc.header}}
			}

			out := Classify(ae, testOp, testBlobID)

			de, ok := AsDriveError(out)
			require.True(t, ok)

			require.GreaterOrEqual(t, de.RetryAfter(), tc.wantMin)
			require.LessOrEqual(t, de.RetryAfter(), tc.wantMax)

			// RetryDelay honors Retry-After verbatim when present.
			if tc.wantMax > 0 {
				require.Equal(t, de.RetryAfter(), RetryDelay(out, 0))
				require.Contains(t, out.Error(), "retry after")
			}
		})
	}
}

func TestRetryDelayBackoff(t *testing.T) {
	err := Classify(gerr(429, "rateLimitExceeded", "slow down"), testOp, testBlobID)

	for attempt := range 12 {
		want := min(BackoffBase<<uint(min(attempt, maxBackoffShift)), BackoffCap)

		for range 50 {
			d := RetryDelay(err, attempt)
			require.GreaterOrEqual(t, d, time.Duration(0))
			require.Less(t, d, want+time.Nanosecond)
		}
	}

	// negative attempts are tolerated.
	require.GreaterOrEqual(t, RetryDelay(err, -3), time.Duration(0))

	// non-DriveError input still produces a backoff.
	require.GreaterOrEqual(t, RetryDelay(errors.New("nope"), 3), time.Duration(0))
	require.LessOrEqual(t, RetryDelay(errors.New("nope"), 3), BackoffCap)
}

func TestRetryDelayHonorsSharingMinimum(t *testing.T) {
	err := Classify(gerr(403, "sharingRateLimitExceeded", "slow down"), testOp, testBlobID)

	for range 50 {
		require.GreaterOrEqual(t, RetryDelay(err, 0), sharingRateLimitDelay)
	}
}

func TestIsPossiblyDailyUploadCap(t *testing.T) {
	yes := []error{
		Classify(gerr(403, "userRateLimitExceeded", "User Rate Limit Exceeded"), testOp, testBlobID),
		Classify(gerr(429, "userRateLimitExceeded", "User Rate Limit Exceeded"), testOp, testBlobID),
		// rclone's heuristic: the exact message on any rate-limit reason.
		Classify(gerr(403, "rateLimitExceeded", "User rate limit exceeded."), testOp, testBlobID),
	}

	for i, e := range yes {
		require.True(t, IsPossiblyDailyUploadCap(e), "case %v: %v", i, e)
		require.True(t, IsPossiblyDailyUploadCap(errors.Wrap(e, "outer")))
	}

	no := []error{
		Classify(gerr(403, "rateLimitExceeded", "Rate Limit Exceeded"), testOp, testBlobID),
		Classify(gerr(403, "storageQuotaExceeded", "full"), testOp, testBlobID),
		Classify(gerr(500, "backendError", "boom"), testOp, testBlobID),
		errors.New("unrelated"),
		nil,
	}

	for i, e := range no {
		require.False(t, IsPossiblyDailyUploadCap(e), "case %v: %v", i, e)
	}
}

func TestMessagesAreActionable(t *testing.T) {
	cases := []struct {
		reason   string
		code     int
		contains []string
	}{
		{
			reason:   "storageQuotaExceeded",
			code:     403,
			contains: []string{"service account", "15 GB", "domain-wide delegation", "Shared Drive", "OAuth"},
		},
		{
			reason:   "domainPolicy",
			code:     403,
			contains: []string{"Workspace admin", "client ID"},
		},
		{
			reason:   "appNotAuthorizedToFile",
			code:     403,
			contains: []string{"drive.file", "created", "scope", "consent"},
		},
		{
			reason:   "insufficientFilePermissions",
			code:     403,
			contains: []string{"drive.file", "scope", "consent"},
		},
		{
			reason:   "dailyLimitExceeded",
			code:     403,
			contains: []string{"daily", "quota", "Cloud project", "dedicated OAuth client"},
		},
		{
			reason:   "activeItemCreationLimitExceeded",
			code:     403,
			contains: []string{"500 million", "trash", "permanently delete"},
		},
		{
			reason:   "numChildrenInNonRootLimitExceeded",
			code:     403,
			contains: []string{"500,000", "trash", "permanently delete"},
		},
		{
			reason:   "teamDriveFileLimitExceeded",
			code:     403,
			contains: []string{"500,000", "Shared Drive", "trash", "relocate"},
		},
		{
			reason:   "userRateLimitExceeded",
			code:     403,
			contains: []string{"750 GB/day", "24 hours"},
		},
		{
			reason:   "notFound",
			code:     404,
			contains: []string{"drive.file", "supportsAllDrives"},
		},
		{
			reason:   "accessNotConfigured",
			code:     403,
			contains: []string{"Google Cloud console", "Drive API", "OAuth client"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			msg := Classify(gerr(tc.code, tc.reason, "m"), testOp, testBlobID).Error()

			for _, want := range tc.contains {
				require.Contains(t, msg, want)
			}

			require.Contains(t, msg, testOp)
			require.Contains(t, msg, testBlobID)
		})
	}
}

func TestEveryTableEntryHasActionableMessage(t *testing.T) {
	for reason, r := range reasonRules {
		require.NotEmpty(t, r.summary, "reason %q has no summary", reason)
		require.NotEmpty(t, r.remediation, "reason %q has no remediation", reason)
	}
}

func TestAccessNotConfiguredSurfacesErrorInfo(t *testing.T) {
	const activationURL = "https://console.developers.google.com/apis/api/drive.googleapis.com/overview?project=123456789"

	ae := gerr(403, "accessNotConfigured", "Google Drive API has not been used in project 123456789 before or it is disabled.")
	ae.Details = []any{
		map[string]any{
			"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
			"reason": "SERVICE_DISABLED",
			"domain": "googleapis.com",
			"metadata": map[string]any{
				"activationUrl": activationURL,
				"consumer":      "projects/123456789",
				"service":       "drive.googleapis.com",
			},
		},
	}

	out := Classify(ae, testOp, testBlobID)

	de, ok := AsDriveError(out)
	require.True(t, ok)
	require.Equal(t, FatalAuth, de.Disposition())
	require.Equal(t, "SERVICE_DISABLED", de.ErrorInfoReason())
	require.Contains(t, out.Error(), activationURL)
	require.ErrorIs(t, out, blob.ErrInvalidCredentials)

	require.Contains(t, de.Fields(), "errorInfoReason")
}

func TestErrorInfoReasonUsedWhenNoLegacyReason(t *testing.T) {
	ae := gerr(403, "", "Google Drive API has not been used in project 42 before or it is disabled.")
	ae.Details = []any{
		map[string]any{
			"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
			"reason": "SERVICE_DISABLED",
			"metadata": map[string]any{
				"consumer": "projects/42",
			},
		},
	}

	out := Classify(ae, testOp, testBlobID)

	de, ok := AsDriveError(out)
	require.True(t, ok)
	require.Equal(t, FatalAuth, de.Disposition())
	require.Equal(t, "accessNotConfigured", de.Reason())
	require.Contains(t, out.Error(), "projects/42")
}

func TestFields(t *testing.T) {
	ae := gerr(429, "userRateLimitExceeded", "User Rate Limit Exceeded")
	ae.Header = http.Header{"Retry-After": []string{"30"}}

	de, ok := AsDriveError(Classify(ae, "GetBlob", "q1"))
	require.True(t, ok)

	f := de.Fields()
	require.Zero(t, len(f)%2, "Fields() must return alternating key/value pairs")

	m := map[string]any{}

	for i := 0; i < len(f); i += 2 {
		k, isString := f[i].(string)
		require.True(t, isString, "field key %v is not a string", i)
		m[k] = f[i+1]
	}

	require.Equal(t, "GetBlob", m["op"])
	require.Equal(t, "q1", m["blobID"])
	require.Equal(t, RetryableBackoff.String(), m["disposition"])
	require.Equal(t, 429, m["httpStatus"])
	require.Equal(t, "userRateLimitExceeded", m["reason"])
	require.Equal(t, 30*time.Second, m["retryAfter"])
	require.Equal(t, true, m["possiblyDailyUploadCap"])
	require.NotNil(t, m["cause"])
}

func TestDispositionString(t *testing.T) {
	seen := map[string]bool{}

	for d := RetryableBackoff; d <= NonRetryableOther; d++ {
		s := d.String()
		require.NotEmpty(t, s)
		require.False(t, seen[s], "duplicate disposition string %q", s)
		seen[s] = true
		require.NotContains(t, s, "disposition(")
	}

	require.Contains(t, Disposition(99).String(), "disposition(")

	require.True(t, RetryableBackoff.Retryable())
	require.True(t, RetryableResume.Retryable())
	require.False(t, FatalQuota.Retryable())
	require.False(t, FatalAuth.Retryable())
	require.False(t, FatalNotFound.Retryable())
	require.False(t, FatalPrecondition.Retryable())
	require.False(t, FatalInvalidRange.Retryable())
	require.False(t, NonRetryableOther.Retryable())
}

func TestNormalizeReason(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "download_restricted_for_revision", want: "downloadrestrictedforrevision"},
		{in: "downloadRestrictedForRevision", want: "downloadrestrictedforrevision"},
		{in: "UrlLeaseLimitExceeded", want: "urlleaselimitexceeded"},
		{in: "urlLeaseLimitExceeded", want: "urlleaselimitexceeded"},
		{in: " notFound\t", want: "notfound"},
		{in: "invalid_grant", want: "invalidgrant"},
		{in: "", want: ""},
	}

	for _, tc := range cases {
		require.Equal(t, tc.want, normalizeReason(tc.in), "normalizeReason(%q)", tc.in)
	}
}

func TestUnwrapReturnsSentinelAndCause(t *testing.T) {
	original := gerr(404, "notFound", "File not found: abc")
	out := Classify(original, testOp, testBlobID)

	de, ok := AsDriveError(out)
	require.True(t, ok)

	u, ok := any(de).(interface{ Unwrap() []error })
	require.True(t, ok, "DriveError must implement Unwrap() []error")

	chain := u.Unwrap()
	require.Len(t, chain, 2)
	require.Same(t, blob.ErrBlobNotFound, chain[0]) //nolint:testifylint
	require.Same(t, original, chain[1])
}

func TestNonSentinelDispositionsUnwrapToCauseOnly(t *testing.T) {
	original := gerr(500, "backendError", "boom")
	out := Classify(original, testOp, testBlobID)

	de, _ := AsDriveError(out)
	chain := de.Unwrap()

	require.Len(t, chain, 1)
	require.Same(t, original, chain[0])
}

func TestErrorStringIncludesCause(t *testing.T) {
	original := gerr(500, "backendError", "the-server-said-this")
	msg := Classify(original, testOp, testBlobID).Error()

	require.Contains(t, msg, "the-server-said-this")
	require.Contains(t, msg, fmt.Sprintf("HTTP %d", 500))
}

func TestDispositionOfUnclassified(t *testing.T) {
	d, ok := DispositionOf(errors.New("plain"))
	require.False(t, ok)
	require.Equal(t, NonRetryableOther, d)
}

func TestNoReasonIsNeverEmptyMessage(t *testing.T) {
	out := Classify(gerr(451, "", ""), testOp, testBlobID)
	require.NotEmpty(t, strings.TrimSpace(out.Error()))
}

// fakeHTTPTimeoutError mimics net/http's unexported timeoutError (returned for
// ResponseHeaderTimeout), which is a net.Error that ALSO claims to be
// context.DeadlineExceeded.
type fakeHTTPTimeoutError struct{}

func (fakeHTTPTimeoutError) Error() string   { return "net/http: timeout awaiting response headers" }
func (fakeHTTPTimeoutError) Timeout() bool   { return true }
func (fakeHTTPTimeoutError) Temporary() bool { return true }

func (fakeHTTPTimeoutError) Is(target error) bool { return target == context.DeadlineExceeded } //nolint:errorlint

func TestStallErrorsAreRetryableResume(t *testing.T) {
	cases := map[string]error{
		"io-idle-deadline":         &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded},
		"io-idle-deadline-wrapped": fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}),
		"closed-conn":              fmt.Errorf("write: %w", net.ErrClosed),
		"h2-conn-lost":             errors.New("http2: client connection lost"),
		"h2-conn-lost-in-url":      &url.Error{Op: "Put", URL: "https://www.googleapis.com/upload", Err: errors.New("http2: client connection lost")},
		"h2-header-timeout":        errors.New("http2: timeout awaiting response headers"),
		"h2-goaway":                errors.New("http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR"),
		"conn-reset":               &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		"unexpected-eof":           fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF),
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := ClassifyContext(t.Context(), in, testOp, testBlobID)

			de, ok := AsDriveError(out)
			require.True(t, ok)
			require.Equal(t, RetryableResume, de.Disposition())
			require.Contains(t, out.Error(), "network error")
		})
	}
}

// TestClassifyContextTransportTimeoutWithLiveContext covers the errors net/http
// reports for its own timeouts that nonetheless match context.DeadlineExceeded.
func TestClassifyContextTransportTimeoutWithLiveContext(t *testing.T) {
	cases := map[string]error{
		"response-header-timeout": &url.Error{Op: "Get", URL: "https://www.googleapis.com/drive/v3/files", Err: fakeHTTPTimeoutError{}},
		"bare-deadline":           context.DeadlineExceeded,
		"chunk-upload-wrapped":    fmt.Errorf("chunk upload failed after 2 attempts, final error: %w", &url.Error{Op: "Post", URL: "u", Err: fakeHTTPTimeoutError{}}),
		"bare-canceled":           context.Canceled,
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			// Plain Classify cannot tell; it keeps the pass-through contract.
			require.Equal(t, in, Classify(in, testOp, testBlobID))

			out := ClassifyContext(t.Context(), in, testOp, testBlobID)

			de, ok := AsDriveError(out)
			require.True(t, ok)
			require.Equal(t, RetryableResume, de.Disposition())
			require.True(t, de.Disposition().Retryable())
			require.Contains(t, out.Error(), "stalled or timed out")
			require.Contains(t, out.Error(), testOp)

			// It must not read as a cancellation to upper layers ...
			require.NotErrorIs(t, out, context.DeadlineExceeded)
			require.NotErrorIs(t, out, context.Canceled)

			// ... while the transport error itself stays reachable.
			var ne net.Error

			require.ErrorAs(t, out, &ne)
			require.True(t, ne.Timeout())

			var inURL *url.Error
			if errors.As(in, &inURL) {
				var ue *url.Error

				require.ErrorAs(t, out, &ue)
			}

			// Idempotent.
			require.Equal(t, out, ClassifyContext(t.Context(), out, testOp, testBlobID))
		})
	}
}

func TestClassifyContextCallerCancellationPassesThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	expired, cancelExpired := context.WithDeadline(t.Context(), clock.Now().Add(-time.Second))
	defer cancelExpired()

	cases := map[string]struct {
		ctx context.Context //nolint:containedctx
		in  error
	}{
		"canceled":                {ctx, context.Canceled},
		"canceled-in-url":         {ctx, &url.Error{Op: "Get", URL: "u", Err: context.Canceled}},
		"deadline":                {expired, context.DeadlineExceeded},
		"response-header-timeout": {expired, &url.Error{Op: "Get", URL: "u", Err: fakeHTTPTimeoutError{}}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out := ClassifyContext(tc.ctx, tc.in, testOp, testBlobID)

			require.Equal(t, tc.in, out)

			_, ok := AsDriveError(out)
			require.False(t, ok)
		})
	}
}

func TestClassifyContextMatchesClassifyForOtherErrors(t *testing.T) {
	for _, in := range []error{
		gerr(http.StatusForbidden, "rateLimitExceeded", "slow down"),
		gerr(http.StatusNotFound, "notFound", "nope"),
		io.ErrUnexpectedEOF,
		errors.New("something else"),
	} {
		require.Equal(t, Classify(in, testOp, testBlobID), ClassifyContext(t.Context(), in, testOp, testBlobID))
	}

	require.NoError(t, ClassifyContext(t.Context(), nil, testOp, testBlobID))
}
