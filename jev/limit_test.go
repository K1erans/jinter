package jev

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestLimits makes limits on a fake clock, as NewHTTP would for opts.
func newTestLimits(t *testing.T, opts HTTPOptions) (*limits, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Now()}
	return newLimits(&clock{now: clk.now, sleep: clk.sleep}, opts, "test-key"), clk
}

// busy sends as fast as the limiters allow for d, then reports success.
func busy(t *testing.T, l *limits, d time.Duration) {
	t.Helper()
	end := l.clock.now().Add(d)
	for l.clock.now().Before(end) {
		_, tok := l.current()
		if _, err := l.wait(context.Background(), int(tok/50)+1); err != nil {
			t.Fatal(err)
		}
	}
	l.recordSuccess()
}

// idle sends one small request after d.
func idle(t *testing.T, l *limits, clk *fakeClock, d time.Duration) {
	t.Helper()
	clk.advance(d)
	if _, err := l.wait(context.Background(), 300); err != nil {
		t.Fatal(err)
	}
	l.recordSuccess()
}

// throttle reports a 429 or 529 for a request sent just now.
func throttle(l *limits, status int, retryAfter time.Duration) {
	l.mu.Lock()
	epoch := l.epoch
	l.mu.Unlock()
	l.recordThrottle(status, retryAfter, epoch)
}

func tps(l *limits) float64 { _, t := l.current(); return math.Round(t) }

func TestBusySecondsRaiseTheRateUpToTheHardCap(t *testing.T) {
	l, _ := newTestLimits(t, HTTPOptions{})
	want := []float64{600_000, 900_000, 1_350_000, 2_025_000, 3_037_500, 4_000_000, 4_000_000}
	for i, w := range want {
		busy(t, l, time.Second)
		if got := tps(l); got != w {
			t.Fatalf("after %d busy seconds the rate is %v tokens/s, want %v", i+1, got, w)
		}
	}
	if rps, _ := l.current(); rps != 3200 {
		t.Fatalf("requests/s should stop at 10x the start, got %v", rps)
	}
}

func TestUntilThrottledItProbesEveryQuarterSecond(t *testing.T) {
	l, clk := newTestLimits(t, HTTPOptions{})
	for _, w := range []float64{600_000, 900_000, 1_350_000} {
		busy(t, l, probeEvery)
		if got := tps(l); got != w {
			t.Fatalf("after a busy quarter second the rate is %v, want %v", got, w)
		}
	}
	throttle(l, http.StatusTooManyRequests, 0) // 1.35M is too much
	idle(t, l, clk, adjustEvery)               // back to 900K
	busy(t, l, probeEvery)
	if got := tps(l); got != 900_000 {
		t.Fatalf("with a limit known, a quarter second is too short to judge; rate %v", got)
	}
	busy(t, l, adjustEvery)
	if got := tps(l); got != 1_125_000 {
		t.Fatalf("a busy second should bisect 900K..1.35M, got %v", got)
	}
}

func TestASearchThatFitsInTheBurstTeachesNothing(t *testing.T) {
	l, clk := newTestLimits(t, HTTPOptions{})
	for range 10 { // 70K tokens, inside the 80K burst
		if _, err := l.wait(context.Background(), 7000); err != nil {
			t.Fatal(err)
		}
	}
	clk.advance(300 * time.Millisecond)
	l.recordSuccess()
	if got := tps(l); got != 400_000 {
		t.Fatalf("a search that never waited raised the rate to %v", got)
	}
}

func TestIdleSecondsDontRaiseTheRate(t *testing.T) {
	l, clk := newTestLimits(t, HTTPOptions{})
	for range 10 {
		idle(t, l, clk, 1100*time.Millisecond)
	}
	if got := tps(l); got != 400_000 {
		t.Fatalf("a client that never used its rate raised it to %v", got)
	}
}

