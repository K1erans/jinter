package jev

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.typesafe.ai"

// HTTPOptions tune the real client. Zero values take the defaults below.
//
// TypeSafe publishes 80 requests/s and 100K tokens/s, but says its limits
// change without notice; on 2026-10-08 a scan at 4x those ran with no 429s.
// So the defaults are a ceiling of 4x, and the client backs off on its own
// when TypeSafe throttles it (see limits).
type HTTPOptions struct {
	BaseURL           string        // default DefaultBaseURL
	TokensPerSecond   float64       // ceiling; default 400,000
	RequestsPerSecond float64       // ceiling; default 320
	MaxRetries        int           // default 6; negative means none
	BaseBackoff       time.Duration // first retry delay, doubled each time; default 500 ms
	MaxBackoff        time.Duration // default 10 s
	Timeout           time.Duration // per attempt; default 60 s
}

// Jev's token count for a request is a fixed overhead plus its payload.
// Fitting 5,400 requests from two scans of a Go repo gave ~290 tokens per
// request (TypeSafe's docs show 296 for a one-line state) plus one token
// per ~2.96 payload bytes. (A first, overhead-blind estimate of 2.83 bytes
// per token mixed the two.) Both are rounded the safe way for a client-side
// limit, which must overestimate rather than under.
const (
	requestOverheadTokens = 300
	bytesPerToken         = 2.75
)

// maxIdleConns is how many connections the client keeps open for reuse.
// http.DefaultTransport keeps two per host, so over HTTP/1.1 all but two of
// search's 128 concurrent calls would redial, and redo TLS, every time.
const maxIdleConns = 256

// HTTP is the Jev client for TypeSafe's API. It is safe for concurrent use;
// all callers share one adaptive rate limiter and one connection pool.
type HTTP struct {
	key      string
	opts     HTTPOptions
	endpoint string
	badURL   error // set when BaseURL can't work, so Ask fails at once
	http     *http.Client
	clock    *clock
	limits   *limits
}

func NewHTTP(apiKey string, opts HTTPOptions) *HTTP {
	opts.BaseURL = strings.TrimRight(cmp.Or(opts.BaseURL, DefaultBaseURL), "/")
	opts.TokensPerSecond = cmp.Or(opts.TokensPerSecond, 400_000)
	opts.RequestsPerSecond = cmp.Or(opts.RequestsPerSecond, 320)
	opts.MaxRetries = cmp.Or(opts.MaxRetries, 6)
	opts.BaseBackoff = cmp.Or(opts.BaseBackoff, 500*time.Millisecond)
	opts.MaxBackoff = cmp.Or(opts.MaxBackoff, 10*time.Second)
	opts.Timeout = cmp.Or(opts.Timeout, 60*time.Second)
	clk := &clock{now: time.Now, sleep: sleep}
	c := &HTTP{
		key:      apiKey,
		opts:     opts,
		endpoint: opts.BaseURL + "/v1/systemone",
		http:     &http.Client{Timeout: opts.Timeout, Transport: newTransport()},
		clock:    clk,
		limits:   newLimits(clk, opts.RequestsPerSecond, opts.TokensPerSecond),
	}
	if u, err := url.Parse(c.endpoint); err != nil {
		c.badURL = fmt.Errorf("jev: bad BaseURL: %w", err)
	} else if u.Scheme != "http" && u.Scheme != "https" {
		c.badURL = fmt.Errorf("jev: BaseURL %q needs an http or https scheme", opts.BaseURL)
	}
	return c
}

// newTransport is http.DefaultTransport with room to keep a connection
// idle for every call in flight.
func newTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t = t.Clone()
	t.MaxIdleConns = maxIdleConns
	t.MaxIdleConnsPerHost = maxIdleConns
	return t
}

// Limits is the rate the client is currently allowing itself.
func (c *HTTP) Limits() (requestsPerSecond, tokensPerSecond float64) {
	return c.limits.current()
}

