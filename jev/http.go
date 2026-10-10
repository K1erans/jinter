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
	MaxRetries        int           // default 6
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

// HTTP is the Jev client for TypeSafe's API. It is safe for concurrent use;
// all callers share one adaptive rate limiter.
type HTTP struct {
	key    string
	opts   HTTPOptions
	http   *http.Client
	limits *limits
}

func NewHTTP(apiKey string, opts HTTPOptions) *HTTP {
	opts.BaseURL = strings.TrimRight(cmp.Or(opts.BaseURL, DefaultBaseURL), "/")
	opts.TokensPerSecond = cmp.Or(opts.TokensPerSecond, 400_000)
	opts.RequestsPerSecond = cmp.Or(opts.RequestsPerSecond, 320)
	opts.MaxRetries = cmp.Or(opts.MaxRetries, 6)
	opts.BaseBackoff = cmp.Or(opts.BaseBackoff, 500*time.Millisecond)
	opts.MaxBackoff = cmp.Or(opts.MaxBackoff, 10*time.Second)
	opts.Timeout = cmp.Or(opts.Timeout, 60*time.Second)
	return &HTTP{
		key:    apiKey,
		opts:   opts,
		http:   &http.Client{Timeout: opts.Timeout},
		limits: newLimits(opts.RequestsPerSecond, opts.TokensPerSecond),
	}
}

// Limits is the rate the client is currently allowing itself.
func (c *HTTP) Limits() (requestsPerSecond, tokensPerSecond float64) {
	return c.limits.current()
}

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

type requestBody struct {
	Model     string              `json:"model"`
	State     json.RawMessage     `json:"state"`
	Questions map[string]question `json:"questions"`
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
	state, err := encodeState(req)
	if err != nil {
		return Response{}, err
	}
	body := requestBody{Model: req.Model, State: state, Questions: map[string]question{}}
	n := 0
	for i, ch := range req.Chunks {
		for _, q := range ch.Questions {
			body.Questions[id(n)] = question{Type: "noul", Instructions: instruction(field(req, i), q)}
			n++
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	tokens := requestOverheadTokens + int(math.Ceil(float64(len(payload))/bytesPerToken))

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
		if !retryable(err) || attempt == c.opts.MaxRetries {
			return Response{}, err
		}
		if err := sleep(ctx, c.backoff(attempt, err)); err != nil {
			return Response{}, err
		}
	}
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

// encodeState writes the path, then each chunk's field in order.
func encodeState(req Request) (json.RawMessage, error) {
	var b bytes.Buffer
	b.WriteString(`{"path":`)
	if err := writeJSON(&b, req.Path); err != nil {
		return nil, err
	}
	for i, ch := range req.Chunks {
		b.WriteByte(',')
		writeJSON(&b, field(req, i))
		b.WriteByte(':')
		if err := writeJSON(&b, ch.Code); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func writeJSON(b *bytes.Buffer, s string) error {
	v, err := json.Marshal(s)
	b.Write(v)
	return err
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

// post sends payload and reads the model and n answers, q0 to q(n-1).
func (c *HTTP) post(ctx context.Context, payload []byte, n int) (string, []float64, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.BaseURL+"/v1/systemone", bytes.NewReader(payload))
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
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, 1<<20))
	if err != nil {
		return "", nil, err
	}
	if hresp.StatusCode/100 != 2 {
		return "", nil, &statusError{
			status:     hresp.StatusCode,
			body:       strings.TrimSpace(string(raw)),
			retryAfter: parseRetryAfter(hresp.Header.Get("Retry-After")),
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
	d := min(c.opts.BaseBackoff<<attempt, c.opts.MaxBackoff)
	d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	var se *statusError
	if errors.As(err, &se) && se.retryAfter > d {
		d = se.retryAfter
	}
	return d
}

// parseRetryAfter reads delay-seconds or an HTTP date.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
