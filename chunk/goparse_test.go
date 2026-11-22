package chunk

import (
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// diffOptions are the option sets the go/parser path is checked under: the
// defaults, small limits that open up most declarations, a byte limit that
// cuts long lines, limits nothing reaches, and a line limit everything
// reaches, the package clause included. None has a timeout, which would
// make the tree-sitter side depend on how busy the machine is.
var diffOptions = []struct {
	name string
	opts Options
}{
	{"default", Options{MinLines: Default.MinLines, MaxLines: Default.MaxLines, MaxBytes: Default.MaxBytes}},
	{"small", Options{MinLines: 3, MaxLines: 12, MaxBytes: 600}},
	{"short-lines", Options{MinLines: 1, MaxLines: 300, MaxBytes: 100}},
	{"unlimited", Options{MinLines: 0, MaxLines: 1 << 30, MaxBytes: 1 << 30}},
	{"all-big", Options{MinLines: 2, MaxLines: 0, MaxBytes: 1 << 20}},
}

// diffResult compares the go/parser path with the tree-sitter path on one
// file. Only a file tree-sitter rejects may split differently.
type diffResult struct {
	same bool
	tree route // how the tree-sitter path split it
	fast route // how the go/parser path split it
	want []Chunk
	got  []Chunk
}

func (d diffResult) bug() bool { return !d.same && d.tree != routePlain }

func compareRoutes(name string, src []byte, opts Options) diffResult {
	want, tree := split(name, src, opts, false)
	got, fast := split(name, src, opts, true)
	return diffResult{same: slices.Equal(got, want), tree: tree, fast: fast, want: want, got: got}
}

// describe shows the first chunk where d's two splits part.
func (d diffResult) describe() string {
	for i := 0; i < max(len(d.want), len(d.got)); i++ {
		var w, g Chunk
		if i < len(d.want) {
			w = d.want[i]
		}
		if i < len(d.got) {
			g = d.got[i]
		}
		if w != g {
			return fmt.Sprintf("chunk %d: tree-sitter %d-%d %q (%d bytes at %d), go/parser %d-%d %q (%d bytes at %d)",
				i, w.Start, w.End, w.Signature, len(w.Code), w.Offset, g.Start, g.End, g.Signature, len(g.Code), g.Offset)
		}
	}
	return "no difference"
}

// TestGoParserMatchesTreeSitterOnGOROOT splits Go files from GOROOT/src
// both ways under every option set and requires identical chunks unless
// tree-sitter rejected the file. By default it checks a sample; set
// JINTER_CHUNK_DIFF=goroot for every file, or =all to add the module cache.
func TestGoParserMatchesTreeSitterOnGOROOT(t *testing.T) {
	roots := []string{filepath.Join(build.Default.GOROOT, "src")}
	mode := os.Getenv("JINTER_CHUNK_DIFF")
	if mode == "all" {
		roots = append(roots, filepath.Join(build.Default.GOPATH, "pkg", "mod"))
	}
	var paths []string
	for _, root := range roots {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() && strings.HasPrefix(d.Name(), "toolchain@") {
				return filepath.SkipDir // another GOROOT
			}
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".go") {
				paths = append(paths, path)
			}
			return nil
		})
	}
	if len(paths) == 0 {
		t.Skip("no Go sources under", roots)
	}
	if mode == "" {
		// A file of at most 32 KB from every 400: about twenty files from
		// all over the tree, quick enough under -race.
		var sample []string
		for i := 0; i < len(paths); i += 400 {
			for j := i; j < len(paths) && j < i+400; j++ {
				if fi, err := os.Stat(paths[j]); err == nil && fi.Size() <= 32<<10 {
					sample = append(sample, paths[j])
					break
				}
			}
		}
		paths = sample
	}

	began := time.Now()
	for _, o := range diffOptions {
		// Split every file both ways in parallel, then once more alone
		// those that split differently or that tree-sitter rejected:
		// gotreesitter stops parsing a file over 64 KB when the process
		// heap grows too much, other parses included, so a big file can
		// fail only under load.
		results := make([]diffResult, len(paths))
		sizes := make([]int, len(paths))
		var next atomic.Int64
		var wg sync.WaitGroup
		for range runtime.GOMAXPROCS(0) {
			wg.Go(func() {
				for i := int(next.Add(1)) - 1; i < len(paths); i = int(next.Add(1)) - 1 {
					if src, err := os.ReadFile(paths[i]); err == nil {
						sizes[i] = len(src)
						results[i] = compareRoutes(paths[i], src, o.opts)
						results[i].want, results[i].got = nil, nil
					}
				}
			})
		}
		wg.Wait()
		var compared, bytes, same, treeRejected, treeRejectedSame, bugs, alone, changed int
		var goRejected, partial, partialFailed int
		for i, path := range paths {
			src, err := os.ReadFile(path)
			if err != nil || len(src) != sizes[i] {
				continue
			}
			d := results[i]
			if !d.same || d.tree == routePlain {
				before := d
				d = compareRoutes(path, src, o.opts)
				alone++
				if d.same != before.same || d.tree != before.tree || d.fast != before.fast {
					changed++
				}
			}
			compared++
			bytes += len(src)
			switch {
			case d.same:
				same++
				if d.tree == routePlain {
					treeRejectedSame++
				}
			case d.tree == routePlain:
				if treeRejected++; treeRejected <= 3 {
					t.Logf("%s: %s: tree-sitter rejects it; %s", o.name, path, d.describe())
				}
			default:
				if bugs++; bugs <= 10 {
					t.Errorf("%s: %s: %s", o.name, path, d.describe())
				}
			}
			switch _, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution); {
			case err != nil:
				goRejected++
			case d.fast == routeGoTree:
				partial++
			case d.fast != routeGo:
				partialFailed++
			}
		}
		t.Logf("%-11s %d files (%.1f MB): %d identical (%d of them rejected by tree-sitter), "+
			"%d differ where tree-sitter rejected the file, %d differ otherwise; %d split again alone, %d of them differently. "+
			"go/parser rejected %d; of the rest, %d needed tree-sitter for big declarations and %d of those were parsed whole after all",
			o.name, compared, float64(bytes)/1e6, same, treeRejectedSame, treeRejected, bugs, alone, changed,
			goRejected, partial+partialFailed, partialFailed)
	}
	t.Logf("%d option sets over %d files in %v", len(diffOptions), len(paths), time.Since(began).Round(time.Second))
}

