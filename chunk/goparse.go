package chunk

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	gotreesitter "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// isGo reports whether filename gets tree-sitter's Go grammar.
func isGo(filename string) bool {
	// Fast check for the common ".go" suffix avoids calling DetectLanguage
	// for every Go file; non-.go names still defer to DetectLanguage.
	if strings.HasSuffix(filename, ".go") {
		return true
	}
	if ext := filepath.Ext(filename); ext != "" && ext != ".go" {
		return false
	}
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

// goCandidates finds a Go file's candidates with a fast Go top-level scanner
// (falling back to go/parser on edge cases), avoiding AST allocation for
// function bodies. For a file both parse, the two agree on the top-level
// nodes, and those alone make the candidates unless one is too big to be a
// piece. Opening that one up needs tree-sitter's tree, so tree-sitter parses
// a copy of src that keeps only the big nodes, the package clause and the
// comments between nodes (see parseOnly).
//
// It returns routePlain when Go parsing rejects src or the partial parse
// fails, and the caller parses the whole file with tree-sitter instead.
func (s splitter) goCandidates(filename string, src []byte) ([]candidate, route) {
	nodes, ok := s.goNodes(src)
	if !ok {
		return nil, routePlain
	}
	var keep []goNode
	hasBig := false
	for i := range nodes {
		n := &nodes[i]
		// Comments have no named children, so they are never opened up.
		n.big = n.typ != "comment" && s.tooBig(n.start, n.end)
		if n.big {
			hasBig = true
		}
	}
	if !hasBig {
		out := make([]candidate, len(nodes))
		for i, n := range nodes {
			out[i] = s.leaf(n.typ, n.start, n.end, "")
		}
		return firstPerRow(out), routeGo
	}
	for _, n := range nodes {
		if n.big || n.typ == "comment" || n.typ == "package_clause" {
			keep = append(keep, n)
		}
	}

	bt, trees, ok := s.parseOnly(filename, src, keep)
	if !ok {
		return nil, routePlain
	}
	defer bt.Release()
	out := make([]candidate, 0, len(nodes))
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

// goNodes lists src's top-level nodes in order, or fails if src is not valid
// top-level Go. Uses a fast zero-AST scanner and falls back to go/parser only
// if an unusual construct is encountered.
func (s splitter) goNodes(src []byte) ([]goNode, bool) {
	nodes, ok := s.goNodesFast(src)
	if !ok {
		return s.goNodesParser(src)
	}
	return nodes, true
}

// goFastScanner scans Go source for top-level nodes without allocating ASTs.
type goFastScanner struct {
	s     splitter
	src   []byte
	i     int
	row   int
	nodes []goNode
}

func (g *goFastScanner) emit(lo, hi int, typ string) {
	for g.row+1 < g.s.nLines && g.s.starts[g.row+1] <= lo {
		g.row++
	}
	st := g.row
	en := st
	for en+1 < g.s.nLines && g.s.starts[en+1] <= hi-1 {
		en++
	}
	g.nodes = append(g.nodes, goNode{lo: lo, hi: hi, start: st, end: en, typ: typ})
}

func (g *goFastScanner) skipTop() bool {
	src := g.src
	n := len(src)
	for g.i < n {
		b := src[g.i]
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			g.i++
			continue
		}
		if b == '/' && g.i+1 < n {
			if src[g.i+1] == '/' {
				lo := g.i
				if nl := bytes.IndexByte(src[g.i+2:], '\n'); nl >= 0 {
					g.i = g.i + 2 + nl
				} else {
					g.i = n
				}
				g.emit(lo, g.i, "comment")
				continue
			}
			if src[g.i+1] == '*' {
				lo := g.i
				end := bytes.Index(src[g.i+2:], []byte("*/"))
				if end < 0 {
					return false
				}
				g.i = g.i + 2 + end + 2
				g.emit(lo, g.i, "comment")
				continue
			}
		}
		return true
	}
	return true
}

func (g *goFastScanner) scanPackage() bool {
	if !g.skipTop() || g.i >= len(g.src) {
		return false
	}
	src := g.src
	n := len(src)
	pkgLo := g.i
	kw, kwEnd, ok := scanGoIdentBytes(src, g.i)
	if !ok || string(kw) != "package" {
		return false
	}
	g.i = kwEnd
	for g.i < n {
		b := src[g.i]
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			g.i++
			continue
		}
		if b == '/' && g.i+1 < n {
			if src[g.i+1] == '/' {
				if nl := bytes.IndexByte(src[g.i+2:], '\n'); nl >= 0 {
					g.i = g.i + 2 + nl
				} else {
					g.i = n
				}
				continue
			}
			if src[g.i+1] == '*' {
				end := bytes.Index(src[g.i+2:], []byte("*/"))
				if end < 0 {
					return false
				}
				g.i = g.i + 2 + end + 2
				continue
			}
		}
		break
	}
	name, nameEnd, ok := scanGoIdentBytes(src, g.i)
	if !ok || isGoKeywordBytes(name) {
		return false
	}
	g.emit(pkgLo, nameEnd, "package_clause")
	g.i = nameEnd

	needSep := true
	for g.i < n {
		b := src[g.i]
		if b == ' ' || b == '\t' || b == '\r' {
			g.i++
			continue
		}
		if b == '\n' || b == ';' {
			g.i++
			needSep = false
			break
		}
		if b == '/' && g.i+1 < n {
			if src[g.i+1] == '/' {
				lo := g.i
				if nl := bytes.IndexByte(src[g.i+2:], '\n'); nl >= 0 {
					g.i = g.i + 2 + nl
				} else {
					g.i = n
				}
				g.emit(lo, g.i, "comment")
				continue
			}
			if src[g.i+1] == '*' {
				lo := g.i
				end := bytes.Index(src[g.i+2:], []byte("*/"))
				if end < 0 {
					return false
				}
				if bytes.IndexByte(src[g.i+2:g.i+2+end], '\n') >= 0 {
					needSep = false
				}
				g.i = g.i + 2 + end + 2
				g.emit(lo, g.i, "comment")
				if !needSep {
					break
				}
				continue
			}
		}
		break
	}
	return !needSep || g.i >= n
}

