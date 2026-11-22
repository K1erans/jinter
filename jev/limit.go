package jev

import (
	"context"
	"errors"
	"fmt"
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
// caller of a client. Every use of the limiters passes clock's time, never
// the wall clock, so a test's fake clock drives them.
type limits struct {
	clock *clock

	mu                 sync.Mutex // guards the fields below, and burst changes
	ceilReq, ceilTok   float64
	share              float64 // fraction of the ceiling in use
	lastCut, lastRaise time.Time
	req, tok           *rate.Limiter
}

func newLimits(clk *clock, requestsPerSecond, tokensPerSecond float64) *limits {
	l := &limits{
		clock:   clk,
		ceilReq: requestsPerSecond,
		ceilTok: tokensPerSecond,
		req:     rate.NewLimiter(0, 1),
		tok:     rate.NewLimiter(0, 1),
	}
	l.set(1)
	return l
}

// wait blocks until one request of the given token cost may be sent. A
// request costing more than the token burst is reserved in burst-sized
// parts, so it is charged in full. The request and all its parts are
// reserved together under mu, so a throttle can't shrink the burst between
// reading it and reserving, and the wait is for whichever comes last.
//
// Like rate.Limiter.Wait, it fails at once, giving its reservation back,
// when the wait would outlast ctx's deadline.
func (l *limits) wait(ctx context.Context, tokens int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rs := make([]*rate.Reservation, 0, 4)
	l.mu.Lock()
	now := l.clock.now()
	rs = append(rs, l.req.ReserveN(now, 1))
	for tokens > 0 {
		n := min(tokens, l.tok.Burst())
		rs = append(rs, l.tok.ReserveN(now, n))
		tokens -= n
	}
	l.mu.Unlock()

	var delay time.Duration
	for _, r := range rs {
		if !r.OK() { // only with a zero rate, which set never makes
			cancelAll(rs, now)
			return errors.New("jev: rate limiter refused a reservation")
		}
		delay = max(delay, r.DelayFrom(now))
	}
	if delay == 0 {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok && delay > time.Until(dl) {
		cancelAll(rs, now)
		return fmt.Errorf("waiting %v for the rate limit would pass the deadline: %w", delay, context.DeadlineExceeded)
	}
	if err := l.clock.sleep(ctx, delay); err != nil {
		cancelAll(rs, l.clock.now())
		return err
	}
	return nil
}

func cancelAll(rs []*rate.Reservation, now time.Time) {
	for _, r := range rs {
		r.CancelAt(now)
	}
}

// recordThrottle notes a 429 or 529.
func (l *limits) recordThrottle() {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.clock.now()
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
	t := l.clock.now()
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
// a tenth of a second's worth and tokens a fifth. The caller holds mu, or
// owns l outright.
func (l *limits) set(share float64) {
	l.share = share
	now := l.clock.now()
	r, t := l.ceilReq*share, l.ceilTok*share
	l.req.SetLimitAt(now, rate.Limit(r))
	l.req.SetBurstAt(now, max(1, int(math.Ceil(r/10))))
	l.tok.SetLimitAt(now, rate.Limit(t))
	l.tok.SetBurstAt(now, max(1, int(t/5)))
}

func (l *limits) current() (requestsPerSecond, tokensPerSecond float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ceilReq * l.share, l.ceilTok * l.share
}
