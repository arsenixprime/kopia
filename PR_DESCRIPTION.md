# PR description — native Google Drive backend rewrite

> This file is a **fork-only artifact**. It is the text to paste into the
> upstream pull request, plus the appendix that says how to build the upstream
> branch. It must not be part of the branch that is proposed to kopia/kopia.

---

## Title

```
feat(providers): rewrite the native Google Drive backend
```

The title must match `.github/workflows/check-pr-title.yml`
(`^(feat|fix|breaking|build|chore|docs|style|refactor|test)\((ci|cli|deps-dev|deps|general|infra|kopiaui|lint|notifications|providers|repository|server|site|snapshots|testing|ui)\)!{0,1}: .*$`),
and `providers` is one of the scopes `.chglog/config.yml` keeps, so the PR will
appear in the generated changelog under "Storage Providers".

---

## Summary

The `gdrive` backend is rewritten. It was registered as `[Not maintained]`,
logged "not actively tested, it may cause data loss, use at your own risk" on
every open, could not pass `kopia repository validate-provider`, and had a
silent blob-dropping bug in `ListBlobs`. This replaces it with a backend that
passes the standard provider suite (including the concurrency and
conditional-create validations) against real Google Drive, adds the two
authentication modes users have been asking for since 2023, and classifies
Drive's error space so that a permanent failure stops instead of being retried
ten times.

Three long-standing issues are fixed and one closed-but-symptomatic issue is
addressed:

- Closes #2656 — *GDrive Delegate domain-wide authority / Service Account
  Quota* (open). Adds `--impersonate-user` (domain-wide delegation) and
  `--client-credentials-file` (sign in as a real user), and classifies 403
  `storageQuotaExceeded` as permanent so it fails immediately with an
  actionable message instead of "Despite 10 retries".
- Closes #3788 — *Trying to create a gdrive repository (PutBlob with
  DoNotRecreate)* (open). `validate-provider` now passes its conditional-create
  check against real Drive.
- Closes #4272 — *Read error when validating concurrency in new Google Drive
  repo* (open). `validate-provider`'s 30-second concurrency soak now passes
  against real Drive.
- References #2344 — *Slow backup process on GDrive*. This issue is **closed**
  (2022-11-21, as completed) but its symptom — the backend being bound by
  `files.list` name-resolution queries rather than by bytes — was never fixed
  and is the thing this rewrite is mostly about. It is referenced, not closed.

Issue states above were verified with `gh issue view` on 2026-08-02.

---

## Architecture

1. **`repo/blob/gdrive/gdriveerr`** — a structured taxonomy that maps a Drive
   response to a *disposition* (`RetryableFast`, `RetryableResume`,
   `NonRetryableOther`, `FatalQuota`, …). It keys on the `(HTTP status, reason)`
   pair, because Drive reuses reason strings across statuses
   (`rateLimitExceeded` appears under both 403 and 429) and uses `notFound`
   rather than `fileNotFound`. It also covers the `oauth2.RetrieveError` cases
   (`invalid_grant`) and the live-observed `accessNotConfigured`.
2. **`repo/blob/gdrive/gdrivepacer`** — an adaptive rate limiter modelled on
   rclone's Drive pacer (100 ms minimum sleep, burst 100, 16 s-capped jittered
   backoff, instant recovery). **The pacer, not `repo/blob/retrying`, is the
   retry layer for this backend**: `retrying` cannot see a quota-exhausted
   sentinel and cannot honour a `Retry-After` header, so wrapping this backend
   in it would reintroduce the ten-attempt storms.
3. **`gdrive_transport.go`** — a counting `http.RoundTripper` that records
   per-Drive-method call counts, bytes, latency histograms, backoffs and error
   reasons. This is what makes any performance claim about this backend
   falsifiable.
4. **`gdrive_auth.go`** — three authentication modes behind one credential
   sniffer: service account, "authorized user" refresh token, and an
   interactive OAuth sign-in (loopback redirect, or device code) against a
   user-supplied OAuth client. The historical `--credentials-file` accepts
   both service-account and authorized-user JSON, selected by the document's
   `type` key.