// simulate runs the client against an account that allows scale limit:
// each second it either throttles (when over the limit) or is busy. It
// returns how many seconds were throttled.
func simulate(t *testing.T, l *limits, clk *fakeClock, limit float64, seconds int) (throttles int) {
	t.Helper()
	for range seconds {
		l.mu.Lock()
		over := l.scale > limit*1.0001
		l.mu.Unlock()
		if over {
			throttles++
			throttle(l, http.StatusTooManyRequests, time.Second)
			clk.advance(time.Second)
			l.recordSuccess()
			continue
		}
		busy(t, l, time.Second)
	}
	return throttles
}

func TestA429CutsTheRateThenSettlesJustUnderTheAccountsLimit(t *testing.T) {
	for _, limit := range []float64{4, 0.25, 1.3} { // 1.6M, 100K and 520K tokens/s
		l, clk := newTestLimits(t, HTTPOptions{})
		n := simulate(t, l, clk, limit, 40)
		got := tps(l) / defaultTokensPerSecond
		if got > limit || got < limit/settledGap {
			t.Errorf("limit %v: settled at scale %v, want within %v of it", limit, got, settledGap)
		}
		if n > 6 {
			t.Errorf("limit %v: finding the rate took %d throttles", limit, n)
		}
		if more := simulate(t, l, clk, limit, 60); more != 0 {
			t.Errorf("limit %v: once settled, it was throttled %d more times", limit, more)
		}
	}
}

func TestA429HalvesTheRateOnceASecond(t *testing.T) {
	l, clk := newTestLimits(t, HTTPOptions{})
	throttle(l, http.StatusTooManyRequests, 0)
	if got := tps(l); got != 200_000 {
		t.Fatalf("after a 429 the rate should halve to 200000, got %v", got)
	}
	clk.advance(500 * time.Millisecond)
	throttle(l, http.StatusTooManyRequests, 0)
	if got := tps(l); got != 200_000 {
		t.Fatalf("429s within a second of a cut are one event, got %v", got)
	}
	for range 10 {
		clk.advance(1100 * time.Millisecond)
		throttle(l, http.StatusTooManyRequests, 0)
	}
	if got := tps(l); got != 25_000 {
		t.Fatalf("the rate never drops below 1/16 of the start, got %v", got)
	}
}

