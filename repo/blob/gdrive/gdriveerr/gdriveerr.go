//go:build !no_extra_providers

// Package gdriveerr classifies errors returned by the Google Drive API v3 into
// actionable dispositions for the Kopia Google Drive backend.
//
// # Why this package exists
//
// Google Drive reports nearly every failure mode as HTTP 403 with a `reason`
// string buried in the JSON body. "Slow down for two seconds", "your Drive is
// full", "a Workspace admin has banned this app" and "you have uploaded 750 GB
// today, come back tomorrow" are all 403s. Retrying them uniformly - which is
// what the generic repo/blob/retrying wrapper does - turns a fail-fast
// condition into a ten-attempt, several-minute hang followed by an error
// message that names none of the above. Classify turns the (HTTP status,
// reason) pair into a Disposition plus a remediation message that names the
// actual fix.
//
// The classification table is a transcription of
// https://developers.google.com/workspace/drive/api/guides/handle-errors
// (complete reason inventory as of 2026-04-20), plus reasons observed in the
// field that the document does not list.
//
// # The userRateLimitExceeded / 750 GB-per-day ambiguity
//
// Google documents no dedicated error reason for the per-user 750 GB/day
// upload cap. It surfaces as 403 userRateLimitExceeded - the same reason
// returned for an ordinary "you are going too fast, back off for a second"
// throttle. The two are indistinguishable from a single response, so
// userRateLimitExceeded is classified RetryableBackoff. A caller that has been
// backing off for minutes without progress on an upload path should use
// IsPossiblyDailyUploadCap to escalate the message instead of retrying for the
// remainder of the 24-hour window.
//
// # Interaction with repo/blob/retrying (design note)
//
// repo/blob/retrying decides retriability by testing errors.Is against a fixed
// list of blob package sentinels; everything else is retried up to ten times,
// and it does not honor Retry-After. Because it cannot know about
// ErrStorageQuotaExhausted, a FatalQuota error returned through it would still
// be retried ten times. That is accepted here: this package does not modify
// repo/blob/retrying. The Phase-2 Drive backend wraps its calls in a
// disposition-aware retry layer that consults Disposition and RetryDelay
// directly, and only the dispositions that map onto blob sentinels
// (FatalNotFound, FatalPrecondition, FatalInvalidRange, FatalAuth) rely on the
// generic wrapper for their behavior.
package gdriveerr

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo/blob"
)

// Disposition describes what a caller should do about an error.
type Disposition int

// Supported dispositions.
const (
	// RetryableBackoff indicates a rate limit: back off (honoring Retry-After
	// when the server supplied it) and retry.
	RetryableBackoff Disposition = iota

	// RetryableResume indicates a transient server-side or network failure.
	// Retrying is safe and a resumable upload may be resumed rather than
	// restarted.
	RetryableResume

	// FatalQuota indicates an exhausted storage, item-count or daily API
	// quota. Never retry: none of these clear within a retry horizon.
	FatalQuota

	// FatalAuth indicates a credential, permission or domain-policy failure.
	// Never retry without operator intervention.
	FatalAuth

	// FatalNotFound maps to blob.ErrBlobNotFound.
	FatalNotFound

	// FatalPrecondition maps to blob.ErrBlobAlreadyExists.
	FatalPrecondition

	// FatalInvalidRange maps to blob.ErrInvalidRange.
	FatalInvalidRange

	// NonRetryableOther is a conclusively permanent error that fits no bucket
	// above - typically a malformed request, i.e. a bug in the caller.
	NonRetryableOther
)

// String implements fmt.Stringer.
func (d Disposition) String() string {
	switch d {
	case RetryableBackoff:
		return "retryable-backoff"
	case RetryableResume:
		return "retryable-resume"
	case FatalQuota:
		return "fatal-quota"
	case FatalAuth:
		return "fatal-auth"
	case FatalNotFound:
		return "fatal-not-found"
	case FatalPrecondition:
		return "fatal-precondition"
	case FatalInvalidRange:
		return "fatal-invalid-range"
	case NonRetryableOther:
		return "non-retryable-other"
	default:
		return "disposition(" + strconv.Itoa(int(d)) + ")"
	}
}

// Retryable returns true if the disposition permits another attempt.
func (d Disposition) Retryable() bool {
	return d == RetryableBackoff || d == RetryableResume
}

// ErrStorageQuotaExhausted is the sentinel unwrapped by every FatalQuota
// error. It has no counterpart in package blob, so repo/blob/retrying does not
// recognize it; see the package documentation.
var ErrStorageQuotaExhausted = errors.New("google drive storage or item quota exhausted")

