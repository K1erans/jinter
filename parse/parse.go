// Package parse parses source files into syntax trees behind one interface,
// so the parser behind it can be swapped per language and backends
// compared: gotreesitter (pure Go, the default, every grammar it ships) and
// C tree-sitter (with the jinter_cgo build tag, a few common grammars).
//
// Callers see only this package's types. A Node is a small value that
// refers into its Tree; walking a tree allocates nothing per node, so the
// interface costs little on top of the parser.
package parse

import (
	"context"
	"errors"
	"time"
	"unsafe"
)

// Point is a position in a source: a 0-based row, and a column counted in
// bytes from the start of the row.
type Point struct {
	Row, Column uint32
}

// Node is one node of a Tree. It is a value: copy it freely, compare it
// with ==, and use it only until its Tree is released. The zero Node is
// the null node, which every method treats as empty.
type Node struct {
	t   treeImpl       // nil for the null node
	p   unsafe.Pointer // the backend's Go pointer to the node, if it has one
	raw [4]uint64      // the backend's value for the node, if it has one
}

// Tree is a whole file parsed by one backend. It is not safe for
// concurrent use; parse a file once per goroutine instead.
type Tree struct {
	t       treeImpl
	root    Node
	lang    string
	backend string
	src     []byte
}

// Capture is a node a highlight query captured, by the capture's name
// without its "@", e.g. "function.method", "type.builtin", "constructor".
type Capture struct {
	Name string
	Node Node
}

// Backend is one way of parsing.
type Backend interface {
	// Name is how routing names the backend, e.g. "gotreesitter" or "c".
	Name() string
	// Languages are the language names it can parse, sorted. Names are
	// gotreesitter's registry names ("typescript", "tsx", "go", …),
	// whichever backend is asked.
	Languages() []string
	// Supports says whether it can parse lang.
	Supports(lang string) bool
	// Parse parses src as lang. timeout gives up on a runaway parse; 0
	// means no limit. A parse that finishes returns its tree even when
	// the tree holds syntax errors: check Root().HasError(). When there
	// is no tree the error wraps ErrUnsupported, ErrTimeout, ErrCancelled
	// or ErrStoppedEarly.
	Parse(ctx context.Context, lang string, src []byte, timeout time.Duration) (*Tree, error)
}

// Why there is no tree.
var (
	// ErrUnsupported: no backend can parse the language, or the file has
	// no language at all.
	ErrUnsupported = errors.New("parse: language not supported")
	// ErrTimeout: the parse ran past its timeout. Whether a file beats the
	// clock depends on how busy the machine is.
	ErrTimeout = errors.New("parse: timed out")
	// ErrCancelled: the context ended the parse.
	ErrCancelled = errors.New("parse: cancelled")
	// ErrStoppedEarly: the parser stopped at one of its own limits, or
	// failed for another reason, before accepting the file.
	ErrStoppedEarly = errors.New("parse: stopped early")
)

// treeImpl is what a backend provides for its trees and their nodes.
type treeImpl interface {
	nodeType(n Node) string
	isNamed(n Node) bool
	isError(n Node) bool
	isMissing(n Node) bool
	hasError(n Node) bool
	startByte(n Node) uint32
	endByte(n Node) uint32
	startPoint(n Node) Point
	endPoint(n Node) Point
	childCount(n Node) int
	child(n Node, i int) Node
	children(n Node, dst []Node, namedOnly bool) []Node
	parent(n Node) Node
	childByField(n Node, field string) Node
	fieldName(n Node) string
	id(n Node) uintptr
	highlights(t *Tree, dst []Capture) ([]Capture, error)
	release()
}

// IsNull reports whether n is the null node, which Child, Parent and
// ChildByField return when there is no such node.
func (n Node) IsNull() bool { return n.t == nil }

// Type is the node's type, e.g. "function_declaration", or for an anonymous
// node its text, e.g. "(".
func (n Node) Type() string {
	if n.t == nil {
		return ""
	}
	return n.t.nodeType(n)
}

// IsNamed reports whether the node is named in the grammar, as opposed to
// punctuation and keywords.
func (n Node) IsNamed() bool { return n.t != nil && n.t.isNamed(n) }