5. **`drive.file` is the default scope.** Kopia asks for access only to the
   files it created, which is a non-sensitive scope and needs no Google app
   verification or security assessment. `--scope=drive` is the opt-out for
   repositories that must live in a pre-existing external folder.
6. **`gdrive_fileids.go`** — a persistent, process-shared blob-name → file-ID
   cache: a CRC32C-checksummed append-only journal guarded by `flock` and
   `O_APPEND`, with a `SameFile` reopen guard, plus a refcounted shared-instance
   registry so that several `blob.Storage` connections in one process share one
   map. This is the direct fix for #4272 (cross-connection read-your-writes) and
   the main fix for #2344 (a `files.get` by ID costs 5 quota units where a
   `files.list` by name costs 100).
7. **`PutBlob` with `DoNotRecreate`** is a create-then-dedupe protocol:
   create, then list by name ordered by creation time, keep the oldest, delete
   the loser, and return `blob.ErrBlobAlreadyExists`. Drive has no
   conditional-create precondition, so this is emulation; it was chosen over the
   sanctioned `ErrUnsupportedPutBlobOption` because it survives
   `validate-provider`'s concurrency checks against real Drive, which the old
   racy read-then-write did not.
8. **`ListBlobs` is rewritten.** The old prefixed branch dropped
   `trashed = false`, used `name contains` (a substring match) with a 26-char
   prefix cap and the default page size of 100, and — the serious one —
   `return nil`ed on the first non-matching entry of a page, which made
   `Pages()` treat the page as consumed and **silently drop every remaining blob
   on it**. The new implementation uses server-side name narrowing for prefixes
   of four characters or more, `pageSize=1000`, a tight `fields` mask, and an
   exact-name client-side filter (Drive's `name =` is case-insensitive, so a
   server-side match is not sufficient to identify a blob).
9. **Deletes are permanent and bounded-parallel.** Trashed items count against
   Drive's 500,000-items-per-folder and per-Shared-Drive caps, so trashing is
   not an option for a backup tool. Batch delete is implemented behind a knob
   and defaults **off**, because Drive runs the parts of a homogeneous batch
   serially: measured live, parallel singles do 19.5–23 blobs/s against batch's
   7.6–8.6.
10. **`Options` grows a `tuning` object** (pacer timings, upload chunk size and
    cutoff, list page size, delete parallelism, batch-delete toggle, cache
    directory) whose zero value selects the built-in defaults. Every default has
    written provenance.

---

## Evidence

### There is no "before" benchmark, and why

