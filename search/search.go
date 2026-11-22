// Package search runs the pipeline: key → cache → Jev → rank.
package search

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/chunk"
	"github.com/K1erans/jinter/jev"
)

// packBytes caps the code sent in one Jev call. A file's chunks share
// calls up to this size, since every call costs ~240 tokens before its
// code; on miniflux that cut a scan's tokens by about a third. It is half
// a chunk's own limit (chunk.Default.MaxBytes), well inside Jev's 32K
// tokens; a bigger chunk goes alone.
const packBytes = 24 << 10

// DefaultWorkers is how many Jev calls may be in flight: enough for the
// client's 320 requests/s ceiling at ~250 ms a call. The client's rate
// limiter, not this number, keeps calls inside TypeSafe's limits.
const DefaultWorkers = 128

type Chunk struct {
	Path string // shown to the user
	Rel  string // sent to Jev and hashed; stable across working directories
	chunk.Chunk
}

type Options struct {
	Questions []string
	Model     string
	Threshold float64 // minimum probability to count as a match
	Top       int     // maximum matches per question; 0 means no limit
	ReadCache bool    // false still writes answers, but never reads them
	Workers   int     // concurrent Jev calls; 0 or less means DefaultWorkers

	// OnCacheError hears about failed cache writes, which don't stop a
	// search: once for each Jev call whose answers couldn't be saved.
	// Calls never overlap.
	OnCacheError func(error)

	// OnAsk hears how many chunks are about to be sent to Jev, when any are.
	OnAsk func(chunks int)

	// OnMatch hears each chunk that reaches Threshold for a question as
	// soon as its answer is known, so callers can show matches while the
	// search is still running: cached answers first, then each Jev call's
	// as it returns. They come in no order of rank and Top doesn't limit
	// them; Run's results are still complete and ranked. Calls never
	// overlap, with each other or with OnCacheError.
	OnMatch func(question string, m Match)

	// Likely marks chunks to ask Jev about first, so that with OnMatch the
	// probable matches arrive early. It only changes the order of Jev
	// calls, never which chunks are asked about. It is called from many
	// goroutines at once.
	Likely func(code string) bool
}

type Match struct {
	Path        string
	Start, End  int
	Signature   string
	Probability float64
}

type Result struct {
	Question string
	Matches  []Match // best first, at or above the threshold
	Best     *Match  // best candidate even when below the threshold; nil if nothing was searched
}

// task is one chunk and the questions still unanswered for it.
type task struct {
	chunk   int
	missing []int // indexes into questions
}

