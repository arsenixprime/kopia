//go:build !no_extra_providers

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/pkg/errors"
)

// unknownValue is what the harness records when it cannot determine a value.
const unknownValue = "unknown"

// resultsDirEnvVar names the directory every result, the baseline and the quota
// ledger live in.
const resultsDirEnvVar = "KOPIA_GDRIVE_BENCH_RESULTS"

// resolveResultsDir picks the results directory and makes sure it exists.
//
// Results NEVER go inside the repository: a benchmark artifact is measurement
// data, not source, and an accidental "git add ." must not be able to sweep a
// few megabytes of JSON into a commit. Only "publish" puts a hand-picked file
// into the tree.
func resolveResultsDir(ctx context.Context, override string) (string, error) {
	dir := override
	if dir == "" {
		dir = os.Getenv(resultsDirEnvVar)
	}

	if dir == "" {
		// Zero-configuration fallback, which is what makes "--smoke" work in CI
		// with no environment at all.
		dir = filepath.Join(os.TempDir(), defaultResultsDirNameSuffix)
	}

	dir, err := expandHome(dir)
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", errors.Wrapf(err, "unable to resolve the results directory %v", dir)
	}

	if root, ok := repoRoot(ctx); ok && isInside(root, abs) {
		return "", errors.Errorf("the results directory %v is inside the repository at %v; point %v somewhere else (results are never stored in the tree)", abs, root, resultsDirEnvVar)
	}

	if err := os.MkdirAll(abs, dirPermissions); err != nil {
		return "", errors.Wrapf(err, "unable to create the results directory %v", abs)
	}

	return abs, nil
}

// expandHome expands a leading "~/" using the current user's home directory,
// because the environment file the operator sources writes results paths that
// way.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~"+string(os.PathSeparator)) {
		return p, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(err, "unable to determine the home directory")
	}

	if p == "~" {
		return home, nil
	}

	return filepath.Join(home, p[2:]), nil
}

// isInside reports whether path is root or lives under it.
func isInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// repoRoot returns the root of the git work tree of the current directory.
func repoRoot(ctx context.Context) (string, bool) {
	out, err := runGit(ctx, "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return "", false
	}

	return out, true
}

// runGit runs a read-only git command in the current directory and returns its
// trimmed stdout.
func runGit(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)

	out, err := cmd.Output()
	if err != nil {
		return "", errors.Wrapf(err, "git %v failed", strings.Join(args, " "))
	}

	return strings.TrimSpace(string(out)), nil
}

// harnessInfo identifies the build that produced a result. The commit is read
// with git at startup and embedded in the document, so a number can always be
// traced back to the code that produced it; a binary running outside a checkout
// falls back to the VCS stamp the Go toolchain embeds.
func harnessInfo(ctx context.Context, override string) HarnessInfo {
	h := HarnessInfo{
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Commit:    override,
	}

	if hn, err := os.Hostname(); err == nil {
		h.Hostname = hn
	}

	if h.Commit != "" {
		return h
	}

	if rev, err := runGit(ctx, "rev-parse", "HEAD"); err == nil {
		h.Commit = rev

		if status, err := runGit(ctx, "status", "--porcelain"); err == nil && status != "" {
			h.Dirty = true
		}

		return h
	}

	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				h.Commit = s.Value
			case "vcs.modified":
				h.Dirty = s.Value == "true"
			}
		}
	}

	if h.Commit == "" {
		h.Commit = unknownValue
	}

	return h
}
