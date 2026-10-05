//go:build !no_extra_providers

package gdrivepacer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/gdrive/gdriveerr"
)

var errBoom = errors.New("boom")

// fakeClock is a virtual clock: sleeping advances it instead of blocking, so
// the whole suite runs in milliseconds. It is safe for concurrent use.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration

	// onSleep, when set, runs before each sleep takes effect. Tests use it to
	// cancel a context mid-sleep, which is what a real interruptible sleep
	// would observe.
	onSleep func(d time.Duration)
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	hook := f.onSleep
	f.mu.Unlock()

	if hook != nil {
		hook(d)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if d <= 0 {
		return nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.now = f.now.Add(d)
	f.sleeps = append(f.sleeps, d)

	return nil
}

func (f *fakeClock) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]time.Duration, len(f.sleeps))
	copy(out, f.sleeps)

	return out
}

func (f *fakeClock) elapsedSince(t time.Time) time.Duration {
	return f.Now().Sub(t)
}

// countingRecorder implements Recorder and counts what it is told.
type countingRecorder struct {
	mu         sync.Mutex
	retries    int
	errors     []string
	backoffs   []time.Duration
	pacedWaits []time.Duration
	pacedOps   []string
}

func (r *countingRecorder) RecordRetry(string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.retries++
}

func (r *countingRecorder) RecordError(_, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.errors = append(r.errors, reason)
}

func (r *countingRecorder) RecordBackoff(_ string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.backoffs = append(r.backoffs, d)
}

func (r *countingRecorder) RecordPacedWait(op string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.pacedWaits = append(r.pacedWaits, d)
	r.pacedOps = append(r.pacedOps, op)
}

func (r *countingRecorder) snapshot() (retries int, errs []string, backoffs []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.retries, append([]string(nil), r.errors...), append([]time.Duration(nil), r.backoffs...)
}

func (r *countingRecorder) pacedSnapshot() (waits []time.Duration, ops []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]time.Duration(nil), r.pacedWaits...), append([]string(nil), r.pacedOps...)
}

// gerr builds a *googleapi.Error the way the Drive client library would.
func gerr(code int, reason string, hdr http.Header) *googleapi.Error {
	e := &googleapi.Error{
		Code:    code,
		Message: reason,
		Header:  hdr,
	}

	if reason != "" {
		e.Errors = []googleapi.ErrorItem{{Reason: reason, Message: reason}}
	}

	return e
}

func retryAfterHeader(seconds string) http.Header {
	return http.Header{"Retry-After": []string{seconds}}
}

// newTestPacer returns a pacer driven by a fake clock. Bursty by default so
// that the token bucket does not interfere with backoff assertions.
func newTestPacer(t *testing.T, cfg Config, rec Recorder) (*Pacer, *fakeClock) {
	t.Helper()

	clk := newFakeClock()

	return newWithTimeSource(cfg, rec, clk), clk
}

func TestWithDefaults(t *testing.T) {
	cases := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "zero value takes every default",
			in:   Config{},
			want: Config{MinSleep: DefaultMinSleep, Burst: DefaultBurst, MaxSleep: DefaultMaxSleep, MaxTries: DefaultMaxTries},
		},
		{
			name: "negatives are treated as unset",
			in:   Config{MinSleep: -1, Burst: -1, MaxSleep: -1, MaxTries: -1},
			want: Config{MinSleep: DefaultMinSleep, Burst: DefaultBurst, MaxSleep: DefaultMaxSleep, MaxTries: DefaultMaxTries},
		},
		{
			name: "explicit values survive",
			in:   Config{MinSleep: 20 * time.Millisecond, Burst: 5, MaxSleep: 2 * time.Second, MaxTries: 3},
			want: Config{MinSleep: 20 * time.Millisecond, Burst: 5, MaxSleep: 2 * time.Second, MaxTries: 3},
		},
		{
			name: "maxSleep below minSleep is raised",
			in:   Config{MinSleep: time.Second, MaxSleep: time.Millisecond},
			want: Config{MinSleep: time.Second, Burst: DefaultBurst, MaxSleep: time.Second, MaxTries: DefaultMaxTries},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.in.withDefaults())
		})
	}
}

