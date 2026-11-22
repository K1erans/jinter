//go:build jinter_tsasm && cgo && linux && amd64

package parse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/K1erans/tsasm"
)

func init() { backends["tsasm"] = tsasmB }

// Tsasm is tsasm (github.com/K1erans/tsasm), a tree-sitter parser runtime
// in x86-64 assembly that reads the official grammars' parse tables,
// through cgo. It is in builds with the jinter_tsasm tag on linux/amd64
// only, and parses Go, C, Java and JSON, with the grammar versions go.mod
// pins for the C backend.
//
// It differs from the other backends in three ways. It has no error
// recovery: a source with syntax errors gives no tree and ErrSyntax. It
// has no highlight query, so Highlights adds nothing. And it ignores the
// timeout: its own work budget, a fixed number of steps per input byte,
// stops a runaway parse with ErrStoppedEarly instead.
var Tsasm Backend = tsasmB

var tsasmB = &tsasmBackend{}

type tsasmBackend struct {
	mu   sync.Mutex
	idle []*tsasmParser
}

// tsasmParser is a parser and the scratch its trees use. A tree holds its
// parser until it is released, since the nodes are the parser's.
type tsasmParser struct {
	p     *tsasm.Parser
	sizes []int32  // subtree sizes, by node
	lines []uint32 // the byte offset where each row starts
}

func (*tsasmBackend) Name() string { return "tsasm" }

func (*tsasmBackend) Languages() []string { return tsasm.Languages() }

func (*tsasmBackend) Supports(lang string) bool { return tsasm.Lookup(lang) != nil }

func (b *tsasmBackend) get() (*tsasmParser, error) {
	b.mu.Lock()
	if n := len(b.idle); n > 0 {
		p := b.idle[n-1]
		b.idle = b.idle[:n-1]
		b.mu.Unlock()
		return p, nil
	}
	b.mu.Unlock()
	p, err := tsasm.NewParser()
	if err != nil {
		return nil, err
	}
	return &tsasmParser{p: p}, nil
}

func (b *tsasmBackend) put(p *tsasmParser) {
	b.mu.Lock()
	b.idle = append(b.idle, p)
	b.mu.Unlock()
}

func (b *tsasmBackend) Parse(ctx context.Context, lang string, src []byte, _ time.Duration) (*Tree, error) {
	l := tsasm.Lookup(lang)
	if l == nil {
		return nil, fmt.Errorf("%w: tsasm has no %q grammar", ErrUnsupported, lang)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCancelled, err)
	}
	p, err := b.get()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStoppedEarly, err)
	}
	nodes, err := p.p.Parse(l, src)
	if err != nil {
		b.put(p)
		if errors.Is(err, tsasm.ErrSyntax) {
			return nil, ErrSyntax
		}
		return nil, fmt.Errorf("%w: %v", ErrStoppedEarly, err)
	}
	t := &tsasmTree{nodes: nodes, lang: l, p: p, src: src}
	return &Tree{t: t, root: t.node(0), lang: lang, backend: "tsasm", src: src}, nil
}

// A tsasm Node is its index in the tree's nodes, in raw[0].
type tsasmTree struct {
	nodes []tsasm.Node // the parser's, until release
	lang  *tsasm.Language
	p     *tsasmParser
	src   []byte
	sizes []int32  // from p, filled on first use
	lines []uint32 // from p, filled on first use
}

func (t *tsasmTree) node(i int32) Node { return Node{t: t, raw: [4]uint64{uint64(i)}} }

func tsasmIndex(n Node) int32 { return int32(n.raw[0]) }

func (t *tsasmTree) at(n Node) *tsasm.Node { return &t.nodes[tsasmIndex(n)] }

