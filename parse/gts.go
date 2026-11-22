package parse

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"unsafe"

	gotreesitter "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// GoTreeSitter is the pure-Go backend, github.com/odvcencio/gotreesitter,
// and the default for every language. It parses every grammar the
// registry ships.
var GoTreeSitter Backend = gts

var gts = &gtsBackend{}

type gtsBackend struct {
	pools   sync.Map // gtsPoolKey → *gotreesitter.ParserPool
	queries sync.Map // language name → *gtsQuery
	langs   sync.Once
	names   []string
}

// gtsPoolKey picks a parser pool: pools are per language, and a pool's
// parsers carry its timeout.
type gtsPoolKey struct {
	lang    string
	timeout time.Duration
}

type gtsQuery struct {
	once sync.Once
	q    *gotreesitter.Query
	err  error
}

// grammarLexer names the grammars parsed with their own lexer even though
// they register a token source. gotreesitter serves its certified grammars
// with ccore, its faster engine that matches C tree-sitter, but only when no
// token source, work limits or included ranges are set. With C's token
// source, C parses on the old engine at a fifth of the speed or less, and
// a few valid files fail. C is the only certified grammar with a token
// source in v0.58.0; recheck on upgrades. C++ and Java, which ccore doesn't
// serve, keep theirs: their grammar lexers are slower.
var grammarLexer = map[string]bool{"c": true}

func (*gtsBackend) Name() string { return "gotreesitter" }

func (b *gtsBackend) Languages() []string {
	b.langs.Do(func() {
		for _, e := range grammars.AllLanguages() {
			b.names = append(b.names, e.Name)
		}
		slices.Sort(b.names)
		b.names = slices.Compact(b.names)
	})
	return slices.Clone(b.names)
}

func (*gtsBackend) Supports(lang string) bool { return grammars.DetectLanguageByName(lang) != nil }

func (b *gtsBackend) Parse(ctx context.Context, lang string, src []byte, timeout time.Duration) (*Tree, error) {
	entry := grammars.DetectLanguageByName(lang)
	if entry == nil {
		return nil, fmt.Errorf("%w: gotreesitter has no %q grammar", ErrUnsupported, lang)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCancelled, err)
	}
	return b.parseEntry(entry, entry.Language(), src, timeout)
}

// parseEntry parses src with entry's grammar, lang.
func (b *gtsBackend) parseEntry(entry *grammars.LangEntry, lang *gotreesitter.Language, src []byte, timeout time.Duration) (*Tree, error) {
	pool := b.pool(entry.Name, lang, timeout)
	var tree *gotreesitter.Tree
	var err error
	if entry.TokenSourceFactory != nil && !grammarLexer[entry.Name] {
		tree, err = pool.ParseWithTokenSourceStrict(src, entry.TokenSourceFactory(src, lang))
	} else {
		tree, err = pool.ParseStrict(src)
	}
	if err != nil {
		if tree != nil {
			tree.Release()
		}
		var stopped *gotreesitter.ParseStoppedEarlyError
		if errors.As(err, &stopped) {
			switch stopped.Reason {
			case gotreesitter.ParseStopTimeout:
				return nil, fmt.Errorf("%w after %v", ErrTimeout, timeout)
			case gotreesitter.ParseStopCancelled:
				return nil, ErrCancelled
			}
		}
		return nil, fmt.Errorf("%w: %v", ErrStoppedEarly, err)
	}
	bt := gotreesitter.Bind(tree)
	root := bt.RootNode()
	if root == nil {
		bt.Release()
		return nil, fmt.Errorf("%w: no root node", ErrStoppedEarly)
	}
	t := &gtsTree{bt: bt, lang: lang, name: entry.Name}
	return &Tree{t: t, root: t.node(root), lang: entry.Name, backend: "gotreesitter", src: src}, nil
}

// pool is the pool of parsers for the grammar name, lang, that give up past
// timeout (0 means no limit).
func (b *gtsBackend) pool(name string, lang *gotreesitter.Language, timeout time.Duration) *gotreesitter.ParserPool {
	if timeout > 0 {
		timeout = max(timeout, time.Microsecond) // the parser counts whole microseconds; 0 is no limit
	}
	k := gtsPoolKey{name, timeout}
	p, loaded := b.pools.Load(k)
	if !loaded {
		p, _ = b.pools.LoadOrStore(k, gotreesitter.NewParserPool(lang,
			gotreesitter.WithParserPoolTimeoutMicros(uint64(timeout/time.Microsecond))))
	}
	return p.(*gotreesitter.ParserPool)
}