// Run asks every question about every chunk, using cached answers where it
// can, and ranks the results. Answers are cached as they arrive, each Jev
// call's in one write, so an interrupted run keeps its progress.
func Run(ctx context.Context, chunks []Chunk, client jev.Client, store cache.Cache, opts Options) ([]Result, error) {
	questions := dedupe(opts.Questions)
	instructions := make([]string, len(questions))
	for i, q := range questions {
		instructions[i] = jev.Instruction(q)
	}

	all, err := keyAll(ctx, chunks, opts.Model, instructions)
	if err != nil {
		return nil, err
	}
	// scores[c][q] is chunk c's probability for question q, and keys[c][q]
	// its cache key.
	scores := make([][]float64, len(chunks))
	keys := make([][]cache.Key, len(chunks))
	flat := make([]float64, len(all))
	for c := range chunks {
		scores[c] = flat[c*len(questions) : (c+1)*len(questions)]
		keys[c] = all[c*len(questions) : (c+1)*len(questions)]
	}

	hits := map[cache.Key]float64{}
	if opts.ReadCache && len(all) > 0 {
		var err error
		if hits, err = store.Get(all); err != nil {
			return nil, fmt.Errorf("reading cache: %w", err)
		}
	}
	// heard tells OnMatch about chunk c's answer to question q.
	heard := func(c, q int) {
		if opts.OnMatch != nil && scores[c][q] >= opts.Threshold {
			opts.OnMatch(questions[q], matchOf(chunks[c], scores[c][q]))
		}
	}
	var tasks []task
	for c := range chunks {
		t := task{chunk: c}
		for q := range questions {
			if v, ok := hits[keys[c][q]]; ok {
				scores[c][q] = v
				heard(c, q)
			} else {
				t.missing = append(t.missing, q)
			}
		}
		if len(t.missing) > 0 {
			tasks = append(tasks, t)
		}
	}

	// Answers arrive on many workers at once; callers needn't make
	// OnCacheError or OnMatch safe for concurrent use.
	var callbacks sync.Mutex
	cacheError := func(err error) {
		if opts.OnCacheError != nil {
			callbacks.Lock()
			defer callbacks.Unlock()
			opts.OnCacheError(err)
		}
	}

	// Each answer is cached under its own chunk's key, as if the chunk had
	// been sent alone, so editing one function re-asks only that function
	// and a chunk's answers don't depend on which others shared its call.
	ask := func(ctx context.Context, pack []task) error {
		first, last := chunks[pack[0].chunk], chunks[pack[len(pack)-1].chunk]
		req := jev.Request{Model: opts.Model, Path: first.Rel}
		for _, t := range pack {
			c := jev.Chunk{Code: chunks[t.chunk].Code}
			for _, q := range t.missing {
				c.Questions = append(c.Questions, questions[q])
			}
			req.Chunks = append(req.Chunks, c)
		}
		resp, err := client.Ask(ctx, req)
		if err != nil {
			return fmt.Errorf("%s:%d-%d: %w", first.Path, first.Start, last.End, err)
		}
		if len(resp.Nouls) != len(pack) {
			return fmt.Errorf("%s:%d-%d: got answers for %d chunks, asked about %d", first.Path, first.Start, last.End, len(resp.Nouls), len(pack))
		}
		// The call's answers are saved as soon as it returns, in one write,
		// so with SQLite they share one transaction.
		answers := make([]cache.Answer, 0, len(pack)*len(questions))
		save := func() {
			if len(answers) == 0 {
				return
			}
			if err := store.PutAnswers(answers); err != nil {
				cacheError(err)
			}
		}
		for i, t := range pack {
			if len(resp.Nouls[i]) != len(t.missing) {
				save() // the chunks before this one were answered properly
				ch := chunks[t.chunk]
				return fmt.Errorf("%s:%d-%d: got %d answers for %d questions", ch.Path, ch.Start, ch.End, len(resp.Nouls[i]), len(t.missing))
			}
			for j, q := range t.missing {
				scores[t.chunk][q] = resp.Nouls[i][j]
				answers = append(answers, cache.Answer{Key: keys[t.chunk][q], Noul: resp.Nouls[i][j]})
			}
			if opts.OnMatch != nil {
				callbacks.Lock()
				for _, q := range t.missing {
					heard(t.chunk, q)
				}
				callbacks.Unlock()
			}
		}
		save()
		return nil
	}
	if len(tasks) > 0 && opts.OnAsk != nil {
		opts.OnAsk(len(tasks))
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	calls := packs(chunks, tasks)
	if opts.Likely != nil {
		calls = likelyFirst(ctx, chunks, calls, opts.Likely)
	}
	if err := parallel(ctx, calls, workers, ask); err != nil {
		return nil, err
	}

	results := make([]Result, len(questions))
	for q, question := range questions {
		results[q] = rank(question, chunks, scores, q, opts)
	}
	return results, nil
}

// packs groups tasks into Jev calls: neighbouring chunks of one file, up
// to packBytes of code. Chunks are in file order, so a file's chunks are
// neighbours.
func packs(chunks []Chunk, tasks []task) [][]task {
	var out [][]task
	size := 0
	for _, t := range tasks {
		c := chunks[t.chunk]
		if n := len(out); n > 0 && chunks[out[n-1][0].chunk].Path == c.Path && size+len(c.Code) <= packBytes {
			out[n-1] = append(out[n-1], t)
			size += len(c.Code)
			continue
		}
		out = append(out, []task{t})
		size = len(c.Code)
	}
	return out
}

// likelyFirst moves the calls holding a likely chunk ahead of the rest,
// keeping file order within each group.
func likelyFirst(ctx context.Context, chunks []Chunk, calls [][]task, likely func(code string) bool) [][]task {
	first := make([]bool, len(calls))
	if each(ctx, len(calls), func(i int) {
		first[i] = slices.ContainsFunc(calls[i], func(t task) bool { return likely(chunks[t.chunk].Code) })
	}) != nil {
		return calls // cancelled: nothing will be asked anyway
	}
	out := make([][]task, 0, len(calls))
	for _, want := range []bool{true, false} {
		for i, call := range calls {
			if first[i] == want {
				out = append(out, call)
			}
		}
	}
	return out
}

// stateEncoder encodes chunks as Jev would be shown them alone, for their
// cache keys. It reuses one buffer: a fresh one per chunk was most of what
// a cached search allocated.
type stateEncoder struct {
	buf bytes.Buffer
	enc *json.Encoder
}

var stateEncoders = sync.Pool{New: func() any {
	s := new(stateEncoder)
	s.enc = json.NewEncoder(&s.buf)
	return s
}}

// encode returns exactly what json.Marshal would, valid until the next
// call.
func (s *stateEncoder) encode(ch Chunk) ([]byte, error) {
	s.buf.Reset()
	if err := s.enc.Encode(jev.State{Path: ch.Rel, Code: ch.Code}); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(s.buf.Bytes(), []byte("\n")), nil // Encode adds a newline
}

// keyAll builds every chunk's cache key for each instruction,
// keys[c*len(instructions)+q], one worker per CPU: hashing every chunk once
// per question is most of a cached search's time.
func keyAll(ctx context.Context, chunks []Chunk, model string, instructions []string) ([]cache.Key, error) {
	keys := make([]cache.Key, len(chunks)*len(instructions))
	errs := make([]error, len(chunks))
	if err := each(ctx, len(chunks), func(c int) {
		s := stateEncoders.Get().(*stateEncoder)
		defer stateEncoders.Put(s)
		sent, err := s.encode(chunks[c])
		if err != nil {
			errs[c] = err
			return
		}
		for q, in := range instructions {
			keys[c*len(instructions)+q] = cache.KeyOf(model, in, sent)
		}
	}); err != nil {
		return nil, err
	}
	return keys, errors.Join(errs...)
}

// ErrNotAsked means no chunk has a cached answer for the question.
var ErrNotAsked = errors.New("no cached answers")

// Narrow keeps the n chunks that scored best for an earlier question, in
// their original order, reading only cached answers. It makes retrying a
// reworded question cheap: replaying 24 rewordings of 12 tasks, the right
// chunk was always in the earlier question's top 11, and asking the new
// wording about only those ranked it as well as a full scan did. Chunks
// with no cached answer are left out: the earlier search never saw them.
func Narrow(chunks []Chunk, store cache.Cache, model, question string, n int) ([]Chunk, error) {
	keys, err := keyAll(context.Background(), chunks, model, []string{jev.Instruction(question)})
	if err != nil {
		return nil, err
	}
	hits, err := store.Get(keys)
	if err != nil {
		return nil, fmt.Errorf("reading cache: %w", err)
	}
	var answered []int
	score := make([]float64, len(chunks))
	for i, k := range keys {
		if v, ok := hits[k]; ok {
			answered = append(answered, i)
			score[i] = v
		}
	}
	if len(answered) == 0 {
		return nil, ErrNotAsked
	}
	// Best first; equal scores keep file order.
	answered = topK(answered, n, func(a, b int) int {
		return cmp.Or(cmp.Compare(score[b], score[a]), cmp.Compare(a, b))
	})
	slices.Sort(answered)
	kept := make([]Chunk, len(answered))
	for j, i := range answered {
		kept[j] = chunks[i]
	}
	return kept, nil
}

// parallel runs fn over items with n workers and stops at the first error.
func parallel[T any](ctx context.Context, items []T, n int, fn func(context.Context, T) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	work := make(chan T)
	var wg sync.WaitGroup
	for range min(n, len(items)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				if err := fn(ctx, t); err != nil {
					cancel(err)
				}
			}
		}()
	}