func TestLate429sForRequestsSentBeforeACutAreIgnored(t *testing.T) {
	l, clk := newTestLimits(t, HTTPOptions{})
	busy(t, l, probeEvery) // 400K fine; now 600K
	sentAt600K, _ := l.wait(context.Background(), 1)
	sentAlsoAt600K, _ := l.wait(context.Background(), 1)
	l.recordThrottle(http.StatusTooManyRequests, time.Second, sentAt600K)
	if got := tps(l); got != 300_000 || l.bad != 1.5 {
		t.Fatalf("a 429 at 600K should set bad there and halve it; rate %v, bad %v", got, l.bad)
	}
	clk.advance(2 * time.Second)
	start := clk.now()
	l.recordThrottle(http.StatusTooManyRequests, 5*time.Second, sentAlsoAt600K)
	if got := tps(l); got != 300_000 || l.bad != 1.5 {
		t.Fatalf("a late 429 for a request sent before the cut changed the rate to %v, bad %v", got, l.bad)
	}
	if _, err := l.wait(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if d := clk.now().Sub(start); d > 10*time.Millisecond {
		t.Fatalf("a late 429 paused the client for %v", d)
	}
}

// Callers queued at the old rate requeue at the new one rather than leave
// their debt behind for everyone after them to repay at the lower rate.
func TestACutLeavesNoQueueBehind(t *testing.T) {
	l, clk := newTestLimits(t, HTTPOptions{RequestsPerSecond: 10, TokensPerSecond: 1e9})
	l.mu.Lock()
	for range 50 { // five seconds of queue at 10 a second
		l.req.ReserveN(clk.now(), 1)
	}
	changed := l.changed
	l.mu.Unlock()
	throttle(l, http.StatusTooManyRequests, 0)
	if changed.Err() == nil {
		t.Fatal("the cut didn't wake the queued callers")
	}
	start := clk.now()
	if _, err := l.wait(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if d := clk.now().Sub(start); d > time.Millisecond {
		t.Fatalf("after the cut a request waited %v behind the old queue", d)
	}
}

func TestAfterACutTheRateReturnsToTheLastGoodOne(t *testing.T) {
	l, clk := newTestLimits(t, HTTPOptions{})
	busy(t, l, time.Second) // 400K was fine: good; now 600K
	busy(t, l, time.Second) // 600K was fine: good; now 900K
	throttle(l, http.StatusTooManyRequests, 0)
	if got := tps(l); got != 450_000 {
		t.Fatalf("the cut should halve 900K, got %v", got)
	}
	idle(t, l, clk, 1100*time.Millisecond) // busy or not
	if got := tps(l); got != 600_000 {
		t.Fatalf("a second after the cut the rate should be back at 600K, got %v", got)
	}
	busy(t, l, time.Second)
	if got := tps(l); got != 750_000 {
		t.Fatalf("next it should try halfway between 600K and 900K, got %v", got)
	}
}

func TestAnOverloadHalvesTheRateButTeachesNothing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rates.json")
	l, clk := newTestLimits(t, HTTPOptions{StateFile: file})
	busy(t, l, time.Second)
	throttle(l, 529, 0)
	if got := tps(l); got != 300_000 {
		t.Fatalf("a 529 should halve 600K, got %v", got)
	}
	if l.bad != 0 {
		t.Fatalf("a 529 isn't this account's limit, but bad is %v", l.bad)
	}
	idle(t, l, clk, 1100*time.Millisecond) // back to 400K, the last good rate
	busy(t, l, time.Second)
	busy(t, l, time.Second)
	if got := tps(l); got != 900_000 {
		t.Fatalf("after a 529 the probe should go on as before, to 900K; got %v", got)
	}
	if e, _ := (&rateMemory{path: file, id: fingerprint("", "test-key")}).load(); e.BadTPS != 0 {
		t.Fatalf("a 529 was saved as a limit: %+v", e)
	}
}

func TestRetryAfterPausesEveryCallerThenTheyGoAtTheSteadyRate(t *testing.T) {
	l, clk := newTestLimits(t, HTTPOptions{RequestsPerSecond: 10, TokensPerSecond: 1e9})
	start := clk.now()
	throttle(l, http.StatusTooManyRequests, 2*time.Second) // and 10 req/s halves to 5
	var sent []time.Duration
	for range 3 {
		if _, err := l.wait(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, clk.now().Sub(start))
	}
	near := func(got, want time.Duration) bool { return (got - want).Abs() < 5*time.Millisecond }
	if !near(sent[0], 2*time.Second) || !near(sent[1], 2200*time.Millisecond) || !near(sent[2], 2400*time.Millisecond) {
		t.Fatalf("sent at %v; want 2s, 2.2s, 2.4s: none before Retry-After, then 5 a second", sent)
	}
}

func TestRetryAfterIsCappedForThePause(t *testing.T) {
	l, clk := newTestLimits(t, HTTPOptions{})
	start := clk.now()
	throttle(l, http.StatusTooManyRequests, time.Hour)
	if _, err := l.wait(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if d := clk.now().Sub(start); d > maxPause+time.Millisecond {
		t.Fatalf("paused %v", d)
	}
}

func TestSetRatesAreAFixedCeiling(t *testing.T) {
	l, _ := newTestLimits(t, HTTPOptions{RequestsPerSecond: 80, TokensPerSecond: 100_000})
	for range 5 {
		busy(t, l, time.Second)
	}
	if rps, tps := l.current(); rps != 80 || tps != 100_000 {
		t.Fatalf("with both rates set the client went to %v req/s, %v tok/s", rps, tps)
	}

	// One rate set caps that rate; the other still finds its level.
	l, _ = newTestLimits(t, HTTPOptions{TokensPerSecond: 100_000})
	busy(t, l, time.Second)
	if rps, tps := l.current(); rps != 480 || tps != 100_000 {
		t.Fatalf("with tokens/s set the client went to %v req/s, %v tok/s; want 480, 100000", rps, tps)
	}
}

func TestWhatWasLearnedIsKeptForTheNextClient(t *testing.T) {
	file := filepath.Join(t.TempDir(), "jrep", "rates.json")
	opts := HTTPOptions{BaseURL: "https://api.example", StateFile: file}
	l, clk := newTestLimits(t, opts)
	simulate(t, l, clk, 2, 30) // an account allowing 800K tokens/s
	learned := tps(l)

	again, _ := newTestLimits(t, opts)
	if got := tps(again); got != learned {
		t.Fatalf("the next client started at %v, want the %v learned", got, learned)
	}
	if again.bad == 0 {
		t.Fatal("the next client forgot where it was throttled, so it would probe again")
	}
	busy(t, again, time.Second)
	if got := tps(again); got > 800_000 {
		t.Fatalf("the next client probed past the known limit, to %v", got)
	}

	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "test-key") {
		t.Fatalf("the state file holds the API key:\n%s", b)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v", st.Mode().Perm())
	}

	// Another key, or another server, starts afresh, and both are kept.
	other := opts
	other.BaseURL = "https://staging.example"
	l2, _ := newTestLimits(t, other)
	if got := tps(l2); got != 400_000 {
		t.Fatalf("another server started at %v", got)
	}
	busy(t, l2, time.Second)
	var f stateFile
	b, _ = os.ReadFile(file)
	if err := json.Unmarshal(b, &f); err != nil || len(f.Servers) != 2 {
		t.Fatalf("want entries for both servers, got %v (%v):\n%s", len(f.Servers), err, b)
	}
}

func TestAfterAThrottleTheNextClientStartsLowerWithTheUsualBurst(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rates.json")
	l, _ := newTestLimits(t, HTTPOptions{StateFile: file})
	throttle(l, http.StatusTooManyRequests, 0) // at 400K

	next, clk := newTestLimits(t, HTTPOptions{StateFile: file})
	if got := tps(next); got != 200_000 {
		t.Fatalf("after a 429 at 400K the next client should start at 200K, got %v", got)
	}
	start := clk.now()
	if _, err := next.wait(context.Background(), 75_000); err != nil {
		t.Fatal(err)
	}
	if d := clk.now().Sub(start); d != 0 {
		t.Fatalf("a small search's one burst waited %v; it should have 400K's 80K burst in hand", d)
	}

	// A 429 for that burst cuts, but isn't taken as the rate's limit.
	throttle(next, http.StatusTooManyRequests, 0)
	if got := tps(next); got != 100_000 || next.bad != 1 {
		t.Fatalf("a 429 for the first burst: rate %v, bad %v; want 100000 and bad unchanged at 1", got, next.bad)
	}
}

func TestAnOldThrottleIsForgottenSoARaisedLimitIsFound(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rates.json")
	opts := HTTPOptions{StateFile: file}
	l, clk := newTestLimits(t, opts)
	simulate(t, l, clk, 2, 30)

	m := &rateMemory{path: file, id: fingerprint("", "test-key")}
	e, _ := m.load()
	e.BadAt = e.BadAt.Add(-badTTL - time.Minute)
	m.store(e)

	again, _ := newTestLimits(t, opts)
	before := tps(again)
	busy(t, again, time.Second)
	if got := tps(again); got != math.Round(before*probeStep) {
		t.Fatalf("with the throttle forgotten the client should probe up from %v, got %v", before, got)
	}
}

func TestABadStateFileIsIgnoredAndReplaced(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":      "{not json",
		"other format": `{"format":99,"servers":{}}`,
		"nonsense":     `{"format":1,"servers":{"` + fingerprint("", "test-key") + `":{"good_tokens_per_second":-5,"throttled_tokens_per_second":1e400}}}`,
		"empty":        "",
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "rates.json")
			os.WriteFile(file, []byte(content), 0o600)
			l, _ := newTestLimits(t, HTTPOptions{StateFile: file})
			if got := tps(l); got != 400_000 {
				t.Fatalf("started at %v, want the default", got)
			}
			busy(t, l, time.Second)
			if e, ok := (&rateMemory{path: file, id: fingerprint("", "test-key")}).load(); !ok || e.GoodTPS != 400_000 {
				t.Fatalf("the file wasn't rewritten: %+v %v", e, ok)
			}
		})
	}
}

