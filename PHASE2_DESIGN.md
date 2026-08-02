# Phase 2 Design — shared contracts for work packages 2a–2e

EM-authored interface contracts. Subagents implement AGAINST these; changes to
this file require EM sign-off (serialize anything touching shared interfaces).
Read RECON.md first: §A for Kopia contracts, §B for rclone prior art, §C for
current Drive API facts. Error handling ALWAYS goes through
`repo/blob/gdrive/gdriveerr` (Phase 1 deliverable).

## Package layout (new code under repo/blob/gdrive/)

- `gdriveerr/`      — Phase 1 taxonomy (done before Phase 2 starts)
- `gdrivepacer/`    — WP-PACER: adaptive rate limiter (rclone-style)
- `gdrive_transport.go` — WP-INSTR: counting RoundTripper + stats snapshot
- `gdrive_auth.go`  — WP-2a: credential loading, three auth modes
- `gdrive_fileids.go` — WP-2b: persistent blobID→fileID cache
- `gdrive_storage.go` — WP-2c/2d/2e: rewritten Storage impl (data path,
  conditional create, listing/bulk ops)
- `gdrive_options.go` — extended Options (additive, omitempty only;
  preserve existing JSON keys: folderID, credentialsFile, credentials,
  readOnly + inline throttling.Limits — see RECON §A.2.8)

All files carry `//go:build !no_extra_providers`.

## Contract 1 — Pacer (gdrivepacer)

```go
type Pacer interface {
    // Call runs fn under the global rate limiter. It classifies the returned
    // error via gdriveerr.Classify (op/blobID supplied by caller), applies
    // disposition: RetryableBackoff → backoff (honor RetryAfter) + retry;
    // RetryableResume → retry with mild backoff; Fatal* → return immediately.
    // Respects ctx cancellation at every wait point. maxTries bounds attempts.
    Call(ctx context.Context, op, blobID string, fn func() error) error
    // CallNoRetry: single attempt, still rate-limited and classified.
    CallNoRetry(ctx context.Context, op, blobID string, fn func() error) error
}
```

- Implementation: token-bucket steady state (min-sleep interval, burst) +
  jittered truncated exponential backoff on RetryableBackoff (attack/decay
  per rclone GoogleDrive calculator, RECON §B.1), instant re-acceleration on
  success. gdriveerr.RetryDelay supplies backoff quanta.
- Defaults (tunable): minSleep 100ms, burst 100, maxSleep 16s, maxTries 10.
- ALL Drive API calls in the backend flow through the pacer. Concurrency
  (parallel deletes etc.) sits ABOVE it.
- Config: every knob in `PacerConfig` struct, JSON-(de)serializable, wired
  from Options.Tuning (Contract 5) — Phase 4's agent sweeps these without
  recompiling.
- The backend does NOT wrap with repo/blob/retrying (RECON §A.3.2: it can't
  honor Retry-After and would retry FatalQuota). The pacer IS the retry
  layer. Sentinel unwrapping in gdriveerr keeps errors.Is contracts intact
  for upper layers.

## Contract 2 — Instrumented transport (gdrive_transport.go)

```go
type transportStats struct{ ... } // atomic counters
type Stats struct { // JSON-serializable snapshot
    PerMethod map[string]MethodStats // files.list/get/create/update/delete/batch/upload
    // MethodStats: Calls, Errors, Retries, BytesUp, BytesDown,
    // LatencyMS histogram buckets (fixed: 1,2,5,10,...,60000)
    ErrorsByReason map[string]int64
}
func (s *gdriveStorage) StatsSnapshot() Stats  // programmatic
```

- Implemented as an http.RoundTripper wrapping the oauth2 transport; method
  attribution from URL path+verb (path patterns for /drive/v3/files etc.,
  /upload/ prefix = upload). Retries counted by the pacer, errors-by-reason
  fed from gdriveerr.Classify results (pacer reports into stats — pass a
  *transportStats into the pacer constructor).
- Dumpable as JSON; the Phase 3 harness consumes StatsSnapshot between
  workload stages. Never log request bodies/tokens.

## Contract 3 — FileID cache (gdrive_fileids.go)

```go
type fileIDCache interface {
    Get(blobID blob.ID) (fileID string, ok bool)
    Put(blobID blob.ID, fileID string)
    Delete(blobID blob.ID)
    Flush() // FlushCaches support: drop everything
    Close() error
}
```

- Persistent, per-(local-user, folderID) scoped, stored under kopia's cache
  dir (resolve via internal/ospath default; overridable via
  Options.Tuning.CacheDir). Format: single append-friendly flat file or
  internal/cache.NewStorageOrNil-backed — implementer picks per RECON §A.5,
  NO new third-party dependency.
- Multi-process safe: corruption-proof (checksummed records; a torn write
  loses entries, never poisons); advisory flock or write-to-temp+rename.
