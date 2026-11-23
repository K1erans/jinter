package jev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Adaptive limiting.
//
// TypeSafe sets rate limits per organization, in requests and tokens per
// second, shows them only in its console, and answers a request over
// either with 429 (https://docs.typesafe.ai/models.md#rate-limits). Replies
// carry no headers saying what the limits are, so the client finds its
// account's rate by trying it.
//
// Both rates move together as one scale: scale 1 is the start rate
// (defaultRequestsPerSecond and defaultTokensPerSecond, or what was set),
// and each rate is capped on its own (by default at hardCapFactor times the
// start). The client keeps a bracket on the scale: good, the highest it has
// run at while busy without a 429, and bad, the lowest at which it got one.
// Each change of scale starts an epoch, and wakes queued callers to queue
// again at the new rate (see wait), so a request's epoch says the rate it
// was sent at.
//
//   - With no 429 yet, each busy quarter second (probeEvery) multiplies the
//     scale by probeStep, so even a search of a second or two finds out
//     whether the account allows more, and the next one starts there.
//   - A 429 sets bad to the scale its request was sent at, halves that, and
//     pauses every caller for the reply's Retry-After. A second later the
//     scale goes back to good. Only requests sent after the last cut count:
//     the 429s still coming back for requests sent before it say nothing
//     about the new rate, so they neither cut again nor pause.
//   - Between good and bad it bisects, a step per busy second, while bad is
//     more than settledGap above good, then stays at good, so an account
//     causes only a handful of throttles while the client finds its rate.
//   - A stretch only counts as busy when the limiters made callers wait and
//     the client sent at least busyShare of what they allowed: a
//     search held back by latency or concurrency, or one small enough to
//     fit in the burst, says nothing about the server's limit.
//   - A 529 (TypeSafe overloaded) halves the scale but teaches nothing, as
//     it isn't about this account.
//
// The scale never drops below minScale. With a state file, good and bad are
// kept per base URL and API key (a hash of them) so the next process starts
// at good; a bad older than badTTL is forgotten, so a raised limit is found
// again.
const (
	defaultRequestsPerSecond = 320
	defaultTokensPerSecond   = 400_000
	hardCapFactor            = 10

	minScale    = 1.0 / 16
	probeStep   = 1.5
	settledGap  = 1.1
	busyShare   = 0.7
	adjustEvery = time.Second
	probeEvery  = time.Second / 4
	maxPause    = time.Minute

	badTTL      = 6 * time.Hour
	stateTTL    = 30 * 24 * time.Hour
	stateFormat = 1
)

// limits shares one request limiter and one token limiter across every
// caller of a client. Every use of the limiters passes clock's time, never
// the wall clock, so a test's fake clock drives them.
type limits struct {
	clock  *clock
	memory *rateMemory // nil: nothing is kept between processes

	mu               sync.Mutex // guards the fields below, and burst changes
	baseReq, baseTok float64    // rates at scale 1
	capReq, capTok   float64    // rates never go above these
	maxScale         float64    // where both rates are at their caps
	scale            float64
	good, bad        float64 // bracket on the scale; 0 = not known
	badAt            time.Time
	lastCut          time.Time
	lastAdjust       time.Time // start of the window used to judge busyness
	usedReq, usedTok float64   // let through since lastAdjust
	waited           bool      // whether a request since lastAdjust had to wait
	pauseUntil       time.Time // Retry-After: no request goes out before this
	saveSeq          uint64    // bumped for each change worth saving
	epoch            uint64    // bumped by each change of scale
	cutEpoch         uint64    // the epoch the last cut began
	epochScale       [8]float64
	bigBurst         bool            // epoch 0 has scale 1's burst at a lower rate
	changed          context.Context // done at the next change of scale, waking waiters
	endEpoch         context.CancelFunc
	req, tok         *rate.Limiter
}