type responseBody struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Type string   `json:"type"`
		Noul *float64 `json:"noul"`
	} `json:"answers"`
}

// statusError is a non-2xx reply.
type statusError struct {
	status     int
	body       string
	retryAfter time.Duration
}

func (e *statusError) Error() string {
	switch e.status {
	case http.StatusUnauthorized:
		return "TypeSafe rejected the API key (401); check TYPESAFE_API_KEY"
	case http.StatusTooManyRequests:
		return "TypeSafe rate limit (429) persisted after retries"
	case 529:
		return "TypeSafe is overloaded (529) and stayed so after retries"
	}
	return fmt.Sprintf("TypeSafe API error %d: %s", e.status, e.body)
}

// isThrottled reports a rate limit (429) or overload (529) reply.
func isThrottled(err error) bool {
	var se *statusError
	return errors.As(err, &se) && (se.status == http.StatusTooManyRequests || se.status == 529)
}

func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.status == http.StatusRequestTimeout || se.status == http.StatusTooManyRequests || se.status >= 500
	}
	return true // network errors and timeouts
}

func (c *HTTP) Ask(ctx context.Context, req Request) (Response, error) {
	if c.badURL != nil {
		return Response{}, c.badURL
	}
	payload, n := encodeRequest(req)
	tokens := cost(payload)

	for attempt := 0; ; attempt++ {
		if err := c.limits.wait(ctx, tokens); err != nil {
			return Response{}, err
		}
		model, nouls, err := c.post(ctx, payload, n)
		if err == nil {
			c.limits.recordSuccess()
			return Response{Model: model, Nouls: perChunk(req, nouls)}, nil
		}
		if isThrottled(err) {
			c.limits.recordThrottle()
		}
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if !retryable(err) || attempt >= c.opts.MaxRetries {
			return Response{}, err
		}
		if err := c.clock.sleep(ctx, c.backoff(attempt, err)); err != nil {
			return Response{}, err
		}
	}
}

// cost is the number of tokens Jev will count for payload.
func cost(payload []byte) int {
	return requestOverheadTokens + int(math.Ceil(float64(len(payload))/bytesPerToken))
}

// field names chunk i in the state: a lone chunk is "code", exactly as it
// was sent before packing, and packed chunks are code_1, code_2, …. Jev
// answers each question about the field it names; on miniflux a target
// scored the same packed among other chunks, in any position, as alone.
func field(req Request, i int) string {
	if len(req.Chunks) == 1 {
		return "code"
	}
	return "code_" + strconv.Itoa(i+1)
}

// encodeRequest writes the request body and returns it with its number of
// questions, q0 to q(n-1), numbered across every chunk's questions in turn.
// The state holds the path, then each chunk's field in order.
//
// The bytes are exactly what json.Marshal makes of the struct
//
//	{Model string; State json.RawMessage; Questions map[string]{Type, Instructions string}}
//
// (with the usual json tags), which is how the body used to be built and how
// the token estimate was fitted, but written straight into one buffer: no map
// of questions, and no second pass over the code to validate the state.
func encodeRequest(req Request) ([]byte, int) {
	type question struct{ id, instructions string }
	var qs []question
	size := len(req.Model) + len(req.Path) + 64
	for i, ch := range req.Chunks {
		f := field(req, i)
		size += len(f) + len(ch.Code) + 8
		for _, q := range ch.Questions {
			in := instruction(f, q)
			qs = append(qs, question{id(len(qs)), in})
			size += len(in) + 48
		}
	}
	// json.Marshal writes map keys in sorted order: q0, q1, q10, q11, …, q2.
	slices.SortFunc(qs, func(a, b question) int { return strings.Compare(a.id, b.id) })

	var b bytes.Buffer
	b.Grow(size + size/8) // room for some escaping
	enc := json.NewEncoder(&b)
	str := func(s string) {
		enc.Encode(s) // can't fail for a string; escapes HTML as json.Marshal does
		b.Truncate(b.Len() - 1)
	}
	b.WriteString(`{"model":`)
	str(req.Model)
	b.WriteString(`,"state":{"path":`)
	str(req.Path)
	for i, ch := range req.Chunks {
		b.WriteByte(',')
		str(field(req, i))
		b.WriteByte(':')
		str(ch.Code)
	}
	b.WriteString(`},"questions":{`)
	for i, q := range qs {
		if i > 0 {
			b.WriteByte(',')
		}
		str(q.id)
		b.WriteString(`:{"type":"noul","instructions":`)
		str(q.instructions)
		b.WriteByte('}')
	}
	b.WriteString(`}}`)
	return b.Bytes(), len(qs)
}

