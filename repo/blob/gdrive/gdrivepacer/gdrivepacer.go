//go:build !no_extra_providers

// Package gdrivepacer provides the single global rate limiter and retry loop
// that every Google Drive API call made by Kopia's Drive backend passes
// through.
//
// # Shape of the algorithm
//
// The pacer is a token bucket (steady state) behind a global backoff barrier
// (error state), modeled on rclone's GoogleDrive pacer, which is the only
// widely-deployed Drive client with a well-understood throttling story:
//
//   - Steady state: one token every Config.MinSleep with a burst pool of
//     Config.Burst. With the defaults (100ms / 100) that is 10 calls per
//     second sustained and 100 back-to-back.
//   - Error state: a 403 rate-limit error moves a shared deadline into the
//     future. Every goroutine using this pacer waits for that deadline before
//     its next call, so one rate-limit response slows the whole backend down,
//     not just the goroutine that received it.
//   - Re-acceleration is instant: the first successful call clears the barrier
//     and resets the consecutive-error counter, so everybody returns to the
//     token-bucket pace immediately. There is no gradual decay. This is the
//     part of rclone's design that matters most in practice: a decayed pacer
//     spends minutes crawling after a single burst of 403s.
//
// # Where retry decisions come from
//
// The pacer never inspects errors itself. Every error returned by the wrapped
// function is passed to gdriveerr.Classify and the resulting
// gdriveerr.Disposition alone decides what happens next:
//
//   - RetryableBackoff: enter the global backoff state and retry.
//   - RetryableResume: retry after a mild, call-local delay. A transient
//     network or 5xx failure says nothing about the account's rate budget, so
//     it deliberately does not slow down other goroutines.
//   - FatalQuota, FatalAuth, FatalNotFound, FatalPrecondition,
//     FatalInvalidRange, NonRetryableOther: return immediately, after one
//     attempt.
//   - context.Canceled and context.DeadlineExceeded are returned unchanged and
//     never retried - when the caller's ctx is done. With ctx still live they
//     came from inside the HTTP stack (a stall timeout) and are retried as
//     RetryableResume; see gdriveerr.ClassifyContext.
//
// Call always returns the classified error, so callers get gdriveerr's
// sentinel unwrapping (blob.ErrBlobNotFound, blob.ErrBlobAlreadyExists,
// blob.ErrInvalidRange, blob.ErrInvalidCredentials,
// gdriveerr.ErrStorageQuotaExhausted) for free through errors.Is.
//
// # Layering
//
// The pacer is the Drive backend's only retry layer; the backend is not
// wrapped with repo/blob/retrying, which cannot honor Retry-After and would
// happily retry a fatal quota error ten times.
//
// Concurrency limits (parallel deletes, parallel metadata fetches, ...) live
// ABOVE the pacer: callers spawn as many goroutines as they like and all of
// them funnel through one shared *Pacer, which is what makes the global
// backoff meaningful. A *Pacer is safe for concurrent use.
package gdrivepacer

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo/blob/gdrive/gdriveerr"
)

// Defaults applied by Config.withDefaults to zero-valued fields. They are
// rclone's Google Drive settings, which are the best-attested values in the
// wild: 10 calls/second sustained, 100 burst, 1s..16s backoff ladder, 10
// attempts per call.
const (
	DefaultMinSleep = 100 * time.Millisecond
	DefaultBurst    = 100
	DefaultMaxSleep = 16 * time.Second
	DefaultMaxTries = 10
)

const (
	// backoffBase is the first backoff window: the ladder of windows is
	// 1s, 2s, 4s, 8s, 16s, ... truncated at Config.MaxSleep.
	backoffBase = time.Second

	// jitterShift aligns gdriveerr.RetryDelay's jitter window
	// (gdriveerr.BackoffBase << attempt) with the ladder above:
	// 500ms << 1 == backoffBase. TestJitterShiftMatchesGdriveerr asserts it.
	jitterShift = 1

	// maxLadderShift bounds the exponent applied to backoffBase so the shift
	// cannot overflow no matter how long a rate limit persists. The windows
	// saturate at MaxSleep long before this.
	maxLadderShift = 16

	// ladderFloorDivisor sets how much of each backoff window is jittered: the
	// delay is drawn from the top half of the window, i.e. window/2 .. window.
	ladderFloorDivisor = 2
)

