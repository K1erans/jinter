//go:build jinter_tsasm && cgo && linux && amd64

package parse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRouterRoutesToTsasm(t *testing.T) {
	r, err := NewRouter("*=tsasm")
	if err != nil {
		t.Fatal(err)
	}
	if r.For("go") != Tsasm || r.For("json") != Tsasm || r.For("python") != GoTreeSitter {
		t.Error("go and json aren't routed to tsasm, or python, which it lacks, isn't left to gotreesitter")
	}
	if _, err := NewRouter("python=tsasm"); err == nil {
		t.Error("routing python to tsasm: no error")
	}
	tree, err := r.ParseFile(context.Background(), "a.java", []byte("class A { void f() {} }\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Release()
	if tree.Backend() != "tsasm" || tree.Language() != "java" || tree.Root().Type() != "program" {
		t.Errorf("parsed as %s by %s, root %q", tree.Language(), tree.Backend(), tree.Root().Type())
	}
}

func TestTsasmSaysWhyThereIsNoTree(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		src  string
		want error
	}{
		{"syntax", context.Background(), "package p\nfunc f( {\n", ErrSyntax},
		{"cancelled", cancelled, "package p\n", ErrCancelled},
	} {
		tree, err := Tsasm.Parse(tc.ctx, "go", []byte(tc.src), 0)
		if tree != nil {
			tree.Release()
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err %v, want %v", tc.name, err, tc.want)
		}
	}
	// The parser that failed parses again.
	tree, err := Tsasm.Parse(context.Background(), "go", []byte("package p\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	tree.Release()
}

// A tree's nodes belong to the parser that made it, so trees alive at the
// same time must not share one.
func TestTsasmTreesLiveTogether(t *testing.T) {
	a, err := Tsasm.Parse(context.Background(), "json", []byte(`{"a": [1, 2]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := Tsasm.Parse(context.Background(), "go", []byte("package p\n\nvar x = 1\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	b.Release()
	if got := shapeOf(a.Root(), a.Source()); got != `document[0,13) {object[0,13) {"{"[0,1) pair[1,12) {string[1,4) {"\""[1,2) string_content[2,3) "\""[3,4)} ":"[4,5) array[6,12) {"["[6,7) number[7,8) ","[8,9) number[10,11) "]"[11,12)}} "}"[12,13)}}` {
		t.Errorf("first tree changed under the second parse:\n%s", got)
	}
}

// shapeOf writes n's subtree as type[start,end) {children}.
func shapeOf(n Node, src []byte) string {
	s := fmt.Sprintf("%s[%d,%d)", n.Type(), n.StartByte(), n.EndByte())
	if !n.IsNamed() {
		s = fmt.Sprintf("%q[%d,%d)", n.Type(), n.StartByte(), n.EndByte())
	}
	if n.ChildCount() == 0 {
		return s
	}
	s += " {"
	for i, c := range n.Children(nil) {
		if i > 0 {
			s += " "
		}
		s += shapeOf(c, src)
	}
	return s + "}"
}

// tsasm reads the same grammar versions as the C backend, so where a file
// parses, their trees are the same: types, spans, rows, parents and
// fields.
func TestTsasmTreesMatchC(t *testing.T) {
	c := Lookup("c")
	if c == nil {
		t.Skip("no C backend in this build; add -tags jinter_cgo")
	}
	files, _ := filepath.Glob("../chunk/testdata/golden/*")
	for _, s := range samples {
		files = append(files, s.lang+":"+s.src)
	}
	compared := 0
	for _, f := range files {
		var lang string
		var src []byte
		if l, text, ok := cutSample(f); ok {
			lang, src = l, []byte(text)
		} else {
			lang = Detect(f)
			var err error
			if src, err = os.ReadFile(f); err != nil {
				t.Fatal(err)
			}
		}
		if !Tsasm.Supports(lang) {
			continue
		}
		want, err := c.Parse(context.Background(), lang, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Tsasm.Parse(context.Background(), lang, src, 0)
		if want.Root().HasError() {
			if !errors.Is(err, ErrSyntax) {
				t.Errorf("%s: C finds syntax errors, tsasm gives err %v", f, err)
			}
			want.Release()
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var wn, gn []Node
		walk(want.Root(), func(n Node) { wn = append(wn, n) })
		walk(got.Root(), func(n Node) { gn = append(gn, n) })
		if len(wn) != len(gn) {
			t.Errorf("%s: %d nodes, C has %d", f, len(gn), len(wn))
		}
		for i := range min(len(wn), len(gn)) {
			w, g := wn[i], gn[i]
			if w.Type() != g.Type() || w.IsNamed() != g.IsNamed() || w.StartByte() != g.StartByte() || w.EndByte() != g.EndByte() ||
				w.StartPoint() != g.StartPoint() || w.EndPoint() != g.EndPoint() || w.Parent().StartByte() != g.Parent().StartByte() ||
				w.ChildCount() != g.ChildCount() || w.FieldName() != g.FieldName() {
				t.Errorf("%s: node %d is %s:%q %v-%v, C's is %s:%q %v-%v", f, i, g.FieldName(), g.Type(), g.StartPoint(), g.EndPoint(), w.FieldName(), w.Type(), w.StartPoint(), w.EndPoint())
				break
			}
		}
		want.Release()
		got.Release()
		compared++
	}
	if compared < 6 {
		t.Errorf("compared %d files", compared)
	}
}

// cutSample splits a "lang:source" sample.
func cutSample(f string) (lang, src string, ok bool) {
	for _, s := range samples {
		if f == s.lang+":"+s.src {
			return s.lang, s.src, true
		}
	}
	return "", "", false
}