// perChunk splits answers, numbered across every chunk's questions in
// turn, into one row per chunk.
func perChunk(req Request, nouls []float64) [][]float64 {
	out := make([][]float64, len(req.Chunks))
	for i, ch := range req.Chunks {
		out[i], nouls = nouls[:len(ch.Questions):len(ch.Questions)], nouls[len(ch.Questions):]
	}
	return out
}

// Reply bodies are read up to maxBody, which drains any normal reply so the
// connection can be reused. An error reply's body is quoted in the error up
// to maxErrorBody: a proxy's HTML error page can run to many kilobytes.
const (
	maxBody      = 1 << 20
	maxErrorBody = 1 << 10
)

// post sends payload and reads the model and n answers, q0 to q(n-1).
func (c *HTTP) post(ctx context.Context, payload []byte, n int) (string, []float64, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", nil, err
	}
	hreq.Header.Set("Authorization", "Bearer "+c.key)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("User-Agent", "jinter")
	hresp, err := c.http.Do(hreq)
	if err != nil {
		return "", nil, err
	}
	defer hresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, maxBody))
	if err != nil {
		return "", nil, err
	}
	if hresp.StatusCode/100 != 2 {
		body := strings.TrimSpace(string(raw))
		if len(body) > maxErrorBody {
			body = strings.ToValidUTF8(body[:maxErrorBody], "") + "…"
		}
		return "", nil, &statusError{
			status:     hresp.StatusCode,
			body:       body,
			retryAfter: parseRetryAfter(hresp.Header.Get("Retry-After"), c.clock.now()),
		}
	}
	var rb responseBody
	if err := json.Unmarshal(raw, &rb); err != nil {
		return "", nil, fmt.Errorf("reading TypeSafe response: %w", err)
	}
	flat := make([]float64, n)
	for i := range n {
		a, ok := rb.Answers[id(i)]
		if !ok || a.Noul == nil {
			return "", nil, fmt.Errorf("TypeSafe response has no noul answer for %s", id(i))
		}
		flat[i] = *a.Noul
	}
	return rb.Model, flat, nil
}

func id(i int) string { return "q" + strconv.Itoa(i) }

// backoff doubles from BaseBackoff with jitter, capped at MaxBackoff. A
// retry-after header wins when it asks for longer.
func (c *HTTP) backoff(attempt int, err error) time.Duration {
	base, ceil := max(c.opts.BaseBackoff, 0), max(c.opts.MaxBackoff, 0)
	d := ceil
	// Shifting past the cap would overflow on a late attempt.
	if attempt < 63 && base <= ceil>>attempt {
		d = base << attempt
	}
	d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	var se *statusError
	if errors.As(err, &se) && se.retryAfter > d {
		d = se.retryAfter
	}
	return d
}

// parseRetryAfter reads delay-seconds or an HTTP date, as of now.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil {
		// The bound keeps the conversion from overflowing (and rules out +Inf).
		if s > 0 && s < float64(math.MaxInt64/time.Second) {
			return time.Duration(s * float64(time.Second))
		}
		return 0
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}

// clock is the client's time source, shared with its limiter. Tests swap in
// a fake so that waiting for the rate limit or a backoff takes no real time.
type clock struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
