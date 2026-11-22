package chunk

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	gotreesitter "github.com/odvcencio/gotreesitter"
)

// nodesOfType lists every node under n, n included, whose type is typ.
func nodesOfType(t *Tree, n *gotreesitter.Node, typ string) []*gotreesitter.Node {
	var out []*gotreesitter.Node
	if t.NodeType(n) == typ {
		out = append(out, n)
	}
	for _, c := range n.Children() {
		out = append(out, nodesOfType(t, c, typ)...)
	}
	return out
}

func TestParseReturnsTheWholeTree(t *testing.T) {
	tree, err := Parse("billing.go", []byte(goSrc), Default.ParseTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Release()

	if tree.Lang.Name != "go" || tree.Lang.Grammar == nil || tree.Lang.Entry == nil {
		t.Fatalf("language = %+v, want go with its grammar and entry", tree.Lang)
	}
	if string(tree.Source()) != goSrc {
		t.Error("the tree's source is not the source parsed")
	}
	root := tree.RootNode()
	if typ := tree.NodeType(root); typ != "source_file" {
		t.Fatalf("root is %q, want source_file", typ)
	}

	// Nested nodes are there, not just the top-level pieces Split cuts at.
	var names []string
	for _, fn := range nodesOfType(tree, root, "function_declaration") {
		names = append(names, tree.NodeText(tree.ChildByField(fn, "name")))
	}
	for _, m := range nodesOfType(tree, root, "method_declaration") {
		names = append(names, tree.NodeText(tree.ChildByField(m, "name")))
	}
	if want := []string{"RetryFailedPayment", "Total"}; !slices.Equal(names, want) {
		t.Errorf("functions = %v, want %v", names, want)
	}
	if n := len(nodesOfType(tree, root, "if_statement")); n != 1 {
		t.Errorf("found %d if statements inside functions, want 1", n)
	}
	var comments []string
	for _, c := range nodesOfType(tree, root, "comment") {
		comments = append(comments, tree.NodeText(c))
	}
	if want := []string{"// RetryFailedPayment tries the charge again."}; !slices.Equal(comments, want) {
		t.Errorf("comments = %q, want %q", comments, want)
	}
}

func TestParsedTreesRunTheirHighlightQuery(t *testing.T) {
	cases := []struct {
		file, src string
		capture   string // a capture the query must make
		text      string // on this text
	}{
		// Go and Python capture a callee as plain @function, not
		// @function.call, and as @variable too.
		{"a.go", "package p\n\nfunc f(x int) int { return g(x.y) }\n", "function", "g"},
		{"a.ts", "function f(x: number) { return g(x.y); }\n", "property", "y"},
		{"a.py", "def f(x):\n    return g(x.y)\n", "function", "g"},
		{"a.rs", "fn f(x: i32) -> i32 { g(x) }\n", "function", "f"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			tree, err := Parse(tc.file, []byte(tc.src), Default.ParseTimeout)
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Release()
			if tree.Lang.HighlightQuery == "" {
				t.Fatalf("%s has no highlight query", tree.Lang.Name)
			}
			q, err := gotreesitter.NewQuery(tree.Lang.HighlightQuery, tree.Lang.Grammar)
			if err != nil {
				t.Fatalf("%s's highlight query doesn't compile: %v", tree.Lang.Name, err)
			}
			var got []string
			for _, m := range q.ExecuteNode(tree.RootNode(), tree.Lang.Grammar, tree.Source()) {
				for _, c := range m.Captures {
					got = append(got, c.Name+"="+tree.NodeText(c.Node))
					if c.Name == tc.capture && tree.NodeText(c.Node) == tc.text {
						return
					}
				}
			}
			t.Errorf("no @%s capture of %q; captures: %v", tc.capture, tc.text, got)
		})
	}
}