// Backoff parameters used by RetryDelay. They implement the truncated
// exponential backoff with full jitter that Google's own documentation
// recommends for all time-based Drive errors.
const (
	// BackoffBase is the base delay of the exponential backoff.
	BackoffBase = 500 * time.Millisecond

	// BackoffCap is the maximum delay the exponential backoff will produce.
	BackoffCap = 32 * time.Second

	// maxBackoffShift is the largest exponent applied to BackoffBase before
	// the result saturates at BackoffCap (500ms << 6 == 32s).
	maxBackoffShift = 6

	// sharingRateLimitDelay is the floor applied to the sharing rate limit,
	// which recovers far more slowly than the per-user request limit.
	sharingRateLimitDelay = 10 * time.Second
)

// Header names and detail keys.
const (
	retryAfterHeader = "Retry-After"
	errorInfoType    = "google.rpc.ErrorInfo"
)

// DriveError is a classified Google Drive error. It unwraps to both the
// sentinel matching its disposition (so errors.Is works through it) and the
// original error (so errors.As recovers *googleapi.Error).
type DriveError struct {
	op          string
	blobID      string
	reason      string
	infoReason  string
	httpStatus  int
	disposition Disposition
	retryAfter  time.Duration
	summary     string
	remediation string
	extra       string
	dailyCap    bool
	cause       error
}

// Op returns the blob.Storage operation during which the error occurred.
func (e *DriveError) Op() string { return e.op }

// BlobID returns the blob the failing operation was working on, if any.
func (e *DriveError) BlobID() string { return e.blobID }

// Reason returns the Google Drive `reason` string verbatim, or "" when the
// error carried none.
func (e *DriveError) Reason() string { return e.reason }

// ErrorInfoReason returns the google.rpc.ErrorInfo reason (e.g.
// "SERVICE_DISABLED") when the response carried structured details.
func (e *DriveError) ErrorInfoReason() string { return e.infoReason }

// HTTPStatus returns the HTTP status code, or 0 for non-HTTP errors.
func (e *DriveError) HTTPStatus() int { return e.httpStatus }

// Disposition returns what the caller should do about this error.
func (e *DriveError) Disposition() Disposition { return e.disposition }

// RetryAfter returns the delay requested by the server's Retry-After header,
// or 0 when the header was absent, unparseable or in the past.
func (e *DriveError) RetryAfter() time.Duration { return e.retryAfter }

// Fields returns the error as alternating key/value pairs suitable for
// zap's SugaredLogger `w`-style calls (log(ctx).Debugw("msg", err.Fields()...))
// and for metric labeling.
func (e *DriveError) Fields() []any {
	f := []any{
		"op", e.op,
		"blobID", e.blobID,
		"disposition", e.disposition.String(),
	}

	if e.httpStatus != 0 {
		f = append(f, "httpStatus", e.httpStatus)
	}

	if e.reason != "" {
		f = append(f, "reason", e.reason)
	}

	if e.infoReason != "" {
		f = append(f, "errorInfoReason", e.infoReason)
	}

	if e.retryAfter > 0 {
		f = append(f, "retryAfter", e.retryAfter)
	}

	if e.dailyCap {
		f = append(f, "possiblyDailyUploadCap", true)
	}

	return append(f, "cause", e.cause)
}

// Error implements the error interface.
func (e *DriveError) Error() string {
	var sb strings.Builder

	sb.WriteString("gdrive ")
	sb.WriteString(e.op)
	fmt.Fprintf(&sb, " blob %q", e.blobID)

	if e.httpStatus != 0 {
		fmt.Fprintf(&sb, ": HTTP %d", e.httpStatus)

		if e.reason != "" {
			fmt.Fprintf(&sb, " %s", e.reason)
		}
	}

	fmt.Fprintf(&sb, ": %s", e.summary)

	if e.remediation != "" {
		fmt.Fprintf(&sb, " -- %s", e.remediation)
	}

	if e.extra != "" {
		fmt.Fprintf(&sb, " %s", e.extra)
	}

	if e.retryAfter > 0 {
		fmt.Fprintf(&sb, " (server asked to retry after %v)", e.retryAfter)
	}

	if e.cause != nil {
		fmt.Fprintf(&sb, " [cause: %v]", e.cause)
	}

	return sb.String()
}

// Unwrap returns both the sentinel matching this error's disposition and the
// original error, so that errors.Is finds blob.ErrBlobNotFound (etc.) and
// errors.As still recovers the original *googleapi.Error.
func (e *DriveError) Unwrap() []error {
	s := sentinelFor(e.disposition)

	switch {
	case s != nil && e.cause != nil:
		return []error{s, e.cause}
	case s != nil:
		return []error{s}
	case e.cause != nil:
		return []error{e.cause}
	default:
		return nil
	}
}

func sentinelFor(d Disposition) error {
	switch d {
	case FatalNotFound:
		return blob.ErrBlobNotFound
	case FatalPrecondition:
		return blob.ErrBlobAlreadyExists
	case FatalInvalidRange:
		return blob.ErrInvalidRange
	case FatalAuth:
		return blob.ErrInvalidCredentials
	case FatalQuota:
		return ErrStorageQuotaExhausted
	case RetryableBackoff, RetryableResume, NonRetryableOther:
		return nil
	default:
		return nil
	}
}

