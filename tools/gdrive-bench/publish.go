//go:build !no_extra_providers

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pkg/errors"
)

// curatedResultsDir is where a promoted artifact lands, relative to the
// repository root.
const curatedResultsDir = "tools/gdrive-bench/results"

// publishableExtensions are the file types the curated directory accepts: run
// results and comparison reports (.json) and hand-written analyses (.md).
var publishableExtensions = []string{".json", ".md"}

// cmdPublish implements "gdrive-bench publish".
func cmdPublish(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("publish", errOut)
	as := fs.String("as", "", "file name to publish under (default: the source file's name)")
	dest := fs.String("dest", "", "destination directory (default: <repo root>/"+curatedResultsDir+")")
	force := fs.Bool("force", false, "overwrite an existing curated artifact")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	if fs.NArg() != 1 {
		outln(errOut, "usage: gdrive-bench publish <file.json|file.md> [--as=name] [--dest=dir] [--force]")

		return exitRefused
	}

	src := fs.Arg(0)

	dstDir, err := resolvePublishDir(ctx, *dest)
	if err != nil {
		outf(errOut, "gdrive-bench publish: %v\n", err)

		return exitRefused
	}

	dst, err := publishOne(src, dstDir, *as, *force)
	if err != nil {
		outf(errOut, "gdrive-bench publish: %v\n", err)

		return exitFailure
	}

	outf(out, "published: %v\n", dst)

	return exitOK
}

// resolvePublishDir finds the curated results directory.
func resolvePublishDir(ctx context.Context, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	root, ok := repoRoot(ctx)
	if !ok {
		return "", errors.New("not inside a git work tree; pass --dest to name the curated results directory explicitly")
	}

	return filepath.Join(root, curatedResultsDir), nil
}

// publishOne copies one artifact into the curated directory.
func publishOne(src, dstDir, as string, force bool) (string, error) {
	name := as
	if name == "" {
		name = filepath.Base(src)
	}

	if name != filepath.Base(name) {
		return "", errors.Errorf("--as must be a file name, not a path: %v", as)
	}

	ext := strings.ToLower(filepath.Ext(name))
	if !contains(publishableExtensions, ext) {
		return "", errors.Errorf("refusing to publish %v: only %v are curated artifacts", name, strings.Join(publishableExtensions, ", "))
	}

	if _, err := os.Stat(src); err != nil {
		return "", errors.Wrapf(err, "unable to read %v", src)
	}

	if err := os.MkdirAll(dstDir, publishDirPermissions); err != nil {
		return "", errors.Wrapf(err, "unable to create %v", dstDir)
	}

	dst := filepath.Join(dstDir, name)

	if _, err := os.Stat(dst); err == nil && !force {
		return "", errors.Errorf("%v already exists; pass --force to replace it", dst)
	}

	if err := copyFile(src, dst); err != nil {
		return "", err
	}

	// Curated artifacts are committed, so they get ordinary file permissions
	// rather than the owner-only mode the results directory uses.
	if err := os.Chmod(dst, publishFilePermissions); err != nil {
		return "", errors.Wrapf(err, "unable to set the permissions of %v", dst)
	}

	return dst, nil
}

// Permissions for artifacts that end up in the repository.
const (
	publishFilePermissions = 0o644
	publishDirPermissions  = 0o755
)

// contains reports whether s contains v.
func contains(s []string, v string) bool {
	return slices.Contains(s, v)
}
