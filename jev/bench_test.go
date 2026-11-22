package jev

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// A packed request near search's 24 KiB pack limit: three chunks of Go,
// five questions each.
func benchRequest() Request {
	code := strings.Repeat("func f(a, b int) bool {\n\tif a < b && b > 0 {\n\t\treturn true\n\t}\n\treturn false\n}\n", 120)
	qs := []string{"retries failed payments", "sends email to customers", "parses YAML configuration files", "opens a database transaction", "validates user input"}
	return Request{Model: "jev-1.13.0", Path: "src/billing/retry.go", Chunks: []Chunk{{code, qs}, {code, qs}, {code, qs}}}
}

func BenchmarkEncodeRequest(b *testing.B) {
	r := benchRequest()
	b.ReportAllocs()
	for b.Loop() {
		encodeRequest(r)
	}
}

// BenchmarkAskParallel runs search's 128 concurrent calls (rounded up to a
// multiple of GOMAXPROCS) against a local HTTP/1.1 TLS server, where every
// redial costs a handshake.
func BenchmarkAskParallel(b *testing.B) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		ok(w)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer srv.Close()
	c := NewHTTP("k", HTTPOptions{BaseURL: srv.URL, RequestsPerSecond: 1e9, TokensPerSecond: 1e12})
	c.http.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	procs := runtime.GOMAXPROCS(0)
	b.SetParallelism((128 + procs - 1) / procs)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := c.Ask(context.Background(), req); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
