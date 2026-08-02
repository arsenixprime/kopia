# Tuning the native Google Drive backend

**Audience: an agent or engineer with no other context.** Everything needed to
run one complete tuning cycle is in this file. Read it top to bottom once, then
work the checklist in §3.

The deliverable of a cycle is **a decision with evidence**, not a speedup. A
rejected hypothesis that is written down is worth as much as an adopted one,
because it stops the next session from re-running it.

Three documents define the ground rules and this one does not repeat them:

| Document | What it settles |
|---|---|
| `SCHEMA.md` | the shape of every result and comparison document, and **the significance rule** (§6) |
| `results/native-vs-rclone-initial.md` | the numbers this backend starts from, and its erratum |
| `gdrive-bench knobs` | every knob, its default, its legal range and the workloads it touches |

Two more you will read and write every cycle:

| Document | What it is |
|---|---|
| `$KOPIA_GDRIVE_BENCH_RESULTS/TUNING_LOG.md` | the durable, append-only record of every cycle, **including rejected ones** — read it first, so you do not re-run a settled hypothesis (§4) |
| `gdrive-tuned.json` (this directory) | the shippable recommendation: the `tuning` object with only measured values in it (§8) |

---

## 1. The one rule

> **Act only on `REAL`.**

`REAL` means the `[p10..p90]` intervals of the two series do not overlap
(`SCHEMA.md` §6). `WEAK` means "measure again with more trials". `NOISE` means
"there is nothing here". Neither is ever a reason to ship a knob change. The
significance rule is crude on purpose so that it is unambiguous; do not
substitute judgement for it, and do not eyeball medians.

---

## 2. Preconditions

Run these before every cycle. Every one of them has bitten someone.

```bash
# 1. Credentials and folders. Shell state does not persist between commands in
#    an agent session, so source this inline in EVERY command that needs it.
set -a; source ~/secrets/gdrive.env; set +a

# 2. The results directory must exist and must be outside the repository.
echo "$KOPIA_GDRIVE_BENCH_RESULTS"        # e.g. /home/<user>/kopia-bench-results
ls "$KOPIA_GDRIVE_BENCH_RESULTS"

# 3. A promoted baseline must exist (baseline check needs it).
ls -l "$KOPIA_GDRIVE_BENCH_RESULTS/baseline.json"

# 4. Quota-ledger headroom for today (UTC). The run subcommand enforces this
#    itself and refuses with exit 2 at the cap, but look first so a two-hour
#    plan does not die at minute zero.
cat "$KOPIA_GDRIVE_BENCH_RESULTS/quota-ledger.json"

# 5. Build the harness. It is behind the !no_extra_providers build tag; the
#    default build is the right one.
go build -o /tmp/gdrive-bench ./tools/gdrive-bench
/tmp/gdrive-bench knobs | head
```

Also check that **no other tuning run is in flight** (§6) — `pgrep -af
gdrive-bench`.

---

## 3. One cycle, step by step

### 3.1 Pick exactly one hypothesis

Write it down before touching anything, in this shape:

| Part | Example |
|---|---|
| **Knob** | `tuning.pacerMinSleepMS`, `100` → `50` |
| **Predicted movement** | `delete-sweep` `wall_clock_ms` down ≥ 15 % |
| **Why** | the pacer is a token bucket; only a workload that outruns the refill rate can feel `minSleep` |
| **Workload set** | `small-write,read-metadata,delete-sweep` |
| **Safety metric** | `errors` = 0 everywhere; `errors_by_reason` gains no `rate_limit_exceeded` / `user_rate_limit_exceeded` / `http_403` |

One knob. A hypothesis that changes two knobs cannot be attributed, and the
ABBA machinery below buys you nothing.

Before predicting, read the knob's `doc` string in `gdrive-bench knobs` and, if
the knob is a pacer knob, read `repo/blob/gdrive/gdrivepacer/gdrivepacer.go` —
the pacer is `golang.org/x/time/rate` with rate `1/minSleep` and burst
`pacerBurst` (default 10 calls/s, burst 100). **A workload that issues fewer
API calls than the burst pool, or that issues them slower than the refill rate,
cannot be affected by `minSleep` at all.** Predict accordingly, and prefer a
prediction that can be falsified.