// goEdgeCases are small Go files at the edges of what go/parser and
// tree-sitter accept, seeding the fuzz test and checked by the generated
// files test.
var goEdgeCases = []string{
	"",
	"package p",
	"package p\n",
	"package p // trailing",
	"\ufeffpackage p\n\nfunc f() {}\n",
	"package\nmain\n\nfunc f() {}\n",
	"package /* inside */ p\n",
	"//go:build linux && !cgo\n\n// Package p is built on Linux.\npackage p\n",
	"package p\n\n/*\n#include <stdio.h>\nint add(int a, int b) { return a + b; }\n*/\nimport \"C\"\n\nfunc f() { C.add(1, 2) }\n",
	"package p\r\n\r\n// F does a thing.\r\nfunc F() {\r\n\treturn\r\n}\r\n",
	"package p\n\nvar s = `a\r\nb\r\nc`\n\n/* a\r\nb\r\nc */\n",
	"package p\n\n//line other.go:100\nfunc f() {\n}\n\n//line :5\nfunc g() {}\n",
	"package p\n\nfunc f() {} // trailing\n/* after */ func g() {}\n",
	"package p; func f() {}; var x = 1; type T int",
	"package p\n\nimport (\n\t\"fmt\" // fmt\n\n\t// os\n\t\"os\"\n)\n\nconst (\n\tA = iota // a\n\tB\n)\n\nvar (\n\tx, y int\n)\n\ntype (\n\tT struct{ a int }\n\tU = T\n)\n",
	"package p\n\ntype List[T any] struct{ next *List[T] }\n\nfunc (l *List[T]) Push(v T) {}\n\nfunc Map[T, U any](s []T, f func(T) U) []U { return nil }\n",
	"package p\n\nfunc f()\n\nfunc g() {\n\tx := 1\n\t_ = x\n}\n// last\n// words",
	"package p\n\nvar x = \"\xff\"\n",
	"package p\n\n// \xff\nfunc f() {}\n",
	"package p\n\nx := 1\n",
	"package p\n\nvar ()\nconst ()\n",
	"package p\n\nfunc f() {\n\tswitch {\n\tcase true:\n\t\tgoto L\n\t}\nL:\n\tfor range 3 {\n\t}\n}\n",
}

