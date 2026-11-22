//go:build jinter_tsasm && cgo && linux && amd64

package parse

/*
// tsasm.h's declarations. The library is libtsasm.a, built outside this
// module; point the linker at it with CGO_LDFLAGS=-L<its directory>.
#cgo LDFLAGS: -ltsasm

#include <stdint.h>

typedef struct { uint16_t symbol; uint8_t named; uint8_t pad; uint32_t start_byte; uint32_t end_byte; int32_t parent; } TsasmNode;
typedef struct TsasmLanguage TsasmLanguage;
typedef struct TsasmParser TsasmParser;

const TsasmLanguage *tsasm_language_go(void);
const TsasmLanguage *tsasm_language_c(void);
const TsasmLanguage *tsasm_language_java(void);
const TsasmLanguage *tsasm_language_json(void);

TsasmParser *tsasm_parser_new(void);
void tsasm_parser_free(TsasmParser *p);
void tsasm_parser_set_retain(TsasmParser *p, uint64_t bytes);
int64_t tsasm_parse(TsasmParser *p, const TsasmLanguage *lang, const uint8_t *src, uint64_t len, const TsasmNode **nodes);
const char *tsasm_language_symbol_name(const TsasmLanguage *lang, uint16_t symbol);
uint32_t tsasm_language_symbol_count(const TsasmLanguage *lang);
*/
import "C"

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
	"unsafe"
)

// tsasmGrammars are the grammars libtsasm.a is built with, by
// gotreesitter's names for them. They are the versions go.mod pins for the
// C backend: tree-sitter-go v0.25.0, -c v0.24.2, -java v0.23.5 and -json
// v0.24.8.
var tsasmGrammars = map[string]func() *C.TsasmLanguage{
	"go":   func() *C.TsasmLanguage { return C.tsasm_language_go() },
	"c":    func() *C.TsasmLanguage { return C.tsasm_language_c() },
	"java": func() *C.TsasmLanguage { return C.tsasm_language_java() },
	"json": func() *C.TsasmLanguage { return C.tsasm_language_json() },
}

func init() { backends["tsasm"] = tsasm }

// Tsasm is tsasm, a tree-sitter parser runtime in x86-64 assembly that
// reads the official grammars' parse tables, through cgo. It is in builds
// with the jinter_tsasm tag on linux/amd64 only, and parses Go, C, Java and
// JSON.
//
// It differs from the other backends in three ways. It has no error
// recovery: a source with syntax errors gives no tree and ErrSyntax. Its
// trees have no grammar fields, so ChildByField returns the null node and
// FieldName "", and no highlight query, so Highlights adds nothing. And it
// ignores the timeout: its own work budget, a fixed number of steps per
// input byte, stops a runaway parse with ErrStoppedEarly instead.
var Tsasm Backend = tsasm

var tsasm = &tsasmBackend{}

type tsasmBackend struct {
	load  sync.Once
	langs map[string]*tsasmLang

	mu   sync.Mutex
	idle []*tsasmParser
}

// tsasmLang is one grammar and its node type names, by symbol.
type tsasmLang struct {
	lang  *C.TsasmLanguage
	names []string
}

// tsasmParser is a C parser and the scratch its trees use. A tree holds its
// parser until it is released, since the nodes are the parser's.
type tsasmParser struct {
	p     *C.TsasmParser
	sizes []int32  // subtree sizes, by node
	lines []uint32 // the byte offset where each row starts
}

func (*tsasmBackend) Name() string { return "tsasm" }

func (*tsasmBackend) Languages() []string { return slices.Sorted(maps.Keys(tsasmGrammars)) }

func (*tsasmBackend) Supports(lang string) bool {
	_, ok := tsasmGrammars[lang]
	return ok
}

func (b *tsasmBackend) language(name string) *tsasmLang {
	b.load.Do(func() {
		b.langs = map[string]*tsasmLang{}
		for name, f := range tsasmGrammars {
			l := &tsasmLang{lang: f()}
			l.names = make([]string, int(C.tsasm_language_symbol_count(l.lang)))
			for i := range l.names {
				l.names[i] = C.GoString(C.tsasm_language_symbol_name(l.lang, C.uint16_t(i)))
			}
			b.langs[name] = l
		}
	})
	return b.langs[name]
}

func (b *tsasmBackend) get() *tsasmParser {
	b.mu.Lock()
	if n := len(b.idle); n > 0 {
		p := b.idle[n-1]
		b.idle = b.idle[:n-1]
		b.mu.Unlock()
		return p
	}
	b.mu.Unlock()
	p := C.tsasm_parser_new()
	if p == nil {
		return nil
	}
	return &tsasmParser{p: p}
}

