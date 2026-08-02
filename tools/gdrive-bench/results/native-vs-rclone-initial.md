# Native Google Drive backend vs. rclone — first interleaved comparison

**Question this answers:** on real Google Drive, with everything except the
backend held constant, how does Kopia's native `gdrive` backend compare to the
same Kopia driving the same Drive through `rclone serve webdav`?

**Verdict:** native is faster on every timed row, all of them `REAL`, by
between 1.25x (`delete-sweep`) and 3.6x (metadata reads), and 2.3x on the
end-to-end backup cycle. There is no row where rclone is genuinely faster. The
rows that *look* like rclone wins are an instrumentation artifact and are
explained below — read them as "not measured", not as "free".

---

## Setup

| | |
|---|---|
| Run | `20260802T052322Z-native-vs-rclone-initial.json` |
| When | 2026-08-02 05:23:22Z → 08:09:37Z (2 h 46 min) |
| Trials | 5 per lane, `--order=abba` (A B / B A / A B / B A / A B), 10 s cooldown |
| Workloads | the six blob-level ones + `snapshot-e2e`, `--true-cold` |
| Harness commit | `36fb2b7d` (working tree dirty: the WP-3b harness changes were not yet committed) |
| Seed | `1785648202915059831` |
| Uploaded | 2.71 GB · 231,500 quota units · ~1,393 QU/min average (0.43 % of the 325,000 QU/min/user ceiling) |
| Errors | none — zero failed operations, zero retries, zero Drive error reasons in any trial |

**Both lanes are the same account, the same OAuth client and the same scope.**
The native target authenticates with the `authorized_user` token of OAuth client
`949080498481-…`; the rclone remote `gdrive-bench` was synthesized from that same
token, with the same `client_id`, and requests the same `drive.file` scope. Each
lane writes into its own app-created folder (`kopia-gdrive-bench` for native,
`kopia-gdrive-bench-rclone` for rclone). So the comparison isolates the backend:
the network, the machine, the account, the project's quota bucket and the
permission scope are identical, and ABBA interleaving means a slow ten minutes
hits both lanes equally.

The rclone lane is rclone v1.75.0 driven by Kopia's own `rclone` backend
(`kopia … rclone --remote-path gdrive-bench:bench-blobs`), which runs
`rclone serve webdav` and talks WebDAV to it. Its blob path was purged before
the run so both lanes started empty.

`snapshot-e2e` drives the real `kopia` binary built from this tree through a
complete cycle against the same target: `repository create`, initial snapshot of
a synthetic 150 MB / 800-file tree, a mutation (5 % of files deleted, 5 % added,
2 % appended to), an incremental snapshot, `snapshot verify
--verify-files-percent=100`, and `maintenance run --full --safety=none`. Each
stage is one process invocation, timed by the harness around it.

---

## Results

Medians over 5 trials, with the `[p10..p90]` interval. `REAL` means the two
intervals do not overlap (see `../SCHEMA.md` §6); the delta is rclone relative to
native, and every positive number here means rclone is slower.

### Wall clock of the measured phase

