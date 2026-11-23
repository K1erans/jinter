package jev

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// h2Origin is an HTTPS, HTTP/2 fake TypeSafe, and a client that trusts it.
func h2Origin(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32, *HTTP) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	c := NewHTTP("k", HTTPOptions{BaseURL: srv.URL, RequestsPerSecond: 1e9, TokensPerSecond: 1e12})
	c.http.Transport.(*firstConnGate).rt.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	return srv, &conns, c
}

func TestABurstOfFirstCallsSharesOneConnection(t *testing.T) {
	var protos sync.Map
	_, conns, c := h2Origin(t, func(w http.ResponseWriter, r *http.Request) {
		protos.Store(r.Proto, true)
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, relayReply)
	})
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Ask(context.Background(), req); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, ok := protos.Load("HTTP/2.0"); !ok {
		t.Fatal("calls didn't use HTTP/2")
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("64 concurrent first calls opened %d connections, want 1", n)
	}
}

func TestCallsWaitingOnTheFirstConnectionCanBeCancelled(t *testing.T) {
	release := make(chan struct{})
	g := newFirstConnGate(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-release // the first call never gets a connection
		return nil, context.Canceled
	}))
	first, _ := http.NewRequest("POST", "https://example.test/", nil)
	go g.RoundTrip(first)
	for {
		g.mu.Lock()
		_, ok := g.hosts["example.test"]
		g.mu.Unlock()
		if ok {
			break
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second, _ := http.NewRequestWithContext(ctx, "POST", "https://example.test/", nil)
	if _, err := g.RoundTrip(second); err != context.Canceled {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	close(release)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
