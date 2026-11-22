package search

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/chunk"
	"github.com/K1erans/jinter/jev"
	"github.com/K1erans/jinter/jev/jevtest"
)

func chunks() []Chunk {
	return []Chunk{
		{Path: "src/retry.ts", Rel: "app/src/retry.ts", Chunk: chunk.Chunk{Start: 12, End: 80, Signature: "retryFailedPayment(invoice)", Code: "retry payment"}},
		{Path: "src/worker.ts", Rel: "app/src/worker.ts", Chunk: chunk.Chunk{Start: 40, End: 95, Signature: "handleFailure(job)", Code: "worker payment email"}},
		{Path: "src/http.ts", Rel: "app/src/http.ts", Chunk: chunk.Chunk{Start: 5, End: 40, Signature: "get(url)", Code: "http"}},
	}
}

// scores gives fixed probabilities per path and question.
func scores(table map[string]map[string]float64) func(jev.State, string) float64 {
	return func(s jev.State, q string) float64 { return table[q][s.Path] }
}

var paymentScores = map[string]map[string]float64{
	"retries failed payments": {"app/src/retry.ts": 0.94, "app/src/worker.ts": 0.81, "app/src/http.ts": 0.21},
	"sends email":             {"app/src/retry.ts": 0.05, "app/src/worker.ts": 0.70, "app/src/http.ts": 0.10},
}

func opts(questions ...string) Options {
	return Options{Questions: questions, Model: "jev-1.13.0", Threshold: 0.6, Top: 10, ReadCache: true}
}