**For any pacer hypothesis, `paced_wait_ms` is the falsifier.** Every workload
phase reports the total time its calls spent waiting for the token bucket
(`SCHEMA.md` §8.6; per method in `transport_delta.perMethod[…].pacedWaitMS`).
`paced_wait_ms = 0` on the control lane means the pacer's steady-state interval
never bound at all, so **no value of `pacerMinSleepMS` or `pacerBurst` can
change anything** and the cycle is answered without reading a single wall clock.
Design the workload set until the control lane shows a non-zero
`paced_wait_ms`, *then* run the comparison.

### 3.2 Build a two-target targets file

The controlled experiment is **one run with two targets**, not two runs. The
harness's ABBA interleaving (`--order abba`, the default) alternates which
target goes first in each trial, so a slow ten minutes on the network hits both
lanes equally. Two separate runs cannot do this and their comparison is not
evidence.

- **`control`** = exactly what `gdrive-tuned.json` currently recommends (as of
  cycle 1 every one of its values is still the built-in default, so an empty
  `tuning` object and a verbatim copy of `gdrive-tuned.json` are equivalent
  controls; prefer the verbatim copy, it stays correct after the first adopt).
- **`candidate`** = the same, plus the single changed knob.

`cacheDir` is the one exception on both sides: it is a harness-side isolation
setting, not a performance recommendation, and both lanes always set it (to
*different* directories — see below).

```jsonc
[
  {"name":"control","connection":{"type":"gdrive","config":{
    "folderID":"${KOPIA_GDRIVE_BENCH_FOLDER_NATIVE}",
    "credentialsFile":"${KOPIA_GDRIVE_TEST_CREDENTIALS}",
    "tuning":{"cacheDir":"/abs/path/scratch/cache-control"}}}},
  {"name":"candidate","connection":{"type":"gdrive","config":{
    "folderID":"${KOPIA_GDRIVE_BENCH_FOLDER_NATIVE}",
    "credentialsFile":"${KOPIA_GDRIVE_TEST_CREDENTIALS}",
    "tuning":{"cacheDir":"/abs/path/scratch/cache-candidate","pacerMinSleepMS":50}}}}
]
```

`${VAR}` is expanded from the environment and an undefined variable is a hard
error, so a typo can never silently point a lane somewhere else.

Three requirements that are easy to get wrong:

1. **Same `folderID` for both lanes.** The Drive-side environment (folder child
   count, the account, the quota bucket) is then identical by construction. It
   is safe: blob names are namespaced `bench-<run_id>-t<trial>-<workload>-`,
   the two lanes never run concurrently, each workload cleans its prefix up
   before the next lane starts, and the backend deletes permanently rather than
   trashing, so a name is genuinely free when the next lane reuses it. Watch
   the result's top-level `errors[]` for cleanup failures — residue would make
   the *next* lane's `PutBlob` take the update path instead of the create path
   and quietly invalidate the comparison.
2. **A different `cacheDir` per lane**, both under a scratch directory you own.
   Sharing one file-ID journal lets one lane warm the other's cache.
   `--true-cold` additionally *refuses to start* unless every Drive target sets
   `cacheDir`.
3. **Pin `--seed`.** With a fixed seed the run is byte-for-byte reproducible;
   the harness records the seed either way, but pinning it makes a re-run a
   one-liner.

### 3.3 Run it

```bash
set -a; source ~/secrets/gdrive.env; set +a
/tmp/gdrive-bench run \
  --targets  /abs/path/scratch/targets-cycleN.json \
  --workloads small-write,read-metadata,delete-sweep \
  --trials 5 --order abba --seed <fixed> \
  --label tuning-cycle-N-<knob>
```

- **`--trials 5` is the floor** for any latency metric. Fewer than 3 makes the
  p10/p90 interval meaningless; 5 is the default and what every published
  number so far used. Raise it to 7–9 to resolve a `WEAK` row, never lower it.
- Pick the **smallest workload set that can falsify the hypothesis**, plus any
  workload the knob could plausibly damage. Each extra workload costs wall
  clock and quota; each omitted one is a regression you will not see.
