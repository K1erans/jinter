// Package jinter searches code by what it does, using TypeSafe's Jev model.
// Search walks the paths, splits files into chunks, asks Jev about each
// chunk (reusing cached answers) and ranks the results. The packages below
// it (walk, chunk, prefilter, jev, cache, search) can also be used on their
// own.
package jinter

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/chunk"
	"github.com/K1erans/jinter/jev"
	"github.com/K1erans/jinter/prefilter"
	"github.com/K1erans/jinter/search"
	"github.com/K1erans/jinter/walk"
)

// DefaultModel is the pinned Jev version.
const DefaultModel = "jev-1.13.0"

// DefaultMaxFileSize skips files over 1 MB: almost always bundles, lockfiles
// or data dumps, and expensive to send.
const DefaultMaxFileSize = 1 << 20

// DefaultRefineTop is how many chunks Query.Refine searches. Replaying 24
// rewordings of 12 miniflux tasks, the right chunk was always in the earlier
// question's top 11 and kept its rank with only the top 30 searched; 300
// leaves room for bigger repos at about 9% of that 3,400-chunk scan.
const DefaultRefineTop = 300

// ErrNotAsked means Query.Refine named a question with no cached answers
// over these files.
var ErrNotAsked = search.ErrNotAsked

// Engine holds what a search talks to.
type Engine struct {
	// Client asks Jev about chunks. It is only called for answers that
	// aren't cached, so a nil Client still serves fully cached searches.
	Client jev.Client

	// OpenCache opens the cache for a repository (its main checkout's
	// folder, shared by its worktrees). Nil means DiskCache. If it fails,
	// the search carries on with an in-memory cache and reports the error
	// to Query.OnCacheError: a broken cache costs money, not correctness.
	OpenCache func(repo, model string) (cache.Cache, func() error, error)
}

// New returns an Engine that calls TypeSafe's API with apiKey and caches
// answers on disk.
func New(apiKey string, opts jev.HTTPOptions) *Engine {
	return &Engine{Client: jev.NewHTTP(apiKey, opts)}
}

// DiskCache opens the repo's SQLite cache in cache.Dir().
func DiskCache(repo, model string) (cache.Cache, func() error, error) {
	dir, err := cache.Dir()
	if err != nil {
		return nil, nil, err
	}
	c, err := cache.Open(cache.Path(dir, repo), model)
	if err != nil {
		return nil, nil, err
	}
	return c, c.Close, nil
}

// ClearCache deletes the on-disk cache of the repository holding path.
func ClearCache(path string) error {
	repo, err := walk.Repo(path)
	if err != nil {
		return err
	}
	dir, err := cache.Dir()
	if err != nil {
		return err
	}
	return cache.Clear(cache.Path(dir, repo))
}

// Query is one search. Zero values take the defaults noted.
type Query struct {
	Questions []string // answered in the same Jev calls, so extra ones are nearly free
	Paths     []string // files and folders to search; default "."

	// Prepass sends only chunks matching Keywords to Jev: faster and
	// cheaper, but misses code that uses other words. Keywords is a regexp;
	// empty means the questions' words minus common stopwords.
	Prepass  bool
	Keywords string

	// Refine retries a reworded question cheaply: only the RefineTop chunks
	// that best matched this earlier question (from its cached answers) are
	// searched. RefineTop defaults to DefaultRefineTop.
	Refine    string
	RefineTop int

	Threshold float64 // minimum probability to count as a match
	Top       int     // maximum matches per question; 0 means no limit

	NoIgnore    bool  // also search gitignored files
	Tests       bool  // also search tests; on by itself when a question is about tests
	Docs        bool  // also search documentation
	MaxFileSize int64 // default DefaultMaxFileSize; negative means no limit
	Model       string
	NoCache     bool // don't read cached answers (still writes them)

	// OnCacheError hears about cache failures, which don't stop a search.
	// Calls never overlap.
	OnCacheError func(error)

	// OnFiles hears which files were left out as soon as they are listed,
	// before any chunk is sent to Jev, so callers can say so up front. Only
	// Repo, TestsIncluded and the skipped and unreadable files are set.
	OnFiles func(Report)

	// OnAsk hears how many chunks are about to be sent to Jev, when any are.
	OnAsk func(chunks int)

	// OnMatch hears each chunk that reaches Threshold for a question as
	// soon as its answer is known, so callers can show matches while the
	// search is still running. Chunks that share words with the questions
	// are asked about first, so the probable matches tend to arrive early.
	// Matches come in no order of rank and Top doesn't limit them; the
	// Report is still complete and ranked. Calls never overlap, with each
	// other or with OnCacheError.
	OnMatch func(question string, m search.Match)
}

