package search

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/chunk"
	"github.com/K1erans/jinter/jev"
	"github.com/K1erans/jinter/jev/jevtest"
	"github.com/K1erans/jinter/walk"
)

// benchChunks makes n chunks of about 1.5 KB, ten to a file.
func benchChunks(n int) []Chunk {
	r := rand.New(rand.NewPCG(1, 2))
	out := make([]Chunk, n)
	for i := range out {
		path := fmt.Sprintf("src/pkg%03d/file%04d.go", i/1000, i/10)
		var b strings.Builder
		for b.Len() < 1500 {
			fmt.Fprintf(&b, "\tx%d := call%d(y, %q)\n", r.IntN(1000), r.IntN(1000), "s")
		}
		start := (i%10)*40 + 1
		out[i] = Chunk{Path: path, Rel: path, Chunk: chunk.Chunk{Start: start, End: start + 39, Signature: "func f()", Code: b.String()}}
	}
	return out
}

// benchScores gives every chunk a pseudo-random score for one question.
func benchScores(n int) [][]float64 {
	r := rand.New(rand.NewPCG(3, 4))
	s := make([][]float64, n)
	for i := range s {
		s[i] = []float64{r.Float64()}
	}
	return s
}

func BenchmarkRank(b *testing.B) {
	const n = 50_000
	cs, s := benchChunks(n), benchScores(n)
	for _, bc := range []struct {
		name      string
		threshold float64
		top       int
	}{{"all-top10", 0, 10}, {"all-top0", 0, 0}, {"threshold0.99", 0.99, 0}} {
		b.Run(bc.name, func(b *testing.B) {
			o := Options{Threshold: bc.threshold, Top: bc.top}
			b.ReportAllocs()
			for b.Loop() {
				rank("q", cs, s, 0, o)
			}
		})
	}
}

// cachedStore holds an answer for every chunk and question.
func cachedStore(b *testing.B, cs []Chunk, questions ...string) cache.Cache {
	b.Helper()
	store := cache.NewMemory()
	r := rand.New(rand.NewPCG(5, 6))
	for _, q := range questions {
		keys, err := keyAll(context.Background(), cs, "m", []string{jev.Instruction(q)})
		if err != nil {
			b.Fatal(err)
		}
		answers := make([]cache.Answer, len(keys))
		for i, k := range keys {
			answers[i] = cache.Answer{Key: k, Noul: r.Float64()}
		}
		if err := store.PutAnswers(answers); err != nil {
			b.Fatal(err)
		}
	}
	return store
}

func BenchmarkNarrow(b *testing.B) {
	cs := benchChunks(50_000)
	store := cachedStore(b, cs, "q")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Narrow(cs, store, "m", "q", 300); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkKeyAll(b *testing.B) {
	cs := benchChunks(20_000)
	ins := []string{jev.Instruction("a"), jev.Instruction("b"), jev.Instruction("c")}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := keyAll(context.Background(), cs, "m", ins); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRunCached is a repeat search: every answer is cached.
func BenchmarkRunCached(b *testing.B) {
	cs := benchChunks(20_000)
	qs := []string{"a", "b", "c"}
	store := cachedStore(b, cs, qs...)
	o := Options{Questions: qs, Model: "m", Top: 10, ReadCache: true}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Run(context.Background(), cs, &jevtest.Fake{}, store, o); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRunFresh is a first search of 1024 chunks, 8 to a file (one
// Jev call each), asked 3 questions, with the SQLite cache: every answer
// is new and saved. Jev answers at once, so this is the search's own cost,
// most of it saving answers. It reports the cost per answer.
func BenchmarkRunFresh(b *testing.B) {
	const files, perFile = 128, 8
	cs := make([]Chunk, 0, files*perFile)
	for f := range files {
		path := fmt.Sprintf("src/file%03d.go", f)
		for c := range perFile {
			start := c*40 + 1
			code := fmt.Sprintf("func f%d() {\n%s}\n", c, strings.Repeat(fmt.Sprintf("\tcall(%d, %d)\n", f, c), 60))
			cs = append(cs, Chunk{Path: path, Rel: path, Chunk: chunk.Chunk{Start: start, End: start + 39, Signature: "func f()", Code: code}})
		}
	}
	qs := []string{"a", "b", "c"}
	store, err := cache.Open(filepath.Join(b.TempDir(), "repo.db"), "m")
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	score := func(jev.State, string) float64 { return 0.5 }
	run := 0
	b.ReportAllocs()
	for b.Loop() {
		run++ // a new model each time, so no answer is in the cache yet
		o := Options{Questions: qs, Model: fmt.Sprintf("m%d", run), Top: 10}
		fake := &jevtest.Fake{Score: score}
		if _, err := Run(context.Background(), cs, fake, store, o); err != nil {
			b.Fatal(err)
		}
		if n := len(fake.Requests()); n != files {
			b.Fatalf("made %d calls, want %d", n, files)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(cs)*len(qs)), "ns/answer")
}

// benchFiles writes Go files of mixed sizes, with one big file last, as in
// a repo whose slowest file sorts late.
func benchFiles(b *testing.B) []walk.File {
	b.Helper()
	dir := b.TempDir()
	var out []walk.File
	add := func(name string, funcs int) {
		var s strings.Builder
		s.WriteString("package p\n\n")
		for i := range funcs {
			fmt.Fprintf(&s, "// F%d does a thing.\nfunc F%d(a, b int) (int, error) {\n\tif a > b {\n\t\treturn a - b, nil\n\t}\n\tfor i := 0; i < a; i++ {\n\t\tb += i * %d\n\t}\n\treturn b, nil\n}\n\n", i, i, i)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s.String()), 0o644); err != nil {
			b.Fatal(err)
		}
		out = append(out, walk.File{Path: p, Rel: name})
	}
	for i := range 400 {
		add(fmt.Sprintf("a%03d.go", i), 20+i%60)
	}
	add("zz_big.go", 6000)
	return out
}

func BenchmarkLoad(b *testing.B) {
	fs := benchFiles(b)
	b.Run("cold", func(b *testing.B) {
		for b.Loop() {
			if _, err := Load(fs, LoadOptions{Chunk: chunk.Default}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		o := LoadOptions{Chunk: chunk.Default, Store: cache.NewMemory(), ReadCache: true}
		if _, err := Load(fs, o); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			if _, err := Load(fs, o); err != nil {
				b.Fatal(err)
			}
		}
	})
}