func FuzzGoParserMatchesTreeSitter(f *testing.F) {
	for _, src := range goEdgeCases {
		f.Add([]byte(src))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		for _, o := range diffOptions {
			if d := compareRoutes("fuzz.go", src, o.opts); d.bug() {
				t.Fatalf("%s: %s\n%q", o.name, d.describe(), src)
			}
		}
	})
}

// goFragments are top-level pieces genGo strings together.
var goFragments = []string{
	"// F%[1]d does a thing.\n// It has two lines.\nfunc F%[1]d(a int) int {\n\treturn a\n}\n",
	"func (r *R%[1]d) M%[1]d() {\n\t// inside\n\tr.n++\n}\n",
	"func G%[1]d[T comparable](a, b T) bool { return a == b }\n",
	"type T%[1]d struct {\n\tA int // a\n\tB string\n}\n",
	"type (\n\tU%[1]d int\n\t// V is a V.\n\tV%[1]d = U%[1]d\n)\n",
	"const C%[1]d = %[1]d\n",
	"const (\n\tK%[1]d = iota\n\tL%[1]d\n)\n",
	"var v%[1]d, w%[1]d = %[1]d, \"%[1]d\" // trailing\n",
	"var (\n\tx%[1]d int\n\n\ty%[1]d = []int{\n\t\t1, // one\n\t\t2,\n\t}\n)\n",
	"/* block comment %[1]d\n   over lines */\n",
	"// loose comment %[1]d\n",
	"var a%[1]d = 1; var b%[1]d = 2\n",
	"func H%[1]d() {} // trailing\n",
	"var r%[1]d = `raw\nstring`\n",
	"//line gen.go:%[1]d\nfunc J%[1]d() {}\n",
	"func N%[1]d()\n",
	"func S%[1]d(x int) {\n\tswitch x {\n\tcase 1:\n\t}\n}\n",
}

// genGo builds a random Go file of fragments, sometimes with a long
// function, imports, CRLF line endings, a BOM or no final newline.
func genGo(r *rand.Rand) string {
	var b strings.Builder
	if r.IntN(4) == 0 {
		b.WriteString("// Copyright notice.\n\n//go:build !windows\n\n")
	}
	b.WriteString("package gen\n")
	switch r.IntN(3) {
	case 1:
		b.WriteString("\nimport \"fmt\"\n")
	case 2:
		b.WriteString("\nimport (\n\t\"fmt\"\n\t// os is for files.\n\tos \"os\"\n)\n")
	}
	for i := range r.IntN(12) {
		b.WriteString(strings.Repeat("\n", r.IntN(3)))
		if r.IntN(6) == 0 {
			fmt.Fprintf(&b, "func Long%d() {\n", i)
			for j := range 5 + r.IntN(40) {
				fmt.Fprintf(&b, "\tx%d := %d\n", j, j)
				switch r.IntN(12) {
				case 0:
					fmt.Fprintf(&b, "\tif x%d > 0 {\n\t\tprintln(x%d)\n\t}\n", j, j)
				case 1:
					fmt.Fprintf(&b, "\tswitch x%d {\n\tcase 1:\n\t\tprintln()\n\n\tdefault:\n\t}\n", j)
				case 2:
					b.WriteString("\t// a comment in the body\n")
				case 3:
					b.WriteString("\n")
				}
			}
			// Where a statement list ends depends on what else is in the
			// file when the body ends with a blank line.
			b.WriteString(strings.Repeat("\n", r.IntN(2)) + "}\n")
			continue
		}
		fmt.Fprintf(&b, goFragments[r.IntN(len(goFragments))], i)
	}
	src := b.String()
	if r.IntN(5) == 0 {
		src = strings.ReplaceAll(src, "\n", "\r\n")
	}
	if r.IntN(8) == 0 {
		src = "\ufeff" + src
	}
	if r.IntN(4) == 0 {
		src = strings.TrimRight(src, "\r\n")
	}
	return src
}