// Report is a search's results and what it left out.
type Report struct {
	Results []search.Result // one per distinct question, in order
	Repo    string          // the repository searched, as used for its cache
	Chunks  int             // chunks searched, after Prepass and Refine
	Split   int             // chunks the files split into, before Prepass and Refine

	// Keywords is the pattern Prepass kept chunks by: Query.Keywords, or
	// the one built from the questions.
	Keywords string

	// TestsIncluded is set when test files were searched because a question
	// was about tests, without Query.Tests.
	TestsIncluded bool

	// Files skipped, so callers can say so: nothing is skipped silently.
	Oversize     []walk.File
	Tests        int
	Generated    int
	Translations int
	Docs         int

	// Unreadable are the files and folders found under Paths that couldn't
	// be read, such as ones without read permission, each with its error.
	// The search carries on without them. A path named in Query.Paths that
	// is missing or can't be read fails the search instead.
	Unreadable []walk.Unreadable
}

// Matched reports whether any question had a match.
func (r Report) Matched() bool {
	for _, res := range r.Results {
		if len(res.Matches) > 0 {
			return true
		}
	}
	return false
}

// Search runs q.
func (e *Engine) Search(ctx context.Context, q Query) (Report, error) {
	if !slices.ContainsFunc(q.Questions, func(s string) bool { return strings.TrimSpace(s) != "" }) {
		return Report{}, errors.New("no question")
	}
	paths := q.Paths
	if len(paths) == 0 {
		paths = []string{"."}
	}
	model := cmp.Or(q.Model, DefaultModel)
	maxSize := cmp.Or(q.MaxFileSize, DefaultMaxFileSize)
	if maxSize < 0 {
		maxSize = 0
	}
	var cbMu sync.Mutex
	onCacheError := func(err error) {
		if q.OnCacheError != nil {
			cbMu.Lock()
			q.OnCacheError(err)
			cbMu.Unlock()
		}
	}
	var onMatch func(string, search.Match)
	if q.OnMatch != nil {
		onMatch = func(question string, m search.Match) {
			cbMu.Lock()
			q.OnMatch(question, m)
			cbMu.Unlock()
		}
	}

	open := e.OpenCache
	if open == nil {
		open = DiskCache
	}
	// Resolve the repository path up front (pure-Go .git lookup) and open the
	// cache concurrently with walk.Walk so SQLite open/init overlaps with file
	// discovery and reading.
	type openResult struct {
		store cache.Cache
		close func() error
		err   error
	}
	var openCh chan openResult
	var preRepo string
	if r, rerr := walk.Repo(paths[0]); rerr == nil {
		preRepo = r
		openCh = make(chan openResult, 1)
		go func() {
			s, cl, oerr := open(r, model)
			openCh <- openResult{store: s, close: cl, err: oerr}
		}()
	}

	tests := q.Tests || AboutTests(q.Questions)
	files, err := walk.Walk(paths, walk.Options{NoIgnore: q.NoIgnore, Tests: tests, Docs: q.Docs, MaxFileSize: maxSize})
	if err != nil {
		if openCh != nil {
			if res := <-openCh; res.err == nil && res.close != nil {
				_ = res.close()
			}
		}
		return Report{}, err
	}
	rep := Report{
		Repo:          files.Repo,
		TestsIncluded: tests && !q.Tests,
		Oversize:      files.Oversize,
		Tests:         files.Tests,
		Generated:     files.Generated,
		Translations:  files.Translations,
		Docs:          files.Docs,
		Unreadable:    files.Unreadable,
	}
	if q.OnFiles != nil {
		q.OnFiles(rep)
	}

	var store cache.Cache
	var closeStore func() error
	if openCh != nil {
		res := <-openCh
		if preRepo == files.Repo {
			store, closeStore, err = res.store, res.close, res.err
		} else {
			if res.err == nil && res.close != nil {
				_ = res.close()
			}
			store, closeStore, err = open(files.Repo, model)
		}
	} else {
		store, closeStore, err = open(files.Repo, model)
	}
	if err != nil {
		onCacheError(fmt.Errorf("cache unavailable, answers won't be saved: %w", err))
		store, closeStore = cache.NewMemory(), func() error { return nil }
	}
	// Defer saving freshly computed chunk layouts until after Run starts
	// asking Jev (overlapping SQLite disk writes with network RTT), and always
	// flush before closing the store.
	var pendingChunks map[cache.Key][]byte
	var chunkSaveWG sync.WaitGroup
	var chunkSaveOnce sync.Once
	startChunkSave := func() {
		chunkSaveOnce.Do(func() {
			if len(pendingChunks) == 0 {
				return
			}
			layouts := pendingChunks
			pendingChunks = nil
			chunkSaveWG.Go(func() {
				if err := store.PutChunks(layouts); err != nil {
					onCacheError(fmt.Errorf("saving chunk cache: %w", err))
				}
			})
		})
	}
	defer func() {
		startChunkSave()
		chunkSaveWG.Wait()
		if err := closeStore(); err != nil {
			onCacheError(fmt.Errorf("closing cache: %w", err))
		}
	}()

	loadStore := deferredChunkStore{Cache: store, onPut: func(layouts map[cache.Key][]byte) {
		pendingChunks = layouts
	}}
	chunks, err := search.LoadContext(ctx, files.Files, search.LoadOptions{
		Chunk:        chunk.Default,
		Store:        loadStore,
		ReadCache:    !q.NoCache,
		OnCacheError: onCacheError,
	})
	if err != nil {
		return rep, err
	}
	rep.Split = len(chunks)
	if q.Prepass {
		f, err := prefilter.New(q.Keywords, q.Questions)
		if err != nil {
			return rep, fmt.Errorf("prepass: %w", err)
		}
		rep.Keywords = f.Pattern()
		if chunks, err = prepass(ctx, chunks, f); err != nil {
			return rep, err
		}
	}
	if q.Refine != "" {
		top := q.RefineTop
		if top <= 0 {
			top = DefaultRefineTop
		}
		chunks, err = search.Narrow(chunks, store, model, q.Refine, top)
		if err != nil {
			return rep, fmt.Errorf("refine %q: %w", q.Refine, err)
		}
	}
	rep.Chunks = len(chunks)

	client := e.Client
	if client == nil {
		client = noClient{}
	}
	// Asking first about chunks that share a word with the questions only
	// matters to a caller watching matches arrive, and not after a keyword
	// Prepass, when every chunk left shares one.
	var likely func(string) bool
	if q.OnMatch != nil && !(q.Prepass && q.Keywords == "") {
		if f, err := prefilter.New("", q.Questions); err == nil {
			likely = f.Keep
		}
	}
	onAsk := func(n int) {
		// Start saving chunk layouts in the background right as Jev calls begin.
		startChunkSave()
		if q.OnAsk != nil {
			q.OnAsk(n)
		}
	}
	rep.Results, err = search.Run(ctx, chunks, client, store, search.Options{
		Questions:    q.Questions,
		Model:        model,
		Threshold:    q.Threshold,
		Top:          q.Top,
		ReadCache:    !q.NoCache,
		OnCacheError: onCacheError,
		OnAsk:        onAsk,
		OnMatch:      onMatch,
		Likely:       likely,
	})
	return rep, err
}

