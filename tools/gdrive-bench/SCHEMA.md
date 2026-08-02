# gdrive-bench output contract

**`schema_version: 1` — FROZEN.** Everything in this document is a contract.
Additive changes (new optional fields, new workload names, new metric names) do
not bump the version; anything that removes a field, renames one, or changes the
meaning or unit of an existing one **must** bump `schema_version` and readers
must reject a document whose version they do not know (the harness does:
`readResult` fails on a mismatch).

Two documents are described here: the **run result** written by
`gdrive-bench run`, and the **comparison report** written by
`gdrive-bench compare --json-out` and `gdrive-bench baseline check --json-out`.

---

## 1. Where results go

Results are written to:

```
$KOPIA_GDRIVE_BENCH_RESULTS/<UTC timestamp>-<label>.json
```

with the timestamp formatted `20060102T150405Z` and the label taken from
`--label` (default: the target names joined by `-`, sanitized to
`[A-Za-z0-9._-]`). `--results-dir` overrides the environment variable; when
neither is set the harness falls back to `<tempdir>/kopia-gdrive-bench-results`,
which is what makes `--smoke` work with no environment at all.

The same directory also holds `baseline.json` (see `baseline set`) and
`quota-ledger.json`.

**Results are never written inside the repository.** The harness resolves the
git work tree of the current directory and refuses (exit 2) if the results
directory is inside it. Only `gdrive-bench publish` puts a hand-picked artifact
into `tools/gdrive-bench/results/`.

Every file the harness writes is written atomically (temp file + rename) with
mode `0600`; published artifacts get `0644`.

---

## 2. Run result — by example

```jsonc
{
  "schema_version": 1,
  "run_id": "r3f9a1c",                       // short id; also namespaces every blob the run writes
  "label": "native-vs-rclone",
  "started_at": "2026-08-01T20:14:02.117Z",  // always UTC, RFC 3339
  "finished_at": "2026-08-01T20:41:55.884Z",

  "harness": {
    "commit": "62f35a2d…",                   // git rev-parse HEAD at startup (build stamp if outside a checkout)
    "dirty": false,                          // git status --porcelain was non-empty
    "go_version": "go1.25.8",
    "os": "linux", "arch": "amd64",
    "hostname": "bench01"
  },

  "config": {                                // every harness knob a number depends on
    "trials": 5,
    "order": "abba",                         // abba | random | sequential
    "cooldown_ms": 10000,
    "smoke": false,
    "seed": 1785640000000000000,             // reuse to reproduce sizes, offsets and orderings
    "keep_going": false,
    "workloads": ["small-write", "pack-write", "read-metadata", "range-read", "list-heavy", "delete-sweep"],
    "targets_file": "/home/u/targets.json",
    "params": {                              // workload sizes; see §4
      "small_write_n": 100, "small_blob_min_bytes": 2048, "small_blob_max_bytes": 8192,
      "pack_write_m": 5, "pack_size_bytes": 20971520,
      "read_metadata_n": 50,
      "range_read_r": 50, "range_read_packs": 2, "range_read_pack_size_bytes": 20971520,
      "range_read_min_bytes": 4096, "range_read_max_bytes": 262144,
      "list_traversals": 5, "list_blobs_n": 40,
      "delete_sweep_n": 100, "delete_percent": 50,
      "parallelism": 8
    }
  },

  "targets": [                               // in configuration order, not execution order
    {
      "name": "native",
      "storage_type": "gdrive",              // the blob provider type
      "knobs": {"pacerMinSleepMS": 50, "listPageSize": 500},   // the connection's "tuning" object, or null for built-in defaults
      "config": {"folderID": "1IOd…", "credentialsFile": "[redacted]"}  // whole connection config, credentials redacted
    },
    {"name": "rclone", "storage_type": "rclone", "knobs": null, "config": {"remotePath": "bench:kopia"}}
  ],

  "trials": [                                // one entry per (trial, target) execution, in EXECUTION order
    {
      "trial": 0,                            // 0-based trial number
      "sequence": 0,                         // 0-based position in the whole run
      "position": 0,                         // 0-based position within this trial
      "target": "native",
      "started_at": "2026-08-01T20:14:02.119Z",
      "workloads": {                         // result key -> metrics, see §3
        "small-write":            { … },
        "pack-write":             { … },
        "read-metadata:hot":      { … },
        "read-metadata:cold":     { … },
        "range-read":             { … },
        "list-heavy:full":        { … },
        "list-heavy:prefixed":    { … },
        "delete-sweep":           { … }
      },
      "errors_by_reason": {"http_403": 2, "rate_limit_exceeded": 1}   // summed from the transport deltas of this trial
    }
  ],

  "quota": { … },                            // §5
  "errors": ["cleanup of bench-r3f9a1c-t0-list-heavy- on native: …"]  // non-fatal problems
}
```

