package jev

import (
	"context"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Adaptive limiting. The client starts at its ceiling and halves its rate
// on 429 or 529 (at most once a second, since one overload answers many
// requests in flight). After each full second without throttling it adds
// back a tenth of the ceiling. It never drops below a sixteenth.
const (
	minShare    = 1.0 / 16
	raiseStep   = 0.1
	adjustEvery = time.Second
)

// limits shares one request limiter and one token limiter across every
// caller of a client.
type limits struct {
	mu                 sync.Mutex
	ceilReq, ceilTok   float64
	share              float64 // fraction of the ceiling in use
	lastCut, lastRaise time.Time
	now                func() time.Time // the clock; tests replace it
	req, tok           *rate.Limiter
}

func newLimits(requestsPerSecond, tokensPerSecond float64) *limits {
	l := &limits{
		ceilReq: requestsPerSecond,
		ceilTok: tokensPerSecond,
		now:     time.Now,
		req:     rate.NewLimiter(0, 1),
		tok:     rate.NewLimiter(0, 1),
	}
	l.set(1)
	return l
}

// wait blocks until one request of the given token cost may be sent. A
// request costing more than the token burst waits for it in parts, so it
// is charged in full.
func (l *limits) wait(ctx context.Context, tokens int) error {
	if err := l.req.Wait(ctx); err != nil {
		return err
	}
	for tokens > 0 {
		n := min(tokens, l.tok.Burst())
		if err := l.tok.WaitN(ctx, n); err != nil {
			return err
		}
		tokens -= n
	}
	return nil
}

// recordThrottle notes a 429 or 529.
func (l *limits) recordThrottle() {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	if t.Sub(l.lastCut) < adjustEvery {
		return
	}
	l.lastCut = t
	l.set(max(minShare, l.share/2))
}

// recordSuccess notes a request that wasn't throttled.
func (l *limits) recordSuccess() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.share >= 1 {
		return
	}
	t := l.now()
	last := l.lastCut
	if l.lastRaise.After(last) {
		last = l.lastRaise
	}
	if t.Sub(last) < adjustEvery {
		return
	}
	l.lastRaise = t
	l.set(min(1, l.share+raiseStep))
}

// set applies a share of the ceiling. Bursts shrink with the rate, so a
// backed-off client can't send a burst sized for full speed: requests get
// a tenth of a second's worth and tokens a fifth.
func (l *limits) set(share float64) {
	l.share = share
	r, t := l.ceilReq*share, l.ceilTok*share
	l.req.SetLimit(rate.Limit(r))
	l.req.SetBurst(max(1, int(math.Ceil(r/10))))
	l.tok.SetLimit(rate.Limit(t))
	l.tok.SetBurst(max(1, int(t/5)))
}

func (l *limits) current() (requestsPerSecond, tokensPerSecond float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ceilReq * l.share, l.ceilTok * l.share
}
