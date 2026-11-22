package parse

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// sample is a small file per language, with the functions it defines and a
// highlight capture its query must make.
type sample struct {
	lang, src string
	funcType  string   // the node type of a function definition
	funcs     []string // their names, by the "name" field
	capture   string   // a capture name the highlight query makes …
	captured  string   // … on this text
}

var samples = []sample{
	{"go", "package p\n\n// F adds.\nfunc F(a int) int { return a + 1 }\n\nfunc G() { F(2) }\n",
		"function_declaration", []string{"F", "G"}, "type", "int"},
	{"typescript", "class Shop { total(n: number): number { return n } }\nfunction f(x: string) { return new Shop() }\nfunction g() {}\n",
		"function_declaration", []string{"f", "g"}, "constructor", "Shop"},
	{"javascript", "function f(a) { return a }\nfunction g() { return new Map() }\n",
		"function_declaration", []string{"f", "g"}, "constructor", "Map"},
	{"python", "def f(a):\n    return a\n\ndef g():\n    return f(1)\n",
		"function_definition", []string{"f", "g"}, "function", "f"},
	{"rust", "fn f(a: i32) -> i32 { a }\nfn g() -> String { String::new() }\n",
		"function_item", []string{"f", "g"}, "type", "String"},
}

// walk calls fn on every node under n, n included, in order.
func walk(n Node, fn func(Node)) {
	fn(n)
	for i := range n.ChildCount() {
		walk(n.Child(i), fn)
	}
}

// eachBackend runs f for every backend in this build and every sample it
// parses.
func eachBackend(t *testing.T, f func(t *testing.T, b Backend, s sample)) {
	for _, b := range Backends() {
		for _, s := range samples {
			if !b.Supports(s.lang) {
				continue
			}
			t.Run(b.Name()+"/"+s.lang, func(t *testing.T) { f(t, b, s) })
		}
	}
}

func TestBackendsReturnTheWholeTree(t *testing.T) {
	eachBackend(t, func(t *testing.T, b Backend, s sample) {
		tree, err := b.Parse(context.Background(), s.lang, []byte(s.src), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer tree.Release()
		if tree.Language() != s.lang || tree.Backend() != b.Name() || string(tree.Source()) != s.src {
			t.Fatalf("tree is %s by %s", tree.Language(), tree.Backend())
		}
		root := tree.Root()
		if root.IsNull() || root.HasError() || !root.Parent().IsNull() {
			t.Fatalf("root %q: null %v, error %v", root.Type(), root.IsNull(), root.HasError())
		}
		if root.StartByte() != 0 || int(root.EndByte()) > len(s.src) {
			t.Errorf("root spans %d-%d of %d bytes", root.StartByte(), root.EndByte(), len(s.src))
		}

		var names []string
		count := 0
		walk(root, func(n Node) {
			count++
			// Every child knows its parent, and lies inside it.
			for i := range n.ChildCount() {
				c := n.Child(i)
				if c.Parent() != n && c.Parent().ID() != n.ID() {
					t.Fatalf("%q's child %q has parent %q", n.Type(), c.Type(), c.Parent().Type())
				}
				if c.StartByte() < n.StartByte() || c.EndByte() > n.EndByte() {
					t.Fatalf("%q's child %q spans %d-%d, outside %d-%d", n.Type(), c.Type(), c.StartByte(), c.EndByte(), n.StartByte(), n.EndByte())
				}
			}
			// Points agree with byte offsets.
			if p := n.StartPoint(); offset(s.src, p) != int(n.StartByte()) {
				t.Fatalf("%q starts at %+v, byte %d", n.Type(), p, n.StartByte())
			}
			if n.Type() == s.funcType {
				name := n.ChildByField("name")
				if name.FieldName() != "name" {
					t.Errorf("%q's name field is %q", tree.Text(name), name.FieldName())
				}
				names = append(names, tree.Text(name))
			}
		})
		if !slices.Equal(names, s.funcs) {
			t.Errorf("functions %v, want %v", names, s.funcs)
		}
		if count < 10 {
			t.Errorf("walked %d nodes; want the whole tree", count)
		}

		// Children and NamedChildren agree with Child.
		all := root.Children(nil)
		named := root.NamedChildren(nil)
		if len(all) != root.ChildCount() {
			t.Errorf("Children gave %d, ChildCount %d", len(all), root.ChildCount())
		}
		var want []Node
		for i := range root.ChildCount() {
			if c := root.Child(i); c.IsNamed() {
				want = append(want, c)
			}
		}
		if len(named) != len(want) {
			t.Errorf("NamedChildren gave %d, want %d", len(named), len(want))
		}
		for i := range named {
			if named[i].ID() != want[i].ID() {
				t.Errorf("named child %d differs", i)
			}
		}
		if c := root.Child(root.ChildCount()); !c.IsNull() {
			t.Errorf("child past the end is %q, want null", c.Type())
		}
	})
}

// offset is the byte offset of p in src.
func offset(src string, p Point) int {
	off := 0
	for range p.Row {
		i := strings.IndexByte(src[off:], '\n')
		if i < 0 {
			return -1
		}
		off += i + 1
	}
	return off + int(p.Column)
}

func TestBackendsRunTheirHighlightQuery(t *testing.T) {
	eachBackend(t, func(t *testing.T, b Backend, s sample) {
		tree, err := b.Parse(context.Background(), s.lang, []byte(s.src), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer tree.Release()
		caps, err := tree.Highlights(nil)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		found := false
		for i, c := range caps {
			got = append(got, c.Name+"="+tree.Text(c.Node))
			found = found || (c.Name == s.capture && tree.Text(c.Node) == s.captured)
			if i > 0 && c.Node.StartByte() < caps[i-1].Node.StartByte() {
				t.Fatalf("captures out of order at %d", i)
			}
		}
		if !found {
			t.Errorf("no @%s on %q; captures %v", s.capture, s.captured, got)
		}
	})
}

func TestBackendsReturnTreesWithErrors(t *testing.T) {
	for _, b := range Backends() {
		t.Run(b.Name(), func(t *testing.T) {
			tree, err := b.Parse(context.Background(), "python", []byte("def f(:\n    return )\n"), 0)
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Release()
			if !tree.Root().HasError() {
				t.Fatal("root of a broken file has no error")
			}
			found := false
			walk(tree.Root(), func(n Node) { found = found || n.IsError() || n.IsMissing() })
			if !found {
				t.Error("no ERROR or missing node in a broken file")
			}
		})
	}
}

// bigPython takes far longer than a microsecond to parse.
func bigPython() []byte {
	var b strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&b, "def f%d(a):\n    if a > %d:\n        return a\n    return %d\n\n", i, i, i)
	}
	return []byte(b.String())
}

