//go:build !no_extra_providers

package gdrive

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/timetrack"
	"github.com/kopia/kopia/repo/blob/gdrive/gdriveerr"
)

// stallTestTimeout is the stall bound used by the tests that wait one out. It
// is far above anything a healthy localhost exchange needs, even under -race.
const stallTestTimeout = 250 * time.Millisecond

// stallTestDeadline is the generous outer bound on anything that has to wait
// out a stall or two.
const stallTestDeadline = 20 * time.Second

// relaxedTimeouts has every stall bound far out of a test's reach, so that a
// test can tighten exactly the one mechanism it exercises.
func relaxedTimeouts() transportTimeouts {
	return transportTimeouts{
		http2ReadIdle:  time.Minute,
		http2Ping:      time.Minute,
		responseHeader: time.Minute,
		ioIdle:         time.Minute,
	}
}

func TestResolveTuningTransportTimeoutDefaults(t *testing.T) {
	got := resolveTuning(&Options{Tuning: TuningOptions{CacheDir: t.TempDir()}}).timeouts

	require.Equal(t, transportTimeouts{
		http2ReadIdle:  31 * time.Second,
		http2Ping:      15 * time.Second,
		responseHeader: 2 * time.Minute,
		ioIdle:         5 * time.Minute,
	}, got)

	// Long enough for one stall to be detected and the chunk re-sent on the
	// same session; see mediaOptions.
	require.Equal(t, 10*time.Minute, got.chunkRetryDeadline())
}

func TestResolveTuningTransportTimeoutOverrides(t *testing.T) {
	got := resolveTuning(&Options{Tuning: TuningOptions{
		CacheDir:                 t.TempDir(),
		HTTP2ReadIdleTimeoutSec:  1,
		HTTP2PingTimeoutSec:      2,
		ResponseHeaderTimeoutSec: 3,
		IOIdleTimeoutSec:         4,
	}}).timeouts

	require.Equal(t, transportTimeouts{
		http2ReadIdle:  1 * time.Second,
		http2Ping:      2 * time.Second,
		responseHeader: 3 * time.Second,
		ioIdle:         4 * time.Second,
	}, got)

	// Never below googleapi's own default.
	require.Equal(t, minChunkRetryDeadline, got.chunkRetryDeadline())
}

func TestNewHTTPTransportKeepsDefaultTransportSettings(t *testing.T) {
	def, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok)

	tr := newHTTPTransport(resolveTuning(&Options{Tuning: TuningOptions{CacheDir: t.TempDir()}}).timeouts)

	require.True(t, tr.ForceAttemptHTTP2)
	require.NotNil(t, tr.Proxy)
	require.Equal(t, def.IdleConnTimeout, tr.IdleConnTimeout)
	require.Equal(t, def.MaxIdleConns, tr.MaxIdleConns)
	require.Equal(t, def.TLSHandshakeTimeout, tr.TLSHandshakeTimeout)
	require.Equal(t, 2*time.Minute, tr.ResponseHeaderTimeout)
	require.NotNil(t, tr.HTTP2)
	require.Equal(t, 31*time.Second, tr.HTTP2.SendPingTimeout)
	require.Equal(t, 15*time.Second, tr.HTTP2.PingTimeout)

	// The default transport itself must not have been touched.
	require.Zero(t, def.ResponseHeaderTimeout)

	if def.HTTP2 != nil { // net/http populates it lazily with an empty config
		require.Zero(t, def.HTTP2.SendPingTimeout)
		require.Zero(t, def.HTTP2.PingTimeout)
	}
}

// newTLSTestServer starts an HTTP/2-capable TLS server and returns it with a
// production transport that trusts it.
func newTLSTestServer(t *testing.T, h http.Handler, wrap func(net.Listener) net.Listener, timeouts transportTimeouts) (*httptest.Server, *http.Transport) {
	t.Helper()

	srv := httptest.NewUnstartedServer(h)
	if wrap != nil {
		srv.Listener = wrap(srv.Listener)
	}

	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	testClientTransport, ok := srv.Client().Transport.(*http.Transport)
	require.True(t, ok)

	tr := newHTTPTransport(timeouts)
	tr.TLSClientConfig = testClientTransport.TLSClientConfig.Clone()
	t.Cleanup(tr.CloseIdleConnections)

	return srv, tr
}

// TestNewHTTPTransportNegotiatesHTTP2 proves that replacing the dialer and
// setting the HTTP/2 config did not silently downgrade the connection to
// HTTP/1.1, and that connections are dialed through the idle-timeout wrapper.
func TestNewHTTPTransportNegotiatesHTTP2(t *testing.T) {
	var accepted atomic.Int32

	srv, tr := newTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok") //nolint:errcheck
	}), func(l net.Listener) net.Listener {
		return &countingListener{Listener: l, accepted: &accepted}
	}, relaxedTimeouts())

	dial := tr.DialContext

	var wrapped atomic.Bool

	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		_, ok := c.(*idleTimeoutConn)
		wrapped.Store(ok)

		return c, err
	}

	client := &http.Client{Transport: tr}

	for range 3 {
		require.Equal(t, 2, protoMajorOf(t, client, srv.URL), "HTTP/2 must still be negotiated through the production transport")
	}

	require.True(t, wrapped.Load(), "the raw connection must be wrapped by idleTimeoutConn")
	require.EqualValues(t, 1, accepted.Load(), "HTTP/2 requests must share one connection")
}

