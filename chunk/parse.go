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
	if entry.TokenSourceFactory != nil {
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