- The harness prints its upload/quota estimate and refuses (exit 2) if the
  ledger would go past `--daily-upload-fraction × 750 GB`. Sanity-check the
  estimate against §7 before committing an hour to the run.
- The result lands at
  `$KOPIA_GDRIVE_BENCH_RESULTS/<UTC>-<label>.json`. It is written **even when a
  workload fails**.

### 3.4 Compare

One file holding two targets compares against itself.

```bash
/tmp/gdrive-bench compare --json-out /abs/path/scratch/cycleN.compare.json \
  "$KOPIA_GDRIVE_BENCH_RESULTS/<UTC>-<label>.json"
```

(Flags must precede the file: Go's `flag` package stops parsing at the first
non-flag argument.)

**The reference is the first target in the targets file.** Put `control` first
and every `delta_pct` reads *candidate relative to control*: negative means the
candidate was faster on a lower-is-better metric. `--reference=<name>` overrides
it (a series key, or a target name when unambiguous).

> **Historical note.** Before WP-4b, `compare` picked the **alphabetically
> first** series, so lanes named `control` / `candidate` were reported against
> `candidate` and every delta was inverted. Cycle 1's table in `TUNING_LOG.md`
> is written in that convention and says so; any report generated before WP-4b
> must be read with its own `reference` field in hand. Reading `reference`
> before interpreting a number is still the cheap habit — it is one line at the
> top of the printed table.

`improvement` says whether the delta's direction is the good one (only
`ops_per_sec` is higher-is-better). Read `significance` on every row, not just
the ones you care about.

### 3.5 Decide

**Adopt only if all four hold:**

1. the **target metric** moved in the predicted direction and its row is `REAL`;
2. **no other metric on any workload regressed `REAL`** — check
   `wall_clock_ms`, `p50/p90/p99_ms`, `ops_per_sec`, `errors`, `quota_units`,
   `paced_wait_ms`;
3. **no error-rate increase**: `errors` stays 0 on every workload, and summing
   `trials[].errors_by_reason` over each target shows no new or larger
   `rate_limit_exceeded`, `user_rate_limit_exceeded`, `http_403` or
   `http_429` count for the candidate;
4. the run itself was clean: top-level `errors[]` empty (cleanup failures
   included).

**Otherwise reject.** A `WEAK` improvement is a reason to re-run with more
trials, once. A second `WEAK` is a rejection: the effect is smaller than this
harness can see, which means it is smaller than it is worth shipping.

### 3.6 Follow through

**On adopt:**

1. Edit `gdrive-tuned.json`: change the knob, and append a line to
   `_provenance` naming the cycle, the run file and the measured delta.
2. Re-run the **full default workload set** with the adopted configuration as a
   single target, and promote *that*:
   ```bash
   /tmp/gdrive-bench baseline set "$KOPIA_GDRIVE_BENCH_RESULTS/<UTC>-<confirm-label>.json"
   /tmp/gdrive-bench publish  "$KOPIA_GDRIVE_BENCH_RESULTS/<UTC>-<confirm-label>.json" \
       --as recommended-baseline.json --force
   ```
   **Never promote a subset run.** `baseline check` compares whatever it is
   given against `baseline.json`, and `SCHEMA.md` §2 is explicit that two
   results are comparable only when their `config.params`, `config.trials` and
   `config.smoke` agree. A three-workload baseline silently stops guarding the
   other four.
3. Verify the promotion did not encode a regression:
   `gdrive-bench baseline check <the same file>` must exit 0.

**On reject:** change nothing except the log. Leave `gdrive-tuned.json`,
`baseline.json` and `results/` exactly as they were.

**Always**, adopt or reject, append an entry to
`$KOPIA_GDRIVE_BENCH_RESULTS/TUNING_LOG.md` (§4).

---

## 4. The log

`$KOPIA_GDRIVE_BENCH_RESULTS/TUNING_LOG.md` is the durable, append-only record.
It lives **outside the repository**, next to the raw runs it cites, and it is
the only place a rejected hypothesis is recorded — so it is the first thing to
read before choosing a hypothesis. Never rewrite an entry; append a correction
as a new one.

