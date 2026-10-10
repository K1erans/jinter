# jinter

jrep's search engine as a Go library: it finds code by what it *does*,
using TypeSafe's Jev model.

```go
e := jinter.New(os.Getenv("TYPESAFE_API_KEY"), jev.HTTPOptions{})
rep, err := e.Search(ctx, jinter.Query{
	Questions: []string{"retries failed payments", "sends the receipt email"},
	Paths:     []string{"src/billing"},
	Threshold: 0.6,
	Top:       10,
})
for _, r := range rep.Results {
	for _, m := range r.Matches {
		fmt.Printf("%.2f  %s:%d-%d  %s\n", m.Probability, m.Path, m.Start, m.End, m.Signature)
	}
}
```

`Search` runs the whole pipeline: walk (following `.gitignore`, skipping
binaries, generated files, translations, and tests and docs unless asked),
chunk with tree-sitter, an optional keyword pre-pass, an optional `Refine`,
then cached Jev calls and ranking. The `Report` says what was skipped so
callers can tell the user.

## Packages

| Package | Does |
|---|---|
| `jinter` | `Engine.Search`: the pipeline end to end |
| `walk` | file listing, gitignore, binary/generated/test/doc detection |
| `chunk` | splitting files into function-sized chunks |
| `prefilter` | the keyword pre-pass |
| `jev` | Jev API client, adaptive rate limiter, retries; `jevtest.Fake` for offline tests |
| `cache` | SQLite and in-memory caches of answers and chunk layouts |
| `search` | `Load` (chunk with cached layouts), `Run` (key → cache → Jev → rank), `Narrow` |

## Notes

- The on-disk cache is jrep's (`~/Library/Caches/jrep`, or `$JREP_CACHE_DIR`).
  The keys match, so answers paid for by either serve both.
- `Query.MaxFileSize` of 0 means the 1 MB default; use a negative value for
  no limit.
- By default every tree-sitter grammar is embedded (~33 MB). To embed only
  common ones, build with jrep's tags, e.g.
  `-tags 'grammar_subset grammar_subset_go grammar_subset_python …'` (see
  jrep's Makefile).
- Every chunk searched is sent to TypeSafe.