| workload | native median [p10..p90] | rclone median [p10..p90] | rclone vs native | flag |
|---|---|---|---|---|
| `small-write` (100 × 2–8 KB) | 120.9 s [120.0 s..123.6 s] | 199.0 s [193.4 s..205.2 s] | +64.6% | REAL |
| `pack-write` (5 × 20 MB) | 19.0 s [18.1 s..23.7 s] | 30.3 s [26.9 s..32.6 s] | +59.0% | REAL |
| `read-metadata:hot` (50) | 7.10 s [7.05 s..7.25 s] | 32.7 s [31.7 s..35.9 s] | +359.6% | REAL |
| `read-metadata:warm` (50) | 9.29 s [9.06 s..9.71 s] | 32.6 s [30.8 s..33.1 s] | +251.1% | REAL |
| `read-metadata:cold` (50) | 9.08 s [9.03 s..9.56 s] | 32.3 s [30.3 s..32.7 s] | +255.2% | REAL |
| `range-read` (50 ranges) | 27.3 s [27.0 s..27.8 s] | 99.9 s [96.0 s..108.6 s] | +266.6% | REAL |
| `list-heavy:full` (5 traversals) | 1.04 s [1.01 s..1.31 s] | 38.5 s [11.3 s..59.5 s] | +3598.0% | REAL † |
| `list-heavy:prefixed` (5) | 991 ms [953 ms..1.09 s] | 3.01 s [2.98 s..3.09 s] | +203.5% | REAL |
| `delete-sweep` (50 deletes, par. 8) | 2.38 s [2.34 s..2.76 s] | 2.99 s [2.89 s..3.88 s] | +25.5% | REAL |
| `snapshot-e2e:create` | 12.9 s [12.7 s..13.2 s] | 28.0 s [23.4 s..31.4 s] | +117.7% | REAL |
| `snapshot-e2e:init` | 48.6 s [47.0 s..49.4 s] | 66.2 s [63.8 s..68.8 s] | +36.2% | REAL |
| `snapshot-e2e:incremental` | 12.6 s [12.3 s..13.4 s] | 20.9 s [19.2 s..21.4 s] | +65.7% | REAL |
| `snapshot-e2e:verify` | 77.3 s [77.2 s..77.4 s] | 251.2 s [247.1 s..259.7 s] | +224.8% | REAL |
| `snapshot-e2e:maintenance` | 32.7 s [29.9 s..34.3 s] | 55.5 s [51.1 s..60.6 s] | +69.8% | REAL |

† see "How to read `list-heavy:full`" below — the honest number is smaller.

### Median per-operation latency (p50)

| workload | native | rclone | rclone vs native | flag |
|---|---|---|---|---|
| `small-write` | 1.21 s | 1.91 s | +58.0% | REAL |
| `pack-write` | 3.82 s | 5.27 s | +38.0% | REAL |
| `read-metadata:hot` | 137 ms | 637 ms | +365.6% | REAL |
| `read-metadata:warm` | 180 ms | 620 ms | +244.7% | REAL |
| `read-metadata:cold` | 177 ms | 608 ms | +244.1% | REAL |
| `range-read` | 534 ms | 1.93 s | +261.9% | REAL |
| `list-heavy:full` | 203 ms | 7.71 s | +3699.8% | REAL † |
| `list-heavy:prefixed` | 207 ms | 602 ms | +190.4% | REAL |
| `delete-sweep` | 360 ms | 424 ms | +17.6% | REAL |

The `snapshot-e2e` stages are one process invocation each, so their p50 equals
their wall clock and is not repeated here.

### The end-to-end backup cycle

Summing the five `snapshot-e2e` stage medians — 150 MB, 800 files, create →
backup → mutate → incremental → verify everything → full maintenance:

| lane | total |
|---|---|
| native | **184 s** |
| rclone | **422 s** |

2.3x, and the biggest single contributor is `verify`: downloading and hashing the
whole repository takes 77 s natively and 251 s through rclone.

### Quota units — do not read this table as a win

| workload | native | rclone | flag |
|---|---|---|---|
| every blob-level workload | 250 – 15,000 QU | **0** | REAL "better" |

This is an artifact and the report flags it rather than hiding it. The harness
counts Drive quota units from its own instrumented HTTP transport, which only
exists in the native backend; rclone's traffic reaches Drive through a child
process the harness cannot see, so it is recorded as `quota_units_source: "none"`
and totals zero. **rclone spends Drive quota too — probably more, since it makes
more round trips.** The only quota figures in this document that are comparable
between lanes are the `snapshot-e2e` rows, where both lanes are estimated from
bytes moved and therefore agree by construction.

The one genuinely comparable quota fact: over the whole run the native lane spent
214,500 *observed* quota units for 1.35 GB of measured traffic, and the run
averaged 1,393 QU/min — 0.43 % of the per-user ceiling. Quota is not the binding
constraint at this scale; latency is.

---

## How to read `list-heavy:full`

The +3598 % is real in the sense that the intervals are nowhere near each other,
but the size of it is inflated by an effect that is half benchmark artifact and
half genuine property, and it should not be quoted as a headline.