One entry per cycle, in this shape:

```markdown
## Cycle N — <knob>: <one-line hypothesis>   (YYYY-MM-DD)

**Hypothesis.** <knob, change, predicted movement, mechanism>
**Run.** `<UTC>-<label>.json` (compare: `<UTC>-<label>.compare.json`) —
         <workloads>, <trials> trials, ABBA, seed <n>, harness <commit>
         (+ dirty?), <wall clock>, <bytes> up, <QU>, <QU/min>.
**Safety.** errors: <…>; errors_by_reason: <…>; retries/backoffs: <…>;
            top-level errors[]: <…>.

| workload | metric | <reference lane> | <other lane> | delta | flag |
|---|---|---|---|---|---|
| … | … | … | … | … | … |

<!-- Column order follows the compare report's own "reference" field, and the
     entry says so out loud, so nobody has to guess the sign of the delta. -->

**Decision: ADOPT / REJECT.** <reasoning, referring to the four conditions of
TUNING.md §3.5, naming which one decided it>
**What this established.** <the transferable finding, even for a null result>
**Follow-through.** <what changed, or "nothing promoted, nothing reverted">
**Change requests raised.** <harness/backend defects the cycle exposed, if any>
```

---

## 5. Priority queue

Work these in order unless the log says one is already settled. Each entry
names its expected mechanism, because a hypothesis without one is a lottery
ticket.

1. **Pacer `pacerMinSleepMS` — SETTLED, do not re-run as-is.** Cycle 1 tested
   100 → 50 and got zero `REAL` rows and an identical Drive call mix. The
   mechanism explains it: the pacer is a token bucket of rate `1/minSleep`
   (10 calls/s) and burst `pacerBurst` (100), built fresh per connection, and
   **no workload in the current suite draws tokens faster than the bucket
   refills**, so `minSleep` never binds at any value. Revisiting it requires a
   workload that *sustains* more than 10 API calls/s — that means parallelism,
   not more blobs. **`pacerBurst` is the untested half** and the same arithmetic
   applies to it. Do the arithmetic before spending an hour; cycle 1 is the
   worked example — and since WP-4b you no longer have to trust the arithmetic:
   run anything at all and read `paced_wait_ms`. A zero there is the direct
   measurement cycle 1 could not take. The cheapest way to reach a non-zero one
   is item 2's high-parallelism `delete-sweep`, which is why that cycle should
   come first.
2. **`deleteParallelism`** (default 8, range 1..32). Mechanism: more concurrent
   `files.delete` calls in flight. Phase 2 measured parallel singles at
   19.5–23 blobs/s and batch deletes at 8.6, so the parallel path is the one
   worth sweeping.

   **Recipe (corrected after cycle 1).** Two things used to make this hypothesis
   untestable, and both are fixed:

   - The harness's own `--parallelism` (default 8) was the tighter of the two
     bounds and clamped the backend knob out of the experiment. `delete-sweep`
     now takes its parallelism from the **target's** `tuning.deleteParallelism`
     when the target sets one, so setting the knob in the targets file is
     enough — do **not** also pass `--parallelism`, which would change the
     cleanup phase of every other workload and make the run non-comparable to
     the baseline. Confirm what was actually in force by reading
     `trials[].workloads["delete-sweep"].params.parallelism` in the result.
   - The measured phase was too short to be worth timing (50 deletes, ~2.4 s).
     Pass `--delete-sweep-n 400` (or more) so the phase lasts tens of seconds
     and the difference is bigger than the trial-to-trial spread. Both lanes get
     the same `-n`, so the comparison stays controlled; the number is recorded
     in `config.params.delete_sweep_n`, so a run at a different `-n` is simply
     not comparable to one at the default.

   ```jsonc
   [
     {"name":"control","connection":{"type":"gdrive","config":{
       "folderID":"${KOPIA_GDRIVE_BENCH_FOLDER_NATIVE}",
       "credentialsFile":"${KOPIA_GDRIVE_TEST_CREDENTIALS}",
       "tuning":{"cacheDir":"/abs/scratch/cache-control","deleteParallelism":8}}}},
     {"name":"candidate","connection":{"type":"gdrive","config":{
       "folderID":"${KOPIA_GDRIVE_BENCH_FOLDER_NATIVE}",
       "credentialsFile":"${KOPIA_GDRIVE_TEST_CREDENTIALS}",
       "tuning":{"cacheDir":"/abs/scratch/cache-candidate","deleteParallelism":24}}}}
   ]
   ```
   ```bash
   /tmp/gdrive-bench run --targets … --workloads delete-sweep \
     --delete-sweep-n 400 --trials 5 --order abba --seed <fixed> \
     --label tuning-cycle-N-deleteparallelism
   ```

   Spell the control's `8` out rather than leaving it unset: the two lanes then
   differ in exactly one JSON value, and `targets[].knobs` in the result proves
   it. Watch `errors_by_reason` closely — this is the knob most likely to earn
   a `userRateLimitExceeded`. At 24 concurrent deletes the workload draws well
   over 10 calls/s, so it is also the **first workload in the suite that can
   make `pacerMinSleepMS` bind**: check `paced_wait_ms` (§below) to see whether
   it did.
