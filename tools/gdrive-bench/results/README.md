# Curated benchmark artifacts

This directory holds **only curated, hand-promoted artifacts**. Nothing writes
here automatically.

Raw benchmark output never lives in the repository. `gdrive-bench run` writes
its results to `$KOPIA_GDRIVE_BENCH_RESULTS` (see `../SCHEMA.md`) and refuses to
start if that directory is inside the git work tree. A result reaches this
directory only when a human decides it is worth keeping, and only through:

```
gdrive-bench publish <file.json|file.md> [--as <name>] [--force]
```

which copies the named file here (`.json` and `.md` only).

What belongs here:

- **A reference copy of the output contract** — the frozen `../SCHEMA.md` is the
  authority; publish a copy alongside an artifact only if a reader would
  otherwise have to guess which version produced it.
- **Comparison reports** — the JSON written by `gdrive-bench compare --json-out`,
  plus any hand-written `.md` analysis of them.
- **The recommended baseline** — the run result promoted with
  `gdrive-bench baseline set`, published here under a descriptive name so the
  numbers a decision was based on stay in the history.

What does not belong here: every trial you ran, exploratory sweeps, anything
containing credentials (the harness redacts credential-bearing config keys, but
check before publishing), and anything larger than a few hundred kilobytes.

Each published artifact should be accompanied by a line in the commit message,
or a short `.md` next to it, saying what question it answers.
