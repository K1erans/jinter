// Command parsebench compares the parse backends on real code: speed on
// one thread and on all of them, whether their trees agree, and how often
// each fails. Build it with -tags jinter_cgo to compare C tree-sitter with
// gotreesitter, and add jinter_tsasm for tsasm (its library's directory in
// CGO_LDFLAGS):
//
//	go run -tags jinter_cgo ./internal/parsebench ~/src/repo-a ~/src/repo-b
//	CGO_LDFLAGS=-L$TSASM go run -tags 'jinter_cgo jinter_tsasm' ./internal/parsebench .
//
// It parses the files jinter would search (gitignore respected, test files
// included), grouped by language, and prints a Markdown table.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/K1erans/jinter/parse"
	"github.com/K1erans/jinter/walk"
)

var (
	backendsFlag = flag.String("backends", "", "comma-separated backends to compare (default: every backend in this build)")
	langsFlag    = flag.String("langs", "", "comma-separated languages (default: those every compared backend parses)")
	timeout      = flag.Duration("timeout", 30*time.Second, "per-file parse timeout, as chunk.Default uses")
	runs         = flag.Int("runs", 3, "timed passes per backend and language; the fastest counts")
	maxSize      = flag.Int64("max-size", 1<<20, "skip files larger than this many bytes")
	examples     = flag.Int("examples", 3, "mismatches to show per language")
)

type file struct {
	path string
	src  []byte
}

func main() {
	flag.Parse()
	paths := flag.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	backends, err := pickBackends()
	if err != nil {
		fail(err)
	}

	byLang := map[string][]file{}
	for _, p := range paths {
		res, err := walk.Walk([]string{p}, walk.Options{Tests: true, MaxFileSize: *maxSize})
		if err != nil {
			fail(err)
		}
		for _, f := range res.Files {
			lang := parse.Detect(f.Rel)
			if lang == "" {
				continue
			}
			src := f.Src
			if src == nil {
				if src, err = os.ReadFile(f.Path); err != nil {
					continue
				}
			}
			byLang[lang] = append(byLang[lang], file{f.Path, src})
		}
	}
	langs := pickLangs(backends, byLang)
	if len(langs) == 0 {
		fail(errors.New("no files in a language every backend parses"))
	}

	fmt.Printf("parsebench: %s; GOMAXPROCS %d; timeout %v; best of %d\n\n", strings.Join(paths, " "), runtime.GOMAXPROCS(0), *timeout, *runs)
	fmt.Println("| language | files | MB | backend | MB/s, 1 thread | MB/s, all threads | failed (timeout / other) | trees with syntax errors | same tree as " + backends[0].Name() + " (all nodes / named nodes) |")
	fmt.Println("|---|---:|---:|---|---:|---:|---:|---:|---:|")
	var notes []string
	for _, lang := range langs {
		files := byLang[lang]
		size := 0
		for _, f := range files {
			size += len(f.src)
		}
		sts := check(backends, lang, files)
		for i, b := range backends {
			one := best(func() time.Duration { return timeAll(b, lang, files, 1) })
			all := best(func() time.Duration { return timeAll(b, lang, files, runtime.GOMAXPROCS(0)) })
			st := sts[i]
			parity := "—"
			if i > 0 {
				parity = fmt.Sprintf("%s / %s", pct(st.same, len(files)), pct(st.sameNamed, len(files)))
				for _, e := range st.examples {
					notes = append(notes, fmt.Sprintf("%s, %s vs %s: %s", lang, backends[0].Name(), b.Name(), e))
				}
			}
			fmt.Printf("| %s | %d | %.2f | %s | %.1f | %.1f | %s | %s | %s |\n", lang, len(files), mb(size), b.Name(),
				mb(size)/one.Seconds(), mb(size)/all.Seconds(),
				rate(st.timeouts+st.failed, len(files), fmt.Sprintf(" (%d / %d)", st.timeouts, st.failed)),
				rate(st.syntax, len(files), ""), parity)
		}
	}
	if len(notes) > 0 {
		fmt.Println("\nFirst mismatches (type [start,end) \"text\" from each backend, at the first node where their preorder walks differ):")
		for _, n := range notes {
			fmt.Println("- " + n)
		}
	}
}

func pickBackends() ([]parse.Backend, error) {
	if *backendsFlag == "" {
		bs := parse.Backends()
		// gotreesitter, the default, is the reference.
		slices.SortStableFunc(bs, func(a, b parse.Backend) int {
			return cmp.Compare(boolInt(a != parse.GoTreeSitter), boolInt(b != parse.GoTreeSitter))
		})
		return bs, nil
	}
	var bs []parse.Backend
	for _, name := range strings.Split(*backendsFlag, ",") {
		b := parse.Lookup(strings.TrimSpace(name))
		if b == nil {
			return nil, fmt.Errorf("no backend %q in this build; build with -tags jinter_cgo for c, jinter_tsasm for tsasm", name)
		}
		bs = append(bs, b)
	}
	return bs, nil
}