// rule is one row of the classification table.
type rule struct {
	disposition Disposition
	summary     string
	remediation string

	// minDelay is a floor applied by RetryDelay for retryable rules whose
	// underlying limit recovers unusually slowly.
	minDelay time.Duration

	// dailyCap marks reasons that may actually be the undocumented
	// 750 GB/day per-user upload cap.
	dailyCap bool
}

// reasonAuthError is the Drive reason used for credential failures, including
// token-minting errors that carry no reason of their own.
const reasonAuthError = "authError"

// Remediation text shared by several reasons.
const (
	transientServerHelp = "transient server-side failure; retrying with exponential backoff."

	driveFileScopeHelp = "under the drive.file scope an app can only see files that it created itself or that the " +
		"user explicitly opened with it, so a folder or blob created by another app, another user or through the " +
		"Drive web UI is invisible to this app even though the account can see it; fix by reconnecting with a " +
		"broader scope or by re-granting access to this app (client ID) through the consent/picker flow using the " +
		"account that owns the data. Not retryable."

	itemLimitHelp = "note that items in the trash still count toward Drive item limits, so blobs that were trashed " +
		"rather than permanently deleted do not free capacity: permanently delete items (files.delete, or empty the " +
		"trash) or relocate the repository. Not retryable."
)

// reasonRules is the (reason -> rule) classification table, keyed on the
// canonical camelCase spelling of the Drive `reason` value. Lookup is
// case-insensitive and ignores underscores, so the snake_case
// (download_restricted_for_revision) and capitalized (UrlLeaseLimitExceeded)
// spellings Google actually emits all resolve correctly.
//
//nolint:gochecknoglobals
var reasonRules = normalizeRules(map[string]rule{
	// ---------------- 400 Bad Request ----------------
	"badRequest": {
		disposition: NonRetryableOther,
		summary:     "Drive rejected the request as malformed",
		remediation: "this normally indicates a bug in Kopia's Drive backend (missing field, invalid value " +
			"combination, duplicate parent or a parent cycle) rather than a transient condition. Not retryable.",
	},
	"illegalKeepForeverModification": {
		disposition: NonRetryableOther,
		summary:     "cannot clear keepForever on a revision",
		remediation: "revision-only condition that Kopia should never trigger. Not retryable.",
	},
	"invalidSharingRequest": {
		disposition: NonRetryableOther,
		summary:     "Drive rejected a sharing request",
		remediation: "the permission change was not allowed, or notification email could not be delivered; " +
			"inspect the server message. Not retryable.",
	},

	// ---------------- 401 Unauthorized ----------------
	reasonAuthError: {
		disposition: FatalAuth,
		summary:     "invalid Drive credentials",
		remediation: "the access token is expired or invalid, or it is missing the scopes this operation needs. " +
			"Refresh the token; if refresh fails, re-run the authorization flow (kopia repository connect) for " +
			"this account. Not retryable.",
	},
	"invalid_grant": {
		disposition: FatalAuth,
		summary:     "the OAuth refresh token was rejected (invalid_grant)",
		remediation: "the refresh token has been revoked, expired, or belongs to a different client ID; the " +
			"service-account key may also have been deleted or its clock skewed. Re-authorize this account. " +
			"Not retryable.",
	},
	"unauthorized": {
		disposition: FatalAuth,
		summary:     "Drive refused the credentials",
		remediation: "re-authorize this account. Not retryable.",
	},
	"fileNotDownloadable": {
		disposition: NonRetryableOther,
		summary:     "the file has no binary content to download",
		remediation: "only files with binary content can be downloaded; a Docs-Editors file has appeared where a " +
			"Kopia blob was expected. Not retryable.",
	},

	// ---------------- 403 Forbidden ----------------
	"accessNotConfigured": {
		disposition: FatalAuth,
		summary:     "the Google Drive API is not enabled for this OAuth client's Cloud project",
		remediation: "enable the Google Drive API in the Google Cloud console for the project that owns the OAuth " +
			"client ID being used (APIs & Services > Library > Google Drive API > Enable), then wait a few minutes " +
			"for the change to propagate. Not retryable.",
	},
	"activeItemCreationLimitExceeded": {
		disposition: FatalQuota,
		summary:     "the account has created 500 million Drive items, the per-account maximum",
		remediation: itemLimitHelp,
	},
	"appNotAuthorizedToFile": {
		disposition: FatalAuth,
		summary:     "this app has not been granted access to the target file by this user",
		remediation: driveFileScopeHelp,
	},
	"cannotDownloadAbusiveFile": {
		disposition: NonRetryableOther,
		summary:     "Drive flagged the file as abusive and refused to serve it",
		remediation: "Drive believes the blob contains malware or spam; download requires explicit abuse " +
			"acknowledgement. Not retryable.",
	},
	"cannotModifyInheritedTeamDrivePermission": {
		disposition: NonRetryableOther,
		summary:     "cannot modify an inherited Shared Drive permission",
		remediation: "change the permission on the Shared Drive itself instead of the item. Not retryable.",
	},
	"dailyLimitExceeded": {
		disposition: FatalQuota,
		summary:     "the Drive API daily request quota for this Cloud project is exhausted",
		remediation: "this will not clear inside any sane retry window: wait for the daily quota reset, raise or " +
			"remove the 'Queries per day' cap on the Drive API in the Google Cloud console, or switch to a " +
			"dedicated OAuth client in your own Cloud project so the quota is not shared with other users of " +
			"this client ID. Failing fast rather than retrying.",
	},
	"domainPolicy": {
		disposition: FatalAuth,
		summary:     "a Google Workspace admin policy is blocking this app for this user",
		remediation: "the domain administrators have disabled Drive apps, or have not allowlisted this one. A " +
			"Workspace admin must allow this app (its OAuth client ID) in the Admin console under Apps > Google " +
			"Workspace Marketplace apps / API controls, or you must use an account in a different domain. " +
			"Not retryable.",
	},
	"downloadQuotaExceeded": {
		disposition: FatalQuota,
		summary:     "the download quota for this file has been exceeded",
		remediation: "Drive rate-limits downloads of heavily-shared files for up to 24 hours; wait for the quota " +
			"to reset or make a copy owned by this account. Failing fast rather than retrying.",
	},
	"download_restricted_for_revision": {
		disposition: NonRetryableOther,
		summary:     "this revision cannot be downloaded by the authenticated user",
		remediation: "the revision is download-restricted. Not retryable.",
	},
	"fileNotExportable": {
		disposition: NonRetryableOther,
		summary:     "the file cannot be exported",
		remediation: "a non-blob file type has appeared where a Kopia blob was expected. Not retryable.",
	},
	"fileOwnerNotMemberOfTeamDrive": {
		disposition: FatalAuth,
		summary:     "the file owner is not a member of the target Shared Drive",
		remediation: "add the owning account to the Shared Drive, or write with an account that owns the data. " +
			"Not retryable.",
	},
	"fileWriterTeamDriveMoveInDisabled": {
		disposition: FatalAuth,
		summary:     "the Workspace admin does not allow writers to move items into this Shared Drive",
		remediation: "a Workspace admin must relax the Shared Drive move-in restriction, or use an account with " +
			"content-manager or higher access. Not retryable.",
	},
	"insufficientFilePermissions": {
		disposition: FatalAuth,
		summary:     "the authenticated user does not have sufficient permissions for this file",
		remediation: driveFileScopeHelp,
	},
	"insufficientParentPermissions": {
		disposition: FatalAuth,
		summary:     "the authenticated user does not have sufficient permissions on the parent folder",
		remediation: driveFileScopeHelp,
	},
	"myDriveHierarchyDepthLimitExceeded": {
		disposition: NonRetryableOther,
		summary:     "My Drive cannot contain more than 100 levels of nested folders",
		remediation: "move the repository closer to the root of My Drive. Not retryable.",
	},
	"numChildrenInNonRootLimitExceeded": {
		disposition: FatalQuota,
		summary:     "the destination folder has reached the Drive limit of 500,000 direct children",
		remediation: "shard the repository across more folders (items nested in subfolders do not count toward a " +
			"folder's limit) or relocate it; " + itemLimitHelp,
	},
	"quotaExceeded": {
		disposition: FatalQuota,
		summary:     "a Drive quota has been exceeded",
		remediation: "the account's storage or item quota is exhausted; free space or move the repository to " +
			"storage with capacity. Failing fast rather than retrying.",
	},
	"rateLimitExceeded": {
		disposition: RetryableBackoff,
		summary:     "the Drive API rate limit for this Cloud project was exceeded",
		remediation: "backing off and retrying with truncated exponential backoff. If this is persistent, reduce " +
			"parallelism or use a dedicated OAuth client in your own Cloud project - the per-project rate limit " +
			"is shared by every user of the same client ID.",
	},
	"sharingRateLimitExceeded": {
		disposition: RetryableBackoff,
		summary:     "the Drive sharing rate limit was exceeded",
		remediation: "the sharing/permissions limit recovers much more slowly than the request limit; backing off " +
			"for longer before retrying.",
		minDelay: sharingRateLimitDelay,
	},
	"storageQuotaExceeded": {
		disposition: FatalQuota,
		summary:     "the Drive storage quota of the authenticated account is exhausted",
		remediation: "if you are authenticating as a service account, note that the service account has its OWN " +
			"Drive storage quota - roughly 15 GB - which is entirely separate from any human user's quota, is " +
			"consumed by everything the service account creates, and cannot be increased. The fix is one of: " +
			"(a) authenticate with OAuth as the user who owns the storage, (b) configure domain-wide delegation " +
			"and impersonate that user, or (c) put the repository in a Shared Drive, which is billed to the " +
			"Workspace domain pool (service accounts cannot own files at all, so they must write into a Shared " +
			"Drive). Failing fast, not retrying: no amount of retrying frees storage.",
	},
	"teamDriveFileLimitExceeded": {
		disposition: FatalQuota,
		summary:     "the Shared Drive has reached its 500,000 item limit",
		remediation: "this is a whole-drive cap, so sharding the blob namespace does not help: relocate the " +
			"repository to another Shared Drive or to My Drive; " + itemLimitHelp,
	},
	"teamDriveHierarchyTooDeep": {
		disposition: NonRetryableOther,
		summary:     "the Shared Drive folder hierarchy would exceed the 100-level limit",
		remediation: "move the repository closer to the root of the Shared Drive. Not retryable.",
	},
	"teamDriveMembershipRequired": {
		disposition: FatalAuth,
		summary:     "the operation requires membership in the target Shared Drive",
		remediation: "add the authenticated account (or the impersonated user, when using domain-wide delegation) " +
			"as a member of the Shared Drive with at least content-manager access. Not retryable.",
	},
	"teamDrivesFolderMoveInNotSupported": {
		disposition: NonRetryableOther,
		summary:     "moving folders into a Shared Drive is not supported",
		remediation: "create the repository folder inside the Shared Drive instead of moving it in. Not retryable.",
	},
	"teamDrivesParentLimit": {
		disposition: NonRetryableOther,
		summary:     "a Shared Drive item must have exactly one parent",
		remediation: "this indicates a bug in Kopia's Drive backend. Not retryable.",
	},
	"UrlLeaseLimitExceeded": {
		disposition: RetryableBackoff,
		summary:     "too many pending uploads for this account",
		remediation: "finish or cancel pending uploads; backing off and retrying.",
	},
	"userRateLimitExceeded": {
		disposition: RetryableBackoff,
		summary:     "the per-user Drive rate limit was exceeded",
		remediation: "backing off and retrying. NOTE: Google publishes no dedicated error for the 750 GB/day " +
			"per-user upload cap - it also surfaces as 403 userRateLimitExceeded - so if this keeps recurring on " +
			"an upload path across several minutes of backoff, the daily upload cap has most likely been reached " +
			"and no amount of retrying will clear it for up to 24 hours. The 750 GB budget is shared across My " +
			"Drive and every Shared Drive the user writes to.",
		dailyCap: true,
	},

	// ---------------- 404 Not Found ----------------
	"notFound": {
		disposition: FatalNotFound,
		summary:     "blob not found in Google Drive",
		remediation: "note that Drive overloads 404: under the drive.file scope it does not distinguish 'does not " +
			"exist' from 'exists but was not created by, or shared with, this app', so this can also mean the " +
			"blob is present but invisible to this OAuth client. Also make sure supportsAllDrives=true is set on " +
			"every request when the repository lives in a Shared Drive.",
	},
	"fileNotFound": {
		disposition: FatalNotFound,
		summary:     "blob not found in Google Drive",
		remediation: "note that Drive overloads 404: under the drive.file scope it does not distinguish 'does not " +
			"exist' from 'exists but was not created by, or shared with, this app', so this can also mean the " +
			"blob is present but invisible to this OAuth client. Also make sure supportsAllDrives=true is set on " +
			"every request when the repository lives in a Shared Drive.",
	},

	// ---------------- 5xx ----------------
	"backendError": {
		disposition: RetryableResume,
		summary:     "Google Drive returned a backend error",
		remediation: transientServerHelp,
	},
	"internalError": {
		disposition: RetryableResume,
		summary:     "Google Drive returned an internal error",
		remediation: transientServerHelp,
	},
	"internalServerError": {
		disposition: RetryableResume,
		summary:     "Google Drive returned an internal server error",
		remediation: transientServerHelp,
	},
})