func (b *tsasmBackend) put(p *tsasmParser) {
	b.mu.Lock()
	b.idle = append(b.idle, p)
	b.mu.Unlock()
}

func (b *tsasmBackend) Parse(ctx context.Context, lang string, src []byte, _ time.Duration) (*Tree, error) {
	l := b.language(lang)
	if l == nil {
		return nil, fmt.Errorf("%w: tsasm has no %q grammar", ErrUnsupported, lang)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCancelled, err)
	}
	p := b.get()
	if p == nil {
		return nil, fmt.Errorf("%w: tsasm is out of memory", ErrStoppedEarly)
	}
	var srcPtr *C.uint8_t
	if len(src) > 0 {
		srcPtr = (*C.uint8_t)(unsafe.Pointer(&src[0]))
	}
	var out *C.TsasmNode
	n := int64(C.tsasm_parse(p.p, l.lang, srcPtr, C.uint64_t(len(src)), &out))
	if n <= 0 {
		b.put(p)
		switch n {
		case -1:
			return nil, ErrSyntax
		case -3:
			return nil, fmt.Errorf("%w: tsasm ran out of its work, memory or size budget", ErrStoppedEarly)
		case -4:
			return nil, fmt.Errorf("%w: tsasm is out of memory", ErrStoppedEarly)
		}
		return nil, fmt.Errorf("%w: tsasm returned %d", ErrStoppedEarly, n)
	}
	t := &tsasmTree{nodes: unsafe.Slice((*tsasmNode)(unsafe.Pointer(out)), n), lang: l, p: p, src: src}
	return &Tree{t: t, root: t.node(0), lang: lang, backend: "tsasm", src: src}, nil
}

// tsasmNode is TsasmNode: the tree's visible nodes, named and anonymous, in
// preorder, each with its parent's index (-1 for the root).
type tsasmNode struct {
	symbol uint16
	named  uint8
	_      uint8
	start  uint32
	end    uint32
	parent int32
}

var _ [unsafe.Sizeof(tsasmNode{}) - unsafe.Sizeof(C.TsasmNode{})]byte
var _ [unsafe.Sizeof(C.TsasmNode{}) - unsafe.Sizeof(tsasmNode{})]byte

// A tsasm Node is its index in the tree's nodes, in raw[0].
type tsasmTree struct {
	nodes []tsasmNode // the parser's, until release
	lang  *tsasmLang
	p     *tsasmParser
	src   []byte
	sizes []int32  // from p, filled on first use
	lines []uint32 // from p, filled on first use
}

func (t *tsasmTree) node(i int32) Node { return Node{t: t, raw: [4]uint64{uint64(i)}} }

func tsasmIndex(n Node) int32 { return int32(n.raw[0]) }

func (t *tsasmTree) at(n Node) *tsasmNode { return &t.nodes[tsasmIndex(n)] }

func (t *tsasmTree) nodeType(n Node) string  { return t.lang.names[t.at(n).symbol] }
func (t *tsasmTree) isNamed(n Node) bool     { return t.at(n).named != 0 }
func (t *tsasmTree) isError(Node) bool       { return false }
func (t *tsasmTree) isMissing(Node) bool     { return false }
func (t *tsasmTree) hasError(Node) bool      { return false }
func (t *tsasmTree) startByte(n Node) uint32 { return t.at(n).start }
func (t *tsasmTree) endByte(n Node) uint32   { return t.at(n).end }
func (t *tsasmTree) startPoint(n Node) Point { return t.point(t.at(n).start) }
func (t *tsasmTree) endPoint(n Node) Point   { return t.point(t.at(n).end) }
func (t *tsasmTree) id(n Node) uintptr       { return uintptr(tsasmIndex(n)) + 1 }

func (t *tsasmTree) childByField(Node, string) Node { return Node{} }
func (t *tsasmTree) fieldName(Node) string          { return "" }

func (t *tsasmTree) highlights(_ *Tree, dst []Capture) ([]Capture, error) { return dst, nil }

func (t *tsasmTree) parent(n Node) Node {
	if p := t.at(n).parent; p >= 0 {
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
			s[t.nodes[j].parent] += s[j]
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
	if i+1 >= end || t.nodes[i+1].parent != i {
		return
	}
	sizes := t.subtreeSizes()
	for c := i + 1; c < end && t.nodes[c].parent == i; c += sizes[c] {
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
		if !namedOnly || t.nodes[c].named != 0 {
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
	tsasm.put(t.p)
	t.nodes, t.p, t.sizes, t.lines = nil, nil, nil, nil
}