func (g *goFastScanner) scanDecl(pending *[][2]int) bool {
	src := g.src
	n := len(src)
	declLo := g.i
	kw, kwEnd, ok := scanGoIdentBytes(src, g.i)
	if !ok {
		return false
	}
	var typ string
	isFunc := false
	switch string(kw) {
	case "import":
		typ = "import_declaration"
	case "const":
		typ = "const_declaration"
	case "type":
		typ = "type_declaration"
	case "var":
		typ = "var_declaration"
	case "func":
		typ = "function_declaration"
		isFunc = true
	default:
		return false
	}
	i := kwEnd
	lastTokEnd := kwEnd
	insertSemi := false
	seenAfterFunc := false
	*pending = (*pending)[:0]

	for i < n {
		b := src[i]
		switch {
		case b == ' ' || b == '\t' || b == '\r':
			i++
			continue
		case b == '\n':
			i++
			if insertSemi {
				goto done
			}
			continue
		case b == '/' && i+1 < n && src[i+1] == '/':
			lo := i
			if nl := bytes.IndexByte(src[i+2:], '\n'); nl >= 0 {
				i = i + 2 + nl
			} else {
				i = n
			}
			if insertSemi {
				*pending = append(*pending, [2]int{lo, i})
			}
			continue
		case b == '/' && i+1 < n && src[i+1] == '*':
			lo := i
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				return false
			}
			hasNL := bytes.IndexByte(src[i+2:i+2+end], '\n') >= 0
			i = i + 2 + end + 2
			if insertSemi {
				*pending = append(*pending, [2]int{lo, i})
				if hasNL {
					goto done
				}
			}
			continue
		case b == ';':
			i++
			goto done
		case b == '"':
			end := scanGoString(src, i)
			if end < 0 {
				return false
			}
			i, lastTokEnd, insertSemi, seenAfterFunc = end, end, true, true
			*pending = (*pending)[:0]
			continue
		case b == '`':
			end := bytes.IndexByte(src[i+1:], '`')
			if end < 0 {
				return false
			}
			i = i + 1 + end + 1
			lastTokEnd, insertSemi, seenAfterFunc = i, true, true
			*pending = (*pending)[:0]
			continue
		case b == '\'':
			end := scanGoRune(src, i)
			if end < 0 {
				return false
			}
			i, lastTokEnd, insertSemi, seenAfterFunc = end, end, true, true
			*pending = (*pending)[:0]
			continue
		case b == '(' || b == '[' || b == '{':
			if isFunc && !seenAfterFunc && b == '(' {
				typ = "method_declaration"
			}
			*pending = (*pending)[:0]
			seenAfterFunc = true
			nextI, ok := skipBalancedGo(src, i+1)
			if !ok {
				return false
			}
			i, lastTokEnd, insertSemi = nextI, nextI, true
			continue
		case b == ')' || b == ']' || b == '}':
			return false
		}

		*pending = (*pending)[:0]
		seenAfterFunc = true
		if isIdentStart(b) || b >= utf8.RuneSelf {
			tok, end, ok := scanGoIdentBytes(src, i)
			if !ok {
				return false
			}
			i, lastTokEnd = end, end
			insertSemi = !isGoKeywordNoSemiBytes(tok)
			continue
		}
		if b >= '0' && b <= '9' {
			i++
			for i < n {
				c := src[i]
				if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '.' {
					if (c == 'e' || c == 'E' || c == 'p' || c == 'P') && i+1 < n && (src[i+1] == '+' || src[i+1] == '-') {
						i += 2
						continue
					}
					i++
					continue
				}
				break
			}
			lastTokEnd, insertSemi = i, true
			continue
		}
		if (b == '+' && i+1 < n && src[i+1] == '+') || (b == '-' && i+1 < n && src[i+1] == '-') {
			i += 2
			lastTokEnd, insertSemi = i, true
			continue
		}
		i++
		lastTokEnd, insertSemi = i, false
	}