// Config holds every pacer knob. The zero value is valid and means "all
// defaults", so a Config decoded from a JSON document that sets only some of
// the fields behaves sensibly.
//
// Durations serialize as integer nanoseconds, which is how encoding/json
// renders time.Duration; the millisecond-valued fields of the backend's
// tuning options are converted into this struct by the caller.
type Config struct {
	// MinSleep is the steady-state interval between Drive API calls: the token
	// bucket refills one token every MinSleep. It is also the floor applied to
	// every backoff delay. Zero means DefaultMinSleep.
	MinSleep time.Duration `json:"minSleep,omitempty"`

	// Burst is the number of calls that may be made back-to-back before the
	// steady-state interval starts to apply. Zero means DefaultBurst.
	Burst int `json:"burst,omitempty"`

	// MaxSleep caps the exponential backoff ladder. A delay the server itself
	// requested through a Retry-After header is honored verbatim and is NOT
	// capped by MaxSleep - ignoring an explicit Retry-After only earns another
	// rate-limit response. Zero means DefaultMaxSleep.
	MaxSleep time.Duration `json:"maxSleep,omitempty"`

	// MaxTries is the maximum number of attempts Call makes, including the
	// first one. Zero means DefaultMaxTries. CallNoRetry ignores it.
	MaxTries int `json:"maxTries,omitempty"`
}

// withDefaults returns a copy of c with every unset (or nonsensical) field
// replaced by its default.
func (c Config) withDefaults() Config {
	if c.MinSleep <= 0 {
		c.MinSleep = DefaultMinSleep
	}

	if c.Burst <= 0 {
		c.Burst = DefaultBurst
	}

	if c.MaxSleep <= 0 {
		c.MaxSleep = DefaultMaxSleep
	}

	if c.MaxSleep < c.MinSleep {
		c.MaxSleep = c.MinSleep
	}

	if c.MaxTries <= 0 {
		c.MaxTries = DefaultMaxTries
	}

	return c
}

// Recorder receives the pacer's observations. It is implemented by the Drive
// backend's instrumented transport; every method must be safe for concurrent
// use and must not block.
type Recorder interface {
	// RecordRetry is called once per retried attempt, before the delay.
	RecordRetry(op string)

	// RecordError is called once per classified error, with the Google Drive
	// `reason` string, or "" when the error carried none.
	RecordError(op, reason string)

	// RecordBackoff is called once per delay actually slept, with its length.
	RecordBackoff(op string, d time.Duration)

	// RecordPacedWait is called once per steady-state token wait actually
	// slept, with its length, and never with a zero or negative duration: a
	// call that found a token waiting for it records nothing.
	//
	// This is the error-free half of the pacer, and it is the only way to tell
	// a configuration in which Config.MinSleep binds from one in which the
	// token bucket refills faster than the workload can draw from it. Without
	// it a pacer hypothesis can only be argued from arithmetic (see the cycle-1
	// entry of the tuning log), never measured.
	RecordPacedWait(op string, d time.Duration)
}

// noopRecorder stands in for a nil Recorder so the hot path never has to test
// for nil.
type noopRecorder struct{}

func (noopRecorder) RecordRetry(string)                    {}
func (noopRecorder) RecordError(string, string)            {}
func (noopRecorder) RecordBackoff(string, time.Duration)   {}
func (noopRecorder) RecordPacedWait(string, time.Duration) {}

// timeSource abstracts wall-clock reads and interruptible sleeps so that unit
// tests can exercise the ladders without real delays.
type timeSource interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// realTime is the production timeSource.
type realTime struct{}

func (realTime) Now() time.Time {
	return clock.Now()
}

func (realTime) Sleep(ctx context.Context, d time.Duration) error {
	if d > 0 && !clock.SleepInterruptibly(ctx, d) {
		return ctx.Err() //nolint:wrapcheck // context errors are propagated verbatim by contract
	}

	return ctx.Err() //nolint:wrapcheck // context errors are propagated verbatim by contract
}