// query is lang's compiled highlight query, or nil if it has none.
func (b *gtsBackend) query(name string, lang *gotreesitter.Language) (*gotreesitter.Query, error) {
	v, _ := b.queries.LoadOrStore(name, &gtsQuery{})
	q := v.(*gtsQuery)
	q.once.Do(func() {
		entry := grammars.DetectLanguageByName(name)
		if entry == nil || entry.HighlightQuery == "" {
			return
		}
		q.q, q.err = gotreesitter.NewQuery(entry.HighlightQuery, lang)
		if q.err != nil {
			q.err = fmt.Errorf("parse: gotreesitter's %s highlight query: %w", name, q.err)
		}
	})
	return q.q, q.err
}

type gtsTree struct {
	bt   *gotreesitter.BoundTree
	lang *gotreesitter.Language
	name string
}

func (t *gtsTree) node(n *gotreesitter.Node) Node {
	if n == nil {
		return Node{}
	}
	return Node{t: t, p: unsafe.Pointer(n)}
}

func gn(n Node) *gotreesitter.Node { return (*gotreesitter.Node)(n.p) }

func (t *gtsTree) nodeType(n Node) string   { return t.bt.NodeType(gn(n)) }
func (t *gtsTree) isNamed(n Node) bool      { return gn(n).IsNamed() }
func (t *gtsTree) isError(n Node) bool      { return gn(n).IsError() }
func (t *gtsTree) isMissing(n Node) bool    { return gn(n).IsMissing() }
func (t *gtsTree) hasError(n Node) bool     { return gn(n).HasError() }
func (t *gtsTree) startByte(n Node) uint32  { return gn(n).StartByte() }
func (t *gtsTree) endByte(n Node) uint32    { return gn(n).EndByte() }
func (t *gtsTree) childCount(n Node) int    { return gn(n).ChildCount() }
func (t *gtsTree) child(n Node, i int) Node { return t.node(gn(n).Child(i)) }
func (t *gtsTree) parent(n Node) Node       { return t.node(gn(n).Parent()) }
func (t *gtsTree) id(n Node) uintptr        { return uintptr(n.p) }

func (t *gtsTree) startPoint(n Node) Point {
	p := gn(n).StartPoint()
	return Point{p.Row, p.Column}
}

func (t *gtsTree) endPoint(n Node) Point {
	p := gn(n).EndPoint()
	return Point{p.Row, p.Column}
}

// children uses Children, not NamedChild(i): NamedChild(i) and
// NamedChildCount count from the first child on every call, so looping over
// them is quadratic, and a 3 MB file holding one long literal took 7
// minutes.
func (t *gtsTree) children(n Node, dst []Node, namedOnly bool) []Node {
	for _, c := range gn(n).Children() {
		if !namedOnly || c.IsNamed() {
			dst = append(dst, Node{t: t, p: unsafe.Pointer(c)})
		}
	}
	return dst
}

func (t *gtsTree) childByField(n Node, field string) Node {
	return t.node(t.bt.ChildByField(gn(n), field))
}

func (t *gtsTree) fieldName(n Node) string {
	parent := gn(n).Parent()
	if parent == nil {
		return ""
	}
	for i, c := range parent.Children() {
		if c == gn(n) {
			return parent.FieldNameForChild(i, t.lang)
		}
	}
	return ""
}

func (t *gtsTree) highlights(tree *Tree, dst []Capture) ([]Capture, error) {
	q, err := gts.query(t.name, t.lang)
	if q == nil {
		return dst, err
	}
	first := len(dst)
	for _, m := range q.ExecuteNode(gn(tree.root), t.lang, tree.src) {
		for _, c := range m.Captures {
			dst = append(dst, Capture{Name: c.Name, Node: t.node(c.Node)})
		}
	}
	sortCaptures(dst[first:])
	return dst, nil
}

// sortCaptures puts captures in source order: by start, outer nodes first,
// keeping the query's order between captures of one node.
func sortCaptures(cs []Capture) {
	slices.SortStableFunc(cs, func(a, b Capture) int {
		return cmp.Or(cmp.Compare(a.Node.StartByte(), b.Node.StartByte()), cmp.Compare(b.Node.EndByte(), a.Node.EndByte()))
	})
}

func (t *gtsTree) release() { t.bt.Release() }
