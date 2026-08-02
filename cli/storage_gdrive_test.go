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

	c := &storageGDriveFlags{}

	app := kingpin.New("test", "test")
	app.Terminate(nil)

	cmd := app.Command("connect", "").Command("gdrive", "")
	c.Setup(nil, cmd)

	_, err := app.Parse(append([]string{"connect", "gdrive", "--folder-id", "some-folder"}, args...))
	require.NoError(t, err)

	// same fold-in that Connect() performs before handing the options to the backend.
	c.applyOptionalTuningFlags()

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
		UseBatchDelete:       ptrTo(true),
		CacheDir:             "/some/cache/dir",
	}, c.options.Tuning)
}

func TestGDriveUseBatchDeleteIsTriState(t *testing.T) {
	cases := []struct {
		args []string
		want *bool
	}{
		{nil, nil},
		{[]string{"--use-batch-delete", "true"}, ptrTo(true)},
		{[]string{"--use-batch-delete", "false"}, ptrTo(false)},
		{[]string{"--use-batch-delete=true"}, ptrTo(true)},
		{[]string{"--use-batch-delete=false"}, ptrTo(false)},
		// last one on the command line wins.
		{[]string{"--use-batch-delete=true", "--use-batch-delete=false"}, ptrTo(false)},
	}

	for _, tc := range cases {
		c := parseGDriveFlags(t, tc.args...)
		require.Equal(t, tc.want, c.options.Tuning.UseBatchDelete, "args: %v", tc.args)
	}
}

func ptrTo[T any](v T) *T {
	return &v
}
