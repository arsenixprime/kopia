# Fork notes: native Google Drive backend

This fork of [kopia/kopia](https://github.com/kopia/kopia) adds a
ground-up rewrite of the native Google Drive storage backend
(`repo/blob/gdrive`) plus its CLI integration. Everything else is
unmodified upstream kopia, periodically merged in.

Upstream has not expressed interest in adopting the backend, so this
fork is **self-maintained** for now: upstream `master` is merged
regularly, and a PR-ready squashed branch is kept current should that
change. The on-Drive format is standard kopia — repositories created
with this fork remain readable by any kopia build that has a working
Drive backend.

## Branches

| Branch | What it is |
| --- | --- |
| `gdrive-native-v2` | Working branch (default): full development history, project log in [PROJECT_STATE.md](PROJECT_STATE.md), regular merges from upstream `master`. |
| `gdrive-native-upstream` | The same backend as a **single squashed commit on top of the latest upstream `master`** — the build-from-source / PR-ready branch. Force-pushed on every upstream sync; its diff vs. `gdrive-native-v2` for the backend paths is kept at zero. |
| `master` | Untouched upstream mirror as of the last sync. |

## Background: why a rewrite

Upstream's Google Drive backend is marked *"[Not maintained]"* and has
long-standing correctness problems; the commonly recommended
alternative (kopia → WebDAV → `rclone serve webdav` → Drive) is slow
and operationally awkward.

Problems with the old native backend this rewrite fixes:

- [#4272](https://github.com/kopia/kopia/issues/4272) — no
  cross-connection read-your-writes: the file-ID cache was in-memory
  and per-instance (and held a per-blob mutex across entire uploads),
  so a second connection could not see blobs the first just wrote.
- [#2656](https://github.com/kopia/kopia/issues/2656) —
  `storageQuotaExceeded` was retried ten times with backoff instead of
  failing fast with a clear error.
- [#3788](https://github.com/kopia/kopia/issues/3788) —
  `validate-provider` concurrency failures.
- An unfiled `ListBlobs` pagination bug: an early `return nil` on the
  first non-matching prefix silently dropped later pages (and the
  `trashed=false` filter was dropped), so listings could be
  incomplete — fatal for a content-addressed store.
- Drive's `name=` queries are case-insensitive, so a lookup could
  match a *different* blob whose name differs only by case and return
  wrong bytes; the rewrite applies an exact-name client-side filter.

Problems with the rclone/WebDAV route it replaces:

- Kopia spawns and supervises an `rclone serve webdav` subprocess; most
  of the CPU overhead is the WebDAV layer itself.
- It rides rclone's shared OAuth client ID — quota is pooled across all
  rclone users worldwide, and rclone is retiring that client.
- Full-Drive OAuth scope (the rewrite uses `drive.file`: the token can
  only touch files the app created).
- Gaps in rclone's Drive retry map (`dailyLimitExceeded`,
  `domainPolicy`, 429 + `Retry-After` unhandled; quota errors abort).

## What's added

All new code lives in `repo/blob/gdrive/**`, `cli/storage_gdrive*.go`,
and `tools/gdrive-bench/**`:

- **Error taxonomy (`gdriveerr`)** — every documented Drive error
  *reason* maps to a distinct retry class: quota exhaustion fails fast
  with actionable messages, rate limits honor `Retry-After`,
  transient faults retry, permanent faults don't.
- **Adaptive rate limiting (`gdrivepacer`)** — AIMD pacing of Drive
  API calls with jittered backoff; the backend owns its retry policy
  rather than relying on kopia's generic `repo/blob/retrying` layer.
- **Persistent cross-process file-ID cache** — a CRC32C-checksummed
  append-only journal with `flock`-based locking and a refcounted
  shared-instance registry. Fixes #4272 (read-your-writes across
  connections) and makes warm reopen cheap (`files.get` at 250 quota
  units vs. a 5000-unit `files.list` sweep).
- **`drive.file` OAuth scope** with `--create-folder-name` (the app
  creates and owns its backup folder; it cannot see the rest of the
  Drive), browser consent and `--device-flow` for headless machines,
  plus token-file and client-credentials-file options.
- **HTTP/2 stall detection** — `SendPingTimeout` 31s / `PingTimeout`
  15s, `ResponseHeaderTimeout` 2m, and a 5-minute I/O idle deadline,
  so backups survive sleep/wake and network changes instead of hanging.
- **Instrumented transport + 10 tuning CLI flags** (concurrency,
  pacing, cache, timeouts) and a live benchmark harness
  (`tools/gdrive-bench`).

## Performance vs. the rclone/WebDAV route

Measured with the in-repo harness against real Google Drive: 5-trial
ABBA-interleaved runs (2h46m total). The native backend won every
statistically significant comparison; the rclone lane ran with default
tuning.

| Workload | Native vs. kopia+rclone-WebDAV |
| --- | --- |
| End-to-end backup, wall clock | **2.3× faster** (184s vs. 422s) |
| End-to-end backup, CPU | **3.2× cheaper** (9.4s vs. 29.8s) |
| Metadata reads (warm) | **4.6× faster** |
| Range reads | **3.7× faster** |
| List-heavy steady state | **~3× faster** |
| Small writes | **+65%** |
| Pack writes | **+59%** |
| Delete sweep | **+25%** |
| Absent-blob lookup | **5.35s → 0.48s** |

Honest caveats: the raw list-workload median showed 36×, but that is
inflated by rclone's shard-directory accumulation during the run —
~3× is the steady-state figure; and delete-sweep is the one margin
that rclone-side tuning could plausibly narrow.

Other findings baked into the defaults: parallel single-blob deletes
(19.5 blobs/s) beat Drive batch API deletes (8.6 blobs/s — Drive
executes batch parts serially), so the backend parallelizes instead of
batching.

## Known issues

- Live `kopia repository validate-provider` can flake (~2/13 runs):
  Drive's list index lags writes, so a blob written by another
  connection may briefly be invisible to `ListBlobs` (GetMetadata has
  a journal-tail compensation path; ListBlobs does not yet). Does not
  affect backups; a fix (merging recent journal entries into ListBlobs
  results) is sketched in PROJECT_STATE.md.

## Using it

Prebuilt binaries (Linux/macOS/Windows), turnkey installers, and
day-2 docs live in
[arsenixprime/kopia-gdrive-setup](https://github.com/arsenixprime/kopia-gdrive-setup).
To build from source:

```sh
git clone -b gdrive-native-upstream https://github.com/arsenixprime/kopia
cd kopia && CGO_ENABLED=0 go build -o kopia-gdrive .
```

The full engineering log — phase gates, design decisions, benchmark
methodology and raw results — is in [PROJECT_STATE.md](PROJECT_STATE.md)
on the `gdrive-native-v2` branch.