// errorInfoAliases maps google.rpc.ErrorInfo reasons onto the classic Drive
// `reason` strings, for responses that carry only structured details.
//
//nolint:gochecknoglobals
var errorInfoAliases = map[string]string{
	"SERVICE_DISABLED":                "accessNotConfigured",
	"RATE_LIMIT_EXCEEDED":             "rateLimitExceeded",
	"RESOURCE_EXHAUSTED":              "quotaExceeded",
	"ACCESS_TOKEN_EXPIRED":            reasonAuthError,
	"ACCESS_TOKEN_SCOPE_INSUFFICIENT": reasonAuthError,
}

// normalizeRules rewrites the table's keys into their normalized form and
// panics on a collision, which can only be a programming error in the table.
func normalizeRules(in map[string]rule) map[string]rule {
	out := make(map[string]rule, len(in))

	for k, v := range in {
		nk := normalizeReason(k)
		if _, ok := out[nk]; ok {
			panic("gdriveerr: duplicate reason in classification table: " + k)
		}

		out[nk] = v
	}

	return out
}

// normalizeReason lowercases a reason and strips separators so that
// "download_restricted_for_revision", "downloadRestrictedForRevision" and
// "UrlLeaseLimitExceeded" all resolve to a single table entry.
func normalizeReason(s string) string {
	var sb strings.Builder

	sb.Grow(len(s))

	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r == '_' || r == '-' || r == ' ' || r == '.' {
			continue
		}

		sb.WriteRune(r)
	}

	return sb.String()
}