Kopia's rclone/WebDAV path stores blobs in a **sharded directory tree**; a whole-
storage traversal walks every directory. Every trial writes blobs under new
random names, which creates new shard directories, and deleting a blob does not
delete the directory that held it. So the rclone lane's traversal got steadily
more expensive within the run, trial by trial: 3.2 s, 23.6 s, 38.5 s, 51.8 s,
64.6 s — which is exactly the `[11.3 s..59.5 s]` spread in the table. The native
backend keeps one flat Drive folder and its traversal stayed at 1.0 s throughout.

The artifact part is that a real repository reuses its shard directories instead
of inventing new ones every trial. The genuine part is that empty shard
directories accumulate in a long-lived rclone-backed repository and nothing ever
prunes them, so the cost is real, just slower-growing. Treat trial 1's ratio
(3.2 s vs 1.0 s, ~3x) as the defensible steady-state number and the rest as a
demonstration of how the two layouts age differently.

---

## Interpretation

Native wins every timed comparison, and the pattern of the wins says what it is
winning at: the gap is smallest where the wire time dominates (`pack-write`
+59 %, `delete-sweep` +26 %) and largest where a per-operation round trip
dominates (`read-metadata` +250-360 %, `range-read` +267 %). That is the expected
shape for removing a layer: rclone adds a local WebDAV hop and, in Kopia's rclone
backend, a `vfs/forget` remote-control call before every list, metadata read and
blob read, so every cheap operation pays two extra round trips. The end-to-end
number is the one worth quoting to a user — a full backup-plus-verify cycle takes
2.3x longer through rclone — and it is more modest than the microbenchmarks
because a real backup spends much of its time uploading bytes, where the two
lanes are closest.

Where native does *not* look good is in absolute terms, and this comparison
should not be allowed to hide it. A 2–8 KB blob write costs **1.21 s**, and the
transport counters say why: every `PutBlob` issues `files.list` (a name lookup)
followed by the upload — 100 blobs, 200 API calls, and the lookup is a
100-quota-unit list call. Writing 100 index-sized blobs takes two minutes. Beating
rclone at 1.91 s is not the same as being fast, and the pre-write name lookup is
the obvious first target for Phase 4: it is a full round trip and 100 QU per
blob, spent to answer a question the file-ID cache should usually already know.

The `--true-cold` experiment produced a negative result that is worth recording.
`read-metadata:warm` (fresh connection, on-disk file-ID journal intact) and
`read-metadata:cold` (fresh connection, journal deleted) are statistically
indistinguishable — 9.29 s vs 9.08 s, overlapping intervals — and their transport
profiles are *identical*: 50 `files.list` calls each, no `files.get`. The hot
pass, on the connection that wrote the blobs, made 50 `files.get` calls instead
and cost 250 QU against the other two passes' 5,000 QU each. In other words the
in-memory file-ID map is worth 20x the quota and ~25 % of the latency, and the
**persistent journal currently contributes nothing across a reconnect** even
though it is written to disk correctly (the journal file contains the entries).
Either the replay is not repopulating the map that `GetMetadata` consults, or the
entries are not on disk yet at the moment the next connection opens. This is a
flag for Phase 4, not a conclusion — but until it is resolved, the true-cold
purge measures the same thing the warm pass does, which is why both rows are
published rather than just one.

Two more caveats on the numbers themselves. The `snapshot-e2e` byte and quota
columns are **derived from the source tree size, not measured** — the uploader
runs in a child process — so they are upper bounds that ignore deduplication and
compression, and they are equal between lanes by construction rather than by
measurement. And this is one machine on one network on one afternoon: five trials
of ABBA interleaving protect against a slow ten minutes, not against a systematic
property of this connection. The result to trust is the direction and the rough
magnitude, not the third digit.

---

## Files

- `native-vs-rclone-initial.compare.json` — the machine-readable comparison
  (`gdrive-bench compare --json-out`), every workload × metric with its
  significance verdict.
- `recommended-baseline.json` — the run result promoted with
  `gdrive-bench baseline set`; `gdrive-bench baseline check <new-result>` fails
  on any `REAL` worsening of `wall_clock_ms` or `p50_ms` beyond 15 %.

The raw run result and the per-trial dumps stay in `$KOPIA_GDRIVE_BENCH_RESULTS`
and are deliberately not committed.