The old backend was never benchmarked, and it cannot be benchmarked
side-by-side now: it does not pass `validate-provider`, and its `ListBlobs`
silently drops blobs, so any throughput number measured against it would be a
number for an incorrect traversal. The honest evidence base is therefore two
things: a controlled comparison against the alternative users are actually
told to use today (Kopia's rclone backend pointed at the same Drive), and a
defect-class table for the things that were broken rather than slow.

### Measured: native backend vs. the same Kopia driving the same Drive through rclone

Five trials per lane, ABBA-interleaved (`A B / B A / A B / B A / A B`) with 10 s
cooldowns, 2 h 46 min, 2.71 GB uploaded, zero errors and zero retries in any
trial. **Both lanes use the same Google account, the same OAuth client and the
same `drive.file` scope**, each writing into its own app-created folder, so the
network, machine, quota bucket and permissions are held constant and the backend
is the only variable. `REAL` means the two `[p10..p90]` intervals do not overlap.

| Workload | Native (median) | Rclone (median) | Rclone vs native | Flag |
| --- | --- | --- | --- | --- |
| `small-write` (100 × 2–8 KB) | 120.9 s | 199.0 s | +64.6% | REAL |
| `pack-write` (5 × 20 MB) | 19.0 s | 30.3 s | +59.0% | REAL |
| `read-metadata:hot` (50) | 7.10 s | 32.7 s | +359.6% | REAL |
| `read-metadata:cold` (50) | 9.08 s | 32.3 s | +255.2% | REAL |
| `range-read` (50 ranges) | 27.3 s | 99.9 s | +266.6% | REAL |
| `list-heavy:full` (5 traversals) | 1.04 s | 38.5 s | +3598.0% | REAL † |
| `list-heavy:prefixed` (5) | 991 ms | 3.01 s | +203.5% | REAL |
| `delete-sweep` (50 deletes, par. 8) | 2.38 s | 2.99 s | +25.5% | REAL |
| `snapshot-e2e:create` | 12.9 s | 28.0 s | +117.7% | REAL |
| `snapshot-e2e:init` (150 MB / 800 files) | 48.6 s | 66.2 s | +36.2% | REAL |
| `snapshot-e2e:incremental` | 12.6 s | 20.9 s | +65.7% | REAL |
| `snapshot-e2e:verify` (`--verify-files-percent=100`) | 77.3 s | 251.2 s | +224.8% | REAL |
| `snapshot-e2e:maintenance` (`--full --safety=none`) | 32.7 s | 55.5 s | +69.8% | REAL |
| **End-to-end cycle (sum of the five stages)** | **184 s** | **422 s** | **2.3x** | REAL |

† The `list-heavy:full` ratio is inflated and should not be quoted as a
headline. Kopia's rclone/WebDAV path stores blobs in a sharded directory tree
and never prunes emptied shard directories, so the rclone lane's traversal got
steadily more expensive within the run (3.2 s → 64.6 s across the five trials)
while native stayed at 1.0 s. Trial 1's ~3x is the defensible steady-state
number. The report says this in its own text rather than hiding it.

Two more caveats carried over from the report: the quota-unit columns are **not
comparable between lanes** (the harness only instruments the native lane's HTTP
transport; rclone's traffic goes through a child process and is recorded as
zero, which is an instrumentation artifact, not a saving), and the
`snapshot-e2e` byte counts are derived from the source tree size rather than
measured. Also, the `read-metadata:warm` row in the published report is
withdrawn by an erratum: the harness's warm-pass recycle called `FlushCaches`,
which contractually wipes the on-disk journal, so ":warm" measured a second cold
pass. After the harness fix a live re-run shows `:warm ≡ :hot`
(`files.get`-dominated, 250 quota units against 5,000 for the same 50 blobs).
The rclone comparison is unaffected — both lanes ran the same flow.

And the number this backend should be judged on rather than congratulated for:
a 2–8 KB blob write still costs **1.21 s**, because every `PutBlob` pays a
`files.list` name lookup before the upload. Beating rclone's 1.91 s is not the
same as being fast.

### Defect-class fixes

| Behaviour | Before | After |
| --- | --- | --- |
| `kopia repository validate-provider` | fails at "Validating conditional creates" (#3788) and at the 30 s concurrency soak (#4272) | passes, 56 s including the concurrency soak, against real Drive |
| 403 `storageQuotaExceeded` | not classified permanent → 10 retries with exponential backoff before surfacing (`Despite 10 retries: …`) (#2656) | classified `FatalQuota`, fails immediately with a message naming the cause and the three fixes |
| `ListBlobs` on a page with a non-matching entry | `return nil` aborts the page; every later blob on it is silently dropped | every entry is examined; exact-name filter applied client-side |
| `ListBlobs` with a prefix | drops `trashed = false`; trashed files can be reported as live blobs | `trashed = false` in every query variant |
| Blob name → file ID | in-memory, per-connection, lost on restart, `files.list` (100 quota units) per resolution | persistent shared journal, `files.get` (5 quota units) on a hit; 50 metadata reads cost 250 quota units hot against 5,000 cold |
| Service-account-owned files | undocumented; the ~15 GB service-account quota is hit with no explanation | loud startup warning naming the three fixes, plus `--impersonate-user` and interactive user sign-in as real alternatives |
| Absent-blob lookup | ~5.35 s | 0.48 s |
| Duplicate names in the folder | "Multiple files found for blob id", effectively unrecoverable | deduped deterministically (oldest wins, loser deleted), warned about, and prevented at the source by the conditional-create protocol |
| Backend self-description | `[Not maintained]`, plus a "may cause data loss, use at your own risk" warning on every open | neither; covered by the provider suite |

### Tests

- 146 test functions across the three packages — 95 in `repo/blob/gdrive`, 24 in
  `gdriveerr`, 27 in `gdrivepacer` — all passing under `-race`.
- The mock suite runs against a **fake Drive server** (`gdrive_fake_server_test.go`,
  ~1,300 lines) reached through a test seam that replaces only the
  OAuth-authenticated transport and the API endpoint, so instrumentation, pacer,
  ID cache and name resolution are the production code paths. It runs
  `blobtesting.VerifyStorage`, `blobtesting.AssertConnectionInfoRoundTrips`,
  `providervalidation.ValidateProvider` (five independent connections, exactly
  the #3788/#4272 shape) and `blobtesting.VerifyConcurrentAccess`.
- The file-ID journal's multi-process guarantees are tested by **re-executing
  the test binary**, so the `flock`/`O_APPEND`/`SameFile` guards are proven
  load-bearing rather than assumed.
- Live, against real Google Drive: `kopia repository validate-provider` →
  "All good." in 56 s including the 30 s concurrency test. A full end-to-end
  cycle: initial snapshot of 335 MB / 1,200 files in 71 s, incremental (+50
  files) in 14 s, `snapshot verify --verify-files-percent=100` (368 MB
  downloaded) in 117 s, byte-identical restore, full maintenance in 13.5 s.
- `golangci-lint` 2.6.1 clean; builds and vets under both the default and
  `no_extra_providers` tag sets.

---

## Compatibility

- **`ConnectionInfo` JSON is byte-compatible.** Every existing key keeps its
  name, type and meaning: `folderID`, `credentialsFile`, `credentials`
  (still tagged `kopia:"sensitive"`), `readOnly`, and the inlined
  `throttling.Limits` keys. An existing `repository.config` or repository token
  written by the current release deserializes unchanged.
- **All new options are additive and `omitempty`**: `clientCredentialsFile`,
  `tokenCacheFile`, `impersonate`, `useDeviceFlow`, `scope`, `tuning`. A config
  written by this backend without any of them set is byte-identical to one
  written by the old one.
  - `tuning` is tagged **`omitzero`, not `omitempty`**, on purpose: `omitempty`
    is inert on a struct field and would make every serialized connection info
    grow an empty `"tuning":{}`.
- **Existing CLI invocations keep working.** `--folder-id`, `--credentials-file`,
  `--embed-credentials`, `--read-only` and the common throttling flags are
  unchanged. `--credentials-file` now additionally accepts an authorized-user
  JSON; it previously accepted only a service-account key.
- **No default changed.** The old backend already requested
  `drive.DriveFileScope`, overridden by `drive.DriveReadonlyScope` when
  `--read-only` was set (old `gdrive_storage.go:516-519`). The new backend
  requests exactly the same scopes for the same inputs; `--scope=drive` is a new
  opt-*out* that did not previously exist, and `ReadOnly` still takes precedence
  over it.

### Breaking-behaviour notes

- **Deletes are permanent — and they already were.** The old backend called
  `files.delete` (`gdrive.client.Delete(fileID)`), which is a permanent delete,
  not a move to trash. This is **not a behaviour change**; it is called out
  because the docs now state it explicitly and because Drive's item caps count
  trashed items, which is why it must stay that way.
- **`ListBlobs` now returns blobs it previously dropped.** This is a bug fix,
  but a repository maintained by the old backend may have had blobs that
  maintenance never saw. The first maintenance run after upgrading may
  therefore do more work than expected. No data is at risk; the old behaviour
  was the risky one.
- **A duplicate-named pair of files in the repository folder is now resolved
  automatically** (oldest kept, newer deleted, warning logged) instead of
  failing with "Multiple files found for blob id". If the old backend left such
  a pair behind — which #3788 caused — upgrading will silently clean it up.
- **`repo/blob/gdrive/file_id_cache.go` is deleted.** It was an unexported
  in-memory type with no consumer outside the package.

---

## What is deliberately not in this PR

- **The benchmark harness.** Every number above was produced by a
  `tools/gdrive-bench` harness (workload definitions, ABBA interleaving, a
  frozen result schema, a significance rule, baseline promotion and checking, a
  quota ledger) that lives in the fork and is **not** proposed here. It is a
  large tree, it needs live Google credentials to be useful, and whether Kopia
  wants a provider-specific benchmark tool in-tree is a separate question from
  whether it wants a working Drive backend. Happy to open it as a follow-up if
  there is appetite.
- **Domain-wide delegation is experimental and not live-validated.** The code
  path is complete and unit-tested, but exercising it requires a Workspace
  super-admin to authorize the service account's client ID for the scope. It is
  marked EXPERIMENTAL in the flag help, the option comment and the user docs.
  If a reviewer has a Workspace domain, this is the one thing worth testing.
- **The Google Picker (`trigger_onepick`) flow**, which is Google's documented
  way for a desktop app to be handed a pre-existing folder under `drive.file`.
  Without it, a `drive.file` repository must live in a folder Kopia created,
  and Kopia does not create the repository folder — so users pointing at a
  hand-made folder need `--scope=drive`. The docs say so plainly. This is the
  most valuable follow-up.

## For reviewer attention

- **`gdrive_storage.go:254`, `var testDriveTransports sync.Map`** — a
  package-level test seam, keyed by folder ID, that lets tests substitute the
  transport and endpoint. It is always empty in production and carries a
  `//nolint:gochecknoglobals`. It is the mechanism that lets the mock suite
  exercise the production code path rather than a parallel one, but it is a
  global, and if the project would rather have an unexported constructor
  parameter or an options field, say so and it will be reworked.
- **Exported diagnostics with no in-tree consumer.** `NewTransportStats`,
  `StatsFromStorage`, `TransportStatsSnapshot` and `MethodStats` are exported
  because the (fork-only) benchmark harness reads them. With the harness out of
  the PR, `StatsFromStorage` and `TransportStatsSnapshot` have no consumer in
  the proposed tree. They can be unexported on request; they are left exported
  because per-method Drive call counts are genuinely useful when diagnosing a
  user's slow repository.
- **The pacer replaces `repo/blob/retrying` for this backend**, which is a
  deliberate deviation from what most providers do. The reasoning is in the
  package comment: `retrying` cannot distinguish a permanent quota failure from
  a transient one and cannot honour `Retry-After`.
- **Unknown Drive errors default to `RetryableResume`**, i.e. the taxonomy is
  optimistic about reasons it has never seen. The alternative — failing closed
  on anything unrecognised — was rejected because Drive adds reason strings
  without notice.

---

# Appendix A — building the upstream branch

**Nothing in this appendix has been executed.** It is the plan; run it
deliberately, on a fresh branch, with the fork branch left untouched.

## A.1 Fork-only paths that must never reach the upstream branch

```
RECON.md
PROJECT_STATE.md
PHASE2_DESIGN.md
PR_DESCRIPTION.md
tools/gdrive-bench/**            (the entire tree, including results/ and TUNING.md)
```

Verify with `git diff --stat upstream/master..<branch>` that no path outside
this set appears:

```
repo/blob/gdrive/**
cli/storage_gdrive.go
site/content/docs/Repositories/_index.md
site/content/docs/_index.md
site/content/docs/Features/_index.md
```

(The last two only gain a "Google Drive" bullet in their storage-location
lists, which omitted the native backend while it was marked
`[Not maintained]`.)

## A.2 Commit-by-commit disposition of `61e611ef..HEAD`

| Commit | Subject | Disposition |
| --- | --- | --- |
| `61e611ef` | project: recon report, project state, phase 2 design contracts | **DROP** — fork-only docs |
| `861eabe1` | feat(gdrive): structured error taxonomy | **KEEP** |
| `d416be9a` | feat(gdrive): adaptive pacer | **KEEP** |
| `07d714af` | feat(gdrive): instrumented HTTP transport | **KEEP** |
| `2c282639` | feat(gdrive): three auth modes | **KEEP** (touches `cli/storage_gdrive.go`) |
| `4c72c7bc` | feat(gdrive): persistent blob-name to file-ID cache | **KEEP** |
| `6da6bf7b` | feat(gdrive): rewrite storage backend | **KEEP** |
| `62f35a2d` | perf(gdrive): bulk deletes, list narrowing, fast not-found path | **KEEP** |
| `a1ced232` | project: phase 2 gate passed | **DROP** — `PROJECT_STATE.md` only |
| `36fb2b7d` | feat(tools): gdrive-bench harness | **DROP** — `tools/gdrive-bench` only |
| `339ffd7d` | feat(tools): snapshot-e2e workload and first comparison | **DROP** — `tools/gdrive-bench` only |
| `1280815f` | project: phase 3 gate passed | **DROP** — `PROJECT_STATE.md` only |
| `3239593a` | fix(tools): warm-pass recycle no longer wipes the fileID journal | **SPLIT** — keep only `repo/blob/gdrive/gdrive_storage_test.go` (+82 lines: the regression tests that pin the `FlushCaches` contract in both directions); drop the harness and results changes |
| `9caf1c7c` | docs(tools): tuning process artifact | **DROP** — `tools/gdrive-bench` only |
| `0dad0b7d` | feat(tools): falsifiable pacer metrics, compare order, delete-sweep knobs | **SPLIT** — keep `repo/blob/gdrive/gdrive_transport.go`, `gdrive_transport_test.go`, `gdrivepacer/gdrivepacer.go`, `gdrivepacer/gdrivepacer_test.go` (the paced-wait counter); drop the harness changes |
| *(uncommitted, WP-5)* | docs + de-`[Not maintained]` | **KEEP** as a new commit |

Two commits are `feat(tools):` by subject but carry backend changes in their
bodies. Do not filter by commit message.

## A.3 The mechanics

Because two commits must be split by path, the least error-prone route is not
an interactive rebase but a fresh branch with a single reconstructed tree:

```bash
git fetch upstream
git switch -c gdrive-upstream upstream/master

# take the final state of exactly the paths we ship
git checkout gdrive-native-v2 -- \
    repo/blob/gdrive \
    cli/storage_gdrive.go \
    'site/content/docs/Repositories/_index.md' \
    'site/content/docs/_index.md' \
    'site/content/docs/Features/_index.md'

# file_id_cache.go was deleted by the rewrite; the checkout above will not
# remove it, so remove it explicitly
git rm -f repo/blob/gdrive/file_id_cache.go

git status --short          # must show nothing outside those three paths
git commit -m 'feat(providers): rewrite the native Google Drive backend'
```

If the reviewers would rather read it in stages, replay the seven KEEP commits
in order with `git cherry-pick`, then apply the two SPLIT commits with
`git cherry-pick -n <sha>` followed by
`git restore --staged --worktree tools/ PROJECT_STATE.md` before committing,
then add the WP-5 docs commit. Either way, finish with the `git status --short`
check above.

## A.4 Squash before merge

Kopia's contribution guidelines say: *"Once review has occurred, please rebase
your PR down to a single commit. This will ensure a nice clean Git history."*
So the multi-commit form is for review only; the branch must be a single commit
before it is merged. Given that, opening the PR as one commit (the first recipe
above) is the lower-friction choice unless a reviewer asks for stages.

## A.5 Pre-submit checks

`site/content/docs/Contribution guidelines/_index.md` names three:

```bash
make ci-tests      # = make vet + make test
make lint-all      # "if developing cross-platform code"
make goreleaser
```

---

# Appendix B — changelog and CLA

## B.1 Changelog: there is no file to edit

Kopia does not keep a hand-maintained changelog. `CHANGELOG.md` does not exist
anywhere in the tree. What exists is `.chglog/` — configuration for
[`git-chglog`](https://github.com/git-chglog/git-chglog), which **generates** the
release changelog from commit subjects at release time. `.chglog/config.yml`
parses each subject with `^(\w*)(?:\(([\w\$\.\-\*\s]*)\))?\:\s(.*)$`, filters on
the scope, and groups by it; `providers` maps to the heading "Storage
Providers".

Confirmed empirically as well: the file lists of the five most recently merged
PRs (#5531, #5530, #5528, #5315, #5524) contain no changelog file.

**Therefore: no changelog file is created or edited by this work.** The
changelog entry *is* the PR title, which is why the title above matters and why
the scope must be `providers`.

## B.2 CLA / DCO: neither is required

There is **no CLA and no DCO** in this project, and this was checked four ways:

1. There is no `CONTRIBUTING.md` in the repository root. The contribution
   guidelines live at `site/content/docs/Contribution guidelines/_index.md`, and
   their full text on submitting code is quoted below. They mention no CLA, no
   DCO and no sign-off.
2. No CLA-bot or DCO workflow exists in `.github/workflows/` (the 18 workflows
   there are build, test, lint, license-check, scorecard, stale and auto-merge).
3. The full status-check list on a recently merged PR (#5528, checked with
   `gh pr checks`) contains no CLA or DCO check.
4. Commits on recently merged PRs carry no `Signed-off-by:` trailer.

The complete set of requirements the guidelines place on a submitter:

> * We follow the [Github Pull Request Model](https://help.github.com/en/articles/about-pull-requests) for all contributions.
> * For large bodies of work, we recommend creating an issue and labelling it `design` outlining the feature that you wish to build, and describing how it will be implemented. This gives a chance for review to happen early, and ensures no wasted effort occurs.
> * For new features, documentation must be included.
> * Once review has occurred, please rebase your PR down to a single commit. This will ensure a nice clean Git history.
> * Pull Requests must go through a number of CI checks before being approved. Before submitting a PR, it's a good idea to run some of these checks locally:
>   - `make ci-tests`
>   - `make lint-all` (if developing cross-platform code)
>   - `make goreleaser`
> * When creating a PR, the title should match the pattern `<type>`(`<domain>`): `<description>` […]

Two of these bear on this change specifically:

- *"For large bodies of work, we recommend creating an issue and labelling it
  `design`…"* — this is a ~12,700-line change to a provider. Consider opening a
  `design` issue, or at minimum posting the plan on #2656/#3788/#4272, before
  the PR rather than after.
- *"For new features, documentation must be included."* — satisfied: the Google
  Drive section of `site/content/docs/Repositories/_index.md` is rewritten in
  this branch.

## B.3 License check

`make license-check` exists and runs in CI. This change adds **no new
dependencies** — `go.mod` and `go.sum` are untouched — so it cannot affect that
check.

---

# Appendix C — benchmark charts (fork artifacts)

These SVGs are generated by `gdrive-bench graph` from the curated comparison in
`tools/gdrive-bench/results/`. They live under
`tools/gdrive-bench/results/graphs/` on the fork branch **and are fork-only
artifacts: the whole `tools/gdrive-bench/` tree is on Appendix A's
"never reaches the upstream branch" list.** Attach them to the PR body (drag the
file into the GitHub comment box) rather than linking a path reviewers cannot
resolve.

| File | What it shows |
|---|---|
| `native-vs-rclone-cpu-wall-clock-linear.svg` | Median wall clock per workload, native vs rclone, one linear axis with `[p10..p90]` whiskers — the whole picture in one panel. |
| `native-vs-rclone-cpu-wall-clock-split.svg` | The same data faceted into magnitude bands, each panel linear with its own axis, so the sub-second rows are readable next to the four-minute one. |
| `native-vs-rclone-cpu-snapshot-stages.svg` | The `snapshot-e2e` backup cycle stage by stage (create → init → incremental → verify → maintenance), both lanes side by side. |
| `native-vs-rclone-cpu-cpu-total.svg` | CPU cost to the machine per workload (`cpu_total_ms` = harness process + every descendant), carrying the fairness note that the rclone lane's bar is Kopia **plus** its rclone subprocess. |
| `native-vs-rclone-cpu-quota-units.svg` | Drive quota units per workload, **native lane only and labelled as such** — rclone's quota is unobservable through a subprocess, so it is shown as absent rather than as zero. |

Each chart is self-contained (no script, no external font or stylesheet), 900 px
wide, carries its own units, trial count, significance flags and a caption naming
the run, and bakes in a light surface so it stays legible in GitHub's dark theme.

The two charts worth putting in the PR body itself are
`native-vs-rclone-cpu-snapshot-stages.svg` (the end-to-end story a user
recognises) and `native-vs-rclone-cpu-cpu-total.svg` (the CPU story, which the
prose table in "Evidence" does not carry).
