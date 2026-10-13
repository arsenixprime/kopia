//go:build !no_extra_providers

package cli

import (
	"encoding/json"
	"testing"

	"github.com/alecthomas/kingpin/v2"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/blob/gdrive"
)

// parseGDriveFlags wires up the gdrive provider flags the same way 'repository create'
// and 'repository connect' do and parses the given command line against them.
func parseGDriveFlags(t *testing.T, args ...string) *storageGDriveFlags {
	t.Helper()

	c := parseGDriveFlagsRaw(t, append([]string{"--folder-id", "some-folder"}, args...)...)

	// same fold-in that Connect() performs before handing the options to the backend.
	c.applyOptionalTuningFlags()

	return c
}

// parseGDriveFlagsRaw is parseGDriveFlags without the implied --folder-id, for
// the tests that are about how the folder is named in the first place.
func parseGDriveFlagsRaw(t *testing.T, args ...string) *storageGDriveFlags {
	t.Helper()

	c := &storageGDriveFlags{}

	app := kingpin.New("test", "test")
	app.Terminate(nil)

	cmd := app.Command("connect", "").Command("gdrive", "")
	c.Setup(nil, cmd)

	_, err := app.Parse(append([]string{"connect", "gdrive"}, args...))
	require.NoError(t, err)

	return c
}

func TestGDriveTuningFlagsUnsetLeaveTuningEmpty(t *testing.T) {
	c := parseGDriveFlags(t)

	require.Equal(t, "some-folder", c.options.FolderID)

	// no tuning flag given: every knob keeps its zero value, so nothing is written into
	// the connection config (Options.Tuning is tagged omitzero).
	require.Equal(t, gdrive.TuningOptions{}, c.options.Tuning)
	require.Nil(t, c.options.Tuning.UseBatchDelete)

	// which is observable in the serialized connection info.
	j, err := json.Marshal(c.options)
	require.NoError(t, err)
	require.NotContains(t, string(j), "tuning")
}

func TestGDriveTuningFlagsSetOnlyWhatWasGiven(t *testing.T) {
	c := parseGDriveFlags(t, "--list-page-size", "250", "--max-tries", "7")

	require.Equal(t, gdrive.TuningOptions{
		ListPageSize: 250,
		MaxTries:     7,
	}, c.options.Tuning)
	require.Nil(t, c.options.Tuning.UseBatchDelete)
}

func TestGDriveTuningFlagsAll(t *testing.T) {
	c := parseGDriveFlags(t,
		"--pacer-min-sleep-ms", "10",
		"--pacer-burst", "20",
		"--pacer-max-sleep-ms", "30",
		"--max-tries", "40",
		"--upload-chunk-size-mb", "50",
		"--simple-upload-cutoff-mb", "60",
		"--list-page-size", "70",
		"--delete-parallelism", "80",
		"--use-batch-delete", "true",
		"--tuning-cache-dir", "/some/cache/dir",
		"--http2-read-idle-timeout-sec", "90",
		"--http2-ping-timeout-sec", "100",
		"--response-header-timeout-sec", "110",
		"--io-idle-timeout-sec", "120",
	)

	require.Equal(t, gdrive.TuningOptions{
		PacerMinSleepMS:      10,
		PacerBurst:           20,
		PacerMaxSleepMS:      30,
		MaxTries:             40,
		UploadChunkSizeMB:    50,
		SimpleUploadCutoffMB: 60,
		ListPageSize:         70,
		DeleteParallelism:    80,
		UseBatchDelete:       new(true),
		CacheDir:             "/some/cache/dir",

		HTTP2ReadIdleTimeoutSec:  90,
		HTTP2PingTimeoutSec:      100,
		ResponseHeaderTimeoutSec: 110,
		IOIdleTimeoutSec:         120,
	}, c.options.Tuning)
}

func TestGDriveUseBatchDeleteIsTriState(t *testing.T) {
	cases := []struct {
		args []string
		want *bool
	}{
		{nil, nil},
		{[]string{"--use-batch-delete", "true"}, new(true)},
		{[]string{"--use-batch-delete", "false"}, new(false)},
		{[]string{"--use-batch-delete=true"}, new(true)},
		{[]string{"--use-batch-delete=false"}, new(false)},
		// last one on the command line wins.
		{[]string{"--use-batch-delete=true", "--use-batch-delete=false"}, new(false)},
	}

	for _, tc := range cases {
		c := parseGDriveFlags(t, tc.args...)
		require.Equal(t, tc.want, c.options.Tuning.UseBatchDelete, "args: %v", tc.args)
	}
}

// TestGDriveCreateFolderNameSetsTheOption: at create time the flag becomes
// Options.FolderName, which is what makes the backend resolve or create the
// folder. The name is trimmed, because it ends up in a Drive query.
func TestGDriveCreateFolderNameSetsTheOption(t *testing.T) {
	c := parseGDriveFlagsRaw(t, "--create-folder-name", "  my-backups  ")

	require.NoError(t, c.applyFolderFlags(true))
	require.Equal(t, "my-backups", c.options.FolderName)
	require.Empty(t, c.options.FolderID)
}

// TestGDriveFolderFlagsRejected covers every combination the two folder flags
// can be given in that cannot mean anything.
func TestGDriveFolderFlagsRejected(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		isCreate bool
		wantErr  string
	}{
		{
			name:     "neither flag",
			isCreate: true,
			wantErr:  "--folder-id must be specified",
		},
		{
			name:    "neither flag on connect",
			wantErr: "--folder-id must be specified",
		},
		{
			name:     "both flags",
			args:     []string{"--folder-id", "some-folder", "--create-folder-name", "my-backups"},
			isCreate: true,
			wantErr:  "mutually exclusive",
		},
		{
			name:    "create-folder-name on connect",
			args:    []string{"--create-folder-name", "my-backups"},
			wantErr: "only supported by 'kopia repository create'",
		},
		{
			name:     "blank name",
			args:     []string{"--create-folder-name", "   "},
			isCreate: true,
			wantErr:  "must not be blank",
		},
		{
			name:     "read-only",
			args:     []string{"--create-folder-name", "my-backups", "--read-only"},
			isCreate: true,
			wantErr:  "--read-only",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := parseGDriveFlagsRaw(t, tc.args...)

			err := c.applyFolderFlags(tc.isCreate)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
			require.Empty(t, c.options.FolderName, "a rejected command line must not reach the backend")
		})
	}
}

// TestGDriveFolderIDAloneIsAccepted: the ordinary path is untouched, on both
// create and connect, and leaves no folderName in the configuration.
func TestGDriveFolderIDAloneIsAccepted(t *testing.T) {
	for _, isCreate := range []bool{true, false} {
		c := parseGDriveFlagsRaw(t, "--folder-id", "some-folder")

		require.NoError(t, c.applyFolderFlags(isCreate))
		require.Equal(t, "some-folder", c.options.FolderID)
		require.Empty(t, c.options.FolderName)

		j, err := json.Marshal(c.options)
		require.NoError(t, err)
		require.NotContains(t, string(j), "folderName")
	}
}