// Classify converts an error returned by the Google Drive client into a
// *DriveError carrying a disposition and actionable remediation text.
//
// Errors that are, or wrap, context.Canceled or context.DeadlineExceeded are
// returned unchanged: a canceled request is the caller's decision and must
// never be reported as retryable. nil returns nil, and an error that is
// already a *DriveError is returned unchanged.
func Classify(err error, op, blobID string) error {
	if err == nil {
		return nil
	}

	// Context errors pass through untouched, including when the HTTP stack has
	// wrapped them in a *url.Error - url.Error implements Unwrap, so errors.Is
	// still finds them and callers can keep using errors.Is on the result.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var already *DriveError
	if errors.As(err, &already) {
		return err
	}

	var ae *googleapi.Error
	if errors.As(err, &ae) {
		return fromGoogleAPIError(err, ae, op, blobID)
	}

	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return fromOAuthError(err, re, op, blobID)
	}

	if isNetworkRetryable(err) {
		return &DriveError{
			op:          op,
			blobID:      blobID,
			disposition: RetryableResume,
			summary:     "network error talking to Google Drive",
			remediation: "transient; retrying (safe to resume an in-flight resumable upload).",
			cause:       err,
		}
	}

	return &DriveError{
		op:          op,
		blobID:      blobID,
		disposition: RetryableResume,
		summary:     "unexpected Google Drive error",
		remediation: "the error is not recognized by Kopia's Drive error taxonomy, so it is treated as " +
			"potentially transient and retried, matching Kopia's default behavior for unclassified errors.",
		cause: err,
	}
}