// TestGoParserMatchesTreeSitterOnGeneratedFiles checks the edge cases and
// files from genGo: 120 of them, or 5000 with JINTER_CHUNK_DIFF set.
func TestGoParserMatchesTreeSitterOnGeneratedFiles(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	srcs := slices.Clone(goEdgeCases)
	n := 120
	if os.Getenv("JINTER_CHUNK_DIFF") != "" {
		n = 5000
	}
	for range n {
		srcs = append(srcs, genGo(r))
	}
	routes := map[route]int{}
	for _, src := range srcs {
		for _, o := range diffOptions {
			d := compareRoutes("gen.go", []byte(src), o.opts)
			if d.bug() {
				t.Fatalf("%s: %s\n%s", o.name, d.describe(), src)
			}
			routes[d.fast]++
		}
	}
	// The generator is meant to exercise the go/parser path, both alone
	// and with tree-sitter opening up big declarations.
	if routes[routeGo] == 0 || routes[routeGoTree] == 0 {
		t.Errorf("routes taken: %v", routes)
	}
}

func TestGoFilesTakeTheGoParserPath(t *testing.T) {
	long := "package p\n\nfunc Long() {\n" + strings.Repeat("\tx := 1\n\t_ = x\n", 200) + "}\n"
	for _, c := range []struct {
		name, file, src string
		want            route
	}{
		{"small declarations", "a.go", goSrc, routeGo},
		{"a declaration over MaxLines", "a.go", long, routeGoTree},
		{"not Go to go/parser", "a.go", "package p\n\nx := 1\n", routeTree},
		{"not Go to either parser", "a.go", "package p\n\nfunc {{{\n", routePlain},
		{"not a Go file name", "a.py", "def f():\n    return 1\n", routeTree},
		{"invalid UTF-8", "a.go", "package p\n\n// \xff\nfunc f() {}\n", routeTree},
	} {
		if _, got := split(c.file, []byte(c.src), Default, true); got != c.want {
			t.Errorf("%s: route %d, want %d", c.name, got, c.want)
		}
	}
}

func TestBigGoDeclarationsOpenUpAsTreeSitterWould(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\n\n// Huge is long.\nfunc Huge() {\n")
	for i := range 40 {
		fmt.Fprintf(&b, "\tx%d := %d\n", i, i)
	}
	b.WriteString("}\n\nfunc Small() {}\n")
	src := b.String()
	opts := Options{MinLines: 1, MaxLines: 15, MaxBytes: 1 << 20}
	got, via := split("huge.go", []byte(src), opts, true)
	if via != routeGoTree {
		t.Fatalf("route %d, want go/parser with tree-sitter", via)
	}
	want, _ := split("huge.go", []byte(src), opts, false)
	if !slices.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", spans(got), spans(want))
	}
	assertCovers(t, src, got, opts)
}

func TestTreeSitterRejectingValidGoStillGetsRealChunks(t *testing.T) {
	// Two labels on one line are valid Go; tree-sitter's grammar wants a
	// statement after each.
	src := "package p\n\nimport \"fmt\"\n\n// F prints.\nfunc F() {\n\tfmt.Println()\n\tgoto L\nL: M:\n}\n"
	opts := Options{MinLines: 1, MaxLines: 300, MaxBytes: 1 << 20}
	if _, via := split("labels.go", []byte(src), opts, false); via != routePlain {
		t.Fatalf("tree-sitter route %d: the test needs a file tree-sitter rejects", via)
	}
	got, via := split("labels.go", []byte(src), opts, true)
	if via != routeGo {
		t.Fatalf("route %d, want go/parser", via)
	}
	assertSpans(t, got, []span{{1, 10, "func F()"}})
}

