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
	"math/bits"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"
)

const DefaultBaseURL = "https://api.typesafe.ai"

// HTTPOptions tune the real client. Zero values take the defaults below.
//
// TypeSafe sets rate limits per organization and doesn't say in its replies
// what they are; it once published 80 requests/s and 100K tokens/s, and on
// 2026-10-08 a scan at 4x those ran with no 429s. So with the rates left at
// zero the client starts at 4x, finds the account's rate by trying (up to
// 10x the start), and backs off when TypeSafe throttles it (see limits).
type HTTPOptions struct {
	BaseURL           string        // default DefaultBaseURL
	TokensPerSecond   float64       // fixed ceiling; zero: start at 400,000 and probe up to 4,000,000
	RequestsPerSecond float64       // fixed ceiling; zero: start at 320 and probe up to 3,200
	StateFile         string        // if set, and neither rate is, the rate learned is kept here for the next client
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

var (
	contentTypeHeader = []string{"application/json"}
	userAgentHeader   = []string{"jinter"}

	idTable        [256]string
	codeFieldTable [256]string

	// jsonSafe marks ASCII bytes that json.Marshal emits verbatim (with HTML
	// escaping on: '<', '>', '&' are false, as are '"', '\\', and control chars).
	jsonSafe [256]bool
	// wireSafe is jsonSafe with '<', '>' and '&' safe too: request bodies
	// skip HTML escaping, which only ever made them bigger.
	wireSafe [256]bool

	reqBufs  = sync.Pool{New: func() any { b := make([]byte, 0, 32<<10); return &b }}
	respBufs = sync.Pool{New: func() any { b := make([]byte, 0, 4<<10); return &b }}
)

func init() {
	for i := range idTable {
		idTable[i] = "q" + strconv.Itoa(i)
		codeFieldTable[i] = "code_" + strconv.Itoa(i+1)
	}
	for c := 0x20; c < 0x80; c++ {
		jsonSafe[c] = true
	}
	jsonSafe['"'] = false
	jsonSafe['\\'] = false
	jsonSafe['<'] = false
	jsonSafe['>'] = false
	jsonSafe['&'] = false
	wireSafe = jsonSafe
	wireSafe['<'] = true
	wireSafe['>'] = true
	wireSafe['&'] = true
}

// HTTP is the Jev client for TypeSafe's API. It is safe for concurrent use;
// all callers share one adaptive rate limiter and one connection pool.
type HTTP struct {
	key        string
	authHeader []string
	opts       HTTPOptions
	endpoint   string
	parsedURL  *url.URL
	badURL     error // set when BaseURL can't work, so Ask fails at once
	http       *http.Client
	clock      *clock
	limits     *limits
}

// payloadReader is a pooled io.ReadCloser over an in-memory request payload,
// avoiding per-request bytes.Reader and io.NopCloser allocations.
type payloadReader struct {
	b []byte
	i int
}

func (r *payloadReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func (r *payloadReader) Close() error { return nil }

var payloadReaders = sync.Pool{New: func() any { return &payloadReader{} }}

func NewHTTP(apiKey string, opts HTTPOptions) *HTTP {
	opts.BaseURL = strings.TrimRight(cmp.Or(opts.BaseURL, DefaultBaseURL), "/")
	opts.MaxRetries = cmp.Or(opts.MaxRetries, 6)
	opts.BaseBackoff = cmp.Or(opts.BaseBackoff, 500*time.Millisecond)
	opts.MaxBackoff = cmp.Or(opts.MaxBackoff, 10*time.Second)
	opts.Timeout = cmp.Or(opts.Timeout, 60*time.Second)
	clk := &clock{now: time.Now, sleep: sleep}
	c := &HTTP{
		key:        apiKey,
		authHeader: []string{"Bearer " + apiKey},
		opts:       opts,
		endpoint:   opts.BaseURL + "/v1/systemone",
		http:       &http.Client{Timeout: opts.Timeout, Transport: newTransport()},
		clock:      clk,
		limits:     newLimits(clk, opts, apiKey),
	}
	if u, err := url.Parse(c.endpoint); err != nil {
		c.badURL = fmt.Errorf("jev: bad BaseURL: %w", err)
	} else if u.Scheme != "http" && u.Scheme != "https" {
		c.badURL = fmt.Errorf("jev: BaseURL %q needs an http or https scheme", opts.BaseURL)
	} else {
		c.parsedURL = u
	}
	return c
}

// newTransport is http.DefaultTransport with room to keep a connection
// idle for every call in flight and buffers sized for packed requests.
func newTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t = t.Clone()
	t.MaxIdleConns = maxIdleConns
	t.MaxIdleConnsPerHost = maxIdleConns
	t.WriteBufferSize = 32 << 10
	t.ReadBufferSize = 16 << 10
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

// throttled returns err's rate limit (429) or overload (529) reply, or nil.
func throttled(err error) *statusError {
	var se *statusError
	if errors.As(err, &se) && (se.status == http.StatusTooManyRequests || se.status == 529) {
		return se
	}
	return nil
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
	bp := reqBufs.Get().(*[]byte)
	payload, n := encodeRequestTo((*bp)[:0], req)
	defer func() {
		if cap(payload) <= 256<<10 {
			*bp = payload[:0]
			reqBufs.Put(bp)
		}
	}()
	var resp Response
	err := c.do(ctx, payload, func(raw []byte) error {
		model, nouls, err := decodeNouls(raw, n, req.Model)
		if err != nil {
			return err
		}
		resp = Response{Model: model, Nouls: perChunk(req, nouls)}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	return resp, nil
}

// Decide asks q's questions, of any mix of kinds, about its state in one
// call. It shares Ask's rate limiter, retries and backoff.
func (c *HTTP) Decide(ctx context.Context, q Query) (Answers, error) {
	if c.badURL != nil {
		return Answers{}, c.badURL
	}
	payload, err := q.Body()
	if err != nil {
		return Answers{}, err
	}
	var as Answers
	err = c.do(ctx, payload, func(raw []byte) error {
		var err error
		as, err = decodeAnswers(q, raw)
		return err
	})
	if err != nil {
		return Answers{}, err
	}
	return as, nil
}

// do sends payload until decode accepts a reply, waiting for the rate
// limit before each attempt and backing off between them. A reply decode
// rejects is retried like a network error.
func (c *HTTP) do(ctx context.Context, payload []byte, decode func(raw []byte) error) error {
	tokens := cost(payload)
	for attempt := 0; ; attempt++ {
		epoch, err := c.limits.wait(ctx, tokens)
		if err != nil {
			return err
		}
		err = c.postAndDecode(ctx, payload, decode)
		if err == nil {
			c.limits.recordSuccess()
			return nil
		}
		if se := throttled(err); se != nil {
			c.limits.recordThrottle(se.status, se.retryAfter, epoch)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) || attempt >= c.opts.MaxRetries {
			return err
		}
		if err := c.clock.sleep(ctx, c.backoff(attempt, err)); err != nil {
			return err
		}
	}
}

// cost is the number of tokens Jev will count for payload. The estimate
// was fitted on bodies with '<', '>' and '&' HTML-escaped (6 bytes each),
// as encoding/json writes them; the wire now sends them as 1 byte, so they
// are charged as before and the limiter paces requests exactly as it did.
func cost(payload []byte) int {
	n := len(payload) + 5*(bytes.Count(payload, ltByte)+bytes.Count(payload, gtByte)+bytes.Count(payload, ampByte))
	return requestOverheadTokens + int(math.Ceil(float64(n)/bytesPerToken))
}

var ltByte, gtByte, ampByte = []byte{'<'}, []byte{'>'}, []byte{'&'}

// field names chunk i in the state: a lone chunk is "code", exactly as it
// was sent before packing, and packed chunks are code_1, code_2, …. Jev
// answers each question about the field it names; on miniflux a target
// scored the same packed among other chunks, in any position, as alone.
func field(req Request, i int) string {
	if len(req.Chunks) == 1 {
		return "code"
	}
	if i >= 0 && i < len(codeFieldTable) {
		return codeFieldTable[i]
	}
	return "code_" + strconv.Itoa(i+1)
}

// AppendJSONString appends s as a JSON string literal to dst, matching
// encoding/json.Marshal byte-for-byte (including HTML escaping of <, >, &,
// U+2028/U+2029 escaping, and U+FFFD replacement of invalid UTF-8).
func AppendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	dst = appendJSONStringBody(dst, s, true)
	return append(dst, '"')
}

// appendWireString is AppendJSONString without HTML escaping, as
// json.Encoder writes with SetEscapeHTML(false): the same string to any
// JSON decoder, in fewer bytes. Request bodies use it; cache keys keep
// AppendJSONString, so existing caches stay valid.
func appendWireString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	dst = appendJSONStringBody(dst, s, false)
	return append(dst, '"')
}

// appendJSONStringBody escapes s for a JSON string, without the quotes.
// With html, '<', '>' and '&' are escaped as \u003c etc., as json.Marshal
// does.
func appendJSONStringBody(dst []byte, s string, html bool) []byte {
	const (
		hex      = "0123456789abcdef"
		hiBits   = 0x8080808080808080
		loBits   = 0x0101010101010101
		sp20     = 0x2020202020202020
		qQuote   = 0x2222222222222222 // '"'
		qSlash   = 0x5c5c5c5c5c5c5c5c // '\\'
		chunkLen = 512
	)
	// Without html the '<', '>' and '&' probes repeat the '"' probe, which
	// changes nothing; the byte table likewise lets them through.
	qAmp, qLt, qGt := uint64(0x2626262626262626), uint64(0x3c3c3c3c3c3c3c3c), uint64(0x3e3e3e3e3e3e3e3e)
	safe := &jsonSafe
	if !html {
		qAmp, qLt, qGt = qQuote, qQuote, qQuote
		safe = &wireSafe
	}
	for len(s) > 0 {
		n := min(len(s), chunkLen)
		if n < len(s) {
			for n > 0 && !utf8.RuneStart(s[n]) {
				n--
			}
			if n == 0 {
				n = min(len(s), chunkLen)
			}
		}
		part := s[:n]
		s = s[n:]

		// +8 extra bytes allow speculative 8-byte SWAR stores before advancing w.
		need := len(dst) + n*6 + 8
		if cap(dst) < need {
			grown := make([]byte, len(dst), max(cap(dst)*2, need))
			copy(grown, dst)
			dst = grown
		}
		out := dst[:cap(dst)]
		w := len(dst)
		srcPtr := unsafe.StringData(part)
		dstPtr := unsafe.SliceData(out)
		i := 0
		for i < n {
			// 8-byte SWAR fast path: speculatively copy 8 bytes, check if all 8
			// are JSON-safe ASCII in registers with zero table lookups.
			for i+8 <= n {
				v := *(*uint64)(unsafe.Add(unsafe.Pointer(srcPtr), i))
				*(*uint64)(unsafe.Add(unsafe.Pointer(dstPtr), w)) = v
				xQ := v ^ qQuote
				xA := v ^ qAmp
				xL := v ^ qLt
				xG := v ^ qGt
				xS := v ^ qSlash
				mask := (v |
					((v - sp20) &^ v) |
					((xQ - loBits) &^ xQ) |
					((xA - loBits) &^ xA) |
					((xL - loBits) &^ xL) |
					((xG - loBits) &^ xG) |
					((xS - loBits) &^ xS)) & hiBits
				if mask == 0 {
					i += 8
					w += 8
					continue
				}
				k := bits.TrailingZeros64(mask) >> 3
				i += k
				w += k
				break
			}
			if i >= n {
				break
			}
			for i < n {
				b := part[i]
				if safe[b] {
					if i+8 <= n {
						break // re-enter SWAR loop
					}
					out[w] = b
					w++
					i++
					continue
				}
				if b < utf8.RuneSelf {
					switch b {
					case '\\', '"':
						out[w] = '\\'
						out[w+1] = b
						w += 2
					case '\n':
						out[w] = '\\'
						out[w+1] = 'n'
						w += 2
					case '\r':
						out[w] = '\\'
						out[w+1] = 'r'
						w += 2
					case '\t':
						out[w] = '\\'
						out[w+1] = 't'
						w += 2
					default:
						out[w] = '\\'
						out[w+1] = 'u'
						out[w+2] = '0'
						out[w+3] = '0'
						out[w+4] = hex[b>>4]
						out[w+5] = hex[b&0x0f]
						w += 6
					}
					i++
					continue
				}
				r, size := utf8.DecodeRuneInString(part[i:])
				if r == utf8.RuneError && size == 1 {
					out[w] = '\\'
					out[w+1] = 'u'
					out[w+2] = 'f'
					out[w+3] = 'f'
					out[w+4] = 'f'
					out[w+5] = 'd'
					w += 6
					i++
					continue
				}
				if r == '\u2028' || r == '\u2029' {
					out[w] = '\\'
					out[w+1] = 'u'
					out[w+2] = '2'
					out[w+3] = '0'
					out[w+4] = '2'
					out[w+5] = hex[r&0x0f]
					w += 6
					i += size
					continue
				}
				w += copy(out[w:], part[i:i+size])
				i += size
			}
		}
		dst = out[:w]
	}
	return dst
}

// encodeRequest writes the request body and returns it with its number of
// questions, q0 to q(n-1), numbered across every chunk's questions in turn.
// The state holds the path, then each chunk's field in order.
//
// The bytes are exactly what a json.Encoder with SetEscapeHTML(false) makes
// of the struct
//
//	{Model string; State json.RawMessage; Questions map[string]{Type, Instructions string}}
//
// (with the usual json tags), which is how the body used to be built (with
// HTML escaping; see cost) but written straight into one buffer: no map of
// questions, no reflection, and no second pass over the code.
func encodeRequest(req Request) ([]byte, int) {
	return encodeRequestTo(nil, req)
}

func encodeRequestTo(dst []byte, req Request) ([]byte, int) {
	type question struct {
		id    string
		field string
		q     string
	}
	totalQ := 0
	size := len(req.Model) + len(req.Path) + 64
	for i, ch := range req.Chunks {
		f := field(req, i)
		size += len(f) + len(ch.Code) + 8
		totalQ += len(ch.Questions)
		for _, q := range ch.Questions {
			size += len(f) + len(q) + 56
		}
	}
	var qsBuf [16]question
	var qs []question
	if totalQ <= len(qsBuf) {
		qs = qsBuf[:0]
	} else {
		qs = make([]question, 0, totalQ)
	}
	for i, ch := range req.Chunks {
		f := field(req, i)
		for _, q := range ch.Questions {
			qs = append(qs, question{id: id(len(qs)), field: f, q: q})
		}
	}
	// json.Marshal writes map keys in sorted order: q0, q1, q10, q11, …, q2.
	// Up to 10 questions (q0..q9) are already sorted.
	if len(qs) > 10 {
		slices.SortFunc(qs, func(a, b question) int { return strings.Compare(a.id, b.id) })
	}

	need := size + size/2 + 4096
	if cap(dst) < need {
		dst = make([]byte, 0, need)
	}
	b := dst[:0]
	b = append(b, `{"model":`...)
	b = appendWireString(b, req.Model)
	b = append(b, `,"state":{"path":`...)
	b = appendWireString(b, req.Path)
	for i, ch := range req.Chunks {
		b = append(b, ',')
		b = appendWireString(b, field(req, i))
		b = append(b, ':')
		b = appendWireString(b, ch.Code)
	}
	b = append(b, `},"questions":{`...)
	for i, q := range qs {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '"')
		b = append(b, q.id...)
		b = append(b, "\":{\"type\":\"noul\",\"instructions\":\"The `"...)
		b = append(b, q.field...)
		b = append(b, "` "...)
		b = appendNormalizedJSONContent(b, q.q)
		b = append(b, `"}`...)
	}
	b = append(b, `}}`...)
	return b, len(qs)
}

// appendNormalizedJSONContent appends NormalizeSpace(s) with JSON string
// escaping (without surrounding quotes) directly to dst.
func appendNormalizedJSONContent(dst []byte, s string) []byte {
	// Fast path: check if s has any non-ASCII or leading/trailing/double whitespace.
	// If it's simple single-spaced ASCII (almost all questions), escape directly.
	if isCleanSingleSpacedASCII(s) {
		return appendJSONStringBody(dst, s, false)
	}
	norm := NormalizeSpace(s)
	return appendJSONStringBody(dst, norm, false)
}

func isCleanSingleSpacedASCII(s string) bool {
	if len(s) == 0 {
		return true
	}
	if s[0] <= ' ' || s[len(s)-1] <= ' ' {
		return false
	}
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			return false
		}
		if c == ' ' {
			if prevSpace {
				return false
			}
			prevSpace = true
		} else if c < ' ' {
			return false
		} else {
			prevSpace = false
		}
	}
	return true
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