func TestDetectFindsTheGrammarParseUses(t *testing.T) {
	lang := Detect("src/app.tsx")
	if lang == nil || lang.Name != "tsx" || lang.Grammar == nil || lang.HighlightQuery == "" {
		t.Fatalf("Detect(app.tsx) = %+v, want tsx with a grammar and highlight query", lang)
	}
	if lang := Detect("notes.unknownext"); lang != nil {
		t.Errorf("Detect(notes.unknownext) = %+v, want nil", lang)
	}
}

// bigPython is a file the parser and the plain splitter cut differently,
// and that takes far longer than a microsecond to parse.
func bigPython() []byte {
	var b strings.Builder
	b.WriteString("import os\n\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "def f%d(a):\n    if a > %d:\n        return a\n    return %d\n\n", i, i, i)
	}
	return []byte(b.String())
}

func TestParseSaysWhyThereIsNoTree(t *testing.T) {
	cases := []struct {
		name, file, src string
		timeout         time.Duration
		want            error
	}{
		{"no grammar", "notes.unknownext", "step one\n", 0, ErrNoGrammar},
		{"syntax errors", "broken.py", "def f(:\n    return )\n", 0, ErrSyntax},
		{"timeout", "big.py", string(bigPython()), time.Nanosecond, ErrTimeout},
	}
	sentinels := []error{ErrNoGrammar, ErrTimeout, ErrStoppedEarly, ErrSyntax}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := Parse(tc.file, []byte(tc.src), tc.timeout)
			if tree != nil {
				tree.Release()
				t.Fatal("got a tree, want none")
			}
			for _, s := range sentinels {
				if errors.Is(err, s) != (s == tc.want) {
					t.Errorf("errors.Is(%v, %v) = %v", err, s, !(s == tc.want))
				}
			}
			if !strings.Contains(err.Error(), tc.file) {
				t.Errorf("error %q doesn't name the file", err)
			}
		})
	}
}

// Parse and Split must agree on which files have a tree: a caller that
// gets an error from Parse knows Split cut that file with the plain
// splitter, and one that gets a tree knows Split cut it at syntax nodes.
func TestParseFailsExactlyWhenSplitFallsBack(t *testing.T) {
	opts := Options{MinLines: 1, MaxLines: 300, MaxBytes: 1 << 20}
	cases := []struct {
		name, file string
		src        []byte
		timeout    time.Duration
	}{
		{"valid Python", "big.py", bigPython(), 0},
		{"Python past its timeout", "big.py", bigPython(), time.Nanosecond},
		{"broken Python", "broken.py", []byte("def f(:\n    return )\n"), 0},
		{"no grammar", "big.unknownext", bigPython(), 0},
		{"valid Go, by tree-sitter", "billing.go", []byte(goSrc), 0},
		{"broken Go", "broken.go", []byte("package p\n\nfunc {{{ broken\n"), 0},
		{"valid TypeScript", "a.ts", []byte("export function f(x: number) {\n  return x + 1;\n}\n"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := opts
			o.ParseTimeout = tc.timeout
			tree, err := Parse(tc.file, tc.src, tc.timeout)
			if tree != nil {
				tree.Release()
			}
			_, via := split(tc.file, tc.src, o, false) // tree-sitter for Go too, as Parse
			if (err == nil) != (via == routeTree) {
				t.Fatalf("Parse error %v, but Split took route %d", err, via)
			}
			if err != nil {
				plain := Split("x.unknownext", tc.src, o)
				if got := Split(tc.file, tc.src, o); !isGo(tc.file) && fmt.Sprint(got) != fmt.Sprint(plain) {
					t.Errorf("Split didn't use the plain splitter:\n got  %v\n want %v", spans(got), spans(plain))
				}
			}
		})
	}
}

func TestParseIsSafeForConcurrentUse(t *testing.T) {
	src := bigPython()
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			tree, err := Parse("big.py", src, Default.ParseTimeout)
			if err == nil {
				if n := len(nodesOfType(tree, tree.RootNode(), "function_definition")); n != 400 {
					err = fmt.Errorf("found %d functions, want 400", n)
				}
				tree.Release()
			}
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