done:
	if lastTokEnd <= declLo {
		return false
	}
	g.i = i
	g.emit(declLo, lastTokEnd, typ)
	for _, c := range *pending {
		g.emit(c[0], c[1], "comment")
	}
	return true
}

// goNodesFast scans Go source directly for package_clause, top-level
// declarations (import/const/type/var/func/method) and top-level comments
// without building any AST for function bodies.
func (s splitter) goNodesFast(src []byte) ([]goNode, bool) {
	if len(src) == 0 || !utf8.Valid(src) || bytes.IndexByte(src, 0) >= 0 {
		return nil, false
	}
	start := 0
	if len(src) >= 3 && src[0] == 0xef && src[1] == 0xbb && src[2] == 0xbf {
		start = 3
	}
	g := goFastScanner{s: s, src: src, i: start, nodes: make([]goNode, 0, len(src)/256+8)}
	if !g.scanPackage() {
		return nil, false
	}
	var pending [][2]int
	for {
		if !g.skipTop() {
			return nil, false
		}
		if g.i >= len(src) {
			break
		}
		if src[g.i] == ';' {
			g.i++
			continue
		}
		if !g.scanDecl(&pending) {
			return nil, false
		}
	}
	return g.nodes, true
}

// goBodySpecial marks the only 10 bytes that can affect delimiter nesting
// inside a Go block/paren/bracket body.
var goBodySpecial = [256]bool{
	'{': true, '}': true,
	'(': true, ')': true,
	'[': true, ']': true,
	'"': true, '\'': true, '`': true,
	'/': true,
}

