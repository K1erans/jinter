package chunk

import (
	"sync"
	"time"

	gotreesitter "github.com/odvcencio/gotreesitter"
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

// parserPool is the pool of parsers for the grammar name, lang, that give
// up past timeout (0 means no limit).
func parserPool(name string, lang *gotreesitter.Language, timeout time.Duration) *gotreesitter.ParserPool {
	if timeout > 0 {
		timeout = max(timeout, time.Microsecond) // the parser counts whole microseconds; 0 is no limit
	}
	k := poolKey{name, timeout}
	p, loaded := pools.Load(k)
	if !loaded {
		p, _ = pools.LoadOrStore(k, gotreesitter.NewParserPool(lang,
			gotreesitter.WithParserPoolTimeoutMicros(uint64(timeout/time.Microsecond))))
	}
	return p.(*gotreesitter.ParserPool)
}