func TestConfigJSONRoundTrip(t *testing.T) {
	in := Config{MinSleep: 250 * time.Millisecond, Burst: 7, MaxSleep: 5 * time.Second, MaxTries: 3}

	buf, err := json.Marshal(in)
	require.NoError(t, err)

	var out Config

	require.NoError(t, json.Unmarshal(buf, &out))
	require.Equal(t, in, out)

	// Zero value is omitted entirely, so a config file need only carry the
	// knobs it actually overrides.
	buf, err = json.Marshal(Config{})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(buf))

	// Partial documents decode and default the rest.
	var partial Config

	require.NoError(t, json.Unmarshal([]byte(`{"burst":42}`), &partial))
	require.Equal(t, 42, partial.Burst)
	require.Equal(t, Config{MinSleep: DefaultMinSleep, Burst: 42, MaxSleep: DefaultMaxSleep, MaxTries: DefaultMaxTries},
		partial.withDefaults())
}

func TestJitterShiftMatchesGdriveerr(t *testing.T) {
	// The ladder assumes gdriveerr.RetryDelay's jitter window at attempt
	// n+jitterShift is exactly backoffBase<<n.
	require.Equal(t, backoffBase, gdriveerr.BackoffBase<<jitterShift)
}

func TestSteadyStatePacingHonorsBurst(t *testing.T) {
	const (
		minSleep = 100 * time.Millisecond
		burst    = 3
		calls    = 6
	)

	p, clk := newTestPacer(t, Config{MinSleep: minSleep, Burst: burst}, nil)
	start := clk.Now()

	var n int

	for range calls {
		require.NoError(t, p.Call(t.Context(), "ListBlobs", "", func() error {
			n++

			return nil
		}))
	}

	require.Equal(t, calls, n)

	// The first `burst` calls consume the pre-filled bucket and do not wait;
	// every call after that waits one refill interval.
	sleeps := clk.recorded()
	require.Len(t, sleeps, calls-burst)

	for _, d := range sleeps {
		require.Equal(t, minSleep, d)
	}

	require.Equal(t, time.Duration(calls-burst)*minSleep, clk.elapsedSince(start))
}

// TestSteadyStateWaitsAreRecorded pins the counter that makes a pacer
// hypothesis falsifiable: every token wait actually slept is reported to the
// Recorder with its real length, and nothing else is.
func TestSteadyStateWaitsAreRecorded(t *testing.T) {
	const (
		minSleep = 100 * time.Millisecond
		burst    = 3
		calls    = 6
	)

	rec := &countingRecorder{}
	p, clk := newTestPacer(t, Config{MinSleep: minSleep, Burst: burst}, rec)
	start := clk.Now()

	for range calls {
		require.NoError(t, p.Call(t.Context(), "ListBlobs", "", func() error { return nil }))
	}

	waits, ops := rec.pacedSnapshot()

	// Only the calls that outran the bucket are recorded - the first `burst`
	// found a token waiting for them and paid nothing.
	require.Len(t, waits, calls-burst)
	require.Equal(t, []string{"ListBlobs", "ListBlobs", "ListBlobs"}, ops)

	var total time.Duration

	for _, d := range waits {
		require.Equal(t, minSleep, d)

		total += d
	}

	// The counter sums to exactly the wall clock the pacer cost, which is the
	// property a tuning cycle reads it for.
	require.Equal(t, clk.elapsedSince(start), total)

	// A clean run touches the error path not at all.
	retries, errs, backoffs := rec.snapshot()
	require.Zero(t, retries)
	require.Empty(t, errs)
	require.Empty(t, backoffs)
}

func TestNoPacedWaitRecordedWhenTheBucketNeverEmpties(t *testing.T) {
	const calls = 10

	rec := &countingRecorder{}
	p, clk := newTestPacer(t, Config{MinSleep: time.Second, Burst: calls}, rec)

	for range calls {
		require.NoError(t, p.Call(t.Context(), "GetMetadata", "", func() error { return nil }))
	}

	waits, _ := rec.pacedSnapshot()
	require.Empty(t, waits, "a workload that stays inside the burst pool must record no paced wait")
	require.Empty(t, clk.recorded())
}

func TestInterruptedTokenWaitIsNotRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rec := &countingRecorder{}
	p, clk := newTestPacer(t, Config{MinSleep: time.Second, Burst: 1}, rec)

	// The first call empties the bucket; the second has to wait, and the
	// context dies while it does.
	require.NoError(t, p.Call(ctx, "PutBlob", "b1", func() error { return nil }))

	clk.mu.Lock()
	clk.onSleep = func(time.Duration) { cancel() }
	clk.mu.Unlock()

	require.ErrorIs(t, p.Call(ctx, "PutBlob", "b2", func() error { return nil }), context.Canceled)

	waits, _ := rec.pacedSnapshot()
	require.Empty(t, waits, "a wait that was never paid must not be counted")
}