// ClassifyContext is Classify for a request made under ctx, which is what lets
// it tell the caller's cancellation from a timeout inside the HTTP stack.
//
// Classify alone cannot: Go's transport reports several of its own timeouts -
// ResponseHeaderTimeout ("net/http: timeout awaiting response headers") and a
// dialer timeout ("i/o timeout" from package net) - as errors for which
// errors.Is(err, context.DeadlineExceeded) is true, exactly like an expired
// caller context. The caller's context is the only authority on that question:
//
//   - ctx done: err is returned unchanged, as Classify does, and is never
//     retried.
//   - ctx still live: the context-like error came from below the caller (a
//     stalled or dead connection after a suspend, a network change, ...), and
//     it is classified RetryableResume like any other network failure. The
//     result deliberately does NOT satisfy errors.Is(_, context.Canceled) or
//     errors.Is(_, context.DeadlineExceeded): if the retries run out, the error
//     that reaches Kopia must read as a failure, not as a cancellation the user
//     never asked for.
func ClassifyContext(ctx context.Context, err error, op, blobID string) error {
	if err == nil {
		return nil
	}

	if ctx.Err() != nil || !isContextError(err) {
		return Classify(err, op, blobID)
	}

	if _, already := AsDriveError(err); already {
		return err
	}

	return &DriveError{
		op:          op,
		blobID:      blobID,
		disposition: RetryableResume,
		summary:     "connection to Google Drive stalled or timed out",
		remediation: "transient; retrying on a fresh connection (safe to resume an in-flight resumable upload).",
		cause:       transportTimeoutError{err},
	}
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// transportTimeoutError carries a transport timeout that claims to be a
// context error (see ClassifyContext) while hiding exactly that claim. It has
// no Unwrap on purpose, since errors.Is would otherwise walk past Is into the
// wrapped error; Is and As delegate everything else.
type transportTimeoutError struct {
	err error
}

func (e transportTimeoutError) Error() string { return e.err.Error() }

// Timeout reports true: this is, by construction, a timeout.
func (e transportTimeoutError) Timeout() bool { return true }

// Temporary reports true; it is part of net.Error.
func (e transportTimeoutError) Temporary() bool { return true }

func (e transportTimeoutError) Is(target error) bool {
	if target == context.Canceled || target == context.DeadlineExceeded {
		return false
	}

	return errors.Is(e.err, target)
}

func (e transportTimeoutError) As(target any) bool {
	return errors.As(e.err, target)
}

func fromGoogleAPIError(orig error, ae *googleapi.Error, op, blobID string) error {
	infoReason, activationURL, consumer := errorInfoFrom(ae.Details)

	reason := firstReason(ae)
	if reason == "" && infoReason != "" {
		if alias, ok := errorInfoAliases[strings.ToUpper(infoReason)]; ok {
			reason = alias
		}
	}

	r := lookup(ae.Code, reason)

	de := &DriveError{
		op:          op,
		blobID:      blobID,
		reason:      reason,
		infoReason:  infoReason,
		httpStatus:  ae.Code,
		disposition: r.disposition,
		retryAfter:  parseRetryAfter(ae.Header),
		summary:     r.summary,
		remediation: r.remediation,
		dailyCap:    r.dailyCap || isUploadLimitMessage(ae),
		cause:       orig,
	}

	if normalizeReason(reason) == normalizeReason("accessNotConfigured") {
		switch {
		case activationURL != "":
			de.extra = "Enable it here: " + activationURL + "."
		case consumer != "":
			de.extra = "The affected project is " + consumer + "."
		}
	}

	return de
}

func fromOAuthError(orig error, re *oauth2.RetrieveError, op, blobID string) error {
	code := 0
	if re.Response != nil {
		code = re.Response.StatusCode
	}

	reason := re.ErrorCode
	if reason == "" {
		reason = reasonAuthError
	}

	r := lookup(code, reason)
	if !isAuthLike(r.disposition) {
		// Any failure to mint a token is a credential problem, whatever the
		// transport said, unless it is a plain rate limit or server error.
		if code == 0 || (code >= http.StatusBadRequest && code < http.StatusInternalServerError && code != http.StatusTooManyRequests) {
			r = reasonRules[normalizeReason(reasonAuthError)]
		}
	}

	var hdr http.Header
	if re.Response != nil {
		hdr = re.Response.Header
	}

	return &DriveError{
		op:          op,
		blobID:      blobID,
		reason:      re.ErrorCode,
		httpStatus:  code,
		disposition: r.disposition,
		retryAfter:  parseRetryAfter(hdr),
		summary:     r.summary,
		remediation: r.remediation,
		cause:       orig,
	}
}

func isAuthLike(d Disposition) bool {
	return d == FatalAuth
}

// lookup resolves a (HTTP status, reason) pair to a rule. Status wins for the
// codes whose meaning is unambiguous regardless of reason (404, 412, 416, 429);
// otherwise the reason table wins, and a status-derived default is the
// fallback for unknown reasons.
func lookup(code int, reason string) rule {
	r, known := reasonRules[normalizeReason(reason)]

	switch code {
	case http.StatusNotFound:
		if known && r.disposition == FatalNotFound {
			return r
		}

		nf := reasonRules[normalizeReason("notFound")]
		nf.summary = withReason(nf.summary, reason)

		return nf

	case http.StatusPreconditionFailed:
		return rule{
			disposition: FatalPrecondition,
			summary:     withReason("the blob already exists and DoNotRecreate was requested", reason),
			remediation: "Drive returned a failed precondition. Not retryable.",
		}

	case http.StatusRequestedRangeNotSatisfiable:
		return rule{
			disposition: FatalInvalidRange,
			summary:     withReason("the requested byte range is not satisfiable", reason),
			remediation: "the blob is shorter than the requested offset+length. Not retryable.",
		}

	case http.StatusTooManyRequests:
		if known && r.disposition == RetryableBackoff {
			return r
		}

		rl := reasonRules[normalizeReason("rateLimitExceeded")]
		rl.summary = withReason("Drive returned 429 Too Many Requests", reason)

		return rl
	}

	if known {
		return r
	}

	return defaultRuleForStatus(code, reason)
}

func defaultRuleForStatus(c int, rs string) rule {
	switch {
	case c == http.StatusUnauthorized:
		r := reasonRules[normalizeReason(reasonAuthError)]
		r.summary = withReason(r.summary, rs)

		return r

	case c == http.StatusRequestTimeout:
		return rule{
			disposition: RetryableResume,
			summary:     withReason("Drive timed out receiving the request", rs),
			remediation: "transient; retrying with exponential backoff.",
		}

	case c >= http.StatusInternalServerError:
		return rule{
			disposition: RetryableResume,
			summary:     withReason("Google Drive returned a server error", rs),
			remediation: "transient server-side failure; retrying with exponential backoff (an in-flight " +
				"resumable upload may be resumed rather than restarted).",
		}

	case c >= http.StatusBadRequest:
		return rule{
			disposition: NonRetryableOther,
			summary:     withReason("Google Drive rejected the request", rs),
			remediation: "the reason is not recognized by Kopia's Drive error taxonomy; treating it as permanent " +
				"and not retrying. Please report this reason string to the Kopia project.",
		}

	default:
		return rule{
			disposition: RetryableResume,
			summary:     withReason("unexpected Google Drive error", rs),
			remediation: "the error is not recognized by Kopia's Drive error taxonomy, so it is treated as " +
				"potentially transient and retried.",
		}
	}
}

// withReason appends an unrecognized reason string to a summary so that it is
// never lost, whatever bucket the error ends up in.
func withReason(summary, reason string) string {
	if reason == "" {
		return summary
	}

	return summary + " (reason " + strconv.Quote(reason) + ")"
}

func firstReason(ae *googleapi.Error) string {
	for _, it := range ae.Errors {
		if it.Reason != "" {
			return it.Reason
		}
	}

	return ""
}

// isUploadLimitMessage detects rclone's heuristic for the 750 GB/day cap: the
// message is exactly "User rate limit exceeded." on a rate-limit reason.
func isUploadLimitMessage(ae *googleapi.Error) bool {
	const uploadLimitMessage = "User rate limit exceeded."

	if strings.EqualFold(strings.TrimSpace(ae.Message), uploadLimitMessage) {
		return true
	}

	for _, it := range ae.Errors {
		if strings.EqualFold(strings.TrimSpace(it.Message), uploadLimitMessage) {
			return true
		}
	}

	return false
}

// errorInfoFrom extracts the google.rpc.ErrorInfo reason and the metadata
// Kopia can act on (activationUrl, consumer) from googleapi's Details slice.
func errorInfoFrom(details []any) (reason, activationURL, consumer string) {
	for _, d := range details {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}

		if t, _ := m["@type"].(string); !strings.Contains(t, errorInfoType) {
			continue
		}

		reason, _ = m["reason"].(string)

		md, ok := m["metadata"].(map[string]any)
		if !ok {
			return reason, "", ""
		}

		activationURL, _ = md["activationUrl"].(string)
		consumer, _ = md["consumer"].(string)

		return reason, activationURL, consumer
	}

	return "", "", ""
}