func TestBackendsSayWhyThereIsNoTree(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, b := range Backends() {
		t.Run(b.Name(), func(t *testing.T) {
			cases := []struct {
				name    string
				ctx     context.Context
				lang    string
				timeout time.Duration
				want    error
			}{
				{"unsupported", context.Background(), "no-such-language", 0, ErrUnsupported},
				{"timeout", context.Background(), "python", time.Nanosecond, ErrTimeout},
				{"cancelled", cancelled, "python", 0, ErrCancelled},
			}
			for _, tc := range cases {
				tree, err := b.Parse(tc.ctx, tc.lang, bigPython(), tc.timeout)
				if tree != nil {
					tree.Release()
				}
				if !errors.Is(err, tc.want) {
					t.Errorf("%s: err %v, want %v", tc.name, err, tc.want)
				}
			}
		})
	}
}

// Walking a tree must cost no allocation per node, so the interface adds
// little to a parse.
func TestWalkingAllocatesNothingPerNode(t *testing.T) {
	for _, b := range Backends() {
		t.Run(b.Name(), func(t *testing.T) {
			tree, err := b.Parse(context.Background(), "python", bigPython(), 0)
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Release()
			buf := make([]Node, 0, 4096)
			nodes := 0
			allocs := testing.AllocsPerRun(3, func() {
				nodes = 0
				var visit func(n Node)
				visit = func(n Node) {
					nodes++
					_, _, _, _ = n.Type(), n.IsNamed(), n.StartPoint(), n.EndByte()
					for i := range n.ChildCount() {
						visit(n.Child(i))
					}
				}
				visit(tree.Root())
				buf = tree.Root().NamedChildren(buf[:0])
			})
			if allocs > 10 {
				t.Errorf("%v allocations walking %d nodes", allocs, nodes)
			}
		})
	}
}

func TestRouterSpecs(t *testing.T) {
	for _, tc := range []struct{ spec, want string }{
		{"", ""},
		{"*=gotreesitter", ""},
		{" go = gotreesitter , python=gotreesitter", ""},
	} {
		r, err := NewRouter(tc.spec)
		if err != nil {
			t.Fatalf("%q: %v", tc.spec, err)
		}
		if r.String() != tc.want || r.For("go") != GoTreeSitter {
			t.Errorf("%q: routing %q, want %q", tc.spec, r.String(), tc.want)
		}
	}
	for _, bad := range []string{"go", "go=", "=c", "go=nope", "*=nope"} {
		if _, err := NewRouter(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestRouterParsesFilesByName(t *testing.T) {
	r, _ := NewRouter("")
	tree, err := r.ParseFile(context.Background(), "src/a.rs", []byte("fn f() {}\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Release()
	if tree.Language() != "rust" || tree.Backend() != "gotreesitter" {
		t.Fatalf("parsed as %s by %s", tree.Language(), tree.Backend())
	}
	if _, err := r.ParseFile(context.Background(), "notes.unknownext", nil, 0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v, want ErrUnsupported", err)
	}
}

func TestNullNodesAreEmpty(t *testing.T) {
	var n Node
	if !n.IsNull() || n.Type() != "" || n.ChildCount() != 0 || !n.Child(0).IsNull() || !n.Parent().IsNull() ||
		!n.ChildByField("name").IsNull() || n.FieldName() != "" || n.HasError() || len(n.Children(nil)) != 0 {
		t.Fatal("the null node isn't empty")
	}
}

func TestParseIsSafeForConcurrentUse(t *testing.T) {
	src := bigPython()
	for _, b := range Backends() {
		t.Run(b.Name(), func(t *testing.T) {
			errs := make(chan error, 8)
			for range 8 {
				go func() {
					tree, err := b.Parse(context.Background(), "python", src, 0)
					if err == nil {
						if n := len(tree.Root().NamedChildren(nil)); n != 2000 {
							err = fmt.Errorf("%d functions, want 2000", n)
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
		})
	}
}