3. **`uploadChunkSizeMB` × pack size grid**, for `pack-write`. Mechanism:
   fewer, larger resumable chunks trade request count (each chunk is an
   edit-class request = quota) against retry granularity. Sweep
   `uploadChunkSizeMB` in {8, 32, 64, 128} against `--pack-size-mb` in {20,
   100}. This is the one hypothesis where `quota_units` is a first-class target
   metric, not just a guard.
4. **`listPageSize` / `files.list` fields.** Low expectation, listed for
   completeness: our data says listing is *latency*-bound, not page-bound —
   `list-heavy:full` is ~200 ms per whole-storage traversal and the page size
   is already at the API maximum of 1000. Only worth a cycle if a repository
   large enough to page ever becomes available.
5. **`--pack-size-mb` guidance for the docs.** Not a backend knob: the output
   is a sentence in the user documentation about what `--max-pack-size` to
   choose for Drive. Mechanism: larger packs amortize the per-`PutBlob`
   `files.list` name lookup (100 quota units and a full round trip **per
   blob** — the single largest known inefficiency, see
   `results/native-vs-rclone-initial.md`) over more data.

**BLOCKED — OAuth client identity ("our client vs rclone's shared client").**
The mission's quota-isolation question cannot be answered by an agent. Google's
per-project quota buckets key on the OAuth client's Cloud project, so comparing
our dedicated client against rclone's built-in shared client would require
authorizing *rclone's* client, which needs an interactive browser consent from
the user. Both existing lanes deliberately share our client and token, which is
what makes the published native-vs-rclone comparison controlled. Do not attempt
this cycle without the user; if it is ever unblocked, note that rclone is
retiring its shared client in 2026 anyway, so the answer may expire.

---

## 6. Safety rails

- **Never run two tuning runs concurrently.** The quota ledger
  (`quota-ledger.json`) is a single shared file with last-write-wins semantics,
  so concurrent runs mis-account the 750 GB/day cap; and two runs against the
  same Drive account contend for the same rate limits, which corrupts both
  results. Check `pgrep -af gdrive-bench` before launching.
- **Never adopt on `WEAK` or `NOISE`.** §1.
- **Watch `errors_by_reason` specifically for rate limiting.**
  `rate_limit_exceeded` and `user_rate_limit_exceeded` are how Drive says the
  knob you just relaxed is too aggressive; they can appear *without* any
  workload failing, because the pacer retries them successfully. A candidate
  that is 10 % faster and generates 403s is a rejection, not a trade-off — the
  403s are charged to a per-minute bucket shared with the user's real backups.
  A run that trips them should also be treated as evidence that the *previous*
  cycles' adopted values need re-checking together.
- **One knob per cycle.** Combined effects get their own cycle, after both
  single-knob cycles are logged.
- **Revert protocol.** `gdrive-tuned.json` is in git, so an adopted value that
  later proves wrong is undone by (a) reverting the commit that changed it, (b)
  `gdrive-bench baseline set` back to the previous promoted run — its file name
  is in the `TUNING_LOG.md` entry of the cycle that promoted it, and it is also
  the copy published at `results/recommended-baseline.json` — and (c) appending
  a new `TUNING_LOG.md` entry that says what was reverted and why. Do not
  delete the original entry.
