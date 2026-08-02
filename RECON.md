# RECON — Kopia Native Google Drive Backend v2

Phase 0 reconnaissance for the gdrive-native-v2 rewrite. Assembled 2026-08-01
from three parallel recon packages: R1 (Kopia internals, §A), R2 (rclone Drive
catalog, §B), R3 (live Drive API v3 doc verification, §C). All file:line
references verified against the working tree (kopia @ 133fc64c, rclone local
clone @ 1d03a771) at recon time.

## Executive summary — what changes the design

1. **The performance problem is queries, not bytes.** Every cache-cold blob
   operation costs a `files.list` name lookup; short Kopia prefixes degenerate
   to whole-folder substring scans. Fix = persistent blobID→fileID index +
   ID-based `files.get`/`update`/`delete` + trimmed field masks (§A.2).
2. **Consistency, not just speed, is broken.** #3788 and #4272 are both
   read-your-writes failures across connections; the in-memory per-instance
   cache cannot fix them. The existing `file_id_cache.go` is not salvageable
   (§A.2.3). Kopia already ships the right primitives to reuse:
   `internal/cache.NewStorageOrNil` + the `internal/ownwrites` pattern (§A.5).
3. **DoNotRecreate:** sentinel is `blob.ErrBlobAlreadyExists`; the validator
   equally blesses `errors.Wrap(blob.ErrUnsupportedPutBlobOption,
   "do-not-recreate")`, which is what every non-CAS backend (s3, azure, b2,
   filesystem, sftp, webdav) returns. Drive has no server-side precondition.
   Phase 2d will attempt the careful emulation per the mission spec, with the
   sanctioned `ErrUnsupportedPutBlobOption` fallback if the emulation cannot
   pass validate-provider against real Drive (§A.1.3).
4. **Live unfiled bug found:** current `ListBlobs` page consumer does
   `return nil` on the first prefix non-match, silently dropping the rest of
   every page (gdrive_storage.go:269-271), and the prefixed branch drops
   `trashed = false`. Potential missed-blob/data-loss path during maintenance;
   the rewrite supersedes it, but worth an upstream issue reference in the PR.
5. **rclone's Drive speed comes from call-count reduction + one global pacer,
   not batching.** rclone does not batch Drive calls at all. Its pacer: 100 ms
   min sleep (10 req/s), burst 100, jittered exponential backoff capped 16 s,
   instant re-acceleration on success. Its ListR trick (50 parent IDs per `q`
   query) is the single biggest documented win (22 min → 58 s on 39k files).
   Its retry map has gaps we must beat: `dailyLimitExceeded`, `domainPolicy`,
   `sharingRateLimitExceeded`, bare 429 + `Retry-After` are unhandled (§B).
6. **Config compatibility is a hard constraint:** the `gdrive` ConnectionInfo
   JSON shape (`folderID`, `credentialsFile`, `credentials`, `readOnly` +
   inline `throttling.Limits`) must be preserved; new fields additive with
   `omitempty`. Keep `throttling.Limits` embedded — `repo/open.go` discovers
   limits by re-marshaling the config (§A.2.8, §A.3.3).
7. **Retry integration:** `retrying.isRetriable` hard-fails only on the blob
   sentinels; everything else gets 10 attempts. So the taxonomy must map
   fatal Drive errors (quota, auth, domain policy) onto non-retriable
   sentinels or wrap them so `errors.Is` matches. Note `ListBlobs` and
   `GetCapacity` are NOT wrapped by retrying — the backend owns their retry
   behavior (§A.3.2).
8. **Issue map:** #2344 = query-bound slowness (§A.2.9); #2656 = service
   account owns the files + no DWD Subject support + quota errors retried
   10× (§A.2.9); #3788 = racy read-then-write DoNotRecreate returning nil
   (§A.2.9); #4272 = cross-connection GetMetadata after write with only
   300 ms of list-lag retry (§A.2.9, §A.4.3).

9. **Drive quota model changed 2026-05-01 (training data is stale):** quota
   units, not queries/100s — 1M QU/min/project, 325k QU/min/user/project,
   1 TB/day per-project egress cap, 400M QU/day billing threshold. Costs:
   read 5, list 100, download 200, edit 50 QU. Per-user quota keys on the
   authenticated principal; per-project buckets key on the Cloud project
   owning the OAuth client — a dedicated client ID buys project-bucket
   isolation (rclone is retiring its shared client during 2026 for exactly
   this) (§C.2).
10. **Batching is alive but saves zero quota** (batch of n counts as n).
    `batch/drive/v3`, max 100 calls. Choose batch vs bounded-parallel singles
    on RTT grounds only (§C.1).
