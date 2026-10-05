//go:build !no_extra_providers

package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/gdrive"
)

type storageGDriveFlags struct {
	options gdrive.Options

	embedCredentials bool

	// createFolderName holds --create-folder-name. It is not bound directly to
	// gdrive.Options.FolderName because the two are not the same thing: the flag
	// is a request to make a folder, which only 'repository create' can honor,
	// while the option is also what a configuration written by an earlier create
	// carries forever afterwards. Keeping them apart is what lets Connect tell a
	// user asking for a new folder from a machine reconnecting to an old one.
	createFolderName string

	// useBatchDelete is an optional boolean. gdrive.TuningOptions.UseBatchDelete is
	// a *bool where nil means "let the backend decide", so the flag must be able to
	// tell "not specified" from "specified false". The CLI does that with a bool
	// list everywhere else it needs a tri-state flag (see 'maintenance set' and
	// 'policy set'): zero elements == not set, otherwise the last value wins.
	useBatchDelete []bool
}

// Setup registers the provider's flags. It is called once per command that can
// open a storage - 'repository create', 'connect', 'sync-to' and 'repair' - and
// is given no way to tell them apart; the create/connect distinction arrives
// later, as the isCreate argument of Connect. --create-folder-name is therefore
// registered everywhere and refused at Connect time on every command but
// create, which is the same shape as --point-in-time on the azure, s3 and gcs
// providers.
//
// --folder-id consequently cannot stay Required(): a create that names a folder
// to make does not have an ID to give. The requirement is enforced in Connect
// instead, where it can say which of the two flags is missing.
func (c *storageGDriveFlags) Setup(_ StorageProviderServices, cmd *kingpin.CmdClause) {
	cmd.Flag("folder-id", "FolderID to use for objects in the bucket").StringVar(&c.options.FolderID)
	cmd.Flag("create-folder-name",
		"Create (or reuse) a folder of this name in the root of My Drive and put the repository in it, instead of naming an existing "+
			"folder with --folder-id. Only valid when creating a repository. The resulting folder ID is printed and saved in the "+
			"configuration; other machines connect to the repository with --folder-id.").
		PlaceHolder("NAME").StringVar(&c.createFolderName)
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
	cmd.Flag("http2-read-idle-timeout-sec", "Seconds an HTTP/2 connection may receive nothing before it is health-checked with a ping (advanced tuning).").IntVar(&t.HTTP2ReadIdleTimeoutSec)
	cmd.Flag("http2-ping-timeout-sec", "Seconds to wait for a health-check ping reply before an HTTP/2 connection is declared dead (advanced tuning).").IntVar(&t.HTTP2PingTimeoutSec)
	cmd.Flag("response-header-timeout-sec", "Seconds to wait for response headers once a request has been fully sent (advanced tuning).").IntVar(&t.ResponseHeaderTimeoutSec)
	cmd.Flag("io-idle-timeout-sec", "Seconds a connection may go without reading or writing anything before it is closed as stalled (advanced tuning).").IntVar(&t.IOIdleTimeoutSec)
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

// applyFolderFlags validates the two mutually exclusive ways of naming the
// repository folder and folds --create-folder-name into the options.
func (c *storageGDriveFlags) applyFolderFlags(isCreate bool) error {
	if c.createFolderName == "" {
		if c.options.FolderID == "" {
			return errors.New("--folder-id must be specified")
		}

		return nil
	}

	switch {
	case !isCreate:
		return errors.New("--create-folder-name is only supported by 'kopia repository create': connect to an existing repository with --folder-id")

	case c.options.FolderID != "":
		return errors.New("--create-folder-name and --folder-id are mutually exclusive: the first makes a folder, the second names one that exists")

	case strings.TrimSpace(c.createFolderName) == "":
		return errors.New("--create-folder-name must not be blank")

	case c.options.ReadOnly:
		return errors.New("--create-folder-name cannot be combined with --read-only: a read-only connection cannot create anything")
	}

	c.options.FolderName = strings.TrimSpace(c.createFolderName)

	return nil
}

func (c *storageGDriveFlags) Connect(ctx context.Context, isCreate bool, formatVersion int) (blob.Storage, error) {
	_ = formatVersion

	c.applyOptionalTuningFlags()

	if err := c.applyFolderFlags(isCreate); err != nil {
		return nil, err
	}

	if c.embedCredentials {
		data, err := os.ReadFile(c.options.ServiceAccountCredentialsFile)
		if err != nil {
			return nil, errors.Wrap(err, "unable to open service account credentials file")
		}

		c.options.ServiceAccountCredentialJSON = json.RawMessage(data)
		c.options.ServiceAccountCredentialsFile = ""
	}

	st, err := gdrive.New(ctx, &c.options, isCreate)
	if err != nil {
		//nolint:wrapcheck
		return nil, err
	}

	if c.options.FolderName != "" {
		// gdrive.New has resolved the name into a concrete folder ID, which is the
		// only thing another machine needs in order to connect to this repository.
		// It is saved in the configuration, but a user who has just created the
		// repository should not have to go looking for it there.
		log(ctx).Infof("Google Drive folder %q has ID %v - connect other machines with: kopia repository connect gdrive --folder-id %v",
			c.options.FolderName, c.options.FolderID, c.options.FolderID)
	}

	return st, nil
}

func init() {
	mustRegisterStorageProvider(
		"gdrive",
		"a Google Drive folder",
		func() StorageFlags { return &storageGDriveFlags{} },
	)
}
