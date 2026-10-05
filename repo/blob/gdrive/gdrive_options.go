//go:build !no_extra_providers

package gdrive

import (
	"encoding/json"

	"github.com/kopia/kopia/repo/blob/throttling"
)

// Options defines options Google Cloud Storage-backed storage.
type Options struct {
	// FolderId is Google Drive's ID of a folder where data is stored.
	FolderID string `json:"folderID"`

	// FolderName names the folder in the root of My Drive that holds the
	// repository. It is used ONLY when FolderID is empty and the repository is
	// being created: the backend then reuses the folder of that name if it can
	// see exactly one, creates it otherwise, and writes the resulting ID into
	// FolderID before the connection info is serialized. From that moment on
	// FolderID is the operative field and this one is kept purely as a record of
	// how the folder came to be - which is what lets a second machine connect
	// with nothing but the folder ID.
	//
	// A folder created this way is owned by the application, so it is visible
	// under the default "drive.file" scope, unlike a folder made by hand in the
	// Drive web UI.
	FolderName string `json:"folderName,omitempty"`

	// ServiceAccountCredentialsFile specifies the name of the file with Drive credentials.
	ServiceAccountCredentialsFile string `json:"credentialsFile,omitempty"`

	// ServiceAccountCredentialJSON specifies the raw JSON credentials.
	ServiceAccountCredentialJSON json.RawMessage `json:"credentials,omitempty" kopia:"sensitive"`

	// ReadOnly causes GCS connection to be opened with read-only scope to prevent accidental mutations.
	ReadOnly bool `json:"readOnly,omitempty"`

	// ClientCredentialsFile specifies the name of an OAuth client secret JSON file
	// (the document with an "installed" or "web" section) downloaded from the Google
	// Cloud console. When set, Kopia performs an interactive sign-in the first time and
	// caches the resulting refresh token in TokenCacheFile.
	//
	// Using a dedicated OAuth client is strongly recommended: Google's per-project quota
	// buckets (1M quota units/minute, 1 TB/day egress) are charged to the Cloud project
	// that owns the client ID, so a client of your own is never diluted by other users.
	ClientCredentialsFile string `json:"clientCredentialsFile,omitempty"`

	// TokenCacheFile specifies where the refresh token obtained by the interactive
	// sign-in is stored. It holds a path, not a secret; the file it names is written
	// with owner-only permissions. Defaults to a file under Kopia's configuration
	// directory named after the OAuth client and the folder ID.
	TokenCacheFile string `json:"tokenCacheFile,omitempty"`

	// Impersonate specifies the user a service account acts on behalf of, using
	// Google Workspace domain-wide delegation. EXPERIMENTAL: the code path is
	// complete but has not been validated against a real Workspace domain.
	Impersonate string `json:"impersonate,omitempty"`

	// UseDeviceFlow selects the OAuth device authorization flow instead of the loopback
	// redirect for the interactive sign-in, for machines that cannot run a browser and
	// cannot accept a loopback connection. Google restricts this flow to the "drive.file"
	// scope.
	UseDeviceFlow bool `json:"useDeviceFlow,omitempty"`

	// Scope selects the Google Drive OAuth scope: "drive.file" (the default, per-file
	// access limited to what Kopia created) or "drive" (full access to the user's Drive,
	// required only for repositories in a pre-existing, externally created folder).
	Scope string `json:"scope,omitempty"`

	// Tuning holds performance knobs. Zero values select the built-in defaults.
	//
	// NOTE: the tag is "omitzero", not "omitempty". "omitempty" has no effect on a struct
	// field, which would make every serialized connection info grow an empty "tuning":{}
	// object; "omitzero" keeps the addition genuinely invisible until a knob is set,
	// which is what the config-compatibility contract requires. The JSON key is "tuning"
	// either way.
	Tuning TuningOptions `json:"tuning,omitzero"`

	throttling.Limits
}

// TuningOptions holds the performance knobs of the Google Drive backend. Every field is
// optional; a zero value selects the built-in default. The struct is deliberately flat
// and typed so it can be enumerated by reflection.
type TuningOptions struct {
	// PacerMinSleepMS is the minimum delay between API calls, in milliseconds.
	PacerMinSleepMS int `json:"pacerMinSleepMS,omitempty"`

	// PacerBurst is the number of calls allowed without any pacing delay.
	PacerBurst int `json:"pacerBurst,omitempty"`

	// PacerMaxSleepMS caps the backoff delay, in milliseconds.
	PacerMaxSleepMS int `json:"pacerMaxSleepMS,omitempty"`

	// MaxTries is the number of attempts a retriable operation gets.
	MaxTries int `json:"maxTries,omitempty"`

	// UploadChunkSizeMB is the resumable upload chunk size, in megabytes.
	UploadChunkSizeMB int `json:"uploadChunkSizeMB,omitempty"`

	// SimpleUploadCutoffMB is the size below which a simple (non-resumable) upload is
	// used, in megabytes.
	SimpleUploadCutoffMB int `json:"simpleUploadCutoffMB,omitempty"`

	// ListPageSize is the files.list page size.
	ListPageSize int `json:"listPageSize,omitempty"`

	// DeleteParallelism bounds the number of concurrent delete calls.
	DeleteParallelism int `json:"deleteParallelism,omitempty"`

	// UseBatchDelete selects the batch delete endpoint; nil selects the built-in default.
	UseBatchDelete *bool `json:"useBatchDelete,omitempty"`

	// CacheDir overrides the directory holding the backend's local caches.
	CacheDir string `json:"cacheDir,omitempty"`

	// HTTP2ReadIdleTimeoutSec is how long an HTTP/2 connection may receive
	// nothing before a health-check ping is sent on it, in seconds.
	HTTP2ReadIdleTimeoutSec int `json:"http2ReadIdleTimeoutSec,omitempty"`

	// HTTP2PingTimeoutSec is how long an unanswered health-check ping waits
	// before the HTTP/2 connection is declared dead, in seconds.
	HTTP2PingTimeoutSec int `json:"http2PingTimeoutSec,omitempty"`

	// ResponseHeaderTimeoutSec bounds the wait for response headers after the
	// request, including its body, has been sent, in seconds.
	ResponseHeaderTimeoutSec int `json:"responseHeaderTimeoutSec,omitempty"`

	// IOIdleTimeoutSec is how long a connection may go without reading or
	// writing a single byte before it is closed as stalled, in seconds.
	IOIdleTimeoutSec int `json:"ioIdleTimeoutSec,omitempty"`
}
