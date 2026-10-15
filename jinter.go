// Package jinter searches code by what it does, using TypeSafe's Jev model.
// Search walks the paths, splits files into chunks, asks Jev about each
// chunk (reusing cached answers) and ranks the results. The packages below
// it (walk, chunk, jev, cache, search) can also be used on their
// own.
package jinter

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/chunk"
	"github.com/K1erans/jinter/jev"
	"github.com/K1erans/jinter/search"
	"github.com/K1erans/jinter/walk"
)

// DefaultModel is the pinned Jev version.
const DefaultModel = "jev-1.13.0"

// DefaultMaxFileSize skips files over 1 MB: almost always bundles, lockfiles
// or data dumps, and expensive to send.
const DefaultMaxFileSize = 1 << 20

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

// Query is one search. Zero values take the defaults noted.
type Query struct {
	Questions []string // answered in the same Jev calls, so extra ones are nearly free
	Paths     []string // files and folders to search; default "."

	Threshold float64 // minimum probability to count as a match
	Top       int     // maximum matches per question; 0 means no limit

	NoIgnore    bool  // also search gitignored files
	Tests       bool  // also search tests
	Docs        bool  // also search documentation
	MaxFileSize int64 // default DefaultMaxFileSize; negative means no limit
	Model       string
	NoCache     bool // don't read cached answers (still writes them)

	// OnCacheError hears about cache failures, which don't stop a search.
	OnCacheError func(error)

	// OnAsk hears how many chunks are about to be sent to Jev, when any are.
	OnAsk func(chunks int)
}

// Report is a search's results and what it left out.
type Report struct {
	Results []search.Result // one per distinct question, in order
	Repo    string          // the repository searched, as used for its cache
	Chunks  int             // chunks searched

	// Files skipped, so callers can say so: nothing is skipped silently.
	Oversize     []walk.File
	Tests        int
	Generated    int
	Translations int
	Docs         int
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
	if len(q.Questions) == 0 {
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
	onCacheError := func(err error) {
		if q.OnCacheError != nil {
			q.OnCacheError(err)
		}
	}

	files, err := walk.Walk(paths, walk.Options{NoIgnore: q.NoIgnore, Tests: q.Tests, Docs: q.Docs, MaxFileSize: maxSize})
	if err != nil {
		return Report{}, err
	}
	rep := Report{
		Repo:         files.Repo,
		Oversize:     files.Oversize,
		Tests:        files.Tests,
		Generated:    files.Generated,
		Translations: files.Translations,
		Docs:         files.Docs,
	}

	open := e.OpenCache
	if open == nil {
		open = DiskCache
	}
	store, closeStore, err := open(files.Repo, model)
	if err != nil {
		onCacheError(fmt.Errorf("cache unavailable, answers won't be saved: %w", err))
		store, closeStore = cache.NewMemory(), func() error { return nil }
	}
	defer closeStore()

	chunks, err := search.Load(files.Files, search.LoadOptions{
		Chunk:        chunk.Default,
		Store:        store,
		ReadCache:    !q.NoCache,
		OnCacheError: onCacheError,
	})
	if err != nil {
		return rep, err
	}
	rep.Chunks = len(chunks)

	client := e.Client
	if client == nil {
		client = noClient{}
	}
	rep.Results, err = search.Run(ctx, chunks, client, store, search.Options{
		Questions:    q.Questions,
		Model:        model,
		Threshold:    q.Threshold,
		Top:          q.Top,
		ReadCache:    !q.NoCache,
		OnCacheError: onCacheError,
		OnAsk:        q.OnAsk,
	})
	return rep, err
}

// noClient stands in when Engine.Client is nil, so searches answered
// entirely from the cache still work.
type noClient struct{}

func (noClient) Ask(context.Context, jev.Request) (jev.Response, error) {
	return jev.Response{}, errors.New("no Jev client: set Engine.Client to ask about uncached chunks")
}