feed:
	for _, t := range items {
		select {
		case work <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	return context.Cause(ctx)
}

func matchOf(ch Chunk, probability float64) Match {
	return Match{Path: ch.Path, Start: ch.Start, End: ch.End, Signature: ch.Signature, Probability: probability}
}

// rank finds the best chunk in one pass and sorts only the matches it
// keeps: with Top set, it picks them out without sorting the rest, which
// matters at the default threshold of 0, where every chunk matches.
func rank(question string, chunks []Chunk, scores [][]float64, q int, opts Options) Result {
	res := Result{Question: question}
	if len(chunks) == 0 {
		return res
	}
	match := func(c int) Match { return matchOf(chunks[c], scores[c][q]) }
	// better orders chunks best first. The chunk index breaks the last
	// ties, so the order never depends on how the sort works.
	better := func(a, b int) int {
		return cmp.Or(
			cmp.Compare(scores[b][q], scores[a][q]),
			cmp.Compare(chunks[a].Path, chunks[b].Path),
			cmp.Compare(chunks[a].Start, chunks[b].Start),
			cmp.Compare(a, b),
		)
	}
	best := 0
	var matched []int
	for c := range chunks {
		if better(c, best) < 0 {
			best = c
		}
		if scores[c][q] >= opts.Threshold {
			matched = append(matched, c)
		}
	}
	m := match(best)
	res.Best = &m

	k := len(matched)
	if opts.Top > 0 {
		k = min(k, opts.Top)
	}
	if k > 0 {
		res.Matches = make([]Match, k)
		for i, c := range topK(matched, k, better) {
			res.Matches[i] = match(c)
		}
	}
	return res
}

// topK returns the k best of idx (all of them if k ≥ len(idx)), best
// first by cmp, reusing idx's array. cmp must never return 0 for two
// different indexes, or ties would come out in an arbitrary order.
func topK(idx []int, k int, cmp func(a, b int) int) []int {
	if k >= len(idx) {
		slices.SortFunc(idx, cmp)
		return idx
	}
	if k <= 0 {
		return idx[:0]
	}
	// h is a heap of the k best seen so far, with the worst of them on top,
	// so each later index only has to beat h[0].
	h := idx[:k]
	for i := k/2 - 1; i >= 0; i-- {
		siftDown(h, i, cmp)
	}
	for _, x := range idx[k:] {
		if cmp(x, h[0]) < 0 {
			h[0] = x
			siftDown(h, 0, cmp)
		}
	}
	slices.SortFunc(h, cmp)
	return h
}

// siftDown restores the heap below h[i], with the worst index by cmp at the
// root.
func siftDown(h []int, i int, cmp func(a, b int) int) {
	for {
		w := 2*i + 1
		if w >= len(h) {
			return
		}
		if r := w + 1; r < len(h) && cmp(h[r], h[w]) > 0 {
			w = r
		}
		if cmp(h[w], h[i]) <= 0 {
			return
		}
		h[i], h[w] = h[w], h[i]
		i = w
	}
}

// dedupe drops questions that differ only in case or white space.
func dedupe(questions []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, q := range questions {
		q = jev.NormalizeSpace(q)
		if k := strings.ToLower(q); !seen[k] {
			seen[k] = true
			out = append(out, q)
		}
	}
	return out
}
