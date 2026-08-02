//go:build !no_extra_providers

package cli

import (
	"context"
	"encoding/json"
	"os"

	"github.com/alecthomas/kingpin/v2"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/gdrive"
)

type storageGDriveFlags struct {
	options gdrive.Options

	embedCredentials bool

	// useBatchDelete is an optional boolean. gdrive.TuningOptions.UseBatchDelete is
	// a *bool where nil means "let the backend decide", so the flag must be able to
	// tell "not specified" from "specified false". The CLI does that with a bool
	// list everywhere else it needs a tri-state flag (see 'maintenance set' and
	// 'policy set'): zero elements == not set, otherwise the last value wins.
	useBatchDelete []bool
}

func (c *storageGDriveFlags) Setup(_ StorageProviderServices, cmd *kingpin.CmdClause) {
	cmd.Flag("folder-id", "FolderID to use for objects in the bucket").Required().StringVar(&c.options.FolderID)
	cmd.Flag("read-only", "Use read-only scope to prevent write access").BoolVar(&c.options.ReadOnly)
	cmd.Flag("credentials-file", "Use the provided JSON file with credentials").ExistingFileVar(&c.options.ServiceAccountCredentialsFile)
	cmd.Flag("embed-credentials", "Embed GCS credentials JSON in Kopia configuration").BoolVar(&c.embedCredentials)

	cmd.Flag("client-credentials-file",
		"Use the provided OAuth client secret JSON file (the one with an 'installed' or 'web' section) and sign in interactively. "+
			"Creating your own OAuth client in your own Google Cloud project is recommended: Google charges the per-project "+
			"rate and egress quotas to the project that owns the client ID.").
		ExistingFileVar(&c.options.ClientCredentialsFile)
	cmd.Flag("token-cache-file",
		"Store the refresh token obtained by the interactive sign-in in this file (owner-readable only). "+
			"Defaults to a file in Kopia's configuration directory.").
		StringVar(&c.options.TokenCacheFile)
	cmd.Flag("impersonate-user",
		"Act on behalf of this user using Google Workspace domain-wide delegation. Requires service account credentials. EXPERIMENTAL.").
		StringVar(&c.options.Impersonate)
	cmd.Flag("device-flow",
		"Use the OAuth device-code flow instead of a loopback redirect for the interactive sign-in, for machines that cannot open "+
			"a browser or accept a loopback connection. Google only permits the 'drive.file' scope with this flow.").
		BoolVar(&c.options.UseDeviceFlow)
	cmd.Flag("scope",
		"Google Drive OAuth scope. 'drive.file' (the default) lets Kopia see and modify only the files and folders it created "+
			"itself, so it can never touch the rest of your Drive; the flip side is that a folder you created by hand is invisible "+
			"to Kopia and it will make its own. Use 'drive' only when the repository must live in a pre-existing, externally "+
			"created folder - it grants full access to every file in your Drive.").
		EnumVar(&c.options.Scope, gdrive.ScopeDriveFile, gdrive.ScopeDrive)

	commonThrottlingFlags(cmd, &c.options.Limits)
	c.gdriveTuningFlags(cmd)
}

// gdriveTuningFlags exposes gdrive.TuningOptions on the command line, mirroring the
// knobs that are also settable through the connection config JSON. Every knob keeps
// the JSON semantics: a flag that is not given leaves the corresponding field at its
// zero value, which is what selects the backend default, so an untouched command line
// leaves Options.Tuning entirely empty.
func (c *storageGDriveFlags) gdriveTuningFlags(cmd *kingpin.CmdClause) {
	t := &c.options.Tuning

	cmd.Flag("pacer-min-sleep-ms", "Minimum delay between Google Drive API calls, in milliseconds (advanced tuning).").IntVar(&t.PacerMinSleepMS)
	cmd.Flag("pacer-burst", "Number of Google Drive API calls allowed without any pacing delay (advanced tuning).").IntVar(&t.PacerBurst)
	cmd.Flag("pacer-max-sleep-ms", "Upper bound on the pacer backoff delay, in milliseconds (advanced tuning).").IntVar(&t.PacerMaxSleepMS)
	cmd.Flag("max-tries", "Number of attempts a retriable Google Drive operation gets (advanced tuning).").IntVar(&t.MaxTries)
	cmd.Flag("upload-chunk-size-mb", "Resumable upload chunk size, in megabytes (advanced tuning).").IntVar(&t.UploadChunkSizeMB)
	cmd.Flag("simple-upload-cutoff-mb", "Size below which a simple, non-resumable upload is used, in megabytes (advanced tuning).").IntVar(&t.SimpleUploadCutoffMB)
	cmd.Flag("list-page-size", "Page size used when listing files, up to 1000 (advanced tuning).").IntVar(&t.ListPageSize)
	cmd.Flag("delete-parallelism", "Maximum number of concurrent delete calls (advanced tuning).").IntVar(&t.DeleteParallelism)
	cmd.Flag("use-batch-delete", "Delete blobs through the batch endpoint instead of one call each, 'true' or 'false' (advanced tuning).").PlaceHolder("BOOL").BoolListVar(&c.useBatchDelete)
	cmd.Flag("tuning-cache-dir", "Directory holding the Google Drive backend's file ID cache (advanced tuning).").PlaceHolder("PATH").StringVar(&t.CacheDir)
}

// applyOptionalTuningFlags folds the tuning flags that cannot be bound directly to
// their Options.Tuning field into the options. Only --use-batch-delete needs this:
// it is the one knob whose "unset" is not the field's zero value.
func (c *storageGDriveFlags) applyOptionalTuningFlags() {
	// zero elements == --use-batch-delete not given, in which case UseBatchDelete stays
	// nil and the backend picks; otherwise the last value on the command line wins.
	if n := len(c.useBatchDelete); n > 0 {
		v := c.useBatchDelete[n-1]
		c.options.Tuning.UseBatchDelete = &v
	}
}

func (c *storageGDriveFlags) Connect(ctx context.Context, isCreate bool, formatVersion int) (blob.Storage, error) {
	_ = formatVersion

	c.applyOptionalTuningFlags()

	if c.embedCredentials {
		data, err := os.ReadFile(c.options.ServiceAccountCredentialsFile)
		if err != nil {
			return nil, errors.Wrap(err, "unable to open service account credentials file")
		}

		c.options.ServiceAccountCredentialJSON = json.RawMessage(data)
		c.options.ServiceAccountCredentialsFile = ""
	}

	//nolint:wrapcheck
	return gdrive.New(ctx, &c.options, isCreate)
}

func init() {
	mustRegisterStorageProvider(
		"gdrive",
		"a Google Drive folder",
		func() StorageFlags { return &storageGDriveFlags{} },
	)
}
