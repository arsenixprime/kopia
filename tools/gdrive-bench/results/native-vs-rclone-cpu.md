# Native Google Drive backend vs. rclone — wall clock **and CPU**

**This report supersedes `native-vs-rclone-initial.md`.** It is the same
experiment on the same machine, the same account and the same random seed, with
three things the first run did not have: **CPU accounting**, a corrected
`read-metadata:warm` pass (the first report's erratum is closed here), and
pacer steady-state instrumentation. Where the two disagree, this one is the
authority.

**Question:** on real Google Drive, with everything except the backend held
constant, how does Kopia's native `gdrive` backend compare to the same Kopia
driving the same Drive through `rclone serve webdav` — in time *and* in CPU?

**Verdict:** native is faster on every timed row and cheaper on **every** CPU
row, all `REAL` except one (`delete-sweep` wall clock, `WEAK`). The end-to-end
backup cycle takes **180 s and 9.4 s of CPU** natively against **418 s and
29.8 s of CPU** through rclone — 2.3x the time and **3.2x the CPU**. There is no
row where rclone is genuinely faster or cheaper.

---

## Setup

| | |
|---|---|
| Run | `20260802T141202Z-native-vs-rclone-cpu.json` |
| When | 2026-08-02 14:12:02Z → 16:59:07Z (2 h 47 min) |
| Trials | 5 per lane, `--order=abba` (A B / B A / A B / B A / A B), 10 s cooldown |
| Workloads | the six blob-level ones + `snapshot-e2e`, `--true-cold` |
| Seed | `1785648202915059831` — **the same seed as the first comparison**, so both lanes wrote byte-identical content in both runs |
| Harness commit | `17f104c4` (working tree dirty: the WP-6 CPU and graph changes were not yet committed) |
| rclone | v1.75.0, built from source at tag `v1.75.0` (reports itself as `v1.75.0-DEV`: no release ldflags) |
| Uploaded | 2.71 GB · 207,750 quota units · ~1,243 QU/min (0.38 % of the 325,000 QU/min/user ceiling) |
| Errors | none — zero failed operations, zero retries, zero Drive error reasons, zero pacer waits in any trial |

**Both lanes are the same account, the same OAuth client and the same scope.**
The native target authenticates with the `authorized_user` token of OAuth client
`949080498481-…`; the rclone remote `gdrive-bench` was synthesized from that same
token, with the same `client_id`, and requests the same `drive.file` scope. Each
lane writes into its own app-created folder (`kopia-gdrive-bench` for native,
`kopia-gdrive-bench-rclone` for rclone). The network, machine, account, quota
bucket and permission scope are identical, and ABBA interleaving means a slow ten
minutes hits both lanes equally.

`snapshot-e2e` drives the real `kopia` binary built from this tree through a
complete cycle: `repository create`, initial snapshot of a synthetic
150 MB / 800-file tree, a mutation (5 % of files deleted, 5 % added, 2 %
appended to), an incremental snapshot, `snapshot verify
--verify-files-percent=100`, and `maintenance run --full --safety=none`.

**Housekeeping before the run, and the bias it leaves.** Both lanes were emptied
first: the native folder to zero objects, and the rclone lane to zero objects
plus a `rclone rmdirs` that removed 30 of its 31 leftover shard directories. The
31st — `bench-blobs/p` — **cannot be deleted under the `drive.file` scope**
(`403 appNotAuthorizedToChild`: the app may not write to all children of a folder
it did not create every child of). It is empty, and it is the only residue the
run started with. It does **not** remove the trial-over-trial growth described
under "How to read `list-heavy:full`" below, which is generated *within* the run.

---

## Results

Medians over 5 trials, with the `[p10..p90]` interval. `REAL` means the two
intervals do not overlap (see `../SCHEMA.md` §6); the delta is rclone relative to
native, and every positive number means rclone is worse.

### Wall clock of the measured phase

| workload | native median [p10..p90] | rclone median [p10..p90] | rclone vs native | flag |
|---|---|---|---|---|
| `small-write` (100 × 2–8 KB) | 124.1 s [123.1 s..125.8 s] | 205.1 s [193.8 s..213.8 s] | +65.2% | REAL |
| `pack-write` (5 × 20 MB) | 17.7 s [17.6 s..21.1 s] | 29.4 s [27.6 s..30.2 s] | +66.0% | REAL |
| `read-metadata:hot` (50) | 7.18 s [6.95 s..7.46 s] | 32.0 s [30.8 s..33.8 s] | +345.8% | REAL |
| `read-metadata:warm` (50) | 7.39 s [6.83 s..7.62 s] | 31.5 s [31.1 s..31.8 s] | +325.8% | REAL |
| `read-metadata:cold` (50) | 9.11 s [8.69 s..9.45 s] | 31.7 s [31.4 s..32.7 s] | +247.9% | REAL |
| `range-read` (50 ranges) | 27.3 s [26.3 s..28.6 s] | 99.6 s [96.7 s..122.3 s] | +265.1% | REAL |
| `list-heavy:full` (5 traversals) | 1.03 s [984 ms..1.08 s] | 38.9 s [12.0 s..61.0 s] | +3664.8% | REAL † |
| `list-heavy:prefixed` (5) | 1.06 s [988 ms..1.20 s] | 3.11 s [3.02 s..3.21 s] | +193.4% | REAL |
| `delete-sweep` (50 deletes, par. 8) | 2.53 s [2.33 s..4.31 s] | 2.92 s [2.85 s..3.21 s] | +15.5% | **WEAK** |
| `snapshot-e2e:create` | 13.0 s [12.3 s..13.7 s] | 27.4 s [24.0 s..31.8 s] | +111.1% | REAL |
| `snapshot-e2e:init` | 47.5 s [45.0 s..50.5 s] | 67.2 s [64.3 s..71.1 s] | +41.6% | REAL |
| `snapshot-e2e:incremental` | 12.6 s [10.9 s..12.8 s] | 19.9 s [18.0 s..21.4 s] | +57.8% | REAL |
| `snapshot-e2e:verify` | 77.3 s [77.3 s..77.4 s] | 249.8 s [247.6 s..261.2 s] | +223.1% | REAL |
| `snapshot-e2e:maintenance` | 29.7 s [29.0 s..31.0 s] | 53.2 s [53.2 s..55.1 s] | +79.2% | REAL |

† see "How to read `list-heavy:full`" — the honest number is much smaller.

`delete-sweep` is the one row that lost its `REAL` flag relative to the first
comparison, and it lost it on the **native** side: one native trial took 4.3 s
against the other four at ~2.4 s, which widened the native interval enough to
overlap rclone's. The per-operation view of the same workload is still `REAL`
(+16.6 %), and the direction is unchanged. This is the row the first report
already named as the only margin an rclone-side tuning pass could plausibly
close; it is now also the only row this run cannot call significant on wall
clock.

### Median per-operation latency (p50)

| workload | native | rclone | rclone vs native | flag |
|---|---|---|---|---|
| `small-write` | 1.22 s | 1.95 s | +59.6% | REAL |
| `pack-write` | 3.43 s | 5.14 s | +49.8% | REAL |
| `read-metadata:hot` | 141 ms | 639 ms | +353.3% | REAL |
| `read-metadata:warm` | 144 ms | 612 ms | +325.1% | REAL |
| `read-metadata:cold` | 178 ms | 624 ms | +250.9% | REAL |
| `range-read` | 535 ms | 1.94 s | +262.9% | REAL |
| `list-heavy:full` | 208 ms | 7.43 s | +3474.1% | REAL † |
| `list-heavy:prefixed` | 217 ms | 616 ms | +184.1% | REAL |
| `delete-sweep` | 364 ms | 424 ms | +16.6% | REAL |

The `snapshot-e2e` stages are one process invocation each, so their p50 equals
their wall clock and is not repeated.

### CPU cost to the machine

This is the new material. **`cpu_total_ms` is the harness process plus every
descendant of it, user + system** — see `../SCHEMA.md` §8.8.

> **Fairness, both ways.** On the native lane the backend *is* the harness, so
> `cpu_total_ms` is almost entirely the harness's own CPU and the descendant
> column is zero. On the rclone lane it is the harness (Kopia's WebDAV client)
> **plus** the `rclone serve webdav` subprocess — and inside `snapshot-e2e`,
> plus the rclone *grandchild* that the child kopia spawned. Neither column is
> "the backend's CPU" on its own. **The sum is the comparable number, and it is
> the sum both lanes report.**

| workload | native median [p10..p90] | rclone median [p10..p90] | rclone vs native | flag |
|---|---|---|---|---|
| `small-write` | 4.17 s [4.13 s..4.18 s] | 7.36 s [7.04 s..7.82 s] | +76.4% | REAL |
| `pack-write` | 684 ms [649 ms..778 ms] | 1.18 s [1.14 s..1.23 s] | +72.1% | REAL |
| `read-metadata:hot` | 246 ms [242 ms..274 ms] | 1.18 s [1.17 s..1.30 s] | +379.3% | REAL |
| `read-metadata:warm` | 268 ms [242 ms..279 ms] | 1.21 s [1.18 s..1.23 s] | +349.8% | REAL |
| `read-metadata:cold` | 324 ms [311 ms..345 ms] | 1.22 s [1.17 s..1.25 s] | +276.0% | REAL |
| `range-read` | 931 ms [895 ms..992 ms] | 3.96 s [3.76 s..4.72 s] | +325.6% | REAL |
| `list-heavy:full` | 49 ms [44 ms..52 ms] | 1.58 s [498 ms..2.43 s] | +3138.4% | REAL † |
| `list-heavy:prefixed` | 44 ms [41 ms..54 ms] | 135 ms [130 ms..148 ms] | +205.4% | REAL |
| `delete-sweep` | 98 ms [96 ms..152 ms] | 166 ms [163 ms..180 ms] | +69.0% | REAL |
| `snapshot-e2e:create` | 776 ms [746 ms..807 ms] | 1.61 s [1.47 s..1.81 s] | +107.7% | REAL |
| `snapshot-e2e:init` | 2.31 s [2.17 s..2.41 s] | 3.67 s [3.61 s..3.78 s] | +58.8% | REAL |
| `snapshot-e2e:incremental` | 622 ms [559 ms..650 ms] | 1.07 s [1.02 s..1.13 s] | +71.5% | REAL |
| `snapshot-e2e:verify` | 4.43 s [4.32 s..4.47 s] | 20.6 s [20.3 s..21.1 s] | +365.7% | REAL |
| `snapshot-e2e:maintenance` | 1.28 s [1.23 s..1.33 s] | 2.78 s [2.64 s..2.78 s] | +117.8% | REAL |

**Every CPU row is `REAL`, including `delete-sweep`, which wall clock could not
call.** CPU turns out to be the *less* noisy of the two measurements here: it
does not depend on Drive's tail latency.

Where the rclone lane's CPU actually goes (medians):

| workload | lane | harness (self) | descendants | total | `cpu_pct` |
|---|---|---|---|---|---|
| `small-write` | native | 4.17 s | 0 | 4.17 s | 3.4 % |
| `small-write` | rclone | 6.81 s | 540 ms | 7.36 s | 3.6 % |
| `read-metadata:hot` | native | 246 ms | 0 | 246 ms | 3.5 % |
| `read-metadata:hot` | rclone | 1.06 s | 120 ms | 1.18 s | 3.8 % |
| `range-read` | native | 931 ms | 0 | 931 ms | 3.4 % |
| `range-read` | rclone | 3.37 s | 590 ms | 3.96 s | 3.9 % |
| `list-heavy:full` | native | 49 ms | 0 | 49 ms | 4.7 % |
| `list-heavy:full` | rclone | 1.41 s | 170 ms | 1.58 s | 4.1 % |
| `snapshot-e2e:verify` | native | 2.60 s | 1.82 s | 4.43 s | 5.7 % |
| `snapshot-e2e:verify` | rclone | 8.34 s | **12.3 s** | 20.6 s | 8.1 % |

Two things worth reading off this table.

1. **For the in-process workloads, most of the rclone lane's extra CPU is in the
   harness, not in rclone.** Kopia's rclone backend pays a local WebDAV hop —
   HTTP framing, a `vfs/forget` remote-control call before every list, metadata
   read and blob read, XML PROPFIND parsing — and that cost is charged to *Kopia*,
   not to the subprocess. The rclone process itself is comparatively cheap
   (120–590 ms per phase). The layer is not free even when the thing under it is.
2. **For `snapshot-e2e`, the descendants dominate**, and that column is where the
   grandchild lives: 12.3 s of descendant CPU on `verify` against native's 1.8 s.
   A `snapshot-e2e` stage on the rclone lane is `kopia` → `rclone serve webdav` →
   Drive, and the middle process is a *grandchild* of the harness. Accounting
   that only looked at direct children would have reported roughly the native
   number here and made the rclone lane look free.

Neither lane ever saturated a core: `cpu_pct` runs 3.4–8.1 %. Both are
latency-bound, not CPU-bound, at this scale — CPU is a secondary cost, and the
ratio between the lanes is the interesting part, not the absolute value.

### The end-to-end backup cycle

Summing the five `snapshot-e2e` stage medians — 150 MB, 800 files, create →
backup → mutate → incremental → verify everything → full maintenance:

| lane | wall clock | CPU |
|---|---|---|
| native | **180 s** | **9.4 s** |
| rclone | **418 s** | **29.8 s** |
| ratio | **2.3x** | **3.2x** |

The biggest single contributor to both is `verify`: downloading and hashing the
whole repository takes 77 s and 4.4 s of CPU natively, and 250 s and 20.6 s of
CPU through rclone.

### Quota units — native lane only, and not a win

| | |
|---|---|
| native, **counted** from the instrumented transport | 173,750 QU |
| native + rclone, **estimated** from bytes moved (`snapshot-e2e` only) | 17,000 QU each |
| rclone, blob-level workloads | **not observable** |

The harness counts Drive quota units from its own instrumented HTTP transport,
which only the native backend has. rclone's traffic reaches Drive through a child
process the harness cannot see, so its blob-level quota is recorded as
`quota_units_source: "none"` and totals zero. **That zero is "not measured", not
"free" — rclone spends Drive quota too, and probably more, since it makes more
round trips.** The `quota-units` chart therefore plots the native lane only and
says so; it also omits the `snapshot-e2e` rows, whose quota is estimated from
bytes rather than counted and would be identical on both lanes by construction.

The one comparable quota fact: the run averaged 1,243 QU/min, 0.38 % of the
per-user ceiling. Quota is not the binding constraint at this scale; latency is.

### The corrected `warm` pass

The first report's erratum is closed by measurement here. With the harness's
warm-pass recycle no longer calling `FlushCaches`, `read-metadata:warm` is now
indistinguishable from `:hot` — 7.39 s against 7.18 s, 144 ms against 141 ms
per operation — and its transport profile is identical to `:hot`'s: **50
`files.get` calls, zero `files.list`, 250 quota units**, where `:cold` makes 50
`files.list` calls and costs 5,000. The persistent file-ID journal is worth 20x
the quota and ~20 % of the latency across a reconnect, which is what it was
always supposed to be worth. Against the promoted baseline (whose `:warm` was a
second cold pass) this shows up as a −20.5 % `REAL` "improvement" that is not a
code change at all.

---

## How the CPU numbers are made, and how you can tell they are not leaking

The full contract is `../SCHEMA.md` §8.8. The short version:

- Descendant CPU is attributed **live**, from the cumulative `utime`+`stime` in
  `/proc/<pid>/stat`, read for every descendant at both phase boundaries. That is
  necessary rather than fancy: Kopia's rclone backend reaps its `rclone serve
  webdav` at storage `Close`, which happens *outside* every measured window and
  once per workload rather than once per phase. Reap-time accounting
  (`RUSAGE_CHILDREN`) alone would have reported ~0 for every rclone phase.
- A 200 ms background poll is a **catch-net only**, for a descendant that exits
  mid-phase.
- A per-process ledger reconciles the two: when a descendant disappears, exactly
  what was already attributed to it is subtracted from the `RUSAGE_CHILDREN`
  lump, so nothing is counted twice. `snapshot-e2e` stages additionally fold in
  the child's `wait4` rusage with `max()`, never `+`.

**The audit.** Every trial publishes `cpu_total_ms` (the CPU the whole trial cost
the machine) and `cpu_residual_ms` (the part no measured phase claimed). If the
phases had stopped seeing the rclone subprocess, its CPU would not vanish — it
would move into the residual.

| lane | trial CPU total (median) | attributed to phases | residual | residual share |
|---|---|---|---|---|
| native | 17.0 s | 16.4 s | 638 ms | **3.7–3.9 %** |
| rclone | 51.6 s | 48.6 s | 3.04 s | **5.6–5.9 %** |

Both are inside the band `SCHEMA.md` predicts, and the residual is stable across
all five trials of both lanes (native 628–662 ms, rclone 2.87–3.09 s). The
residual is real work: every workload's setup writes and cleanup deletes happen
outside its measured phase, and on the rclone lane `rclone serve webdav` is
spawned and shut down once per workload. **At most 6 % of either lane's CPU is
unattributed, and nothing about the rclone lane's subprocess is missing.**

**The tripwire did not fire.** The harness treats any phase with ≥ 1 s of wall
clock whose `cpu_total_ms` is below `max(20 ms, 0.5 % of wall)` as broken
accounting rather than a measurement, and publishes it as a hatched
`BROKEN-ACCOUNTING` cell instead of a bar. **No cell in this run was flagged**;
the lowest `cpu_pct` anywhere is 3.4 %.

---

## How to read `list-heavy:full`

The +3665 % is real in the sense that the intervals are nowhere near each other,
but its *size* is half benchmark artifact and it should not be quoted as a
headline.

Kopia's rclone/WebDAV path stores blobs in a **sharded directory tree**; a
whole-storage traversal walks every directory. Every trial writes blobs under new
random names, which creates new shard directories, and deleting a blob does not
delete the directory that held it. So the rclone lane's traversal got steadily
more expensive within the run:

| trial | 1 | 2 | 3 | 4 | 5 |
|---|---|---|---|---|---|
| native | 1.03 s | 1.06 s | 968 ms | 1.09 s | 1.01 s |
| rclone | 4.15 s | 23.9 s | 38.9 s | 53.1 s | 66.3 s |

which is exactly the `[12.0 s..61.0 s]` spread in the table. The native backend
keeps one flat Drive folder and its traversal stayed at ~1.0 s throughout.

The artifact part is that a real repository reuses its shard directories instead
of inventing new ones every trial. The genuine part is that empty shard
directories accumulate in a long-lived rclone-backed repository and **nothing
ever prunes them** — and, on `drive.file`, nothing *can*: the pre-run
`rclone rmdirs` failed on the top shard directory with
`403 appNotAuthorizedToChild`. Treat **trial 1's ratio (4.15 s vs 1.03 s, ~4x)
as the defensible steady-state number** and the rest as a demonstration of how
the two layouts age differently. The same caveat applies to the row's CPU
(1.58 s vs 49 ms): the median carries the accumulated directories with it.

---

## Interpretation

Native wins every timed comparison and every CPU comparison, and the pattern says
what it is winning at. The wall-clock gap is smallest where wire time dominates
(`pack-write` +66 %, `delete-sweep` +16 %) and largest where a per-operation
round trip dominates (`read-metadata` +250–350 %, `range-read` +265 %). That is
the expected shape for removing a layer: rclone adds a local WebDAV hop and, in
Kopia's rclone backend, a `vfs/forget` remote-control call before every list,
metadata read and blob read, so every cheap operation pays two extra round trips.

The CPU result is the same shape but sharper, and it is the one this run adds. A
full backup-plus-verify cycle costs **3.2x the CPU** through rclone, and the
majority of that excess is not rclone's own work but the WebDAV client Kopia has
to run in front of it. On a NAS or a laptop doing a nightly backup, that is 20 s
of CPU instead of 9 s for 150 MB — and it scales with the data, because `verify`
is where it concentrates.

Where native does *not* look good is in absolute terms, and this comparison must
not be allowed to hide it. **A 2–8 KB blob write still costs 1.22 s**, and the
transport counters say exactly why: every `PutBlob` issues `files.list` (a name
lookup) followed by the upload — 100 blobs, 100 list calls and 100 uploads in the
trace, and the lookup is a 100-quota-unit list call. Writing 100 index-sized
blobs takes two minutes and 4.2 s of CPU. Beating rclone's 1.95 s is not the same
as being fast, and the pre-write name lookup remains the obvious first target for
tuning: a full round trip and 100 QU per blob, spent to answer a question the
file-ID cache should usually already know.

Two caveats on the numbers themselves. The `snapshot-e2e` byte and quota columns
are **derived from the source tree size, not measured** — the uploader runs in a
child process — so they are upper bounds that ignore deduplication and
compression. And this is one machine on one network on one afternoon: five trials
of ABBA interleaving protect against a slow ten minutes, not against a systematic
property of this connection or this CPU. The result to trust is the direction and
the rough magnitude, not the third digit. That applies with extra force to CPU,
which is why `cpu_total_ms` is a *compared* metric but deliberately **not** a
baseline regression gate.

---

## Baseline check against the promoted baseline (informational)

`gdrive-bench baseline check` on this result against the baseline promoted from
the first comparison **passes, exit 0**: no `REAL` regression beyond 15 % on
`wall_clock_ms` or `p50_ms`, on either lane. Three rows are `REAL`, all
improvements, and none of them is a backend change:

| workload | metric | lane | baseline → candidate | delta |
|---|---|---|---|---|
| `read-metadata:warm` | `wall_clock_ms` | native | 9.29 s → 7.39 s | −20.5 % |
| `read-metadata:warm` | `p50_ms` | native | 180 ms → 144 ms | −19.9 % |
| `pack-write` | `p50_ms` | native | 3.82 s → 3.43 s | −10.1 % |

The two `:warm` rows are the harness defect fix, not the backend: the baseline's
warm pass was a second cold pass. `pack-write` p50 is a 390 ms shift on a
20 MB upload, i.e. the network. Everything else on both lanes is `NOISE`.

**This report does not promote a new baseline.** The native lane's numbers differ
from the baseline through no code change, and baseline management is an EM
decision; this section is information, not an action.

---

## Files

- `native-vs-rclone-cpu.compare.json` — the machine-readable comparison
  (`gdrive-bench compare --json-out`), every workload × metric with its
  significance verdict.
- `graphs/native-vs-rclone-cpu-*.svg` — the five charts, generated from **this**
  run by `gdrive-bench graph`.
- `native-vs-rclone-initial.md` — the superseded first comparison, kept for the
  record.
- `recommended-baseline.json` — the promoted baseline this run was checked
  against. **Unchanged by this run.**

The raw run result and the per-trial dumps stay in `$KOPIA_GDRIVE_BENCH_RESULTS`
and are deliberately not committed.
