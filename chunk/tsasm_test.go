//go:build jinter_tsasm && cgo && linux && amd64

package chunk

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/K1erans/jinter/parse"
)

// Split works with Go, C, Java and JSON routed to tsasm. A file tsasm finds
// syntax errors in is split as plain text, as a tree with errors is.
// gotreesitter's trees differ from the official grammars' in places, so
// the chunks may too; the test lists the files where they do.
func TestSplitWithTsasm(t *testing.T) {
	files, _ := filepath.Glob("testdata/golden/*")
	same, diff := 0, []string{}
	for _, f := range files {
		name := filepath.Base(f)
		if !parse.Tsasm.Supports(parse.Detect(name)) {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, opts := range goldenOptions {
			c := opts
			c.Parsers = "*=tsasm"
			if !strings.Contains(c.String(), "Parsers:*=tsasm") {
				t.Fatalf("options %s don't name their routing", c)
			}
			got := Split(name, src, c)
			for _, ch := range got {
				if ch.Offset < 0 || ch.Offset+len(ch.Code) > len(src) || string(src[ch.Offset:ch.Offset+len(ch.Code)]) != ch.Code {
					t.Fatalf("%s: chunk %d-%d isn't the source at its offset", name, ch.Start, ch.End)
				}
			}
			if slices.Equal(got, Split(name, src, opts)) {
				same++
			} else {
				diff = append(diff, name)
			}
		}
	}
	if same == 0 {
		t.Fatal("no golden file in a language tsasm parses")
	}
	diff = slices.Compact(diff)
	t.Logf("%d file×options splits match gotreesitter's; these files differ: %v", same, diff)
}

// With Go routed to tsasm, Go files skip go/parser; with Go elsewhere they
// still take it.
func TestGoRoutedToTsasmSkipsGoParser(t *testing.T) {
	src := []byte("package p\n\n// F does nothing.\nfunc F() {}\n\nfunc G() {}\n")
	tsasm := Default
	tsasm.Parsers = "go=tsasm"
	if _, via := split("a.go", src, tsasm, true); via != routeTree {
		t.Errorf("go=tsasm: route %d, want tree-sitter's (%d)", via, routeTree)
	}
	for _, parsers := range []string{"", "*=tsasm,go=gotreesitter"} {
		o := Default
		o.Parsers = parsers
		if _, via := split("a.go", src, o, true); via != routeGo {
			t.Errorf("%q: route %d, want go/parser's (%d)", parsers, via, routeGo)
		}
	}
}