// newLimits makes the limiters for opts. A rate set in opts is where that
// rate starts and its cap; one left at zero starts at its default and may
// grow to hardCapFactor times that. When either is set, what was learned
// before is neither read nor written: the caller has said what the account
// allows.
func newLimits(clk *clock, opts HTTPOptions, apiKey string) *limits {
	l := &limits{clock: clk, scale: 1}
	l.baseReq, l.capReq = baseAndCap(opts.RequestsPerSecond, defaultRequestsPerSecond)
	l.baseTok, l.capTok = baseAndCap(opts.TokensPerSecond, defaultTokensPerSecond)
	l.maxScale = max(l.capReq/l.baseReq, l.capTok/l.baseTok)
	now := clk.now()
	l.lastAdjust = now
	if opts.StateFile != "" && opts.RequestsPerSecond == 0 && opts.TokensPerSecond == 0 {
		l.memory = &rateMemory{path: opts.StateFile, id: fingerprint(opts.BaseURL, apiKey)}
		if e, ok := l.memory.load(); ok {
			l.recall(e, now)
		}
	}
	// A new limiter starts with its burst in hand, so the first calls of a
	// search go out at once instead of trickling out at the steady rate.
	// Started below scale 1 by a past throttle, it keeps scale 1's burst
	// until its first change of scale, so a small search still goes out at
	// once: the throttle said the account can't keep up that rate, not that
	// it can't take one burst of it.
	l.epochScale[0] = l.scale
	l.changed, l.endEpoch = context.WithCancel(context.Background())
	r, t, _, _ := l.rates(l.scale)
	_, _, reqBurst, tokBurst := l.rates(max(l.scale, 1))
	l.bigBurst = l.scale < 1
	l.req = rate.NewLimiter(rate.Limit(r), reqBurst)
	l.tok = rate.NewLimiter(rate.Limit(t), tokBurst)
	return l
}

func baseAndCap(set, def float64) (base, ceil float64) {
	if set > 0 {
		return set, set
	}
	return def, def * hardCapFactor
}

// recall starts from what an earlier process learned, as tokens per second
// at the default rates.
func (l *limits) recall(e learned, now time.Time) {
	scaleOf := func(tps float64) float64 {
		if !(tps > 0) || math.IsInf(tps, 0) {
			return 0
		}
		return min(max(tps/l.baseTok, minScale), l.maxScale)
	}
	l.good = scaleOf(e.GoodTPS)
	if now.Sub(e.BadAt) < badTTL {
		l.bad, l.badAt = scaleOf(e.BadTPS), e.BadAt
	}
	if l.bad > 0 && l.good >= l.bad {
		l.good = 0
	}
	switch {
	case l.good > 0:
		l.scale = l.good
	case l.bad > 0:
		l.scale = max(minScale, min(1, l.bad/2))
	}
}

// rates are the limits and bursts for a scale. Bursts shrink with the
// rate, so a backed-off client can't send a burst sized for full speed:
// requests get a tenth of a second's worth and tokens a fifth.
func (l *limits) rates(scale float64) (req, tok float64, reqBurst, tokBurst int) {
	req, tok = min(l.capReq, l.baseReq*scale), min(l.capTok, l.baseTok*scale)
	return req, tok, max(1, int(math.Ceil(req/10))), max(1, int(tok/5))
}

// wait blocks until one request of the given token cost may be sent, and
// returns the epoch it was let through in, for recordThrottle. A
// request costing more than the token burst is reserved in burst-sized
// parts, so it is charged in full. The request and all its parts are
// reserved together under mu, so a throttle can't shrink the burst between
// reading it and reserving, and the wait is for whichever comes last.
// During a Retry-After pause, reservations are made as of the pause's end,
// so the callers held back go out at the steady rate after it instead of
// all at once.
//
// A change of scale replaces the limiters (see set) and wakes every caller
// still waiting, to queue again at the new rate. Otherwise those already
// queued would go out on the old rate's schedule: after a cut, too fast,
// with the queue's debt, repaid at the new rate, holding back everyone
// after them for seconds; after a raise, overlapping the new schedule.
//
// Like rate.Limiter.Wait, it fails at once, giving its reservation back,
// when the wait would outlast ctx's deadline.
func (l *limits) wait(ctx context.Context, tokens int) (epoch uint64, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		rs := make([]*rate.Reservation, 0, 4)
		l.mu.Lock()
		epoch = l.epoch
		changed := l.changed
		now := l.clock.now()
		at := now
		if l.pauseUntil.After(now) {
			at = l.pauseUntil
		}
		rs = append(rs, l.req.ReserveN(at, 1))
		for left := tokens; left > 0; {
			n := min(left, l.tok.Burst())
			rs = append(rs, l.tok.ReserveN(at, n))
			left -= n
		}
		l.mu.Unlock()

		var delay time.Duration
		for _, r := range rs {
			if !r.OK() { // only with a zero rate, which set never makes
				cancelAll(rs, now)
				return 0, errors.New("jev: rate limiter refused a reservation")
			}
			delay = max(delay, r.DelayFrom(now))
		}
		if delay == 0 {
			l.sent(epoch, tokens, false)
			return epoch, nil
		}
		if dl, ok := ctx.Deadline(); ok && delay > time.Until(dl) {
			cancelAll(rs, now)
			return 0, fmt.Errorf("waiting %v for the rate limit would pass the deadline: %w", delay, context.DeadlineExceeded)
		}
		sctx, stop := context.WithCancel(ctx)
		unwatch := context.AfterFunc(changed, stop)
		err := l.clock.sleep(sctx, delay)
		unwatch()
		stop()
		if err == nil {
			l.sent(epoch, tokens, true)
			return epoch, nil
		}
		if ctx.Err() != nil {
			cancelAll(rs, l.clock.now())
			return 0, ctx.Err()
		}
		// The scale changed: the limiters these reservations came from are
		// gone, so queue again on the new ones.
	}
}

