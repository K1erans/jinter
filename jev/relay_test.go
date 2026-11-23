package jev

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const relayReply = `{"model":"jev-1.13.0","answers":{"q0":{"type":"noul","noul":0.9},"q1":{"type":"noul","noul":0.1}}}`

// origin is a fake TypeSafe that counts calls and the connections they
// arrive on.
type origin struct {
	*httptest.Server
	calls, conns atomic.Int32
	mu           sync.Mutex
	auths        map[string]map[string]bool // Authorization -> remote addrs
}

func newOrigin(t *testing.T, handler http.HandlerFunc) *origin {
	t.Helper()
	o := &origin{auths: map[string]map[string]bool{}}
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, relayReply) }
	}
	o.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.calls.Add(1)
		o.mu.Lock()
		a := r.Header.Get("Authorization")
		if o.auths[a] == nil {
			o.auths[a] = map[string]bool{}
		}
		o.auths[a][r.RemoteAddr] = true
		o.mu.Unlock()
		handler(w, r)
	}))
	o.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			o.conns.Add(1)
		}
	}
	o.Start()
	t.Cleanup(o.Close)
	return o
}

// startRelay serves a relay on a local socket and returns a dialer for it
// and a count of the dials.
func startRelay(t *testing.T, opts RelayOptions) (dial func(context.Context) (net.Conn, error), dials *atomic.Int32, stop func() error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeRelay(ctx, ln, opts) }()
	var once sync.Once
	var serveErr error
	stop = func() error {
		once.Do(func() { cancel(); serveErr = <-done })
		return serveErr
	}
	t.Cleanup(func() { stop() })
	dials = &atomic.Int32{}
	addr := ln.Addr().String()
	dial = func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	return dial, dials, stop
}

func ask(t *testing.T, c *HTTP) {
	t.Helper()
	resp, err := c.Ask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "jev-1.13.0" || resp.Nouls[0][0] != 0.9 || resp.Nouls[0][1] != 0.1 {
		t.Fatalf("got %+v", resp)
	}
}

func TestRelayedCallsShareConnectionsAcrossClients(t *testing.T) {
	var gotPath, gotUA, gotType, gotUpstream, gotBody string
	o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotUA, gotType, gotUpstream, gotBody = r.URL.Path, r.UserAgent(), r.Header.Get("Content-Type"), r.Header.Get(relayUpstreamHeader), string(b)
		io.WriteString(w, relayReply)
	})
	dial, dials, _ := startRelay(t, RelayOptions{})
	// Three clients stand in for three jrep processes, one after another.
	for i := 0; i < 3; i++ {
		c := NewHTTP("k", HTTPOptions{BaseURL: o.URL, Relay: dial})
		ask(t, c)
		c.http.CloseIdleConnections() // the process exits
	}
	if o.calls.Load() != 3 || o.conns.Load() != 1 {
		t.Fatalf("%d calls on %d origin connections, want 3 on 1", o.calls.Load(), o.conns.Load())
	}
	if dials.Load() != 3 {
		t.Fatalf("%d relay dials, want 3", dials.Load())
	}
	payload, _ := encodeRequest(req)
	if gotPath != "/v1/systemone" || gotUA != "jinter" || gotType != "application/json" || gotUpstream != "" || gotBody != string(payload) {
		t.Fatalf("origin saw path %q ua %q type %q upstream header %q body %q", gotPath, gotUA, gotType, gotUpstream, gotBody)
	}
}

func TestRelayKeepsKeysOnSeparateConnections(t *testing.T) {
	o := newOrigin(t, nil)
	dial, _, _ := startRelay(t, RelayOptions{})
	for _, key := range []string{"a", "b", "a"} {
		ask(t, NewHTTP(key, HTTPOptions{BaseURL: o.URL, Relay: dial}))
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	a, b := o.auths["Bearer a"], o.auths["Bearer b"]
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("key a on %d connections, b on %d; want 1 each", len(a), len(b))
	}
	for addr := range a {
		if b[addr] {
			t.Fatalf("keys a and b shared connection %s", addr)
		}
	}
}