func TestRetryableBackoffRetriesWithGrowingDelay(t *testing.T) {
	const (
		maxSleep = 16 * time.Second
		maxTries = 8
	)

	rec := &countingRecorder{}
	p, clk := newTestPacer(t, Config{Burst: 1000, MaxSleep: maxSleep, MaxTries: maxTries}, rec)

	var attempts int

	err := p.Call(t.Context(), "PutBlob", "b1", func() error {
		attempts++

		return gerr(http.StatusForbidden, "rateLimitExceeded", nil)
	})

	require.Error(t, err)
	require.Equal(t, maxTries, attempts)

	// The classified error comes back, not the raw googleapi one.
	de, ok := gdriveerr.AsDriveError(err)
	require.True(t, ok)
	require.Equal(t, gdriveerr.RetryableBackoff, de.Disposition())
	require.Equal(t, "rateLimitExceeded", de.Reason())

	sleeps := clk.recorded()
	require.Len(t, sleeps, maxTries-1)

	for i, d := range sleeps {
		// Ladder floor for the i-th consecutive error: half of
		// backoffBase<<i, truncated at MaxSleep.
		floor := min(backoffBase<<i/2, maxSleep)

		require.GreaterOrEqual(t, d, floor, "sleep %d (%v) below ladder floor", i, d)
		require.LessOrEqual(t, d, maxSleep, "sleep %d (%v) above MaxSleep", i, d)
	}

	// Once the window exceeds MaxSleep the delay is pinned at exactly MaxSleep.
	require.Equal(t, maxSleep, sleeps[len(sleeps)-1])

	retries, errs, backoffs := rec.snapshot()
	require.Equal(t, maxTries-1, retries)
	require.Len(t, errs, maxTries)
	require.Equal(t, sleeps, backoffs)
}

func TestRetryAfterIsHonoredVerbatim(t *testing.T) {
	p, clk := newTestPacer(t, Config{Burst: 1000}, nil)

	var attempts int

	require.NoError(t, p.Call(t.Context(), "PutBlob", "b1", func() error {
		attempts++
		if attempts == 1 {
			return gerr(http.StatusForbidden, "userRateLimitExceeded", retryAfterHeader("3"))
		}

		return nil
	}))

	require.Equal(t, 2, attempts)
	require.Equal(t, []time.Duration{3 * time.Second}, clk.recorded())
}

func TestRetryAfterAboveMaxSleepIsNotTruncated(t *testing.T) {
	// A server-dictated delay wins over MaxSleep: ignoring it only earns
	// another rate-limit response.
	p, clk := newTestPacer(t, Config{Burst: 1000, MaxSleep: 2 * time.Second}, nil)

	var attempts int

	require.NoError(t, p.Call(t.Context(), "PutBlob", "b1", func() error {
		attempts++
		if attempts == 1 {
			return gerr(http.StatusTooManyRequests, "rateLimitExceeded", retryAfterHeader("30"))
		}

		return nil
	}))

	require.Equal(t, []time.Duration{30 * time.Second}, clk.recorded())
}

