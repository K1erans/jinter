package jev

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var req = Request{
	Model:  "jev-1.13.0",
	Path:   "src/retry.ts",
	Chunks: []Chunk{{Code: "retry()", Questions: []string{"retries failed payments", "sends email"}}},
}

// withCode is req with its one chunk's code replaced.
func withCode(code string) Request {
	r := req
	r.Chunks = []Chunk{{Code: code, Questions: req.Chunks[0].Questions}}
	return r
}

// server answers with handler and counts calls.
func server(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, call int)) (*HTTP, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, int(calls.Add(1)))
	}))
	t.Cleanup(srv.Close)
	c := NewHTTP("test-key", HTTPOptions{BaseURL: srv.URL, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})
	return c, &calls
}

func ok(w http.ResponseWriter) {
	io.WriteString(w, `{"model":"jev-1.13.0","answers":{"q1":{"type":"noul","noul":0.05},"q0":{"type":"noul","noul":0.94}},"usage":{"input_tokens":300,"output_tokens":20}}`)
}

func TestSendsTheDocumentedRequestAndReadsAnswersInOrder(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header %q", got)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		want := map[string]any{
			"model": "jev-1.13.0",
			"state": map[string]any{"path": "src/retry.ts", "code": "retry()"},
			"questions": map[string]any{
				"q0": map[string]any{"type": "noul", "instructions": "The `code` retries failed payments"},
				"q1": map[string]any{"type": "noul", "instructions": "The `code` sends email"},
			},
		}
		if b, _ := json.Marshal(body); string(b) != mustJSON(want) {
			t.Errorf("body %s", b)
		}
		ok(w)
	})
	resp, err := c.Ask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "jev-1.13.0" || len(resp.Nouls) != 1 || len(resp.Nouls[0]) != 2 || resp.Nouls[0][0] != 0.94 || resp.Nouls[0][1] != 0.05 {
		t.Fatalf("got %+v", resp)
	}
}