func TestRelayPassesCompressedRepliesThrough(t *testing.T) {
	o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			io.WriteString(w, relayReply)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		io.WriteString(zw, relayReply)
		zw.Close()
	})
	dial, _, _ := startRelay(t, RelayOptions{})
	ask(t, NewHTTP("k", HTTPOptions{BaseURL: o.URL, Relay: dial}))
}

func TestUnreachableRelayFallsBackDirectForGood(t *testing.T) {
	o := newOrigin(t, nil)
	var dials atomic.Int32
	dial := func(context.Context) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("no relay")
	}
	c := NewHTTP("k", HTTPOptions{BaseURL: o.URL, Relay: dial})
	for i := 0; i < 3; i++ {
		ask(t, c)
	}
	if dials.Load() != 1 || o.calls.Load() != 3 {
		t.Fatalf("%d relay dials, %d calls; want 1 and 3", dials.Load(), o.calls.Load())
	}
}

func TestRelayThatDiesMidCallIsRetriedDirectAtOnce(t *testing.T) {
	o := newOrigin(t, nil)
	// A "relay" that reads the request and hangs up without answering.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Read(make([]byte, 64<<10))
			conn.Close()
		}
	}()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}
	c := NewHTTP("k", HTTPOptions{BaseURL: o.URL, Relay: dial, BaseBackoff: time.Hour})
	start := time.Now()
	ask(t, c)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v: waited out a backoff", d)
	}
	if !c.http.Transport.(*relayTransport).down.Load() {
		t.Fatal("relay still in use after failing")
	}
}

func TestOriginErrorsThroughTheRelayReadLikeDirectOnes(t *testing.T) {
	// A port with nothing listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()
	dial, _, _ := startRelay(t, RelayOptions{})
	opts := HTTPOptions{BaseURL: dead, MaxRetries: -1}
	_, direct := NewHTTP("k", opts).Ask(context.Background(), req)
	opts.Relay = dial
	c := NewHTTP("k", opts)
	_, relayed := c.Ask(context.Background(), req)
	if direct == nil || relayed == nil {
		t.Fatalf("direct %v, relayed %v; want errors", direct, relayed)
	}
	if direct.Error() != relayed.Error() {
		t.Fatalf("direct: %v\nrelayed: %v", direct, relayed)
	}
	if c.http.Transport.(*relayTransport).down.Load() {
		t.Fatal("a working relay was dropped because the origin was down")
	}
}

func TestCancellingARelayedCallCancelsItAtTheOrigin(t *testing.T) {
	arrived, cancelled := make(chan struct{}), make(chan struct{})
	o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) // so the server watches for the client going away
		close(arrived)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
		}
	})
	dial, _, _ := startRelay(t, RelayOptions{})
	c := NewHTTP("k", HTTPOptions{BaseURL: o.URL, Relay: dial})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := c.Ask(ctx, req); errc <- err }()
	<-arrived
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("origin call still running after the client cancelled")
	}
}

func TestRelayStopsWhenIdle(t *testing.T) {
	o := newOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond) // a call in flight keeps it up
		io.WriteString(w, relayReply)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- ServeRelay(context.Background(), ln, RelayOptions{IdleTimeout: 500 * time.Millisecond})
	}()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}
	ask(t, NewHTTP("k", HTTPOptions{BaseURL: o.URL, Relay: dial}))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d < 1500*time.Millisecond {
			t.Fatalf("stopped after %v, during a call", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay still running")
	}
	if _, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		t.Fatal("listener still open")
	}
}

func TestCallTheRelayRefusesGoesDirect(t *testing.T) {
	o := newOrigin(t, nil)
	// A relay that refuses every call, as one from another version might.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(relayErrorHeader, "1")
		http.Error(w, "unknown protocol", http.StatusBadRequest)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}
	c := NewHTTP("k", HTTPOptions{BaseURL: o.URL, Relay: dial, BaseBackoff: time.Hour})
	ask(t, c)
	ask(t, c)
	if o.calls.Load() != 2 || !c.http.Transport.(*relayTransport).down.Load() {
		t.Fatalf("%d calls reached the origin; relay down %v", o.calls.Load(), c.http.Transport.(*relayTransport).down.Load())
	}
}
