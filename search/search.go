// Package search runs the pipeline: key → cache → Jev → rank.
package search

import (
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
	Workers   int     // concurrent Jev calls; 0 means DefaultWorkers

	// OnCacheError hears about failed cache writes, which don't stop a search.
	OnCacheError func(error)

	// OnAsk hears how many chunks are about to be sent to Jev, when any are.
	OnAsk func(chunks int)
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
// can, and ranks the results. Each answer is cached as it arrives, so an
// interrupted run keeps its progress.
func Run(ctx context.Context, chunks []Chunk, client jev.Client, store cache.Cache, opts Options) ([]Result, error) {
	questions := dedupe(opts.Questions)
	instructions := make([]string, len(questions))
	for i, q := range questions {
		instructions[i] = jev.Instruction(q)
	}

	all, err := keyAll(chunks, opts.Model, instructions)
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
	var tasks []task
	for c := range chunks {
		t := task{chunk: c}
		for q := range questions {
			if v, ok := hits[keys[c][q]]; ok {
				scores[c][q] = v
			} else {
				t.missing = append(t.missing, q)
			}
		}
		if len(t.missing) > 0 {
			tasks = append(tasks, t)
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
		for i, t := range pack {
			if len(resp.Nouls[i]) != len(t.missing) {
				ch := chunks[t.chunk]
				return fmt.Errorf("%s:%d-%d: got %d answers for %d questions", ch.Path, ch.Start, ch.End, len(resp.Nouls[i]), len(t.missing))
			}
			for j, q := range t.missing {
				scores[t.chunk][q] = resp.Nouls[i][j]
				if err := store.Put(keys[t.chunk][q], resp.Nouls[i][j]); err != nil && opts.OnCacheError != nil {
					opts.OnCacheError(err)
				}
			}
		}
		return nil
	}
	if len(tasks) > 0 && opts.OnAsk != nil {
		opts.OnAsk(len(tasks))
	}
	if err := parallel(ctx, packs(chunks, tasks), cmp.Or(opts.Workers, DefaultWorkers), ask); err != nil {
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

// stateOf encodes a chunk as Jev would be shown it alone, for its cache
// keys.
func stateOf(ch Chunk) ([]byte, error) {
	return json.Marshal(jev.State{Path: ch.Rel, Code: ch.Code})
}

// keyAll builds every chunk's cache key for each instruction,
// keys[c*len(instructions)+q], one worker per CPU: hashing every chunk once
// per question is most of a cached search's time.
func keyAll(chunks []Chunk, model string, instructions []string) ([]cache.Key, error) {
	keys := make([]cache.Key, len(chunks)*len(instructions))
	errs := make([]error, len(chunks))
	each(len(chunks), func(c int) {
		sent, err := stateOf(chunks[c])
		if err != nil {
			errs[c] = err
			return
		}
		for q, in := range instructions {
			keys[c*len(instructions)+q] = cache.KeyOf(model, in, sent)
		}
	})
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
	keys, err := keyAll(chunks, model, []string{jev.Instruction(question)})
	if err != nil {
		return nil, err
	}
	hits, err := store.Get(keys)
	if err != nil {
		return nil, fmt.Errorf("reading cache: %w", err)
	}
	var answered []int
	for i, k := range keys {
		if _, ok := hits[k]; ok {
			answered = append(answered, i)
		}
	}
	if len(answered) == 0 {
		return nil, ErrNotAsked
	}
	slices.SortStableFunc(answered, func(a, b int) int { return cmp.Compare(hits[keys[b]], hits[keys[a]]) })
	answered = answered[:min(n, len(answered))]
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

// rank sorts only the chunks at or above the threshold, usually a handful,
// and finds the best of the rest in one pass.
func rank(question string, chunks []Chunk, scores [][]float64, q int, opts Options) Result {
	match := func(c int) Match {
		ch := chunks[c]
		return Match{Path: ch.Path, Start: ch.Start, End: ch.End, Signature: ch.Signature, Probability: scores[c][q]}
	}
	better := func(a, b Match) int {
		return cmp.Or(
			cmp.Compare(b.Probability, a.Probability),
			cmp.Compare(a.Path, b.Path),
			cmp.Compare(a.Start, b.Start),
		)
	}
	res := Result{Question: question}
	best := -1
	for c := range chunks {
		if best < 0 || better(match(c), match(best)) < 0 {
			best = c
		}
		if scores[c][q] >= opts.Threshold {
			res.Matches = append(res.Matches, match(c))
		}
	}
	if best >= 0 {
		m := match(best)
		res.Best = &m
	}
	slices.SortStableFunc(res.Matches, better)
	if opts.Top > 0 && len(res.Matches) > opts.Top {
		res.Matches = res.Matches[:opts.Top]
	}
	return res
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