// sent counts a request let through, for judging whether the client is
// busy, unless the scale has changed since it was let through.
func (l *limits) sent(epoch uint64, tokens int, waited bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if epoch != l.epoch {
		return
	}
	l.usedReq++
	l.usedTok += float64(tokens)
	l.waited = l.waited || waited
}

func cancelAll(rs []*rate.Reservation, now time.Time) {
	for _, r := range rs {
		r.CancelAt(now)
	}
}

// recordThrottle notes a 429 or 529 reply, the delay it asked for, and the
// epoch its request was let through in.
func (l *limits) recordThrottle(status int, retryAfter time.Duration, epoch uint64) {
	l.mu.Lock()
	t := l.clock.now()
	// One overload answers many requests in flight: count it once, and
	// once a second at most.
	if epoch < l.cutEpoch || t.Sub(l.lastCut) < adjustEvery {
		l.mu.Unlock()
		return
	}
	if retryAfter > 0 {
		if until := t.Add(min(retryAfter, maxPause)); until.After(l.pauseUntil) {
			l.pauseUntil = until
		}
	}
	l.lastCut = t
	at := l.scale
	if l.epoch-epoch < uint64(len(l.epochScale)) {
		at = l.epochScale[epoch%uint64(len(l.epochScale))]
	}
	var save *learned
	// A 429 for the oversized first burst (see newLimits) is about the
	// burst, not the rate: it cuts, but teaches nothing.
	if status == http.StatusTooManyRequests && !(epoch == 0 && l.bigBurst) {
		if l.bad == 0 || at < l.bad {
			l.bad, l.badAt = at, t
		}
		if l.good >= at { // the limit fell, or good was never right
			l.good = 0
		}
		save = l.snapshot(t)
	}
	l.cut(max(minScale, at/2))
	seq := l.saveSeq
	l.mu.Unlock()
	l.save(seq, save)
}

// recordSuccess notes a request that wasn't throttled. Once a second, or
// a quarter second while probing, it judges the stretch gone by: see the
// comment at the top of the file.
func (l *limits) recordSuccess() {
	l.mu.Lock()
	t := l.clock.now()
	window := t.Sub(l.lastAdjust)
	every := adjustEvery
	if l.bad == 0 && l.scale >= l.good {
		every = probeEvery
	}
	if window < every {
		l.mu.Unlock()
		return
	}
	req, tok, _, _ := l.rates(l.scale)
	secs := window.Seconds()
	busy := l.waited && (l.usedReq >= busyShare*req*secs || l.usedTok >= busyShare*tok*secs)
	l.usedReq, l.usedTok, l.waited = 0, 0, false
	l.lastAdjust = t

	var save *learned
	switch {
	case l.scale < l.good:
		l.set(l.good) // back to the rate known to work, busy or not
	case busy:
		if l.scale > l.good {
			l.good = l.scale
			save = l.snapshot(t)
		}
		next := l.scale
		switch {
		case l.bad == 0:
			next = l.scale * probeStep
		case l.bad > l.good*settledGap:
			next = (l.good + l.bad) / 2
		}
		if next = min(next, l.maxScale); next != l.scale {
			l.set(next)
		}
	}
	seq := l.saveSeq
	l.mu.Unlock()
	l.save(seq, save)
}

// cut drops to a lower scale after a throttle. The new limiters start
// with their (smaller) burst in hand, as a new client's do. The caller
// holds mu.
func (l *limits) cut(scale float64) {
	l.change(scale, true)
	l.cutEpoch = l.epoch
}