func TestFatalErrorsGetExactlyOneAttempt(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantIs      error
		disposition gdriveerr.Disposition
	}{
		{
			name:        "storage quota exceeded",
			err:         gerr(http.StatusForbidden, "storageQuotaExceeded", nil),
			wantIs:      gdriveerr.ErrStorageQuotaExhausted,
			disposition: gdriveerr.FatalQuota,
		},
		{
			name:        "not found",
			err:         gerr(http.StatusNotFound, "notFound", nil),
			wantIs:      blob.ErrBlobNotFound,
			disposition: gdriveerr.FatalNotFound,
		},
		{
			name:        "precondition failed",
			err:         gerr(http.StatusPreconditionFailed, "", nil),
			wantIs:      blob.ErrBlobAlreadyExists,
			disposition: gdriveerr.FatalPrecondition,
		},
		{
			name:        "range not satisfiable",
			err:         gerr(http.StatusRequestedRangeNotSatisfiable, "", nil),
			wantIs:      blob.ErrInvalidRange,
			disposition: gdriveerr.FatalInvalidRange,
		},
		{
			name:        "auth",
			err:         gerr(http.StatusUnauthorized, "authError", nil),
			wantIs:      blob.ErrInvalidCredentials,
			disposition: gdriveerr.FatalAuth,
		},
		{
			name:        "malformed request",
			err:         gerr(http.StatusBadRequest, "badRequest", nil),
			wantIs:      nil,
			disposition: gdriveerr.NonRetryableOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &countingRecorder{}
			p, clk := newTestPacer(t, Config{Burst: 1000}, rec)

			var attempts int

			err := p.Call(t.Context(), "GetBlob", "b1", func() error {
				attempts++

				return tc.err
			})

			require.Error(t, err)
			require.Equal(t, 1, attempts, "fatal errors must not be retried")
			require.Empty(t, clk.recorded(), "fatal errors must not sleep")

			if tc.wantIs != nil {
				require.ErrorIs(t, err, tc.wantIs)
			}

			de, ok := gdriveerr.AsDriveError(err)
			require.True(t, ok)
			require.Equal(t, tc.disposition, de.Disposition())

			retries, errs, backoffs := rec.snapshot()
			require.Equal(t, 0, retries)
			require.Len(t, errs, 1)
			require.Empty(t, backoffs)
		})
	}
}

func TestRetryableResumeUsesMildLocalBackoff(t *testing.T) {
	const maxSleep = 16 * time.Second

	p, clk := newTestPacer(t, Config{Burst: 1000, MaxSleep: maxSleep}, nil)

	var attempts int

	require.NoError(t, p.Call(t.Context(), "GetBlob", "b1", func() error {
		attempts++
		if attempts < 3 {
			return gerr(http.StatusInternalServerError, "backendError", nil)
		}

		return nil
	}))

	require.Equal(t, 3, attempts)

	sleeps := clk.recorded()
	require.Len(t, sleeps, 2)

	for _, d := range sleeps {
		require.GreaterOrEqual(t, d, DefaultMinSleep)
		require.LessOrEqual(t, d, maxSleep)
	}
}

func TestRetryableResumeDoesNotSlowOtherCallers(t *testing.T) {
	// Transient 5xx/network failures say nothing about the account's rate
	// budget, so they must not raise the global barrier.
	p, _ := newTestPacer(t, Config{Burst: 1000}, nil)

	err := p.CallNoRetry(t.Context(), "GetBlob", "b1", func() error {
		return gerr(http.StatusInternalServerError, "backendError", nil)
	})

	require.Error(t, err)
	require.LessOrEqual(t, p.backoffRemaining(), time.Duration(0))
}

func TestSuccessInstantlyReAccelerates(t *testing.T) {
	p, clk := newTestPacer(t, Config{Burst: 1000}, nil)

	var attempts int

	require.NoError(t, p.Call(t.Context(), "PutBlob", "b1", func() error {
		attempts++
		if attempts == 1 {
			return gerr(http.StatusForbidden, "rateLimitExceeded", nil)
		}

		return nil
	}))

	require.Len(t, clk.recorded(), 1, "exactly one backoff sleep before the retry")
	require.LessOrEqual(t, p.backoffRemaining(), time.Duration(0), "success must clear the shared barrier")

	// The next call runs at steady state: no residual backoff at all.
	before := clk.Now()

	require.NoError(t, p.Call(t.Context(), "PutBlob", "b2", func() error { return nil }))
	require.Zero(t, clk.elapsedSince(before))
	require.Len(t, clk.recorded(), 1)
}

func TestBackoffIsGlobalAcrossCalls(t *testing.T) {
	p, clk := newTestPacer(t, Config{Burst: 1000}, nil)

	// One call learns about the rate limit and gives up (no retries)...
	err := p.CallNoRetry(t.Context(), "PutBlob", "b1", func() error {
		return gerr(http.StatusForbidden, "userRateLimitExceeded", nil)
	})
	require.Error(t, err)
	require.Empty(t, clk.recorded(), "CallNoRetry must not sleep after its single attempt")
	require.Positive(t, p.backoffRemaining(), "the shared barrier must be raised even without retries")

	// ...and an unrelated call, which would otherwise succeed immediately,
	// waits behind the barrier it raised.
	before := clk.Now()

	require.NoError(t, p.Call(t.Context(), "GetBlob", "b2", func() error { return nil }))
	require.GreaterOrEqual(t, clk.elapsedSince(before), backoffBase/2)
}

