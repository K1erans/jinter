# jinter

[![Go Reference](https://pkg.go.dev/badge/github.com/K1erans/jinter.svg)](https://pkg.go.dev/github.com/K1erans/jinter)

Find code by what it *does*, from Go, using TypeSafe's Jev model.

Ask a question in plain words, such as "retries failed payments", and get
back the functions that do it, ranked by probability. No names or keywords
needed.

```
0.94  src/billing/retry.go:12-80   func RetryFailedPayment(invoice Invoice, attempt int) error
0.81  src/jobs/payment.go:40-95    func handlePaymentFailure(job Job) error
```

## Install

```
go get github.com/K1erans/jinter
```

You need a TypeSafe API key for any answer that isn't cached yet.

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

Make one `Engine` and reuse it: its searches share one rate limiter.

## How a search works

1. **List files.** Follows `.gitignore` and skips binaries, generated files
   (lockfiles, source maps, minified files, SVGs, `node_modules`) and
   translation catalogs. Tests and docs are skipped unless asked for, or
   unless a question is about tests.
2. **Split** each file into function- or block-sized chunks with
   tree-sitter (206 grammars). Files with no grammar are split at
   unindented lines.
3. **Narrow**, optionally, with a keyword pre-pass (`Prepass`) or to the
   chunks that best matched an earlier question (`Refine`).
4. **Ask Jev** about every chunk not already cached. Every question about
   a file shares its calls, so extra questions cost little more than one.
5. **Rank** the chunks for each question, best first.

> [!NOTE]
> Every chunk searched is sent to TypeSafe. Cost grows with the amount of
> code searched, so pass the narrowest `Paths` you can.

## Query

| Field | Does | Default |
|---|---|---|
| `Questions` | What the code does: "sends the password reset email" | required |
| `Paths` | Files and folders to search | `.` |
| `Threshold` | Minimum probability to count as a match | `0` |
| `Top` | Maximum matches per question | no limit |
| `Prepass`, `Keywords` | Send Jev only the chunks matching a regexp. Without `Keywords`, uses the questions' words | off |
| `Refine`, `RefineTop` | Search only the chunks that best matched this earlier question, from its cached answers | off, 300 |
| `Tests`, `Docs`, `NoIgnore` | Also search tests, docs, gitignored files | off |
| `MaxFileSize` | Skip larger files; negative means no limit | 1 MB |
| `Model` | Jev version | `jev-1.13.0` |
| `NoCache` | Don't read cached answers (new answers are still saved) | off |
| `OnFiles` | Hears which files were skipped, before anything is sent to Jev | |
| `OnAsk` | Hears how many chunks are about to be sent to Jev | |
| `OnCacheError` | Hears about cache failures, which never stop a search | |

## Report

| Field | Holds |
|---|---|
| `Results` | One per question: `Matches` best first, and `Best` even when nothing passed the threshold |
| `Matched()` | Whether any question matched |
| `Chunks`, `Split` | Chunks searched, and chunks before `Prepass` and `Refine` |
| `Keywords` | The pattern the pre-pass used |
| `Oversize`, `Tests`, `Docs`, `Generated`, `Translations` | What was skipped, so you can tell the user. Nothing is skipped silently |
| `TestsIncluded` | Tests were searched because a question was about them |

## Caching

Answers and file layouts are cached in SQLite, one database per
repository, shared by its worktrees. Repeat searches, and searches at a
different `Threshold` or `Top`, make no new calls. Editing a function
re-asks only that function.

The default folder is `~/Library/Caches/jinter` on macOS (the user cache
folder elsewhere); set `$JINTER_CACHE_DIR` to move it. To keep the cache
somewhere else, or in memory, set `Engine.OpenCache`.

## Testing without the API

`jevtest.Fake` answers offline, so tests need no key or network:

```go
engine := &jinter.Engine{
	Client: &jevtest.Fake{Score: jevtest.Contains(map[string]string{
		"retries failed payments": "charge", // scores 0.9 where the code contains "charge"
	})},
	OpenCache: func(string, string) (cache.Cache, func() error, error) {
		return cache.NewMemory(), func() error { return nil }, nil
	},
}
```

## Packages

`jinter` runs the whole search. The packages under it can also be used on
their own:

| Package | Does |
|---|---|
| [`walk`](walk) | File listing, `.gitignore`, binary, generated, test and doc detection |
| [`chunk`](chunk) | Splitting files into function-sized chunks |
| [`prefilter`](prefilter) | The keyword pre-pass |
| [`jev`](jev) | Jev API client with an adaptive rate limiter and retries; `jevtest` fakes it |
| [`cache`](cache) | SQLite and in-memory caches of answers and chunk layouts |
| [`search`](search) | Chunking with cached layouts, then key → cache → Jev → rank |

## Binary size

Every tree-sitter grammar is embedded by default, about 33 MB. To embed
only the languages you need (about 20 MB for common ones), build with one
tag per language:

```
go build -tags 'grammar_subset grammar_subset_go grammar_subset_python grammar_subset_typescript'
```

Files in other languages fall back to the plain splitter.