func pickLangs(backends []parse.Backend, byLang map[string][]file) []string {
	if *langsFlag != "" {
		var out []string
		for _, l := range strings.Split(*langsFlag, ",") {
			if l = strings.TrimSpace(l); len(byLang[l]) > 0 {
				out = append(out, l)
			}
		}
		return out
	}
	var out []string
	for _, lang := range slices.Sorted(maps.Keys(byLang)) {
		ok := true
		for _, b := range backends {
			ok = ok && b.Supports(lang)
		}
		if ok {
			out = append(out, lang)
		}
	}
	return out
}

// best runs f *runs times and returns the fastest.
func best(f func() time.Duration) time.Duration {
	var min time.Duration
	for i := range max(*runs, 1) {
		if d := f(); i == 0 || d < min {
			min = d
		}
	}
	return min
}

// timeAll parses every file with workers goroutines and returns the wall
// time, failures included.
func timeAll(b parse.Backend, lang string, files []file, workers int) time.Duration {
	var next atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for range workers {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= len(files) {
					return
				}
				if t, err := b.Parse(context.Background(), lang, files[i].src, *timeout); err == nil {
					t.Release()
				}
			}
		})
	}
	wg.Wait()
	return time.Since(start)
}

type node struct {
	typ        string
	start, end uint32
	named      bool
}

type stats struct {
	timeouts, failed, syntax int
	same, sameNamed          int // files whose tree matches the first backend's
	examples                 []string
}

// check parses every file once more with each backend, in parallel,
// counting failures and comparing each backend's tree with the first
// backend's: node types and byte ranges in preorder, over all nodes and
// over named nodes only. A file either backend fails on is a mismatch.
func check(backends []parse.Backend, lang string, files []file) []stats {
	sts := make([]stats, len(backends))
	var mu sync.Mutex
	var next atomic.Int64
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= len(files) {
					return
				}
				f := files[i]
				var ref []node
				var refErr error
				for bi, b := range backends {
					nodes, syntax, err := shape(b, lang, f.src)
					mu.Lock()
					st := &sts[bi]
					switch {
					case errors.Is(err, parse.ErrTimeout):
						st.timeouts++
					case errors.Is(err, parse.ErrSyntax): // no tree, as from tsasm
						st.syntax++
					case err != nil:
						st.failed++
					case syntax:
						st.syntax++
					}
					if bi == 0 {
						ref, refErr = nodes, err
					} else if err != nil || refErr != nil {
						if len(st.examples) < *examples {
							st.examples = append(st.examples, fmt.Sprintf("%s: errors %v / %v", f.path, refErr, err))
						}
					} else if j := firstDiff(ref, nodes, false); j < 0 {
						st.same++
						st.sameNamed++
					} else {
						if len(st.examples) < *examples {
							st.examples = append(st.examples, fmt.Sprintf("%s: node %d of %d / %d: %s / %s", f.path, j, len(ref), len(nodes), at(ref, j, f.src), at(nodes, j, f.src)))
						}
						if firstDiff(ref, nodes, true) < 0 {
							st.sameNamed++
						}
					}
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	return sts
}

// shape parses src and lists its nodes in preorder; syntax says whether
// the tree holds errors.
func shape(b parse.Backend, lang string, src []byte) (nodes []node, syntax bool, err error) {
	t, err := b.Parse(context.Background(), lang, src, *timeout)
	if err != nil {
		return nil, false, err
	}
	defer t.Release()
	stack := []parse.Node{t.Root()}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		nodes = append(nodes, node{n.Type(), n.StartByte(), n.EndByte(), n.IsNamed()})
		k := len(stack)
		stack = n.Children(stack)
		slices.Reverse(stack[k:])
	}
	return nodes, t.Root().HasError(), nil
}

// firstDiff is the index of the first node where a and b differ, or -1.
func firstDiff(a, b []node, namedOnly bool) int {
	if namedOnly {
		a, b = named(a), named(b)
	}
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b))
	}
	return -1
}

func named(ns []node) []node {
	var out []node
	for i, n := range ns {
		if n.named || i == 0 {
			out = append(out, n)
		}
	}
	return out
}

func at(ns []node, i int, src []byte) string {
	if i >= len(ns) {
		return "(no more nodes)"
	}
	n := ns[i]
	text := ""
	if int(n.end) <= len(src) && n.start <= n.end {
		text = string(src[n.start:min(n.end, n.start+30)])
	}
	return fmt.Sprintf("%s [%d,%d) %q", n.typ, n.start, n.end, text)
}

func rate(n, total int, detail string) string {
	if n == 0 {
		return "0"
	}
	return pct(n, total) + detail
}

func pct(n, total int) string { return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(max(total, 1))) }

func mb(n int) float64 { return float64(n) / 1e6 }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "parsebench:", err)
	os.Exit(1)
}