func TestCallNoRetryMakesExactlyOneAttempt(t *testing.T) {
	p, _ := newTestPacer(t, Config{Burst: 1000, MaxTries: 10}, nil)

	var attempts int

	err := p.CallNoRetry(t.Context(), "PutBlob", "b1", func() error {
		attempts++

		return gerr(http.StatusForbidden, "rateLimitExceeded", nil)
	})

	require.Error(t, err)
	require.Equal(t, 1, attempts)

	de, ok := gdriveerr.AsDriveError(err)
	require.True(t, ok)
	require.Equal(t, gdriveerr.RetryableBackoff, de.Disposition(), "the error is still classified")
}

func TestContextCanceledDuringBackoffReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	p, clk := newTestPacer(t, Config{Burst: 1000}, nil)

	// Cancel while the pacer is asleep in its backoff, exactly as a real
	// interruptible sleep would observe it.
	clk.mu.Lock()
	clk.onSleep = func(time.Duration) { cancel() }
	clk.mu.Unlock()

	var attempts int

	err := p.Call(ctx, "PutBlob", "b1", func() error {
		attempts++

		return gerr(http.StatusForbidden, "rateLimitExceeded", nil)
	})

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, context.Canceled, err, "the context error must be returned unchanged")
	require.Equal(t, 1, attempts)
	require.Empty(t, clk.recorded(), "the interrupted sleep must not complete")
}

func TestContextCanceledBeforeCallSkipsFn(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	p, _ := newTestPacer(t, Config{}, nil)

	called := false
	err := p.Call(ctx, "GetBlob", "b1", func() error {
		called = true

		return nil
	})

	require.Equal(t, context.Canceled, err)
	require.False(t, called)
}

func TestContextErrorFromFnPassesThroughUnchanged(t *testing.T) {
	p, _ := newTestPacer(t, Config{Burst: 1000}, nil)

	ctx, cancel := context.WithCancel(t.Context())

	var attempts int

	err := p.Call(ctx, "GetBlob", "b1", func() error {
		attempts++

		// The caller gives up while the request is in flight.
		cancel()

		return context.Canceled
	})

	require.Equal(t, context.Canceled, err)
	require.Equal(t, 1, attempts, "a canceled request is the caller's decision, never a retry")
}

// TestTransportTimeoutWithLiveContextIsRetried: net/http reports its own
// ResponseHeaderTimeout (and a dial timeout) as errors matching
// context.DeadlineExceeded. With the caller's context still live such an error
// is a stalled connection, not a cancellation, and must be retried.
func TestTransportTimeoutWithLiveContextIsRetried(t *testing.T) {
	p, _ := newTestPacer(t, Config{Burst: 1000}, nil)

	var attempts int

	err := p.Call(t.Context(), "GetBlob", "b1", func() error {
		attempts++
		if attempts < 3 {
			return &url.Error{Op: "Get", URL: "https://www.googleapis.com/drive/v3/files/x", Err: context.DeadlineExceeded}
		}

		return nil
	})

	require.NoError(t, err)
	require.Equal(t, 3, attempts)
}

func TestTransportTimeoutRetriesExhaustedIsNotACancellation(t *testing.T) {
	p, _ := newTestPacer(t, Config{Burst: 1000, MaxTries: 2}, nil)

	err := p.Call(t.Context(), "GetBlob", "b1", func() error {
		return &url.Error{Op: "Get", URL: "https://www.googleapis.com/drive/v3/files/x", Err: context.DeadlineExceeded}
	})

	de, ok := gdriveerr.AsDriveError(err)
	require.True(t, ok)
	require.Equal(t, gdriveerr.RetryableResume, de.Disposition())
	require.NotErrorIs(t, err, context.DeadlineExceeded, "upper layers must not mistake a stall for the caller's cancellation")
}

func TestUnclassifiedErrorIsRetriedThenReturnedClassified(t *testing.T) {
	p, _ := newTestPacer(t, Config{Burst: 1000, MaxTries: 3}, nil)

	var attempts int

	err := p.Call(t.Context(), "GetBlob", "b1", func() error {
		attempts++

		return errBoom
	})

	require.Equal(t, 3, attempts)
	require.ErrorIs(t, err, errBoom, "the original error stays reachable")

	de, ok := gdriveerr.AsDriveError(err)
	require.True(t, ok)
	require.Equal(t, gdriveerr.RetryableResume, de.Disposition())
}

