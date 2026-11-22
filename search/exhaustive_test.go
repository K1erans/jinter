package search

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/jev"
)

// This file checks Run's answer bookkeeping on every small search, not on
// hand-picked ones. The agent skill that explains how to read a failure and
// when to update this file is .claude/skills/verify-answer-bookkeeping.
//
// The promise: whatever is already cached and however chunks are packed
// into Jev calls, every (chunk, question) pair ends up with the answer Jev
// gave for that chunk and that question, in the results and in the cache,
// and Jev is asked about exactly the pairs the cache didn't have. OnMatch
// hears each of those answers once, and Likely only reorders the calls.
//
// The scope is every search with
//
//   - up to exChunksTwoQ chunks and 1 or 2 questions, or up to exChunksOneQ
//     chunks and 1 question,
//   - the chunks split across files in every way that keeps a file's chunks
//     together,
//   - each chunk's code one of exCodes, sized so that packs fill exactly,
//     overflow, and hold a chunk too big to share a call; equal codes in
//     one file make two chunks share a cache key,
//   - every subset of the cache keys answered beforehand,
//   - ReadCache on (via both Cache.Get and DirectGetter.GetInto) and off,
//     1 and 3 workers, and 1 worker with Likely putting the calls that hold a
//     chunk of the second or fourth code first,
//   - Jev answering everything, or failing every call that carries one
//     chosen chunk.
//
// It does not enumerate goroutine schedules: each case runs once per worker
// count. Under -race the chunk bounds are one smaller (see
// exhaustive_bounds_race_test.go), which keeps the race run to seconds.
const exModel = "jev-1"

// exCodes are the chunk bodies. Two halves fill a pack exactly, so a third
// chunk must start a new call, and the last is too big to share one.
var exCodes = [...]string{
	"a",
	"b",
	strings.Repeat("h", packBytes/2),
	strings.Repeat("x", packBytes+1),
}

var exCodeNames = [...]string{"small a", "small b", "half a pack", "bigger than a pack"}

var exQuestions = [...]string{"q0", "q1"}

const exMaxFiles = exChunksOneQ

func exRel(file int) string { return fmt.Sprintf("f%d.go", file) }

// exAnswer is what Jev says about one chunk and one question. Every
// (file, code, question) has its own value, so an answer filed under the
// wrong chunk or question is always visible. All are below 0.5.
func exAnswer(file, code, question int) float64 {
	return 0.001 + 0.1*float64(file) + 0.01*float64(code) + 0.001*float64(question)
}

// exStale is the answer already in the cache for a key that was answered
// beforehand. It differs from every exAnswer, so a cached answer that was
// asked again, or a fresh one that should have come from the cache, shows.
func exStale(file, code, question int) float64 { return exAnswer(file, code, question) + 0.5 }

// exKeys[file][code][question] is the cache key, built the way the docs
// define it (model, phrased question, the chunk as sent alone) and not
// through Run's own key builder.
var exKeys = sync.OnceValue(func() (keys [exMaxFiles][len(exCodes)][len(exQuestions)]cache.Key) {
	for f := range keys {
		for c := range keys[f] {
			state, err := json.Marshal(jev.State{Path: exRel(f), Code: exCodes[c]})
			if err != nil {
				panic(err)
			}
			for q := range keys[f][c] {
				keys[f][c][q] = cache.KeyOf(exModel, jev.Instruction(exQuestions[q]), state)
			}
		}
	}
	return keys
})

// exCase is one search.
type exCase struct {
	files     []int // files[c] is chunk c's file; a file's chunks are neighbours
	codes     []int // codes[c] indexes exCodes
	questions int
	cached    uint // bit i: the i-th distinct cache key was answered beforehand
	readCache bool
	directGet bool // store implements cache.DirectGetter (only when readCache is true)
	workers   int
	likely    bool // Likely is set, and picks exLikely's codes
	fail      int  // Jev fails every call carrying this chunk; -1 for none
}

// exLikely is the Likely used when a case sets one: two of the four codes,
// one small and one too big to share a call.
func exLikely(code string) bool { return code == exCodes[1] || code == exCodes[3] }

type exPair struct{ file, code, question int }

func (c exCase) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d question(s), ReadCache=%v", c.questions, c.readCache)
	if c.readCache && c.directGet {
		b.WriteString(" (DirectGetter)")
	}
	fmt.Fprintf(&b, ", Workers=%d", c.workers)
	if c.likely {
		fmt.Fprintf(&b, ", Likely picks %q and %q", exCodeNames[1], exCodeNames[3])
	}
	if c.fail >= 0 {
		fmt.Fprintf(&b, ", Jev fails calls carrying chunk %d", c.fail)
	}
	seen := map[exPair]int{}
	for i := range c.files {
		fmt.Fprintf(&b, "\n  chunk %d: %s, code %q, cached beforehand:", i, exRel(c.files[i]), exCodeNames[c.codes[i]])
		for q := range c.questions {
			p := exPair{c.files[i], c.codes[i], q}
			bit, ok := seen[p]
			if !ok {
				bit = len(seen)
				seen[p] = bit
			}
			if c.cached>>bit&1 == 1 {
				fmt.Fprintf(&b, " %s", exQuestions[q])
			}
		}
	}
	return b.String()
}