// set raises or restores the scale. The new limiters start empty: a burst
// on top of the old rate's last requests would overshoot the new rate. The
// caller holds mu.
func (l *limits) set(scale float64) { l.change(scale, false) }

// change applies a scale (see rates) and starts a new epoch, and a new
// window for judging it. The limiters are replaced, as of the end of any
// pause, and every waiting caller is woken to queue on them (see wait).
func (l *limits) change(scale float64, withBurst bool) {
	l.scale = scale
	l.epoch++
	l.epochScale[l.epoch%uint64(len(l.epochScale))] = scale
	now := l.clock.now()
	l.lastAdjust = now
	l.usedReq, l.usedTok, l.waited = 0, 0, false
	at := now
	if l.pauseUntil.After(now) {
		at = l.pauseUntil
	}
	r, t, reqBurst, tokBurst := l.rates(scale)
	l.req = rate.NewLimiter(rate.Limit(r), reqBurst)
	l.tok = rate.NewLimiter(rate.Limit(t), tokBurst)
	if !withBurst {
		l.req.ReserveN(at, reqBurst)
		l.tok.ReserveN(at, tokBurst)
	}
	l.endEpoch()
	l.changed, l.endEpoch = context.WithCancel(context.Background())
}

func (l *limits) current() (requestsPerSecond, tokensPerSecond float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, t, _, _ := l.rates(l.scale)
	return r, t
}

// snapshot is what to save after a change to good or bad, or nil when
// nothing is kept. The caller holds mu.
func (l *limits) snapshot(now time.Time) *learned {
	if l.memory == nil {
		return nil
	}
	l.saveSeq++
	e := learned{GoodTPS: l.good * l.baseTok, Updated: now}
	if l.bad > 0 {
		e.BadTPS, e.BadAt = l.bad*l.baseTok, l.badAt
	}
	return &e
}

// save writes e, unless a newer snapshot was written first. Saving is best
// effort: a search never fails for want of a state file.
func (l *limits) save(seq uint64, e *learned) {
	if e == nil {
		return
	}
	m := l.memory
	m.mu.Lock()
	defer m.mu.Unlock()
	if seq <= m.saved {
		return
	}
	m.saved = seq
	m.store(*e)
}

// rateMemory keeps what a client learned about its account's rate limit in
// a JSON file shared by every base URL and key, each under a hash of the
// two. The key itself is never written.
type rateMemory struct {
	path, id string

	mu    sync.Mutex // serializes writes from this process
	saved uint64     // the newest snapshot written
}

// learned is one account's entry: rates in tokens per second at the default
// ratio of requests to tokens.
type learned struct {
	GoodTPS float64   `json:"good_tokens_per_second,omitempty"`
	BadTPS  float64   `json:"throttled_tokens_per_second,omitempty"`
	BadAt   time.Time `json:"throttled_at,omitzero"`
	Updated time.Time `json:"updated"`
}

type stateFile struct {
	Format  int                `json:"format"`
	Servers map[string]learned `json:"servers"`
}

func fingerprint(baseURL, apiKey string) string {
	sum := sha256.Sum256([]byte("jev rate limits\x00" + baseURL + "\x00" + apiKey))
	return hex.EncodeToString(sum[:12])
}

// read returns the file's entries; a missing or unreadable file, or one in
// another format, reads as empty.
func (m *rateMemory) read() map[string]learned {
	b, err := os.ReadFile(m.path)
	if err != nil {
		return nil
	}
	var f stateFile
	if json.Unmarshal(b, &f) != nil || f.Format != stateFormat {
		return nil
	}
	return f.Servers
}

func (m *rateMemory) load() (learned, bool) {
	e, ok := m.read()[m.id]
	return e, ok
}

// store writes e as this account's entry, keeping other accounts' entries
// that are under stateTTL old. It writes a temporary file and renames it
// over the old one, so a reader never sees half a file; when two processes
// save at once, the last one wins.
func (m *rateMemory) store(e learned) {
	servers := m.read()
	if servers == nil {
		servers = map[string]learned{}
	}
	for id, old := range servers {
		if e.Updated.Sub(old.Updated) > stateTTL {
			delete(servers, id)
		}
	}
	servers[m.id] = e
	b, err := json.MarshalIndent(stateFile{Format: stateFormat, Servers: servers}, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(m.path)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".jev-rate-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), m.path) != nil {
		os.Remove(tmp.Name())
	}
}