func TestRecorderCounts(t *testing.T) {
	rec := &countingRecorder{}
	p, clk := newTestPacer(t, Config{Burst: 1000}, rec)

	var attempts int

	require.NoError(t, p.Call(t.Context(), "GetBlob", "b1", func() error {
		attempts++
		if attempts <= 2 {
			return gerr(http.StatusForbidden, "userRateLimitExceeded", nil)
		}

		return nil
	}))

	retries, errs, backoffs := rec.snapshot()
	require.Equal(t, 2, retries)
	require.Equal(t, []string{"userRateLimitExceeded", "userRateLimitExceeded"}, errs)
	require.Equal(t, clk.recorded(), backoffs)
	require.Len(t, backoffs, 2)
}

func TestRecorderReportsEmptyReasonForNonDriveErrors(t *testing.T) {
	rec := &countingRecorder{}
	p, _ := newTestPacer(t, Config{Burst: 1000, MaxTries: 1}, rec)

	require.Error(t, p.Call(t.Context(), "GetBlob", "b1", func() error { return errBoom }))

	_, errs, _ := rec.snapshot()
	require.Equal(t, []string{""}, errs)
}

func TestNilRecorderDoesNotPanic(t *testing.T) {
	p, _ := newTestPacer(t, Config{Burst: 1000, MaxTries: 2}, nil)

	require.Error(t, p.Call(t.Context(), "PutBlob", "b1", func() error {
		return gerr(http.StatusForbidden, "rateLimitExceeded", nil)
	}))

	require.NoError(t, p.CallNoRetry(t.Context(), "PutBlob", "b2", func() error { return nil }))
}

func TestNewUsesRealClock(t *testing.T) {
	// New must be usable as-is; a call that neither waits nor fails should
	// return immediately.
	p := New(Config{}, nil)

	require.NoError(t, p.Call(t.Context(), "GetBlob", "b1", func() error { return nil }))
}

func TestConcurrentCallsAreRaceFree(t *testing.T) {
	const (
		goroutines = 16
		perRoutine = 8
	)

	rec := &countingRecorder{}
	p, _ := newTestPacer(t, Config{MinSleep: 10 * time.Millisecond, Burst: 4, MaxTries: 4}, rec)

	var (
		wg       sync.WaitGroup
		attempts atomic.Int64
		failures atomic.Int64
	)

	for g := range goroutines {
		wg.Go(func() {
			for i := range perRoutine {
				var local int

				err := p.Call(t.Context(), "PutBlob", "b", func() error {
					attempts.Add(1)

					local++
					// Every fourth logical call fails once with a rate limit,
					// which every other goroutine then has to observe.
					if local == 1 && (g+i)%4 == 0 {
						return gerr(http.StatusForbidden, "rateLimitExceeded", nil)
					}

					return nil
				})
				if err != nil {
					failures.Add(1)
				}
			}
		})
	}

	wg.Wait()

	require.Zero(t, failures.Load())
	require.GreaterOrEqual(t, attempts.Load(), int64(goroutines*perRoutine))

	retries, errs, _ := rec.snapshot()
	require.Equal(t, len(errs), retries)
}

func TestBackoffForBoundsAcrossTheLadder(t *testing.T) {
	const (
		minSleep = 100 * time.Millisecond
		maxSleep = 16 * time.Second
		samples  = 200
	)

	p, _ := newTestPacer(t, Config{MinSleep: minSleep, MaxSleep: maxSleep}, nil)

	err := gdriveerr.Classify(gerr(http.StatusForbidden, "rateLimitExceeded", nil), "PutBlob", "b1")

	for n := range 10 {
		floor := max(min(backoffBase<<min(n, maxLadderShift)/2, maxSleep), minSleep)

		for range samples {
			d := p.backoffFor(err, n)

			require.GreaterOrEqual(t, d, floor, "attempt %d", n)
			require.LessOrEqual(t, d, maxSleep, "attempt %d", n)
		}
	}
}

func TestBackoffForHonorsSlowRecoveringReasons(t *testing.T) {
	p, _ := newTestPacer(t, Config{}, nil)

	// sharingRateLimitExceeded carries a 10s floor inside gdriveerr; the ladder
	// must not clamp it away on the first attempt.
	err := gdriveerr.Classify(gerr(http.StatusForbidden, "sharingRateLimitExceeded", nil), "PutBlob", "b1")

	require.GreaterOrEqual(t, p.backoffFor(err, 0), 10*time.Second)
}