// goFiles reads every nth .go file under root, biggest first as
// search.Load splits them, so no big file starts last.
func goFiles(b *testing.B, root string, nth int) [][]byte {
	var srcs [][]byte
	i := 0
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if i++; (i-1)%nth == 0 {
			if src, err := os.ReadFile(path); err == nil {
				srcs = append(srcs, src)
			}
		}
		return nil
	})
	if len(srcs) == 0 {
		b.Skip("no Go files under", root)
	}
	slices.SortStableFunc(srcs, func(a, b []byte) int { return len(b) - len(a) })
	return srcs
}

// BenchmarkSplitCorpus splits real Go code both ways, on one goroutine and
// on one per CPU: this module, and every tenth file of GOROOT/src, or every
// file with JINTER_CHUNK_BENCH=goroot.
func BenchmarkSplitCorpus(b *testing.B) {
	goroot := struct {
		name, root string
		nth        int
	}{"goroot-1in10", filepath.Join(build.Default.GOROOT, "src"), 10}
	if os.Getenv("JINTER_CHUNK_BENCH") == "goroot" {
		goroot.name, goroot.nth = "goroot", 1
	}
	for _, c := range []struct {
		name, root string
		nth        int
	}{{"jinter", "..", 1}, goroot} {
		srcs := goFiles(b, c.root, c.nth)
		size := 0
		for _, src := range srcs {
			size += len(src)
		}
		for _, p := range []struct {
			name     string
			goParser bool
		}{{"tree-sitter", false}, {"go-parser", true}} {
			for _, workers := range []int{1, runtime.GOMAXPROCS(0)} {
				b.Run(fmt.Sprintf("%s/%s/workers=%d", c.name, p.name, workers), func(b *testing.B) {
					b.SetBytes(int64(size))
					for b.Loop() {
						var next atomic.Int64
						var wg sync.WaitGroup
						for range workers {
							wg.Go(func() {
								for i := int(next.Add(1)) - 1; i < len(srcs); i = int(next.Add(1)) - 1 {
									split("corpus.go", srcs[i], Default, p.goParser)
								}
							})
						}
						wg.Wait()
					}
				})
			}
		}
	}
}

// TestGoParserMatchesTreeSitterAtEveryLineLimit sweeps MaxLines over files
// whose big function ends in a blank line, where tree-sitter's statement
// list ends depend on words elsewhere in the file (see boundaryHints): a
// one-row difference only shows when a node is just at the limit.
func TestGoParserMatchesTreeSitterAtEveryLineLimit(t *testing.T) {
	var body strings.Builder
	for i := range 12 {
		fmt.Fprintf(&body, "\tx%d := %d\n", i, i)
	}
	big := "func Big() {\n" + body.String() + "\tif x0 > 0 {\n\t\tprintln()\n\t}\n\n}\n"
	srcs := []string{
		"package p\n\nfunc small() {\n\t// the only comment\n}\n\n" + big,
		"package p\n\nfunc small(x int) {\n\tswitch x {\n\t}\n}\n\n" + big,
		"package p\n\nvar url = \"http://example.com\"\n\n" + big,
		"// Package p.\npackage p\n\n" + big,
		"package p\n\n" + big + "\n// after\n",
		"package p\n\n" + big,
	}
	for _, src := range srcs {
		for maxLines := range 25 {
			for _, maxBytes := range []int{1 << 20, 120} {
				opts := Options{MinLines: 1, MaxLines: maxLines, MaxBytes: maxBytes}
				if d := compareRoutes("big.go", []byte(src), opts); d.bug() {
					t.Fatalf("MaxLines %d, MaxBytes %d: %s\n%s", maxLines, maxBytes, d.describe(), src)
				}
			}
		}
	}
}