// Pacer paces and retries Google Drive API calls. Use New to create one and
// share it across every goroutine that talks to one Drive repository.
type Pacer struct {
	cfg     Config
	rec     Recorder
	clk     timeSource
	limiter *rate.Limiter

	// retryDelay is gdriveerr.RetryDelay in production; tests substitute a
	// deterministic function.
	retryDelay func(err error, attempt int) time.Duration

	mu sync.Mutex
	// backoffUntil is the shared deadline no call may start before. Zero when
	// the pacer is in steady state.
	backoffUntil time.Time
	// consecutive counts RetryableBackoff errors observed since the last
	// success, across all goroutines. It indexes the backoff ladder.
	consecutive int
}

// New returns a Pacer configured by cfg (zero fields take their defaults) that
// reports into rec. rec may be nil, in which case observations are discarded.
func New(cfg Config, rec Recorder) *Pacer {
	return newWithTimeSource(cfg, rec, realTime{})
}

// newWithTimeSource is New plus an injectable clock, for tests.
func newWithTimeSource(cfg Config, rec Recorder, clk timeSource) *Pacer {
	cfg = cfg.withDefaults()

	if rec == nil {
		rec = noopRecorder{}
	}

	return &Pacer{
		cfg:        cfg,
		rec:        rec,
		clk:        clk,
		limiter:    rate.NewLimiter(rate.Every(cfg.MinSleep), cfg.Burst),
		retryDelay: gdriveerr.RetryDelay,
	}
}

// Call runs fn under the global rate limiter, retrying it up to
// Config.MaxTries times for as long as gdriveerr classifies its error as
// retryable. op and blobID are passed to gdriveerr.Classify for the error
// message and for the Recorder.
//
// The error returned is always the classified one (a *gdriveerr.DriveError),
// except for context errors, which pass through unchanged.
func (p *Pacer) Call(ctx context.Context, op, blobID string, fn func() error) error {
	return p.call(ctx, op, blobID, fn, p.cfg.MaxTries)
}

// CallNoRetry makes exactly one attempt, still rate-limited and still
// classified. Use it for requests whose body cannot be replayed. A rate-limit
// error still puts the pacer into the global backoff state, so the caller's
// own retry (if any) is paced correctly.
func (p *Pacer) CallNoRetry(ctx context.Context, op, blobID string, fn func() error) error {
	return p.call(ctx, op, blobID, fn, 1)
}

func (p *Pacer) call(ctx context.Context, op, blobID string, fn func() error, maxTries int) error {
	var lastErr error

	for attempt := range maxTries {
		if err := p.wait(ctx, op); err != nil {
			return err
		}

		err := gdriveerr.ClassifyContext(ctx, fn(), op, blobID)
		if err == nil {
			p.succeeded()

			return nil
		}

		// ClassifyContext leaves an error unclassified only when it is the
		// caller's own cancellation.
		if _, classified := gdriveerr.AsDriveError(err); !classified {
			return err //nolint:wrapcheck // context errors are propagated verbatim by contract
		}

		p.rec.RecordError(op, reasonOf(err))

		lastErr = err

		disposition, _ := gdriveerr.DispositionOf(err)

		if disposition == gdriveerr.RetryableBackoff {
			// Global on purpose: the account is being throttled, so every
			// goroutine sharing this pacer must slow down - including when
			// this particular call has no attempts left.
			p.enterBackoff(err)
		}

		if !disposition.Retryable() || attempt == maxTries-1 {
			break
		}

		p.rec.RecordRetry(op)

		if disposition == gdriveerr.RetryableResume {
			// A transient network or 5xx failure is not evidence of
			// throttling, so it delays only this call.
			if serr := p.mildBackoff(ctx, op, err, attempt); serr != nil {
				return serr
			}
		}
	}

	return lastErr
}

// wait blocks until this call may proceed: first behind the global backoff
// barrier, then behind the steady-state token bucket. It returns ctx.Err()
// unchanged if the context is done at any point.
func (p *Pacer) wait(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // context errors are propagated verbatim by contract
	}

	if err := p.waitForBackoff(ctx, op); err != nil {
		return err
	}

	return p.waitForToken(ctx, op)
}