// skipBalancedGo scans from just after an opening '(', '[', or '{' (depth == 1)
// until matching depth 0, skipping Go comments, strings, raw strings and runes.
// Uses a table-driven inner loop with zero allocations.
func skipBalancedGo(src []byte, i int) (int, bool) {
	n := len(src)
	depth := 1
	for i < n {
		for i+4 <= n && !goBodySpecial[src[i]] && !goBodySpecial[src[i+1]] && !goBodySpecial[src[i+2]] && !goBodySpecial[src[i+3]] {
			i += 4
		}
		for i < n && !goBodySpecial[src[i]] {
			i++
		}
		if i >= n {
			break
		}
		switch src[i] {
		case '{', '(', '[':
			depth++
			i++
		case '}', ')', ']':
			depth--
			i++
			if depth == 0 {
				return i, true
			}
		case '"':
			end := scanGoString(src, i)
			if end < 0 {
				return 0, false
			}
			i = end
		case '`':
			end := bytes.IndexByte(src[i+1:], '`')
			if end < 0 {
				return 0, false
			}
			i = i + 1 + end + 1
		case '\'':
			end := scanGoRune(src, i)
			if end < 0 {
				return 0, false
			}
			i = end
		case '/':
			if i+1 < n {
				if src[i+1] == '/' {
					if nl := bytes.IndexByte(src[i+2:], '\n'); nl >= 0 {
						i = i + 2 + nl + 1
					} else {
						i = n
					}
					continue
				}
				if src[i+1] == '*' {
					end := bytes.Index(src[i+2:], []byte("*/"))
					if end < 0 {
						return 0, false
					}
					i = i + 2 + end + 2
					continue
				}
			}
			i++
		}
	}
	return 0, false
}

func scanGoString(src []byte, i int) int {
	i++ // skip opening '"'
	n := len(src)
	for i < n {
		b := src[i]
		if b == '"' {
			return i + 1
		}
		if b == '\\' {
			i += 2
			continue
		}
		if b == '\n' {
			return -1
		}
		i++
	}
	return -1
}

func scanGoRune(src []byte, i int) int {
	i++ // skip opening '\''
	n := len(src)
	for i < n {
		b := src[i]
		if b == '\'' {
			return i + 1
		}
		if b == '\\' {
			i += 2
			continue
		}
		if b == '\n' {
			return -1
		}
		i++
	}
	return -1
}

func isIdentStart(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || b == '_'
}

func isIdentCont(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9') || b == '_'
}

func scanGoIdentBytes(src []byte, i int) ([]byte, int, bool) {
	n := len(src)
	if i >= n {
		return nil, i, false
	}
	start := i
	if src[i] < utf8.RuneSelf {
		if !isIdentStart(src[i]) {
			return nil, i, false
		}
		i++
	} else {
		r, size := utf8.DecodeRune(src[i:])
		if !unicode.IsLetter(r) {
			return nil, i, false
		}
		i += size
	}
	for i < n {
		b := src[i]
		if b < utf8.RuneSelf {
			if !isIdentCont(b) {
				break
			}
			i++
		} else {
			r, size := utf8.DecodeRune(src[i:])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				break
			}
			i += size
		}
	}
	return src[start:i], i, true
}

func isGoKeywordBytes(s []byte) bool {
	switch string(s) {
	case "break", "case", "chan", "const", "continue",
		"default", "defer", "else", "fallthrough", "for",
		"func", "go", "goto", "if", "import",
		"interface", "map", "package", "range", "return",
		"select", "struct", "switch", "type", "var":
		return true
	}
	return false
}

// isGoKeywordNoSemiBytes reports whether s is a Go keyword that does NOT
// trigger automatic semicolon insertion at a newline.
func isGoKeywordNoSemiBytes(s []byte) bool {
	switch string(s) {
	case "case", "chan", "const", "default", "defer", "else", "for",
		"func", "go", "goto", "if", "import", "interface", "map",
		"package", "range", "select", "struct", "switch", "type", "var":
		return true
	}
	return false
}

// goNodesParser is the fallback using go/parser when goNodesFast declines src.
func (s splitter) goNodesParser(src []byte) ([]goNode, bool) {
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
	r, found := slices.BinarySearch(s.starts[:s.numLines()], off)
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