// Packed chunks get numbered fields, in order after the path, and each
// question names its chunk's field. Answers come back one row per chunk.
func TestPacksChunksOfOneFileIntoOneRequest(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var body struct {
			State     json.RawMessage `json:"state"`
			Questions map[string]struct {
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if want := `{"path":"src/pay.go","code_1":"retry()","code_2":"mail()"}`; string(body.State) != want {
			t.Errorf("state %s", body.State)
		}
		want := map[string]string{
			"q0": "The `code_1` retries failed payments",
			"q1": "The `code_1` sends email",
			"q2": "The `code_2` sends email",
		}
		for id, in := range want {
			if body.Questions[id].Instructions != in {
				t.Errorf("%s: %q", id, body.Questions[id].Instructions)
			}
		}
		io.WriteString(w, `{"model":"jev-1.13.0","answers":{"q0":{"noul":0.9},"q1":{"noul":0.1},"q2":{"noul":0.8}}}`)
	})
	resp, err := c.Ask(context.Background(), Request{Model: "jev-1.13.0", Path: "src/pay.go", Chunks: []Chunk{
		{Code: "retry()", Questions: []string{"retries failed payments", "sends email"}},
		{Code: "mail()", Questions: []string{"sends email"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(resp.Nouls); got != `[[0.9,0.1],[0.8]]` {
		t.Fatalf("nouls %s", got)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestRetriesRateLimitsAndOverloadHonouringRetryAfter(t *testing.T) {
	c, calls := server(t, func(w http.ResponseWriter, r *http.Request, call int) {
		switch call {
		case 1:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(529)
		default:
			ok(w)
		}
	})
	start := time.Now()
	if _, err := c.Ask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls %d", calls.Load())
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("retry-after: 1 should wait about a second, waited %v", d)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	c, calls := server(t, func(w http.ResponseWriter, r *http.Request, _ int) { w.WriteHeader(529) })
	c.opts.MaxRetries = 3
	_, err := c.Ask(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "529") {
		t.Fatalf("err %v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("want 1 try + 3 retries, got %d calls", calls.Load())
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	c, calls := server(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"detail":"invalid key"}`)
	})
	_, err := c.Ask(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "TYPESAFE_API_KEY") || calls.Load() != 1 {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}

	c, _ = server(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"detail":"questions.q0.instructions: field required"}`)
	})
	if _, err := c.Ask(context.Background(), req); err == nil || !strings.Contains(err.Error(), "field required") {
		t.Fatalf("422 should carry the body, got %v", err)
	}
}

func TestMissingAnswerIsAnError(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		io.WriteString(w, `{"model":"jev-1.13.0","answers":{"q0":{"type":"noul","noul":0.9}}}`)
	})
	if _, err := c.Ask(context.Background(), req); err == nil {
		t.Fatal("expected an error for the missing q1 answer")
	}
}

func TestRequestRateIsLimited(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request, _ int) { ok(w) })
	c = NewHTTP("k", HTTPOptions{BaseURL: c.opts.BaseURL, RequestsPerSecond: 20, TokensPerSecond: 1e9})
	start := time.Now()
	for range 10 {
		if _, err := c.Ask(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	// A burst of 2, then one every 50 ms.
	if d := time.Since(start); d < 350*time.Millisecond {
		t.Fatalf("10 requests at 20/s took only %v", d)
	}
}

func TestTokenRateIsLimited(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request, _ int) { ok(w) })
	big := withCode(strings.Repeat("x", 3000)) // ~1000 tokens a request
	c = NewHTTP("k", HTTPOptions{BaseURL: c.opts.BaseURL, RequestsPerSecond: 1e6, TokensPerSecond: 5000})
	start := time.Now()
	for range 6 {
		if _, err := c.Ask(context.Background(), big); err != nil {
			t.Fatal(err)
		}
	}
	// ~6000 tokens at 5000/s with a 1000-token burst.
	if d := time.Since(start); d < 800*time.Millisecond {
		t.Fatalf("6 requests of ~1000 tokens at 5000 tokens/s took only %v", d)
	}
}

func TestCancelStopsWaiting(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Ask(ctx, req); err == nil {
		t.Fatal("expected an error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

// TestRealAPISmoke calls TypeSafe for real. It runs only with a key set.
func TestRealAPISmoke(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Skip("TYPESAFE_API_KEY not set")
	}
	c := NewHTTP(key, HTTPOptions{BaseURL: os.Getenv("TYPESAFE_BASE_URL")})
	questions := []string{"retries failed payments", "parses YAML configuration files"}
	resp, err := c.Ask(context.Background(), Request{
		Model: "jev-1.13.0",
		Path:  "billing/retry.go",
		Chunks: []Chunk{{Code: `func RetryFailedPayment(inv Invoice) error {
	for attempt := 1; attempt <= 3; attempt++ {
		if err := charge(inv); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return ErrPaymentFailed
}`, Questions: questions}, {Code: `func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, yaml.Unmarshal(b, &c)
}`, Questions: questions}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model %s, nouls %v", resp.Model, resp.Nouls)
	n := resp.Nouls
	if !strings.HasPrefix(resp.Model, "jev-") || n[0][0] < 0.5 || n[0][1] > 0.5 || n[1][0] > 0.5 || n[1][1] < 0.5 {
		t.Fatalf("unexpected answers %+v", resp)
	}
}

func TestRequestsLargerThanTheBurstAreChargedInFull(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request, _ int) { ok(w) })
	big := withCode(strings.Repeat("x", 3000)) // ~1450 tokens: 300 overhead + bytes ÷ 2.75
	c = NewHTTP("k", HTTPOptions{BaseURL: c.opts.BaseURL, RequestsPerSecond: 1e6, TokensPerSecond: 3000})
	start := time.Now()
	for range 4 {
		if _, err := c.Ask(context.Background(), big); err != nil {
			t.Fatal(err)
		}
	}
	// ~5800 tokens at 3000/s with a 600-token burst is ~1.7 s. Capping each
	// request at the burst would count only 2400 tokens and take ~0.6 s.
	if d := time.Since(start); d < time.Second {
		t.Fatalf("4 requests of ~1150 tokens at 3000 tokens/s took only %v", d)
	}
}

func TestDefaultLimitsAreTheProbedCeiling(t *testing.T) {
	rps, tps := NewHTTP("k", HTTPOptions{}).Limits()
	if rps != 320 || tps != 400_000 {
		t.Fatalf("got %v req/s, %v tok/s", rps, tps)
	}
}

func TestThrottlingHalvesTheRateThenItRecovers(t *testing.T) {
	var throttle atomic.Bool
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if throttle.Load() {
			throttle.Store(false)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		ok(w)
	})
	c = NewHTTP("k", HTTPOptions{BaseURL: c.opts.BaseURL, RequestsPerSecond: 100, TokensPerSecond: 1e6, BaseBackoff: time.Millisecond})
	now := time.Unix(1_000_000, 0)
	c.limits.now = func() time.Time { return now }
	ask := func() {
		t.Helper()
		if _, err := c.Ask(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	// Rounded, since the share is a running float sum.
	rps := func() float64 { r, _ := c.Limits(); return math.Round(r*1000) / 1000 }

	throttle.Store(true)
	ask() // a 429, then a success
	if got := rps(); got != 50 {
		t.Fatalf("after a 429 the rate should halve to 50, got %v", got)
	}

	now = now.Add(500 * time.Millisecond)
	throttle.Store(true)
	ask()
	if got := rps(); got != 50 {
		t.Fatalf("429s within a second of a cut are one event, got %v", got)
	}

	now = now.Add(1100 * time.Millisecond)
	ask()
	if got := rps(); got != 60 {
		t.Fatalf("a second without 429s should add 10%% of the ceiling, got %v", got)
	}
	for range 10 {
		now = now.Add(1100 * time.Millisecond)
		ask()
	}
	if got := rps(); got != 100 {
		t.Fatalf("the rate recovers up to the ceiling and stops, got %v", got)
	}

	for range 10 {
		now = now.Add(1100 * time.Millisecond)
		throttle.Store(true)
		ask()
	}
	if got := rps(); got != 6.25 {
		t.Fatalf("the rate never drops below 1/16 of the ceiling, got %v", got)
	}
}

func TestSmallRequestsAreChargedTheFixedOverhead(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request, _ int) { ok(w) })
	c = NewHTTP("k", HTTPOptions{BaseURL: c.opts.BaseURL, RequestsPerSecond: 1e6, TokensPerSecond: 3000})
	start := time.Now()
	for range 10 {
		if _, err := c.Ask(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	// Each tiny request is ~300 tokens of fixed overhead plus ~100 of
	// payload: ~4000 tokens at 3000/s with a 600-token burst is ~1.1 s.
	// Counting bytes alone would charge ~1000 tokens and finish at once.
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("10 small requests at 3000 tokens/s took only %v", d)
	}
}
