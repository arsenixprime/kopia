# PROJECT_STATE — Kopia Native Google Drive Backend v2

**Branch:** `gdrive-native-v2` (fork of kopia/kopia master @ 133fc64c)
**Orchestration:** Fable 5 as EM; Opus 5 subagents implement work packages.

## Current Phase

**Phase 5 — Upstream preparation** (starting 2026-08-02)

Phase 4 GATE PASSED 2026-08-02 (9caf1c7c): TUNING.md (agent-followable
loop, four adoption conditions, REAL-only rule) + gdrive-tuned.json
(defaults w/ provenance) in repo; TUNING_LOG.md + run artifacts in
$KOPIA_GDRIVE_BENCH_RESULTS. Cycle 1 (pacerMinSleepMS 100→50) = REJECT,
zero REAL rows, pre-registered mechanism: token bucket (10/s, burst 100)
never binds at suite scale — all suite draws are sequential and slower
than refill. Baseline correctly untouched. Cost 57.6min / 5MB / 227.5k QU.
Cycle-1 change requests → WP-4b (dispatched): (1) compare reference is
alphabetical not config-order (delta-sign inversion hazard for automated
agents); (2) pacer steady-state waits uninstrumented (add paced-wait
counter so pacer hypotheses are falsifiable); (3) delete-sweep size not
flag-settable + harness --parallelism masks tuning.deleteParallelism.

**Phase 4 — Tuning loop** (COMPLETE)

Phase 3 GATE PASSED 2026-08-02: WP-3b merged 339ffd7d; curated artifacts in
tools/gdrive-bench/results/ (report .md + compare .json +
recommended-baseline.json); raw run + baseline in
$KOPIA_GDRIVE_BENCH_RESULTS (20260802T052322Z-native-vs-rclone-initial).
5 ABBA trials, 2h46m, 2.71GB up, 231.5k QU, zero errors/retries.
HEADLINE: native wins EVERY REAL timing row; e2e 184s vs 422s (2.3x);
read-metadata hot 4.6x; range-read 3.7x; list-heavy steady-state ~3x
(the 36x median is inflated by rclone shard-dir accumulation — honest
note in report); delete-sweep +25%; small-write +65%; pack-write +59%.

DEFECT CLOSED 2026-08-02 (3239593a): backend was INNOCENT — the harness
warm-pass recycle called FlushCaches (contractual full wipe, truncates
journal) before reopening, measuring cold twice. "Journal replay broken"
hypothesis DISPROVEN: replay, RescanTail, Close-flush, registry all
correct. Harness fixed; regression tests pin the contract both ways;
live re-run: :warm ≡ :hot (files.get, 250 QU) vs :cold (files.list,
5000 QU). Erratum appended to published report. Phase 4 UNBLOCKED.

PHASE 4 HYPOTHESIS BACKLOG (from comparison data):
1. small-write 1.21s/blob: PutBlob pays files.list (100 QU) per new blob
   for exists-check. Ideas: skip pre-resolve when DoNotRecreate absent??
   (semantics!), cheaper existence probe (fields=files(id) pageSize=1
   already?), pacer min-sleep sweep, write parallelism at kopia layer.
2. pacer MinSleep 100ms may be conservative for dedicated client (WP-2e
   relaxed-pacer delete data: 19.5→23 blobs/s).
3. DeleteParallelism sweep (8 default).
4. ListPageSize + fields already tight; list is latency-bound.
5. UploadChunkSize × pack-size grid for pack-write.
6. rclone residue: shard dirs undeletable under drive.file
   (appNotAuthorizedToChild) — purge rclone lane before each run.

**Phase 3 — Benchmark & comparison framework** (COMPLETE)

