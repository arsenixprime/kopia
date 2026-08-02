//go:build no_extra_providers

// Command gdrive-bench is the benchmark harness for Kopia's native Google Drive
// backend. This build of it does nothing: the harness links the optional
// storage providers (gdrive, gcs, rclone), which are compiled out by the
// no_extra_providers build tag, so the whole tool is stubbed out under that tag
// rather than breaking a tree-wide "go build ./...".
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gdrive-bench was built with the 'no_extra_providers' build tag and has no storage providers to benchmark; rebuild without it.")
	os.Exit(2) //nolint:mnd // 2 is the harness's "refused to start" exit code.
}
