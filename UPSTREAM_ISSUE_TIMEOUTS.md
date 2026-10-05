# Draft upstream issue: storage backends hang indefinitely on dead connections

Fork-only draft. File on kopia/kopia separately from the Google Drive PR.

---

**Title:** providers: no stall detection on HTTP storage connections; backups hang after sleep/network change

**Summary**

Kopia has no provider-independent stall timeout. Each HTTP-based storage
backend inherits whatever its SDK's transport defaults are, and most of
them cannot detect a TCP connection that died silently (laptop suspend,
Wi-Fi roam, NAT state loss). After such an event a running
`snapshot create` keeps running but transfers nothing until the kernel's
TCP retransmission timeout gives up, which takes tens of minutes for a
connection with unacknowledged upload data.

**Observed:** a laptop suspended mid-snapshot. After resume, the kopia
process was alive with zero throughput.

**Current state per backend** (kopia master b1da5999):

| Backend | Transport source | Stall protection |
|---|---|---|
| GCS | google.golang.org/api transport/http | HTTP/2 `ReadIdleTimeout` 31s (health-check pings): covered |
| S3 | minio-go `DefaultTransport` | `ResponseHeaderTimeout` 1m only: mid-upload write stalls not covered |
| Azure | azcore default transport | dial/TLS handshake timeouts only |
| WebDAV, B2 | net/http defaults | dial/TLS handshake timeouts only |
| Google Drive | custom `http.Client` passed via `option.WithHTTPClient`, which bypasses the GCS-style HTTP/2 health check | none |

For comparison, rclone applies a 5-minute per-connection I/O idle
timeout (deadline pushed forward on every Read/Write) plus
`ResponseHeaderTimeout` to every backend by default.

**Proposal**

A shared transport helper (for example in `internal/`) that backends opt
into:

- HTTP/2 health checks through the standard library
  (`http.Transport.HTTP2 = &http.HTTP2Config{SendPingTimeout, PingTimeout}` --
  the standard-library names for x/net/http2's ReadIdleTimeout/PingTimeout,
  Go 1.24+, no new dependency)
- `ResponseHeaderTimeout`
- an optional I/O idle-deadline dialer wrapper for HTTP/1.1 connections

Each backend must also confirm that the resulting timeout errors are
classified as retryable by its retry layer, so a stall turns into a
retry rather than a failed snapshot.

The Google Drive backend rewrite (PR #TBD) implements this locally for
that backend, including the classification half (a transport timeout
while the caller's context is live is retried, not treated as a
cancellation) and raising googleapi's 32s per-chunk retry deadline so a
stalled resumable upload resumes its session instead of restarting;
it could move to the shared helper if this is accepted.