Phase 2 GATE PASSED 2026-08-01 (EM-run, real Drive, kopia binary built from
62f35a2d): `kopia repository validate-provider` → "All good." in 56s incl.
30s concurrency test (the #3788 + #4272 scenarios); e2e cycle in folder
kopia-e2e-gate (1vQOJWfgRGK_uG5V3NVfBkv9eyzBsKWa8): create → initial
snapshot 335MB/1200 files 71s → mutate → incremental 14s → verify 100%
(368MB downloaded) 117s → restore 1.2s (cache-warm) byte-identical →
full maintenance 13.5s. WP-2e merged 62f35a2d with live datapoints:
batch delete LOSES ~2.5x to parallel singles (Drive runs batch parts
serially; parallel singles 19.5 blobs/s vs batch 8.6 at default pacer) —
default stays parallel; absent-blob lookup 5.35s→0.48s; server-side name
narrowing for prefixes ≥4 chars (epoch/log/format listings no longer page
whole repo); BONUS FIX: Drive name= is case-insensitive → exact-name
client filter (wrong-blob-bytes hazard).

**Phase 2 — Backend rewrite** (COMPLETE)

Phase 1 GATE PASSED 2026-08-01: repo/blob/gdrive/gdriveerr merged @ 861eabe1.
368 tests green (also -race), golangci-lint 2.6.1 clean, builds under both
tag sets. Superset of RECON C.5 incl. live-observed accessNotConfigured +
oauth2.RetrieveError(invalid_grant). Key reviewed deviations accepted:
unknown errors default RetryableResume; hierarchy-depth = NonRetryableOther
not FatalQuota; Fields() = zap-style k/v pairs. Design note: backend must
NOT rely on repo/blob/retrying (can't see ErrStorageQuotaExhausted, can't
honor Retry-After) — pacer is the retry layer (PHASE2_DESIGN.md Contract 1).

Phase 0 GATE PASSED 2026-08-01: RECON.md (repo root, 1805 lines) assembled
from R1+R2+R3; all mandated items answered with file:line refs and doc URLs.

Decomposition decision: Phase 0 split into three parallel recon packages (R1
Kopia internals, R2 rclone catalog, R3 live Drive API doc verification) rather
than a single agent, to keep each within context budget. EM assembles and
reviews RECON.md against the gate checklist.

## Phase Gates

| Phase | Gate | Status |
|-------|------|--------|
| 0 Recon | RECON.md complete w/ file:line refs + doc URLs | PASSED 2026-08-01 |
| 1 Error taxonomy | taxonomy merged, unit tests green, covers all reasons from RECON | PASSED 2026-08-01 (commit 861eabe1) |
| 2 Backend rewrite | provider suite green (mock) + validate-provider clean vs real Drive + e2e snapshot cycle | PASSED 2026-08-01 (62f35a2d) |
| 3 Benchmark framework | one interleaved native-vs-rclone report promoted to curated results | PASSED 2026-08-02 (339ffd7d) |
| 4 Tuning loop | TUNING.md + one documented cycle in TUNING_LOG.md | PASSED 2026-08-02 (9caf1c7c) |
| 5 Upstream prep | PR branch, docs, changelog ready | PASSED 2026-08-02 (branch gdrive-native-upstream @ c69b4072) |

## Work Packages

### Phase 5 (dispatched 2026-08-02)
- **WP-5** — upstream prep: user docs rewrite (three auth modes, limits,
  tuning), de-"[Not maintained]", PR_DESCRIPTION.md w/ verified issue
  states + before/after evidence, changelog-mechanism investigation,
  CLA/DCO audit, upstream commit/rebase plan (fork-only files listed,
  plan written not executed). Status: DONE 2026-08-02, merged a733e16e.
  Audit findings: NO manual changelog (git-chglog generates from commit
  subjects — PR title with `providers` scope IS the changelog entry); NO
  CLA/DCO; upstream guidelines recommend a `design` issue for large work
  and REQUIRE squash-to-one-commit after review. EM decisions on flagged
  items: (1) folder auto-create under drive.file (PHASE2_DESIGN Contract
  4 clause) NOT implemented by 2c — ACCEPTED as documented follow-up
  (Google Picker trigger_onepick, RECON §C.6); docs honestly describe
  actual behavior + --scope=drive workaround. (2) StatsFromStorage/
  TransportStatsSnapshot exported without in-tree consumer once harness
  is filtered from upstream PR — KEEP exported, flagged for reviewers in
  PR_DESCRIPTION. (3) blanket "experimental" wording removed in favor of
  specific validation statements — ACCEPTED.
- **WP-5b** — tuning knobs as CLI flags: DONE, merged eb8e93f7. All ten
  knobs at create/connect; unset leaves tuning empty; tri-state
  --use-batch-delete via kopia's BoolListVar idiom; 4 tests -race green.

### Post-completion additions (user-requested, 2026-08-02)
- **WP-6** — CPU metrics + graphs + refreshed comparison: DONE, merged
  b4038c47. CPU attribution = /proc cumulative-counter boundary reads of
  live descendant tree, reconciled vs rusage per-process (residual
  3.7-5.9% published; tripwire never fired; agent caught+fixed a real
  double-count via negative residuals). Refreshed 5-trial comparison
  (native-vs-rclone-cpu, supersedes initial): every CPU row REAL,
  native 3.2x cheaper CPU e2e (9.4s vs 29.8s), wall unchanged 2.3x;
  delete-sweep wall degraded to WEAK this run (one 4.3s native trial;
  p50 still REAL +16.6%, CPU REAL +69%); :warm ≡ :hot confirmed,
  erratum closed. Note: most of rclone-lane extra CPU is kopia's WebDAV
  client, not rclone itself. 5 SVG graphs published (split linear
  panels, dataviz-validated palette). Baseline check informational:
  exit 0, no REAL regression; baseline deliberately untouched.
- Upstream branch REFRESHED: gdrive-native-upstream @ 03290d1e (amended
  single commit, now includes WP-7 folder creation; byte-identical to
  work branch for upstream paths; builds green).
- **WP-7** — native folder creation: DONE, merged 4b2ad623.
  --create-folder-name at repository create; idempotent reuse;
  ambiguous-duplicates error; resolved ID persisted into config/tokens
  + printed with connect hint. Live + CLI e2e verified w/ cleanup.
  LIVE FINDING: under drive.file even files.get("root") 404s (alias
  writable, not readable). Un-defers the non-Picker half of the WP-5
  EM decision (1); Picker flow remains the follow-up for hand-created
  folders.
- LOCAL_TESTING.md rewritten rclone-free (per-machine repo layout
  recommended for client machines; cross-user drive.file sharing
  explicitly steered away from pending live testing).

## PROJECT COMPLETE 2026-08-02

All six phase gates passed. Upstream branch: gdrive-native-upstream,
single commit c69b4072 off master 133fc64c (per upstream's
squash-to-one guideline), byte-identical to gdrive-native-v2 for the
five upstream paths, zero fork-only artifacts, builds+tests green.
Working branch gdrive-native-v2: 18 commits ahead of master, retains
project docs + benchmark harness + curated results.

Definition-of-Done accounting:
1. Provider suite + validate-provider vs real Drive: MET (Phase 2 gate).
2. Distinct actionable errors per reason, quota fail-fast: MET (Phase 1).
3. Interleaved significance-flagged comparisons, frozen schema,
   unattended regression detection: MET (Phase 3).
4. One tuning cycle logged + gdrive-tuned.json: MET (Phase 4; cycle 1 =
   honest REJECT with mechanism).
5. Beat-or-match tuned rclone on list-heavy/delete-sweep, within noise
   on pack-write: EXCEEDED vs DEFAULT rclone (native wins every REAL
   row incl. pack-write +59%). CAVEAT: rclone lane was default-tuned;
   delete-sweep's +25.5% is the only margin an rclone-side tuning pass
   could plausibly close. List-heavy margins (3-36x) structural.
6. PR branch, docs, changelog ready: MET (changelog = PR title per
   git-chglog; design-issue recommendation noted in PR_DESCRIPTION).

Remaining user-action items (optional): (a) rclone shared-client quota
experiment needs interactive browser consent; (b) open upstream design
issue / post plan on #2656 #3788 #4272 before submitting PR; (c) push
branches + open PR (user's GitHub account).

### Phase 4 (dispatched 2026-08-02)
- **WP-4** — TUNING.md + gdrive-tuned.json + cycle 1 (pacer MinSleep):
  DONE, merged 9caf1c7c. Cycle 1 REJECT (zero REAL rows, pre-registered
  mechanism). TUNING_LOG.md live in $KOPIA_GDRIVE_BENCH_RESULTS.
- **WP-4b** — cycle-1 change requests: DONE, merged 0dad0b7d.
  compare reference = config order + --reference override;
  RecordPacedWait → paced_wait_ms per workload (pacer hypotheses now
  falsifiable by measurement); --delete-sweep-n +
  tuning.deleteParallelism honored w/ effective value in result params.
  220 tests green -race; SCHEMA.md +77/-0 additive.

### Phase 3 (dispatched 2026-08-01)
- **WP-3a** — tools/gdrive-bench harness: 6 workloads, ABBA interleaving,
  SCHEMA.md v1 (frozen), compare w/ REAL/WEAK/NOISE, baseline check,
  knobs reflection, publish, quota ledger. Status: DONE 2026-08-02,
  EM-verified, merged 36fb2b7d. Live smoke vs real Drive green (62s,
  3825 QU, cleanup verified). Accepted deviations: tool build-tagged
  !no_extra_providers w/ stub main; multi-phase result keys
  (read-metadata:hot/:cold, list-heavy:full/:prefixed); cold pass keeps
  on-disk journal (true-cold → WP-3b); knob defaults hand-maintained
  with staleness-failing test.
- **WP-3b** — snapshot-e2e workload (real kopia binary, 4 timed stages),
  true-cold cache purge, and THE gate artifact: first full interleaved
  native-vs-rclone comparison on real Drive, published to
  tools/gdrive-bench/results/. Status: DISPATCHED (long-running: the
  comparison itself is 1.5-3h of live benchmarking)

### Phase 2 wave 1 (dispatched 2026-08-01)
- **WP-PACER** — repo/blob/gdrive/gdrivepacer/: adaptive rate limiter per
  Contract 1. Status: DONE 2026-08-01, EM-verified (race tests, vet, fmt,
  go.mod untouched); ACCEPTED pending wave-1 integration build. Accepted
  deviations: Retry-After honored uncapped; RetryableResume delays are
  call-local (no global barrier); ladder shape = package constants (only
  the 4 Contract-5 knobs exposed).
- **WP-INSTR** — gdrive_transport.go: counting RoundTripper + Stats per
  Contract 2. Status: DONE 2026-08-01, EM-verified (race tests pass in live
  tree, fmt clean, go.mod untouched); ACCEPTED pending wave-1 integration
  build. Accepted deviations: per-bucket latency []int64 + shared bucket
  array (diffable JSON); MethodStats adds Backoffs/BackoffMS/LatencySumMS;
  transport-level errors recorded as fixed "transport_error" (url.Error
  text can embed query-borne tokens). NOTE: wiring onto the Drive service
  deliberately deferred to WP-2c. WP-INSTR pre-reported 11 lint issues in
  WP-2a's in-progress files — check resolved at WP-2a review.
- **WP-2a** — gdrive_auth.go + options/CLI additions: three auth modes per
  Contract 4. Status: DONE 2026-08-01, EM-verified (live token test PASS
  against real Drive, lint exit 0, additive-only diffs). Accepted
  deviations: tuning tagged omitzero (omitempty inert on structs — literal
  Contract-5 tag would emit "tuning":{} in every config); hand-built
  authorized_user token source + own client-JSON parser (deprecated/
  insufficient library funcs, go.mod frozen); impersonation errors loudly
  when combined with non-SA credential modes. Impersonation path NOT
  live-validated (no DWD configured); interactive flows covered by
  fake-Google tests only (no KOPIA_GDRIVE_TEST_CLIENT_CREDENTIALS).

WAVE 1 INTEGRATED 2026-08-01: commits d416be9a (pacer), 07d714af
(transport), 2c282639 (auth). Full-tree builds both tag sets, all package
tests -race green, live auth test green, golangci-lint 2.6.1 exit 0.

### Phase 2 wave 2 (dispatched 2026-08-01)
- **WP-2b** — gdrive_fileids.go: persistent blobID→fileID cache per
  Contract 3. Status: DONE 2026-08-01, EM-verified, merged 4c72c7bc.
  CRC32C append-only journal + flock + O_APPEND + SameFile reopen guard;
  refcounted shared-instance registry (#4272 fix); multi-process test
  re-execs test binary (guards proven load-bearing). Accepted deviations:
  gofrs/flock (existing kopia dep), write-failure disables persistence
  for process, tombstones persist past entry cap, interface declaration
  deferred to WP-2c.
- **WP-2c** — gdrive_storage.go full rewrite (incl. folded-in WP-2d
  conditional create), legacy cache deletion, fake-Drive test server.
  Status: DONE 2026-08-01, EM-verified, merged 6da6bf7b. Mock suite
  (VerifyStorage + ValidateProvider + VerifyConcurrentAccess) green -race;
  LIVE ValidateProvider PASS vs real Drive (49s, zero retries needed).
  All §A.2.5 legacy defects addressed (checklist in agent report).
  Live finding: files.list on nonexistent parent returns EMPTY not error
  → liveness probe now files.get on folder. Accepted deviations incl.
  cached-ID fast-path delete (single file), orderBy name,createdTime desc
  dedupe, test seam sync.Map (flag for Phase 5 upstream review).
  FLAGGED → WP-2e: 5s not-found escalation budget costs ~10s per
  genuinely-absent blob lookup while live evidence shows escalation never
  fired; reduce budget + add cache journal tail re-read on miss.
- **WP-2e** — bulk deletes + listing optimization + not-found budget.
  Status: DISPATCHED

### Phase 1 (dispatched 2026-08-01)
- **WP1 — gdriveerr taxonomy**: DONE, merged 861eabe1 (see gate note).

### Phase 0 (dispatched 2026-08-01)
- **R1 — Kopia internals recon**: blob.Storage semantics, PutOptions, sentinel
  errors, current gdrive read (incl. existing file_id_cache.go), gcs/s3/retrying/
  throttling/sharded idioms, provider test harness, open issues #2344 #2656
  #3788 #4272. Status: DONE 2026-08-01 → scratchpad/recon-r1-kopia.md.
  Headlines: DoNotRecreate sentinel = blob.ErrBlobAlreadyExists, but the
  sanctioned no-CAS alternative (used by s3/azure/b2/fs/sftp/webdav) is
  ErrUnsupportedPutBlobOption — validate-provider accepts both; existing
  file_id_cache.go NOT salvageable (in-memory, per-instance, holds per-blob
  mutex across uploads); #4272 root cause = no cross-connection
  read-your-writes + 300ms list retry; #2656 = no DWD Subject support + 403
  storageQuotaExceeded not classified permanent (10-retry storm);
  NEW UNFILED BUG: ListBlobs page consumer `return nil` on first prefix
  non-match drops rest of page (gdrive_storage.go:269-271) and prefixed
  branch drops `trashed=false`; ConnectionInfo config-compat contract must
  keep throttling.Limits inline; internal/ownwrites +
  internal/cache.NewStorageOrNil are the reusable persistence pieces;
  retrying does NOT wrap ListBlobs/GetCapacity; providervalidation
  listBlobWorker is a TODO stub.
- **R2 — rclone Drive catalog**: pacer impl (lib/pacer), error-reason→retry map,
  batching of deletes/moves, chunked resumable uploads, from ~/sandbox/rclone
  (read-only local clone). Status: DONE 2026-08-01 →
  scratchpad/recon-r2-rclone.md. Headlines: pacer = 100ms min sleep / burst
  100 / 16s-capped jittered backoff / instant recovery; NO batching anywhere
  in rclone's Drive backend (wins come from call-count reduction: 50-parent
  OR listing queries, Fields("") on deletes, 1-call moves); upload cutoff +
  chunk 8 MiB; retry map gaps we can beat (dailyLimitExceeded, domainPolicy,
  429+Retry-After unhandled; quota = fatal abort).
- **R3 — Drive API v3 live doc verification**: batch endpoint status/limits,
  quotas, 750 GB/day cap, Shared Drive item limits, resumable session lifetime,
  full googleapi reason-string set, drive.file scope visibility semantics
  (multi-machine shared-repo question from Phase 2a). Status: DONE 2026-08-01
  → scratchpad/recon-r3-driveapi.md. Headlines: batching ALIVE
  (batch/drive/v3, max 100, zero quota savings); quota model changed
  2026-05-01 to quota units (1M QU/min/project, 325k QU/min/user, 1 TB/day
  project egress cap; read 5 / list 100 / download 200 / edit 50 QU);
  per-project buckets key on OAuth client's Cloud project (dedicated client
  ID = isolation; rclone retiring shared client in 2026); 750 GB/day cap
  surfaces as plain userRateLimitExceeded (no dedicated reason); 500k
  children/folder + 500k items/Shared Drive, trashed items count (deletes
  must be permanent); resumable sessions 1 week; drive.file cross-user
  visibility = open experiment (docs converge on NOT visible; OAuth Picker
  trigger_onepick is Google's documented desktop path).

## Decisions Log

- 2026-08-01: WP-2d (conditional create) FOLDED INTO WP-2c — the
  DoNotRecreate protocol lives inside PutBlob; a separate package would
  force a second rewrite of the same function. 2d's acceptance criteria
  (emulation semantics + #3788/#4272 validation) reviewed as part of 2c +
  the phase-gate live validate-provider run.
- 2026-08-01: Phase 0 split 1→3 parallel recon agents (EM assembles RECON.md).
- 2026-08-01: Noted current backend already has a fileID cache
  (repo/blob/gdrive/file_id_cache.go) — Phase 2b must evaluate/replace, not
  assume greenfield.

## Credentials / environment (updated 2026-08-01)

- User provided `~/secrets/gdrive.env` (source it inline per-command; shell
  state does not persist). Defines: KOPIA_GDRIVE_TEST_CREDENTIALS (file),
  KOPIA_GDRIVE_TEST_FOLDER, KOPIA_GDRIVE_TEST_FOLDER_RCLONE (separate folder
  for rclone-side benchmarks), KOPIA_GDRIVE_BENCH_RESULTS (existing dir).
- VERIFIED 2026-08-01 (live probe, scratchpad/credprobe): credentials file is
  rejected as-is by google.CredentialsFromJSON ("missing 'type' field");
  ACCEPTED after injecting `"type":"authorized_user"`. Phase 2a loader must
  tolerate the missing type key. Token itself is valid (auth succeeded).
- RESOLVED 2026-08-01: user enabled the Drive API in project 949080498481;
  live probe now succeeds. (Live-observed 403 accessNotConfigured → forwarded
  to WP1 for taxonomy coverage.)
- LIVE CONFIRMATION of drive.file semantics: the token is drive.file-scoped;
  the user's hand-created folder 404s on files.get (invisible to the app) —
  exactly RECON §C.6. Fix: created app-owned folders via the token:
  kopia-gdrive-native-test = 1JnHvA7iweRGRpnKRjb90jKRA-VyVaJfv (now
  KOPIA_GDRIVE_TEST_FOLDER; original value preserved as comment in
  ~/secrets/gdrive.env, backup at gdrive.env.bak) and kopia-gdrive-bench =
  1IOdQS3NMaANeq7KbA0cAdSinFxfpi4si (new KOPIA_GDRIVE_BENCH_FOLDER_NATIVE).
  KOPIA_GDRIVE_TEST_FOLDER_RCLONE left untouched (rclone brings its own auth).
- Note for Phase 2 tests: under drive.file, per-test uuid subfolders must be
  created BY the app (they will be, since the backend creates them) — the
  existing gdrive_storage_test.go pattern already does this via
  CreateDriveService.
- `KOPIA_GDRIVE_TEST_CLIENT_CREDENTIALS` (OAuth client JSON) NOT provided —
  fine for tests (authorized_user embeds client id/secret) but the
  interactive loopback-flow test in Phase 2a will need it or must be
  design-reviewed only.

## Open Questions

- drive.file scope: exact visibility of app-created files across multiple
  machines authorizing the same OAuth client ID in one Workspace domain (R3 to
  answer from docs; must also be live-tested in Phase 2).
- ANSWERED (R1): DoNotRecreate violation sentinel is blob.ErrBlobAlreadyExists;
  validate-provider equally accepts ErrUnsupportedPutBlobOption (the route
  taken by every non-CAS backend except gcs). DESIGN TENSION for Phase 2d:
  mission spec says emulate DoNotRecreate via query→create→dup-check; R1
  evidence says the racy emulation IS issue #3788 and upstream's sanctioned
  answer for no-CAS stores is ErrUnsupportedPutBlobOption. EM decision due at
  Phase 2d dispatch: implement the emulation carefully (spec) but be prepared
  to fall back to ErrUnsupportedPutBlobOption if the emulation cannot pass
  validate-provider's concurrency checks against real Drive.
- Whether homogeneous batch is still supported for Drive v3 deletes (R3) —
  determines Phase 2e design.

## Benchmark Baselines

Informal Phase-2 gate numbers (real Drive, drive.file, dedicated OAuth
client, 2026-08-01, single trial — NOT statistical baselines):
- validate-provider 56s incl. 30s concurrency soak
- initial snapshot 335MB/1200 files: 71s; incremental (+50 files): 14s
- verify --verify-files-percent=100 (368MB down): 117s
- full maintenance (small repo): 13.5s
- deletes: parallel singles 19.5-23 blobs/s; batch 7.6-8.6 blobs/s
Formal interleaved baselines land in $KOPIA_GDRIVE_BENCH_RESULTS (Phase 3).

## Phase 3 environment notes

- rclone lane READY (EM-prepared 2026-08-01): rclone v1.75.0 at
  scratchpad/tools/rclone; remote `gdrive-bench` in ~/.config/rclone/
  rclone.conf (0600, synthesized from the authorized_user token — same
  OAuth client + user as native = controlled comparison); app-created
  folder kopia-gdrive-bench-rclone = 1Q-VRL4RZAikrHI8uyHIA4lv6QiTbczfT
  (also in gdrive.env as KOPIA_GDRIVE_BENCH_FOLDER_RCLONE; pass to rclone
  via --drive-root-folder-id or root_folder_id). RW verified (rcat/ls/
  delete). Legacy KOPIA_GDRIVE_TEST_FOLDER_RCLONE (hand-created, invisible
  to drive.file) left in env but unused.
- kopia rclone backend invocation: kopia repository create rclone
  --remote-path gdrive-bench:<sub> --rclone-exe scratchpad/tools/rclone
  (kopia runs `rclone serve webdav` internally).
- Phase 4 note: the "our client vs rclone shared client" quota experiment
  requires an interactive rclone OAuth consent (browser) for rclone's
  baked-in client — needs user action when we get there, or drop that
  comparison and sweep client-independent knobs only.