### Field table — top level

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | Always 1 for this document. Readers must reject unknown versions. |
| `run_id` | string | Short unique id. Also the namespace of every blob the run writes (`bench-<run_id>-t<trial>-<workload>-…`). |
| `label` | string | Sanitized `--label`; appears in the file name. |
| `started_at` / `finished_at` | RFC 3339 UTC | Wall-clock bounds of the run. |
| `harness.commit` | string | Commit the harness was built from, read with `git rev-parse HEAD` at startup; `"unknown"` if neither git nor a VCS build stamp is available. |
| `harness.dirty` | bool | Working tree had uncommitted changes. A `true` here means the number is not reproducible from a commit. |
| `config.*` | — | Every harness-level knob. Two results are only comparable when their `config.params`, `config.trials` and `config.smoke` agree. |
| `targets[]` | array | What was measured. `knobs` is the tuning object *as configured*, so a comparison can attribute a difference to a knob change. |
| `trials[]` | array | One pass of the suite by one target, in execution order. |
| `quota` | object | §5. |
| `errors[]` | string array | Cleanup failures, ledger write failures, and (with `--keep-going`) workload failures. Empty on a clean run. |

### Result keys

A result key is a registered workload name, optionally suffixed with
`:<phase>`. The registry name is everything before the first colon. Current
keys:

| Key | Produced by |
|---|---|
| `small-write` | small-write |
| `pack-write` | pack-write |
| `read-metadata:hot` / `read-metadata:cold` | read-metadata |
| `range-read` | range-read |
| `list-heavy:full` / `list-heavy:prefixed` | list-heavy |
| `delete-sweep` | delete-sweep |

**Adding a workload does not change this schema.** Workload names are strings
and `trials[].workloads` is a map, so a new workload — including WP-3b's
out-of-process snapshot workload — simply adds a key.

---

## 3. Workload metrics

```jsonc
{
  "wall_clock_ms": 8421.7,      // float; the measured phase only, excluding setup and cleanup
  "ops": 100,                   // operations measured
  "errors": 3,                  // operations that returned an error (they are still counted in ops and in the latency samples)
  "bytes_up": 2097152,          // payload bytes offered to PutBlob
  "bytes_down": 1048576,        // payload bytes returned by GetBlob
  "samples": 100,               // latency samples; always == ops
  "latency_ms": {"p10": 41.2, "p50": 58.9, "p90": 121.0, "p99": 402.5},
  "quota_units": 15000,         // Drive quota units, see §5
  "quota_units_source": "observed",  // "observed" (counted from Drive API calls) or "none" (backend is not Drive)
  "transport_delta": { … },     // native Drive backend only; null everywhere else
  "error": "…"                  // first operation error, omitted when there was none
}
```

- **Latencies are per operation, in milliseconds, as floating point** — a
  sub-millisecond filesystem operation must not collapse onto zero.
- **Percentiles** use linear interpolation between the two closest ranks
  (Excel `PERCENTILE` / NumPy default / R type 7):
  `rank = p/100 × (n−1)`. The median of an even sample is the average of the
  middle two; every percentile of a one-sample series is that sample.
- **`wall_clock_ms` covers the measured phase only.** A workload's setup writes
  (the blobs `read-metadata`, `range-read`, `list-heavy` and `delete-sweep`
  measure against) and its cleanup deletes are outside it.