- Correctness rule (non-negotiable): cache is optimization, never authority.
  Any cached-ID operation that 404s must invalidate the entry and re-resolve
  by name query before returning ErrBlobNotFound. Duplicate names resolve
  newest-createdTime-wins + log warning with both IDs.
- NOT a consistency mechanism across machines; within-machine cross-process
  read-your-writes is the target (fixes #4272's 5-connection scenario since
  validate-provider connections share one process/machine).
- Additionally: PutBlob records fileID immediately (write-through), so the
  writer's process never needs a list query for its own writes.

## Contract 4 — Auth (gdrive_auth.go, WP-2a)

- Mode 1 (default): OAuth user. Accepts (a) Google authorized_user JSON,
  (b) same but missing "type" (inject authorized_user — VERIFIED needed for
  our test token, PROJECT_STATE), (c) OAuth client JSON ("installed" format)
  → run loopback flow (127.0.0.1 ephemeral port, one-shot listener), store
  resulting authorized_user JSON per kopia credential conventions. Device
  flow behind a flag for headless (drive.file only, RECON §C.6).
- Mode 2: service account + impersonation (Subject) — code path complete,
  marked experimental, NOT integration-tested (no delegation configured).
- Mode 3: bare service account — permitted; startup warning about its own
  ~15 GB quota (issue #2656).
- Scope: drive.file default; --scope=drive opt-out flag for pre-existing
  external folders. Under drive.file the backend creates its own repo folder
  when FolderID is absent/invisible (live-confirmed: hand-created folders
  404 under drive.file).
- Existing Options fields keep their meaning (ServiceAccountCredentialsFile /
  ServiceAccountCredentialJSON accept BOTH service-account and user JSON —
  sniff by "type" key; new optional fields: ClientCredentialsFile,
  TokenCacheFile?, Impersonate, Scope — all omitempty, names finalized in
  WP-2a review).

## Contract 5 — Options.Tuning

```go
type TuningOptions struct { // json:"tuning,omitempty"
    PacerMinSleepMS, PacerBurst, PacerMaxSleepMS, MaxTries int
    UploadChunkSizeMB int   // resumable chunk; default 64
    SimpleUploadCutoffMB int // below → simple upload; default 8
    ListPageSize int        // default 1000 (API max)
    DeleteParallelism int   // bounded-concurrency deletes; default 8
    UseBatchDelete *bool    // nil=auto (Phase 3 decides default)
    CacheDir string
}
```
- Zero values = defaults (constants in one place, `gdrive_defaults.go`).
- Readable from JSON config file AND flags; `gdrive-bench knobs` (Phase 3)
  reflects over this struct — keep it flat and typed.

## Data-path decisions (WP-2c/2e, from RECON)

- Deletes: PERMANENT (files.delete, never trash) — trashed items count
  against the 500k caps (RECON §C.4). Default path = bounded-parallel
  singles through pacer; batch/drive/v3 variant implemented behind
  UseBatchDelete for Phase 3 A/B (batch saves RTT, zero quota, RECON §C.1).
- Listing: pageSize 1000, fields mask exactly kopia's Metadata needs
  (files(name,id,size,modifiedTime),nextPageToken), `q` = parent + mimeType
  + trashed=false (+ name-range narrowing where the prefix allows; `name
  contains` is substring — client-side prefix filter stays, with `continue`
  not `return`, per the RECON §A.2.5 bug).
- GetBlob: files.get?alt=media + Range header; 416→ErrInvalidRange;
  length==0 validated via metadata, not a 1-byte read.
- PutBlob: resumable above cutoff (256 KiB-multiple chunks, single-request
  resumable preferred per RECON §C.3), simple below; upload URL sessions
  respect ctx cancellation (abort session cleanly).
- DoNotRecreate (WP-2d): query(cache+name) → absent ⇒ create → post-create
  dup check (list by name, orderBy createdTime); if duplicate, delete OWN
  newer file, return ErrBlobAlreadyExists. Residual race documented in code.
  Fallback decision recorded in PROJECT_STATE if live validate-provider
  can't be made to pass: return ErrUnsupportedPutBlobOption (sanctioned,
  RECON §A.1.3).
- GetMetadata/read-after-write: on miss for a blob this machine recently
  wrote (fileID cache hit), files.get by ID is authoritative — never a name
  query. On genuine name-query miss, retry with escalating delay up to
  ~5s total (configurable) before ErrBlobNotFound (#4272 defense-in-depth
  beyond the shared cache).

## Integration order

PACER + INSTR + 2a first (independent, parallel) → review → 2b (needs pacer)
→ 2c/2e (need all prior; parallel with each other, different files possible
but both touch gdrive_storage.go ⇒ SERIALIZE: 2c then 2e) → 2d last (small,
touches PutBlob path from 2c). Mock-based provider suite green required at
each merge; live validate-provider at the end of the phase.
