package jinter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/chunk"
	"github.com/K1erans/jinter/jev"
	"github.com/K1erans/jinter/jev/jevtest"
	"github.com/K1erans/jinter/prefilter"
	"github.com/K1erans/jinter/search"
	"github.com/K1erans/jinter/walk"
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
	if rep.Tests != 1 || rep.TestsIncluded {
		t.Fatalf("tests skipped %d, included %v", rep.Tests, rep.TestsIncluded)
	}
}

func TestAQuestionAboutTestsSearchesTestFiles(t *testing.T) {
	project(t, billing)
	e, _ := engine(scorer())
	rep, err := e.Search(context.Background(), Query{Questions: []string{"the tests for payment retries"}, Threshold: 0.6})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.TestsIncluded || rep.Tests != 0 {
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

func TestPrepassAndRefineNarrowWhatJevIsAskedAbout(t *testing.T) {
	project(t, billing)
	fake := scorer()
	e, _ := engine(fake)
	ctx := context.Background()

	rep, err := e.Search(ctx, Query{Questions: []string{"sends email"}, Prepass: true, Keywords: "smtp"})
	if err != nil || rep.Chunks != 1 || rep.Split != 2 || rep.Keywords != "smtp" {
		t.Fatalf("prepass: err %v, chunks %d of %d, keywords %q", err, rep.Chunks, rep.Split, rep.Keywords)
	}

	if _, err := e.Search(ctx, Query{Questions: []string{"retries failed payments"}}); err != nil {
		t.Fatal(err)
	}
	rep, err = e.Search(ctx, Query{Questions: []string{"charges a failed payment again"}, Refine: "retries failed payments", RefineTop: 1})
	if err != nil || rep.Chunks != 1 {
		t.Fatalf("refine: err %v, chunks %d", err, rep.Chunks)
	}
	last := fake.Requests()[len(fake.Requests())-1]
	if last.Path != "src/billing/retry.go" {
		t.Fatalf("refine asked about %s", last.Path)
	}

	_, err = e.Search(ctx, Query{Questions: []string{"x"}, Refine: "never asked"})
	if !errors.Is(err, ErrNotAsked) {
		t.Fatalf("refine by an unasked question: %v", err)
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

func TestAboutTests(t *testing.T) {
	for q, want := range map[string]bool{
		"the test that covers retries":    true,
		"tests for the payment client":    true,
		"tests whether the token expired": false,
		"testing if a port is free":       false,
		"test helpers that build users":   true,
		"mocks the payment client":        true,
		"loads fixtures for the db":       true,
		"retries failed payments":         false,
		"parses the openapi spec":         false,
		"latest attestation is verified":  false,
		"where retry_test checks backoff": true,
	} {
		if got := AboutTests([]string{q}); got != want {
			t.Errorf("AboutTests(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestSkippedFilesAreReportedBeforeJevIsAsked(t *testing.T) {
	project(t, billing)
	e, _ := engine(scorer())
	var events []string
	_, err := e.Search(context.Background(), Query{
		Questions: []string{"retries failed payments"},
		OnFiles: func(r Report) {
			events = append(events, fmt.Sprintf("files: %d tests skipped", r.Tests))
		},
		OnAsk: func(n int) { events = append(events, fmt.Sprintf("ask: %d", n)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"files: 1 tests skipped", "ask: 2"}; !slices.Equal(events, want) {
		t.Fatalf("got %q, want %q", events, want)
	}
}

func TestUnreadableFilesAreReportedAndTheRestSearched(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files without permission")
	}
	for _, inGit := range []bool{false, true} {
		t.Run(map[bool]string{false: "outside git", true: "in git"}[inGit], func(t *testing.T) {
			project(t, billing)
			if inGit {
				cmd := exec.Command("git", "init", "-q")
				cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init: %v\n%s", err, out)
				}
			}
			secret := filepath.Join("src", "billing", "secret.go")
			if err := os.WriteFile(secret, []byte("package billing\n\nfunc charge(id string) error { return nil }\n"), 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(secret, 0o644) })

			e, _ := engine(scorer())
			var heard []walk.Unreadable
			rep, err := e.Search(context.Background(), Query{
				Questions: []string{"retries failed payments"},
				Threshold: 0.6,
				OnFiles:   func(r Report) { heard = r.Unreadable },
			})
			if err != nil {
				t.Fatalf("an unreadable file must not fail the search: %v", err)
			}
			if m := rep.Results[0].Matches; len(m) != 1 || m[0].Path != "src/billing/retry.go" {
				t.Fatalf("the readable files are still searched: %+v", m)
			}
			for name, got := range map[string][]walk.Unreadable{"report": rep.Unreadable, "OnFiles": heard} {
				if len(got) != 1 || got[0].Path != secret || got[0].Rel != "src/billing/secret.go" || !errors.Is(got[0].Err, fs.ErrPermission) {
					t.Errorf("%s: unreadable %+v", name, got)
				}
			}

			if _, err := e.Search(context.Background(), Query{Questions: []string{"retries failed payments"}, Paths: []string{secret}}); !errors.Is(err, fs.ErrPermission) {
				t.Errorf("a named unreadable file: got %v, want a permission error", err)
			}
		})
	}
}

func TestBlankQuestionsAreRejected(t *testing.T) {
	project(t, billing)
	fake := scorer()
	e, _ := engine(fake)
	if _, err := e.Search(context.Background(), Query{Questions: []string{"", "  \t"}}); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(fake.Requests()); n != 0 {
		t.Fatalf("made %d calls", n)
	}
}

func TestNegativeRefineTopTakesTheDefault(t *testing.T) {
	project(t, billing)
	e, _ := engine(scorer())
	ctx := context.Background()
	if _, err := e.Search(ctx, Query{Questions: []string{"retries failed payments"}}); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Search(ctx, Query{Questions: []string{"x"}, Refine: "retries failed payments", RefineTop: -1})
	if err != nil || rep.Chunks != 2 {
		t.Fatalf("err %v, chunks %d", err, rep.Chunks)
	}
}

func TestACancelledSearchAsksNothing(t *testing.T) {
	project(t, billing)
	fake := scorer()
	e, _ := engine(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Search(ctx, Query{Questions: []string{"retries failed payments"}, Prepass: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if n := len(fake.Requests()); n != 0 {
		t.Fatalf("made %d calls", n)
	}
}

func TestAFailedCacheCloseIsReported(t *testing.T) {
	project(t, billing)
	e := &Engine{Client: scorer(), OpenCache: func(string, string) (cache.Cache, func() error, error) {
		return cache.NewMemory(), func() error { return errors.New("close failed") }, nil
	}}
	var cacheErr error
	if _, err := e.Search(context.Background(), Query{
		Questions:    []string{"retries failed payments"},
		OnCacheError: func(err error) { cacheErr = err },
	}); err != nil {
		t.Fatal(err)
	}
	if cacheErr == nil || !strings.Contains(cacheErr.Error(), "close failed") {
		t.Fatalf("cache error %v", cacheErr)
	}
}

func TestPrepassKeepsMatchingChunksInOrder(t *testing.T) {
	f, err := prefilter.New("pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []search.Chunk
	var want []string
	for i := range 1000 {
		code := fmt.Sprintf("chunk %d", i)
		if i%7 == 0 {
			code += " pay"
			want = append(want, code)
		}
		chunks = append(chunks, search.Chunk{Path: "a.go", Chunk: chunk.Chunk{Code: code}})
	}
	kept, err := prepass(context.Background(), chunks, f)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range kept {
		got = append(got, c.Code)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("kept %d chunks, want %d in order", len(got), len(want))
	}
}