// postAndDecode sends payload, reads the body into a pooled buffer, and
// decodes a 2xx reply before returning the buffer to the pool.
func (c *HTTP) postAndDecode(ctx context.Context, payload []byte, decode func(raw []byte) error) error {
	pr := payloadReaders.Get().(*payloadReader)
	pr.b = payload
	pr.i = 0
	defer func() {
		pr.b = nil
		payloadReaders.Put(pr)
	}()

	var hreq *http.Request
	if c.parsedURL != nil {
		if ctx == nil {
			return errors.New("net/http: nil Context")
		}
		u := *c.parsedURL
		hreq = (&http.Request{
			Method:     http.MethodPost,
			URL:        &u,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header: http.Header{
				"Authorization": c.authHeader,
				"Content-Type":  contentTypeHeader,
				"User-Agent":    userAgentHeader,
			},
			Body:          pr,
			GetBody:       getBody(payload),
			ContentLength: int64(len(payload)),
			Host:          u.Host,
		}).WithContext(ctx)
	} else {
		var err error
		hreq, err = http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, pr)
		if err != nil {
			return err
		}
		hreq.GetBody = getBody(payload)
		hreq.Header = http.Header{
			"Authorization": c.authHeader,
			"Content-Type":  contentTypeHeader,
			"User-Agent":    userAgentHeader,
		}
	}
	hresp, err := c.http.Do(hreq)
	if err != nil {
		return err
	}
	defer hresp.Body.Close()

	bp := respBufs.Get().(*[]byte)
	raw, err := readBody((*bp)[:0], hresp.Body, hresp.ContentLength)
	defer func() {
		if cap(raw) <= 64<<10 {
			*bp = raw[:0]
			respBufs.Put(bp)
		}
	}()
	if err != nil {
		return err
	}
	if hresp.StatusCode/100 != 2 {
		body := strings.TrimSpace(string(raw))
		if len(body) > maxErrorBody {
			body = strings.ToValidUTF8(body[:maxErrorBody], "") + "…"
		}
		return &statusError{
			status:     hresp.StatusCode,
			body:       body,
			retryAfter: retryAfter(hresp.Header, c.clock.now()),
		}
	}
	return decode(raw)
}