func TestAMissingOrUnwritableStateFileIsHarmless(t *testing.T) {
	dir := t.TempDir()
	l, _ := newTestLimits(t, HTTPOptions{StateFile: filepath.Join(dir, "none", "rates.json")})
	if got := tps(l); got != 400_000 {
		t.Fatalf("started at %v, want the default", got)
	}
	busy(t, l, time.Second)

	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o600)
	l, _ = newTestLimits(t, HTTPOptions{StateFile: filepath.Join(blocker, "rates.json")}) // parent is a file
	busy(t, l, time.Second)
	if got := tps(l); got != 600_000 {
		t.Fatalf("a failed save changed the rate: %v", got)
	}
}

func TestSetRatesNeitherReadNorWriteTheStateFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rates.json")
	l, clk := newTestLimits(t, HTTPOptions{StateFile: file})
	simulate(t, l, clk, 0.5, 20)

	fixed, fclk := newTestLimits(t, HTTPOptions{StateFile: file, TokensPerSecond: 1_000_000})
	if got := tps(fixed); got != 1_000_000 {
		t.Fatalf("a set rate should start there, not at what was learned; got %v", got)
	}
	before, _ := os.ReadFile(file)
	throttle(fixed, http.StatusTooManyRequests, 0)
	idle(t, fixed, fclk, 1100*time.Millisecond)
	busy(t, fixed, time.Second)
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("a client with a set rate wrote the state file")
	}
}

