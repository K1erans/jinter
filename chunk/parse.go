package chunk

import (
	"sync"
	"time"

	gotreesitter "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// poolKey picks a parser pool: pools are per language, and a pool's
// parsers carry its timeout.
type poolKey struct {
	lang    string
	timeout time.Duration
}

var pools sync.Map // poolKey → *gotreesitter.ParserPool

// grammarLexer names the grammars parsed with their own lexer even though
// they register a token source. gotreesitter serves its certified grammars
// with ccore, its faster engine that matches C tree-sitter, but only when no
// token source, work limits or included ranges are set. With C's token
// source, C parses on the old engine at a fifth of the speed or less, and
// a few valid files fail. C is the only certified grammar with a token
// source in v0.58.0; recheck on upgrades. C++ and Java, which ccore doesn't
// serve, keep theirs: their grammar lexers are slower.
var grammarLexer = map[string]bool{"c": true}

// parse parses src with the grammar for filename. It fails when there is no
// grammar, the source has syntax errors, or the parse stops early: at the
// parser's own work limits or past timeout (0 means no limit).
func parse(filename string, src []byte, timeout time.Duration) (bt *gotreesitter.BoundTree, ok bool) {
	entry := grammars.DetectLanguage(filename)
	if entry == nil {
		return nil, false
	}
	lang := entry.Language()
	if timeout > 0 {
		timeout = max(timeout, time.Microsecond) // the parser counts whole microseconds; 0 is no limit
	}
	k := poolKey{entry.Name, timeout}
	p, loaded := pools.Load(k)
	if !loaded {
		p, _ = pools.LoadOrStore(k, gotreesitter.NewParserPool(lang,
			gotreesitter.WithParserPoolTimeoutMicros(uint64(timeout/time.Microsecond))))
	}
	pool := p.(*gotreesitter.ParserPool)
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
		return nil, false
	}
	bt = gotreesitter.Bind(tree)
	if root := bt.RootNode(); root == nil || root.HasError() {
		bt.Release()
		return nil, false
	}
	return bt, true
}