// getBody lets the transport resend payload by itself when a reused
// keep-alive connection turns out to be closed, as http.NewRequest arranges
// for a bytes.Reader body. Without it that request fails and waits out a
// retry backoff instead.
func getBody(payload []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
}

// readBody reads up to maxBody bytes from r into dst, pre-sizing from contentLen
// when known to avoid repeated slice growth.
func readBody(dst []byte, r io.Reader, contentLen int64) ([]byte, error) {
	if contentLen > 0 && contentLen <= maxBody && int(contentLen)+1 > cap(dst) {
		dst = make([]byte, 0, contentLen+1)
	}
	lr := io.LimitedReader{R: r, N: maxBody}
	for {
		if len(dst) == cap(dst) {
			nextCap := max(cap(dst)*2, 4096)
			if nextCap > maxBody+1 {
				nextCap = maxBody + 1
			}
			if len(dst) >= maxBody {
				return dst, nil
			}
			grown := make([]byte, len(dst), nextCap)
			copy(grown, dst)
			dst = grown
		}
		n, err := lr.Read(dst[len(dst):cap(dst)])
		dst = dst[:len(dst)+n]
		if err == io.EOF {
			return dst, nil
		}
		if err != nil {
			return dst, err
		}
	}
}

// decodeNouls reads the model and n noul answers, q0 to q(n-1).
// Uses a zero-map fast scanner for standard TypeSafe JSON responses and falls
// back to encoding/json for escaped keys or unusual JSON forms.
func decodeNouls(raw []byte, n int, expectedModel string) (string, []float64, error) {
	if model, flat, ok := decodeNoulsFast(raw, n, expectedModel); ok {
		return model, flat, nil
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

// decodeNoulsFast parses {"model":"...","answers":{"q0":{"noul":0.9},...},...}
// directly without allocating a map. Returns ok=false on any syntax error,
// escaped string, or missing answer so the caller falls back to json.Unmarshal
// for exact error messages and full RFC 8259 coverage.
func decodeNoulsFast(b []byte, n int, expectedModel string) (string, []float64, bool) {
	p := jsonScanner{b: b}
	p.skipWS()
	if !p.consume('{') {
		return "", nil, false
	}
	var model string
	flat := make([]float64, n)
	var seenBuf [32]bool
	var seen []bool
	if n <= len(seenBuf) {
		seen = seenBuf[:n]
	} else {
		seen = make([]bool, n)
	}
	found := 0

	p.skipWS()
	if !p.consume('}') {
		for {
			k, ok := p.readSimpleString()
			if !ok {
				return "", nil, false
			}
			p.skipWS()
			if !p.consume(':') {
				return "", nil, false
			}
			p.skipWS()
			switch string(k) {
			case "model":
				v, ok := p.readSimpleString()
				if !ok {
					return "", nil, false
				}
				if string(v) == expectedModel {
					model = expectedModel
				} else {
					model = string(v)
				}
			case "answers":
				if !p.consume('{') {
					return "", nil, false
				}
				p.skipWS()
				if !p.consume('}') {
					for {
						qk, ok := p.readSimpleString()
						if !ok {
							return "", nil, false
						}
						p.skipWS()
						if !p.consume(':') {
							return "", nil, false
						}
						p.skipWS()
						qi, validQ := parseQIndex(qk, n)
						if !validQ {
							if !p.skipValue() {
								return "", nil, false
							}
						} else {
							val, hasNoul, ok := p.readNoulObject()
							if !ok {
								return "", nil, false
							}
							if hasNoul {
								if !seen[qi] {
									seen[qi] = true
									found++
								}
								flat[qi] = val
							}
						}
						p.skipWS()
						if p.consume(',') {
							p.skipWS()
							continue
						}
						if p.consume('}') {
							break
						}
						return "", nil, false
					}
				}
			default:
				if !p.skipValue() {
					return "", nil, false
				}
			}
			p.skipWS()
			if p.consume(',') {
				p.skipWS()
				continue
			}
			if p.consume('}') {
				break
			}
			return "", nil, false
		}
	}
	p.skipWS()
	if p.i != len(p.b) || found != n {
		return "", nil, false
	}
	return model, flat, true
}

func parseQIndex(k []byte, n int) (int, bool) {
	if len(k) < 2 || k[0] != 'q' {
		return 0, false
	}
	if len(k) > 2 && k[1] == '0' {
		return 0, false
	}
	v := 0
	for _, c := range k[1:] {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
		if v >= n {
			return 0, false
		}
	}
	return v, true
}

type jsonScanner struct {
	b []byte
	i int
}

func (p *jsonScanner) skipWS() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jsonScanner) consume(c byte) bool {
	if p.i < len(p.b) && p.b[p.i] == c {
		p.i++
		return true
	}
	return false
}

// readSimpleString reads a JSON string containing no backslash escapes or control chars.
func (p *jsonScanner) readSimpleString() ([]byte, bool) {
	if !p.consume('"') {
		return nil, false
	}
	start := p.i
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			out := p.b[start:p.i]
			p.i++
			return out, true
		}
		if c == '\\' || c < 0x20 {
			return nil, false
		}
		p.i++
	}
	return nil, false
}

