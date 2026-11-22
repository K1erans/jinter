package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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

// fakeClock stands in for the wall clock: sleeping moves it on at once.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(max(d, 0))
	f.slept = append(f.slept, d)
	return nil
}

func (f *fakeClock) advance(d time.Duration) { f.sleep(context.Background(), d) }

func (f *fakeClock) sleeps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.slept...)
}

// serve starts a fake TypeSafe that answers with handler and counts calls,
// and returns a client of it on a fake clock, so waiting for the rate limit
// or a backoff takes no real time.
func serve(t *testing.T, opts HTTPOptions, handler func(w http.ResponseWriter, r *http.Request, call int)) (*HTTP, *fakeClock, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, int(calls.Add(1)))
	}))
	t.Cleanup(srv.Close)
	opts.BaseURL = srv.URL
	c := NewHTTP("test-key", opts)
	clk := &fakeClock{t: time.Now()}
	*c.clock = clock{now: clk.now, sleep: clk.sleep}
	return c, clk, &calls
}

// quick are options for tests that aren't about rate limits or backoff:
// short backoff, and a rate limit too high to ever wait for.
var quick = HTTPOptions{
	BaseBackoff:       time.Millisecond,
	MaxBackoff:        5 * time.Millisecond,
	RequestsPerSecond: 1e9,
	TokensPerSecond:   1e12,
}

func ok(w http.ResponseWriter) {
	io.WriteString(w, `{"model":"jev-1.13.0","answers":{"q1":{"type":"noul","noul":0.05},"q0":{"type":"noul","noul":0.94}},"usage":{"input_tokens":300,"output_tokens":20}}`)
}

func okHandler(w http.ResponseWriter, _ *http.Request, _ int) { ok(w) }