- **`transport_delta`** is `repo/blob/gdrive.TransportStatsSnapshot` — the
  backend's own Drive API counters, subtracted element by element between the
  start and the end of the phase. Its `perMethod` map is keyed by Drive method
  (`files.list`, `files.get`, `files.get.media`, `files.create`, `files.update`,
  `files.delete`, `upload.simple`, `upload.resumable`, `batch`, `other`) and
  carries calls, errors, retries, backoffs, bytes, a latency sum and a
  per-bucket latency histogram aligned with `latencyBucketsMS`. It is `null` for
  every backend that is not the native Drive one, which is how a reader tells
  "no Drive traffic" from "Drive traffic not measured".
- The cold pass of `read-metadata` flushes the storage caches, closes the
  connection and opens a new one. **This only means anything for the native
  backend**, and even there it is only half-cold: dropping the last connection
  discards the in-memory file-ID map, but the on-disk journal survives, exactly
  as it does in production. A genuinely cold cache needs a fresh process
  (WP-3b). For every other backend the two passes differ only in HTTP
  connection reuse.

---

## 4. Workloads and their setup/cleanup contract

| Workload | Setup | Measures | Default (smoke) |
|---|---|---|---|
| `small-write` | — | `PutBlob` of N index-sized blobs, one at a time | N=100 (10), 2–8 KB |
| `pack-write` | — | `PutBlob` of M pack blobs | M=5 (1), 20 MB each |
| `read-metadata` | writes N blobs | `GetMetadata` over the set, hot then cold | N=50 (5) |
| `range-read` | writes P packs | R seeded random offset/length `GetBlob`s | R=50 (5), P=2 (1), 4 KB–256 KB ranges |
| `list-heavy` | writes N blobs | C whole-storage traversals, then C prefixed ones | C=5 (2), N=40 (5) |
| `delete-sweep` | writes N blobs | deletes `delete_percent`% with bounded parallelism | N=100 (10), 50%, parallelism 8 |

Every workload writes under a run-unique prefix
(`bench-<run_id>-t<trial>-<workload>-`), and **cleanup is deferred, so it runs
even when the workload fails or the run is interrupted** — it uses a
non-cancellable context for exactly that reason. Cleanup lists the prefix and
deletes what it finds; cleanup failures are appended to the top-level `errors`
array and never fail the run.

Randomness is seeded per `(seed, workload, trial)` and deliberately **not** per
target, so every target of a trial writes the same bytes and reads the same
offsets.

---

## 5. Quota accounting

```jsonc
"quota": {
  "decision": {
    "allowed": true,
    "daily_upload_fraction": 0.5,
    "budget_bytes": 375000000000,
    "already_uploaded_bytes_today": 12000000000,
    "estimated_upload_bytes": 2100000000,
    "estimated_quota_units": 84000,
    "quota_units_per_minute_ceiling": 325000,
    "reason": ""                       // set when allowed=false
  },
  "actual_upload_bytes": 2097152000,   // charged to the ledger
  "actual_quota_units": 79500,
  "observed_qu_per_minute": 2841.0,
  "ledger_path": "/home/u/kopia-bench-results/quota-ledger.json"
}
```

Two different Google limits, handled two different ways (RECON.md §C.2, §C.3):

1. **750 GB/day/user upload cap — ENFORCED.** `quota-ledger.json` records
   uploaded bytes and quota units per UTC day (7 days of history, pruned on
   write). Before a run the harness estimates what the run will upload and
   **refuses to start with exit code 2** if
   `ledger[today] + estimate > --daily-upload-fraction × 750 GB`
   (default fraction 0.5, i.e. 375 GB). Only targets whose traffic leaves the
   machine are counted — everything except `filesystem` — which over-counts a
   non-Drive remote, the safe direction.

2. **325,000 quota units per minute per user — REPORTED ONLY.** This is a
   *rate*, and a daily ledger cannot enforce a rate. The harness prints the
   run's estimated quota units before it starts (framed as "if they were all
   spent inside one minute, that is X% of the ceiling" — the worst case) and the
   observed average `QU/min` afterwards. Averages hide bursts: treat both as
   informational. Quota-unit costs come from the RECON.md §C.2 table (read 5,
   list 100, download 200, edit 50, other 5); `files.create`, `files.delete` and
   the upload protocols are **assumed** edit-class, which RECON.md records as
   unverified. A batch envelope is charged 0, so batched deletes under-report.