func TestRetryAfterMsWinsOverRetryAfter(t *testing.T) {
	now := time.Now()
	h := http.Header{}
	h.Set("Retry-After", "3")
	if got := retryAfter(h, now); got != 3*time.Second {
		t.Fatalf("got %v", got)
	}
	h.Set("Retry-After-Ms", "1500")
	if got := retryAfter(h, now); got != 1500*time.Millisecond {
		t.Fatalf("got %v", got)
	}
	h.Set("Retry-After-Ms", "soon")
	if got := retryAfter(h, now); got != 3*time.Second {
		t.Fatalf("a bad retry-after-ms should fall back to Retry-After, got %v", got)
	}
}

// Through the client: a 429's Retry-After holds back the other callers, and
// its status reaches the limiter.
func TestA429ThroughTheClientPausesAndCuts(t *testing.T) {
	opts := quick
	opts.RequestsPerSecond, opts.TokensPerSecond = 100, 1e9
	c, clk, _ := serve(t, opts, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 1 {
			w.Header().Set("Retry-After-Ms", "1500")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		ok(w)
	})
	start := clk.now()
	if _, err := c.Ask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if d := clk.now().Sub(start); d < 1500*time.Millisecond || d > 1600*time.Millisecond {
		t.Fatalf("the retry went after %v, want 1.5s", d)
	}
	if rps, _ := c.Limits(); rps != 50 {
		t.Fatalf("a 429 should halve 100 req/s, got %v", rps)
	}
	if c.limits.bad != 1 {
		t.Fatalf("the 429 wasn't recorded as the limit: bad %v", c.limits.bad)
	}
}