func (p *jsonScanner) readNoulObject() (val float64, hasNoul bool, ok bool) {
	if !p.consume('{') {
		return 0, false, false
	}
	p.skipWS()
	if p.consume('}') {
		return 0, false, true
	}
	for {
		k, ok := p.readSimpleString()
		if !ok {
			return 0, false, false
		}
		p.skipWS()
		if !p.consume(':') {
			return 0, false, false
		}
		p.skipWS()
		if string(k) == "noul" {
			v, ok := p.readFloat()
			if !ok {
				return 0, false, false
			}
			val, hasNoul = v, true
		} else {
			if !p.skipValue() {
				return 0, false, false
			}
		}
		p.skipWS()
		if p.consume(',') {
			p.skipWS()
			continue
		}
		if p.consume('}') {
			return val, hasNoul, true
		}
		return 0, false, false
	}
}

func (p *jsonScanner) readFloat() (float64, bool) {
	start := p.i
	for p.i < len(p.b) {
		c := p.b[p.i]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' || c == 'e' || c == 'E' {
			p.i++
			continue
		}
		break
	}
	if p.i == start {
		return 0, false
	}
	v, err := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (p *jsonScanner) skipValue() bool {
	p.skipWS()
	if p.i >= len(p.b) {
		return false
	}
	switch p.b[p.i] {
	case '"':
		p.i++
		for p.i < len(p.b) {
			c := p.b[p.i]
			if c == '"' {
				p.i++
				return true
			}
			if c == '\\' {
				p.i += 2
				continue
			}
			if c < 0x20 {
				return false
			}
			p.i++
		}
		return false
	case '{':
		p.i++
		p.skipWS()
		if p.consume('}') {
			return true
		}
		for {
			if !p.skipValue() {
				return false
			}
			p.skipWS()
			if !p.consume(':') {
				return false
			}
			if !p.skipValue() {
				return false
			}
			p.skipWS()
			if p.consume(',') {
				p.skipWS()
				continue
			}
			return p.consume('}')
		}
	case '[':
		p.i++
		p.skipWS()
		if p.consume(']') {
			return true
		}
		for {
			if !p.skipValue() {
				return false
			}
			p.skipWS()
			if p.consume(',') {
				continue
			}
			return p.consume(']')
		}
	case 't':
		if p.i+4 <= len(p.b) && string(p.b[p.i:p.i+4]) == "true" {
			p.i += 4
			return true
		}
		return false
	case 'f':
		if p.i+5 <= len(p.b) && string(p.b[p.i:p.i+5]) == "false" {
			p.i += 5
			return true
		}
		return false
	case 'n':
		if p.i+4 <= len(p.b) && string(p.b[p.i:p.i+4]) == "null" {
			p.i += 4
			return true
		}
		return false
	default:
		_, ok := p.readFloat()
		return ok
	}
}

func id(i int) string {
	if i >= 0 && i < len(idTable) {
		return idTable[i]
	}
	return "q" + strconv.Itoa(i)
}

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

// retryAfter is the delay a reply asks for: retry-after-ms (which
// TypeSafe's SDKs honour alongside Retry-After) if it parses, being the
// finer of the two, else Retry-After.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if v := h.Get("Retry-After-Ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 && ms < float64(math.MaxInt64/time.Millisecond) {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	return parseRetryAfter(h.Get("Retry-After"), now)
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
