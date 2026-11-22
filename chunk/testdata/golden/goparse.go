package chunk

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"

	gotreesitter "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// isGo reports whether filename gets tree-sitter's Go grammar.
func isGo(filename string) bool {
	e := grammars.DetectLanguage(filename)
	return e != nil && e.Name == "go"
}

// goNode is a top-level node of a Go file as tree-sitter sees it: the
// package clause, a declaration, or a comment outside them.
type goNode struct {
	lo, hi     int    // byte range
	start, end int    // rows
	typ        string // tree-sitter's node type, for kind
	big        bool   // too big to be one piece, so it's opened up
}

// genDeclTypes are tree-sitter's names for go/ast's GenDecl kinds.
var genDeclTypes = map[token.Token]string{
	token.IMPORT: "import_declaration",
	token.CONST:  "const_declaration",
	token.TYPE:   "type_declaration",
	token.VAR:    "var_declaration",
}

// goCandidates finds a Go file's candidates with go/parser, which reads Go
// about ten times faster than tree-sitter. For a file both parse, the two
// agree on the top-level nodes, and those alone make the candidates unless
// one is too big to be a piece. Opening that one up needs tree-sitter's
// tree, so tree-sitter parses a copy of src that keeps only the big nodes,
// the package clause and the comments between nodes (see parseOnly).
//
// It returns routePlain when go/parser rejects src or the partial parse
// fails, and the caller parses the whole file with tree-sitter instead, so
// a file whose partial parse runs out of time gets ParseTimeout twice.
func (s splitter) goCandidates(filename string, src []byte) ([]candidate, route) {
	nodes, ok := s.goNodes(src)
	if !ok {
		return nil, routePlain
	}
	var keep []goNode
	for i, n := range nodes {
		// Comments have no named children, so they are never opened up.
		nodes[i].big = n.typ != "comment" && s.tooBig(n.start, n.end)
		if nodes[i].big || n.typ == "comment" || n.typ == "package_clause" {
			keep = append(keep, nodes[i])
		}
	}
	if !slices.ContainsFunc(nodes, func(n goNode) bool { return n.big }) {
		out := make([]candidate, len(nodes))
		for i, n := range nodes {
			out[i] = s.leaf(n.typ, n.start, n.end, "")
		}
		return firstPerRow(out), routeGo
	}

	bt, trees, ok := s.parseOnly(filename, src, keep)
	if !ok {
		return nil, routePlain
	}
	defer bt.Release()
	var out []candidate
	k := 0 // nodes[i] is keep[k] if it was kept
	for _, n := range nodes {
		if n.big {
			out = append(out, s.nodeCandidates(bt, trees[k], "")...)
		} else {
			out = append(out, s.leaf(n.typ, n.start, n.end, ""))
		}
		if k < len(keep) && keep[k].lo == n.lo {
			k++
		}
	}
	return firstPerRow(out), routeGoTree
}

// goNodes lists src's top-level nodes in order, or fails if go/parser
// rejects src. Rows count newlines in src, so //line directives, which
// go/token would follow, don't move them.
func (s splitter) goNodes(src []byte) ([]goNode, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false
	}
	tf := fset.File(f.Package)
	decls := []goNode{{lo: tf.Offset(f.Package), hi: tf.Offset(f.Name.End()), typ: "package_clause"}}
	for _, d := range f.Decls {
		n := goNode{lo: tf.Offset(d.Pos()), hi: tf.Offset(d.End())} // Pos is the keyword, after any doc comment
		switch d := d.(type) {
		case *ast.GenDecl:
			n.typ = genDeclTypes[d.Tok]
		case *ast.FuncDecl:
			n.typ = "function_declaration"
			if d.Recv != nil {
				n.typ = "method_declaration"
			}
		default:
			return nil, false
		}
		decls = append(decls, n)
	}

	// Comments inside a declaration belong to it; the rest are nodes of
	// their own.
	var comments []goNode
	j := 0
	for _, g := range f.Comments {
		for _, c := range g.List {
			lo := tf.Offset(c.Slash)
			for j < len(decls) && decls[j].hi <= lo {
				j++
			}
			if j < len(decls) && decls[j].lo <= lo {
				continue
			}
			comments = append(comments, goNode{lo: lo, hi: commentEnd(src, lo), typ: "comment"})
		}
	}

	nodes := make([]goNode, 0, len(decls)+len(comments))
	for len(decls) > 0 || len(comments) > 0 {
		if len(comments) == 0 || (len(decls) > 0 && decls[0].lo < comments[0].lo) {
			nodes, decls = append(nodes, decls[0]), decls[1:]
		} else {
			nodes, comments = append(nodes, comments[0]), comments[1:]
		}
	}
	for i := range nodes {
		nodes[i].start, nodes[i].end = s.row(nodes[i].lo), s.row(nodes[i].hi-1)
	}
	return nodes, true
}