// parseRetryAfter understands both forms of the Retry-After header: a
// delta-seconds integer and an HTTP-date.
func parseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}

	v := strings.TrimSpace(h.Get(retryAfterHeader))
	if v == "" {
		return 0
	}

	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}

		return time.Duration(secs) * time.Second
	}

	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(clock.Now()); d > 0 {
			return d
		}
	}

	return 0
}

// transportErrorMessages are failures the HTTP/2 client reports as plain
// errors.New values - no type to match, and not always wrapped in a *url.Error
// (a response body read returns them bare) - so they can only be recognized by
// their text. All of them mean the connection died under the request.
//
//nolint:gochecknoglobals
var transportErrorMessages = []string{
	"http2: client connection lost",
	"http2: client connection force closed",
	"http2: server sent GOAWAY and closed the connection",
	"http2: timeout awaiting response headers",
	"net/http: timeout awaiting response headers",
	"use of closed network connection",
}

// isNetworkRetryable reports whether err is a transport-level failure that is
// worth another attempt.
func isNetworkRetryable(err error) bool {
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return true
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, syscall.EHOSTUNREACH):
		return true
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, net.ErrClosed):
		// The backend's per-connection idle deadline firing, and the transport
		// closing the connection it fired on.
		return true
	case hasTransportErrorMessage(err):
		return true
	}

	var (
		opErr  *net.OpError
		dnsErr *net.DNSError
		urlErr *url.Error
		netErr net.Error
	)

	switch {
	case errors.As(err, &opErr), errors.As(err, &dnsErr), errors.As(err, &urlErr):
		return true
	case errors.As(err, &netErr):
		return netErr.Timeout()
	default:
		return false
	}
}