The estimate assumes a name lookup (100 QU) plus one edit-class upload per
blob, or a resumable session plus one request per 64 MB chunk above the 8 MB
cutoff.

---

## 6. Comparison report

Written by `compare --json-out` and `baseline check --json-out`.

```jsonc
{
  "schema_version": 1,
  "generated_at": "2026-08-01T21:03:00Z",
  "noise_threshold_pct": 10,
  "reference": "native",                    // the series everything is compared against
  "sources": [
    {"key": "native", "file_index": 0, "file": "…-native-vs-rclone.json", "label": "native-vs-rclone",
     "target": "native", "commit": "62f35a2d…", "trials": 5}
  ],
  "rows": [
    {
      "workload": "small-write", "metric": "wall_clock_ms",
      "reference": "native", "target": "rclone",
      "reference_stats": {"n": 5, "median": 8421, "p10": 8100, "p90": 8900, "min": 8050, "max": 9000},
      "target_stats":    {"n": 5, "median": 11800, "p10": 11200, "p90": 12500, "min": 11000, "max": 12600},
      "delta_pct": 40.1,
      "delta_undefined": false,             // true when the reference median is 0 (JSON cannot carry an infinity)
      "significance": "REAL",
      "improvement": false                  // did the target move the metric in the good direction
    }
  ]
}
```

- A **series** is one `(file, target)` pair. Its key is the target name when
  that is unique across all inputs, and `<target>@<file index>` when it is not —
  which is what makes comparing a document against itself work.
- One file holding two or more targets can be compared on its own; that is the
  normal interleaved-run case.
- Every series after the first is compared against the first.
- Metrics compared: `wall_clock_ms`, `p50_ms`, `p90_ms`, `p99_ms`,
  `ops_per_sec`, `errors`, `bytes_up`, `bytes_down`, `quota_units`. `ops_per_sec`
  is the only one where higher is better.

### Significance rule

| Verdict | Condition |
|---|---|
| `REAL` | the `[p10, p90]` intervals of the two series **do not overlap** |
| `NOISE` | they overlap and the medians differ by **< 10%** |
| `WEAK` | they overlap and the medians differ by **≥ 10%** |
| `NODATA` | one of the series has no samples |

This is crude on purpose. It is not a significance test and it does not pretend
to be one: with the default five trials the intervals are order statistics of a
tiny sample. It is designed so a human can verify a verdict by eye and so an
automated tuning agent has one unambiguous rule — **act only on `REAL`.** A
`WEAK` row means "measure again with more trials", never "ship the change".

### `baseline check`

`baseline set <result.json>` copies a document to
`$KOPIA_GDRIVE_BENCH_RESULTS/baseline.json`. `baseline check <result.json>`
compares same-named targets against it on the **key metrics**
(`wall_clock_ms`, `p50_ms`) and **exits 1** when any row is `REAL`, is a
worsening, and moves more than `--threshold-pct` (default 15). That is the
cron/CI hook.

---

## 7. Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | a workload failed (`run`), or a REAL regression beyond the threshold (`baseline check`), or an artifact could not be written |
| 2 | refused to start: bad usage, bad targets file, unknown workload, results directory inside the repository, or the quota guard |

`run` writes its result document even when a workload fails, so a failed run is
still diagnosable; `--keep-going` continues past a failing workload and still
exits 1 at the end.

---

## 8. Additive notes (post-freeze)

Everything below was added after `schema_version: 1` was frozen. All of it is
additive under the rules at the top of this document: new optional fields, new
workload names, new result keys and one new value of an existing enumeration.
A reader written against §1–§7 still reads these documents correctly; it will
simply not know what the new keys mean. **The version stays 1.**

### 8.1 New workload: `snapshot-e2e` (opt-in)

Not part of the default suite - `--workloads` has to name it, and it needs a
kopia binary (`--kopia-exe`, or the harness builds one from the current work
tree and records the path in `config.kopia_exe`).

It measures **kopia**, not `blob.Storage`: a real repository is created in the
target's own folder, a synthetic source tree is backed up, mutated and backed up
again, verified in full and then compacted. Five result keys, one kopia
invocation each, timed by the harness around the process:

