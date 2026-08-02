//go:build !no_extra_providers

// Command gdrive-bench is the benchmark and comparison harness for Kopia's
// native Google Drive backend.
//
// It runs a fixed suite of blob.Storage workloads against any number of
// configured backends (native gdrive, rclone-backed, gcs, filesystem, ...),
// interleaving the targets so that a slow network minute cannot be mistaken for
// a slow backend, and writes a machine-readable result document described by
// SCHEMA.md. Companion subcommands compare result documents, guard a promoted
// baseline, enumerate the tunable knobs and promote curated artifacts into the
// repository.
//
// Every subcommand is non-interactive and communicates through its exit code:
//
//	0  success
//	1  a workload, comparison or baseline check failed
//	2  refused to start (bad usage, bad configuration, quota guard)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"

	_ "github.com/kopia/kopia/repo/blob/filesystem"
	_ "github.com/kopia/kopia/repo/blob/gcs"
	_ "github.com/kopia/kopia/repo/blob/gdrive"
	_ "github.com/kopia/kopia/repo/blob/rclone"
)

// Process exit codes. They are part of the harness contract: cron and CI hooks
// switch on them, so they are documented in SCHEMA.md too.
const (
	exitOK      = 0
	exitFailure = 1
	exitRefused = 2
)

// command is one subcommand of the harness.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, out, errOut io.Writer) int
}

// commands returns the subcommand table, in help order.
func commands() []command {
	return []command{
		{"run", "run the workload suite against one or more targets and write a result document", cmdRun},
		{"compare", "compare two or more result documents and flag significant differences", cmdCompare},
		{"baseline", "set or check the promoted baseline result (baseline set|check <file>)", cmdBaseline},
		{"graph", "render a result or comparison document as self-contained SVG charts", cmdGraph},
		{"knobs", "dump every tunable knob, its default, range hint and affected workloads as JSON", cmdKnobs},
		{"publish", "copy a result or report into the repository's curated results directory", cmdPublish},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)

	code := dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr)

	// Not deferred: os.Exit would skip it.
	stop()

	os.Exit(code)
}

// dispatch runs one subcommand and returns the process exit code. It is the
// entry point the tests drive, so it never touches os.Exit itself.
func dispatch(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		usage(errOut)

		return exitRefused
	}

	switch args[0] {
	case "-h", "--help", "help":
		usage(out)

		return exitOK
	}

	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(ctx, args[1:], out, errOut)
		}
	}

	outf(errOut, "unknown subcommand %q\n\n", args[0])
	usage(errOut)

	return exitRefused
}

// usage prints the subcommand table.
func usage(w io.Writer) {
	outln(w, "gdrive-bench - benchmark harness for Kopia's native Google Drive backend")
	outln(w)
	outln(w, "usage: gdrive-bench <subcommand> [flags]")
	outln(w)

	tw := tabwriter.NewWriter(w, 0, 0, tabwriterPadding, ' ', 0)

	for _, c := range commands() {
		outf(tw, "  %v\t%v\n", c.name, c.summary)
	}

	tw.Flush() //nolint:errcheck // best-effort help output.

	outln(w)
	outln(w, "run 'gdrive-bench <subcommand> --help' for the flags of a subcommand.")
	outln(w, "exit codes: 0 ok, 1 failure/regression, 2 refused (usage, config, quota guard).")
}

// tabwriterPadding is the column padding used by every human-readable table the
// harness prints.
const tabwriterPadding = 2

// newFlagSet returns a FlagSet that reports errors to errOut and never calls
// os.Exit, so that dispatch stays testable.
func newFlagSet(name string, errOut io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)

	return fs
}

// parseFlags parses args, returning the exit code to use when parsing failed.
// The boolean is false when the caller should return that code immediately.
func parseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		return exitRefused, false
	}

	return exitOK, true
}

// flagsFirst moves positional arguments behind the flags.
//
// Go's flag package stops parsing at the first non-flag argument, so
// "graph result.json --out-dir x" would otherwise leave --out-dir unset and the
// subcommand would refuse a command line that reads perfectly well. Only the
// subcommands documented with a trailing flag use it.
//
// A flag's value is carried along with it when it is given as two words, which
// is what the FlagSet lookup is for: a boolean flag must not swallow the file
// name that follows it.
func flagsFirst(fs *flag.FlagSet, args []string) []string {
	flags := []string{}
	positional := []string{}

	for i := 0; i < len(args); i++ {
		a := args[i]

		if a == "--" {
			positional = append(positional, args[i+1:]...)

			break
		}

		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)

			continue
		}

		flags = append(flags, a)

		name := strings.TrimLeft(a, "-")
		if strings.ContainsRune(name, '=') {
			continue
		}

		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++

			flags = append(flags, args[i])
		}
	}

	return append(flags, positional...)
}

// isBoolFlag reports whether a flag is one that takes no separate value.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })

	return ok && bf.IsBoolFlag()
}

// outf writes a formatted line to a harness output stream. Every message the
// harness prints goes through it (and outln) so that the deliberate decision to
// ignore write errors on stdout/stderr is made in exactly one place.
func outf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// outln writes a line to a harness output stream.
func outln(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}