// IsError reports whether the node is an ERROR node.
func (n Node) IsError() bool { return n.t != nil && n.t.isError(n) }

// IsMissing reports whether the parser inserted the node to recover from
// an error; it has no text.
func (n Node) IsMissing() bool { return n.t != nil && n.t.isMissing(n) }

// HasError reports whether the node is or contains an ERROR or missing node.
func (n Node) HasError() bool { return n.t != nil && n.t.hasError(n) }

// StartByte is the byte offset where the node starts in the source.
func (n Node) StartByte() uint32 {
	if n.t == nil {
		return 0
	}
	return n.t.startByte(n)
}

// EndByte is the byte offset just past the node's end.
func (n Node) EndByte() uint32 {
	if n.t == nil {
		return 0
	}
	return n.t.endByte(n)
}

// StartPoint is the row and column where the node starts.
func (n Node) StartPoint() Point {
	if n.t == nil {
		return Point{}
	}
	return n.t.startPoint(n)
}

// EndPoint is the row and column just past the node's end.
func (n Node) EndPoint() Point {
	if n.t == nil {
		return Point{}
	}
	return n.t.endPoint(n)
}

// ChildCount is the number of children, named and anonymous.
func (n Node) ChildCount() int {
	if n.t == nil {
		return 0
	}
	return n.t.childCount(n)
}

// Child is the i'th child, named or anonymous, or the null node.
func (n Node) Child(i int) Node {
	if n.t == nil || i < 0 {
		return Node{}
	}
	return n.t.child(n, i)
}

// Children appends every child to dst and returns it, so a walk can reuse
// one buffer.
func (n Node) Children(dst []Node) []Node {
	if n.t == nil {
		return dst
	}
	return n.t.children(n, dst, false)
}

// NamedChildren appends the named children to dst and returns it.
func (n Node) NamedChildren(dst []Node) []Node {
	if n.t == nil {
		return dst
	}
	return n.t.children(n, dst, true)
}

// Parent is the node's parent, or the null node at the root.
func (n Node) Parent() Node {
	if n.t == nil {
		return Node{}
	}
	return n.t.parent(n)
}

// ChildByField is the first child in the grammar field name, e.g. a
// function's "name" or "body", or the null node.
func (n Node) ChildByField(name string) Node {
	if n.t == nil {
		return Node{}
	}
	return n.t.childByField(n, name)
}

// FieldName is the grammar field the node fills in its parent, or "" if
// none. It looks through the parent's children, so it costs more than the
// other methods.
func (n Node) FieldName() string {
	if n.t == nil {
		return ""
	}
	return n.t.fieldName(n)
}

// ID identifies the node within its tree, for use as a map key.
func (n Node) ID() uintptr {
	if n.t == nil {
		return 0
	}
	return n.t.id(n)
}

// Root is the tree's root node.
func (t *Tree) Root() Node { return t.root }

// Language is the language the tree was parsed as.
func (t *Tree) Language() string { return t.lang }

// Backend is the name of the backend that parsed the tree.
func (t *Tree) Backend() string { return t.backend }

// Source is the source the tree was parsed from. Don't modify it.
func (t *Tree) Source() []byte { return t.src }

// Bytes is n's text, sharing the tree's source.
func (t *Tree) Bytes(n Node) []byte {
	start, end := n.StartByte(), n.EndByte()
	if int(end) > len(t.src) || start > end {
		return nil
	}
	return t.src[start:end]
}

// Text is n's text.
func (t *Tree) Text(n Node) string { return string(t.Bytes(n)) }

// Highlights appends the captures of the language's highlight query (its
// highlights.scm) to dst, in source order, and returns it. A node can be
// captured more than once, under different names. It returns dst as it is
// when the backend has no highlight query for the language.
func (t *Tree) Highlights(dst []Capture) ([]Capture, error) {
	return t.t.highlights(t, dst)
}

// Release frees the tree. Its nodes must not be used after.
func (t *Tree) Release() {
	if t != nil && t.t != nil {
		t.t.release()
		t.t = nil
	}
}
