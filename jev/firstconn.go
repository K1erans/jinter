package jev

import (
	"net/http"
	"net/http/httptrace"
	"sync"
)

// firstConnGate holds back calls to an HTTPS origin until the first call
// has a connection, so a burst of calls shares that one HTTP/2 connection.
//
// net/http dials once per waiting call when no connection is open: search
// starts up to 128 calls at once, and each paid its own TCP and TLS
// handshake (measured: 24 concurrent first calls to api.typesafe.ai made
// 23 handshakes, each fetching the ~5 KB certificate chain). Waiting costs
// the held-back calls nothing over HTTP/2, since they would have spent the
// time on their own handshakes; an HTTP/1.1 origin delays them by one
// handshake, once.
type firstConnGate struct {
	rt http.RoundTripper

	mu    sync.Mutex
	hosts map[string]chan struct{} // closed once the host's first call has a connection
}

func newFirstConnGate(rt http.RoundTripper) *firstConnGate {
	return &firstConnGate{rt: rt, hosts: map[string]chan struct{}{}}
}

func (g *firstConnGate) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return g.rt.RoundTrip(req)
	}
	g.mu.Lock()
	ready, started := g.hosts[req.URL.Host]
	if !started {
		ready = make(chan struct{})
		g.hosts[req.URL.Host] = ready
	}
	g.mu.Unlock()

	if started {
		select {
		case <-ready:
		case <-req.Context().Done():
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, req.Context().Err()
		}
		return g.rt.RoundTrip(req)
	}
	// This call goes first. Let the others go once it has a connection,
	// or has failed to get one.
	var once sync.Once
	open := func() { once.Do(func() { close(ready) }) }
	ctx := httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { open() },
	})
	defer open()
	return g.rt.RoundTrip(req.WithContext(ctx))
}

// CloseIdleConnections closes idle connections and re-arms the gate for
// the next burst.
func (g *firstConnGate) CloseIdleConnections() {
	if c, ok := g.rt.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
	g.mu.Lock()
	for host, ready := range g.hosts {
		select {
		case <-ready:
			delete(g.hosts, host)
		default: // a first call still dialing keeps its gate
		}
	}
	g.mu.Unlock()
}
