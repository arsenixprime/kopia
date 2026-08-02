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
