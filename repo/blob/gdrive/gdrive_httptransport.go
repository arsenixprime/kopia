//go:build !no_extra_providers

package gdrive

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// Fallbacks for the (unusual) case of http.DefaultTransport having been
// replaced by something that is not an *http.Transport; they mirror the
// standard library's own DefaultTransport.
const (
	fallbackDialTimeout           = 30 * time.Second
	fallbackKeepAlive             = 30 * time.Second
	fallbackMaxIdleConns          = 100
	fallbackIdleConnTimeout       = 90 * time.Second
	fallbackTLSHandshakeTimeout   = 10 * time.Second
	fallbackExpectContinueTimeout = 1 * time.Second
)

const (
	// minChunkRetryDeadline is googleapi's own default per-chunk retry deadline.
	minChunkRetryDeadline = 32 * time.Second

	// chunkRetryStallMultiple sizes the chunk retry deadline in units of the
	// longest stall-detection bound: one unit for transferring the chunk up to
	// the point where it stalled, one for detecting the stall, so that at least
	// one same-session retry is still allowed after a stall.
	chunkRetryStallMultiple = 2
)

// transportTimeouts are the stall-detection bounds of the backend's HTTP stack.
// None of them limits how long a healthy, progressing transfer may take.
type transportTimeouts struct {
	http2ReadIdle  time.Duration
	http2Ping      time.Duration
	responseHeader time.Duration
	ioIdle         time.Duration
}

// chunkRetryDeadline is the googleapi per-chunk retry deadline for resumable
// uploads; see gdriveStorage.mediaOptions.
func (t transportTimeouts) chunkRetryDeadline() time.Duration {
	stall := max(t.ioIdle, t.responseHeader, t.http2ReadIdle+t.http2Ping)

	return max(minChunkRetryDeadline, chunkRetryStallMultiple*stall)
}

// newHTTPTransport returns the transport every Drive request travels on: the
// standard library's default transport (proxy settings, connection pooling,
// IdleConnTimeout, TLS handshake timeout, HTTP/2 negotiation) plus stall
// detection, which the default lacks entirely. Without it a connection whose
// peer silently disappeared - a laptop resuming from suspend on a new IP, a NAT
// that forgot the flow - blocks a request until the kernel gives up on TCP
// retransmissions, tens of minutes later.
//
// Three independent mechanisms, each turning a stall into a retryable error:
//
//   - HTTP/2 health checks: a connection that has received nothing for
//     http2ReadIdle is pinged and closed if the ping is not answered within
//     http2Ping.
//   - ResponseHeaderTimeout: bounds the wait for the response once the request
//     has been completely sent.
//   - an idle deadline on the raw TCP connection (beneath TLS), pushed forward
//     by every successful read or write; see idleTimeoutConn.
func newHTTPTransport(timeouts transportTimeouts) *http.Transport {
	t := cloneDefaultTransport()

	dial := t.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: fallbackDialTimeout, KeepAlive: fallbackKeepAlive}).DialContext
	}

	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		return newIdleTimeoutConn(conn, timeouts.ioIdle)
	}

	t.ResponseHeaderTimeout = timeouts.responseHeader

	h2 := http.HTTP2Config{}
	if t.HTTP2 != nil {
		h2 = *t.HTTP2
	}

	// SendPingTimeout is the standard library's name for x/net/http2's ReadIdleTimeout.
	h2.SendPingTimeout = timeouts.http2ReadIdle
	h2.PingTimeout = timeouts.http2Ping
	t.HTTP2 = &h2

	return t
}

func cloneDefaultTransport() *http.Transport {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		return dt.Clone()
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          fallbackMaxIdleConns,
		IdleConnTimeout:       fallbackIdleConnTimeout,
		TLSHandshakeTimeout:   fallbackTLSHandshakeTimeout,
		ExpectContinueTimeout: fallbackExpectContinueTimeout,
	}
}

// idleTimeoutConn closes the connection - by failing its pending I/O with
// os.ErrDeadlineExceeded - once nothing has been read from or written to it
// for the idle timeout, the way rclone's fshttp.timeoutConn does.
//
// Activity in EITHER direction extends both deadlines. That matters for an
// HTTP/1.1 upload, where the transport's read loop sits in a Read for the
// whole time the body is being written: a read-only deadline would kill every
// upload that takes longer than the idle timeout.
//
// It wraps the raw TCP connection, beneath TLS, so TLS records and HTTP/2
// control frames (pings included) count as activity. Consequences for
// connection reuse: an idle pooled HTTP/1.1 connection is closed when the
// deadline fires, and an idle HTTP/2 connection is kept alive by its own
// health-check pings until IdleConnTimeout closes it; in both cases the next
// request simply dials a new connection.
//
// Deadlines set by the connection's user (crypto/tls interrupts a canceled
// handshake by setting one in the past) are remembered and always win when
// earlier, so wrapping never defeats them.
type idleTimeoutConn struct {
	net.Conn

	timeout time.Duration

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
}

func newIdleTimeoutConn(conn net.Conn, timeout time.Duration) (net.Conn, error) {
	c := &idleTimeoutConn{Conn: conn, timeout: timeout}

	if err := c.extend(); err != nil {
		conn.Close() //nolint:errcheck

		return nil, err
	}

	return c, nil
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && err == nil {
		err = c.extend()
	}

	return n, err
}

func (c *idleTimeoutConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 && err == nil {
		err = c.extend()
	}

	return n, err
}

func (c *idleTimeoutConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

func (c *idleTimeoutConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.readDeadline = t

	return c.Conn.SetReadDeadline(earliestDeadline(t, c.idleDeadline())) //nolint:wrapcheck // net.Conn contract
}

func (c *idleTimeoutConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.writeDeadline = t

	return c.Conn.SetWriteDeadline(earliestDeadline(t, c.idleDeadline())) //nolint:wrapcheck // net.Conn contract
}

// extend pushes the idle deadline forward. The lock serializes it with the
// Set*Deadline methods, so a deadline set concurrently by the connection's
// user can never be overwritten by a later one computed before it.
func (c *idleTimeoutConn) extend() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	idle := c.idleDeadline()

	return errors.Join(
		c.Conn.SetReadDeadline(earliestDeadline(c.readDeadline, idle)),
		c.Conn.SetWriteDeadline(earliestDeadline(c.writeDeadline, idle)),
	)
}

func (c *idleTimeoutConn) idleDeadline() time.Time {
	if c.timeout <= 0 {
		return time.Time{}
	}

	// Socket deadlines are absolute instants checked by the Go runtime against
	// the real clock. clock.Now can be shifted by the fake time server in tests,
	// which would arm these deadlines in the past or the far future.
	return time.Now().Add(c.timeout) //nolint:forbidigo // see above
}

// earliestDeadline returns the earlier of two deadlines, where the zero time
// means "no deadline".
func earliestDeadline(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case a.Before(b):
		return a
	default:
		return b
	}
}