func TestSendsTheDocumentedRequestAndReadsAnswersInOrder(t *testing.T) {
	c, _, _ := serve(t, quick, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type %q", got)
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
	c, _, _ := serve(t, quick, func(w http.ResponseWriter, r *http.Request, _ int) {
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

// marshalRequest is how the body was built before encodeRequest, and how
// the token estimate was fitted: json.Marshal of a struct.
func marshalRequest(req Request) []byte {
	type question struct {
		Type         string `json:"type"`
		Instructions string `json:"instructions"`
	}
	state := map[string]string{"path": req.Path}
	keys := []string{"path"}
	for i, ch := range req.Chunks {
		state[field(req, i)] = ch.Code
		keys = append(keys, field(req, i))
	}
	// The state's fields are in order, not sorted, so it is built by hand.
	var s strings.Builder
	s.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			s.WriteByte(',')
		}
		s.WriteString(mustJSON(k) + ":" + mustJSON(state[k]))
	}
	s.WriteByte('}')
	qs := map[string]question{}
	for i, ch := range req.Chunks {
		for _, q := range ch.Questions {
			qs[id(len(qs))] = question{"noul", instruction(field(req, i), q)}
		}
	}
	b, err := json.Marshal(struct {
		Model     string              `json:"model"`
		State     json.RawMessage     `json:"state"`
		Questions map[string]question `json:"questions"`
	}{req.Model, json.RawMessage(s.String()), qs})
	if err != nil {
		panic(err)
	}
	return b
}

// The wire format, and the byte count the token estimate rests on, stay
// exactly as json.Marshal wrote them: HTML escaped, questions in key order.
func TestRequestBodyIsByteForByteWhatJSONMarshalWrote(t *testing.T) {
	many := make([]string, 23) // q10 … q22 sort between q1 and q2
	for i := range many {
		many[i] = "question " + strings.Repeat("x", i)
	}
	for name, r := range map[string]Request{
		"one chunk":    req,
		"no chunks":    {Model: "jev-1.13.0", Path: "a.go"},
		"no questions": {Model: "jev-1.13.0", Path: "a.go", Chunks: []Chunk{{Code: "x"}}},
		"packed": {Model: "jev-1.13.0", Path: "src/pay.go", Chunks: []Chunk{
			{Code: "retry()", Questions: many[:7]},
			{Code: "mail()", Questions: many[7:]},
			{Code: "z()", Questions: many[:1]},
		}},
		"escaping": {Model: "m\"<>", Path: "dir/ä &.go", Chunks: []Chunk{{
			Code:      "if a < b && c > d {\n\treturn \"\\\" + '\u2028\u2029' + \x00\x1f\x7f + \xff\xfe }",
			Questions: []string{"  uses <b>HTML</b> &  \n entities ", "invalid \xc3\x28 utf-8"},
		}}},
	} {
		got, n := encodeRequest(r)
		if want := marshalRequest(r); string(got) != string(want) {
			t.Errorf("%s:\n got %s\nwant %s", name, got, want)
		}
		want := 0
		for _, ch := range r.Chunks {
			want += len(ch.Questions)
		}
		if n != want {
			t.Errorf("%s: %d questions, want %d", name, n, want)
		}
	}
}

func TestRetriesRateLimitsAndOverloadHonouringRetryAfter(t *testing.T) {
	c, clk, calls := serve(t, quick, func(w http.ResponseWriter, r *http.Request, call int) {
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
	if _, err := c.Ask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls %d", calls.Load())
	}
	s := clk.sleeps()
	if len(s) != 2 || s[0] != time.Second || s[1] > 2*time.Millisecond {
		t.Fatalf("retry-after: 1 should wait a second, then backoff a millisecond or two; slept %v", s)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	opts := quick
	opts.MaxRetries = 3
	c, clk, calls := serve(t, opts, func(w http.ResponseWriter, r *http.Request, _ int) { w.WriteHeader(529) })
	_, err := c.Ask(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "529") {
		t.Fatalf("err %v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("want 1 try + 3 retries, got %d calls", calls.Load())
	}
	// Doubling from 1 ms with jitter: [0.5, 1], [1, 2], [2, 4] ms.
	s := clk.sleeps()
	if len(s) != 3 {
		t.Fatalf("slept %v", s)
	}
	for i, d := range s {
		if hi := time.Millisecond << i; d < hi/2 || d > hi {
			t.Errorf("backoff %d was %v, want %v to %v", i, d, hi/2, hi)
		}
	}

	opts.MaxRetries = -1
	c, _, calls = serve(t, opts, func(w http.ResponseWriter, r *http.Request, _ int) { w.WriteHeader(529) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // it once retried forever
	defer cancel()
	if _, err := c.Ask(ctx, req); err == nil || calls.Load() != 1 {
		t.Fatalf("negative MaxRetries should mean no retries: %v after %d calls", err, calls.Load())
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	c, _, calls := serve(t, quick, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"detail":"invalid key"}`)
	})
	_, err := c.Ask(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "TYPESAFE_API_KEY") || calls.Load() != 1 {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}

	c, _, _ = serve(t, quick, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"detail":"questions.q0.instructions: field required"}`)
	})
	if _, err := c.Ask(context.Background(), req); err == nil || !strings.Contains(err.Error(), "field required") {
		t.Fatalf("422 should carry the body, got %v", err)
	}
}

func TestLongErrorBodiesAreCut(t *testing.T) {
	c, _, _ := serve(t, quick, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "<html>x"+strings.Repeat("é", 50_000)+"</html>")
	})
	_, err := c.Ask(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "400: <html>xéé") || !strings.HasSuffix(err.Error(), "é…") || len(err.Error()) > 1100 {
		t.Fatalf("err %.200q… (%d bytes)", err, len(err.Error()))
	}
}

func TestMissingAnswerIsAnError(t *testing.T) {
	c, _, _ := serve(t, quick, func(w http.ResponseWriter, r *http.Request, _ int) {
		io.WriteString(w, `{"model":"jev-1.13.0","answers":{"q0":{"type":"noul","noul":0.9}}}`)
	})
	if _, err := c.Ask(context.Background(), req); err == nil {
		t.Fatal("expected an error for the missing q1 answer")
	}
}

func TestBadBaseURLFailsWithoutTrying(t *testing.T) {
	for _, u := range []string{"localhost:8080", "http://[::1", "ftp://example.com"} {
		c := NewHTTP("k", HTTPOptions{BaseURL: u})
		start := time.Now()
		if _, err := c.Ask(context.Background(), req); err == nil || !strings.Contains(err.Error(), "BaseURL") {
			t.Errorf("%s: err %v", u, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: took %v, so it was retried", u, d)
		}
	}
}

// elapsedWithin checks that n requests costing tokens each, at rate per
// second with the given burst, took as long as the limiter allows: no
// less than if the burst were spent at once, no more than if it weren't.
func elapsedWithin(t *testing.T, got time.Duration, total, burst, rate float64) {
	t.Helper()
	sec := func(x float64) time.Duration { return time.Duration(x * float64(time.Second)) }
	lo, hi := sec((total-burst)/rate), sec(total/rate)
	if got < lo-time.Millisecond || got > hi+time.Millisecond {
		t.Fatalf("took %v, want %v to %v", got, lo, hi)
	}
}

func TestRequestRateIsLimited(t *testing.T) {
	c, clk, _ := serve(t, HTTPOptions{RequestsPerSecond: 20, TokensPerSecond: 1e9}, okHandler)
	start := clk.now()
	for range 10 {
		if _, err := c.Ask(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	// A burst of 2, then one every 50 ms.
	elapsedWithin(t, clk.now().Sub(start), 10, 2, 20)
}

func TestTokenRateIsLimited(t *testing.T) {
	c, clk, _ := serve(t, HTTPOptions{RequestsPerSecond: 1e6, TokensPerSecond: 5000}, okHandler)
	big := withCode(strings.Repeat("x", 3000))
	p, _ := encodeRequest(big)
	start := clk.now()
	for range 6 {
		if _, err := c.Ask(context.Background(), big); err != nil {
			t.Fatal(err)
		}
	}
	// ~1450 tokens a request at 5000/s with a 1000-token burst.
	elapsedWithin(t, clk.now().Sub(start), float64(6*cost(p)), 1000, 5000)
}

func TestRequestsLargerThanTheBurstAreChargedInFull(t *testing.T) {
	c, clk, _ := serve(t, HTTPOptions{RequestsPerSecond: 1e6, TokensPerSecond: 3000}, okHandler)
	big := withCode(strings.Repeat("x", 3000)) // ~1450 tokens: 300 overhead + bytes ÷ 2.75
	p, _ := encodeRequest(big)
	start := clk.now()
	for range 4 {
		if _, err := c.Ask(context.Background(), big); err != nil {
			t.Fatal(err)
		}
	}
	// ~5800 tokens at 3000/s with a 600-token burst is ~1.7 s. Capping each
	// request at the burst would count only 2400 tokens and take ~0.8 s.
	elapsedWithin(t, clk.now().Sub(start), float64(4*cost(p)), 600, 3000)
}

func TestSmallRequestsAreChargedTheFixedOverhead(t *testing.T) {
	c, clk, _ := serve(t, HTTPOptions{RequestsPerSecond: 1e6, TokensPerSecond: 3000}, okHandler)
	p, _ := encodeRequest(req)
	if want := 300 + int(math.Ceil(float64(len(p))/2.75)); cost(p) != want {
		t.Fatalf("a %d-byte request costs %d tokens, want %d", len(p), cost(p), want)
	}
	start := clk.now()
	for range 10 {
		if _, err := c.Ask(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	// Each tiny request is ~300 tokens of fixed overhead plus ~70 of
	// payload: ~3700 tokens at 3000/s with a 600-token burst is ~1 s.
	// Counting bytes alone would charge ~700 tokens and finish at once.
	elapsedWithin(t, clk.now().Sub(start), float64(10*cost(p)), 600, 3000)
}

func TestCancelStopsWaiting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewHTTP("k", HTTPOptions{BaseURL: srv.URL}) // on the real clock
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Ask(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

// A request whose turn would come after its deadline fails at once, and
// gives its turn back to the requests behind it.
func TestAWaitPastTheDeadlineFailsAtOnce(t *testing.T) {
	c, clk, calls := serve(t, HTTPOptions{RequestsPerSecond: 1, TokensPerSecond: 1e9}, okHandler)
	if _, err := c.Ask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := c.Ask(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("it waited for the deadline instead of failing at once")
	}
	start := clk.now()
	if _, err := c.Ask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if d := clk.now().Sub(start); d > time.Second+time.Millisecond {
		t.Fatalf("the next request waited %v, so the failed one kept its turn", d)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls %d", calls.Load())
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

func TestDefaultLimitsAreTheProbedCeiling(t *testing.T) {
	rps, tps := NewHTTP("k", HTTPOptions{}).Limits()
	if rps != 320 || tps != 400_000 {
		t.Fatalf("got %v req/s, %v tok/s", rps, tps)
	}
}

func TestThrottlingHalvesTheRateThenItRecovers(t *testing.T) {
	var throttle atomic.Bool
	c, clk, _ := serve(t, HTTPOptions{RequestsPerSecond: 100, TokensPerSecond: 1e6, BaseBackoff: time.Millisecond}, func(w http.ResponseWriter, r *http.Request, _ int) {
		if throttle.Load() {
			throttle.Store(false)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		ok(w)
	})
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

	clk.advance(500 * time.Millisecond)
	throttle.Store(true)
	ask()
	if got := rps(); got != 50 {
		t.Fatalf("429s within a second of a cut are one event, got %v", got)
	}

	clk.advance(1100 * time.Millisecond)
	ask()
	if got := rps(); got != 60 {
		t.Fatalf("a second without 429s should add 10%% of the ceiling, got %v", got)
	}
	for range 10 {
		clk.advance(1100 * time.Millisecond)
		ask()
	}
	if got := rps(); got != 100 {
		t.Fatalf("the rate recovers up to the ceiling and stops, got %v", got)
	}

	for range 10 {
		clk.advance(1100 * time.Millisecond)
		throttle.Store(true)
		ask()
	}
	if got := rps(); got != 6.25 {
		t.Fatalf("the rate never drops below 1/16 of the ceiling, got %v", got)
	}
}

// Many callers share one client while TypeSafe throttles some of them; run
// with -race.
func TestConcurrentCallersShareOneClient(t *testing.T) {
	opts := quick
	opts.RequestsPerSecond, opts.TokensPerSecond = 50, 20_000
	c, _, _ := serve(t, opts, func(w http.ResponseWriter, r *http.Request, call int) {
		if call%7 == 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		ok(w)
	})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 10 {
				if _, err := c.Ask(context.Background(), req); err != nil {
					t.Error(err)
					return
				}
				c.Limits()
			}
		})
	}
	wg.Wait()
}

// Connections are kept for reuse, one per concurrent caller, rather than
// the two per host of http.DefaultTransport.
func TestConnectionsAreReused(t *testing.T) {
	var dialed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ok(w) }))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dialed.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	c := NewHTTP("k", HTTPOptions{BaseURL: srv.URL, RequestsPerSecond: 1e6, TokensPerSecond: 1e9})
	const callers = 16
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			for range 20 {
				if _, err := c.Ask(context.Background(), req); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if n := dialed.Load(); n > 2*callers {
		t.Fatalf("%d callers making 320 calls opened %d connections", callers, n)
	}
}

func TestBackoffDoublesWithJitterUpToTheCap(t *testing.T) {
	c := NewHTTP("k", HTTPOptions{})
	for attempt := range 200 { // late attempts once overflowed the shift
		want := min(500*time.Millisecond<<min(attempt, 10), 10*time.Second)
		if d := c.backoff(attempt, errors.New("network")); d < want/2 || d > want {
			t.Fatalf("attempt %d: %v, want %v to %v", attempt, d, want/2, want)
		}
	}
	if d := c.backoff(0, &statusError{status: 429, retryAfter: time.Minute}); d != time.Minute {
		t.Fatalf("retry-after should win when longer, got %v", d)
	}
	if d := c.backoff(5, &statusError{status: 429, retryAfter: time.Millisecond}); d < 5*time.Second {
		t.Fatalf("backoff should win when longer, got %v", d)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":                              0,
		"0":                             0,
		"3":                             3 * time.Second,
		"1.5":                           1500 * time.Millisecond,
		"-2":                            0,
		"NaN":                           0,
		"Inf":                           0,
		"1e300":                         0,
		"soon":                          0,
		"Sat, 21 Nov 2026 12:00:30 GMT": 30 * time.Second,
		"Sat, 21 Nov 2026 11:59:00 GMT": 0,
	} {
		if got := parseRetryAfter(v, now); got != want {
			t.Errorf("%q: got %v, want %v", v, got, want)
		}
	}
}