func TestRanksByProbabilityAndAppliesThresholdAndTop(t *testing.T) {
	fake := &jevtest.Fake{Score: scores(paymentScores)}
	res, err := Run(context.Background(), chunks(), fake, cache.NewMemory(), opts("retries failed payments"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Match{
		{Path: "src/retry.ts", Start: 12, End: 80, Signature: "retryFailedPayment(invoice)", Probability: 0.94},
		{Path: "src/worker.ts", Start: 40, End: 95, Signature: "handleFailure(job)", Probability: 0.81},
	}
	if !reflect.DeepEqual(res[0].Matches, want) {
		t.Fatalf("got %+v", res[0].Matches)
	}

	o := opts("retries failed payments")
	o.Top = 1
	res, _ = Run(context.Background(), chunks(), fake, cache.NewMemory(), o)
	if len(res[0].Matches) != 1 || res[0].Matches[0].Path != "src/retry.ts" {
		t.Fatalf("top 1: %+v", res[0].Matches)
	}
}

func TestNoMatchStillReportsTheBestCandidate(t *testing.T) {
	fake := &jevtest.Fake{Score: scores(paymentScores)}
	o := opts("retries failed payments")
	o.Threshold = 0.95
	res, err := Run(context.Background(), chunks(), fake, cache.NewMemory(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res[0].Matches) != 0 {
		t.Fatalf("expected no matches, got %+v", res[0].Matches)
	}
	if res[0].Best == nil || res[0].Best.Path != "src/retry.ts" || res[0].Best.Probability != 0.94 {
		t.Fatalf("best: %+v", res[0].Best)
	}
}

func TestAllQuestionsShareOneRequest(t *testing.T) {
	fake := &jevtest.Fake{Score: scores(paymentScores)}
	res, err := Run(context.Background(), chunks(), fake, cache.NewMemory(), opts("retries failed payments", "sends email"))
	if err != nil {
		t.Fatal(err)
	}
	reqs := fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("want one request per file, got %d", len(reqs))
	}
	for _, r := range reqs {
		if len(r.Chunks) != 1 || len(r.Chunks[0].Questions) != 2 || r.Model != "jev-1.13.0" {
			t.Fatalf("request %+v", r)
		}
		if r.Path == "app/src/retry.ts" && r.Chunks[0].Code != "retry payment" {
			t.Fatalf("the request should carry the repo path and code: %+v", r)
		}
	}
	if res[1].Question != "sends email" || len(res[1].Matches) != 1 || res[1].Matches[0].Path != "src/worker.ts" {
		t.Fatalf("second question: %+v", res[1])
	}
}

func TestCachedAnswersAreReusedAcrossThresholds(t *testing.T) {
	store := cache.NewMemory()
	first := &jevtest.Fake{Score: scores(paymentScores)}
	if _, err := Run(context.Background(), chunks(), first, store, opts("retries failed payments")); err != nil {
		t.Fatal(err)
	}

	again := &jevtest.Fake{Score: scores(paymentScores)}
	o := opts("Retries  FAILED payments")
	o.Threshold = 0.2
	res, err := Run(context.Background(), chunks(), again, store, o)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.Requests()); n != 0 {
		t.Fatalf("a cached search should make no calls, made %d", n)
	}
	if len(res[0].Matches) != 3 {
		t.Fatalf("lower threshold from cache: %+v", res[0].Matches)
	}

	o.ReadCache = false
	if _, err := Run(context.Background(), chunks(), again, store, o); err != nil {
		t.Fatal(err)
	}
	if n := len(again.Requests()); n != 3 {
		t.Fatalf("without reading the cache every chunk is asked, got %d", n)
	}
}

func TestOnlyUncachedQuestionsAreAsked(t *testing.T) {
	store := cache.NewMemory()
	fake := &jevtest.Fake{Score: scores(paymentScores)}
	if _, err := Run(context.Background(), chunks(), fake, store, opts("retries failed payments")); err != nil {
		t.Fatal(err)
	}
	fake2 := &jevtest.Fake{Score: scores(paymentScores)}
	if _, err := Run(context.Background(), chunks(), fake2, store, opts("retries failed payments", "sends email")); err != nil {
		t.Fatal(err)
	}
	for _, r := range fake2.Requests() {
		if q := r.Chunks[0].Questions; !reflect.DeepEqual(q, []string{"sends email"}) {
			t.Fatalf("only the new question should be sent, got %q", q)
		}
	}
}

type failOn struct {
	jev.Client
	path string
}

func (f failOn) Ask(ctx context.Context, r jev.Request) (jev.Response, error) {
	if r.Path == f.path {
		return jev.Response{}, errors.New("boom")
	}
	return f.Client.Ask(ctx, r)
}

func TestAFailedCallStopsTheSearchButKeepsEarlierAnswers(t *testing.T) {
	store := cache.NewMemory()
	client := failOn{Client: &jevtest.Fake{Score: scores(paymentScores)}, path: "app/src/http.ts"}
	o := opts("retries failed payments")
	o.Workers = 1
	_, err := Run(context.Background(), chunks(), client, store, o)
	if err == nil {
		t.Fatal("expected the error")
	}

	fake := &jevtest.Fake{Score: scores(paymentScores)}
	if _, err := Run(context.Background(), chunks(), fake, store, o); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Requests()); n != 1 {
		t.Fatalf("answers saved before the failure should be reused; made %d calls", n)
	}
}

func TestDuplicateQuestionsAreAskedOnce(t *testing.T) {
	fake := &jevtest.Fake{Score: scores(paymentScores)}
	res, err := Run(context.Background(), chunks(), fake, cache.NewMemory(), opts("sends email", "sends  email"))
	if err != nil {
		t.Fatal(err)
	}
	if q := fake.Requests()[0].Chunks[0].Questions; len(res) != 1 || len(q) != 1 {
		t.Fatalf("got %d results, %d questions", len(res), len(q))
	}
}

// fileChunks are three chunks of one file and one of another.
func fileChunks() []Chunk {
	c := func(path string, start int, code string) Chunk {
		return Chunk{Path: path, Rel: "app/" + path, Chunk: chunk.Chunk{Start: start, End: start + 9, Code: code}}
	}
	return []Chunk{c("src/pay.ts", 1, "retry payment"), c("src/pay.ts", 11, "refund"), c("src/pay.ts", 21, "invoice"), c("src/http.ts", 1, "http")}
}