| Key | Command |
|---|---|
| `snapshot-e2e:create` | `repository create <provider> …` |
| `snapshot-e2e:init` | `snapshot create <tree>` |
| `snapshot-e2e:incremental` | `snapshot create <tree>` after the mutation |
| `snapshot-e2e:verify` | `snapshot verify --verify-files-percent=100` |
| `snapshot-e2e:maintenance` | `maintenance run --full --safety=none` |

`--safety=none` is deliberate: full safety refuses to touch blobs younger than
its retention margins, and every blob in a repository created seconds earlier is
younger than that, so the default would measure a no-op.

Reading its metrics:

- `ops` is 1 and the four latency percentiles are all the stage's wall clock -
  one invocation is one sample. Compare these rows on `wall_clock_ms`;
  `ops_per_sec` is meaningless here.
- `transport_delta` is **always null**, even for the native Drive target: the
  API counters live in the child process. The API-call profile of the native
  backend comes from the in-process workloads.
- `bytes_up` / `bytes_down` are the **size of the source tree**, not a byte
  counter - deduplication and compression make the real transfer smaller, which
  is the safe direction for the quota ledger.
- The workload requires the target folder to hold no `kopia.repository` blob and
  fails fast if it does. Cleanup deletes exactly the blobs that appeared while
  it ran (a before/after listing difference), because kopia's blob names are its
  own and cannot be namespaced by the harness.

### 8.2 New value of `quota_units_source`: `"estimated"`

§3 lists `"observed"` and `"none"`. `snapshot-e2e` on a remote target reports
`"estimated"`: the Drive traffic happens in a child process where the harness's
transport cannot see it, so `quota_units` is derived from the bytes moved on the
same assumptions the pre-run estimate uses. `"none"` keeps its meaning - the
backend spends no Drive quota at all.

### 8.3 New result key `read-metadata:warm`, and what `:cold` now means

`--true-cold` changes the read-metadata workload from two passes to three:

| Key | State |
|---|---|
| `read-metadata:hot` | the connection that wrote the blobs; every file ID in memory |
| `read-metadata:warm` | fresh connection, in-memory map gone, **on-disk journal intact** |
| `read-metadata:cold` | fresh connection **and a deleted journal**: every name resolved against Drive again |

Without `--true-cold` the workload behaves exactly as §3 describes and produces
`:hot` and `:cold` only, where `:cold` is the half-cold state that `:warm` names
above.

**This is a semantics change to an existing key, and it is deliberately not
version-bumping** because the flag that causes it is recorded next to the number:
`config.params.true_cold` says which of the two meanings a document's
`read-metadata:cold` carries, and two results are only ever comparable when
their `config.params` agree (§2). A reader that ignores `true_cold` and compares
a true-cold `:cold` against a half-cold one is making the same mistake as one
that compares different `read_metadata_n` values.

The purge is surgical: only `<config.tuning.cacheDir>/gdrive-fileids` is
removed. `--true-cold` **refuses to start** unless every native Drive target
sets `config.tuning.cacheDir` to a directory the harness owns, so a benchmark
can never delete the cache of a real repository.

### 8.4 New fields

`config.params` (all optional; zero values mean the workload was not selected or
the default applies):

| Field | Meaning |
|---|---|
| `snapshot_files` | files in the snapshot-e2e source tree (default 800, smoke 40) |
| `snapshot_dirs` | top-level directories it is spread over (default 8, smoke 3) |
| `snapshot_total_bytes` | total size of the tree (default 150 MiB, smoke 6 MiB) |
| `snapshot_mutate_percent` | share of files deleted and re-added between the two snapshots (default 5; a further 2% of the survivors are appended to) |
| `true_cold` | §8.3 |

`config`:

| Field | Meaning |
|---|---|
| `kopia_exe` | the binary `snapshot-e2e` drove; omitted when the workload did not run |

### 8.5 Comparison reference is **configuration order**, and `--reference`

§6 says "every series after the first is compared against the first" and §2 says
`targets[]` is in configuration order. Until WP-4b the two did not agree:
`compare` sorted target names alphabetically and took the first, so a run whose
lanes were named `control` and `candidate` was reported with **`candidate`** as
the reference and every `delta_pct` inverted relative to what the targets file
implied. The sort is gone.

