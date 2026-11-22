---
name: verify-answer-bookkeeping
description: Check that search.Run still files every Jev answer under its own chunk and question, in the results and in the cache, by running every small search exhaustively. Use after any change to search/search.go (Run, its ask closure, packs, keyAll, parallel, rank), to cache.KeyOf or the cache.Cache interface, or to jev.Request/Response/Instruction, and before trusting a refactor of how chunks are packed into Jev calls or how answers are cached.
---

# Verify answer bookkeeping in `search.Run`

## What is checked, and why it matters

**The promise.** Whatever is already cached and however chunks are packed into Jev calls, every (chunk, question) pair ends up with the answer Jev gave for that chunk and that question, in the results and in the cache, and Jev is asked about exactly the pairs the cache did not have.

**The harm if it breaks.** An answer filed under the wrong chunk or question is saved in the user's cache, so every later search silently reports wrong matches until the row expires. Asking again about cached pairs spends the user's Jev quota for nothing.

`Run` keeps four indexes in step: the chunk, the question, the chunk's place in a call, and the question's place among the questions still missing for that chunk. A mistake only shows when the cache holds some answers and not others and several chunks share a call, which hand-written tests rarely set up.

## How it is checked

No verifier runs on this Go code (it uses generics, goroutines and the standard library throughout), so the check runs the real `Run` on **every** search inside a small scope and compares it with a pair-by-pair calculation that knows nothing about packing. It is one Go test:

```
go test ./search -run TestEveryAnswerLandsOnItsOwnChunk -count=1 -v
```

About 5 seconds, about 750,000 searches, no tools beyond the Go toolchain in `go.mod`. It also runs as part of `go test ./...`. Under `-race` it uses bounds one chunk smaller so the race run stays at a few seconds.

The code is in `search/exhaustive_test.go`; the bounds are in `search/exhaustive_bounds_test.go` and `search/exhaustive_bounds_race_test.go`.

### Scope: every search with

- up to 3 chunks and 1 or 2 questions, or 4 chunks and 1 question;
- the chunks split across files in every way that keeps a file's chunks together;
- each chunk's code one of four bodies: two small ones, one exactly half of `packBytes`, one bigger than `packBytes`. So packs fill exactly, overflow, and hold a chunk too big to share a call, and two chunks of a file with the same code share one cache key;
- every subset of the cache keys answered beforehand;
- `ReadCache` on and off, `Workers` 1 and 3;
- Jev answering everything, or failing every call that carries one chosen chunk.

### For each search it requires

1. Every value written to the cache is Jev's answer for exactly that key's file, code and question. This must hold even when the search fails part-way.
2. Each missing pair is written once, nothing already cached is written again, and no other key appears.
3. Jev is asked once about each missing pair and never about a cached one; no chunk is sent with no question; no call with more than one chunk exceeds `packBytes`.
4. If Jev fails a call the search needed, `Run` returns that error.
5. Otherwise each question's result lists every chunk with its own answer (the cached one when `ReadCache` is on and it was cached), best first, and `Best` is the first of them.

The fake Jev in the test knows only what the real one would: the call's path, each chunk's code, and its questions. Every (file, code, question) has its own answer value, and cached answers have values Jev never gives, so any mix-up changes a number.

### Not covered

- Goroutine schedules are not enumerated: each case runs once per worker count. Run with `-race` for data races.
- Searches bigger than the bounds. Nothing in `Run` depends on a count above 2, which is why the bounds were chosen, but that is an argument, not a check.
- Jev returning the wrong number of answers, cancellation, `Top` and `Threshold`, `OnAsk`/`OnCacheError`, duplicate questions. Ordinary tests in `search/search_test.go` cover these.
- That packing is as tight as it could be, only that it is never wrong or over the cap.
- Stores other than the recording one in the test. SQLite's own behaviour is tested in `cache/`.
- Whether cache keys stay the same between releases (`cache/key_test.go` pins those).

## Coverage record