// TestNewHTTPTransportHTTP2HealthCheckDetectsDeadPeer freezes the server end of
// an established HTTP/2 connection - it reads and discards everything and
// writes nothing, which is what a peer that vanished during a suspend looks
// like - and checks that the client's health-check ping, not any other
// timeout, fails the request with a retryable error.
func TestNewHTTPTransportHTTP2HealthCheckDetectsDeadPeer(t *testing.T) {
	fl := &freezableListener{release: make(chan struct{})}

	timeouts := relaxedTimeouts()
	timeouts.http2ReadIdle = stallTestTimeout
	timeouts.http2Ping = stallTestTimeout

	srv, tr := newTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok") //nolint:errcheck
	}), func(l net.Listener) net.Listener {
		fl.Listener = l
		return fl
	}, timeouts)

	t.Cleanup(func() { close(fl.release) })

	client := &http.Client{Transport: tr}

	require.Equal(t, 2, protoMajorOf(t, client, srv.URL))

	fl.frozen.Store(true)

	ctx := t.Context()
	timer := timetrack.StartTimer()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req) //nolint:bodyclose
	if resp != nil {
		resp.Body.Close() //nolint:errcheck
	}

	elapsed := timer.Elapsed()

	require.Error(t, err)
	require.ErrorContains(t, err, "http2: client connection lost")
	require.Less(t, elapsed, stallTestDeadline)

	de, ok := gdriveerr.AsDriveError(gdriveerr.ClassifyContext(ctx, err, "GetBlob", "b"))
	require.True(t, ok)
	require.Equal(t, gdriveerr.RetryableResume, de.Disposition())
}

func TestIdleTimeoutConnClosesSilentConnection(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { server.Close() }) //nolint:errcheck

	c, err := newIdleTimeoutConn(client, stallTestTimeout)
	require.NoError(t, err)

	t.Cleanup(func() { c.Close() }) //nolint:errcheck

	timer := timetrack.StartTimer()
	_, err = c.Read(make([]byte, 1))

	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	require.GreaterOrEqual(t, timer.Elapsed(), stallTestTimeout)
	require.Less(t, timer.Elapsed(), stallTestDeadline)

	de, ok := gdriveerr.AsDriveError(gdriveerr.ClassifyContext(t.Context(), err, "GetBlob", "b"))
	require.True(t, ok)
	require.Equal(t, gdriveerr.RetryableResume, de.Disposition())
}

func TestIdleTimeoutConnActivityExtendsDeadline(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { server.Close() }) //nolint:errcheck

	c, err := newIdleTimeoutConn(client, stallTestTimeout)
	require.NoError(t, err)

	t.Cleanup(func() { c.Close() }) //nolint:errcheck

	// Trickle bytes for several idle periods; every read must succeed because
	// every byte pushes the deadline forward.
	const steps = 8

	go func() {
		for range steps {
			time.Sleep(stallTestTimeout / 4) //nolint:forbidigo
			server.Write([]byte{1})          //nolint:errcheck
		}
	}()

	timer := timetrack.StartTimer()

	for range steps {
		_, err := c.Read(make([]byte, 1))
		require.NoError(t, err)
	}

	require.Greater(t, timer.Elapsed(), stallTestTimeout, "the reads must have spanned more than one idle period")
}

func TestIdleTimeoutConnHonorsEarlierCallerDeadline(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { server.Close() }) //nolint:errcheck

	c, err := newIdleTimeoutConn(client, time.Hour)
	require.NoError(t, err)

	t.Cleanup(func() { c.Close() }) //nolint:errcheck

	// crypto/tls interrupts a canceled handshake exactly like this.
	require.NoError(t, c.SetDeadline(clock.Now().Add(-time.Second)))

	go server.Write([]byte{1, 2}) //nolint:errcheck

	_, err = c.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "activity must not push a caller's deadline back")
}

// protoMajorOf makes a successful GET and returns the HTTP major version it
// traveled over.
func protoMajorOf(t *testing.T, client *http.Client, url string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)

	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return resp.ProtoMajor
}

type countingListener struct {
	net.Listener

	accepted *atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}

	return c, err //nolint:wrapcheck
}

// freezableListener hands out connections that, once frozen, silently swallow
// everything they receive and never send anything again.
type freezableListener struct {
	net.Listener

	frozen  atomic.Bool
	release chan struct{}
}

func (l *freezableListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	return &freezableConn{Conn: c, l: l}, nil
}

type freezableConn struct {
	net.Conn

	l *freezableListener
}

func (c *freezableConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.l.frozen.Load() {
		<-c.l.release
		return 0, net.ErrClosed
	}

	return n, err //nolint:wrapcheck
}

func (c *freezableConn) Write(b []byte) (int, error) {
	if c.l.frozen.Load() {
		<-c.l.release
		return 0, net.ErrClosed
	}

	return c.Conn.Write(b) //nolint:wrapcheck
}