11. **Error-reason traps for the taxonomy:** key on (HTTP code, reason) —
    `rateLimitExceeded` appears under both 403 and 429; it's `notFound` not
    `fileNotFound`; 404 conflates absence with permission-denial under
    drive.file; the 750 GB/day upload cap surfaces as plain
    `userRateLimitExceeded` (no dedicated reason — needs heuristic
    escalation so we don't backoff-retry for 24 h); some reasons are
    snake_case (`download_restricted_for_revision`) or oddly cased
    (`UrlLeaseLimitExceeded`) (§C.5).
12. **Deletes must be permanent, and sharding is mandatory at scale:**
    trashed items count against the 500k-children-per-folder limit and the
    500k-items-per-Shared-Drive cap (the latter caps repo size regardless of
    folder layout). Resumable sessions live 1 week; 256 KiB chunk multiples;
    single-request resumable preferred over chunking per Google (§C.3, §C.4).
13. **drive.file cross-user visibility is the open experiment:** grants are
    (user × app × file); docs converge on NOT visible across users of the
    same client ID, unconfirmed by any single doc sentence. Google's
    documented desktop path is the OAuth Picker (`trigger_onepick=true`)
    returning `picked_file_ids` alongside the auth code. Whether a folder
    grant recurses to files created by a different user decides drive.file
    vs full-drive for shared repos — must be tested live in Phase 2 (§C.6).

Sections: §A = R1 Kopia internals · §B = R2 rclone catalog · §C = R3 Drive
API v3 live-doc verification.

---

# Section A — Kopia internals (R1)


Repo: `/home/jpeverill/sandbox/kopia` @ branch `gdrive-native-v2` (base commit `133fc64c`).
All paths below are repo-relative; line numbers verified against working tree at time of recon.

---

## 1. `blob.Storage` interface & semantics

### 1.1 Interface decomposition

`repo/blob/storage.go` splits the contract into small interfaces:

| Interface | Methods | Location |
|---|---|---|
| `Volume` | `GetCapacity(ctx) (Capacity, error)` | `repo/blob/storage.go:78-81` |
| `Lister` | `ListBlobs(ctx, blobIDPrefix ID, cb func(Metadata) error) error` | `repo/blob/storage.go:84-88` |
| `Reader` | embeds `Lister`; `GetBlob`, `GetMetadata`, `ConnectionInfo`, `DisplayName` | `repo/blob/storage.go:91-109` |
| `Storage` | embeds `Volume` + `Reader`; `PutBlob`, `DeleteBlob`, `Close`, `FlushCaches`, `ExtendBlobRetention`, `IsReadOnly` | `repo/blob/storage.go:204-227` |

Provider requirements are documented in the `Storage` doc comment (`repo/blob/storage.go:193-203`): high durability, **read-after-write** (a blob written with `PutBlob` must be immediately readable by `GetBlob` *and* `ListBlobs`), atomicity (no partial results observable), monotonic timestamps (minutes of skew tolerated), low retrieval latency. This is exactly the contract the current gdrive backend violates (see §2.6).

### 1.2 Method semantics

**`GetBlob(ctx, blobID, offset, length int64, output OutputBuffer) error`** — `repo/blob/storage.go:94-98`
- `length > 0` ⇒ fetch byte range `[offset, offset+length)`.
- `length < 0` (canonical: `-1`) ⇒ fetch **entire** blob. With `offset == 0, length == -1` this is a full read.
- `length == 0` ⇒ legal; returns zero bytes but must still validate existence. `providervalidation` explicitly exercises `{offset:5, length:0}` (`internal/providervalidation/providervalidation.go:211`), and `blobtesting.AssertGetBlob` asserts `GetBlob(id,0,0,…)` succeeds with `b.Length()==0` (`internal/blobtesting/asserts.go:54-63`).
- Out-of-range: must return an error. Helper `blob.EnsureLengthExactly(gotLength int, length int64) error` (`repo/blob/storage.go:307-317`) returns `errors.Wrapf(ErrInvalidRange, …)` when the returned length ≠ requested length; `length < 0` disables the check. Backends call this as the last statement of `GetBlob` (gdrive: `repo/blob/gdrive/gdrive_storage.go:103`; gcs: `repo/blob/gcs/gcs_storage.go:76`; s3: `repo/blob/s3/s3_storage.go:85`).
- Negative offset ⇒ `blob.ErrInvalidRange` directly (gdrive `repo/blob/gdrive/gdrive_storage.go:79-81`, gcs `repo/blob/gcs/gcs_storage.go:46-48`).
- HTTP 416 must map to `ErrInvalidRange` (gdrive `…/gdrive_storage.go:471-472`, gcs `…/gcs_storage.go:110-111`, s3 `…/s3_storage.go:107-108`, azure `…/azure_storage.go:128-129`).
- The test suite asserts *any* error for `(-3,1)`, `(len,3)`, `(len-1,3)`, `(len+1,3)` — see `internal/blobtesting/asserts.go:85-88` + `AssertInvalidOffsetLength` at `:92-101`. Note it asserts `err != nil`, not specifically `ErrInvalidRange`.
- `output.Reset()` is **not** the callee's responsibility at the raw-provider layer; the retrying wrapper resets before each attempt (`repo/blob/retrying/retrying_storage.go:21`) and the throttling wrapper does too (`repo/blob/throttling/throttling_storage.go:60`).

**`GetMetadata(ctx, blobID) (Metadata, error)`** — `repo/blob/storage.go:100-101`. Returns `Metadata{BlobID, Length int64, Timestamp time.Time}` (`repo/blob/storage.go:232-237`). Must return `ErrBlobNotFound` for missing blobs (`internal/blobtesting/asserts.go:130-137`). `Length` and `Timestamp` must agree with what `ListBlobs` reports for the same blob within `maxTimeDiffBetweenGetAndList = time.Minute` (`internal/blobtesting/asserts.go:18`, checked at `:148-169`).

**`PutBlob(ctx, blobID, data Bytes, opts PutOptions) error`** — `repo/blob/storage.go:208-210`. Creates or replaces. `data` is a `blob.Bytes` (`repo/blob/storage.go:54-59`) exposing `Length() int`, `Reader() io.ReadSeekCloser` and `io.WriterTo` — the reader is seekable, so resumable/retryable uploads are feasible.

**`DeleteBlob(ctx, blobID) error`** — `repo/blob/storage.go:212-213`. **Deleting a nonexistent blob may return either `nil` or `ErrBlobNotFound`** — the suite tolerates both: `internal/blobtesting/verify.go:46-48` (`if err != nil && !errors.Is(err, blob.ErrBlobNotFound) { t.Errorf(…) }`). But `verify.go:135-136` requires **two successive deletes of the same blob to both return `nil`** (`require.NoError` twice), so returning `ErrBlobNotFound` on the 2nd delete would fail the suite. Practical rule: **return `nil` for missing blobs.** gcs does this (`repo/blob/gcs/gcs_storage.go:174-181`), azure does (`repo/blob/azure/azure_storage.go:~157-165`), gdrive does via `handleError` (`repo/blob/gdrive/gdrive_storage.go:224-238`). `blobtesting.VerifyConcurrentAccess` also accepts only `nil` or `ErrBlobNotFound` (`internal/blobtesting/concurrent.go:115-124`).

**`ListBlobs(ctx, prefix, cb)`** — `repo/blob/storage.go:84-88`.
- Prefix is a **literal string prefix** on the blob ID; `prefix == ""` means all blobs.
- **No ordering guarantee.** `AssertListResults` sorts both got and want before comparing (`internal/blobtesting/asserts.go:176-181`), and `AssertListResultsIDs` uses `ElementsMatch` (`:197`).
- Callback errors **must be propagated verbatim** and stop iteration — asserted with `require.ErrorIs` for both empty and non-empty prefixes (`internal/blobtesting/verify.go:79-92`). *This is a live bug in the current gdrive code — see §2.5.*
- Callback may be invoked concurrently only if the impl says so; the standard impls call it serially.
- Convenience helpers: `ListAllBlobs` (`repo/blob/storage.go:252-261`), `IterateAllPrefixesInParallel` (`:264-302`), `ReadBlobMap` (`:405-423`).

**`GetCapacity(ctx) (Capacity, error)`** — `repo/blob/storage.go:78-81`; `Capacity{SizeB, FreeB uint64}` at `:70-75`. Default impl returns `ErrNotAVolume` (`repo/blob/storage.go:183-185`). Callers switch on it: `cli/command_repository_status.go:73,165`, `internal/server/grpc_session.go:585`, `repo/grpc_repository_client.go:689`, `internal/providervalidation/providervalidation.go:132`. Validation additionally requires `FreeB <= SizeB` (`providervalidation.go:133-137`).

**`FlushCaches(ctx) error`** — `repo/blob/storage.go:218-219`; default no-op (`:177-180`). gdrive overrides it to wipe the fileID cache (`repo/blob/gdrive/gdrive_storage.go:339-342`).

**`ConnectionInfo() ConnectionInfo`** — `repo/blob/storage.go:103-105`. Must round-trip: `blob.NewStorage(ctx, st.ConnectionInfo(), false)` must produce a storage whose `ConnectionInfo()` is `require.Equal` to the original (`internal/blobtesting/AssertConnectionInfoRoundTrips`, `internal/blobtesting/verify.go:203-215`).

**`DisplayName() string`** — `repo/blob/storage.go:107-108`. Human-readable; gdrive returns `"Google Drive: <folderID>"` (`repo/blob/gdrive/gdrive_storage.go:335-337`).

**`Close(ctx) error`** — `repo/blob/storage.go:215-216`; default no-op (`:172-175`). gdrive does **not** override it, so it never closes the HTTP client.

**`ExtendBlobRetention` / `IsReadOnly`** — default impls return `ErrUnsupportedObjectLock` and `false` (`repo/blob/storage.go:162-170`).

`blob.DefaultProviderImplementation` (`repo/blob/storage.go:154-160`) is the struct to embed for the defaults.

### 1.3 `blob.PutOptions` — `repo/blob/storage.go:134-146`

```go
type PutOptions struct {
    RetentionMode   RetentionMode   // "GOVERNANCE" | "COMPLIANCE" | "Locked" (Azure)
    RetentionPeriod time.Duration
    DoNotRecreate   bool            // fail with ErrBlobAlreadyExists if blob exists
    SetModTime      time.Time       // if non-zero, set this mtime; ErrSetTimeUnsupported if impossible
    GetModTime      *time.Time      // if non-nil, populate with actual mtime
}
```

| Option | Expected sentinel on violation / non-support | Evidence |
|---|---|---|
| `DoNotRecreate` (blob exists) | **`blob.ErrBlobAlreadyExists`** | contract at `repo/blob/storage.go:139-140`; suite `internal/blobtesting/verify.go:112`; validation `internal/providervalidation/providervalidation.go:182` |
| `DoNotRecreate` (unsupported by backend) | `blob.ErrUnsupportedPutBlobOption` wrapped with `"do-not-recreate"` | `internal/providervalidation/providervalidation.go:180`; impls: s3 `:136-137`, azure `:137-138`, b2 `:157-158`, filesystem `:165-166`, sftp `:197`, webdav `:147`, mapStorage `internal/blobtesting/map.go:120-121` |
| `SetModTime` unsupported | `blob.ErrSetTimeUnsupported` | `repo/blob/storage.go:142-144`; suite skips on it `internal/blobtesting/verify.go:153-155`; s3 returns it `repo/blob/s3/s3_storage.go:138-139`; `cli/command_repository_sync.go:325` |
| `GetModTime` | no sentinel — must be populated on success; suite requires `!outTime.IsZero()` and that it matches `GetMetadata` & `ListBlobs` within 1 min | `internal/blobtesting/verify.go:171-196`; helper `blob.PutBlobAndGetMetadata` `repo/blob/storage.go:389-402` |
| `RetentionMode`/`RetentionPeriod` unsupported | `blob.ErrUnsupportedPutBlobOption` wrapped with `"blob-retention"` | gdrive `repo/blob/gdrive/gdrive_storage.go:145-147`; filesystem `:163-164`; b2 `:155-156`; mapStorage `internal/blobtesting/map.go:118-119`. Helper `PutOptions.HasRetentionOptions()` at `repo/blob/storage.go:189-191` |
| `ExtendBlobRetention` unsupported | `blob.ErrUnsupportedObjectLock` | `repo/blob/storage.go:162-165` |

**⇒ Answer to the explicit question: a `DoNotRecreate` violation must return `blob.ErrBlobAlreadyExists`** (or, if the backend cannot do conditional writes at all, `blob.ErrUnsupportedPutBlobOption`). Any other value — including `nil` — fails `providervalidation` at `internal/providervalidation/providervalidation.go:177-187`. The current gdrive code *does* return `ErrBlobAlreadyExists` (`repo/blob/gdrive/gdrive_storage.go:159-161`) but its existence check is non-atomic and read-your-writes-unsafe, which is exactly issue #3788 (§2.6).

Note: gcs is the **only** backend that implements `DoNotRecreate` for real, via a GCS precondition `Conditions{DoesNotExist: true}` (`repo/blob/gcs/gcs_storage.go:132-135`) with HTTP 412 → `ErrBlobAlreadyExists` in `translateError` (`repo/blob/gcs/gcs_storage.go:112-113`). gdrive attempts it but with a read-then-write race.

### 1.4 Complete sentinel error inventory (package `blob`)

| Sentinel | Declared | Meaning |
|---|---|---|
| `ErrSetTimeUnsupported` | `repo/blob/storage.go:20` | `SetModTime` not supported |
| `ErrInvalidRange` | `repo/blob/storage.go:23` | bad offset/length |
| `ErrInvalidCredentials` | `repo/blob/storage.go:31` (string const at `:27`, `"The provided token has expired"`) | expired token |
| `ErrBlobAlreadyExists` | `repo/blob/storage.go:34` | `DoNotRecreate` violated |
| `ErrUnsupportedPutBlobOption` | `repo/blob/storage.go:38` | unsupported `PutOptions` field |
| `ErrNotAVolume` | `repo/blob/storage.go:42` | `GetCapacity` unsupported |
| `ErrUnsupportedObjectLock` | `repo/blob/storage.go:46` | object-lock unsupported |
| `ErrBlobNotFound` | `repo/blob/storage.go:249` | blob missing |

There is also `repo.ErrRepositoryUnavailableDueToUpgradeInProgress` (non-`blob` but treated as a hard-fail sentinel by the retrying wrapper, `repo/blob/retrying/retrying_storage.go:70-72`).

### 1.5 Where upper layers switch on the sentinels (`errors.Is` call sites)

**`ErrBlobNotFound`** (most load-bearing):
- `repo/connect.go:37` — detect "no repository at this location".
- `repo/format/format_manager.go:167`, `:436`, `:445` — format blob presence; `repo/format/format_blob.go:102` synthesizes it.
- `repo/content/committed_read_manager.go:272`
- `repo/content/sessions.go:125`, `:216`
- `repo/content/indexblob/index_blob_manager_v0.go:236`, `:269`, `:410`
- `repo/maintenance/maintenance_schedule.go:128` — treats missing schedule blob as "no schedule yet".
- `internal/ownwrites/ownwrites.go:109` — tolerate marker-without-blob.
- `internal/cache/persistent_lru_cache.go:117`, `:459`
- `repo/blob/sharded/sharded.go:218-222` — missing shard-parameters blob ⇒ defaults.
- `repo/blob/logging/logging_storage.go:311` — logged at lower severity.
- `internal/server/api_repo.go:169`
- `cli/command_repository_sync.go:311`, `:347`, `:367`
- `repo/blob/retrying/retrying_storage.go:52-53` — **not** retriable.
- Test/validation: `internal/blobtesting/asserts.go:111,134`, `internal/blobtesting/verify.go:46`, `internal/blobtesting/concurrent.go:79,119`, `internal/providervalidation/providervalidation.go:152,157,162,388,427`.

**`ErrBlobAlreadyExists`**: `repo/blob/retrying/retrying_storage.go:67` (non-retriable), `internal/providervalidation/providervalidation.go:182`, `internal/blobtesting/verify.go:112`. *No production (non-test) consumer currently relies on it* — Kopia core does not yet use `DoNotRecreate` on the write path, so its only consumer is validation. It still must be correct because `validate-provider` is the documented post-create step.

**`ErrSetTimeUnsupported`**: `cli/command_repository_sync.go:325`, `internal/blobtesting/verify.go:153`, `repo/blob/retrying/retrying_storage.go:58`.

**`ErrInvalidRange`**: `repo/blob/retrying/retrying_storage.go:55`, `repo/blob/retrying/retrying_storage_test.go:46`, produced by `internal/blobtesting/map.go:77,82` and `internal/blobtesting/object_locking_map.go:97,102`.

**`ErrUnsupportedPutBlobOption`**: `repo/blob/retrying/retrying_storage.go:64`, `internal/providervalidation/providervalidation.go:180`, produced by `internal/blobtesting/map.go:119,121`.

**`ErrInvalidCredentials`**: `repo/blob/retrying/retrying_storage.go:61`, `internal/blobtesting/asserts.go:124`.

**`ErrNotAVolume`**: `cli/command_repository_status.go:73,165`, `internal/server/grpc_session.go:585`, `repo/grpc_repository_client.go:689`, `internal/providervalidation/providervalidation.go:132`, `internal/server/server_test.go:473`, `repo/blob/storagemetrics/storage_metrics_test.go:138`.

**`ErrUnsupportedObjectLock`**: only produced (`repo/blob/storage.go:164`); no `errors.Is` consumer found outside provider code.

---

## 2. Current `repo/blob/gdrive/` backend

Four files, all gated behind `//go:build !no_extra_providers`:
`gdrive_storage.go` (588 lines), `gdrive_options.go` (26), `file_id_cache.go` (138), `gdrive_storage_test.go` (154).

The package literally warns at open time: `log(ctx).Warn("The GDrive storage provider is not actively tested, it may cause data loss, use at your own risk")` — `repo/blob/gdrive/gdrive_storage.go:552`. The CLI registers it as `"a Google Drive folder [Not maintained]"` (`cli/storage_gdrive.go:52`).

### 2.1 Auth path

`CreateDriveService(ctx, opt)` — `repo/blob/gdrive/gdrive_storage.go:511-541`:
- Scope selection (`:516-519`): `drive.DriveFileScope` normally, `drive.DriveReadonlyScope` when `opt.ReadOnly`. `DriveFileScope` = per-file scope, restricted to files the app created — **this is a significant design constraint**: Kopia can only see/manage files it created itself.
- Credential precedence (`:521-527`):
  1. `opt.ServiceAccountCredentialJSON` (raw JSON embedded in config) → `tokenSourceFromCredentialsJSON` (`:499-506`).
  2. `opt.ServiceAccountCredentialsFile` (path) → `tokenSourceFromCredentialsFile` (`:485-497`).
  3. Otherwise `google.DefaultTokenSource(ctx, scope)` (ADC).
- **Both file and JSON paths go through `google.JWTConfigFromJSON`** (`:491`, `:500`) — i.e. **only service-account JWT credentials are accepted**. An OAuth *installed-app* / user client-secret JSON, or an authorized-user refresh-token JSON, will fail here. There is **no OAuth user-consent flow, no refresh-token support, no domain-wide-delegation `Subject` support**. This is the direct cause of issue #2656 (§2.7).
- HTTP client: `oauth2.NewClient(ctx, ts)` + `drive.NewService(ctx, option.WithHTTPClient(hc))` (`:533-538`). Note `blob.ApplicationID` (`repo/blob/storage.go:50`) is **not** applied as a User-Agent here, unlike other backends.

### 2.2 Blob-name → file-ID mapping

- Blob ID maps 1:1 to a Drive **file name** in a single flat parent folder: `toFileName(blobID) = string(blobID)` (`:433-435`), `toblobID(fileName) = blob.ID(fileName)` (`:437-439`). **Flat, no sharding, no prefix.**
- All blobs share MIME type `application/x-kopia` (`blobMimeType`, `:32`) and live under `opt.FolderID`.
- Every operation needs a **file ID**, which requires a `files.list` query: `getFileByBlobID` (`:383-403`) issues
  `Q("'<folder>' in parents and name = '<blobID>' and mimeType = 'application/x-kopia' and trashed = false")`, `PageSize(2)`, and errors with `"Multiple files found for blob id"` when >1 result (`:398-399`) — a real hazard because Drive permits duplicate names in a folder (and the racy `DoNotRecreate` path can create them).
- Wrapped by `tryGetFileByBlobID` (`:371-375`) which retries **only on `ErrBlobNotFound`** (`retryNotFound`, `:460-462`) with `retry.Periodically(ctx, 100ms, 3, …)` (`queryRetryDelay`/`queryRetryMax`, `:37-38`). This 300 ms total is the entire mitigation for Drive's list-index lag — far too short (issue #4272).

### 2.3 `file_id_cache.go` — what it actually is

**In-memory only, per-`gdriveStorage`-instance, not persistent, not shared across connections.**

- `fileIDCache` (`repo/blob/gdrive/file_id_cache.go:16-30`): `Blobs sync.Map` of `blob.ID → *cacheEntry`, plus a 256-slot circular `changeLog` array (`changeLogCacheSize = 1 << 8`, `:12`) guarded by `mu sync.RWMutex`.
- `cacheEntry` (`:33-40`): per-entry `sync.Mutex` + `BlobID` + `FileID` (empty string = unknown/deleted).
- `Lookup(blobID, cb)` (`:53-63`) is the concurrency primitive: `LoadOrStore` the entry then hold `entry.Mut` across the whole callback. **Every gdrive operation on a blob serializes on that per-blob mutex** — `GetMetadata` `:107`, `PutBlob` `:149`, `DeleteBlob` `:223`, `getFileID` `:345`. So it *is* concurrency-safe per blob, but the mutex is held across network I/O (whole upload!), meaning per-blob operations are fully serialized.
- `changeLog` + `VisitBlobChanges` (`:86-111`) exist purely to patch up `ListBlobs`: recently created/deleted blobs are replayed so a lagging Drive list still reports them. **Only 256 entries and only consulted for prefixed lists** (`repo/blob/gdrive/gdrive_storage.go:294-304`) — the un-prefixed `ListBlobs(ctx, "")` path never consults it, and 256 entries is trivially overrun during a snapshot.
- `Clear()` (`:114-120`) wipes both maps, invoked from `FlushCaches` (`gdrive_storage.go:339-342`). Note `Clear` replaces `cache.Blobs` (a `sync.Map` **by value**) while other goroutines may hold `*cacheEntry` pointers — a torn-cache hazard.
- **Nothing is written to disk.** A fresh process, or a second `blob.Storage` created from the same `ConnectionInfo` (which is what `providervalidation` does), starts with an empty cache and must re-resolve every fileID by list query.

**Verdict: not salvageable as-is for the rewrite.** It solves the wrong problem (it is a *latency* cache with an ad-hoc 256-entry consistency patch), it is per-instance so it cannot fix cross-connection read-your-writes, and its lock-across-I/O design caps throughput. A rewrite should use a *persistent* blobID→fileID map (see §5) plus proper Drive-native consistency primitives (`files.create` with `appProperties`, `changes.list` cursor, or a `spaces=drive` name index), and should not hold a mutex across the upload.

### 2.4 Retry / error handling

- `translateError` (`repo/blob/gdrive/gdrive_storage.go:464-483`) maps `*googleapi.Error`:
  - 404 → `blob.ErrBlobNotFound` (`:469-470`)
  - 416 → `blob.ErrInvalidRange` (`:471-472`)
  - 412 → `blob.ErrBlobAlreadyExists` (`:473-474`)
  - everything else → `errors.Wrap(err, "unexpected Google Drive error")` (`:479`).
  **Gaps:** no mapping of 401/403 `authError`/`invalidCredentials` to `blob.ErrInvalidCredentials`; no special handling of 403 `userRateLimitExceeded` / `rateLimitExceeded` / `storageQuotaExceeded`; no 429 handling; no handling of the "file in trash" state. Since everything unmapped is *retriable* per `retrying`, a permanent 403 quota error is retried 10× before failing — exactly the "`Despite 10 retries: … storageQuotaExceeded`" message in issue #2656.
- The storage is wrapped with `retrying.NewWrapper(gdrive)` at construction (`:582`).
- Constructor `New` (`:549-583`) requires `FolderID` (`:554-556`) and then does a liveness probe: `ListBlobs` with a synthetic non-existent prefix `kopia-gdrive-storage-initializing-<unixnano>` (`:573-580`) — same idiom as gcs (`repo/blob/gcs/gcs_storage.go:289-296`).

### 2.5 `ListBlobs` implementation — `repo/blob/gdrive/gdrive_storage.go:260-326`

```go
query := "'<folder>' in parents and mimeType = 'application/x-kopia' and trashed = false"
if prefix != "" {
    query = "'<folder>' in parents and name contains '<capPrefix(prefix)>' and mimeType = '<mime>'"
    // NOTE: 'trashed = false' is DROPPED in the prefixed branch (:292)
}
```

Serious defects:

1. **`name contains` is a substring match, not a prefix match** (`:292`). Drive has no prefix operator. The code compensates client-side with `matchesPrefix` (`:405-407`), so correctness is preserved for the callback — but the server returns a superset, and for short prefixes (Kopia uses 1–3 char prefixes like `p`, `q`, `x`, `n`, `m`) this returns essentially the whole folder every time. Combined with `maxPrefixLen = 26` truncation (`capPrefix`, `:410-416`), this is a major cost driver.
2. **The consumer `return nil`s on the first non-matching entry instead of `continue`** — `:269-271`:
   ```go
   if !matchesPrefix(blobID, prefix) {
       return nil        // <-- silently ABORTS the rest of this page
   }
   ```
   `Pages()` treats a `nil` return as "page consumed OK" and moves on, so **every blob after the first non-match on a page is silently dropped**. Because the `name contains` query returns a superset, non-matches are common. This is a correctness bug that can cause missed blobs (and therefore data loss on maintenance/GC), and it is the single most alarming thing in the file.
3. **Callback errors are conflated with the "skip" signal.** A user callback error *is* propagated (`:281-283`), but the `return nil` at `:270` makes control flow ambiguous.
4. **Prefixed queries drop `trashed = false`** (`:292` vs `:289`), so trashed files can be reported as live blobs.
5. **The un-prefixed path never consults the change log** — `VisitBlobChanges` is only called inside `if prefix != ""` (`:294-303`), so `ListBlobs(ctx, "")` (used by `blob.ReadBlobMap`, `internal/blobtesting/cleanup.go:26`, and `providervalidation` cleanup) has no eventual-consistency compensation at all.
6. Fields mask: `Fields("nextPageToken", listMetadataFields)` where `listMetadataFields = "files(name,id,mimeType,size,modifiedTime)"` (`:43`, used `:306`). No `PageSize` is set (defaults to 100), so large repos page a lot.
7. Every listed file is unconditionally shoved into the fileID cache via `BlindPut` (`:267`) — including files that don't match the prefix.

### 2.6 Upload path (`PutBlob`) — `repo/blob/gdrive/gdrive_storage.go:144-220`

- **Simple/multipart media upload, not truly resumable.** `Media(data.Reader(), googleapi.ChunkSize(uploadChunkSize), googleapi.ContentType("application/octet-stream"))` with `uploadChunkSize = 1 << 20` (1 MiB) — `:180-183` (create) and `:198-202` (update). With `googleapi.ChunkSize` set and a reader whose size exceeds the chunk size, the Go client switches to resumable protocol internally, but there is **no upload-URI persistence**, so a process restart restarts the whole upload; and `ChunkSize(1MiB)` forces many round-trips for Kopia's default 20 MB pack blobs.
- Content type is `application/octet-stream` for the media body (`uploadContentType`, `:35`) while the file's declared MIME type is `application/x-kopia` (`:32`) — only the latter is used for listing/filtering.
- Fields mask on write: `Fields(metadataFields)` where `metadataFields = "name,id,mimeType,size,modifiedTime"` (`:41`, used `:179`, `:197`).
- `SupportsAllDrives(true)` everywhere (`:88`, `:178`, `:196`, `:245`, `:306`, `:378`, `:385`), plus `IncludeItemsFromAllDrives(true)` on lists (`:306`, `:386`) — Shared Drives are supported.
- Retention options rejected up front with `ErrUnsupportedPutBlobOption` (`:145-147`).
- `SetModTime` → `ModifiedTime: opts.SetModTime.Format(time.RFC3339)` (`:165-169`); `GetModTime` filled from the response (`:210-214`), silently ignoring a parse error.
- **`DoNotRecreate` is implemented as a non-atomic read-modify-write** (`:150-161`): resolve fileID (a `files.list` query), and if found *and* `opts.DoNotRecreate`, return `blob.ErrBlobAlreadyExists`. If the list query hasn't caught up, `existingFile` is `false`, so it **creates a second file with the same name and returns `nil`**. That is issue #3788 verbatim.
- The whole `PutBlob` body runs inside `fileIDCache.Lookup`, i.e. **holding the per-blob mutex for the duration of the upload**.

### 2.7 `GetBlob` range handling — `:78-104` / `toRange` `:419-431`

| offset/length | Range header |
|---|---|
| `0, <0` | `""` (no header — full read) |
| `off>0, <0` | `bytes=<off>-` |
| `any, 0` | `bytes=<off>-<off>` (reads 1 byte, then discards because of the `if length != 0` guard at `:97`) |
| else | `bytes=<off>-<off+len-1>` |

Correctness note: for `length == 0` the code requests 1 byte and then **skips the copy** (`:97-101`), so `output` stays empty and `EnsureLengthExactly(0, 0)` passes. Works, but wastes a request. Out-of-range reads rely on Drive returning 416.

### 2.8 Registration & `ConnectionInfo` shape (config compatibility)

- Type string: `gdriveStorageType = "gdrive"` (`repo/blob/gdrive/gdrive_storage.go:31`).
- Registration: `blob.AddSupportedStorage(gdriveStorageType, Options{}, New)` in `init()` (`:585-587`), via the generic registry `repo/blob/registry.go:19-36`.
- `ConnectionInfo()` returns `{Type: "gdrive", Config: &gdrive.Options}` (`:328-333`).
- `Options` (`repo/blob/gdrive/gdrive_options.go:12-26`) — **this JSON shape is the config-compatibility contract**:
  ```json
  { "folderID": "...", "credentialsFile": "...", "credentials": <raw JSON>, "readOnly": false,
    /* plus throttling.Limits inlined */ }
  ```
  - `FolderID` → `folderID` (required)
  - `ServiceAccountCredentialsFile` → `credentialsFile,omitempty`
  - `ServiceAccountCredentialJSON` → `credentials,omitempty`, tagged `kopia:"sensitive"` (`gdrive_options.go:20`)
  - `ReadOnly` → `readOnly,omitempty`
  - embedded `throttling.Limits` (`gdrive_options.go:25`) — inline JSON keys `readsPerSecond`, `writesPerSecond`, `listsPerSecond`, `maxUploadSpeedBytesPerSecond`, `maxDownloadSpeedBytesPerSecond`, `concurrentReads`, `concurrentWrites` (`repo/blob/throttling/throttler.go:142-150`).
- **Any rewrite must keep these JSON keys** or existing `repository.config` files (`repo/local_config.go:97`, `Storage *blob.ConnectionInfo`) and repo tokens (`repo/token.go:14`, `:26`, `:42`) will fail to deserialize — `ConnectionInfo.UnmarshalJSON` (`repo/blob/config.go:16-39`) unmarshals `config` straight into the registered `Options` type. New fields must be `omitempty` additive.
- CLI flags (`cli/storage_gdrive.go:23-30`): `--folder-id` (required), `--read-only`, `--credentials-file` (must exist), `--embed-credentials`, plus `commonThrottlingFlags(cmd, &c.options.Limits)`. `--embed-credentials` slurps the file into `ServiceAccountCredentialJSON` and clears the path (`:35-43`).

### 2.9 Mapping to the open GitHub issues

#### #2344 — "Slow backup process on GDrive" (CLOSED)
*Symptom:* 9 TB backup to a Shared Drive; after switching to `--max-pack-size-mb=40`, snapshot throughput collapsed to **under 1 uploaded file per minute**; interrupted snapshot triggered a full maintenance whose "Rewriting contents from short packs" ran for days. Network was fine (250/280 Mbit). A second reporter saw multi-hour `full-rewrite-contents` phases.

*Root cause (best determinable, with code pointers):* the per-blob cost of gdrive is dominated by **`files.list` name-resolution queries**, not by bytes.
- Every `GetBlob`/`GetMetadata`/`DeleteBlob`/`PutBlob` on a cache-cold blob costs at least one `files.list` (`gdrive_storage.go:383-403`, reached via `getFileIDWithCache` `:355-368`).
- Smaller packs ⇒ ~5× more blobs ⇒ ~5× more list queries; Drive's per-user query quota is far lower than its byte throughput, so the workload becomes quota-bound. Maintenance ("rewriting short packs") is read-heavy over many small blobs — the worst case for this design.
- `ListBlobs` with a short prefix degenerates to a whole-folder `name contains` scan (`:292`) with default page size, so index/maintenance passes re-scan the folder repeatedly.
- The per-blob mutex held across network I/O (`file_id_cache.go:53-63` used at `gdrive_storage.go:149`) prevents any pipelining on the same blob.
- The cache is in-memory only, so it is cold after every process start (`file_id_cache.go` — no persistence anywhere).
- Also relevant: `blob.ApplicationID` isn't sent, and the gdrive backend doesn't batch anything.

*Rewrite implications:* persistent name→fileID index; use `files.get` by ID (cheap) instead of `files.list` by name; larger page sizes; `fields` masks trimmed; avoid holding locks across I/O; consider `drive.changes.list` for incremental index refresh.

#### #2656 — "GDrive Delegate domain-wide authority / Service Account Quota" (OPEN, stale)
*Symptom:* `Despite 10 retries: Create in PutBlob(...): unexpected Google Drive error: googleapi: Error 403: The user's Drive storage quota has been exceeded., storageQuotaExceeded`.

*Root cause:* files created by a service account are **owned by the service account**, which has its own (tiny) Drive storage quota; the user's/organization's quota is not used. Two fixes exist and neither is supported by the current code:
1. **Domain-wide delegation with impersonation** — needs `jwt.Config.Subject` to be set. `tokenSourceFromCredentialsFile`/`…JSON` (`gdrive_storage.go:485-506`) call `google.JWTConfigFromJSON` and never set `Subject`, and `Options` (`gdrive_options.go`) has no field for it. There is no way to express "impersonate user X".
2. **Shared Drive** — files in a Shared Drive are owned by the organization. This works today (`SupportsAllDrives(true)` is set everywhere) and is the workaround the issue thread converged on (comments from `oinegue`/`Skylinar`), but it is undocumented in Kopia and requires a Workspace plan.

Secondary defect: `translateError` (`:464-483`) doesn't recognize 403 `storageQuotaExceeded` as **non-retriable**, so `retrying` burns 10 attempts with exponential backoff (up to 32 s each, `internal/retry/retry.go:16-20`) before surfacing the error — hence the "Despite 10 retries" prefix and a very long hang.

*Rewrite implications:* add `Subject`/impersonation option; add optional OAuth-user (refresh-token) credentials; document/detect Shared Drive; classify 403 quota/rate errors properly (`storageQuotaExceeded` → permanent; `userRateLimitExceeded`/`rateLimitExceeded`/429 → retriable with `Retry-After` honored).

#### #3788 — "Trying to create a gdrive repository (PutBlob with DoNotRecreate)" (OPEN)
*Symptom:* `kopia repository validate-provider` fails at "Validating conditional creates..." with
`ERROR provider validation error: unexpected error returned from PutBlob with DoNotRecreate: <nil>`.

*Root cause:* the assertion is `internal/providervalidation/providervalidation.go:177-187` — `PutBlob(..., DoNotRecreate:true)` on an existing blob must return `ErrBlobAlreadyExists` **or** `ErrUnsupportedPutBlobOption`; gdrive returned `nil`. Chain:
1. Validation writes the blob via `st.pickOne()` (`providervalidation.go:171`) — a **randomly chosen one of 5 equivalent connections** (`pickOne` `:60-62`, connections opened at `:77-99`).
2. The conditional put uses `st.pickOne()` again (`:177`) — statistically a *different* connection, hence a **different `gdriveStorage` with a different, empty `fileIDCache`** (each `blob.NewStorage` call runs `New` → `newFileIDCache()`, `gdrive_storage.go:568`).
3. That connection has no cached fileID, so `getFileIDWithCache` (`:355-368`) issues a `files.list` by name; Drive's name index has not caught up (only 3 × 100 ms of retry, `:37-38`, `:371-375`), so it returns `ErrBlobNotFound`.
4. `PutBlob` sets `existingFile = false` (`:153-154`), skips the `DoNotRecreate` check at `:159-161`, **creates a duplicate file**, and returns `nil` (`:216`).

Note the follow-on damage: the folder now contains two files with the same name, so any later `getFileByBlobID` hits `"Multiple files found for blob id"` (`:398-399`) — a hard, unretriable-in-practice error.

*Rewrite implications:* `DoNotRecreate` must be atomic or must not exist. Drive v3 has **no conditional-create precondition**, so the honest options are (a) return `errors.Wrap(blob.ErrUnsupportedPutBlobOption, "do-not-recreate")` like s3/azure/b2/filesystem/sftp/webdav all do, or (b) implement a create-then-dedupe protocol using `files.list` with `orderBy=createdTime` and delete-the-loser. **(a) is what every non-GCS backend does and is what the validator explicitly blesses at `providervalidation.go:180`.**

#### #4272 — "Read error when validating concurrency in new Google Drive repo" (OPEN)
*Symptom:* `validate-provider` reaches "Running concurrency test for 30s..." then fails with
`error validating concurrency: encountered errors: unexpected error when reading z…: get file by blob id in GetMetadata(z…): unable to complete getFileIDByblobID(z…) despite 3 retries: No results found for blob id (z…): BLOB not found`.
Reporter confirms the file **does exist** in the Drive folder. Reproduced on macOS and Windows; a commenter reports the same class of failure with the rclone→Drive and rclone→SharePoint backends, and that dropping to `--num-storage-connections=1 --put-blob-workers=1 --get-blob-workers=1 --get-metadata-workers=1` makes it pass.

*Root cause:* read-your-writes violation across connections. The failing check is `internal/providervalidation/providervalidation.go:427`:
```go
if !errors.Is(err, blob.ErrBlobNotFound) || fullyWritten { return errors.Wrapf(err, "unexpected error when reading %v", blobID) }
```
`ErrBlobNotFound` is *tolerated* only while the blob is still being written; once `blobWritten[id] = true` (`:340-342`, set right after `PutBlob` returns), a `NotFound` is fatal. gdrive's `GetMetadata` (`gdrive_storage.go:106-142`) resolves the name through `tryGetFileByBlobID` → `files.list` — **eventually consistent** — and the reader is a *different* one of the 5 connections (`pickOne` `:60-62`) with a *different, empty* `fileIDCache`, so it cannot see the writer's in-memory `entry.FileID`. The only mitigation is `retry.Periodically(100ms, 3)` ≈ **300 ms** (`gdrive_storage.go:37-38, 371-375`), which is far below Drive's observed index latency (seconds). The commenter's "reduce concurrency ⇒ passes" observation is consistent: with 1 connection the writer's in-memory cache serves the read.

Note the change-log compensation in `ListBlobs` (`:294-303`) does not help here — it is per-instance and only applies to `ListBlobs`, not `GetMetadata`/`GetBlob`.

*Rewrite implications:* the fileID index must be **shared across connections and durable** (see §5), or name resolution must be replaced with something strongly consistent (e.g. store the fileID in the blob's own `appProperties` and resolve via a `files.get` on a deterministically derived ID — not possible in Drive — or maintain a Kopia-side index blob). At minimum the retry budget must be seconds, not 300 ms, and `New()` should mention the constraint.

---

## 3. Reference backends & shared idioms

### 3.1 Options structure

Every backend has a `<name>_options.go` with a plain struct, JSON tags, `kopia:"sensitive"` on secrets, and an **embedded `throttling.Limits`**:
- gcs `repo/blob/gcs/gcs_options.go:11-31` (`bucket`, `prefix`, `credentialsFile`, `credentials` (sensitive), `readOnly`, `throttling.Limits` at `:27`, `pointInTime`).
- s3 `repo/blob/s3/s3_options.go` — `secretAccessKey` `:24`, `sessionToken` `:25`, `webIdentityToken` `:29` all `kopia:"sensitive"`; `throttling.Limits` at `:42`.
- Others: b2 `:18`, filesystem `:21`, azure `:42`, rclone `:24`, gdrive `:25`, sftp `:32`, webdav `:19`.

### 3.2 `repo/blob/retrying` — `repo/blob/retrying/retrying_storage.go`

- Thin embedder of `blob.Storage` (`:15-17`) wrapping only the four mutating/fetching ops: `GetBlob` (`:19-25`), `GetMetadata` (`:27-31`), `PutBlob` (`:33-37`), `DeleteBlob` (`:39-43`). **`ListBlobs`, `GetCapacity`, `ExtendBlobRetention` are NOT retried** (they fall through to the embedded storage).
- `GetBlob` calls `output.Reset()` before every attempt (`:21`) — providers must be safe to re-invoke.
- Backoff comes from `internal/retry`: `maxAttempts = 10`, initial sleep `100ms`, max sleep `32s`, exponent `1.5` (`internal/retry/retry.go:16-22`), implemented in `internalRetry` (`:58-91`). On exhaustion the error is `errors.Wrapf(lastError, "unable to complete %v despite %v retries", desc, i)` (`:90`) — the exact string seen in issues #2656 and #4272. Context cancellation is checked each iteration (`:69-72`).
- **How a backend opts an error out of retry:** return (or wrap) one of the sentinels. `isRetriable` (`retrying_storage.go:50-77`) returns `false` for `ErrBlobNotFound`, `ErrInvalidRange`, `ErrSetTimeUnsupported`, `ErrInvalidCredentials`, `ErrUnsupportedPutBlobOption`, `ErrBlobAlreadyExists`, `repo.ErrRepositoryUnavailableDueToUpgradeInProgress`; **everything else is retried**. Since `errors.Is` unwraps, `errors.Wrap`/`errors.WithMessagef` around a sentinel preserves the classification (gdrive uses `errors.WithMessagef(blob.ErrBlobNotFound, …)`, `gdrive_storage.go:470`).
- Applied by the backend itself at construction: gcs `repo/blob/gcs/gcs_storage.go:298`, gdrive `repo/blob/gdrive/gdrive_storage.go:582`.
- Some backends also do their own inner retry: filesystem `repo/blob/filesystem/filesystem_storage.go:155,171` uses `retry.WithExponentialBackoffMaxRetries` with `fs.isRetriable`.

### 3.3 `repo/blob/throttling`

- `Limits` struct (`repo/blob/throttling/throttler.go:142-150`): `ReadsPerSecond`, `WritesPerSecond`, `ListsPerSecond`, `UploadBytesPerSecond` (json `maxUploadSpeedBytesPerSecond`), `DownloadBytesPerSecond` (json `maxDownloadSpeedBytesPerSecond`), `ConcurrentReads`, `ConcurrentWrites`.
- Token-bucket implementation `tokenBucketBasedThrottler` (`throttler.go:23-40`, buckets created in `NewThrottler` `:155-172`), plus counting semaphores for concurrency (`throttling/throttling_semaphore.go`).
- Op classification in `BeforeOperation` (`throttler.go:42-53`): `ListBlobs` → list bucket; `GetBlob`/`GetMetadata` → read bucket + `concurrentReads` semaphore; `PutBlob`/`DeleteBlob` → write bucket + `concurrentWrites` semaphore.
- The wrapper `throttlingStorage` (`repo/blob/throttling/throttling_storage.go:44-118`) handles byte accounting: `GetBlob` pre-acquires `length` bytes, or `unknownBlobAcquireLength = 20000000` when `length < 0`, then trues up / refunds after the call (`:49-76`). `PutBlob` acquires `data.Length()` before the call (`:96`).
- **A backend does not wire this itself.** `repo/open.go:278` derives limits by re-marshaling `st.ConnectionInfo().Config` to JSON and unmarshaling into `throttling.Limits` (`throttlingLimitsFromConnectionInfo`, `repo/open.go:519-533`) — hence the *inline* embedded `throttling.Limits` in every `Options` struct. CLI-level `cliOpts.Throttling` overrides (`repo/open.go:279-281`), then `addThrottler` wraps (`repo/open.go:283`, impl at `:452-459`) and registers an `OnUpdate` handler that persists changed limits back into the config file (`repo/open.go:288+`).
- ⇒ **For the rewrite: keep `throttling.Limits` embedded inline in `gdrive.Options` and the `--upload-limit`-style flags keep working for free via `commonThrottlingFlags` (`cli/storage_gdrive.go:29`).**

### 3.4 `repo/blob/sharded`

- `sharded.Storage` (`repo/blob/sharded/sharded.go:34-45`) maps blob IDs to nested directories; providers implement the `Impl` interface (`:26-32`: `GetBlobFromPath`, `GetMetadataFromPath`, `PutBlobInPath`, `DeleteBlobInPath`, `ReadDir`). Completed blobs get the `.f` suffix (`CompleteBlobSuffix`, `:21`; `makeFileName` `:66-68`).
- Shard parameters are read from a blob in storage, with `ErrBlobNotFound` ⇒ defaults (`sharded.go:218-222`).
- **Users:** `filesystem`, `sftp`, `webdav` (path-based backends). **Non-users:** `gcs`, `s3`, `azure`, `b2`, `gdrive` — object stores use a flat namespace with an optional `Prefix` string (gcs `getObjectNameString` `repo/blob/gcs/gcs_storage.go:199-201`).
- **gdrive is flat today** (`toFileName`/`toblobID`, `gdrive_storage.go:433-439`) and has **no `Prefix` option at all** — unlike gcs/s3, you cannot put two repos in one Drive folder. Worth considering for the rewrite (additive `prefix,omitempty` field), though Drive *subfolders* would be the more natural sharding mechanism and would also cut `files.list` result sizes.
- `internal/cache/cache_storage.go:57-64` shows the canonical way to build a local sharded filesystem storage (`DirectoryShards: []int{2}`).

### 3.5 gcs error translation & `DoNotRecreate` (the model to copy)

`repo/blob/gcs/gcs_storage.go:105-125`:
```go
func translateError(err error) error {
    var ae *googleapi.Error
    if errors.As(err, &ae) {
        switch ae.Code {
        case http.StatusRequestedRangeNotSatisfiable: return blob.ErrInvalidRange   // :110-111
        case http.StatusPreconditionFailed:           return blob.ErrBlobAlreadyExists // :112-113
        }
    }
    switch {
    case err == nil:                                    return nil                  // :118-119
    case errors.Is(err, gcsclient.ErrObjectNotExist):   return blob.ErrBlobNotFound // :120-121
    default:                                            return errors.Wrap(err, "unexpected GCS error") // :122-123
    }
}
```
`DoNotRecreate` is a real server-side precondition: `conds := gcsclient.Conditions{DoesNotExist: opts.DoNotRecreate}; if conds != (gcsclient.Conditions{}) { obj = obj.If(conds) }` (`gcs_storage.go:132-135`); the 412 that GCS returns is then mapped to `ErrBlobAlreadyExists`. **Google Drive v3 has no equivalent precondition on `files.create`**, which is why gdrive cannot copy this and should follow the s3/azure/b2 "`ErrUnsupportedPutBlobOption`" route instead.

Other gcs idioms worth mirroring: `timestampmeta.ToMap/FromValue` for `SetModTime`/`GetModTime` via custom metadata (`gcs_storage.go:98-101`, `:140`), `writerChunkSize = 1<<20` (`:26`), `ContentType = "application/x-kopia"` (`:139`), the non-existent-prefix liveness probe in `New` (`:287-296`), and `Close` closing the client (`:237-239`).

### 3.6 Registration & config encoding

- `blob.AddSupportedStorage[T](urlScheme string, defaultConfig T, createStorageFunc func(ctx, *T, bool) (Storage, error))` — generic, `repo/blob/registry.go:19-36`; global `factories` map at `:10`. Called from each backend's `init()` (gdrive `:585-587`, gcs `repo/blob/gcs/gcs_storage.go:301-303`).
- `blob.NewStorage(ctx, cfg ConnectionInfo, isCreate bool)` looks up the factory (`registry.go:40-46`).
- `ConnectionInfo` JSON round-trip: `repo/blob/config.go:10-51`. `UnmarshalJSON` (`:16-39`) reads `{"type":…, "config": <raw>}`, looks the factory up, allocates the registered `Options` via `defaultConfigFunc` and unmarshals into it. `MarshalJSON` (`:42-51`) writes the same shape.
- **Config is stored in plaintext JSON**, not encrypted: `repo/local_config.go:96-97` (`Storage *blob.ConnectionInfo \`json:"storage,omitempty"\``), written by `LocalConfig.writeToFile` (`repo/local_config.go:104-127`) via `atomicfile.Write`. So embedded service-account credentials sit in cleartext in `repository.config`.
- Secrets are only redacted **for display**, by `internal/scrubber/scrub_sensitive.go` — `ScrubSensitiveData` walks struct fields and blanks anything tagged `kopia:"sensitive"` (`scrub_sensitive.go:24`).
- Repository connection *tokens* embed the same `ConnectionInfo` and are password-encrypted: `repo/token.go:14`, `EncodeToken` `:26`, `DecodeToken` `:42`.
- CLI provider registration: `mustRegisterStorageProvider("gdrive", "a Google Drive folder [Not maintained]", func() StorageFlags {…})` (`cli/storage_gdrive.go:49-55`), with `Setup(…)` for flags and `Connect(ctx, isCreate, formatVersion)` returning the `blob.Storage` (`:23-47`).

---

## 4. Provider test suite & `validate-provider`

### 4.1 The compatibility harness lives in `internal/blobtesting/`, not `storagetest`

Files: `asserts.go`, `verify.go` (the `VerifyStorage` entry point), `concurrent.go`, `cleanup.go`, `map.go` (in-memory reference storage), `eventually_consistent.go`, `faulty.go`, `object_locking_map.go`, `storage.go`.
`repo/blob/suite_test.go` is unrelated — it just parameterizes format versions (`repo/blob/suite_test.go:10-24`).

### 4.2 `blobtesting.VerifyStorage(ctx, t, r, opts)` — `internal/blobtesting/verify.go:22-197`

Fixture blobs (`:23-32`): `abcdbbf4…` (empty), `zxce0e35…` (1 byte), `abff4585…` (1 000 bytes), `abgc3dca…` (10 000 bytes), `kopia.repository` (100 bytes). Note the deliberate **empty blob** and the dotted name.

Every behavior asserted, in order:

1. **`VerifyBlobsNotFound`** (`:35-44`) — for each blob, `GetBlob(id,0,-1)` must be `ErrBlobNotFound` with an *empty output buffer* (`AssertGetBlobNotFound`, `asserts.go:104-114` — note `b.Length() != 0` is also a failure), and `GetMetadata` must be `ErrBlobNotFound` (`AssertGetMetadataNotFound`, `asserts.go:130-137`).
2. **Delete-nonexistent** (`:46-48`) — `DeleteBlob("no-such-blob")` must return `nil` or `ErrBlobNotFound`.
3. **`AddBlobs`** (`:56-66`) — **concurrent duplicate writes**: each blob is written `initialAddConcurrency` times in parallel `t.Parallel()` subtests (2, or **4 under `CI`**, `:50-53`). All must return `nil`. This is a real concurrency requirement on `PutBlob` of the *same* ID.
4. **`GetBlobs`** (`:68-76`) → `AssertGetBlob` (`asserts.go:36-89`): full read equals expected; `GetBlob(id,0,0)` succeeds with zero length; first-half read; second-half read; then four **invalid-range** assertions `(-3,1)`, `(len,3)`, `(len-1,3)`, `(len+1,3)` must each return *some* error (`asserts.go:85-88`, `:92-101`).
5. **`ListBlobs`** (`:78-101`):
   - `ListErrorNoPrefix` / `ListErrorWithPrefix` — a callback returning `errExpected` must surface via `require.ErrorIs` (`:81-92`). **The current gdrive `ListBlobs` `return nil` at `gdrive_storage.go:270` is a latent failure here.**
   - `ListNoPrefix` — exactly the 5 blobs; `ListWithPrefix("ab")` — exactly 3. Both via `AssertListResults` (`asserts.go:140-182`), which *also* cross-checks `GetMetadata` for every listed blob: same `BlobID`, same `Length`, and `|Δtimestamp| <= 1 minute` (`asserts.go:148-169`). Comparison is order-insensitive (sorted, `:176-181`).
6. **`OverwriteBlobs`** (`:103-120`) — **the DoNotRecreate assertion**: writing `[]byte{99}` over each existing blob must, when `opts.DoNotRecreate` is set, fail with `require.ErrorIs(err, blob.ErrBlobAlreadyExists)` *and* leave the original contents intact (`:112-113`); otherwise it must succeed and the new contents must be readable (`:115-116`).
7. **`ExtendBlobRetention`** (`:122-132`) — must succeed iff `opts.RetentionMode != ""`, must error otherwise.
8. **`DeleteBlobsAndList`** (`:134-140`) — `DeleteBlob` **twice** on the same ID, both `require.NoError`; then list results must reflect the deletion for both `"ab"` and `""` prefixes.
9. **`PutBlobsWithSetTime`** (`:142-169`) — either `ErrSetTimeUnsupported` (test skips) or the mtime must be reflected in *both* `GetMetadata` and `ListAllBlobs` within 1 minute (`AssertTimestampsCloseEnough`, `asserts.go:22-33`).
10. **`PutBlobsWithGetTime`** (`:171-196`) — `GetModTime` must be populated non-zero and must agree with `GetMetadata` and `ListAllBlobs`.

Then, separately (as the gdrive test does):
- `blobtesting.AssertConnectionInfoRoundTrips` (`verify.go:203-215`).
- `providervalidation.ValidateProvider(ctx, st, blobtesting.TestValidationOptions)` — `TestValidationOptions` at `verify.go:220-229` (same as `DefaultOptions` but `ConcurrencyTestDuration: 15s`).

Also available: `blobtesting.VerifyConcurrentAccess(t, st, ConcurrentAccessOptions{…})` (`internal/blobtesting/concurrent.go:37-156`) — N getters/putters/deleters/listers over a shared pool of random blob IDs; getters accept `nil` or `ErrBlobNotFound` only (`:73-84`), deleters same (`:115-124`), listers must never error (`:142-146`), putters must never error (`:99-101`). **The gdrive test does not currently call this.**
And `blobtesting.CleanupOldData(ctx, tb, st, MinCleanupAge)` (`internal/blobtesting/cleanup.go:19-40`, `MinCleanupAge = time.Hour` at `:16`).

### 4.3 `kopia repository validate-provider` — `cli/command_repository_validate_provider.go`

- Command definition `:17-30`: flags `--num-storage-connections`, `--concurrency-test-duration`, `--put-blob-workers`, `--get-blob-workers`, `--get-metadata-workers` (note: **no `--list-blobs-workers` flag**, though the option field exists). Defaults come from `providervalidation.DefaultOptions` (`:20`).
- Runs `providervalidation.ValidateProvider(ctx, dr.BlobStorage(), c.opt)` under `directRepositoryWriteAction` (`:29`, `:32-36`).
- `DefaultOptions` (`internal/providervalidation/providervalidation.go:43-52`): `MaxClockDrift 3m`, `ConcurrencyTestDuration 30s`, `NumEquivalentStorageConnections 5`, 3 each of put/get/getmetadata/list workers, `MaxBlobLength 10e6`.
- Can be bypassed entirely with `KOPIA_SKIP_PROVIDER_VALIDATION` (`providervalidation.go:106-108`).

**What `ValidateProvider` asserts, in execution order** (`providervalidation.go:105-277`) — these are the log lines users paste into issues:

| Log line | Assertion | Lines |
|---|---|---|
| `Opening N equivalent storage connections...` | `blob.NewStorage(ctx, st.ConnectionInfo(), false)` succeeds N−1 times; each wrapped in a logging wrapper | `:77-99` |
| `Validating storage capacity and usage` | `GetCapacity` returns `ErrNotAVolume`, or `FreeB <= SizeB`, else fail | `:127-138` |
| `Validating blob list responses` | `ListAllBlobs(uberPrefix)` returns 0 | `:140-144`, helper `:490-501` |
| `Validating non-existent blob responses` | full `GetBlob(id,0,-1)`, partial `GetBlob(id,0,5)`, and `GetMetadata` on a missing blob all return `ErrBlobNotFound` | `:146-164` |
| `Writing blob (5000000 bytes)` | `PutBlob` of 5 MB succeeds | `:166-173` |
| **`Validating conditional creates...`** | `PutBlob(DoNotRecreate:true)` on the existing blob returns `ErrUnsupportedPutBlobOption` **or** `ErrBlobAlreadyExists`; anything else (incl. `nil`) ⇒ `"unexpected error returned from PutBlob with DoNotRecreate: %v"` — **this is #3788** | `:175-187` |
| `Validating list responses...` | counts: `uberPrefix`→1, `prefix1`→1, `prefix2`→0 | `:189-201` |
| `Validating partial reads...` | offsets/lengths `{0,10} {1,10} {2,1} {5,0} {len-5,5}` must return exactly the right bytes | `:203-225` |
| `Validating full reads...` | `GetBlob(id,0,-1)` returns all 5 MB byte-identical | `:227-237` |
| `Validating metadata...` | `GetMetadata().Length` matches; `|now − Timestamp| <= MaxClockDrift` (3 min) | `:239-265` |
| **`Running concurrency test for 30s...`** | see below — **this is #4272** | `:267-272`, `:458-478` |
| `Cleaning up temporary data...` | best-effort list+delete of `uberPrefix` (deferred, so it runs even on failure — which is why it prints *before* the error) | `:122`, `:480-488` |

**The concurrency test** (`providervalidation.go:279-478`) runs, against a *random* one of the N connections per call (`pickOne` `:60-62`):
- `NumPutBlobWorkers` writers (`:315-349`): generate random data, register `blobSeeds[id]`+`blobIDs`, sleep 0–500 ms (`randomSleep` `:351-353`) so readers race the write, `PutBlob`, then mark `blobWritten[id] = true`.
- `NumGetBlobWorkers` readers (`:368-409`): pick a random registered blob; `GetBlob(id,0,-1)`; **`ErrBlobNotFound` is only acceptable while `fullyWritten == false`** (`:388`); returned bytes must equal the seed-derived bytes (`:397-402`).
- `NumGetMetadataWorkers` (`:411-447`): same tolerance rule at `:427`; `bm.Length` must equal the expected length (`:438-440`). **#4272's error text (`unexpected error when reading …`) comes from `:429`** (the `GetMetadata` worker), matching the `GetMetadata(...)` in the reported message.
- `listBlobWorker` (`:449-456`) is a **stub — `// TODO: implement me`**, returns nil immediately. So `--list-blobs-workers` validation is currently vacuous.

⇒ **#4272 is precisely: "a blob that `PutBlob` reported as written was not visible to `GetMetadata` on another connection."** Fixing it requires cross-connection read-your-writes, i.e. a shared/persistent fileID index or an inherently consistent lookup.

### 4.4 Credential-absent skip pattern

- Global gate: `testutil.ProviderTest(t)` skips unless `KOPIA_PROVIDER_TEST` is set — `internal/testutil/testutil.go:20-26`. Called first in every gdrive test (`gdrive_storage_test.go:28`, `:41`, `:65`).
- gdrive-specific env vars (`repo/blob/gdrive/gdrive_storage_test.go:93-115`):
  - `KOPIA_GDRIVE_TEST_FOLDER_ID` — required, else `t.Skip("KOPIA_GDRIVE_TEST_FOLDER_ID not provided")` (`:96-99`, also `:67-70`).
  - `KOPIA_GDRIVE_CREDENTIALS_JSON_GZIP` — base64(gzip(service-account JSON)); decode failure ⇒ `t.Skip` (`:101-109`).
  - `KOPIA_GDRIVE_CREDENTIALS_FILE` — used by `TestGdriveStorageInvalid` (`:76`).
- gcs uses the identical idiom with a shared helper: `getEnvVarOrSkip(t, envVarName)` → `t.Skipf("%q is not set", envVarName)` (`repo/blob/gcs/gcs_storage_test.go:95-104`) and `getCredJSONFromEnv` doing base64→gunzip (`:106-118`).
- Test isolation idiom in gdrive: `createTestFolderOrSkip` creates a uuid-named subfolder and returns a copy of `Options` pointing at it (`gdrive_storage_test.go:117-141`); `deleteTestFolder` cleans up (`:143-154`). `CreateDriveService` is exported solely for this (`gdrive_storage.go:508-511`).
- ⇒ **Follow this exact pattern**: `testutil.ProviderTest(t)` + `getEnvVarOrSkip`-style helpers + per-test uuid subfolder + `defer deleteTestFolder`.

---

## 5. Cache-storage idiom — what can be reused for a persistent blobID→fileID map

There are **three** existing, dependency-free mechanisms; all are built on `blob.Storage` over a local sharded filesystem, so a fileID cache can reuse them verbatim.

1. **`internal/cache.Storage` / `NewStorageOrNil`** — `internal/cache/cache_storage.go:24-65`.
   A narrow interface (`GetBlob`, `GetMetadata`, `PutBlob`, `DeleteBlob`, `ListBlobs`, `TouchBlob`) backed by `filesystem.New(..., sharded.Options{DirectoryShards: []int{2}})` rooted at `<cacheDir>/<subdir>` (`:57-64`), created with `DirMode = 0o700` (`:21`). Returns `(nil, nil)` when caching is disabled (`:41-43`). **This is the ready-made persistent KV store — key = blob ID string, value = bytes.**

2. **`internal/cache.PersistentCache`** — `internal/cache/persistent_lru_cache.go:31-…`.
   Durable on-disk cache with LRU sweeping, HMAC integrity via `internal/cacheprot`, metrics, and `GetOrLoad(ctx, key, fetch, output)` (`:59+`). Handles `ErrBlobNotFound` tolerantly (`:117`, `:459`). Heavier than needed for a fileID map, but it is the blessed "cache with integrity + eviction" primitive.

3. **`internal/ownwrites.CacheStorage`** — `internal/ownwrites/ownwrites.go:34-206`. **This is the closest existing analogue to what gdrive needs, and it is the strongest signal for the rewrite design.**
   It is a `blob.Storage` wrapper whose stated purpose (`ownwrites.go:1-4`) is *"ensures that recently-written blobs show up in ListBlob() results, if the underlying provider is eventually consistent when it comes to list results."*
   - `PutBlob` writes a marker blob `add<blobID>` into the local cache storage on success (`:128-139`); `DeleteBlob` writes `del<blobID>` (`:142-151`).
   - `ListBlobs` merges: reads cached adds and deletes, subtracts server results, suppresses locally-deleted blobs, and for any add-marker the server didn't return it falls back to `GetMetadata` — tolerating `ErrBlobNotFound` (`:49-125`, esp. `:107-122`).
   - Markers are swept after `cacheDuration` on a `sweepFrequency = 5 * time.Minute` cadence (`:22-27`, `:163-188`).
   - Wired in `repo/content/committed_read_manager.go:391-397` (`newOwnWritesCache`), with backing storage from `newCacheBackingStorage(ctx, caching, "own-writes")` (`:409+`) and `ownWritesCacheDuration = 15 * time.Minute` (`committed_read_manager.go:41`), applied only to `cachedIndexBlobPrefixes` (`:53-58`).
   - **Limitation for our purposes:** it only fixes `ListBlobs`, not `GetBlob`/`GetMetadata`, and it is scoped to index-blob prefixes. But its cache-backing-storage pattern is exactly reusable, and a gdrive-specific variant could store the actual fileID as the marker payload, making it a genuine cross-process blobID→fileID index.

4. **`internal/listcache`** — `internal/listcache/listcache.go`. Caches whole `ListBlobs` results as HMAC-signed JSON blobs in a cache storage (`saveListToCache` `:36-52`, `readBlobsFromCache` `:54+`), keyed by prefix, with an expiry (`cachedList{ExpireAfter, Blobs}` `:32-35`). Wired at `repo/content/committed_read_manager.go:400-407`. Another reusable persistent-KV precedent (uses `internal/hmac` for integrity).

**Conclusion for R1:** no new dependency is needed. A persistent fileID index can be built as `cache.NewStorageOrNil(ctx, cacheDir, maxBytes, "gdrive-fileids")` storing `blobID → fileID` (bytes), optionally HMAC-protected the way `listcache` does it. The important caveat: the cache directory is a *client-side* artifact — two different machines connecting to the same Drive folder will each have their own, so the fileID index cannot be the *only* consistency mechanism; it must degrade gracefully to a (longer-retry) name lookup, and `PutBlob`/`DeleteBlob` must keep it coherent. Note also that `blob.Storage` construction (`New`) has **no access to `CachingOptions`** today — the signature is `New(ctx, *Options, isCreate bool)` (`repo/blob/gdrive/gdrive_storage.go:549`) — so the cache directory would need to come from `Options` (an additive `omitempty` JSON field) or from a package-level default resolved via `internal/ospath`.

---

## Appendix — files read for this recon

`repo/blob/storage.go`, `repo/blob/config.go`, `repo/blob/registry.go`, `repo/blob/storage_test.go`, `repo/blob/suite_test.go`,
`repo/blob/gdrive/gdrive_storage.go`, `repo/blob/gdrive/gdrive_options.go`, `repo/blob/gdrive/file_id_cache.go`, `repo/blob/gdrive/gdrive_storage_test.go`,
`repo/blob/gcs/gcs_storage.go`, `repo/blob/gcs/gcs_options.go`, `repo/blob/gcs/gcs_storage_test.go`,
`repo/blob/s3/s3_storage.go`, `repo/blob/s3/s3_options.go`, `repo/blob/azure/azure_storage.go`, `repo/blob/b2/b2_storage.go`, `repo/blob/filesystem/filesystem_storage.go`,
`repo/blob/retrying/retrying_storage.go`, `repo/blob/throttling/throttler.go`, `repo/blob/throttling/throttling_storage.go`, `repo/blob/sharded/sharded.go`,
`internal/retry/retry.go`, `internal/providervalidation/providervalidation.go`, `internal/blobtesting/{verify,asserts,concurrent,cleanup,map}.go`, `internal/testutil/testutil.go`,
`internal/ownwrites/ownwrites.go`, `internal/listcache/listcache.go`, `internal/cache/cache_storage.go`, `internal/cache/persistent_lru_cache.go`, `internal/scrubber/scrub_sensitive.go`,
`repo/open.go`, `repo/local_config.go`, `repo/token.go`, `repo/content/committed_read_manager.go`,
`cli/storage_gdrive.go`, `cli/command_repository_validate_provider.go`,
`site/content/docs/Repositories/_index.md`.

GitHub issues fetched via `gh issue view <n> --repo kopia/kopia [--comments]`: #2344 (CLOSED), #2656 (OPEN, stale), #3788 (OPEN), #4272 (OPEN).

---

# Section B — rclone Drive catalog (R2)


Source: local read-only clone at `~/sandbox/rclone`, commit `1d03a771727b6b158d4675442c41d3280d9eee88` (2026-08-01).
All paths below are relative to `/home/jpeverill/sandbox/rclone`. Every claim carries a `file:line`.

Key files:
- `backend/drive/drive.go` (4773 lines) — the backend
- `backend/drive/upload.go` (239 lines) — resumable upload
- `backend/drive/metadata.go` (641 lines) — permissions/labels/metadata
- `lib/pacer/pacer.go`, `lib/pacer/pacers.go` — the pacer
- `fs/pacer.go` — rclone's wrapper (logging + wiring to `--low-level-retries`)

---

## 1. Pacer

### 1.1 The generic `Pacer` machine (`lib/pacer/pacer.go`)

The Pacer is a **single-token gate plus a pluggable `Calculator`**. It is *not* a
classic token bucket at this layer — the bucket lives inside the GoogleDrive
calculator (see 1.2).

- State handed to the calculator on every call: `SleepTime`, `ConsecutiveRetries`, `LastError` — `lib/pacer/pacer.go:16-20`.
- Defaults from `New()`: `maxConnections: 0` (unlimited), `retries: 3` — `lib/pacer/pacer.go:80-83`. Drive overrides these (see 1.4).
- `beginCall` takes the single pacer token, then spawns a goroutine that sleeps `SleepTime` and puts the token back — `lib/pacer/pacer.go:157-186`. Note the sleep time is **re-read after acquiring the token** (`:170-174`) because it may have gone stale while waiting — a nice detail: a rate-limit hit that happened while you were queued applies to you too.
- If `SleepTime == 0` the token dance is skipped entirely (`:167`) — zero-cost fast path.
- `endCall` increments `ConsecutiveRetries` on retry, **resets it to 0 on any non-retry outcome**, and recomputes `SleepTime` — `lib/pacer/pacer.go:192-205`.
- The retry loop: `for i := 1; i <= retries; i++ { beginCall; invoker; endCall; if !retry break }` — `lib/pacer/pacer.go:220-235`.
- `Call` uses full retries; `CallNoRetry` runs exactly once and wraps the error as a retry error for the caller/upper layer to handle — `lib/pacer/pacer.go:243-257`.
- `MaxConnectionsOption` adds a second semaphore of connection tokens, with reentrancy detection via stack inspection (`caller.Present("(*Pacer).call")`) so a pacer call nested in another pacer call doesn't deadlock — `lib/pacer/pacer.go:220-225`, `:106-122`.
- `RetryAfterError(err, d)` / `IsRetryAfter(err)` let a backend attach a server-dictated delay to an error; every calculator honours it first — `lib/pacer/pacer.go:263-302`.

### 1.2 The GoogleDrive calculator (`lib/pacer/pacers.go:142-210`) — **this is the one Drive uses**

```go
// GoogleDrive is a specialized pacer for Google Drive
//
// It implements a truncated exponential backoff strategy with randomization.
// Normally operations are paced at the interval set with SetMinSleep. On errors
// the sleep timer is set to (2 ^ n) + random_number_milliseconds seconds.
//
// See https://developers.google.com/drive/v2/web/handle-errors#exponential-backoff
```
— `lib/pacer/pacers.go:142-148`

Structure — `lib/pacer/pacers.go:149-153`:
```go
type GoogleDrive struct {
	minSleep time.Duration // minimum sleep time
	burst    int           // number of requests without sleeping
	limiter  *rate.Limiter // rate limiter for the minSleep
}
```

- Library defaults for the calculator itself: `minSleep: 10ms`, `burst: 100` — `lib/pacer/pacers.go:161-168`. **Drive overrides minSleep to 100ms** (see 1.3).
- The limiter is `golang.org/x/time/rate.NewLimiter(rate.Every(minSleep), burst)` — `lib/pacer/pacers.go:178`. So the *steady state* is a genuine token bucket: 10 req/s sustained with a burst pool of 100. `burst <= 0` is clamped to 1 (`:175-177`).
- `Calculate` — `lib/pacer/pacers.go:192-210`:
  1. If the error carries a Retry-After, sleep for that (floored at `minSleep`) — `:193-198`.
  2. If `ConsecutiveRetries == 0` (success): `return c.limiter.Reserve().Delay()` — `:201-203`. **On success the sleep time collapses straight back to whatever the token bucket says** (usually 0 while burst tokens remain). There is no gradual decay — it is instant re-acceleration.
  3. Otherwise, truncated exponential backoff capped at 5: `time.Second << (n-1) + rand.Int63n(time.Second)` — `:204-209`. Comment: `// consecutiveRetries starts at 1 so go from 1,2,3,4,5,5 => 1,2,4,8,16,16` — `lib/pacer/pacers.go:207`. So the backoff ladder is **1s, 2s, 4s, 8s, 16s, 16s… each plus 0-1000ms jitter**, with no maxSleep field (16s is the effective cap).

Contrast with the generic `Default` calculator (`lib/pacer/pacers.go:30-102`), which Drive does *not* use: minSleep 10ms, maxSleep 2s, decayConstant 2, attackConstant 1 (`:44-49`); attack `sleep << 1 / ((1<<1)-1)` = doubling (`:93`); decay `(s<<2 - s) >> 2` = ×0.75 per success (`:100`).

### 1.3 Drive's defaults and flags (`backend/drive/drive.go`)

```go
defaultMinSleep = fs.Duration(100 * time.Millisecond)   // drive.go:66
defaultBurst    = 100                                    // drive.go:67
```

Construction — `backend/drive/drive.go:1404`:
```go
pacer: fs.NewPacer(ctx, pacer.NewGoogleDrive(pacer.MinSleep(opt.PacerMinSleep), pacer.Burst(opt.PacerBurst))),
```

Flags:
- `--drive-pacer-min-sleep` / `pacer_min_sleep`, default `100ms` — `backend/drive/drive.go:580-584`, docs `docs/content/drive.md:1154-1163`.
- `--drive-pacer-burst` / `pacer_burst`, default `100` — `backend/drive/drive.go:585-589`, docs `docs/content/drive.md:1165-1174`.

**Net effect: sustained 10 API calls/second per Fs, with a 100-call burst allowance, and a 1s→16s jittered exponential backoff on rate limits.**

### 1.4 Retries wiring (`fs/pacer.go`)

- `fs.NewPacer` sets `retries = max(ci.LowLevelRetries, 1)` and `maxConnections = max(ci.MaxConnections, 0)` — `fs/pacer.go:23-37`.
- `--low-level-retries` default **10** — `fs/config.go:175-179`. So each Drive API call gets up to 10 in-pacer attempts.
- `--max-connections` default **0** = unlimited — `fs/config.go:554-558`.
- Every retry is logged and the error wrapped: `Debugf("pacer", "low level retry %d/%d (error %v)", ...)`, `err = fserrors.RetryError(err)` — `fs/pacer.go:85-92`.
- A `logCalculator` decorator logs "Rate limited, increasing sleep to %v" / "Reducing sleep to %v" — `fs/pacer.go:39-52`. Cheap and very useful for field diagnosis.

### 1.5 Concurrency defaults

- `--transfers` default **4** — `fs/config.go:65-69`.
- `--checkers` default **8** — `fs/config.go:60-64`.
- `--retries` (high level, whole-sync) default **3** — `fs/config.go:165-169`.
- ListR uses `--checkers` goroutines — `backend/drive/drive.go:2295`.
- Bulk deletes use `--checkers` goroutines — `fs/operations/operations.go:599-633`.

Note that transfers/checkers concurrency sits *above* the single shared pacer, so the pacer is the real global throttle: 8 checkers all funnel through one 10/s token bucket.

---

## 2. Error → retry decision map

Everything goes through one function, `(*Fs).shouldRetry` — `backend/drive/drive.go:941-977`. It is called in 26 places in drive.go (every single API call site is wrapped in `f.pacer.Call(func() (bool, error) { ...; return f.shouldRetry(ctx, err) })`).

| Condition | Decision | Line |
|---|---|---|
| `ctx` cancelled/deadline (`fserrors.ContextError`) | **stop**, return ctx error | `drive.go:942-944` |
| `err == nil` | no retry | `drive.go:945-947` |
| `fserrors.ShouldRetry(err)` — net timeouts/temporary, plus a string-match list | **retry** | `drive.go:948-950` |
| `googleapi.Error` with `500 <= Code < 600` | **retry** (comment: `// All 5xx errors should be retried`) | `drive.go:953-957` |
| reason `"rateLimitExceeded"` | **retry** | `drive.go:960-966` |
| reason `"userRateLimitExceeded"` | **retry** | `drive.go:960-966` |
| reason `rateLimitExceeded`/`userRateLimitExceeded` **AND** `Message == "User rate limit exceeded."` **AND** `--drive-stop-on-upload-limit` | **fatal**, abort sync (`fserrors.FatalError`) | `drive.go:961-965` |
| reason `"downloadQuotaExceeded"` **AND** `--drive-stop-on-download-limit` | **fatal** | `drive.go:966-968` |
| reason `"quotaExceeded"` or `"storageQuotaExceeded"` **AND** `--drive-stop-on-upload-limit` | **fatal** | `drive.go:968-971` |
| reason `"teamDriveFileLimitExceeded"` **AND** `--drive-stop-on-upload-limit` | **fatal** | `drive.go:971-974` |
| anything else (incl. all other 4xx) | **fail fast**, no retry | `drive.go:976` |

The exact code — `backend/drive/drive.go:958-975`:
```go
if len(gerr.Errors) > 0 {
	reason := gerr.Errors[0].Reason
	if reason == "rateLimitExceeded" || reason == "userRateLimitExceeded" {
		if f.opt.StopOnUploadLimit && gerr.Errors[0].Message == "User rate limit exceeded." {
			fs.Errorf(f, "Received upload limit error: %v", err)
			return false, fserrors.FatalError(err)
		}
		return true, err
	} else if f.opt.StopOnDownloadLimit && reason == "downloadQuotaExceeded" {
	...
	} else if f.opt.StopOnUploadLimit && (reason == "quotaExceeded" || reason == "storageQuotaExceeded") {
	...
	} else if f.opt.StopOnUploadLimit && reason == "teamDriveFileLimitExceeded" {
```

Notes and gaps worth flagging for our design:

- **Only `gerr.Errors[0]` is inspected** — `drive.go:959`. If Google returns multiple error items, rclone looks at the first only.
- **`dailyLimitExceeded`, `sharingRateLimitExceeded`, `appNotAuthorizedToFile`, `insufficientFilePermissions`, `domainPolicy` are NOT handled at all.** A repo-wide grep finds zero occurrences of these strings in any `.go` file. They land in the default branch and **fail fast without retry**. That is a deliberate-looking choice (they're all non-transient authorization/policy errors) except `sharingRateLimitExceeded`, which arguably should be retried — a gap we could improve on.
- **429 + `Retry-After` is not parsed by the Drive backend.** The pacer machinery *supports* it (`pacer.RetryAfterError` → `Calculate` honours it — `lib/pacer/pacers.go:193-198`), and other backends use it, but the Drive backend never constructs one. Google Drive rate limits arrive as HTTP 403 with `rateLimitExceeded`, not 429, which is why. HTTP 429 would only be caught if `googleapi.Error` maps it — it does not fall into the 5xx branch, so a bare 429 with no recognised reason would **fail fast**. `fs/fshttp/http.go:483-492` defines `isRetryableResponse` covering `429 || >=500` but that is only used for log dumping (`fs/fshttp/http.go:677`), not retry policy.
- `--drive-stop-on-upload-limit` help explains why: the 750 GiB/day upload limit is undocumented and only distinguishable by error *message* string — `backend/drive/drive.go:616-632`, with the caveat `"Note that this detection is relying on error message strings which Google don't document so it may break in the future."` (`drive.go:625-626`).
- `fserrors.ShouldRetry` — `fs/fserrors/error.go:398-434` — walks the error chain for `Timeout()`/`Temporary()` (`:349-371`), then matches a hard-coded list of network-error substrings (`fs/fserrors/error.go:377-388`): `"use of closed network connection"`, `"unexpected EOF reading trailer"`, `"transport connection broken"`, `"http: ContentLength="`, `"server closed idle connection"`, `"bad record MAC"`, `"stream error:"`, `"http2: server sent GOAWAY"`, `"tls: use of closed connection"`. Comment above it: `// This is incredibly ugly - if only errors.Cause worked for all errors and all errors were exported from the stdlib.` (`fs/fserrors/error.go:376-377`).
- Separate one-off: `cannotDownloadAbusiveFile` is special-cased at the download site, retrying the GET with `&acknowledgeAbuse=true` if `--drive-acknowledge-abuse` — `backend/drive/drive.go:4386-4402`, helper `isGoogleError` at `drive.go:4371-4380`.
- Root lookup special case: a 404 on `getRootID` means the OAuth scope can't read root, so fall back to the literal string `"root"` — `backend/drive/drive.go:1465-1476`.
- Tests pinning the fatal-error semantics: `backend/drive/drive_internal_test.go:340-388`.

---

## 3. Batching — **rclone does NOT batch Drive operations at all**

`grep -rni "batch" backend/drive/` returns **zero hits**. There is no `batcher` for Drive (unlike `backend/dropbox/batcher.go` / `lib/batcher` which other backends use). Drive uses no homogeneous batch endpoint (`https://www.googleapis.com/batch/drive/v3`), no multipart batch bodies, nothing.

What it does instead:

### 3.1 Deletes — parallelised by worker pool, one API call each
- Single delete: `(*Fs).delete` — `backend/drive/drive.go:2732-2751`. Trash = `Files.Update(id, {Trashed:true}).Fields("")`; permanent = `Files.Delete(id).Fields("")`. Note `Fields("")` — **asks for an empty response body**, cheap.
- Bulk: the generic `operations.DeleteFilesWithBackupDir` spins up `--checkers` (8) goroutines all reading from one channel — `fs/operations/operations.go:599-633`. Each goroutine's call still passes through the single shared pacer, so real concurrency is bounded by 10/s.
- Fatal errors abort the whole delete pool early — `fs/operations/operations.go:616-622`.
- Directory purge is *not* recursive-delete-per-file: `purgeCheck` deletes the folder ID itself in one call and lets Drive cascade — `backend/drive/drive.go:2755-2800`. That is the big win: **deleting a folder is 1 API call, not N**.
- `CleanUp` on My Drive is a single `Files.EmptyTrash()` — `backend/drive/drive.go:2990-3009`, with the note `"Note that emptying the trash happens in the background and can take some time."` (`drive.go:3006`).
- Shared Drives have no EmptyTrash, so `cleanupTeamDrive` recursively lists and deletes item-by-item — `backend/drive/drive.go:2957-2986`.

### 3.2 Moves — one `Files.Update` with `AddParents`/`RemoveParents`
- `Move` — `backend/drive/drive.go:3119-3131`. A rename+reparent is **one call**.
- `MergeDirs` moves every child individually in a serial loop — `backend/drive/drive.go:2645-2662`. Not batched, not parallel.
- `DirMove` is likewise a single `Files.Update` — `backend/drive/drive.go:3207+`.

### 3.3 Metadata — parallelised with errgroup, not batched
- Permissions for a file are fetched with `errgroup` limited to `--checkers` — `backend/drive/metadata.go:385-411` (`g.SetLimit(o.fs.ci.Checkers)`).
- Permissions are memoised in a process-wide map guarded by a mutex — `backend/drive/metadata.go:110-112`, `:134-136`, `:181-190`. On Shared Drives `HasAugmentedPermissions == false` short-circuits the whole fetch: `"Don't process permissions if there aren't any specifically set"` — `backend/drive/metadata.go:370-378`. **A cheap "is there anything to fetch?" flag saves an entire round of transactions.**

**Lesson for us:** rclone's answer to Drive throughput is *not* batching. It is (a) fewer calls per logical op, (b) a worker pool, (c) one global rate limiter. Google's Drive batch endpoint was in fact deprecated for global HTTP batch, so this is probably the right call.

---

## 4. Uploads (`backend/drive/upload.go`)

### 4.1 Simple vs resumable
- Cutoff: `size >= 0 && size < int64(f.opt.UploadCutoff)` → simple media upload; otherwise resumable — `backend/drive/drive.go:2587-2607` (create path) and `drive.go:4496-4512` (update path).
- `--drive-upload-cutoff` default = `defaultChunkSize` = **8 MiB** — `backend/drive/drive.go:519-523`, `drive.go:74`.
- `--drive-chunk-size` default **8 MiB**, minimum **256 KiB** (`googleapi.MinUploadChunkSize`), and must be a **power of two** — `drive.go:524-535`, `drive.go:73-74`, validation `checkUploadChunkSize` at `drive.go:1338-1346` using `isPowerOfTwo` (`drive.go:1204-1213`). Confirmed `MinUploadChunkSize = 256 * 1024` in `google.golang.org/api@v0.290.0/googleapi/googleapi.go:62`.
- `checkUploadCutoff` is a no-op — `drive.go:1356-1358`. Cutoff is unconstrained.
- Chunk-size help documents the memory trade-off: `"Making this larger will improve performance, but note that each chunk is buffered in memory one per transfer."` — `drive.go:530-534`. With defaults that's 4 transfers × 8 MiB = 32 MiB.
- Chunk size can be changed at runtime via `rclone backend set drive: -o chunk_size=…` — `drive.go:3387-3402`.

### 4.2 Simple upload
One `Files.Create(...).Media(in, ContentType(srcMimeType), googleapi.ChunkSize(0))` — `drive.go:2590-2599`. `ChunkSize(0)` **disables the Google client library's own chunking**, forcing a single-shot multipart upload. Crucially it uses `f.pacer.CallNoRetry` with the comment `// Don't retry, return a retry error instead` (`drive.go:2588-2590`) — because the `io.Reader` has been consumed and cannot be replayed, so the retry must happen at the higher sync level where the source can be reopened.

### 4.3 Resumable upload session
- Session init — `upload.go:53-101`. `POST/PATCH https://www.googleapis.com/upload/drive/v3/files` with `uploadType=resumable`, `alt=json`, `fields=partialFields`, `supportsAllDrives=true`, and `keepRevisionForever=true` when configured (`upload.go:54-70`).
- Method: `POST` for create; `PATCH .../{fileId}` plus `setModifiedDate=true` for update — `upload.go:65-69`.
- Headers: `Content-Type: application/json; charset=UTF-8` for the metadata body, `X-Upload-Content-Type: <contentType>`, `X-Upload-Content-Length: <size>` (omitted if size unknown) — `upload.go:87-91`.
- Session URL comes from the `Location` response header and is stored on the `resumableUpload` struct — `upload.go:102-110`.
- The session-init call itself **is** retried through the pacer (`upload.go:73`, `:97`) — safe, since the body is metadata JSON re-created each attempt (`upload.go:74-78`).

### 4.4 Chunk transfer and retry
- Chunk loop — `upload.go:167-214`. One reusable `buf := make([]byte, ChunkSize)` for the whole upload (`upload.go:171`).
- Known size: `readers.NewRepeatableLimitReaderBuffer(rx.Media, buf, reqSize)` — `upload.go:180-181`, comment `// If size known use repeatable reader for smoother bwlimit`. Unknown size: `readers.ReadFill` into the buffer, and on EOF set `ContentLength = start + n` so Google learns the total — comment `// Send the last chunk with the correct ContentLength otherwise Google doesn't know we've finished` (`upload.go:186-190`).
- `Content-Range: bytes <start>-<start+size-1>/<total>` where total is `*` if unknown — `upload.go:114-129`.
- Each chunk is wrapped in `rx.f.pacer.Call(...)` — `upload.go:199-208`. **Before each retry the chunk is `Seek(0, io.SeekStart)`'d** — `upload.go:133` — so the same bytes are re-sent from a memory buffer. This is the key design point:

  > **rclone does NOT query the session status to resume from a server-confirmed offset.** It buffers the whole chunk in memory and replays it from the start of that chunk. `start` only advances after the pacer loop returns success (`upload.go:213`). There is no `PUT`/`POST` with `Content-Range: bytes */total` status probe in the retry path — `makeRequest` *can* emit that form when `reqSize == 0` (`upload.go:124-126`), but nothing calls it that way.

- Success codes treated as terminal-OK: `308` (`statusResumeIncomplete`, `upload.go:30-33`), `201 Created`, `200 OK` — `upload.go:203-206`, which explicitly clears `again`/`err`.
- Transport error → synthetic status `599` (`upload.go:137`); JSON decode failure → `598` (`upload.go:159`).
- The final metadata is parsed out of the completing chunk's body — `upload.go:148-162`, with the comment explaining 201-on-create vs 200-on-update.
- If the loop ends without a parsed `rx.ret`, it returns `fserrors.RetryErrorf("Incomplete upload - retry, last error %d", StatusCode)` — `upload.go:235-237`, kicking the retry up to the sync layer.
- A long block comment at `upload.go:215-234` reproduces Google's guidance: retry all 5xx with exponential backoff, cap the retry count, and **`Handle 404 Not Found errors when doing resumable uploads by starting the entire upload over from the beginning.`** (`upload.go:233-234`) — note this is documented but not implemented; the 404 just falls through `shouldRetry` and fails.

### 4.5 Content-type and metadata on create
- `createFileInfo` builds `drive.File{Name, Parents:[dirID], ModifiedTime: modTime.Format(timeFormatOut)}` — `drive.go:2503-2518`.
- MIME type sniffed from the filename: `createInfo.MimeType = fs.MimeTypeFromName(remote)` — `drive.go:2580-2584`.
- `Fields(partialFields)` on the create so the response already contains everything needed to build the object without a follow-up GET — `drive.go:2593`, `upload.go:57`.
- Metadata (permissions/labels/owner) is applied via a deferred `updateMetadata(ctx, info)` callback returned by `fetchAndUpdateMetadata` *before* the upload starts and invoked after — `drive.go:2585-2588`, `:2610-2613`.

---

## 5. Listing

Single entry point `(*Fs).list` — `backend/drive/drive.go:1021-1201`.

### 5.1 Page size
- `--drive-list-chunk` default **1000**, help `"Size of listing chunk 100-1000, 0 to disable."` — `drive.go:503-507`. 1000 is the Drive API maximum.
- Applied as `list.PageSize(f.opt.ListChunk)` when `> 0` — `drive.go:1120-1122`. Same in `queryFn` (`drive.go:3660-3663`) and in `Changes.List` (`drive.go:3296-3299`).

### 5.2 Fields mask
```go
partialFields = "id,name,size,md5Checksum,sha1Checksum,sha256Checksum,trashed,explicitlyTrashed,modifiedTime,createdTime,mimeType,parents,webViewLink,shortcutDetails,exportLinks,resourceKey"
```
— `drive.go:75`

Wrapped for list calls as:
```go
fields := fmt.Sprintf("files(%s),nextPageToken,incompleteSearch", f.getFileFields(ctx))
```
— `drive.go:1135`

`getFileFields` conditionally appends only what's needed — `drive.go:1561-1580`: `,owners` (auth_owner_only), `,sharedWithMeTime` (use_shared_date), `,spaces` (skip_checksum_gphotos), `,quotaBytesUsed` (size_as_quota), `,<metadataFields>` (`--metadata`). **Nothing is requested speculatively.**

`incompleteSearch` is checked and logged as an ERROR — `drive.go:1150-1152`.

### 5.3 `q` query construction — `drive.go:1021-1113`
- `trashed=false` (or `true` for trashed-only) unless `includeAll` — `drive.go:1023-1029`. With `--drive-trashed-only` it becomes `(mimeType='<folder>' or trashed=true)` so folders are still traversable.
- Parents: OR-joined into one parenthesised clause, `'<id>' in parents` per dir — `drive.go:1034-1064`. This is the ListR batching mechanism.
- `sharedWithMe=true` / `starred=true` substitute for the parent clause at the root — `drive.go:1043-1052`, with the comment block at `drive.go:1031-1033` warning that `sharedWithMe` always returns parentless items so you must not also filter by parent.
- Name filter: `(name='<escaped>')`, plus `or name='<stem>'` variants for each export extension — `drive.go:1066-1088`. Escaping: `\` → `\\` then `'` → `\'`, with the comment `// Escaping the backslash isn't documented but seems to work` (`drive.go:1069-1071`).
- **`=` in Drive queries is case-insensitive**, so results are re-checked client side: `// Check the case of items is correct since the `=` operator is case insensitive.` — `drive.go:1183-1194`.
- Directories only: `(mimeType='<folder>' or mimeType='<shortcut>')` — `drive.go:1090-1092`. Files only: `mimeType!='<folder>'` — `drive.go:1093-1095`.
- Filter pushdown: `--min-age`/`--max-age` become `(modifiedTime >= 'ts' or mimeType = '<folder>')` server-side terms — `drive.go:1097-1111`. Server-side filtering to avoid paging through irrelevant files.
- Shared Drives: `SupportsAllDrives(true)`, `IncludeItemsFromAllDrives(true)`, plus `DriveId(...)` + `Corpora("drive")` — `drive.go:1122-1127`.
- Resource keys sent as the `X-Goog-Drive-Resource-Keys` header, value `id/key` comma-joined — `drive.go:1055-1060`, `:1132-1134`.
- The composed query string is stashed in `f.lastQuery` for unit tests — `drive.go:1119`.

### 5.4 ListR / fast-list — the big win
- `ListR` — `drive.go:2244-2355`. `listRGrouping = 50` parent IDs per request, `listRInputBuffer = 1000` channel depth — `drive.go:76-77`.
- `--checkers` (8) `listRRunner` goroutines — `drive.go:2295-2297`; each greedily drains up to `grouping` dir IDs from the channel **without blocking** (`default:` in the select) — `drive.go:2099-2115`.
- Dirs are sorted so parent lookup can use `sort.SearchStrings` — `drive.go:2116`, `:2140-2145`.
- Overflow handling when the input channel is full — `drive.go:2261-2277`, refilling only half the buffer at a time to avoid re-overflow (`drive.go:2317-2320`).
- Measured speedups in the docs: small folder 38s → 10s; large folder (10600 dirs / 39000 files) **22:05 → 58s** — `docs/content/drive.md:398-416`, and `"In tests, these batch requests were up to 20x faster than the regular method."` (`docs/content/drive.md:400`).
- **The Drive bug workaround** — `drive.go:2166-2200`: `"(A in parents) or (B in parents)" returns nothing sometimes. See #3114, #4289 and https://issuetracker.google.com/issues/149522397`. Detection: multi-dir list returned zero items ⇒ atomically set `grouping = 1`, requeue those dirs individually, and remember them in `listRempties`. If they all really were empty, grouping is restored to 50 and it logs `"Re-enabling ListR as previous detection was in error"` (`drive.go:2185-2200`). Gated behind `--drive-fast-list-bug-fix` (`drive.go:688+`).
- `ListP` (`drive.go:2023-2062`) is the non-recursive streaming path.
- Teamdrive sanity check: zero entries at the root triggers `teamDriveOK` to distinguish "empty" from "no access" — `drive.go:2055-2061`, `:2348-2354`.
- Incremental listing via `Changes.List` with a start page token and a tight field mask `"nextPageToken,newStartPageToken,changes(fileId,file(name,parents,mimeType))"` — `drive.go:3289-3312`; feeds `ChangeNotify` (`drive.go:3229+`).
- Directory ID cache (`lib/dircache`) is populated as a side-effect of every listing: `f.dirCache.Put(remote, item.Id)` in `itemToDirEntry` — `drive.go:2477-2482`. Cache is RWMutex-guarded and bidirectional (path↔ID) — `lib/dircache/dircache.go:91-118`.

---

## 6. Misc perf/correctness tricks worth stealing

### 6.1 Modified times
- Precision is **1 ms**: `func (f *Fs) Precision() time.Duration { return time.Millisecond }` — `drive.go:2811-2813`; docs `"Google drive stores modification times accurate to 1 ms."` — `docs/content/drive.md:419`.
- Written to the standard `modifiedTime` field, formatted with `timeFormatOut = "2006-01-02T15:04:05.000000000Z07:00"` (nanosecond digits, though Drive truncates to ms) — `drive.go:65`, used at `drive.go:2515`, `:4271`, `:4531`, `:4574`.
- Read with `timeFormatIn = time.RFC3339` — `drive.go:64`, `:4258-4265`. Parse failure returns `time.Now()` rather than erroring (`drive.go:4260-4263`) — arguably a bug we shouldn't copy.
- `--drive-use-created-date` / `--drive-use-shared-date` substitute `createdTime` / `sharedWithMeTime` — `drive.go:1534-1539`.
- `SetModTime` is a `Files.Update` with only `ModifiedTime` set — `drive.go:4268-4289`.
- Metadata layer exposes `mtime`/`btime` as "with mS accuracy", and notes `btime` `"is only writable on fresh uploads - it can't be written for updates"` — `backend/drive/metadata.go:27-35`.

### 6.2 `--drive-server-side-across-configs`
- Backend flag is **deprecated** in favour of the global `--server-side-across-configs` — `drive.go:590-601` (`Help: "Deprecated: use --server-side-across-configs instead."`).
- Feeds `fs.Features.ServerSideAcrossConfigs` — `drive.go:1419`.
- Consumed at `fs/operations/copy.go:147` (`c.dstFeatures.ServerSideAcrossConfigs || c.ci.ServerSideAcrossConfigs`) and `fs/operations/operations.go:465` for Move.
- Rationale from the help text: `"Note that this isn't enabled by default because it isn't easy to tell if it will work between any two configurations."` — `drive.go:597-600`.

### 6.3 Shortcuts (a whole subsystem)
- Composite ID encoding: `actualID \t shortcutID` — `drive.go:2357-2397`. Rationale comment: `"This is to make sure that we have decided in all use places what we are doing with the ID."` (`drive.go:2405-2408`).
- `resolveShortcut` — `drive.go:2414-2440`. A 404 on the target means a **dangling shortcut**: mime type is mangled to the synthetic `shortcutMimeTypeDangling` rather than erroring (`drive.go:2424-2431`, synthetic type defined `drive.go:62`).
- Assumption documented: `"we assume shortcuts can't point to shortcuts. Google drive web interface doesn't offer the option to create a shortcut to a shortcut. The documentation is silent on the issue."` — `drive.go:2410-2413`.
- Name/Parents/Trashed are taken from the *shortcut*, everything else from the target — `drive.go:2434-2437`.
- **Loop detection**: a folder shortcut pointing at its own ancestor would recurse forever; `shortcutLoop` walks up the dircache and drops it with an ERROR — `drive.go:2450-2463`, used at `drive.go:2471-2476`.
- Full behaviour matrix (list/update/move/copy/delete/setmodtime semantics for file vs folder shortcuts) — `docs/content/drive.md:452-509`.

### 6.4 Duplicate names
- `fs.Features.DuplicateFiles: true` — `drive.go:1415`. Drive genuinely allows two files with the same name in one folder.
- Deduplication is a separate user-invoked operation: `fs/operations/dedupe.go`, header comment `"dedupe - gets rid of identical files remotes which can have duplicate file names (drive, mega)"` (`fs/operations/dedupe.go:1`) and `"Only useful with Google Drive which can have duplicate file names."` (`fs/operations/dedupe.go:400-401`).
- Duplicate *directories* are fixed first via `MergeDirs` — `fs/operations/dedupe.go:413-422`; Drive's `MergeDirs` reparents children then trashes the empty source — `drive.go:2618-2670`, skipping shortcut dirs (`drive.go:2622-2630`).
- Docs: `"Sometimes, for no reason I've been able to track down, drive will duplicate a file that rclone uploads. ... Note that this isn't just a problem with rclone, even Google Photos on Android duplicates files on drive sometimes."` — `docs/content/drive.md:1920-1932`.
- Known-issue note: teamdrive + `--fast-list` can return stale listings, and `"Waiting a moderate period of time between attempts (estimated to be approximately 1 hour) and/or not using --fast-list both seem to be effective"` — `docs/content/drive.md:1934-1947`.
- The file-level FIXME at the very top of the backend is honest about the whole class of problems — `drive.go:4-8`:
  ```go
  // FIXME need to deal with some corner cases
  // * multiple files with the same name
  // * files can be in multiple directories
  // * can have directory loops
  // * files with / in name
  ```

### 6.5 `keepRevisionForever`
- `--drive-keep-revision-forever` default `false` — `drive.go:551-555`.
- Applied on create (`drive.go:2596`), update (`drive.go:4501`), copy (`drive.go:2895`), and as the `keepRevisionForever=true` query param on resumable sessions (`upload.go:60-62`).
- Docs on the default revision policy: `"They are deleted after 30 days or 100 revisions (whatever comes first). They do not count towards a user storage quota."` — `docs/content/drive.md:433-444`. Relevant to us: **without this flag, old snapshot blob versions are garbage-collected by Google for free**; with `--drive-size-as-quota` you can see the cost (`drive.go:556-574`).

### 6.6 HTTP/2 disabled by default
```go
Name:    "disable_http2",
Default: true,
Help: `Disable drive using http2.

There is currently an unsolved issue with the google drive backend and
HTTP/2.  HTTP/2 is therefore disabled by default for the drive backend
but can be re-enabled here.  When the issue is solved this flag will
be removed.

See: https://github.com/rclone/rclone/issues/3631
```
— `drive.go:602-615`. Implemented by nulling `TLSNextProto` on the transport — `drive.go:1280-1289`. **This is a real, load-bearing default; worth testing on our side.**

### 6.7 Other lessons-learned comments worth quoting
- Google Docs lose their mtime on server-side copy, requiring a 2s sleep then an explicit `SetModTime` — `drive.go:2905-2920`:
  ```go
  // Google docs aren't preserving their mod time after copy, so set them explicitly
  // See: https://github.com/rclone/rclone/issues/4517
  //
  // FIXME remove this when google fixes the problem!
  ...
  // A short sleep is needed here in order to make the
  // change effective, without it is ignored. This is
  // probably some eventual consistency nastiness.
  ```
- Descriptions behave differently for docs vs files on copy — `drive.go:2860-2872`: `"don't overwrite the description on copy for files / this should work for docs but it doesn't - it is probably a bug in Google Drive"`.
- Multi-parent files cannot be safely deleted: `if len(o.parents) > 1 { return errors.New("can't delete safely - has multiple parents") }` — `drive.go:4617-4622`.
- Range requests do not work on Google Docs exports — `drive.go:4435-4437`.
- Root folder ID lookup is a startup cost worth caching: `fs.Debugf(f, "'root_folder_id = %s' - save this in the config to speed up startup", rootID)` — `drive.go:1477`.
- Object URLs are constructed directly rather than fetched: `fmt.Sprintf("%sfiles/%s?alt=media", f.svc.BasePath, actualID(info.Id))` — `drive.go:1590`. No extra API call to get a download link.
- `--drive-v2-download-min-size` exists because the v2 `downloadUrl` was faster for large files — `drive.go:575-579`, `drive.go:4410-4425`.
- `rclone backend rescue` finds parentless orphans with the query `"'me' in owners and trashed=false"` filtered by `len(item.Parents) != 0` — `drive.go:4029-4056`. Drive really does orphan files; a repository GC needs to know this.
- Overall throughput reality check from the docs — `docs/content/drive.md:1887-1897`:
  > `Drive has quite a lot of rate limiting. This causes rclone to be limited to transferring about 2 files per second only. Individual files may be transferred much faster at 100s of MiB/s but lots of small files can take a long time.`
  > `Server side copies are also subject to a separate rate limit. If you see User rate limit exceeded errors, wait at least 24 hours and retry.`

---

## Numeric defaults summary

| Knob | Default | Source |
|---|---|---|
| pacer min sleep (Drive) | **100 ms** (⇒ 10 req/s sustained) | `backend/drive/drive.go:66`, `:580-584` |
| pacer burst (Drive) | **100** | `backend/drive/drive.go:67`, `:585-589` |
| pacer backoff ladder | 1s, 2s, 4s, 8s, 16s, 16s… + `rand(0..1s)` jitter | `lib/pacer/pacers.go:204-209` |
| pacer re-acceleration | instant — `limiter.Reserve().Delay()` on first success | `lib/pacer/pacers.go:201-203` |
| low-level retries per API call | **10** | `fs/config.go:175-179`, `fs/pacer.go:25` |
| high-level sync retries | 3 | `fs/config.go:165-169` |
| transfers / checkers | **4 / 8** | `fs/config.go:60-69` |
| max connections | 0 (unlimited) | `fs/config.go:554-558` |
| upload cutoff | **8 MiB** | `backend/drive/drive.go:519-523`, `:74` |
| chunk size | **8 MiB** (min 256 KiB, power of two) | `backend/drive/drive.go:73-74`, `:524-535`, `:1338-1346` |
| list page size | **1000** (API max) | `backend/drive/drive.go:503-507` |
| ListR parent grouping | **50** IDs per `q` | `backend/drive/drive.go:76` |
| ListR input buffer | 1000 | `backend/drive/drive.go:77` |
| batch size / flush interval | **N/A — no batching exists** | grep of `backend/drive/` |
| mtime precision | 1 ms | `backend/drive/drive.go:2811-2813` |
| HTTP/2 | disabled | `backend/drive/drive.go:602-615` |

---

# Section C — Drive API v3 live-doc verification (R3)


**Date of verification:** 2026-08-01. All facts below were fetched live from the URLs cited
(no reliance on training data). Where a Google doc page carries a "Last updated" stamp, it is
quoted so staleness can be judged. Anything not confirmable on a `developers.google.com` /
`support.google.com` / `knowledge.workspace.google.com` page is explicitly marked **UNVERIFIED**.

> ⚠️ **Headline:** the Drive API quota model changed on **2026-05-01**. It is no longer
> "queries per 100 seconds". It is now a **quota-unit pool** with a **1 TB/day per-project egress
> cap** and a **400,000,000 quota-unit/day billing threshold**. Any design document written from
> pre-2026 knowledge is wrong about quotas.

---

## 1. Batching status — **ALIVE and supported for Drive v3**

**Primary source:** <https://developers.google.com/workspace/drive/api/guides/performance>
(Last updated 2026-04-20 UTC)

The homogeneous per-API batch endpoint is documented, current, and carries **no deprecation
notice**. The page's own worked example posts to:

```
POST https://www.googleapis.com/batch/drive/v3
```

**Independently confirmed against the live discovery document**
(<https://www.googleapis.com/discovery/v1/apis/drive/v3/rest>, `revision: 20260728`):

```
batchPath: batch/drive/v3
rootUrl:   https://www.googleapis.com/
```

→ full endpoint `https://www.googleapis.com/batch/drive/v3`. The discovery doc is machine-readable
truth, so this is as authoritative as it gets.

### Documented rules (verbatim quotes)

| Rule | Exact wording |
|---|---|
| Homogeneity | "All the inner requests must go to the same Google API." |
| Max calls | "You're limited to 100 calls in a single batch request. If you must make more calls than that, use multiple batch requests." |
| Overflow | "Batch requests with more than 100 calls might cause an error." |
| Inner URL length | "There's an 8,000 character limit on the length of the URL for each inner request." |
| No media | "Google Drive doesn't support batch operations for media, either for upload or download, or for exporting files." |
| **Quota accounting** | **"A set of _n_ requests batched together counts toward your usage limit as _n_ requests, not as one request. The batch request is separated into a set of requests before processing."** |
| Ordering | "The server might perform your calls in any order. Don't count on their being executed in the order in which you specified them." |
| Transport | `multipart/mixed`; each part `Content-Type: application/http`; inner requests must use **path-only** URLs ("full URLs are not allowed in batch requests"). |
| Auth | Outer `Authorization` header applies to all inner calls unless an inner call overrides it. |
| Response | `multipart/mixed`, parts **in the same order as the requests**, `Content-ID: response-N` matching request `content-id: N`. |

### Global batch endpoint (`www.googleapis.com/batch`)

Dead since 2018-2019. Google's announcement
(<https://developers.googleblog.com/discontinuing-support-for-json-rpc-and-global-http-batch-endpoints/>)
states that **heterogeneous** batching is gone but **"Homogenous batching will still be supported
but through API specific batch endpoints"**, with the migration being "change the endpoint from
`www.googleapis.com/batch` to `www.googleapis.com/batch/<api>/<version>`". That is exactly the
endpoint Drive still documents today.

### Announced deprecation of homogeneous batching?

**None found.** Checked:
- the performance/batching guide itself (no Warning/Deprecated callout of any kind);
- the Drive API release notes <https://developers.google.com/workspace/drive/release-notes>
  back through 2025-07 — deprecations listed there are `enforceExpansiveAccess` (2026-02-25) only;
- the live discovery doc still advertises `batchPath`.

### Design implication for the delete path

Batching gives you **HTTP-connection savings only, not quota savings**. A batch of 100
`files.delete` calls costs the same quota as 100 individual deletes. Under the new quota-unit
model a delete is an "edit"-class operation (see §2), so **100 deletes ≈ 5,000 quota units**
either way.

Batching is still worth it if: (a) you are connection/RTT-bound rather than quota-bound, and
(b) you can tolerate unordered execution and per-part error handling. Parallel singles are
simpler, give you per-request retry/backoff for free, and cost identically against quota.
**Recommendation: batching is a legitimate option (not deprecated), but it is not a quota
optimization — choose it purely on RTT grounds.** Note also you cannot batch uploads or
downloads, so batching only ever helps metadata/delete paths.

---

## 2. Quotas — **new model as of 2026-05-01**

**Primary source:** <https://developers.google.com/workspace/drive/api/guides/limits>
(Last updated **2026-07-31 UTC** — i.e. yesterday relative to this report)

### The banner at the top of the page (verbatim)

> "As of May 1, 2026, the usage limits for this API were updated. Google Cloud projects that made
> any use of this API between November 2025 and April 2026 will continue with their previously set
> usage quotas. Cloud projects created on or after May 1, 2026 are subject to the new API quotas."

Announcement context:
- <https://developers.google.com/workspace/tools-safety> ("Google Workspace standardized model for agent tools and APIs")
- <https://workspaceupdates.googleblog.com/2026/05/agent-tools-and-security-updates-for-workspace-developers.html>
- Drive release note, 2026-05-01: *"To help align resource usage in the Google Drive API, usage
  quotas will change to be measured in quota units instead of the number of requests. Additionally,
  Google Workspace users can now only egress 1 TB per day."*

### Current quota table (verbatim)

| Usage limit type | Limit |
|---|---|
| Per minute per project | **1,000,000 quota units** |
| Per minute per user per project | **325,000 quota units** |
| Per day per project | **1 TB** |

Definitions given on the page:
- *"Per minute per project: This is the number of quota units your Google Cloud project can use in one minute."*
- *"Per minute per user per project: This is the number of quota units any one particular user can use in your Cloud project. This limit aims at helping you ensure a fair distribution of usage among your users."*
- *"Per day per project: This defines the maximum number of bytes your Google Cloud project can egress within a 24-hour period before charges apply."*

### Daily billing threshold (separate table, verbatim)

| Threshold limit type | Limit |
|---|---|
| Per day per project | **400,000,000 quota units** |

> "You cannot request an increase on this daily threshold limit."
> "Usage under this threshold doesn't incur extra charges and your Google Cloud account isn't billed. Full billing details will be shared later in 2026 with at least 90 days' notice before any changes take effect."

### Per-method quota unit cost (verbatim)

| Action | Quota units |
|---|---|
| Read items, such as `files.get` | 5 |
| List items, such as `files.list` | 100 |
| Download items, such as `files.download` | 200 |
| Edit items, such as `files.update` | 50 |
| Other actions, such as `files.generateIds` | 5 |

**Derived per-user-per-minute ceilings** (325,000 QU/min ÷ cost) — arithmetic ours, inputs from the doc:

| Operation class | Cost | Per user/min | Per user/sec |
|---|---|---|---|
| Read (`files.get` metadata) | 5 | 65,000 | ~1,083 |
| Edit (`files.create`/`update`/`delete` — assumed edit-class) | 50 | 6,500 | ~108 |
| List (`files.list` page) | 100 | 3,250 | ~54 |
| Download (`files.download` / `alt=media`) | 200 | 1,625 | ~27 |

Per-project ceiling is ~3× those numbers (1,000,000 QU/min).

> **UNVERIFIED:** the doc's cost table does not name `files.create` or `files.delete` explicitly.
> They are almost certainly "edit"-class (50) but the page only says "Edit items, such as
> `files.update`". Treat 50 as the working assumption and confirm empirically.

### Other constraints from the same page (verbatim)

- "Notifications delivered to the address specified when opening a notification channel don't count against your quota limits. However, calls to the `changes.watch`, `channels.stop`, and `files.watch` methods do count against your quota."
- **"Provided you stay within the per-minute quotas, there's no limit to the number of requests you can make per day."** (i.e. the only daily caps are the 1 TB egress and the 400M-QU billing threshold.)

### Are quotas raisable?

Yes for the per-minute quotas, no for the daily billing threshold. Verbatim:

> "Depending on your project's resource usage, you might want to request a quota adjustment.
> **API calls by a service account are considered to be using a single account.** Applying for an
> adjusted quota doesn't guarantee approval. Quota adjustment requests that would significantly
> increase the quota value can take longer to be approved."

Requests go through <https://console.cloud.google.com/iam-admin/quotas>. Pricing section:
"All standard use of the Google Drive API is available at no additional cost. Exceeding the quota
request limits is planned to incur charges to your Google Cloud billing account later in 2026."

### What key does Google bucket per-user quota under?

Two independent axes, both confirmed:

1. **Project axis** — quota is charged to the **Google Cloud project that owns the OAuth client ID**.
   Drive's own table says "per project" and "per user *per project*". Google Cloud quota docs:
   *"Quotas generally apply at the Google Cloud project level. Your use of a resource in one project
   doesn't affect your available quota in another project."*
   (<https://docs.cloud.google.com/docs/quotas/overview>)

2. **User axis** — the per-user bucket key is, by default, **the authenticated principal**
   (the Google account behind the OAuth token), *not* the client ID. Verbatim from
   <https://docs.cloud.google.com/apis/docs/system-parameters>:

   > `quotaUser` — "A pseudo user identifier for charging per-user quotas. If not specified, the
   > authenticated principal is used. If there is no authenticated principal, the client IP address
   > will be used. When specified, a valid API key with service restrictions must be used to
   > identify the quota project. Otherwise, this parameter is ignored."

   Also available as the `X-Goog-Quota-User` header
   (<https://docs.cloud.google.com/apis/docs/capping-api-usage>). Drive's own error docs recommend
   it: for `userRateLimitExceeded` and `sharingRateLimitExceeded`, *"If one user is making numerous
   requests on behalf of many users of a Google Workspace account, consider a service account with
   domain-wide delegation using the `quotaUser` parameter."*

**Therefore — dedicated OAuth client vs shared client (rclone-style):**

- The **per-minute-per-user** bucket (325,000 QU) is keyed on the *end user's Google account*, so it
  is **not** diluted by other people using the same client ID.
- The **per-minute-per-project** bucket (1,000,000 QU), the **1 TB/day egress** cap, and the
  **400M QU/day billing threshold** are keyed on the *Cloud project owning the client ID*, so they
  **are shared across every user of a shared OAuth client**. A popular shared client ID will hit the
  project ceiling and every user gets `rateLimitExceeded` / `dailyLimitExceeded`.
- This is exactly why rclone is retiring its shared client. From <https://rclone.org/drive/>:
  > "When you use rclone with Google drive in its default configuration you are using rclone's
  > `client_id`. This is shared between all the rclone users. **This shared `client_id` is being
  > retired and will stop working during 2026.** To avoid interruption you must create and use your
  > own `client_id`, so creating one is now required rather than merely recommended."
  > "There is a global rate limit on the number of queries per second that each `client_id` can do
  > set by Google."

  (The specific "10 transactions per second" figure rclone quotes is **UNVERIFIED** — it does not
  appear on any Google page and predates the 2026 quota-unit model.)

**Design implication:** ship a Kopia-owned OAuth client ID *and* make it overridable by the user
(`--drive-client-id` / config field), the way rclone does. Users running large fleets should be
told to create their own Cloud project so the 1 TB/day egress and 1M QU/min project buckets are
theirs alone. The 1 TB/day project egress cap is a **hard restore-throughput ceiling** and is new.

> **UNVERIFIED — legacy projects:** what "previously set usage quotas" means numerically for a
> project that used the API between Nov 2025 and Apr 2026. Secondary sources disagree
> (12,000 queries/60s vs 20,000 calls/100s). The current Google page does not state the legacy
> numbers. **The Google Cloud console Quotas page for the project is the only authority.** Our code
> should therefore *not* hard-code any assumed QPS; it should be adaptive (backoff-driven) with a
> user-tunable ceiling.

---

## 3. Upload limits

**Sources:**
- <https://developers.google.com/workspace/drive/api/guides/limits> (Last updated 2026-07-31)
- <https://developers.google.com/workspace/drive/api/guides/manage-uploads> (Last updated 2026-04-20)
- <https://knowledge.workspace.google.com/admin/drive/storage-and-upload-limits-for-google-workspace>

### 750 GB/day — confirmed, scope = per user

Verbatim from the Drive API limits page ("Additional constraints"):

> "Google Workspace users can only upload 750 GB per day between My Drive and all shared drives;
> this limit also applies to copies."
> "Users who reach the 750 GB limit or upload a file larger than 750 GB can't upload or copy
> additional files until 24 hours have passed."

Workspace admin knowledge base, verbatim:

> "Each user can upload and copy 750 GB to Drive within 24 hours."
> "Users who reach the 750-GB limit can't upload or copy more files until the limit refreshes.
> The limit refreshes within 24 hours."

Also from <https://support.google.com/a/users/answer/7338880>: "Each user can upload and copy
750 GB to Drive within 24 hours." — i.e. the cap follows the **user**, and it is a *combined*
budget across My Drive **and all shared drives** the user writes to. Writing into a Shared Drive
does **not** get you a fresh 750 GB.

**Service accounts:** service accounts are a distinct principal and have their own 750 GB budget in
practice, but Google's docs do not say this in so many words — what they *do* say (verbatim, from
both the error guide and the folder-limits guide) is:

> "Note that service accounts can't own any files. Instead, they must upload files and folders into
> shared drives, or use OAuth 2.0 to upload items on behalf of a human user."

> **UNVERIFIED:** that a service account gets its own independent 750 GB/day budget. No Google page
> states it. What *is* documented is that a service account has no storage quota of its own and must
> write into a Shared Drive (whose storage is billed to the Workspace domain pool), and that for
> *API quota* purposes "API calls by a service account are considered to be using a single account."

### Error reason when 750 GB is exceeded

> **UNVERIFIED / no dedicated reason string.** The `handle-errors` page has **no** entry for a
> daily-upload-cap error. The limits page only says generically: "If you exceed a quota, you'll
> receive a `403: User rate limit exceeded` HTTP status code response. Additional rate limit checks
> on the Drive backend might also generate a `429: Rate limit exceeded` response."
>
> In practice the 750 GB cap surfaces as **403 `userRateLimitExceeded`** ("User Rate Limit
> Exceeded") — this matches the documented description of `userRateLimitExceeded`: *"This error
> occurs when the per-user limit has been reached. This might be a limit from the Google Cloud
> console **or a limit from the Drive backend**."* That last clause is the doc's own hook for
> backend caps like 750 GB.
>
> **Design implication:** `userRateLimitExceeded` is ambiguous — it can mean "slow down for a few
> seconds" *or* "you are done uploading for 24 hours". Exponential backoff will not clear the
> latter. Our error taxonomy should treat repeated `userRateLimitExceeded` on *upload* paths that
> survives several minutes of backoff as a probable daily-cap condition and surface a distinct,
> actionable message rather than retrying for 24 hours. This is a real footgun.

### File size

- Max upload: **5 TB**. Verbatim: "The maximum file size that users can upload is 5 TB; only the
  first file that breaks the limit completes uploading."
- Max copy: **750 GB**. "The maximum file size that users can copy is 750 GB."
- (Irrelevant for Kopia — blobs are MBs — but confirms no low ceiling.)

### Upload types and thresholds (verbatim from `manage-uploads`)

| Type | `uploadType` | Google's stated threshold |
|---|---|---|
| Simple | `media` | "Use this upload type to transfer a small media file (**5 MB or less**) **without supplying metadata**." |
| Multipart | `multipart` | "Use this upload type to transfer a small file (**5 MB or less**) **along with metadata** that describes the file, in a single request." |
| Resumable | `resumable` | "Use this upload type for **large files (greater than 5 MB)** and when there's a high chance of network interruption… **Resumable uploads are also a good choice for most applications because they also work for small files at a minimal cost of one additional HTTP request per upload.**" |

Note the bolded clause: Google explicitly blesses resumable-for-everything. Since Kopia needs to
set metadata (name, parent, appProperties) on every blob, **multipart is the natural choice for
blobs ≤ 5 MB and resumable above** — but a uniform resumable path is documented as acceptable at
the cost of one extra RTT.

Upload endpoint (from the doc's examples): `https://www.googleapis.com/upload/drive/v3/files?uploadType=…`

### Resumable session lifetime

Stated **three times** on the page, verbatim:
> "A resumable session URI expires after one week."
> "Upload sessions also expire after one week of inactivity."

### Chunk size

Verbatim:
> "Create chunks in multiples of 256 KB (256 x 1024 bytes) in size, except for the final chunk that
> completes the upload. **Keep the chunk size as large as possible so that the upload is efficient.**"

Headers required per chunk: `Content-Length` (bytes in this chunk) and `Content-Range`
(e.g. `Content-Range: bytes 0-524287/2000000`).

Google also documents a **single-request resumable upload** and says to prefer it:
> "Upload content in a single request: Use this approach when the file can be uploaded in one
> request… **This approach is best because it requires fewer requests and results in better
> performance.**"

For Kopia blob sizes (typically ≤ ~20 MB) this means: create session, PUT the whole thing, done.
Chunking is only for progress reporting or fixed request-time limits.

### Resume protocol (verbatim behaviours)

- Query status: empty `PUT` to the session URI with `Content-Range: */TOTAL` (or `*/*` if size unknown).
- `200 OK` / `201 Created` → upload already complete, no action.
- `308 Resume Incomplete` → continue; read the `Range` response header. "a `Range` header of
  `bytes=0-42` indicates that the first 43 bytes of the file were received and that the next chunk
  to upload would start with byte 44." **"If the response doesn't have a `Range` header, no bytes
  have been received."**
- `404 Not Found` → "the upload session has expired and the upload must be restarted from the beginning."
- **"Don't assume that the server received all bytes sent in the previous request."**

### Upload error-handling guidance (verbatim)

> - "For `5xx` errors, resume or retry uploads that fail due to connection interruptions."
> - "For `403` rate limit errors, retry the upload."
> - "**For any `4xx` errors (including `403`) during a resumable upload, restart the upload.** These
>   errors indicate the upload session has expired and must be restarted by requesting a new session URI."

Note the internal tension between bullets 2 and 3 — Google says retry on 403-rate-limit but also
says any 4xx during a resumable upload means restart the session. Practical reading: retry the
*operation*, but be prepared to re-acquire a session URI rather than resuming the old one.

---

## 4. Shared Drive / folder limits

**Sources:**
- <https://developers.google.com/workspace/drive/api/guides/manage-shareddrives#folder-limits> (Last updated 2026-04-20)
- <https://developers.google.com/workspace/drive/api/guides/folder#folder-limits> (Last updated 2026-04-20)
- <https://support.google.com/a/users/answer/7338880>

| Limit | Value | Scope | Error reason | Source |
|---|---|---|---|---|
| Shared Drive item cap | **500,000** items ("including files, folders, and shortcuts") | per Shared Drive | `teamDriveFileLimitExceeded` (403) | manage-shareddrives |
| Shared Drive nesting | **100 levels** of nested folders | per Shared Drive | `teamDriveHierarchyTooDeep` (403) | manage-shareddrives |
| **My Drive per-folder item cap** | **500,000** items | per *folder*; **"This limit doesn't apply to the root folder of My Drive."** | `numChildrenInNonRootLimitExceeded` (403) | folder guide |
| My Drive nesting | **100 levels** | per My Drive | `myDriveHierarchyDepthLimitExceeded` (403) | folder guide |
| **Per-account item cap** | **500 million** items created by that account | per user account | `activeItemCreationLimitExceeded` (403) | folder guide |
| Shared Drive parents | exactly one parent per item | — | `teamDrivesParentLimit` (403) | handle-errors |
| Shared Drive members | 100 groups / 600 members / 50,000 individuals | per Shared Drive | — | support.google.com/a/users/answer/7338880 |
| Direct group shares on a file | max 100 groups | per file | — | support.google.com/a/users/answer/7338880 |
| Shared drives in left nav | 1,000 listed (rest reachable by URL/search) | UI only | — | support.google.com/a/users/answer/7338880 |

### Verbatim, the ones that matter

Shared Drive item cap:
> "Each user's shared drive has a limit of 500,000 items, including files, folders, and shortcuts.
> When the limit is reached, the shared drive can no longer accept items. To resume receiving files,
> users must permanently delete items from the shared drive. **Note that items in the trash count
> toward the limit, but permanently-deleted items don't.**"

My Drive folder-item cap (**yes, 500k children per folder is current and documented**):
> "Each folder in a user's My Drive has a limit of 500,000 items. **This limit doesn't apply to the
> root folder of My Drive.** Items that count toward this limit are: Folders; Files. All file types,
> regardless of file ownership; Shortcuts…; Third-party shortcuts…"

And from the error guide:
> "There's a 500,000 item limit for folders, files, and shortcuts directly in a folder. **Items
> nested in subfolders don't count against this 500,000 item limit.**"

Per-account 500 million cap — what counts (verbatim):
> Counts: "Items created or uploaded by the user in Drive; Items created by the user but now owned by
> someone else; **Items in the trash**; Shortcuts; Third-party shortcuts"
> Doesn't count: "Permanently-deleted items; Items shared with the user but owned by someone else;
> Items owned by the user but created by someone else"

Folder-depth semantics (verbatim):
> "A child folder cannot be stored under a folder that's more than 99 levels deep. **This limitation
> only applies to child folders. A child file with a MIME type other than
> `application/vnd.google-apps.folder` is exempt from this limitation.**"
> "Note: There's no `files` field in the Google Drive API that can return the depth of a folder. To
> determine the folder depth, you must manually ascend the folder hierarchy."

### Implications for a backup tool storing hundreds of thousands of blobs

1. **Trash counts.** Both the Shared Drive 500k cap and the 500M account cap count trashed items.
   A backup tool that "deletes" blobs must use `files.delete` (permanent) or explicitly empty trash;
   `trashed=true` updates will silently consume the cap. This is a correctness requirement, not an
   optimization.
2. **Shard the blob namespace.** With a 500k-children-per-folder cap and Kopia's blob counts easily
   reaching six figures, a flat blob folder is a real risk. Kopia's existing 2-hex-char shard prefix
   pattern (as used by the filesystem/other providers) keeps every folder ≤ ~a few thousand items
   and is the right layout here. Nesting depth is a non-issue (100 levels available, we need 2-3).
3. **Shared Drive 500k is a whole-drive cap, not per-folder** — sharding does *not* help there. A
   repository exceeding ~500k total objects cannot live in a single Shared Drive. My Drive has no
   equivalent whole-account cap below 500 million, so My Drive scales further for a single
   repository. Worth documenting for users.

### Documented listing-performance limits

- `files.list` **`pageSize` maximum is 1000** (default 100), verbatim from the live discovery doc:
  *"The maximum number of files to return… If unspecified, at most 100 files will be returned for
  shared drives, and the entire list of files for non-shared drives. The maximum value is 1000;
  values above 1000 will be coerced to 1000."* Same for `changes.list`.
- Each `files.list` page costs **100 quota units**. Listing 500,000 blobs at 1000/page = 500 pages =
  50,000 QU, which is trivial against 325,000 QU/min — **listing is not quota-bound, it is
  latency-bound.** Use `pageSize=1000` and a tight `fields` mask (`nextPageToken,files(id,name,size,md5Checksum,modifiedTime)`).
- `corpora` guidance, verbatim: **"Prefer `user` or `drive` to `allDrives` for efficiency."**
- Performance guide recommends gzip (`Accept-Encoding: gzip` **and** "gzip" in the User-Agent) and
  the `fields` parameter; note that "Methods for `about`, `comments`, and `replies` resources
  require the `fields` parameter."

> **UNVERIFIED:** Google documents no *quantitative* listing-performance degradation for large
> folders. There is no published "folders over N children list slowly" statement. The only hard
> documented numbers are the 500k cap and the 1000 pageSize ceiling.

---

## 5. Error reasons — complete current set

**Source:** <https://developers.google.com/workspace/drive/api/guides/handle-errors>
**Last updated 2026-04-20 UTC.** The list below is the **complete** set of reason strings on that
page, transcribed in full. This is the contract for our taxonomy.

### HTTP status summary (verbatim)

| Code | Description |
|---|---|
| 200 - OK | "The request is successful (this is the standard response for successful HTTP requests)." |
| 400 - Bad Request | "The request cannot be fulfilled due to a client error in the request." |
| 401 - Unauthorized | "The request contains invalid credentials." |
| 403 - Forbidden | "The request was received and understood, but the user doesn't have permission to perform the request." |
| 404 - Not Found | "The requested page couldn't be found." |
| 429 - Too Many Requests | "Too many requests to the API." |
| 500, 502, 503, 504 - Server Errors | "Unexpected error arises while processing the request." |

### 400 — Bad Request

| `reason` | `domain` | Documented `message` | Notes / fix |
|---|---|---|---|
| `badRequest` | `global` | e.g. "Sorting is not supported for queries with fullText terms. Results are always in descending relevance order." | Caused by: missing required field/param; invalid value or field combination; **"You tried to add a duplicate parent to a Drive file."**; **"You tried to add a parent that would create a cycle in the directory graph."** Doc note: *"This list isn't exhaustive, other cases can cause a 400 error."* **Non-retryable.** |
| `illegalKeepForeverModification` | `global` | "Bad Request. Cannot update a revision to false that is marked as keepForever." | Revisions only. Non-retryable. |
| `invalidSharingRequest` | `global` | "Bad Request. User message: \"Sorry, the items were successfully shared but emails could not be sent to email@domain.com.\"" **or** "Bad Request. User message: \"ACL change not allowed.\"" | Two distinct causes behind one reason; must inspect `message`. Non-retryable. |

### 401 — Unauthorized

| `reason` | `domain` | Documented `message` | Notes / fix |
|---|---|---|---|
| `authError` | `global` | "Invalid Credentials" (`locationType: header`, `location: Authorization`) | *"the access token that you're using is either expired or invalid. This error can also be caused by **missing authorization for the requested scopes**."* Fix: refresh via refresh token; if that fails, re-run OAuth. **Retry once after refresh.** |
| `fileNotDownloadable` | `global` | "Only files with binary content can be downloaded. Use Export with Docs Editors files." | ⚠️ **The doc's own JSON sample for this reason shows `"code": 403`, not 401, even though it is listed under the 401 section.** Do not key on status alone. |

### 403 — Forbidden (the big one)

| `reason` | `domain` | Documented `message` | Relevant to us? |
|---|---|---|---|
| `activeItemCreationLimitExceeded` | `global` | "This account has exceeded the creation limit of 500 million items. To create more items, permanently delete some items." | Yes (hard fail) |
| `appNotAuthorizedToFile` | `global` | "The user has not granted the app {appId} {verb} access to the file {fileId}." | **Yes — this is the `drive.file` cross-user failure mode.** |
| `cannotModifyInheritedTeamDrivePermission` | `global` | "Cannot update or delete an inherited permission on a shared drive item." | No |
| `dailyLimitExceeded` | **`usageLimits`** | "Daily Limit Exceeded" | Yes. Doc: *"This error appears when the application's owner has set a quota limit to cap usage of a particular resource."* Fix is to remove the "Queries per day" cap. **Not retryable within the day.** |
| `domainPolicy` | `global` | "The domain administrators have disabled Drive apps." | Yes (hard fail, actionable message) |
| `download_restricted_for_revision` | `global` | "This revision cannot be downloaded by the authenticated user." | ⚠️ **Note the snake_case reason string** — the doc's section *heading* is `downloadRestrictedForRevision` but the JSON `reason` value is `download_restricted_for_revision`. Match on the snake_case form. |
| `fileNotExportable` | `global` | "Google Vids does not support files.export. Use files.download with Vids files." | No |
| `fileOwnerNotMemberOfTeamDrive` | `global` | "Cannot move a file into a shared drive as a writer when the owner of the file is not a member of that shared drive." | Maybe (Shared Drive setup) |
| `fileWriterTeamDriveMoveInDisabled` | `global` | "The domain administrator has not allowed writers to move items into a shared drive." | Maybe |
| `insufficientFilePermissions` | `global` | "The user does not have sufficient permissions for file {fileId}." | **Yes** |
| `myDriveHierarchyDepthLimitExceeded` | `global` | "Your My Drive can't contain more than 100 levels of folders. For details, see …#nested-folder-levels." | Unlikely (we use 2-3 levels) |
| `numChildrenInNonRootLimitExceeded` | `global` | "The limit for this folder's number of children (files and folders) has been exceeded." | **Yes — 500k/folder** |
| `rateLimitExceeded` | **`usageLimits`** | "Rate Limit Exceeded" | **Yes.** Doc: *"the project's rate limit has been reached."* Fixes: raise per-user quota / batch requests / exponential backoff. **Retryable.** |
| `sharingRateLimitExceeded` | `global` | "Rate limit exceeded. User message: \"These item(s) could not be shared because a rate limit was exceeded: filename\"" | Only if we call `permissions.*` |
| `storageQuotaExceeded` | `global` | "The user's Drive storage quota has been exceeded." | **Yes (hard fail).** Doc note: *"Service accounts don't have storage quota and can't own any files. Instead, they must upload files and folders into shared drives, or use OAuth 2.0 to upload items on behalf of a human user."* |
| `teamDriveFileLimitExceeded` | `global` | "The file limit for this shared drive has been exceeded." | **Yes — 500k/Shared Drive** |
| `teamDriveHierarchyTooDeep` | `global` | "The shared drive hierarchy depth will exceed the limit." | Unlikely |
| `teamDriveMembershipRequired` | `global` | "The attempted action requires shared drive membership." | **Yes** |
| `teamDrivesFolderMoveInNotSupported` | `global` | "Moving folders into shared drives is not supported." | No |
| `teamDrivesParentLimit` | `global` | "A shared drive item must have exactly one parent." | Maybe |
| `UrlLeaseLimitExceeded` | **`usageLimits`** | "Too many pending uploads for this snapshot. Please finish or cancel some before creating more." | No (Google Play games). ⚠️ **Capital `U`** — inconsistent with every other reason string. |
| `userRateLimitExceeded` | **`usageLimits`** | "User Rate Limit Exceeded" | **Yes — the most important one.** Doc: *"the per-user limit has been reached. This might be a limit from the Google Cloud console **or a limit from the Drive backend**."* Fixes: raise per-user quota / `quotaUser` with DWD service account / exponential backoff. |

### 404 — Not Found

| `reason` | `domain` | Documented `message` | Notes |
|---|---|---|---|
| `notFound` | `global` | inner: "File not found {fileId}"; outer: "File not found: {fileId}" | ⚠️ **The reason string is `notFound`, NOT `fileNotFound`.** Doc: *"the user doesn't have read access to a file, or the file doesn't exist"* — 404 is **overloaded** and does not distinguish absence from permission denial. First fix listed: *"If the file is located in a shared drive, and you're using the `files.get` method, make sure the `supportsAllDrives` query parameter is set to `true`."* |

### 429 — Too Many Requests

| `reason` | `domain` | Documented `message` |
|---|---|---|
| `rateLimitExceeded` | `usageLimits` | "Rate Limit Exceeded" (code 429) |

Verbatim: *"This error occurs when the user has sent too many requests in a given amount of time…
To fix this error, use exponential backoff to retry the request."*

⚠️ **`rateLimitExceeded` appears under BOTH 403 and 429 with the same domain and message.**
Any taxonomy keyed on `reason` alone will conflate a project-rate-limit 403 with a 429. Key on
`(code, reason)`.

### 5xx

Verbatim list: **500 Backend error / 502 Bad Gateway / 503 Service Unavailable / 504 Gateway
Timeout**. *"To fix this error, use exponential backoff to retry the request."* Causes given:
*"a request's timing overlapping with another request or a request for an unsupported action."*

### Google's currently-recommended backoff (verbatim, from the limits page)

> "For all time-based errors (maximum of N requests per X minutes), we recommend your code catches
> the exception and uses a **truncated exponential backoff**…"
>
> - "Make a request to Google Drive API."
> - "If the request fails, wait 1 + `random_number_milliseconds` and retry the request."
> - "If the request fails, wait 2 + `random_number_milliseconds` and retry the request."
> - "If the request fails, wait 4 + `random_number_milliseconds` and retry the request."
> - "And so on, up to a `maximum_backoff` time."
> - "Continue waiting and retrying up to some maximum number of retries, but don't increase the wait period between retries."
>
> "The wait time is `min(((2^n)+random_number_milliseconds), maximum_backoff)`, with `n` incremented
> by 1 for each iteration (request)."
>
> "`random_number_milliseconds` is a random number of milliseconds **less than or equal to 1,000**.
> This helps to avoid cases in which many clients are synchronized by some situation and all retry
> at once, sending requests in synchronized waves. The value of `random_number_milliseconds` is
> recalculated after each retry request."
>
> "`maximum_backoff` is **typically 32 or 64 seconds**. The appropriate value depends on the use case."
>
> "At some point, clients should be prevented from retrying indefinitely."

### Suggested taxonomy mapping for Kopia

| Bucket | Match on | Behaviour |
|---|---|---|
| **Retryable-throttle** | `429/*`, `403/rateLimitExceeded`, `403/userRateLimitExceeded`, `403/sharingRateLimitExceeded`, all 5xx | Truncated exponential backoff, jitter ≤1000 ms, cap 32-64 s. Escalate to "probable daily cap" if `userRateLimitExceeded` persists past N minutes on an upload path. |
| **Auth-refresh** | `401/authError` | Refresh token, retry once; then fail with re-consent instruction. |
| **Not-found** | `404/notFound` | Map to `blob.ErrBlobNotFound`. **Ensure `supportsAllDrives=true` on every call first**, otherwise Shared Drive blobs masquerade as missing. |
| **Permission / app-authorization** | `403/appNotAuthorizedToFile`, `403/insufficientFilePermissions`, `403/teamDriveMembershipRequired`, `403/domainPolicy` | Non-retryable; distinct actionable messages. `appNotAuthorizedToFile` specifically means "re-run the picker/consent flow for this user". |
| **Capacity exhausted** | `403/storageQuotaExceeded`, `403/teamDriveFileLimitExceeded`, `403/numChildrenInNonRootLimitExceeded`, `403/activeItemCreationLimitExceeded`, `403/dailyLimitExceeded` | Non-retryable; each needs its own message. Sharding fixes only `numChildrenInNonRootLimitExceeded`. |
| **Bad request** | `400/*` | Non-retryable bug in our code. Note `badRequest` covers duplicate-parent and parent-cycle — relevant if we ever add parents. |

---

## 6. `drive.file` scope semantics, and the desktop OAuth story

### Current documented definition

**Source:** <https://developers.google.com/workspace/drive/api/guides/api-specific-auth>

Verbatim scope description (this is the *current* wording — note it has moved on from the older
"files created or opened by the app" phrasing to explicitly name the Picker):

> `https://www.googleapis.com/auth/drive.file` — **"Create new Drive files, or modify existing
> files, that you open with an app or that the user shares with an app while using the Google
> Picker API or the app's file picker."**

Classified **non-sensitive** — "These scopes provide the smallest scope of authorization and only
require basic OAuth App Verification." Page also states: *"Whenever possible, use non-sensitive
scopes as they grant per-file access and narrow access to specific features needed by an app."*

Live discovery doc user-facing string: *"See, edit, create, and delete only the specific Google
Drive files you use with this app."*

For contrast, `https://www.googleapis.com/auth/drive` is **restricted** ("View and manage all your
Drive files") and requires restricted-scope verification + a third-party security assessment if
restricted-scope data is stored or transmitted on servers. Kopia does qualify for the
"**Backup and sync**" category the page lists as eligible for restricted scopes:

> "Backup and sync: Platform-specific and web apps that provide local sync or automatic backup of
> users' Drive files."

### ⭐ The critical question: multiple machines / multiple users, same client ID

**Scenario:** machine A (user A's token, client ID C) creates blobs in a folder; that folder is
shared with user B; machine B authenticates with the *same* client ID C but user B's account.
Can machine B see the blobs under `drive.file`?

**Documented answer: No — not automatically.** `drive.file` authorization is a
**(user × app × file)** grant, not a (app × file) grant. The evidence, all from Google docs:

1. **The `appNotAuthorizedToFile` error message itself** (handle-errors, verbatim):
   > "**The user has not granted** the app {appId} {verb} access to the file {fileId}."

   The grant subject is *the user*, not the app. The doc's own description: *"This error occurs when
   your app isn't on the ACL for the file"* — and ACLs are per-file, evaluated for the calling user.
   The prescribed fixes are all "get **this user** to grant it":
   > "Open the Google Drive picker and prompt the user to open the file."
   > "Instruct the user to open the file using the Open with context menu in the Drive UI of your app."
   > "Use the `files.get` method to check the `isAppAuthorized` field on the files resource to verify
   > that your app created or opened the file."

2. **`isAppAuthorized`** (Files resource reference,
   <https://developers.google.com/workspace/drive/api/reference/rest/v3/files>), verbatim:
   > "Output only. Whether the file was created or opened by **the requesting app**."

   Read in combination with #1: it is evaluated in the context of the requesting *call*, i.e. the
   (user, app) pair — a file can be `isAppAuthorized: true` for user A's token and `false` for
   user B's token with the same client ID.

3. **The desktop picker guide** (<https://developers.google.com/workspace/drive/picker/guides/desktop-mobile-picker>),
   verbatim:
   > "For apps to get authorization to files **previously granted to them**, you must use the
   > following steps: You must obtain an OAuth 2.0 token with the `drive.file`, `drive`, or
   > `drive.readonly` scope… Pass the OAuth 2.0 token to the Drive API to read and modify files in
   > which **the user previously granted access**."

   Again: grants are held per-user and must be re-established for a new user.

4. Corroborating (non-Google): rclone's docs, <https://rclone.org/drive/>:
   > "`drive.file` — With this scope rclone can read/view/modify only those files and folders **it
   > creates**. So if you uploaded files to drive via the web interface (or any other means) they
   > will not be visible to rclone."

> **UNVERIFIED (framing caveat):** no single Google page states the cross-user case in one sentence
> ("files created by user A with your app are invisible to user B with your app"). The conclusion
> above is assembled from the four documented facts. It should be **confirmed empirically** with two
> Google accounts and one client ID before we commit the design. This is the single highest-value
> experiment in this work package.

### ⭐⭐ NEW: the desktop OAuth picker (`trigger_onepick`) — this changes our design

**Source:** <https://developers.google.com/workspace/drive/picker/guides/desktop-mobile-picker>
and <https://developers.google.com/workspace/drive/picker/guides/overview>

Google now documents a **desktop/CLI-friendly Picker flow driven entirely by OAuth URL parameters**
— no JavaScript, no embedded webview. This is exactly what a Go CLI needs, and it is the documented
escape hatch from the multi-machine `drive.file` problem.

From the Picker overview comparison table (verbatim, "Desktop & mobile apps" column):

| Feature | Desktop & mobile apps |
|---|---|
| Primary technology | "OAuth 2.0 URL parameters and HTTP redirects." |
| Rendering | "Opens in a new tab of the user's default system browser. **Can no longer be displayed within an embedded webview.**" |
| Auth flow | "Triggered by adding `trigger_onepick=true` to the OAuth request." |
| Response method | "Redirect URIs or custom URL schemes." |
| **Scopes** | **"Strict; only `drive.file` is permitted and cannot be combined with other scopes."** |

> "Note that to use the `drive.file` scope the user must be signed in while accessing the Google Picker."

Authorization URL parameters (verbatim table):

| Parameter | Description | Status |
|---|---|---|
| `prompt=consent` | "Prompt for file access." | **Required** |
| `trigger_onepick=true` | "Enable the Google Picker." | **Required** |
| `allow_multiple=true` | "If true, allow the user to select multiple files." | Optional |
| `mimetypes=MIMETYPES` | "A comma-separated list of MIME types to filter the search results…" | Optional |
| `file_ids=FILE_IDS` | "A comma-separated list of file IDs to filter the search results…" | Optional |
| **`allow_folder_selection=true`** | **"If true, allow the user to also select folders."** | Optional |

Documented sample request:

```
https://accounts.google.com/o/oauth2/v2/auth?
  client_id=CLIENT_ID
  &scope=https://www.googleapis.com/auth/drive.file
  &redirect_uri=REDIRECT_URI
  &response_type=code
  &access_type=offline
  &prompt=consent
  &trigger_onepick=true
```

> "Select a `redirect_uri` that works with your application type and OAuth setup.
> **The Google Picker imposes no additional restrictions.**" — i.e. a loopback `http://127.0.0.1:PORT`
> redirect works here.

Redirect response parameters (verbatim):
- `picked_file_ids` — "If the user granted access and picked files, a comma-separated list of selected file IDs."
- `code` — "The access token or access code based on the `response_type` parameter set in the request. This parameter includes a new authorization code."
- `scope` — "The scope(s) included in the request."
- `error` — "If the user cancelled the request within the consent flow, an error is shown."

Format: `https://REDIRECT_URI?picked_file_ids=PICKED_FILE_IDS&code=CODE&scope=SCOPES`

**Design implication — this is the answer to the multi-machine problem.** Kopia's connect flow on
machine B can issue a single loopback OAuth request with
`trigger_onepick=true&prompt=consent&allow_folder_selection=true&scope=drive.file`. User B picks the
existing repository folder in a real Google Picker in their browser; we get back both a refresh
token *and* `picked_file_ids` containing the repo folder ID — which is simultaneously (a) the
`drive.file` grant for that user and (b) the folder-ID discovery step, in one round trip. No manual
folder-ID copy/paste, no full `drive` scope, no restricted-scope verification, no security
assessment.

> **UNVERIFIED — the one thing that decides whether this actually works:** whether granting
> `drive.file` access to a **folder** transitively grants access to files **created inside it by a
> different user**. Google documents `allow_folder_selection` but does **not** document the
> recursion semantics of a folder grant anywhere I could find. Two possibilities:
> (a) folder grant is recursive → machine B sees all of machine A's blobs → design solved;
> (b) folder grant covers only the folder object → machine B can create/list-its-own but cannot read
> A's blobs → `drive.file` is unusable for multi-machine repos and we need full `drive` scope.
> **This must be tested empirically with two accounts.** It is the load-bearing unknown.

### OOB flow — dead. Loopback — the supported desktop flow.

**Shutdown notice:** <https://developers.google.com/identity/protocols/oauth2/resources/oob-migration>
Timeline documented there: **2022-02-28** new OOB usage blocked → **2022-09-05** user-facing warning →
**2022-10-03** deprecated for pre-Feb-2022 clients → **2023-01-31** "all existing clients are blocked
(including exempted clients)." Announcement blog:
<https://developers.googleblog.com/making-google-oauth-interactions-safer-by-using-more-secure-oauth-flows/>

Confirmed as **currently dead** in the native-app guide
(<https://developers.google.com/identity/protocols/oauth2/native-app>), verbatim:

> "**Manual copy/paste (Deprecated)** — Important: The manual copy/paste option, also referred to as
> an out of band (OOB) redirect method, is **no longer supported**."

And in that page's error reference for `redirect_uri_mismatch`:
> "The `redirect_uri` parameter may refer to the OAuth out-of-band (OOB) flow that has been
> deprecated and is no longer supported."

**Loopback is the supported and recommended desktop flow** — verbatim from the same page:

> "**Loopback IP address (macOS, Linux, Windows desktop)**"
> "To receive the authorization code using this URL, your application must be listening on the local
> web server. That is possible on many, but not all, platforms. However, **if your platform supports
> it, this is the recommended mechanism for obtaining the authorization code.**"
> "Recommended usage: macOS, Linux, and Windows desktop (but not Universal Windows Platform) apps"
> "Form values: Set the application type to **Desktop app**."

Redirect URI forms (verbatim): `http://127.0.0.1:port` or `http://[::1]:port` —
> "Query your platform for the relevant loopback IP address and start an HTTP listener on a random
> available port."
> "Note: … It is also possible to use `localhost` in place of the loopback IP, but this configuration
> may cause issues with client firewalls. Most, but not all, firewalls allow loopback communication."

⚠️ Loopback is **DEPRECATED for Android, Chrome app, and iOS** client types only
(<https://developers.google.com/identity/protocols/oauth2/resources/loopback-migration>) —
**not** for Desktop app clients. Prefer `127.0.0.1` over `localhost`, and support `[::1]`.

Also verbatim from the native-app guide: "Custom URI schemes are no longer supported on Android and
Chrome apps." (Irrelevant to us, but rules out that alternative.)

### Device-code flow — supports `drive.file`, NOT full `drive`

**Source:** <https://developers.google.com/identity/protocols/oauth2/limited-input-device>,
section "Allowed scopes", verbatim:

> "The OAuth 2.0 flow for devices is supported only for the following scopes:"
>
> **OpenID Connect, Google Sign-In**
> - `email`
> - `openid`
> - `profile`
>
> **Drive API**
> - `https://www.googleapis.com/auth/drive.appdata`
> - `https://www.googleapis.com/auth/drive.file`
>
> **YouTube API**
> - `https://www.googleapis.com/auth/youtube`
> - `https://www.googleapis.com/auth/youtube.readonly`

Confirmed: **`https://www.googleapis.com/auth/drive` (full) is NOT in the allowed list.**
Also verbatim: *"Note: Incremental authorization is not supported for installed apps or devices."*

Endpoints: `POST https://oauth2.googleapis.com/device/code` to start; poll for the token.

**Design implication:** the device-code flow is attractive for headless servers (no browser on the
box), but it locks us to `drive.file` — which in turn depends on the unresolved folder-recursion
question above. If we end up needing full `drive` scope, headless boxes must use loopback with SSH
port-forwarding, or import a token/refresh-token minted elsewhere.

---

## Summary of verification status

| Claim | Status |
|---|---|
| Homogeneous batch endpoint `www.googleapis.com/batch/drive/v3` alive, 100 calls max, n-counts-as-n | **Verified** (docs + live discovery doc, revision 20260728) |
| No announced deprecation of homogeneous batching | **Verified** (absence checked in guide + release notes + discovery) |
| Quota-unit model, 1M/min project, 325k/min user, 1 TB/day egress, 400M QU/day billing threshold | **Verified** (limits page, updated 2026-07-31) |
| Legacy (pre-May-2026 project) quota numbers | **UNVERIFIED** — not published; check Cloud console |
| `files.create`/`files.delete` cost 50 QU (edit class) | **UNVERIFIED** — inferred, doc names only `files.update` |
| Per-user quota keyed on authenticated principal; project quota keyed on client ID's project | **Verified** (Cloud system-parameters + quotas overview + Drive limits) |
| rclone shared client ID retiring during 2026 | **Verified** (rclone docs — non-Google, but authoritative for rclone) |
| rclone's "10 transactions per second" default figure | **UNVERIFIED** |
| 750 GB/day per user, combined My Drive + all Shared Drives, 24 h reset | **Verified** (3 Google sources) |
| Service accounts get an independent 750 GB budget | **UNVERIFIED** |
| A dedicated error reason exists for the 750 GB cap | **UNVERIFIED — none documented**; surfaces as 403 `userRateLimitExceeded` |
| 5 TB max upload, 750 GB max copy | **Verified** |
| 5 MB simple/multipart vs resumable threshold; resumable OK for small files too | **Verified** |
| Resumable session expires after one week (of inactivity) | **Verified** (stated 3× on the page) |
| Chunks in multiples of 256 KB, last chunk exempt, "as large as possible" | **Verified** |
| Shared Drive 500k items / 100 levels; My Drive 500k per non-root folder / 100 levels; 500M per account; trash counts | **Verified** |
| `files.list` pageSize max 1000 | **Verified** (live discovery doc) |
| Documented listing-performance degradation for large folders | **UNVERIFIED — none published** |
| Complete 400/401/403/404/429/5xx reason list + backoff algorithm | **Verified** (handle-errors, updated 2026-04-20) |
| `drive.file` grant is per (user × app × file), not per (app × file) | **Verified by inference from 4 Google docs; not stated in one sentence — TEST EMPIRICALLY** |
| Desktop OAuth Picker via `trigger_onepick=true` + `allow_folder_selection=true`, returns `picked_file_ids` | **Verified** |
| Folder grant is recursive to files created by *other* users | **UNVERIFIED — load-bearing unknown, must test** |
| OOB dead (fully blocked 2023-01-31); loopback is the recommended desktop flow | **Verified** |
| Device-code flow allows `drive.file` + `drive.appdata` but not full `drive` | **Verified** |

## All source URLs

- <https://developers.google.com/workspace/drive/api/guides/performance> (batching; upd. 2026-04-20)
- <https://www.googleapis.com/discovery/v1/apis/drive/v3/rest> (live discovery, rev. 20260728)
- <https://developers.googleblog.com/discontinuing-support-for-json-rpc-and-global-http-batch-endpoints/>
- <https://developers.google.com/workspace/drive/api/guides/limits> (quotas + backoff; upd. 2026-07-31)
- <https://developers.google.com/workspace/tools-safety>
- <https://workspaceupdates.googleblog.com/2026/05/agent-tools-and-security-updates-for-workspace-developers.html>
- <https://developers.google.com/workspace/drive/release-notes>
- <https://docs.cloud.google.com/apis/docs/system-parameters> (`quotaUser`)
- <https://docs.cloud.google.com/apis/docs/capping-api-usage>
- <https://docs.cloud.google.com/docs/quotas/overview>
- <https://developers.google.com/workspace/drive/api/guides/manage-uploads> (upd. 2026-04-20)
- <https://knowledge.workspace.google.com/admin/drive/storage-and-upload-limits-for-google-workspace>
- <https://support.google.com/a/users/answer/7338880> (Shared drive limits)
- <https://developers.google.com/workspace/drive/api/guides/folder> (folder limits; upd. 2026-04-20)
- <https://developers.google.com/workspace/drive/api/guides/manage-shareddrives> (upd. 2026-04-20)
- <https://developers.google.com/workspace/drive/api/guides/handle-errors> (upd. 2026-04-20)
- <https://developers.google.com/workspace/drive/api/reference/rest/v3/files> (`isAppAuthorized`)
- <https://developers.google.com/workspace/drive/api/guides/api-specific-auth> (scopes)
- <https://developers.google.com/workspace/drive/picker/guides/overview>
- <https://developers.google.com/workspace/drive/picker/guides/desktop-mobile-picker>
- <https://developers.google.com/identity/protocols/oauth2/native-app>
- <https://developers.google.com/identity/protocols/oauth2/resources/oob-migration>
- <https://developers.google.com/identity/protocols/oauth2/resources/loopback-migration>
- <https://developers.google.com/identity/protocols/oauth2/limited-input-device>
- <https://rclone.org/drive/> (non-Google, for shared-client-ID context)
