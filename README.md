# jinter

[![Go Reference](https://pkg.go.dev/badge/github.com/K1erans/jinter.svg)](https://pkg.go.dev/github.com/K1erans/jinter)

Find code by what it *does*, from Go, using TypeSafe's Jev model.

Ask a question in plain words, such as `"retries failed payments"`, and get back the functions that do it, ranked by probability. No symbol names or regular expressions are needed.

```
0.94  src/billing/retry.go:12-80   func RetryFailedPayment(invoice Invoice, attempt int) error
0.81  src/jobs/payment.go:40-95    func handlePaymentFailure(job Job) error
```

`jinter` is the search engine behind [jrep](https://github.com/K1erans/Jrep), exposed as a standalone Go library (`jinter` and its subpackages `walk`, `chunk`, `prefilter`, `jev`, `cache`, and `search`).

## Why jinter

Keyword search (`rg`) only works when you already know what the code calls things. Embedding search returns loose similarity neighbors and requires maintaining an index. TypeSafe's Jev model answers a direct probability question (`noul` in `[0, 1]`) about a piece of code, but scanning a repository naively wastes tokens on noise, hits rate limits, and re-pays on every run.

`jinter` handles the systems work needed to make model-backed code search fast, cheap, and exact:

- **Syntax-aware chunking across 206 languages, with a `go/parser` fast path.** Splits files at function, method, class, and declaration boundaries using pure-Go tree-sitter (`gotreesitter`, 206 embedded grammars). Go files take a standard-library `go/parser` fast path (**233 MB/s** on Apple M3, matching tree-sitter boundaries across all 7,710 files in `GOROOT`) and call tree-sitter only to subdivide declarations too large for a single chunk.
- **Aggressive noise filtering before the network.** Walks repositories via `git ls-files` (or `.gitignore` parsing outside git) and drops binaries, generated artifacts (lockfiles, source maps, minified bundles, SVGs, `node_modules`), translation catalogs (which accounted for 43% of chunks in `miniflux` without changing a single match rank), docs, and files over 1 MB. Test files are skipped automatically unless a question asks about tests (`AboutTests`). Nothing is dropped silently: `Report` and `OnFiles` report every skipped category before any chunk goes to Jev.
- **Multi-chunk and multi-question request packing.** Every Jev API call carries a fixed ~240-token framing cost. `jinter` packs up to 24 KB of chunks from the same file into one request (`code_1`, `code_2`, ...) and asks all of a query's questions (`q0`, `q1`, ...) in that same call. Packing cuts scan tokens by about a third, and adding extra questions to a scan costs almost nothing.
- **Line-number-free SQLite cache shared across git worktrees.** Each answer key hashes `(model, normalized question, repo-relative path, chunk code)` without line numbers, so edits near the top of a file never invalidate unmodified functions below them. The SQLite store drives `modernc.org/sqlite/lib` directly with reusable C buffers, cached prepared statements, WAL mode, and a group-commit queue, reading 20,000 cached answers in **4.8 ms** and finishing a warm repository search in **17.7 ms**.
- **Early match streaming (`OnMatch`) with keyword-prioritized scheduling.** When `OnMatch` is set, cached matches stream immediately and uncached chunks that share words with the questions are scheduled first. On a cold 1.4 MB TypeScript repository scan, the top matches arrive in **0.7 to 1.0 s** of a 1.7 s search.
- **Exhaustively verified answer bookkeeping.** Because `search.Run` packs multiple chunks and questions into shared API requests while merging partial cache hits across concurrent workers, `TestEveryAnswerLandsOnItsOwnChunk` runs **1.68 million search permutations** in ~9 seconds to verify that no answer is ever filed under the wrong chunk, question, or cache key.

## Performance and verification

Measured on Apple M3 (`go test -bench=. . ./cache ./chunk`) and repository benchmark corpora:

| Operation | Measurement | Details |
|---|---:|---|
| Warm end-to-end search (225 files, 3 questions, SQLite) | **17.7 ms** | Overlapping cache open, layout cache hit, and batched answer lookup (`BenchmarkSearchEndToEnd/warm-cached-sqlite`). |
| SQLite answer lookup (20,000 cached keys) | **4.8 ms** | Direct `modernc.org/sqlite/lib` driver with caller-slice `GetInto` (`BenchmarkGet/hits-20000`). |
| Cache key hashing (`sha256` with prefix reuse) | **24.1 us** | Pre-hashed `(model, question)` prefix via `cache.Keyer` (`BenchmarkKeyOf`). |
| Go AST chunk splitting (5,000 functions) | **233 MB/s** | `go/parser` fast path, 3.9 ms for 5,000 functions (`BenchmarkSplit/go-5000funcs`). |
| JSON chunking on long lines (20 workers) | **115 MB/s** | Bounded rune scanning in `signature()` (up from 3.8 MB/s on TextMate grammars). |
| Reworded retry narrowing (`Query.Refine`) | **~9% cost** | Re-asks only the top 300 chunks from an earlier question's cached answers. |
| Exhaustive answer-bookkeeping verification | **1.68M searches / 9 s** | Checks all small chunk, question, pack-size, cache-hit, worker, and failure combinations (`search/exhaustive_test.go`). |

## Install

```sh
go get github.com/K1erans/jinter
```

You need a TypeSafe API key (`TYPESAFE_API_KEY`) for any answer that is not already in the cache.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/K1erans/jinter"
	"github.com/K1erans/jinter/jev"
)

func main() {
	engine := jinter.New(os.Getenv("TYPESAFE_API_KEY"), jev.HTTPOptions{})
	rep, err := engine.Search(context.Background(), jinter.Query{
		Questions: []string{"retries failed payments", "sends the receipt email"},
		Paths:     []string{"src/billing"},
		Threshold: 0.6,
		Top:       10,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range rep.Results {
		fmt.Println("#", r.Question)
		for _, m := range r.Matches {
			fmt.Printf("%.2f  %s:%d-%d  %s\n", m.Probability, m.Path, m.Start, m.End, m.Signature)
		}
	}
}
```

Create one `Engine` and reuse it across searches so they share its adaptive rate limiter.

### Showing matches as they arrive

`Search` returns once every chunk has been answered. To stream matches while the search is still running, set `OnMatch`:

```go
rep, err := engine.Search(ctx, jinter.Query{
	Questions: []string{"retries failed payments"},
	Threshold: 0.6,
	OnMatch: func(question string, m search.Match) {
		fmt.Printf("found %.2f  %s:%d-%d\n", m.Probability, m.Path, m.Start, m.End)
	},
})
```

Cached matches are delivered first, followed by live matches as each Jev call returns. Chunks that share a word with the questions are asked about first so likely hits arrive early. `OnMatch` callbacks never overlap, arrive unranked, and are not capped by `Top`. The final `Report` still returns the complete, ranked match list.

## How a search works

1. **List files (`walk`).** Follows `.gitignore` and skips binaries, generated files (lockfiles, source maps, minified files, SVGs, `node_modules`), and translation catalogs. Tests and docs are skipped unless requested, or unless a question asks about tests. Unreadable files or subdirectories are recorded in `Report.Unreadable` while the rest of the search continues; an unreadable root path named in `Query.Paths` returns an error.
2. **Split into chunks (`chunk`).** Parses files into function- or block-sized chunks (~10 to 300 lines) with tree-sitter (206 grammars) or `go/parser` for Go files, attaching doc comments and headers to the declaration they describe. Files with no grammar or syntax errors fall back to an indentation and blank-line splitter. File chunk layouts are cached in SQLite by content hash.
3. **Narrow (`prefilter` and `search.Narrow`).** Optionally filters chunks locally with a case-insensitive keyword regular expression (`Prepass`), or restricts a reworded retry to the top `RefineTop` chunks that best matched an earlier question in the cache (`Refine`).
4. **Ask Jev (`jev` and `search.Run`).** Looks up all `(model, question, chunk)` keys in one batch, packs cache misses from the same file into shared requests (up to 24 KB per call), and dispatches them in parallel through an adaptive token- and request-rate limiter with exponential backoff on HTTP 429 and 529.
5. **Save and rank (`cache` and `search.Run`).** Writes each call's answers to SQLite as soon as it returns so interrupted runs keep their progress, then ranks every chunk per question from highest probability to lowest.

> [!NOTE]
> Every uncached chunk searched is sent to TypeSafe. Cost and latency scale with the amount of code searched, so pass the narrowest `Paths` that fit your question.

## Query

| Field | Purpose | Default |
|---|---|---|
| `Questions` | Plain-language descriptions of what the code does, such as `"sends the password reset email"` | required |
| `Paths` | Files and directories to search | `["."]` |
| `Threshold` | Minimum probability in `[0, 1]` to count as a match | `0` |
| `Top` | Maximum matches returned per question (`0` means no limit) | `0` |
| `Prepass`, `Keywords` | Send Jev only chunks matching a regular expression; when `Keywords` is empty, uses non-stopword terms from `Questions` | `false`, `""` |
| `Refine`, `RefineTop` | Search only the chunks that best matched this earlier question in the cache | `""`, `300` |
| `Tests`, `Docs`, `NoIgnore` | Include test files, documentation, or gitignored files | `false` |
| `MaxFileSize` | Skip larger files (negative means no limit) | `1 MB` |
| `Model` | Jev model version | `"jev-1.13.0"` |
| `NoCache` | Ignore cached answers on read (new answers are still written) | `false` |
| `OnFiles` | Called with skipped and unreadable file counts before any chunk is sent to Jev | `nil` |
| `OnAsk` | Called with the number of uncached chunks about to be sent to Jev | `nil` |
| `OnMatch` | Called as soon as a chunk reaches `Threshold` for a question (cached hits first, then keyword-likely chunks first) | `nil` |
| `OnCacheError` | Called on non-fatal cache read or write errors (never overlaps) | `nil` |

## Report

| Field | Contents |
|---|---|
| `Results` | One `search.Result` per distinct question: `Matches` ordered best first, plus `Best` even when nothing reached `Threshold` |
| `Matched()` | Reports whether any question had at least one match |
| `Repo` | Canonical repository path used to select the cache database |
| `Chunks`, `Split` | Chunks searched after `Prepass` and `Refine`, and total chunks split before narrowing |
| `Keywords` | Regular expression used by `Prepass` |
| `Oversize`, `Tests`, `Docs`, `Generated`, `Translations` | Skipped file lists and counts so callers can report everything left out |
| `Unreadable` | Files and directories under `Paths` that could not be read (`Path`, `Rel`, `Err`) |
| `TestsIncluded` | True when test files were included automatically because a question asked about tests |

## Caching

Answers and file chunk layouts are stored in SQLite, one database per repository, shared across all of the repository's git worktrees. Repeat searches, and searches at a different `Threshold` or `Top`, make zero API calls. Editing a function re-asks only that function.

The default cache folder is `~/Library/Caches/jinter` on macOS (or `os.UserCacheDir()/jinter` on Linux and Windows). Set `$JINTER_CACHE_DIR` to move it, or provide a custom store via `Engine.OpenCache` implementing `cache.Cache`:

```go
type Cache interface {
	Get(keys []Key) (map[Key]float64, error)
	PutAnswers(answers []Answer) error // one Jev call's answers; duplicate keys carry the same value
	GetChunks(keys []Key) (map[Key][]byte, error)
	PutChunks(layouts map[Key][]byte) error
}
```

`PutAnswers` receives every answer from one Jev request as soon as the call returns, so an interrupted scan keeps every completed call. Cache failures are reported to `OnCacheError` and fall back to an in-memory cache without aborting the search.

## Testing without the API

`jevtest.Fake` answers `Ask` and `Decide` calls deterministically in memory so tests need no API key or network access:

```go
engine := &jinter.Engine{
	Client: &jevtest.Fake{Score: jevtest.Contains(map[string]string{
		"retries failed payments": "charge", // scores 0.9 where the chunk contains "charge"
	})},
	OpenCache: func(string, string) (cache.Cache, func() error, error) {
		return cache.NewMemory(), func() error { return nil }, nil
	},
}
```

## Packages

Each subpackage has a focused interface and can be used independently:

| Package | Responsibility |
|---|---|
| [`walk`](walk) | Repository discovery, `.gitignore` handling, and binary, generated, translation, test, and doc classification |
| [`chunk`](chunk) | Splitting files into function-sized chunks via `go/parser` and tree-sitter; `chunk.Parse` returns a file's full tree-sitter syntax tree and grammar |
| [`prefilter`](prefilter) | Local keyword extraction and regular-expression pre-filtering |
| [`jev`](jev) | Jev HTTP client with adaptive rate limiting and retries (`Ask` for Noul code search, `Decide` for mixed Noul, Choice, and Score questions); [`jevtest`](jev/jevtest) fakes both offline |
| [`cache`](cache) | Direct-driver SQLite and in-memory caches for Jev answers and file chunk layouts |
| [`search`](search) | Concurrent layout loading, request packing, cache lookup, Jev dispatch, and result ranking |

## Binary size

All 206 tree-sitter grammars are embedded by default (~33 MB binary). To embed only the languages your binary needs (~20 MB for common languages), build with `grammar_subset` and one tag per language:

```sh
go build -tags 'grammar_subset grammar_subset_go grammar_subset_python grammar_subset_typescript'
```

Files in other languages fall back to the indentation splitter automatically.