func hasTransportErrorMessage(err error) bool {
	msg := err.Error()

	for _, m := range transportErrorMessages {
		if strings.Contains(msg, m) {
			return true
		}
	}

	return false
}

// AsDriveError recovers the *DriveError from an error chain.
func AsDriveError(err error) (*DriveError, bool) {
	var de *DriveError

	ok := errors.As(err, &de)

	return de, ok
}

// DispositionOf returns the disposition of err, and whether it was classified
// by this package at all.
func DispositionOf(err error) (Disposition, bool) {
	if de, ok := AsDriveError(err); ok {
		return de.disposition, true
	}

	return NonRetryableOther, false
}

// IsPossiblyDailyUploadCap reports whether err may be the undocumented
// 750 GB/day per-user upload cap masquerading as an ordinary rate limit. A
// retry layer should use it to escalate the message - not to give up
// immediately - after sustained backoff has failed to make progress.
func IsPossiblyDailyUploadCap(err error) bool {
	de, ok := AsDriveError(err)

	return ok && de.dailyCap
}

// RetryDelay returns how long to wait before attempt number `attempt`
// (0-based) of an operation that failed with err. It honors the server's
// Retry-After when present, and otherwise applies truncated exponential
// backoff with full jitter, per Google's own guidance.
func RetryDelay(err error, attempt int) time.Duration {
	var minDelay time.Duration

	if de, ok := AsDriveError(err); ok {
		if de.retryAfter > 0 {
			return de.retryAfter
		}

		minDelay = reasonRules[normalizeReason(de.reason)].minDelay
	}

	if attempt < 0 {
		attempt = 0
	}

	shift := min(attempt, maxBackoffShift)

	window := min(BackoffBase<<uint(shift), BackoffCap)

	// Full jitter: uniform in [0, window).
	d := time.Duration(rand.Int64N(int64(window))) //nolint:gosec

	if d < minDelay {
		return minDelay
	}

	return d
}