func (t *tsasmTree) nodeType(n Node) string  { return t.lang.SymbolName(t.at(n).Symbol) }
func (t *tsasmTree) isNamed(n Node) bool     { return t.at(n).Named != 0 }
func (t *tsasmTree) isError(Node) bool       { return false }
func (t *tsasmTree) isMissing(Node) bool     { return false }
func (t *tsasmTree) hasError(Node) bool      { return false }
func (t *tsasmTree) startByte(n Node) uint32 { return t.at(n).Start }
func (t *tsasmTree) endByte(n Node) uint32   { return t.at(n).End }
func (t *tsasmTree) startPoint(n Node) Point { return t.point(t.at(n).Start) }
func (t *tsasmTree) endPoint(n Node) Point   { return t.point(t.at(n).End) }
func (t *tsasmTree) id(n Node) uintptr       { return uintptr(tsasmIndex(n)) + 1 }

// Each node carries its own field, as C tree-sitter's tree cursor gives
// it, so the first child in a field is the first child with it.
func (t *tsasmTree) childByField(n Node, field string) Node {
	var out Node
	t.eachChild(n, func(c int32) bool {
		if f := t.nodes[c].Field; f != 0 && t.lang.FieldName(f) == field {
			out = t.node(c)
			return false
		}
		return true
	})
	return out
}

func (t *tsasmTree) fieldName(n Node) string { return t.lang.FieldName(t.at(n).Field) }

func (t *tsasmTree) highlights(_ *Tree, dst []Capture) ([]Capture, error) { return dst, nil }

func (t *tsasmTree) parent(n Node) Node {
	if p := t.at(n).Parent; p >= 0 {
		return t.node(p)
	}
	return Node{}
}

// subtreeSizes counts the nodes under each node, itself included, so a walk
// can step from a child to its next sibling.
func (t *tsasmTree) subtreeSizes() []int32 {
	if t.sizes == nil {
		s := slices.Grow(t.p.sizes[:0], len(t.nodes))[:len(t.nodes)]
		for i := range s {
			s[i] = 1
		}
		for j := len(t.nodes) - 1; j > 0; j-- {
			s[t.nodes[j].Parent] += s[j]
		}
		t.p.sizes, t.sizes = s, s
	}
	return t.sizes
}

// eachChild calls f on n's children in order, until f returns false. In
// preorder a node's first child follows it, and each next sibling follows
// the subtree before it.
func (t *tsasmTree) eachChild(n Node, f func(c int32) bool) {
	i := tsasmIndex(n)
	end := int32(len(t.nodes))
	if i+1 >= end || t.nodes[i+1].Parent != i {
		return
	}
	sizes := t.subtreeSizes()
	for c := i + 1; c < end && t.nodes[c].Parent == i; c += sizes[c] {
		if !f(c) {
			return
		}
	}
}

func (t *tsasmTree) childCount(n Node) int {
	k := 0
	t.eachChild(n, func(int32) bool { k++; return true })
	return k
}

func (t *tsasmTree) child(n Node, i int) Node {
	var out Node
	t.eachChild(n, func(c int32) bool {
		if i == 0 {
			out = t.node(c)
			return false
		}
		i--
		return true
	})
	return out
}

func (t *tsasmTree) children(n Node, dst []Node, namedOnly bool) []Node {
	t.eachChild(n, func(c int32) bool {
		if !namedOnly || t.nodes[c].Named != 0 {
			dst = append(dst, t.node(c))
		}
		return true
	})
	return dst
}

// point is the row and byte column of offset b.
func (t *tsasmTree) point(b uint32) Point {
	if t.lines == nil {
		lines := append(t.p.lines[:0], 0)
		for at := 0; ; {
			k := bytes.IndexByte(t.src[at:], '\n')
			if k < 0 {
				break
			}
			at += k + 1
			lines = append(lines, uint32(at))
		}
		t.p.lines, t.lines = lines, lines
	}
	row, found := slices.BinarySearch(t.lines, b)
	if !found {
		row--
	}
	return Point{uint32(row), b - t.lines[row]}
}

func (t *tsasmTree) release() {
	tsasmB.put(t.p)
	t.nodes, t.p, t.sizes, t.lines = nil, nil, nil, nil
}
