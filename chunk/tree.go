package chunk

import (
	"errors"
	"fmt"
	"time"

	gotreesitter "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// Language is the tree-sitter grammar for a file.
type Language struct {
	Name    string                 // the registry's name, e.g. "go", "typescript", "tsx"
	Grammar *gotreesitter.Language // what queries and node types are resolved against
	// HighlightQuery is the grammar's highlights.scm, with the queries it
	// inherits composed in (TypeScript includes JavaScript's). It is empty
	// for a grammar that ships none.
	HighlightQuery string
	Entry          *grammars.LangEntry // the full registry entry
}

// Detect finds the grammar for filename, or nil when there is none. It
// loads the grammar on first use.
func Detect(filename string) *Language {
	entry := grammars.DetectLanguage(filename)
	if entry == nil {
		return nil
	}
	return &Language{Name: entry.Name, Grammar: entry.Language(), HighlightQuery: entry.HighlightQuery, Entry: entry}
}

// Tree is a whole file parsed by tree-sitter: the bound tree (its root
// node, node types and text, and Source) and the grammar it was parsed
// with. Call Release when done with it, which returns its nodes to the
// parser's arena; nodes must not be used after that.
type Tree struct {
	*gotreesitter.BoundTree
	Lang *Language
}

// Why Parse gave no tree. Every one of them means Split cuts the file with
// its plain indentation splitter instead of at syntax nodes, except Go
// files, which Split reads with go/parser first and only falls back when
// that fails too.
var (
	// ErrNoGrammar: no tree-sitter grammar matches the file name.
	ErrNoGrammar = errors.New("chunk: no grammar for file")
	// ErrTimeout: the parse ran past its timeout. Whether a file beats the
	// clock depends on how busy the machine is, so the same file can
	// parse on one run and time out on the next.
	ErrTimeout = errors.New("chunk: parse timed out")
	// ErrStoppedEarly: the parser stopped at one of its own limits (on
	// iterations, stack depth, nodes or memory, which scale with file
	// size), or failed for another reason, before accepting the file.
	ErrStoppedEarly = errors.New("chunk: parse stopped early")
	// ErrSyntax: the parse finished but the tree holds errors, either
	// because the file is invalid or because the grammar can't read it.
	ErrSyntax = errors.New("chunk: syntax errors")
)

// Parse parses src with the tree-sitter grammar for filename, with the
// same parsers, pools and acceptance rules Split uses, so a file Parse
// accepts is one Split cuts at syntax nodes (Go files aside: Split reads
// those with go/parser, but Parse always uses tree-sitter). timeout gives
// up on a runaway parse, as Options.ParseTimeout does for Split; 0 means
// no limit.
//
// When there is no tree, the error says why and wraps one of ErrNoGrammar,
// ErrTimeout, ErrStoppedEarly or ErrSyntax.
func Parse(filename string, src []byte, timeout time.Duration) (*Tree, error) {
	entry := grammars.DetectLanguage(filename)
	if entry == nil {
		return nil, fmt.Errorf("%s: %w", filename, ErrNoGrammar)
	}
	lang := entry.Language()
	bt, err := parseEntry(entry, lang, src, timeout)
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", filename, entry.Name, err)
	}
	return &Tree{BoundTree: bt, Lang: &Language{Name: entry.Name, Grammar: lang, HighlightQuery: entry.HighlightQuery, Entry: entry}}, nil
}

// parse is Parse for Split, which only needs to know whether there is a
// tree.
func parse(filename string, src []byte, timeout time.Duration) (*gotreesitter.BoundTree, bool) {
	entry := grammars.DetectLanguage(filename)
	if entry == nil {
		return nil, false
	}
	bt, err := parseEntry(entry, entry.Language(), src, timeout)
	return bt, err == nil
}

// parseEntry parses src with entry's grammar, lang. It fails when the source
// has syntax errors or the parse stops early: at the parser's own work
// limits or past timeout.
func parseEntry(entry *grammars.LangEntry, lang *gotreesitter.Language, src []byte, timeout time.Duration) (*gotreesitter.BoundTree, error) {
	if entry.Name == "swift" {
		// See swiftUnits: a unit that fails means the whole file fails.
		units, sl := swiftUnits(src)
		for _, u := range units {
			bt, err := parseWhole(entry, lang, u.text(src), timeout)
			if err != nil {
				return nil, fmt.Errorf("lines %d-%d on their own: %w", sl.row(u.lo)+1, sl.row(u.hi-1)+1, err)
			}
			bt.Release()
		}
	}
	return parseWhole(entry, lang, src, timeout)
}

// parseWhole is parseEntry without parsing a Swift file's units first.
func parseWhole(entry *grammars.LangEntry, lang *gotreesitter.Language, src []byte, timeout time.Duration) (*gotreesitter.BoundTree, error) {
	pool := parserPool(entry.Name, lang, timeout)
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
		if errors.As(err, &stopped) && stopped.Reason == gotreesitter.ParseStopTimeout {
			return nil, fmt.Errorf("%w after %v", ErrTimeout, timeout)
		}
		return nil, fmt.Errorf("%w: %v", ErrStoppedEarly, err)
	}
	bt := gotreesitter.Bind(tree)
	if root := bt.RootNode(); root == nil || root.HasError() {
		bt.Release()
		return nil, ErrSyntax
	}
	return bt, nil
}