- **Do not edit harness or backend code during a cycle.** A cycle measures one
  knob against one build; the result document records `harness.commit` and
  `harness.dirty`, and a `dirty: true` result is not attributable to a commit.
  If a cycle exposes a needed code change, log it as a finding and stop that
  branch.

---

## 7. What a cycle costs

Measured on cycle 1 (`small-write,read-metadata,delete-sweep`, 5 trials, 2
targets, default sizes):

| | |
|---|---|
| Wall clock | **57.6 min** (08:36:45Z → 09:34:18Z), plus a couple of minutes to build and set up |
| Uploaded | **5.03 MB** actual; the harness estimated 9.7 MB, so the estimator is conservative by ~2x |
| Quota units | **227,500** actual (653,000 estimated); **3,953 QU/min** average = 1.2 % of the 325,000 QU/min/user ceiling |
| Daily upload cap | negligible — 5 MB against a 375 GB budget |

Read that as: **a standard three-workload cycle costs an hour of wall clock and
essentially no quota.** The 750 GB/day guard will never be what stops you; time
will.

Scaling notes for planning a different cycle:

- The dominant cost is **wall clock, not quota**. `small-write` alone is ~2 min
  per lane per trial (100 blobs at ~1.2 s each), and `read-metadata` and
  `delete-sweep` each spend ~1–2 min *setting up* blobs before the measured
  phase. One lane-trial of the three-workload set is ~5.8 min; multiply by
  targets × trials. Of cycle 1's 57.6 minutes, only 23.5 were *measured* phase —
  the rest was setup, cleanup and cooldown.
- Adding `pack-write` costs ~100 MB of upload per lane per trial (5 × 20 MB)
  and ~20 s; adding `range-read` adds ~40 MB of setup upload and ~30 s; adding
  `snapshot-e2e` adds ~150 MB and ~3–7 min per lane per trial.
- A full-suite promotion run with `snapshot-e2e` and `--true-cold` is the
  2 h 46 min / 2.71 GB / 231.5k QU shape recorded in
  `results/native-vs-rclone-initial.md`. Budget an afternoon.

---

## 8. `gdrive-tuned.json`

`gdrive-tuned.json` in this directory is the **shippable recommendation**: the
`tuning` object to paste into a Kopia Drive connection, carrying only knobs
whose values have been *measured*. It is a normal `gdrive.Options.Tuning`
document (`repo/blob/gdrive/gdrive_options.go`) plus one extra key:

- **`_provenance`** — an array of strings recording where each value came from.
  JSON has no comments, so the provenance lives in the document.

**This extra key is safe, and that has been verified rather than assumed.**
`blob.ConnectionInfo.UnmarshalJSON` (`repo/blob/config.go`) decodes the
`config` object with a plain `json.Unmarshal` into `*gdrive.Options`, and
nothing on that path calls `Decoder.DisallowUnknownFields` — the only four uses
of it in the tree are in `cli/command_policy_*` and test helpers. Unknown keys
are therefore ignored by `encoding/json`'s default behaviour. Confirmed live:
a targets file whose `tuning` object carried both `_provenance` and a
nonsense `totallyUnknownKnob` loaded without error and reached the quota guard.

Two consequences worth knowing:

- The same tolerance means **a misspelled knob is silently ignored**. After
  editing `gdrive-tuned.json`, confirm the value actually took effect by
  reading `targets[].knobs` in the next result document — that field is the
  tuning object *as the targets file provided it*, so a typo shows up there
  next to a metric that did not move.
- `Options.Tuning` is tagged `omitzero`, so an all-defaults recommendation
  serializes away to nothing. That is deliberate: shipping
  `gdrive-tuned.json` with no measured overrides is a real, honest state, and
  the file documents it in `_provenance` instead of inventing values.

When a cycle adopts a knob, set it here **and** append the provenance line in
the same edit. A value in this file without a `_provenance` line naming a run
is not a recommendation; it is a guess, and the next cycle should delete it.