- The default reference is the **first target of the first result file, in the
  order that file's `targets[]` lists them** — which is the order the targets
  file listed them. With one file holding `control` then `candidate`, the
  reference is `control` and every delta reads *candidate relative to control*.
- `compare --reference=<name>` overrides it. `<name>` is a series key, or a
  target name when that is unambiguous; an unknown or ambiguous name is refused
  with exit 2 and the available keys are listed.
- The report's `reference` field is unchanged in meaning and remains the
  authority. A reader that already consulted it was always correct.
- A target that appears in `trials[]` but not in `targets[]` (only possible in a
  hand-edited document) is ordered last rather than dropped.

`baseline check` is unaffected: it pairs same-named targets across two documents
and always labels the reference `baseline:<target>`.

### 8.6 Pacer steady-state wait instrumentation

The pacer records the delays it makes calls sleep waiting for its **token
bucket**, not just the ones it sleeps after an **error**. Without this a
hypothesis about `pacerMinSleepMS` or `pacerBurst` can only be argued from
arithmetic — the knob is inert unless the workload outruns the refill rate, and
nothing measured whether it did.

`transport_delta.perMethod[<method>]` (i.e. `gdrive.MethodStats`) gains:

| Field | Meaning |
|---|---|
| `pacedWaits` | number of steady-state token waits this method actually paid. A call that found a token waiting for it counts 0 — not even the count moves. |
| `pacedWaitMS` | their total length, in milliseconds |

`transport_delta` itself (i.e. `gdrive.TransportStatsSnapshot`) gains the same
two keys at top level, holding the sums over `perMethod`; `perMethod` remains
the authority and the two always agree.

Workload metrics (§3) gain:

| Field | Meaning |
|---|---|
| `paced_wait_ms` | float; the phase's total steady-state pacer wait, taken from `transport_delta.pacedWaitMS`. `0` for every backend that is not the native Drive one, and `0` on a Drive phase that never outran the bucket — which is a **measurement**, not a missing value. |

`paced_wait_ms` is also a compared metric (§6): lower is better, and a pacer
cycle that shows `0` on both lanes has falsified its own hypothesis before
looking at wall clock.

Backoffs and paced waits never mix: `backoffs`/`backoffMS` remain the error
path, `pacedWaits`/`pacedWaitMS` are the error-free one.

### 8.7 `delete-sweep` size and effective parallelism

- **`--delete-sweep-n`** sets `config.params.delete_sweep_n` the way
  `--small-write-n` sets `small_write_n`. The default is unchanged (100; smoke
  10), so no existing result changes meaning.
- **`delete-sweep`'s parallelism now prefers the TARGET's
  `tuning.deleteParallelism`** over the harness's `--parallelism`, falling back
  to `--parallelism` when the target does not set one. Before, the harness's own
  semaphore (default 8) was the tighter of the two and clamped the backend knob
  out of every experiment that swept it.
- Workload metrics gain an optional **`params`** object recording the values the
  harness resolved at run time rather than reading straight from
  `config.params`. One key exists today:

| Key | Meaning |
|---|---|
| `params.parallelism` (on `delete-sweep`) | the number of concurrent deletes actually issued: the target's `tuning.deleteParallelism` when it set one, otherwise `config.params.parallelism` |

  `params` is omitted entirely by workloads that resolve nothing, so its absence
  means "`config.params` is the whole story".

### 8.8 CPU accounting

Wall clock is half of what a backup backend costs. The other half is CPU, and
measuring it across these two lanes is not symmetric: the native Drive backend
runs **inside** the harness process, while the rclone lane's work happens in an
`rclone serve webdav` child that Kopia's rclone backend spawns — and inside
`snapshot-e2e`, in an rclone *grandchild* that the child kopia spawned. Only one
number is comparable between them:

> **`cpu_total_ms` = the CPU this phase cost the machine** — the harness process
> plus every descendant of it, user + system.

