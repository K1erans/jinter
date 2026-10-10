package search

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

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