func TestChunksOfAFileShareARequestButAreCachedOneByOne(t *testing.T) {
	store := cache.NewMemory()
	fake := &jevtest.Fake{Score: jevtest.Contains(map[string]string{"retries failed payments": "retry"})}
	res, err := Run(context.Background(), fileChunks(), fake, store, opts("retries failed payments"))
	if err != nil {
		t.Fatal(err)
	}
	reqs := fake.Requests()
	// Calls run in parallel, so they may arrive in either order.
	slices.SortFunc(reqs, func(a, b jev.Request) int { return strings.Compare(b.Path, a.Path) })
	if len(reqs) != 2 || reqs[0].Path != "app/src/pay.ts" || len(reqs[0].Chunks) != 3 || reqs[0].Chunks[1].Code != "refund" || len(reqs[1].Chunks) != 1 {
		t.Fatalf("want the three pay.ts chunks in one request and http.ts in another, got %+v", reqs)
	}
	if m := res[0].Matches; len(m) != 1 || m[0].Start != 1 || m[0].Probability != 0.9 {
		t.Fatalf("each chunk keeps its own answer: %+v", m)
	}

	// Editing one chunk re-asks only that chunk, alone.
	edited := fileChunks()
	edited[1].Code = "refund twice"
	again := &jevtest.Fake{Score: fake.Score}
	if _, err := Run(context.Background(), edited, again, store, opts("retries failed payments")); err != nil {
		t.Fatal(err)
	}
	if reqs := again.Requests(); len(reqs) != 1 || len(reqs[0].Chunks) != 1 || reqs[0].Chunks[0].Code != "refund twice" {
		t.Fatalf("want only the edited chunk asked, got %+v", reqs)
	}
}

func TestAPackStaysUnderItsSizeCap(t *testing.T) {
	big := strings.Repeat("x", packBytes/2)
	cs := fileChunks()[:3]
	for i := range cs {
		cs[i].Code = big + strconv.Itoa(i)
	}
	fake := &jevtest.Fake{Score: scores(paymentScores)}
	if _, err := Run(context.Background(), cs, fake, cache.NewMemory(), opts("retries failed payments")); err != nil {
		t.Fatal(err)
	}
	// Two halves and change don't fit together, so each goes alone.
	if n := len(fake.Requests()); n != 3 {
		t.Fatalf("want 3 requests, got %d", n)
	}
}

func TestAnswersSurviveCodeMovingWithinItsFile(t *testing.T) {
	store := cache.NewMemory()
	if _, err := Run(context.Background(), chunks(), &jevtest.Fake{Score: scores(paymentScores)}, store, opts("retries failed payments")); err != nil {
		t.Fatal(err)
	}
	moved := chunks()
	for i := range moved {
		moved[i].Start += 7 // lines added above every chunk
		moved[i].End += 7
	}
	again := &jevtest.Fake{Score: scores(paymentScores)}
	res, err := Run(context.Background(), moved, again, store, opts("retries failed payments"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.Requests()); n != 0 {
		t.Fatalf("unchanged code that only moved should come from the cache, made %d calls", n)
	}
	if m := res[0].Matches[0]; m.Start != 19 || m.End != 87 {
		t.Fatalf("results show the new lines, got %d-%d", m.Start, m.End)
	}
}

func TestNarrowKeepsTheChunksThatBestMatchedAnEarlierQuestion(t *testing.T) {
	store := cache.NewMemory()
	fake := &jevtest.Fake{Score: scores(paymentScores)}
	if _, err := Run(context.Background(), chunks(), fake, store, opts("retries failed payments")); err != nil {
		t.Fatal(err)
	}
	got, err := Narrow(chunks(), store, "jev-1.13.0", "Retries  FAILED payments", 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := chunks()[:2]; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant the two best, in file order", got)
	}

	// A chunk the earlier question never saw isn't searched again.
	added := append(chunks(), Chunk{Path: "src/new.ts", Rel: "app/src/new.ts", Chunk: chunk.Chunk{Start: 1, End: 3, Code: "new"}})
	if got, _ := Narrow(added, store, "jev-1.13.0", "retries failed payments", 10); !reflect.DeepEqual(got, chunks()) {
		t.Fatalf("got %+v", got)
	}
	if len(fake.Requests()) != 3 {
		t.Fatal("narrowing reads the cache only")
	}
}

func TestNarrowingByAQuestionNeverAskedIsAnError(t *testing.T) {
	store := cache.NewMemory()
	Run(context.Background(), chunks(), &jevtest.Fake{Score: scores(paymentScores)}, store, opts("retries failed payments"))
	if _, err := Narrow(chunks(), store, "jev-1.13.0", "sends email", 2); !errors.Is(err, ErrNotAsked) {
		t.Fatalf("got %v, want ErrNotAsked", err)
	}
}

func TestStateEncodingMatchesMarshal(t *testing.T) {
	// Cache keys hash this encoding, so it must never drift from
	// json.Marshal, including its escaping of HTML, U+2028 and bad UTF-8.
	s := new(stateEncoder)
	s.enc = json.NewEncoder(&s.buf)
	for _, code := range []string{
		"if a < b && c > d { return \"x\" }\n\ttab",
		"line sep para",
		"bad \xff utf-8",
		"",
		"short",
	} {
		ch := Chunk{Rel: "app/<x>.go", Chunk: chunk.Chunk{Code: code}}
		want, err := json.Marshal(jev.State{Path: ch.Rel, Code: code})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.encode(ch)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("encode(%q) = %s, want %s", code, got, want)
		}
	}
}

func TestNegativeWorkersUseTheDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	o := opts("retries failed payments")
	o.Workers = -1
	if _, err := Run(ctx, chunks(), &jevtest.Fake{Score: scores(paymentScores)}, cache.NewMemory(), o); err != nil {
		t.Fatal(err)
	}
}

// failingPuts is a store whose answer writes all fail.
type failingPuts struct{ *cache.Memory }

func (failingPuts) PutAnswers([]cache.Answer) error { return errors.New("disk full") }

// batchRecorder is a store that records each PutAnswers call, and fails
// them all when fail is set.
type batchRecorder struct {
	*cache.Memory
	fail    bool
	mu      sync.Mutex
	batches [][]cache.Answer
}

func (s *batchRecorder) PutAnswers(answers []cache.Answer) error {
	s.mu.Lock()
	s.batches = append(s.batches, slices.Clone(answers))
	s.mu.Unlock()
	if s.fail {
		return errors.New("disk full")
	}
	return s.Memory.PutAnswers(answers)
}

func TestEachCallsAnswersAreSavedInOneWrite(t *testing.T) {
	// pay.ts's chunks share one call; the second and fourth have the same
	// code, so they share keys, and the write repeats them.
	cs := fileChunks()
	cs = slices.Insert(cs, 3, cs[1])
	cs[3].Start, cs[3].End = 31, 40
	qs := []string{"retries failed payments", "sends email"}
	store := &batchRecorder{Memory: cache.NewMemory()}
	fake := &jevtest.Fake{Score: jevtest.Contains(map[string]string{"retries failed payments": "retry"})}
	if _, err := Run(context.Background(), cs, fake, store, opts(qs...)); err != nil {
		t.Fatal(err)
	}
	if len(store.batches) != len(fake.Requests()) || len(store.batches) != 2 {
		t.Fatalf("want one write per Jev call (2), got %d writes for %d calls", len(store.batches), len(fake.Requests()))
	}
	slices.SortFunc(store.batches, func(a, b []cache.Answer) int { return len(b) - len(a) })
	if n := len(store.batches[0]); n != 4*len(qs) {
		t.Fatalf("pay.ts's write holds %d answers, want %d", n, 4*len(qs))
	}
	if n := len(store.batches[1]); n != len(qs) {
		t.Fatalf("http.ts's write holds %d answers, want %d", n, len(qs))
	}
	for _, b := range store.batches {
		for _, a := range b {
			if got, _ := store.Get([]cache.Key{a.Key}); got[a.Key] != a.Noul {
				t.Fatalf("cache holds %v for a key written as %v", got[a.Key], a.Noul)
			}
		}
	}

	// A failed write is reported once for the call, not once per answer.
	var errs []error
	o := opts(qs...)
	o.OnCacheError = func(err error) { errs = append(errs, err) }
	failing := &batchRecorder{Memory: cache.NewMemory(), fail: true}
	if _, err := Run(context.Background(), cs, &jevtest.Fake{Score: fake.Score}, failing, o); err != nil {
		t.Fatal(err)
	}
	if len(errs) != 2 {
		t.Fatalf("want one error per failed write (2), got %d", len(errs))
	}
}