| Check | Code it describes |
|---|---|
| 1, 2 (cache writes) | `search/search.go`: `Run`'s `ask` closure, `keyAll`, `stateEncoder.encode`; `cache/cache.go`: `KeyOf`, `Cache.PutAnswers` |
| 3 (what Jev is asked) | `search/search.go`: `Run`'s cache-read loop that builds `tasks`, `packs`, `packBytes`, the request built in `ask` |
| 4 (errors) | `search/search.go`: `parallel`, the error returns in `ask` |
| 5 (results) | `search/search.go`: `rank`, `topK`, the `scores` layout in `Run` |
| Expected keys | `jev/jev.go`: `State`, `Instruction`; `cache/cache.go`: `KeyOf` |

**Last confirmed in step with the code:** commit `9a54276` (2026-11-22). The covered files' contents at that commit, from `git hash-object <file>`:

| File | Blob |
|---|---|
| `search/search.go` | `a8b76acaa9f6f685e6fa29e9c912f8cbc1dbb292` |
| `cache/cache.go` | `3fa4f624bfb3647300d38aa787335e502939bd86` |
| `jev/jev.go` | `876b75fb5ac1ad19950d7c2cd8ebcabfedb77b41` |

If `git hash-object` gives a different value for a file, it changed after the check was last confirmed: read `git diff 9a54276 -- <file>` before trusting a pass, and update the commit and the table together.

Move this record forward only after you have confirmed, by reading the changed code against the five requirements and the scope, that the test still describes what `Run` does. A passing run alone is not that confirmation: a new code path the scope never reaches also passes.

## When you change the covered code

1. Run the command above.
2. **It passes.** Ask whether your change added a way for `Run` to behave that the scope cannot reach (a new option, a new packing rule, a new kind of cache write). If so, extend the scope first (see below), then run again. If not, update the coverage record.
3. **It fails.** The output is one search and what went wrong, for example:

   ```
   search: 1 question(s), ReadCache=true, Workers=1
     chunk 0: f0.go, code "small b", cached beforehand:
     chunk 1: f0.go, code "small a", cached beforehand:
     wrong:
       the cache got 0.011 for f0.go code "small a" question q0, but Jev's answer for it is 0.001: a later search would report another chunk's or question's answer
   ```

   Read it as a story: two chunks of `f0.go` shared a call, nothing was cached, and chunk 1's key received chunk 0's answer. At most three failing searches are printed; fix the first and rerun. A panic inside `Run` (an index out of range) is also a failure of the promise.

4. **Decide whether it is a real bug or the check being out of date.**
   - It is a real bug if the search described is one a user could run and the outcome described would hurt them. Fix the code, not the test.
   - The check is out of date only if you changed, on purpose, something it states as a rule: for instance the cache key recipe (`exKeys`), the pack cap or what may share a call (`exJev.Ask`), how often a pair may be written (requirement 2), or the result order (requirement 5). Then change that one rule in `search/exhaustive_test.go`, say so in your summary to the user, and prove the edited check can still fail (next section).
   - Never make it pass by shrinking the scope, loosening a comparison, or skipping a case.

## Changing or extending the check

- New `Options` field or packing rule: add a dimension to `exCase` and a loop for it in `TestEveryAnswerLandsOnItsOwnChunk`, and state the expected outcome in `exCase.check` without calling the code under test.
- Keep the run under about 10 seconds. The count multiplies fast: one more chunk at two questions is roughly 60 times more searches.
- After any edit to the check, prove it can still fail. Break `search/search.go` on purpose, run the command, see it fail, and restore the file (copy it aside first; do not use `git checkout` if there are uncommitted changes). These eight breaks were all caught when the check was written, and each should still be:

  | Break in `search/search.go` | What the check reports |
  |---|---|
  | `Key: keys[t.chunk][q]` → `keys[pack[0].chunk][q]` | the cache got another chunk's answer |
  | `Noul: resp.Nouls[i][j]` → `resp.Nouls[i][0]` | the cache got another question's answer |
  | `scores[t.chunk][q] =` → `scores[t.chunk][j] =` | a question reported the wrong probability |
  | drop the `Path == c.Path` test in `packs` | answers for the wrong file |
  | `size+len(c.Code) <= packBytes` → `true` | a call packed over the cap |
  | always add a question to `t.missing`, even on a cache hit | Jev asked about a cached pair |
  | send `questions[len(questions)-1-q]` | answers swapped between questions |
  | remove the final `save()` in `ask` | a pair was cached 0 times, want 1 |

Then update the coverage record.