**Fairness, stated in both directions.** On the native lane `cpu_total_ms` is
essentially all harness CPU, because the backend *is* the harness. On the rclone
lane it is the harness (Kopia's WebDAV client) **plus** the rclone subprocess.
Neither column is "the backend's CPU" on its own; the **sum is the comparable
number**, and it is the sum that both lanes report.

Workload metrics (§3) gain:

| Field | Meaning |
|---|---|
| `cpu_user_ms` | float; the harness process's own user CPU during the phase — `getrusage(RUSAGE_SELF)` delta |
| `cpu_sys_ms` | float; the same for system CPU |
| `cpu_children_user_ms` | float; user CPU of every **descendant** process attributed to the phase (see the attribution rules below) |
| `cpu_children_sys_ms` | float; the same for system CPU |
| `cpu_total_ms` | float; the sum of the four. **The headline metric.** |
| `cpu_pct` | float; `cpu_total_ms / wall_clock_ms × 100`. `100` is one core saturated for the whole phase; more than 100 means more than one core was busy |
| `cpu_source` | string; which accounting produced the numbers: `"rusage"`, `"proc"`, `"wait4"`, `+`-joined in that order, or `"none"` where the platform has none |

#### How descendants are attributed

Three sources contribute. They are estimates of the **same** quantity, so they
are reconciled — never summed naively.

1. **`rusage`** — `RUSAGE_SELF` and `RUSAGE_CHILDREN` deltas across the phase.
   `RUSAGE_CHILDREN` only moves when a direct child is **reaped**, and Kopia's
   rclone backend reaps its `rclone serve webdav` at storage `Close` — which
   happens at a connection recycle or in the runner's deferred cleanup, i.e.
   **outside** every measured window and once per *workload* rather than once per
   *phase*. Reap-time accounting alone would therefore report ~0 CPU for every
   rclone phase and dump a whole connection's CPU into whichever window happened
   to contain a `Close`. It is not the authority; it is exact for a child whose
   whole life falls inside one phase.
2. **`proc`** (Linux) — `/proc/<pid>/stat` `utime`+`stime` are **cumulative**
   counters. The harness reads them for every live descendant at both phase
   boundaries, and the difference *is* that phase's descendant CPU, with no
   sampling error. A background poll every 200 ms is a catch-net only: it exists
   so that a descendant which **exits mid-phase** still contributes what it burned
   up to its last observation. Each process's own `utime`+`stime` are counted and
   its `cutime`/`cstime` deliberately are not — every descendant is visited in its
   own right, so adding a parent's accumulated child time would count it twice.
3. **`wait4`** — the `ProcessState` of a `snapshot-e2e` stage's kopia
   invocation. On Linux the kernel fills `wait4`'s rusage with `RUSAGE_BOTH` of
   the dead child, so it *includes* the CPU of the child's own reaped children —
   the rclone grandchild. It is folded in with `max()`, never `+`, so it can only
   raise a floor.

**Reconciliation.** A process attributed live and later reaped also lands in
`RUSAGE_CHILDREN`. The harness keeps a per-process ledger of how much of each
descendant's cumulative CPU has already been accounted for; when a descendant
disappears, exactly that much is subtracted from the reap lump, and only the
remainder — the tail it burned after its last observation — is charged. So a
`snapshot-e2e` stage on the rclone lane accounts for kopia **and** its rclone
grandchild, once each.

**Known limits, stated rather than hidden.**

- Non-Linux platforms have no `proc` source: `cpu_source` reads `"rusage"`, a
  grandchild is invisible, and a phase whose child outlives it reports **no**
  child CPU rather than a wrong one.
- Windows has no `getrusage` either; only the `wait4`-equivalent
  (`os.ProcessState`) times survive.
- The `proc` walk counts **every** descendant, including one that is merely idle
  — on the rclone lane the harness's own `rclone serve webdav` is alive during a
  `snapshot-e2e` stage and contributes its (near-zero) idle CPU to it. That is a
  real cost of the configuration, not an error, but it is not *caused* by the
  stage.

#### `cpu_residual_ms` — the audit

`trials[]` gains two fields:

| Field | Meaning |
|---|---|
| `cpu_total_ms` | float; the CPU the **whole trial** cost the machine: `RUSAGE_SELF` delta + `RUSAGE_CHILDREN` delta + the increment of every descendant still alive at the end (which is therefore not in `RUSAGE_CHILDREN` yet). The three terms are disjoint by construction. |
| `cpu_residual_ms` | float; `cpu_total_ms` − Σ(`cpu_total_ms` of the trial's phases): the CPU the trial spent that **no measured phase claimed**. |

The residual exists so that an attribution leak is visible **in the result
document itself** rather than having to be inferred. The CPU a workload spends
outside its measured phase is real: `read-metadata`, `range-read`, `list-heavy`
and `delete-sweep` all write their working set as unmeasured setup, every
workload's cleanup deletes are unmeasured, and on the rclone lane spawning
`rclone serve webdav` and shutting it down again happens between phases.
**Expected magnitude: single-digit percent of `cpu_total_ms` on the native lane
and roughly 5–15 % on the rclone lane** (its setup does more work and its
subprocess is started and stopped once per workload). A residual comparable to
the sum of the phases means the phases stopped seeing a subprocess — the CPU did
not vanish, it moved here.

#### Comparison and baselines

- `cpu_total_ms` **is** a compared metric (§6): `compare` reports it and applies
  the REAL/WEAK/NOISE rule to it like any other.
- `cpu_total_ms` is deliberately **not** a key metric, so no `baseline check`
  ever fails on it. Both lanes of a comparison are measured minutes apart on one
  machine, and the CPU a desktop has spare depends on whatever else its operator
  is running; a baseline promoted on a quiet machine would fail every check made
  on a busy one. `compare` flags it, a human reads it, nothing fails a build over
  it.

#### The accounting tripwire

The failure mode here is asymmetric and flattering: a lane whose subprocess CPU
was not attributed reports a small number that *looks* like a win. Seconds of
WebDAV-over-TLS traffic cannot cost single-digit milliseconds of CPU. So a phase
with at least **1 s** of wall clock whose `cpu_total_ms` is below
`max(20 ms, 0.5 % of wall clock)` is treated as **broken accounting, not a
measurement**: `gdrive-bench graph` draws that cell as a red hatch labelled
`BROKEN-ACCOUNTING` instead of a bar, and a report must say so in words.

### 8.9 New subcommand: `graph`

```
gdrive-bench graph <result.json|compare.json> --out-dir <dir> [--prefix=name-]
```

Renders a run result **or** a comparison report as self-contained SVG files. It
reads both document kinds by probing for `trials` / `rows`, and a run result is
summarized through exactly the same code path `compare` uses, so a chart and a
comparison table can never disagree. It writes, skipping any chart the document
has no data for:

| File | What it shows |
|---|---|
| `wall-clock-linear.svg` | median `wall_clock_ms` per workload, one bar per lane, one linear axis |
| `wall-clock-split.svg` | the same data faceted into magnitude bands, each panel linear with **its own** axis |
| `snapshot-stages.svg` | the `snapshot-e2e` stages, side by side per stage |
| `cpu-total.svg` | median `cpu_total_ms` per workload, with the tripwire above applied |
| `quota-units.svg` | median `quota_units` per workload, **reference lane only** (§5: the other lane's quota is unobservable, and a zero-height bar would state something false) |

Contract of the output:

- **Self-contained and inert.** Stdlib-only generation, no scripting, no external
  stylesheet, font or image; one embedded `<style>` element and a baked light
  surface rectangle, because an SVG pasted into a GitHub comment cannot rely on
  `prefers-color-scheme` and dark ink on a transparent background is invisible in
  GitHub's dark theme. 900 px wide, which is about the width a GitHub comment
  renders an image at.
- **Every chart carries its own context**: title, units, the number of trials
  behind every median, the `[p10..p90]` interval over every bar, and a caption
  naming the run label, the date, the source file and the harness commit.
- **Significance is drawn, not just described.** A row whose two intervals
  overlap (anything but `REAL`) is dimmed and labelled with its verdict.
- **Split panels are labelled as such.** Bar lengths are comparable *inside* a
  panel and meaningless *across* panels; the subtitle says so. A log axis was
  rejected for the same reason — a bar twice as long would not be twice the time.

`graph` never writes outside `--out-dir` and never modifies its input. Exit codes
follow §7.
