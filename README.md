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

### Showing matches as they arrive

A search returns once every chunk is answered. To show matches while it is
still running, set `OnMatch`:

```go
rep, err := engine.Search(ctx, jinter.Query{
	Questions: []string{"retries failed payments"},
	Threshold: 0.6,
	OnMatch: func(question string, m search.Match) {
		fmt.Printf("found %.2f  %s:%d-%d\n", m.Probability, m.Path, m.Start, m.End)
	},
})
```

Cached answers are heard first, then each Jev call's as it returns. Chunks
that share a word with the questions are asked about first, so the likely
matches tend to come early. The matches arrive unranked and `Top` doesn't
limit them; `rep` still has every match, best first.

## How a search works

1. **List files.** Follows `.gitignore` and skips binaries, generated files
   (lockfiles, source maps, minified files, SVGs, `node_modules`) and
   translation catalogs. Tests and docs are skipped unless asked for, or
   unless a question is about tests. A file or folder that can't be read
   is reported in `Report.Unreadable` and the search carries on; a path
   you name in `Paths` that is missing or can't be read is an error.
2. **Split** each file into function- or block-sized chunks with
   tree-sitter (206 grammars). Go files are read with the standard
   library's `go/parser` instead, which is many times faster and finds the
   same chunks; tree-sitter only opens up declarations too big for one
   chunk (unless Go is routed to tsasm; see [Parser backends](#parser-backends)).
   Files with no grammar, or that fail to parse, are split at
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
| `OnFiles` | Hears which files were skipped or unreadable, before anything is sent to Jev | |
| `OnAsk` | Hears how many chunks are about to be sent to Jev | |
| `OnMatch` | Hears each match as soon as its answer is known, cached ones first, so results can be shown while the search runs. Chunks sharing words with the questions are asked about first. Unranked and not limited by `Top`; the `Report` is still complete and ranked | |
| `OnCacheError` | Hears about cache failures, which never stop a search | |

## Report

| Field | Holds |
|---|---|
| `Results` | One per question: `Matches` best first, and `Best` even when nothing passed the threshold |
| `Matched()` | Whether any question matched |
| `Chunks`, `Split` | Chunks searched, and chunks before `Prepass` and `Refine` |
| `Keywords` | The pattern the pre-pass used |
| `Oversize`, `Tests`, `Docs`, `Generated`, `Translations` | What was skipped, so you can tell the user. Nothing is skipped silently |
| `Unreadable` | Files and folders that couldn't be read (permission denied, for example), each with its `Path`, `Rel` and `Err`. The rest are still searched |
| `TestsIncluded` | Tests were searched because a question was about them |

## Caching

Answers and file layouts are cached in SQLite, one database per
repository, shared by its worktrees. Repeat searches, and searches at a
different `Threshold` or `Top`, make no new calls. Editing a function
re-asks only that function.

The default folder is `~/Library/Caches/jinter` on macOS (the user cache
folder elsewhere); set `$JINTER_CACHE_DIR` to move it. To keep the cache
somewhere else, or in memory, set `Engine.OpenCache`.

A store of your own implements `cache.Cache`:

```go
type Cache interface {
	Get(keys []Key) (map[Key]float64, error)
	PutAnswers(answers []Answer) error // one call's answers; a key may repeat, with the same value
	GetChunks(keys []Key) (map[Key][]byte, error)
	PutChunks(layouts map[Key][]byte) error
}
```

`PutAnswers` gets every answer from one Jev call as soon as the call
returns, so a store can save them in one write and an interrupted search
keeps its progress. Keep the answer already stored for a key: the key pins
the model, question and code. A failed write is passed to `OnCacheError`
and doesn't stop the search.

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
| [`parse`](parse) | Syntax trees and highlight captures behind one interface, with the parser chosen per language: gotreesitter by default, C tree-sitter with `-tags jinter_cgo`, tsasm with `-tags jinter_tsasm` |
| [`prefilter`](prefilter) | The keyword pre-pass |
| [`jev`](jev) | Jev API client with an adaptive rate limiter and retries: `Ask` for search's Noul questions, `Decide` for any mix of Noul, Choice and Score questions about one state; `jevtest` fakes both |
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

## Parser backends

Files are parsed with [gotreesitter](https://github.com/odvcencio/gotreesitter),
in pure Go. Built with `-tags jinter_cgo` (cgo and a C compiler needed),
jinter also has the C tree-sitter runtime with the official grammars for
JavaScript, TypeScript, TSX, Go, Python, Rust, C, C++, Java, Ruby, JSON
and HTML. Route languages to it with `$JINTER_PARSER` or
`chunk.Options.Parsers`, e.g. `JINTER_PARSER='typescript=c,tsx=c'`, or
`'*=c'` for every language it has; the rest stay on gotreesitter. A
non-default routing gets its own cached chunk layouts.

With `-tags jinter_tsasm` on linux/amd64, jinter also has
[tsasm](https://github.com/K1erans/tsasm), a tree-sitter parser runtime
in x86-64 assembly, for Go, C, Java and JSON.
It reads the same official grammars as the C backend, so where a file
parses, the trees are the same. It has no error recovery: a file with
syntax errors gets no tree, and jinter splits it as plain text, as it
does a tree with errors. Its trees have no grammar fields or highlight
captures, which chunking doesn't use. Like the C backend it needs cgo and
a C compiler, which assembles it; then route to it:

```
go build -tags jinter_tsasm .
JINTER_PARSER='*=tsasm' ./yourprogram
```

With Go routed to tsasm, Go files skip go/parser and are parsed by tsasm
alone, which is faster still (20-35% on Go's own source with 20 workers).
The catch is the one go/parser was added for: the rare valid Go file that
tree-sitter rejects is split as plain text. On Go's source, 17 of 7,710
files split differently from the go/parser route, most of them
type-checker test data.

Both tags can be used together, e.g. `'*=c,go=tsasm,c=tsasm,java=tsasm,json=tsasm'`.

`go run -tags jinter_cgo ./internal/parsebench <paths>` compares the
backends on your code: speed, failures, and whether their trees agree.
