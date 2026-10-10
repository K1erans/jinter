# jinter

A Go library that finds code by what it *does*, using TypeSafe's Jev model.

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

- Answers and chunk layouts are cached on disk, one SQLite database per
  repository (shared by its worktrees), in `~/Library/Caches/jinter` on
  macOS. `$JINTER_CACHE_DIR` overrides the folder.
- `Query.MaxFileSize` of 0 means the 1 MB default; use a negative value for
  no limit.
- By default every tree-sitter grammar is embedded (~33 MB). To embed only
  common ones (~20 MB), build with
  `-tags 'grammar_subset grammar_subset_go grammar_subset_python …'`, one
  `grammar_subset_<language>` tag per language to keep.
- Every chunk searched is sent to TypeSafe.