func (p *Pacer) waitForBackoff(ctx context.Context, op string) error {
	// Loop: the barrier may have been pushed further out by another goroutine
	// while this one was sleeping, exactly as it may have been cleared by a
	// success.
	for {
		d := p.backoffRemaining()
		if d <= 0 {
			return nil
		}

		p.rec.RecordBackoff(op, d)

		if err := p.clk.Sleep(ctx, d); err != nil {
			return err //nolint:wrapcheck // Sleep only ever returns ctx.Err(), propagated verbatim
		}
	}
}

func (p *Pacer) waitForToken(ctx context.Context, op string) error {
	now := p.clk.Now()

	r := p.limiter.ReserveN(now, 1)
	if !r.OK() {
		// Unreachable: withDefaults guarantees a burst of at least 1.
		return nil
	}

	d := r.DelayFrom(now)
	if d <= 0 {
		return nil
	}

	if err := p.clk.Sleep(ctx, d); err != nil {
		r.CancelAt(p.clk.Now())

		return err //nolint:wrapcheck // Sleep only ever returns ctx.Err(), propagated verbatim
	}

	// Recorded AFTER the sleep, unlike RecordBackoff: a wait cut short by a
	// canceled context was not actually paid, and a counter meant to prove
	// whether MinSleep binds must not be inflated by one.
	p.rec.RecordPacedWait(op, d)

	return nil
}

// succeeded returns the pacer to steady state. This is the instant
// re-acceleration that keeps a single 403 from costing minutes of throughput.
func (p *Pacer) succeeded() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.consecutive = 0
	p.backoffUntil = time.Time{}
}

// enterBackoff moves the shared barrier out by the delay the ladder (or the
// server) dictates for the current consecutive-error count.
func (p *Pacer) enterBackoff(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	d := p.backoffFor(err, p.consecutive)
	p.consecutive++

	if until := p.clk.Now().Add(d); until.After(p.backoffUntil) {
		p.backoffUntil = until
	}
}

func (p *Pacer) backoffRemaining() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.backoffUntil.IsZero() {
		return 0
	}

	return p.backoffUntil.Sub(p.clk.Now())
}

// backoffFor returns the delay for the n-th (0-based) consecutive
// RetryableBackoff error. It touches no mutable pacer state, so it is safe to
// call with or without p.mu held.
func (p *Pacer) backoffFor(err error, n int) time.Duration {
	if de, ok := gdriveerr.AsDriveError(err); ok && de.RetryAfter() > 0 {
		// The server named a delay: honor it verbatim, only raising it to the
		// steady-state interval. Deliberately not capped by MaxSleep.
		return max(de.RetryAfter(), p.cfg.MinSleep)
	}

	// gdriveerr.RetryDelay samples uniformly in [0, gdriveerr.BackoffBase <<
	// attempt) - Google's recommended full jitter - and applies the per-reason
	// floors (sharingRateLimitExceeded recovers in tens of seconds, not
	// hundreds of milliseconds). Full jitter alone can sample near zero, which
	// is too weak for a barrier the whole backend waits behind, so the sample
	// is raised to half its window: the result is rclone's 1s..16s ladder with
	// the top half of each step jittered.
	window := backoffBase << min(n, maxLadderShift)
	floor := min(window/ladderFloorDivisor, p.cfg.MaxSleep)

	return min(max(p.retryDelay(err, n+jitterShift), floor, p.cfg.MinSleep), p.cfg.MaxSleep)
}

// mildBackoff sleeps the short, call-local delay used for RetryableResume.
func (p *Pacer) mildBackoff(ctx context.Context, op string, err error, attempt int) error {
	d := min(max(p.retryDelay(err, attempt), p.cfg.MinSleep), p.cfg.MaxSleep)

	p.rec.RecordBackoff(op, d)

	return p.clk.Sleep(ctx, d) //nolint:wrapcheck // Sleep only ever returns ctx.Err(), propagated verbatim
}

func reasonOf(err error) string {
	if de, ok := gdriveerr.AsDriveError(err); ok {
		return de.Reason()
	}

	return ""
}