// exStore is a cache that records every write.
type exStore struct {
	mu   sync.Mutex
	m    map[cache.Key]float64
	puts map[cache.Key][]float64
}

func (s *exStore) Get(keys []cache.Key) (map[cache.Key]float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[cache.Key]float64{}
	for _, k := range keys {
		if v, ok := s.m[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (s *exStore) PutAnswers(answers []cache.Answer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range answers {
		s.m[a.Key] = a.Noul
		s.puts[a.Key] = append(s.puts[a.Key], a.Noul)
	}
	return nil
}

func (*exStore) GetChunks([]cache.Key) (map[cache.Key][]byte, error) { return nil, nil }
func (*exStore) PutChunks(map[cache.Key][]byte) error                { return nil }

// exDirectStore wraps exStore and implements cache.DirectGetter so the
// exhaustive check exercises Run's GetInto fast path as well as Cache.Get.
type exDirectStore struct{ *exStore }

func (s *exDirectStore) GetInto(keys []cache.Key, vals []float64, found []bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range keys {
		if v, ok := s.m[k]; ok {
			vals[i] = v
			found[i] = true
		}
	}
	return nil
}

// exJev answers from exAnswer, knowing only what a real Jev would: the
// call's path, each chunk's code and its questions.
type exJev struct {
	failRel, failCode string // fail calls for this file carrying this code
	mu                sync.Mutex
	asked             map[exPair]int
	likely            []bool // for each call in order, whether it held a likely chunk
	problems          []string
}

var errExJev = errors.New("jev is down")

func (j *exJev) Ask(_ context.Context, req jev.Request) (jev.Response, error) {
	var file int
	if _, err := fmt.Sscanf(req.Path, "f%d.go", &file); err != nil {
		return jev.Response{}, fmt.Errorf("unexpected path %q", req.Path)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if req.Model != exModel {
		j.problems = append(j.problems, fmt.Sprintf("a call used model %q", req.Model))
	}
	size, fail, likely := 0, false, false
	nouls := make([][]float64, len(req.Chunks))
	defer func() { j.likely = append(j.likely, likely) }()
	for i, ch := range req.Chunks {
		size += len(ch.Code)
		likely = likely || exLikely(ch.Code)
		code := slices.Index(exCodes[:], ch.Code)
		if code < 0 {
			return jev.Response{}, errors.New("unexpected code")
		}
		if len(ch.Questions) == 0 {
			j.problems = append(j.problems, fmt.Sprintf("a call sent %s code %q with no question, paying for a chunk whose answers were all cached", req.Path, exCodeNames[code]))
		}
		fail = fail || (req.Path == j.failRel && ch.Code == j.failCode)
		for _, question := range ch.Questions {
			q := slices.Index(exQuestions[:], question)
			if q < 0 {
				return jev.Response{}, fmt.Errorf("unexpected question %q", question)
			}
			j.asked[exPair{file, code, q}]++
			nouls[i] = append(nouls[i], exAnswer(file, code, q))
		}
	}
	if len(req.Chunks) > 1 && size > packBytes {
		j.problems = append(j.problems, fmt.Sprintf("a call for %s packed %d chunks holding %d bytes, over the %d-byte cap", req.Path, len(req.Chunks), size, packBytes))
	}
	if fail {
		return jev.Response{}, errExJev
	}
	return jev.Response{Model: exModel, Nouls: nouls}, nil
}

// check runs one search and returns what it got wrong, in plain words.
func (c exCase) check() (problems []string) {
	keys := exKeys()
	chunks := make([]Chunk, len(c.files))
	for i, f := range c.files {
		line := 1
		if i > 0 && c.files[i-1] == f {
			line = chunks[i-1].End + 1
		}
		chunks[i].Path, chunks[i].Rel = "/repo/"+exRel(f), exRel(f)
		chunks[i].Start, chunks[i].End, chunks[i].Code = line, line+9, exCodes[c.codes[i]]
	}

	// What should happen, worked out pair by pair with no packing involved.
	store := &exStore{m: map[cache.Key]float64{}, puts: map[cache.Key][]float64{}}
	bits := map[exPair]int{}
	want := make([][]float64, len(chunks)) // want[c][q] is the answer the search must report
	wantAsked := map[exPair]int{}
	failHasWork := false
	for i := range chunks {
		want[i] = make([]float64, c.questions)
		for q := range c.questions {
			p := exPair{c.files[i], c.codes[i], q}
			bit, ok := bits[p]
			if !ok {
				bit = len(bits)
				bits[p] = bit
			}
			cached := c.cached>>bit&1 == 1
			if cached {
				store.m[keys[p.file][p.code][q]] = exStale(p.file, p.code, q)
			}
			if cached && c.readCache {
				want[i][q] = exStale(p.file, p.code, q)
				continue
			}
			want[i][q] = exAnswer(p.file, p.code, q)
			wantAsked[p]++
			if c.fail >= 0 && p.file == c.files[c.fail] && p.code == c.codes[c.fail] {
				failHasWork = true
			}
		}
	}

	client := &exJev{asked: map[exPair]int{}}
	if c.fail >= 0 {
		client.failRel, client.failCode = exRel(c.files[c.fail]), exCodes[c.codes[c.fail]]
	}
	// heard[c][q] are the probabilities OnMatch reported for chunk c and
	// question q. Run promises its calls never overlap, so no lock: the
	// race detector checks that promise.
	heard := make([][][]float64, len(chunks))
	for i := range heard {
		heard[i] = make([][]float64, c.questions)
	}
	var strays []string
	opts := Options{
		Questions: exQuestions[:c.questions],
		Model:     exModel,
		ReadCache: c.readCache,
		Workers:   c.workers,
		OnMatch: func(question string, m Match) {
			q := slices.Index(exQuestions[:c.questions], question)
			i := slices.IndexFunc(chunks, func(ch Chunk) bool { return ch.Path == m.Path && ch.Start == m.Start && ch.End == m.End })
			if q < 0 || i < 0 {
				strays = append(strays, fmt.Sprintf("OnMatch heard %+v for question %q, which is no chunk or question of this search", m, question))
				return
			}
			heard[i][q] = append(heard[i][q], m.Probability)
		},
	}
	if c.likely {
		opts.Likely = exLikely
	}
	var runStore cache.Cache = store
	if c.directGet {
		runStore = &exDirectStore{store}
	}
	results, err := Run(context.Background(), chunks, client, runStore, opts)
	say := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	problems = append(problems, client.problems...)
	problems = append(problems, strays...)

	// What OnMatch heard: each chunk's own answer, at most once, and
	// exactly once when the search finished. This holds even when the
	// search failed part-way.
	for i := range chunks {
		for q := range c.questions {
			for _, v := range heard[i][q] {
				if v != want[i][q] {
					say("OnMatch heard %v for chunk %d question %s, but its answer is %v", v, i, exQuestions[q], want[i][q])
				}
			}
			if n := len(heard[i][q]); n > 1 || (err == nil && n != 1) {
				say("OnMatch heard about chunk %d question %s %d time(s), want 1", i, exQuestions[q], n)
			}
		}
	}

	// Likely only reorders: with one worker, every call holding a likely
	// chunk is made before any call holding none.
	if c.likely && c.workers == 1 {
		if i := slices.Index(client.likely, false); i >= 0 && slices.Contains(client.likely[i:], true) {
			say("call %d held no likely chunk, but a later call did: Likely should have put that one first", i)
		}
	}

	// The cache: only answers Jev gave, each under its own chunk's key.
	// This holds even when the search failed part-way.
	for p, n := range wantAsked {
		for _, v := range store.puts[keys[p.file][p.code][p.question]] {
			if v != exAnswer(p.file, p.code, p.question) {
				say("the cache got %v for %s code %q question %s, but Jev's answer for it is %v: a later search would report another chunk's or question's answer",
					v, exRel(p.file), exCodeNames[p.code], exQuestions[p.question], exAnswer(p.file, p.code, p.question))
			}
		}
		if got := len(store.puts[keys[p.file][p.code][p.question]]); got > n || (err == nil && got != n) {
			say("%s code %q question %s was cached %d time(s), want %d", exRel(p.file), exCodeNames[p.code], exQuestions[p.question], got, n)
		}
	}
	for p := range bits {
		if wantAsked[p] == 0 && len(store.puts[keys[p.file][p.code][p.question]]) > 0 {
			say("%s code %q question %s was already cached but was written again", exRel(p.file), exCodeNames[p.code], exQuestions[p.question])
		}
	}
	if len(store.m) > len(bits) {
		say("the cache holds %d keys, but the search only has %d distinct (chunk, question) keys", len(store.m), len(bits))
	}

	// What Jev was asked: nothing the cache had, nothing twice.
	for p, n := range client.asked {
		if n > wantAsked[p] {
			say("Jev was asked %d time(s) about %s code %q question %s, want %d", n, exRel(p.file), exCodeNames[p.code], exQuestions[p.question], wantAsked[p])
		}
	}

	if failHasWork {
		if !errors.Is(err, errExJev) {
			say("Jev failed a call the search needed, but Run returned error %v", err)
		}
		return problems
	}
	if err != nil {
		say("Run failed: %v", err)
		return problems
	}
	for p, n := range wantAsked {
		if client.asked[p] < n { // asking too often is reported above
			say("Jev was asked %d time(s) about %s code %q question %s, want %d", client.asked[p], exRel(p.file), exCodeNames[p.code], exQuestions[p.question], n)
		}
	}

	// The results: every chunk, with its own answer, best first.
	if len(results) != c.questions {
		say("got %d results for %d questions", len(results), c.questions)
		return problems
	}
	for q, res := range results {
		order := make([]int, len(chunks))
		for i := range order {
			order[i] = i
		}
		slices.SortFunc(order, func(a, b int) int {
			return cmp.Or(cmp.Compare(want[b][q], want[a][q]), cmp.Compare(chunks[a].Path, chunks[b].Path), cmp.Compare(chunks[a].Start, chunks[b].Start))
		})
		wantMatches := make([]Match, len(order))
		for i, ch := range order {
			wantMatches[i] = Match{Path: chunks[ch].Path, Start: chunks[ch].Start, End: chunks[ch].End, Probability: want[ch][q]}
		}
		if res.Question != exQuestions[q] {
			say("result %d is for question %q, want %q", q, res.Question, exQuestions[q])
		}
		if !slices.Equal(res.Matches, wantMatches) {
			say("question %s reported\n      %+v\n    want\n      %+v", exQuestions[q], res.Matches, wantMatches)
		}
		switch {
		case len(chunks) == 0 && res.Best != nil:
			say("question %s has a best match with no chunks", exQuestions[q])
		case len(chunks) > 0 && (res.Best == nil || *res.Best != wantMatches[0]):
			say("question %s's best match is %+v, want %+v", exQuestions[q], res.Best, wantMatches[0])
		}
	}
	return problems
}

// exShapes lists every way to lay out chunks and questions; each shape is
// then run against every cache state, option and failure.
func exShapes() []exCase {
	var shapes []exCase
	for _, bound := range []struct{ chunks, questions int }{{exChunksTwoQ, 2}, {exChunksOneQ, 1}} {
		for n := 0; n <= bound.chunks; n++ {
			if bound.questions == 1 && n <= exChunksTwoQ {
				// Covered by the two-question bound's one-question cases.
				continue
			}
			for q := 1; q <= bound.questions; q++ {
				// Bit i of breaks starts a new file before chunk i+1.
				for breaks := 0; breaks < 1<<max(n-1, 0); breaks++ {
					files := make([]int, n)
					for i := 1; i < n; i++ {
						files[i] = files[i-1] + breaks>>(i-1)&1
					}
					codes := make([]int, n)
					for {
						shapes = append(shapes, exCase{files: files, codes: slices.Clone(codes), questions: q})
						i := 0
						for ; i < n; i++ {
							if codes[i]++; codes[i] < len(exCodes) {
								break
							}
							codes[i] = 0
						}
						if i == n {
							break
						}
					}
				}
			}
		}
	}
	return shapes
}

func TestEveryAnswerLandsOnItsOwnChunk(t *testing.T) {
	shapes := exShapes()
	var next, cases, failures atomic.Int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= len(shapes) || failures.Load() >= 3 {
					return
				}
				c := shapes[i]
				distinct := map[exPair]bool{}
				for j := range c.files {
					for q := range c.questions {
						distinct[exPair{c.files[j], c.codes[j], q}] = true
					}
				}
				for c.cached = 0; c.cached < 1<<len(distinct); c.cached++ {
					for _, cacheMode := range []struct {
						read, direct bool
					}{{true, false}, {true, true}, {false, false}} {
						c.readCache, c.directGet = cacheMode.read, cacheMode.direct
						for _, run := range []struct {
							workers int
							likely  bool
						}{{1, false}, {3, false}, {1, true}} {
							c.workers, c.likely = run.workers, run.likely
							for c.fail = -1; c.fail < len(c.files); c.fail++ {
								cases.Add(1)
								if problems := c.check(); len(problems) > 0 {
									if failures.Add(1) <= 3 {
										mu.Lock()
										t.Errorf("search: %v\n  wrong:\n    %s", c, strings.Join(problems, "\n    "))
										mu.Unlock()
									}
									return
								}
							}
						}
					}
				}
			}
		})
	}
	wg.Wait()
	t.Logf("checked %d searches", cases.Load())
}
