# PROJECT_STATE — Kopia Native Google Drive Backend v2

**Branch:** `gdrive-native-v2` (fork of kopia/kopia master @ 133fc64c)
**Orchestration:** Fable 5 as EM; Opus 5 subagents implement work packages.

## Current Phase

**Phase 1 — Error taxonomy** (started 2026-08-01)

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
| 1 Error taxonomy | taxonomy merged, unit tests green, covers all reasons from RECON | IN PROGRESS |
| 2 Backend rewrite | provider suite green (mock) + validate-provider clean vs real Drive + e2e snapshot cycle | not started |
| 3 Benchmark framework | one interleaved native-vs-rclone report promoted to curated results | not started |
| 4 Tuning loop | TUNING.md + one documented cycle in TUNING_LOG.md | not started |
| 5 Upstream prep | PR branch, docs, changelog ready | not started |

## Work Packages

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

None yet (Phase 3).
