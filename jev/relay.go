package jev

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// A relay lets short-lived processes share long-lived connections to
// TypeSafe. Each process would otherwise pay a TCP and TLS handshake (two
// round trips, ~50-120 ms) before its first call. The relay is a small
// HTTP proxy, reached over a local socket the caller dials (see
// HTTPOptions.Relay): calls reach it as plain HTTP and leave over its own
// pool of HTTPS connections, which stay open between processes.
//
// The relay holds no state of its own beyond those connections. The API
// key, base URL, rate limits and cache all stay in the calling process:
// each call carries its key and destination, and calls with different
// keys never share an upstream connection.

const (
	// relayUpstreamHeader names the origin (scheme://host[:port]) a call
	// sent to the relay is for.
	relayUpstreamHeader = "Jinter-Relay-Upstream"
	// relayErrorHeader marks a reply the relay wrote itself because it
	// couldn't reach the origin. Its body is the error.
	relayErrorHeader = "Jinter-Relay-Error"
	// relayHost is the placeholder host of calls sent to the relay.
	relayHost = "jinter-relay"
	// maxRelayBody caps a call's body, which the relay holds in memory so
	// its transport can resend it on a connection that turns out closed.
	maxRelayBody = 64 << 20
)

// relayDialError is a failure to reach the relay at all, as opposed to a
// relay that answered.
type relayDialError struct{ err error }

func (e *relayDialError) Error() string { return "jev relay: " + e.err.Error() }
func (e *relayDialError) Unwrap() error { return e.err }

// relayTransport sends calls through the relay while it works and
// directly otherwise. The first time the relay can't be reached, or
// fails during a call, every later call in this process goes direct; the
// failed call is resent direct at once rather than waiting out a retry.
type relayTransport struct {
	relay  *http.Transport
	direct http.RoundTripper
	down   atomic.Bool
}

func newRelayTransport(dial func(context.Context) (net.Conn, error), direct http.RoundTripper) *relayTransport {
	return &relayTransport{
		relay: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := dial(ctx)
				if err != nil {
					return nil, &relayDialError{err}
				}
				return c, nil
			},
			MaxIdleConns:        maxIdleConns,
			MaxIdleConnsPerHost: maxIdleConns,
			IdleConnTimeout:     90 * time.Second,
			WriteBufferSize:     32 << 10,
			ReadBufferSize:      16 << 10,
		},
		direct: direct,
	}
}

func (t *relayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.down.Load() {
		return t.direct.RoundTrip(req)
	}
	resp, err := t.relay.RoundTrip(relayRequest(req))
	if err == nil {
		if resp.Header.Get(relayErrorHeader) == "" {
			return resp, nil
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusBadGateway {
			// The relay works but couldn't reach the origin: report its
			// error as a direct call would have. jev retries it like any
			// network error.
			return nil, errors.New(string(msg))
		}
		// The relay refused the call itself (a relay from another version,
		// say): don't retry through it.
		err = errors.New(string(msg))
	} else if req.Context().Err() != nil {
		return nil, err
	}
	// The relay is gone, broke mid-call or refused it. Stop using it and
	// resend direct.
	t.down.Store(true)
	t.relay.CloseIdleConnections()
	if req.Body == nil || req.Body == http.NoBody {
		return t.direct.RoundTrip(req)
	}
	if req.GetBody == nil {
		return nil, err
	}
	body, gerr := req.GetBody()
	if gerr != nil {
		return nil, err
	}
	retry := req.Clone(req.Context())
	retry.Body = body
	return t.direct.RoundTrip(retry)
}

func (t *relayTransport) CloseIdleConnections() {
	t.relay.CloseIdleConnections()
	if c, ok := t.direct.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// transportFor is the client's transport: direct, or through the relay
// opts.Relay dials.
func transportFor(opts HTTPOptions) http.RoundTripper {
	t := newFirstConnGate(newTransport())
	if opts.Relay == nil {
		return t
	}
	return newRelayTransport(opts.Relay, t)
}

// relayRequest is req addressed to the relay, naming its real origin.
func relayRequest(req *http.Request) *http.Request {
	out := req.Clone(req.Context())
	out.URL = &url.URL{Scheme: "http", Host: relayHost, Path: req.URL.Path, RawPath: req.URL.RawPath, RawQuery: req.URL.RawQuery}
	out.Host = ""
	out.Header.Set(relayUpstreamHeader, req.URL.Scheme+"://"+req.URL.Host)
	return out
}

// RelayOptions tune ServeRelay. Zero values take the defaults.
type RelayOptions struct {
	// IdleTimeout ends ServeRelay once no call has been in flight for
	// this long; default 10 minutes. Wall-clock time counts, so a machine
	// that slept through it ends the relay when it wakes.
	IdleTimeout time.Duration
}

// ServeRelay answers calls sent through HTTPOptions.Relay on ln until ctx
// ends or the relay has been idle for opts.IdleTimeout. It closes ln
// before returning.
//
// ln must be reachable only by the user whose key the calls carry (a unix
// socket in a folder only they can open): the relay forwards calls to
// any origin they name.
func ServeRelay(ctx context.Context, ln net.Listener, opts RelayOptions) error {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 10 * time.Minute
	}
	r := &relay{idle: opts.IdleTimeout, transports: map[[32]byte]*firstConnGate{}}
	r.touch()
	srv := &http.Server{
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	r.watch(ctx, done)
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	err := srv.Shutdown(sctx)
	r.closeIdle()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// watch returns when ctx ends, the server stops, or the relay has been
// idle for its timeout.
func (r *relay) watch(ctx context.Context, done <-chan error) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case now := <-tick.C:
			// Wall-clock time far past the monotonic clock means the
			// machine slept: its connections are likely dead, so drop them
			// rather than let the next call hang on one.
			if now.Round(0).Sub(last.Round(0)) > now.Sub(last)+5*time.Second {
				r.closeIdle()
			}
			last = now
			if r.idleFor(now) {
				return
			}
		}
	}
}