func TestOnCacheErrorIsNeverCalledConcurrently(t *testing.T) {
	var cs []Chunk
	for i := range 64 {
		p := fmt.Sprintf("src/f%d.ts", i)
		cs = append(cs, Chunk{Path: p, Rel: "app/" + p, Chunk: chunk.Chunk{Start: 1, End: 2, Code: p}})
	}
	var inside atomic.Int32
	var errs []error // appended without a lock: -race catches concurrent calls
	o := opts("retries failed payments")
	o.OnCacheError = func(err error) {
		if inside.Add(1) > 1 {
			t.Error("OnCacheError called concurrently")
		}
		time.Sleep(time.Millisecond)
		errs = append(errs, err)
		inside.Add(-1)
	}
	if _, err := Run(context.Background(), cs, &jevtest.Fake{Score: scores(paymentScores)}, failingPuts{cache.NewMemory()}, o); err != nil {
		t.Fatal(err)
	}
	if len(errs) != len(cs) {
		t.Fatalf("want one error per failed write, got %d", len(errs))
	}
}

func TestACancelledRunAsksNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &jevtest.Fake{Score: scores(paymentScores)}
	if _, err := Run(ctx, chunks(), fake, cache.NewMemory(), opts("retries failed payments")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if n := len(fake.Requests()); n != 0 {
		t.Fatalf("made %d calls", n)
	}
}

func TestTopPicksTheSameMatchesAsSortingThemAll(t *testing.T) {
	// Few distinct scores and repeated paths, so most of the order comes
	// from the tie-breaks.
	r := rand.New(rand.NewPCG(7, 8))
	var cs []Chunk
	var s [][]float64
	for i := range 500 {
		p := fmt.Sprintf("src/f%d.go", r.IntN(20))
		cs = append(cs, Chunk{Path: p, Rel: p, Chunk: chunk.Chunk{Start: r.IntN(30), Signature: strconv.Itoa(i)}})
		s = append(s, []float64{float64(r.IntN(5)) / 4})
	}
	all := rank("q", cs, s, 0, Options{Threshold: 0.25})
	if !slices.IsSortedFunc(all.Matches, func(a, b Match) int {
		return cmp.Or(cmp.Compare(b.Probability, a.Probability), cmp.Compare(a.Path, b.Path), cmp.Compare(a.Start, b.Start))
	}) {
		t.Fatal("matches are not best first")
	}
	if *all.Best != all.Matches[0] {
		t.Fatalf("best %+v, first match %+v", *all.Best, all.Matches[0])
	}
	for _, top := range []int{1, 2, 7, 100, len(all.Matches), len(all.Matches) + 1} {
		got := rank("q", cs, s, 0, Options{Threshold: 0.25, Top: top})
		if want := all.Matches[:min(top, len(all.Matches))]; !reflect.DeepEqual(got.Matches, want) {
			t.Fatalf("top %d: got %+v\nwant %+v", top, got.Matches, want)
		}
	}
}

func TestNarrowBreaksTiesByFileOrder(t *testing.T) {
	store := cache.NewMemory()
	same := func(jev.State, string) float64 { return 0.5 }
	cs := fileChunks()
	if _, err := Run(context.Background(), cs, &jevtest.Fake{Score: same}, store, opts("q")); err != nil {
		t.Fatal(err)
	}
	got, err := Narrow(cs, store, "jev-1.13.0", "q", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cs[:2]) {
		t.Fatalf("got %+v", got)
	}
	if got, err := Narrow(cs, store, "jev-1.13.0", "q", 0); err != nil || len(got) != 0 {
		t.Fatalf("n = 0: %+v, %v", got, err)
	}
}
