package jinter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/jev"
	"github.com/K1erans/jinter/jev/jevtest"
)

// project writes a small non-git source tree and moves into it.
func project(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

var billing = map[string]string{
	"src/billing/retry.go":      "package billing\n\n// RetryFailedPayment charges again.\nfunc RetryFailedPayment(id string) error {\n\treturn charge(id)\n}\n",
	"src/billing/retry_test.go": "package billing\n\nfunc TestRetry(t *testing.T) { charge(\"x\") }\n",
	"src/mail/send.go":          "package mail\n\nfunc SendReceipt(to string) error {\n\treturn smtp(to)\n}\n",
}

func scorer() *jevtest.Fake {
	return &jevtest.Fake{Score: jevtest.Contains(map[string]string{
		"retries failed payments":       "charge",
		"sends email":                   "smtp",
		"parses yaml":                   "yaml",
		"the tests for payment retries": "TestRetry",
	})}
}

// engine shares one in-memory cache across searches.
func engine(client jev.Client) (*Engine, *cache.Memory) {
	store := cache.NewMemory()
	return &Engine{
		Client: client,
		OpenCache: func(string, string) (cache.Cache, func() error, error) {
			return store, func() error { return nil }, nil
		},
	}, store
}

func TestSearchRanksMatchesForEveryQuestion(t *testing.T) {
	project(t, billing)
	e, _ := engine(scorer())
	rep, err := e.Search(context.Background(), Query{
		Questions: []string{"retries failed payments", "sends email", "parses yaml"},
		Threshold: 0.6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Matched() || len(rep.Results) != 3 {
		t.Fatalf("report %+v", rep)
	}
	m := rep.Results[0].Matches
	if len(m) != 1 || m[0].Path != "src/billing/retry.go" || m[0].Start != 1 || m[0].End != 6 || m[0].Probability != 0.9 {
		t.Fatalf("payments: %+v", m)
	}
	if m := rep.Results[1].Matches; len(m) != 1 || m[0].Path != "src/mail/send.go" {
		t.Fatalf("email: %+v", m)
	}
	if r := rep.Results[2]; len(r.Matches) != 0 || r.Best == nil || r.Best.Probability != 0.1 {
		t.Fatalf("yaml: %+v", r)
	}
	if rep.Tests != 1 {
		t.Fatalf("tests skipped %d", rep.Tests)
	}
}

func TestTestsSearchesTestFiles(t *testing.T) {
	project(t, billing)
	e, _ := engine(scorer())
	rep, err := e.Search(context.Background(), Query{Questions: []string{"the tests for payment retries"}, Threshold: 0.6, Tests: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tests != 0 {
		t.Fatalf("report %+v", rep)
	}
	if m := rep.Results[0].Matches; len(m) != 1 || m[0].Path != "src/billing/retry_test.go" {
		t.Fatalf("matches %+v", m)
	}
}

func TestRepeatSearchIsServedFromCacheWithoutAClient(t *testing.T) {
	project(t, billing)
	e, store := engine(scorer())
	q := Query{Questions: []string{"retries failed payments"}, Threshold: 0.6}
	if _, err := e.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	cached := &Engine{OpenCache: func(string, string) (cache.Cache, func() error, error) {
		return store, func() error { return nil }, nil
	}}
	asked := false
	q.OnAsk = func(int) { asked = true }
	rep, err := cached.Search(context.Background(), q)
	if err != nil || asked || !rep.Matched() {
		t.Fatalf("err %v, asked %v, report %+v", err, asked, rep)
	}
	q.Questions = []string{"sends email"}
	if _, err := cached.Search(context.Background(), q); err == nil || !strings.Contains(err.Error(), "no Jev client") {
		t.Fatalf("uncached search without a client: %v", err)
	}
}

func TestUnusableCacheIsReportedButSearches(t *testing.T) {
	project(t, billing)
	e := &Engine{Client: scorer(), OpenCache: func(string, string) (cache.Cache, func() error, error) {
		return nil, nil, errors.New("disk full")
	}}
	var cacheErr error
	rep, err := e.Search(context.Background(), Query{
		Questions:    []string{"retries failed payments"},
		Threshold:    0.6,
		OnCacheError: func(err error) { cacheErr = err },
	})
	if err != nil || !rep.Matched() {
		t.Fatalf("err %v, report %+v", err, rep)
	}
	if cacheErr == nil || !strings.Contains(cacheErr.Error(), "disk full") {
		t.Fatalf("cache error %v", cacheErr)
	}
}

func TestJevErrorsAreReturned(t *testing.T) {
	project(t, billing)
	e, _ := engine(&jevtest.Fake{Err: errors.New("401 invalid API key")})
	if _, err := e.Search(context.Background(), Query{Questions: []string{"retries failed payments"}}); err == nil || !strings.Contains(err.Error(), "invalid API key") {
		t.Fatalf("err %v", err)
	}
}