// prepass keeps the chunks f matches, in order. Matching a keyword regexp
// runs at only a few MB/s, seconds on a big repo, so it runs on every CPU.
func prepass(ctx context.Context, chunks []search.Chunk, f *prefilter.Filter) ([]search.Chunk, error) {
	keep := make([]bool, len(chunks))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(chunks)) {
		wg.Go(func() {
			for ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= len(chunks) {
					return
				}
				keep[i] = f.Keep(chunks[i].Code)
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var kept []search.Chunk
	for i, c := range chunks {
		if keep[i] {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// deferredChunkStore intercepts PutChunks during LoadContext so Engine.Search
// can overlap saving new chunk layouts with Jev network calls.
type deferredChunkStore struct {
	cache.Cache
	onPut func(map[cache.Key][]byte)
}

func (s deferredChunkStore) PutChunks(layouts map[cache.Key][]byte) error {
	s.onPut(layouts)
	return nil
}

// noClient stands in when Engine.Client is nil, so searches answered
// entirely from the cache still work.
type noClient struct{}

func (noClient) Ask(context.Context, jev.Request) (jev.Response, error) {
	return jev.Response{}, errors.New("no Jev client: set Engine.Client to ask about uncached chunks")
}

// testWords in a question mean it is looking for tests, which are
// otherwise skipped. "spec" is left out, as API specs are production code.
var testWords = map[string]bool{
	"test": true, "tests": true, "testing": true, "unittest": true, "unittests": true,
	"testdata": true, "fixture": true, "fixtures": true, "mock": true, "mocks": true, "mocked": true,
}

// AboutTests reports whether any question asks about tests: "the tests
// for retries", "mocks the payment client". "tests" followed by whether or
// if is a verb, as in "tests whether the token expired", which asks about
// production code.
func AboutTests(questions []string) bool {
	for _, q := range questions {
		words := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		for i, w := range words {
			if !testWords[w] {
				continue
			}
			if i+1 < len(words) && strings.HasPrefix(w, "test") {
				switch words[i+1] {
				case "whether", "if":
					continue
				}
			}
			return true
		}
	}
	return false
}