// commentEnd is where the comment at src[lo:] ends, as tree-sitter has it:
// a line comment runs to its newline. ast.Comment.End is no use, as it
// measures the text after go/scanner drops carriage returns.
func commentEnd(src []byte, lo int) int {
	if src[lo+1] == '*' {
		// go/parser accepted src, so the comment is closed.
		return lo + 2 + bytes.Index(src[lo+2:], []byte("*/")) + len("*/")
	}
	if i := bytes.IndexByte(src[lo:], '\n'); i >= 0 {
		return lo + i
	}
	return len(src)
}

// row is the row holding byte offset off.
func (s splitter) row(off int) int {
	r, found := slices.BinarySearch(s.starts, off)
	if !found {
		r--
	}
	return r
}

// parseOnly parses keep's byte ranges of src with tree-sitter, in a copy
// where every other byte but newlines is a space, so offsets and rows stay
// as they are. Parsing what the big nodes are left among is cheap: comments
// are one token each, the package clause two.
//
// The tree must come out as it would from src, so parseOnly fails, and the
// caller parses src instead, when the copy could parse differently:
//   - when keep isn't exactly the tree's top-level nodes, which happens if
//     the copy has a syntax error, such as two kept declarations left on
//     one line without the semicolon between them, or the parse times out.
//   - when the copy and src differ in boundaryHints.
func (s splitter) parseOnly(filename string, src []byte, keep []goNode) (*gotreesitter.BoundTree, []*gotreesitter.Node, bool) {
	blank := make([]byte, len(src))
	k := 0
	for i, b := range src {
		for k < len(keep) && keep[k].hi <= i {
			k++
		}
		switch {
		case k < len(keep) && keep[k].lo <= i, b == '\n':
			blank[i] = b
		default:
			blank[i] = ' '
		}
	}
	if boundaryHints(blank) != boundaryHints(src) {
		return nil, nil, false
	}
	bt, ok := parse(filename, blank, s.opts.ParseTimeout)
	if !ok {
		return nil, nil, false
	}
	nodes := namedChildren(bt.RootNode())
	if len(nodes) != len(keep) {
		bt.Release()
		return nil, nil, false
	}
	for i, n := range nodes {
		if int(n.StartByte()) != keep[i].lo || int(n.EndByte()) != keep[i].hi {
			bt.Release()
			return nil, nil, false
		}
	}
	return bt, nodes, true
}

// hints says which of gotreesitter's Go tree repairs run on a source.
type hints struct{ comments, statements bool }

// boundaryHints mirrors goCompatibilitySourceFlagsFor in gotreesitter's
// parser_result_go_compat.go. After parsing Go, gotreesitter moves the ends
// of statement lists and switch cases, but only in sources that contain
// one of these words anywhere, even in a string. A big function in a file
// with a comment parses differently from the same function alone.
func boundaryHints(src []byte) hints {
	h := hints{comments: bytes.Contains(src, []byte("//")) || bytes.Contains(src, []byte("/*"))}
	h.statements = h.comments || slices.ContainsFunc([]string{"case", "default", "switch", "select"}, func(w string) bool {
		return bytes.Contains(src, []byte(w))
	})
	return h
}
