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
}

func (c *storageGDriveFlags) Connect(ctx context.Context, isCreate bool, formatVersion int) (blob.Storage, error) {
	_ = formatVersion

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