type relay struct {
	idle     time.Duration
	active   atomic.Int64
	lastUsed atomic.Int64 // wall-clock unix nanos

	mu         sync.Mutex
	transports map[[32]byte]*firstConnGate // by Authorization header
}

func (r *relay) touch() { r.lastUsed.Store(time.Now().UnixNano()) }

func (r *relay) idleFor(now time.Time) bool {
	return r.active.Load() == 0 && now.UnixNano()-r.lastUsed.Load() > int64(r.idle)
}

// transport is the connection pool for one API key, so calls with
// different keys never share a connection.
func (r *relay) transport(auth string) *firstConnGate {
	k := sha256.Sum256([]byte(auth))
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.transports[k]
	if g == nil {
		t, ok := newTransport().(*http.Transport)
		if !ok {
			t = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
		}
		t.IdleConnTimeout = r.idle
		// Ping idle HTTP/2 connections so a dead one (a changed network)
		// is found and dropped within ~20 s rather than on the next call.
		t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 5 * time.Second}
		g = newFirstConnGate(t)
		r.transports[k] = g
	}
	return g
}

func (r *relay) closeIdle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.transports {
		t.CloseIdleConnections()
	}
}

func (r *relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.active.Add(1)
	defer func() {
		r.touch()
		r.active.Add(-1)
	}()
	r.touch()

	fail := func(status int, err error) {
		w.Header().Set(relayErrorHeader, "1")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		io.WriteString(w, err.Error())
	}
	up, err := url.Parse(req.Header.Get(relayUpstreamHeader))
	if err != nil || (up.Scheme != "http" && up.Scheme != "https") || up.Host == "" || up.User != nil || (up.Path != "" && up.Path != "/") {
		fail(http.StatusBadRequest, fmt.Errorf("jev relay: bad %s %q", relayUpstreamHeader, req.Header.Get(relayUpstreamHeader)))
		return
	}
	// Hold the body so the transport can resend it by itself when a pooled
	// connection turns out to be closed.
	if req.Body != nil && req.Body != http.NoBody {
		body, err := io.ReadAll(io.LimitReader(req.Body, maxRelayBody+1))
		if err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		if len(body) > maxRelayBody {
			fail(http.StatusRequestEntityTooLarge, errors.New("jev relay: request body too large"))
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		req.ContentLength = int64(len(body))
	}
	up.Path = ""
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Header.Del(relayUpstreamHeader)
		},
		Transport: r.transport(req.Header.Get("Authorization")),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			fail(http.StatusBadGateway, err)
		},
	}
	p.ServeHTTP(w, req)
}
