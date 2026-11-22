//go:build jinter_cgo

package parse

import (
	"context"
	"testing"
)

func TestRouterRoutesToC(t *testing.T) {
	r, err := NewRouter("typescript=c, *=c")
	if err != nil {
		t.Fatal(err)
	}
	if r.String() != "typescript=c,*=c" {
		t.Errorf("routing %q", r.String())
	}
	if r.For("typescript") != CTreeSitter || r.For("rust") != CTreeSitter {
		t.Error("typescript or rust isn't routed to c")
	}
	if r.For("swift") != GoTreeSitter {
		t.Error("swift, which c lacks, isn't left to gotreesitter")
	}
	if _, err := NewRouter("swift=c"); err == nil {
		t.Error("routing swift to c: no error")
	}
	tree, err := r.ParseFile(context.Background(), "a.tsx", []byte("const a = <div/>\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Release()
	if tree.Backend() != "c" || tree.Language() != "tsx" || tree.Root().HasError() {
		t.Errorf("parsed as %s by %s, error %v", tree.Language(), tree.Backend(), tree.Root().HasError())
	}
}

// Every C grammar's highlight query compiles, and every grammar has a name
// gotreesitter's registry knows, so Detect routes files to it.
func TestCGrammarsAndQueriesLoad(t *testing.T) {
	for _, lang := range CTreeSitter.Languages() {
		if !GoTreeSitter.Supports(lang) {
			t.Errorf("%s: not a gotreesitter language name", lang)
		}
		tree, err := CTreeSitter.Parse(context.Background(), lang, []byte("x\n"), 0)
		if err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		if _, err := tree.Highlights(nil); err != nil {
			t.Errorf("%s: %v", lang, err)
		}
		tree.Release()
	}
}
